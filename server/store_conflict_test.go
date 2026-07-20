package main

import (
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
