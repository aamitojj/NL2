package main

// Simple traffic numbers from the site's own web server (nginx) log, read
// over SSH with "docker logs". No tracking code is added to the site.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var accessRe = regexp.MustCompile(`^(\S+) \S+ \S+ \[([^\]]+)\] "(\S+) (\S+)[^"]*" (\d{3}) (\d+|-) "([^"]*)" "([^"]*)"`)

type LogHit struct {
	IP     string
	Time   time.Time
	Method string
	Path   string
	Status int
	Bytes  int64
	Ref    string
	UA     string
	Bot    bool
}

type Count struct {
	Key string `json:"key"`
	N   int    `json:"n"`
}

type TrafficStats struct {
	Requests  int            `json:"requests"`
	PageViews int            `json:"page_views"`
	Visitors  int            `json:"visitors"`
	Bots      int            `json:"bots"`
	Bytes     int64          `json:"bytes"`
	Days      []Count        `json:"days"` // oldest first, page views per day
	TopPages  []Count        `json:"top_pages"`
	NotFound  []Count        `json:"not_found"`
	Referrers []Count        `json:"referrers"`
	Status    map[string]int `json:"status"` // 2xx, 3xx, 4xx, 5xx
	From      string         `json:"from,omitempty"`
	To        string         `json:"to,omitempty"`
	Recent    []string       `json:"recent"`
	Errors    []string       `json:"errors"`
}

var botRe = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|curl|wget|python|go-http|headless|monitor|uptime|preview`)

func parseAccessLine(l string) (LogHit, bool) {
	m := accessRe.FindStringSubmatch(l)
	if m == nil {
		return LogHit{}, false
	}
	t, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[2])
	if err != nil {
		return LogHit{}, false
	}
	st, _ := strconv.Atoi(m[5])
	b, _ := strconv.ParseInt(m[6], 10, 64)
	p := m[4]
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return LogHit{IP: m[1], Time: t, Method: m[3], Path: p, Status: st, Bytes: b, Ref: m[7], UA: m[8], Bot: botRe.MatchString(m[8])}, true
}

func isPage(p string) bool {
	if strings.HasSuffix(p, "/") {
		return true
	}
	last := p[strings.LastIndex(p, "/")+1:]
	if !strings.Contains(last, ".") {
		return true
	}
	return strings.HasSuffix(last, ".html") || strings.HasSuffix(last, ".htm")
}

func topN(m map[string]int, n int) []Count {
	out := make([]Count, 0, len(m))
	for k, v := range m {
		out = append(out, Count{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// analyzeLog turns raw log text into numbers. now and days set the window
// for the per-day chart; ownHost referrers are ignored.
func analyzeLog(raw string, now time.Time, days int, ownHosts []string) TrafficStats {
	st := TrafficStats{Status: map[string]int{}, Recent: []string{}, Errors: []string{}}
	ips := map[string]bool{}
	pages, nf, refs := map[string]int{}, map[string]int{}, map[string]int{}
	perDay := map[string]int{}
	var first, last time.Time
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		h, ok := parseAccessLine(l)
		if !ok {
			if strings.Contains(l, "[error]") || strings.Contains(l, "[crit]") || strings.Contains(l, "[emerg]") {
				st.Errors = append(st.Errors, l)
			}
			continue
		}
		st.Requests++
		st.Bytes += h.Bytes
		st.Status[strconv.Itoa(h.Status/100)+"xx"]++
		if first.IsZero() || h.Time.Before(first) {
			first = h.Time
		}
		if h.Time.After(last) {
			last = h.Time
		}
		st.Recent = append(st.Recent, l)
		if h.Bot {
			st.Bots++
			continue
		}
		if h.Status == 404 {
			nf[h.Path]++
		}
		if h.Method != "GET" || !isPage(h.Path) || h.Status >= 400 {
			continue
		}
		st.PageViews++
		ips[h.IP] = true
		pages[h.Path]++
		perDay[h.Time.In(now.Location()).Format("2006-01-02")]++
		if r := refHost(h.Ref); r != "" {
			own := false
			for _, o := range ownHosts {
				if strings.EqualFold(r, o) {
					own = true
				}
			}
			if !own {
				refs[r]++
			}
		}
	}
	st.Visitors = len(ips)
	for i := days - 1; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		st.Days = append(st.Days, Count{d, perDay[d]})
	}
	st.TopPages, st.NotFound, st.Referrers = topN(pages, 10), topN(nf, 10), topN(refs, 8)
	if !first.IsZero() {
		st.From, st.To = first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339)
	}
	if n := len(st.Recent); n > 60 {
		st.Recent = st.Recent[n-60:]
	}
	if n := len(st.Errors); n > 20 {
		st.Errors = st.Errors[n-20:]
	}
	return st
}

func refHost(r string) string {
	if r == "" || r == "-" {
		return ""
	}
	r = strings.TrimPrefix(strings.TrimPrefix(r, "https://"), "http://")
	if i := strings.IndexAny(r, "/?#"); i >= 0 {
		r = r[:i]
	}
	return strings.ToLower(r)
}

var uuidRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)

func fetchSiteLog(ctx context.Context, s *Site, lines int) (string, error) {
	if !sshS.connected() {
		return "", errNotConnected
	}
	if !uuidRe.MatchString(s.AppUUID) {
		return "", errors.New("this project is not linked to a server app yet")
	}
	script := "c=$(docker ps --format '{{.Names}}' --filter name=" + shellQuote(s.AppUUID) + " | head -n1); " +
		"[ -n \"$c\" ] || { echo 'NO_CONTAINER' >&2; exit 3; }; docker logs --tail " + strconv.Itoa(lines) + " \"$c\" 2>&1"
	return runRemoteOutput(ctx, script)
}

func registerLogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sites/{slug}/logs", func(w http.ResponseWriter, r *http.Request) {
		s, err := siteOr404(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		raw, err := fetchSiteLog(ctx, s, 20000)
		if err != nil {
			if strings.Contains(err.Error(), "NO_CONTAINER") || strings.Contains(raw, "NO_CONTAINER") {
				err = errors.New("the site is not running on the server right now, so there is no log. Deploy or restart it first")
			}
			writeErr(w, err)
			return
		}
		hosts := []string{s.Domain, "www." + s.Domain, refHost(s.InstantURL)}
		writeJSON(w, 200, analyzeLog(raw, time.Now(), 14, hosts))
	})
}
