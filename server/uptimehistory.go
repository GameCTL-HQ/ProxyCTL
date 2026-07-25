package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// External reachability history — ProxyCTL's own uptime sparklines. This is
// the ONLY app that ever checks these external addresses; GameCTL never
// re-probes them (see the design note in the entry/webroute UI — GameCTL
// reads this data back over the existing admin link instead of duplicating
// the check).
//
// Two different strategies, since the two entry kinds have genuinely
// different failure surfaces:
//   - Proxy Entries (raw TCP/UDP via the WireGuard droplet): reuses the
//     peer handshake recency the stats() handler already computes — real
//     traffic can't cross a dead tunnel regardless of what protocol is on
//     the other end, so this is a protocol-agnostic signal that works for
//     any port without ProxyCTL needing to know what's behind it (that's
//     deliberately GameCTL's job, not this one's).
//   - Web Apps (Cloudflare Tunnel, HTTPS-only): an HTTPS HEAD to the public
//     hostname — real DNS, real Cloudflare edge, real backend, exactly
//     what a visitor's browser would do.
const (
	uptimeSampleEvery = 60 * time.Second // droplet SSH is pricier than a k8s call — slower than GameCTL's 30s
	// Ring length comes from the configured retention (default 30d, see
	// AlertConfig.RetentionDays) at this cadence. Held in memory and
	// snapshotted to /data (next to entries.json) every few ticks + on
	// shutdown, so history survives a pod restart.
	uptimeSnapshotEveryTicks = 5
)

// UptimeSample is one point of an entry's or web route's reachability
// history.
type UptimeSample struct {
	T         int64 `json:"t"`
	Reachable bool  `json:"reachable"`
	LatencyMS int64 `json:"latencyMs,omitempty"`
}

type uptimeHistState struct {
	mu        sync.Mutex
	entries   map[string][]UptimeSample // Entry.ID -> ring buffer
	webRoutes map[string][]UptimeSample // WebRoute.ID -> ring buffer
}

var uptimeHist uptimeHistState

// StartUptimeSampler launches the background poller. Call once at startup;
// it survives config reload (each tick re-reads the live store).
//
// The first sample is delayed a few seconds rather than fired immediately
// at process start: right after a pod (re)starts, outbound SSH/DNS isn't
// always warm yet, and a spurious "unreachable" first sample skews the
// reachability% badly when it's still one of only one or two points on the
// graph (observed: every entry read 0% right after a redeploy, then
// self-corrected on the next tick 60s later).
func (a *API) StartUptimeSampler(ctx context.Context) {
	a.loadUptimeSnapshot()
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		a.sampleUptime(ctx)
		t := time.NewTicker(uptimeSampleEvery)
		defer t.Stop()
		ticks := 0
		for {
			select {
			case <-ctx.Done():
				a.saveUptimeSnapshot()
				return
			case <-t.C:
				a.sampleUptime(ctx)
				if ticks++; ticks%uptimeSnapshotEveryTicks == 0 {
					a.saveUptimeSnapshot()
				}
			}
		}
	}()
}

// uptimeSnapshot is the on-disk shape of the sampler's state.
type uptimeSnapshot struct {
	Entries   map[string][]UptimeSample `json:"entries"`
	WebRoutes map[string][]UptimeSample `json:"webRoutes"`
}

func (a *API) loadUptimeSnapshot() {
	if a.historyPath == "" {
		return
	}
	b, err := os.ReadFile(a.historyPath)
	if err != nil {
		return
	}
	var snap uptimeSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return
	}
	uptimeHist.mu.Lock()
	defer uptimeHist.mu.Unlock()
	if snap.Entries != nil {
		uptimeHist.entries = snap.Entries
	}
	if snap.WebRoutes != nil {
		uptimeHist.webRoutes = snap.WebRoutes
	}
}

// saveUptimeSnapshot writes the sampler state atomically (tmp + rename,
// same convention as every other store here). Best-effort: a failed write
// costs at most a few minutes of history on the next restart.
func (a *API) saveUptimeSnapshot() {
	if a.historyPath == "" {
		return
	}
	uptimeHist.mu.Lock()
	snap := uptimeSnapshot{
		Entries:   make(map[string][]UptimeSample, len(uptimeHist.entries)),
		WebRoutes: make(map[string][]UptimeSample, len(uptimeHist.webRoutes)),
	}
	for k, v := range uptimeHist.entries {
		snap.Entries[k] = append([]UptimeSample(nil), v...)
	}
	for k, v := range uptimeHist.webRoutes {
		snap.WebRoutes[k] = append([]UptimeSample(nil), v...)
	}
	uptimeHist.mu.Unlock()

	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	tmp := a.historyPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, a.historyPath)
}

func (a *API) sampleUptime(ctx context.Context) {
	now := time.Now().Unix()

	entrySamples := map[string]UptimeSample{}
	if sa, ok := a.applier.(SSHApplier); ok && sa.dropletReady() {
		st := sa.sshDroplet("uptime-sample", "", "wg show wg0 dump")
		peers := map[string]int64{} // gateway pubkey -> last handshake unix
		for i, ln := range strings.Split(st.Stdout, "\n") {
			f := strings.Split(ln, "\t")
			if i == 0 || len(f) < 7 { // line 0 = interface
				continue
			}
			hs, _ := strconv.ParseInt(f[4], 10, 64)
			peers[f[0]] = hs
		}
		for _, e := range a.store.List() {
			if !e.Enabled || e.GatewayPubKey == "" {
				continue
			}
			hs := peers[e.GatewayPubKey]
			tunnelUp := hs > 0 && now-hs < 200

			// The WireGuard handshake only proves the tunnel (droplet ↔
			// gateway pod) is alive — the gateway pod is a separate,
			// always-running pod that keeps forwarding regardless of
			// whether anything is actually listening at TargetIP. Scaling
			// the backend game to zero doesn't touch the gateway pod at
			// all, so the handshake alone never catches that. Cross-check
			// against the target Service's actual ready endpoints — a
			// protocol-agnostic, Kubernetes-native signal that works for
			// TCP or UDP without ProxyCTL needing to know the game's
			// protocol. Best-effort: if the check can't run (no kube
			// access, or the Service label isn't set), fall back to the
			// tunnel-only signal rather than report a false down.
			reachable := tunnelUp
			if tunnelUp && strings.Contains(e.Service, ".") {
				if ready, ok := endpointsReady(a.kube, e.Service); ok {
					reachable = ready
				}
			}
			// Latency: a raw TCP dial to the target, in-cluster (ProxyCTL's
			// own pod can reach the ClusterIP directly — this measures the
			// backend's responsiveness, not the WireGuard hop). Only for TCP
			// ports; UDP has no connect-time signal to measure without
			// protocol knowledge, so those entries just won't have a
			// latency graph, same as they don't get one on the LB check in
			// GameCTL.
			var latencyMS int64
			if reachable {
				if port, ok := firstTCPPort(e.Ports); ok {
					start := time.Now()
					conn, err := net.DialTimeout("tcp", net.JoinHostPort(e.TargetIP, strconv.Itoa(port)), 3*time.Second)
					if err == nil {
						latencyMS = time.Since(start).Milliseconds()
						conn.Close()
					}
				}
			}
			entrySamples[e.ID] = UptimeSample{T: now, Reachable: reachable, LatencyMS: latencyMS}
		}
	}

	webSamples := map[string]UptimeSample{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, wr := range a.webroutes.List() {
		if !wr.Enabled {
			continue
		}
		wr := wr
		wg.Add(1)
		go func() {
			defer wg.Done()
			reachable, latency := checkHTTPSReachable(ctx, wr.Hostname)
			mu.Lock()
			webSamples[wr.ID] = UptimeSample{T: now, Reachable: reachable, LatencyMS: latency}
			mu.Unlock()
		}()
	}
	wg.Wait()

	uptimeHist.mu.Lock()
	defer uptimeHist.mu.Unlock()
	if uptimeHist.entries == nil {
		uptimeHist.entries = map[string][]UptimeSample{}
	}
	if uptimeHist.webRoutes == nil {
		uptimeHist.webRoutes = map[string][]UptimeSample{}
	}

	// Alert on reachability transitions only, never on an ID's first-ever
	// sample (nothing to compare yet). Fire-and-forget — a webhook hiccup
	// must never affect the sampler loop.
	if cfg := a.alerts.Get(); cfg.Enabled && cfg.DiscordWebhookURL != "" {
		entryNames := map[string]string{}
		for _, e := range a.store.List() {
			entryNames[e.ID] = e.Name
		}
		webNames := map[string]string{}
		for _, wr := range a.webroutes.List() {
			webNames[wr.ID] = wr.Hostname
		}
		for id, s := range entrySamples {
			if prev := uptimeHist.entries[id]; len(prev) > 0 && prev[len(prev)-1].Reachable != s.Reachable {
				go fireReachabilityAlert(cfg.DiscordWebhookURL, entryNames[id], "tunnel", s.Reachable)
			}
		}
		for id, s := range webSamples {
			if prev := uptimeHist.webRoutes[id]; len(prev) > 0 && prev[len(prev)-1].Reachable != s.Reachable {
				go fireReachabilityAlert(cfg.DiscordWebhookURL, webNames[id], "web app", s.Reachable)
			}
		}
	}

	maxLen := a.alerts.Get().EffectiveRetentionDays() * 24 * 60 // days × samples/day at the 60s cadence
	for id, s := range entrySamples {
		appendUptimeSample(uptimeHist.entries, id, s, maxLen)
	}
	for id, s := range webSamples {
		appendUptimeSample(uptimeHist.webRoutes, id, s, maxLen)
	}

	// Drop entries/routes that no longer exist so deleted ones don't linger.
	liveEntryIDs := map[string]bool{}
	for _, e := range a.store.List() {
		liveEntryIDs[e.ID] = true
	}
	for id := range uptimeHist.entries {
		if !liveEntryIDs[id] {
			delete(uptimeHist.entries, id)
		}
	}
	liveWebIDs := map[string]bool{}
	for _, wr := range a.webroutes.List() {
		liveWebIDs[wr.ID] = true
	}
	for id := range uptimeHist.webRoutes {
		if !liveWebIDs[id] {
			delete(uptimeHist.webRoutes, id)
		}
	}
}

// firstTCPPort returns the in-cluster port to dial for a latency check:
// the first port whose proto is "tcp" or "both", preferring TargetPort
// when the entry remaps public→internal ports.
func firstTCPPort(ports []PortSpec) (int, bool) {
	for _, p := range ports {
		if p.Proto == "tcp" || p.Proto == "both" {
			if p.TargetPort != 0 {
				return p.TargetPort, true
			}
			return p.Port, true
		}
	}
	return 0, false
}

// endpointsReady resolves an entry's "name.namespace" Service label (same
// format liveClusterIP in main.go uses for drift detection) and reports
// whether it currently has at least one ready endpoint address. ok=false
// means the check couldn't run (no kube access, bad label, Service gone)
// — callers should fall back to another signal rather than treat that as
// "down".
func endpointsReady(kube KubeBrowser, svc string) (ready, ok bool) {
	i := strings.Index(svc, ".")
	if i <= 0 || !kube.available() {
		return false, false
	}
	name, ns := svc[:i], svc[i+1:]
	b, err := kube.kubectl("get", "endpoints", name, "-n", ns, "-o",
		"jsonpath={.subsets[*].addresses[*].ip}")
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(b)) != "", true
}

func appendUptimeSample(m map[string][]UptimeSample, id string, s UptimeSample, maxLen int) {
	h := append(m[id], s)
	if len(h) > maxLen {
		h = h[len(h)-maxLen:]
	}
	m[id] = h
}

// checkHTTPSReachable does a real HTTPS HEAD against the public hostname —
// DNS resolution, TLS handshake (default cert verification, same as a
// browser), and one round trip through Cloudflare to the backend. Any
// response at all, even a 4xx from the backend app itself, means the whole
// path is up; only a transport-level failure counts as unreachable.
func checkHTTPSReachable(ctx context.Context, hostname string) (reachable bool, latencyMS int64) {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{}},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+hostname, nil)
	if err != nil {
		return false, 0
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	return true, time.Since(start).Milliseconds()
}

// monitoringSummaryRow is the latest-sample-only view for one entry/route —
// the Monitoring tab's "what's being watched right now" list. No history
// per row (that already lives in the Tunnels/Web Apps table sparklines);
// this is just "is anything down right now."
type monitoringSummaryRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "tunnel" | "webapp"
	Reachable bool   `json:"reachable"`
	LatencyMS int64  `json:"latencyMs,omitempty"`
}

// monitoringSummary handles GET /api/monitoring/summary.
func (a *API) monitoringSummary(w http.ResponseWriter, r *http.Request) {
	uptimeHist.mu.Lock()
	defer uptimeHist.mu.Unlock()

	var rows []monitoringSummaryRow
	for _, e := range a.store.List() {
		h := uptimeHist.entries[e.ID]
		if len(h) == 0 {
			continue
		}
		last := h[len(h)-1]
		rows = append(rows, monitoringSummaryRow{ID: e.ID, Name: e.Name, Kind: "tunnel", Reachable: last.Reachable, LatencyMS: last.LatencyMS})
	}
	for _, wr := range a.webroutes.List() {
		h := uptimeHist.webRoutes[wr.ID]
		if len(h) == 0 {
			continue
		}
		last := h[len(h)-1]
		rows = append(rows, monitoringSummaryRow{ID: wr.ID, Name: wr.Hostname, Kind: "webapp", Reachable: last.Reachable, LatencyMS: last.LatencyMS})
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"instances": rows})
}

// UptimeHistoryForEntry returns the reachability history for one Proxy
// Entry (nil if the sampler hasn't seen it yet).
func UptimeHistoryForEntry(id string) []UptimeSample {
	uptimeHist.mu.Lock()
	defer uptimeHist.mu.Unlock()
	return append([]UptimeSample(nil), uptimeHist.entries[id]...)
}

// UptimeHistoryForWebRoute returns the reachability history for one Web App
// route (nil if the sampler hasn't seen it yet).
func UptimeHistoryForWebRoute(id string) []UptimeSample {
	uptimeHist.mu.Lock()
	defer uptimeHist.mu.Unlock()
	return append([]UptimeSample(nil), uptimeHist.webRoutes[id]...)
}
