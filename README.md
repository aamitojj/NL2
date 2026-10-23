# HostPanel v0.2

A small Windows app for running your own "Netlify" on one VPS with
[Coolify](https://coolify.io). It installs Coolify for you, then lets you create
static websites, upload them as a `.zip` or a folder, and connect domains with
automatic HTTPS. You don't need GitHub, Docker knowledge, or the command line.

**New in 0.2: App Testing.** Test a site on your own PC before it goes live: a
local preview server that behaves like Netlify (`_redirects`, `_headers`,
`netlify.toml`), a built-in browser with phone/tablet/desktop sizes and a
console panel, a checks report (broken links, missing files, case problems,
secrets...), and a **Deploy this version** button that uploads exactly what
you tested. The same redirect and header rules now also work on the server:
HostPanel turns them into the site's nginx config on every deployment.

`hostpanel.exe` is one file. When it starts, it runs a local web UI on
`http://127.0.0.1:8787` (or a random free port) and opens your browser. Keep the
console window open while you use it. Closing the window, or clicking **Quit**,
stops it.

---

## Quick start

1. **Rent a VPS** running Ubuntu 22.04/24.04 or Debian 12 (x86-64 or ARM64).
   For ~100 small static sites, 4 vCPU / 8 GB RAM / 80 GB SSD is comfortable
   (Hetzner CX32/CPX31, DigitalOcean 8 GB, Vultr 8 GB...). Coolify's minimum is
   2 vCPU / 2 GB / 30 GB. Use a fresh server.
2. **Open these firewall ports** (in the provider's cloud firewall if there is one):
   22 (SSH), 80 and 443 (websites), 8000 (Coolify dashboard), 6001 and 6002
   (Coolify realtime/terminal).
3. Run `hostpanel.exe`. Windows SmartScreen may warn you because the exe is
   unsigned: click *More info → Run anyway*.
4. **Setup tab**
   1. Enter the server IP, SSH user (usually `root`), and your password, or the
      path to a private key (OpenSSH format; if you use PuTTY `.ppk` files,
      convert them with PuTTYgen → *Conversions → Export OpenSSH key*).
   2. Click **Connect**. The first time, the server's host key fingerprint is
      saved, and HostPanel refuses to connect later if it changes.
   3. Click **Install Coolify**. This runs the official installer
      `curl -fsSL https://cdn.coollabs.io/coolify/install.sh | bash` and shows the
      log live. It takes 2-10 minutes.
   4. **Straight away**, open `http://YOUR_IP:8000` and create the admin account.
      The first person to open this page becomes the admin.
   5. In Coolify, go to **Keys & Tokens → API tokens** and create a token with
      **root** permission (or at least `read`, `write` and `deploy`). If API
      access is turned off, turn it on under **Settings → Advanced**.
   6. Paste the dashboard URL (`http://YOUR_IP:8000`) and the token into
      HostPanel, then click **Save & test**. If you tick *"Talk to the API
      through the SSH connection"*, the token never goes over the internet
      unencrypted. This is recommended until the Coolify dashboard itself has
      HTTPS.
5. **Sites tab**
   1. Type a name and click **Create site**. A few seconds later it has a
      temporary `http://<id>.<ip>.sslip.io` address.
   2. Pick a `.zip`, a folder, or type a path on this PC, then click **Deploy**.
      The folder must contain `index.html`. If everything is inside one wrapper
      folder (e.g. `dist/`), that folder is removed automatically.
   3. Enter a domain such as `example.com` and click **Set domain**. HostPanel
      shows the exact DNS records to create (A `@` → your IP, A `www` → your IP).
      Add them at your registrar, then leave the page open. It checks DNS every
      15 s through 1.1.1.1 and 8.8.8.8. When the records match, it connects the
      domain in Coolify, and Coolify's proxy gets a Let's Encrypt certificate on
      its own.
   4. **Releases** keeps the last 5 deployments, and you can make an older one
      live again instantly (rollback).
6. **App Testing tab** (optional, recommended before every deploy): see below.
7. **Settings tab**: check for and install HostPanel updates, see the installed
   and latest Coolify versions, update Coolify on the server, list the Coolify
   projects, and quit.

The **Guide** tab inside the app covers choosing a VPS, DNS step by step
(including Cloudflare), testing before you deploy, and troubleshooting.

---

## App Testing (new in 0.2)

1. **Load a site**: pick a `.zip`, a folder, or type a path on this PC (paths
   can be reloaded after you edit files). HostPanel copies it into a private temp
   workspace (`%TEMP%\hostpanel-test-*`, deleted when you load another site,
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
5. **Deploy this version**: choose a site and click the button. If any check
   failed, HostPanel lists the failures and asks you to confirm. Otherwise it
   asks once, showing the build fingerprint. It then deploys the **exact files you
   tested** from the temp workspace through the normal deploy (SFTP, new
   release, atomic switch, rollback). If the workspace was changed or removed
   meanwhile, it refuses.

### Why an iframe and not WebView2

The built-in browser is an `<iframe>` in the HostPanel page, which runs in your
normal browser (Edge/Chrome/Firefox). WebView2 would need cgo or a native
Windows window. That would break the single, pure-Go exe and add a runtime
dependency. The iframe still gets a real browser engine, and the injected script
captures console and network problems. The preview runs on a different origin
(`localhost` vs. the UI's `127.0.0.1`), so the previewed site cannot read or
call HostPanel. Limits: the preview can't show other websites in the frame (most
forbid it), and sites that sent `X-Frame-Options`/`frame-ancestors` have those
headers relaxed **in the preview only** (logged as info). For full DevTools,
click **Open in browser**.

### Redirects and headers on the server

On every deployment HostPanel converts the rules into
`/data/hostpanel/sites/<id>/nginx/default.conf`. This folder is mounted into
the container at `/etc/nginx/conf.d`, with one config per release in
`nginx-releases/`, so rollbacks restore the matching rules. HostPanel runs
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

When you deploy, HostPanel:

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
HostPanel falls back to `GET` automatically.

---

## Security

* **SSH passwords and key passphrases are never saved.** They stay in memory
  only while the app runs, so you enter them once per session. Use key
  authentication if you'd rather not type a password.
* The server's SSH host key is pinned on first connect (trust on first use).
  If it changes, HostPanel refuses to connect.
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
  route to the HostPanel API (`/api/...` on the preview is just a path of your
  site). Its only link back is a log endpoint on the preview's own port, which
  accepts browser-log messages from the preview page.
* If your SSH user isn't root, commands run through `sudo`, and the password
  you typed (memory only) is used for sudo.
* Self-updates are only accepted over HTTPS and only installed if the SHA-256
  matches.

Config file: `%APPDATA%\HostPanel\config.json`. It holds the server IP, user,
key path, pinned host key, Coolify URL, the encrypted token, the update URL,
and your site list. It never contains your SSH password.

---

## Self-update

HostPanel shows its version in the header and in Settings. Under **Settings →
Update manifest URL**, set an HTTPS URL that returns JSON like this:

```json
{
  "version": "0.2.1",
  "url": "https://downloads.example.com/hostpanel/0.2.1/hostpanel.exe",
  "sha256": "3f1c0d9b8e7a6f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c",
  "notes": "Fixes DNS check for .co.uk domains."
}
```

To get the SHA-256 of a new build:

* Windows: `certutil -hashfile hostpanel.exe SHA256`
  (or PowerShell `Get-FileHash hostpanel.exe`)
* Linux/macOS: `sha256sum hostpanel.exe`

When you click **Check for updates → Download & install**, HostPanel:

1. downloads the new exe next to the current one (`hostpanel.exe.new`);
2. checks the SHA-256;
3. renames the running exe to `hostpanel.exe.old` (Windows allows renaming a
   running exe);
4. moves the new exe into place;
5. starts it with the same arguments, then exits.

The new version deletes `.old` when it starts. If any step fails, the old exe
is put back. Because the new process opens a new browser tab, close the old one.

You can host the manifest and exe anywhere that serves HTTPS, e.g. GitHub
Releases, an S3/R2 bucket, or one of your own HostPanel sites. The default URL
(`https://example.com/hostpanel/latest.json`) is only a placeholder.

**Updating Coolify:** *Settings → Update Coolify on the server* runs the official
installer again over SSH, which upgrades an existing installation to the latest
version. The installed version comes from `GET /api/v1/version`, and the latest
version from `https://cdn.coollabs.io/coolify/versions.json`. Coolify's API has
no "upgrade" endpoint. Your websites keep running while the dashboard restarts.

---

## Command-line flags

```
hostpanel.exe [-port 8787] [-no-browser] [-print-url]
```

The environment variable `HOSTPANEL_CONFIG_DIR` overrides the config folder (for
testing).

## Building from source

Requires Go 1.26 or newer.

```
go vet ./...
go test ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o hostpanel.exe .
# set the version at build time:
go build -ldflags "-s -w -X main.Version=0.2.1" -o hostpanel.exe .
```

`devtools/mockcoolify` is a development-only fake Coolify API, used for
end-to-end testing without a real server. It is not part of the exe.

Tests (`go test ./...`):

* `rules_test.go`: `_redirects`/`_headers`/`netlify.toml` parsing and
  issues, splat/placeholder matching, shadowing, SPA, 404/custom status, header
  matching, clean URLs, script injection, the link checker and the other checks.
* `preview_test.go`: the preview server (loopback-only listeners, Host check,
  no API access, method limits, injection, headers, redirects, SPA, 404). It
  also runs a differential test that starts a real `nginx` (if installed) with
  the generated config and compares status, `Location`, body and headers with
  the preview for a list of paths. It is skipped when nginx isn't available.
* `hostpanel_test.go`: archive cleaning, DNS helpers, config.

## Source layout

| File | Purpose |
|---|---|
| `main.go` | HTTP server, routes, auth, browser launch |
| `sshclient.go` | SSH connect (password/key/keyboard-interactive), host-key pinning, sudo, SFTP |
| `coolify.go` | Coolify API v1 client (only documented endpoints) |
| `sites.go` | create / deploy / rollback / delete sites |
| `archive.go` | zip/folder → sanitized tar.gz |
| `dns.go` | required records + public-resolver DNS checks |
| `update.go` | self-update, Coolify install/upgrade |
| `secret_windows.go` | DPAPI token encryption |
| `netlifyrules.go` | `_redirects` / `_headers` / `netlify.toml` parser and matcher |
| `workspace.go` | App Testing temp workspace (load zip/folder/path, publish dir, fingerprint) |
| `preview.go`, `inject.js` | local preview server and the injected logging script |
| `checks.go` | the checks report (links, assets, case, secrets, meta, rules...) |
| `apptest.go` | App Testing session, API handlers, "deploy this version" |
| `nginxconf.go` | rules → production nginx config |
| `web/` | the embedded UI (HTML/CSS/JS, no external assets) |

## Known limitations (v0.2)

* App Testing is a static preview. It doesn't run `npm install`/`npm run build`,
  Netlify Functions, Edge Functions, Forms, or server-side rendering (Next.js
  SSR etc.). Build on your PC first and load the output folder. The checks warn
  when they see unbuilt source or Netlify-only features.
* Rules that are not supported (and that the checks flag): domain-level rules,
  query-parameter matching, Country/Language/Role/Cookie conditions, proxying
  to other sites (`200` to an `https://` URL), and a `*` in the middle of a path.
* The built-in browser is an iframe, not a separate browser engine: no DevTools
  panel inside HostPanel (use *Open in browser*), no device emulation beyond the
  frame size (no touch or user-agent switching). Sites that break out of frames
  or need third-party cookies may behave differently there.
* Files are checked statically. Links created only by JavaScript are caught at
  runtime by the console panel, not by the report.
* Only tested against a mock Coolify API, a local SSH server, a local nginx
  (standing in for the site container), and Wine. It has **not** been tested
  against a real VPS running Coolify. See the test notes in the release report.
* The storages API (`fs_path` bind mounts) needs a recent Coolify v4 (it is in the current
  official OpenAPI spec; older versions answer 404/405, and HostPanel then asks
  you to update Coolify) and a token with `write`+`deploy` (or `root`) permission. Coolify
  rejects host paths outside its resource directory for tokens that lack
  `deploy` permission.
* Static sites only: no server-side code, no build step on the server (upload
  the built output, e.g. `dist/`). Redirect/header rules are supported (see
  App Testing), with the limits listed above.
* You have to create the DNS records yourself at your registrar. There's no
  registrar or Cloudflare API integration. If you use Cloudflare, set records
  to "DNS only" until the certificate has been issued.
* Apex vs. subdomain detection uses a heuristic (handles common two-part TLDs
  like `.co.uk`, but not every public suffix).
* The Coolify dashboard itself is plain HTTP on port 8000 unless you give it a
  domain in Coolify. Use the "API through SSH" option.
* Site details (which Coolify app belongs to which site) are kept in the local
  config. Sites created on another PC won't show up automatically.
* The exe is unsigned (SmartScreen warning), and a console window stays open
  while it runs.
* You have to enter the SSH password again each session (by design).
