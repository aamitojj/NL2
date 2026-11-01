package main

// Health checks (is the site answering?) and HTTPS certificate expiry.
// Results live in memory; they are refreshed every 10 minutes in the
// background and on demand ("Check now").

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type HealthResult struct {
	URL      string `json:"url"`
	OK       bool   `json:"ok"`
	Code     int    `json:"code,omitempty"`
	MS       int64  `json:"ms"`
	Error    string `json:"error,omitempty"`
	CertDays *int   `json:"cert_days,omitempty"` // days until the HTTPS certificate expires
	CertEnd  string `json:"cert_end,omitempty"`
	At       string `json:"at"`
}

var (
	healthMu      sync.Mutex
	healthResults = map[string]*HealthResult{}
	certNotified  = map[string]string{} // slug -> day last warned
)

func healthURL(s *Site) string {
	if s.HTTPSActive && s.Domain != "" {
		return "https://" + s.Domain
	}
	return s.InstantURL
}

// healthProbe is replaced in tests.
var healthProbe = func(u string) *HealthResult {
	r := &HealthResult{URL: u, At: time.Now().UTC().Format(time.RFC3339)}
	cl := &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}
	t0 := time.Now()
	resp, err := cl.Get(u)
	r.MS = time.Since(t0).Milliseconds()
	if err != nil {
		r.Error = plainNetErr(err)
		return r
	}
	defer resp.Body.Close()
	r.Code = resp.StatusCode
	r.OK = resp.StatusCode < 500
	if !r.OK {
		r.Error = fmt.Sprintf("the server answered with an error (HTTP %d)", resp.StatusCode)
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		end := resp.TLS.PeerCertificates[0].NotAfter
		d := int(time.Until(end).Hours() / 24)
		r.CertDays, r.CertEnd = &d, end.UTC().Format(time.RFC3339)
	}
	return r
}

func plainNetErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "no such host"):
		return "the address could not be found (DNS not ready?)"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"):
		return "no answer in 12 seconds"
	case strings.Contains(s, "certificate"):
		return "the HTTPS certificate is not valid yet"
	case strings.Contains(s, "refused"):
		return "the server refused the connection"
	}
	return s
}

func sslWarnDays(c *Config) int {
	if c.SSLWarnDays > 0 {
		return c.SSLWarnDays
	}
	return 14
}

// runHealthFor checks the given sites (all deployed sites when slugs is nil).
func runHealthFor(slugs []string) map[string]*HealthResult {
	c := snapshotConfig()
	want := map[string]bool{}
	for _, s := range slugs {
		want[s] = true
	}
	out := map[string]*HealthResult{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 6)
	for _, s := range c.Sites {
		if (slugs != nil && !want[s.Slug]) || s.LastDeploy == "" {
			continue
		}
		u := healthURL(s)
		if u == "" {
			continue
		}
		wg.Add(1)
		go func(slug, u string) {
			defer wg.Done()
			sem <- struct{}{}
			r := healthProbe(u)
			<-sem
			mu.Lock()
			out[slug] = r
			mu.Unlock()
		}(s.Slug, u)
	}
	wg.Wait()
	warn := sslWarnDays(&c)
	today := time.Now().Format("2006-01-02")
	healthMu.Lock()
	var notes [][2]string
	for slug, r := range out {
		healthResults[slug] = r
		if r.CertDays != nil && *r.CertDays <= warn && certNotified[slug] != today {
			certNotified[slug] = today
			notes = append(notes, [2]string{slug, fmt.Sprintf("The HTTPS certificate for %s ends in %d days. It normally renews by itself; if this message comes back tomorrow, open the project and click Fix it.", r.URL, *r.CertDays)})
		}
	}
	healthMu.Unlock()
	for _, n := range notes {
		notify("warn", n[0], n[1])
	}
	return out
}

func healthSnapshot() map[string]HealthResult {
	healthMu.Lock()
	defer healthMu.Unlock()
	m := make(map[string]HealthResult, len(healthResults))
	for k, v := range healthResults {
		m[k] = *v
	}
	return m
}

func registerHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"results": healthSnapshot()})
	})
	mux.HandleFunc("POST /api/health/run", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Slugs []string `json:"slugs"`
		}
		_ = readJSON(r, &req)
		if len(req.Slugs) == 0 {
			req.Slugs = nil
		}
		writeJSON(w, 200, map[string]any{"results": runHealthFor(req.Slugs)})
	})
}
