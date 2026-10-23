package main

// Import from Git or link: paste any repository or archive URL, Dilreet
// downloads it WITHOUT needing git (each host's archive download), unpacks it
// with the same zip-slip-safe code as uploads, and continues with the normal
// flow (framework detection, optional build, App Testing, deploy).
//
// Endpoints used (verified against the hosts' docs and live public requests):
//   GitHub    codeload.github.com/{o}/{r}/zip/{ref} (public),
//             api.github.com/repos/{o}/{r}/zipball/{ref} (token; 302 to codeload),
//             /repos/{o}/{r}, /branches, /commits/{ref}, /user, /user/repos
//   GitLab    {host}/api/v4/projects/{id}/repository/archive.zip?sha=,
//             /repository/branches, /repository/commits/{ref}, /user, /projects?membership=true
//   Bitbucket bitbucket.org/{ws}/{r}/get/{ref}.zip,
//             api.bitbucket.org/2.0/repositories/{ws}/{r}[/refs/branches|/commit/{ref}]
//   Gitea/Forgejo/Codeberg {host}/{o}/{r}/archive/{ref}.zip, /api/v1/repos/{o}/{r}[/branches[/{b}]]
//   Azure DevOps dev.azure.com/{org}/{proj}/_apis/git/repositories/{r}/items?$format=zip,
//             /refs?filter=heads/

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// RepoRef is a parsed "paste any URL" input.
type RepoRef struct {
	Kind    string `json:"kind"` // github gitlab bitbucket gitea azure git archive netlify
	Host    string `json:"host"` // e.g. github.com, gitlab.example.com
	Owner   string `json:"owner,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Org     string `json:"org,omitempty"`     // Azure DevOps organisation
	Project string `json:"project,omitempty"` // Azure DevOps project
	Branch  string `json:"branch,omitempty"`
	Subdir  string `json:"subdir,omitempty"`
	// TreePath is the "<branch>/<subdir>" tail of a /tree/ URL; branch names
	// may contain slashes, so it is split once the branch list is known.
	TreePath string `json:"-"`
	URL      string `json:"url"`
	Display  string `json:"display"`
	Site     string `json:"netlify_site,omitempty"`
}

func (r *RepoRef) AccountKey() string {
	switch r.Kind {
	case "azure":
		return "dev.azure.com/" + strings.ToLower(r.Org)
	case "netlify":
		return "netlify"
	}
	return strings.ToLower(r.Host)
}

var (
	// Overridable in tests (httptest TLS servers).
	githubAPI      = "https://api.github.com"
	githubCodeload = "https://codeload.github.com"
	bitbucketAPI   = "https://api.bitbucket.org/2.0"
	bitbucketWeb   = "https://bitbucket.org"
	azureBase      = "https://dev.azure.com"
	gitHTTP        = &http.Client{Timeout: 0, Transport: aiHTTP.Transport, CheckRedirect: httpsOnlyRedirect}
	// hostProbe detects self-hosted GitLab / Gitea instances.
	hostProbe = probeGitHost
)

const (
	maxRepoDownload = 500 << 20 // compressed archive size limit
	gitAPITimeout   = 20 * time.Second
)

func httpsOnlyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" {
		return errors.New("refusing a redirect to a non-HTTPS address")
	}
	return nil
}

var (
	scpLikeRe = regexp.MustCompile(`^(?:[\w.-]+@)?([\w.-]+\.[a-z]{2,}):(.+)$`)
	archiveRe = regexp.MustCompile(`(?i)\.(zip|tar\.gz|tgz)$`)
)

// parseRepoURL detects the host type and repository from anything a user
// might paste: https URLs, URLs without a scheme, SSH "git@host:o/r.git",
// /tree/<branch>/<dir> links, archive links and Netlify site addresses.
func parseRepoURL(raw string) (*RepoRef, error) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, `"'<>`)
	if s == "" {
		return nil, errors.New("paste a repository or download link")
	}
	if m := scpLikeRe.FindStringSubmatch(s); m != nil && !strings.Contains(s, "://") {
		s = "https://" + m[1] + "/" + strings.TrimPrefix(m[2], "/")
	}
	if strings.HasPrefix(s, "ssh://") {
		u, err := url.Parse(s)
		if err == nil {
			s = "https://" + u.Hostname() + u.Path
		}
	}
	if strings.HasPrefix(s, "git://") {
		s = "https://" + strings.TrimPrefix(s, "git://")
	}
	if strings.HasPrefix(s, "http://") {
		return nil, errors.New("only https:// links are supported (plain http could be tampered with on the way)")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("that doesn't look like a web address; paste something like https://github.com/you/your-site")
	}
	u.User = nil // never keep credentials pasted inside a URL
	host := strings.ToLower(u.Host)
	host = strings.TrimPrefix(host, "www.")
	p := strings.Trim(u.EscapedPath(), "/")
	p, _ = url.PathUnescape(p)
	segs := []string{}
	if p != "" {
		segs = strings.Split(p, "/")
	}
	ref := &RepoRef{Host: host, URL: u.String()}
	trimGit := func(x string) string { return strings.TrimSuffix(x, ".git") }

	switch {
	case host == "github.com":
		if len(segs) < 2 {
			return nil, errors.New("a GitHub link needs an owner and repository, like github.com/owner/repo")
		}
		ref.Kind, ref.Owner, ref.Repo = "github", segs[0], trimGit(segs[1])
		if len(segs) > 3 && (segs[2] == "tree" || segs[2] == "blob") {
			ref.TreePath = strings.Join(segs[3:], "/")
		}
		if len(segs) > 4 && segs[2] == "archive" { // github.com/o/r/archive/refs/heads/main.zip
			ref.Branch = archiveRe.ReplaceAllString(strings.Join(segs[3:], "/"), "")
			ref.Branch = strings.TrimPrefix(strings.TrimPrefix(ref.Branch, "refs/heads/"), "refs/tags/")
		}
	case host == "codeload.github.com" && len(segs) >= 4:
		ref.Kind, ref.Host, ref.Owner, ref.Repo = "github", "github.com", segs[0], segs[1]
		ref.Branch = strings.TrimPrefix(strings.Join(segs[3:], "/"), "refs/heads/")
	case host == "bitbucket.org":
		if len(segs) < 2 {
			return nil, errors.New("a Bitbucket link needs a workspace and repository, like bitbucket.org/workspace/repo")
		}
		ref.Kind, ref.Owner, ref.Repo = "bitbucket", segs[0], trimGit(segs[1])
		if len(segs) > 3 && segs[2] == "src" {
			ref.TreePath = strings.Join(segs[3:], "/")
		}
		if len(segs) == 4 && segs[2] == "get" {
			ref.Branch = archiveRe.ReplaceAllString(segs[3], "")
		}
	case host == "dev.azure.com" || strings.HasSuffix(host, ".visualstudio.com"):
		// dev.azure.com/{org}/{project}/_git/{repo} or {org}.visualstudio.com/{project}/_git/{repo}
		if host != "dev.azure.com" {
			segs = append([]string{strings.TrimSuffix(host, ".visualstudio.com")}, segs...)
			ref.Host = "dev.azure.com"
		}
		gi := indexOf(segs, "_git")
		if gi < 1 || gi+1 >= len(segs) {
			return nil, errors.New("an Azure DevOps link looks like dev.azure.com/org/project/_git/repo")
		}
		ref.Kind, ref.Org, ref.Repo = "azure", segs[0], segs[gi+1]
		ref.Project = ref.Repo
		if gi >= 2 {
			ref.Project = segs[1]
		}
		q := u.Query()
		if v := q.Get("version"); strings.HasPrefix(v, "GB") {
			ref.Branch = v[2:]
		}
		if v := strings.Trim(q.Get("path"), "/"); v != "" {
			ref.Subdir = v
		}
	case host == "app.netlify.com" && len(segs) >= 2 && (segs[0] == "sites" || segs[0] == "projects"):
		ref.Kind, ref.Site = "netlify", segs[1]
	case strings.HasSuffix(host, ".netlify.app") || strings.HasSuffix(host, ".netlify.com"):
		ref.Kind, ref.Site = "netlify", strings.SplitN(host, ".", 2)[0]
		if i := strings.Index(ref.Site, "--"); i > 0 { // deploy-preview--site.netlify.app
			ref.Site = ref.Site[i+2:]
		}
	case archiveRe.MatchString(u.Path) && !strings.Contains(p, "/-/archive/") && !strings.Contains(p, "/archive/"):
		ref.Kind = "archive"
	default:
		probed, probedOK := "", false
		probe := func() string { // network probe for self-hosted instances, at most once
			if !probedOK {
				probed, probedOK = hostProbe(host), true
			}
			return probed
		}
		// GitLab (gitlab.com or self-hosted): /group/sub/project[/-/tree/<ref>/<dir>]
		if gi := indexOf(segs, "-"); gi >= 2 || host == "gitlab.com" || strings.Contains(host, "gitlab") || (len(segs) >= 2 && !strings.HasSuffix(u.Path, ".git") && !archiveRe.MatchString(u.Path) && probe() == "gitlab") {
			ns := segs
			tail := []string{}
			if gi >= 0 {
				ns, tail = segs[:gi], segs[gi+1:]
			}
			if len(ns) >= 2 && (gi >= 0 || host == "gitlab.com" || strings.Contains(host, "gitlab") || probe() == "gitlab") {
				ref.Kind = "gitlab"
				ns[len(ns)-1] = trimGit(ns[len(ns)-1])
				ref.Owner, ref.Repo = strings.Join(ns[:len(ns)-1], "/"), ns[len(ns)-1]
				if len(tail) > 1 && (tail[0] == "tree" || tail[0] == "blob") {
					ref.TreePath = strings.Join(tail[1:], "/")
				}
				if len(tail) > 1 && tail[0] == "archive" {
					ref.Branch = tail[1]
				}
				break
			}
		}
		// Gitea / Forgejo / Codeberg: /owner/repo[/src/branch/<ref>/<dir>]
		if len(segs) >= 2 && (host == "codeberg.org" || host == "gitea.com" || strings.Contains(host, "gitea") || strings.Contains(host, "forgejo") || (len(segs) > 3 && segs[2] == "src" && (segs[3] == "branch" || segs[3] == "tag" || segs[3] == "commit")) || (!strings.HasSuffix(u.Path, ".git") && !archiveRe.MatchString(u.Path) && probe() == "gitea")) {
			ref.Kind, ref.Owner, ref.Repo = "gitea", segs[0], trimGit(segs[1])
			if len(segs) > 4 && segs[2] == "src" {
				ref.TreePath = strings.Join(segs[4:], "/")
			}
			if len(segs) == 4 && segs[2] == "archive" {
				ref.Branch = archiveRe.ReplaceAllString(segs[3], "")
			}
			break
		}
		if strings.HasSuffix(u.Path, ".git") {
			ref.Kind = "git"
			break
		}
		if archiveRe.MatchString(u.Path) {
			ref.Kind = "archive"
			break
		}
		return nil, fmt.Errorf("Dilreet doesn't recognise %s. Paste a GitHub, GitLab, Bitbucket, Gitea/Codeberg or Azure DevOps repository link, a link ending in .git, or a direct .zip/.tar.gz download link", host)
	}
	if ref.Subdir == "" && ref.TreePath != "" && ref.Branch == "" {
		// Best guess until branches are known: first segment is the branch.
		b, rest, _ := strings.Cut(ref.TreePath, "/")
		ref.Branch, ref.Subdir = b, rest
	}
	ref.Display = ref.display()
	return ref, nil
}

func indexOf(a []string, x string) int {
	for i, v := range a {
		if v == x {
			return i
		}
	}
	return -1
}

func (r *RepoRef) display() string {
	switch r.Kind {
	case "github", "bitbucket", "gitea", "gitlab":
		h := r.Host
		return h + "/" + r.Owner + "/" + r.Repo
	case "azure":
		return r.Org + "/" + r.Project + "/" + r.Repo
	case "netlify":
		return "Netlify site " + r.Site
	}
	return r.URL
}

// resolveTree splits a "/tree/<branch>/<dir>" tail now that the branch list
// is known (longest matching branch wins, so "feature/x/docs" works).
func (r *RepoRef) resolveTree(branches []string) {
	if r.TreePath == "" {
		return
	}
	best := ""
	for _, b := range branches {
		if (r.TreePath == b || strings.HasPrefix(r.TreePath, b+"/")) && len(b) > len(best) {
			best = b
		}
	}
	if best != "" {
		r.Branch = best
		r.Subdir = strings.TrimPrefix(strings.TrimPrefix(r.TreePath, best), "/")
	}
}

// probeGitHost asks an unknown host whether it is GitLab or Gitea/Forgejo
// using their public, unauthenticated endpoints.
func probeGitHost(host string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	try := func(p string, want func([]byte) bool) bool {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+host+p, nil)
		resp, err := gitHTTP.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return want(b)
	}
	if try("/api/v1/version", func(b []byte) bool { return strings.Contains(string(b), `"version"`) }) {
		return "gitea"
	}
	if try("/api/v4/projects?per_page=1", func(b []byte) bool { return strings.HasPrefix(strings.TrimSpace(string(b)), "[") }) {
		return "gitlab"
	}
	return ""
}

// ---------- accounts (tokens) ----------

// GitAccount is an optional connection for private repositories. Token is
// protected with protectString (DPAPI on Windows) and never sent to the UI.
type GitAccount struct {
	Kind  string `json:"kind"`
	Host  string `json:"host"`
	Org   string `json:"org,omitempty"`  // Azure DevOps
	User  string `json:"user,omitempty"` // Bitbucket: Atlassian account email
	Login string `json:"login,omitempty"`
	Token string `json:"token"`
	Hint  string `json:"hint"`
}

func accountFor(r *RepoRef) (*GitAccount, string) {
	c := snapshotConfig()
	a := c.GitAccounts[r.AccountKey()]
	if a == nil {
		return nil, ""
	}
	return a, secret(a.Token)
}

// tokenCreateURL is the "Create token" link with minimal read-only scopes
// pre-filled where the host supports it.
func tokenCreateURL(kind, host, org string) string {
	switch kind {
	case "github":
		// Fine-grained PAT template (GitHub docs, Aug 2025): contents=read (+metadata=read automatically).
		return "https://github.com/settings/personal-access-tokens/new?name=Dilreet&description=" + url.QueryEscape("Read-only access so Dilreet can download your sites") + "&expires_in=366&contents=read"
	case "gitlab":
		// GitLab docs: name/description/scopes query parameters.
		return "https://" + host + "/-/user_settings/personal_access_tokens?name=Dilreet&description=" + url.QueryEscape("Read-only, for Dilreet") + "&scopes=read_api,read_repository"
	case "bitbucket":
		// Atlassian API tokens "with scopes"; scopes cannot be pre-filled. Needed: read:repository:bitbucket (+ read:user:bitbucket to list repos).
		return "https://id.atlassian.com/manage-profile/security/api-tokens"
	case "gitea":
		return "https://" + host + "/user/settings/applications"
	case "azure":
		return "https://dev.azure.com/" + url.PathEscape(org) + "/_usersSettings/tokens"
	case "netlify":
		return "https://app.netlify.com/user/applications#personal-access-tokens"
	}
	return ""
}

func tokenScopesHint(kind string) string {
	switch kind {
	case "github":
		return "Repository access: the repos you want; Permissions: Contents = Read-only (pre-filled)."
	case "gitlab":
		return "Scopes read_api and read_repository (pre-filled)."
	case "bitbucket":
		return "Create an API token with scopes, app Bitbucket, scopes read:repository:bitbucket and read:user:bitbucket. Enter your Atlassian email too."
	case "gitea":
		return "Generate a token with repository: Read and user: Read."
	case "azure":
		return "New token, Scopes: Custom defined > Code: Read."
	case "netlify":
		return "New access token (Netlify tokens have full account access; you can revoke it after importing)."
	}
	return ""
}

// ---------- per-host API ----------

type gitAPI struct {
	ref   *RepoRef
	token string
	user  string
}

func newGitAPI(r *RepoRef) *gitAPI {
	a, tok := accountFor(r)
	g := &gitAPI{ref: r, token: tok}
	if a != nil {
		g.user = a.User
	}
	return g
}

func (g *gitAPI) authHeaders() map[string]string {
	h := map[string]string{}
	if g.token == "" {
		return h
	}
	switch g.ref.Kind {
	case "github":
		h["Authorization"] = "Bearer " + g.token
	case "gitlab":
		h["PRIVATE-TOKEN"] = g.token
	case "gitea":
		h["Authorization"] = "token " + g.token
	case "bitbucket":
		h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(g.user+":"+g.token))
	case "azure":
		h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+g.token))
	}
	return h
}

func (g *gitAPI) base() string {
	r := g.ref
	switch r.Kind {
	case "github":
		return githubAPI + "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Repo)
	case "gitlab":
		return "https://" + r.Host + "/api/v4/projects/" + url.PathEscape(r.Owner+"/"+r.Repo)
	case "gitea":
		return "https://" + r.Host + "/api/v1/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Repo)
	case "bitbucket":
		return bitbucketAPI + "/repositories/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Repo)
	case "azure":
		return azureBase + "/" + url.PathEscape(r.Org) + "/" + url.PathEscape(r.Project) + "/_apis/git/repositories/" + url.PathEscape(r.Repo)
	}
	return ""
}

// GitError is a friendly error with a hint whether connecting an account helps.
type GitError struct {
	Msg        string
	NeedsToken bool
	Status     int
}

func (e *GitError) Error() string { return e.Msg }

func (g *gitAPI) get(ctx context.Context, u string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, gitAPITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(req.URL.String(), "https://") {
		return errors.New("only HTTPS is allowed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Dilreet/"+Version)
	if g.ref.Kind == "github" {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	for k, v := range g.authHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := gitHTTP.Do(req)
	if err != nil {
		return &GitError{Msg: "Couldn't reach " + g.ref.Host + ". Check your internet connection and try again."}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return gitStatusError(g.ref, resp.StatusCode, g.token != "", resp.Header)
	}
	if s, ok := out.(*string); ok {
		*s = string(b)
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &GitError{Msg: g.ref.Host + " sent an unexpected answer (is this really a repository link?)."}
	}
	return nil
}

func gitStatusError(r *RepoRef, status int, hadToken bool, h http.Header) error {
	who := hostLabel(r.Kind)
	switch {
	case status == 401 || status == 403 && hadToken:
		if hadToken {
			return &GitError{Msg: "The " + who + " token was refused (expired, revoked or missing read access to this repository). Reconnect the account with a new token.", NeedsToken: true, Status: status}
		}
		return &GitError{Msg: "This repository is private. Connect your " + who + " account (a read-only token) to import it.", NeedsToken: true, Status: status}
	case status == 403 || status == 429:
		if h.Get("X-RateLimit-Remaining") == "0" || status == 429 {
			return &GitError{Msg: who + " is limiting requests right now. Wait a few minutes, or connect your account (higher limits).", Status: status}
		}
		return &GitError{Msg: who + " refused access to this repository.", NeedsToken: !hadToken, Status: status}
	case status == 404:
		if hadToken {
			return &GitError{Msg: "Repository or branch not found on " + who + ". Check the link, the branch name, and that your token can see this repository.", Status: status}
		}
		return &GitError{Msg: "Repository or branch not found. If it is private, connect your " + who + " account; otherwise check the link.", NeedsToken: true, Status: status}
	}
	return &GitError{Msg: fmt.Sprintf("%s answered with an error (HTTP %d). Try again in a minute.", who, status), Status: status}
}

func hostLabel(kind string) string {
	return map[string]string{"github": "GitHub", "gitlab": "GitLab", "bitbucket": "Bitbucket", "gitea": "Gitea/Codeberg", "azure": "Azure DevOps", "netlify": "Netlify"}[kind]
}

func (g *gitAPI) DefaultBranch(ctx context.Context) (string, error) {
	switch g.ref.Kind {
	case "github", "gitlab", "gitea":
		var v struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := g.get(ctx, g.base(), &v); err != nil {
			return "", err
		}
		return v.DefaultBranch, nil
	case "bitbucket":
		var v struct {
			Mainbranch struct{ Name string } `json:"mainbranch"`
		}
		if err := g.get(ctx, g.base(), &v); err != nil {
			return "", err
		}
		return v.Mainbranch.Name, nil
	case "azure":
		var v struct {
			DefaultBranch string `json:"defaultBranch"`
		}
		if err := g.get(ctx, g.base()+"?api-version=7.1", &v); err != nil {
			return "", err
		}
		return strings.TrimPrefix(v.DefaultBranch, "refs/heads/"), nil
	}
	return "", nil
}

// Branches returns branch names (first page, up to 100) where the host has a public API.
func (g *gitAPI) Branches(ctx context.Context) ([]string, error) {
	var names []string
	switch g.ref.Kind {
	case "github", "gitlab", "gitea":
		u := g.base() + "/branches?per_page=100"
		switch g.ref.Kind {
		case "gitlab":
			u = g.base() + "/repository/branches?per_page=100"
		case "gitea":
			u = g.base() + "/branches?limit=50"
		}
		var v []struct{ Name string }
		if err := g.get(ctx, u, &v); err != nil {
			return nil, err
		}
		for _, b := range v {
			names = append(names, b.Name)
		}
	case "bitbucket":
		var v struct{ Values []struct{ Name string } }
		if err := g.get(ctx, g.base()+"/refs/branches?pagelen=100", &v); err != nil {
			return nil, err
		}
		for _, b := range v.Values {
			names = append(names, b.Name)
		}
	case "azure":
		var v struct{ Value []struct{ Name string } }
		if err := g.get(ctx, g.base()+"/refs?filter=heads/&api-version=7.1", &v); err != nil {
			return nil, err
		}
		for _, b := range v.Value {
			names = append(names, strings.TrimPrefix(b.Name, "refs/heads/"))
		}
	}
	return names, nil
}

// LatestCommit returns the SHA the branch points to (used for auto-deploy polling).
func (g *gitAPI) LatestCommit(ctx context.Context, branch string) (string, error) {
	switch g.ref.Kind {
	case "github":
		var v struct{ SHA string }
		if err := g.get(ctx, g.base()+"/commits/"+url.PathEscape(branch), &v); err != nil {
			return "", err
		}
		return v.SHA, nil
	case "gitlab":
		var v struct{ ID string }
		if err := g.get(ctx, g.base()+"/repository/commits/"+url.PathEscape(branch), &v); err != nil {
			return "", err
		}
		return v.ID, nil
	case "gitea":
		var v struct{ Commit struct{ ID string } }
		if err := g.get(ctx, g.base()+"/branches/"+url.PathEscape(branch), &v); err != nil {
			return "", err
		}
		return v.Commit.ID, nil
	case "bitbucket":
		var v struct{ Hash string }
		if err := g.get(ctx, g.base()+"/commit/"+url.PathEscape(branch), &v); err != nil {
			return "", err
		}
		return v.Hash, nil
	case "azure":
		var v struct {
			Value []struct {
				Name     string
				ObjectID string `json:"objectId"`
			}
		}
		if err := g.get(ctx, g.base()+"/refs?filter=heads/"+url.QueryEscape(branch)+"&api-version=7.1", &v); err != nil {
			return "", err
		}
		for _, x := range v.Value {
			if x.Name == "refs/heads/"+branch {
				return x.ObjectID, nil
			}
		}
		return "", &GitError{Msg: "Branch " + branch + " not found.", Status: 404}
	}
	return "", errors.New("this source has no commit history to follow")
}

// ArchiveURL returns the archive download URL for ref (a branch or commit SHA).
func (g *gitAPI) ArchiveURL(ref string) (string, map[string]string) {
	r := g.ref
	h := g.authHeaders()
	switch r.Kind {
	case "github":
		if g.token != "" {
			return g.base() + "/zipball/" + url.PathEscape(ref), h
		}
		return githubCodeload + "/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Repo) + "/zip/" + escapeRefPath(ref), h
	case "gitlab":
		return g.base() + "/repository/archive.zip?sha=" + url.QueryEscape(ref), h
	case "gitea":
		return "https://" + r.Host + "/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Repo) + "/archive/" + escapeRefPath(ref) + ".zip", h
	case "bitbucket":
		if g.token != "" { // Git-over-HTTPS style auth for the web download (Atlassian docs)
			h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("x-bitbucket-api-token-auth:"+g.token))
		}
		return bitbucketWeb + "/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Repo) + "/get/" + url.PathEscape(ref) + ".zip", h
	case "azure":
		vt := "branch"
		if isSHA(ref) {
			vt = "commit"
		}
		q := url.Values{"scopePath": {"/"}, "versionDescriptor.version": {ref}, "versionDescriptor.versionType": {vt}, "$format": {"zip"}, "download": {"true"}, "api-version": {"7.1"}}
		return g.base() + "/items?" + q.Encode(), h
	}
	return "", h
}

func escapeRefPath(ref string) string {
	parts := strings.Split(ref, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func isSHA(s string) bool { return shaRe.MatchString(s) }

// UserRepos lists repositories the connected account can read.
type RepoItem struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Private bool   `json:"private"`
	Branch  string `json:"branch,omitempty"`
}

func listUserRepos(ctx context.Context, acc *GitAccount) ([]RepoItem, error) {
	ref := &RepoRef{Kind: acc.Kind, Host: acc.Host, Org: acc.Org}
	g := &gitAPI{ref: ref, token: secret(acc.Token), user: acc.User}
	var out []RepoItem
	switch acc.Kind {
	case "github":
		var v []struct {
			FullName      string `json:"full_name"`
			HTMLURL       string `json:"html_url"`
			Private       bool   `json:"private"`
			DefaultBranch string `json:"default_branch"`
		}
		if err := g.get(ctx, githubAPI+"/user/repos?per_page=100&sort=updated", &v); err != nil {
			return nil, err
		}
		for _, r := range v {
			out = append(out, RepoItem{r.FullName, r.HTMLURL, r.Private, r.DefaultBranch})
		}
	case "gitlab":
		var v []struct {
			Path          string `json:"path_with_namespace"`
			WebURL        string `json:"web_url"`
			Visibility    string `json:"visibility"`
			DefaultBranch string `json:"default_branch"`
		}
		if err := g.get(ctx, "https://"+acc.Host+"/api/v4/projects?membership=true&simple=true&per_page=100&order_by=last_activity_at", &v); err != nil {
			return nil, err
		}
		for _, r := range v {
			out = append(out, RepoItem{r.Path, r.WebURL, r.Visibility != "public", r.DefaultBranch})
		}
	case "gitea":
		var v []struct {
			FullName      string `json:"full_name"`
			HTMLURL       string `json:"html_url"`
			Private       bool   `json:"private"`
			DefaultBranch string `json:"default_branch"`
		}
		if err := g.get(ctx, "https://"+acc.Host+"/api/v1/user/repos?limit=50", &v); err != nil {
			return nil, err
		}
		for _, r := range v {
			out = append(out, RepoItem{r.FullName, r.HTMLURL, r.Private, r.DefaultBranch})
		}
	case "bitbucket":
		var v struct {
			Values []struct {
				FullName  string `json:"full_name"`
				IsPrivate bool   `json:"is_private"`
				Links     struct{ HTML struct{ Href string } }
				Main      struct{ Name string } `json:"mainbranch"`
			}
		}
		if err := g.get(ctx, bitbucketAPI+"/repositories?role=member&pagelen=100&sort=-updated_on", &v); err != nil {
			return nil, err
		}
		for _, r := range v.Values {
			out = append(out, RepoItem{r.FullName, r.Links.HTML.Href, r.IsPrivate, r.Main.Name})
		}
	case "azure":
		var v struct {
			Value []struct {
				Name          string
				WebURL        string `json:"webUrl"`
				DefaultBranch string `json:"defaultBranch"`
				Project       struct{ Name string }
			}
		}
		if err := g.get(ctx, azureBase+"/"+url.PathEscape(acc.Org)+"/_apis/git/repositories?api-version=7.1", &v); err != nil {
			return nil, err
		}
		for _, r := range v.Value {
			out = append(out, RepoItem{r.Project.Name + "/" + r.Name, r.WebURL, true, strings.TrimPrefix(r.DefaultBranch, "refs/heads/")})
		}
	default:
		return nil, errors.New("listing repositories is not available for this account type")
	}
	return out, nil
}

// verifyAccount checks a token and returns the account's login name.
func verifyAccount(ctx context.Context, acc *GitAccount, token string) (string, error) {
	g := &gitAPI{ref: &RepoRef{Kind: acc.Kind, Host: acc.Host, Org: acc.Org}, token: token, user: acc.User}
	switch acc.Kind {
	case "github":
		var v struct{ Login string }
		err := g.get(ctx, githubAPI+"/user", &v)
		return v.Login, err
	case "gitlab":
		var v struct{ Username string }
		err := g.get(ctx, "https://"+acc.Host+"/api/v4/user", &v)
		return v.Username, err
	case "gitea":
		var v struct{ Login string }
		err := g.get(ctx, "https://"+acc.Host+"/api/v1/user", &v)
		return v.Login, err
	case "bitbucket":
		var v struct {
			Name string `json:"display_name"`
		}
		err := g.get(ctx, bitbucketAPI+"/user", &v)
		return v.Name, err
	case "azure":
		var v struct{ Count int }
		err := g.get(ctx, azureBase+"/"+url.PathEscape(acc.Org)+"/_apis/projects?api-version=7.1", &v)
		return acc.Org, err
	case "netlify":
		var v struct {
			Email    string
			FullName string `json:"full_name"`
		}
		err := netlifyGet(ctx, token, "/user", &v)
		return firstNonEmpty(v.FullName, v.Email), err
	}
	return "", errors.New("unknown account type")
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------- download + unpack ----------

// downloadFile streams an HTTPS URL to dst with a size limit; redirects must
// stay on HTTPS. Progress goes to the job when given.
func downloadFile(ctx context.Context, u, dst string, limit int64, headers map[string]string, j *Job) error {
	if !strings.HasPrefix(u, "https://") {
		return errors.New("only HTTPS downloads are allowed")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Dilreet/"+Version)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := gitHTTP.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) || strings.Contains(err.Error(), "dial") {
			return errors.New("couldn't connect; check your internet connection")
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &GitError{Msg: fmt.Sprintf("download failed (HTTP %d)", resp.StatusCode), Status: resp.StatusCode, NeedsToken: resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404}
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("the download is too large (%d MB, max %d MB)", resp.ContentLength>>20, limit>>20)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	var done int64
	buf := make([]byte, 256<<10)
	last := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			done += int64(n)
			if done > limit {
				return fmt.Errorf("the download is too large (more than %d MB)", limit>>20)
			}
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			if j != nil && time.Since(last) > 500*time.Millisecond {
				last = time.Now()
				if resp.ContentLength > 0 {
					j.SetProgress(int(done*100/resp.ContentLength), fmt.Sprintf("Downloading (%s)", humanBytes(done)))
				} else {
					j.SetProgress(-1, fmt.Sprintf("Downloading (%s)", humanBytes(done)))
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("download interrupted: %w", rerr)
		}
	}
	return f.Close()
}

// archiveEntries opens a downloaded .zip or .tar.gz with the zip-slip-safe
// code from archive.go. tar.gz is unpacked to a temp dir first (with the
// same cleanRel checks and limits).
func archiveEntries(file string) ([]srcEntry, func(), error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, nil, err
	}
	magic := make([]byte, 4)
	_, _ = io.ReadFull(f, magic)
	f.Close()
	if string(magic[:2]) == "PK" {
		return zipEntries(file)
	}
	if magic[0] == 0x1f && magic[1] == 0x8b {
		dir, err := os.MkdirTemp("", "dilreet-tgz-*")
		if err != nil {
			return nil, nil, err
		}
		cleanup := func() { os.RemoveAll(dir) }
		if err := untarGz(file, dir); err != nil {
			cleanup()
			return nil, nil, err
		}
		e, err := dirEntries(dir)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		return e, cleanup, nil
	}
	return nil, nil, errors.New("the link did not return a .zip or .tar.gz file (it may be a web page; use the direct download link)")
}

func untarGz(file, dir string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a valid .tar.gz: %w", err)
	}
	tr := tar.NewReader(gz)
	var n int
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("not a valid .tar.gz: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue // directories are implied; links/devices are never extracted
		}
		rel, err := cleanRel(h.Name)
		if err != nil {
			return err
		}
		if rel == "" || strings.HasPrefix(path.Base(rel), "._") || rel == "pax_global_header" {
			continue
		}
		n++
		total += h.Size
		if n > maxSiteFiles {
			return fmt.Errorf("too many files (max %d)", maxSiteFiles)
		}
		if total > maxSiteBytes {
			return fmt.Errorf("the project is too large (max %d MB unpacked)", maxSiteBytes>>20)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, h.Size))
		out.Close()
		if err != nil {
			return err
		}
	}
}

// selectSubdir strips the archive's single wrapper folder (every host adds
// one, e.g. "repo-main/") and keeps only the chosen base directory.
func selectSubdir(entries []srcEntry, subdir string, wrapped bool) ([]srcEntry, error) {
	if wrapped && len(entries) > 0 {
		top := ""
		same := true
		for _, e := range entries {
			i := strings.IndexByte(e.Path, '/')
			if i < 0 {
				same = false
				break
			}
			if top == "" {
				top = e.Path[:i]
			} else if e.Path[:i] != top {
				same = false
				break
			}
		}
		if same && top != "" {
			for i := range entries {
				entries[i].Path = entries[i].Path[len(top)+1:]
			}
		}
	}
	subdir = strings.Trim(strings.ReplaceAll(subdir, "\\", "/"), "/")
	if subdir == "" {
		return entries, nil
	}
	if c, err := cleanRel(subdir); err != nil || c == "" {
		return nil, errors.New("invalid base directory")
	} else {
		subdir = c
	}
	var out []srcEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Path, subdir+"/") {
			e.Path = e.Path[len(subdir)+1:]
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the folder %q was not found in this repository", subdir)
	}
	return out, nil
}

// ---------- fetching a source into a Workspace ----------

type FetchResult struct {
	WS     *Workspace
	Ref    *RepoRef
	SHA    string
	Extras map[string]any
}

// fetchRepo downloads ref (branch/subdir already chosen) into a Workspace.
func fetchRepo(ctx context.Context, j *Job, ref *RepoRef) (*FetchResult, error) {
	tmp, err := os.MkdirTemp("", "dilreet-import-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	logf := func(f string, a ...any) {
		if j != nil {
			j.Logf(f, a...)
		}
	}
	res := &FetchResult{Ref: ref}
	var entries []srcEntry
	var cleanup func()
	switch ref.Kind {
	case "archive":
		logf("Downloading %s ...", ref.URL)
		dst := filepath.Join(tmp, "download")
		if err := downloadFile(ctx, ref.URL, dst, maxRepoDownload, nil, j); err != nil {
			return nil, err
		}
		entries, cleanup, err = archiveEntries(dst)
		if err == nil {
			entries, err = selectSubdir(entries, ref.Subdir, false)
		}
	case "git":
		entries, cleanup, err = gitCloneEntries(ctx, j, ref)
		if err == nil {
			entries, err = selectSubdir(entries, ref.Subdir, false)
		}
	case "netlify":
		return nil, errors.New("use Import from Netlify for Netlify sites")
	default:
		g := newGitAPI(ref)
		if ref.Branch == "" {
			b, err := g.DefaultBranch(ctx)
			if err != nil {
				return nil, err
			}
			ref.Branch = b
		}
		sha, err := g.LatestCommit(ctx, ref.Branch)
		if err != nil {
			var ge *GitError
			if !errors.As(err, &ge) || ge.NeedsToken || (ge.Status != 403 && ge.Status != 429) {
				return nil, err
			}
			logf("Note: %v Downloading the branch without the commit id.", err)
			sha = ""
		}
		res.SHA = sha
		logf("Downloading %s, branch %s (commit %s) without git...", ref.Display, ref.Branch, short(sha))
		u, h := g.ArchiveURL(firstNonEmpty(sha, ref.Branch))
		dst := filepath.Join(tmp, "download")
		derr := downloadFile(ctx, u, dst, maxRepoDownload, h, j)
		if derr != nil {
			var ge *GitError
			if errors.As(derr, &ge) && ge.NeedsToken && g.token == "" {
				return nil, &GitError{Msg: "This repository is private (or the link is wrong). Connect your " + hostLabel(ref.Kind) + " account to import it.", NeedsToken: true}
			}
			if _, lerr := exec.LookPath("git"); lerr == nil && g.token == "" {
				logf("Archive download failed (%v); trying git clone --depth 1 ...", derr)
				entries, cleanup, err = gitCloneEntries(ctx, j, ref)
				if err == nil {
					entries, err = selectSubdir(entries, ref.Subdir, false)
				}
				break
			}
			return nil, derr
		}
		entries, cleanup, err = archiveEntries(dst)
		if err == nil {
			entries, err = selectSubdir(entries, ref.Subdir, ref.Kind != "azure")
		}
	}
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, err
	}
	ws, err := newWorkspace(entries, "import "+ref.Display)
	if err != nil {
		return nil, err
	}
	res.WS = ws
	logf("Unpacked %d files (%s). Detected: %s.", ws.FileCount, humanBytes(ws.Bytes), ws.Project.Framework)
	return res, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// gitCloneEntries is the fallback for plain git URLs: needs git installed.
// Credentials are never passed on the command line; prompts are disabled.
func gitCloneEntries(ctx context.Context, j *Job, ref *RepoRef) ([]srcEntry, func(), error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, nil, errors.New("this link needs git, which is not installed on this PC. Install Git for Windows (git-scm.com), or paste a GitHub/GitLab/Bitbucket/Codeberg link or a .zip download link instead")
	}
	u := ref.URL
	if ref.Kind != "git" {
		u = "https://" + ref.Host + "/" + ref.Owner + "/" + ref.Repo + ".git"
	}
	if !strings.HasPrefix(u, "https://") {
		return nil, nil, errors.New("only https:// git URLs are supported")
	}
	dir, err := os.MkdirTemp("", "dilreet-clone-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	args := []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "clone", "--depth", "1", "--single-branch", "--no-tags"}
	if ref.Branch != "" {
		args = append(args, "--branch", ref.Branch)
	}
	args = append(args, "--", u, filepath.Join(dir, "repo"))
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, gitBin, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_LFS_SKIP_SMUDGE=1")
	hideConsole(cmd)
	if j != nil {
		cmd.Stdout, cmd.Stderr = j, j
	}
	if err := cmd.Run(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("git clone failed (private repository, wrong branch, or no network): %v", err)
	}
	e, err := dirEntries(filepath.Join(dir, "repo"))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return e, cleanup, nil
}
