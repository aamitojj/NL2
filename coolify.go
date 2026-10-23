package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Coolify is a minimal client for the Coolify API v1
// (https://coolify.io/docs/api-reference). Only endpoints that exist in the
// official OpenAPI spec are used.
type Coolify struct {
	base  string
	token string
	http  *http.Client
}

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	msg := e.Body
	var m map[string]any
	if json.Unmarshal([]byte(e.Body), &m) == nil {
		if s, ok := m["message"].(string); ok {
			msg = s
			if errs, ok := m["errors"]; ok {
				b, _ := json.Marshal(errs)
				msg += " " + string(b)
			}
			if c, ok := m["conflicts"]; ok {
				b, _ := json.Marshal(c)
				msg += " conflicts: " + string(b)
			}
		}
	}
	if len(msg) > 600 {
		msg = msg[:600] + "..."
	}
	return fmt.Sprintf("Coolify API error %d: %s", e.Status, msg)
}

func newCoolify() (*Coolify, error) {
	c := snapshotConfig()
	token := getToken()
	if token == "" {
		return nil, errors.New("no Coolify API token saved yet: finish step 3 of Setup")
	}
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second}
	base := strings.TrimRight(c.CoolifyURL, "/")
	if c.APIViaSSH {
		if !sshS.connected() {
			return nil, errNotConnected
		}
		base = "http://127.0.0.1:8000"
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialRemote("127.0.0.1:8000")
		}
	}
	if base == "" {
		return nil, errors.New("Coolify URL is not set")
	}
	return &Coolify{base: base + "/api/v1", token: token, http: &http.Client{Timeout: 60 * time.Second, Transport: tr}}, nil
}

func (c *Coolify) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach Coolify at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Body: string(data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unexpected Coolify response (%s): %w", strings.TrimSpace(string(data[:min(len(data), 200)])), err)
		}
	}
	return nil
}

// doAction calls an action endpoint with POST (current spec) and falls back to
// GET for older Coolify v4 releases where these were GET endpoints.
func (c *Coolify) doAction(path string, out any) error {
	err := c.do("POST", path, nil, out)
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == 405 || ae.Status == 404) {
		if err2 := c.do("GET", path, nil, out); err2 == nil {
			return nil
		}
	}
	return err
}

func (c *Coolify) Version() (string, error) {
	req, _ := http.NewRequest("GET", c.base+"/version", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach Coolify at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return "", &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	return strings.Trim(strings.TrimSpace(string(b)), `"`), nil
}

type CServer struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
	IP   string `json:"ip"`
}

func (c *Coolify) Servers() ([]CServer, error) {
	var s []CServer
	return s, c.do("GET", "/servers", nil, &s)
}

type CProject struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (c *Coolify) Projects() ([]CProject, error) {
	var p []CProject
	return p, c.do("GET", "/projects", nil, &p)
}

func (c *Coolify) CreateProject(name, desc string) (string, error) {
	var r struct {
		UUID string `json:"uuid"`
	}
	err := c.do("POST", "/projects", map[string]any{"name": name, "description": desc}, &r)
	return r.UUID, err
}

func (c *Coolify) DeleteProject(uuid string) error {
	return c.do("DELETE", "/projects/"+url.PathEscape(uuid), nil, nil)
}

type CApp struct {
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	FQDN   string `json:"fqdn"`
	Status string `json:"status"`
}

func (c *Coolify) Applications() ([]CApp, error) {
	var a []CApp
	return a, c.do("GET", "/applications", nil, &a)
}

func (c *Coolify) Application(uuid string) (*CApp, error) {
	var a CApp
	return &a, c.do("GET", "/applications/"+url.PathEscape(uuid), nil, &a)
}

// CreateNginxApp creates a "Docker Image (without git)" application running
// the official nginx image. Site files are bind-mounted from the host.
func (c *Coolify) CreateNginxApp(projectUUID, serverUUID, name string) (string, error) {
	body := map[string]any{
		"project_uuid":               projectUUID,
		"server_uuid":                serverUUID,
		"environment_name":           "production",
		"docker_registry_image_name": "nginx",
		"docker_registry_image_tag":  "stable-alpine",
		"ports_exposes":              "80",
		"name":                       name,
		"description":                "Static site managed by Dilreet",
		"instant_deploy":             false,
		"autogenerate_domain":        true,
		"health_check_enabled":       false,
		"limits_memory":              "128m",
	}
	var r struct {
		UUID string `json:"uuid"`
	}
	err := c.do("POST", "/applications/dockerimage", body, &r)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 422 && strings.Contains(ae.Body, "autogenerate_domain") {
		// Older Coolify versions reject unknown fields.
		delete(body, "autogenerate_domain")
		err = c.do("POST", "/applications/dockerimage", body, &r)
	}
	return r.UUID, err
}

// AddDirectoryMount bind-mounts hostPath (on the server) to mountPath in the
// container, via POST /applications/{uuid}/storages (type=file, is_directory).
func (c *Coolify) AddDirectoryMount(appUUID, hostPath, mountPath string) error {
	return c.do("POST", "/applications/"+url.PathEscape(appUUID)+"/storages", map[string]any{
		"type":         "file",
		"is_directory": true,
		"fs_path":      hostPath,
		"mount_path":   mountPath,
	}, nil)
}

func (c *Coolify) SetDomains(appUUID, domains string, force bool) error {
	body := map[string]any{"domains": domains}
	if force {
		body["force_domain_override"] = true
	}
	return c.do("PATCH", "/applications/"+url.PathEscape(appUUID), body, nil)
}

func (c *Coolify) Deploy(appUUID string) error {
	return c.doAction("/deploy?uuid="+url.QueryEscape(appUUID)+"&force=false", nil)
}

func (c *Coolify) Restart(appUUID string) error {
	return c.doAction("/applications/"+url.PathEscape(appUUID)+"/restart", nil)
}

func (c *Coolify) DeleteApp(appUUID string) error {
	q := "?delete_configurations=true&delete_volumes=true&docker_cleanup=true&delete_connected_networks=true"
	return c.do("DELETE", "/applications/"+url.PathEscape(appUUID)+q, nil, nil)
}
