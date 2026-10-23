package main

// v0.3 mock tests: no real network or cloud calls. Every external API is an
// httptest server and the package-level base URLs are pointed at it.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, c := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, c)
	}
	zw.Close()
	return b.Bytes()
}

// isolate gives each test its own config dir and in-memory config.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("DILREET_CONFIG_DIR", t.TempDir())
	t.Setenv("DILREET_PROJECTS_DIR", t.TempDir())
	cfgMu.Lock()
	old := cfg
	cfg = &Config{SSHPort: 22, SSHUser: "root"}
	cfgMu.Unlock()
	t.Cleanup(func() { cfgMu.Lock(); cfg = old; cfgMu.Unlock() })
}

func TestParseRepoURL(t *testing.T) {
	oldProbe := hostProbe
	hostProbe = func(host string) string {
		switch host {
		case "git.example.com":
			return "gitlab"
		case "code.example.org":
			return "gitea"
		}
		return ""
	}
	defer func() { hostProbe = oldProbe }()
	cases := []struct {
		in                              string
		kind, host, owner, repo, branch string
		subdir, tree                    string
	}{
		{in: "https://github.com/vercel/next.js", kind: "github", host: "github.com", owner: "vercel", repo: "next.js"},
		{in: "github.com/a/b.git", kind: "github", host: "github.com", owner: "a", repo: "b"},
		{in: "git@github.com:a/b.git", kind: "github", host: "github.com", owner: "a", repo: "b"},
		{in: "https://github.com/a/b/tree/main/docs/site", kind: "github", owner: "a", repo: "b", tree: "main/docs/site"},
		{in: "https://gitlab.com/group/sub/proj", kind: "gitlab", host: "gitlab.com", owner: "group/sub", repo: "proj"},
		{in: "https://gitlab.com/g/p/-/tree/dev/web", kind: "gitlab", owner: "g", repo: "p", tree: "dev/web"},
		{in: "https://git.example.com/team/app", kind: "gitlab", host: "git.example.com", owner: "team", repo: "app"},
		{in: "https://bitbucket.org/ws/repo/src/main/", kind: "bitbucket", owner: "ws", repo: "repo"},
		{in: "https://codeberg.org/u/r", kind: "gitea", host: "codeberg.org", owner: "u", repo: "r"},
		{in: "https://code.example.org/u/r/src/branch/main", kind: "gitea", owner: "u", repo: "r"},
		{in: "https://dev.azure.com/org/proj/_git/repo", kind: "azure", repo: "repo"},
		{in: "https://example.com/files/site.zip", kind: "archive"},
		{in: "https://example.com/files/site.tar.gz", kind: "archive"},
		{in: "https://example.com/me/thing.git", kind: "git"},
		{in: "https://my-site.netlify.app", kind: "netlify"},
	}
	for _, c := range cases {
		r, err := parseRepoURL(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		chk := func(name, got, want string) {
			if want != "" && got != want {
				t.Errorf("%s: %s = %q, want %q", c.in, name, got, want)
			}
		}
		chk("kind", r.Kind, c.kind)
		chk("host", r.Host, c.host)
		chk("owner", r.Owner, c.owner)
		chk("repo", r.Repo, c.repo)
		chk("branch", r.Branch, c.branch)
		chk("subdir", r.Subdir, c.subdir)
		chk("tree", r.TreePath, c.tree)
	}
	for _, bad := range []string{"", "http://example.com/site.zip", "not a url", "ftp://x.org/a.zip", "file:///C:/x.zip"} {
		if r, err := parseRepoURL(bad); err == nil {
			t.Errorf("%q should be rejected, got %+v", bad, r)
		}
	}
	// /tree/ splitting once branches are known (branch names may contain "/").
	r, _ := parseRepoURL("https://github.com/a/b/tree/feature/x/site")
	r.resolveTree([]string{"main", "feature/x"})
	if r.Branch != "feature/x" || r.Subdir != "site" {
		t.Errorf("resolveTree: branch %q subdir %q", r.Branch, r.Subdir)
	}
}

// mockGitHosts serves the documented API paths of every host from one TLS server.
func mockGitHosts(t *testing.T, sha string, archive []byte, seen *sync.Map) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		seen.Store(p, r.Header.Clone())
		js := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
		switch {
		// GitHub (REST v3): repo, commit, codeload archive
		case p == "/gh/repos/o/r":
			js(map[string]any{"default_branch": "main"})
		case p == "/gh/repos/o/r/commits/main":
			js(map[string]any{"sha": sha})
		case p == "/gh/repos/o/private":
			http.NotFound(w, r)
		case p == "/codeload/o/r/zip/"+sha:
			w.Write(archive)
		// GitLab v4
		case p == "/api/v4/projects/o/r" || p == "/api/v4/projects/o%2Fr":
			js(map[string]any{"default_branch": "main"})
		case strings.HasPrefix(p, "/api/v4/projects/") && strings.HasSuffix(p, "/repository/commits/main"):
			js(map[string]any{"id": sha})
		case strings.HasPrefix(p, "/api/v4/projects/") && strings.HasSuffix(p, "/repository/archive.zip"):
			if r.URL.Query().Get("sha") != sha {
				http.Error(w, "bad sha", 400)
				return
			}
			w.Write(archive)
		// Gitea / Forgejo v1
		case p == "/api/v1/repos/o/r":
			js(map[string]any{"default_branch": "main"})
		case p == "/api/v1/repos/o/r/branches/main":
			js(map[string]any{"commit": map[string]any{"id": sha}})
		case p == "/o/r/archive/"+sha+".zip":
			w.Write(archive)
		// Bitbucket Cloud 2.0
		case p == "/bb/repositories/o/r":
			js(map[string]any{"mainbranch": map[string]any{"name": "main"}})
		case p == "/bb/repositories/o/r/commit/main":
			js(map[string]any{"hash": sha})
		case p == "/bbweb/o/r/get/"+sha+".zip":
			w.Write(archive)
		// Azure DevOps 7.1
		case p == "/az/org/proj/_apis/git/repositories/r":
			js(map[string]any{"defaultBranch": "refs/heads/main"})
		case p == "/az/org/proj/_apis/git/repositories/r/refs":
			js(map[string]any{"value": []any{map[string]any{"name": "refs/heads/main", "objectId": sha}}})
		case p == "/az/org/proj/_apis/git/repositories/r/items":
			if r.URL.Query().Get("$format") != "zip" || r.URL.Query().Get("versionDescriptor.versionType") != "commit" {
				http.Error(w, "bad query", 400)
				return
			}
			// Azure archives have no wrapper folder.
			w.Write(zipBytes(t, map[string]string{"site/index.html": "<h1>az</h1>", "README.md": "x"}))
		default:
			http.Error(w, "unexpected "+p, 418)
		}
	}))
	t.Cleanup(srv.Close)
	oldHTTP := gitHTTP
	gitHTTP = srv.Client()
	gitHTTP.CheckRedirect = httpsOnlyRedirect
	saved := []string{githubAPI, githubCodeload, bitbucketAPI, bitbucketWeb, azureBase}
	githubAPI, githubCodeload, bitbucketAPI, bitbucketWeb, azureBase = srv.URL+"/gh", srv.URL+"/codeload", srv.URL+"/bb", srv.URL+"/bbweb", srv.URL+"/az"
	t.Cleanup(func() {
		gitHTTP = oldHTTP
		githubAPI, githubCodeload, bitbucketAPI, bitbucketWeb, azureBase = saved[0], saved[1], saved[2], saved[3], saved[4]
	})
	return srv
}

func TestFetchPerHostArchive(t *testing.T) {
	isolate(t)
	sha := strings.Repeat("a1", 20)
	archive := zipBytes(t, map[string]string{
		"r-" + sha[:7] + "/index.html":      "<h1>root</h1>",
		"r-" + sha[:7] + "/site/index.html": "<h1>sub</h1>",
		"r-" + sha[:7] + "/README.md":       "readme",
	})
	var seen sync.Map
	srv := mockGitHosts(t, sha, archive, &seen)
	host := strings.TrimPrefix(srv.URL, "https://")
	refs := []*RepoRef{
		{Kind: "github", Host: "github.com", Owner: "o", Repo: "r", Display: "o/r"},
		{Kind: "gitlab", Host: host, Owner: "o", Repo: "r", Display: "o/r"},
		{Kind: "gitea", Host: host, Owner: "o", Repo: "r", Display: "o/r"},
		{Kind: "bitbucket", Host: "bitbucket.org", Owner: "o", Repo: "r", Display: "o/r"},
		{Kind: "azure", Host: "dev.azure.com", Org: "org", Project: "proj", Repo: "r", Display: "org/proj/r", Subdir: "site"},
		{Kind: "github", Host: "github.com", Owner: "o", Repo: "r", Display: "o/r", Subdir: "site"},
	}
	for _, ref := range refs {
		res, err := fetchRepo(context.Background(), nil, ref)
		if err != nil {
			t.Fatalf("%s %s: %v", ref.Kind, ref.Subdir, err)
		}
		if res.SHA != sha {
			t.Errorf("%s: sha %q", ref.Kind, res.SHA)
		}
		if ref.Branch != "main" {
			t.Errorf("%s: default branch %q", ref.Kind, ref.Branch)
		}
		b, err := os.ReadFile(filepath.Join(res.WS.Publish, "index.html"))
		if err != nil {
			t.Fatalf("%s: %v", ref.Kind, err)
		}
		want := "<h1>root</h1>"
		if ref.Subdir == "site" {
			want = "<h1>sub</h1>"
			if ref.Kind == "azure" {
				want = "<h1>az</h1>"
			}
		}
		if string(b) != want {
			t.Errorf("%s/%s: index.html = %q, want %q", ref.Kind, ref.Subdir, b, want)
		}
		res.WS.Close()
	}
	// Private repo without a token: friendly "connect your account" error.
	_, err := fetchRepo(context.Background(), nil, &RepoRef{Kind: "github", Host: "github.com", Owner: "o", Repo: "private", Display: "o/private"})
	var ge *GitError
	if !errors.As(err, &ge) || !ge.NeedsToken {
		t.Fatalf("private repo: want NeedsToken error, got %v", err)
	}
}

func TestFetchUsesTokenHeaders(t *testing.T) {
	isolate(t)
	sha := strings.Repeat("b2", 20)
	var seen sync.Map
	mockGitHosts(t, sha, zipBytes(t, map[string]string{"w/index.html": "x"}), &seen)
	tok, err := protectString("glpat-SECRET")
	if err != nil {
		t.Fatal(err)
	}
	withConfig(func(c *Config) error {
		c.GitAccounts = map[string]*GitAccount{"bitbucket.org": {Kind: "bitbucket", Host: "bitbucket.org", User: "me@example.com", Token: tok}}
		return nil
	})
	res, err := fetchRepo(context.Background(), nil, &RepoRef{Kind: "bitbucket", Host: "bitbucket.org", Owner: "o", Repo: "r", Display: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	res.WS.Close()
	h, _ := seen.Load("/bb/repositories/o/r")
	if a := h.(http.Header).Get("Authorization"); !strings.HasPrefix(a, "Basic ") {
		t.Errorf("bitbucket API auth header = %q", a)
	}
}

func TestDownloadHTTPSOnlyAndLimit(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "f")
	if err := downloadFile(context.Background(), "http://example.com/a.zip", dst, 1<<20, nil, nil); err == nil {
		t.Fatal("plain http download must be refused")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://example.com/x.zip", http.StatusFound)
			return
		}
		w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer srv.Close()
	old := gitHTTP
	gitHTTP = srv.Client()
	gitHTTP.CheckRedirect = httpsOnlyRedirect
	defer func() { gitHTTP = old }()
	if err := downloadFile(context.Background(), srv.URL+"/big", dst, 1024, nil, nil); err == nil {
		t.Error("size limit not enforced")
	}
	if err := downloadFile(context.Background(), srv.URL+"/redirect", dst, 1<<20, nil, nil); err == nil {
		t.Error("redirect to http must be refused")
	}
}

func TestUntarGzZipSlip(t *testing.T) {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, n := range []string{"ok/index.html", "../../evil.txt"} {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
		tw.Write([]byte("hi"))
	}
	tw.WriteHeader(&tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	tw.Close()
	gz.Close()
	dir := t.TempDir()
	f := filepath.Join(dir, "a.tar.gz")
	os.WriteFile(f, b.Bytes(), 0o600)
	out := filepath.Join(dir, "out")
	os.MkdirAll(out, 0o700)
	if err := untarGz(f, out); err == nil {
		t.Error("tar with ../ entry must be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
		t.Error("zip-slip file was written outside the target")
	}
	if _, err := os.Lstat(filepath.Join(out, "link")); err == nil {
		t.Error("symlinks must never be extracted")
	}
}

func TestSelectSubdir(t *testing.T) {
	mk := func(p ...string) []srcEntry {
		var e []srcEntry
		for _, x := range p {
			e = append(e, srcEntry{Path: x})
		}
		return e
	}
	got, err := selectSubdir(mk("repo-main/a.txt", "repo-main/web/index.html", "repo-main/web/css/x.css"), "web", true)
	if err != nil || len(got) != 2 || got[0].Path != "index.html" {
		t.Fatalf("selectSubdir: %v %+v", err, got)
	}
	if _, err := selectSubdir(mk("repo-main/a.txt"), "nope", true); err == nil {
		t.Error("missing subdir must be an error")
	}
	if _, err := selectSubdir(mk("repo-main/a.txt"), "../etc", true); err == nil {
		t.Error("subdir escaping the repo must be an error")
	}
}

func TestCommitPollRedeploy(t *testing.T) {
	isolate(t)
	oldL, oldS, oldR := latestCommitFn, startAutoDeploy, deployReady
	defer func() { latestCommitFn, startAutoDeploy, deployReady = oldL, oldS, oldR }()
	head := map[string]string{"a": "sha2", "b": "sha1", "c": "sha9", "d": "sha3"}
	var calls []string
	latestCommitFn = func(ctx context.Context, s *Site) (string, error) {
		calls = append(calls, s.Slug)
		return head[s.Slug], nil
	}
	var deployed []string
	startAutoDeploy = func(slug string) { deployed = append(deployed, slug) }
	ready := true
	deployReady = func() bool { return ready }
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	withConfig(func(c *Config) error {
		c.Sites = []*Site{
			{Slug: "a", Git: &SiteGit{Kind: "github", AutoDeploy: true, LastSHA: "sha1", IntervalMin: 5}},                                         // new commit -> deploy
			{Slug: "b", Git: &SiteGit{Kind: "github", AutoDeploy: true, LastSHA: "sha1"}},                                                         // unchanged
			{Slug: "c", Git: &SiteGit{Kind: "github", AutoDeploy: false, LastSHA: "sha1"}},                                                        // off
			{Slug: "d", Git: &SiteGit{Kind: "gitlab", AutoDeploy: true, LastSHA: "x", LastCheck: now.Add(-2 * time.Minute).Format(time.RFC3339)}}, // too soon
			{Slug: "e", Git: &SiteGit{Kind: "archive", AutoDeploy: true}},                                                                         // no history
			{Slug: "f"},
		}
		return nil
	})
	got := commitPollTick(context.Background(), now)
	if fmt.Sprint(got) != "[a]" || fmt.Sprint(deployed) != "[a]" || fmt.Sprint(calls) != "[a b]" {
		t.Fatalf("started %v deployed %v polled %v", got, deployed, calls)
	}
	// Second tick right after: interval not elapsed -> nothing polled.
	calls = nil
	commitPollTick(context.Background(), now.Add(time.Minute))
	if len(calls) != 0 {
		t.Fatalf("polled too early: %v", calls)
	}
	// A running pull blocks another one.
	pullMu.Lock()
	pullRunning["a"] = true
	pullMu.Unlock()
	deployed = nil
	commitPollTick(context.Background(), now.Add(6*time.Minute))
	pullMu.Lock()
	delete(pullRunning, "a")
	pullMu.Unlock()
	if strings.Contains(fmt.Sprint(deployed), "a") {
		t.Fatalf("deployed while a pull was running: %v", deployed)
	}
	// Disconnected: record it instead of deploying.
	ready = false
	commitPollTick(context.Background(), now.Add(12*time.Minute))
	c := snapshotConfig()
	if s := findSite(&c, "a"); !strings.Contains(s.Git.LastError, "waiting for the server") {
		t.Fatalf("LastError = %q", s.Git.LastError)
	}
	// Errors are recorded too.
	ready = true
	latestCommitFn = func(context.Context, *Site) (string, error) { return "", errors.New("rate limited") }
	deployed = nil
	commitPollTick(context.Background(), now.Add(20*time.Minute))
	c = snapshotConfig()
	if s := findSite(&c, "a"); s.Git.LastError != "rate limited" || len(deployed) != 0 {
		t.Fatalf("error not recorded: %q %v", s.Git.LastError, deployed)
	}
}

func TestNetlifyImport(t *testing.T) {
	files := map[string]string{"/index.html": "<h1>n</h1>", "/_redirects": "/old /new 301", "/assets/a.css": "body{}"}
	var auth sync.Map
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"), true)
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch {
		case p == "/sites":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "s1", "name": "blog", "custom_domain": "blog.example.com", "published_deploy": map[string]any{"id": "d1"}}})
		case p == "/deploys/d1/files":
			var out []map[string]any
			for k, v := range files {
				out = append(out, map[string]any{"id": k, "path": k, "size": len(v)})
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(p, "/deploys/d1/files/"):
			if r.Header.Get("Content-Type") != "application/vnd.bitballoon.v1.raw" {
				http.Error(w, "need raw", 400)
				return
			}
			c, ok := files[strings.TrimPrefix(p, "/deploys/d1/files")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, c)
		default:
			http.Error(w, "unexpected "+p, 418)
		}
	}))
	defer srv.Close()
	oldAPI, oldHTTP := netlifyAPI, gitHTTP
	netlifyAPI, gitHTTP = srv.URL+"/api/v1", srv.Client()
	defer func() { netlifyAPI, gitHTTP = oldAPI, oldHTTP }()
	sites, err := netlifySites(context.Background(), "nfp_tok")
	if err != nil || len(sites) != 1 {
		t.Fatalf("sites: %v %v", sites, err)
	}
	res, err := importNetlifySite(context.Background(), nil, "nfp_tok", sites[0])
	if err != nil {
		t.Fatal(err)
	}
	defer res.WS.Close()
	if res.Extras["custom_domain"] != "blog.example.com" {
		t.Errorf("custom domain not carried: %v", res.Extras)
	}
	if fmt.Sprint(res.Extras["carried_rules"]) != "[_redirects]" {
		t.Errorf("carried rules: %v", res.Extras["carried_rules"])
	}
	if _, ok := auth.Load("Bearer nfp_tok"); !ok {
		t.Error("bearer token not sent")
	}
	// Bad token -> friendly message.
	netlifyAPI = srv.URL + "/nope"
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv2.Close()
	netlifyAPI, gitHTTP = srv2.URL, srv2.Client()
	if _, err := netlifySites(context.Background(), "bad"); err == nil || !strings.Contains(err.Error(), "refused the access token") {
		t.Errorf("401: %v", err)
	}
}

func TestCloudPlans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"code":"unauthorized","message":"unable to authenticate"}}`)
			return
		}
		switch r.URL.Path {
		case "/hz/server_types":
			io.WriteString(w, `{"server_types":[
			 {"name":"cx22","cores":2,"memory":4,"disk":40,"architecture":"x86","prices":[{"location":"fsn1","price_monthly":{"gross":"4.5100"}}],"locations":[{"name":"fsn1","deprecation":null}]},
			 {"name":"cax11","cores":2,"memory":4,"disk":40,"architecture":"arm","prices":[{"location":"fsn1","price_monthly":{"gross":"3.9000"}}],"locations":[{"name":"fsn1"}]},
			 {"name":"cx11","cores":1,"memory":2,"disk":20,"architecture":"x86","prices":[],"locations":[{"name":"fsn1"}]},
			 {"name":"cpx31","cores":4,"memory":8,"disk":160,"architecture":"x86","prices":[{"location":"nbg1","price_monthly":{"gross":"15.00"}}],"locations":[{"name":"nbg1"}]}]}`)
		case "/do/sizes":
			io.WriteString(w, `{"sizes":[
			 {"slug":"s-1vcpu-1gb","memory":1024,"vcpus":1,"disk":25,"price_monthly":6,"regions":["fra1"],"available":true},
			 {"slug":"s-2vcpu-2gb","memory":2048,"vcpus":2,"disk":60,"price_monthly":18,"regions":["fra1"],"available":true},
			 {"slug":"s-2vcpu-4gb","memory":4096,"vcpus":2,"disk":80,"price_monthly":24,"regions":["fra1"],"available":false}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oh, od := hetznerAPI, doAPI
	hetznerAPI, doAPI = srv.URL+"/hz", srv.URL+"/do"
	defer func() { hetznerAPI, doAPI = oh, od }()
	hz, err := cloudProvider{kind: "hetzner", token: "tok"}.Plans(context.Background())
	if err != nil || len(hz) != 2 || hz[0].ID != "cx22" || hz[0].Monthly != 4.51 {
		t.Fatalf("hetzner plans %+v %v", hz, err)
	}
	do, err := cloudProvider{kind: "digitalocean", token: "tok"}.Plans(context.Background())
	if err != nil || len(do) != 1 || do[0].ID != "s-2vcpu-2gb" || do[0].MemoryGB != 2 {
		t.Fatalf("do plans %+v %v", do, err)
	}
	if _, err := (cloudProvider{kind: "hetzner", token: "bad"}).Plans(context.Background()); err == nil {
		t.Fatal("bad token must fail")
	}
}

func TestDetectDNSHost(t *testing.T) {
	old := lookupNS
	defer func() { lookupNS = old }()
	var asked string
	lookupNS = func(ctx context.Context, d string) ([]*net.NS, error) {
		asked = d
		switch d {
		case "example.co.uk":
			return []*net.NS{{Host: "ada.ns.cloudflare.com."}}, nil
		case "shop.com":
			return []*net.NS{{Host: "ns07.domaincontrol.com."}}, nil
		}
		return nil, errors.New("nxdomain")
	}
	if h := detectDNSHost("www.example.co.uk"); h.ID != "cloudflare" || !h.Auto || asked != "example.co.uk" {
		t.Errorf("cloudflare: %+v (asked %s)", h, asked)
	}
	if h := detectDNSHost("shop.com"); h.ID != "godaddy" || h.Auto {
		t.Errorf("godaddy: %+v", h)
	}
	if h := detectDNSHost("nothing.dev"); h.ID != "unknown" {
		t.Errorf("unknown: %+v", h)
	}
}

func TestRedact(t *testing.T) {
	in := "token sk-ant-abc123456789012345678901 key ghp_abcdefghijklmnopqrstuvwxyz0123456789 server 203.0.113.7 pass=hunter2"
	out := redact(in, true)
	for _, leak := range []string{"sk-ant-abc", "ghp_abc", "203.0.113.7"} {
		if strings.Contains(out, leak) {
			t.Errorf("redact leaked %q: %s", leak, out)
		}
	}
	if strings.Contains(redact("ip 203.0.113.7", false), "203.0.113.7") == false {
		t.Error("ips=false should keep IPs")
	}
}

func TestParseTextActions(t *testing.T) {
	txt := "Sure.\n```action\n{\"name\":\"pull_and_deploy\",\"args\":{\"site\":\"blog\"}}\n```\nand junk ```action\n{bad json\n```"
	acts := parseTextActions(txt)
	if len(acts) != 1 || acts[0].Name != "pull_and_deploy" || acts[0].Args["site"] != "blog" {
		t.Fatalf("actions %+v", acts)
	}
	if len(parseTextActions(`{"name":"rm_rf","args":{}}`)) != 0 {
		// unknown tools must never become actions
		t.Error("unknown tool accepted")
	}
}

func sse(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, l := range lines {
		io.WriteString(w, l+"\n\n")
	}
}

func collect(t *testing.T, a Adapter, tools bool) (string, []ToolCall, error) {
	req := ChatRequest{Model: "m", System: "sys", Messages: []ChatMsg{{Role: "user", Content: "hi"}}}
	if tools {
		req.Tools = []ToolDef{*toolByName("pull_and_deploy")}
	}
	var text strings.Builder
	var calls []ToolCall
	err := a.Stream(context.Background(), req, func(e StreamEvent) {
		text.WriteString(e.Text)
		if e.Tool != nil {
			calls = append(calls, *e.Tool)
		}
	})
	return text.String(), calls, err
}

func TestAIAdaptersMock(t *testing.T) {
	var lastBody sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody.Store(r.URL.Path, string(b))
		switch {
		case r.URL.Path == "/oa/models":
			io.WriteString(w, `{"data":[{"id":"gpt-b"},{"id":"gpt-a"}]}`)
		case r.URL.Path == "/oa/chat/completions":
			if r.Header.Get("Authorization") != "Bearer k" {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":{"message":"Incorrect API key"}}`)
				return
			}
			sse(w, `data: {"choices":[{"delta":{"content":"Hel"}}]}`, `data: {"choices":[{"delta":{"content":"lo"}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"pull_and_deploy","arguments":"{\"site\":"}}]}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"blog\"}"}}]}}]}`, `data: [DONE]`)
		case r.URL.Path == "/an/messages":
			if r.Header.Get("x-api-key") != "k" {
				w.WriteHeader(401)
				return
			}
			sse(w, `event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"pull_and_deploy"}}`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"site\":\"blog\"}"}}`,
				`data: {"type":"content_block_stop","index":1}`, `data: {"type":"message_stop"}`)
		case r.URL.Path == "/an-busy/messages":
			sse(w, `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		case strings.HasPrefix(r.URL.Path, "/ge/models/m:streamGenerateContent"):
			if r.Header.Get("x-goog-api-key") != "k" {
				w.WriteHeader(400)
				io.WriteString(w, `[{"error":{"code":400,"message":"API key not valid. Please pass a valid API key."}}]`)
				return
			}
			sse(w, `data: {"candidates":[{"content":{"parts":[{"text":"thinking","thought":true},{"text":"Hello"}]}}]}`,
				`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"pull_and_deploy","args":{"site":"blog"}}}]}}]}`)
		case r.URL.Path == "/quota/chat/completions":
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	adapters := map[string]Adapter{
		"openai":    &openAIAdapter{name: "OpenAI", base: srv.URL + "/oa", key: "k"},
		"anthropic": &anthropicAdapter{base: srv.URL + "/an", key: "k"},
		"gemini":    &geminiAdapter{base: srv.URL + "/ge", key: "k"},
	}
	for name, a := range adapters {
		text, calls, err := collect(t, a, true)
		if err != nil || text != "Hello" || len(calls) != 1 || calls[0].Name != "pull_and_deploy" || calls[0].Args["site"] != "blog" {
			t.Errorf("%s: text %q calls %+v err %v", name, text, calls, err)
		}
	}
	if !strings.Contains(fmt.Sprint(lastBody.Load("/an/messages")), `"input_schema"`) {
		t.Error("anthropic tools not sent as input_schema")
	}
	if m, err := adapters["openai"].Models(context.Background()); err != nil || fmt.Sprint(m) != "[gpt-a gpt-b]" {
		t.Errorf("models %v %v", m, err)
	}
	kinds := map[string]Adapter{
		"auth":       &openAIAdapter{name: "OpenAI", base: srv.URL + "/oa", key: "wrong"},
		"overloaded": &anthropicAdapter{base: srv.URL + "/an-busy", key: "k"},
		"rate":       &openAIAdapter{name: "OpenAI", base: srv.URL + "/quota", key: "k"},
	}
	for want, a := range kinds {
		_, _, err := collect(t, a, false)
		var ae *AIError
		if !errors.As(err, &ae) || (ae.Kind != want && !(want == "rate" && ae.Kind == "quota")) {
			t.Errorf("%s: got %v", want, err)
		}
		if err != nil && strings.Contains(err.Error(), "wrong") {
			t.Error("error message echoes the key")
		}
	}
	_, _, err := collect(t, &geminiAdapter{base: srv.URL + "/ge", key: "bad"}, false)
	var ae *AIError
	if !errors.As(err, &ae) || ae.Kind != "auth" {
		t.Errorf("gemini bad key: %v", err)
	}
	// Unreachable endpoint -> network error, not a crash.
	_, _, err = collect(t, &openAIAdapter{name: "Local", base: "http://127.0.0.1:1"}, false)
	if !errors.As(err, &ae) || ae.Kind != "network" {
		t.Errorf("network: %v", err)
	}
}

func TestCloudflareUpsertConflict(t *testing.T) {
	var created int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/dns_records"):
			if r.URL.Query().Get("name") == "www.example.com" {
				io.WriteString(w, `{"success":true,"result":[{"id":"r1","type":"CNAME","name":"www.example.com","content":"old.host"}]}`)
				return
			}
			io.WriteString(w, `{"success":true,"result":[]}`)
		case r.Method == "POST":
			created++
			io.WriteString(w, `{"success":true,"result":{"id":"n1","type":"A","name":"example.com","content":"203.0.113.7"}}`)
		default:
			io.WriteString(w, `{"success":true,"result":{}}`)
		}
	}))
	defer srv.Close()
	old := cfBase
	cfBase = srv.URL
	defer func() { cfBase = old }()
	c := newCF("t")
	if id, _, err := c.UpsertA("z", "example.com", "203.0.113.7", false); err != nil || id != "n1" || created != 1 {
		t.Fatalf("create: %s %v", id, err)
	}
	_, conf, err := c.UpsertA("z", "www.example.com", "203.0.113.7", false)
	if !errors.Is(err, errCFConflict) || len(conf) != 1 {
		t.Fatalf("conflict not reported: %v %v", conf, err)
	}
}
