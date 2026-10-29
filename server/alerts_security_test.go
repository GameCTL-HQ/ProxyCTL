package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateDiscordWebhookURL(t *testing.T) {
	valid := []string{
		"https://discord.com/api/webhooks/123/token",
		"https://discordapp.com/api/webhooks/123/token",
	}
	for _, raw := range valid {
		if err := validateDiscordWebhookURL(raw); err != nil {
			t.Errorf("validateDiscordWebhookURL(%q) = %v", raw, err)
		}
	}
	invalid := []string{
		"http://discord.com/api/webhooks/123/token",
		"https://example.com/api/webhooks/123/token",
		"https://127.0.0.1/api/webhooks/123/token",
		"https://discord.com.evil.example/api/webhooks/123/token",
		"https://discord.com/not-webhooks/123/token",
		"https://user@discord.com/api/webhooks/123/token",
	}
	for _, raw := range invalid {
		if err := validateDiscordWebhookURL(raw); err == nil {
			t.Errorf("validateDiscordWebhookURL(%q) accepted unsafe URL", raw)
		}
	}
}

func TestAlertConfigStoreWritesSecretsMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	store, err := NewAlertConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(AlertConfig{DiscordWebhookURL: "https://discord.com/api/webhooks/123/token"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("alerts.json mode = %o, want 600", got)
	}
}

func TestAlertConfigStoreHardensExistingFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	if err := os.WriteFile(path, []byte(`{"enabled":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAlertConfigStore(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("existing alerts.json mode = %o, want 600", got)
	}
}

func TestAlertConfigEffectiveReachabilityDefaultsAndBounds(t *testing.T) {
	if got := (AlertConfig{}).EffectiveCheckInterval(); got != 60*time.Second {
		t.Fatalf("default check interval = %s, want 60s", got)
	}
	if got := (AlertConfig{}).EffectiveFailureThreshold(); got != 2 {
		t.Fatalf("default failure threshold = %d, want 2", got)
	}

	tooLow := AlertConfig{CheckIntervalSeconds: 1, FailureThreshold: -4}
	if got := tooLow.EffectiveCheckInterval(); got != 10*time.Second {
		t.Fatalf("minimum check interval = %s, want 10s", got)
	}
	if got := tooLow.EffectiveFailureThreshold(); got != 1 {
		t.Fatalf("minimum failure threshold = %d, want 1", got)
	}

	tooHigh := AlertConfig{CheckIntervalSeconds: 99999, FailureThreshold: 99}
	if got := tooHigh.EffectiveCheckInterval(); got != time.Hour {
		t.Fatalf("maximum check interval = %s, want 1h", got)
	}
	if got := tooHigh.EffectiveFailureThreshold(); got != 10 {
		t.Fatalf("maximum failure threshold = %d, want 10", got)
	}

	if got := (AlertConfig{}).EffectiveRetentionDays(); got != DefaultRetentionDays {
		t.Fatalf("default retention = %d, want %d", got, DefaultRetentionDays)
	}
	if got := (AlertConfig{RetentionDays: MaxRetentionDays + 100}).EffectiveRetentionDays(); got != MaxRetentionDays {
		t.Fatalf("oversized retention = %d, want clamped %d", got, MaxRetentionDays)
	}

	custom := AlertConfig{CheckIntervalSeconds: 90, FailureThreshold: 4}
	if got := custom.EffectiveCheckInterval(); got != 90*time.Second {
		t.Fatalf("custom check interval = %s, want 90s", got)
	}
	if got := custom.EffectiveFailureThreshold(); got != 4 {
		t.Fatalf("custom failure threshold = %d, want 4", got)
	}
}

func TestAlertConfigStorePersistsCanonicalReachabilityPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	store, err := NewAlertConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(AlertConfig{CheckIntervalSeconds: 1, FailureThreshold: 99}); err != nil {
		t.Fatal(err)
	}
	got := store.Get()
	if got.CheckIntervalSeconds != 10 || got.FailureThreshold != 10 {
		t.Fatalf("stored policy = interval %d, threshold %d; want 10 and 10", got.CheckIntervalSeconds, got.FailureThreshold)
	}

	reloaded, err := NewAlertConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got = reloaded.Get()
	if got.CheckIntervalSeconds != 10 || got.FailureThreshold != 10 {
		t.Fatalf("reloaded policy = interval %d, threshold %d; want 10 and 10", got.CheckIntervalSeconds, got.FailureThreshold)
	}
}
