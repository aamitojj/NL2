# HostPanel v0.1

A small Windows app for running your own "Netlify" on one VPS with
[Coolify](https://coolify.io). It installs Coolify for you, then lets you create
static websites, upload them as a `.zip` or a folder, and connect domains with
automatic HTTPS. You don't need GitHub, Docker knowledge, or the command line.

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
6. **Settings tab**: check for and install HostPanel updates, see the installed
   and latest Coolify versions, update Coolify on the server, list the Coolify
   projects, and quit.

The **Guide** tab inside the app covers choosing a VPS, DNS step by step
(including Cloudflare), and troubleshooting.

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
  custom header on every state-changing request (CSRF).
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
  "version": "0.1.1",
  "url": "https://downloads.example.com/hostpanel/0.1.1/hostpanel.exe",
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
go build -ldflags "-s -w -X main.Version=0.1.1" -o hostpanel.exe .
```

`devtools/mockcoolify` is a development-only fake Coolify API, used for
end-to-end testing without a real server. It is not part of the exe.

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
| `web/` | the embedded UI (HTML/CSS/JS, no external assets) |

## Known limitations (v0.1)

* Only tested against a mock Coolify API, a local SSH server, and Wine. It has
  **not** been tested against a real VPS running Coolify. See the test notes
  in the release report.
* The storages API (`fs_path` bind mounts) needs a recent Coolify v4 (it is in the current
  official OpenAPI spec; older versions answer 404/405, and HostPanel then asks
  you to update Coolify) and a token with `write`+`deploy` (or `root`) permission. Coolify
  rejects host paths outside its resource directory for tokens that lack
  `deploy` permission.
* Static sites only: no server-side code, no build step (upload the built
  output, e.g. `dist/`). Nginx uses its default config, so there are no SPA
  fallback rules or custom redirects yet.
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
