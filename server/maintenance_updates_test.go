package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMaintenanceUpdatesPrioritizeWireguardAndIncludeDropletWork(t *testing.T) {
	got := maintenanceUpdatesFrom(
		DropletManagementStatus{PendingUpdates: 7, RebootRequired: true},
		WireguardImageStatus{UpdateAvailable: true, UpstreamRef: "ghcr.io/gamectl-hq/wireguard-kube@sha256:new"},
		&DropletWireguardStatus{Version: "v1.0.20210914", Expected: "v1.0.20260223", Synced: false},
	)
	var kinds []string
	for _, update := range got {
		kinds = append(kinds, update.Kind)
	}
	want := []string{"wireguard-image", "wireguard-droplet", "packages", "reboot"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds = %#v, want %#v", kinds, want)
	}
	if !strings.Contains(got[1].Summary, "v1.0.20210914") || !strings.Contains(got[1].Summary, "v1.0.20260223") {
		t.Fatalf("droplet WireGuard summary lacks versions: %q", got[1].Summary)
	}
}

func TestMaintenanceUpdatesIgnoreUnavailableOrCurrentSignals(t *testing.T) {
	got := maintenanceUpdatesFrom(
		DropletManagementStatus{},
		WireguardImageStatus{LookupError: "registry unavailable"},
		&DropletWireguardStatus{Expected: "v1.0.20260223", Error: "droplet unreachable"},
	)
	if len(got) != 0 {
		t.Fatalf("updates = %#v, want none", got)
	}
}

func TestMaintenanceNotificationPlanDeduplicatesUntilConditionClears(t *testing.T) {
	updates := []MaintenanceUpdateNotice{{Kind: "wireguard-image"}, {Kind: "packages"}}
	active, added := maintenanceNotificationPlan(updates, map[string]bool{"packages": true})
	if !reflect.DeepEqual(added, []string{"wireguard-image"}) {
		t.Fatalf("added = %#v", added)
	}
	if !active["wireguard-image"] || !active["packages"] {
		t.Fatalf("active = %#v", active)
	}

	cleared, added := maintenanceNotificationPlan(nil, active)
	if len(cleared) != 0 || len(added) != 0 {
		t.Fatalf("cleared plan = %#v added=%#v", cleared, added)
	}

	_, added = maintenanceNotificationPlan(updates, cleared)
	if !reflect.DeepEqual(added, []string{"packages", "wireguard-image"}) {
		t.Fatalf("reappeared added = %#v", added)
	}
}

func TestMaintenanceAlertStatePersistsPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintenance-alerts.json")
	want := map[string]bool{"wireguard-image": true, "packages": true}
	if err := saveMaintenanceAlertState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadMaintenanceAlertState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestNotifyMaintenanceUpdatesUsesExistingAlertPipelineWithoutSpam(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintenance-alerts.json")
	cfg := AlertConfig{Enabled: true, DiscordWebhookURL: "https://discord.com/api/webhooks/1/token"}
	updates := []MaintenanceUpdateNotice{{Kind: "wireguard-image", Summary: "WireGuard update"}}
	reliable := map[string]bool{"wireguard-image": true}
	sends := 0
	send := func(title, description string) error {
		sends++
		if !strings.Contains(title, "maintenance updates") || !strings.Contains(description, "WireGuard update") {
			t.Fatalf("unexpected notification: %q %q", title, description)
		}
		return nil
	}
	if err := notifyMaintenanceUpdates(path, cfg, updates, reliable, send); err != nil {
		t.Fatal(err)
	}
	if err := notifyMaintenanceUpdates(path, cfg, updates, reliable, send); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("sends = %d, want 1", sends)
	}
	if err := notifyMaintenanceUpdates(path, cfg, nil, reliable, send); err != nil {
		t.Fatal(err)
	}
	if err := notifyMaintenanceUpdates(path, cfg, updates, reliable, send); err != nil {
		t.Fatal(err)
	}
	if sends != 2 {
		t.Fatalf("sends after reappearance = %d, want 2", sends)
	}
}

func TestNotifyMaintenanceUpdatesKeepsStateWhenProbeUnreliable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintenance-alerts.json")
	cfg := AlertConfig{Enabled: true, DiscordWebhookURL: "https://discord.com/api/webhooks/1/token"}
	updates := []MaintenanceUpdateNotice{{Kind: "wireguard-image", Summary: "WireGuard update"}}
	reliable := map[string]bool{"wireguard-image": true}
	sends := 0
	send := func(string, string) error { sends++; return nil }

	// First observation: reliable → notified.
	if err := notifyMaintenanceUpdates(path, cfg, updates, reliable, send); err != nil {
		t.Fatal(err)
	}
	// Probe blip: the condition is still reported by the (stale-aware)
	// probe, but the kind is NOT reliable. Nothing must be sent AND the
	// notified state must survive so a recovered probe does not re-notify.
	unreliable := map[string]bool{}
	if err := notifyMaintenanceUpdates(path, cfg, updates, unreliable, send); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("sends after unreliable tick = %d, want 1", sends)
	}
	if err := notifyMaintenanceUpdates(path, cfg, updates, reliable, send); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("recovered probe re-notified: sends = %d, want 1", sends)
	}
	// Affirmative clearance prunes; a later reappearance notifies again.
	if err := notifyMaintenanceUpdates(path, cfg, nil, reliable, send); err != nil {
		t.Fatal(err)
	}
	if err := notifyMaintenanceUpdates(path, cfg, updates, reliable, send); err != nil {
		t.Fatal(err)
	}
	if sends != 2 {
		t.Fatalf("sends after true reappearance = %d, want 2", sends)
	}
}

func TestNotifyMaintenanceUpdatesSurvivesCorruptStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintenance-alerts.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := AlertConfig{Enabled: true, DiscordWebhookURL: "https://discord.com/api/webhooks/1/token"}
	updates := []MaintenanceUpdateNotice{{Kind: "packages", Summary: "3 droplet package updates available"}}
	sends := 0
	send := func(string, string) error { sends++; return nil }
	// A corrupt file must not silence notifications: it is treated as a
	// fresh state and the condition is reported.
	if err := notifyMaintenanceUpdates(path, cfg, updates, map[string]bool{"packages": true}, send); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("sends = %d, want 1", sends)
	}
	// The state file is now valid JSON again.
	got, err := loadMaintenanceAlertState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got["packages"] {
		t.Fatalf("state = %#v, want packages notified", got)
	}
}

func TestMaintenanceDiscordMessageUsesOneCombinedAlert(t *testing.T) {
	title, description := maintenanceDiscordMessage([]MaintenanceUpdateNotice{
		{Kind: "wireguard-image", Summary: "WireGuard gateway image update available"},
		{Kind: "packages", Summary: "7 droplet package updates available"},
	})
	if !strings.Contains(title, "maintenance updates") {
		t.Fatalf("title = %q", title)
	}
	if !strings.Contains(description, "WireGuard gateway") || !strings.Contains(description, "7 droplet") {
		t.Fatalf("description = %q", description)
	}
}
