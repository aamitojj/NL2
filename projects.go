package main

// Projects: every site has a folder on this PC
// (Documents\Dilreet\Projects\<name>\ by default) with
//
//	current\                         the files of the latest deploy (edit and redeploy)
//	deploys\<time>_<id>.zip          every deploy, kept permanently unless a retention limit is set
//	deploys\index.json               metadata: time, source, checks, size, status, notes
//
// Nothing here talks to the network; publishing an archived deploy re-uses
// the normal deploy path.

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var projMu sync.Mutex

type CheckSummary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

type DeployRecord struct {
	ID       string        `json:"id"`
	Time     string        `json:"time"`
	File     string        `json:"file"`             // zip name inside deploys\
	Source   string        `json:"source"`           // drop | folder | git | netlify | watch | archive
	Detail   string        `json:"detail,omitempty"` // e.g. "uploaded folder dist", "github.com/x/y@main abc1234"
	Commit   string        `json:"commit,omitempty"`
	Checks   *CheckSummary `json:"checks,omitempty"`
	Files    int           `json:"files"`
	Bytes    int64         `json:"bytes"` // uncompressed
	Size     int64         `json:"size"`  // zip size on disk
	Status   string        `json:"status"`
	Release  string        `json:"release,omitempty"` // server release name
	Error    string        `json:"error,omitempty"`
	Notes    string        `json:"notes,omitempty"`
	Publish  string        `json:"publish_dir,omitempty"`
	Restored string        `json:"restored,omitempty"` // last time it was published again
}

type deployIndex struct {
	Project string          `json:"project"`
	Name    string          `json:"name"`
	Deploys []*DeployRecord `json:"deploys"` // newest first
}

func projectsRoot(c *Config) string {
	if c.ProjectsDir != "" {
		return c.ProjectsDir
	}
	if d := envOr("PROJECTS_DIR"); d != "" {
		return d
	}
	return filepath.Join(documentsDir(), "Dilreet", "Projects")
}

var reservedWin = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])$`)

// folderName turns a project name into a safe Windows folder name.
func folderName(name, slug string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteRune('-')
			continue
		}
		b.WriteRune(r)
	}
	s := strings.TrimRight(b.String(), ". ")
	if len([]rune(s)) > 60 {
		s = strings.TrimRight(string([]rune(s)[:60]), ". ")
	}
	if s == "" || reservedWin.MatchString(s) {
		s = slug
	}
	return s
}

// assignFolders gives every site a project folder name (migration for sites
// created before projects existed) and creates the folders. Caller holds cfgMu
// via withConfig.
func assignFolders(c *Config) (changed bool) {
	used := map[string]bool{}
	for _, s := range c.Sites {
		if s.Folder != "" {
			used[strings.ToLower(s.Folder)] = true
		}
	}
	for _, s := range c.Sites {
		if s.Folder == "" {
			f := folderName(s.Name, s.Slug)
			if used[strings.ToLower(f)] {
				f = f + "-" + s.Slug
			}
			used[strings.ToLower(f)] = true
			s.Folder, changed = f, true
		}
	}
	root := projectsRoot(c)
	for _, s := range c.Sites {
		d := filepath.Join(root, s.Folder)
		if err := os.MkdirAll(filepath.Join(d, "current"), 0o755); err != nil {
			log.Printf("project folder %s: %v", d, err)
			continue
		}
		_ = os.MkdirAll(filepath.Join(d, "deploys"), 0o755)
		ip := filepath.Join(d, "deploys", "index.json")
		if _, err := os.Stat(ip); errors.Is(err, os.ErrNotExist) {
			b, _ := json.MarshalIndent(deployIndex{Project: s.Slug, Name: s.Name, Deploys: []*DeployRecord{}}, "", "  ")
			_ = os.WriteFile(ip, b, 0o644)
		}
	}
	return changed
}

func ensureProjects() {
	if len(snapshotConfig().Sites) == 0 {
		return
	}
	_ = withConfig(func(c *Config) error {
		assignFolders(c)
		return nil
	})
}

func projectDir(slug string) (string, *Site, error) {
	c := snapshotConfig()
	s := findSite(&c, slug)
	if s == nil {
		return "", nil, errors.New("project not found")
	}
	if s.Folder == "" {
		ensureProjects()
		c = snapshotConfig()
		s = findSite(&c, slug)
	}
	return filepath.Join(projectsRoot(&c), s.Folder), s, nil
}

func readIndex(dir string) (*deployIndex, error) {
	b, err := os.ReadFile(filepath.Join(dir, "deploys", "index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &deployIndex{Deploys: []*DeployRecord{}}, nil
	}
	if err != nil {
		return nil, err
	}
	ix := &deployIndex{}
	if err := json.Unmarshal(b, ix); err != nil {
		return nil, fmt.Errorf("deploys\\index.json is damaged: %w", err)
	}
	return ix, nil
}

func writeIndex(dir string, ix *deployIndex) error {
	_ = os.MkdirAll(filepath.Join(dir, "deploys"), 0o755)
	b, _ := json.MarshalIndent(ix, "", "  ")
	p := filepath.Join(dir, "deploys", "index.json")
	if err := os.WriteFile(p+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (ix *deployIndex) find(id string) *DeployRecord {
	for _, d := range ix.Deploys {
		if d.ID == id {
			return d
		}
	}
	return nil
}

var deployIDRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}_[0-9a-f]{6}$`)

// sourceKind classifies a workspace's origin for the deploy history.
func sourceKind(ws *Workspace, projDir string) (kind, detail string) {
	src := ws.Source
	switch {
	case strings.HasPrefix(src, "import "):
		return "git", strings.TrimPrefix(src, "import ")
	case strings.HasPrefix(src, "Netlify site"):
		return "netlify", src
	case strings.HasPrefix(src, "uploaded "):
		return "drop", src
	case strings.HasPrefix(src, "archive "):
		return "archive", strings.TrimPrefix(src, "archive ")
	case projDir != "" && strings.HasPrefix(strings.ToLower(filepath.Clean(src)), strings.ToLower(filepath.Join(projDir, "current"))):
		return "folder", "project folder (current)"
	case src != "":
		return "folder", src
	}
	return "drop", ""
}

// writeZip zips the files under root (skipping .git and node_modules).
func writeZip(dst, root string) (files int, bytes int64, err error) {
	entries, err := dirEntries(root)
	if err != nil {
		return 0, 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	f, err := os.Create(dst + ".tmp")
	if err != nil {
		return 0, 0, err
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.Path, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			break
		}
		rc, err2 := e.Open()
		if err2 != nil {
			err = err2
			break
		}
		n, err2 := io.Copy(w, rc)
		rc.Close()
		if err2 != nil {
			err = err2
			break
		}
		files++
		bytes += n
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst + ".tmp")
		return 0, 0, err
	}
	return files, bytes, os.Rename(dst+".tmp", dst)
}

// replaceDir makes dst an exact copy of src.
func replaceDir(dst, src string) error {
	tmp := dst + ".new"
	os.RemoveAll(tmp)
	entries, err := dirEntries(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(tmp, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		rc, err := e.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(p)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	_ = os.MkdirAll(tmp, 0o755)
	old := dst + ".old"
	os.RemoveAll(old)
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Rename(old, dst)
		return err
	}
	return os.RemoveAll(old)
}

// archiveDeploy stores ws in the project folder before it is published. A
// failure here is reported but never blocks the deploy.
func archiveDeploy(j *Job, slug string, ws *Workspace) *DeployRecord {
	dir, site, err := projectDir(slug)
	if err != nil {
		return nil
	}
	projMu.Lock()
	defer projMu.Unlock()
	now := time.Now()
	rec := &DeployRecord{ID: now.Format("20060102-150405") + "_" + randHex(3), Time: now.Format(time.RFC3339), Status: "deploying", Publish: ws.PublishRel}
	rec.File = rec.ID + ".zip"
	rec.Source, rec.Detail = sourceKind(ws, dir)
	if rec.Source == "git" && site.Git != nil {
		rec.Commit = site.Git.LastSHA
	}
	if ws.Source == "watch" || (site.Watch && site.WatchPath != "" && filepath.Clean(ws.Source) == filepath.Clean(site.WatchPath)) {
		rec.Source = "watch"
	}
	rep := runChecks(ws)
	rec.Checks = &CheckSummary{Pass: rep.Counts["pass"], Warn: rep.Counts["warn"], Fail: rep.Counts["fail"]}
	if err := os.MkdirAll(filepath.Join(dir, "deploys"), 0o755); err != nil {
		j.Logf("WARNING: could not create the project folder %s: %v", dir, err)
		return nil
	}
	zp := filepath.Join(dir, "deploys", rec.File)
	if rec.Files, rec.Bytes, err = writeZip(zp, ws.Root); err != nil {
		j.Logf("WARNING: could not save a local copy of this deploy: %v", err)
		return nil
	}
	if st, err := os.Stat(zp); err == nil {
		rec.Size = st.Size()
	}
	if err := replaceDir(filepath.Join(dir, "current"), ws.Root); err != nil {
		j.Logf("WARNING: could not update %s: %v", filepath.Join(dir, "current"), err)
	}
	ix, err := readIndex(dir)
	if err != nil {
		j.Logf("WARNING: %v", err)
		ix = &deployIndex{}
	}
	ix.Project, ix.Name = slug, site.Name
	ix.Deploys = append([]*DeployRecord{rec}, ix.Deploys...)
	if err := writeIndex(dir, ix); err != nil {
		j.Logf("WARNING: could not update the deploy history: %v", err)
	}
	j.Logf("Saved a copy of this deploy on this PC: %s", zp)
	return rec
}

// finishDeploy records the outcome and applies the retention setting.
func finishDeploy(slug, id string, deployErr error) {
	dir, site, err := projectDir(slug)
	if err != nil || id == "" {
		return
	}
	projMu.Lock()
	defer projMu.Unlock()
	ix, err := readIndex(dir)
	if err != nil {
		return
	}
	r := ix.find(id)
	if r == nil {
		return
	}
	if deployErr != nil {
		r.Status, r.Error = "failed", truncate(deployErr.Error(), 300)
	} else {
		r.Status, r.Release, r.Error = "published", site.Release, ""
	}
	if keep := snapshotConfig().KeepDeploys; keep > 0 && deployErr == nil {
		var kept []*DeployRecord
		n := 0
		for _, d := range ix.Deploys {
			if d.Status == "published" || d.Status == "deploying" {
				n++
			}
			if n > keep && d.ID != id && d.Release != site.Release {
				os.Remove(filepath.Join(dir, "deploys", d.File))
				continue
			}
			kept = append(kept, d)
		}
		ix.Deploys = kept
	}
	_ = writeIndex(dir, ix)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// markRestored notes that an archived deploy was published again.
func markRestored(slug, id, release string) {
	dir, _, err := projectDir(slug)
	if err != nil {
		return
	}
	projMu.Lock()
	defer projMu.Unlock()
	ix, err := readIndex(dir)
	if err != nil {
		return
	}
	if r := ix.find(id); r != nil {
		r.Restored, r.Status = time.Now().Format(time.RFC3339), "published"
		if release != "" {
			r.Release = release
		}
		_ = writeIndex(dir, ix)
	}
}

// zipManifest lists the files of an archived deploy with size+CRC.
func zipManifest(p string) (map[string][2]uint64, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	m := map[string][2]uint64{}
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			m[f.Name] = [2]uint64{f.UncompressedSize64, uint64(f.CRC32)}
		}
	}
	return m, nil
}

type DeployDiff struct {
	Base    string   `json:"base"`
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
	Same    int      `json:"unchanged"`
}

func compareManifests(oldM, newM map[string][2]uint64) DeployDiff {
	d := DeployDiff{Added: []string{}, Removed: []string{}, Changed: []string{}}
	for k, v := range newM {
		o, ok := oldM[k]
		switch {
		case !ok:
			d.Added = append(d.Added, k)
		case o != v:
			d.Changed = append(d.Changed, k)
		default:
			d.Same++
		}
	}
	for k := range oldM {
		if _, ok := newM[k]; !ok {
			d.Removed = append(d.Removed, k)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Changed)
	return d
}

func dirUsage(dir string) (n int64) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if i, e := d.Info(); e == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}

// safeJoin resolves rel inside base and refuses anything outside it
// (including through symlinks/junctions).
func safeJoin(base, rel string) (string, error) {
	cr, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	p := filepath.Join(base, filepath.FromSlash(cr))
	rb, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("not found: %s", cr)
	}
	if rp != rb && !strings.HasPrefix(rp, rb+string(filepath.Separator)) {
		return "", errors.New("path is outside the project folder")
	}
	return rp, nil
}

var riskyExt = map[string]bool{".exe": true, ".bat": true, ".cmd": true, ".com": true, ".ps1": true, ".vbs": true, ".vbe": true, ".js": true, ".jse": true, ".wsf": true, ".wsh": true, ".hta": true, ".lnk": true, ".scr": true, ".msi": true, ".msp": true, ".reg": true, ".pif": true, ".cpl": true, ".jar": true, ".url": true, ".application": true}

// isRiskyToOpen reports files whose default "open" action runs code; the
// editor action opens them in Notepad instead.
func isRiskyToOpen(p string) bool { return riskyExt[strings.ToLower(filepath.Ext(p))] }

type fileItem struct {
	Path  string `json:"path"`
	Dir   bool   `json:"dir,omitempty"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
}

func registerProjectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects/{slug}/files", func(w http.ResponseWriter, r *http.Request) {
		dir, _, err := projectDir(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		cur := filepath.Join(dir, "current")
		_ = os.MkdirAll(cur, 0o755)
		items := []fileItem{}
		var total int64
		truncated := false
		_ = filepath.WalkDir(cur, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == cur {
				return nil
			}
			if len(items) >= 5000 {
				truncated = true
				return filepath.SkipAll
			}
			rel, _ := filepath.Rel(cur, p)
			info, e := d.Info()
			if e != nil {
				return nil
			}
			it := fileItem{Path: filepath.ToSlash(rel), Dir: d.IsDir(), MTime: info.ModTime().Format(time.RFC3339)}
			if !d.IsDir() {
				it.Size = info.Size()
				total += it.Size
			}
			items = append(items, it)
			return nil
		})
		writeJSON(w, 200, map[string]any{"dir": dir, "current": cur, "items": items, "bytes": total, "truncated": truncated})
	})
	// Raw file for preview/download. The response is sandboxed (no scripts,
	// opaque origin) so a previewed HTML page can never call the Dilreet API.
	mux.HandleFunc("GET /api/projects/{slug}/raw/{path...}", func(w http.ResponseWriter, r *http.Request) {
		dir, _, err := projectDir(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		p, err := safeJoin(filepath.Join(dir, "current"), r.PathValue("path"))
		if err != nil {
			writeJSON(w, 404, map[string]any{"error": err.Error()})
			return
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			writeJSON(w, 404, map[string]any{"error": "not a file"})
			return
		}
		ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
		if ct == "" {
			ct = "application/octet-stream"
		}
		if r.URL.Query().Get("text") == "1" {
			ct = "text/plain; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Del("X-Frame-Options") // framed by the Files tab only (frame-ancestors 'self')
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; media-src 'self'; frame-ancestors 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(p)))
		}
		f, err := os.Open(p)
		if err != nil {
			writeErr(w, err)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, "", st.ModTime(), f) // ServeContent: no index.html redirect
	})
	mux.HandleFunc("GET /api/projects/{slug}/folder.zip", func(w http.ResponseWriter, r *http.Request) {
		dir, s, err := projectDir(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		tmp, err := os.CreateTemp("", "dilreet-folder-*.zip")
		if err != nil {
			writeErr(w, err)
			return
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		os.Remove(tmp.Name())
		if _, _, err := writeZip(tmp.Name(), filepath.Join(dir, "current")); err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", s.Slug+"-files.zip"))
		http.ServeFile(w, r, tmp.Name())
	})
	mux.HandleFunc("POST /api/projects/{slug}/open", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			What string `json:"what"` // folder | deploys | reveal | editor
			Path string `json:"path"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		dir, _, err := projectDir(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		cur := filepath.Join(dir, "current")
		switch req.What {
		case "folder":
			err = revealInExplorer(cur, false)
		case "deploys":
			err = revealInExplorer(filepath.Join(dir, "deploys"), false)
		case "reveal", "editor":
			var p string
			if p, err = safeJoin(cur, req.Path); err == nil {
				if st, e := os.Stat(p); e == nil && st.IsDir() {
					err = revealInExplorer(p, false)
				} else if req.What == "reveal" {
					err = revealInExplorer(p, true)
				} else {
					err = openInEditor(p)
				}
			}
		default:
			err = errors.New("unknown action")
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/projects/{slug}/deploys", func(w http.ResponseWriter, r *http.Request) {
		dir, s, err := projectDir(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		ix, err := readIndex(dir)
		if err != nil {
			writeErr(w, err)
			return
		}
		type out struct {
			*DeployRecord
			Live    bool `json:"live"`
			Missing bool `json:"missing,omitempty"`
		}
		list := []out{}
		for _, d := range ix.Deploys {
			_, e := os.Stat(filepath.Join(dir, "deploys", d.File))
			list = append(list, out{d, d.Release != "" && d.Release == s.Release, e != nil})
		}
		writeJSON(w, 200, map[string]any{"dir": dir, "deploys": list, "bytes": dirUsage(filepath.Join(dir, "deploys")), "keep": snapshotConfig().KeepDeploys})
	})
	withRec := func(w http.ResponseWriter, r *http.Request) (string, *Site, *deployIndex, *DeployRecord, bool) {
		id := r.PathValue("id")
		if !deployIDRe.MatchString(id) {
			writeErr(w, errors.New("invalid deploy id"))
			return "", nil, nil, nil, false
		}
		dir, s, err := projectDir(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return "", nil, nil, nil, false
		}
		ix, err := readIndex(dir)
		if err != nil {
			writeErr(w, err)
			return "", nil, nil, nil, false
		}
		rec := ix.find(id)
		if rec == nil {
			writeJSON(w, 404, map[string]any{"error": "deploy not found in the history"})
			return "", nil, nil, nil, false
		}
		return dir, s, ix, rec, true
	}
	mux.HandleFunc("GET /api/projects/{slug}/deploys/{id}/zip", func(w http.ResponseWriter, r *http.Request) {
		dir, s, _, rec, ok := withRec(w, r)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", s.Slug+"-"+rec.ID+".zip"))
		http.ServeFile(w, r, filepath.Join(dir, "deploys", rec.File))
	})
	mux.HandleFunc("GET /api/projects/{slug}/deploys/{id}/compare", func(w http.ResponseWriter, r *http.Request) {
		dir, _, ix, rec, ok := withRec(w, r)
		if !ok {
			return
		}
		base := r.URL.Query().Get("with")
		var baseRec *DeployRecord
		if base != "" {
			baseRec = ix.find(base)
		} else {
			for i, d := range ix.Deploys {
				if d.ID == rec.ID && i+1 < len(ix.Deploys) {
					baseRec = ix.Deploys[i+1]
				}
			}
		}
		newM, err := zipManifest(filepath.Join(dir, "deploys", rec.File))
		if err != nil {
			writeErr(w, fmt.Errorf("cannot read this deploy's archive: %w", err))
			return
		}
		oldM := map[string][2]uint64{}
		baseID := ""
		if baseRec != nil {
			if oldM, err = zipManifest(filepath.Join(dir, "deploys", baseRec.File)); err != nil {
				writeErr(w, fmt.Errorf("cannot read the previous deploy's archive: %w", err))
				return
			}
			baseID = baseRec.ID
		}
		d := compareManifests(oldM, newM)
		d.Base = baseID
		writeJSON(w, 200, d)
	})
	mux.HandleFunc("POST /api/projects/{slug}/deploys/{id}/notes", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Notes string `json:"notes"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		projMu.Lock()
		defer projMu.Unlock()
		dir, _, ix, rec, ok := withRec(w, r)
		if !ok {
			return
		}
		rec.Notes = truncate(strings.TrimSpace(req.Notes), 500)
		if err := writeIndex(dir, ix); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("DELETE /api/projects/{slug}/deploys/{id}", func(w http.ResponseWriter, r *http.Request) {
		projMu.Lock()
		defer projMu.Unlock()
		dir, s, ix, rec, ok := withRec(w, r)
		if !ok {
			return
		}
		if rec.Release != "" && rec.Release == s.Release {
			writeErr(w, errors.New("this deploy is live; publish another one before deleting its archive"))
			return
		}
		if err := os.Remove(filepath.Join(dir, "deploys", rec.File)); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeErr(w, err)
			return
		}
		var kept []*DeployRecord
		for _, d := range ix.Deploys {
			if d.ID != rec.ID {
				kept = append(kept, d)
			}
		}
		ix.Deploys = kept
		if err := writeIndex(dir, ix); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	// Publish an archived deploy: switch to its server release when that copy
	// still exists, otherwise upload it again from the local zip.
	mux.HandleFunc("POST /api/projects/{slug}/deploys/{id}/publish", func(w http.ResponseWriter, r *http.Request) {
		dir, s, _, rec, ok := withRec(w, r)
		if !ok {
			return
		}
		if !sshS.connected() {
			writeErr(w, errNotConnected)
			return
		}
		slug, id := s.Slug, rec.ID
		zp := filepath.Join(dir, "deploys", rec.File)
		release := rec.Release
		j := newJob("publish-archive", func(j *Job) error {
			if release != "" {
				if rels, _, err := listReleases(slug); err == nil {
					for _, x := range rels {
						if x == release {
							j.Logf("The server still has this deploy (release %s): switching back to it.", release)
							if err := activateRelease(slug, release); err != nil {
								return err
							}
							markRestored(slug, id, release)
							j.Logf("Done. Deploy %s is live again.", id)
							return nil
						}
					}
				}
				j.Logf("The server no longer keeps release %s; uploading it again from %s.", release, zp)
			} else {
				j.Logf("Uploading deploy %s from %s.", id, zp)
			}
			entries, cleanup, err := zipEntries(zp)
			if err != nil {
				return fmt.Errorf("cannot open the local archive: %w", err)
			}
			defer cleanup()
			ws, err := newWorkspace(entries, "archive "+id)
			if err != nil {
				return err
			}
			defer ws.Close()
			ws.restoreOf = id
			return deployWorkspace(j, slug, ws)
		})
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
	mux.HandleFunc("GET /api/storage", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		root := projectsRoot(&c)
		type pu struct {
			Slug    string `json:"slug"`
			Name    string `json:"name"`
			Dir     string `json:"dir"`
			Current int64  `json:"current"`
			Deploys int64  `json:"deploys"`
			Count   int    `json:"count"`
		}
		list := []pu{}
		var total int64
		for _, s := range c.Sites {
			if s.Folder == "" {
				continue
			}
			d := filepath.Join(root, s.Folder)
			u := pu{Slug: s.Slug, Name: s.Name, Dir: d, Current: dirUsage(filepath.Join(d, "current")), Deploys: dirUsage(filepath.Join(d, "deploys"))}
			if ix, err := readIndex(d); err == nil {
				u.Count = len(ix.Deploys)
			}
			total += u.Current + u.Deploys
			list = append(list, u)
		}
		writeJSON(w, 200, map[string]any{"root": root, "default_root": filepath.Join(documentsDir(), "Dilreet", "Projects"), "keep": c.KeepDeploys, "projects": list, "bytes": total})
	})
	mux.HandleFunc("POST /api/storage", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Root *string `json:"root"`
			Keep *int    `json:"keep"`
			Move bool    `json:"move"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		c := snapshotConfig()
		oldRoot := projectsRoot(&c)
		newRoot := oldRoot
		if req.Root != nil {
			nr := strings.Trim(strings.TrimSpace(*req.Root), `"`)
			if nr != "" {
				if !filepath.IsAbs(nr) {
					writeErr(w, errors.New("enter a full folder path, e.g. D:\\Websites"))
					return
				}
				if err := os.MkdirAll(nr, 0o755); err != nil {
					writeErr(w, fmt.Errorf("cannot use that folder: %w", err))
					return
				}
				newRoot = filepath.Clean(nr)
			} else {
				newRoot = filepath.Join(documentsDir(), "Dilreet", "Projects")
			}
		}
		moved := 0
		if req.Move && !strings.EqualFold(newRoot, oldRoot) {
			for _, s := range c.Sites {
				if s.Folder == "" {
					continue
				}
				src, dst := filepath.Join(oldRoot, s.Folder), filepath.Join(newRoot, s.Folder)
				if _, err := os.Stat(dst); err == nil {
					writeErr(w, fmt.Errorf("%s already exists in the new location; nothing was moved", dst))
					return
				}
				if err := os.Rename(src, dst); err != nil {
					if err := copyTree(dst, src); err != nil {
						writeErr(w, fmt.Errorf("moving %s failed: %w", s.Folder, err))
						return
					}
					os.RemoveAll(src)
				}
				moved++
			}
		}
		err := withConfig(func(c *Config) error {
			if req.Root != nil {
				if strings.EqualFold(newRoot, filepath.Join(documentsDir(), "Dilreet", "Projects")) {
					c.ProjectsDir = ""
				} else {
					c.ProjectsDir = newRoot
				}
			}
			if req.Keep != nil {
				if *req.Keep < 0 || *req.Keep > 10000 {
					return errors.New("keep must be 0 (all) or a positive number")
				}
				c.KeepDeploys = *req.Keep
			}
			assignFolders(c)
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "root": newRoot, "moved": moved})
	})
}

func copyTree(dst, src string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(t)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// migrateConfigDir copies settings from the pre-rename folder the first time
// Dilreet starts. The old folder is left untouched as a backup.
func migrateConfigDir(oldDir, newDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(newDir, "config.json")); err == nil {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(oldDir, "config.json")); err != nil {
		return false, nil
	}
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return false, err
	}
	ents, err := os.ReadDir(oldDir)
	if err != nil {
		return false, err
	}
	for _, e := range ents {
		if e.IsDir() || !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(oldDir, e.Name()))
		if err != nil {
			return false, err
		}
		if e.Name() == "config.json" {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				m["migrated_from"] = oldDir
				b, _ = json.MarshalIndent(m, "", "  ")
			}
		}
		if err := os.WriteFile(filepath.Join(newDir, e.Name()), b, 0o600); err != nil {
			return false, err
		}
	}
	return true, nil
}
