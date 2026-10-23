package main

// A Workspace is an unpacked copy of an uploaded site on this PC. App Testing
// previews and checks it, and "Deploy this version" uploads exactly its
// publish folder. Regular deployments use a short-lived workspace too, so
// both paths share the same publish-folder detection and rule parsing.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type fileInfo struct {
	Size int64
}

// siteFiles is the exact set of files that will be published (the deploy
// skip-list applied), keyed by slash path. Lookups are case-sensitive like the
// Linux server, even on Windows.
type siteFiles struct {
	files map[string]fileInfo
	lower map[string][]string
}

func (s *siteFiles) IsFile(rel string) bool { _, ok := s.files[rel]; return ok }

func (s *siteFiles) sorted() []string {
	out := make([]string, 0, len(s.files))
	for k := range s.files {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// caseMatch returns the real name of a file that matches rel only when case
// is ignored.
func (s *siteFiles) caseMatch(rel string) string {
	for _, c := range s.lower[strings.ToLower(rel)] {
		if c != rel {
			return c
		}
	}
	return ""
}

type ProjectInfo struct {
	Framework     string   `json:"framework"`
	PackageJSON   bool     `json:"package_json"`
	BuildScript   bool     `json:"build_script"`
	BuildCommand  string   `json:"build_command,omitempty"`
	TomlCommand   string   `json:"toml_command,omitempty"`
	EnvFiles      []string `json:"env_files,omitempty"`
	FunctionsDir  string   `json:"functions_dir,omitempty"`
	SourceProject bool     `json:"source_project"`
}

type Workspace struct {
	Dir         string      `json:"-"`
	Root        string      `json:"-"`
	Publish     string      `json:"-"`
	PublishRel  string      `json:"publish_dir"`
	PublishNote string      `json:"publish_note"`
	Rules       *SiteRules  `json:"-"`
	Files       *siteFiles  `json:"-"`
	Issues      []RuleIssue `json:"issues"`
	Project     ProjectInfo `json:"project"`
	Source      string      `json:"source"`
	LocalPath   string      `json:"local_path,omitempty"`
	Hash        string      `json:"hash"`
	FileCount   int         `json:"files"`
	Bytes       int64       `json:"bytes"`
	Loaded      time.Time   `json:"loaded"`
}

func (w *Workspace) Close() {
	if w != nil && w.Dir != "" {
		os.RemoveAll(w.Dir)
	}
}

func (w *Workspace) issue(level, src, msg, fix string) {
	w.Issues = append(w.Issues, RuleIssue{Level: level, Source: src, Msg: msg, Fix: fix})
}

var outputDirCandidates = []string{"dist", "build", "out", "public", "_site", "www", "site", "docs"}

// newWorkspace unpacks entries into a fresh temp folder.
func newWorkspace(entries []srcEntry, source string) (*Workspace, error) {
	entries = stripCommonRoot(entries)
	if len(entries) == 0 {
		return nil, errors.New("the upload contains no files")
	}
	if len(entries) > maxSiteFiles {
		return nil, fmt.Errorf("too many files (%d, max %d)", len(entries), maxSiteFiles)
	}
	var total int64
	for _, e := range entries {
		if e.Size < 0 {
			return nil, fmt.Errorf("bad size for %s", e.Path)
		}
		total += e.Size
	}
	if total > maxSiteBytes {
		return nil, fmt.Errorf("site is too large (%d MB, max %d MB)", total>>20, maxSiteBytes>>20)
	}
	dir, err := os.MkdirTemp("", "hostpanel-test-*")
	if err != nil {
		return nil, err
	}
	w := &Workspace{Dir: dir, Root: filepath.Join(dir, "site"), Source: source, Loaded: time.Now()}
	ok := false
	defer func() {
		if !ok {
			w.Close()
		}
	}()
	if err := os.Mkdir(w.Root, 0o700); err != nil {
		return nil, err
	}
	seenLower := map[string]string{}
	for _, e := range entries {
		if prev, dup := seenLower[strings.ToLower(e.Path)]; dup {
			if prev == e.Path {
				continue
			}
			w.issue("fail", e.Path, fmt.Sprintf("%s and %s differ only in upper/lower case. Windows can only keep one of them, so %s was skipped here.", prev, e.Path, e.Path), "Rename one of the files; links should use one consistent spelling.")
			continue
		}
		seenLower[strings.ToLower(e.Path)] = e.Path
		if err := w.extractOne(e); err != nil {
			w.issue("fail", e.Path, fmt.Sprintf("%s could not be unpacked on this PC (%v), so it is missing from the test and from 'Deploy this version'.", e.Path, err), "Rename the file (Windows forbids names like con, aux, nul and characters like : * ? \" < > |).")
		}
	}
	w.detectPublish()
	if err := w.index(); err != nil {
		return nil, err
	}
	w.detectProject()
	ok = true
	return w, nil
}

func (w *Workspace) extractOne(e srcEntry) error {
	dst := filepath.Join(w.Root, filepath.FromSlash(e.Path))
	if rel, err := filepath.Rel(w.Root, dst); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path escapes the workspace")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	rc, err := e.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(rc, e.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != e.Size {
		err = fmt.Errorf("size mismatch (expected %d bytes, got %d)", e.Size, n)
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

func isFileOS(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isDirOS(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// detectPublish picks the folder to serve: netlify.toml [build] publish,
// else the top level when it has index.html, else a common build output
// folder (dist, build, ...) that has index.html.
func (w *Workspace) detectPublish() {
	tomlDir := w.Root
	pre := &SiteRules{}
	if b, err := os.ReadFile(filepath.Join(w.Root, "netlify.toml")); err == nil {
		pre.parseNetlifyTOML(string(b), "netlify.toml")
	}
	choose := func(rel, note string) {
		w.PublishRel, w.PublishNote = rel, note
		w.Publish = filepath.Join(w.Root, filepath.FromSlash(rel))
	}
	if pre.TomlFound && pre.Publish != "" {
		base, _ := cleanRel(strings.TrimPrefix(pre.Base, "./"))
		pub := strings.TrimPrefix(pre.Publish, "./")
		joined := pub
		if base != "" && !strings.HasPrefix(path.Clean(pub), path.Clean(base)+"/") {
			joined = path.Join(base, pub)
		}
		rel, err := cleanRel(joined)
		switch {
		case err != nil:
			w.issue("fail", "netlify.toml", fmt.Sprintf("The publish folder %q points outside the upload.", pre.Publish), "Use a folder inside your project, e.g. publish = \"dist\".")
		case rel == "" || isDirOS(filepath.Join(w.Root, filepath.FromSlash(rel))):
			label := rel
			if label == "" {
				label = "the top level"
			}
			choose(rel, fmt.Sprintf("from netlify.toml (publish = %q): serving %s", pre.Publish, label))
			w.Rules = loadSiteRules(tomlDir, w.Publish)
			return
		default:
			w.issue("fail", "netlify.toml", fmt.Sprintf("netlify.toml says publish = %q but that folder is not in the upload.", pre.Publish), "Run your build first (e.g. npm run build) and include its output folder, or zip only the built folder.")
		}
	}
	if isFileOS(filepath.Join(w.Root, "index.html")) {
		choose("", "top level of the upload (index.html found)")
	} else {
		found := false
		for _, c := range outputDirCandidates {
			if isFileOS(filepath.Join(w.Root, c, "index.html")) {
				choose(c, fmt.Sprintf("auto-detected build folder %q (it has index.html)", c))
				found = true
				break
			}
		}
		if !found {
			choose("", "top level of the upload (no index.html found anywhere obvious)")
		}
	}
	w.Rules = loadSiteRules(tomlDir, w.Publish)
}

// index builds the file set with the same skip rules as a deployment and
// computes a content hash of exactly what would be published.
func (w *Workspace) index() error {
	entries, err := dirEntries(w.Publish)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	fs := &siteFiles{files: map[string]fileInfo{}, lower: map[string][]string{}}
	h := sha256.New()
	for _, e := range entries {
		fs.files[e.Path] = fileInfo{Size: e.Size}
		lk := strings.ToLower(e.Path)
		fs.lower[lk] = append(fs.lower[lk], e.Path)
		w.Bytes += e.Size
		fh, err := fileHash(filepath.Join(w.Publish, filepath.FromSlash(e.Path)))
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", e.Path, e.Size, fh)
	}
	w.Files = fs
	w.FileCount = len(entries)
	w.Hash = hex.EncodeToString(h.Sum(nil))
	return nil
}

func fileHash(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyUnchanged recomputes the hash before a deployment.
func (w *Workspace) verifyUnchanged() error {
	old := w.Hash
	cp := *w
	cp.Bytes = 0
	if err := cp.index(); err != nil {
		return err
	}
	if cp.Hash != old {
		return errors.New("the tested files changed on disk since the test; load the site again and re-test")
	}
	return nil
}

var envFileNames = map[string]bool{".env": true, ".env.local": true, ".env.production": true, ".env.development": true}

// detectProject is the port of AppLab's analyzer: framework, build script,
// env files and Netlify-only features. Purely informational.
func (w *Workspace) detectProject() {
	p := &w.Project
	p.Framework = "Static HTML"
	if w.Rules != nil {
		p.TomlCommand = w.Rules.BuildCommand
		if w.Rules.HasFunctions {
			p.FunctionsDir = "netlify.toml [functions]"
		}
	}
	for _, d := range []string{"netlify/functions", "netlify/edge-functions"} {
		if isDirOS(filepath.Join(w.Root, filepath.FromSlash(d))) {
			p.FunctionsDir = d
		}
	}
	_ = filepath.WalkDir(w.Root, func(fp string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git") {
			return filepath.SkipDir
		}
		n := d.Name()
		if !d.IsDir() && (envFileNames[n] || strings.HasPrefix(n, ".env.")) {
			rel, _ := filepath.Rel(w.Root, fp)
			p.EnvFiles = append(p.EnvFiles, filepath.ToSlash(rel))
		}
		return nil
	})
	b, err := os.ReadFile(filepath.Join(w.Root, "package.json"))
	if err != nil {
		return
	}
	p.PackageJSON = true
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		w.issue("warn", "package.json", "package.json is not valid JSON.", "Fix the JSON syntax (it only matters for your build tools).")
		p.Framework = "Node.js project"
		return
	}
	deps := map[string]bool{}
	for k := range pkg.Dependencies {
		deps[k] = true
	}
	for k := range pkg.DevDependencies {
		deps[k] = true
	}
	switch {
	case deps["next"]:
		p.Framework = "Next.js"
	case deps["nuxt"]:
		p.Framework = "Nuxt"
	case deps["@angular/core"]:
		p.Framework = "Angular"
	case deps["@sveltejs/kit"]:
		p.Framework = "SvelteKit"
	case deps["astro"]:
		p.Framework = "Astro"
	case deps["gatsby"]:
		p.Framework = "Gatsby"
	case deps["@11ty/eleventy"]:
		p.Framework = "Eleventy"
	case deps["vue"] && deps["vite"]:
		p.Framework = "Vue + Vite"
	case deps["react"] && deps["vite"]:
		p.Framework = "React + Vite"
	case deps["vue"]:
		p.Framework = "Vue"
	case deps["react"]:
		p.Framework = "React"
	case deps["vite"]:
		p.Framework = "Vite"
	default:
		p.Framework = "Node.js project"
	}
	if s, ok := pkg.Scripts["build"]; ok && strings.TrimSpace(s) != "" {
		p.BuildScript = true
		p.BuildCommand = "npm run build"
	}
	// The source is being served (not a build output) when package.json sits
	// in the publish folder.
	p.SourceProject = w.PublishRel == "" || isFileOS(filepath.Join(w.Publish, "package.json"))
}
