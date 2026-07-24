package main

import (
	"errors"
	"strings"
	"testing"
)

// A remapped port must produce its own gateway rule pair — DNAT to
// TargetIP:targetPort and a FORWARD accept on the TARGET port (the port
// as it exists after the DNAT rewrite) — while passthrough ports keep
// sharing one multiport rule keyed on the public ports.
func TestGatewayRulesRemapMixedWithPassthrough(t *testing.T) {
	in := DefaultInfra()
	e := &Entry{
		Name: "l4d2", Enabled: true, TunnelIP: "10.8.0.7", TargetIP: "10.43.0.20",
		Ports: []PortSpec{
			{Port: 27017, Proto: "udp", TargetPort: 27015}, // remapped
			{Port: 27020, Proto: "udp"},                    // passthrough
		},
	}
	up, _ := gatewayRulesForEntry(in, e)
	script := strings.Join(up, "\n")
	for _, want := range []string{
		"--dport 27017 -j DNAT --to-destination 10.43.0.20:27015",
		"-d 10.43.0.20 -p udp --dport 27015 -j ACCEPT",
		"--dports 27020 -j DNAT --to-destination 10.43.0.20",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("gateway rules missing %q\n---\n%s", want, script)
		}
	}
	// The passthrough multiport DNAT must not carry a :port (that would
	// collapse the group onto one target port).
	if strings.Contains(script, "--dports 27020 -j DNAT --to-destination 10.43.0.20:") {
		t.Errorf("passthrough DNAT must not pin a target port:\n%s", script)
	}
}

// The droplet hop always matches the PUBLIC port — a remap is invisible
// there by design (the packet crossing the tunnel still carries the port
// players typed).
func TestDropletNATUsesPublicPortForRemap(t *testing.T) {
	in := DefaultInfra()
	e := &Entry{
		Name: "l4d2", Enabled: true, TunnelIP: "10.8.0.7", TargetIP: "10.43.0.20",
		Ports: []PortSpec{{Port: 27017, Proto: "udp", TargetPort: 27015}},
	}
	script := RenderDropletNATScript(in, []*Entry{e})
	if !strings.Contains(script, "--dports 27017") {
		t.Errorf("droplet rules must match the public port 27017:\n%s", script)
	}
	if strings.Contains(script, "27015") {
		t.Errorf("the target port must never appear in droplet rules:\n%s", script)
	}
}

// Two enabled entries delivering to the same TARGET port must not
// conflict — contested is the public port only. That IS the feature:
// two Source games both on 27015 in-cluster, one published remapped.
func TestRemapAvoidsPublicPortConflict(t *testing.T) {
	s, err := NewStore(t.TempDir() + "/entries.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(conflictEntry("a", "cs2", true, PortSpec{Port: 27015, Proto: "udp"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(conflictEntry("b", "l4d2", true,
		PortSpec{Port: 27017, Proto: "udp", TargetPort: 27015})); err != nil {
		t.Fatalf("remapped entry must not conflict: %v", err)
	}
	// But the same public port still conflicts even when remapped away.
	err = s.Put(conflictEntry("c", "abiotic", true,
		PortSpec{Port: 27015, Proto: "udp", TargetPort: 27099}))
	var pc *PortConflictError
	if !errors.As(err, &pc) {
		t.Fatalf("want *PortConflictError, got %v", err)
	}
	if pc.Port != 27015 || pc.Holder != "cs2" {
		t.Errorf("conflict details wrong: %+v", pc)
	}
	// Suggestion must be a genuinely free public port, not 27017 (taken
	// by l4d2's remapped claim) and not the contested 27015.
	if pc.Suggested == 0 || pc.Suggested == 27015 || pc.Suggested == 27017 {
		t.Errorf("bad suggestion %d — must skip both claimed public ports", pc.Suggested)
	}
}

// The suggestion never lands on the droplet's own WireGuard listen port.
func TestSuggestFreePortSkipsWireGuard(t *testing.T) {
	s, err := NewStore(t.TempDir() + "/entries.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(conflictEntry("a", "wgish", true, PortSpec{Port: 51819, Proto: "udp"})); err != nil {
		t.Fatal(err)
	}
	err = s.Put(conflictEntry("b", "clash", true, PortSpec{Port: 51819, Proto: "udp"}))
	var pc *PortConflictError
	if !errors.As(err, &pc) {
		t.Fatalf("want *PortConflictError, got %v", err)
	}
	if pc.Suggested == 51820 {
		t.Error("suggestion must skip the droplet's WireGuard port 51820")
	}
}

func TestRenderSummaryShowsRemap(t *testing.T) {
	in := DefaultInfra()
	e := &Entry{
		Name: "l4d2", Subdomain: "l4d2.example.com", Enabled: true,
		TunnelIP: "10.8.0.7", TargetIP: "10.43.0.20",
		Ports: []PortSpec{{Port: 27017, Proto: "udp", TargetPort: 27015}},
	}
	sum := RenderSummary(in, []*Entry{e})
	if !strings.Contains(sum, "27017->27015/udp") {
		t.Errorf("summary should show the remap, got:\n%s", sum)
	}
}
