package main

// Droplet auto-update policy + background maintenance runner.
//
// Two triggers, one runner:
//  1. Version catch-up — when ProxyCTL itself moves to a new version the
//     new pod runs the droplet + WireGuard maintenance update once at
//     startup, so an app update always forces the remote half current.
//  2. Operator interval — when the operator sets "auto-update every N
//     days", the maintenance monitor runs the same update when due.
//
// Both are strictly read-then-update: they never reboot the droplet. If the
// finished update leaves reboot-required state, the existing maintenance
// notice (in-app banner + Discord) tells the operator, and the reboot is
// scheduled only through the existing explicit "Reboot droplet" flow.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// DropletAutoUpdateState is the persisted policy + bookkeeping.
// EveryDays: 0 = disabled, 1..365 = run the full droplet + WireGuard
// maintenance update every N days.
type DropletAutoUpdateState struct {
	EveryDays int   `json:"everyDays"`
	LastRun   int64 `json:"lastRun"` // unix seconds of the last auto/catch-up run
}

type autoUpdateStore struct {
	mu    sync.Mutex
	path  string
	state DropletAutoUpdateState
}

func newAutoUpdateStore(path string) *autoUpdateStore {
	s := &autoUpdateStore{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s.state)
	}
	if s.state.EveryDays < 0 || s.state.EveryDays > 365 {
		s.state.EveryDays = 0
	}
	return s
}

func (s *autoUpdateStore) Get() DropletAutoUpdateState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// SetEveryDays validates and persists the interval. 0 disables; 1..365
// enables. Out-of-range values are rejected rather than clamped so the UI
// can surface an honest error.
func (s *autoUpdateStore) SetEveryDays(days int) (DropletAutoUpdateState, error) {
	if days < 0 || days > 365 {
		return s.Get(), fmt.Errorf("auto-update interval must be 0 (off) or 1-365 days, got %d", days)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.EveryDays = days
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return s.state, err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return s.state, err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return s.state, err
	}
	return s.state, nil
}

// recordRun marks the interval as satisfied now so a restart mid-update
// cannot double-run.
func (s *autoUpdateStore) recordRun(t time.Time) {
	s.mu.Lock()
	s.state.LastRun = t.Unix()
	b, _ := json.MarshalIndent(s.state, "", "  ")
	s.mu.Unlock()
	tmp := s.path + ".tmp"
	_ = os.WriteFile(tmp, b, 0o600)
	_ = os.Rename(tmp, s.path)
}

// autoUpdateDue reports whether an interval run is pending. A never-run
// policy is due immediately once enabled; an unconfigured droplet is
// handled by the caller (the run is skipped and NOT recorded, so the
// interval fires the moment a droplet is configured).
func (s *autoUpdateStore) autoUpdateDue(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.EveryDays == 0 {
		return false
	}
	if s.state.LastRun == 0 {
		return true
	}
	return now.Unix() >= s.state.LastRun+int64(s.state.EveryDays*86400)
}

// versionCatchupDecision: run the catch-up exactly when a previous version
// was recorded and it differs from the current one. First boot (no record)
// only writes the baseline.
func versionCatchupDecision(last, current string) bool {
	last = strings.TrimSpace(last)
	current = strings.TrimSpace(current)
	return last != "" && last != current
}

// autoUpdateStateJSON is the wire shape shared by GET/PUT.
func autoUpdateStateJSON(st DropletAutoUpdateState) map[string]any {
	resp := map[string]any{"everyDays": st.EveryDays, "lastRun": st.LastRun}
	if st.EveryDays > 0 && st.LastRun > 0 {
		resp["nextRun"] = st.LastRun + int64(st.EveryDays*86400)
	}
	return resp
}

func (a *API) getAutoUpdate(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, autoUpdateStateJSON(a.autoUpdate.Get()))
}

func (a *API) setAutoUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EveryDays *int `json:"everyDays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.EveryDays == nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"everyDays\": 0-365}"})
		return
	}
	st, err := a.autoUpdate.SetEveryDays(*body.EveryDays)
	if err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("droplet autoupdate: interval set to %s", map[bool]string{true: "off", false: fmt.Sprintf("every %d day(s)", st.EveryDays)}[st.EveryDays == 0])
	a.writeJSON(w, http.StatusOK, autoUpdateStateJSON(st))
}

// startVersionCatchup records the running version and, when the version
// changed since the last recorded one, forces the droplet + WireGuard
// maintenance update once the new pod has settled.
func (a *API) startVersionCatchup(ctx context.Context) {
	go func() {
		var last string
		if b, err := os.ReadFile(a.lastVersionPath); err == nil {
			last = strings.TrimSpace(string(b))
		}
		_ = os.WriteFile(a.lastVersionPath, []byte(version), 0o600)
		if !versionCatchupDecision(last, version) {
			return
		}
		log.Printf("droplet autoupdate: ProxyCTL version changed %s -> %s; scheduling droplet + WireGuard catch-up update", last, version)
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}
		a.autoUpdate.recordRun(time.Now())
		summary := a.runDropletMaintenanceUpdate("proxyctl-update-" + version)
		log.Printf("droplet autoupdate: version catch-up finished ok=%v", summary["ok"])
		a.checkAndNotifyMaintenanceUpdates()
	}()
}

// runDropletMaintenanceUpdate performs the same work the UI's "Update
// droplet & Cloudflare" and "Update WireGuard" buttons do — OS packages +
// connector refresh, then the two-phase WireGuard update — without any HTTP
// layer, for the background triggers. It is guarded by the same
// droplet-maintenance lock as the UI operations, so a background run and a
// manual run can never interleave on the same host. It NEVER reboots the
// droplet; a reboot left required by the update is surfaced through the
// maintenance notice for the operator to schedule explicitly.
func (a *API) runDropletMaintenanceUpdate(reason string) map[string]any {
	summary := map[string]any{"reason": reason, "startedAt": time.Now().Unix()}
	cfg := dropletConfigOrNil(a.droplet)
	_, isSSH := a.applier.(SSHApplier)
	if !isSSH || !cfg.Configured() {
		summary["ok"] = false
		summary["skipped"] = "droplet not configured or ssh apply-mode unavailable"
		return summary
	}

	// Phase 1: OS packages + cloudflared connectors (UI "update" kind).
	opCtx, cancel := context.WithTimeout(context.Background(), 13*time.Minute)
	boot := a.runDropletBootstrapUpgrade(opCtx, true)
	cancel()
	summary["dropletUpdate"] = boot

	// Phase 2: WireGuard gateway image + droplet tools (UI "wireguard-image" kind).
	wgCtx, wgCancel := context.WithTimeout(context.Background(), 9*time.Minute)
	wg := a.runWireguardImageUpdate(wgCtx)
	wgCancel()
	summary["wireguardUpdate"] = wg

	summary["ok"] = boot["ok"] == true && wg["ok"] == true
	summary["finishedAt"] = time.Now().Unix()
	return summary
}

func dropletConfigOrNil(store DropletStore) *DropletConfig {
	if store == nil {
		return nil
	}
	return store.Get()
}
