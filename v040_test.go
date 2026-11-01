package main

// Dilreet 0.4.0 tests: organisation, migration, auto-fix, templates,
// dashboard, logs, backups and health. No real network calls.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func orgMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerOrgRoutes(mux)
	registerTemplateRoutes(mux)
	registerBackupRoutes(mux)
	registerDashboardRoutes(mux)
	registerHealthRoutes(mux)
	return mux
}

func call(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestMigrateV03Config(t *testing.T) {
	isolate(t)
	dir := os.Getenv("DILREET_CONFIG_DIR")
	old := `{"server_ip":"1.2.3.4","ssh_port":22,"sites":[{"slug":"shop","name":"Shop","tags":["Food","food"," "],"group":"gone"},{"slug":"blog","name":"Blog"}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	c := snapshotConfig()
	if c.ConfigVersion != currentConfigVersion || c.MigratedFrom != "0.3" {
		t.Fatalf("version %d from %q", c.ConfigVersion, c.MigratedFrom)
	}
	if c.Backup == nil || c.Backup.Every != "weekly" {
		t.Fatalf("existing users should get weekly backups: %+v", c.Backup)
	}
	s := findSite(&c, "shop")
	if s.Group != "" || len(s.Tags) != 1 || s.Tags[0] != "food" {
		t.Fatalf("not normalised: group=%q tags=%v", s.Group, s.Tags)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if !strings.Contains(string(b), `"config_version": 4`) {
		t.Fatal("upgraded config was not saved")
	}
	// second load is a no-op and keeps data
	if err := loadConfig(); err != nil || len(snapshotConfig().Sites) != 2 {
		t.Fatal("reload lost data")
	}
}

func TestSnapshotCopiesOrg(t *testing.T) {
	isolate(t)
	_ = withConfig(func(c *Config) error {
		c.Sites = []*Site{{Slug: "a", Name: "A", Tags: []string{"x"}}}
		c.Groups = []*Group{{ID: "g", Name: "G", Color: "blue"}}
		c.TagColors = map[string]string{"x": "red"}
		return nil
	})
	s := snapshotConfig()
	s.Sites[0].Tags[0] = "changed"
	s.Groups[0].Name = "changed"
	s.TagColors["x"] = "changed"
	c := snapshotConfig()
	if c.Sites[0].Tags[0] != "x" || c.Groups[0].Name != "G" || c.TagColors["x"] != "red" {
		t.Fatal("snapshot shares memory with the live config")
	}
}

func TestGroupsTagsAndBulk(t *testing.T) {
	isolate(t)
	_ = withConfig(func(c *Config) error {
		c.Sites = []*Site{{Slug: "a", Name: "A"}, {Slug: "b", Name: "B"}, {Slug: "c", Name: "C"}}
		return nil
	})
	h := orgMux()
	code, out := call(t, h, "POST", "/api/org/groups", map[string]any{"name": "Clients", "color": "green"})
	if code != 200 || out["id"] != "clients" {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := call(t, h, "POST", "/api/org/groups", map[string]any{"name": "clients"}); code == 200 {
		t.Fatal("duplicate group name accepted")
	}
	if code, out := call(t, h, "POST", "/api/bulk", map[string]any{"action": "group", "value": "clients", "slugs": []string{"a", "b"}}); code != 200 || out["changed"].(float64) != 2 {
		t.Fatalf("%d %v", code, out)
	}
	call(t, h, "POST", "/api/bulk", map[string]any{"action": "tag", "value": "Urgent!", "slugs": []string{"a", "c"}})
	call(t, h, "POST", "/api/bulk", map[string]any{"action": "pin", "slugs": []string{"c"}})
	call(t, h, "POST", "/api/bulk", map[string]any{"action": "archive", "slugs": []string{"c"}})
	if code, _ := call(t, h, "POST", "/api/bulk", map[string]any{"action": "group", "value": "nope", "slugs": []string{"a"}}); code == 200 {
		t.Fatal("unknown group accepted")
	}
	c := snapshotConfig()
	if findSite(&c, "a").Group != "clients" || strings.Join(findSite(&c, "a").Tags, ",") != "urgent" {
		t.Fatalf("a: %+v", findSite(&c, "a"))
	}
	if cs := findSite(&c, "c"); !cs.Archived || cs.Pinned {
		t.Fatal("archiving should unpin")
	}
	_, org := call(t, h, "GET", "/api/org", nil)
	g := org["groups"].([]any)[0].(map[string]any)
	if g["count"].(float64) != 2 {
		t.Fatalf("group count %v", g)
	}
	// rename tag, then delete group: sites become ungrouped
	call(t, h, "POST", "/api/org/tags", map[string]any{"name": "urgent", "rename": "today", "color": "red"})
	call(t, h, "POST", "/api/org/groups", map[string]any{"id": "clients", "delete": true})
	c = snapshotConfig()
	if findSite(&c, "a").Group != "" || findSite(&c, "a").Tags[0] != "today" || c.TagColors["today"] != "red" {
		t.Fatalf("after rename/delete: %+v %v", findSite(&c, "a"), c.TagColors)
	}
	// meta endpoint
	notes := "call client on monday"
	mux := http.NewServeMux()
	registerOrgRoutes(mux)
	if code, _ := call(t, mux, "POST", "/api/sites/b/meta", map[string]any{"notes": notes, "pinned": true}); code != 200 {
		t.Fatal(code)
	}
	c = snapshotConfig()
	if b := findSite(&c, "b"); b.Notes != notes || !b.Pinned {
		t.Fatalf("%+v", b)
	}
}

func TestCleanTags(t *testing.T) {
	got := cleanTags([]string{"  Client <b>", "client b", "पुणे", "", "a-very-long-tag-name-that-goes-on-and-on"})
	want := "a-very-long-tag-name-tha,client b,पुणे"
	if strings.Join(got, ",") != want {
		t.Fatalf("%q", got)
	}
}

func TestAutoFixHomePage(t *testing.T) {
	cases := map[string]map[string]string{
		"Index.HTML":  {"Index.HTML": "<h1>x</h1>", "a.css": "x"},
		"home.html":   {"home.html": "<h1>x</h1>", "about.html": "y"},
		"single page": {"mysite.html": "<h1>x</h1>", "img/a.png": "p"},
	}
	for name, files := range cases {
		dir := t.TempDir()
		for f, c := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
			os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644)
		}
		entries, _ := dirEntries(dir)
		ws, err := newWorkspace(entries, dir)
		if err != nil {
			t.Fatal(name, err)
		}
		if !ws.Files.IsFile("index.html") {
			t.Fatalf("%s: no index.html after auto-fix", name)
		}
		found := false
		for _, is := range ws.Issues {
			if is.Source == "auto-fix" && is.Level == "info" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: auto-fix not reported: %+v", name, ws.Issues)
		}
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
			t.Fatalf("%s: the user's own folder was changed", name)
		}
		ws.Close()
	}
	// two pages, no obvious home: do not guess
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.html"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.html"), []byte("b"), 0o644)
	entries, _ := dirEntries(dir)
	ws, _ := newWorkspace(entries, dir)
	defer ws.Close()
	if ws.Files.IsFile("index.html") {
		t.Fatal("guessed a home page among several")
	}
}

func TestStartersCreateAndStage(t *testing.T) {
	isolate(t)
	list := listStarters()
	if len(list) < 6 || list[0].ID != "business" {
		t.Fatalf("starters: %+v", list)
	}
	for _, s := range list {
		dir := t.TempDir()
		if err := writeStarter(s.ID, `Ravi's <Shop>`, dir); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "index.html"))
		if err != nil {
			t.Fatal(s.ID, err)
		}
		if strings.Contains(string(b), "{{") || !strings.Contains(string(b), "Ravi&#39;s &lt;Shop&gt;") {
			t.Fatalf("%s: placeholders not filled safely", s.ID)
		}
		if _, err := os.Stat(filepath.Join(dir, "starter.json")); err == nil {
			t.Fatal("starter.json must not be published")
		}
		entries, _ := dirEntries(dir)
		ws, err := newWorkspace(entries, dir)
		if err != nil {
			t.Fatal(err)
		}
		rep := runChecks(ws)
		if rep.Counts["fail"] > 0 {
			t.Fatalf("%s: template fails its own checks: %+v", s.ID, rep)
		}
		ws.Close()
	}
	if err := writeStarter("../x", "a", t.TempDir()); err == nil {
		t.Fatal("path traversal accepted")
	}
	code, out := call(t, orgMux(), "POST", "/api/templates/links/use", map[string]any{"name": "Asha"})
	if code != 200 || out["import_id"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	dropStaged(out["import_id"].(string))
}

func TestAnalyzeLog(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	raw := strings.Join([]string{
		`/docker-entrypoint.sh: Configuration complete; ready for start up`,
		`10.0.0.1 - - [01/Nov/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 512 "https://www.google.com/search?q=x" "Mozilla/5.0"`,
		`10.0.0.1 - - [01/Nov/2026:10:00:01 +0000] "GET /style.css HTTP/1.1" 200 100 "https://shop.in/" "Mozilla/5.0"`,
		`10.0.0.2 - - [31/Oct/2026:09:00:00 +0000] "GET /about?ref=1 HTTP/1.1" 200 300 "https://shop.in/" "Mozilla/5.0"`,
		`10.0.0.2 - - [31/Oct/2026:09:00:05 +0000] "GET /old-page HTTP/1.1" 404 153 "-" "Mozilla/5.0"`,
		`10.0.0.3 - - [31/Oct/2026:09:00:05 +0000] "GET / HTTP/1.1" 200 512 "-" "Googlebot/2.1"`,
		`2026/11/01 10:00:00 [error] 29#29: *1 open() "/usr/share/nginx/html/x" failed`,
	}, "\n")
	st := analyzeLog(raw, now, 7, []string{"shop.in"})
	if st.Requests != 5 || st.PageViews != 2 || st.Visitors != 2 || st.Bots != 1 {
		t.Fatalf("%+v", st)
	}
	if len(st.Days) != 7 || st.Days[6].N != 1 || st.Days[5].N != 1 {
		t.Fatalf("days %+v", st.Days)
	}
	if len(st.NotFound) != 1 || st.NotFound[0].Key != "/old-page" {
		t.Fatalf("404s %+v", st.NotFound)
	}
	if len(st.Referrers) != 1 || st.Referrers[0].Key != "www.google.com" {
		t.Fatalf("refs %+v", st.Referrers)
	}
	if st.Status["4xx"] != 1 || len(st.Errors) != 1 {
		t.Fatalf("status %+v errors %v", st.Status, st.Errors)
	}
}

func TestAttentionAndChecklist(t *testing.T) {
	isolate(t)
	c := &Config{ServerIP: "1.2.3.4", CoolifyToken: "x", Sites: []*Site{
		{Slug: "new", Name: "New"},
		{Slug: "down", Name: "Down", LastDeploy: "t", AppUUID: "u1"},
		{Slug: "dns", Name: "Dns", LastDeploy: "t", AppUUID: "u2", Domain: "x.in"},
		{Slug: "cert", Name: "Cert", LastDeploy: "t", AppUUID: "u3", Domain: "y.in", DomainLive: true, HTTPSActive: true},
		{Slug: "arch", Name: "Arch", Archived: true},
	}}
	days := 3
	items := attentionItems(c, map[string]CApp{"u1": {Status: "exited:unhealthy"}, "u2": {Status: "running:healthy"}, "u3": {Status: "running:healthy"}}, "",
		map[string]HealthResult{"cert": {OK: true, CertDays: &days}}, true, time.Now())
	kinds := map[string]string{}
	for _, it := range items {
		kinds[it.Slug+"/"+it.Title] = it.Level
		if it.Fix != nil && (it.Fix.Kind == "restart" || it.Fix.Kind == "pull") && !it.Fix.Confirm {
			t.Fatal("server-changing fix without confirm")
		}
		if it.Slug == "arch" {
			t.Fatal("archived project listed")
		}
	}
	for _, k := range []string{"new/Not online yet", "down/Site is stopped", "dns/Web address not working yet", "cert/Padlock certificate ends soon", "/Backups are off"} {
		if _, ok := kinds[k]; !ok {
			t.Fatalf("missing %q in %v", k, kinds)
		}
	}
	if items[0].Level != "fail" {
		t.Fatal("failures must come first")
	}
	cl := setupChecklist(c)
	if !cl[0].Done || !cl[1].Done || !cl[2].Done || !cl[3].Done || cl[5].Done {
		t.Fatalf("%+v", cl)
	}
}

func TestBackupNowAndPrune(t *testing.T) {
	isolate(t)
	root := os.Getenv("DILREET_PROJECTS_DIR")
	bdir := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Shop", "current", "css"), 0o755)
	os.MkdirAll(filepath.Join(root, "Shop", "deploys"), 0o755)
	os.WriteFile(filepath.Join(root, "Shop", "current", "index.html"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(root, "Shop", "current", "css", "a.css"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "Shop", "deploys", "index.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(root, "Shop", "deploys", "old.zip"), []byte("zip"), 0o644)
	c := &Config{CoolifyToken: "SECRET-TOKEN", Backup: &BackupSettings{Every: "daily", Dir: bdir, Keep: 2},
		Sites: []*Site{{Slug: "shop", Name: "Shop", Folder: "Shop"}}}
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	var p string
	for i := 0; i < 3; i++ {
		var err error
		if p, _, err = backupNow(c, now.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(listBackups(bdir)); n != 2 {
		t.Fatalf("kept %d backups, want 2", n)
	}
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
		rc, _ := f.Open()
		var buf bytes.Buffer
		buf.ReadFrom(rc)
		rc.Close()
		if strings.Contains(buf.String(), "SECRET-TOKEN") {
			t.Fatal("secret written to backup")
		}
	}
	for _, n := range []string{"projects/Shop/current/index.html", "projects/Shop/current/css/a.css", "projects/Shop/deploys/index.json", "dilreet-projects.json"} {
		if !names[n] {
			t.Fatalf("missing %s in %v", n, names)
		}
	}
	if names["projects/Shop/deploys/old.zip"] {
		t.Fatal("deploy zips included without include_deploys")
	}
	b := backupSettings(c)
	if backupDue(b, now) != true {
		t.Fatal("never-run backup should be due")
	}
	b.Last = now.Format(time.RFC3339)
	if backupDue(b, now.Add(time.Hour)) || !backupDue(b, now.Add(25*time.Hour)) {
		t.Fatal("daily schedule wrong")
	}
}

func TestHealthProbeTLSExpiry(t *testing.T) {
	isolate(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	old := healthProbe
	defer func() { healthProbe = old }()
	healthProbe = func(u string) *HealthResult {
		// use the test server's client (trusts its certificate)
		r := &HealthResult{URL: u}
		resp, err := srv.Client().Get(u)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		resp.Body.Close()
		r.OK, r.Code = true, resp.StatusCode
		d := int(time.Until(resp.TLS.PeerCertificates[0].NotAfter).Hours() / 24)
		r.CertDays = &d
		return r
	}
	_ = withConfig(func(c *Config) error {
		c.Sites = []*Site{{Slug: "s", Name: "S", LastDeploy: "t", InstantURL: srv.URL}, {Slug: "nd", Name: "Not deployed"}}
		return nil
	})
	res := runHealthFor(nil)
	if len(res) != 1 || !res["s"].OK || res["s"].CertDays == nil {
		t.Fatalf("%+v", res)
	}
	if _, ok := healthSnapshot()["s"]; !ok {
		t.Fatal("result not stored")
	}
	if liveState("running:healthy") != "live" || liveState("exited:unhealthy") != "down" || liveState("") != "unknown" {
		t.Fatal("liveState")
	}
}
