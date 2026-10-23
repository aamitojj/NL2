package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// Site is one static website managed by Dilreet. Each site maps to one
// Coolify project containing one Docker-image application (nginx).
type Site struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	ProjectUUID string `json:"project_uuid"`
	AppUUID     string `json:"app_uuid"`
	Domain      string `json:"domain,omitempty"`
	IncludeWWW  bool   `json:"include_www,omitempty"`
	DomainLive  bool   `json:"domain_live,omitempty"`
	CreatedAt   string `json:"created_at"`
	LastDeploy  string `json:"last_deploy,omitempty"`
	Release     string `json:"release,omitempty"`
	ConfMount   bool   `json:"conf_mount,omitempty"` // nginx rules mount added (v0.2+)

	// v0.3 automation
	InstantURL  string   `json:"instant_url,omitempty"`  // https://<slug>.<ip>.sslip.io
	HTTPSActive bool     `json:"https_active,omitempty"` // a valid certificate was seen on the custom domain
	DNSMode     string   `json:"dns_mode,omitempty"`     // "cloudflare" or "manual"
	CFZoneID    string   `json:"cf_zone_id,omitempty"`
	CFRecordIDs []string `json:"cf_record_ids,omitempty"`
	CFProxy     bool     `json:"cf_proxy,omitempty"` // turn the orange cloud on once HTTPS works
	Watch       bool     `json:"watch,omitempty"`    // auto-redeploy when WatchPath changes
	WatchPath   string   `json:"watch_path,omitempty"`
	LastSource  string   `json:"last_source,omitempty"`  // local folder/zip of the last deploy, if known
	Folder      string   `json:"folder,omitempty"`       // project folder name under ProjectsDir
	DNSProvider string   `json:"dns_provider,omitempty"` // registrar API used for the records (godaddy/porkbun)
	PendingNS   []string `json:"pending_ns,omitempty"`

	Git *SiteGit `json:"git,omitempty"` // set when the site was imported from a repository
}

// SiteGit links a site to a repository branch for "Pull latest and deploy"
// and auto-deploy on new commits (polling while Dilreet runs; no webhooks).
type SiteGit struct {
	URL         string `json:"url"`
	Kind        string `json:"kind"`
	Display     string `json:"display"`
	Branch      string `json:"branch"`
	Subdir      string `json:"subdir,omitempty"`
	LastSHA     string `json:"last_sha,omitempty"`
	AutoDeploy  bool   `json:"auto_deploy"`
	IntervalMin int    `json:"interval_min,omitempty"` // default 5
	TestFirst   bool   `json:"test_first"`             // run checks; skip deploy on failures
	Build       bool   `json:"build,omitempty"`        // user consented to npm build for this site
	LastCheck   string `json:"last_check,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

type Config struct {
	ServerIP   string `json:"server_ip"`
	SSHPort    int    `json:"ssh_port"`
	SSHUser    string `json:"ssh_user"`
	SSHKeyPath string `json:"ssh_key_path,omitempty"`
	// HostKey is the server's SSH host key (authorized_keys format), pinned on
	// first successful connection (trust on first use).
	HostKey string `json:"host_key,omitempty"`

	CoolifyURL string `json:"coolify_url,omitempty"`
	// CoolifyToken is encrypted with Windows DPAPI ("dpapi:" prefix) on Windows.
	CoolifyToken string `json:"coolify_token,omitempty"`
	APIViaSSH    bool   `json:"api_via_ssh,omitempty"`
	ServerUUID   string `json:"server_uuid,omitempty"`

	UpdateManifestURL string `json:"update_manifest_url,omitempty"`

	// v0.3 automation. Secrets are protected with protectString (DPAPI on Windows).
	ManagedKey       string `json:"managed_key,omitempty"`      // Dilreet's own SSH private key (protected)
	ManagedKeyPub    string `json:"managed_key_pub,omitempty"`  // its public key (authorized_keys format)
	AdminEmail       string `json:"admin_email,omitempty"`      // Coolify admin login created by Dilreet
	AdminPassword    string `json:"admin_password,omitempty"`   // protected
	CloudflareToken  string `json:"cloudflare_token,omitempty"` // protected, scoped "Edit zone DNS" token
	AutoSecurityUpd  bool   `json:"auto_security_updates,omitempty"`
	BackgroundChecks *bool  `json:"background_checks,omitempty"` // nil = on
	Provider         string `json:"provider,omitempty"`          // "hetzner"/"digitalocean" if Dilreet created the server
	ProviderServerID string `json:"provider_server_id,omitempty"`

	// Assistant. API keys are protected with protectString (DPAPI on Windows)
	// and never returned to the UI in full.
	AIConns     map[string]*AIConn `json:"ai_conns,omitempty"`
	AIDefault   string             `json:"ai_default,omitempty"`    // "local:<id>|<model>" or "<provider>|<model>"
	AINoContext bool               `json:"ai_no_context,omitempty"` // never attach app context to cloud requests
	Notices     []Notice           `json:"notices,omitempty"`

	// Optional Git/Netlify accounts for private imports, keyed by host
	// ("github.com", "gitlab.example.com", "dev.azure.com/org", "netlify").
	// Tokens are protected with protectString (DPAPI on Windows).
	GitAccounts map[string]*GitAccount `json:"git_accounts,omitempty"`

	// Projects: local copy of every deploy (Documents\Dilreet\Projects by default).
	ProjectsDir string `json:"projects_dir,omitempty"`
	KeepDeploys int    `json:"keep_deploys,omitempty"` // 0 = keep every deploy archive
	// Registrar DNS API keys (GoDaddy, Porkbun), protected like the other secrets.
	Registrars map[string]*RegistrarCred `json:"registrars,omitempty"`
	// MigratedFrom records the legacy config folder this config was copied from.
	MigratedFrom string `json:"migrated_from,omitempty"`

	Sites []*Site `json:"sites"`
}

// RegistrarCred is a domain registrar API credential. Key and Secret are
// protected with protectString and never sent back to the UI.
type RegistrarCred struct {
	Key    string `json:"key"`
	Secret string `json:"secret,omitempty"`
	Hint   string `json:"hint,omitempty"`
	Mode   string `json:"mode,omitempty"` // godaddy: "pat" (v3) or "key" (v1 sso-key)
}

// AIConn is a connected cloud AI provider.
type AIConn struct {
	Key     string `json:"key,omitempty"`      // protected
	KeyHint string `json:"key_hint,omitempty"` // e.g. "sk-...9fQ2" for display
	Model   string `json:"model,omitempty"`
	BaseURL string `json:"base_url,omitempty"` // custom provider only
	Name    string `json:"name,omitempty"`     // custom provider display name
	Consent bool   `json:"consent,omitempty"`  // user accepted the data notice
}

// Notice is an in-app notification (DNS live, HTTPS ready, update...).
type Notice struct {
	ID    string `json:"id"`
	Time  string `json:"time"`
	Level string `json:"level"` // info | success | warn
	Text  string `json:"text"`
	Site  string `json:"site,omitempty"`
	Read  bool   `json:"read,omitempty"`
}

var (
	cfgMu sync.Mutex
	cfg   = &Config{SSHPort: 22, SSHUser: "root"}
)

// envOr reads a DILREET_* variable, falling back to the pre-rename HOSTPANEL_* name.
func envOr(name string) string {
	if v := os.Getenv("DILREET_" + name); v != "" {
		return v
	}
	return os.Getenv("HOSTPANEL_" + name)
}

func configDir() (string, error) {
	if d := envOr("CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir() // %APPDATA% on Windows
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Dilreet"), nil
}

// legacyConfigDir is where versions before the rename kept their settings.
func legacyConfigDir() string {
	if envOr("CONFIG_DIR") != "" {
		return ""
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "HostPanel")
}

func configPath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

func loadConfig() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if old := legacyConfigDir(); old != "" {
		if moved, err := migrateConfigDir(old, filepath.Dir(p)); err != nil {
			log.Printf("could not copy settings from %s: %v", old, err)
		} else if moved {
			log.Printf("copied settings from %s to %s", old, filepath.Dir(p))
		}
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	c := &Config{}
	if err := json.Unmarshal(b, c); err != nil {
		return err
	}
	if c.SSHPort == 0 {
		c.SSHPort = 22
	}
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	return nil
}

// saveConfigLocked writes the config atomically. Caller must hold cfgMu.
func saveConfigLocked() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func withConfig(fn func(c *Config) error) error {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if err := fn(cfg); err != nil {
		return err
	}
	return saveConfigLocked()
}

func snapshotConfig() Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	c := *cfg
	c.Sites = make([]*Site, len(cfg.Sites))
	for i, s := range cfg.Sites {
		cp := *s
		if s.Git != nil {
			g := *s.Git
			cp.Git = &g
		}
		cp.CFRecordIDs = append([]string(nil), s.CFRecordIDs...)
		c.Sites[i] = &cp
	}
	// Maps and slices are copied too, so readers never race with withConfig.
	c.AIConns = make(map[string]*AIConn, len(cfg.AIConns))
	for k, v := range cfg.AIConns {
		cp := *v
		c.AIConns[k] = &cp
	}
	c.Registrars = make(map[string]*RegistrarCred, len(cfg.Registrars))
	for k, v := range cfg.Registrars {
		cp := *v
		c.Registrars[k] = &cp
	}
	c.GitAccounts = make(map[string]*GitAccount, len(cfg.GitAccounts))
	for k, v := range cfg.GitAccounts {
		cp := *v
		c.GitAccounts[k] = &cp
	}
	c.Notices = append([]Notice(nil), cfg.Notices...)
	return c
}

func findSite(c *Config, slug string) *Site {
	for _, s := range c.Sites {
		if s.Slug == slug {
			return s
		}
	}
	return nil
}

func getToken() string {
	cfgMu.Lock()
	enc := cfg.CoolifyToken
	cfgMu.Unlock()
	if enc == "" {
		return ""
	}
	t, err := unprotectString(enc)
	if err != nil {
		return ""
	}
	return t
}

// secret returns a decrypted protected config field ("" if unset or unreadable).
func secret(enc string) string {
	if enc == "" {
		return ""
	}
	t, err := unprotectString(enc)
	if err != nil {
		return ""
	}
	return t
}

func backgroundEnabled(c *Config) bool { return c.BackgroundChecks == nil || *c.BackgroundChecks }
