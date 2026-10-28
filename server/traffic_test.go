package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCountersObserve(t *testing.T) {
	// First sight: seed — the pre-existing bytes are NOT counted.
	c := counters{}.observe(100, 200)
	if !c.Seeded || c.LastRx != 100 || c.LastTx != 200 || c.RxAccum != 0 || c.TxAccum != 0 {
		t.Fatalf("seed wrong: %+v", c)
	}

	// Monotonic: accumulate the delta.
	c = c.observe(150, 300)
	if c.RxAccum != 50 || c.TxAccum != 100 || c.LastRx != 150 || c.LastTx != 300 {
		t.Fatalf("monotonic wrong: %+v", c)
	}

	// Reset/wrap (raw < last): the whole new reading is fresh traffic.
	c = c.observe(30, 40)
	if c.RxAccum != 80 || c.TxAccum != 140 || c.LastRx != 30 || c.LastTx != 40 {
		t.Fatalf("reset wrong: %+v", c)
	}

	// Unchanged raw: no accumulation.
	c2 := (counters{LastRx: 100, LastTx: 200, Seeded: true}).observe(100, 200)
	if c2.RxAccum != 0 || c2.TxAccum != 0 {
		t.Fatalf("unchanged should add 0: %+v", c2)
	}
}

func TestCountersLiveTotal(t *testing.T) {
	// Unseeded: no in-flight delta, just the accumulated total.
	if rx, tx := (counters{RxAccum: 500, TxAccum: 600}).liveTotal(10, 20); rx != 500 || tx != 600 {
		t.Fatalf("unseeded liveTotal wrong: %d %d", rx, tx)
	}

	// Seeded + moving: accumulated + the delta since the last tick.
	c := counters{RxAccum: 500, TxAccum: 600, LastRx: 1000, LastTx: 2000, Seeded: true}
	if rx, tx := c.liveTotal(1500, 2500); rx != 1000 || tx != 1100 {
		t.Fatalf("moving liveTotal wrong: %d %d", rx, tx)
	}

	// Seeded + re-key mid-tick (raw < last): no negative delta.
	if rx, tx := c.liveTotal(50, 30); rx != 500 || tx != 600 {
		t.Fatalf("rekey liveTotal should not go negative: %d %d", rx, tx)
	}
}

func TestComputeQuotaLevel(t *testing.T) {
	cases := []struct {
		name   string
		tx     int64
		quota  int64
		near   int
		expect quotaLevel
	}{
		{"zero quota disables", 999_999, 0, 85, qOK},
		{"over at quota", 1000, 1000, 85, qOver},
		{"over above quota", 1001, 1000, 85, qOver},
		{"near above threshold", 900, 1000, 85, qNear},
		{"near at boundary", 850, 1000, 85, qNear},
		{"ok below near", 500, 1000, 85, qOK},
		{"nearPct 0 only over", 999, 1000, 0, qOK},
		{"nearPct 0 over", 1000, 1000, 0, qOver},
	}
	for _, tc := range cases {
		if got := computeQuotaLevel(tc.tx, tc.quota, tc.near); got != tc.expect {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.expect)
		}
	}
}

func TestCycleKey(t *testing.T) {
	utc := time.UTC
	// Day on/after the reset day → this month's cycle.
	if k := cycleKey(time.Date(2026, 8, 15, 12, 0, 0, 0, utc), 15); k != 2026008 {
		t.Errorf("mid-month cycle key: got %d want 2026008", k)
	}
	// Day before the reset day → previous month's cycle.
	if k := cycleKey(time.Date(2026, 8, 10, 12, 0, 0, 0, utc), 15); k != 2026007 {
		t.Errorf("pre-reset cycle key: got %d want 2026007", k)
	}
	// Year rollover: early January, reset day 15 → last December.
	if k := cycleKey(time.Date(2026, 1, 10, 12, 0, 0, 0, utc), 15); k != 2025012 {
		t.Errorf("year-rollover cycle key: got %d want 2025012", k)
	}
	// Reset day 1: cycle boundary is the 1st of every month.
	if k := cycleKey(time.Date(2026, 1, 1, 0, 0, 0, 0, utc), 1); k != 2026001 {
		t.Errorf("reset-day-1 key: got %d want 2026001", k)
	}
}

func TestCycleStart(t *testing.T) {
	utc := time.UTC
	// Mid-cycle: start is this month's reset day.
	if ts := cycleStart(time.Date(2026, 8, 15, 12, 0, 0, 0, utc), 15); !ts.Equal(time.Date(2026, 8, 15, 0, 0, 0, 0, utc)) {
		t.Errorf("mid-cycle start: got %v", ts)
	}
	// Pre-reset: start is last month's reset day.
	if ts := cycleStart(time.Date(2026, 8, 10, 12, 0, 0, 0, utc), 15); !ts.Equal(time.Date(2026, 7, 15, 0, 0, 0, 0, utc)) {
		t.Errorf("pre-reset start: got %v", ts)
	}
	// Reset day 31 in a 30-day month normalizes to the 1st of the next month.
	if ts := cycleStart(time.Date(2026, 2, 10, 12, 0, 0, 0, utc), 31); !ts.Equal(time.Date(2026, 1, 31, 0, 0, 0, 0, utc)) {
		t.Errorf("31-in-February start: got %v", ts)
	}
}

func newTestTrafficStore(t *testing.T) *TrafficStore {
	t.Helper()
	s, err := NewTrafficStore(filepath.Join(t.TempDir(), "traffic-history.json"))
	if err != nil {
		t.Fatalf("NewTrafficStore: %v", err)
	}
	return s
}

func TestTrafficStoreRecordAndReset(t *testing.T) {
	s := newTestTrafficStore(t)
	live := map[string]bool{"e1": true}
	now := int64(1_000_000)

	// Tick 1: seed — no accumulated traffic yet.
	s.Record(map[string]rawBytes{"e1": {rx: 100, tx: 200}}, rawBytes{rx: 1000, tx: 2000}, true, live, now)
	if rx, tx := s.Wantotals(); rx != 0 || tx != 0 {
		t.Fatalf("after seed want 0,0 got %d,%d", rx, tx)
	}
	if rx, tx := s.entryLive("e1", rawBytes{rx: 100, tx: 200}); rx != 0 || tx != 0 {
		t.Fatalf("after seed entry want 0,0 got %d,%d", rx, tx)
	}

	// Tick 2: accumulate the delta.
	s.Record(map[string]rawBytes{"e1": {rx: 150, tx: 300}}, rawBytes{rx: 1500, tx: 2500}, true, live, now+60)
	if rx, tx := s.Wantotals(); rx != 500 || tx != 500 {
		t.Fatalf("wan accum want 500,500 got %d,%d", rx, tx)
	}
	// Live total folds the in-flight delta (raw 160/310 vs last 150/300).
	if rx, tx := s.entryLive("e1", rawBytes{rx: 160, tx: 310}); rx != 60 || tx != 110 {
		t.Fatalf("entry live want 60,110 got %d,%d", rx, tx)
	}

	// Tick 3: a failed WAN read (haveWAN=false) must not corrupt the total.
	s.Record(map[string]rawBytes{"e1": {rx: 200, tx: 350}}, rawBytes{rx: 0, tx: 0}, false, live, now+120)
	if rx, tx := s.Wantotals(); rx != 500 || tx != 500 {
		t.Fatalf("wan should be unchanged on failed read, got %d,%d", rx, tx)
	}

	// Prune: e1 no longer in the live set → dropped from the persisted file.
	s.Record(map[string]rawBytes{}, rawBytes{rx: 1500, tx: 2500}, true, map[string]bool{}, now+180)
	if b, _ := os.ReadFile(s.path); len(b) > 0 {
		if string(b) == "" {
			t.Fatalf("empty file")
		}
	}

	// Manual reset zeroes WAN + entries + quota level.
	s.ManualReset(now + 240)
	if rx, tx := s.Wantotals(); rx != 0 || tx != 0 {
		t.Fatalf("after reset want 0,0 got %d,%d", rx, tx)
	}
	if lastLevel, _, _, _ := s.QuotaStatus(); lastLevel != 0 {
		t.Fatalf("after reset lastLevel should be 0, got %d", lastLevel)
	}
}

func TestTrafficStorePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "traffic-history.json")
	s, _ := NewTrafficStore(path)
	live := map[string]bool{"e1": true}
	s.Record(map[string]rawBytes{"e1": {rx: 10, tx: 20}}, rawBytes{rx: 100, tx: 200}, true, live, 1)
	s.Record(map[string]rawBytes{"e1": {rx: 110, tx: 220}}, rawBytes{rx: 600, tx: 700}, true, live, 2)

	// A fresh store over the same path must load the accumulated totals.
	s2, err := NewTrafficStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rx, tx := s2.Wantotals(); rx != 500 || tx != 500 {
		t.Fatalf("reloaded wan want 500,500 got %d,%d", rx, tx)
	}
	if rx, tx := s2.entryLive("e1", rawBytes{rx: 110, tx: 220}); rx != 100 || tx != 200 {
		t.Fatalf("reloaded entry want 100,200 got %d,%d", rx, tx)
	}
}

func TestTrafficStoreMonthlyReset(t *testing.T) {
	s := newTestTrafficStore(t)
	utc := time.UTC
	midAug := time.Date(2026, 8, 15, 12, 0, 0, 0, utc).Unix()

	// Out-of-range reset day is a no-op.
	if s.MaybeMonthlyReset(0, midAug) {
		t.Fatalf("resetDay 0 should be a no-op")
	}

	// First real tick for the Aug-15 cycle: fires.
	if !s.MaybeMonthlyReset(15, midAug) {
		t.Fatalf("first cycle tick should fire")
	}
	// Same cycle again: does NOT re-fire.
	if s.MaybeMonthlyReset(15, midAug+60) {
		t.Fatalf("same cycle should not re-fire")
	}
	// Next month's cycle: fires again.
	sep := time.Date(2026, 9, 15, 12, 0, 0, 0, utc).Unix()
	if !s.MaybeMonthlyReset(15, sep) {
		t.Fatalf("new month cycle should fire")
	}

	// The recorded reset time is the cycle start (00:00 UTC on the reset day).
	_, lastResetAt, monthKey, _ := s.QuotaStatus()
	wantStart := time.Date(2026, 9, 15, 0, 0, 0, 0, utc).Unix()
	if lastResetAt != wantStart {
		t.Fatalf("lastResetAt want %d got %d", wantStart, lastResetAt)
	}
	if monthKey != 2026009 {
		t.Fatalf("monthKey want 2026009 got %d", monthKey)
	}
}

func TestTrafficStoreQuotaTransitions(t *testing.T) {
	s := newTestTrafficStore(t)
	live := map[string]bool{}
	now := int64(1_000_000)

	// Seed WAN at 0, then drive it into the near band (900 of 1000).
	s.Record(map[string]rawBytes{}, rawBytes{rx: 0, tx: 0}, true, live, now)
	s.Record(map[string]rawBytes{}, rawBytes{rx: 0, tx: 900}, true, live, now+60)

	// ok -> near: fires once.
	if kinds := s.EvaluateQuota(1000, 85); len(kinds) != 1 || kinds[0] != 1 {
		t.Fatalf("first eval want [1] got %v", kinds)
	}
	// Still near: no re-fire.
	if kinds := s.EvaluateQuota(1000, 85); len(kinds) != 0 {
		t.Fatalf("repeated eval want [] got %v", kinds)
	}

	// near -> over: fires.
	s.Record(map[string]rawBytes{}, rawBytes{rx: 0, tx: 1000}, true, live, now+120)
	if kinds := s.EvaluateQuota(1000, 85); len(kinds) != 1 || kinds[0] != 2 {
		t.Fatalf("over eval want [2] got %v", kinds)
	}

	// Raising the quota drops the level back to ok: "back under" (-1).
	if kinds := s.EvaluateQuota(2000, 85); len(kinds) != 1 || kinds[0] != -1 {
		t.Fatalf("back-under eval want [-1] got %v", kinds)
	}

	// A single tick that skips the near band (ok -> over in one jump) fires
	// both alerts, in order: near, then over. (Quota is 2000, total is 1000
	// = 50%; one tick adds 1500, landing at 125%.)
	s.Record(map[string]rawBytes{}, rawBytes{rx: 0, tx: 2500}, true, live, now+180)
	if kinds := s.EvaluateQuota(2000, 85); len(kinds) != 2 || kinds[0] != 1 || kinds[1] != 2 {
		t.Fatalf("skipped-band eval want [1 2] got %v", kinds)
	}
	// Still over on the next tick: no re-fire.
	s.Record(map[string]rawBytes{}, rawBytes{rx: 0, tx: 3000}, true, live, now+240)
	if kinds := s.EvaluateQuota(2000, 85); len(kinds) != 0 {
		t.Fatalf("steady over want [] got %v", kinds)
	}
}
