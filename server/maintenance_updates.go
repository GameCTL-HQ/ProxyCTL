package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"
)

// MaintenanceUpdateNotice is one actionable host or WireGuard maintenance
// condition. Kind is stable and is used for Discord de-duplication; Summary is
// operator-facing text used by both the app and the webhook notification.
type MaintenanceUpdateNotice struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

// maintenanceUpdatesFrom converts the live droplet + WireGuard probes into a
// stable, priority-ordered list. WireGuard leads because it is the security and
// connectivity-sensitive maintenance path the operator most wants surfaced.
func maintenanceUpdatesFrom(droplet DropletManagementStatus, image WireguardImageStatus, wg *DropletWireguardStatus) []MaintenanceUpdateNotice {
	var out []MaintenanceUpdateNotice
	if image.UpdateAvailable {
		out = append(out, MaintenanceUpdateNotice{
			Kind:    "wireguard-image",
			Summary: "WireGuard gateway image update available",
		})
	}
	if wg != nil && wg.Error == "" && !wg.Synced && wg.Version != "" {
		out = append(out, MaintenanceUpdateNotice{
			Kind:    "wireguard-droplet",
			Summary: fmt.Sprintf("Droplet WireGuard tools %s → %s", wg.Version, wg.Expected),
		})
	}
	if droplet.PendingUpdates > 0 {
		out = append(out, MaintenanceUpdateNotice{
			Kind:    "packages",
			Summary: fmt.Sprintf("%d droplet package update%s available", droplet.PendingUpdates, map[bool]string{true: "", false: "s"}[droplet.PendingUpdates == 1]),
		})
	}
	if droplet.RebootRequired {
		out = append(out, MaintenanceUpdateNotice{
			Kind:    "reboot",
			Summary: "Droplet reboot required to finish installed updates",
		})
	}
	return out
}

// maintenanceNotificationPlan returns the currently active kinds and the
// subset that were not present in the persisted notified set. Clearing a
// condition prunes it, so a later reappearance produces a fresh notification.
func maintenanceNotificationPlan(updates []MaintenanceUpdateNotice, notified map[string]bool) (active map[string]bool, added []string) {
	active = make(map[string]bool, len(updates))
	for _, update := range updates {
		if update.Kind == "" {
			continue
		}
		active[update.Kind] = true
		if !notified[update.Kind] {
			added = append(added, update.Kind)
		}
	}
	sort.Strings(added)
	return active, added
}

func maintenanceDiscordMessage(updates []MaintenanceUpdateNotice) (title, description string) {
	lines := make([]string, 0, len(updates))
	for _, update := range updates {
		if update.Summary != "" {
			lines = append(lines, "• "+update.Summary)
		}
	}
	return "🟡 ProxyCTL maintenance updates available", strings.Join(lines, "\n")
}

type maintenanceAlertState struct {
	Notified map[string]bool `json:"notified"`
}

func loadMaintenanceAlertState(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	var state maintenanceAlertState
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, err
	}
	if state.Notified == nil {
		state.Notified = map[string]bool{}
	}
	return state.Notified, nil
}

func saveMaintenanceAlertState(path string, notified map[string]bool) error {
	b, err := json.MarshalIndent(maintenanceAlertState{Notified: notified}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// notifyMaintenanceUpdates reports newly-appeared conditions through the
// configured Discord webhook. reliable marks the kinds whose probes actually
// succeeded this round: only reliable kinds can be added (notified) or
// pruned (treated as cleared). A kind whose probe failed keeps its previous
// notification state, so a transient SSH/GHCR/kubectl blip can never clear
// a notification and cause the same condition to be re-reported later.
func notifyMaintenanceUpdates(path string, cfg AlertConfig, updates []MaintenanceUpdateNotice, reliable map[string]bool, send func(title, description string) error) error {
	notified, err := loadMaintenanceAlertState(path)
	if err != nil {
		// A corrupt or unreadable state file must not silence maintenance
		// alerts forever: start fresh (worst case: one re-notification).
		log.Printf("maintenance alert state unreadable, starting fresh: %v", err)
		notified = map[string]bool{}
	}
	active, _ := maintenanceNotificationPlan(updates, nil)
	var addedKinds []string
	for _, update := range updates {
		if update.Kind == "" || notified[update.Kind] || !reliable[update.Kind] {
			continue
		}
		addedKinds = append(addedKinds, update.Kind)
	}
	sort.Strings(addedKinds)
	// Keep every previously-notified kind that is either still active or
	// could not be checked this round; prune only affirmatively-observed
	// clearances.
	kept := make(map[string]bool, len(notified))
	for kind := range notified {
		if active[kind] || !reliable[kind] {
			kept[kind] = true
		}
	}
	if !cfg.Enabled || cfg.DiscordWebhookURL == "" || len(addedKinds) == 0 {
		return saveMaintenanceAlertState(path, kept)
	}
	addedSet := make(map[string]bool, len(addedKinds))
	for _, kind := range addedKinds {
		addedSet[kind] = true
	}
	added := make([]MaintenanceUpdateNotice, 0, len(addedKinds))
	for _, update := range updates {
		if addedSet[update.Kind] {
			added = append(added, update)
		}
	}
	title, description := maintenanceDiscordMessage(added)
	if err := send(title, description); err != nil {
		// Send failed: keep the pre-add state so the condition retries on
		// the next tick (it still counts as "notified=false" for the new
		// kinds — they were never reported).
		_ = saveMaintenanceAlertState(path, kept)
		return err
	}
	// Next state: what we have reported and have not affirmatively seen
	// clear.
	next := make(map[string]bool, len(kept)+len(addedSet))
	for kind := range kept {
		next[kind] = true
	}
	for kind := range addedSet {
		next[kind] = true
	}
	return saveMaintenanceAlertState(path, next)
}

// Monitor timing as package vars (not consts) so tests can shrink the
// startup grace and the check cadence to milliseconds; production keeps
// these values.
var (
	maintenanceMonitorInitialDelay = 45 * time.Second
	maintenanceUpdateCheckInterval = 6 * time.Hour
)

// StartMaintenanceUpdateMonitor checks from the server rather than the browser,
// so Discord notices continue even when no ProxyCTL tab is open.
func (a *API) StartMaintenanceUpdateMonitor(ctx context.Context) {
	go func() {
		initial := time.NewTimer(maintenanceMonitorInitialDelay)
		defer initial.Stop()
		select {
		case <-ctx.Done():
			return
		case <-initial.C:
			a.checkAndNotifyMaintenanceUpdates()
		}
		ticker := time.NewTicker(maintenanceUpdateCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.checkAndNotifyMaintenanceUpdates()
			}
		}
	}()
}

func (a *API) checkAndNotifyMaintenanceUpdates() {
	// Operator interval auto-update: when due, run the same busy-gated
	// droplet + WireGuard maintenance the UI buttons run. Never reboots.
	if a.autoUpdate != nil && a.autoUpdate.autoUpdateDue(time.Now()) {
		if cfg := dropletConfigOrNil(a.droplet); cfg == nil || !cfg.Configured() {
			// Nothing to update yet; do NOT record the run, so the
			// interval fires as soon as a droplet is configured.
		} else {
			st := a.autoUpdate.Get()
			a.autoUpdate.recordRun(time.Now())
			log.Printf("droplet autoupdate: interval run due (every %d day(s), last run %d)", st.EveryDays, st.LastRun)
			summary := a.runDropletMaintenanceUpdate("auto-interval")
			log.Printf("droplet autoupdate: interval run finished ok=%v", summary["ok"])
		}
	}
	status, complete, err := a.readDropletManagementStatus()
	switch {
	case err == nil && !complete:
		log.Printf("maintenance update check skipped: droplet status only partially parsed")
		return
	case err != nil && !errors.Is(err, errDropletNotConfigured):
		log.Printf("maintenance update check skipped: droplet status probe failed (%v)", err)
		return
	}
	dropletConfigured := err == nil
	if !dropletConfigured {
		status = DropletManagementStatus{}
	}
	ns, kubeContext := a.wireguardImageContext()
	imageStatus := wireguardImageStatus(ns, kubeContext)
	var dropletWG *DropletWireguardStatus
	if dropletConfigured {
		dropletWG = a.dropletWireguardStatus()
	}
	updates := maintenanceUpdatesFrom(status, imageStatus, dropletWG)
	reliable := map[string]bool{}
	if dropletConfigured {
		reliable["packages"] = true
		reliable["reboot"] = true
	}
	if imageStatus.liveImagesOK && imageStatus.LookupError == "" {
		reliable["wireguard-image"] = true
	}
	if dropletConfigured && dropletWG != nil && dropletWG.Error == "" {
		reliable["wireguard-droplet"] = true
	}
	cfg := a.alerts.Get()
	err = notifyMaintenanceUpdates(a.maintenanceAlertPath, cfg, updates, reliable, func(title, description string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return SendDiscordAlert(ctx, cfg.DiscordWebhookURL, title, description, 0xF59E0B)
	})
	if err != nil {
		log.Printf("maintenance update notification: %v", err)
	}
}
