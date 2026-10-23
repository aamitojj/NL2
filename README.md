# Dilreet v0.3

<img src="web/icon.png" width="64" align="right" alt="">

Dilreet is a Windows app that works like Netlify, but on **your own server**.
Drop a folder, paste a GitHub link or import from Netlify, and your site is online
with HTTPS in a minute. Under the hood it sets up and drives
[Coolify](https://coolify.io) on a VPS for you. You don't need Docker, the
command line or GitHub.

**New in 0.3**

* **A real app.** `dilreet.exe` opens its own window (WebView2, built into
  Windows 10/11), remembers its size and position, has a Start-menu entry and an
  installer (`Dilreet-Setup-0.3.0.exe`). `-browser` still opens it in your
  normal browser.
* **Projects**, like Netlify: a project list, **Add new project** (folder or
  zip, Git or link, Netlify), and per project **Overview / Files / Deploys /
  Domain / Settings**. Every project has a folder on your PC:
  `Documents\Dilreet\Projects\<name>\current` (the live files) and
  `deploys\<time>_<id>.zip` (every deploy) with `deploys\index.json`. Compare any two
  deploys, download one, publish an older one again, or open the folder in
  Explorer. **Settings → Storage** shows disk usage, moves the folder and sets how
  many deploys to keep.
* **Domains made easy.** Dilreet recognises where the domain is (GoDaddy,
  Namecheap, Porkbun, Hostinger, Squarespace, Cloudflare…) and shows exact steps
  with a button to that registrar's DNS page, copy buttons, warnings and a live
  check that notifies you when the domain works. With a **GoDaddy** or **Porkbun**
  API key it adds the records itself; **Fully automatic** moves DNS to a free
  Cloudflare account once. See [docs/AUTOMATION.md](docs/AUTOMATION.md).
* **Jiv**, the built-in assistant, is at the bottom of every screen ("Ask Jiv
  anything…"). It answers questions, explains errors, walks you through domains,
  and can do things for you after you click a confirm card. It connects to a free
  local AI on your PC (Ollama, LM Studio, Jan, GPT4All, llama.cpp, LocalAI, vLLM,
  text-generation-webui), or, only if you choose it, to a cloud AI (OpenAI,
  Anthropic, Gemini, xAI, OpenRouter, Mistral, Groq, DeepSeek or any
  OpenAI-compatible URL). Without AI it answers from the built-in guide.
* **Zero-touch server setup**: create a Hetzner or DigitalOcean server from the
  app, install Coolify and get an API token without opening the Coolify dashboard.
  Auto-deploy from Git, Netlify migration, and background checks.

---

## Quick start

1. Install with `Dilreet-Setup-0.3.0.exe` (or just run `dilreet.exe`). Windows
   SmartScreen may warn because the exe is unsigned: *More info → Run anyway*.
   If WebView2 is missing (rare on Windows 10/11), Dilreet offers to open the
   Microsoft download page or to use your browser.
2. **Connect a server.** Either paste a Hetzner/DigitalOcean API token and click
   **Create server**, or enter the IP and root password/SSH key of any fresh
   Ubuntu 22.04/24.04 VPS (2 vCPU / 2 GB minimum; 4 vCPU / 8 GB for ~100 sites).
   Open ports 22, 80, 443 (and 8000 for the Coolify dashboard). Click **Set up
   automatically**: Dilreet installs Coolify and creates its admin account and API
   token.
3. **Add new project** → drop a folder/zip, paste a Git link, or import from
   Netlify. You get `https://<name>.<server-ip>.sslip.io` straight away.
4. **Domain** tab → type your domain and follow the steps (or let Dilreet do it).
5. Edit files in the project's `current` folder and click **Deploy from this
   folder**, drop a new version, or push to Git with auto-deploy on.

Stuck? Ask Jiv.

---

## App Testing

1. **Load a site**: pick a `.zip`, a folder, or type a path on this PC (paths
   can be reloaded after you edit files). Dilreet copies it into a private temp
   workspace (`%TEMP%\dilreet-test-*`, deleted when you load another site,
   click **Stop**, or quit), using the same cleaning as deployments (zip-slip
   protection, no `__MACOSX`/`.git`/`node_modules`). The published folder is
   taken from `netlify.toml` (`[build] publish`), or auto-detected
   (`dist/`, `build/`, `out/`, `public/`... when it has `index.html`).
2. **Local preview server** on `http://localhost:<random port>`, only on the
   loopback interfaces (127.0.0.1 and ::1). It follows Netlify's rules:
   * `_redirects` and `[[redirects]]` / `[[headers]]` in `netlify.toml`, plus
     `_headers`;
   * status 200 (rewrite), 301, 302, 303, 307, 308, 404 and 410 (with a
     custom page), forced rules (`200!`);
   * splats (`/blog/*  /news/:splat`) and placeholders
     (`/users/:id  /profile.html?u=:id`);
   * "shadowing": an existing file wins over a non-forced rule, as on Netlify;
   * SPA fallback (`/*  /index.html  200`), custom `404.html`, clean URLs
     (`/about` → `about.html`, `/dir` → `/dir/`);
   * the rule files themselves are never served.
3. **Built-in browser**: address bar, back/forward/reload, home, open in your
   normal browser, sizes 375×667 (phone), 768×1024 (tablet), 1280×800
   (desktop), full width or a custom size, rotate and fit-to-window. A small
   script is injected **only into previewed HTML pages** (never into deployed
   files). It reports JavaScript errors, `console.error`/`warn`, unhandled promise
   rejections, failed images/scripts/styles, and failed `fetch`/XHR calls. The
   server also logs every 4xx/5xx response. Everything appears in the console
   panel ("Problems" or "All"). Links to other websites are not opened inside the
   preview; a notice offers to open them in your browser.
4. **Checks report**: every check is *pass*, *warn* or *fail*, with an
   explanation and how to fix it:
   home page, published folder, built output vs. unbuilt source (React/Vue/Vite/
   Angular/Next... detection), internal links, images/scripts/styles (incl.
   `srcset`, CSS `url()`, `@import`), upper/lower-case mismatches (work on
   Windows, 404 online), `C:\`/`file://` paths, `http://` mixed content,
   `#anchor` targets, large files, title/viewport/lang/charset/favicon,
   redirect/header rule problems, SPA routing, secret files (`.env`, keys,
   `.npmrc`...), Netlify-only features (Forms, Functions, Edge), and the custom
   404 page. The report also names missing files that an SPA rule hides by
   answering with `index.html`. **View** links open the affected page in the
   preview. **Export** downloads the report plus the browser log as JSON, and
   **nginx config** shows the exact server config the rules will produce.
5. **Deploy this version**: choose a project and click the button. If any check
   failed, Dilreet lists the failures and asks you to confirm. Otherwise it
   asks once, showing the build fingerprint. It then deploys the **exact files you
   tested** from the temp workspace through the normal deploy (SFTP, new
   release, atomic switch, rollback). If the workspace was changed or removed
   meanwhile, it refuses.

### The built-in browser

The built-in browser is an `<iframe>` inside the Dilreet window (WebView2, the
Edge engine), so it is a real browser engine, and the injected script captures
console and network problems. The preview runs on a different origin
(`localhost` vs. the UI's `127.0.0.1`), so the previewed site cannot read or
call Dilreet. Limits: the preview can't show other websites in the frame (most
forbid it), and sites that sent `X-Frame-Options`/`frame-ancestors` have those
headers relaxed **in the preview only** (logged as info). For full DevTools,
click **Open in browser**.

### Redirects and headers on the server

On every deployment Dilreet converts the rules into
`/data/hostpanel/sites/<id>/nginx/default.conf`. This folder is mounted into
the container at `/etc/nginx/conf.d`, with one config per release in
`nginx-releases/`, so rollbacks restore the matching rules. Dilreet runs
`nginx -t` before switching. If nginx rejects the config, the deployment stops
and the live site is unchanged. Then it reloads nginx (no restart). Sites created
with 0.1 get the mount added automatically on their first 0.2 deploy (one
container restart, about 30-60 s). A test suite runs the generated config in
a real nginx and compares every answer with the preview server.

---
## How deployments work without GitHub

Each site is **one Coolify project** containing **one "Docker Image"
application** (`nginx:stable-alpine`, port 80). These are created with the
documented API endpoints `POST /api/v1/projects` and
`POST /api/v1/applications/dockerimage`.

The site's files are on the server's disk at `/data/hostpanel/sites/<site-id>/`,
which is bind-mounted into the container at `/usr/share/nginx`
(`POST /api/v1/applications/{uuid}/storages`, type `file`, `is_directory: true`):

```
/data/hostpanel/sites/<site-id>/
├── html -> releases/20261023-101500      (symlink = what nginx serves)
└── releases/
    ├── 20261023-101500/   (newest, live)
    ├── 20261022-180312/
    └── initial/           (placeholder page)
```

When you deploy, Dilreet:

1. builds a cleaned `.tar.gz` on your PC. It blocks path tricks like `../`
   (zip-slip) and leaves out `__MACOSX`, `.DS_Store`, `Thumbs.db`, `.git`, and
   `node_modules`;
2. uploads it over SFTP;
3. unpacks it into a new release folder and switches the `html` symlink to it
   in one atomic step.

Nginx serves the new files immediately. No container rebuild or restart is
needed, and visitors never see a half-uploaded site.

Why this approach:

* It only uses API endpoints that exist in Coolify's official OpenAPI spec. A
  Dockerfile app with no git repository has no build context for your files,
  and pushing images would need a registry.
* Deploys take seconds and don't depend on Docker builds.
* Coolify still handles the parts it's good at: the container, the Traefik
  proxy, domains, and Let's Encrypt certificates.

Domains are set with `PATCH /api/v1/applications/{uuid}` (`domains`, optionally
`force_domain_override`), followed by `POST /api/v1/deploy?uuid=...` so the proxy
picks them up. For older Coolify versions where these actions are `GET`,
Dilreet falls back to `GET` automatically.

---
## Security

* **SSH passwords and key passphrases are never saved.** They stay in memory
  only while the app runs, so you enter them once per session. Use key
  authentication if you'd rather not type a password.
* The server's SSH host key is pinned on first connect (trust on first use).
  If it changes, Dilreet refuses to connect.
* The Coolify API token is saved encrypted with **Windows DPAPI**, which ties it
  to your Windows user account. On Linux and macOS development builds it is
  stored in a user-only (0600) file.
* The local web UI only listens on 127.0.0.1. It needs the random session key
  printed in the console (stored as an HttpOnly, SameSite=Strict cookie),
  rejects requests with a foreign `Host` header (DNS rebinding), and needs a
  custom header on every state-changing request (CSRF). In 0.2 it also
  rejects cross-site requests (`Sec-Fetch-Site`/`Origin` checks), sends a strict
  CSP, and refuses to be framed.
* The App Testing preview server only listens on loopback (127.0.0.1/::1),
  accepts only `GET`/`HEAD`, checks the `Host` header, runs on a different
  origin than the UI, and serves only files from the temp workspace. It has no
  route to the Dilreet API (`/api/...` on the preview is just a path of your
  site). Its only link back is a log endpoint on the preview's own port, which
  accepts browser-log messages from the preview page.
* If your SSH user isn't root, commands run through `sudo`, and the password
  you typed (memory only) is used for sudo.
* Self-updates are only accepted over HTTPS and only installed if the SHA-256
  matches.

* API keys for cloud AI, Git, Netlify, Cloudflare, Hetzner/DigitalOcean, GoDaddy
  and Porkbun are DPAPI-encrypted, never logged, and only shown again as a hint
  (`…a1b2`).
* Jiv uses AI on this PC by default. A cloud AI is used only after you add its
  key and accept a one-time consent per provider. SSH passwords, tokens and keys
  are never sent; secrets and IP addresses are redacted; "Include my status" can
  be switched off.
* Every change Jiv suggests appears as a card you must click. Nothing runs by itself.

Config file: `%APPDATA%\Dilreet\config.json` (an old `%APPDATA%\HostPanel`
folder is moved there automatically on first start). It holds the server IP, user,
key path, pinned host key, Coolify URL, encrypted tokens, the update URL and your
project list. It never contains your SSH password.

---
## Self-update

Dilreet shows its version in the header and in Settings. Under **Settings →
Update manifest URL**, set an HTTPS URL that returns JSON like this:

```json
{
  "version": "0.3.1",
  "url": "https://downloads.example.com/dilreet/0.3.1/dilreet.exe",
  "sha256": "3f1c0d9b8e7a6f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c",
  "notes": "Fixes DNS check for .co.uk domains."
}
```

To get the SHA-256 of a new build:

* Windows: `certutil -hashfile dilreet.exe SHA256`
  (or PowerShell `Get-FileHash dilreet.exe`)
* Linux/macOS: `sha256sum dilreet.exe`

When you click **Check for updates → Download & install**, Dilreet:

1. downloads the new exe next to the current one (`dilreet.exe.new`);
2. checks the SHA-256;
3. renames the running exe to `dilreet.exe.old` (Windows allows renaming a
   running exe);
4. moves the new exe into place;
5. starts it with the same arguments, then exits.

The new version deletes `.old` when it starts. If any step fails, the old exe
is put back. Because the new process opens a new browser tab, close the old one.

You can host the manifest and exe anywhere that serves HTTPS, e.g. GitHub
Releases, an S3/R2 bucket, or one of your own Dilreet projects. The default URL
(`https://example.com/dilreet/latest.json`) is only a placeholder.

**Updating Coolify:** *Settings → Update Coolify on the server* runs the official
installer again over SSH, which upgrades an existing installation to the latest
version. The installed version comes from `GET /api/v1/version`, and the latest
version from `https://cdn.coollabs.io/coolify/versions.json`. Coolify's API has
no "upgrade" endpoint. Your websites keep running while the dashboard restarts.

---
## Command-line flags

```
dilreet.exe [-browser] [-port 8787] [-no-browser] [-print-url] [-debug]
```

Environment variables for testing: `DILREET_CONFIG_DIR` (settings folder),
`DILREET_PROJECTS_DIR` (projects folder), `DILREET_NO_BACKGROUND=1`. The old
`HOSTPANEL_*` names still work.

## Building from source

Requires Go 1.26 or newer (pure Go, no cgo).

```
go vet ./... && go test -race ./...
go run github.com/tc-hib/go-winres@latest make --in winres/winres.json --arch amd64   # icon + manifest
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H=windowsgui -X main.Version=0.3.0" -o dilreet.exe .
iscc /DAppVersion=0.3.0 installer\dilreet.iss      # -> dist\Dilreet-Setup-0.3.0.exe
```

`.github/workflows/build.yml` runs the tests and builds `dilreet.exe`, the
installer and `SHA256SUMS.txt` on every push and tag. `devtools/mockcoolify` is a
development-only fake Coolify API used for end-to-end tests.

## Source layout

| File | Purpose |
|---|---|
| `main.go` | HTTP server, routes, auth, startup |
| `desktop_windows.go` | WebView2 window, single instance, window placement |
| `projects.go` | project folders, deploy archive, index.json, files/deploys/storage API |
| `registrars.go`, `nameservers.go` | registrar detection, GoDaddy/Porkbun APIs, Cloudflare move |
| `cloudflare.go`, `dns.go` | Cloudflare API, required records and DNS checks |
| `automation.go`, `cloudservers.go` | zero-touch Coolify setup, Hetzner/DigitalOcean |
| `gitimport.go`, `imports.go`, `netlify.go`, `build.go` | Git/link/Netlify import, auto-deploy, builds |
| `ai.go`, `ai_providers.go` | Jiv: local AI detection, cloud providers, tools, consent |
| `sshclient.go`, `coolify.go`, `sites.go` | SSH/SFTP, Coolify API, create/deploy/rollback |
| `workspace.go`, `preview.go`, `checks.go`, `apptest.go`, `netlifyrules.go`, `nginxconf.go` | App Testing |
| `secret_windows.go` | DPAPI encryption |
| `web/` | the embedded UI (no external assets) |

## Known limitations (v0.3)

* Static sites only (plus an optional local `npm run build`); no server-side code,
  Netlify Functions, Forms or SSR.
* The Files tab's HTML preview is sandboxed; pages that load images or CSS from
  the project may show without them there. Use App Testing for a full preview.
* GoDaddy/Porkbun/Cloudflare automation, Hetzner/DigitalOcean server creation,
  Netlify import and private Azure DevOps/Bitbucket repos were tested against mock
  APIs only, not live accounts.
* Namecheap, Hostinger and Squarespace use guided steps (no usable public API).
  Domain Connect is not supported (see docs/AUTOMATION.md).
* Project details live in the local config; projects created on another PC don't
  show up automatically.
* The exe and installer are unsigned (SmartScreen warning).
* You enter the SSH password once per session unless you use key auth (Dilreet
  can install a key for you during setup).
* Server paths are still `/data/hostpanel/sites/<id>` so servers set up by older
  versions keep working.
