package main

import (
	"context"
	"errors"
	"fmt"
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
</head><body><div><h1>This site is ready.</h1><p>Upload your files with HostPanel to replace this page.</p></div></body></html>`

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

	j.Logf("1/5 Preparing folder %s on the server...", siteDir(slug))
	script := fmt.Sprintf(`set -e
BASE=%[1]s
mkdir -p "$BASE/releases/initial"
if [ ! -e "$BASE/html" ]; then
  cat > "$BASE/releases/initial/index.html" <<'HPEOF'
%[2]s
HPEOF
  ln -sfn releases/initial "$BASE/html"
fi
chmod -R a+rX "$BASE"
echo ok`, shellQuote(siteDir(slug)), placeholderHTML)
	if out, err := runRemoteOutput(ctx, script); err != nil {
		return fmt.Errorf("prepare folder: %v: %s", err, out)
	}

	j.Logf("2/5 Creating Coolify project %q...", name)
	projUUID, err := api.CreateProject(name, "HostPanel site: "+slug)
	if err != nil {
		return err
	}
	site := &Site{Slug: slug, Name: name, ProjectUUID: projUUID, CreatedAt: time.Now().Format(time.RFC3339)}
	save := func() {
		_ = withConfig(func(c *Config) error {
			if s := findSite(c, slug); s != nil {
				*s = *site
			} else {
				c.Sites = append(c.Sites, site)
			}
			return nil
		})
	}
	save()

	j.Logf("3/5 Creating nginx application (Docker image nginx:stable-alpine)...")
	appUUID, err := api.CreateNginxApp(projUUID, serverUUID, slug)
	if err != nil {
		return fmt.Errorf("%w (the project was created; delete the site to clean up)", err)
	}
	site.AppUUID = appUUID
	save()

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

	j.Logf("5/5 Starting the site...")
	if err := api.Deploy(appUUID); err != nil {
		return err
	}
	if app, err := api.Application(appUUID); err == nil && app.FQDN != "" {
		j.Logf("Temporary address: %s (works after the first deployment finishes, ~30-60 s)", app.FQDN)
	}
	j.Logf("Site %q created. Next: upload your files, then connect a domain.", name)
	return nil
}

func deploySiteJob(j *Job, slug, tarball string, files int, size int64) error {
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
	remoteTmp := fmt.Sprintf("/tmp/hostpanel-%s-%s.tar.gz", slug, randHex(4))
	if user == "root" {
		remoteTmp = fmt.Sprintf("/root/.hostpanel-%s-%s.tar.gz", slug, randHex(4))
	}
	j.Logf("Uploading %d files (%.1f MB) over SFTP...", files, float64(size)/1e6)
	if err := uploadFile(tarball, remoteTmp, func(done, total int64) {
		j.Logf("  uploaded %.1f / %.1f MB (compressed)", float64(done)/1e6, float64(total)/1e6)
	}); err != nil {
		return err
	}
	j.Logf("Extracting into release %s and switching the site over...", release)
	script := fmt.Sprintf(`set -e
BASE=%[1]s
TMP=%[2]s
REL="$BASE/releases/%[3]s"
trap 'rm -f "$TMP"' EXIT
mkdir -p "$BASE/releases"
mkdir "$REL" || { echo "Another deployment of this site started in the same second; wait a moment and retry." >&2; exit 1; }
tar -xzf "$TMP" -C "$REL" --no-same-owner --no-same-permissions
chmod -R a+rX "$REL"
ln -sfn "releases/%[3]s" "$BASE/html.new"
mv -Tf "$BASE/html.new" "$BASE/html" 2>/dev/null || { rm -f "$BASE/html.new"; rm -f "$BASE/html"; ln -s "releases/%[3]s" "$BASE/html"; }
cd "$BASE/releases"
ls -1t | grep -v '^initial$' | tail -n +6 | xargs -r rm -rf --
echo "live release: $(readlink "$BASE/html")"`, shellQuote(siteDir(slug)), shellQuote(remoteTmp), release)
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
	script := fmt.Sprintf(`set -e
BASE=%[1]s
test -d "$BASE/releases/%[2]s"
ln -sfn "releases/%[2]s" "$BASE/html.new"
mv -Tf "$BASE/html.new" "$BASE/html" 2>/dev/null || { rm -f "$BASE/html.new" "$BASE/html"; ln -s "releases/%[2]s" "$BASE/html"; }
echo ok`, shellQuote(siteDir(slug)), release)
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
	j.Logf("Site %q removed from HostPanel.", slug)
	return nil
}
