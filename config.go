package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Site is one static website managed by HostPanel. Each site maps to one
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

	Sites []*Site `json:"sites"`
}

var (
	cfgMu sync.Mutex
	cfg   = &Config{SSHPort: 22, SSHUser: "root"}
)

func configDir() (string, error) {
	if d := os.Getenv("HOSTPANEL_CONFIG_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir() // %APPDATA% on Windows
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "HostPanel"), nil
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
		c.Sites[i] = &cp
	}
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
