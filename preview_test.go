package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var siteForServers = map[string]string{
	"index.html":       `<!doctype html><html lang="en"><head><title>Home</title></head><body>home</body></html>`,
	"about.html":       "<html><head></head><body>about</body></html>",
	"docs/index.html":  "<html><head></head><body>docs</body></html>",
	"docs/guide.html":  "<p>guide</p>",
	"assets/app.js":    "console.log(1)",
	"assets/style.css": "body{}",
	"blog/index.html":  "blog",
	"404.html":         "<html><head></head><body>not found page</body></html>",
	"secret/.env":      "X=1",
	"posts/hello.html": "post",
	"data.json":        `{"a":1}`,
	"_redirects": `
/old-about     /about          301
/blog/*        /posts/:splat   302
/u/:id/:tab    /users/:tab?id=:id 302
/legacy        https://example.com/new 301
/assets/old.js /assets/app.js  200
/docs-home     /docs/          200
/about         /never          301
/force-me      /docs/guide.html 200!
/gone          /404.html       410
/app/*         /index.html     200
`,
	"_headers": `
/*
  X-Frame-Options: DENY
  X-Test: all
/assets/*
  Cache-Control: public, max-age=31536000, immutable
`,
}

var probePaths = []string{
	"/", "/index.html", "/about", "/about/", "/about.html", "/docs", "/docs/", "/docs/guide", "/docs/guide/",
	"/old-about", "/old-about?x=1", "/blog/hello", "/blog", "/u/7/settings", "/legacy", "/assets/old.js",
	"/assets/app.js", "/docs-home", "/force-me", "/gone", "/app/deep/link", "/nope", "/Assets/app.js",
	"/secret/.env", "/_redirects", "/_headers", "/data.json", "/posts/hello", "/404",
}

func noRedirectClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type probe struct {
	status         int
	location, body string
	xtest, cache   string
}

func get(t *testing.T, c *http.Client, url, host string) probe {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if host != "" {
		req.Host = host
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return probe{resp.StatusCode, resp.Header.Get("Location"), string(b), resp.Header.Get("X-Test"), resp.Header.Get("Cache-Control")}
}

func TestPreviewServer(t *testing.T) {
	ws := loadWS(t, siteForServers)
	log := &eventLog{}
	ps, err := startPreview(ws, "127.0.0.1:4321", log)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Stop()
	if !strings.HasPrefix(ps.URL(), "http://localhost:") {
		t.Errorf("preview must use the other loopback name: %s", ps.URL())
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", ps.port)
	host := fmt.Sprintf("localhost:%d", ps.port)
	c := noRedirectClient()

	// Only loopback addresses are listened on.
	for _, l := range ps.lns {
		if ip := l.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
			t.Errorf("listening on non-loopback %v", ip)
		}
	}
	if p := get(t, c, base+"/", "evil.example:80"); p.status != 403 && p.status != 421 && p.status != 400 {
		t.Errorf("foreign Host header accepted: %d", p.status)
	}
	// The HostPanel API is not reachable through the preview.
	for _, u := range []string{"/api/state", "/api/sites", "/api/test"} {
		if p := get(t, c, base+u, host); p.status != 404 || strings.Contains(p.body, "\"sites\"") {
			t.Errorf("%s reachable via preview: %d", u, p.status)
		}
	}
	req, _ := http.NewRequest("POST", base+"/api/sites", strings.NewReader("{}"))
	req.Host = host
	req.Header.Set("X-HostPanel", "1")
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Errorf("POST through preview: %d", resp.StatusCode)
		}
	}

	home := get(t, c, base+"/", host)
	if home.status != 200 || !strings.Contains(home.body, "/__hostpanel/inject.js") {
		t.Errorf("home not injected: %d %q", home.status, home.body)
	}
	if home.xtest != "all" {
		t.Errorf("_headers not applied: %+v", home)
	}
	if js := get(t, c, base+"/assets/app.js", host); strings.Contains(js.body, "inject.js") || !strings.Contains(js.cache, "immutable") {
		t.Errorf("asset: %+v", js)
	}
	nf := get(t, c, base+"/nope", host)
	if nf.status != 404 || !strings.Contains(nf.body, "not found page") {
		t.Errorf("custom 404: %+v", nf)
	}
	if p := get(t, c, base+"/old-about", host); p.status != 301 || p.location != "/about" {
		t.Errorf("redirect: %+v", p)
	}
	if p := get(t, c, base+"/app/x/y", host); p.status != 200 || !strings.Contains(p.body, "home") {
		t.Errorf("spa rewrite: %+v", p)
	}
	if p := get(t, c, base+"/_headers", host); p.status != 404 {
		t.Errorf("rule file served: %+v", p)
	}
	if p := get(t, c, base+"/__hostpanel/inject.js", host); p.status != 200 || !strings.Contains(p.body, "http://127.0.0.1:4321") {
		t.Errorf("inject.js must know the UI origin: %d", p.status)
	}
	time.Sleep(50 * time.Millisecond)
	evs, _, errs, _ := log.since(0)
	if len(evs) == 0 || errs == 0 {
		t.Errorf("404 should be logged as an error event: %+v", evs)
	}
	// X-Frame-Options from _headers must not block the built-in browser.
	if p := get(t, c, base+"/", host); strings.Contains(fmt.Sprint(p), "DENY") {
		t.Errorf("X-Frame-Options passed through")
	}
}

// TestNginxMatchesPreview runs the generated production config in a real nginx
// (when installed) and checks that it answers like the preview for every probe.
func TestNginxMatchesPreview(t *testing.T) {
	bin, err := exec.LookPath("nginx")
	if err != nil {
		if _, e := os.Stat("/usr/sbin/nginx"); e == nil {
			bin = "/usr/sbin/nginx"
		} else {
			t.Skip("nginx not installed")
		}
	}
	ws := loadWS(t, siteForServers)
	conf := generateNginxConf(ws.Rules, ws.Files)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	prefix := t.TempDir()
	os.Chmod(prefix, 0o755)
	confd := filepath.Join(prefix, "conf.d")
	os.MkdirAll(confd, 0o755)
	conf = strings.Replace(conf, "listen 80 default_server;", fmt.Sprintf("listen 127.0.0.1:%d default_server;", port), 1)
	conf = strings.Replace(conf, "root /usr/share/nginx/html;", "root "+ws.Publish+";", 1)
	os.WriteFile(filepath.Join(confd, "default.conf"), []byte(conf), 0o644)
	mime := "/etc/nginx/mime.types"
	inc := ""
	if _, err := os.Stat(mime); err == nil {
		inc = "include " + mime + ";"
	}
	main := fmt.Sprintf(`daemon off; worker_processes 1; pid %[1]s/nginx.pid; error_log %[1]s/error.log;
events { worker_connections 64; }
http { %[2]s default_type application/octet-stream; access_log off;
  client_body_temp_path %[1]s/b; proxy_temp_path %[1]s/p; fastcgi_temp_path %[1]s/f; uwsgi_temp_path %[1]s/u; scgi_temp_path %[1]s/s;
  include %[1]s/conf.d/*.conf; }
`, prefix, inc)
	mainPath := filepath.Join(prefix, "nginx.conf")
	os.WriteFile(mainPath, []byte(main), 0o644)
	if out, err := exec.Command(bin, "-t", "-p", prefix, "-c", mainPath).CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v\n%s\n---\n%s", err, out, conf)
	}
	cmd := exec.Command(bin, "-p", prefix, "-c", mainPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Signal(syscall.SIGQUIT)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	}()
	if d := os.Getenv("HP_NGINX_DEBUG"); d != "" {
		os.WriteFile(d, []byte(conf), 0o644)
	}

	ps, err := startPreview(ws, "127.0.0.1:1", &eventLog{})
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Stop()
	c := noRedirectClient()
	ngx := fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; i < 50; i++ {
		if conn, err := net.Dial("tcp", ngx[7:]); err == nil {
			conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	pv := fmt.Sprintf("http://127.0.0.1:%d", ps.port)
	pvHost := fmt.Sprintf("localhost:%d", ps.port)
	strip := func(s string) string { return strings.Replace(s, injectTag, "", 1) }
	for _, p := range probePaths {
		a := get(t, c, ngx+p, "")
		b := get(t, c, pv+p, pvHost)
		sameBody := strip(a.body) == strip(b.body) || (a.status >= 300 && a.status < 400)
		if a.status != b.status || a.location != b.location || !sameBody || a.xtest != b.xtest || a.cache != b.cache {
			t.Errorf("%s differs:\n  nginx:   %d loc=%q x=%q cc=%q body=%.40q\n  preview: %d loc=%q x=%q cc=%q body=%.40q",
				p, a.status, a.location, a.xtest, a.cache, a.body, b.status, b.location, b.xtest, b.cache, strip(b.body))
		}
	}
}
