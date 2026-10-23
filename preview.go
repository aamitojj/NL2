package main

// The App Testing preview server: a separate HTTP server on 127.0.0.1 /
// [::1] with a random port that serves one Workspace with Netlify-style
// rules. It has no API routes and is served from the "other" loopback name
// (localhost vs 127.0.0.1) than the Dilreet UI, so previewed pages are
// cross-site to Dilreet and never receive or send its session cookie.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed inject.js
var injectJS string

const maxEvents = 1000

type TestEvent struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`  // request, console, error, network, csp, nav, info
	Level  string    `json:"level"` // info, warn, error
	Msg    string    `json:"msg"`
	URL    string    `json:"url,omitempty"`
	Method string    `json:"method,omitempty"`
	Status int       `json:"status,omitempty"`
	Via    string    `json:"via,omitempty"`
	Stack  string    `json:"stack,omitempty"`
}

type eventLog struct {
	mu      sync.Mutex
	seq     int64
	events  []TestEvent
	dropped int64
	errors  int
	warns   int
	noted   map[string]bool
}

func (l *eventLog) add(e TestEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	switch e.Level {
	case "error":
		l.errors++
	case "warn":
		l.warns++
	}
	l.events = append(l.events, e)
	if len(l.events) > maxEvents {
		n := len(l.events) - maxEvents
		l.dropped += int64(n)
		l.events = append([]TestEvent(nil), l.events[n:]...)
	}
}

// once records an info event only the first time key is seen.
func (l *eventLog) once(key string, e TestEvent) {
	l.mu.Lock()
	if l.noted == nil {
		l.noted = map[string]bool{}
	}
	seen := l.noted[key]
	l.noted[key] = true
	l.mu.Unlock()
	if !seen {
		l.add(e)
	}
}

func (l *eventLog) since(from int64) ([]TestEvent, int64, int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []TestEvent{}
	for _, e := range l.events {
		if e.Seq > from {
			out = append(out, e)
		}
	}
	return out, l.seq, l.errors, l.warns
}

func (l *eventLog) clear() {
	l.mu.Lock()
	l.events = nil
	l.errors, l.warns = 0, 0
	l.noted = nil
	l.mu.Unlock()
}

type PreviewServer struct {
	ws       *Workspace
	log      *eventLog
	port     int
	host     string // host name used in the preview URL
	uiOrigin string
	srv      *http.Server
	lns      []net.Listener
}

// previewHostFor returns the loopback name the preview must use given the
// host the UI is opened on.
func previewHostFor(uiHost string) string {
	if h, _, err := net.SplitHostPort(uiHost); err == nil {
		uiHost = h
	}
	if uiHost == "localhost" {
		return "127.0.0.1"
	}
	return "localhost"
}

func startPreview(ws *Workspace, uiHost string, log *eventLog) (*PreviewServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ps := &PreviewServer{ws: ws, log: log, port: port, host: previewHostFor(uiHost), uiOrigin: "http://" + uiHost, lns: []net.Listener{ln}}
	// "localhost" may resolve to ::1 first; listen there too when possible.
	if ln6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port)); err == nil {
		ps.lns = append(ps.lns, ln6)
	}
	ps.srv = &http.Server{Handler: ps, ReadHeaderTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	for _, l := range ps.lns {
		go ps.srv.Serve(l)
	}
	return ps, nil
}

func (ps *PreviewServer) URL() string { return fmt.Sprintf("http://%s:%d/", ps.host, ps.port) }

func (ps *PreviewServer) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = ps.srv.Shutdown(ctx)
	ps.srv.Close()
}

func (ps *PreviewServer) hostOK(h string) bool {
	host, port, err := net.SplitHostPort(h)
	if err != nil || port != strconv.Itoa(ps.port) {
		return false
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

var builtinMIME = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".css": "text/css; charset=utf-8", ".js": "application/javascript; charset=utf-8",
	".mjs": "application/javascript; charset=utf-8", ".cjs": "application/javascript; charset=utf-8",
	".json": "application/json", ".map": "application/json", ".webmanifest": "application/manifest+json",
	".xml": "application/xml", ".txt": "text/plain; charset=utf-8", ".csv": "text/csv; charset=utf-8",
	".md": "text/plain; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".avif": "image/avif", ".ico": "image/x-icon", ".bmp": "image/bmp", ".woff": "font/woff",
	".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf", ".eot": "application/vnd.ms-fontobject",
	".pdf": "application/pdf", ".mp4": "video/mp4", ".webm": "video/webm", ".ogv": "video/ogg",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".m4a": "audio/mp4",
	".wasm": "application/wasm", ".zip": "application/zip", ".gz": "application/gzip",
}

func contentTypeFor(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := builtinMIME[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

func isHTMLName(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".html" || ext == ".htm"
}

var headOpenRe = regexp.MustCompile(`(?i)<head(\s[^>]*)?>`)
var htmlOpenRe = regexp.MustCompile(`(?i)<html(\s[^>]*)?>`)
var frameAncestorsRe = regexp.MustCompile(`(?i)(^|;)\s*frame-ancestors[^;]*`)

const injectTag = `<script src="/__dilreet/inject.js"></script>`

// injectInto adds the capture script as early as possible in the document.
func injectInto(b []byte) []byte {
	for _, re := range []*regexp.Regexp{headOpenRe, htmlOpenRe} {
		if loc := re.FindIndex(b); loc != nil {
			out := make([]byte, 0, len(b)+len(injectTag))
			out = append(out, b[:loc[1]]...)
			out = append(out, injectTag...)
			return append(out, b[loc[1]:]...)
		}
	}
	// No <html>/<head>: keep a leading doctype first.
	if bytes.HasPrefix(bytes.ToLower(bytes.TrimSpace(b[:min(len(b), 64)])), []byte("<!doctype")) {
		if i := bytes.IndexByte(b, '>'); i >= 0 {
			return append(append(append([]byte{}, b[:i+1]...), injectTag...), b[i+1:]...)
		}
	}
	return append([]byte(injectTag), b...)
}

func (ps *PreviewServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !ps.hostOK(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/__dilreet/") {
		ps.serveInternal(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		ps.log.add(TestEvent{Kind: "request", Level: "warn", Method: r.Method, URL: r.URL.RequestURI(), Status: 405,
			Msg: r.Method + " is not allowed on a static site (forms and APIs need a backend; Netlify Forms are not available on Dilreet)"})
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p, ok := cleanURLPath(r.URL.Path)
	if !ok {
		ps.log.add(TestEvent{Kind: "request", Level: "error", Method: r.Method, URL: r.URL.RequestURI(), Status: 400, Msg: "rejected: unsafe path"})
		http.Error(w, "400 bad request", http.StatusBadRequest)
		return
	}
	res := ps.ws.Rules.resolve(ps.ws.Files, p, r.URL.RawQuery)
	ps.writeHeaders(w, p)
	level := "info"
	if res.Status >= 400 {
		level = "error"
		if res.Status == 404 && (p == "/favicon.ico" || strings.HasSuffix(p, ".map")) {
			level = "warn"
		}
	}
	ev := TestEvent{Kind: "request", Level: level, Method: r.Method, URL: r.URL.RequestURI(), Status: res.Status, Via: res.Via}
	if res.Location != "" {
		ev.Msg = "→ " + res.Location
		w.Header().Set("Location", res.Location)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(res.Status)
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, "Redirecting to %s\n", res.Location)
		}
		ps.log.add(ev)
		return
	}
	if res.File == "" {
		ev.Msg = "not found (no 404.html in the site, so visitors get a plain error page)"
		ps.log.add(ev)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(res.Status)
		fmt.Fprintf(w, "<!doctype html><title>%d</title><p>%d — Not Found (Dilreet preview: the site has no 404.html)</p>", res.Status, res.Status)
		return
	}
	if res.Via != "" || res.File != strings.TrimPrefix(p, "/") {
		ev.Msg = "served " + res.File
	}
	ps.log.add(ev)
	full := filepath.Join(ps.ws.Publish, filepath.FromSlash(res.File))
	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "500 cannot read file", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", contentTypeFor(res.File))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if isHTMLName(res.File) {
		b, err := io.ReadAll(io.LimitReader(f, 64<<20))
		if err != nil {
			http.Error(w, "500 cannot read file", http.StatusInternalServerError)
			return
		}
		b = injectInto(b)
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(res.Status)
		if r.Method == http.MethodGet {
			w.Write(b)
		}
		return
	}
	if res.Status == http.StatusOK {
		http.ServeContent(w, r, res.File, st.ModTime(), f)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(res.Status)
	if r.Method == http.MethodGet {
		io.Copy(w, f)
	}
}

// writeHeaders applies _headers / netlify.toml rules, with the same default
// Cache-Control the production config uses. Anti-framing headers are relaxed
// only here so the page can be shown inside Dilreet's browser.
func (ps *PreviewServer) writeHeaders(w http.ResponseWriter, p string) {
	h := w.Header()
	h.Set("Cache-Control", defaultCacheControl)
	for _, kv := range ps.ws.Rules.headersFor(p) {
		name := http.CanonicalHeaderKey(kv[0])
		val := kv[1]
		switch strings.ToLower(name) {
		case "x-frame-options":
			ps.log.once("xfo", TestEvent{Kind: "info", Level: "info", Msg: "The site sends X-Frame-Options: " + val + ". It is ignored in the preview so the page can show inside Dilreet; it still applies when deployed."})
			continue
		case "content-security-policy":
			if frameAncestorsRe.MatchString(val) {
				ps.log.once("fa", TestEvent{Kind: "info", Level: "info", Msg: "The site's Content-Security-Policy has frame-ancestors; it is removed in the preview only."})
				val = strings.Trim(strings.TrimSpace(frameAncestorsRe.ReplaceAllString(val, "$1")), ";")
				val = strings.TrimSpace(val)
				if val == "" {
					continue
				}
			}
		}
		h.Set(name, val)
	}
}

type clientEvent struct {
	Kind  string `json:"kind"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
	URL   string `json:"url"`
	Stack string `json:"stack"`
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (ps *PreviewServer) serveInternal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case r.URL.Path == "/__dilreet/inject.js" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		js := strings.Replace(injectJS, "__HP_UI_ORIGIN__", ps.uiOrigin, 1)
		io.WriteString(w, js)
	case r.URL.Path == "/__dilreet/event" && r.Method == http.MethodPost:
		// Only the previewed pages themselves may report.
		if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var evs []clientEvent
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&evs); err != nil {
			http.Error(w, "bad event", http.StatusBadRequest)
			return
		}
		for i, e := range evs {
			if i >= 50 {
				break
			}
			switch e.Kind {
			case "console", "error", "network", "csp", "nav":
			default:
				continue
			}
			switch e.Level {
			case "error", "warn", "info":
			default:
				e.Level = "info"
			}
			ps.log.add(TestEvent{Kind: e.Kind, Level: e.Level, Msg: trunc(e.Msg, 2000), URL: trunc(e.URL, 500), Stack: trunc(e.Stack, 4000)})
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}
