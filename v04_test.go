package main

// v0.4 (Dilreet) mock tests: registrars, project folders and deploy history,
// config migration. No real network calls.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func plainCred(mode, key, sec string) RegistrarCred {
	k, _ := protectString(key)
	s := ""
	if sec != "" {
		s, _ = protectString(sec)
	}
	return RegistrarCred{Key: k, Secret: s, Mode: mode}
}

// fake GoDaddy zone shared by the v1 and v3 handlers.
type gdFake struct {
	mu      sync.Mutex
	recs    []map[string]any // recordId,name,type,data
	auths   []string
	deny    bool
	nextID  int
	nsSet   []string
	putBody []map[string]any
}

func (f *gdFake) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if f.deny {
			w.WriteHeader(403)
			io.WriteString(w, `{"code":"ACCESS_DENIED","message":"Authenticated user is not allowed access"}`)
			return
		}
		p := r.URL.Path
		match := func(typ, name string) []map[string]any {
			var out []map[string]any
			for _, x := range f.recs {
				if x["type"] == typ && x["name"] == name {
					out = append(out, x)
				}
			}
			return out
		}
		switch {
		case strings.HasPrefix(p, "/v3/domains/domain-names/") && r.Method == "PUT":
			json.NewDecoder(r.Body).Decode(&f.nsSet)
			w.WriteHeader(202)
		case strings.HasPrefix(p, "/v3/domains/zones/example.com/dns-records"):
			id := strings.TrimPrefix(strings.TrimPrefix(p, "/v3/domains/zones/example.com/dns-records"), "/")
			switch r.Method {
			case "GET":
				items := match(r.URL.Query().Get("type"), r.URL.Query().Get("name"))
				if r.URL.Query().Get("type") == "" {
					items = f.recs
				}
				json.NewEncoder(w).Encode(map[string]any{"items": items})
			case "POST":
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				f.nextID++
				b["recordId"] = "n" + string(rune('0'+f.nextID))
				f.recs = append(f.recs, b)
				w.WriteHeader(201)
				json.NewEncoder(w).Encode(b)
			case "PUT":
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				for _, x := range f.recs {
					if x["recordId"] == id {
						x["data"] = b["data"]
					}
				}
				io.WriteString(w, `{}`)
			case "DELETE":
				var kept []map[string]any
				for _, x := range f.recs {
					if x["recordId"] != id {
						kept = append(kept, x)
					}
				}
				f.recs = kept
				w.WriteHeader(204)
			}
		case p == "/v1/domains/example.com":
			io.WriteString(w, `{"domain":"example.com","status":"ACTIVE"}`)
		case strings.HasPrefix(p, "/v1/domains/example.com/records/"):
			parts := strings.Split(strings.TrimPrefix(p, "/v1/domains/example.com/records/"), "/")
			typ, name := parts[0], parts[1]
			switch r.Method {
			case "GET":
				out := []map[string]any{}
				for _, x := range match(typ, name) {
					out = append(out, map[string]any{"type": x["type"], "name": x["name"], "data": x["data"], "ttl": 600})
				}
				json.NewEncoder(w).Encode(out)
			case "PUT":
				var b []map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				f.putBody = b
				var kept []map[string]any
				for _, x := range f.recs {
					if !(x["type"] == typ && x["name"] == name) {
						kept = append(kept, x)
					}
				}
				for _, x := range b {
					kept = append(kept, map[string]any{"type": typ, "name": name, "data": x["data"]})
				}
				f.recs = kept
				io.WriteString(w, `{}`)
			case "DELETE":
				var kept []map[string]any
				for _, x := range f.recs {
					if !(x["type"] == typ && x["name"] == name) {
						kept = append(kept, x)
					}
				}
				f.recs = kept
				w.WriteHeader(204)
			}
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"code":"NOT_FOUND","message":"not found"}`)
		}
	})
}

func gdDefaultZone() []map[string]any {
	return []map[string]any{
		{"recordId": "a1", "type": "A", "name": "@", "data": "Parked"},
		{"recordId": "c1", "type": "CNAME", "name": "www", "data": "@"},
		{"recordId": "m1", "type": "MX", "name": "@", "data": "mail.example.net"},
	}
}

func TestGoDaddyBothModes(t *testing.T) {
	for _, mode := range []string{"pat", "key"} {
		t.Run(mode, func(t *testing.T) {
			f := &gdFake{recs: gdDefaultZone()}
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()
			old := gdBase
			gdBase = srv.URL
			defer func() { gdBase = old }()
			cred := plainCred(mode, "KEY", "SEC")
			if mode == "pat" {
				cred = plainCred("pat", "PAT123", "")
			}
			g := newGoDaddy(cred)
			if err := g.Verify("example.com"); err != nil {
				t.Fatal(err)
			}
			want := "sso-key KEY:SEC"
			if mode == "pat" {
				want = "Bearer PAT123"
			}
			if f.auths[0] != want {
				t.Fatalf("auth header %q, want %q", f.auths[0], want)
			}
			// The Parked A record is a conflict that needs confirmation.
			conf, err := upsertRegistrarA(g, "example.com", "@", "203.0.113.7", false)
			if !errors.Is(err, errCFConflict) || len(conf) != 1 || conf[0].Content != "Parked" {
				t.Fatalf("expected Parked conflict, got %v %v", conf, err)
			}
			if _, err := upsertRegistrarA(g, "example.com", "@", "203.0.113.7", true); err != nil {
				t.Fatal(err)
			}
			// www CNAME @ follows the apex and is kept.
			if conf, err := upsertRegistrarA(g, "example.com", "www", "203.0.113.7", false); err != nil || conf != nil {
				t.Fatalf("www CNAME @ should be kept: %v %v", conf, err)
			}
			var a, mx, cname int
			for _, x := range f.recs {
				switch x["type"] {
				case "A":
					a++
					if x["data"] != "203.0.113.7" {
						t.Errorf("A not updated: %v", x)
					}
				case "MX":
					mx++
				case "CNAME":
					cname++
				}
			}
			if a != 1 || mx != 1 || cname != 1 {
				t.Fatalf("zone after: %v", f.recs)
			}
			if mode == "key" && (len(f.putBody) != 1 || f.putBody[0]["ttl"].(float64) != 600) {
				t.Errorf("v1 PUT body %v", f.putBody)
			}
			err = g.SetNameservers("example.com", []string{"ada.ns.cloudflare.com", "bob.ns.cloudflare.com"})
			if mode == "key" && err == nil {
				t.Error("classic key must not switch nameservers")
			}
			if mode == "pat" && (err != nil || len(f.nsSet) != 2) {
				t.Errorf("nameservers: %v %v", err, f.nsSet)
			}
		})
	}
}

func TestGoDaddy403Explained(t *testing.T) {
	f := &gdFake{deny: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	old := gdBase
	gdBase = srv.URL
	defer func() { gdBase = old }()
	err := newGoDaddy(plainCred("key", "k", "s")).Verify("example.com")
	if err == nil || !strings.Contains(err.Error(), "at least one domain") || !strings.Contains(err.Error(), "10+ domains") {
		t.Fatalf("403 should explain eligibility: %v", err)
	}
	if strings.Contains(err.Error(), "sso-key") || strings.Contains(err.Error(), ":s") {
		t.Fatal("error leaks the key")
	}
}

func TestPorkbunAdapter(t *testing.T) {
	recs := []map[string]string{
		{"id": "1", "name": "example.com", "type": "ALIAS", "content": "pixie.porkbun.com"},
		{"id": "2", "name": "*.example.com", "type": "CNAME", "content": "uixie.porkbun.com"},
	}
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		calls = append(calls, r.URL.Path)
		if b["apikey"] != "pk1_x" || b["secretapikey"] != "sk1_y" {
			io.WriteString(w, `{"status":"ERROR","message":"Invalid API key."}`)
			return
		}
		p := r.URL.Path
		full := func(sub string) string {
			if sub == "" {
				return "example.com"
			}
			return sub + ".example.com"
		}
		switch {
		case p == "/ping":
			io.WriteString(w, `{"status":"SUCCESS","yourIp":"198.51.100.1"}`)
		case strings.HasPrefix(p, "/dns/retrieveByNameType/example.com/"):
			parts := strings.SplitN(strings.TrimPrefix(p, "/dns/retrieveByNameType/example.com/"), "/", 2)
			out := []map[string]string{}
			for _, x := range recs {
				if x["type"] == parts[0] && x["name"] == full(parts[1]) {
					out = append(out, x)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "SUCCESS", "records": out})
		case strings.HasPrefix(p, "/dns/deleteByNameType/example.com/"):
			parts := strings.SplitN(strings.TrimPrefix(p, "/dns/deleteByNameType/example.com/"), "/", 2)
			var kept []map[string]string
			for _, x := range recs {
				if !(x["type"] == parts[0] && x["name"] == full(parts[1])) {
					kept = append(kept, x)
				}
			}
			recs = kept
			io.WriteString(w, `{"status":"SUCCESS"}`)
		case p == "/dns/create/example.com":
			recs = append(recs, map[string]string{"id": "9", "name": full(b["name"].(string)), "type": b["type"].(string), "content": b["content"].(string)})
			io.WriteString(w, `{"status":"SUCCESS","id":9}`)
		default:
			io.WriteString(w, `{"status":"ERROR","message":"Domain is not opted in to API access."}`)
		}
	}))
	defer srv.Close()
	old := pbBase
	pbBase = srv.URL
	defer func() { pbBase = old }()
	if err := newPorkbun(plainCred("key", "bad", "bad")).Verify(""); err == nil {
		t.Fatal("bad key accepted")
	}
	p := newPorkbun(plainCred("key", "pk1_x", "sk1_y"))
	if err := p.Verify("example.com"); err != nil {
		t.Fatal(err)
	}
	conf, err := upsertRegistrarA(p, "example.com", "@", "203.0.113.7", false)
	if !errors.Is(err, errCFConflict) || len(conf) != 1 || conf[0].Type != "ALIAS" {
		t.Fatalf("ALIAS conflict expected: %v %v", conf, err)
	}
	if _, err := upsertRegistrarA(p, "example.com", "@", "203.0.113.7", true); err != nil {
		t.Fatal(err)
	}
	if _, err := upsertRegistrarA(p, "example.com", "www", "203.0.113.7", false); err != nil {
		t.Fatal(err)
	}
	var a int
	for _, x := range recs {
		if x["type"] == "ALIAS" {
			t.Error("ALIAS not removed")
		}
		if x["type"] == "A" {
			a++
		}
	}
	if a != 2 {
		t.Fatalf("records: %v", recs)
	}
	err = p.do("/dns/other", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "API Access") {
		t.Errorf("opt-in hint missing: %v", err)
	}
}

func TestRegistrarGuides(t *testing.T) {
	old := lookupNS
	defer func() { lookupNS = old }()
	lookupNS = func(ctx context.Context, d string) ([]*net.NS, error) {
		m := map[string]string{"shop.com": "ns07.domaincontrol.com.", "nc.com": "dns1.registrar-servers.com.", "pb.com": "curitiba.ns.porkbun.com.", "hs.com": "ns1.dns-parking.com.", "sq.com": "ns-cloud-a1.googledomains.com."}
		if h, ok := m[d]; ok {
			return []*net.NS{{Host: h}}, nil
		}
		return nil, errors.New("nx")
	}
	h := detectDNSHost("www.shop.com")
	g := guideFor(h.ID, "www.shop.com")
	if h.ID != "godaddy" || g == nil || g.API != "godaddy" || !strings.Contains(g.DNSURL, "domainName=shop.com") || len(g.Warnings) < 2 {
		t.Fatalf("godaddy guide: %+v %+v", h, g)
	}
	for d, want := range map[string]string{"nc.com": "namecheap", "pb.com": "porkbun"} {
		if h := detectDNSHost(d); h.ID != want || guideFor(h.ID, d) == nil {
			t.Errorf("%s: got %s", d, h.ID)
		}
	}
	if g := guideFor("namecheap", "nc.com"); g.API != "" || !strings.Contains(strings.Join(g.Warnings, " "), "whitelisted IPv4") {
		t.Errorf("namecheap must be guided only with the whitelist note: %+v", g)
	}
	if z, n := splitZone("blog.example.co.uk"); z != "example.co.uk" || n != "blog" {
		t.Errorf("splitZone %s %s", z, n)
	}
}

func TestCloudflareMove(t *testing.T) {
	isolate(t)
	var zonesCreated, mx, txt, a int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/zones":
			io.WriteString(w, `{"success":true,"result":[]}`)
		case r.Method == "GET" && r.URL.Path == "/accounts":
			io.WriteString(w, `{"success":true,"result":[{"id":"acc1","name":"Me"}]}`)
		case r.Method == "POST" && r.URL.Path == "/zones":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["account"].(map[string]any)["id"] != "acc1" || b["type"] != "full" {
				t.Errorf("zone body %v", b)
			}
			zonesCreated++
			io.WriteString(w, `{"success":true,"result":{"id":"z1","name":"example.com","status":"pending","name_servers":["ada.ns.cloudflare.com","bob.ns.cloudflare.com"]}}`)
		case r.Method == "GET":
			io.WriteString(w, `{"success":true,"result":[]}`)
		case r.Method == "POST":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			switch b["type"] {
			case "MX":
				mx++
			case "TXT":
				txt++
			case "A":
				a++
			}
			io.WriteString(w, `{"success":true,"result":{"id":"r`+string(rune('0'+a))+`"}}`)
		default:
			io.WriteString(w, `{"success":true,"result":{}}`)
		}
	}))
	defer srv.Close()
	oldB, oldMX, oldTXT, oldNS := cfBase, lookupMX, lookupTXT, lookupNS
	cfBase = srv.URL
	lookupMX = func(ctx context.Context, d string) ([]*netMX, error) {
		return []*netMX{{Host: "mx.mail.example.net.", Pref: 10}}, nil
	}
	lookupTXT = func(ctx context.Context, d string) ([]string, error) { return []string{"v=spf1 include:x -all"}, nil }
	lookupNS = func(ctx context.Context, d string) ([]*net.NS, error) {
		return []*net.NS{{Host: "ns1.domaincontrol.com."}}, nil
	}
	defer func() { cfBase, lookupMX, lookupTXT, lookupNS = oldB, oldMX, oldTXT, oldNS }()
	tok, _ := protectString("cf")
	_ = withConfig(func(c *Config) error {
		c.CloudflareToken, c.ServerIP = tok, "203.0.113.7"
		c.Sites = []*Site{{Slug: "shop", Name: "Shop"}}
		return nil
	})
	mux := http.NewServeMux()
	registerRegistrarRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sites/shop/cloudflare-move", strings.NewReader(`{"domain":"example.com","www":true}`)))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var out struct {
		Nameservers []string
		Copied      []string
		Guide       *RegistrarGuide
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Nameservers) != 2 || zonesCreated != 1 || mx != 1 || txt != 1 || a != 2 || len(out.Copied) != 2 || out.Guide == nil || len(out.Guide.NSSteps) == 0 {
		t.Fatalf("move result %+v zones=%d mx=%d txt=%d a=%d", out, zonesCreated, mx, txt, a)
	}
	c := snapshotConfig()
	if s := c.Sites[0]; s.DNSMode != "cloudflare" || s.CFZoneID != "z1" || len(s.PendingNS) != 2 || s.Domain != "example.com" {
		t.Fatalf("site %+v", s)
	}
}

func TestProjectArchiveIndexCompareRetention(t *testing.T) {
	isolate(t)
	_ = withConfig(func(c *Config) error {
		c.Sites = []*Site{{Slug: "shop", Name: `My: Shop?`}, {Slug: "con", Name: "CON"}}
		assignFolders(c)
		return nil
	})
	c := snapshotConfig()
	if c.Sites[0].Folder != "My- Shop-" || c.Sites[1].Folder != "con" {
		t.Fatalf("folder names %q %q", c.Sites[0].Folder, c.Sites[1].Folder)
	}
	deploy := func(files map[string]string, release string, fail bool) *DeployRecord {
		ents, err := dirEntries(writeTree(t, files))
		if err != nil {
			t.Fatal(err)
		}
		ws, err := newWorkspace(ents, "uploaded folder")
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		j := &Job{}
		rec := archiveDeploy(j, "shop", ws)
		if rec == nil {
			t.Fatalf("no archive: %v", j.lines)
		}
		var derr error
		if fail {
			derr = errors.New("boom")
		} else {
			_ = withConfig(func(c *Config) error { findSite(c, "shop").Release = release; return nil })
		}
		finishDeploy("shop", rec.ID, derr)
		return rec
	}
	r1 := deploy(map[string]string{"index.html": "<h1>v1</h1>", "a.css": "x"}, "20260101-000001", false)
	r2 := deploy(map[string]string{"index.html": "<h1>v2</h1>", "b.js": "y"}, "20260101-000002", false)
	r3 := deploy(map[string]string{"index.html": "<h1>v3</h1>"}, "", true)
	dir, _, _ := projectDir("shop")
	ix, err := readIndex(dir)
	if err != nil || len(ix.Deploys) != 3 || ix.Deploys[0].ID != r3.ID {
		t.Fatalf("index %v %+v", err, ix)
	}
	if ix.find(r1.ID).Status != "published" || ix.find(r3.ID).Status != "failed" || ix.find(r2.ID).Release != "20260101-000002" || ix.find(r1.ID).Checks == nil || ix.find(r1.ID).Source != "drop" {
		t.Fatalf("records %+v %+v %+v", ix.find(r1.ID), ix.find(r2.ID), ix.find(r3.ID))
	}
	// current\ holds the last deploy's files.
	if b, _ := os.ReadFile(filepath.Join(dir, "current", "index.html")); string(b) != "<h1>v3</h1>" {
		t.Errorf("current = %q", b)
	}
	m1, _ := zipManifest(filepath.Join(dir, "deploys", r1.File))
	m2, _ := zipManifest(filepath.Join(dir, "deploys", r2.File))
	d := compareManifests(m1, m2)
	if strings.Join(d.Added, ",") != "b.js" || strings.Join(d.Removed, ",") != "a.css" || strings.Join(d.Changed, ",") != "index.html" {
		t.Fatalf("diff %+v", d)
	}
	// HTTP: list, compare, refuse deleting the live deploy, delete an old one.
	mux := http.NewServeMux()
	registerProjectRoutes(mux)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	if rec := do("GET", "/api/projects/shop/deploys/"+r2.ID+"/compare", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"b.js"`) {
		t.Fatalf("compare %d %s", rec.Code, rec.Body)
	}
	if rec := do("DELETE", "/api/projects/shop/deploys/"+r2.ID, ""); rec.Code == 200 {
		t.Fatal("deleted the live deploy")
	}
	if rec := do("DELETE", "/api/projects/shop/deploys/"+r1.ID, ""); rec.Code != 200 {
		t.Fatalf("delete %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploys", r1.File)); !os.IsNotExist(err) {
		t.Error("zip not removed")
	}
	if rec := do("DELETE", "/api/projects/shop/deploys/..%2f..%2fx", ""); rec.Code == 200 {
		t.Error("bad id accepted")
	}
	// Raw preview is sandboxed and cannot escape the folder.
	rec := do("GET", "/api/projects/shop/raw/index.html", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Security-Policy"), "sandbox") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("raw %d %v", rec.Code, rec.Header())
	}
	if rec := do("GET", "/api/projects/shop/raw/../deploys/index.json", ""); rec.Code == 200 {
		t.Error("path traversal served")
	}
	if rec := do("GET", "/api/projects/shop/files", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"index.html"`) {
		t.Errorf("files %s", rec.Body)
	}
	if rec := do("POST", "/api/projects/shop/deploys/"+r2.ID+"/notes", `{"notes":"launch day"}`); rec.Code != 200 {
		t.Errorf("notes %s", rec.Body)
	}
	// Retention: keep 1 -> only the live deploy (and failed ones beyond count are dropped).
	_ = withConfig(func(c *Config) error { c.KeepDeploys = 1; return nil })
	r4 := deploy(map[string]string{"index.html": "<h1>v4</h1>"}, "20260101-000004", false)
	ix, _ = readIndex(dir)
	if ix.find(r4.ID) == nil || ix.find(r2.ID) != nil {
		t.Fatalf("retention kept %+v", ix.Deploys)
	}
	if rec := do("GET", "/api/storage", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"keep":1`) {
		t.Errorf("storage %s", rec.Body)
	}
}

func TestRiskyOpenAndSafeJoin(t *testing.T) {
	for _, f := range []string{"a.EXE", "x.bat", "y.ps1", "z.js", "s.lnk", "r.reg"} {
		if !isRiskyToOpen(f) {
			t.Errorf("%s should be risky", f)
		}
	}
	if isRiskyToOpen("index.html") || isRiskyToOpen("style.css") {
		t.Error("html/css are safe to edit")
	}
	base := writeTree(t, map[string]string{"a/b.txt": "x"})
	if _, err := safeJoin(base, "a/b.txt"); err != nil {
		t.Error(err)
	}
	if _, err := safeJoin(base, "../etc/passwd"); err == nil {
		t.Error("escape allowed")
	}
	outside := writeTree(t, map[string]string{"secret.txt": "s"})
	if os.Symlink(outside, filepath.Join(base, "link")) == nil {
		if _, err := safeJoin(base, "link/secret.txt"); err == nil {
			t.Error("symlink escape allowed")
		}
	}
}

func TestConfigMigrationAndEnv(t *testing.T) {
	old, nw := t.TempDir(), filepath.Join(t.TempDir(), "Dilreet")
	os.WriteFile(filepath.Join(old, "config.json"), []byte(`{"server_ip":"203.0.113.9","sites":[{"slug":"a","name":"A"}]}`), 0o600)
	os.WriteFile(filepath.Join(old, "id_ed25519"), []byte("KEY"), 0o600)
	ok, err := migrateConfigDir(old, nw)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	b, _ := os.ReadFile(filepath.Join(nw, "config.json"))
	var c Config
	if json.Unmarshal(b, &c) != nil || c.ServerIP != "203.0.113.9" || c.MigratedFrom != old || len(c.Sites) != 1 {
		t.Fatalf("migrated config %s", b)
	}
	if k, _ := os.ReadFile(filepath.Join(nw, "id_ed25519")); string(k) != "KEY" {
		t.Error("key file not copied")
	}
	if _, err := os.Stat(filepath.Join(old, "config.json")); err != nil {
		t.Error("old config must stay as a backup")
	}
	if ok, _ := migrateConfigDir(old, nw); ok {
		t.Error("migrated twice")
	}
	t.Setenv("DILREET_X", "")
	t.Setenv("HOSTPANEL_X", "legacy")
	if envOr("X") != "legacy" {
		t.Error("legacy env not read")
	}
	t.Setenv("DILREET_X", "new")
	if envOr("X") != "new" {
		t.Error("DILREET_ env must win")
	}
}
