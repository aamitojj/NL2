package main

// The dashboard: what is live, what failed, what needs attention, plus the
// first-run checklist. Each problem comes with one "Fix it" suggestion; the
// UI asks before any fix that changes the server.

import (
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Fix struct {
	Label   string `json:"label"`
	Kind    string `json:"kind"` // href | restart | health | backup | pull | reconnect | redeploy
	Href    string `json:"href,omitempty"`
	Confirm bool   `json:"confirm,omitempty"` // changes the server: UI must ask first
}

type Attention struct {
	Level string `json:"level"` // fail | warn | info
	Slug  string `json:"slug,omitempty"`
	Name  string `json:"name,omitempty"`
	Title string `json:"title"`
	Text  string `json:"text"`
	Fix   *Fix   `json:"fix,omitempty"`
}

type SetupItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	Done     bool   `json:"done"`
	Href     string `json:"href"`
	Optional bool   `json:"optional,omitempty"`
}

// liveState classifies a Coolify status string in plain words.
func liveState(status string) string {
	s := strings.ToLower(status)
	switch {
	case strings.HasPrefix(s, "running"):
		return "live"
	case strings.Contains(s, "starting") || strings.Contains(s, "restarting") || strings.Contains(s, "deploying"):
		return "busy"
	case strings.HasPrefix(s, "exited") || strings.HasPrefix(s, "degraded") || strings.Contains(s, "missing") || strings.HasPrefix(s, "stopped"):
		return "down"
	}
	return "unknown"
}

func appStatuses(c *Config) (map[string]CApp, string) {
	status := map[string]CApp{}
	if c.ServerIP == "" {
		return status, "no server yet"
	}
	api, err := newCoolify()
	if err != nil {
		return status, err.Error()
	}
	apps, err := api.Applications()
	if err != nil {
		return status, err.Error()
	}
	for _, a := range apps {
		status[a.UUID] = a
	}
	return status, ""
}

func siteStatus(s *Site, status map[string]CApp, apiErr string) string {
	if a, ok := status[s.AppUUID]; ok {
		return a.Status
	}
	if apiErr == "" {
		return "missing in Coolify"
	}
	return "unknown"
}

func lastDeployOf(c *Config, s *Site) *DeployRecord {
	if s.Folder == "" {
		return nil
	}
	ix, err := readIndex(filepath.Join(projectsRoot(c), s.Folder))
	if err != nil || len(ix.Deploys) == 0 {
		return nil
	}
	return ix.Deploys[0]
}

func setupChecklist(c *Config) []SetupItem {
	anyDomain, httpsOn := false, false
	for _, s := range c.Sites {
		if s.DomainLive {
			anyDomain = true
		}
		if s.HTTPSActive {
			httpsOn = true
		}
	}
	b := backupSettings(c)
	return []SetupItem{
		{ID: "server", Title: "Connect your server", Text: "Your server (VPS) is the computer on the internet that shows your websites.", Done: c.ServerIP != "", Href: "#/setup"},
		{ID: "hosting", Title: "Install the hosting tools", Text: "Dilreet installs Coolify on the server for you. It runs your sites.", Done: c.CoolifyToken != "", Href: "#/setup"},
		{ID: "project", Title: "Put your first website online", Text: "Drag a folder in, or start from a ready-made template.", Done: len(c.Sites) > 0, Href: "#/new"},
		{ID: "domain", Title: "Use your own web address", Text: "Connect a name like myshop.in instead of the free test address.", Done: anyDomain, Href: "#/domains", Optional: true},
		{ID: "https", Title: "Secure padlock (HTTPS)", Text: "Turns on by itself after your web address works. Nothing to do.", Done: httpsOn, Href: "#/domains", Optional: true},
		{ID: "backup", Title: "Turn on automatic backups", Text: "Keeps a copy of all your sites in a folder you choose.", Done: b.Every != "off", Href: "#/settings/backups"},
		{ID: "ai", Title: "Connect Jiv, the helper (optional)", Text: "Ask questions in plain words and let Jiv explain problems.", Done: len(c.AIConns) > 0, Href: "#/settings/ai", Optional: true},
	}
}

func attentionItems(c *Config, status map[string]CApp, apiErr string, health map[string]HealthResult, connected bool, now time.Time) []Attention {
	var out []Attention
	if c.ServerIP != "" && !connected {
		out = append(out, Attention{Level: "warn", Title: "Not connected to your server", Text: "Dilreet cannot reach your server right now, so deploys and restarts will wait.", Fix: &Fix{Label: "Connect", Kind: "reconnect"}})
	}
	if c.ServerIP != "" && apiErr != "" && apiErr != "no server yet" {
		out = append(out, Attention{Level: "warn", Title: "Hosting tools are not answering", Text: "Coolify did not reply: " + apiErr, Fix: &Fix{Label: "Open server settings", Kind: "href", Href: "#/settings/server"}})
	}
	warn := sslWarnDays(c)
	for _, s := range c.Sites {
		if s.Archived {
			continue
		}
		base := Attention{Slug: s.Slug, Name: s.Name}
		add := func(level, title, text string, fix *Fix) {
			a := base
			a.Level, a.Title, a.Text, a.Fix = level, title, text, fix
			out = append(out, a)
		}
		if s.LastDeploy == "" {
			add("info", "Not online yet", "This project has no files online. Drop your files to publish it.", &Fix{Label: "Deploy files", Kind: "href", Href: "#/project/" + s.Slug})
			continue
		}
		if d := lastDeployOf(c, s); d != nil && d.Status == "failed" {
			add("fail", "Last deploy failed", firstNonEmpty(d.Error, "The last update did not go online. The previous version is still live."), &Fix{Label: "See what went wrong", Kind: "href", Href: "#/project/" + s.Slug + "/deploys"})
		}
		if apiErr == "" && liveState(siteStatus(s, status, apiErr)) == "down" {
			add("fail", "Site is stopped", "The web server for this site is not running.", &Fix{Label: "Restart it", Kind: "restart", Confirm: true})
		} else if h, ok := health[s.Slug]; ok && !h.OK {
			add("fail", "Site is not answering", h.URL+": "+h.Error, &Fix{Label: "Check again", Kind: "health"})
		}
		if h, ok := health[s.Slug]; ok && h.CertDays != nil && *h.CertDays <= warn {
			add("warn", "Padlock certificate ends soon", "The HTTPS certificate ends in "+strconv.Itoa(*h.CertDays)+" days. It normally renews by itself.", &Fix{Label: "Check again", Kind: "health"})
		}
		if s.Domain != "" && !s.DomainLive {
			add("warn", "Web address not working yet", s.Domain+" does not point to your server yet. This can take up to a few hours after you change it.", &Fix{Label: "Finish domain setup", Kind: "href", Href: "#/project/" + s.Slug + "/domain"})
		} else if s.Domain != "" && s.DomainLive && !s.HTTPSActive {
			add("info", "Waiting for the padlock (HTTPS)", s.Domain+" works. The secure certificate usually arrives within a few minutes.", nil)
		}
		if s.Git != nil && s.Git.LastError != "" {
			add("warn", "Could not get the latest code", s.Git.LastError, &Fix{Label: "Try again", Kind: "pull", Confirm: true})
		}
	}
	b := backupSettings(c)
	if len(c.Sites) > 0 && b.Every == "off" {
		out = append(out, Attention{Level: "info", Title: "Backups are off", Text: "Turn on automatic backups so you never lose your site files.", Fix: &Fix{Label: "Turn on", Kind: "href", Href: "#/settings/backups"}})
	} else if b.LastError != "" {
		out = append(out, Attention{Level: "warn", Title: "Last backup did not work", Text: b.LastError, Fix: &Fix{Label: "Back up now", Kind: "backup"}})
	}
	rank := map[string]int{"fail": 0, "warn": 1, "info": 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Level] < rank[out[j].Level] })
	return out
}

func registerDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/dashboard", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		status, apiErr := appStatuses(&c)
		health := healthSnapshot()
		counts := map[string]int{"total": 0, "live": 0, "down": 0, "not_deployed": 0, "domains": 0, "archived": 0}
		type recent struct {
			*DeployRecord
			Slug string `json:"slug"`
			Name string `json:"name"`
		}
		var rec []recent
		pinned := []map[string]any{}
		for _, s := range c.Sites {
			if s.Archived {
				counts["archived"]++
				continue
			}
			counts["total"]++
			st := siteStatus(s, status, apiErr)
			switch {
			case s.LastDeploy == "":
				counts["not_deployed"]++
			case liveState(st) == "live":
				counts["live"]++
			case liveState(st) == "down":
				counts["down"]++
			}
			if s.DomainLive {
				counts["domains"]++
			}
			if s.Folder != "" {
				if ix, err := readIndex(filepath.Join(projectsRoot(&c), s.Folder)); err == nil {
					for i, d := range ix.Deploys {
						if i >= 5 {
							break
						}
						rec = append(rec, recent{d, s.Slug, s.Name})
					}
				}
			}
			if s.Pinned {
				pinned = append(pinned, map[string]any{"slug": s.Slug, "name": s.Name, "status": st, "domain": s.Domain, "instant_url": s.InstantURL, "https": s.HTTPSActive, "last_deploy": s.LastDeploy})
			}
		}
		sort.Slice(rec, func(i, j int) bool { return rec[i].Time > rec[j].Time })
		if len(rec) > 8 {
			rec = rec[:8]
		}
		items := attentionItems(&c, status, apiErr, health, sshS.connected(), time.Now())
		writeJSON(w, 200, map[string]any{
			"counts": counts, "attention": items, "recent": rec, "pinned": pinned,
			"checklist": setupChecklist(&c), "setup_hidden": c.SetupHidden, "api_error": apiErr,
			"connected": sshS.connected(), "server_ip": c.ServerIP, "health": health, "backup": backupSettings(&c),
		})
	})
	mux.HandleFunc("POST /api/setup/hide", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Hidden bool `json:"hidden"`
		}
		_ = readJSON(r, &req)
		_ = withConfig(func(c *Config) error { c.SetupHidden = req.Hidden; return nil })
		writeJSON(w, 200, map[string]any{"ok": true})
	})
}
