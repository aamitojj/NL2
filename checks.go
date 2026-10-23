package main

// Automatic checks for App Testing. Every result is pass / warn / fail (or
// info) with a plain-language explanation and a fix.

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type CheckItem struct {
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Detail string `json:"detail"`
}

type CheckResult struct {
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	Status  string      `json:"status"` // pass, warn, fail, info
	Summary string      `json:"summary"`
	Explain string      `json:"explain"`
	Fix     string      `json:"fix,omitempty"`
	Items   []CheckItem `json:"items,omitempty"`
	More    int         `json:"more,omitempty"`
}

type Report struct {
	Generated  time.Time      `json:"generated"`
	Source     string         `json:"source"`
	PublishDir string         `json:"publish_dir"`
	PublishWhy string         `json:"publish_note"`
	Files      int            `json:"files"`
	Bytes      int64          `json:"bytes"`
	Hash       string         `json:"hash"`
	Project    ProjectInfo    `json:"project"`
	Redirects  int            `json:"redirect_rules"`
	Headers    int            `json:"header_rules"`
	Counts     map[string]int `json:"counts"`
	Checks     []CheckResult  `json:"checks"`
}

const maxItems = 200

type checker struct {
	ws      *Workspace
	res     map[string]*CheckResult
	order   []string
	pages   map[string]*pageInfo
	cssRefs map[string][]ref
}

type pageInfo struct {
	file                         string
	ids                          map[string]bool
	hasTitle, hasViewport, lang  bool
	hasCharset, hasIcon, isRedir bool
	refs                         []ref
	rootDiv                      bool
	moduleSrc                    []string
}

type ref struct {
	file   string
	line   int
	tag    string // element name or "css"
	attr   string
	value  string
	asset  bool // the browser loads it (vs. a link the visitor clicks)
	active bool // script/style/iframe (blocked as mixed content)
}

func (c *checker) add(id, title, explain, fix string) *CheckResult {
	if r, ok := c.res[id]; ok {
		return r
	}
	r := &CheckResult{ID: id, Title: title, Status: "pass", Explain: explain, Fix: fix}
	c.res[id] = r
	c.order = append(c.order, id)
	return r
}

var rank = map[string]int{"pass": 0, "info": 1, "warn": 2, "fail": 3}

func (r *CheckResult) item(status, file string, line int, detail string) {
	if rank[status] > rank[r.Status] {
		r.Status = status
	}
	if len(r.Items) >= maxItems {
		r.More++
		return
	}
	r.Items = append(r.Items, CheckItem{File: file, Line: line, Detail: detail})
}

func runChecks(ws *Workspace) *Report {
	c := &checker{ws: ws, res: map[string]*CheckResult{}, pages: map[string]*pageInfo{}}
	files := ws.Files.sorted()

	// Order of the report.
	idx := c.add("index", "Home page (index.html)", "Visitors who open your domain get index.html from the top of the published folder.", "Put index.html at the top level of the folder or zip you upload (or set publish in netlify.toml).")
	c.add("publish", "Published folder", "", "")
	c.add("source", "Built output, not source code", "Sites made with React, Vue, Vite and similar tools must be built first. HostPanel serves files as they are and never runs npm.", "Run your build (usually npm run build) and test/deploy the output folder (dist, build, out…), or set publish in netlify.toml.")
	links := c.add("links", "Internal links", "Links (<a href>) to pages of your own site that don't exist give visitors a 404 page.", "Fix the link target or add the missing page. Paths are relative to the page unless they start with /.")
	assets := c.add("assets", "Images, scripts and styles", "Files the page loads itself (src, srcset, stylesheets, CSS url(), icons, fonts). Missing ones show as broken images, unstyled pages or a broken app.", "Upload the missing file or correct the path.")
	cased := c.add("case", "Upper/lower case in file names", "Your PC (Windows) ignores upper/lower case in file names, the web server does not: Logo.PNG and logo.png are different files online, so a link that works on your PC can 404 after deploying.", "Make the link and the file name match exactly. Lower-case file names are easiest.")
	local := c.add("localpaths", "Links to files on your PC", "Addresses like C:\\Users\\… or file:///… only exist on your own computer; for visitors they are broken. Backslashes (\\) are not valid in web addresses either.", "Use relative paths with forward slashes (images/photo.jpg) or site paths (/images/photo.jpg).")
	mixed := c.add("mixed", "Insecure http:// content", "Your site will be served over https. Browsers block scripts, styles and frames loaded over plain http://, and may block or flag images and media.", "Change http:// to https:// (most services support it) or download the file into your site.")
	c.add("fragments", "#anchors on pages", "Links like page.html#contact jump to an element with id=\"contact\". If it doesn't exist the page opens at the top.", "Add the id to the target element or fix the link.")
	sizes := c.add("sizes", "Large files and images", "Big files make the site slow, especially on phones. Images over 500 KB and files over 10 MB are flagged.", "Compress images (e.g. squoosh.app, WebP/AVIF, max ~2000px wide) and host big videos on a video service.")
	c.add("title", "Page titles", "The <title> appears in browser tabs, bookmarks and search results.", "Add <title>Your page name</title> inside <head>.")
	c.add("viewport", "Mobile viewport", "Without <meta name=\"viewport\"> phones show the desktop layout zoomed out.", "Add <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"> inside <head>.")
	c.add("lang", "Page language", "<html lang=\"…\"> tells screen readers, translators and search engines the language.", "Change <html> to <html lang=\"en\"> (or your language code).")
	c.add("charset", "Character encoding", "Without <meta charset=\"utf-8\"> accents and symbols can show as garbage.", "Add <meta charset=\"utf-8\"> as the first thing inside <head>.")
	c.add("favicon", "Favicon", "The small icon in browser tabs. Without one browsers request /favicon.ico and get a 404.", "Add favicon.ico to the top level, or <link rel=\"icon\" href=\"/icon.png\"> in <head>.")
	rules := c.add("rules", "Redirect and header rules", "Rules from _redirects, _headers and netlify.toml. HostPanel applies them in this preview and converts them into the production web-server config on deploy.", "See each item; the syntax is the same as Netlify's.")
	c.add("spa", "Single-page app routing", "Apps with client-side routing need every unknown path to return index.html, otherwise refreshing or sharing a deep link (/about) gives a 404.", "Add a file named _redirects containing:  /*  /index.html  200")
	secrets := c.add("secrets", "Private files", "Everything in the published folder is downloadable by anyone. Environment files, keys and credentials must never be uploaded.", "Remove these files from the upload and rotate any passwords or keys they contain.")
	c.add("netlify", "Netlify-only features", "HostPanel serves static files with Netlify's redirect and header rules, but not Netlify's server features.", "")
	c.add("notfound", "Custom 404 page", "A 404.html at the top level is shown for addresses that don't exist.", "Add a friendly 404.html with a link back home.")

	// index.html
	if ws.Files.IsFile("index.html") {
		idx.Summary = "index.html found."
	} else if m := ws.Files.caseMatch("index.html"); m != "" {
		idx.item("fail", m, 0, "Found "+m+" but the server needs exactly index.html (lower case).")
		idx.Summary = "The home page is named " + m + "; it must be index.html."
		idx.Fix = "Rename " + m + " to index.html."
	} else {
		idx.item("fail", "", 0, "There is no index.html at the top of the published folder.")
		idx.Summary = "No index.html: the domain would show a 404."
	}

	c.checkPublish()
	c.checkSecretsAndNames(files, secrets, sizes)

	// Parse pages and CSS.
	for _, f := range files {
		switch {
		case isHTMLName(f):
			c.scanHTML(f)
		case strings.HasSuffix(strings.ToLower(f), ".css"):
			c.scanCSSFile(f)
		}
	}
	for _, f := range files {
		p := c.pages[f]
		if p == nil {
			continue
		}
		for _, r := range p.refs {
			c.checkRef(r, links, assets, cased, local, mixed)
		}
	}
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f), ".css") {
			for _, r := range c.cssRefs[f] {
				c.checkRef(r, links, assets, cased, local, mixed)
			}
		}
	}
	c.checkMeta()
	c.checkRules(rules)
	c.checkSPA()
	c.checkNetlify()
	if ws.Files.IsFile("404.html") {
		c.res["notfound"].Summary = "404.html found; it is shown for unknown addresses."
	} else {
		c.res["notfound"].Status = "info"
		c.res["notfound"].Summary = "No 404.html; visitors get a plain server error page for unknown addresses."
	}

	rep := &Report{Generated: time.Now(), Source: ws.Source, PublishDir: ws.PublishRel, PublishWhy: ws.PublishNote,
		Files: ws.FileCount, Bytes: ws.Bytes, Hash: ws.Hash, Project: ws.Project, Counts: map[string]int{}}
	if ws.Rules != nil {
		rep.Redirects, rep.Headers = len(ws.Rules.Redirects), len(ws.Rules.Headers)
	}
	for _, id := range c.order {
		r := c.res[id]
		if r.Summary == "" {
			switch r.Status {
			case "pass":
				r.Summary = "No problems found."
			default:
				n := len(r.Items) + r.More
				r.Summary = fmt.Sprintf("%d problem%s found.", n, plural(n))
			}
		}
		if r.Status == "pass" && r.Title != "" {
			r.Fix = ""
		}
		rep.Counts[r.Status]++
		rep.Checks = append(rep.Checks, *r)
	}
	return rep
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (c *checker) checkPublish() {
	r := c.res["publish"]
	ws := c.ws
	label := ws.PublishRel
	if label == "" {
		label = "(top level)"
	}
	r.Explain = "The folder whose contents become your website. Choice: " + ws.PublishNote + "."
	r.Summary = fmt.Sprintf("Serving %s: %d files, %s.", label, ws.FileCount, humanBytes(ws.Bytes))
	r.Status = "pass"
	for _, is := range ws.Issues {
		st := is.Level
		if st != "fail" && st != "warn" {
			st = "info"
		}
		r.item(st, is.Source, is.Line, is.Msg+" "+is.Fix)
	}
	if ws.FileCount == 0 {
		r.item("fail", "", 0, "The published folder is empty.")
	}
	src := c.res["source"]
	p := ws.Project
	src.Summary = "Framework: " + p.Framework + "."
	if p.PackageJSON && p.SourceProject && p.Framework != "Static HTML" {
		st := "warn"
		if !ws.Files.IsFile("index.html") {
			st = "fail"
		}
		cmd := p.BuildCommand
		if cmd == "" {
			cmd = "your build command"
		}
		src.item(st, "package.json", 0, fmt.Sprintf("This looks like %s source code (package.json is in the published folder). Run %s and test the output folder instead.", p.Framework, cmd))
	}
	if p.TomlCommand != "" {
		src.item("info", "netlify.toml", 0, fmt.Sprintf("netlify.toml has build command %q. HostPanel doesn't run it; build on your PC and upload the result.", p.TomlCommand))
	}
}

var imageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".avif": true, ".bmp": true, ".tif": true, ".tiff": true, ".svg": true}

var secretRe = regexp.MustCompile(`(?i)(^|/)(\.env(\..+)?|id_rsa|id_dsa|id_ecdsa|id_ed25519|\.npmrc|\.pypirc|\.htpasswd|\.git-credentials|credentials\.json|service-account.*\.json|.*\.(pem|key|p12|pfx|ppk|kdbx))$`)

func (c *checker) checkSecretsAndNames(files []string, secrets, sizes *CheckResult) {
	var total int64
	for _, f := range files {
		size := c.ws.Files.files[f].Size
		total += size
		if secretRe.MatchString(f) {
			st := "fail"
			base := path.Base(f)
			if strings.Contains(base, "example") || strings.Contains(base, "sample") || strings.HasSuffix(base, ".template") {
				st = "warn"
			}
			secrets.item(st, f, 0, f+" would be public at /"+f)
		}
		ext := strings.ToLower(path.Ext(f))
		switch {
		case imageExt[ext] && size > 500<<10:
			sizes.item("warn", f, 0, fmt.Sprintf("Image is %s.", humanBytes(size)))
		case size > 10<<20:
			sizes.item("warn", f, 0, fmt.Sprintf("File is %s.", humanBytes(size)))
		}
		if strings.ContainsAny(f, " #?%") {
			c.add("names", "Awkward file names", "Spaces, #, ? and % in file names must be encoded in links (%20…) and are easy to get wrong.", "Use letters, digits, - and _ only.").item("warn", f, 0, "File name contains a space, #, ? or %.")
		}
	}
	if total > 500<<20 {
		sizes.item("warn", "", 0, "The whole site is "+humanBytes(total)+"; uploads and deployments will be slow.")
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// ---- HTML scanning ----

var urlAttrs = map[string]map[string]bool{
	"a": {"href": true}, "area": {"href": true}, "link": {"href": true}, "script": {"src": true},
	"img": {"src": true, "srcset": true}, "source": {"src": true, "srcset": true}, "video": {"src": true, "poster": true},
	"audio": {"src": true}, "track": {"src": true}, "iframe": {"src": true}, "embed": {"src": true},
	"object": {"data": true}, "input": {"src": true}, "use": {"href": true, "xlink:href": true},
	"image": {"href": true, "xlink:href": true}, "frame": {"src": true},
}

var skipLinkRels = map[string]bool{"preconnect": true, "dns-prefetch": true, "canonical": true, "alternate": true, "me": true, "author": true, "license": true, "pingback": true}

func (c *checker) scanHTML(f string) {
	b, err := os.ReadFile(filepath.Join(c.ws.Publish, filepath.FromSlash(f)))
	if err != nil {
		return
	}
	p := &pageInfo{file: f, ids: map[string]bool{}}
	c.pages[f] = p
	z := html.NewTokenizer(bytes.NewReader(b))
	line := 1
	base := ""
	inTitle, inStyle := false, false
	var titleText strings.Builder
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		raw := z.Raw()
		tokLine := line
		line += bytes.Count(raw, []byte{'\n'})
		switch tt {
		case html.TextToken:
			if inTitle {
				titleText.Write(z.Text())
			}
			if inStyle {
				c.scanCSS(f, string(z.Text()), tokLine, base, p)
			}
		case html.EndTagToken:
			n, _ := z.TagName()
			switch string(n) {
			case "title":
				inTitle = false
				if strings.TrimSpace(titleText.String()) != "" {
					p.hasTitle = true
				}
			case "style":
				inStyle = false
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			tag := tok.Data
			attrs := map[string]string{}
			for _, a := range tok.Attr {
				k := a.Key
				if a.Namespace != "" {
					k = a.Namespace + ":" + a.Key
				}
				attrs[strings.ToLower(k)] = a.Val
			}
			if id := attrs["id"]; id != "" {
				p.ids[id] = true
			}
			if tag == "a" && attrs["name"] != "" {
				p.ids[attrs["name"]] = true
			}
			if s, ok := attrs["style"]; ok {
				c.scanCSS(f, s, tokLine, base, p)
			}
			switch tag {
			case "html":
				if strings.TrimSpace(attrs["lang"]) != "" {
					p.lang = true
				}
			case "title":
				if tt == html.StartTagToken {
					inTitle = true
					titleText.Reset()
				}
			case "style":
				if tt == html.StartTagToken {
					inStyle = true
				}
			case "base":
				if h, ok := attrs["href"]; ok {
					base = h
				}
			case "meta":
				if strings.EqualFold(attrs["name"], "viewport") {
					p.hasViewport = true
				}
				if _, ok := attrs["charset"]; ok {
					p.hasCharset = true
				}
				if strings.EqualFold(attrs["http-equiv"], "content-type") && strings.Contains(strings.ToLower(attrs["content"]), "charset") {
					p.hasCharset = true
				}
				if strings.EqualFold(attrs["http-equiv"], "refresh") {
					p.isRedir = true
				}
			case "div":
				if attrs["id"] == "root" || attrs["id"] == "app" || attrs["id"] == "__next" {
					p.rootDiv = true
				}
			case "form":
				if _, ok := attrs["data-netlify"]; ok {
					c.netlifyForm(f, tokLine)
				} else if _, ok := attrs["netlify"]; ok {
					c.netlifyForm(f, tokLine)
				}
			}
			rel := strings.ToLower(attrs["rel"])
			if tag == "link" {
				for _, r := range strings.Fields(rel) {
					if r == "icon" || r == "shortcut" || r == "apple-touch-icon" {
						p.hasIcon = true
					}
				}
			}
			if tag == "script" && strings.EqualFold(attrs["type"], "module") && attrs["src"] != "" {
				p.moduleSrc = append(p.moduleSrc, attrs["src"])
			}
			for attr, val := range attrs {
				if !urlAttrs[tag][attr] {
					continue
				}
				if tag == "link" {
					skip := false
					for _, r := range strings.Fields(rel) {
						if skipLinkRels[r] {
							skip = true
						}
					}
					if skip {
						continue
					}
				}
				isLink := tag == "a" || tag == "area"
				active := tag == "script" || tag == "iframe" || tag == "frame" || tag == "embed" || tag == "object" || (tag == "link" && strings.Contains(rel, "stylesheet"))
				if attr == "srcset" {
					for _, cand := range strings.Split(val, ",") {
						fs := strings.Fields(cand)
						if len(fs) > 0 {
							p.refs = append(p.refs, ref{file: f, line: tokLine, tag: tag, attr: attr, value: resolveBase(base, fs[0]), asset: true})
						}
					}
					continue
				}
				p.refs = append(p.refs, ref{file: f, line: tokLine, tag: tag, attr: attr, value: resolveBase(base, val), asset: !isLink, active: active})
			}
		}
	}
}

func resolveBase(base, v string) string {
	v = strings.TrimSpace(v)
	if base == "" || v == "" || strings.HasPrefix(v, "#") {
		return v
	}
	bu, err := url.Parse(base)
	if err != nil {
		return v
	}
	vu, err := url.Parse(v)
	if err != nil {
		return v
	}
	if bu.IsAbs() {
		return bu.ResolveReference(vu).String()
	}
	// Relative <base>: keep it relative to the page.
	return bu.ResolveReference(vu).String()
}

var cssURLRe = regexp.MustCompile(`(?i)url\(\s*(?:"([^"]*)"|'([^']*)'|([^)'"\s]*))\s*\)|@import\s+(?:"([^"]*)"|'([^']*)')`)
var cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

func cssURLs(css string) []struct {
	v    string
	line int
} {
	css = cssCommentRe.ReplaceAllStringFunc(css, func(s string) string { return strings.Repeat("\n", strings.Count(s, "\n")) })
	var out []struct {
		v    string
		line int
	}
	for _, m := range cssURLRe.FindAllStringSubmatchIndex(css, -1) {
		v := ""
		for g := 1; g <= 5; g++ {
			if m[2*g] >= 0 {
				v = css[m[2*g]:m[2*g+1]]
				break
			}
		}
		out = append(out, struct {
			v    string
			line int
		}{strings.TrimSpace(v), strings.Count(css[:m[0]], "\n")})
	}
	return out
}

func (c *checker) scanCSS(f, css string, startLine int, base string, p *pageInfo) {
	for _, u := range cssURLs(css) {
		if u.v == "" || strings.HasPrefix(u.v, "#") {
			continue
		}
		p.refs = append(p.refs, ref{file: f, line: startLine + u.line, tag: "css", attr: "url", value: resolveBase(base, u.v), asset: true, active: false})
	}
}

func (c *checker) scanCSSFile(f string) {
	b, err := os.ReadFile(filepath.Join(c.ws.Publish, filepath.FromSlash(f)))
	if err != nil {
		return
	}
	if c.cssRefs == nil {
		c.cssRefs = map[string][]ref{}
	}
	for _, u := range cssURLs(string(b)) {
		if u.v == "" || strings.HasPrefix(u.v, "#") {
			continue
		}
		c.cssRefs[f] = append(c.cssRefs[f], ref{file: f, line: 1 + u.line, tag: "css", attr: "url", value: u.v, asset: true})
	}
}

func (c *checker) netlifyForm(f string, line int) {
	c.res["netlify"].item("warn", f, line, "This form uses Netlify Forms (data-netlify / netlify attribute). Submissions are handled by Netlify's servers and will not work on HostPanel; use a form service (Formspree, Basin, …) or your own backend.")
}

var winPathRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
var skipSchemes = []string{"mailto:", "tel:", "sms:", "javascript:", "data:", "blob:", "about:", "geo:", "skype:", "whatsapp:", "callto:"}
var assetLikeExt = map[string]bool{".js": true, ".mjs": true, ".css": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".avif": true, ".svg": true, ".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true, ".json": true, ".map": true, ".mp4": true, ".webm": true, ".mp3": true, ".wasm": true, ".pdf": true, ".txt": true, ".xml": true, ".webmanifest": true}

// pageURL is the address a file is served at (clean URLs).
func pageURL(file string) string {
	switch {
	case file == "index.html":
		return "/"
	case strings.HasSuffix(file, "/index.html"):
		return "/" + strings.TrimSuffix(file, "index.html")
	}
	return "/" + file
}

func (c *checker) checkRef(r ref, links, assets, cased, local, mixed *CheckResult) {
	v := strings.TrimSpace(r.value)
	if v == "" {
		return
	}
	lv := strings.ToLower(v)
	for _, s := range skipSchemes {
		if strings.HasPrefix(lv, s) {
			return
		}
	}
	what := fmt.Sprintf("<%s %s=\"%s\">", r.tag, r.attr, trunc(v, 120))
	if r.tag == "css" {
		what = "url(" + trunc(v, 120) + ")"
	}
	switch {
	case strings.HasPrefix(lv, "file:"):
		local.item("fail", r.file, r.line, what+" points to a file on your PC.")
		return
	case winPathRe.MatchString(v) || strings.HasPrefix(v, `\\`):
		local.item("fail", r.file, r.line, what+" is a Windows path on your PC.")
		return
	case strings.Contains(v, `\`):
		local.item("fail", r.file, r.line, what+" contains a backslash (\\); use / in web addresses.")
		return
	}
	if strings.HasPrefix(lv, "http://") || strings.HasPrefix(lv, "https://") || strings.HasPrefix(lv, "//") {
		u, err := url.Parse(v)
		if err == nil {
			h := strings.ToLower(u.Hostname())
			if h == "localhost" || h == "127.0.0.1" || h == "0.0.0.0" || h == "[::1]" || h == "::1" {
				local.item("fail", r.file, r.line, what+" points to a server on your own PC; visitors can't reach it.")
				return
			}
		}
		if strings.HasPrefix(lv, "http://") && r.asset {
			if r.active {
				mixed.item("fail", r.file, r.line, what+" will be blocked on https.")
			} else {
				mixed.item("warn", r.file, r.line, what+" is insecure; browsers may block it or show 'Not secure'.")
			}
		}
		return // external: not fetched
	}
	if strings.HasPrefix(lv, "#") {
		frag := v[1:]
		if frag != "" && frag != "top" && !strings.HasPrefix(frag, "!") && !strings.HasPrefix(frag, "/") {
			if p := c.pages[r.file]; p != nil && !p.ids[frag] && !p.ids[unescapeFrag(frag)] {
				c.res["fragments"].item("warn", r.file, r.line, what+": no element with id=\""+frag+"\" on this page.")
			}
		}
		return
	}
	if strings.Contains(v, "{{") || strings.Contains(v, "${") || strings.Contains(v, "<%") {
		return // template placeholder, not a real URL
	}
	baseURL := pageURL(r.file)
	if r.tag == "css" && strings.HasSuffix(strings.ToLower(r.file), ".css") {
		baseURL = "/" + r.file
	}
	bu, _ := url.Parse(baseURL)
	vu, err := url.Parse(v)
	if err != nil {
		links.item("warn", r.file, r.line, what+" is not a valid address.")
		return
	}
	full := bu.ResolveReference(vu)
	target, ok := cleanURLPath(full.Path)
	if !ok {
		local.item("fail", r.file, r.line, what+" is not a valid site path.")
		return
	}
	res := c.ws.Rules.resolve(c.ws.Files, target, full.RawQuery)
	hops := 0
	for res.Location != "" && hops < 5 && strings.HasPrefix(res.Location, "/") {
		lu, err := url.Parse(res.Location)
		if err != nil {
			break
		}
		lp, ok := cleanURLPath(lu.Path)
		if !ok {
			break
		}
		res = c.ws.Rules.resolve(c.ws.Files, lp, lu.RawQuery)
		hops++
	}
	if res.Location != "" {
		return // redirects elsewhere (external, or a long chain)
	}
	result := links
	noun := "Link"
	if r.asset {
		result = assets
		noun = "File"
	}
	if res.Status >= 400 {
		if m := c.caseVariant(target); m != "" {
			cased.item("fail", r.file, r.line, fmt.Sprintf("%s uses %s but the file is %s.", what, target, "/"+m))
			return
		}
		result.item("fail", r.file, r.line, fmt.Sprintf("%s → %s not found (%d).", what, target, res.Status))
		return
	}
	// Assets "found" only because a catch-all rewrite returned a page.
	ext := strings.ToLower(path.Ext(target))
	if r.asset && res.Via != "" && assetLikeExt[ext] && isHTMLName(res.File) && !isHTMLName(target) {
		if m := c.caseVariant(target); m != "" {
			cased.item("fail", r.file, r.line, fmt.Sprintf("%s uses %s but the file is %s (the rewrite rule hides this as %s).", what, target, "/"+m, res.File))
			return
		}
		rule := "a rewrite rule"
		if res.Rule != nil {
			rule = fmt.Sprintf("the rule %q (%s line %d)", res.Rule.From+" "+res.Rule.To, res.Rule.Source, res.Rule.Line)
		}
		assets.item("fail", r.file, r.line, fmt.Sprintf("%s → %s does not exist; %s answers with %s instead, which breaks the page.", what, target, rule, res.File))
		return
	}
	// Fragment on another page.
	if vu.Fragment != "" && isHTMLName(res.File) && !r.asset {
		if p := c.pages[res.File]; p != nil && !p.ids[vu.Fragment] {
			c.res["fragments"].item("warn", r.file, r.line, fmt.Sprintf("%s: %s has no element with id=\"%s\".", what, res.File, vu.Fragment))
		}
	}
	_ = noun
}

func unescapeFrag(s string) string {
	u, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return u
}

func (c *checker) caseVariant(target string) string {
	rel := strings.TrimPrefix(target, "/")
	for _, cand := range []string{rel, strings.TrimSuffix(rel, "/") + ".html", strings.TrimSuffix(rel, "/") + "/index.html", rel + "index.html"} {
		if cand == "" || cand == "/index.html" {
			continue
		}
		if m := c.ws.Files.caseMatch(cand); m != "" {
			return m
		}
	}
	// Directory part differs in case.
	return ""
}

func (c *checker) checkMeta() {
	var pages []string
	for f := range c.pages {
		pages = append(pages, f)
	}
	sort.Strings(pages)
	icon := c.ws.Files.IsFile("favicon.ico")
	for _, f := range pages {
		p := c.pages[f]
		if p.isRedir && !p.hasTitle {
			continue
		}
		if strings.HasPrefix(strings.ToLower(path.Base(f)), "google") && len(p.refs) == 0 {
			continue // verification files
		}
		if !p.hasTitle {
			c.res["title"].item("warn", f, 0, "No <title>.")
		}
		if !p.hasViewport {
			c.res["viewport"].item("warn", f, 0, "No <meta name=\"viewport\">.")
		}
		if !p.lang {
			c.res["lang"].item("warn", f, 0, "No lang on <html>.")
		}
		if !p.hasCharset {
			c.res["charset"].item("warn", f, 0, "No <meta charset>.")
		}
		if p.hasIcon {
			icon = true
		}
	}
	if !icon && len(pages) > 0 {
		c.res["favicon"].item("warn", "", 0, "No favicon.ico and no <link rel=\"icon\"> on any page.")
	}
}

func (c *checker) checkRules(r *CheckResult) {
	rules := c.ws.Rules
	if rules == nil {
		return
	}
	var parts []string
	if n := len(rules.Redirects); n > 0 {
		parts = append(parts, fmt.Sprintf("%d redirect/rewrite rule%s", n, plural(n)))
	}
	if n := len(rules.Headers); n > 0 {
		parts = append(parts, fmt.Sprintf("%d header rule%s", n, plural(n)))
	}
	for _, is := range rules.Issues {
		st := is.Level
		if st != "fail" && st != "warn" {
			st = "info"
		}
		d := is.Msg
		if is.Fix != "" {
			d += " Fix: " + is.Fix
		}
		r.item(st, is.Source, is.Line, d)
	}
	// Shadowed and unreachable rules.
	catchAll := -1
	for i, rr := range rules.Redirects {
		if catchAll >= 0 && !rr.Force {
			prev := rules.Redirects[catchAll]
			r.item("warn", rr.Source, rr.Line, fmt.Sprintf("Rule %q is never used: the earlier catch-all %q (%s line %d) matches first.", rr.From, prev.From, prev.Source, prev.Line))
		}
		if rr.From == "/*" && catchAll < 0 {
			catchAll = i
		}
		if !rr.Force && !strings.ContainsAny(rr.From, "*:") {
			p, _ := cleanURLPath(rr.From)
			if st, ok := resolveStatic(c.ws.Files, p, ""); ok && st.File != "" && st.Status == 200 {
				r.item("warn", rr.Source, rr.Line, fmt.Sprintf("Rule %q does nothing because the file %s exists (existing files win). Add ! after the status code (e.g. 301!) to force it.", rr.From, st.File))
			}
		}
		if rr.Force {
			for j := 0; j < i; j++ {
				if !rules.Redirects[j].Force {
					r.item("info", rr.Source, rr.Line, fmt.Sprintf("Forced rule %q is checked before the non-forced rules above it on the live server. Put forced rules first in your file to get identical behaviour everywhere.", rr.From))
					break
				}
			}
		}
	}
	if len(parts) == 0 {
		r.Summary = "No _redirects, _headers or netlify.toml rules (that's fine for most simple sites)."
		r.Status = "pass"
	} else if r.Status == "pass" {
		r.Summary = strings.Join(parts, " and ") + "; all valid. They are applied here and on the live server."
	} else {
		r.Summary = strings.Join(parts, " and ") + " found; see the notes below."
	}
}

func (c *checker) hasCatchAll200() bool {
	for _, r := range c.ws.Rules.Redirects {
		if r.Status == 200 && (r.From == "/*" || r.From == "/") {
			return true
		}
	}
	return false
}

func (c *checker) checkSPA() {
	r := c.res["spa"]
	p := c.pages["index.html"]
	spa := p != nil && p.rootDiv
	htmlPages := 0
	for f := range c.pages {
		if f != "404.html" && f != "200.html" {
			htmlPages++
		}
	}
	if !spa || htmlPages > 2 {
		r.Summary = "Not a single-page app (nothing to do)."
		return
	}
	if c.hasCatchAll200() {
		r.Summary = "Single-page app with a catch-all rule: deep links work."
		return
	}
	r.item("warn", "index.html", 0, "This looks like a single-page app (one page with <div id=\"root\"/\"app\">) but there is no catch-all rewrite. If it uses client-side routes, add _redirects with  /*  /index.html  200")
	r.Summary = "Possible single-page app without a fallback rule."
	// Unbuilt Vite source: <script type=module src="/src/main.tsx">
	for _, s := range p.moduleSrc {
		ls := strings.ToLower(s)
		if strings.HasSuffix(ls, ".jsx") || strings.HasSuffix(ls, ".tsx") || strings.HasSuffix(ls, ".ts") || strings.HasSuffix(ls, ".vue") {
			c.res["source"].item("fail", "index.html", 0, "index.html loads "+s+" directly; browsers can't run that. This is the unbuilt project: run the build and use its output folder.")
		}
	}
}

func (c *checker) checkNetlify() {
	r := c.res["netlify"]
	p := c.ws.Project
	if p.FunctionsDir != "" {
		r.item("warn", p.FunctionsDir, 0, "Netlify Functions / Edge Functions found. They don't run on HostPanel; anything calling /.netlify/functions/… will fail.")
	}
	for _, f := range c.ws.Files.sorted() {
		if strings.HasPrefix(f, ".netlify/") {
			r.item("info", f, 0, "The .netlify folder is Netlify CLI state and is not needed on the server.")
			break
		}
	}
	if r.Status == "pass" {
		r.Summary = "No Netlify Functions or Netlify Forms used."
	}
}
