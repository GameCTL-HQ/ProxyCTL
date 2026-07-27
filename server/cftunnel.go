package main

// Cloudflare Tunnel client — the web-app data path.
//
// A web app is exposed by: a Cloudflare Tunnel (one per ProxyCTL
// install, "proxyctl") whose ingress rules map public hostname →
// in-cluster Service, an in-cluster `cloudflared` connector pod that
// runs the tunnel, and a proxied CNAME per hostname pointing at the
// tunnel. Cloudflare's edge terminates TLS and runs WAF/DDoS; no public
// IP, no droplet, no certs on our side.
//
// Spoofing is structurally impossible: cloudflared only routes the exact
// hostnames in the ingress list ProxyCTL sets, each to one Service. An
// unknown Host hits the trailing http_status:404 rule.
//
// These methods need the token to carry `Account › Cloudflare Tunnel ›
// Edit` in addition to the existing `Zone › DNS › Edit`.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const cfTunnelName = "proxyctl"

// TunnelIngress is one routing rule: a public hostname → an in-cluster
// service URL (or, for the trailing catch-all, just a service).
type TunnelIngress struct {
	Hostname string `json:"hostname,omitempty"`
	Service  string `json:"service"`
}

// accountID resolves (and caches) the Cloudflare account id. The tunnel
// API is account-scoped.
//
// GET /accounts is tried first, but an API token scoped to a zone +
// the Cloudflare Tunnel permission frequently can't *list* accounts and
// gets an empty result back — so we fall back to reading the account id
// off a zone object, which every token with zone access can see.
func (c *CF) accountID(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.acctID != "" {
		id := c.acctID
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	setAcct := func(id string) (string, error) {
		c.mu.Lock()
		c.acctID = id
		c.mu.Unlock()
		return id, nil
	}

	// 1. GET /accounts (works for tokens that can list accounts).
	if raw, err := c.do(ctx, "GET", "/accounts?per_page=50", nil); err == nil {
		var accs []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &accs) == nil && len(accs) > 0 && accs[0].ID != "" {
			return setAcct(accs[0].ID)
		}
	}

	// 2. Fallback: every zone object carries its owning account's id.
	if raw, err := c.do(ctx, "GET", "/zones?per_page=1", nil); err == nil {
		var zs []struct {
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		}
		if json.Unmarshal(raw, &zs) == nil && len(zs) > 0 && zs[0].Account.ID != "" {
			return setAcct(zs[0].Account.ID)
		}
	}

	return "", fmt.Errorf("cloudflare: couldn't resolve the account id — the token " +
		"needs 'Account › Cloudflare Tunnel › Edit' plus at least one zone under " +
		"'Zone › DNS › Edit'")
}

// TunnelInfo identifies the ProxyCTL tunnel.
type TunnelInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	AcctID string `json:"-"`
	CNAME  string `json:"-"` // <id>.cfargotunnel.com — DNS target for routes
}

// EnsureTunnel finds the "proxyctl" tunnel or creates it (remotely-
// managed, so ProxyCTL sets the ingress config via the API).
func (c *CF) EnsureTunnel(ctx context.Context) (TunnelInfo, error) {
	var ti TunnelInfo
	acct, err := c.accountID(ctx)
	if err != nil {
		return ti, err
	}
	ti.AcctID = acct

	// Look for an existing, non-deleted tunnel by name.
	raw, err := c.do(ctx, "GET",
		"/accounts/"+acct+"/cfd_tunnel?name="+cfTunnelName+"&is_deleted=false", nil)
	if err != nil {
		return ti, err
	}
	var found []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	json.Unmarshal(raw, &found)
	for _, t := range found {
		if t.Name == cfTunnelName {
			ti.ID, ti.Name = t.ID, t.Name
			ti.CNAME = t.ID + ".cfargotunnel.com"
			return ti, nil
		}
	}

	// Create it. config_src=cloudflare → ingress is managed via the API.
	raw, err = c.do(ctx, "POST", "/accounts/"+acct+"/cfd_tunnel", map[string]any{
		"name":       cfTunnelName,
		"config_src": "cloudflare",
	})
	if err != nil {
		return ti, err
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		return ti, fmt.Errorf("cloudflare: tunnel create returned no id")
	}
	ti.ID, ti.Name = created.ID, created.Name
	ti.CNAME = created.ID + ".cfargotunnel.com"
	return ti, nil
}

// TunnelToken returns the connector token the cloudflared pod runs with
// (`cloudflared tunnel run --token <token>`).
func (c *CF) TunnelToken(ctx context.Context, ti TunnelInfo) (string, error) {
	raw, err := c.do(ctx, "GET",
		"/accounts/"+ti.AcctID+"/cfd_tunnel/"+ti.ID+"/token", nil)
	if err != nil {
		return "", err
	}
	var tok string
	if err := json.Unmarshal(raw, &tok); err != nil || tok == "" {
		return "", fmt.Errorf("cloudflare: tunnel token response was empty")
	}
	return tok, nil
}

// SetTunnelConfig replaces the tunnel's ingress rules. A trailing
// catch-all (http_status:404) is appended so an unmatched Host can't
// fall through to anything.
func (c *CF) SetTunnelConfig(ctx context.Context, ti TunnelInfo, rules []TunnelIngress) error {
	ingress := append([]TunnelIngress{}, rules...)
	ingress = append(ingress, TunnelIngress{Service: "http_status:404"})
	_, err := c.do(ctx, "PUT",
		"/accounts/"+ti.AcctID+"/cfd_tunnel/"+ti.ID+"/configurations",
		map[string]any{"config": map[string]any{"ingress": ingress}})
	return err
}

// UpsertCNAME creates/updates a proxied CNAME fqdn → target. Tunnel
// routes require the record be proxied (orange-cloud). Any pre-existing
// A record for the same name is removed first (a name can't hold both).
func (c *CF) UpsertCNAME(ctx context.Context, fqdn, target string) (action string, err error) {
	fqdn = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(fqdn)), ".")
	// zoneForHost handles the apex case: routing "example.com" itself
	// through the tunnel is legal — Cloudflare flattens the apex CNAME.
	z, err := c.zoneForHost(ctx, fqdn)
	if err != nil {
		return "", err
	}
	raw, err := c.do(ctx, "GET", "/zones/"+z+"/dns_records?name="+fqdn, nil)
	if err != nil {
		return "", err
	}
	var existing []CFRecord
	json.Unmarshal(raw, &existing)
	payload := map[string]any{"type": "CNAME", "name": fqdn, "content": target, "proxied": true, "ttl": 1}
	var cnameID string
	for _, r := range existing {
		switch r.Type {
		case "CNAME":
			cnameID = r.ID
		default: // a stale A/AAAA on this name would conflict — drop it
			c.do(ctx, "DELETE", "/zones/"+z+"/dns_records/"+r.ID, nil)
		}
	}
	if cnameID != "" {
		_, err = c.do(ctx, "PUT", "/zones/"+z+"/dns_records/"+cnameID, payload)
		return "updated", err
	}
	_, err = c.do(ctx, "POST", "/zones/"+z+"/dns_records", payload)
	return "created", err
}
