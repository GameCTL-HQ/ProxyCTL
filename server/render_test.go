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

// Every install that hasn't opted into tunnel-only must see the chain
// created+flushed (harmless, always-idempotent no-op) but NO jump into
// INPUT and no gate rules — an install that never touches SSH lockdown
// must never have port 22 firewalled.
func TestDropletNATScript_NoSSHGateByDefault(t *testing.T) {
	in := DefaultInfra()
	script := RenderDropletNATScript(in, nil)
	if !strings.Contains(script, "iptables -N PROXYCTL-SSH") {
		t.Errorf("expected the PROXYCTL-SSH chain to always be created (idempotent no-op when unused):\n%s", script)
	}
	if strings.Contains(script, "-I INPUT 1") {
		t.Errorf("SSH firewall gate must not be wired into INPUT when SSHTunnelOnly is false:\n%s", script)
	}
}

// Once SSH is restricted to tunnel-only, the firewall gate must accept
// the control tunnel AND every personal-access peer, then drop everything
// else — and must be wired into INPUT ahead of anything else so nothing
// else in the chain gets a chance to accept it first.
func TestDropletNATScript_SSHGateWhenTunnelOnly(t *testing.T) {
	in := DefaultInfra()
	in.SSHTunnelOnly = true
	in.PersonalAccessPeers = []PersonalAccessPeerInfo{
		{Name: "Laptop", PubKey: "pPUBKEY", IP: "10.8.0.244"},
	}
	script := RenderDropletNATScript(in, nil)
	for _, want := range []string{
		"iptables -I INPUT 1 -p tcp --dport 22 -m state --state NEW -j PROXYCTL-SSH",
		"iptables -A PROXYCTL-SSH -s " + controlTunnelIP + "/32 -j RETURN",
		"iptables -A PROXYCTL-SSH -s 10.8.0.244/32 -j RETURN",
		"iptables -A PROXYCTL-SSH -j DROP",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("SSH firewall gate missing %q\n---\n%s", want, script)
		}
	}
}

// Turning tunnel-only back off must remove a previous apply's INPUT jump
// (an empty custom chain is an inert pass-through, but the stale jump
// should still go away) rather than leaving the gate silently in place.
func TestDropletNATScript_SSHGateRemovedWhenTunnelOnlyOff(t *testing.T) {
	in := DefaultInfra()
	in.SSHTunnelOnly = false
	script := RenderDropletNATScript(in, nil)
	if !strings.Contains(script, "iptables -D INPUT -p tcp --dport 22 -m state --state NEW -j PROXYCTL-SSH") {
		t.Errorf("expected a best-effort removal of a stale INPUT jump when SSHTunnelOnly is false:\n%s", script)
	}
	if strings.Contains(script, "-j DROP") {
		t.Errorf("no DROP rule should be rendered when SSHTunnelOnly is false:\n%s", script)
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

// With no control-tunnel keypair generated yet (the common case for every
// install that hasn't opted in), the droplet's wg0.conf must render
// byte-for-byte as it did before this feature existed — no stray [Peer].
func TestDropletWG0OmitsControlPeerWhenUnconfigured(t *testing.T) {
	in := DefaultInfra()
	conf := RenderDropletWG0(in, nil)
	if strings.Contains(conf, "control tunnel") {
		t.Errorf("control peer rendered with no ControlPubKey set:\n%s", conf)
	}
}

// Once ProxyCTL has a control-tunnel keypair, the droplet's wg0.conf must
// carry its peer — scoped to its own reserved /32, same as a game peer.
func TestDropletWG0IncludesControlPeer(t *testing.T) {
	in := DefaultInfra()
	in.ControlPubKey = "cPUBKEY0000000000000000000000000000000000="
	conf := RenderDropletWG0(in, nil)
	for _, want := range []string{
		"PublicKey = cPUBKEY0000000000000000000000000000000000=",
		"AllowedIPs = " + controlTunnelIP + "/32",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("wg0.conf missing %q\n---\n%s", want, conf)
		}
	}
}

// Each personal-access device gets its own [Peer] stanza scoped to its
// own reserved /32 — never dropping the control peer or any other device.
func TestDropletWG0IncludesPersonalAccessPeers(t *testing.T) {
	in := DefaultInfra()
	in.ControlPubKey = "cPUBKEY0000000000000000000000000000000000="
	in.PersonalAccessPeers = []PersonalAccessPeerInfo{
		{Name: "Laptop", PubKey: "lPUBKEY0000000000000000000000000000000000=", IP: "10.8.0.244"},
		{Name: "Phone", PubKey: "phPUBKEY000000000000000000000000000000000=", IP: "10.8.0.245"},
	}
	conf := RenderDropletWG0(in, nil)
	for _, want := range []string{
		"AllowedIPs = " + controlTunnelIP + "/32", // control peer still present
		"PublicKey = lPUBKEY0000000000000000000000000000000000=",
		"AllowedIPs = 10.8.0.244/32",
		"PublicKey = phPUBKEY000000000000000000000000000000000=",
		"AllowedIPs = 10.8.0.245/32",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("wg0.conf missing %q\n---\n%s", want, conf)
		}
	}
}

// The client-side control tunnel conf must dial the droplet's public IP
// (not its own tunnel address) and reach ONLY the droplet's own /32 —
// never the whole 10.8.0.0/24 subnet.
func TestRenderControlTunnelWG0(t *testing.T) {
	in := DefaultInfra()
	conf := RenderControlTunnelWG0(in, "cPRIVKEY")
	for _, want := range []string{
		"Address = " + controlTunnelIP + "/32",
		"PrivateKey = cPRIVKEY",
		"Endpoint = " + in.DropletPublicIP + ":51820",
		"AllowedIPs = " + in.DropletWGIP + "/32",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("control tunnel wg0.conf missing %q\n---\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "/24") {
		t.Errorf("control tunnel must be scoped to a single /32, not the subnet:\n%s", conf)
	}
}
