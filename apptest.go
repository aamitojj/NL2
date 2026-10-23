package main

// App Testing: one active test session (workspace + preview server + report).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type TestSession struct {
	ID      string
	WS      *Workspace
	Report  *Report
	Preview *PreviewServer
	Log     *eventLog
	Created time.Time

	// guarded by testMu
	inUse   int  // deployments reading WS
	closing bool // remove WS once inUse drops to 0
}

var (
	testMu  sync.Mutex
	curTest *TestSession
)

func (t *TestSession) close() {
	if t == nil {
		return
	}
	if t.Preview != nil {
		t.Preview.Stop()
	}
	testMu.Lock()
	busy := t.inUse > 0
	t.closing = true
	testMu.Unlock()
	if !busy {
		t.WS.Close()
	}
}

// stopTest ends the current session (preview server and temp files).
func stopTest() {
	testMu.Lock()
	t := curTest
	curTest = nil
	testMu.Unlock()
	t.close()
}

// cleanupStaleWorkspaces removes temp folders left by a crashed run.
func cleanupStaleWorkspaces() {
	m, _ := filepath.Glob(filepath.Join(os.TempDir(), "dilreet-test-*"))
	for _, d := range m {
		if st, err := os.Stat(d); err == nil && st.IsDir() && time.Since(st.ModTime()) > time.Hour {
			os.RemoveAll(d)
		}
	}
}

func startTest(ws *Workspace, uiHost string) (*TestSession, error) {
	log := &eventLog{}
	rep := runChecks(ws)
	ps, err := startPreview(ws, uiHost, log)
	if err != nil {
		ws.Close()
		return nil, err
	}
	t := &TestSession{ID: randHex(6), WS: ws, Report: rep, Preview: ps, Log: log, Created: time.Now()}
	log.add(TestEvent{Kind: "info", Level: "info", Msg: fmt.Sprintf("Preview started at %s (%d files from %s)", ps.URL(), ws.FileCount, ws.Source)})
	testMu.Lock()
	old := curTest
	curTest = t
	testMu.Unlock()
	old.close()
	return t, nil
}

func currentTest() *TestSession {
	testMu.Lock()
	defer testMu.Unlock()
	return curTest
}

func testState(t *TestSession) map[string]any {
	if t == nil {
		return map[string]any{"active": false}
	}
	return map[string]any{
		"active":      true,
		"id":          t.ID,
		"preview_url": t.Preview.URL(),
		"workspace":   t.WS,
		"report":      t.Report,
		"reloadable":  t.WS.LocalPath != "",
		"created":     t.Created,
	}
}

func registerTestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/test/load", func(w http.ResponseWriter, r *http.Request) {
		ws, err := siteUpload(w, r)
		if err != nil {
			writeErr(w, err)
			return
		}
		t, err := startTest(ws, r.Host)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, testState(t))
	})
	mux.HandleFunc("POST /api/test/reload", func(w http.ResponseWriter, r *http.Request) {
		t := currentTest()
		if t == nil || t.WS.LocalPath == "" {
			writeErr(w, errors.New("only a site loaded from a path on this PC can be reloaded; load it again instead"))
			return
		}
		lp := t.WS.LocalPath
		var entries []srcEntry
		var cleanup func()
		var err error
		if st, e := os.Stat(lp); e != nil {
			err = fmt.Errorf("path not found: %s", lp)
		} else if st.IsDir() {
			entries, err = dirEntries(lp)
		} else {
			entries, cleanup, err = zipEntries(lp)
		}
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		ws, err := newWorkspace(entries, lp)
		if err != nil {
			writeErr(w, err)
			return
		}
		ws.LocalPath = lp
		nt, err := startTest(ws, r.Host)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, testState(nt))
	})
	mux.HandleFunc("GET /api/test", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, testState(currentTest()))
	})
	mux.HandleFunc("GET /api/test/events", func(w http.ResponseWriter, r *http.Request) {
		t := currentTest()
		if t == nil {
			writeJSON(w, 200, map[string]any{"active": false})
			return
		}
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		evs, last, nerr, nwarn := t.Log.since(from)
		writeJSON(w, 200, map[string]any{"active": true, "id": t.ID, "events": evs, "last": last, "errors": nerr, "warnings": nwarn})
	})
	mux.HandleFunc("POST /api/test/events/clear", func(w http.ResponseWriter, r *http.Request) {
		if t := currentTest(); t != nil {
			t.Log.clear()
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/test/stop", func(w http.ResponseWriter, r *http.Request) {
		stopTest()
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/test/report", func(w http.ResponseWriter, r *http.Request) {
		t := currentTest()
		if t == nil {
			writeErr(w, errors.New("no test running"))
			return
		}
		evs, _, _, _ := t.Log.since(0)
		out := map[string]any{"dilreet": Version, "report": t.Report, "browser_log": evs, "publish": t.WS}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="dilreet-test-report-%s.json"`, time.Now().Format("20060102-150405")))
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	})
	mux.HandleFunc("GET /api/test/nginx", func(w http.ResponseWriter, r *http.Request) {
		t := currentTest()
		if t == nil {
			writeErr(w, errors.New("no test running"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(generateNginxConf(t.WS.Rules, t.WS.Files)))
	})
	mux.HandleFunc("POST /api/test/deploy", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Slug        string `json:"slug"`
			AcceptFails bool   `json:"accept_fails"`
			TestID      string `json:"test_id"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		t := currentTest()
		if t == nil || t.ID != req.TestID {
			writeErr(w, errors.New("the tested version is no longer loaded; load and test it again"))
			return
		}
		if _, err := siteOr404(req.Slug); err != nil {
			writeErr(w, err)
			return
		}
		if !sshS.connected() {
			writeErr(w, errNotConnected)
			return
		}
		if n := t.Report.Counts["fail"]; n > 0 && !req.AcceptFails {
			writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("%d check(s) failed; confirm to deploy anyway", n), "needs_confirm": true})
			return
		}
		if !t.WS.Files.IsFile("index.html") {
			writeErr(w, errors.New("the tested build has no index.html at the top level, so it cannot be deployed"))
			return
		}
		ws, slug := t.WS, req.Slug
		testMu.Lock()
		t.inUse++
		testMu.Unlock()
		j := newJob("deploy", func(j *Job) error {
			defer func() {
				testMu.Lock()
				t.inUse--
				done := t.inUse == 0 && t.closing
				testMu.Unlock()
				if done {
					t.WS.Close()
				}
			}()
			j.Logf("Deploying the tested build (%d files, fingerprint %s) to %s...", ws.FileCount, ws.Hash[:12], slug)
			return deployWorkspace(j, slug, ws)
		})
		writeJSON(w, 200, map[string]any{"job": j.ID})
	})
}
