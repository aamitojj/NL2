// Dilreet: a Windows desktop control panel for a self-hosted, Netlify-like
// static hosting platform built on Coolify (https://coolify.io).
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	sessionToken = randHex(24)
	listenAddr   string
	httpServer   *http.Server
)

func main() {
	port := flag.Int("port", 8787, "local port for the UI (0 = random)")
	noBrowser := flag.Bool("no-browser", false, "browser mode: do not open the browser automatically")
	printToken := flag.Bool("print-url", true, "print the UI address with its session key")
	forceBrowser := flag.Bool("browser", false, "use the default web browser instead of the Dilreet window")
	afterUpdate := flag.Bool("after-update", false, "internal: started by the self-updater")
	debug := flag.Bool("debug", false, "enable WebView2 developer tools and context menu")
	flag.Parse()

	platformInit()
	wait := time.Duration(0)
	if *afterUpdate {
		wait = 20 * time.Second
	}
	if !acquireInstanceLock(wait) {
		// Dilreet is already running: bring it to the front and leave.
		if err := signalRunningInstance(); err != nil {
			showInfo("Dilreet is already running. Look for its window in the taskbar.")
		}
		return
	}

	if err := loadConfig(); err != nil {
		log.Printf("warning: could not read config: %v", err)
	}
	cleanupOldBinary()
	cleanupStaleWorkspaces()
	ensureProjects() // gives sites from older versions a project folder

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(*port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			showInfo("Dilreet could not start its local server: " + err.Error())
			log.Fatal(err)
		}
	}
	listenAddr = ln.Addr().String()
	httpServer = &http.Server{Handler: routes(), ReadHeaderTimeout: 20 * time.Second}
	uiURL = fmt.Sprintf("http://%s/?k=%s", listenAddr, sessionToken)

	// Decide between the native window and the browser fallback.
	appMode = modeBrowser
	if isNativePlatform() && !*forceBrowser {
		ver, verr := webView2Status()
		if ver != "" {
			appMode = modeNative
		} else {
			detail := ""
			if verr != nil {
				detail = " (" + verr.Error() + ")"
			}
			switch askNoWebView2(detail) {
			case "download":
				openBrowser(webview2DownloadURL)
				appModeReason = "WebView2 is not installed; using the browser until it is."
			case "browser":
				appModeReason = "WebView2 is not installed; you chose browser mode."
			default:
				return
			}
		}
	} else if *forceBrowser {
		appModeReason = "started with -browser"
	}

	p, _ := configPath()
	fmt.Printf("Dilreet %s (%s, %s mode)\nConfig: %s\n", Version, osName(), appMode, p)
	if *printToken && appMode == modeBrowser {
		fmt.Printf("Open: %s\n", uiURL)
	}
	writeInstanceFile()
	if envOr("NO_BACKGROUND") == "" {
		startBackground(context.Background())
	}
	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
			shutdown()
		}
	}()

	if appMode == modeNative {
		if err := runNativeWindow(uiURL, *debug); err != nil {
			log.Printf("native window failed: %v; falling back to the browser", err)
			appMode, appModeReason = modeBrowser, "the Dilreet window could not be created: "+err.Error()
		} else {
			shutdown() // window closed by the user
			return
		}
	}
	// Browser mode.
	if hasConsole() {
		fmt.Println("Keep this window open while you use Dilreet. Close it (or click Quit in the app) to stop.")
	} else {
		go idleWatchdog(15 * time.Minute)
	}
	if !*noBrowser {
		go func() { time.Sleep(300 * time.Millisecond); openBrowser(uiURL) }()
	}
	select {}
}

var shutdownOnce sync.Once

func shutdown() {
	shutdownOnce.Do(func() {
		if closeNativeWindow() {
			time.Sleep(400 * time.Millisecond) // let the window save its placement
		}
		sshS.close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stopTest()
		stopOllamaPull()
		for _, id := range stagedIDs() {
			dropStaged(id)
		}
		_ = httpServer.Shutdown(ctx)
		removeInstanceFile()
		os.Exit(0)
	})
	select {} // another goroutine is shutting down
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("Could not open the browser automatically; open the address above manually.")
	}
}

// ---------- HTTP plumbing ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, errNotConnected) {
		code = http.StatusConflict
	}
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// guard protects the API: only loopback Host headers (stops DNS rebinding),
// a per-run session cookie (SameSite=Strict) and a custom header on writes
// (stops cross-site requests from other web pages).
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		// The UI must never be framed (clickjacking) and only loads its own code.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-src http://localhost:* http://127.0.0.1:*; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/__hp/focus" && r.Method == http.MethodPost {
			handleFocus(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// Requests must come from Dilreet's own page, never from another
			// site or from a page shown in the App Testing preview.
			if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-site request blocked"})
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
				return
			}
			ck, err := r.Cookie("hp_session")
			if err != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(sessionToken)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "session expired: close this tab and use the address printed in the Dilreet window"})
				return
			}
			if r.Method != http.MethodGet && r.Header.Get("X-Dilreet") != "1" {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "missing X-Dilreet header"})
				return
			}
			lastUIContact.Store(time.Now().Unix())
		}
		next.ServeHTTP(w, r)
	})
}

func routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(static))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if k := r.URL.Query().Get("k"); k != "" {
			if subtle.ConstantTimeCompare([]byte(k), []byte(sessionToken)) == 1 {
				http.SetCookie(w, &http.Cookie{Name: "hp_session", Value: sessionToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			}
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, r)
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/icon.png", http.StatusMovedPermanently)
	})

	mux.HandleFunc("GET /api/state", apiState)
	registerAutomationRoutes(mux)
	registerTestRoutes(mux)
	registerProjectRoutes(mux)
	registerRegistrarRoutes(mux)
	mux.HandleFunc("POST /api/server", apiSaveServer)
	mux.HandleFunc("POST /api/ssh/connect", apiSSHConnect)
	mux.HandleFunc("POST /api/ssh/disconnect", func(w http.ResponseWriter, r *http.Request) {
		sshS.close()
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/ssh/forget-hostkey", func(w http.ResponseWriter, r *http.Request) {
		_ = withConfig(func(c *Config) error { c.HostKey = ""; return nil })
		sshS.close()
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/install", func(w http.ResponseWriter, r *http.Request) {
		if !sshS.connected() {
			writeErr(w, errNotConnected)
			return
		}
		var o SetupOptions
		o.RememberKey = true
		_ = readJSON(r, &o) // body is optional
		j := newJob("install", func(j *Job) error { return setupServerJob(j, o) })
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
	mux.HandleFunc("POST /api/coolify", apiSaveCoolify)
	mux.HandleFunc("GET /api/coolify/test", apiCoolifyTest)
	mux.HandleFunc("GET /api/coolify/projects", func(w http.ResponseWriter, r *http.Request) {
		api, err := newCoolify()
		if err != nil {
			writeErr(w, err)
			return
		}
		p, err := api.Projects()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("POST /api/coolify/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if !sshS.connected() {
			writeErr(w, errNotConnected)
			return
		}
		j := newJob("coolify-upgrade", func(j *Job) error { return installCoolifyJob(j, true) })
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
	mux.HandleFunc("GET /api/coolify/latest", func(w http.ResponseWriter, r *http.Request) {
		v, err := latestCoolifyVersion()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"latest": v})
	})

	mux.HandleFunc("GET /api/sites", apiSites)
	mux.HandleFunc("POST /api/sites", apiCreateSite)
	mux.HandleFunc("DELETE /api/sites/{slug}", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		keep := r.URL.Query().Get("keep_files") == "1"
		j := newJob("delete", func(j *Job) error { return deleteSiteJob(j, slug, keep) })
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
	mux.HandleFunc("POST /api/sites/{slug}/deploy", apiDeploy)
	mux.HandleFunc("GET /api/sites/{slug}/releases", func(w http.ResponseWriter, r *http.Request) {
		rel, cur, err := listReleases(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"releases": rel, "current": cur})
	})
	mux.HandleFunc("POST /api/sites/{slug}/rollback", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Release string }
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if err := activateRelease(r.PathValue("slug"), req.Release); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/sites/{slug}/restart", func(w http.ResponseWriter, r *http.Request) {
		s, err := siteOr404(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		api, err := newCoolify()
		if err == nil {
			err = api.Deploy(s.AppUUID)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/sites/{slug}/domain", apiSetDomain)
	mux.HandleFunc("GET /api/sites/{slug}/dns", apiDNSCheck)
	mux.HandleFunc("POST /api/sites/{slug}/domain/apply", apiApplyDomain)

	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j := getJob(r.PathValue("id"))
		if j == nil {
			writeJSON(w, 404, map[string]any{"error": "no such job"})
			return
		}
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		writeJSON(w, 200, j.snapshot(from))
	})

	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ManifestURL string `json:"manifest_url"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		req.ManifestURL = strings.TrimSpace(req.ManifestURL)
		if req.ManifestURL != "" {
			if err := allowedUpdateURL(req.ManifestURL); err != nil {
				writeErr(w, err)
				return
			}
		}
		_ = withConfig(func(c *Config) error { c.UpdateManifestURL = req.ManifestURL; return nil })
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/update/check", func(w http.ResponseWriter, r *http.Request) {
		m, err := fetchManifest()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"current": Version, "manifest": m, "newer": compareVersions(m.Version, Version) > 0})
	})
	mux.HandleFunc("POST /api/update/apply", func(w http.ResponseWriter, r *http.Request) {
		j := newJob("self-update", selfUpdateJob)
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
		go func() { time.Sleep(300 * time.Millisecond); shutdown() }()
	})
	return guard(mux)
}

// ---------- handlers ----------

func apiState(w http.ResponseWriter, r *http.Request) {
	c := snapshotConfig()
	fp := ""
	if c.HostKey != "" {
		fp = hostKeyFingerprint(c.HostKey)
	}
	p, _ := configPath()
	writeJSON(w, 200, map[string]any{
		"version":       Version,
		"os":            osName(),
		"config_path":   p,
		"server_ip":     c.ServerIP,
		"ssh_port":      c.SSHPort,
		"ssh_user":      c.SSHUser,
		"ssh_key_path":  c.SSHKeyPath,
		"host_key_fp":   fp,
		"ssh_connected": sshS.connected(),
		"coolify_url":   c.CoolifyURL,
		"token_set":     c.CoolifyToken != "",
		"token_storage": secretStorageNote,
		"api_via_ssh":   c.APIViaSSH,
		"server_uuid":   c.ServerUUID,
		"manifest_url":  manifestURL(),
		"site_count":    len(c.Sites),
	})
}

func apiSaveServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP      string `json:"ip"`
		Port    int    `json:"port"`
		User    string `json:"user"`
		KeyPath string `json:"key_path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	req.IP = strings.TrimSpace(req.IP)
	if net.ParseIP(req.IP) == nil {
		writeErr(w, errors.New("enter the server's public IPv4 address, e.g. 203.0.113.10"))
		return
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if strings.TrimSpace(req.User) == "" {
		req.User = "root"
	}
	req.KeyPath = strings.Trim(strings.TrimSpace(req.KeyPath), `"`)
	if req.KeyPath != "" {
		if _, err := os.Stat(req.KeyPath); err != nil {
			writeErr(w, fmt.Errorf("key file not found: %s", req.KeyPath))
			return
		}
	}
	_ = withConfig(func(c *Config) error {
		if c.ServerIP != req.IP {
			c.HostKey = "" // new server: pin its key on next connect
		}
		c.ServerIP, c.SSHPort, c.SSHUser, c.SSHKeyPath = req.IP, req.Port, strings.TrimSpace(req.User), req.KeyPath
		if c.CoolifyURL == "" || strings.Contains(c.CoolifyURL, ":8000") {
			c.CoolifyURL = "http://" + req.IP + ":8000"
		}
		return nil
	})
	sshS.close()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func apiSSHConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password   string `json:"password"`
		Passphrase string `json:"passphrase"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	fp, first, err := connectSSH(req.Password, req.Passphrase)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if out, err := runRemoteOutput(ctx, "true"); err != nil {
		_, user, _, _ := sshS.get()
		sshS.close()
		writeErr(w, fmt.Errorf("logged in as %q, but could not get root rights with sudo (%v %s). Log in as root, or enter this user's password (used for sudo, kept in memory only), or allow passwordless sudo for this user", user, err, strings.TrimSpace(out)))
		return
	}
	info, _ := runRemoteOutput(ctx, `echo "OS: $(. /etc/os-release 2>/dev/null; echo $PRETTY_NAME)"; echo "CPU cores: $(nproc)"; echo "RAM: $(free -m | awk '/Mem:/{print $2}') MB"; echo "Free disk: $(df -BG / | awk 'NR==2{print $4}')"; if docker ps --format '{{.Names}}' 2>/dev/null | grep -qx coolify; then echo "Coolify: installed and running"; else echo "Coolify: not running/not installed"; fi`)
	writeJSON(w, 200, map[string]any{"ok": true, "fingerprint": fp, "first_time": first, "info": info})
}

func apiSaveCoolify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL       string `json:"url"`
		Token     string `json:"token"`
		APIViaSSH bool   `json:"api_via_ssh"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	req.URL = strings.TrimRight(strings.TrimSpace(req.URL), "/")
	req.URL = strings.TrimSuffix(req.URL, "/api/v1")
	if req.URL != "" && !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		req.URL = "http://" + req.URL
	}
	var enc string
	if t := strings.TrimSpace(req.Token); t != "" {
		var err error
		if enc, err = protectString(t); err != nil {
			writeErr(w, fmt.Errorf("could not encrypt token: %w", err))
			return
		}
	}
	_ = withConfig(func(c *Config) error {
		if req.URL != "" {
			c.CoolifyURL = req.URL
		}
		if enc != "" {
			c.CoolifyToken = enc
		}
		c.APIViaSSH = req.APIViaSSH
		return nil
	})
	apiCoolifyTest(w, r)
}

func apiCoolifyTest(w http.ResponseWriter, r *http.Request) {
	api, err := newCoolify()
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := api.Version()
	if err != nil {
		writeErr(w, err)
		return
	}
	srv, err := api.Servers()
	if err != nil {
		writeErr(w, err)
		return
	}
	c := snapshotConfig()
	if c.ServerUUID == "" && len(srv) > 0 {
		_ = withConfig(func(c *Config) error { c.ServerUUID = srv[0].UUID; return nil })
	}
	writeJSON(w, 200, map[string]any{"ok": true, "version": v, "servers": srv})
}

func siteOr404(slug string) (*Site, error) {
	c := snapshotConfig()
	s := findSite(&c, slug)
	if s == nil {
		return nil, fmt.Errorf("unknown site %q", slug)
	}
	return s, nil
}

func apiSites(w http.ResponseWriter, r *http.Request) {
	c := snapshotConfig()
	type row struct {
		*Site
		Status string `json:"status"`
		FQDN   string `json:"fqdn"`
	}
	rows := make([]row, 0, len(c.Sites))
	status := map[string]CApp{}
	apiErr := ""
	if api, err := newCoolify(); err == nil {
		if apps, err := api.Applications(); err == nil {
			for _, a := range apps {
				status[a.UUID] = a
			}
		} else {
			apiErr = err.Error()
		}
	} else {
		apiErr = err.Error()
	}
	for _, s := range c.Sites {
		a, ok := status[s.AppUUID]
		st := "unknown"
		if ok {
			st = a.Status
		} else if apiErr == "" {
			st = "missing in Coolify"
		}
		rows = append(rows, row{Site: s, Status: st, FQDN: a.FQDN})
	}
	writeJSON(w, 200, map[string]any{"sites": rows, "api_error": apiErr, "server_ip": c.ServerIP})
}

func apiCreateSite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Slug == "" {
		req.Slug = slugify(req.Name)
	}
	if req.Name == "" || !validSlug(req.Slug) {
		writeErr(w, errors.New("enter a site name; the id may only contain a-z, 0-9 and '-' (max 40)"))
		return
	}
	if c := snapshotConfig(); findSite(&c, req.Slug) != nil {
		writeErr(w, fmt.Errorf("a site with the id %q already exists; choose another id", req.Slug))
		return
	}
	if !sshS.connected() {
		writeErr(w, errNotConnected)
		return
	}
	j := newJob("create", func(j *Job) error { return createSiteJob(j, req.Name, req.Slug) })
	writeJSON(w, 200, map[string]any{"job": j.ID, "slug": req.Slug})
}

// apiDeploy accepts multipart: a single .zip ("zip"), or a folder upload
// ("files" + parallel "paths"), or a local path on this PC ("local_path").
// siteUpload reads a site from a multipart request: local_path (folder or
// .zip on this PC), a "zip" file, or a folder upload ("files" + "paths").
// It unpacks it into a Workspace; the caller owns (and must Close) it.
func siteUpload(w http.ResponseWriter, r *http.Request) (*Workspace, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 3<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	defer r.MultipartForm.RemoveAll()

	var entries []srcEntry
	var cleanup func()
	var err error
	source, localPath := "", ""
	if lp := strings.Trim(strings.TrimSpace(r.FormValue("local_path")), `"`); lp != "" {
		st, e := os.Stat(lp)
		switch {
		case e != nil:
			err = fmt.Errorf("path not found: %s", lp)
		case st.IsDir():
			entries, err = dirEntries(lp)
		case strings.EqualFold(filepath.Ext(lp), ".zip"):
			entries, cleanup, err = zipEntries(lp)
		default:
			err = errors.New("local path must be a folder or a .zip file")
		}
		source, localPath = lp, lp
	} else if fhs := r.MultipartForm.File["zip"]; len(fhs) == 1 {
		tmp, e := saveUpload(fhs[0])
		if e != nil {
			return nil, e
		}
		defer os.Remove(tmp)
		entries, cleanup, err = zipEntries(tmp)
		source = "uploaded zip " + filepath.Base(fhs[0].Filename)
	} else if fhs := r.MultipartForm.File["files"]; len(fhs) > 0 {
		paths := r.MultipartForm.Value["paths"]
		if len(paths) != len(fhs) {
			return nil, errors.New("folder upload is missing file paths")
		}
		for i, fh := range fhs {
			rel, e := cleanRel(paths[i])
			if e != nil {
				return nil, e
			}
			if rel == "" {
				continue
			}
			fh := fh
			entries = append(entries, srcEntry{Path: rel, Size: fh.Size, Open: func() (io.ReadCloser, error) { return fh.Open() }})
		}
		source = "uploaded folder"
		if len(paths) > 0 {
			if top, _, ok := strings.Cut(strings.TrimPrefix(paths[0], "/"), "/"); ok {
				source = "uploaded folder " + top
			}
		}
	} else {
		err = errors.New("choose a .zip file or a folder to upload")
	}
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, err
	}
	ws, err := newWorkspace(entries, source)
	if err != nil {
		return nil, err
	}
	ws.LocalPath = localPath
	return ws, nil
}

func apiDeploy(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if _, err := siteOr404(slug); err != nil {
		writeErr(w, err)
		return
	}
	if !sshS.connected() {
		writeErr(w, errNotConnected)
		return
	}
	ws, err := siteUpload(w, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	j := newJob("deploy", func(j *Job) error {
		defer ws.Close()
		rep := runChecks(ws)
		j.Logf("Checks: %d passed, %d warnings, %d failed (open App Testing for details and a live preview).", rep.Counts["pass"], rep.Counts["warn"], rep.Counts["fail"])
		return deployWorkspace(j, slug, ws)
	})
	writeJSON(w, 200, map[string]any{"job": j.ID})
}

func saveUpload(fh *multipart.FileHeader) (string, error) {
	src, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	f, err := os.CreateTemp("", "dilreet-upload-*.zip")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), f.Close()
}

func apiSetDomain(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var req struct {
		Domain string `json:"domain"`
		WWW    bool   `json:"www"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	d, err := normalizeDomain(req.Domain)
	if err != nil {
		writeErr(w, err)
		return
	}
	if strings.HasPrefix(d, "www.") {
		d = strings.TrimPrefix(d, "www.")
		req.WWW = true
	}
	c := snapshotConfig()
	for _, s := range c.Sites {
		if s.Slug != slug && s.Domain == d {
			writeErr(w, fmt.Errorf("%s is already used by site %q", d, s.Name))
			return
		}
	}
	err = withConfig(func(c *Config) error {
		s := findSite(c, slug)
		if s == nil {
			return errors.New("unknown site")
		}
		s.Domain, s.IncludeWWW, s.DomainLive = d, req.WWW, false
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"domain": d, "records": requiredRecords(d, req.WWW, c.ServerIP)})
}

func apiDNSCheck(w http.ResponseWriter, r *http.Request) {
	s, err := siteOr404(r.PathValue("slug"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.Domain == "" {
		writeErr(w, errors.New("no domain set for this site"))
		return
	}
	ip := snapshotConfig().ServerIP
	hosts := []string{s.Domain}
	if s.IncludeWWW {
		hosts = append(hosts, "www."+s.Domain)
	}
	var checks []HostCheck
	all := true
	for _, h := range hosts {
		hc := checkHost(h, ip)
		all = all && hc.OK
		checks = append(checks, hc)
	}
	writeJSON(w, 200, map[string]any{"ok": all, "checks": checks, "records": requiredRecords(s.Domain, s.IncludeWWW, ip), "domain_live": s.DomainLive})
}

func apiApplyDomain(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var req struct {
		Force   bool `json:"force"`    // Coolify force_domain_override
		SkipDNS bool `json:"skip_dns"` // apply even if DNS does not point here yet
	}
	_ = readJSON(r, &req)
	s, err := siteOr404(slug)
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.Domain == "" {
		writeErr(w, errors.New("no domain set"))
		return
	}
	if !req.SkipDNS {
		ip := snapshotConfig().ServerIP
		hosts := []string{s.Domain}
		if s.IncludeWWW {
			hosts = append(hosts, "www."+s.Domain)
		}
		for _, h := range hosts {
			if hc := checkHost(h, ip); !hc.OK {
				writeErr(w, fmt.Errorf("DNS not ready: %s - %s", h, hc.Message))
				return
			}
		}
	}
	api, err := newCoolify()
	if err != nil {
		writeErr(w, err)
		return
	}
	domains := "https://" + s.Domain
	if s.IncludeWWW {
		domains += ",https://www." + s.Domain
	}
	if s.InstantURL != "" {
		domains += "," + s.InstantURL // keep the free address working too
	}
	if err := api.SetDomains(s.AppUUID, domains, req.Force); err != nil {
		writeErr(w, err)
		return
	}
	// Redeploy so Coolify regenerates the proxy (Traefik) labels; the proxy
	// then requests a Let's Encrypt certificate automatically.
	if err := api.Deploy(s.AppUUID); err != nil {
		writeErr(w, err)
		return
	}
	_ = withConfig(func(c *Config) error {
		if x := findSite(c, slug); x != nil {
			x.DomainLive, x.PendingNS = true, nil
		}
		return nil
	})
	writeJSON(w, 200, map[string]any{"ok": true, "domains": domains})
}
