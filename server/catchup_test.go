package main

import (
	"os"
	"path/filepath"
	"testing"

	"encoding/json"
)

// manualApplierTest is a minimal Applier (no SSHApplier capabilities) so
// catchUpWork / gatewayWorkSet can be exercised without a cluster: liveness
// must degrade to "no signal", not to "nothing is deployed".
type manualApplierTest struct{}

func (manualApplierTest) Mode() string   { return "manual" }
func (manualApplierTest) Render(in Infra, entries []*Entry) Rendered { return Rendered{} }
func (manualApplierTest) Apply(in Infra, r Rendered) ApplyResult    { return ApplyResult{OK: true} }

// newCatchUpTestAPI builds an API with a real on-disk Store and a
// manual (non-SSH) applier, so the liveness half of the work set must be a
// no-op "no signal" rather than a crash or a false "everything missing".
func newCatchUpTestAPI(t *testing.T, entries ...*Entry) *API {
	t.Helper()
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "entries.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := st.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	a := &API{store: st, applier: manualApplierTest{},
		infra:       Infra{K8sNamespace: "proxyctl"},
		appliedPath: filepath.Join(dir, "applied.hash"),
	}
	return a
}

func writeSnap(t *testing.T, a *API, entries []Entry) {
	t.Helper()
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.appliedSnapPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The mid-apply creation race: entry "late" exists in the store but the
// snapshot (and the pass) only know "first". catchUpWork must report the
// late entry as work — and even without any cluster signal (manual applier)
// it must, because new-entry detection doesn't depend on liveness.
func TestCatchUpWorkDetectsMidApplyCreation(t *testing.T) {
	first := &Entry{ID: "first", Name: "factorio Arena", TargetIP: "10.8.0.2",
		Enabled: true, TunnelIP: "10.8.0.2", GatewayPubKey: "key1",
		Ports: []PortSpec{{Port: 30077, Proto: "udp"}}}
	late := &Entry{ID: "late", Name: "cs2 Arena", TargetIP: "10.8.0.3",
		Enabled: true, TunnelIP: "10.8.0.3",
		Ports: []PortSpec{{Port: 27015, Proto: "udp"}}}
	a := newCatchUpTestAPI(t, first, late)
	// The pass only ran against `first` (snapshot predates `late`).
	ran := map[string]bool{"first": true}

	work, newIDs, have := a.catchUpWork(ran)
	if have {
		t.Error("manual applier must not claim a liveness signal")
	}
	if !work["late"] || !newIDs["late"] {
		t.Errorf("late entry must be detected as catch-up work, got work=%v newIDs=%v", work, newIDs)
	}
	if work["first"] {
		t.Error("an in-pass entry with no liveness signal must not be re-forced")
	}
}

// An entry that was in the pass and is NOT in the snapshot (created earlier
// in the same apply window but rendered): with no liveness signal it is in
// `ran`, so it must NOT be re-forced — no catch-up loop.
func TestCatchUpWorkNoLoopWhenClusterUnknown(t *testing.T) {
	a := newCatchUpTestAPI(t, &Entry{ID: "x", Name: "valheim Arena", TargetIP: "10.43.0.1",
		Enabled: true, TunnelIP: "10.8.0.4", GatewayPubKey: "k",
		Ports: []PortSpec{{Port: 2456, Proto: "udp"}}})
	work, _, have := a.catchUpWork(map[string]bool{"x": true})
	if have {
		t.Error("no liveness signal expected")
	}
	if len(work) != 0 {
		t.Errorf("in-pass entry must not be re-forced without a liveness signal, got %v", work)
	}
}

// Liveness half: with an SSHApplier in play, an entry that IS in the pass
// but whose gateway Deployment is gone gets re-forced. We can't shell out
// to a cluster in a unit test, so assert the degradation contract directly:
// gatewayWorkSet with a manual applier must NOT force in-sync entries just
// because it can't see the cluster.
func TestGatewayWorkSetLivenessNoFalseForces(t *testing.T) {
	inSync := &Entry{ID: "in-sync", Name: "factorio Arena", TargetIP: "10.8.0.2",
		Enabled: true, TunnelIP: "10.8.0.2", GatewayPubKey: "k1",
		Ports: []PortSpec{{Port: 30077, Proto: "udp"}}}
	a := newCatchUpTestAPI(t, inSync)
	writeSnap(t, a, []Entry{*inSync}) // snapshot says in sync

	work := a.gatewayWorkSet(false)
	if len(work) != 0 {
		t.Errorf("snapshot-in-sync entry must not be forced when the cluster can't be queried, got %v", work)
	}
}

// But a NEW entry (absent from the snapshot) must still be forced — the
// catch-up's fallback detection path.
func TestGatewayWorkSetNewEntryStillForced(t *testing.T) {
	old := &Entry{ID: "old", Name: "factorio Arena", TargetIP: "10.8.0.2",
		Enabled: true, TunnelIP: "10.8.0.2", GatewayPubKey: "k1",
		Ports: []PortSpec{{Port: 30077, Proto: "udp"}}}
	nw := &Entry{ID: "new", Name: "cs2 Arena", TargetIP: "10.8.0.3",
		Enabled: true, TunnelIP: "10.8.0.3",
		Ports: []PortSpec{{Port: 27015, Proto: "udp"}}}
	a := newCatchUpTestAPI(t, old, nw)
	writeSnap(t, a, []Entry{*old})

	work := a.gatewayWorkSet(false)
	if !work["new"] {
		t.Error("new entry must be in the work set")
	}
	if work["old"] {
		t.Error("unchanged entry must not be in the work set")
	}
}
