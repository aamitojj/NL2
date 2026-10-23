package main

// Background automation while Dilreet runs (Settings > "Background checks",
// on by default): reconnect with Dilreet's own key, finish custom domains
// once DNS resolves, notice when HTTPS works, health checks, update checks,
// watch folders and auto-deploy on new commits. Plus the notification centre
// and the extra automation routes.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------- notifications ----------

func notify(level, site, text string) {
	_ = withConfig(func(c *Config) error {
		c.Notices = append(c.Notices, Notice{ID: randHex(6), Time: time.Now().UTC().Format(time.RFC3339), Level: level, Text: redact(text, false), Site: site})
		if n := len(c.Notices); n > 100 {
			c.Notices = c.Notices[n-100:]
		}
		return nil
	})
}

func backgroundOn(c *Config) bool { return c.BackgroundChecks == nil || *c.BackgroundChecks }

// ---------- loop ----------

type bgState struct {
	mu           sync.Mutex
	lastConnect  time.Time
	lastDomain   map[string]time.Time
	health       map[string]bool // last known up/down
	lastHealth   time.Time
	lastUpdate   time.Time
	notifiedVer  string
	watchSig     map[string]string
	watchPending map[string]string
	watchBusy    map[string]bool
}

var bg = &bgState{lastDomain: map[string]time.Time{}, health: map[string]bool{}, watchSig: map[string]string{}, watchPending: map[string]string{}, watchBusy: map[string]bool{}}

func startBackground(ctx context.Context) {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		time.Sleep(5 * time.Second)
		for {
			backgroundTick(ctx, time.Now())
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func backgroundTick(ctx context.Context, now time.Time) {
	defer func() { _ = recover() }() // a bug in a check must never take the app down
	expireStaged()
	c := snapshotConfig()
	if !backgroundOn(&c) {
		return
	}
	bgReconnect(&c, now)
	commitPollTick(ctx, now)
	domainTick(now)
	httpsTick()
	watchTick()
	if now.Sub(bg.lastHealth) > 10*time.Minute {
		bg.lastHealth = now
		healthTick()
	}
	if now.Sub(bg.lastUpdate) > 24*time.Hour {
		bg.lastUpdate = now
		if m, err := fetchManifest(); err == nil && compareVersions(m.Version, Version) > 0 && bg.notifiedVer != m.Version {
			bg.notifiedVer = m.Version
			notify("info", "", fmt.Sprintf("Dilreet %s is available. Open Settings > Updates to install it.", m.Version))
		}
	}
}

// bgReconnect re-opens SSH with Dilreet's own key (never a password).
func bgReconnect(c *Config, now time.Time) {
	if sshS.connected() || c.ManagedKey == "" || c.ServerIP == "" || c.HostKey == "" || c.SSHKeyPath != "" {
		return
	}
	if now.Sub(bg.lastConnect) < 2*time.Minute {
		return
	}
	bg.lastConnect = now
	_, _, _ = connectSSH("", "")
}

// applyDomainNow points the site's Coolify app at its domain(s) and redeploys
// so Traefik requests the Let's Encrypt certificate.
func applyDomainNow(slug string, force bool) (string, error) {
	s, err := siteOr404(slug)
	if err != nil {
		return "", err
	}
	if s.Domain == "" {
		return "", errors.New("no domain set")
	}
	api, err := newCoolify()
	if err != nil {
		return "", err
	}
	domains := "https://" + s.Domain
	if s.IncludeWWW {
		domains += ",https://www." + s.Domain
	}
	if s.InstantURL != "" {
		domains += "," + s.InstantURL
	}
	if err := retry(context.Background(), 3, 2*time.Second, func() error { return api.SetDomains(s.AppUUID, domains, force) }); err != nil {
		return "", err
	}
	if err := api.Deploy(s.AppUUID); err != nil {
		return "", err
	}
	_ = withConfig(func(c *Config) error {
		if x := findSite(c, slug); x != nil {
			x.DomainLive, x.PendingNS = true, nil
		}
		return nil
	})
	return domains, nil
}

func siteHosts(s *Site) []string {
	h := []string{s.Domain}
	if s.IncludeWWW {
		h = append(h, "www."+s.Domain)
	}
	return h
}

func domainTick(now time.Time) {
	c := snapshotConfig()
	if c.CoolifyToken == "" {
		return
	}
	for _, s := range c.Sites {
		if s.Domain == "" || s.DomainLive {
			continue
		}
		bg.mu.Lock()
		recent := now.Sub(bg.lastDomain[s.Slug]) < time.Minute
		if !recent {
			bg.lastDomain[s.Slug] = now
		}
		bg.mu.Unlock()
		if recent {
			continue
		}
		ok := true
		for _, h := range siteHosts(s) {
			if hc := checkHost(h, c.ServerIP); !hc.OK {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if _, err := applyDomainNow(s.Slug, false); err == nil {
			notify("success", s.Slug, fmt.Sprintf("DNS for %s is ready. The site is being switched to it; HTTPS follows in a minute or two.", s.Domain))
		}
	}
}

var tlsCheck = func(host string) error {
	d := &net.Dialer{Timeout: 6 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", host+":443", &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	return conn.Close()
}

func httpsTick() {
	c := snapshotConfig()
	for _, s := range c.Sites {
		if s.Domain == "" || !s.DomainLive || s.HTTPSActive {
			continue
		}
		if err := tlsCheck(s.Domain); err != nil {
			continue
		}
		slug := s.Slug
		_ = withConfig(func(c *Config) error {
			if x := findSite(c, slug); x != nil {
				x.HTTPSActive = true
			}
			return nil
		})
		notify("success", slug, fmt.Sprintf("https://%s is live with a valid certificate.", s.Domain))
		if s.DNSMode == "cloudflare" && s.CFProxy && c.CloudflareToken != "" && s.CFZoneID != "" {
			cf := newCF(secret(c.CloudflareToken))
			for _, id := range s.CFRecordIDs {
				_, _ = cf.Patch(s.CFZoneID, id, map[string]any{"proxied": true})
			}
		}
	}
}

var healthGet = func(u string) error {
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(u)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func healthTick() {
	c := snapshotConfig()
	for _, s := range c.Sites {
		u := s.InstantURL
		if s.HTTPSActive {
			u = "https://" + s.Domain
		}
		if u == "" || s.LastDeploy == "" {
			continue
		}
		err := healthGet(u)
		up := err == nil
		prev, known := bg.health[s.Slug]
		bg.health[s.Slug] = up
		if known && prev && !up {
			notify("warn", s.Slug, fmt.Sprintf("%s is not responding (%v). Open the site and click Restart, or ask the assistant.", u, err))
		} else if known && !prev && up {
			notify("success", s.Slug, u+" is back online.")
		}
	}
}

// ---------- watch folders ----------

func folderSignature(root string) (string, error) {
	var n, total int64
	var latest time.Time
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			n++
			total += fi.Size()
			if fi.ModTime().After(latest) {
				latest = fi.ModTime()
			}
		}
		return nil
	})
	return fmt.Sprintf("%d/%d/%d", n, total, latest.UnixNano()), err
}

// watchTick redeploys a watched folder after it changed and then stayed
// unchanged for one tick (so half-copied files are not published).
func watchTick() {
	c := snapshotConfig()
	for _, s := range c.Sites {
		if !s.Watch || s.WatchPath == "" {
			continue
		}
		sig, err := folderSignature(s.WatchPath)
		if err != nil {
			continue
		}
		if watchDecide(s, sig) {
			startWatchDeploy(s.Slug, s.WatchPath)
		}
	}
}

func watchDecide(s *Site, sig string) bool {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	{
		if bg.watchBusy[s.Slug] {
			return false
		}
		last, seen := bg.watchSig[s.Slug]
		if !seen {
			bg.watchSig[s.Slug] = sig
			return false
		}
		if sig == last {
			delete(bg.watchPending, s.Slug)
			return false
		}
		if bg.watchPending[s.Slug] != sig {
			bg.watchPending[s.Slug] = sig
			return false
		}
		if !sshS.connected() {
			return false
		}
		delete(bg.watchPending, s.Slug)
		bg.watchSig[s.Slug] = sig
		bg.watchBusy[s.Slug] = true
		return true
	}
}

func startWatchDeploy(slug, path string) {
	{
		newJob("watch-deploy", func(j *Job) error {
			defer func() { bg.mu.Lock(); delete(bg.watchBusy, slug); bg.mu.Unlock() }()
			entries, err := dirEntries(path)
			if err == nil {
				var ws *Workspace
				if ws, err = newWorkspace(entries, path); err == nil {
					defer ws.Close()
					ws.LocalPath = path
					err = deployWorkspace(j, slug, ws)
				}
			}
			if err != nil {
				notify("warn", slug, "Watch folder deploy failed: "+err.Error())
			} else {
				notify("success", slug, "Changes in "+path+" were published.")
			}
			return err
		})
	}
}

// ---------- routes ----------

func registerAutomationRoutes(mux *http.ServeMux) {
	registerAIRoutes(mux)
	registerImportRoutes(mux)

	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		unread := 0
		for _, n := range c.Notices {
			if !n.Read {
				unread++
			}
		}
		stage := "ready"
		switch {
		case c.ServerIP == "":
			stage = "no_server"
		case c.CoolifyToken == "" && c.CoolifyURL == "":
			stage = "no_engine"
		case !sshS.connected():
			stage = "disconnected"
		}
		writeJSON(w, 200, map[string]any{
			"version": Version, "app_mode": appMode, "app_mode_reason": appModeReason, "stage": stage,
			"server_ip": c.ServerIP, "ssh_connected": sshS.connected(), "managed_key": c.ManagedKey != "", "uses_key_file": c.SSHKeyPath != "",
			"coolify_url": c.CoolifyURL, "token_set": c.CoolifyToken != "", "admin_email": c.AdminEmail, "admin_password_set": c.AdminPassword != "",
			"cloudflare": c.CloudflareToken != "", "background_checks": backgroundOn(&c), "auto_security_updates": c.AutoSecurityUpd,
			"provider": c.Provider, "site_count": len(c.Sites), "unread": unread, "token_storage": secretStorageNote,
			"git_accounts": len(c.GitAccounts),
		})
	})
	mux.HandleFunc("GET /api/notices", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		out := make([]Notice, 0, len(c.Notices))
		for i := len(c.Notices) - 1; i >= 0; i-- {
			out = append(out, c.Notices[i])
		}
		writeJSON(w, 200, map[string]any{"notices": out})
	})
	mux.HandleFunc("POST /api/notices/read", func(w http.ResponseWriter, r *http.Request) {
		_ = withConfig(func(c *Config) error {
			for i := range c.Notices {
				c.Notices[i].Read = true
			}
			return nil
		})
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("DELETE /api/notices", func(w http.ResponseWriter, r *http.Request) {
		_ = withConfig(func(c *Config) error { c.Notices = nil; return nil })
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/background", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		_ = withConfig(func(c *Config) error { c.BackgroundChecks = &req.Enabled; return nil })
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	// Coolify admin login created by Dilreet (shown only on request, local UI only).
	mux.HandleFunc("POST /api/coolify/admin", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		if c.AdminPassword == "" {
			writeErr(w, errors.New("Dilreet did not create the Coolify admin account on this server"))
			return
		}
		writeJSON(w, 200, map[string]any{"email": c.AdminEmail, "password": secret(c.AdminPassword), "url": c.CoolifyURL})
	})

	// Cloudflare (optional automatic DNS)
	mux.HandleFunc("POST /api/cloudflare", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Token string }
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		cf := newCF(req.Token)
		if err := cf.Verify(); err != nil {
			writeErr(w, err)
			return
		}
		enc, err := protectString(strings.TrimSpace(req.Token))
		if err != nil {
			writeErr(w, err)
			return
		}
		_ = withConfig(func(c *Config) error { c.CloudflareToken = enc; return nil })
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("DELETE /api/cloudflare", func(w http.ResponseWriter, r *http.Request) {
		_ = withConfig(func(c *Config) error { c.CloudflareToken = ""; return nil })
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/dns/host", func(w http.ResponseWriter, r *http.Request) {
		d, err := normalizeDomain(r.URL.Query().Get("domain"))
		if err != nil {
			writeErr(w, err)
			return
		}
		h := detectDNSHost(d)
		c := snapshotConfig()
		writeJSON(w, 200, map[string]any{"host": h, "guide": guideFor(h.ID, d), "registrars": registrarPublic(&c), "cloudflare_connected": c.CloudflareToken != "", "cf_token_url": cfTokenURL})
	})
	mux.HandleFunc("POST /api/sites/{slug}/auto-dns", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		var req struct {
			Domain  string `json:"domain"`
			WWW     bool   `json:"www"`
			Replace bool   `json:"replace"`
			Proxy   bool   `json:"proxy"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		c := snapshotConfig()
		if c.CloudflareToken == "" {
			writeErr(w, errors.New("connect Cloudflare first"))
			return
		}
		if _, err := siteOr404(slug); err != nil {
			writeErr(w, err)
			return
		}
		d, err := normalizeDomain(req.Domain)
		if err != nil {
			writeErr(w, err)
			return
		}
		if strings.HasPrefix(d, "www.") {
			d, req.WWW = strings.TrimPrefix(d, "www."), true
		}
		for _, s := range c.Sites {
			if s.Slug != slug && s.Domain == d {
				writeErr(w, fmt.Errorf("%s is already used by site %q", d, s.Name))
				return
			}
		}
		cf := newCF(secret(c.CloudflareToken))
		zone, zoneName, err := cf.FindZone(d)
		if err != nil {
			writeErr(w, err)
			return
		}
		hosts := []string{d}
		if req.WWW {
			hosts = append(hosts, "www."+d)
		}
		var ids []string
		var conflicts []CFConflict
		for _, h := range hosts {
			id, cfl, err := cf.UpsertA(zone, h, c.ServerIP, req.Replace)
			if errors.Is(err, errCFConflict) {
				conflicts = append(conflicts, cfl...)
				continue
			}
			if err != nil {
				writeErr(w, err)
				return
			}
			ids = append(ids, id)
		}
		if len(conflicts) > 0 {
			writeJSON(w, 409, map[string]any{"error": "existing DNS records would be replaced", "conflicts": conflicts})
			return
		}
		_ = withConfig(func(c *Config) error {
			if s := findSite(c, slug); s != nil {
				s.Domain, s.IncludeWWW, s.DomainLive, s.HTTPSActive = d, req.WWW, false, false
				s.DNSMode, s.CFZoneID, s.CFRecordIDs, s.CFProxy = "cloudflare", zone, ids, req.Proxy
			}
			return nil
		})
		bg.mu.Lock()
		delete(bg.lastDomain, slug)
		bg.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "zone": zoneName, "records": len(ids)})
	})
	mux.HandleFunc("POST /api/sites/{slug}/watch", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		var req struct {
			Enabled bool   `json:"enabled"`
			Path    string `json:"path"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		p := strings.Trim(strings.TrimSpace(req.Path), `"`)
		if req.Enabled {
			st, err := os.Stat(p)
			if err != nil || !st.IsDir() {
				writeErr(w, errors.New("enter the full path of a folder on this PC, e.g. C:\\Users\\you\\my-site\\dist"))
				return
			}
		}
		err := withConfig(func(c *Config) error {
			s := findSite(c, slug)
			if s == nil {
				return errors.New("site not found")
			}
			s.Watch, s.WatchPath = req.Enabled, p
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		bg.mu.Lock()
		delete(bg.watchSig, slug)
		bg.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/sites/{slug}/rename", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name string }
		if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
			writeErr(w, errors.New("enter a name"))
			return
		}
		err := withConfig(func(c *Config) error {
			s := findSite(c, r.PathValue("slug"))
			if s == nil {
				return errors.New("site not found")
			}
			s.Name = strings.TrimSpace(req.Name)
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/sites/{slug}/domain/finish", func(w http.ResponseWriter, r *http.Request) {
		d, err := applyDomainNow(r.PathValue("slug"), false)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "domains": d})
	})

	// Cloud servers (token is used for this request only, never stored)
	mux.HandleFunc("POST /api/cloud/plans", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Kind, Token string }
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if req.Kind != "hetzner" && req.Kind != "digitalocean" {
			writeErr(w, errors.New("choose Hetzner or DigitalOcean"))
			return
		}
		plans, err := cloudProvider{req.Kind, strings.TrimSpace(req.Token)}.Plans(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"plans": plans, "token_url": cloudTokenURL(req.Kind)})
	})
	mux.HandleFunc("POST /api/cloud/create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind, Token, Plan, Region string
			AdminEmail                string `json:"admin_email"`
			AutoUpdates               bool   `json:"auto_updates"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if req.Kind != "hetzner" && req.Kind != "digitalocean" || req.Plan == "" {
			writeErr(w, errors.New("choose a provider and a plan"))
			return
		}
		o := SetupOptions{AdminEmail: req.AdminEmail, AutoUpdates: req.AutoUpdates, RememberKey: false}
		tok := strings.TrimSpace(req.Token)
		j := newJob("cloud-server", func(j *Job) error { return createCloudServerJob(j, req.Kind, tok, req.Plan, req.Region, o) })
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
}
