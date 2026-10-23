package main

// Optional "Create a server for me" with Hetzner Cloud or DigitalOcean.
// Endpoints/fields checked against the official OpenAPI specs
// (docs.hetzner.cloud/cloud.spec.json, DigitalOcean-public.v2.yaml):
//   Hetzner: GET /v1/server_types, POST /v1/ssh_keys {name, public_key},
//            POST /v1/servers {name, server_type, image, location, ssh_keys},
//            GET /v1/servers/{id} -> server.status "running", server.public_net.ipv4.ip
//   DigitalOcean: GET /v2/sizes, POST /v2/account/keys {name, public_key},
//            POST /v2/droplets {name, region, size, image, ssh_keys},
//            GET /v2/droplets/{id} -> droplet.status "active", networks.v4[type=public].ip_address
// The server gets Dilreet's own SSH key, so no password is ever needed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	hetznerAPI = "https://api.hetzner.cloud/v1"
	doAPI      = "https://api.digitalocean.com/v2"
)

type CloudPlan struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Cores    float64  `json:"cores"`
	MemoryGB float64  `json:"memory_gb"`
	DiskGB   float64  `json:"disk_gb"`
	Monthly  float64  `json:"monthly"`
	Currency string   `json:"currency"`
	Regions  []string `json:"regions"`
}

type cloudProvider struct {
	kind, token string
}

func (p cloudProvider) do(ctx context.Context, method, path string, body, out any) error {
	base := hetznerAPI
	if p.kind == "digitalocean" {
		base = doAPI
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := gitHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach %s: %v", p.label(), err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error   struct{ Message string } `json:"error"`
			Message string                   `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		msg := firstNonEmpty(e.Error.Message, e.Message, http.StatusText(resp.StatusCode))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("%s refused the API token (%s). It needs Read & Write access", p.label(), msg)
		}
		return fmt.Errorf("%s: %s", p.label(), msg)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (p cloudProvider) label() string {
	if p.kind == "digitalocean" {
		return "DigitalOcean"
	}
	return "Hetzner Cloud"
}

// Plans lists sizes that meet Coolify's minimum (2 cores, 2 GB RAM, 30 GB
// disk), cheapest first.
func (p cloudProvider) Plans(ctx context.Context) ([]CloudPlan, error) {
	var out []CloudPlan
	if p.kind == "hetzner" {
		var v struct {
			ServerTypes []struct {
				Name         string  `json:"name"`
				Cores        float64 `json:"cores"`
				Memory       float64 `json:"memory"`
				Disk         float64 `json:"disk"`
				Architecture string  `json:"architecture"`
				Prices       []struct {
					Location     string                 `json:"location"`
					PriceMonthly struct{ Gross string } `json:"price_monthly"`
				} `json:"prices"`
				Locations []struct {
					Name        string `json:"name"`
					Deprecation *struct {
						UnavailableAfter string `json:"unavailable_after"`
					} `json:"deprecation"`
				} `json:"locations"`
			} `json:"server_types"`
		}
		if err := p.do(ctx, "GET", "/server_types?per_page=50", nil, &v); err != nil {
			return nil, err
		}
		for _, t := range v.ServerTypes {
			if t.Architecture != "x86" || t.Cores < 2 || t.Memory < 2 || t.Disk < 30 {
				continue
			}
			pl := CloudPlan{ID: t.Name, Label: strings.ToUpper(t.Name), Cores: t.Cores, MemoryGB: t.Memory, DiskGB: t.Disk, Currency: "EUR", Monthly: 1e9}
			for _, l := range t.Locations {
				if l.Deprecation == nil {
					pl.Regions = append(pl.Regions, l.Name)
				}
			}
			for _, pr := range t.Prices {
				if f, err := strconv.ParseFloat(pr.PriceMonthly.Gross, 64); err == nil && f < pl.Monthly {
					pl.Monthly = f
				}
			}
			if len(pl.Regions) > 0 {
				out = append(out, pl)
			}
		}
	} else {
		var v struct {
			Sizes []struct {
				Slug         string   `json:"slug"`
				Memory       float64  `json:"memory"`
				VCPUs        float64  `json:"vcpus"`
				Disk         float64  `json:"disk"`
				PriceMonthly float64  `json:"price_monthly"`
				Regions      []string `json:"regions"`
				Available    bool     `json:"available"`
			} `json:"sizes"`
		}
		if err := p.do(ctx, "GET", "/sizes?per_page=200", nil, &v); err != nil {
			return nil, err
		}
		for _, s := range v.Sizes {
			if !s.Available || s.VCPUs < 2 || s.Memory < 2048 || s.Disk < 30 || !strings.HasPrefix(s.Slug, "s-") {
				continue
			}
			out = append(out, CloudPlan{ID: s.Slug, Label: s.Slug, Cores: s.VCPUs, MemoryGB: s.Memory / 1024, DiskGB: s.Disk, Monthly: s.PriceMonthly, Currency: "USD", Regions: s.Regions})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Monthly < out[j].Monthly })
	if len(out) > 6 {
		out = out[:6]
	}
	return out, nil
}

// CreateServer creates the server with Dilreet's public key and waits for
// its public IPv4 address.
func (p cloudProvider) CreateServer(ctx context.Context, j *Job, name, plan, region, pubKey string) (id, ip string, err error) {
	keyName := "dilreet-" + randHex(4)
	if p.kind == "hetzner" {
		var k struct {
			SSHKey struct {
				ID int64 `json:"id"`
			} `json:"ssh_key"`
		}
		if err := p.do(ctx, "POST", "/ssh_keys", map[string]any{"name": keyName, "public_key": pubKey}, &k); err != nil {
			return "", "", err
		}
		body := map[string]any{"name": name, "server_type": plan, "image": "ubuntu-24.04", "ssh_keys": []string{strconv.FormatInt(k.SSHKey.ID, 10)}, "labels": map[string]string{"managed-by": "dilreet"}}
		if region != "" {
			body["location"] = region
		}
		var s struct {
			Server struct {
				ID int64 `json:"id"`
			} `json:"server"`
		}
		if err := p.do(ctx, "POST", "/servers", body, &s); err != nil {
			return "", "", err
		}
		id = strconv.FormatInt(s.Server.ID, 10)
	} else {
		var k struct {
			SSHKey struct {
				ID int64 `json:"id"`
			} `json:"ssh_key"`
		}
		if err := p.do(ctx, "POST", "/account/keys", map[string]any{"name": keyName, "public_key": pubKey}, &k); err != nil {
			return "", "", err
		}
		var d struct {
			Droplet struct {
				ID int64 `json:"id"`
			} `json:"droplet"`
		}
		body := map[string]any{"name": name, "region": firstNonEmpty(region, "fra1"), "size": plan, "image": "ubuntu-24-04-x64", "ssh_keys": []int64{k.SSHKey.ID}, "tags": []string{"dilreet"}}
		if err := p.do(ctx, "POST", "/droplets", body, &d); err != nil {
			return "", "", err
		}
		id = strconv.FormatInt(d.Droplet.ID, 10)
	}
	j.Logf("%s server %s created (id %s). Waiting for it to start...", p.label(), name, id)
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return id, "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
		ip, ready, err := p.serverIP(ctx, id)
		if err == nil && ready && ip != "" {
			return id, ip, nil
		}
	}
	return id, "", errors.New("the server did not start within 6 minutes; check the provider's console")
}

func (p cloudProvider) serverIP(ctx context.Context, id string) (string, bool, error) {
	if p.kind == "hetzner" {
		var v struct {
			Server struct {
				Status    string `json:"status"`
				PublicNet struct {
					IPv4 *struct{ IP string } `json:"ipv4"`
				} `json:"public_net"`
			} `json:"server"`
		}
		if err := p.do(ctx, "GET", "/servers/"+id, nil, &v); err != nil {
			return "", false, err
		}
		ip := ""
		if v.Server.PublicNet.IPv4 != nil {
			ip = v.Server.PublicNet.IPv4.IP
		}
		return ip, v.Server.Status == "running", nil
	}
	var v struct {
		Droplet struct {
			Status   string `json:"status"`
			Networks struct {
				V4 []struct {
					IPAddress string `json:"ip_address"`
					Type      string `json:"type"`
				} `json:"v4"`
			} `json:"networks"`
		} `json:"droplet"`
	}
	if err := p.do(ctx, "GET", "/droplets/"+id, nil, &v); err != nil {
		return "", false, err
	}
	for _, n := range v.Droplet.Networks.V4 {
		if n.Type == "public" {
			return n.IPAddress, v.Droplet.Status == "active", nil
		}
	}
	return "", false, nil
}

func cloudTokenURL(kind string) string {
	if kind == "digitalocean" {
		return "https://cloud.digitalocean.com/account/api/tokens"
	}
	return "https://console.hetzner.cloud/projects" // Project -> Security -> API tokens
}

// createCloudServerJob: create server -> wait for SSH -> full zero-touch setup.
func createCloudServerJob(j *Job, kind, token, plan, region string, o SetupOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Minute)
	defer cancel()
	p := cloudProvider{kind: kind, token: token}
	_, pub, err := ensureManagedKey()
	if err != nil {
		return err
	}
	j.SetProgress(-1, "Creating your server at "+p.label())
	id, ip, err := p.CreateServer(ctx, j, "dilreet-"+randHex(3), plan, region, pub)
	if err != nil {
		return err
	}
	j.Logf("Server is running at %s.", ip)
	_ = withConfig(func(c *Config) error {
		c.ServerIP, c.SSHPort, c.SSHUser, c.SSHKeyPath, c.HostKey = ip, 22, "root", "", ""
		c.Provider, c.ProviderServerID = kind, id
		c.CoolifyURL, c.CoolifyToken, c.ServerUUID = "", "", ""
		return nil
	})
	sshS.close()
	j.Logf("Connecting with Dilreet's own SSH key (no password)...")
	for i := 0; i < 36; i++ { // cloud-init may need a moment to install the key
		if _, _, err = connectSSH("", ""); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("server created but SSH is not reachable yet (%v). Open Settings > Server and click Connect in a minute", err)
	}
	return setupServerJob(j, o)
}
