package main

// HTTP flow for "Import from Git or link" and "Import from Netlify", the
// per-site "Pull latest and deploy" button and auto-deploy on new commits.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type stagedImport struct {
	ID     string
	FR     *FetchResult
	At     time.Time
	Busy   bool
	Origin string // original pasted URL
}

var (
	stagedMu sync.Mutex
	staged   = map[string]*stagedImport{}
)

func stage(fr *FetchResult, origin string) *stagedImport {
	s := &stagedImport{ID: randHex(8), FR: fr, At: time.Now(), Origin: origin}
	stagedMu.Lock()
	staged[s.ID] = s
	stagedMu.Unlock()
	return s
}

func getStaged(id string) (*stagedImport, error) {
	stagedMu.Lock()
	defer stagedMu.Unlock()
	s := staged[id]
	if s == nil {
		return nil, errors.New("this import expired; paste the link again")
	}
	return s, nil
}

func dropStaged(id string) {
	stagedMu.Lock()
	s := staged[id]
	delete(staged, id)
	stagedMu.Unlock()
	if s != nil && s.FR != nil && s.FR.WS != nil {
		s.FR.WS.Close()
	}
}

func stagedIDs() []string {
	stagedMu.Lock()
	defer stagedMu.Unlock()
	return sortedKeys(staged)
}

// expireStaged frees imports nobody finished within an hour.
func expireStaged() {
	stagedMu.Lock()
	var old []string
	for id, s := range staged {
		if !s.Busy && time.Since(s.At) > time.Hour {
			old = append(old, id)
		}
	}
	stagedMu.Unlock()
	for _, id := range old {
		dropStaged(id)
	}
}

func stagedSummary(s *stagedImport) map[string]any {
	ws := s.FR.WS
	fails := 0
	for _, i := range ws.Issues {
		if i.Level == "fail" {
			fails++
		}
	}
	out := map[string]any{
		"import_id": s.ID, "display": s.FR.Ref.Display, "kind": s.FR.Ref.Kind, "branch": s.FR.Ref.Branch, "subdir": s.FR.Ref.Subdir,
		"sha": s.FR.SHA, "framework": ws.Project.Framework, "files": ws.FileCount, "bytes": ws.Bytes, "publish_dir": ws.PublishRel,
		"publish_note": ws.PublishNote, "has_index": ws.Files.IsFile("index.html"), "build": planBuild(ws), "issues": len(ws.Issues), "fails": fails,
		"suggested_name": suggestName(s.FR.Ref), "issue_list": ws.Issues,
	}
	for k, v := range s.FR.Extras {
		out[k] = v
	}
	return out
}

func suggestName(r *RepoRef) string {
	switch {
	case r.Kind == "netlify" || r.Kind == "upload":
		if r.Site != "" {
			return r.Site
		}
	case r.Repo != "":
		if r.Subdir != "" {
			return r.Repo + "-" + strings.ReplaceAll(r.Subdir, "/", "-")
		}
		return r.Repo
	}
	u, err := url.Parse(r.URL)
	if err == nil {
		b := strings.TrimSuffix(archiveRe.ReplaceAllString(pathBase(u.Path), ""), ".git")
		if b != "" {
			return b
		}
	}
	return "imported-site"
}

func pathBase(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func siteRef(s *Site) (*RepoRef, error) {
	if s.Git == nil {
		return nil, errors.New("this site is not linked to a repository")
	}
	ref, err := parseRepoURL(s.Git.URL)
	if err != nil {
		return nil, err
	}
	ref.Branch, ref.Subdir, ref.TreePath = s.Git.Branch, s.Git.Subdir, ""
	return ref, nil
}

// ---------- pull + deploy ----------

var (
	pullMu      sync.Mutex
	pullRunning = map[string]bool{}
)

func pullAndDeployJob(j *Job, slug string, auto bool) error {
	pullMu.Lock()
	if pullRunning[slug] {
		pullMu.Unlock()
		return errors.New("a pull for this site is already running")
	}
	pullRunning[slug] = true
	pullMu.Unlock()
	defer func() { pullMu.Lock(); delete(pullRunning, slug); pullMu.Unlock() }()

	c := snapshotConfig()
	s := findSite(&c, slug)
	if s == nil {
		return errors.New("site not found")
	}
	ref, err := siteRef(s)
	if err != nil {
		return err
	}
	j.SetSteps("Downloading the latest commit", "Building (if needed)", "Checking", "Publishing")
	j.Step(0)
	ctx := context.Background()
	fr, err := fetchRepo(ctx, j, ref)
	if err != nil {
		return err
	}
	defer fr.WS.Close()
	j.Step(1)
	if bp := planBuild(fr.WS); bp.Needed {
		if !s.Git.Build {
			return errors.New("this project needs a build. Open the site and click 'Pull latest and deploy' once to approve building it on this PC")
		}
		if err := runBuild(ctx, j, fr.WS); err != nil {
			return err
		}
	}
	j.Step(2)
	rep := runChecks(fr.WS)
	j.Logf("Checks: %d passed, %d warnings, %d failed.", rep.Counts["pass"], rep.Counts["warn"], rep.Counts["fail"])
	if s.Git.TestFirst && rep.Counts["fail"] > 0 {
		return fmt.Errorf("commit %s has %d failing check(s), so it was NOT published (Test before publishing is on). Open App Testing to review", short(fr.SHA), rep.Counts["fail"])
	}
	j.Step(3)
	if err := deployWorkspace(j, slug, fr.WS); err != nil {
		return err
	}
	_ = withConfig(func(c *Config) error {
		if st := findSite(c, slug); st != nil && st.Git != nil {
			st.Git.LastSHA, st.Git.LastError = fr.SHA, ""
		}
		return nil
	})
	if auto {
		notify("success", slug, fmt.Sprintf("%s: new commit %s deployed automatically.", s.Name, short(fr.SHA)))
	}
	return nil
}

// ---------- commit poller ----------

var (
	latestCommitFn = func(ctx context.Context, s *Site) (string, error) {
		ref, err := siteRef(s)
		if err != nil {
			return "", err
		}
		return newGitAPI(ref).LatestCommit(ctx, s.Git.Branch)
	}
	startAutoDeploy = func(slug string) {
		newJob("auto-deploy", func(j *Job) error {
			err := pullAndDeployJob(j, slug, true)
			if err != nil {
				notify("warn", slug, "Auto-deploy of "+slug+" failed: "+err.Error())
				_ = withConfig(func(c *Config) error {
					if st := findSite(c, slug); st != nil && st.Git != nil {
						st.Git.LastError = err.Error()
					}
					return nil
				})
			}
			return err
		})
	}
	deployReady = func() bool { return sshS.connected() }
)

// commitPollTick checks every auto-deploy site whose interval has elapsed and
// starts a redeploy when the branch moved. Returns the slugs it redeployed.
func commitPollTick(ctx context.Context, now time.Time) []string {
	c := snapshotConfig()
	var started []string
	for _, s := range c.Sites {
		g := s.Git
		if g == nil || !g.AutoDeploy || g.Kind == "archive" || g.Kind == "git" {
			continue
		}
		iv := g.IntervalMin
		if iv <= 0 {
			iv = 5
		}
		if t, err := time.Parse(time.RFC3339, g.LastCheck); err == nil && now.Sub(t) < time.Duration(iv)*time.Minute {
			continue
		}
		pullMu.Lock()
		busy := pullRunning[s.Slug]
		pullMu.Unlock()
		if busy {
			continue
		}
		sha, err := latestCommitFn(ctx, s)
		slug := s.Slug
		_ = withConfig(func(c *Config) error {
			if st := findSite(c, slug); st != nil && st.Git != nil {
				st.Git.LastCheck = now.UTC().Format(time.RFC3339)
				if err != nil {
					st.Git.LastError = err.Error()
				}
			}
			return nil
		})
		if err != nil || sha == "" || sha == g.LastSHA {
			continue
		}
		if !deployReady() {
			_ = withConfig(func(c *Config) error {
				if st := findSite(c, slug); st != nil && st.Git != nil {
					st.Git.LastError = "New commit " + short(sha) + " found; waiting for the server connection to deploy it."
				}
				return nil
			})
			continue
		}
		startAutoDeploy(slug)
		started = append(started, slug)
	}
	return started
}

// ---------- GitHub device flow (optional) ----------

// GitHubClientID is the public client ID of a GitHub OAuth app with device
// flow enabled. It is empty in this build (no such app is registered), so the
// UI offers a personal access token instead. Set with
// -ldflags "-X main.GitHubClientID=..." to enable "Sign in with GitHub".
var GitHubClientID = ""

// ---------- routes ----------

func registerImportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/import/detect", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ URL string }
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		ref, err := parseRepoURL(req.URL)
		if err != nil {
			writeErr(w, err)
			return
		}
		out := map[string]any{"ref": ref, "label": hostLabel(ref.Kind), "account_key": ref.AccountKey()}
		_, haveGit := exec.LookPath("git")
		out["git_available"] = haveGit == nil
		acc, _ := accountFor(ref)
		out["connected"] = acc != nil
		out["token_url"] = tokenCreateURL(ref.Kind, ref.Host, ref.Org)
		out["token_hint"] = tokenScopesHint(ref.Kind)
		switch ref.Kind {
		case "archive", "git", "netlify":
		default:
			g := newGitAPI(ref)
			def, derr := g.DefaultBranch(r.Context())
			if derr != nil {
				var ge *GitError
				if errors.As(derr, &ge) {
					out["needs_token"] = ge.NeedsToken
				}
				out["warning"] = derr.Error()
				break
			}
			out["default_branch"] = def
			if bs, berr := g.Branches(r.Context()); berr == nil {
				ref.resolveTree(bs)
				out["branches"] = bs
			}
			if ref.Branch == "" {
				ref.Branch = def
			}
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/import/fetch", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ URL, Branch, Subdir string }
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		ref, err := parseRepoURL(req.URL)
		if err != nil {
			writeErr(w, err)
			return
		}
		if ref.Kind == "netlify" {
			writeErr(w, errors.New("this is a Netlify site: use Import from Netlify"))
			return
		}
		if b := strings.TrimSpace(req.Branch); b != "" {
			ref.Branch = b
		}
		if strings.TrimSpace(req.Subdir) != "" || ref.TreePath == "" {
			ref.Subdir = strings.Trim(strings.TrimSpace(req.Subdir), "/")
		}
		j := newJob("import", func(j *Job) error {
			fr, err := fetchRepo(context.Background(), j, ref)
			if err != nil {
				var ge *GitError
				if errors.As(err, &ge) && ge.NeedsToken {
					j.SetResult(map[string]any{"needs_token": true, "account_key": ref.AccountKey()})
				}
				return err
			}
			s := stage(fr, req.URL)
			j.SetResult(stagedSummary(s))
			return nil
		})
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
	// Drag-and-drop / folder / zip / path on this PC: staged like an import so
	// it gets the same detect -> (build) -> (test) -> deploy flow.
	mux.HandleFunc("POST /api/import/upload", func(w http.ResponseWriter, r *http.Request) {
		ws, err := siteUpload(w, r)
		if err != nil {
			writeErr(w, err)
			return
		}
		name := ""
		switch {
		case ws.LocalPath != "":
			name = filepath.Base(strings.TrimRight(ws.LocalPath, `\/`))
			if strings.EqualFold(name, "dist") || strings.EqualFold(name, "build") || strings.EqualFold(name, "public") || strings.EqualFold(name, "out") {
				name = filepath.Base(filepath.Dir(strings.TrimRight(ws.LocalPath, `\/`)))
			}
		case r.MultipartForm != nil && len(r.MultipartForm.File["zip"]) == 1:
			name = r.MultipartForm.File["zip"][0].Filename
		case r.MultipartForm != nil && len(r.MultipartForm.Value["paths"]) > 0:
			name, _, _ = strings.Cut(strings.TrimLeft(r.MultipartForm.Value["paths"][0], "/"), "/")
		}
		name = strings.TrimSuffix(strings.TrimSuffix(name, ".zip"), ".ZIP")
		if slugify(name) == "" {
			name = "my-site"
		}
		st := stage(&FetchResult{WS: ws, Ref: &RepoRef{Kind: "upload", Site: name, Display: ws.Source}}, "")
		out := stagedSummary(st)
		out["local_path"] = ws.LocalPath
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET /api/import/netlify/sites", func(w http.ResponseWriter, r *http.Request) {
		tok, err := netlifyToken()
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error(), "needs_token": true, "token_url": tokenCreateURL("netlify", "", ""), "token_hint": tokenScopesHint("netlify")})
			return
		}
		sites, err := netlifySites(r.Context(), tok)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"sites": sites})
	})
	mux.HandleFunc("POST /api/import/netlify/fetch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SiteID string `json:"site_id"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		tok, err := netlifyToken()
		if err != nil {
			writeErr(w, err)
			return
		}
		j := newJob("netlify-import", func(j *Job) error {
			ctx := context.Background()
			sites, err := netlifySites(ctx, tok)
			if err != nil {
				return err
			}
			for _, s := range sites {
				if s.ID == req.SiteID || s.Name == req.SiteID {
					fr, err := importNetlifySite(ctx, j, tok, s)
					if err != nil {
						return err
					}
					st := stage(fr, "netlify:"+s.Name)
					j.SetResult(stagedSummary(st))
					return nil
				}
			}
			return errors.New("that Netlify site was not found in your account")
		})
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
	mux.HandleFunc("POST /api/import/{id}/build", func(w http.ResponseWriter, r *http.Request) {
		s, err := getStaged(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		j := newJob("build", func(j *Job) error {
			stagedMu.Lock()
			if s.Busy {
				stagedMu.Unlock()
				return errors.New("this import is busy")
			}
			s.Busy = true
			stagedMu.Unlock()
			defer func() { stagedMu.Lock(); s.Busy = false; s.At = time.Now(); stagedMu.Unlock() }()
			if err := runBuild(context.Background(), j, s.FR.WS); err != nil {
				return err
			}
			s.FR.Extras = mergeMap(s.FR.Extras, map[string]any{"built": true})
			j.SetResult(stagedSummary(s))
			return nil
		})
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
	// Test: App Testing gets its own copy so the staged files stay deployable.
	mux.HandleFunc("POST /api/import/{id}/test", func(w http.ResponseWriter, r *http.Request) {
		s, err := getStaged(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		entries, err := dirEntries(s.FR.WS.Root)
		if err != nil {
			writeErr(w, err)
			return
		}
		cp, err := newWorkspace(entries, s.FR.WS.Source)
		if err != nil {
			writeErr(w, err)
			return
		}
		t, err := startTest(cp, r.Host)
		if err != nil {
			cp.Close()
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, testState(t))
	})
	mux.HandleFunc("DELETE /api/import/{id}", func(w http.ResponseWriter, r *http.Request) {
		dropStaged(r.PathValue("id"))
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/import/{id}/deploy", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Slug        string `json:"slug"`
			Name        string `json:"name"`
			AutoDeploy  bool   `json:"auto_deploy"`
			TestFirst   bool   `json:"test_first"`
			IntervalMin int    `json:"interval_min"`
			AcceptFails bool   `json:"accept_fails"`
			Watch       bool   `json:"watch"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		s, err := getStaged(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		if !sshS.connected() {
			writeErr(w, errNotConnected)
			return
		}
		ws := s.FR.WS
		if planBuild(ws).Needed {
			writeJSON(w, 409, map[string]any{"error": "this project must be built first", "needs_build": true})
			return
		}
		if !ws.Files.IsFile("index.html") {
			writeErr(w, errors.New("there is no index.html to publish. Pick the right base directory, or build the project first"))
			return
		}
		slug, name := strings.TrimSpace(req.Slug), strings.TrimSpace(req.Name)
		if slug == "" {
			if name == "" {
				name = suggestName(s.FR.Ref)
			}
			slug = slugify(name)
			c := snapshotConfig()
			for i := 2; findSite(&c, slug) != nil; i++ {
				slug = fmt.Sprintf("%s-%d", slugify(name), i)
			}
		} else if _, err := siteOr404(slug); err != nil {
			writeErr(w, err)
			return
		}
		stagedMu.Lock()
		if s.Busy {
			stagedMu.Unlock()
			writeErr(w, errors.New("this import is busy"))
			return
		}
		s.Busy = true
		stagedMu.Unlock()
		j := newJob("import-deploy", func(j *Job) error {
			defer dropStaged(s.ID)
			if req.Slug == "" {
				j.Logf("Creating site %s ...", name)
				if err := createSiteJob(j, name, slug); err != nil {
					return err
				}
			}
			rep := runChecks(ws)
			j.Logf("Checks: %d passed, %d warnings, %d failed.", rep.Counts["pass"], rep.Counts["warn"], rep.Counts["fail"])
			if err := deployWorkspace(j, slug, ws); err != nil {
				return err
			}
			ref := s.FR.Ref
			if fi, err := os.Stat(ws.LocalPath); ref.Kind == "upload" && req.Watch && ws.LocalPath != "" && err == nil && fi.IsDir() {
				_ = withConfig(func(c *Config) error {
					if st := findSite(c, slug); st != nil {
						st.Watch, st.WatchPath = true, ws.LocalPath
					}
					return nil
				})
				j.Logf("Watching %s: changes are deployed automatically.", ws.LocalPath)
			}
			if ref.Kind != "netlify" && ref.Kind != "upload" {
				built, _ := s.FR.Extras["built"].(bool)
				_ = withConfig(func(c *Config) error {
					if st := findSite(c, slug); st != nil {
						iv := req.IntervalMin
						if iv <= 0 {
							iv = 5
						}
						st.Git = &SiteGit{URL: s.Origin, Kind: ref.Kind, Display: ref.Display, Branch: ref.Branch, Subdir: ref.Subdir, LastSHA: s.FR.SHA,
							AutoDeploy: req.AutoDeploy && ref.Kind != "archive" && ref.Kind != "git", IntervalMin: iv, TestFirst: req.TestFirst, Build: built,
							LastCheck: time.Now().UTC().Format(time.RFC3339)}
					}
					return nil
				})
			}
			res := map[string]any{"slug": slug}
			if d, ok := s.FR.Extras["custom_domain"]; ok {
				res["custom_domain"] = d
			}
			c := snapshotConfig()
			if st := findSite(&c, slug); st != nil {
				res["instant_url"] = st.InstantURL
			}
			j.SetResult(res)
			return nil
		})
		writeJSON(w, 200, map[string]any{"job": j.ID, "slug": slug})
	})

	// Site <-> repository
	mux.HandleFunc("POST /api/sites/{slug}/pull", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		var req struct {
			AllowBuild bool `json:"allow_build"`
		}
		_ = readJSON(r, &req)
		s, err := siteOr404(slug)
		if err != nil {
			writeErr(w, err)
			return
		}
		if s.Git == nil {
			writeErr(w, errors.New("this site is not linked to a repository"))
			return
		}
		if !sshS.connected() {
			writeErr(w, errNotConnected)
			return
		}
		if req.AllowBuild {
			_ = withConfig(func(c *Config) error {
				if st := findSite(c, slug); st != nil && st.Git != nil {
					st.Git.Build = true
				}
				return nil
			})
		}
		j := newJob("pull-deploy", func(j *Job) error { return pullAndDeployJob(j, slug, false) })
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
	mux.HandleFunc("POST /api/sites/{slug}/git", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		var req struct {
			AutoDeploy  *bool   `json:"auto_deploy"`
			TestFirst   *bool   `json:"test_first"`
			IntervalMin *int    `json:"interval_min"`
			Branch      *string `json:"branch"`
			Unlink      bool    `json:"unlink"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		err := withConfig(func(c *Config) error {
			st := findSite(c, slug)
			if st == nil || st.Git == nil {
				return errors.New("this site is not linked to a repository")
			}
			if req.Unlink {
				st.Git = nil
				return nil
			}
			if req.AutoDeploy != nil {
				if *req.AutoDeploy && (st.Git.Kind == "archive" || st.Git.Kind == "git") {
					return errors.New("auto-deploy needs a GitHub, GitLab, Bitbucket, Gitea or Azure DevOps repository")
				}
				st.Git.AutoDeploy = *req.AutoDeploy
			}
			if req.TestFirst != nil {
				st.Git.TestFirst = *req.TestFirst
			}
			if req.IntervalMin != nil {
				st.Git.IntervalMin = min(max(*req.IntervalMin, 1), 1440)
			}
			if req.Branch != nil && strings.TrimSpace(*req.Branch) != "" {
				st.Git.Branch, st.Git.LastSHA = strings.TrimSpace(*req.Branch), ""
			}
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// Accounts
	mux.HandleFunc("GET /api/git/accounts", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		type av struct {
			Key   string `json:"key"`
			Kind  string `json:"kind"`
			Host  string `json:"host"`
			Org   string `json:"org,omitempty"`
			User  string `json:"user,omitempty"`
			Login string `json:"login,omitempty"`
			Hint  string `json:"hint"`
		}
		var out []av
		for _, k := range sortedKeys(c.GitAccounts) {
			a := c.GitAccounts[k]
			out = append(out, av{k, a.Kind, a.Host, a.Org, a.User, a.Login, a.Hint})
		}
		links := map[string]map[string]string{}
		for _, k := range []string{"github", "gitlab", "bitbucket", "gitea", "azure", "netlify"} {
			links[k] = map[string]string{"label": hostLabel(k), "token_url": tokenCreateURL(k, map[string]string{"gitlab": "gitlab.com", "gitea": "codeberg.org"}[k], "{org}"), "hint": tokenScopesHint(k)}
		}
		writeJSON(w, 200, map[string]any{"accounts": out, "providers": links, "device_flow": GitHubClientID != ""})
	})
	mux.HandleFunc("POST /api/git/accounts", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Kind, Host, Org, User, Token string }
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		acc := &GitAccount{Kind: req.Kind, Host: strings.ToLower(strings.TrimSpace(req.Host)), Org: strings.TrimSpace(req.Org), User: strings.TrimSpace(req.User)}
		tok := strings.TrimSpace(req.Token)
		if tok == "" {
			writeErr(w, errors.New("paste the token"))
			return
		}
		switch acc.Kind {
		case "github":
			acc.Host = "github.com"
		case "bitbucket":
			acc.Host = "bitbucket.org"
			if acc.User == "" {
				writeErr(w, errors.New("Bitbucket API tokens need your Atlassian account email too"))
				return
			}
		case "azure":
			acc.Host = "dev.azure.com"
			if acc.Org == "" {
				writeErr(w, errors.New("enter your Azure DevOps organisation name"))
				return
			}
		case "netlify":
			acc.Host = "app.netlify.com"
		case "gitlab", "gitea":
			acc.Host = strings.TrimPrefix(strings.TrimPrefix(acc.Host, "https://"), "www.")
			acc.Host = strings.TrimRight(acc.Host, "/")
			if acc.Host == "" {
				acc.Host = map[string]string{"gitlab": "gitlab.com", "gitea": "codeberg.org"}[acc.Kind]
			}
			if strings.ContainsAny(acc.Host, "/ ") {
				writeErr(w, errors.New("enter only the host name, like gitlab.example.com"))
				return
			}
		default:
			writeErr(w, errors.New("unknown account type"))
			return
		}
		login, err := verifyAccount(r.Context(), acc, tok)
		if err != nil {
			writeErr(w, err)
			return
		}
		enc, err := protectString(tok)
		if err != nil {
			writeErr(w, err)
			return
		}
		acc.Token, acc.Hint, acc.Login = enc, keyHint(tok), login
		ref := &RepoRef{Kind: acc.Kind, Host: acc.Host, Org: acc.Org}
		key := ref.AccountKey()
		err = withConfig(func(c *Config) error {
			if c.GitAccounts == nil {
				c.GitAccounts = map[string]*GitAccount{}
			}
			c.GitAccounts[key] = acc
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "key": key, "login": login})
	})
	mux.HandleFunc("DELETE /api/git/accounts", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		_ = withConfig(func(c *Config) error { delete(c.GitAccounts, key); return nil })
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/git/repos", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		acc := c.GitAccounts[r.URL.Query().Get("key")]
		if acc == nil {
			writeErr(w, errors.New("connect the account first"))
			return
		}
		repos, err := listUserRepos(r.Context(), acc)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"repos": repos})
	})
}

func mergeMap(a, b map[string]any) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}
