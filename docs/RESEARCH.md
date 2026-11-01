# Dilreet 0.4 — research notes

Goal for 0.4: easier to use, more automatic, better organised for people with many
sites, with a Netlify-like interface and plain language.

**Rule we followed:** we took ideas only. No code was copied from any project
below, so Dilreet's own license is unchanged (see [CREDITS.md](CREDITS.md)).

| Idea in 0.4 | Where it came from |
|---|---|
| Dark left sidebar, "Projects" list, per-project sub-menu (Overview / Deploys / Files / Domains / Logs / Settings) | [Netlify app](https://app.netlify.com) and its [docs on the site overview](https://docs.netlify.com/) |
| Dashboard with counters, "Recent deploys", "Getting started" checklist that can be hidden | Netlify team overview; [Vercel dashboard](https://vercel.com/docs/dashboard-features); [Cloudflare Pages](https://developers.cloudflare.com/pages/) onboarding |
| Start from a template, with live thumbnails | [Netlify templates](https://www.netlify.com/integrations/templates/), [Vercel templates](https://vercel.com/templates) |
| Groups (one per client/business) shown in the sidebar | Coolify "Projects → Environments" ([coolify.io](https://coolify.io), [GitHub](https://github.com/coollabsio/coolify)); [Dokploy](https://github.com/Dokploy/dokploy) projects; Netlify teams |
| Tags with colours, pins/favourites, search, filters, sort, grid/list toggle | [Portainer](https://github.com/portainer/portainer) tables; [Uptime Kuma](https://github.com/louislam/uptime-kuma) tags; GitHub/Vercel "starred" projects |
| Select many → bulk actions (redeploy, move, tag, archive) | Portainer bulk container actions; Gmail-style selection bar |
| Archive instead of delete | GitHub repository archiving; Netlify "hide" of old sites |
| Health check every few minutes + padlock (SSL) expiry warning + notification | [Uptime Kuma](https://github.com/louislam/uptime-kuma) (monitors, certificate expiry notification) |
| "Needs your attention" list with a **Fix it** button | Vercel/Netlify failed-deploy banners; [CapRover](https://github.com/caprover/caprover) one-click fixes; Coolify notifications |
| Visitors & logs from the web server's access log (no tracking script) | [GoAccess](https://github.com/allinurl/goaccess) (log-based analytics); Netlify Analytics (server-side, no cookies) |
| Scheduled local backups with "keep the last N" | Coolify scheduled backups; [Dokku](https://github.com/dokku/dokku) backup plugins |
| Duplicate a project | Netlify "duplicate site" / Vercel "clone" |
| Plain words + tooltips for jargon (DNS, SSL, VPS) | [GOV.UK content design guide](https://www.gov.uk/guidance/content-design/writing-for-gov-uk), [plainlanguage.gov](https://www.plainlanguage.gov/guidelines/) |
| Light/dark theme following the system | Netlify, GitHub, Coolify |

## What we did not copy
* Netlify Forms, functions and env vars: not useful for static nginx sites.
* Real-time multi-user features (teams, roles): Dilreet is single-user on one PC.
* Visitor counts are approximate: the proxy hides some real IPs.

## Safety rule kept from 0.3
Anything that changes the server or DNS (redeploy, restart, domain, DNS records,
bulk redeploy, Fix-it actions that touch the server) always asks for a confirm click.
Backups, health checks and log reading are read-only.
