package main

// Desktop integration shared by all platforms: app mode (native window or
// browser fallback), the single-instance hand-off, idle exit in browser mode
// and opening links in the system browser.

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const (
	modeNative  = "native"
	modeBrowser = "browser"
)

var (
	appMode        = modeBrowser
	appModeReason  = ""
	instanceSecret = randHex(24)
	uiURL          string
	logFilePath    string
	lastUIContact  atomic.Int64 // unix seconds of the last API call from the UI
)

type instanceInfo struct {
	PID    int    `json:"pid"`
	Addr   string `json:"addr"`
	Secret string `json:"secret"`
}

func instancePath() string {
	d, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "instance.json")
}

func writeInstanceFile() {
	p := instancePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.Marshal(instanceInfo{PID: os.Getpid(), Addr: listenAddr, Secret: instanceSecret})
	_ = os.WriteFile(p, b, 0o600)
}

func removeInstanceFile() {
	p := instancePath()
	if b, err := os.ReadFile(p); err == nil {
		var in instanceInfo
		if json.Unmarshal(b, &in) == nil && in.PID == os.Getpid() {
			os.Remove(p)
		}
	}
}

// signalRunningInstance asks the already running Dilreet to show itself.
func signalRunningInstance() error {
	b, err := os.ReadFile(instancePath())
	if err != nil {
		return err
	}
	var in instanceInfo
	if err := json.Unmarshal(b, &in); err != nil || in.Addr == "" {
		return errors.New("no instance info")
	}
	allowForeground(in.PID)
	req, _ := http.NewRequest("POST", "http://"+in.Addr+"/__hp/focus", bytes.NewReader(nil))
	req.Header.Set("X-HP-Instance", in.Secret)
	cl := &http.Client{Timeout: 4 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New(resp.Status)
	}
	return nil
}

// handleFocus is called by a second launch: bring the window (or a browser tab) up.
func handleFocus(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-HP-Instance")), []byte(instanceSecret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !focusNativeWindow() {
		go openBrowser(uiURL)
	}
	w.WriteHeader(200)
}

// idleWatchdog ends a browser-mode GUI process (no console window to close)
// when no Dilreet tab has talked to it for a while and nothing is running.
func idleWatchdog(limit time.Duration) {
	lastUIContact.Store(time.Now().Unix())
	for {
		time.Sleep(30 * time.Second)
		idle := time.Since(time.Unix(lastUIContact.Load(), 0))
		if idle > limit && !anyJobRunning() && currentTest() == nil {
			log.Printf("no Dilreet tab open for %s: exiting", idle.Round(time.Second))
			shutdown()
		}
	}
}

// apiOpenExternal opens an http(s) link in the default browser (used by the
// native window, where target=_blank would open an app popup instead).
func apiOpenExternal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeErr(w, errors.New("only http and https links can be opened"))
		return
	}
	openBrowser(u.String())
	writeJSON(w, 200, map[string]any{"ok": true})
}

const webview2DownloadURL = "https://developer.microsoft.com/microsoft-edge/webview2/#download"
