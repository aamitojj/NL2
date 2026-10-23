package main

// Import from Netlify (migration). Uses the documented Netlify API
// (docs.netlify.com "Get started with the Netlify API" and open-api/swagger.yml):
//   GET /api/v1/sites                        list sites (published_deploy, custom_domain, build_settings)
//   GET /api/v1/deploys/{deploy_id}/files    list files of a deploy (paginated, 100 per page)
//   GET /api/v1/deploys/{deploy_id}/files/{path} with
//       Content-Type: application/vnd.bitballoon.v1.raw -> the raw file bytes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var netlifyAPI = "https://api.netlify.com/api/v1"

type NetlifySite struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	URL             string   `json:"url"`
	SSLURL          string   `json:"ssl_url"`
	CustomDomain    string   `json:"custom_domain"`
	DomainAliases   []string `json:"domain_aliases"`
	UpdatedAt       string   `json:"updated_at"`
	PublishedDeploy *struct {
		ID string `json:"id"`
	} `json:"published_deploy"`
	BuildSettings *struct {
		RepoURL    string `json:"repo_url"`
		RepoBranch string `json:"repo_branch"`
		Dir        string `json:"dir"`
		Cmd        string `json:"cmd"`
		Base       string `json:"base"`
	} `json:"build_settings"`
}

type netlifyFile struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	SHA  string `json:"sha"`
}

func netlifyReq(ctx context.Context, token, p string, raw bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", netlifyAPI+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Dilreet/"+Version)
	if raw {
		req.Header.Set("Content-Type", "application/vnd.bitballoon.v1.raw")
	}
	resp, err := gitHTTP.Do(req)
	if err != nil {
		return nil, &GitError{Msg: "Couldn't reach Netlify. Check your internet connection."}
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		switch resp.StatusCode {
		case 401, 403:
			return nil, &GitError{Msg: "Netlify refused the access token. Create a new one and reconnect.", NeedsToken: true, Status: resp.StatusCode}
		case 404:
			return nil, &GitError{Msg: "Not found on Netlify.", Status: 404}
		case 429:
			return nil, &GitError{Msg: "Netlify is limiting requests; wait a minute and try again.", Status: 429}
		}
		return nil, &GitError{Msg: fmt.Sprintf("Netlify answered HTTP %d.", resp.StatusCode), Status: resp.StatusCode}
	}
	return resp, nil
}

func netlifyGet(ctx context.Context, token, p string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, gitAPITimeout)
	defer cancel()
	resp, err := netlifyReq(ctx, token, p, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

func netlifySites(ctx context.Context, token string) ([]NetlifySite, error) {
	var all []NetlifySite
	for page := 1; page <= 10; page++ {
		var v []NetlifySite
		if err := netlifyGet(ctx, token, fmt.Sprintf("/sites?filter=all&per_page=100&page=%d", page), &v); err != nil {
			return nil, err
		}
		all = append(all, v...)
		if len(v) < 100 {
			break
		}
	}
	return all, nil
}

func netlifyToken() (string, error) {
	c := snapshotConfig()
	a := c.GitAccounts["netlify"]
	if a == nil || a.Token == "" {
		return "", &GitError{Msg: "Connect your Netlify account first (paste a personal access token).", NeedsToken: true}
	}
	return secret(a.Token), nil
}

// importNetlifySite downloads the files of the site's current published
// deploy (exactly what visitors see, including _redirects/_headers files).
func importNetlifySite(ctx context.Context, j *Job, token string, site NetlifySite) (*FetchResult, error) {
	if site.PublishedDeploy == nil || site.PublishedDeploy.ID == "" {
		return nil, errors.New("this Netlify site has no published deploy to import")
	}
	dep := site.PublishedDeploy.ID
	var files []netlifyFile
	for page := 1; ; page++ {
		var v []netlifyFile
		if err := netlifyGet(ctx, token, fmt.Sprintf("/deploys/%s/files?page=%d&per_page=100", url.PathEscape(dep), page), &v); err != nil {
			return nil, err
		}
		files = append(files, v...)
		if len(v) < 100 || len(files) > maxSiteFiles {
			break
		}
	}
	if len(files) == 0 {
		return nil, errors.New("the published deploy has no files")
	}
	if len(files) > maxSiteFiles {
		return nil, fmt.Errorf("too many files (%d, max %d)", len(files), maxSiteFiles)
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	if total > maxSiteBytes {
		return nil, fmt.Errorf("site is too large (%d MB, max %d MB)", total>>20, maxSiteBytes>>20)
	}
	dir, err := os.MkdirTemp("", "dilreet-netlify-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if j != nil {
		j.Logf("Downloading %d files (%s) from Netlify deploy %s ...", len(files), humanBytes(total), dep)
	}
	var (
		mu    sync.Mutex
		done  int
		first error
		wg    sync.WaitGroup
		sem   = make(chan struct{}, 6)
	)
	for _, f := range files {
		rel, err := cleanRel(strings.TrimPrefix(f.Path, "/"))
		if err != nil {
			return nil, err
		}
		if rel == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(f netlifyFile, rel string) {
			defer wg.Done()
			defer func() { <-sem }()
			err := retry(ctx, 3, time.Second, func() error {
				return netlifyDownload(ctx, token, dep, rel, filepath.Join(dir, filepath.FromSlash(rel)), f.Size)
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil && first == nil {
				first = fmt.Errorf("%s: %w", rel, err)
			}
			done++
			if j != nil && done%20 == 0 {
				j.SetProgress(done*100/len(files), fmt.Sprintf("Downloaded %d of %d files", done, len(files)))
			}
		}(f, rel)
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	entries, err := dirEntries(dir)
	if err != nil {
		return nil, err
	}
	ws, err := newWorkspace(entries, "Netlify site "+site.Name)
	if err != nil {
		return nil, err
	}
	res := &FetchResult{WS: ws, Ref: &RepoRef{Kind: "netlify", Site: site.Name, Display: "Netlify site " + site.Name}, Extras: map[string]any{}}
	if site.CustomDomain != "" {
		res.Extras["custom_domain"] = site.CustomDomain
	}
	if site.BuildSettings != nil && site.BuildSettings.RepoURL != "" {
		res.Extras["repo_url"] = site.BuildSettings.RepoURL
		res.Extras["repo_branch"] = site.BuildSettings.RepoBranch
	}
	carried := []string{}
	for _, n := range []string{"_redirects", "_headers", "netlify.toml"} {
		if ws.Files.IsFile(n) || isFileOS(filepath.Join(ws.Root, n)) {
			carried = append(carried, n)
		}
	}
	res.Extras["carried_rules"] = carried
	if j != nil {
		rules := "none"
		if len(carried) > 0 {
			rules = strings.Join(carried, ", ")
		}
		j.Logf("Imported %d files. Rules carried over: %s.", ws.FileCount, rules)
		if site.CustomDomain != "" {
			j.Logf("Custom domain on Netlify: %s (the domain wizard can move it after the first deploy).", site.CustomDomain)
		}
	}
	return res, nil
}

func netlifyDownload(ctx context.Context, token, dep, rel, dst string, size int64) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	resp, err := netlifyReq(ctx, token, "/deploys/"+url.PathEscape(dep)+"/files/"+escapeRefPath(rel), true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	limit := size + 1<<20
	if size <= 0 {
		limit = maxSiteBytes
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	f.Close()
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("file larger than announced")
	}
	return nil
}
