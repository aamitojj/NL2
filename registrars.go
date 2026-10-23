package main

// Registrar DNS automation and guided setup.
//
// GoDaddy (developer.godaddy.com, checked Oct 2026):
//   - Production DNS API access: since April 2026 any account with 1+ domain
//     qualifies (April 2024 - April 2026 it needed 10+ domains, Discount Domain
//     Club Pro or $20+/month). Ineligible accounts get 403 ACCESS_DENIED.
//   - Domains v3 uses Personal Access Tokens (Authorization: Bearer <PAT>, scope
//     domains.dns:update; domains.nameserver:update to switch nameservers):
//       GET/POST /v3/domains/zones/{zone}/dns-records?type=&name=
//       PUT/DELETE /v3/domains/zones/{zone}/dns-records/{recordId}
//       PUT /v3/domains/domain-names/{domain}/nameservers   ["ns1","ns2"]
//   - Domains v1 with the classic key (Authorization: sso-key KEY:SECRET,
//     deprecated for Domains through 2026):
//       GET/PUT/DELETE /v1/domains/{domain}/records/{type}/{name}  PUT body [{data,ttl}]
// Porkbun (porkbun.com/api/json/v3/documentation): JSON body apikey+secretapikey,
//   POST /ping, /dns/retrieveByNameType/{d}/{type}/{sub}, /dns/editByNameType/...,
//   /dns/create/{d}, /dns/deleteByNameType/{d}/{type}/{sub}. API Access must be
//   switched on per domain in Porkbun's Domain Management.
// Namecheap's API needs a whitelisted IPv4 and an eligible account (20+ domains,
//   or $50 balance/spend) and setHosts replaces the whole zone, so Dilreet only
//   offers the guided mode there.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

var (
	gdBase = "https://api.godaddy.com"
	pbBase = "https://api.porkbun.com/api/json/v3"
)

type regRecord struct {
	ID   string
	Type string
	Name string // relative: "@" or "www"
	Data string
}

type registrarDNS interface {
	Verify(zone string) error
	List(zone, name string) ([]regRecord, error)
	SetA(zone, name, ip string, existingA []regRecord) error
	Delete(zone string, r regRecord) error
}

// ---------- GoDaddy ----------

type gdClient struct {
	auth string
	v3   bool
	http *http.Client
}

func newGoDaddy(c RegistrarCred) *gdClient {
	g := &gdClient{http: &http.Client{Timeout: 20 * time.Second}}
	if c.Mode == "pat" {
		g.auth, g.v3 = "Bearer "+secret(c.Key), true
	} else {
		g.auth = "sso-key " + secret(c.Key) + ":" + secret(c.Secret)
	}
	return g
}

const gdEligibility = "GoDaddy refused API access for this account (ACCESS_DENIED). GoDaddy's DNS API needs a production key on an account that holds at least one domain (from April 2024 to April 2026 it required 10+ domains or Discount Domain Club). If your account qualifies, contact GoDaddy support; otherwise use the guided steps or move DNS to Cloudflare."

func (g *gdClient) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, gdBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", g.auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Dilreet/"+Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach GoDaddy: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		switch {
		case resp.StatusCode == 401:
			return &APIError{Status: 401, Body: "GoDaddy rejected the key. Use a Production key (not OTE/test) and check it was copied completely."}
		case resp.StatusCode == 403 && (e.Code == "ACCESS_DENIED" || strings.Contains(strings.ToLower(e.Message), "access")):
			return &APIError{Status: 403, Body: gdEligibility}
		case resp.StatusCode == 403:
			return &APIError{Status: 403, Body: "GoDaddy: not allowed (" + firstNonEmpty(e.Message, e.Code) + "). A Personal Access Token needs the domains.dns:update scope."}
		case resp.StatusCode == 404:
			return &APIError{Status: 404, Body: "GoDaddy: this domain is not in the GoDaddy account of this key."}
		}
		return &APIError{Status: resp.StatusCode, Body: "GoDaddy: " + firstNonEmpty(e.Message, e.Code, http.StatusText(resp.StatusCode))}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (g *gdClient) Verify(zone string) error {
	if g.v3 {
		return g.do("GET", "/v3/domains/zones/"+url.PathEscape(zone)+"/dns-records?pageSize=1", nil, nil)
	}
	return g.do("GET", "/v1/domains/"+url.PathEscape(zone), nil, nil)
}

func (g *gdClient) List(zone, name string) ([]regRecord, error) {
	var out []regRecord
	for _, t := range []string{"A", "AAAA", "CNAME"} {
		if g.v3 {
			var r struct {
				Items []struct {
					RecordID string `json:"recordId"`
					Name     string `json:"name"`
					Type     string `json:"type"`
					Data     string `json:"data"`
				} `json:"items"`
			}
			if err := g.do("GET", "/v3/domains/zones/"+url.PathEscape(zone)+"/dns-records?pageSize=100&type="+t+"&name="+url.QueryEscape(name), nil, &r); err != nil {
				return nil, err
			}
			for _, x := range r.Items {
				out = append(out, regRecord{x.RecordID, x.Type, x.Name, x.Data})
			}
			continue
		}
		var r []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := g.do("GET", "/v1/domains/"+url.PathEscape(zone)+"/records/"+t+"/"+url.PathEscape(name), nil, &r); err != nil {
			return nil, err
		}
		for _, x := range r {
			out = append(out, regRecord{"", firstNonEmpty(x.Type, t), firstNonEmpty(x.Name, name), x.Data})
		}
	}
	return out, nil
}

func (g *gdClient) SetA(zone, name, ip string, existing []regRecord) error {
	if !g.v3 {
		return g.do("PUT", "/v1/domains/"+url.PathEscape(zone)+"/records/A/"+url.PathEscape(name), []map[string]any{{"data": ip, "ttl": 600}}, nil)
	}
	body := map[string]any{"name": name, "type": "A", "data": ip, "ttl": 600}
	if len(existing) == 0 {
		return g.do("POST", "/v3/domains/zones/"+url.PathEscape(zone)+"/dns-records", body, nil)
	}
	for _, x := range existing[1:] {
		if err := g.Delete(zone, x); err != nil {
			return err
		}
	}
	return g.do("PUT", "/v3/domains/zones/"+url.PathEscape(zone)+"/dns-records/"+url.PathEscape(existing[0].ID), body, nil)
}

func (g *gdClient) Delete(zone string, r regRecord) error {
	if g.v3 {
		return g.do("DELETE", "/v3/domains/zones/"+url.PathEscape(zone)+"/dns-records/"+url.PathEscape(r.ID), nil, nil)
	}
	return g.do("DELETE", "/v1/domains/"+url.PathEscape(zone)+"/records/"+r.Type+"/"+url.PathEscape(r.Name), nil, nil)
}

// SetNameservers switches the domain's nameservers (v3 PAT with the
// domains.nameserver:update scope only).
func (g *gdClient) SetNameservers(domain string, ns []string) error {
	if !g.v3 {
		return errors.New("switching nameservers needs a GoDaddy Personal Access Token (the classic key cannot do it)")
	}
	return g.do("PUT", "/v3/domains/domain-names/"+url.PathEscape(domain)+"/nameservers", ns, nil)
}

// ---------- Porkbun ----------

type pbClient struct {
	key, secret string
	http        *http.Client
}

func newPorkbun(c RegistrarCred) *pbClient {
	return &pbClient{key: secret(c.Key), secret: secret(c.Secret), http: &http.Client{Timeout: 20 * time.Second}}
}

func (p *pbClient) do(path string, fields map[string]any, out any) error {
	body := map[string]any{"apikey": p.key, "secretapikey": p.secret}
	for k, v := range fields {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", pbBase+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Dilreet/"+Version)
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach Porkbun: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal(rb, &env)
	if resp.StatusCode >= 300 || env.Status != "SUCCESS" {
		msg := firstNonEmpty(env.Message, env.Code, http.StatusText(resp.StatusCode))
		if strings.Contains(strings.ToLower(msg), "api access") || strings.Contains(strings.ToLower(msg), "not opted in") {
			msg += " (turn on API Access for this domain: Porkbun > Domain Management > the domain's Details > API Access)"
		}
		return &APIError{Status: resp.StatusCode, Body: "Porkbun: " + msg}
	}
	if out != nil {
		return json.Unmarshal(rb, out)
	}
	return nil
}

func (p *pbClient) Verify(zone string) error {
	if err := p.do("/ping", nil, nil); err != nil {
		return err
	}
	if zone == "" {
		return nil
	}
	_, err := p.List(zone, "@")
	return err
}

func pbSub(name string) string {
	if name == "@" {
		return ""
	}
	return name
}

func (p *pbClient) List(zone, name string) ([]regRecord, error) {
	var out []regRecord
	for _, t := range []string{"A", "AAAA", "CNAME", "ALIAS"} {
		var r struct {
			Records []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Type    string `json:"type"`
				Content string `json:"content"`
			} `json:"records"`
		}
		if err := p.do("/dns/retrieveByNameType/"+url.PathEscape(zone)+"/"+t+"/"+url.PathEscape(pbSub(name)), nil, &r); err != nil {
			return nil, err
		}
		for _, x := range r.Records {
			out = append(out, regRecord{x.ID, x.Type, name, x.Content})
		}
	}
	return out, nil
}

func (p *pbClient) SetA(zone, name, ip string, existing []regRecord) error {
	if len(existing) == 1 {
		return p.do("/dns/editByNameType/"+url.PathEscape(zone)+"/A/"+url.PathEscape(pbSub(name)), map[string]any{"content": ip, "ttl": "600"}, nil)
	}
	if len(existing) > 1 {
		if err := p.Delete(zone, regRecord{Type: "A", Name: name}); err != nil {
			return err
		}
	}
	return p.do("/dns/create/"+url.PathEscape(zone), map[string]any{"name": pbSub(name), "type": "A", "content": ip, "ttl": "600"}, nil)
}

func (p *pbClient) Delete(zone string, r regRecord) error {
	return p.do("/dns/deleteByNameType/"+url.PathEscape(zone)+"/"+r.Type+"/"+url.PathEscape(pbSub(r.Name)), nil, nil)
}

// ---------- shared upsert ----------

func registrarClient(kind string, c *Config) (registrarDNS, error) {
	cred := c.Registrars[kind]
	if cred == nil || cred.Key == "" {
		return nil, fmt.Errorf("connect your %s API key first", registrarLabel(kind))
	}
	switch kind {
	case "godaddy":
		return newGoDaddy(*cred), nil
	case "porkbun":
		return newPorkbun(*cred), nil
	}
	return nil, errors.New("unsupported registrar")
}

func registrarLabel(kind string) string {
	switch kind {
	case "godaddy":
		return "GoDaddy"
	case "porkbun":
		return "Porkbun"
	}
	return kind
}

// splitZone returns the registrable zone and the relative record name.
func splitZone(domain string) (zone, name string) {
	zone = domain
	if e, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		zone = e
	}
	name = "@"
	if domain != zone {
		name = strings.TrimSuffix(domain, "."+zone)
	}
	return zone, name
}

// upsertRegistrarA makes name resolve to ip. A "www" CNAME that points at the
// apex is fine and kept (it follows the apex A record).
func upsertRegistrarA(p registrarDNS, zone, name, ip string, replace bool) ([]CFConflict, error) {
	recs, err := p.List(zone, name)
	if err != nil {
		return nil, err
	}
	var aRecs, remove []regRecord
	var conflicts []CFConflict
	full := zone
	if name != "@" {
		full = name + "." + zone
	}
	for _, r := range recs {
		switch r.Type {
		case "A":
			aRecs = append(aRecs, r)
			if r.Data != ip {
				conflicts = append(conflicts, CFConflict{full, r.Type, r.Data})
			}
		case "CNAME":
			t := strings.TrimSuffix(strings.ToLower(r.Data), ".")
			if strings.HasPrefix(name, "www") && (t == "@" || t == zone) {
				return nil, nil // www follows the apex
			}
			conflicts = append(conflicts, CFConflict{full, r.Type, r.Data})
			remove = append(remove, r)
		default: // AAAA, ALIAS
			conflicts = append(conflicts, CFConflict{full, r.Type, r.Data})
			remove = append(remove, r)
		}
	}
	if len(aRecs) == 1 && aRecs[0].Data == ip && len(remove) == 0 {
		return nil, nil
	}
	if len(conflicts) > 0 && !replace {
		return conflicts, errCFConflict
	}
	seen := map[string]bool{}
	for _, r := range remove {
		k := r.Type + "|" + r.ID
		if seen[k] {
			continue
		}
		seen[k] = true
		if err := p.Delete(zone, r); err != nil {
			return nil, err
		}
	}
	return nil, p.SetA(zone, name, ip, aRecs)
}

// ---------- Cloudflare "fully automatic": move nameservers ----------

type cfZone struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	NameServers []string `json:"name_servers"`
}

func (c *cfClient) Accounts() ([]struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}, error) {
	var a []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	return a, c.do("GET", "/accounts?per_page=50", nil, &a)
}

// AddZone adds domain as a full (Cloudflare-hosted DNS) zone, or returns the
// existing one. POST /zones {name, account:{id}, type:"full"} -> name_servers.
func (c *cfClient) AddZone(domain, accountID string) (*cfZone, bool, error) {
	var zs []cfZone
	if err := c.do("GET", "/zones?name="+url.QueryEscape(domain), nil, &zs); err == nil {
		for i := range zs {
			if zs[i].Name == domain {
				return &zs[i], false, nil
			}
		}
	}
	if accountID == "" {
		acc, err := c.Accounts()
		if err != nil {
			return nil, false, err
		}
		if len(acc) == 0 {
			return nil, false, errors.New("this Cloudflare token cannot see any account. Create the token with the 'Zone: Zone: Edit' and 'Zone: DNS: Edit' permissions for 'All zones from an account'")
		}
		if len(acc) > 1 {
			return nil, false, fmt.Errorf("this token can use %d Cloudflare accounts; enter the Account ID to use (Cloudflare dashboard > Account home > Account ID)", len(acc))
		}
		accountID = acc[0].ID
	}
	var z cfZone
	if err := c.do("POST", "/zones", map[string]any{"name": domain, "account": map[string]string{"id": accountID}, "type": "full"}, &z); err != nil {
		return nil, false, err
	}
	return &z, true, nil
}

var lookupMX = func(ctx context.Context, d string) ([]*netMX, error) {
	mx, err := resolverFor(publicResolvers[0]).LookupMX(ctx, d)
	var out []*netMX
	for _, m := range mx {
		out = append(out, &netMX{Host: m.Host, Pref: m.Pref})
	}
	return out, err
}

type netMX struct {
	Host string
	Pref uint16
}

var lookupTXT = func(ctx context.Context, d string) ([]string, error) {
	return resolverFor(publicResolvers[0]).LookupTXT(ctx, d)
}

// copyMailRecords recreates the apex MX and TXT records that public DNS shows
// today, so email and verifications keep working after the nameserver move.
func copyMailRecords(cf *cfClient, zoneID, domain string) (copied []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	existing, _ := cf.Records(zoneID, domain)
	have := map[string]bool{}
	for _, r := range existing {
		have[r.Type+"|"+strings.Trim(r.Content, `"`)] = true
	}
	if mx, err := lookupMX(ctx, domain); err == nil {
		for _, m := range mx {
			h := strings.TrimSuffix(m.Host, ".")
			if h == "" || have["MX|"+h] {
				continue
			}
			if err := cf.do("POST", "/zones/"+zoneID+"/dns_records", map[string]any{"type": "MX", "name": domain, "content": h, "priority": m.Pref, "ttl": 1, "comment": "Copied by Dilreet"}, nil); err == nil {
				copied = append(copied, fmt.Sprintf("MX %d %s", m.Pref, h))
			}
		}
	}
	if txt, err := lookupTXT(ctx, domain); err == nil {
		for _, t := range txt {
			if have["TXT|"+t] {
				continue
			}
			if err := cf.do("POST", "/zones/"+zoneID+"/dns_records", map[string]any{"type": "TXT", "name": domain, "content": `"` + strings.ReplaceAll(t, `"`, `\"`) + `"`, "ttl": 1, "comment": "Copied by Dilreet"}, nil); err == nil {
				copied = append(copied, "TXT "+truncate(t, 60))
			}
		}
	}
	sort.Strings(copied)
	return copied
}

// ---------- guided steps per registrar ----------

type RegistrarGuide struct {
	DNSURL   string   `json:"dns_url,omitempty"` // per-domain DNS page
	NSURL    string   `json:"ns_url,omitempty"`  // where nameservers are changed
	Steps    []string `json:"steps"`
	NSSteps  []string `json:"ns_steps,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	API      string   `json:"api,omitempty"` // "godaddy" | "porkbun" when Dilreet can create records
	APINote  string   `json:"api_note,omitempty"`
	KeyURL   string   `json:"key_url,omitempty"`
	WWWCNAME bool     `json:"www_cname,omitempty"` // www is usually a CNAME to the apex here
}

func guideFor(id, domain string) *RegistrarGuide {
	zone, _ := splitZone(domain)
	q := url.QueryEscape(zone)
	p := url.PathEscape(zone)
	switch id {
	case "godaddy":
		return &RegistrarGuide{
			DNSURL: "https://dcc.godaddy.com/control/dnsmanagement?domainName=" + q,
			NSURL:  "https://dcc.godaddy.com/control/dnsmanagement?domainName=" + q + "&subtab=nameservers",
			Steps: []string{
				"Click **Open GoDaddy DNS** and sign in. You land on the DNS Records list for " + zone + ".",
				"Find the record of type **A** with name **@**. GoDaddy often shows its value as **Parked** or a Website Builder address. Click the pencil (Edit), paste the IP from the table below as the value, set TTL to the lowest option GoDaddy offers (1/2 hour), and Save.",
				"If there are other **A** or **AAAA** records named **@**, delete them (trash icon).",
				"For www: GoDaddy normally has a **CNAME** named **www** with value **@**. Keep it. If it points somewhere else, edit it to **@**.",
				"Dilreet checks automatically and notifies you when the domain is live (usually 5-30 minutes, sometimes up to a few hours).",
			},
			NSSteps: []string{
				"Click **Open GoDaddy nameservers** and sign in.",
				"Click **Change Nameservers**, choose **I'll use my own nameservers**.",
				"Delete the existing entries and paste the two Cloudflare nameservers shown below, one per box.",
				"Click **Save** and confirm. GoDaddy may ask you to confirm by email or 2-step code.",
			},
			Warnings: []string{
				"Domain **Forwarding** overrides your A record: in GoDaddy open the domain > Forwarding and delete any forwarding for the domain.",
				"If a GoDaddy **Website Builder / Airo** site is connected to this domain, disconnect it, otherwise GoDaddy may put the Parked/Website Builder records back.",
				"Do not touch the NS and SOA records, and keep MX/TXT records if you use email on this domain.",
			},
			API:      "godaddy",
			APINote:  "Optional: with a GoDaddy API key Dilreet creates the record itself. GoDaddy allows API access for accounts that own at least one domain (since April 2026; before that 10+ domains or Discount Domain Club). Use a Personal Access Token with the domains.dns:update scope, or a classic Production key + secret.",
			KeyURL:   "https://developer.godaddy.com/keys",
			WWWCNAME: true,
		}
	case "namecheap":
		return &RegistrarGuide{
			DNSURL: "https://ap.www.namecheap.com/Domains/DomainControlPanel/" + p + "/advancedns",
			NSURL:  "https://ap.www.namecheap.com/Domains/DomainControlPanel/" + p + "/domain",
			Steps: []string{
				"Click **Open Namecheap DNS** (Domain List > Manage > **Advanced DNS**) and sign in.",
				"Under Host Records delete the default **URL Redirect Record** for @ and the **CNAME www → parkingpage.namecheap.com**.",
				"Click **Add New Record** > **A Record**, Host **@**, Value = the IP below, TTL **5 min** (or Automatic). Click the green check to save.",
				"Add another **A Record** with Host **www** and the same IP (if you want www).",
				"Dilreet checks automatically and notifies you when the domain is live.",
			},
			NSSteps: []string{"Open the domain's **Domain** tab, under Nameservers choose **Custom DNS**, paste the two Cloudflare nameservers and click the green check."},
			Warnings: []string{"The DNS records only work while Nameservers is set to **Namecheap BasicDNS** (Domain tab).",
				"Namecheap's API needs a whitelisted IPv4 address, an eligible account (20+ domains or $50 balance/spend) and replaces the whole zone on every change, so Dilreet uses these guided steps instead."},
		}
	case "porkbun":
		return &RegistrarGuide{
			DNSURL: "https://porkbun.com/account/domainsSpeedy",
			Steps: []string{
				"Click **Open Porkbun** and sign in. Next to " + zone + " click **DNS**.",
				"Delete the default **ALIAS** record for the root (pixie.porkbun.com) and the wildcard **CNAME *** (uixie.porkbun.com) if present.",
				"Add a record: Type **A**, Host empty (root), Answer = the IP below, TTL 600. Add another with Host **www**.",
				"Dilreet checks automatically and notifies you when the domain is live.",
			},
			NSSteps:  []string{"Next to the domain click **Details** > **Authoritative Nameservers** > Edit, replace them with the two Cloudflare nameservers and save."},
			Warnings: []string{"Porkbun's URL forwarding for the domain overrides your records; remove it if set."},
			API:      "porkbun",
			APINote:  "Optional: with a Porkbun API key Dilreet creates the records itself. Create a key at porkbun.com/account/api and turn on **API Access** for this domain (Domain Management > Details).",
			KeyURL:   "https://porkbun.com/account/api",
		}
	case "hostinger":
		return &RegistrarGuide{
			DNSURL: "https://hpanel.hostinger.com/domain/" + p + "/dns",
			Steps: []string{
				"Click **Open Hostinger DNS** and sign in (or hPanel > Domains > your domain > **DNS / Nameservers**).",
				"In **Manage DNS records** edit the **A** record named **@** (or delete it and add a new one): Points to = the IP below, TTL 300.",
				"Delete any **AAAA** record for @. For www: if there is a **CNAME www** pointing to " + zone + " keep it, otherwise add an **A** record **www** with the same IP.",
				"Dilreet checks automatically and notifies you when the domain is live.",
			},
			NSSteps:  []string{"hPanel > Domains > your domain > **DNS / Nameservers** > **Change Nameservers**, choose custom, paste the two Cloudflare nameservers, Save."},
			Warnings: []string{"If the domain is connected to a Hostinger website or parking page, Hostinger may recreate its own A record; disconnect it first."},
			WWWCNAME: true,
		}
	case "squarespace":
		return &RegistrarGuide{
			DNSURL: "https://account.squarespace.com/domains/managed/" + p,
			Steps: []string{
				"Click **Open Squarespace Domains**, sign in, then **DNS** > **DNS Settings**.",
				"Under **Squarespace Defaults** (or Google Workspace / website presets) click the trash icon to remove the preset A and CNAME records for @ and www.",
				"Under **Custom Records** click **Add record**: Type **A**, Host **@**, Data = the IP below. Add another with Host **www**. Save.",
				"Dilreet checks automatically and notifies you when the domain is live.",
			},
			NSSteps:  []string{"Domains > your domain > **DNS** > **Domain Nameservers** > **Use custom nameservers**, paste the two Cloudflare nameservers, Save."},
			Warnings: []string{"Former Google Domains names now live at Squarespace; if Google Workspace email is used keep its MX records."},
		}
	case "cloudflare":
		return &RegistrarGuide{DNSURL: "https://dash.cloudflare.com", Steps: []string{"Connect Cloudflare below and Dilreet adds the records for you."}}
	}
	return nil
}

// ---------- routes ----------

func registrarPublic(c *Config) map[string]any {
	out := map[string]any{}
	for k, v := range c.Registrars {
		out[k] = map[string]any{"hint": v.Hint, "mode": v.Mode}
	}
	return out
}

func registerRegistrarRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/registrars", func(w http.ResponseWriter, r *http.Request) {
		c := snapshotConfig()
		writeJSON(w, 200, map[string]any{"connected": registrarPublic(&c)})
	})
	// Save (and test against the domain when given) a registrar key.
	mux.HandleFunc("POST /api/registrars/{kind}", func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("kind")
		if kind != "godaddy" && kind != "porkbun" {
			writeErr(w, errors.New("Dilreet can use the GoDaddy and Porkbun APIs; other registrars use the guided steps"))
			return
		}
		var req struct {
			Key    string `json:"key"`
			Secret string `json:"secret"`
			Domain string `json:"domain"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		req.Key, req.Secret = strings.TrimSpace(req.Key), strings.TrimSpace(req.Secret)
		if req.Key == "" {
			writeErr(w, errors.New("paste the API key"))
			return
		}
		mode := "key"
		if kind == "godaddy" && req.Secret == "" {
			mode = "pat"
		}
		if kind == "porkbun" && req.Secret == "" {
			writeErr(w, errors.New("Porkbun needs both the API key (pk1_…) and the secret key (sk1_…)"))
			return
		}
		pk, err := protectString(req.Key)
		if err != nil {
			writeErr(w, err)
			return
		}
		ps := ""
		if req.Secret != "" {
			if ps, err = protectString(req.Secret); err != nil {
				writeErr(w, err)
				return
			}
		}
		cred := RegistrarCred{Key: pk, Secret: ps, Mode: mode, Hint: keyHint(req.Key)}
		zone := ""
		if req.Domain != "" {
			d, err := normalizeDomain(req.Domain)
			if err != nil {
				writeErr(w, err)
				return
			}
			zone, _ = splitZone(d)
		}
		var cl registrarDNS
		if kind == "godaddy" {
			cl = newGoDaddy(cred)
		} else {
			cl = newPorkbun(cred)
		}
		if zone != "" || kind == "porkbun" {
			if err := cl.Verify(zone); err != nil {
				writeErr(w, err)
				return
			}
		}
		_ = withConfig(func(c *Config) error {
			if c.Registrars == nil {
				c.Registrars = map[string]*RegistrarCred{}
			}
			c.Registrars[kind] = &cred
			return nil
		})
		writeJSON(w, 200, map[string]any{"ok": true, "hint": cred.Hint, "mode": mode, "tested": zone != "" || kind == "porkbun"})
	})
	mux.HandleFunc("DELETE /api/registrars/{kind}", func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("kind")
		_ = withConfig(func(c *Config) error {
			delete(c.Registrars, kind)
			return nil
		})
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	// Create the A records at the registrar.
	mux.HandleFunc("POST /api/sites/{slug}/registrar-dns", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		var req struct {
			Provider string `json:"provider"`
			Domain   string `json:"domain"`
			WWW      bool   `json:"www"`
			Replace  bool   `json:"replace"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		c := snapshotConfig()
		if c.ServerIP == "" {
			writeErr(w, errors.New("set up your server first"))
			return
		}
		if _, err := siteOr404(slug); err != nil {
			writeErr(w, err)
			return
		}
		d, err := normalizeDomain(req.Domain)
		if err != nil {
			writeErr(w, err)
			return
		}
		if strings.HasPrefix(d, "www.") {
			d, req.WWW = strings.TrimPrefix(d, "www."), true
		}
		for _, s := range c.Sites {
			if s.Slug != slug && s.Domain == d {
				writeErr(w, fmt.Errorf("%s is already used by project %q", d, s.Name))
				return
			}
		}
		cl, err := registrarClient(req.Provider, &c)
		if err != nil {
			writeErr(w, err)
			return
		}
		zone, name := splitZone(d)
		names := []string{name}
		if req.WWW {
			if name == "@" {
				names = append(names, "www")
			} else {
				names = append(names, "www."+name)
			}
		}
		var conflicts []CFConflict
		for _, n := range names {
			cfl, err := upsertRegistrarA(cl, zone, n, c.ServerIP, req.Replace)
			if errors.Is(err, errCFConflict) {
				conflicts = append(conflicts, cfl...)
				continue
			}
			if err != nil {
				writeErr(w, err)
				return
			}
		}
		if len(conflicts) > 0 {
			writeJSON(w, 409, map[string]any{"error": "existing DNS records would be replaced", "conflicts": conflicts})
			return
		}
		_ = withConfig(func(c *Config) error {
			if s := findSite(c, slug); s != nil {
				s.Domain, s.IncludeWWW, s.DomainLive, s.HTTPSActive = d, req.WWW, false, false
				s.DNSMode, s.DNSProvider, s.CFZoneID, s.CFRecordIDs = "registrar", req.Provider, "", nil
			}
			return nil
		})
		bg.mu.Lock()
		delete(bg.lastDomain, slug)
		bg.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "zone": zone, "records": len(names)})
	})
	// Fully automatic: host DNS at Cloudflare. Adds the zone, copies apex MX/TXT
	// from public DNS, creates the A records and returns the 2 nameservers to
	// paste at the registrar.
	mux.HandleFunc("POST /api/sites/{slug}/cloudflare-move", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		var req struct {
			Domain    string `json:"domain"`
			WWW       bool   `json:"www"`
			AccountID string `json:"account_id"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		c := snapshotConfig()
		if c.CloudflareToken == "" {
			writeErr(w, errors.New("connect Cloudflare first"))
			return
		}
		if c.ServerIP == "" {
			writeErr(w, errors.New("set up your server first"))
			return
		}
		if _, err := siteOr404(slug); err != nil {
			writeErr(w, err)
			return
		}
		d, err := normalizeDomain(req.Domain)
		if err != nil {
			writeErr(w, err)
			return
		}
		if strings.HasPrefix(d, "www.") {
			d, req.WWW = strings.TrimPrefix(d, "www."), true
		}
		zoneName, _ := splitZone(d)
		cf := newCF(secret(c.CloudflareToken))
		z, created, err := cf.AddZone(zoneName, strings.TrimSpace(req.AccountID))
		if err != nil {
			writeErr(w, err)
			return
		}
		var copied []string
		if created {
			copied = copyMailRecords(cf, z.ID, zoneName)
		}
		hosts := []string{d}
		if req.WWW {
			hosts = append(hosts, "www."+d)
		}
		var ids []string
		for _, h := range hosts {
			id, _, err := cf.UpsertA(z.ID, h, c.ServerIP, true)
			if err != nil {
				writeErr(w, err)
				return
			}
			ids = append(ids, id)
		}
		_ = withConfig(func(c *Config) error {
			if s := findSite(c, slug); s != nil {
				s.Domain, s.IncludeWWW, s.DomainLive, s.HTTPSActive = d, req.WWW, false, false
				s.DNSMode, s.DNSProvider, s.CFZoneID, s.CFRecordIDs = "cloudflare", "", z.ID, ids
				s.PendingNS = z.NameServers
			}
			return nil
		})
		host := detectDNSHost(zoneName)
		_, canSwitch := c.Registrars["godaddy"]
		canSwitch = canSwitch && host.ID == "godaddy" && c.Registrars["godaddy"].Mode == "pat"
		writeJSON(w, 200, map[string]any{"ok": true, "zone": z.Name, "status": z.Status, "created": created, "nameservers": z.NameServers, "copied": copied, "registrar": host, "guide": guideFor(host.ID, zoneName), "can_switch": canSwitch})
	})
	// Switch nameservers at GoDaddy (PAT with domains.nameserver:update).
	mux.HandleFunc("POST /api/sites/{slug}/switch-nameservers", func(w http.ResponseWriter, r *http.Request) {
		s, err := siteOr404(r.PathValue("slug"))
		if err != nil {
			writeErr(w, err)
			return
		}
		c := snapshotConfig()
		cred := c.Registrars["godaddy"]
		if cred == nil {
			writeErr(w, errors.New("connect a GoDaddy Personal Access Token first"))
			return
		}
		if len(s.PendingNS) < 2 {
			writeErr(w, errors.New("add the domain to Cloudflare first"))
			return
		}
		zone, _ := splitZone(s.Domain)
		if err := newGoDaddy(*cred).SetNameservers(zone, s.PendingNS); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
}
