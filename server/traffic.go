package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Persistent traffic accounting — the byte counters that survive a ProxyCTL
// restart, a peer re-key, and an entry being disabled/re-enabled.
//
// Why it's needed: `wg show wg0` and the droplet's /sys WAN counters are
// both monotonic *while the interface/peer lives*, but they reset to 0 on a
// peer re-key, an entry disable/re-enable, a droplet reboot, or a ProxyCTL
// restart (in which case we simply lose the "last seen" reference). The 10s
// traffic sampler therefore ACCUMULATES deltas each tick and persists the
// running total + the last-seen raw counter to /data (atomic tmp+rename, same
// convention as every other store). Display = persisted total + the in-flight
// delta since the last tick, so the UI's 5s poll still moves smoothly.
//
// Only the WAN *transmit* (outbound) total counts against the quota — DO
// bills egress and inbound is free. Inbound is still recorded and shown, but
// it never affects the quota bar or the alerts.

// counters is one running total of a possibly-resetting byte counter pair.
type counters struct {
	RxAccum int64 `json:"rxAccum"`
	TxAccum int64 `json:"txAccum"`
	LastRx  int64 `json:"lastRx"`
	LastTx  int64 `json:"lastTx"`
	Seeded  bool  `json:"seeded"`
}

// observe folds one raw counter reading into the running total.
//   - first sight (Seeded==false): seed — total starts at 0, remember the raw
//     value as the baseline. We deliberately do NOT count the pre-existing
//     bytes (a peer that already carried 50 GiB before ProxyCTL learned to
//     watch it shouldn't be charged for that history).
//   - monotonic (raw >= last): total += raw - last.
//   - reset/wrap (raw < last): the counter was zeroed (re-key / reboot), so
//     the full new reading is fresh traffic — total += raw.
func (c counters) observe(rawRx, rawTx int64) counters {
	if !c.Seeded {
		return counters{LastRx: rawRx, LastTx: rawTx, Seeded: true}
	}
	out := c
	if rawRx >= c.LastRx {
		out.RxAccum += rawRx - c.LastRx
	} else {
		out.RxAccum += rawRx
	}
	if rawTx >= c.LastTx {
		out.TxAccum += rawTx - c.LastTx
	} else {
		out.TxAccum += rawTx
	}
	out.LastRx, out.LastTx = rawRx, rawTx
	return out
}

// liveTotal adds the in-flight delta since the last persisted tick so a fast
// poller (the 5s stats endpoint) shows a total that moves, not one that steps
// every 10s. It is a pure read — the sampler owns Last*/Accum*.
func (c counters) liveTotal(rawRx, rawTx int64) (rxTotal, txTotal int64) {
	drx, dtx := int64(0), int64(0)
	if c.Seeded {
		if rawRx > c.LastRx {
			drx = rawRx - c.LastRx
		}
		if rawTx > c.LastTx {
			dtx = rawTx - c.LastTx
		}
	}
	return c.RxAccum + drx, c.TxAccum + dtx
}

type rawBytes struct{ rx, tx int64 }

// quotaState tracks the last alerted level + the current billing cycle so a
// restart never re-fires an alert and the monthly auto-reset fires exactly
// once per cycle.
type quotaState struct {
	LastLevel   int   `json:"lastLevel"` // 0=ok 1=near 2=over
	LastResetAt int64 `json:"lastResetAt"`
	MonthKey    int64 `json:"monthKey"` // yyyymm of the cycle start (0 = never)
}

type trafficSnapshot struct {
	Version int                    `json:"version"`
	Entries map[string]counters    `json:"entries"` // Entry.ID -> counters
	WAN     counters               `json:"wan"`     // droplet WAN interface
	Quota   quotaState             `json:"quota"`
}

// TrafficStore persists the accumulated per-entry + WAN byte totals and the
// quota-alert state. One goroutine (the sampler) writes; the stats/quota
// handlers read.
type TrafficStore struct {
	path string
	mu   sync.Mutex
	snap trafficSnapshot
}

func NewTrafficStore(path string) (*TrafficStore, error) {
	s := &TrafficStore{path: path, snap: trafficSnapshot{Version: 1, Entries: map[string]counters{}}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.snap); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if s.snap.Entries == nil {
		s.snap.Entries = map[string]counters{}
	}
	return s, nil
}

func (s *TrafficStore) persistLocked() {
	if s.path == "" {
		return
	}
	b, err := json.MarshalIndent(s.snap, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// Record applies one sampler tick: accumulate the observed per-entry byte
// counters, the droplet WAN counters (only when `haveWAN` — a failed WAN read
// must not corrupt the seed), prune entries that no longer exist, and persist.
// `entries` is keyed by Entry.ID (only the enabled entries actually present in
// the dump this tick). `live` is the set of all current Entry.IDs, used to drop
// deleted ones so the file doesn't grow without bound.
func (s *TrafficStore) Record(entries map[string]rawBytes, wan rawBytes, haveWAN bool, live map[string]bool, now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, raw := range entries {
		s.snap.Entries[id] = s.snap.Entries[id].observe(raw.rx, raw.tx)
	}
	if haveWAN {
		s.snap.WAN = s.snap.WAN.observe(wan.rx, wan.tx)
	}
	for id := range s.snap.Entries {
		if !live[id] {
			delete(s.snap.Entries, id)
		}
	}
	s.persistLocked()
}

// entryLive returns the persisted total plus the in-flight delta for one
// entry, for the fast stats poll. A missing/never-seen entry yields zero.
func (s *TrafficStore) entryLive(id string, raw rawBytes) (rxTotal, txTotal int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap.Entries[id].liveTotal(raw.rx, raw.tx)
}

// Wantotals returns the persisted WAN totals (no in-flight delta — at most
// one 10s sampler tick stale, which is fine for alert evaluation).
func (s *TrafficStore) Wantotals() (rx, tx int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap.WAN.RxAccum, s.snap.WAN.TxAccum
}

// WanLive returns the WAN totals plus the in-flight delta since the last
// persisted tick, so the 5s stats poll shows a moving number (mirrors
// entryLive for the WAN counters). Pure read — the sampler owns Last*/Accum*.
func (s *TrafficStore) WanLive(raw rawBytes) (rx, tx int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap.WAN.liveTotal(raw.rx, raw.tx)
}

// QuotaStatus returns the quota-alert state plus the current WAN tx total.
func (s *TrafficStore) QuotaStatus() (lastLevel int, lastResetAt int64, monthKey int64, txTotal int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap.Quota.LastLevel, s.snap.Quota.LastResetAt, s.snap.Quota.MonthKey, s.snap.WAN.TxAccum
}

// ManualReset zeroes WAN + per-entry totals and the quota level, then
// persists. The next sampler tick re-seeds the baselines from whatever the
// raw counters read at that moment.
func (s *TrafficStore) ManualReset(now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.WAN = counters{}
	s.snap.Entries = map[string]counters{}
	s.snap.Quota.LastLevel = 0
	s.snap.Quota.LastResetAt = now
	s.snap.Quota.MonthKey = 0
	s.persistLocked()
}

// MaybeMonthlyReset rolls the billing cycle when `now` has crossed into a new
// day-of-month cycle. It uses the sampler's own tick (no extra SSH) and fires
// exactly once per cycle, recording that cycle's start time even if the pod
// was down on the exact reset day. Returns true if a reset happened.
func (s *TrafficStore) MaybeMonthlyReset(resetDay int, now int64) bool {
	if resetDay < 1 || resetDay > 31 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := time.Unix(now, 0).UTC()
	key := cycleKey(t, resetDay)
	if s.snap.Quota.MonthKey == key {
		return false
	}
	s.snap.Quota.MonthKey = key
	s.snap.Quota.LastResetAt = cycleStart(t, resetDay).Unix()
	s.snap.Quota.LastLevel = 0
	s.snap.WAN = counters{}
	s.snap.Entries = map[string]counters{}
	s.persistLocked()
	return true
}

// EvaluateQuota computes the current quota level from the accumulated WAN tx
// total and, if it differs from the recorded level, records the transition.
// Returns the alert kinds to send, in order: 1 = now near, 2 = now over,
// -1 = back under. An upward transition fires EVERY threshold it crossed, so
// a single sampler tick that jumps straight from under 85% to over 100% still
// sends both the near and the over alert (a small test quota can be blown
// through in one 10s tick). Empty slice = no alert.
func (s *TrafficStore) EvaluateQuota(quotaBytes int64, nearPct int) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	lvl := computeQuotaLevel(s.snap.WAN.TxAccum, quotaBytes, nearPct)
	old := s.snap.Quota.LastLevel
	if int(lvl) == old {
		return nil
	}
	s.snap.Quota.LastLevel = int(lvl)
	s.persistLocked()
	var kinds []int
	if lvl > quotaLevel(old) {
		for l := quotaLevel(old) + 1; l <= lvl; l++ {
			if l == qNear {
				kinds = append(kinds, 1)
			} else if l == qOver {
				kinds = append(kinds, 2)
			}
		}
	} else if lvl == qOK {
		kinds = append(kinds, -1)
	} else {
		kinds = append(kinds, 1) // e.g. quota raised back into the near band
	}
	return kinds
}

type quotaLevel int

const (
	qOK quotaLevel = iota
	qNear
	qOver
)

// computeQuotaLevel maps a WAN tx total + config to a level. A zero quota
// disables the feature (always ok). nearPct of 0 is treated as no nearing
// threshold (only over-quota alerts).
func computeQuotaLevel(txTotal, quotaBytes int64, nearPct int) quotaLevel {
	if quotaBytes <= 0 {
		return qOK
	}
	if txTotal >= quotaBytes {
		return qOver
	}
	if nearPct > 0 && txTotal*100 >= quotaBytes*int64(nearPct) {
		return qNear
	}
	return qOK
}

// cycleKey is the yyyymm of the monthly cycle containing `t`, where a cycle
// begins on the resetDay-th of each month. Before this month's reset day the
// cycle started last month.
func cycleKey(t time.Time, resetDay int) int64 {
	base := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	if t.Day() < resetDay {
		base = base.AddDate(0, -1, 0)
	}
	return int64(base.Year())*1000 + int64(base.Month())
}

// cycleStart is the timestamp of the most recent cycle start at-or-before `t`.
// time.Date normalizes a day longer than the month (e.g. the 31st of a 30-day
// month rolls into the next month), which is the desired "first day on/after".
func cycleStart(t time.Time, resetDay int) time.Time {
	base := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	if t.Day() < resetDay {
		base = base.AddDate(0, -1, 0)
	}
	return time.Date(base.Year(), base.Month(), resetDay, 0, 0, 0, 0, time.UTC)
}
