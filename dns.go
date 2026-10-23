package main

import (
	"context"
	"errors"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

var publicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

var hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// normalizeDomain accepts "https://Example.com/path", "www.example.com" etc.
func normalizeDomain(in string) (string, error) {
	d := strings.TrimSpace(strings.ToLower(in))
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	d = strings.TrimSuffix(d, ".")
	if a, err := idna.Lookup.ToASCII(d); err == nil {
		d = a
	}
	if !hostRe.MatchString(d) {
		return "", errors.New("that does not look like a domain name (example: example.com or shop.example.com)")
	}
	return d, nil
}

// isApex is a heuristic: "example.com" or "example.co.uk" style names.
func isApex(d string) bool {
	parts := strings.Split(d, ".")
	if len(parts) <= 2 {
		return true
	}
	if len(parts) == 3 && len(parts[1]) <= 3 && len(parts[2]) == 2 { // e.g. example.co.uk
		return true
	}
	return false
}

type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`  // what to type in the "Name/Host" field
	FQDN  string `json:"fqdn"`  // full host name
	Value string `json:"value"` // what to type in the "Value/Points to" field
	TTL   string `json:"ttl"`
}

func requiredRecords(domain string, www bool, ip string) []DNSRecord {
	var recs []DNSRecord
	name := "@"
	if !isApex(domain) {
		name = strings.SplitN(domain, ".", 2)[0]
	}
	recs = append(recs, DNSRecord{Type: "A", Name: name, FQDN: domain, Value: ip, TTL: "300 (or Auto)"})
	if www {
		wname := "www"
		if name != "@" {
			wname = "www." + name
		}
		recs = append(recs, DNSRecord{Type: "A", Name: wname, FQDN: "www." + domain, Value: ip, TTL: "300 (or Auto)"})
	}
	return recs
}

type HostCheck struct {
	Host    string              `json:"host"`
	OK      bool                `json:"ok"`
	Found   map[string][]string `json:"found"` // resolver -> A records
	AAAA    []string            `json:"aaaa,omitempty"`
	Message string              `json:"message"`
}

func resolverFor(server string) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 4 * time.Second}
		return d.DialContext(ctx, network, server)
	}}
}

func checkHost(host, wantIP string) HostCheck {
	hc := HostCheck{Host: host, Found: map[string][]string{}}
	allOK := true
	for _, srv := range publicResolvers {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		ips, err := resolverFor(srv).LookupIP(ctx, "ip4", host)
		cancel()
		var got []string
		for _, ip := range ips {
			got = append(got, ip.String())
		}
		sort.Strings(got)
		hc.Found[srv] = got
		if err != nil || len(got) != 1 || got[0] != wantIP {
			allOK = false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	if ips, err := resolverFor(publicResolvers[0]).LookupIP(ctx, "ip6", host); err == nil {
		for _, ip := range ips {
			hc.AAAA = append(hc.AAAA, ip.String())
		}
	}
	cancel()
	hc.OK = allOK
	switch {
	case allOK && len(hc.AAAA) > 0:
		hc.Message = "A record is correct, but there is also an AAAA (IPv6) record. Delete it unless it points to this server, otherwise HTTPS certificates may fail."
	case allOK:
		hc.Message = "Correct: points to your server."
	default:
		found := map[string]bool{}
		for _, v := range hc.Found {
			for _, ip := range v {
				found[ip] = true
			}
		}
		if len(found) == 0 {
			hc.Message = "No A record found yet. Add it at your domain provider; changes usually appear within minutes, sometimes up to a few hours."
		} else if found[wantIP] {
			hc.Message = "Partly there: some resolvers still show old values, or there are extra A records. Remove A records that point to other IPs and wait a little."
		} else {
			hc.Message = "Points somewhere else. If you use Cloudflare, set the record to 'DNS only' (grey cloud). Otherwise edit the A record value to your server IP."
		}
	}
	return hc
}
