package main

import "testing"

func TestParseUFWStatusNumbered_NotInstalled(t *testing.T) {
	installed, active, sshRules, total := parseUFWStatusNumbered("__UFW_NOT_INSTALLED__\n")
	if installed {
		t.Fatal("want installed=false")
	}
	if active {
		t.Fatal("want active=false")
	}
	if len(sshRules) != 0 || total != 0 {
		t.Fatalf("want no rules, got sshRules=%+v total=%d", sshRules, total)
	}
}

func TestParseUFWStatusNumbered_Inactive(t *testing.T) {
	installed, active, sshRules, _ := parseUFWStatusNumbered("Status: inactive\n")
	if !installed {
		t.Fatal("want installed=true")
	}
	if active {
		t.Fatal("want active=false")
	}
	if len(sshRules) != 0 {
		t.Fatalf("want no rules for inactive ufw, got %+v", sshRules)
	}
}

// Realistic `ufw status numbered` output: a port-22 rule (the leftover
// pre-ProxyCTL lockdown), an unrelated rule that must NOT be flagged, and
// an IPv6 duplicate of the SSH rule — all three should parse, only the
// two SSH ones should end up in sshRules.
func TestParseUFWStatusNumbered_MixedRules(t *testing.T) {
	out := "Status: active\n\n" +
		"     To                         Action      From\n" +
		"     --                         ------      ----\n" +
		"[ 1] 22/tcp                     ALLOW IN    203.0.113.42\n" +
		"[ 2] 8080/tcp                   ALLOW IN    Anywhere\n" +
		"[ 3] 22/tcp (v6)                ALLOW IN    Anywhere (v6)\n"
	installed, active, sshRules, total := parseUFWStatusNumbered(out)
	if !installed || !active {
		t.Fatalf("want installed=true active=true, got installed=%v active=%v", installed, active)
	}
	if total != 3 {
		t.Fatalf("want 3 total rules, got %d", total)
	}
	if len(sshRules) != 2 {
		t.Fatalf("want 2 ssh rules, got %d: %+v", len(sshRules), sshRules)
	}
	if sshRules[0].Number != 1 || sshRules[0].From != "203.0.113.42" {
		t.Errorf("want rule 1 from 203.0.113.42, got %+v", sshRules[0])
	}
	if sshRules[1].Number != 3 {
		t.Errorf("want second ssh rule to be number 3, got %+v", sshRules[1])
	}
	for _, r := range sshRules {
		if r.Number == 2 {
			t.Fatalf("unrelated 8080 rule must not be flagged as SSH-related: %+v", r)
		}
	}
}

// The named "OpenSSH" app profile is a common alternative to a literal
// "22/tcp" rule (e.g. `ufw allow OpenSSH`) — must be recognized too.
func TestParseUFWStatusNumbered_OpenSSHProfile(t *testing.T) {
	out := "Status: active\n\n" +
		"     To                         Action      From\n" +
		"     --                         ------      ----\n" +
		"[ 1] OpenSSH                    ALLOW IN    198.51.100.7\n"
	_, _, sshRules, _ := parseUFWStatusNumbered(out)
	if len(sshRules) != 1 || sshRules[0].From != "198.51.100.7" {
		t.Fatalf("want the OpenSSH rule recognized as SSH-related, got %+v", sshRules)
	}
}

func TestIsSSHRelatedUFWTo(t *testing.T) {
	yes := []string{"22", "22/tcp", "22/udp", "OpenSSH", "openssh", "22/tcp (v6)", "OpenSSH (v6)"}
	for _, to := range yes {
		if !isSSHRelatedUFWTo(to) {
			t.Errorf("want %q to be SSH-related", to)
		}
	}
	no := []string{"2222/tcp", "8080", "Anywhere", "222", "OpenSSH-extra"}
	for _, to := range no {
		if isSSHRelatedUFWTo(to) {
			t.Errorf("want %q to NOT be SSH-related", to)
		}
	}
}

func TestIsWideOpenUFWFrom(t *testing.T) {
	open := []string{"Anywhere", "anywhere", "Anywhere (v6)", "ANYWHERE (V6)"}
	for _, from := range open {
		if !isWideOpenUFWFrom(from) {
			t.Errorf("want %q to be wide-open", from)
		}
	}
	scoped := []string{"203.0.113.42", "198.51.100.0/24", ""}
	for _, from := range scoped {
		if isWideOpenUFWFrom(from) {
			t.Errorf("want %q to NOT be wide-open", from)
		}
	}
}

// The core safety-critical split: a wide-open port-22 rule must NEVER be
// offered for removal (see isWideOpenUFWFrom's doc comment for why —
// removing it broke real tunnel-sourced SSH, not just direct access), but
// its presence must still be reported so fixUFWSSH knows it doesn't need
// to add one back.
func TestSplitUFWSSHRules(t *testing.T) {
	rules := []ufwRule{
		{Number: 6, To: "22/tcp", From: "203.0.113.42"},
		{Number: 12, To: "22/tcp (v6)", From: "Anywhere (v6)"},
	}
	ipRestricted, hasWideOpen := splitUFWSSHRules(rules)
	if !hasWideOpen {
		t.Fatal("want hasWideOpen=true")
	}
	if len(ipRestricted) != 1 || ipRestricted[0].Number != 6 {
		t.Fatalf("want only rule 6 in ipRestricted, got %+v", ipRestricted)
	}
}

// The exact scenario that caused the real incident: BOTH matched rules
// are wide-open (no IP-restricted rule exists at all). ipRestricted must
// come back empty so fixUFWSSH has nothing to remove — this is what
// fixUFWSSH's "nothing to remove" early-return depends on.
func TestSplitUFWSSHRules_AllWideOpen(t *testing.T) {
	rules := []ufwRule{
		{Number: 6, To: "22/tcp", From: "Anywhere"},
		{Number: 12, To: "22/tcp (v6)", From: "Anywhere (v6)"},
	}
	ipRestricted, hasWideOpen := splitUFWSSHRules(rules)
	if !hasWideOpen {
		t.Fatal("want hasWideOpen=true")
	}
	if len(ipRestricted) != 0 {
		t.Fatalf("want zero rules offered for removal when all are wide-open, got %+v", ipRestricted)
	}
}

func TestSplitUFWSSHRules_NoWideOpen(t *testing.T) {
	rules := []ufwRule{{Number: 6, To: "22/tcp", From: "203.0.113.42"}}
	ipRestricted, hasWideOpen := splitUFWSSHRules(rules)
	if hasWideOpen {
		t.Fatal("want hasWideOpen=false")
	}
	if len(ipRestricted) != 1 {
		t.Fatalf("want the one IP-restricted rule, got %+v", ipRestricted)
	}
}

// The snapshot/restore round trip fixUFWSSH relies on to undo a bad
// change exactly, rather than reconstructing `ufw allow`/`delete` syntax.
func TestSplitUFWSnapshot_RoundTrip(t *testing.T) {
	raw := "echo __V4__; cat x\n__V4__\nline one\nline two\n__V6__\nv6 line one\n"
	v4, v6 := splitUFWSnapshot(raw)
	if v4 != "line one\nline two" {
		t.Errorf("want v4 = %q, got %q", "line one\nline two", v4)
	}
	if v6 != "v6 line one" {
		t.Errorf("want v6 = %q, got %q", "v6 line one", v6)
	}
}

func TestSplitUFWSnapshot_NoV6Marker(t *testing.T) {
	v4, v6 := splitUFWSnapshot("__V4__\nonly v4 content\n")
	if v4 != "only v4 content" {
		t.Errorf("want v4 = %q, got %q", "only v4 content", v4)
	}
	if v6 != "" {
		t.Errorf("want empty v6, got %q", v6)
	}
}
