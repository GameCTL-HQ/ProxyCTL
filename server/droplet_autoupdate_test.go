package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAutoUpdateStorePersistsAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "droplet-autoupdate.json")
	s := newAutoUpdateStore(path)
	if got := s.Get().EveryDays; got != 0 {
		t.Fatalf("fresh store everyDays = %d, want 0", got)
	}

	st, err := s.SetEveryDays(7)
	if err != nil {
		t.Fatal(err)
	}
	if st.EveryDays != 7 {
		t.Fatalf("stored everyDays = %d, want 7", st.EveryDays)
	}

	// Reload from disk: the policy must survive a proxyctl restart.
	s2 := newAutoUpdateStore(path)
	if got := s2.Get().EveryDays; got != 7 {
		t.Fatalf("reloaded everyDays = %d, want 7", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}

	if _, err := s2.SetEveryDays(-1); err == nil {
		t.Fatal("negative interval accepted")
	}
	if _, err := s2.SetEveryDays(366); err == nil {
		t.Fatal("interval above 365 accepted")
	}
	if st, err := s2.SetEveryDays(0); err != nil || st.EveryDays != 0 {
		t.Fatalf("disabling failed: %#v err=%v", st, err)
	}
}

func TestAutoUpdateDueLogic(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	cases := []struct {
		name      string
		everyDays int
		lastRun   int64
		want      bool
	}{
		{"disabled is never due", 0, 0, false},
		{"enabled never-run is due immediately", 7, 0, true},
		{"within interval is not due", 7, now.Add(-24 * time.Hour).Unix(), false},
		{"interval elapsed is due", 7, now.Add(-7*24*time.Hour - time.Minute).Unix(), true},
		{"disabled with old run is never due", 0, now.Add(-30 * 24 * time.Hour).Unix(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAutoUpdateStore(filepath.Join(t.TempDir(), "state.json"))
			s.mu.Lock()
			s.state.EveryDays = tc.everyDays
			s.state.LastRun = tc.lastRun
			s.mu.Unlock()
			if got := s.autoUpdateDue(now); got != tc.want {
				t.Fatalf("autoUpdateDue = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAutoUpdateRecordRunMakesIntervalNotDue(t *testing.T) {
	s := newAutoUpdateStore(filepath.Join(t.TempDir(), "state.json"))
	if _, err := s.SetEveryDays(7); err != nil {
		t.Fatal(err)
	}
	if !s.autoUpdateDue(time.Now()) {
		t.Fatal("never-run policy must be due")
	}
	s.recordRun(time.Now())
	if s.autoUpdateDue(time.Now()) {
		t.Fatal("policy still due right after a recorded run")
	}
}

func TestVersionCatchupDecision(t *testing.T) {
	cases := []struct {
		last, current string
		want          bool
	}{
		{"", "v0.7.0", false},          // first boot: record baseline only
		{"v0.7.0", "v0.7.0", false},    // unchanged
		{"v0.6.23", "v0.7.0", true},    // app updated: force catch-up
		{"ecd9cf2", "v0.7.0", true},    // dev commit to release
		{"  v0.6.23 ", "v0.7.0", true}, // tolerant of stray whitespace
	}
	for _, tc := range cases {
		if got := versionCatchupDecision(tc.last, tc.current); got != tc.want {
			t.Errorf("versionCatchupDecision(%q, %q) = %v, want %v", tc.last, tc.current, got, tc.want)
		}
	}
}

func TestRunDropletMaintenanceUpdateSkipsWithoutConfiguredDroplet(t *testing.T) {
	// No droplet store, no applier: must skip neutrally, never error out
	// the background loop, and never touch the host.
	a := &API{}
	got := a.runDropletMaintenanceUpdate("test")
	if got["ok"] == true {
		t.Fatalf("unconfigured run reported ok: %#v", got)
	}
	if _, skipped := got["skipped"]; !skipped {
		t.Fatalf("unconfigured run missing skip marker: %#v", got)
	}
}

func TestRunWireguardImageUpdateRespectsSharedMaintenanceLock(t *testing.T) {
	// A UI "update" operation in progress must block the background
	// WireGuard phase on the same lock (and vice versa).
	a := &API{dropletMaintenance: "update"}
	got := a.runWireguardImageUpdate(t.Context())
	if got["ok"] == true {
		t.Fatalf("WireGuard phase ran during a busy update: %#v", got)
	}
	if got["busy"] != "update" {
		t.Fatalf("busy = %#v, want update", got["busy"])
	}
}

func TestAutoUpdateStateJSONNextRun(t *testing.T) {
	last := int64(1_700_000_000)
	got := autoUpdateStateJSON(DropletAutoUpdateState{EveryDays: 7, LastRun: last})
	wantNext := last + 7*86400
	if !reflect.DeepEqual(got["nextRun"], wantNext) {
		t.Fatalf("nextRun = %#v, want %d", got["nextRun"], wantNext)
	}
	off := autoUpdateStateJSON(DropletAutoUpdateState{})
	if _, present := off["nextRun"]; present {
		t.Fatalf("disabled policy must not advertise nextRun: %#v", off)
	}
}
