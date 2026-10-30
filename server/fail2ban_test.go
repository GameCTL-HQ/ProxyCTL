package main

import (
	"strings"
	"testing"
)

// The rendered jail config must actually carry the policy the status
// endpoint reports — otherwise the UI's "bans after N tries" text could
// silently drift from what's really applied on the droplet.
func TestRenderFail2banSetupScript_MatchesPolicyDefaults(t *testing.T) {
	script := renderFail2banSetupScript(effectiveF2BPolicy(nil))
	for _, want := range []string{
		"maxretry     = 3",
		"findtime     = 10m",
		"bantime      = -1",
		"filter       = sshd[mode=aggressive]",
		"journalmatch = _SYSTEMD_UNIT=${SSHUNIT} + _COMM=sshd",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("setup script missing %q\n---\n%s", want, script)
		}
	}
	// No escalation, no second jail — a single tier (permanent from first
	// offense by default) is the whole policy (see the const block).
	if strings.Contains(script, "bantime.increment") || strings.Contains(script, "[recidive]") {
		t.Errorf("setup script should no longer render escalation or a recidive jail:\n%s", script)
	}
}

func TestRenderFail2banSetupScript_OperatorPolicy(t *testing.T) {
	script := renderFail2banSetupScript(effectiveF2BPolicy(&F2BPolicy{MaxRetry: 5, FindTime: "30m", BanTime: "24h"}))
	for _, want := range []string{"maxretry     = 5", "findtime     = 30m", "bantime      = 24h"} {
		if !strings.Contains(script, want) {
			t.Errorf("setup script missing %q\n---\n%s", want, script)
		}
	}
}

// Partial operator policy: unset fields inherit the defaults instead of
// rendering zero values into the jail.
func TestEffectiveF2BPolicy_PartialMerge(t *testing.T) {
	pol := effectiveF2BPolicy(&F2BPolicy{MaxRetry: 6})
	if pol.MaxRetry != 6 || pol.FindTime != "10m" || pol.BanTime != "-1" {
		t.Fatalf("partial merge wrong: %+v", pol)
	}
}

func TestValidateF2BPolicy(t *testing.T) {
	ok := []F2BPolicy{
		{MaxRetry: 3, FindTime: "10m", BanTime: "-1"},
		{MaxRetry: 1, FindTime: "600", BanTime: "24h"},
		{MaxRetry: 20, FindTime: "1h", BanTime: "1w"},
	}
	for _, p := range ok {
		if err := validateF2BPolicy(p); err != nil {
			t.Errorf("want %+v valid, got %v", p, err)
		}
	}
	// Every rejected case is one that would otherwise land inside a root
	// heredoc on the droplet — the charset is the injection boundary.
	bad := []F2BPolicy{
		{MaxRetry: 0, FindTime: "10m", BanTime: "-1"},
		{MaxRetry: 21, FindTime: "10m", BanTime: "-1"},
		{MaxRetry: 3, FindTime: "10 m", BanTime: "-1"},
		{MaxRetry: 3, FindTime: "10m; rm -rf /", BanTime: "-1"},
		{MaxRetry: 3, FindTime: "10m", BanTime: "-2"},
		{MaxRetry: 3, FindTime: "10m", BanTime: "$(reboot)"},
		{MaxRetry: 3, FindTime: "", BanTime: "-1"},
	}
	for _, p := range bad {
		if err := validateF2BPolicy(p); err == nil {
			t.Errorf("want %+v rejected", p)
		}
	}
}

func TestF2BJournalSince(t *testing.T) {
	cases := map[string]string{
		"10m": "-10min", "600": "-600s", "45s": "-45s", "1h": "-1hour",
		"2d": "-2day", "1w": "-1week", "garbage": "-10min", "": "-10min",
	}
	for in, want := range cases {
		if got := f2bJournalSince(in); got != want {
			t.Errorf("f2bJournalSince(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFail2banStatus(t *testing.T) {
	out := `Status for the jail: sshd
|- Filter
|  |- Currently failed:	2
|  |- Total failed:	7
|  ` + "`" + `- Journal matches:	_SYSTEMD_UNIT=ssh.service + _COMM=sshd
` + "`" + `- Actions
   |- Currently banned:	1
   |- Total banned:	3
   ` + "`" + `- Banned IP list:	192.0.2.1
`
	c := parseFail2banStatus(out)
	if c.CurrentlyFailed != 2 || c.TotalFailed != 7 || c.TotalBanned != 3 {
		t.Fatalf("got %+v", c)
	}
}

func TestParseFail2banBanIPs(t *testing.T) {
	out := "192.0.2.1 \t2026-07-21 13:27:45 + 3600 = 2026-07-21 14:27:45\n" +
		"198.51.100.7 \t2026-07-21 12:00:00 + 7200 = 2026-07-21 14:00:00\n"
	list := parseFail2banBanIPs(out)
	if len(list) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(list), list)
	}
	if list[0].IP != "192.0.2.1" {
		t.Errorf("want first IP 192.0.2.1, got %q", list[0].IP)
	}
	// Independently verified via `date -u -d '2026-07-21 14:27:45' +%s`.
	if want := int64(1784644065); list[0].ExpiresAt != want {
		t.Errorf("want ExpiresAt %d for %q, got %d", want, list[0].IP, list[0].ExpiresAt)
	}
	if list[1].IP != "198.51.100.7" {
		t.Errorf("want second IP 198.51.100.7, got %q", list[1].IP)
	}
	if want := int64(1784642400); list[1].ExpiresAt != want {
		t.Errorf("want ExpiresAt %d for %q, got %d", want, list[1].IP, list[1].ExpiresAt)
	}
}

func TestParseFail2banBanIPs_Empty(t *testing.T) {
	if list := parseFail2banBanIPs(""); len(list) != 0 {
		t.Fatalf("want no entries for empty output, got %+v", list)
	}
	if list := parseFail2banBanIPs("\n\n"); len(list) != 0 {
		t.Fatalf("want no entries for blank lines, got %+v", list)
	}
}

// parseRecentSSHAttempts is best-effort visibility into "who's currently
// trying, below the ban threshold" — fail2ban itself only exposes an
// aggregate count, not a per-IP breakdown. Assert it tallies correctly and
// ignores unrelated log noise (a successful login must never count).
func TestParseRecentSSHAttempts(t *testing.T) {
	log := `Jul 21 13:20:01 host sshd[111]: Invalid user admin from 91.92.47.37 port 4001
Jul 21 13:20:05 host sshd[112]: Failed password for invalid user admin from 91.92.47.37 port 4001 ssh2
Jul 21 13:21:00 host sshd[113]: Connection closed by invalid user test 203.0.113.9 port 5000 [preauth]
Jul 21 13:22:00 host sshd[114]: Accepted publickey for root from 10.8.0.254 port 6000 ssh2
Jul 21 13:23:00 host sshd[115]: Disconnected from authenticating user root 203.0.113.9 port 5001 [preauth]
`
	attempts := parseRecentSSHAttempts(log)
	got := map[string]int{}
	for _, a := range attempts {
		got[a.IP] = a.Attempts
	}
	if got["91.92.47.37"] != 2 {
		t.Errorf("want 2 attempts for 91.92.47.37, got %d (%+v)", got["91.92.47.37"], attempts)
	}
	if got["203.0.113.9"] != 2 {
		t.Errorf("want 2 attempts for 203.0.113.9, got %d (%+v)", got["203.0.113.9"], attempts)
	}
	if _, ok := got["10.8.0.254"]; ok {
		t.Errorf("a successful 'Accepted publickey' login must never count as an attempt: %+v", attempts)
	}
	if len(attempts) != 2 {
		t.Fatalf("want exactly 2 distinct IPs, got %d: %+v", len(attempts), attempts)
	}
	// Sorted most-attempts-first; both are tied at 2 here, so just check
	// the two we expect are present without assuming which comes first.
}

func TestParseFail2banLogHistory(t *testing.T) {
	log := "2026-07-21 13:27:42,123 fail2ban.actions        [1234]: NOTICE  [sshd] Ban 91.92.47.37\n" +
		"2026-07-21 14:27:42,456 fail2ban.actions        [1234]: NOTICE  [sshd] Unban 91.92.47.37\n" +
		"2026-07-21 15:00:00,000 fail2ban.actions        [1234]: NOTICE  [recidive] Ban 203.0.113.9\n" +
		"2026-07-21 15:00:00,000 fail2ban.filter          [1234]: INFO    [sshd] Found 91.92.47.37\n" // unrelated line, must be ignored
	events := parseFail2banLogHistory(log)
	if len(events) != 3 {
		t.Fatalf("want 3 ban/unban events, got %d: %+v", len(events), events)
	}
	// Newest-first.
	if events[0].IP != "203.0.113.9" || events[0].Jail != "recidive" || events[0].Action != "ban" {
		t.Errorf("want newest event to be recidive ban of 203.0.113.9, got %+v", events[0])
	}
	if events[2].IP != "91.92.47.37" || events[2].Action != "ban" {
		t.Errorf("want oldest event to be the sshd ban of 91.92.47.37, got %+v", events[2])
	}
}

// A manual ban is operator-triggered and unchecked by fail2ban's own
// "is this really an attacker" heuristics — the one guard that matters is
// never letting it ban ProxyCTL's own trusted addresses out from under
// itself. Must catch both an exact match and a wider CIDR that happens to
// swallow one.
func TestBanIPBlocksOwnAccess(t *testing.T) {
	cfg := &DropletConfig{
		PersonalAccessPeers: []PersonalAccessPeer{{Name: "Laptop", IP: "10.8.0.244"}},
	}
	cases := []struct {
		in        string
		wantBlock bool
	}{
		{controlTunnelIP, true},   // exact control-tunnel address
		{"10.8.0.244", true},      // exact personal-access address
		{"10.8.0.0/24", true},     // CIDR containing both
		{"203.0.113.9", false},    // an actual attacker IP
		{"203.0.113.0/24", false}, // an attacker CIDR nowhere near the tunnel range
	}
	for _, c := range cases {
		_, blocks := banIPBlocksOwnAccess(c.in, cfg)
		if blocks != c.wantBlock {
			t.Errorf("banIPBlocksOwnAccess(%q) blocks=%v, want %v", c.in, blocks, c.wantBlock)
		}
	}
}
