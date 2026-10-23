package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memFS map[string]bool

func (m memFS) IsFile(rel string) bool { return m[rel] }

func fsOf(names ...string) memFS {
	m := memFS{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func rulesFrom(t *testing.T, redirects, headers, toml string) *SiteRules {
	t.Helper()
	s := &SiteRules{}
	if redirects != "" {
		s.parseRedirectsFile(redirects, "_redirects")
	}
	if toml != "" {
		s.parseNetlifyTOML(toml, "netlify.toml")
	}
	if headers != "" {
		s.parseHeadersFile(headers, "_headers")
	}
	return s
}

func issuesOf(s *SiteRules, level string) []string {
	var out []string
	for _, i := range s.Issues {
		if i.Level == level {
			out = append(out, i.Msg)
		}
	}
	return out
}

func TestParseRedirects(t *testing.T) {
	s := rulesFrom(t, `
# comment
/old            /new
/blog/*         /posts/:splat  302
/a/:id/:name    /b/:name?x=:id 301!
/app/*          /app/index.html 200
/gone           /404.html      410
/search  q=:q   /find/:q       301
/cond    /x  302  Country=us
/bad
/weird  /x  999
https://example.com/*  https://www.example.com/:splat 301!
/proxy  https://api.example.com/  200
/hack   /x";evil 301
`, "", "")
	if got := len(s.Redirects); got != 5 {
		for _, r := range s.Redirects {
			t.Log(r.From, r.To, r.Status)
		}
		t.Fatalf("want 5 rules, got %d; issues=%v", got, s.Issues)
	}
	r := s.Redirects[0]
	if r.Status != 301 || r.Force || r.Line != 3 {
		t.Errorf("default rule: %+v", r)
	}
	if s.Redirects[2].Status != 301 || !s.Redirects[2].Force {
		t.Errorf("forced rule not parsed: %+v", s.Redirects[2])
	}
	fails := strings.Join(issuesOf(s, "fail"), "\n")
	warns := strings.Join(issuesOf(s, "warn"), "\n")
	for _, want := range []string{"999", "/bad"} {
		if !strings.Contains(fails, want) {
			t.Errorf("expected a fail mentioning %q, got:\n%s", want, fails)
		}
	}
	if !strings.Contains(fails+warns, "evil") && !strings.Contains(fails+warns, "character") {
		t.Errorf("unsafe target not reported: %s", fails)
	}
	for _, want := range []string{"example.com", "q=", "Country", "api.example.com"} {
		if !strings.Contains(warns, want) {
			t.Errorf("expected a warning mentioning %q, got:\n%s", want, warns)
		}
	}
}

func TestMatchSplatsAndPlaceholders(t *testing.T) {
	s := rulesFrom(t, "/blog/*  /posts/:splat 301\n/u/:id/:tab /users/:tab?id=:id 302\n/exact /e 301\n", "", "")
	cases := []struct{ path, loc string }{
		{"/blog/2024/hello", "/posts/2024/hello"},
		{"/blog", "/posts/"},
		{"/blog/", "/posts/"},
		{"/u/42/settings", "/users/settings?id=42"},
		{"/exact/", "/e"},
		{"/exact", "/e"},
	}
	for _, c := range cases {
		res := s.resolve(fsOf("index.html"), c.path, "")
		if res.Location != c.loc {
			t.Errorf("%s: got %d %q (%s), want %q", c.path, res.Status, res.Location, res.Via, c.loc)
		}
	}
	if res := s.resolve(fsOf("index.html"), "/u/42", ""); res.Status != 404 {
		t.Errorf("/u/42 should not match /u/:id/:tab: %+v", res)
	}
	if res := s.resolve(fsOf("index.html"), "/Blog/x", ""); res.Status != 404 {
		t.Errorf("matching must be case-sensitive: %+v", res)
	}
	// Query strings are passed through unless the target sets its own.
	if res := s.resolve(fsOf(), "/blog/a", "ref=x"); res.Location != "/posts/a?ref=x" {
		t.Errorf("query passthrough: %q", res.Location)
	}
	if res := s.resolve(fsOf(), "/u/1/a", "ref=x"); res.Location != "/users/a?id=1" {
		t.Errorf("target query must win: %q", res.Location)
	}
}

func TestResolveSPAAndShadowing(t *testing.T) {
	fs := fsOf("index.html", "about.html", "docs/index.html", "assets/app.js", "404.html", "_redirects")
	s := rulesFrom(t, "/about /elsewhere 301\n/assets/* /cdn/:splat 301!\n/* /index.html 200\n", "", "")
	cases := []struct {
		path   string
		status int
		file   string
		loc    string
	}{
		{"/", 200, "index.html", ""},
		{"/about", 200, "about.html", ""},          // file shadows non-forced rule
		{"/about/", 301, "", "/about"},             // trailing slash removed (clean URL)
		{"/docs", 301, "", "/docs/"},               // folder gets a slash
		{"/docs/", 200, "docs/index.html", ""},     // folder index
		{"/assets/app.js", 301, "", "/cdn/app.js"}, // forced rule beats the file
		{"/dashboard/settings", 200, "index.html", ""},
		{"/_redirects", 404, "404.html", ""}, // never published
		{"/index.html", 200, "index.html", ""},
	}
	for _, c := range cases {
		res := s.resolve(fs, c.path, "")
		if res.Status != c.status || res.File != c.file || res.Location != c.loc {
			t.Errorf("%s: got %d file=%q loc=%q (%s); want %d %q %q", c.path, res.Status, res.File, res.Location, res.Via, c.status, c.file, c.loc)
		}
	}
}

func TestResolve404AndCustomStatus(t *testing.T) {
	fs := fsOf("index.html", "404.html", "gone.html")
	s := rulesFrom(t, "/old/*  /gone.html 410\n/old-* /x 301\n/missing /nothing.html 200\n", "", "")
	if r := s.resolve(fs, "/nope", ""); r.Status != 404 || r.File != "404.html" {
		t.Errorf("custom 404: %+v", r)
	}
	if r := s.resolve(fsOf("index.html"), "/nope", ""); r.Status != 404 || r.File != "" {
		t.Errorf("plain 404: %+v", r)
	}
	if r := s.resolve(fs, "/old/page", ""); r.Status != 410 || r.File != "gone.html" {
		t.Errorf("410 rule: %+v", r)
	}
	if len(issuesOf(s, "fail")) != 1 {
		t.Errorf("mid-segment splat must be reported: %+v", s.Issues)
	}
	if r := s.resolve(fs, "/missing", ""); r.Status != 404 || r.File != "404.html" {
		t.Errorf("rewrite to a missing file must 404: %+v", r)
	}
}

func TestHeadersRules(t *testing.T) {
	s := rulesFrom(t, "", `
/*
  X-Frame-Options: DENY
  Cache-Control: public, max-age=0
/assets/*
  Cache-Control: public, max-age=31536000, immutable
  Link: </a.css>; rel=preload
  Link: </b.js>; rel=preload
/u/:id/profile
  X-User: yes
/*.json
  Content-Type: application/json
bad line
/x
  Bad Header: 1
`, "")
	get := func(p, name string) string {
		for _, kv := range s.headersFor(p) {
			if strings.EqualFold(kv[0], name) {
				return kv[1]
			}
		}
		return ""
	}
	if v := get("/assets/x.css", "Cache-Control"); !strings.Contains(v, "immutable") {
		t.Errorf("later rule should win: %q", v)
	}
	if v := get("/index.html", "Cache-Control"); v != "public, max-age=0" {
		t.Errorf("root cache-control: %q", v)
	}
	if v := get("/assets/x", "Link"); v != "</a.css>; rel=preload, </b.js>; rel=preload" {
		t.Errorf("repeated header in one rule must be joined: %q", v)
	}
	if get("/u/7/profile", "X-User") != "yes" || get("/u/7/other", "X-User") != "" {
		t.Errorf("placeholder header path")
	}
	if get("/data/a.json", "Content-Type") == "" {
		t.Errorf("wildcard inside a segment")
	}
	if len(issuesOf(s, "fail")) == 0 {
		t.Errorf("bad lines must be reported: %+v", s.Issues)
	}
}

func TestNetlifyTOML(t *testing.T) {
	s := rulesFrom(t, "/first /one 301\n", "", `
[build]
  publish = "dist"
  command = "npm run build"

[[redirects]]
  from = "/api/*"
  to = "/.netlify/functions/:splat"
  status = 200

[[redirects]]
  from = "/home"
  to = "/"
  force = true

[[redirects]]
  from = "/geo"
  to = "/x"
  conditions = {Country = ["US"]}

[[headers]]
  for = "/*"
  [headers.values]
    X-Test = "1"
    Content-Security-Policy = "default-src 'self'; frame-ancestors 'none'"
`)
	if s.Publish != "dist" || s.BuildCommand != "npm run build" || !s.TomlFound {
		t.Errorf("build section: %+v", s)
	}
	if len(s.Redirects) < 3 || s.Redirects[0].From != "/first" {
		t.Fatalf("_redirects must come before netlify.toml: %+v", s.Redirects)
	}
	if !s.Redirects[2].Force || s.Redirects[2].Status != 301 {
		t.Errorf("force + default status: %+v", s.Redirects[2])
	}
	if len(s.headersFor("/")) != 2 {
		t.Errorf("toml headers: %v", s.headersFor("/"))
	}
	if !strings.Contains(strings.Join(issuesOf(s, "warn"), " "), "ondition") {
		t.Errorf("conditions should be warned about: %v", s.Issues)
	}
	bad := rulesFrom(t, "", "", "[[redirects]\nfrom=")
	if len(issuesOf(bad, "fail")) == 0 {
		t.Errorf("invalid toml must fail")
	}
}

func TestCleanURLPath(t *testing.T) {
	for _, bad := range []string{"/a/../b", "/a\\b", "/a\x00", "/.."} {
		if _, ok := cleanURLPath(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if p, ok := cleanURLPath("/a//b/"); !ok || p != "/a/b/" {
		t.Errorf("got %q", p)
	}
}

func TestInjectInto(t *testing.T) {
	cases := map[string]string{
		"<!doctype html><html><head><title>x</title></head></html>": "<head>" + injectTag,
		"<HTML lang=en><body>hi":                                    "<HTML lang=en>" + injectTag,
		"<!DOCTYPE html><p>hi":                                      "<!DOCTYPE html>" + injectTag,
		"plain":                                                     injectTag + "plain",
	}
	for in, want := range cases {
		if got := string(injectInto([]byte(in))); !strings.Contains(got, want) {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

// ---- workspace + checks ----

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadWS(t *testing.T, files map[string]string) *Workspace {
	t.Helper()
	entries, err := dirEntries(writeTree(t, files))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := newWorkspace(entries, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ws.Close)
	return ws
}

func findCheck(r *Report, id string) CheckResult {
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	return CheckResult{}
}

func itemsText(c CheckResult) string {
	var b strings.Builder
	for _, it := range c.Items {
		b.WriteString(it.File + ": " + it.Detail + "\n")
	}
	return b.String()
}

const goodHead = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>T</title><link rel="icon" href="/favicon.ico"></head>`

func TestLinkChecker(t *testing.T) {
	ws := loadWS(t, map[string]string{
		"index.html": goodHead + `<body>
<a href="about">About</a>
<a href="/about.html#team">Team</a>
<a href="/about#nope">bad anchor</a>
<a href="missing.html">Missing</a>
<a href="docs/">Docs</a>
<a href="#local">local</a><div id="local"></div>
<a href="mailto:a@b.c">mail</a> <a href="https://example.com/x">ext</a>
<img src="img/Logo.png">
<img src="img/photo.jpg" srcset="img/photo.jpg 1x, img/photo@2x.jpg 2x">
<img src="file:///C:/Users/me/site/pic.png">
<img src="C:\Users\me\pic.png">
<script src="http://cdn.example.com/lib.js"></script>
<img src="http://example.com/a.png">
<script src="http://localhost:3000/dev.js"></script>
<div style="background:url('img/bg.png')"></div>
<link rel="stylesheet" href="css/site.css">
</body></html>`,
		"about.html":      goodHead + `<body><h2 id="team">Team</h2><a href="index.html">home</a></body></html>`,
		"docs/index.html": goodHead + `<body><a href="../about">up</a><a href="./page">p</a><img src="../img/logo.png"></body></html>`,
		"img/logo.png":    "png",
		"img/photo.jpg":   "jpg",
		"css/site.css":    "body{background:url(../img/logo.png)} .x{background:url(\"../img/none.png\")} /* url(ignored.png) */\n@import 'other.css';",
		"favicon.ico":     "ico",
		".env":            "SECRET=1",
		"_redirects":      "/about /x 301\n",
	})
	rep := runChecks(ws)
	links := findCheck(rep, "links")
	assets := findCheck(rep, "assets")
	cased := findCheck(rep, "case")
	local := findCheck(rep, "localpaths")
	mixed := findCheck(rep, "mixed")
	frags := findCheck(rep, "fragments")
	t.Logf("links:\n%s\nassets:\n%s\ncase:\n%s\nlocal:\n%s\nmixed:\n%s", itemsText(links), itemsText(assets), itemsText(cased), itemsText(local), itemsText(mixed))

	expect := func(c CheckResult, status string, subs ...string) {
		t.Helper()
		if c.Status != status {
			t.Errorf("%s: status %s, want %s", c.ID, c.Status, status)
		}
		txt := itemsText(c)
		for _, s := range subs {
			if !strings.Contains(txt, s) {
				t.Errorf("%s: missing %q in\n%s", c.ID, s, txt)
			}
		}
	}
	expect(links, "fail", "missing.html", "docs/index.html: <a href=\"./page\">")
	if strings.Contains(itemsText(links), `"about"`) || strings.Contains(itemsText(links), "docs/\"") {
		t.Errorf("valid clean-URL links reported:\n%s", itemsText(links))
	}
	expect(assets, "fail", "photo@2x.jpg", "bg.png", "none.png", "other.css")
	if strings.Contains(itemsText(assets), "ignored.png") {
		t.Errorf("CSS comment was scanned")
	}
	expect(cased, "fail", "img/Logo.png")
	expect(local, "fail", "file:///", `C:\Users`, "localhost:3000")
	expect(mixed, "fail", "cdn.example.com", "example.com/a.png")
	expect(frags, "warn", "nope")
	if strings.Contains(itemsText(frags), "team") || strings.Contains(itemsText(frags), "local") {
		t.Errorf("valid anchors reported: %s", itemsText(frags))
	}
	if c := findCheck(rep, "secrets"); c.Status != "fail" {
		t.Errorf("secrets: %+v", c)
	}
	if c := findCheck(rep, "rules"); c.Status != "warn" || !strings.Contains(itemsText(c), "does nothing") {
		t.Errorf("shadowed rule not reported: %+v", c)
	}
	for _, id := range []string{"title", "viewport", "lang", "charset", "favicon", "index"} {
		if c := findCheck(rep, id); c.Status != "pass" {
			t.Errorf("%s should pass: %s", id, itemsText(c))
		}
	}
}

func TestChecksSPAAndMeta(t *testing.T) {
	ws2 := loadWS(t, map[string]string{
		"dist/index.html":          `<html><head></head><body><div id="root"></div><script type="module" src="/assets/index-abc.js"></script><script src="/assets/missing.js"></script></body></html>`,
		"dist/assets/index-abc.js": "console.log(1)",
		"dist/_redirects":          "/*  /index.html  200\n",
		"package.json":             `{"scripts":{"build":"vite build"},"dependencies":{"react":"18"},"devDependencies":{"vite":"5"}}`,
		"src/main.jsx":             "x",
	})
	if ws2.PublishRel != "dist" {
		t.Fatalf("publish dir: %q (%s)", ws2.PublishRel, ws2.PublishNote)
	}
	if ws2.Project.Framework != "React + Vite" {
		t.Errorf("framework: %q", ws2.Project.Framework)
	}
	rep := runChecks(ws2)
	a := findCheck(rep, "assets")
	if a.Status != "fail" || !strings.Contains(itemsText(a), "missing.js") || !strings.Contains(itemsText(a), "answers with index.html") {
		t.Errorf("missing asset hidden by SPA fallback not reported: %s", itemsText(a))
	}
	if c := findCheck(rep, "spa"); c.Status != "pass" {
		t.Errorf("spa: %+v", c)
	}
	for _, id := range []string{"title", "viewport", "lang", "charset", "favicon"} {
		if c := findCheck(rep, id); c.Status != "warn" {
			t.Errorf("%s should warn, got %s", id, c.Status)
		}
	}
	if c := findCheck(rep, "source"); c.Status == "fail" {
		t.Errorf("built output must not be flagged as source: %s", itemsText(c))
	}

	// Unbuilt Vite source at the top level.
	ws3 := loadWS(t, map[string]string{
		"index.html":   `<html><body><div id="root"></div><script type="module" src="/src/main.tsx"></script></body></html>`,
		"src/main.tsx": "x",
		"package.json": `{"scripts":{"build":"vite build"},"dependencies":{"react":"18","vite":"5"}}`,
	})
	rep = runChecks(ws3)
	if c := findCheck(rep, "source"); c.Status != "fail" {
		t.Errorf("unbuilt source not detected: %+v", c)
	}
	if c := findCheck(rep, "spa"); c.Status != "warn" {
		t.Errorf("missing SPA fallback not suggested: %+v", c)
	}
}

func TestTomlPublishAndHash(t *testing.T) {
	ws := loadWS(t, map[string]string{
		"netlify.toml":      "[build]\npublish = \"public\"\n[[redirects]]\nfrom=\"/old\"\nto=\"/new\"\n",
		"public/index.html": goodHead + "<body>hi</body></html>",
		"public/new.html":   "x",
		"index.html":        "root page that must not be served",
	})
	if ws.PublishRel != "public" || !strings.Contains(ws.PublishNote, "netlify.toml") {
		t.Fatalf("publish: %q %q", ws.PublishRel, ws.PublishNote)
	}
	if len(ws.Rules.Redirects) != 1 {
		t.Errorf("toml rules not loaded")
	}
	if err := ws.verifyUnchanged(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws.Publish, "new.html"), []byte("changed"), 0o644)
	if err := ws.verifyUnchanged(); err == nil {
		t.Errorf("changed files not detected")
	}
	bad := loadWS(t, map[string]string{"netlify.toml": "[build]\npublish=\"nope\"\n", "index.html": "x"})
	if bad.PublishRel != "" || len(bad.Issues) == 0 {
		t.Errorf("missing publish dir: %q %+v", bad.PublishRel, bad.Issues)
	}
}
