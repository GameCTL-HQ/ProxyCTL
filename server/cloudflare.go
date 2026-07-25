package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// CF is a tiny Cloudflare DNS client. SECURITY MODEL (same as the rest of
// proxyctl): it holds NO stored credential. The API token is read ONCE
// from the process environment of whoever launched proxyctl
// (CF_API_TOKEN or CLOUDFLARE_API_TOKEN) and is never persisted, never
// logged, and never sent to the browser. If no token is present every
// method degrades gracefully (Configured()==false) so the UI can disable
// the feature with a clear hint instead of erroring.
//
// Scope of use: listing A records (read-only) populates the DNS-name
// dropdown with the operator's REAL subdomains; creating/updating a
// record is an explicit, confirmed operator action — game records are
// always written grey-cloud (proxied=false) since the orange-cloud HTTP
// proxy cannot carry game UDP/TCP.
type CF struct {
	token  string
	http   *http.Client
	mu     sync.Mutex
	zoneID map[string]string // domain -> zone id (cached)
	acctID string            // Cloudflare account id (cached; tunnel API is account-scoped)
}

func NewCF() *CF {
	t := strings.TrimSpace(os.Getenv("CF_API_TOKEN"))
	if t == "" {
		t = strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN"))
	}
	return &CF{token: t, http: &http.Client{Timeout: 15 * time.Second}, zoneID: map[string]string{}}
}

func (c *CF) Configured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token != ""
}

// SetToken swaps the live token at runtime (used by the setup wizard).
// Also invalidates the zone-ID + account-ID caches so the next request
// re-resolves against the new token's scope. Empty token deconfigures.
func (c *CF) SetToken(t string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = strings.TrimSpace(t)
	c.zoneID = map[string]string{}
	c.acctID = ""
}

// VerifyResult mirrors the GET /user/tokens/verify response — used by
// the wizard's "Save & test" step to prove the token actually works
// before we persist it. status="active" is the green case.
type VerifyResult struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	ExpiresOn string `json:"expires_on,omitempty"`
	NotBefore string `json:"not_before,omitempty"`
}

// Verify hits /user/tokens/verify with the current token. It returns
// the token's status (active / disabled / expired) without leaking the
// token itself anywhere. Used to validate a freshly-pasted token before
// persisting it, and as the wizard's "Test" button.
func (c *CF) Verify(ctx context.Context) (*VerifyResult, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("no token set")
	}
	raw, err := c.do(ctx, "GET", "/user/tokens/verify", nil)
	if err != nil {
		return nil, err
	}
	var v VerifyResult
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ZoneSummary is the slim view of one accessible zone for the wizard
// to show the operator which domains the token controls.
type ZoneSummary struct {
	Name string `json:"name"`
}

// AccessibleZones lists every zone the current token can see. Empty
// list with no error means the token is valid but scoped to no zones
// (a misconfigured token — Edit-DNS without Zone:Read).
func (c *CF) AccessibleZones(ctx context.Context) ([]ZoneSummary, error) {
	raw, err := c.do(ctx, "GET", "/zones?per_page=50", nil)
	if err != nil {
		return nil, err
	}
	var zs []ZoneSummary
	if err := json.Unmarshal(raw, &zs); err != nil {
		return nil, err
	}
	return zs, nil
}

const cfBase = "https://api.cloudflare.com/client/v4"

// cfResp is the standard Cloudflare envelope.
type cfResp struct {
	Success bool              `json:"success"`
	Errors  []json.RawMessage `json:"errors"`
	Result  json.RawMessage   `json:"result"`
}

func (c *CF) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfBase+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var env cfResp
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("cloudflare: bad response (HTTP %d)", resp.StatusCode)
	}
	if !env.Success {
		msg := "request failed"
		if len(env.Errors) > 0 {
			msg = string(env.Errors[0])
		}
		return nil, fmt.Errorf("cloudflare: %s", msg)
	}
	return env.Result, nil
}

// zoneLookup asks Cloudflare for the zone id of EXACTLY this domain.
// A token that simply can't see such a zone returns ("", nil) — err is
// reserved for transport/auth failures, so callers walking candidate
// names can tell "keep trying" from "stop, the token is broken".
func (c *CF) zoneLookup(ctx context.Context, domain string) (string, error) {
	domain = normalizeDomain(domain)
	c.mu.Lock()
	if id, ok := c.zoneID[domain]; ok {
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()
	raw, err := c.do(ctx, "GET", "/zones?name="+url.QueryEscape(domain)+"&status=active", nil)
	if err != nil {
		return "", err
	}
	var zs []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &zs); err != nil || len(zs) == 0 {
		return "", nil
	}
	c.mu.Lock()
	c.zoneID[domain] = zs[0].ID
	c.mu.Unlock()
	return zs[0].ID, nil
}

func (c *CF) zone(ctx context.Context, domain string) (string, error) {
	id, err := c.zoneLookup(ctx, domain)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("cloudflare: zone %q not found on this token", domain)
	}
	return id, nil
}

// zoneCandidates lists the domains that could own fqdn, most specific
// first: "a.b.example.com" → [a.b.example.com b.example.com example.com].
// The fqdn ITSELF is a candidate — an apex route like "example.com" IS
// its own zone. (The old strip-one-label logic sent apex hostnames off
// looking for a zone named "com"/"cc", and broke multi-level subdomains
// the same way.) Single-label names yield nothing: not FQDNs.
func zoneCandidates(fqdn string) []string {
	labels := strings.Split(normalizeDomain(fqdn), ".")
	var out []string
	for i := 0; i+2 <= len(labels); i++ {
		out = append(out, strings.Join(labels[i:], "."))
	}
	return out
}

// zoneForHost resolves the zone that owns fqdn by trying each candidate
// suffix in order. Real API errors abort immediately (walking on would
// mask an auth problem as "zone not found").
func (c *CF) zoneForHost(ctx context.Context, fqdn string) (string, error) {
	cands := zoneCandidates(fqdn)
	if len(cands) == 0 {
		return "", fmt.Errorf("%q is not a fully-qualified name", fqdn)
	}
	for _, cand := range cands {
		id, err := c.zoneLookup(ctx, cand)
		if err != nil {
			return "", err
		}
		if id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("cloudflare: no zone on this token owns %q (tried %s)", fqdn, strings.Join(cands, ", "))
}

// CFRecord is the subset of a DNS record the UI cares about.
type CFRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

// ListA returns the A records in a domain's zone (for the DNS-name picker).
func (c *CF) ListA(ctx context.Context, domain string) ([]CFRecord, error) {
	z, err := c.zone(ctx, domain)
	if err != nil {
		return nil, err
	}
	raw, err := c.do(ctx, "GET", "/zones/"+z+"/dns_records?type=A&per_page=200", nil)
	if err != nil {
		return nil, err
	}
	var recs []CFRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		return nil, fmt.Errorf("cloudflare: unexpected records payload")
	}
	return recs, nil
}

// DeleteA removes the A record for fqdn (idempotent — "not found" is a
// successful no-op). Used when an entry is deleted from the wizard and
// the operator confirms they also want the DNS record gone.
func (c *CF) DeleteA(ctx context.Context, fqdn string) (deleted bool, err error) {
	fqdn = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(fqdn)), ".")
	z, err := c.zoneForHost(ctx, fqdn)
	if err != nil {
		return false, err
	}
	raw, err := c.do(ctx, "GET", "/zones/"+z+"/dns_records?type=A&name="+url.QueryEscape(fqdn), nil)
	if err != nil {
		return false, err
	}
	var recs []CFRecord
	json.Unmarshal(raw, &recs)
	if len(recs) == 0 {
		return false, nil // already gone
	}
	if _, err = c.do(ctx, "DELETE", "/zones/"+z+"/dns_records/"+recs[0].ID, nil); err != nil {
		return false, err
	}
	return true, nil
}

// UpsertA creates or updates an A record for fqdn -> ip. Game records are
// always grey-cloud (proxied=false). Returns the action taken.
func (c *CF) UpsertA(ctx context.Context, fqdn, ip string) (action string, rec CFRecord, err error) {
	fqdn = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(fqdn)), ".")
	z, err := c.zoneForHost(ctx, fqdn)
	if err != nil {
		return "", rec, err
	}
	raw, err := c.do(ctx, "GET", "/zones/"+z+"/dns_records?type=A&name="+url.QueryEscape(fqdn), nil)
	if err != nil {
		return "", rec, err
	}
	var existing []CFRecord
	json.Unmarshal(raw, &existing)
	payload := map[string]any{"type": "A", "name": fqdn, "content": ip, "proxied": false, "ttl": 1}
	if len(existing) > 0 {
		raw, err = c.do(ctx, "PUT", "/zones/"+z+"/dns_records/"+existing[0].ID, payload)
		action = "updated"
	} else {
		raw, err = c.do(ctx, "POST", "/zones/"+z+"/dns_records", payload)
		action = "created"
	}
	if err != nil {
		return "", rec, err
	}
	json.Unmarshal(raw, &rec)
	return action, rec, nil
}
