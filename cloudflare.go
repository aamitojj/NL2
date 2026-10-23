package main

// Cloudflare automatic DNS. Endpoints (developers.cloudflare.com/api, and the
// same calls in Coolify's app/Services/CloudflareTokenValidator.php):
//   GET    /user/tokens/verify
//   GET    /zones?name=<domain>
//   GET    /zones/{zone_id}/dns_records?name=<fqdn>
//   POST   /zones/{zone_id}/dns_records          {type,name,content,ttl,proxied,comment}
//   PATCH  /zones/{zone_id}/dns_records/{id}
//   DELETE /zones/{zone_id}/dns_records/{id}
// The token needs Zone:DNS:Edit (+ Zone:Zone:Read to find the zone).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var cfBase = "https://api.cloudflare.com/client/v4"

// cfTokenURL opens Cloudflare's "create token" page with the "Edit zone DNS"
// template preselected.
const cfTokenURL = "https://dash.cloudflare.com/profile/api-tokens"

type cfClient struct {
	token string
	http  *http.Client
}

func newCF(token string) *cfClient {
	return &cfClient{token: strings.TrimSpace(token), http: &http.Client{Timeout: 20 * time.Second}}
}

type cfEnvelope struct {
	Success bool            `json:"success"`
	Errors  []cfMsg         `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type cfMsg struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type CFRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl,omitempty"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment,omitempty"`
}

func (c *cfClient) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, cfBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach Cloudflare: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env cfEnvelope
	_ = json.Unmarshal(b, &env)
	if resp.StatusCode >= 300 || !env.Success {
		msg := http.StatusText(resp.StatusCode)
		if len(env.Errors) > 0 {
			msg = env.Errors[0].Message
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			msg += " (check that the token has the 'Edit zone DNS' permission for this domain)"
		}
		return &APIError{Status: resp.StatusCode, Body: "Cloudflare: " + msg}
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (c *cfClient) Verify() error {
	var r struct {
		Status string `json:"status"`
	}
	if err := c.do("GET", "/user/tokens/verify", nil, &r); err != nil {
		return err
	}
	if r.Status != "active" {
		return fmt.Errorf("this Cloudflare token is %q, not active", r.Status)
	}
	return nil
}

// FindZone finds the zone that contains domain (tries example.com for
// shop.example.com, etc.).
func (c *cfClient) FindZone(domain string) (id, name string, err error) {
	labels := strings.Split(domain, ".")
	for i := 0; i+1 < len(labels); i++ {
		cand := strings.Join(labels[i:], ".")
		var zones []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := c.do("GET", "/zones?name="+url.QueryEscape(cand), nil, &zones); err != nil {
			return "", "", err
		}
		for _, z := range zones {
			if z.Name == cand {
				return z.ID, z.Name, nil
			}
		}
	}
	return "", "", fmt.Errorf("%s is not in this Cloudflare account (or the token cannot see it). Add the domain to Cloudflare first, or use manual DNS", domain)
}

func (c *cfClient) Records(zone, fqdn string) ([]CFRecord, error) {
	var r []CFRecord
	return r, c.do("GET", "/zones/"+zone+"/dns_records?per_page=100&name="+url.QueryEscape(fqdn), nil, &r)
}

func (c *cfClient) Create(zone string, r CFRecord) (CFRecord, error) {
	var out CFRecord
	return out, c.do("POST", "/zones/"+zone+"/dns_records", r, &out)
}

func (c *cfClient) Patch(zone, id string, fields map[string]any) (CFRecord, error) {
	var out CFRecord
	return out, c.do("PATCH", "/zones/"+zone+"/dns_records/"+id, fields, &out)
}

func (c *cfClient) Delete(zone, id string) error {
	return c.do("DELETE", "/zones/"+zone+"/dns_records/"+id, nil, nil)
}

// CFConflict describes an existing record Dilreet would have to replace.
type CFConflict struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

var errCFConflict = errors.New("existing DNS records would be replaced")

// UpsertA makes fqdn resolve to ip with exactly one A record, DNS-only
// (grey cloud) so Let's Encrypt can issue the certificate directly. Records
// that conflict (CNAME, other A, AAAA) are only removed if replace is true;
// otherwise they are returned so the user can confirm.
func (c *cfClient) UpsertA(zone, fqdn, ip string, replace bool) (string, []CFConflict, error) {
	recs, err := c.Records(zone, fqdn)
	if err != nil {
		return "", nil, err
	}
	var keep *CFRecord
	var conflicts []CFConflict
	var remove []CFRecord
	for i := range recs {
		r := recs[i]
		switch r.Type {
		case "A":
			if keep == nil {
				keep = &recs[i]
				if r.Content != ip {
					conflicts = append(conflicts, CFConflict{r.Name, r.Type, r.Content})
				}
			} else {
				conflicts = append(conflicts, CFConflict{r.Name, r.Type, r.Content})
				remove = append(remove, r)
			}
		case "CNAME", "AAAA":
			conflicts = append(conflicts, CFConflict{r.Name, r.Type, r.Content})
			remove = append(remove, r)
		}
	}
	if len(conflicts) > 0 && !replace {
		return "", conflicts, errCFConflict
	}
	for _, r := range remove {
		if err := c.Delete(zone, r.ID); err != nil {
			return "", nil, err
		}
	}
	if keep != nil {
		out, err := c.Patch(zone, keep.ID, map[string]any{"content": ip, "proxied": false, "ttl": 1})
		return out.ID, nil, err
	}
	out, err := c.Create(zone, CFRecord{Type: "A", Name: fqdn, Content: ip, TTL: 1, Proxied: false, Comment: "Managed by Dilreet"})
	return out.ID, nil, err
}
