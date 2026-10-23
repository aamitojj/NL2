package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Version is the app version. It is a constant in spirit; it is a var only so
// release builds can set it with -ldflags "-X main.Version=x.y.z".
var Version = "0.3.0"

const defaultManifestURL = "https://example.com/dilreet/latest.json"

type Manifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Notes   string `json:"notes"`
}

// compareVersions returns -1, 0 or 1. Accepts "v1.2.3", "1.2", "1.2.3-beta".
func compareVersions(a, b string) int {
	pa, pb := parseVer(a), parseVer(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVer(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out
}

// allowedUpdateURL requires HTTPS, except for loopback (local testing).
func allowedUpdateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
		return nil
	}
	return fmt.Errorf("update URLs must use https:// (got %q)", raw)
}

func manifestURL() string {
	c := snapshotConfig()
	if c.UpdateManifestURL != "" {
		return c.UpdateManifestURL
	}
	return defaultManifestURL
}

func fetchManifest() (*Manifest, error) {
	mu := manifestURL()
	if err := allowedUpdateURL(mu); err != nil {
		return nil, err
	}
	cl := &http.Client{Timeout: 20 * time.Second}
	resp, err := cl.Get(mu)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("manifest download failed: HTTP %d", resp.StatusCode)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if m.Version == "" || m.URL == "" || len(m.SHA256) != 64 {
		return nil, errors.New("manifest must contain version, url and a 64-character sha256")
	}
	return &m, nil
}

func selfUpdateJob(j *Job) error {
	m, err := fetchManifest()
	if err != nil {
		return err
	}
	if compareVersions(m.Version, Version) <= 0 {
		j.Logf("You already have the latest version (%s).", Version)
		return nil
	}
	if err := allowedUpdateURL(m.URL); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	newPath := exe + ".new"
	oldPath := exe + ".old"
	j.Logf("Downloading version %s from %s ...", m.Version, m.URL)
	cl := &http.Client{Timeout: 10 * time.Minute}
	resp, err := cl.Get(m.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("cannot write next to the program (%s). Move Dilreet to a folder you can write to, e.g. your Desktop or Documents: %w", newPath, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 500<<20))
	f.Close()
	if err != nil {
		os.Remove(newPath)
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	j.Logf("Downloaded %.1f MB, sha256 %s", float64(n)/1e6, sum)
	if !strings.EqualFold(sum, m.SHA256) {
		os.Remove(newPath)
		return fmt.Errorf("checksum mismatch: expected %s. The update was NOT installed", m.SHA256)
	}
	j.Logf("Checksum OK. Replacing the program...")
	_ = os.Remove(oldPath)
	if err := os.Rename(exe, oldPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("rename current program: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		_ = os.Rename(oldPath, exe) // roll back
		return fmt.Errorf("install new program: %w", err)
	}
	if appMode == modeNative {
		j.Logf("Installed %s. Dilreet restarts in a moment (the window reopens by itself)...", m.Version)
	} else {
		j.Logf("Installed %s. Restarting Dilreet (a new browser tab will open)...", m.Version)
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		args := []string{"-after-update"}
		for _, a := range os.Args[1:] {
			if a != "-after-update" && a != "--after-update" {
				args = append(args, a)
			}
		}
		cmd := exec.Command(exe, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Println("restart failed:", err, "- please start Dilreet again manually")
		}
		shutdown()
	}()
	return nil
}

// cleanupOldBinary removes the .old file left by a previous self-update.
func cleanupOldBinary() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exe, _ = filepath.EvalSymlinks(exe)
	go func() {
		for i := 0; i < 10; i++ {
			if err := os.Remove(exe + ".old"); err == nil || os.IsNotExist(err) {
				return
			}
			time.Sleep(time.Second) // the old process may still be exiting (Windows locks)
		}
	}()
}

// latestCoolifyVersion reads the official versions file used by Coolify's installer.
func latestCoolifyVersion() (string, error) {
	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Get("https://cdn.coollabs.io/coolify/versions.json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		Coolify struct {
			V4 struct {
				Version string `json:"version"`
			} `json:"v4"`
		} `json:"coolify"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return "", err
	}
	return v.Coolify.V4.Version, nil
}

// The official installer also upgrades an existing installation to the latest
// version (it downloads and runs /data/coolify/source/upgrade.sh).
const coolifyInstallCmd = "curl -fsSL https://cdn.coollabs.io/coolify/install.sh | bash"

func installCoolifyJob(j *Job, upgrade bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	j.Logf("Checking the server...")
	pre := `echo "OS: $(. /etc/os-release 2>/dev/null; echo $PRETTY_NAME)"; echo "Arch: $(uname -m)"; echo "CPU cores: $(nproc)"; echo "RAM: $(free -m | awk '/Mem:/{print $2}') MB"; echo "Free disk on /: $(df -BG / | awk 'NR==2{print $4}')"`
	if err := runRemote(ctx, pre, j); err != nil {
		return err
	}
	if upgrade {
		j.Logf("Updating Coolify to the latest version by re-running the official installer...")
		j.Logf("Running deployments may fail during the update; the dashboard is down for about a minute.")
	} else {
		j.Logf("Installing Coolify with the official installer. This usually takes 2-10 minutes.")
	}
	j.Logf("$ %s", coolifyInstallCmd)
	j.SetProgress(2, "Starting the installer")
	j.mu.Lock()
	j.onLine = installLineHook
	j.mu.Unlock()
	if err := runRemote(ctx, coolifyInstallCmd, j); err != nil {
		return err
	}
	c := snapshotConfig()
	if !upgrade {
		j.Logf("")
		j.Logf("Coolify is installed. IMPORTANT: open http://%s:8000 NOW and create the admin account (the first visitor becomes admin).", c.ServerIP)
		j.Logf("Back up /data/coolify/source/.env (it contains the encryption key).")
	}
	return nil
}

func osName() string { return runtime.GOOS + "/" + runtime.GOARCH }
