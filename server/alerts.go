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
}

// DefaultRetentionDays is used when RetentionDays is unset/invalid.
const DefaultRetentionDays = 30

// EffectiveRetentionDays normalizes the configured retention.
func (c AlertConfig) EffectiveRetentionDays() int {
	if c.RetentionDays <= 0 {
		return DefaultRetentionDays
	}
	return c.RetentionDays
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
