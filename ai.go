package main

// The Assistant: local AI auto-detection, cloud providers, the chat proxy,
// the action (confirm card) protocol, privacy (consent, redaction) and the
// offline helper. The browser never talks to an AI server directly; every
// request goes through this backend (CSP connect-src 'self').

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- local AI detection ----------

type localServer struct {
	ID, Name, Base, Probe, Site string
}

// Well-known default ports of popular local AI apps. All of them expose an
// OpenAI-compatible /v1 API (Ollama since 0.1.24).
var localServers = []localServer{
	{"ollama", "Ollama", "http://127.0.0.1:11434/v1", "http://127.0.0.1:11434/api/tags", "https://ollama.com"},
	{"lmstudio", "LM Studio", "http://127.0.0.1:1234/v1", "", "https://lmstudio.ai"},
	{"jan", "Jan", "http://127.0.0.1:1337/v1", "", "https://jan.ai"},
	{"gpt4all", "GPT4All", "http://127.0.0.1:4891/v1", "", "https://gpt4all.io"},
	{"textgen", "text-generation-webui", "http://127.0.0.1:5000/v1", "", "https://github.com/oobabooga/text-generation-webui"},
	{"vllm", "vLLM", "http://127.0.0.1:8000/v1", "", "https://docs.vllm.ai"},
	{"llamacpp", "llama.cpp / LocalAI", "http://127.0.0.1:8080/v1", "", "https://github.com/ggml-org/llama.cpp"},
}

type LocalAI struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Base   string   `json:"base"`
	Models []string `json:"models"`
}

var (
	localMu    sync.Mutex
	localCache []LocalAI
	localAt    time.Time
)

func detectLocalAI(ctx context.Context, fresh bool) []LocalAI {
	localMu.Lock()
	if !fresh && time.Since(localAt) < 20*time.Second {
		out := localCache
		localMu.Unlock()
		return out
	}
	localMu.Unlock()
	var wg sync.WaitGroup
	res := make([]*LocalAI, len(localServers))
	for i, s := range localServers {
		wg.Add(1)
		go func(i int, s localServer) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
			defer cancel()
			a := &openAIAdapter{name: s.Name, base: s.Base}
			models, err := a.Models(c)
			if err != nil && s.Probe != "" { // older Ollama: native tags endpoint
				var r *http.Response
				r, err = aiDo(c, s.Name, "GET", s.Probe, nil, nil)
				if err == nil {
					var t struct {
						Models []struct{ Name string } `json:"models"`
					}
					_ = json.NewDecoder(r.Body).Decode(&t)
					r.Body.Close()
					for _, m := range t.Models {
						models = append(models, m.Name)
					}
				}
			}
			if err == nil {
				res[i] = &LocalAI{ID: s.ID, Name: s.Name, Base: s.Base, Models: models}
			}
		}(i, s)
	}
	wg.Wait()
	var out []LocalAI
	for _, r := range res {
		if r != nil {
			out = append(out, *r)
		}
	}
	localMu.Lock()
	localCache, localAt = out, time.Now()
	localMu.Unlock()
	return out
}

// ---------- targets ----------

type aiTarget struct {
	Label   string
	Company string
	Cloud   bool
	Adapter Adapter
	Model   string
	Native  bool
	ConnID  string
}

// resolveTarget turns "local:<id>" or "<provider>" + model into an adapter.
func resolveTarget(ctx context.Context, target, model string) (*aiTarget, error) {
	if id, ok := strings.CutPrefix(target, "local:"); ok {
		for _, l := range detectLocalAI(ctx, false) {
			if l.ID == id {
				if model == "" && len(l.Models) > 0 {
					model = l.Models[0]
				}
				return &aiTarget{Label: l.Name, Company: l.Name, Adapter: &openAIAdapter{name: l.Name, base: l.Base}, Model: model}, nil
			}
		}
		return nil, &AIError{Kind: "network", Message: "That local AI app is not running any more. Start it again, or pick another model."}
	}
	p, ok := providerByID(target)
	if !ok {
		return nil, &AIError{Kind: "bad_request", Message: "Unknown AI provider."}
	}
	c := snapshotConfig()
	conn := c.AIConns[p.ID]
	if conn == nil || conn.Key == "" && !p.Custom {
		return nil, &AIError{Kind: "auth", Message: p.Name + " is not connected yet. Open Settings > Connect AI and paste an API key."}
	}
	key := secret(conn.Key)
	if model == "" {
		model = conn.Model
	}
	if model == "" {
		return nil, &AIError{Kind: "bad_request", Message: "Choose a model for " + p.Name + " in Settings > Connect AI."}
	}
	t := &aiTarget{Label: p.Name, Company: p.Company, Cloud: true, Model: model, Native: p.NativeTools, ConnID: p.ID}
	switch p.Kind {
	case "anthropic":
		t.Adapter = &anthropicAdapter{base: p.BaseURL, key: key}
	case "gemini":
		t.Adapter = &geminiAdapter{base: p.BaseURL, key: key}
	default:
		base := p.BaseURL
		if p.Custom {
			base = conn.BaseURL
			if conn.Name != "" {
				t.Label, t.Company = conn.Name, conn.Name
			}
			t.Cloud = !isLoopbackURL(base)
		}
		extra := map[string]string{}
		if p.ID == "openrouter" { // optional attribution headers documented by OpenRouter
			extra["X-Title"] = "Dilreet"
		}
		t.Adapter = &openAIAdapter{name: t.Label, base: base, key: key, extra: extra}
	}
	return t, nil
}

func isLoopbackURL(u string) bool {
	u = strings.ToLower(u)
	for _, p := range []string{"http://127.", "http://localhost", "http://[::1]"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

func adapterForTest(p ProviderInfo, key, base string) Adapter {
	switch p.Kind {
	case "anthropic":
		return &anthropicAdapter{base: p.BaseURL, key: key}
	case "gemini":
		return &geminiAdapter{base: p.BaseURL, key: key}
	}
	if p.Custom {
		return &openAIAdapter{name: "your server", base: base, key: key}
	}
	return &openAIAdapter{name: p.Name, base: p.BaseURL, key: key}
}

func keyHint(k string) string {
	k = strings.TrimSpace(k)
	if len(k) <= 8 {
		return "••••"
	}
	pre := k[:3]
	if i := strings.IndexAny(k[:min(len(k), 8)], "-_"); i > 0 {
		pre = k[:i+1]
	}
	return pre + "…" + k[len(k)-4:]
}

// ---------- actions (confirm cards) ----------

var aiTools = []ToolDef{
	{"open_page", "Open a page of Dilreet for the user.", obj(map[string]any{"page": enum("projects", "new-project", "domains", "testing", "assistant", "settings", "connect-ai", "storage")}, "page")},
	{"setup_server", "Prepare the connected server and install the hosting engine (Coolify). Long-running.", obj(map[string]any{}, "")},
	{"create_site", "Create a new empty project (site).", obj(map[string]any{"name": str("Project name")}, "name")},
	{"open_project", "Open a project on one of its tabs.", obj(map[string]any{"site": str("project slug"), "tab": enum("overview", "files", "deploys", "domain", "settings")}, "site")},
	{"open_project_folder", "Open the project's folder on this PC in File Explorer.", obj(map[string]any{"site": str("project slug")}, "site")},
	{"redeploy_site", "Restart/redeploy an existing site with its current files.", obj(map[string]any{"site": str("site slug")}, "site")},
	{"pull_and_deploy", "Fetch the latest commit from the site's Git repository and deploy it.", obj(map[string]any{"site": str("site slug")}, "site")},
	{"rollback_site", "Switch a site back to its previous deploy.", obj(map[string]any{"site": str("site slug")}, "site")},
	{"set_domain", "Connect a custom domain to a project: opens the domain wizard (GoDaddy/Namecheap/Porkbun/Hostinger/Squarespace guided steps, registrar API, or Cloudflare) with the domain filled in.", obj(map[string]any{"site": str("site slug"), "domain": str("example.com")}, "site", "domain")},
	{"auto_dns", "Create the DNS records at Cloudflare for the site's domain (needs a Cloudflare token).", obj(map[string]any{"site": str("site slug")}, "site")},
	{"check_dns", "Check whether the site's domain already points to the server.", obj(map[string]any{"site": str("site slug")}, "site")},
	{"toggle_auto_deploy", "Turn auto-deploy on new Git commits on or off.", obj(map[string]any{"site": str("site slug"), "on": map[string]any{"type": "boolean"}}, "site", "on")},
	{"install_ollama", "Download and install Ollama (free local AI) on this PC.", obj(map[string]any{}, "")},
}

func obj(props map[string]any, req ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	var r []string
	for _, x := range req {
		if x != "" {
			r = append(r, x)
		}
	}
	if len(r) > 0 {
		m["required"] = r
	}
	return m
}
func str(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
func enum(v ...string) map[string]any {
	return map[string]any{"type": "string", "enum": v}
}

func toolByName(n string) *ToolDef {
	for i := range aiTools {
		if aiTools[i].Name == n {
			return &aiTools[i]
		}
	}
	return nil
}

// actionLabel is the human text on the confirm card.
func actionLabel(tc ToolCall) string {
	s := func(k string) string { v, _ := tc.Args[k].(string); return v }
	switch tc.Name {
	case "open_page":
		return "Open " + s("page")
	case "setup_server":
		return "Set up the server (install the hosting engine)"
	case "create_site":
		return fmt.Sprintf("Create a new project %q", s("name"))
	case "open_project":
		t := s("tab")
		if t == "" {
			t = "overview"
		}
		return "Open " + s("site") + " (" + t + ")"
	case "open_project_folder":
		return "Open the folder of " + s("site") + " in File Explorer"
	case "redeploy_site":
		return "Redeploy " + s("site")
	case "pull_and_deploy":
		return "Pull the latest commit and deploy " + s("site")
	case "rollback_site":
		return "Roll back " + s("site") + " to the previous deploy"
	case "set_domain":
		return fmt.Sprintf("Connect %s to %s", s("domain"), s("site"))
	case "auto_dns":
		return "Create DNS records at Cloudflare for " + s("site")
	case "check_dns":
		return "Check DNS for " + s("site")
	case "toggle_auto_deploy":
		on, _ := tc.Args["on"].(bool)
		if on {
			return "Turn on auto-deploy for " + s("site")
		}
		return "Turn off auto-deploy for " + s("site")
	case "install_ollama":
		return "Download and install Ollama (about 1 GB with a small model)"
	}
	return tc.Name
}

var actionBlockRe = regexp.MustCompile("(?s)```action\\s*(\\{.*?\\})\\s*```")

// parseTextActions implements the JSON-in-text fallback used with models
// that have no native tool calling (most local models).
func parseTextActions(text string) []ToolCall {
	var out []ToolCall
	for _, m := range actionBlockRe.FindAllStringSubmatch(text, 5) {
		var a struct {
			Name   string         `json:"name"`
			Action string         `json:"action"`
			Args   map[string]any `json:"args"`
		}
		if json.Unmarshal([]byte(m[1]), &a) != nil {
			continue
		}
		if a.Name == "" {
			a.Name = a.Action
		}
		if a.Args == nil {
			a.Args = map[string]any{}
		}
		if toolByName(a.Name) == nil {
			continue // unknown actions are never offered
		}
		out = append(out, ToolCall{Name: a.Name, Args: a.Args})
	}
	return out
}

// ---------- privacy ----------

var (
	reIPv4    = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	reIPv6    = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){3,7}[0-9a-fA-F]{1,4}\b`)
	reSecrets = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(bearer|token|password|passwd|secret|api[_-]?key|authorization)\b(["']?\s*[:=]\s*["']?|\s+)[^\s"',;]{4,}`),
		regexp.MustCompile(`\b(sk|pk|rk|xai|gsk|ghp|gho|ghs|github_pat|glpat|nfp|sk-ant|sk-or)[-_][A-Za-z0-9_\-]{12,}`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{30,}`),
		regexp.MustCompile(`\b\d+\|[A-Za-z0-9]{30,}\b`), // Laravel Sanctum (Coolify) tokens
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`\b[A-Fa-f0-9]{40,}\b`),
	}
	reSslipIP = regexp.MustCompile(`\.(\d{1,3}(?:[.-]\d{1,3}){3})\.(sslip|nip)\.io`)
)

// redact removes secrets always and IP addresses when ips is true.
func redact(s string, ips bool) string {
	for _, re := range reSecrets {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			if sm := reSecrets[0].FindStringSubmatch(m); sm != nil {
				return sm[1] + sm[2] + "[hidden]"
			}
			return "[hidden]"
		})
	}
	if ips {
		s = reSslipIP.ReplaceAllString(s, ".[server-ip].$2.io")
		s = reIPv4.ReplaceAllString(s, "[server-ip]")
		s = reIPv6.ReplaceAllString(s, "[ip]")
	}
	return s
}

// appContext summarises Dilreet's state for the model. It never includes
// passwords, tokens or keys (those are not even read here).
func appContext() string {
	c := snapshotConfig()
	var b strings.Builder
	fmt.Fprintf(&b, "Dilreet %s on %s. ", Version, osName())
	if c.ServerIP == "" {
		b.WriteString("No server connected yet. ")
	} else {
		fmt.Fprintf(&b, "Server %s (SSH %s). ", c.ServerIP, map[bool]string{true: "connected", false: "not connected"}[sshS.connected()])
		if c.CoolifyToken != "" {
			b.WriteString("Hosting engine (Coolify) connected. ")
		} else {
			b.WriteString("Hosting engine not set up yet. ")
		}
	}
	if c.CloudflareToken != "" {
		b.WriteString("Cloudflare DNS connected. ")
	}
	if len(c.Sites) == 0 {
		b.WriteString("No sites yet.")
	}
	for _, s := range c.Sites {
		fmt.Fprintf(&b, "\n- site %q (slug %s)", s.Name, s.Slug)
		if s.InstantURL != "" {
			fmt.Fprintf(&b, ", free URL %s", s.InstantURL)
		}
		if s.Domain != "" {
			fmt.Fprintf(&b, ", domain %s (live=%v, https=%v, dns=%s)", s.Domain, s.DomainLive, s.HTTPSActive, s.DNSMode)
		}
		if s.LastDeploy != "" {
			fmt.Fprintf(&b, ", last deploy %s", s.LastDeploy)
		}
		if s.Git != nil {
			fmt.Fprintf(&b, ", git %s@%s auto-deploy=%v", s.Git.Display, s.Git.Branch, s.Git.AutoDeploy)
		}
	}
	return b.String()
}

const aiSystemBase = `You are Jiv, the friendly helper built into Dilreet, a Windows desktop app that publishes websites to the user's own server (like Netlify, but self-hosted with Coolify).
Be warm, short and concrete; the user may not be technical. Use numbered steps for instructions. You may also answer general questions on any topic.
You can PROPOSE actions. Nothing runs until the user clicks the confirm card, so never claim an action already happened.
Never ask for passwords, API keys or tokens in chat; tell the user where in Dilreet to enter them.
Dilreet pages: Projects (list), Add new project (drop a folder/zip, import from Git or a link, or from Netlify), each project has Overview, Files (the project's folder on this PC: Documents\Dilreet\Projects\<name>\current, preview/edit/deploy from it), Deploys (every deploy is kept as a zip on this PC; publish any of them again, compare, download), Domain and Settings tabs. Also Domains, App Testing (local preview), Jiv (full chat) and Settings (server, Connect AI, storage, accounts, updates).
Domains: the wizard detects the registrar. GoDaddy, Namecheap, Porkbun, Hostinger and Squarespace get exact guided steps with a button to their DNS page; with a GoDaddy (Personal Access Token or Production key) or Porkbun API key Dilreet creates the records itself. "Fully automatic" moves DNS to Cloudflare: Dilreet adds the zone and records and shows the two nameservers to paste at the registrar. GoDaddy: replace the A record "@" (often shown as Parked), keep CNAME www -> @, remove Forwarding; its DNS API works for accounts with 1+ domain since April 2026.`

const aiTextProtocol = `
To propose an action, add a fenced block exactly like this (one per action):
` + "```action\n{\"name\": \"redeploy_site\", \"args\": {\"site\": \"my-site\"}}\n```" + `
Available actions: %s`

func systemPrompt(native, withContext bool, extra string, cloud bool) string {
	s := aiSystemBase
	if !native {
		var names []string
		for _, t := range aiTools {
			names = append(names, t.Name+" ("+t.Description+")")
		}
		s += fmt.Sprintf(aiTextProtocol, strings.Join(names, "; "))
	}
	if withContext {
		ctxs := appContext()
		if extra != "" {
			ctxs += "\nThe user is asking about this message from the app:\n" + extra
		}
		if cloud {
			ctxs = redact(ctxs, true)
		} else {
			ctxs = redact(ctxs, false)
		}
		s += "\n\nCurrent app state:\n" + ctxs
	}
	return s
}

// ---------- offline helper ----------

type faq struct {
	keys   []string
	answer string
}

var offlineFAQ = []faq{
	{[]string{"godaddy", "parked", "domaincontrol"}, "GoDaddy, step by step: 1. In the Domain tab click **Open GoDaddy DNS**. 2. Edit the **A** record named **@** (it often says **Parked**) and paste your server IP (copy button in Dilreet), TTL 600. 3. Delete other A/AAAA records for @. 4. Keep the **CNAME www → @**. 5. Remove any **Forwarding** and disconnect Website Builder/Airo if used. Or connect a GoDaddy API key (accounts with 1+ domain can use it since April 2026) and Dilreet does it for you."},
	{[]string{"namecheap", "porkbun", "hostinger", "squarespace", "google domains", "registrar"}, "Open the project's **Domain** tab and type the domain: Dilreet detects the registrar and shows the exact steps for it (Namecheap: Advanced DNS, delete the parking records, add A @ and A www; Porkbun: delete the default ALIAS, add A records, or connect a Porkbun API key; Hostinger: DNS / Nameservers; Squarespace: remove the defaults, add custom A records)."},
	{[]string{"file", "folder", "explorer", "edit", "documents", "archive", "history", "storage"}, "Every project has a folder on this PC: **Documents\\Dilreet\\Projects\\<name>**. **current** holds the live files: open the **Files** tab to preview them, open them in an editor or File Explorer, then click **Deploy from this folder**. **deploys** keeps a zip of every deploy; the **Deploys** tab lets you download, compare or publish any of them again. Change the location or a keep-limit in **Settings > Storage**."},
	{[]string{"domain", "dns", "nameserver", "a record"}, "To use your own domain: open the project, go to the **Domain** tab and type it. Dilreet detects where it is registered. For GoDaddy, Namecheap, Porkbun, Hostinger or Squarespace you get exact steps and a button that opens their DNS page; with a GoDaddy or Porkbun API key (or Cloudflare) Dilreet creates the records itself. **Fully automatic** moves the DNS to Cloudflare and shows the two nameservers to paste at your registrar. Dilreet keeps checking and notifies you when the domain is live."},
	{[]string{"https", "ssl", "certificate", "secure", "padlock"}, "HTTPS is automatic. After the domain points to your server, the hosting engine requests a free Let's Encrypt certificate (usually 1-5 minutes). Dilreet checks it in the background and notifies you. If it takes longer than 30 minutes, check that ports 80 and 443 are open."},
	{[]string{"deploy", "upload", "publish", "drag", "drop", "zip"}, "To publish: click **Add new project** and drag your site folder or .zip onto the drop area, or choose **Import from Git** / **Import from Netlify**. Each upload becomes a deploy kept on this PC, which you can publish again from the project's **Deploys** tab."},
	{[]string{"git", "github", "gitlab", "bitbucket", "repo"}, "Use **Add new project > Import from Git or link**, paste the repository URL, pick the branch and (optionally) a subfolder. Private repos need **Connect account** with a read-only token. Turn on auto-deploy to redeploy when new commits arrive while Dilreet is open."},
	{[]string{"netlify"}, "To move a site from Netlify: **Add new project > Import from Netlify**, paste a Netlify personal access token, pick the site. Dilreet copies the published files, redirects and headers, and offers to move the custom domain."},
	{[]string{"rollback", "previous", "undo", "revert"}, "Open the project, go to **Deploys**, and click **Publish** on an older one. If the server still has it, the switch is instant; otherwise Dilreet uploads it again from the copy on this PC."},
	{[]string{"server", "vps", "hetzner", "digitalocean", "ssh"}, "Go to **Settings > Server**. Enter the server's IP and root password (used only for this session, never stored) or choose an SSH key. Then click **Set up server** and Dilreet installs everything."},
	{[]string{"ai", "ollama", "model", "openai", "claude", "gemini", "grok", "assistant"}, "The assistant works with local AI apps (Ollama, LM Studio, Jan, GPT4All, llama.cpp...) detected automatically, or with a cloud provider you connect in **Settings > Connect AI**. Without either, you get these built-in answers. Want a free local AI? Click **Install Ollama** on the Jiv page."},
	{[]string{"test", "preview", "check", "broken", "link"}, "Open **App Testing** and drop your site there. Dilreet serves it only on this PC, runs checks (broken links, missing files, redirects) and lets you click through it before publishing."},
	{[]string{"update", "upgrade", "version"}, "Dilreet checks for updates in the background. Open **Settings > Updates** to install a new version or upgrade the hosting engine."},
}

func offlineAnswer(q string) string {
	q = strings.ToLower(q)
	best, score := -1, 0
	for i, f := range offlineFAQ {
		n := 0
		for _, k := range f.keys {
			if strings.Contains(q, k) {
				n++
			}
		}
		if n > score {
			best, score = i, n
		}
	}
	if best < 0 {
		return "I'm Jiv, running in offline mode (no AI connected), so I can only answer from Dilreet's guide: domains (GoDaddy, Namecheap, Porkbun...), HTTPS, deploying, project files and deploy history, Git and Netlify import, servers, testing and updates. To ask me anything else, connect an AI: start a local AI app such as Ollama or LM Studio, or add a provider in **Settings > Connect AI**."
	}
	return offlineFAQ[best].answer
}

// ---------- HTTP ----------

type chatReq struct {
	Target         string    `json:"target"`
	Model          string    `json:"model"`
	Messages       []ChatMsg `json:"messages"`
	IncludeContext bool      `json:"include_context"`
	About          string    `json:"about"` // error text from "Ask assistant about this"
}

func registerAIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ai/providers", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		type pv struct {
			ProviderInfo
			Connected bool   `json:"connected"`
			KeyHint   string `json:"key_hint,omitempty"`
			Model     string `json:"model,omitempty"`
			Consent   bool   `json:"consent"`
			ConnBase  string `json:"conn_base,omitempty"`
			ConnName  string `json:"conn_name,omitempty"`
		}
		var out []pv
		for _, p := range cloudProviders {
			x := pv{ProviderInfo: p}
			if cn := c.AIConns[p.ID]; cn != nil {
				x.Connected = cn.Key != "" || (p.Custom && cn.BaseURL != "")
				x.KeyHint, x.Model, x.Consent, x.ConnBase, x.ConnName = cn.KeyHint, cn.Model, cn.Consent, cn.BaseURL, cn.Name
			}
			out = append(out, x)
		}
		writeJSON(w, 200, map[string]any{
			"providers":  out,
			"local":      detectLocalAI(r.Context(), r.URL.Query().Get("fresh") == "1"),
			"default":    c.AIDefault,
			"no_context": c.AINoContext,
			"local_apps": localServers,
		})
	})
	// Test a key (pasted or saved) by listing models. Nothing is saved.
	mux.HandleFunc("POST /api/ai/providers/{id}/test", func(w http.ResponseWriter, r *http.Request) {
		p, ok := providerByID(r.PathValue("id"))
		if !ok {
			writeErr(w, errors.New("unknown provider"))
			return
		}
		var req struct {
			Key     string `json:"key"`
			BaseURL string `json:"base_url"`
		}
		_ = readJSON(r, &req)
		key := strings.TrimSpace(req.Key)
		c := snapshotConfig()
		if key == "" && c.AIConns[p.ID] != nil {
			key = secret(c.AIConns[p.ID].Key)
			if req.BaseURL == "" {
				req.BaseURL = c.AIConns[p.ID].BaseURL
			}
		}
		if p.Custom {
			if err := checkCustomBase(req.BaseURL); err != nil {
				writeErr(w, err)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		models, err := adapterForTest(p, key, req.BaseURL).Models(ctx)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "models": models})
	})
	mux.HandleFunc("POST /api/ai/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := providerByID(r.PathValue("id"))
		if !ok {
			writeErr(w, errors.New("unknown provider"))
			return
		}
		var req struct {
			Key     *string `json:"key"`
			Model   *string `json:"model"`
			BaseURL *string `json:"base_url"`
			Name    *string `json:"name"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if p.Custom && req.BaseURL != nil {
			if err := checkCustomBase(*req.BaseURL); err != nil {
				writeErr(w, err)
				return
			}
		}
		err := withConfig(func(c *Config) error {
			if c.AIConns == nil {
				c.AIConns = map[string]*AIConn{}
			}
			cn := c.AIConns[p.ID]
			if cn == nil {
				cn = &AIConn{}
				c.AIConns[p.ID] = cn
			}
			if req.Key != nil {
				k := strings.TrimSpace(*req.Key)
				if k == "" {
					cn.Key, cn.KeyHint = "", ""
				} else {
					enc, err := protectString(k)
					if err != nil {
						return err
					}
					cn.Key, cn.KeyHint = enc, keyHint(k)
				}
			}
			if req.Model != nil {
				cn.Model = strings.TrimSpace(*req.Model)
			}
			if req.BaseURL != nil {
				cn.BaseURL = strings.TrimRight(strings.TrimSpace(*req.BaseURL), "/")
			}
			if req.Name != nil {
				cn.Name = strings.TrimSpace(*req.Name)
			}
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("DELETE /api/ai/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		_ = withConfig(func(c *Config) error {
			delete(c.AIConns, id)
			if strings.HasPrefix(c.AIDefault, id+"|") {
				c.AIDefault = ""
			}
			return nil
		})
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/ai/consent/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := withConfig(func(c *Config) error {
			cn := c.AIConns[id]
			if cn == nil {
				return errors.New("connect this provider first")
			}
			cn.Consent = true
			return nil
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/ai/settings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Default   *string `json:"default"`
			NoContext *bool   `json:"no_context"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		_ = withConfig(func(c *Config) error {
			if req.Default != nil {
				c.AIDefault = *req.Default
			}
			if req.NoContext != nil {
				c.AINoContext = *req.NoContext
			}
			return nil
		})
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/ai/offline", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Question string }
		_ = readJSON(r, &req)
		writeJSON(w, 200, map[string]any{"answer": offlineAnswer(req.Question)})
	})
	mux.HandleFunc("POST /api/ai/chat", apiAIChat)
	mux.HandleFunc("POST /api/ai/ollama/install", apiOllamaInstall)
	mux.HandleFunc("POST /api/ai/ollama/pull", apiOllamaPull)
}

func checkCustomBase(u string) error {
	u = strings.TrimSpace(u)
	if !(strings.HasPrefix(u, "https://") || isLoopbackURL(u) || strings.HasPrefix(u, "http://192.168.") || strings.HasPrefix(u, "http://10.")) {
		return errors.New("use an https:// address (plain http:// is only allowed for this PC or your home network)")
	}
	return nil
}

func apiAIChat(w http.ResponseWriter, r *http.Request) {
	var req chatReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	t, err := resolveTarget(r.Context(), req.Target, req.Model)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	c := snapshotConfig()
	if t.Cloud && t.ConnID != "" {
		if cn := c.AIConns[t.ConnID]; cn == nil || !cn.Consent {
			writeJSON(w, 409, map[string]any{"need_consent": t.ConnID, "company": t.Company, "error": "consent needed"})
			return
		}
	}
	withCtx := req.IncludeContext && !(t.Cloud && c.AINoContext)
	msgs := req.Messages
	if len(msgs) > 40 {
		msgs = msgs[len(msgs)-40:]
	}
	if t.Cloud { // secrets the user typed by mistake never leave the PC
		for i := range msgs {
			msgs[i].Content = redact(msgs[i].Content, false)
		}
	}
	creq := ChatRequest{Model: t.Model, System: systemPrompt(t.Native, withCtx, req.About, t.Cloud), Messages: msgs}
	if t.Native {
		creq.Tools = aiTools
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(v map[string]any) {
		_ = enc.Encode(v)
		if fl != nil {
			fl.Flush()
		}
	}
	send(map[string]any{"t": "start", "label": t.Label, "model": t.Model, "cloud": t.Cloud, "context": withCtx})
	var full strings.Builder
	var calls []ToolCall
	err = t.Adapter.Stream(r.Context(), creq, func(ev StreamEvent) {
		if ev.Text != "" {
			full.WriteString(ev.Text)
			send(map[string]any{"t": "text", "v": ev.Text})
		}
		if ev.Tool != nil {
			calls = append(calls, *ev.Tool)
		}
	})
	if err != nil {
		var ae *AIError
		if !errors.As(err, &ae) {
			ae = mapAIError(t.Label, 0, nil, err)
		}
		send(map[string]any{"t": "error", "kind": ae.Kind, "v": ae.Message})
		return
	}
	calls = append(calls, parseTextActions(full.String())...)
	for _, tc := range calls {
		if toolByName(tc.Name) == nil {
			continue // unknown actions are dropped, never executed
		}
		send(map[string]any{"t": "action", "name": tc.Name, "args": tc.Args, "label": actionLabel(tc)})
	}
	send(map[string]any{"t": "done"})
}

// ---------- Ollama setup (only after the user clicks the confirm card) ----------

const ollamaInstallerURL = "https://ollama.com/download/OllamaSetup.exe"

var (
	ollamaMu     sync.Mutex
	ollamaCancel context.CancelFunc
)

func stopOllamaPull() {
	ollamaMu.Lock()
	defer ollamaMu.Unlock()
	if ollamaCancel != nil {
		ollamaCancel()
		ollamaCancel = nil
	}
}

func apiOllamaInstall(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "windows" {
		writeJSON(w, 200, map[string]any{"manual": true, "url": "https://ollama.com/download"})
		return
	}
	j := newJob("ollama-install", func(j *Job) error {
		j.SetSteps("Downloading Ollama", "Running the installer", "Waiting for Ollama to start")
		j.Step(0)
		dst := filepath.Join(os.TempDir(), "OllamaSetup-dilreet.exe")
		defer os.Remove(dst)
		if err := downloadFile(context.Background(), ollamaInstallerURL, dst, 3<<30, nil, j); err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
		j.Step(1)
		j.Logf("The Ollama installer window opens now. Follow it to finish.")
		if err := exec.Command(dst).Run(); err != nil {
			return fmt.Errorf("installer did not finish: %w", err)
		}
		j.Step(2)
		for i := 0; i < 60; i++ {
			if l := detectLocalAI(context.Background(), true); len(l) > 0 {
				for _, x := range l {
					if x.ID == "ollama" {
						j.Logf("Ollama is running.")
						return nil
					}
				}
			}
			time.Sleep(2 * time.Second)
		}
		return errors.New("Ollama was installed but is not running yet; start it from the Start menu")
	})
	writeJSON(w, 200, map[string]any{"job": j.ID})
}

var modelNameRe = regexp.MustCompile(`^[a-zA-Z0-9._:/-]{1,80}$`)

func apiOllamaPull(w http.ResponseWriter, r *http.Request) {
	var req struct{ Model string }
	_ = readJSON(r, &req)
	if req.Model == "" {
		req.Model = "llama3.2:3b"
	}
	if !modelNameRe.MatchString(req.Model) {
		writeErr(w, errors.New("invalid model name"))
		return
	}
	stopOllamaPull()
	ctx, cancel := context.WithCancel(context.Background())
	ollamaMu.Lock()
	ollamaCancel = cancel
	ollamaMu.Unlock()
	j := newJob("ollama-pull", func(j *Job) error {
		defer cancel()
		j.Logf("Downloading model %s with Ollama...", req.Model)
		// Ollama API: POST /api/pull {"model": ..., "stream": true} -> NDJSON
		// {status, digest, total, completed}.
		resp, err := aiDo(ctx, "Ollama", "POST", "http://127.0.0.1:11434/api/pull", nil, map[string]any{"model": req.Model, "stream": true})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		last := ""
		for {
			var ev struct {
				Status    string `json:"status"`
				Total     int64  `json:"total"`
				Completed int64  `json:"completed"`
				Error     string `json:"error"`
			}
			if err := dec.Decode(&ev); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			if ev.Error != "" {
				return errors.New(ev.Error)
			}
			if ev.Total > 0 {
				j.SetProgress(int(ev.Completed*100/ev.Total), ev.Status)
			}
			if ev.Status != last {
				j.Logf("%s", ev.Status)
				last = ev.Status
			}
		}
		_ = withConfig(func(c *Config) error {
			if c.AIDefault == "" {
				c.AIDefault = "local:ollama|" + req.Model
			}
			return nil
		})
		detectLocalAI(context.Background(), true)
		return nil
	})
	writeJSON(w, 200, map[string]any{"job": j.ID})
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
