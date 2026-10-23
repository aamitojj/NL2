# Dilreet automation: what runs by itself, what you still click

Rule for everything below: Dilreet only calls documented APIs, never sends
data anywhere you did not choose, and every state-changing action is shown as a
confirm card or button you click first. Background work only *reads* (status
polling, DNS checks, Git "is there a new commit?" checks) and applies changes you
already approved (for example "auto-deploy this repo").

## Server and Coolify

| Step | How | Manual? |
|---|---|---|
| Create a VPS | Hetzner Cloud `POST /v1/servers`, DigitalOcean `POST /v2/droplets` with your API token (DPAPI-encrypted on this PC) | You paste a token, pick a size, click **Create** |
| Install Coolify | SSH + the official `install.sh`, live log | One click |
| Coolify admin + API token | Created over SSH with `php artisan tinker` on the server (no browser step); token named `Dilreet`; the admin login is kept DPAPI-encrypted on this PC | None |
| Instant address | `https://<project>.<server-ip>.sslip.io` | None |
| Health checks | Server reachability and project status every 10 minutes while Dilreet runs | None (read-only) |

Server paths stay `/data/hostpanel/sites/<id>/` so servers set up by older
versions keep working; the name is internal and never shown as the app name.

## Projects on this PC

`Documents\Dilreet\Projects\<name>\` (location changeable in Settings → Storage):

```
current\            latest deployed files (edit and "Deploy from this folder")
deploys\<time>_<id>.zip  every deploy, kept per the retention setting
deploys\index.json  deploy list: id, time, source, file count, size, checks, commit
```

Existing sites from earlier versions are migrated on first start (folders are created,
nothing on the server changes). Retention: all, or last 5/10/20/50 (the live deploy
is always kept).

## Domains

Dilreet looks up the domain's nameservers over public DNS and recognises the
DNS host / registrar from them (e.g. `domaincontrol.com` = GoDaddy), then offers, in this order:

1. **Guided (default, any registrar).** Numbered steps for that registrar, a button
   that opens its DNS page, copy buttons for every value, warnings about
   conflicting A/AAAA/CNAME records and parking pages, and a live check every 15 s
   that notifies you and connects the domain when DNS is right.
2. **Registrar API (GoDaddy, Porkbun).** You paste a key once; Dilreet shows the
   records it will create and asks before it writes them.
3. **Fully automatic (Cloudflare).** Dilreet creates the zone in your Cloudflare
   account (`POST /client/v4/zones`), copies the existing records it found,
   adds the site records, and shows the two Cloudflare nameservers. With a GoDaddy
   personal access token it can also switch the nameservers at GoDaddy for you
   (separate confirm). Otherwise you paste the nameservers at your registrar once.

### GoDaddy

* **Who can use the API:** since April 2026, GoDaddy production DNS and
  Domains API keys work for any account with **at least one domain**. Before
  that, production access needed 10+ domains (or a Discount Domain Club / Pro
  plan), which is why many tools still say "GoDaddy API doesn't work for small
  accounts". The Availability API still needs 50+ domains. Sources:
  <https://www.godaddy.com/resources/news/godaddy-dns-api-now-works-with-a-single-domain>,
  <https://community.letsencrypt.org/t/godaddy-api-access-policy-update/246372>.
* **Auth:** v1 `Authorization: sso-key KEY:SECRET` (keys from
  <https://developer.godaddy.com/keys>, choose **Production**, not OTE), or a v3
  personal access token `Authorization: Bearer …` with `domains.dns:update`
  (and `domains.nameservers:update` for the nameserver switch). Source:
  <https://developer.godaddy.com/en/docs/api-users/auth>.
* **Endpoints used:** v1 `GET/PUT /v1/domains/{domain}/records/{type}/{name}`;
  v3 DNS records and nameservers endpoints as documented on developer.godaddy.com.
* The wizard shows this restriction plainly and falls back to Guided if the key
  is rejected (401/403 with the access message).

### Porkbun

API v3 (`https://api.porkbun.com/api/json/v3/`): `ping`, `dns/retrieve/{domain}`,
`dns/editByNameType/{domain}/{type}/{sub}` (upsert), `dns/create/{domain}`.
API access must be enabled **per domain** in the Porkbun dashboard. Source:
<https://porkbun.com/api/json/v3/documentation>.

### Namecheap

Guided only. Its API requires a whitelisted public IPv4 address and account
eligibility (balance / number of domains), which does not fit a desktop app on a
changing home IP. Source: <https://www.namecheap.com/support/api/intro/>.

### Hostinger, Squarespace, others

Guided steps with a direct link to the DNS page.

### Domain Connect (documented, not implemented)

[Domain Connect](https://www.domainconnect.org/) lets a service apply a DNS
"template" at the user's DNS provider after one click. It needs the template to be
published and onboarded with every DNS provider (GoDaddy, IONOS, Cloudflare, …)
and a public service identity, so it is not usable by a self-hosted desktop app
today. If Dilreet ever gets a public template, the guided mode is where it would
appear.

## Git and Netlify

* GitHub / GitLab / Bitbucket / Azure DevOps / Gitea / Codeberg / Forgejo, plus
  plain `.git` links and `.zip`/`.tar.gz` URLs: archive download through the
  provider's documented API; optional read-only token (encrypted). Auto-deploy is
  available for the named providers (not for plain links or archive URLs). Auto-deploy polls the branch head (every 5 minutes by default,
  configurable per project), with limits on download size, file count and build time.
* Netlify: site list, files of the published deploy, domain and `_redirects`,
  with a personal access token.

## Jiv (assistant)

Local AI apps are found by probing well-known loopback ports only (Ollama 11434,
LM Studio 1234, Jan 1337, GPT4All 4891, llama.cpp 8080, LocalAI, vLLM 8000,
text-generation-webui 5000). Nothing leaves the PC unless you connect a cloud
provider and accept its one-time consent; passwords, tokens and keys are never
sent, and the "Include my status" switch can be turned off.

## What still needs you

* Renting the server (or pasting a cloud API token).
* Buying a domain; for Guided mode, adding 1–2 records (or nameservers) at the registrar.
* Approving each change (confirm cards).
* The first Coolify admin password is shown once for you to save.
