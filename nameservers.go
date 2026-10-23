package main

// Registrar / DNS host detection from a domain's NS records, so the domain
// wizard can say "Your DNS is at Namecheap - open Namecheap > Advanced DNS"
// instead of generic instructions.

import (
	"context"
	"net"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

type DNSHost struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	URL         string   `json:"url,omitempty"`
	Steps       string   `json:"steps,omitempty"`
	Nameservers []string `json:"nameservers"`
	Auto        bool     `json:"auto"` // Dilreet can create the records itself
}

var dnsHosts = []struct {
	match []string
	DNSHost
}{
	{[]string{"ns.cloudflare.com"}, DNSHost{ID: "cloudflare", Name: "Cloudflare", URL: "https://dash.cloudflare.com", Steps: "Connect Cloudflare below and Dilreet adds the records for you.", Auto: true}},
	{[]string{"domaincontrol.com"}, DNSHost{ID: "godaddy", Name: "GoDaddy", URL: "https://dcc.godaddy.com/control/portfolio", Steps: "My Products > your domain > DNS > Add New Record."}},
	{[]string{"registrar-servers.com"}, DNSHost{ID: "namecheap", Name: "Namecheap", URL: "https://ap.www.namecheap.com/domains/list/", Steps: "Domain List > Manage > Advanced DNS > Add New Record."}},
	{[]string{"porkbun.com"}, DNSHost{ID: "porkbun", Name: "Porkbun", URL: "https://porkbun.com/account/domainsSpeedy", Steps: "Domain Management > DNS (next to your domain)."}},
	{[]string{"googledomains.com", "squarespacedns.com"}, DNSHost{ID: "squarespace", Name: "Squarespace Domains", URL: "https://account.squarespace.com/domains", Steps: "Domains > your domain > DNS > Custom records."}},
	{[]string{"awsdns-"}, DNSHost{ID: "route53", Name: "Amazon Route 53", URL: "https://console.aws.amazon.com/route53/v2/hostedzones", Steps: "Hosted zones > your domain > Create record."}},
	{[]string{"digitalocean.com"}, DNSHost{ID: "digitalocean", Name: "DigitalOcean DNS", URL: "https://cloud.digitalocean.com/networking/domains", Steps: "Networking > Domains > your domain."}},
	{[]string{"hetzner.com", "hetzner.de", "your-server.de", "hetzner.cloud"}, DNSHost{ID: "hetzner", Name: "Hetzner DNS", URL: "https://console.hetzner.cloud", Steps: "DNS > your zone > Add record."}},
	{[]string{"nsone.net"}, DNSHost{ID: "netlify", Name: "Netlify DNS (NS1)", URL: "https://app.netlify.com/teams/", Steps: "Team > Domains > your domain > Add new record (or switch nameservers back to your registrar)."}},
	{[]string{"vercel-dns.com"}, DNSHost{ID: "vercel", Name: "Vercel DNS", URL: "https://vercel.com/dashboard/domains", Steps: "Domains > your domain > DNS Records."}},
	{[]string{"name.com"}, DNSHost{ID: "namecom", Name: "Name.com", URL: "https://www.name.com/account/domain", Steps: "My Domains > your domain > Manage DNS Records."}},
	{[]string{"hover.com"}, DNSHost{ID: "hover", Name: "Hover", URL: "https://www.hover.com/control_panel", Steps: "your domain > DNS > Add a record."}},
	{[]string{"ui-dns.", "ionos"}, DNSHost{ID: "ionos", Name: "IONOS", URL: "https://my.ionos.com/domains", Steps: "Domains & SSL > your domain > DNS."}},
	{[]string{"ovh.net", "ovh.ca"}, DNSHost{ID: "ovh", Name: "OVHcloud", URL: "https://www.ovh.com/manager/", Steps: "Web Cloud > Domain names > your domain > DNS zone."}},
	{[]string{"gandi.net"}, DNSHost{ID: "gandi", Name: "Gandi", URL: "https://admin.gandi.net/domain/", Steps: "Domain > your domain > DNS records."}},
	{[]string{"dynadot.com"}, DNSHost{ID: "dynadot", Name: "Dynadot", URL: "https://www.dynadot.com/account/domain/name/list.html", Steps: "My Domains > DNS Settings."}},
	{[]string{"wixdns.net"}, DNSHost{ID: "wix", Name: "Wix", URL: "https://manage.wix.com/account/domains", Steps: "Domains > your domain > Manage DNS records."}},
	{[]string{"azure-dns."}, DNSHost{ID: "azure", Name: "Azure DNS", URL: "https://portal.azure.com/#browse/Microsoft.Network%2FdnsZones", Steps: "DNS zones > your zone > + Record set."}},
	{[]string{"hostinger"}, DNSHost{ID: "hostinger", Name: "Hostinger", URL: "https://hpanel.hostinger.com/domains", Steps: "Domains > your domain > DNS / Nameservers."}},
}

var lookupNS = func(ctx context.Context, domain string) ([]*net.NS, error) {
	return resolverFor(publicResolvers[0]).LookupNS(ctx, domain)
}

// detectDNSHost finds who serves DNS for the registrable part of domain.
func detectDNSHost(domain string) DNSHost {
	apex := domain
	if e, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		apex = e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ns, err := lookupNS(ctx, apex)
	out := DNSHost{ID: "unknown", Name: "your domain provider", Steps: "Sign in where you bought the domain and open its DNS settings."}
	if err != nil || len(ns) == 0 {
		return out
	}
	for _, n := range ns {
		out.Nameservers = append(out.Nameservers, strings.TrimSuffix(strings.ToLower(n.Host), "."))
	}
	for _, h := range dnsHosts {
		for _, m := range h.match {
			for _, n := range out.Nameservers {
				if strings.Contains(n, m) {
					r := h.DNSHost
					r.Nameservers = out.Nameservers
					return r
				}
			}
		}
	}
	return out
}
