package main

import (
	"strings"
	"testing"
)

func TestMultiport(t *testing.T) {
	if got := multiport([]int{27015, 27016, 27017}); got != "27015,27016,27017" {
		t.Fatalf("multiport = %q, want 27015,27016,27017", got)
	}
	if got := multiport(nil); got != "" {
		t.Fatalf("multiport(nil) = %q, want empty", got)
	}
}

func TestEnabledFiltersAndSorts(t *testing.T) {
	in := []*Entry{
		{ID: "3", Name: "valheim", Enabled: true},
		{ID: "1", Name: "ark", Enabled: true},
		{ID: "2", Name: "minecraft", Enabled: false},
	}
	got := enabled(in)
	if len(got) != 2 {
		t.Fatalf("enabled len = %d, want 2 (disabled minecraft dropped)", len(got))
	}
	if got[0].Name != "ark" || got[1].Name != "valheim" {
		t.Fatalf("enabled order = %s,%s; want ark,valheim", got[0].Name, got[1].Name)
	}
	for _, e := range got {
		if !e.Enabled {
			t.Fatalf("disabled entry %q leaked into enabled()", e.Name)
		}
	}
}

// The droplet NAT script is the security boundary: every public port must DNAT
// to the entry's OWN tunnel IP and nothing else. Assert the shape directly.
func TestDropletNATScriptRendersDNAT(t *testing.T) {
	in := DefaultInfra()
	e := &Entry{
		Name: "valheim", Subdomain: "v.example.com", Enabled: true,
		TunnelIP: "10.8.0.5",
		Ports:    []PortSpec{{Port: 2456, Proto: "udp"}, {Port: 2457, Proto: "udp"}},
	}
	script := RenderDropletNATScript(in, []*Entry{e})
	for _, want := range []string{
		"DNAT --to-destination 10.8.0.5",
		"--dports 2456,2457",
		"MASQUERADE",
		"10.8.0.5/32",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("NAT script missing %q\n---\n%s", want, script)
		}
	}
}

func TestDropletNATSkipsEntryWithoutTunnelIP(t *testing.T) {
	in := DefaultInfra()
	e := &Entry{Name: "x", Enabled: true, Ports: []PortSpec{{Port: 9999, Proto: "tcp"}}} // no TunnelIP
	if script := RenderDropletNATScript(in, []*Entry{e}); strings.Contains(script, "9999") {
		t.Errorf("entry without a TunnelIP must not render a DNAT rule:\n%s", script)
	}
}

// A disabled entry must never reach the public droplet rules.
func TestDisabledEntryNotExposed(t *testing.T) {
	in := DefaultInfra()
	entries := []*Entry{
		{Name: "secret", Enabled: false, TunnelIP: "10.8.0.9", Ports: []PortSpec{{Port: 31337, Proto: "tcp"}}},
	}
	if script := RenderDropletNATScript(in, enabled(entries)); strings.Contains(script, "31337") {
		t.Errorf("disabled entry must not be exposed in NAT rules:\n%s", script)
	}
}

func TestDropletWG0HasInterface(t *testing.T) {
	in := DefaultInfra()
	conf := RenderDropletWG0(in, nil)
	for _, want := range []string{"[Interface]", "ListenPort = 51820", "Address = 10.8.0.1/24"} {
		if !strings.Contains(conf, want) {
			t.Errorf("wg0.conf missing %q\n---\n%s", want, conf)
		}
	}
}
