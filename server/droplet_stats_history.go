package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

// Droplet resource history — the over-time graphs behind the resource cards
// on the Droplet Management page (the same style as the entry latency
// charts in Monitoring). A background sampler SSHes the droplet every
// minute with the same read-only probe as the live status endpoint and
// appends one compact sample per successful read. Unreachable probes
// append a reachable=false marker so the charts draw a gap instead of
// misleadingly bridging two far-apart points; probes during a maintenance
// operation are skipped entirely (a CPU reading taken mid-reboot or
// mid-apt is noise, and the busy gate keeps this probe from stacking on
// the operation's own SSH session).
//
// Samples live in memory and are snapshotted to /data (same tmp+rename
// convention as the uptime history) every 5 minutes and on shutdown, so
// the graphs survive a pod restart. Retention is a fixed 7 days at the
// 60s cadence (~10k samples, well under 1MB of JSON); the UI offers
// windows from 1h to 3d.
const (
	dropletStatsSampleEvery   = 60 * time.Second
	dropletStatsRetention     = 7 * 24 * time.Hour
	dropletStatsSnapshotTicks = 5 // 5 * 60s = 5 minutes
)

// DropletStatsSample is one point of the droplet's resource history.
// Percentages (not raw bytes) are stored so the ring stays compact.
type DropletStatsSample struct {
	T         int64   `json:"t"`
	Reachable bool    `json:"reachable"`
	CPUPct    float64 `json:"cpuPct"`
	Load1     float64 `json:"load1"`
	MemPct    float64 `json:"memPct"`
	DiskPct   float64 `json:"diskPct"`
}

type dropletStatsHistStore struct {
	mu      sync.Mutex
	samples []DropletStatsSample
}

var dropletStatsHist = dropletStatsHistStore{}

// dropletStatsFromStatus derives one sample from a fully-parsed status.
func dropletStatsFromStatus(st DropletManagementStatus, now int64) DropletStatsSample {
	memPct := 0.0
	if st.MemoryTotal > 0 {
		memPct = float64(st.MemoryUsed) / float64(st.MemoryTotal) * 100
	}
	diskPct := 0.0
	if st.DiskTotal > 0 {
		diskPct = float64(st.DiskUsed) / float64(st.DiskTotal) * 100
	}
	return DropletStatsSample{
		T:         now,
		Reachable: true,
		CPUPct:    st.CPUUsagePct,
		Load1:     st.Load1,
		MemPct:    memPct,
		DiskPct:   diskPct,
	}
}

// appendDropletStatsSample stores one sample and prunes anything older
// than the retention window (bounded by the newest sample's time, so a
// clock skew can never empty the ring).
func appendDropletStatsSample(s DropletStatsSample) {
	dropletStatsHist.mu.Lock()
	defer dropletStatsHist.mu.Unlock()
	dropletStatsHist.samples = append(dropletStatsHist.samples, s)
	dropletStatsHist.pruneLocked(s.T)
}

func (h *dropletStatsHistStore) pruneLocked(now int64) {
	cutoff := now - int64(dropletStatsRetention/time.Second)
	i := 0
	for i < len(h.samples) && h.samples[i].T < cutoff {
		i++
	}
	if i == 0 {
		return
	}
	copy(h.samples, h.samples[i:])
	n := len(h.samples) - i
	for j := n; j < len(h.samples); j++ {
		h.samples[j] = DropletStatsSample{}
	}
	h.samples = h.samples[:n]
}

// loadDropletStatsSnapshot restores the sampler state from disk (called
// once at startup). A missing or corrupt file just means the graphs start
// empty — never an error.
func loadDropletStatsSnapshot(path string) {
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var snap []DropletStatsSample
	if err := json.Unmarshal(b, &snap); err != nil {
		return
	}
	dropletStatsHist.mu.Lock()
	defer dropletStatsHist.mu.Unlock()
	dropletStatsHist.samples = snap
}

// saveDropletStatsSnapshot writes the sampler state atomically (tmp +
// rename). Best-effort: a failed write costs at most 5 minutes of history
// on the next restart.
func saveDropletStatsSnapshot(path string) {
	if path == "" {
		return
	}
	dropletStatsHist.mu.Lock()
	snap := append([]DropletStatsSample(nil), dropletStatsHist.samples...)
	dropletStatsHist.mu.Unlock()
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// StartDropletStatsSampler launches the 60s droplet resource sampler. Call
// once at startup; it survives config reload (each tick re-reads the live
// droplet config). The first sample is delayed a few seconds rather than
// fired immediately, matching the uptime sampler: right after a pod
// (re)starts, outbound SSH isn't always warm yet, and a spurious
// "unreachable" first sample skews the graph.
func (a *API) StartDropletStatsSampler(ctx context.Context) {
	loadDropletStatsSnapshot(a.dropletStatsPath)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		t := time.NewTicker(dropletStatsSampleEvery)
		defer t.Stop()
		ticks := 0
		for {
			select {
			case <-ctx.Done():
				saveDropletStatsSnapshot(a.dropletStatsPath)
				return
			case <-t.C:
				a.sampleDropletStats()
				if ticks++; ticks%dropletStatsSnapshotTicks == 0 {
					saveDropletStatsSnapshot(a.dropletStatsPath)
				}
			}
		}
	}()
}

func (a *API) sampleDropletStats() {
	cfg := a.droplet.Get()
	if cfg == nil || !cfg.Configured() {
		return
	}
	if a.busyDropletMaintenance() != "" {
		return
	}
	now := time.Now().Unix()
	status, complete, err := a.readDropletManagementStatus()
	if err != nil {
		appendDropletStatsSample(DropletStatsSample{T: now, Reachable: false})
		return
	}
	if !complete {
		return // partial parse: don't plot a half-measured point
	}
	appendDropletStatsSample(dropletStatsFromStatus(status, now))
}

// dropletStatsHistory serves the raw ring for the graphs. The client
// filters by window (same as the latency charts), so one endpoint covers
// every range toggle.
func (a *API) dropletStatsHistory(w http.ResponseWriter, _ *http.Request) {
	cfg := a.droplet.Get()
	dropletStatsHist.mu.Lock()
	samples := append([]DropletStatsSample(nil), dropletStatsHist.samples...)
	dropletStatsHist.mu.Unlock()
	a.writeJSON(w, http.StatusOK, map[string]any{
		"configured":       cfg != nil && cfg.Configured(),
		"samples":          samples,
		"sampleSeconds":    int64(dropletStatsSampleEvery / time.Second),
		"retentionSeconds": int64(dropletStatsRetention / time.Second),
	})
}
