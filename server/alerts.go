package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// External notifications for the uptime history uptimehistory.go already
// collects. Discord-only for now, same shape as GameCTL's alerts feature so
// an operator running both apps gets a consistent experience — but stored
// ProxyCTL's own way (a flat file next to entries.json, atomic tmp+rename
// like every other store here), not a k8s ConfigMap.
type AlertConfig struct {
	DiscordWebhookURL string `json:"discordWebhookUrl,omitempty"`
	Enabled           bool   `json:"enabled"`

	// RetentionDays bounds how much uptime history the sampler keeps.
	// 0 (unset) means the default. Samples are ~24 bytes at a 60s cadence,
	// so even months of retention is only ~1MB per entry — no compaction
	// needed (unlike GameCTL's heavier 30s probe samples).
	RetentionDays int `json:"retentionDays"`

	// Traffic quota — the DigitalOcean data plan. QuotaBytes is the billable
	// allowance in bytes (outbound/WAN transmit); 0 disables the quota bar
	// and its alerts. QuotaNearPct is the "nearing" threshold in percent
	// (0 = use the default, 85). QuotaResetDay is the day-of-month (1-31)
	// at which WAN + per-entry totals auto-reset each month; 0 = manual-only
	// (the operator still gets the reset button).
	QuotaBytes    int64 `json:"quotaBytes,omitempty"`
	QuotaNearPct  int   `json:"quotaNearPct"`
	QuotaResetDay int   `json:"quotaResetDay"`
}

// DefaultRetentionDays is used when RetentionDays is unset/invalid.
const DefaultRetentionDays = 30

// DefaultQuotaBytes is 500 GiB — the default DigitalOcean data allowance.
// GiB is binary (1<<30); DO's data plans are quoted in GiB.
const DefaultQuotaBytes int64 = 500 << 30

// DefaultQuotaNearPct is the "nearing quota" alert threshold.
const DefaultQuotaNearPct = 85

// EffectiveRetentionDays normalizes the configured retention.
func (c AlertConfig) EffectiveRetentionDays() int {
	if c.RetentionDays <= 0 {
		return DefaultRetentionDays
	}
	return c.RetentionDays
}

// EffectiveQuotaNearPct normalizes the nearing threshold to [1,100].
func (c AlertConfig) EffectiveQuotaNearPct() int {
	if c.QuotaNearPct <= 0 {
		return DefaultQuotaNearPct
	}
	if c.QuotaNearPct > 100 {
		return 100
	}
	return c.QuotaNearPct
}

type AlertConfigStore struct {
	path string
	mu   sync.RWMutex
	cfg  AlertConfig
}

func NewAlertConfigStore(path string) (*AlertConfigStore, error) {
	s := &AlertConfigStore{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}

func (s *AlertConfigStore) Get() AlertConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *AlertConfigStore) Set(cfg AlertConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.cfg = cfg
	return nil
}

// discordAlert mirrors Discord's minimal webhook embed shape.
type discordAlert struct {
	Embeds []discordEmbed `json:"embeds,omitempty"`
}
type discordEmbed struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Color       int    `json:"color,omitempty"` // decimal RGB
}

const (
	colorDown = 0xE74C3C // red
	colorUp   = 0x2ECC71 // green
	colorInfo = 0x3498DB // blue, test messages
	colorWarn = 0xF1C40F // yellow, nearing quota
)

// SendDiscordAlert posts one embed to a Discord webhook URL.
func SendDiscordAlert(ctx context.Context, webhookURL, title, description string, color int) error {
	body, err := json.Marshal(discordAlert{Embeds: []discordEmbed{{Title: title, Description: description, Color: color}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook: HTTP %d", resp.StatusCode)
	}
	return nil
}

// fireReachabilityAlert posts one reachability-transition embed. Its own
// context/timeout so it isn't cancelled mid-flight by the sampler's own
// deadline, and it's always called via `go` — best-effort, a webhook hiccup
// must never affect the sampler loop.
func fireReachabilityAlert(webhookURL, name, kind string, up bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	title := fmt.Sprintf("🔴 %s (%s) unreachable", name, kind)
	color := colorDown
	if up {
		title = fmt.Sprintf("🟢 %s (%s) back up", name, kind)
		color = colorUp
	}
	_ = SendDiscordAlert(ctx, webhookURL, title, "", color)
}

// fireTrafficAlert posts one traffic-quota transition embed. Like the
// reachability alerts it runs in its own goroutine with its own deadline and
// is best-effort — a webhook hiccup must never stall the sampler.
func fireTrafficAlert(webhookURL string, kind int, txTotal, quotaBytes int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var title string
	var color int
	pct := 0
	if quotaBytes > 0 {
		pct = int(100 * txTotal / quotaBytes)
	}
	switch kind {
	case 2:
		title = "🔴 Traffic quota exceeded"
		color = colorDown
	case 1:
		title = "🟡 Nearing traffic quota"
		color = colorWarn
	default:
		title = "🟢 Back under traffic quota"
		color = colorUp
	}
	desc := fmt.Sprintf("%s of %s used (%d%%, outbound only).", fmtBytesHuman(txTotal), fmtBytesHuman(quotaBytes), pct)
	_ = SendDiscordAlert(ctx, webhookURL, title, desc, color)
}

// fmtBytesHuman renders a byte count in the largest unit that keeps the value
// >= 1, so a small test quota reads "22.9 MB of 10.0 MB" instead of
// "0.0 GiB of 0.0 GiB".
func fmtBytesHuman(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(1), 0
	for n := b; n >= unit*unit; exp++ {
		n /= unit
		div *= unit
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGT"[exp])
}

func (a *API) getAlertConfig(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, http.StatusOK, a.alerts.Get())
}

func (a *API) setAlertConfig(w http.ResponseWriter, r *http.Request) {
	var cfg AlertConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if err := a.alerts.Set(cfg); err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// testAlertConfig sends a real Discord message against the CURRENTLY SAVED
// config, not a URL from the request body, so "Test" always proves what
// "Save" actually persisted.
func (a *API) testAlertConfig(w http.ResponseWriter, r *http.Request) {
	cfg := a.alerts.Get()
	if cfg.DiscordWebhookURL == "" {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no webhook URL saved yet — save one first"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if err := SendDiscordAlert(ctx, cfg.DiscordWebhookURL,
		"🔔 ProxyCTL test alert", "If you can see this, your webhook is set up correctly.", colorInfo); err != nil {
		a.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "test send failed: " + err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// trafficQuota reports the current WAN egress total, the configured quota, and
// where in the monthly cycle we are — everything the frontend quota bar needs
// without a separate ssh call (values are at most one sampler tick stale).
func (a *API) trafficQuota(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		a.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "traffic accounting not available"})
		return
	}
	cfg := a.alerts.Get()
	lastLevel, lastResetAt, monthKey, txTotal := a.traffic.QuotaStatus()
	wanRx, _ := a.traffic.Wantotals()
	a.writeJSON(w, http.StatusOK, map[string]any{
		"txTotal":     txTotal,
		"rxTotal":     wanRx,
		"quotaBytes":  cfg.QuotaBytes,
		"nearPct":     cfg.EffectiveQuotaNearPct(),
		"resetDay":    cfg.QuotaResetDay,
		"lastResetAt": lastResetAt,
		"monthKey":    monthKey,
		"level":       lastLevel,
		"enabled":     cfg.Enabled,
	})
}

// resetTrafficQuota zeroes the WAN + per-entry byte totals and the quota level.
// The next sampler tick re-seeds the baselines from the live counters, so this
// is safe to call any time (it does not touch the counters themselves).
func (a *API) resetTrafficQuota(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		a.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "traffic accounting not available"})
		return
	}
	a.traffic.ManualReset(time.Now().Unix())
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
