package main

import "testing"

// gatewayNeedsWork is the seam the fast apply hangs on: a nil work set
// means "reconcile everything" (full reconcile, or no snapshot to trust),
// otherwise only the listed entries.
func TestGatewayNeedsWork(t *testing.T) {
	a := &Entry{ID: "a"}
	b := &Entry{ID: "b"}

	if !gatewayNeedsWork(nil, a) || !gatewayNeedsWork(nil, b) {
		t.Error("nil work set must reconcile every entry")
	}

	work := map[string]bool{"a": true}
	if !gatewayNeedsWork(work, a) {
		t.Error("entry in the work set must be reconciled")
	}
	if gatewayNeedsWork(work, b) {
		t.Error("entry absent from the work set must be skipped")
	}

	// An empty (non-nil) set means genuinely nothing changed — skip all
	// gateway phases. The droplet phases still run; they're whole-set.
	if gatewayNeedsWork(map[string]bool{}, a) {
		t.Error("empty work set must skip every gateway")
	}
}

// The work set must never skip an entry whose public key is missing: the
// droplet peer list is built from entries that HAVE a key, so skipping one
// without a key would quietly drop it out of wg0.conf.
func TestChangedFieldsDrivesWorkSet(t *testing.T) {
	old := Entry{ID: "x", Name: "valheim", TargetIP: "10.43.0.1", Enabled: true,
		Ports: []PortSpec{{Port: 2456, Proto: "udp"}}}

	same := old
	same.GatewayPubKey = "key"
	if got := changedFields(old, same); len(got) > 0 {
		t.Errorf("an applier-filled pubkey must not read as an operator edit, got %v", got)
	}

	edited := same
	edited.Ports = []PortSpec{{Port: 2457, Proto: "udp"}}
	if got := changedFields(old, edited); len(got) == 0 {
		t.Error("a port change must mark the entry as needing work")
	}

	disabled := same
	disabled.Enabled = false
	if got := changedFields(old, disabled); len(got) == 0 {
		t.Error("disabling an entry must mark it as needing work")
	}
}
