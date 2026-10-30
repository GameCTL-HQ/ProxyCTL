package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func resetDropletStatsHist() {
	dropletStatsHist.mu.Lock()
	defer dropletStatsHist.mu.Unlock()
	dropletStatsHist.samples = nil
}

func TestDropletStatsFromStatus(t *testing.T) {
	st := DropletManagementStatus{
		CPUUsagePct: 17.5,
		Load1:       0.12,
		MemoryTotal: 4096,
		MemoryUsed:  1024, // 25%
		DiskTotal:   50000,
		DiskUsed:    10000, // 20%
	}
	got := dropletStatsFromStatus(st, 1234)
	if got.T != 1234 || !got.Reachable {
		t.Fatalf("bad base fields: %+v", got)
	}
	if got.CPUPct != 17.5 || got.Load1 != 0.12 {
		t.Fatalf("cpu/load copied wrong: %+v", got)
	}
	if got.MemPct != 25 {
		t.Fatalf("memPct = %v, want 25", got.MemPct)
	}
	if got.DiskPct != 20 {
		t.Fatalf("diskPct = %v, want 20", got.DiskPct)
	}
	// zero totals must not produce NaN/Inf percentages
	zero := dropletStatsFromStatus(DropletManagementStatus{}, 1)
	if zero.MemPct != 0 || zero.DiskPct != 0 {
		t.Fatalf("zero totals should give 0%%: %+v", zero)
	}
}

func TestDropletStatsRetentionPrunesOldSamples(t *testing.T) {
	resetDropletStatsHist()
	defer resetDropletStatsHist()
	now := time.Now().Unix()
	old := now - int64(8*24*time.Hour/time.Second) // beyond the 7d retention
	appendDropletStatsSample(DropletStatsSample{T: old, Reachable: true, CPUPct: 99})
	appendDropletStatsSample(DropletStatsSample{T: now, Reachable: true, CPUPct: 1})

	dropletStatsHist.mu.Lock()
	got := append([]DropletStatsSample(nil), dropletStatsHist.samples...)
	dropletStatsHist.mu.Unlock()
	if len(got) != 1 || got[0].T != now {
		t.Fatalf("expected only the recent sample to remain, got %+v", got)
	}
}

func TestDropletStatsRetentionKeepsWindow(t *testing.T) {
	resetDropletStatsHist()
	defer resetDropletStatsHist()
	now := time.Now().Unix()
	keep := now - int64(6*24*time.Hour/time.Second) // inside retention
	appendDropletStatsSample(DropletStatsSample{T: keep, Reachable: true})
	appendDropletStatsSample(DropletStatsSample{T: now, Reachable: false})

	dropletStatsHist.mu.Lock()
	n := len(dropletStatsHist.samples)
	dropletStatsHist.mu.Unlock()
	if n != 2 {
		t.Fatalf("expected both samples kept, got %d", n)
	}
}

func TestDropletStatsSnapshotRoundTrip(t *testing.T) {
	resetDropletStatsHist()
	defer resetDropletStatsHist()
	now := time.Now().Unix()
	want := []DropletStatsSample{
		{T: now - 120, Reachable: true, CPUPct: 12.5, Load1: 0.1, MemPct: 33.3, DiskPct: 61.2},
		{T: now - 60, Reachable: false},
		{T: now, Reachable: true, CPUPct: 44.4, Load1: 0.3, MemPct: 35.0, DiskPct: 61.2},
	}
	for _, s := range want {
		appendDropletStatsSample(s)
	}
	path := filepath.Join(t.TempDir(), "droplet-stats-history.json")
	saveDropletStatsSnapshot(path)

	// the file must exist and be readable back after a full state reset
	resetDropletStatsHist()
	loadDropletStatsSnapshot(path)

	dropletStatsHist.mu.Lock()
	got := append([]DropletStatsSample(nil), dropletStatsHist.samples...)
	dropletStatsHist.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("round trip length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// no stray tmp file after an atomic rename
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind: %v", err)
	}
}

func TestDropletStatsSnapshotMissingFileIsNoop(t *testing.T) {
	resetDropletStatsHist()
	defer resetDropletStatsHist()
	loadDropletStatsSnapshot(filepath.Join(t.TempDir(), "does-not-exist.json"))
	dropletStatsHist.mu.Lock()
	n := len(dropletStatsHist.samples)
	dropletStatsHist.mu.Unlock()
	if n != 0 {
		t.Fatalf("missing file should leave state empty, got %d samples", n)
	}
}
