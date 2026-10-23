package main

// Netlify-compatible routing rules: _redirects, _headers and netlify.toml.
//
// The same parsed rules drive two things, so what you test is what you get:
//   - the local App Testing preview server (resolve / headersFor below), and
//   - the nginx configuration generated for the production container
//     (nginxconf.go).
//
// Semantics implemented (identical in preview and production):
//  1. the first matching *forced* rule ("200!", force = true) wins;
//  2. otherwise an existing file is served ("shadowing"): /a -> a, a.html,
//     a/index.html (Netlify default clean URLs: /dir -> 301 /dir/,
//     /page/ -> 301 /page when page.html exists);
//  3. otherwise the first matching rule from _redirects, then netlify.toml;
//  4. otherwise 404 with 404.html when present.
//
// Rule paths match with or without a trailing slash (Netlify normalises).

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// RuleIssue is a problem found while reading rules. Level is "fail" or "warn".
type RuleIssue struct {
	Level  string `json:"level"`
	Source string `json:"source"`
	Line   int    `json:"line,omitempty"`
	Msg    string `json:"msg"`
	Fix    string `json:"fix,omitempty"`
}

type RedirectRule struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Status int    `json:"status"`
	Force  bool   `json:"force"`
	Source string `json:"source"`
	Line   int    `json:"line,omitempty"`

	re    *regexp.Regexp
	reSrc string // PCRE-compatible, also used for nginx
}

type HeaderRule struct {
	For    string      `json:"for"`
	Values [][2]string `json:"values"` // name, value (names keep original case)
	Source string      `json:"source"`
	Line   int         `json:"line,omitempty"`

	re    *regexp.Regexp
	reSrc string
}

type SiteRules struct {
	Redirects []*RedirectRule `json:"redirects"`
	Headers   []*HeaderRule   `json:"headers"`
	Issues    []RuleIssue     `json:"issues"`

	// From netlify.toml [build]
	TomlFound    bool   `json:"toml_found"`
	Base         string `json:"base,omitempty"`
	Publish      string `json:"publish,omitempty"`
	BuildCommand string `json:"build_command,omitempty"`
	HasFunctions bool   `json:"has_functions,omitempty"`
}

func (s *SiteRules) issue(level, src string, line int, msg, fix string) {
	s.Issues = append(s.Issues, RuleIssue{Level: level, Source: src, Line: line, Msg: msg, Fix: fix})
}

var (
	placeholderRe = regexp.MustCompile(`^:([A-Za-z_][A-Za-z0-9_]{0,23})$`)
	targetTokenRe = regexp.MustCompile(`:([A-Za-z_][A-Za-z0-9_]*)`)
	statusTokRe   = regexp.MustCompile(`^([0-9]{3})(!?)$`)
	headerNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

var supportedStatus = map[int]bool{200: true, 301: true, 302: true, 303: true, 307: true, 308: true, 404: true, 410: true}

// unsafeForConfig reports characters that we refuse in rule paths/targets
// because they cannot be represented safely in the generated nginx config.
func unsafeForConfig(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '"' || r == '\\' || r == '$' || r == '{' || r == '}' || r == '\'' || r == ';' || r == '#' {
			return true
		}
	}
	return false
}

// compileFrom turns a redirect "from" path into an anchored regexp that works
// in Go and PCRE (nginx). Placeholders become (?<hpc_name>[^/]+), a trailing
// /* becomes an optional (?<hpc_splat>.*).
func compileFrom(from string) (string, error) {
	if !strings.HasPrefix(from, "/") {
		return "", fmt.Errorf("must start with /")
	}
	dec, err := url.PathUnescape(from)
	if err != nil {
		return "", fmt.Errorf("bad %%-encoding")
	}
	if unsafeForConfig(dec) {
		return "", fmt.Errorf("contains characters HostPanel can't use (spaces, quotes, $, {, }, ;, #, \\)")
	}
	segs := strings.Split(strings.TrimPrefix(dec, "/"), "/")
	// Drop a trailing empty segment ("/blog/" == "/blog").
	if len(segs) > 1 && segs[len(segs)-1] == "" {
		segs = segs[:len(segs)-1]
	}
	var b strings.Builder
	b.WriteString("^")
	seen := map[string]bool{}
	splat := false
	for i, s := range segs {
		last := i == len(segs)-1
		if s == "*" {
			if !last {
				return "", fmt.Errorf("a splat (*) is only allowed at the end")
			}
			b.WriteString(`(?:/(?<hpc_splat>.*))?`)
			splat = true
			continue
		}
		if strings.Contains(s, "*") {
			return "", fmt.Errorf("a splat (*) must be a whole path segment at the end, like /blog/*")
		}
		if m := placeholderRe.FindStringSubmatch(s); m != nil {
			if seen[m[1]] || m[1] == "splat" {
				return "", fmt.Errorf("placeholder :%s is used twice", m[1])
			}
			seen[m[1]] = true
			b.WriteString(`/(?<hpc_` + m[1] + `>[^/]+)`)
			continue
		}
		if strings.HasPrefix(s, ":") {
			return "", fmt.Errorf("bad placeholder %q (use letters, digits and _)", s)
		}
		if s == "" && len(segs) == 1 {
			b.WriteString("/")
			continue
		}
		b.WriteString("/" + regexp.QuoteMeta(s))
	}
	if !splat && b.String() != "^/" {
		b.WriteString("/?")
	}
	b.WriteString("$")
	return b.String(), nil
}

// compileHeaderFor compiles a _headers path: * matches anything (also "/"),
// :name matches one path segment. Matching is exact otherwise.
func compileHeaderFor(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("must start with /")
	}
	dec, err := url.PathUnescape(p)
	if err != nil {
		return "", fmt.Errorf("bad %%-encoding")
	}
	if unsafeForConfig(dec) {
		return "", fmt.Errorf("contains characters HostPanel can't use (spaces, quotes, $, {, }, ;, #, \\)")
	}
	var b strings.Builder
	b.WriteString("^")
	for i, s := range strings.Split(dec, "/") {
		if i > 0 {
			b.WriteString("/")
		}
		if strings.HasPrefix(s, ":") {
			if strings.Contains(s, "*") || !placeholderRe.MatchString(s) {
				return "", fmt.Errorf("bad placeholder %q", s)
			}
			b.WriteString(`[^/]+`)
			continue
		}
		parts := strings.Split(s, "*")
		for j, part := range parts {
			if j > 0 {
				b.WriteString(".*")
			}
			b.WriteString(regexp.QuoteMeta(part))
		}
	}
	b.WriteString("$")
	return b.String(), nil
}

func (s *SiteRules) addRedirect(r *RedirectRule) {
	src := r.Source
	if r.From == "" || r.To == "" {
		s.issue("fail", src, r.Line, "Rule is missing its 'from' or 'to' path.", "Write rules as: /old-path /new-path 301")
		return
	}
	if strings.HasPrefix(r.From, "http://") || strings.HasPrefix(r.From, "https://") {
		s.issue("warn", src, r.Line, fmt.Sprintf("Domain-level rule %q is ignored.", r.From), "Redirect domains in the Sites tab (the www option) instead of in _redirects.")
		return
	}
	if !supportedStatus[r.Status] {
		s.issue("fail", src, r.Line, fmt.Sprintf("Status %d is not supported for %s.", r.Status, r.From), "Use 200 (rewrite), 301/302 (redirect) or 404.")
		return
	}
	if r.Status == 307 || r.Status == 308 || r.Status == 303 {
		s.issue("warn", src, r.Line, fmt.Sprintf("Status %d works on HostPanel but Netlify itself doesn't support it.", r.Status), "Use 301 or 302 if you also deploy to Netlify.")
	}
	reSrc, err := compileFrom(r.From)
	if err != nil {
		s.issue("fail", src, r.Line, fmt.Sprintf("Path %q: %v.", r.From, err), "Fix the 'from' path. Splats go at the end (/blog/*), placeholders are whole segments (/blog/:slug).")
		return
	}
	external := strings.HasPrefix(r.To, "http://") || strings.HasPrefix(r.To, "https://")
	if !external && !strings.HasPrefix(r.To, "/") {
		s.issue("fail", src, r.Line, fmt.Sprintf("Target %q must start with / or https://.", r.To), "Use a site path like /index.html or a full URL.")
		return
	}
	if unsafeForConfig(r.To) {
		s.issue("fail", src, r.Line, fmt.Sprintf("Target %q contains characters HostPanel can't use.", r.To), "Remove spaces, quotes, $, {, }, ; and backslashes (%-encode them instead).")
		return
	}
	if external && r.Status == 200 {
		s.issue("warn", src, r.Line, fmt.Sprintf("Proxy rule %s -> %s is ignored: HostPanel serves static files and doesn't proxy.", r.From, r.To), "Call the external URL directly from your page, or use a 301/302 redirect.")
		return
	}
	if (r.Status == 404 || r.Status == 410) && !external && targetTokenRe.MatchString(r.To) {
		s.issue("fail", src, r.Line, fmt.Sprintf("Status %d rules can't use placeholders in the target (%s).", r.Status, r.To), "Point to a fixed page like /404.html.")
		return
	}
	if (r.Status == 404 || r.Status == 410) && external {
		s.issue("fail", src, r.Line, fmt.Sprintf("Status %d rules need a page of this site as target, not %s.", r.Status, r.To), "Point to a page like /404.html.")
		return
	}
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`\(\?<hpc_([A-Za-z0-9_]+)>`).FindAllStringSubmatch(reSrc, -1) {
		names[m[1]] = true
	}
	for _, m := range targetTokenRe.FindAllStringSubmatch(r.To, -1) {
		if !names[m[1]] {
			s.issue("warn", src, r.Line, fmt.Sprintf("Target uses :%s but the 'from' path %s doesn't define it; it will stay literal.", m[1], r.From), "Add the placeholder to the 'from' path or remove it from the target.")
		}
	}
	re, err := regexp.Compile(reSrc)
	if err != nil {
		s.issue("fail", src, r.Line, fmt.Sprintf("Path %q could not be compiled: %v", r.From, err), "")
		return
	}
	r.re, r.reSrc = re, reSrc
	s.Redirects = append(s.Redirects, r)
}

func (s *SiteRules) addHeader(h *HeaderRule) {
	if h.For == "" {
		return
	}
	if strings.HasPrefix(h.For, "http://") || strings.HasPrefix(h.For, "https://") {
		s.issue("warn", h.Source, h.Line, fmt.Sprintf("Domain-level header rule %q is ignored.", h.For), "Use a path such as /* instead.")
		return
	}
	reSrc, err := compileHeaderFor(h.For)
	if err != nil {
		s.issue("fail", h.Source, h.Line, fmt.Sprintf("Header path %q: %v.", h.For, err), "Use paths like /*, /assets/* or /blog/:slug.")
		return
	}
	var vals [][2]string
	for _, kv := range h.Values {
		name, val := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		if !headerNameRe.MatchString(name) {
			s.issue("fail", h.Source, h.Line, fmt.Sprintf("Header name %q is not valid.", name), "Header names use letters, digits and dashes, e.g. X-Frame-Options.")
			continue
		}
		bad := false
		for _, r := range val {
			if r < 0x20 && r != '\t' || r == 0x7f {
				bad = true
			}
		}
		if bad {
			s.issue("fail", h.Source, h.Line, fmt.Sprintf("Header %s has control characters in its value.", name), "Keep each header value on one line.")
			continue
		}
		vals = append(vals, [2]string{name, val})
	}
	if len(vals) == 0 {
		s.issue("warn", h.Source, h.Line, fmt.Sprintf("Header path %s has no headers below it.", h.For), "Indent 'Name: value' lines under the path.")
		return
	}
	h.Values = vals
	h.re = regexp.MustCompile(reSrc)
	h.reSrc = reSrc
	s.Headers = append(s.Headers, h)
}

// parseRedirectsFile parses Netlify's _redirects format:
//
//	/from  [query=:param ...]  /to  [status[!]]  [Country=.. Language=.. Role=.. Cookie=..]
func (s *SiteRules) parseRedirectsFile(text, src string) {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if ln == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			s.issue("fail", src, ln, fmt.Sprintf("Rule %q has no target.", line), "Write rules as: /old-path /new-path 301")
			continue
		}
		r := &RedirectRule{From: f[0], Status: 301, Source: src, Line: ln}
		i := 1
		var query []string
		for i < len(f) && strings.Contains(f[i], "=") && !strings.HasPrefix(f[i], "/") && !strings.Contains(f[i], "://") {
			query = append(query, f[i])
			i++
		}
		if i >= len(f) {
			s.issue("fail", src, ln, fmt.Sprintf("Rule %q has no target.", line), "Write rules as: /old-path /new-path 301")
			continue
		}
		r.To = f[i]
		i++
		if i < len(f) {
			if m := statusTokRe.FindStringSubmatch(f[i]); m != nil {
				r.Status, _ = strconv.Atoi(m[1])
				r.Force = m[2] == "!"
				i++
			} else if !strings.Contains(f[i], "=") {
				s.issue("fail", src, ln, fmt.Sprintf("%q is not a valid status code.", f[i]), "Use a status like 301, 302, 200 or 404 (add ! to force).")
				continue
			}
		}
		if len(query) > 0 {
			s.issue("warn", src, ln, fmt.Sprintf("Query-parameter rule %s %s is ignored.", r.From, strings.Join(query, " ")), "HostPanel only matches on the path. Use a path-based rule instead.")
			continue
		}
		if i < len(f) {
			s.issue("warn", src, ln, fmt.Sprintf("Conditions (%s) on %s are not supported; the rule is ignored.", strings.Join(f[i:], " "), r.From), "Country/Language/Role/Cookie conditions need Netlify's edge; remove them or the rule.")
			continue
		}
		s.addRedirect(r)
	}
}

// parseHeadersFile parses Netlify's _headers format: a path line followed by
// indented "Name: value" lines.
func (s *SiteRules) parseHeadersFile(text, src string) {
	var cur *HeaderRule
	flush := func() {
		if cur != nil {
			s.addHeader(cur)
		}
		cur = nil
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	ln := 0
	for sc.Scan() {
		ln++
		raw := sc.Text()
		if ln == 1 {
			raw = strings.TrimPrefix(raw, "\ufeff")
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		indented := raw[0] == ' ' || raw[0] == '\t'
		if !indented {
			flush()
			if strings.Contains(line, ": ") && !strings.HasPrefix(line, "/") {
				s.issue("fail", src, ln, fmt.Sprintf("Header line %q is not under a path.", line), "Put a path line (e.g. /*) above it and indent the header lines.")
				continue
			}
			cur = &HeaderRule{For: line, Source: src, Line: ln}
			continue
		}
		if cur == nil {
			s.issue("fail", src, ln, fmt.Sprintf("Header line %q is not under a path.", line), "Put a path line (e.g. /*) above it.")
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			s.issue("fail", src, ln, fmt.Sprintf("%q is not a 'Name: value' header line.", line), "Write headers as Name: value")
			continue
		}
		cur.Values = append(cur.Values, [2]string{k, v})
	}
	flush()
}

type netlifyTOML struct {
	Build struct {
		Base      string `toml:"base"`
		Publish   string `toml:"publish"`
		Command   string `toml:"command"`
		Functions string `toml:"functions"`
	} `toml:"build"`
	Redirects []struct {
		From       string         `toml:"from"`
		To         string         `toml:"to"`
		Status     int            `toml:"status"`
		Force      bool           `toml:"force"`
		Query      map[string]any `toml:"query"`
		Conditions map[string]any `toml:"conditions"`
		Headers    map[string]any `toml:"headers"`
		Signed     string         `toml:"signed"`
	} `toml:"redirects"`
	Headers []struct {
		For    string         `toml:"for"`
		Values map[string]any `toml:"values"`
	} `toml:"headers"`
	Functions     map[string]any `toml:"functions"`
	EdgeFunctions []any          `toml:"edge_functions"`
}

// parseNetlifyTOML reads netlify.toml. [build] base/publish/command are kept;
// [[redirects]] and [[headers]] are added after _redirects/_headers rules
// (call it after the plain files, Netlify's order).
func (s *SiteRules) parseNetlifyTOML(text, src string) {
	var t netlifyTOML
	if _, err := toml.Decode(text, &t); err != nil {
		line := 0
		if pe, ok := err.(toml.ParseError); ok {
			line = pe.Position.Line
		}
		s.issue("fail", src, line, "netlify.toml can't be read: "+firstLine(err.Error()), "Fix the TOML syntax (strings need quotes, tables look like [[redirects]]).")
		return
	}
	s.TomlFound = true
	s.Base = strings.TrimSpace(t.Build.Base)
	s.Publish = strings.TrimSpace(t.Build.Publish)
	s.BuildCommand = strings.TrimSpace(t.Build.Command)
	if t.Build.Functions != "" || len(t.Functions) > 0 || len(t.EdgeFunctions) > 0 {
		s.HasFunctions = true
	}
	for i, r := range t.Redirects {
		label := fmt.Sprintf("%s [[redirects]] #%d", src, i+1)
		rr := &RedirectRule{From: r.From, To: r.To, Status: r.Status, Force: r.Force, Source: label}
		if rr.Status == 0 {
			rr.Status = 301
		}
		if len(r.Query) > 0 {
			s.issue("warn", label, 0, fmt.Sprintf("Query-parameter rule for %s is ignored.", r.From), "HostPanel only matches on the path.")
			continue
		}
		if len(r.Conditions) > 0 {
			s.issue("warn", label, 0, fmt.Sprintf("Conditions on %s are not supported; the rule is ignored.", r.From), "Remove the conditions or the rule.")
			continue
		}
		if r.Signed != "" || len(r.Headers) > 0 {
			s.issue("warn", label, 0, fmt.Sprintf("Proxy options (headers/signed) on %s are ignored.", r.From), "HostPanel doesn't proxy requests.")
		}
		s.addRedirect(rr)
	}
	for i, h := range t.Headers {
		label := fmt.Sprintf("%s [[headers]] #%d", src, i+1)
		hr := &HeaderRule{For: h.For, Source: label}
		keys := make([]string, 0, len(h.Values))
		for k := range h.Values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch v := h.Values[k].(type) {
			case string:
				hr.Values = append(hr.Values, [2]string{k, v})
			case []any:
				for _, x := range v {
					hr.Values = append(hr.Values, [2]string{k, fmt.Sprint(x)})
				}
			default:
				hr.Values = append(hr.Values, [2]string{k, fmt.Sprint(v)})
			}
		}
		if h.For == "" {
			s.issue("fail", label, 0, "Header rule has no 'for' path.", `Add for = "/*" (or another path).`)
			continue
		}
		s.addHeader(hr)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// loadSiteRules reads the rules of a site. tomlDir is where netlify.toml lives
// (the base directory); publishDir holds _redirects and _headers.
func loadSiteRules(tomlDir, publishDir string) *SiteRules {
	s := &SiteRules{}
	read := func(p string) (string, bool) {
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			return "", false
		}
		if st.Size() > 1<<20 {
			s.issue("fail", filepath.Base(p), 0, "File is larger than 1 MB and was ignored.", "Keep rule files small.")
			return "", false
		}
		b, err := os.ReadFile(p)
		return string(b), err == nil
	}
	if t, ok := read(filepath.Join(publishDir, "_redirects")); ok {
		s.parseRedirectsFile(t, "_redirects")
	}
	if t, ok := read(filepath.Join(publishDir, "_headers")); ok {
		s.parseHeadersFile(t, "_headers")
	}
	if tomlDir != "" {
		if t, ok := read(filepath.Join(tomlDir, "netlify.toml")); ok {
			s.parseNetlifyTOML(t, "netlify.toml")
		}
	}
	return s
}

// ---------- matching ----------

func (r *RedirectRule) match(p string) (map[string]string, bool) {
	m := r.re.FindStringSubmatch(p)
	if m == nil {
		return nil, false
	}
	caps := map[string]string{}
	for i, n := range r.re.SubexpNames() {
		if strings.HasPrefix(n, "hpc_") {
			caps[n[4:]] = m[i]
		}
	}
	return caps, true
}

func expandTarget(to string, caps map[string]string) string {
	return targetTokenRe.ReplaceAllStringFunc(to, func(tok string) string {
		if v, ok := caps[tok[1:]]; ok {
			return v
		}
		return tok
	})
}

// headersFor returns the headers for a request path. Every matching rule
// applies; for the same header name a later rule replaces an earlier one, and
// repeated names within one rule are joined with ", " (Netlify behaviour).
func (s *SiteRules) headersFor(p string) [][2]string {
	if s == nil {
		return nil
	}
	type hv struct {
		name, val string
		order     int
	}
	res := map[string]*hv{}
	n := 0
	for _, h := range s.Headers {
		if !h.re.MatchString(p) {
			continue
		}
		local := map[string][]string{}
		var orderNames []string
		for _, kv := range h.Values {
			k := strings.ToLower(kv[0])
			if _, ok := local[k]; !ok {
				orderNames = append(orderNames, kv[0])
			}
			local[k] = append(local[k], kv[1])
		}
		for _, name := range orderNames {
			k := strings.ToLower(name)
			n++
			res[k] = &hv{name: name, val: strings.Join(local[k], ", "), order: n}
		}
	}
	out := make([]*hv, 0, len(res))
	for _, v := range res {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].order < out[j].order })
	r := make([][2]string, len(out))
	for i, v := range out {
		r[i] = [2]string{v.name, v.val}
	}
	return r
}

// ---------- resolution (shared by preview, link checker and tests) ----------

// FileSet answers "does this site file exist?" for slash paths without the
// leading slash (e.g. "about.html").
type FileSet interface {
	IsFile(rel string) bool
}

type Resolution struct {
	Status   int           `json:"status"`
	File     string        `json:"file,omitempty"`     // file to serve (relative), empty for redirects/plain 404
	Location string        `json:"location,omitempty"` // for 3xx
	Via      string        `json:"via"`                // human-readable explanation
	Rule     *RedirectRule `json:"-"`
}

var hiddenFiles = map[string]bool{"_redirects": true, "_headers": true, "netlify.toml": true}

func fileOf(fs FileSet, rel string) bool {
	if rel == "" || hiddenFiles[rel] {
		return false
	}
	return fs.IsFile(rel)
}

// resolveStatic implements the "a file exists" step (clean URLs included).
func resolveStatic(fs FileSet, p, rawQuery string) (Resolution, bool) {
	rel := strings.TrimPrefix(p, "/")
	q := ""
	if rawQuery != "" {
		q = "?" + rawQuery
	}
	if !strings.HasSuffix(p, "/") {
		if fileOf(fs, rel) {
			return Resolution{Status: 200, File: rel, Via: "file " + rel}, true
		}
		if fileOf(fs, rel+".html") {
			return Resolution{Status: 200, File: rel + ".html", Via: "clean URL -> " + rel + ".html"}, true
		}
		if fileOf(fs, rel+"/index.html") {
			return Resolution{Status: 301, Location: p + "/" + q, Via: "folder " + rel + "/ has index.html: add the trailing slash"}, true
		}
		return Resolution{}, false
	}
	if fileOf(fs, rel+"index.html") {
		return Resolution{Status: 200, File: rel + "index.html", Via: "folder index " + rel + "index.html"}, true
	}
	if trimmed := strings.TrimSuffix(rel, "/"); trimmed != "" && fileOf(fs, trimmed+".html") {
		return Resolution{Status: 301, Location: "/" + trimmed + q, Via: trimmed + ".html exists: remove the trailing slash"}, true
	}
	return Resolution{}, false
}

func (s *SiteRules) apply(fs FileSet, r *RedirectRule, caps map[string]string, rawQuery string) Resolution {
	to := expandTarget(r.To, caps)
	where := r.Source
	if r.Line > 0 {
		where = fmt.Sprintf("%s line %d", r.Source, r.Line)
	}
	desc := fmt.Sprintf("rule %s %s %d%s (%s)", r.From, r.To, r.Status, map[bool]string{true: "!", false: ""}[r.Force], where)
	switch r.Status {
	case 200:
		tp, tq, _ := strings.Cut(to, "?")
		if tq == "" {
			tq = rawQuery
		}
		if res, ok := resolveStatic(fs, tp, tq); ok {
			res.Via = desc + " -> " + res.Via
			res.Rule = r
			return res
		}
		return Resolution{Status: 404, File: s.notFoundFile(fs), Via: desc + " -> target " + tp + " not found", Rule: r}
	case 404, 410:
		tp, _, _ := strings.Cut(to, "?")
		if res, ok := resolveStatic(fs, tp, ""); ok && res.Status == 200 {
			return Resolution{Status: r.Status, File: res.File, Via: desc, Rule: r}
		}
		return Resolution{Status: r.Status, Via: desc + " (target page missing)", Rule: r}
	default:
		loc := to
		if rawQuery != "" && !strings.Contains(to, "?") {
			loc += "?" + rawQuery
		}
		return Resolution{Status: r.Status, Location: loc, Via: desc, Rule: r}
	}
}

func (s *SiteRules) notFoundFile(fs FileSet) string {
	if fileOf(fs, "404.html") {
		return "404.html"
	}
	return ""
}

// resolve decides what the server returns for a decoded URL path.
func (s *SiteRules) resolve(fs FileSet, p, rawQuery string) Resolution {
	if s == nil {
		s = &SiteRules{}
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if hiddenFiles[strings.TrimPrefix(p, "/")] {
		return Resolution{Status: 404, File: s.notFoundFile(fs), Via: "rule/config files are never published"}
	}
	for _, r := range s.Redirects {
		if !r.Force {
			continue
		}
		if caps, ok := r.match(p); ok {
			return s.apply(fs, r, caps, rawQuery)
		}
	}
	if res, ok := resolveStatic(fs, p, rawQuery); ok {
		return res
	}
	for _, r := range s.Redirects {
		if caps, ok := r.match(p); ok {
			return s.apply(fs, r, caps, rawQuery)
		}
	}
	nf := s.notFoundFile(fs)
	via := "no file and no rule matched"
	if nf != "" {
		via += " -> 404.html"
	}
	return Resolution{Status: 404, File: nf, Via: via}
}

// cleanURLPath validates and normalises a decoded request path.
func cleanURLPath(p string) (string, bool) {
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\\") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c, true
}
