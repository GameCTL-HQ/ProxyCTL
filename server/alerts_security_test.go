package main

import (
	"os"
	"path/filepath"
	"testing"
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
