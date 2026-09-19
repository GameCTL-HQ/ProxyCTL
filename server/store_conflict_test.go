package main

import (
	"fmt"
	"strings"
	"testing"
)

func conflictEntry(id, name string, enabled bool, ports ...PortSpec) *Entry {
	return &Entry{ID: id, Name: name, Enabled: enabled,
		TargetIP: "10.43.0.10", Ports: ports}
}

// One public port can only reach one target: a second ENABLED entry claiming
// an already-routed port must be rejected at save time, naming the holder —
// on the droplet it would just silently lose to iptables first-match.
func TestPutRejectsCrossEntryPortConflicts(t *testing.T) {
	s, err := NewStore(t.TempDir() + "/entries.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(conflictEntry("a", "livekit", true,
		PortSpec{Port: 443, Proto: "tcp"})); err != nil {
		t.Fatal(err)
	}

	// Same port, same proto → rejected, naming the conflicting entry.
	err = s.Put(conflictEntry("b", "livekit-media", true,
		PortSpec{Port: 443, Proto: "tcp"}))
	if err == nil || !strings.Contains(err.Error(), `"livekit"`) {
		t.Fatalf("want conflict naming livekit, got %v", err)
	}
	// "both" overlaps a single-proto claim.
	if err := s.Put(conflictEntry("b", "livekit-media", true,
		PortSpec{Port: 443, Proto: "both"})); err == nil {
		t.Fatal(`"both" must conflict with an existing tcp claim`)
	}
	// Different proto on the same port is fine (tcp vs udp).
	if err := s.Put(conflictEntry("b", "livekit-media", true,
		PortSpec{Port: 443, Proto: "udp"})); err != nil {
		t.Fatalf("udp beside tcp on the same port should be allowed: %v", err)
	}
	// Disjoint ports are fine — this is the shared-hostname LiveKit split.
	if err := s.Put(conflictEntry("c", "livekit-rtc", true,
		PortSpec{Port: 7881, Proto: "both"})); err != nil {
		t.Fatalf("disjoint ports must not conflict: %v", err)
	}
	// A DISABLED entry may hold conflicting config (renders nothing)…
	if err := s.Put(conflictEntry("d", "staging", false,
		PortSpec{Port: 443, Proto: "tcp"})); err != nil {
		t.Fatalf("disabled entry should save regardless: %v", err)
	}
	// …but enabling it re-checks and refuses.
	d, _ := s.Get("d")
	d.Enabled = true
	if err := s.Put(d); err == nil {
		t.Fatal("enabling a conflicting entry must be rejected")
	}
	// Updating an entry doesn't conflict with itself.
	a2, _ := s.Get("a")
	a2.Name = "livekit-signal"
	if err := s.Put(a2); err != nil {
		t.Fatalf("self-update must not self-conflict: %v", err)
	}
}

// The control tunnel's fixed address must never be handed to a game entry
// — regression guard for the 2026-07-21 control-tunnel work (allocating it
// dynamically would let a game gateway collide with ProxyCTL's own peer).
func TestAllocTunnelIPNeverReturnsControlIP(t *testing.T) {
	s, err := NewStore(t.TempDir() + "/entries.json")
	if err != nil {
		t.Fatal(err)
	}
	// Fill every address below the reserved one so the allocator is
	// forced to either return controlTunnelIP or fail — either result
	// exercises the guard (a silent skip that still finds nothing free
	// would be a bug too).
	for h := 2; h <= 253; h++ {
		id := fmt.Sprintf("e%d", h)
		e := conflictEntry(id, id, true, PortSpec{Port: 20000 + h, Proto: "tcp"})
		e.TunnelIP = fmt.Sprintf("10.8.0.%d", h)
		if err := s.Put(e); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	s.mu.Lock()
	ip, err := s.allocTunnelIPLocked("new-entry")
	s.mu.Unlock()
	if err == nil && ip == controlTunnelIP {
		t.Fatalf("allocator handed out the reserved control-tunnel IP %s to a game entry", controlTunnelIP)
	}
}

// Same guard for the operator's personal-access peer range.
func TestAllocTunnelIPNeverReturnsPersonalAccessRange(t *testing.T) {
	s, err := NewStore(t.TempDir() + "/entries.json")
	if err != nil {
		t.Fatal(err)
	}
	// Fill every address below the reserved range, leaving .244-.254
	// apparently free — the allocator must skip the WHOLE range, not just
	// find "the next free slot" inside it.
	for h := 2; h < personalAccessIPLow; h++ {
		id := fmt.Sprintf("e%d", h)
		e := conflictEntry(id, id, true, PortSpec{Port: 20000 + h, Proto: "tcp"})
		e.TunnelIP = fmt.Sprintf("10.8.0.%d", h)
		if err := s.Put(e); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	s.mu.Lock()
	ip, err := s.allocTunnelIPLocked("new-entry")
	s.mu.Unlock()
	if err == nil {
		var h int
		fmt.Sscanf(ip, "10.8.0.%d", &h)
		if h >= personalAccessIPLow {
			t.Fatalf("allocator handed out a reserved personal-access-range IP %s to a game entry", ip)
		}
	}
}
