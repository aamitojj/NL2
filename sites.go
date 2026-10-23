package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Remote layout (on the VPS):
//
//	/data/hostpanel/sites/<slug>/releases/<release>/   uploaded files
//	/data/hostpanel/sites/<slug>/html -> releases/<release>   (atomic symlink)
//
// The whole /data/hostpanel/sites/<slug> directory is bind-mounted into the
// nginx container at /usr/share/nginx, so nginx serves /usr/share/nginx/html.
const remoteSitesRoot = "/data/hostpanel/sites"

var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

func siteDir(slug string) string { return remoteSitesRoot + "/" + slug }

func validSlug(s string) bool { return slugRe.MatchString(s) }

func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	return out
}

const placeholderHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Coming soon</title>
<style>body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#f4f6fb;color:#334}</style>
</head><body><div><h1>This site is ready.</h1><p>Upload your files with Dilreet to replace this page.</p></div></body></html>`

func createSiteJob(j *Job, name, slug string) error {
	ctx := context.Background()
	c := snapshotConfig()
	if findSite(&c, slug) != nil {
		return fmt.Errorf("a site with the id %q already exists", slug)
	}
	api, err := newCoolify()
	if err != nil {
		return err
	}
	if !sshS.connected() {
		return errNotConnected
	}
	serverUUID := c.ServerUUID
	if serverUUID == "" {
		srv, err := api.Servers()
		if err != nil {
			return err
		}
		if len(srv) == 0 {
			return errors.New("Coolify has no servers yet")
		}
		serverUUID = srv[0].UUID
		_ = withConfig(func(c *Config) error { c.ServerUUID = serverUUID; return nil })
		j.Logf("Using Coolify server %q (%s)", srv[0].Name, serverUUID)
	}

	j.SetProgress(5, "Preparing your site on the server")
	j.Logf("1/5 Preparing folder %s on the server...", siteDir(slug))
	script := fmt.Sprintf(`set -e
BASE=%[1]s
mkdir -p "$BASE/releases/initial" "$BASE/nginx" "$BASE/nginx-releases"
cat > "$BASE/nginx-releases/_default.conf" <<'%[4]s'
%[3]s
%[4]s
[ -f "$BASE/nginx/default.conf" ] || cp "$BASE/nginx-releases/_default.conf" "$BASE/nginx/default.conf"
if [ ! -e "$BASE/html" ]; then
  cat > "$BASE/releases/initial/index.html" <<'HPEOF'
%[2]s
HPEOF
  ln -sfn releases/initial "$BASE/html"
fi
chmod -R a+rX "$BASE"
echo ok`, shellQuote(siteDir(slug)), placeholderHTML, defaultNginxConf(), "HPCONF_"+randHex(8))
	if out, err := runRemoteOutput(ctx, script); err != nil {
		return fmt.Errorf("prepare folder: %v: %s", err, out)
	}

	j.SetProgress(25, "Creating the project")
	j.Logf("2/5 Creating Coolify project %q...", name)
	projUUID, err := api.CreateProject(name, "Dilreet site: "+slug)
	if err != nil {
		return err
	}
	site := &Site{Slug: slug, Name: name, ProjectUUID: projUUID, CreatedAt: time.Now().Format(time.RFC3339)}
	save := func() {
		_ = withConfig(func(c *Config) error {
			if s := findSite(c, slug); s != nil {
				f := s.Folder
				*s = *site
				if s.Folder == "" {
					s.Folder = f
				}
			} else {
				c.Sites = append(c.Sites, site)
			}
			assignFolders(c)
			site.Folder = findSite(c, slug).Folder
			return nil
		})
	}
	save()

	j.SetProgress(45, "Creating the web server")
	j.Logf("3/5 Creating nginx application (Docker image nginx:stable-alpine)...")
	appUUID, err := api.CreateNginxApp(projUUID, serverUUID, slug)
	if err != nil {
		return fmt.Errorf("%w (the project was created; delete the site to clean up)", err)
	}
	site.AppUUID = appUUID
	save()

	j.SetProgress(65, "Connecting the site folder")
	j.Logf("4/5 Mounting %s into the container at /usr/share/nginx ...", siteDir(slug))
	if err := api.AddDirectoryMount(appUUID, siteDir(slug), "/usr/share/nginx"); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 403 {
			return fmt.Errorf("%w. Create the API token with the 'deploy' permission (root/write+deploy), then save it again in Setup", err)
		}
		if errors.As(err, &ae) && ae.Status == 404 {
			return fmt.Errorf("%w. Your Coolify version is too old for the storages API: update Coolify in Settings and retry", err)
		}
		return err
	}

	j.Logf("   Mounting %s/nginx at /etc/nginx/conf.d (redirect and header rules)...", siteDir(slug))
	if err := api.AddDirectoryMount(appUUID, siteDir(slug)+"/nginx", "/etc/nginx/conf.d"); err != nil {
		j.Logf("   WARNING: %v. The site works, but _redirects/_headers rules will only apply after a later deployment adds this mount.", err)
	} else {
		site.ConfMount = true
		save()
	}

	// Free instant address: <slug>.<server-ip>.sslip.io resolves to the
	// server without any DNS setup, and Coolify/Traefik requests a Let's
	// Encrypt certificate for it (sslip.io has a raised LE rate limit).
	if ip := snapshotConfig().ServerIP; ip != "" {
		host := sslipHost(slug, ip)
		if err := retry(context.Background(), 3, 2*time.Second, func() error { return api.SetDomains(appUUID, "https://"+host, true) }); err != nil {
			j.Logf("   Note: could not set the instant address %s (%v); Coolify's generated address is used instead.", host, err)
		} else {
			site.InstantURL = "https://" + host
			save()
			j.Logf("   Instant address: %s (no DNS needed)", site.InstantURL)
		}
	}

	j.SetProgress(85, "Starting the site")
	j.Logf("5/5 Starting the site...")
	if err := api.Deploy(appUUID); err != nil {
		return err
	}
	if site.InstantURL != "" {
		j.Logf("Live at %s in about a minute (the HTTPS certificate can take a little longer).", site.InstantURL)
	} else if app, err := api.Application(appUUID); err == nil && app.FQDN != "" {
		j.Logf("Temporary address: %s (works after the first deployment finishes, ~30-60 s)", app.FQDN)
	}
	j.Logf("Site %q created. Next: upload your files, then connect a domain.", name)
	return nil
}

// deployWorkspace publishes exactly the tested files of ws plus the nginx
// config generated from its rules.
func deployWorkspace(j *Job, slug string, ws *Workspace) error {
	if err := ws.verifyUnchanged(); err != nil {
		return err
	}
	entries, err := dirEntries(ws.Publish)
	if err != nil {
		return err
	}
	tarball, n, size, err := buildSiteTarball(entries)
	if err != nil {
		return err
	}
	defer os.Remove(tarball)
	label := ws.PublishRel
	if label == "" {
		label = "top level"
	}
	j.Logf("Publishing folder: %s (%s)", label, ws.PublishNote)
	nr, nh := 0, 0
	if ws.Rules != nil {
		nr, nh = len(ws.Rules.Redirects), len(ws.Rules.Headers)
	}
	j.Logf("Rules: %d redirect/rewrite and %d header rules -> production nginx config. Build fingerprint %s", nr, nh, ws.Hash[:12])
	if ws.restoreOf != "" {
		err := deploySiteJob(j, slug, tarball, n, size, generateNginxConf(ws.Rules, ws.Files))
		if err == nil {
			c := snapshotConfig()
			if s := findSite(&c, slug); s != nil {
				markRestored(slug, ws.restoreOf, s.Release)
			}
		}
		return err
	}
	rec := archiveDeploy(j, slug, ws)
	err = deploySiteJob(j, slug, tarball, n, size, generateNginxConf(ws.Rules, ws.Files))
	if rec != nil {
		finishDeploy(slug, rec.ID, err)
	}
	return err
}

func defaultNginxConf() string { return generateNginxConf(nil, &siteFiles{}) }

// confShellFuncs are shell helpers that install a generated nginx config,
// test it inside the running container and roll back if nginx rejects it.
const confShellFuncs = `
HP_RELOAD=""
hp_container() {
  command -v docker >/dev/null 2>&1 || return 0
  docker ps --filter "name=$APP" --filter status=running --format '{{.Names}}' 2>/dev/null | grep "^$APP" | head -n1
}
hp_conf_install() {
  CD="$BASE/nginx"
  mkdir -p "$CD"
  if [ -f "$CD/default.conf" ]; then cp -f "$CD/default.conf" "$CD/.prev.conf.bak"; else rm -f "$CD/.prev.conf.bak"; fi
  cp -f "$1" "$CD/.new.tmp" && chmod 644 "$CD/.new.tmp" && mv -f "$CD/.new.tmp" "$CD/default.conf"
  C=$(hp_container)
  if [ -n "$C" ] && docker inspect -f '{{range .Mounts}}{{println .Destination}}{{end}}' "$C" 2>/dev/null | grep -qx /etc/nginx/conf.d; then
    if ! OUT=$(docker exec "$C" nginx -t 2>&1); then
      echo "nginx rejected the generated config:" >&2; echo "$OUT" >&2
      if [ -f "$CD/.prev.conf.bak" ]; then mv -f "$CD/.prev.conf.bak" "$CD/default.conf"; fi
      return 1
    fi
    HP_RELOAD="$C"
    echo "nginx config test passed in container $C"
  elif command -v docker >/dev/null 2>&1 && docker image inspect nginx:stable-alpine >/dev/null 2>&1; then
    if ! OUT=$(docker run --rm --network none -v "$CD":/etc/nginx/conf.d:ro nginx:stable-alpine nginx -t 2>&1); then
      echo "nginx rejected the generated config:" >&2; echo "$OUT" >&2
      if [ -f "$CD/.prev.conf.bak" ]; then mv -f "$CD/.prev.conf.bak" "$CD/default.conf"; fi
      return 1
    fi
    echo "nginx config test passed (container not running or rules mount not active yet)"
  else
    echo "note: could not test the nginx config (no running container / docker)"
  fi
  rm -f "$CD/.prev.conf.bak"
  return 0
}
hp_reload() {
  if [ -n "$HP_RELOAD" ]; then
    docker exec "$HP_RELOAD" nginx -s reload >/dev/null 2>&1 && echo "nginx reloaded with the new rules" || echo "WARNING: nginx reload failed; restart the site to apply the rules"
  fi
}
`

func deploySiteJob(j *Job, slug, tarball string, files int, size int64, conf string) error {
	ctx := context.Background()
	c := snapshotConfig()
	site := findSite(&c, slug)
	if site == nil {
		return errors.New("unknown site")
	}
	_, user, _, err := sshS.get()
	if err != nil {
		return err
	}
	release := time.Now().UTC().Format("20060102-150405")
	remoteTmp := fmt.Sprintf("/tmp/dilreet-%s-%s.tar.gz", slug, randHex(4))
	if user == "root" {
		remoteTmp = fmt.Sprintf("/root/.dilreet-%s-%s.tar.gz", slug, randHex(4))
	}
	j.Logf("Uploading %d files (%.1f MB) over SFTP...", files, float64(size)/1e6)
	j.SetProgress(10, "Uploading files")
	if err := uploadFile(tarball, remoteTmp, func(done, total int64) {
		j.Logf("  uploaded %.1f / %.1f MB (compressed)", float64(done)/1e6, float64(total)/1e6)
		if total > 0 {
			j.SetProgress(10+int(done*70/total), "Uploading files")
		}
	}); err != nil {
		return err
	}
	j.Logf("Extracting into release %s and switching the site over...", release)
	j.SetProgress(85, "Publishing")
	delim := "HPCONF_" + randHex(8)
	script := fmt.Sprintf(`set -e
BASE=%[1]s
TMP=%[2]s
APP=%[4]s
REL="$BASE/releases/%[3]s"
trap 'rm -f "$TMP"' EXIT
%[7]s
mkdir -p "$BASE/releases" "$BASE/nginx" "$BASE/nginx-releases"
mkdir "$REL" || { echo "Another deployment of this site started in the same second; wait a moment and retry." >&2; exit 1; }
tar -xzf "$TMP" -C "$REL" --no-same-owner --no-same-permissions
chmod -R a+rX "$REL"
cat > "$BASE/nginx-releases/%[3]s.conf" <<'%[6]s'
%[5]s
%[6]s
cat > "$BASE/nginx-releases/_default.conf" <<'%[6]s'
%[8]s
%[6]s
chmod a+r "$BASE/nginx-releases/"*.conf
hp_conf_install "$BASE/nginx-releases/%[3]s.conf" || { rm -rf "$REL" "$BASE/nginx-releases/%[3]s.conf"; echo "Deployment stopped before switching: the live site was not changed. Fix the rule nginx complains about (App Testing shows rule problems)." >&2; exit 1; }
ln -sfn "releases/%[3]s" "$BASE/html.new"
mv -Tf "$BASE/html.new" "$BASE/html" 2>/dev/null || { rm -f "$BASE/html.new"; rm -f "$BASE/html"; ln -s "releases/%[3]s" "$BASE/html"; }
hp_reload
cd "$BASE/releases"
ls -1t | grep -v '^initial$' | tail -n +6 | xargs -r rm -rf --
for f in "$BASE"/nginx-releases/*.conf; do r=$(basename "$f" .conf); [ "$r" = _default ] || [ -d "$BASE/releases/$r" ] || rm -f "$f"; done
echo "live release: $(readlink "$BASE/html")"`, shellQuote(siteDir(slug)), shellQuote(remoteTmp), release, shellQuote(site.AppUUID), strings.TrimRight(conf, "\n"), delim, confShellFuncs, strings.TrimRight(defaultNginxConf(), "\n"))
	if err := runRemote(ctx, script, j); err != nil {
		return err
	}
	_ = withConfig(func(c *Config) error {
		if s := findSite(c, slug); s != nil {
			s.LastDeploy = time.Now().Format(time.RFC3339)
			s.Release = release
		}
		return nil
	})
	if !site.ConfMount && site.AppUUID != "" {
		j.Logf("This site was created with Dilreet 0.1: adding the rules mount (%s/nginx -> /etc/nginx/conf.d) so _redirects/_headers work...", siteDir(slug))
		api, err := newCoolify()
		if err == nil {
			err = api.AddDirectoryMount(site.AppUUID, siteDir(slug)+"/nginx", "/etc/nginx/conf.d")
		}
		if err != nil {
			j.Logf("WARNING: could not add the mount (%v). Files are live, but redirect/header rules are not active yet; deploy again later to retry.", err)
		} else {
			_ = withConfig(func(c *Config) error {
				if s := findSite(c, slug); s != nil {
					s.ConfMount = true
				}
				return nil
			})
			if err := api.Deploy(site.AppUUID); err != nil {
				j.Logf("WARNING: restart failed (%v); restart the site from the Sites tab to apply the rules.", err)
			} else {
				j.Logf("Restarting the container once to activate the mount (~30-60 s).")
				j.Logf("Deployed. The new files are live now; the rules apply once the restart finishes.")
				return nil
			}
		}
	}
	j.Logf("Deployed. Files are served immediately (no container restart needed).")
	return nil
}

func listReleases(slug string) ([]string, string, error) {
	out, err := runRemoteOutput(context.Background(), fmt.Sprintf(`cd %s/releases && ls -1t; echo "@@CURRENT=$(readlink ../html)"`, shellQuote(siteDir(slug))))
	if err != nil {
		return nil, "", fmt.Errorf("%v: %s", err, out)
	}
	var rel []string
	cur := ""
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "@@CURRENT=") {
			cur = strings.TrimPrefix(strings.TrimPrefix(l, "@@CURRENT="), "releases/")
		} else if l != "" {
			rel = append(rel, l)
		}
	}
	return rel, cur, nil
}

var releaseRe = regexp.MustCompile(`^([0-9]{8}-[0-9]{6}|initial)$`)

func activateRelease(slug, release string) error {
	if !releaseRe.MatchString(release) {
		return errors.New("invalid release name")
	}
	c := snapshotConfig()
	site := findSite(&c, slug)
	if site == nil {
		return errors.New("unknown site")
	}
	delim := "HPCONF_" + randHex(8)
	script := fmt.Sprintf(`set -e
BASE=%[1]s
APP=%[3]s
%[4]s
test -d "$BASE/releases/%[2]s"
if [ -d "$BASE/nginx" ]; then
  mkdir -p "$BASE/nginx-releases"
  cat > "$BASE/nginx-releases/_default.conf" <<'%[6]s'
%[5]s
%[6]s
  CONF="$BASE/nginx-releases/%[2]s.conf"
  [ -f "$CONF" ] || CONF="$BASE/nginx-releases/_default.conf"
  hp_conf_install "$CONF" >/dev/null || { echo "nginx rejected the rules of release %[2]s; nothing was changed" >&2; exit 1; }
fi
ln -sfn "releases/%[2]s" "$BASE/html.new"
mv -Tf "$BASE/html.new" "$BASE/html" 2>/dev/null || { rm -f "$BASE/html.new" "$BASE/html"; ln -s "releases/%[2]s" "$BASE/html"; }
hp_reload >/dev/null
echo ok`, shellQuote(siteDir(slug)), release, shellQuote(site.AppUUID), confShellFuncs, strings.TrimRight(defaultNginxConf(), "\n"), delim)
	out, err := runRemoteOutput(context.Background(), script)
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return withConfig(func(c *Config) error {
		if s := findSite(c, slug); s != nil {
			s.Release = release
		}
		return nil
	})
}

func deleteSiteJob(j *Job, slug string, keepFiles bool) error {
	c := snapshotConfig()
	site := findSite(&c, slug)
	if site == nil {
		return errors.New("unknown site")
	}
	api, err := newCoolify()
	if err != nil {
		return err
	}
	if site.AppUUID != "" {
		j.Logf("Deleting Coolify application %s...", site.AppUUID)
		if err := api.DeleteApp(site.AppUUID); err != nil {
			var ae *APIError
			if !(errors.As(err, &ae) && ae.Status == 404) {
				return err
			}
			j.Logf("  (already gone)")
		}
	}
	if site.ProjectUUID != "" {
		j.Logf("Deleting Coolify project %s (waits until the app removal has finished)...", site.ProjectUUID)
		var lastErr error
		for i := 0; i < 20; i++ {
			lastErr = api.DeleteProject(site.ProjectUUID)
			var ae *APIError
			if lastErr == nil || (errors.As(lastErr, &ae) && ae.Status == 404) {
				lastErr = nil
				break
			}
			time.Sleep(3 * time.Second)
		}
		if lastErr != nil {
			j.Logf("  WARNING: could not delete the project yet (%v). Delete it later in the Coolify dashboard.", lastErr)
		}
	}
	if !keepFiles {
		if sshS.connected() {
			j.Logf("Removing files %s on the server...", siteDir(slug))
			if out, err := runRemoteOutput(context.Background(), "rm -rf -- "+shellQuote(siteDir(slug))); err != nil {
				j.Logf("  WARNING: %v %s", err, out)
			}
		} else {
			j.Logf("Not connected over SSH: files in %s were left on the server.", siteDir(slug))
		}
	}
	_ = withConfig(func(c *Config) error {
		out := c.Sites[:0]
		for _, s := range c.Sites {
			if s.Slug != slug {
				out = append(out, s)
			}
		}
		c.Sites = out
		return nil
	})
	j.Logf("Site %q removed from Dilreet.", slug)
	return nil
}
