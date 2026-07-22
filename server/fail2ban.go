package main

// fail2ban.go — optional sshd brute-force protection for the droplet.
//
// This box is pubkey-only (PasswordAuthentication no) and sshd is reachable
// from the whole internet regardless of whether the "restrict SSH to these
// IPs" wizard step (main.go's lockdownSSH) is in use — fail2ban is an
// independent second layer: watch sshd's own auth failures and temporarily
// firewall off whoever's clearly hammering it.
//
// Two non-obvious fixes are baked into the install script, both found by
// testing directly against a real droplet rather than assumed:
//  1. Ubuntu's openssh-server systemd unit is `ssh.service`, not
//     `sshd.service` — the fail2ban package's own default journalmatch
//     targets sshd.service. Where that unit doesn't exist, it would
//     silently watch nothing while reporting itself as active. The
//     install script detects which unit is actually present.
//  2. The stock "normal" filter only counts INVALID usernames as
//     failures. On a pubkey-only, root-login box the one real attack
//     surface — a wrong/stolen key tried against "root" — just closes
//     the connection pre-auth, which "normal" mode explicitly does NOT
//     count. "aggressive" mode fixes this; verified with fail2ban-regex
//     against a real log sample that it still correctly ignores every
//     successful "Accepted publickey" + clean-disconnect pair, which is
//     exactly what ProxyCTL's own SSH calls look like.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ban policy — the single source of truth the install script AND the
// status endpoint both read from, so the UI's explanation of "how many
// tries, how long" can never drift from what's actually applied.
//
// Deliberately aggressive and NOT escalating: this box's real, intended
// traffic is ProxyCTL's own control tunnel and authenticated
// personal-access devices (see restrictSSHToTunnel/personalaccess.go) —
// anyone reaching sshd from outside that, and failing repeatedly, isn't
// a legitimate user who mistyped something. f2bMaxRetry failures within
// f2bFindTime bans permanently (bantime -1), first offense, no grace
// period of temporary bans first. Always revertible from the SSH
// Security tab's banned-IP list if a ban ever turns out to be wrong.
const (
	f2bMaxRetry = 3
	f2bFindTime = "10m"
	f2bBanTime  = "-1" // permanent

	// f2bFindTimeJournal mirrors f2bFindTime (10m) in journalctl's --since
	// syntax — a separate constant since the formats differ, not derived
	// from f2bFindTime by string surgery. Used for the "recent attempts,
	// not yet banned" view — see recentSSHAttempts.
	f2bFindTimeJournal = "-10min"

	// f2bLogHistoryLines caps how much of fail2ban's own log the "ban
	// history" view reads — recent activity, not an unbounded scan of
	// however far back the (rotated) log happens to go.
	f2bLogHistoryLines = 500
)

// renderFail2banSetupScript is the idempotent droplet-side install +
// config. Safe to re-run any time (e.g. every time the operator clicks
// "Set up fail2ban") — re-asserts the same state, never duplicates it.
func renderFail2banSetupScript() string {
	return fmt.Sprintf(`set -e
echo "=== fail2ban (idempotent) ==="
export DEBIAN_FRONTEND=noninteractive
if ! command -v fail2ban-client >/dev/null 2>&1; then
  apt-get update -y >/dev/null
  apt-get install -y --no-install-recommends fail2ban >/dev/null
fi

SSHUNIT=sshd.service
systemctl cat sshd.service >/dev/null 2>&1 || SSHUNIT=ssh.service

mkdir -p /etc/fail2ban/jail.d
cat > /etc/fail2ban/jail.d/proxyctl-sshd.local <<JAIL
# Managed by ProxyCTL (Setup -> Droplet -> SSH security). Safe to re-apply.
[sshd]
enabled      = true
backend      = systemd
journalmatch = _SYSTEMD_UNIT=${SSHUNIT} + _COMM=sshd
filter       = sshd[mode=aggressive]
port         = ssh
maxretry     = %d
findtime     = %s
bantime      = %s
JAIL

fail2ban-client -t
systemctl enable fail2ban >/dev/null 2>&1
systemctl restart fail2ban
sleep 1
systemctl is-active fail2ban
fail2ban-client status sshd
`, f2bMaxRetry, f2bFindTime, f2bBanTime)
}

// f2bStatusCounts is the small subset of `fail2ban-client status sshd`
// worth surfacing — the banned IP list itself is read separately
// (parseFail2banBanIPs) since `status` carries no per-IP expiry, only
// `get sshd banip --with-time` does.
type f2bStatusCounts struct {
	CurrentlyFailed int
	TotalFailed     int
	TotalBanned     int
}

func parseFail2banStatus(out string) f2bStatusCounts {
	var c f2bStatusCounts
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "Currently failed:"):
			c.CurrentlyFailed = lastFieldInt(line)
		case strings.Contains(line, "Total failed:"):
			c.TotalFailed = lastFieldInt(line)
		case strings.Contains(line, "Total banned:"):
			c.TotalBanned = lastFieldInt(line)
		}
	}
	return c
}

func lastFieldInt(line string) int {
	f := strings.Fields(line)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(f[len(f)-1])
	return n
}

// BannedIP is one entry in the live ban list, enriched with best-effort
// geolocation (see geoLookupBatch). Country/City are empty when the
// lookup hasn't run yet or failed — never blocks showing the ban itself.
type BannedIP struct {
	IP        string `json:"ip"`
	ExpiresAt int64  `json:"expiresAt,omitempty"` // unix seconds, 0 if unknown
	Country   string `json:"country,omitempty"`
	City      string `json:"city,omitempty"`
}

// parseFail2banBanIPs parses `fail2ban-client get sshd banip --with-time`
// output — one line per banned IP shaped like:
//
//	192.0.2.1 	2026-07-21 13:27:45 + 3600 = 2026-07-21 14:27:45
//
// (start-time + duration-seconds = end-time, tab-separated from the IP).
func parseFail2banBanIPs(out string) []BannedIP {
	var list []BannedIP
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			// Unexpected format (older fail2ban, no time info) — keep the
			// IP so it still shows up, just without an expiry.
			if ip := strings.Fields(line); len(ip) > 0 {
				list = append(list, BannedIP{IP: ip[0]})
			}
			continue
		}
		b := BannedIP{IP: strings.TrimSpace(fields[0])}
		if eq := strings.LastIndex(fields[1], "="); eq >= 0 {
			end := strings.TrimSpace(fields[1][eq+1:])
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", end, time.UTC); err == nil {
				b.ExpiresAt = t.Unix()
			}
		}
		list = append(list, b)
	}
	return list
}

// ---- Geolocation (best-effort, cached) -------------------------------

type geoResult struct {
	country string
	city    string
	fetched time.Time
}

const geoCacheTTL = time.Hour

var (
	geoMu    sync.Mutex
	geoCache = map[string]geoResult{}
)

// geoLookupBatch resolves country/city for a batch of IPs via ip-api.com's
// free, keyless batch endpoint — fine for the occasional handful of
// banned IPs this deals with. Results are cached for geoCacheTTL so
// repeated dashboard polls don't re-query IPs that haven't changed.
// Best-effort throughout: any failure just leaves country/city empty,
// never blocks the ban list itself from rendering.
func geoLookupBatch(ips []string) map[string]geoResult {
	out := map[string]geoResult{}
	var need []string
	geoMu.Lock()
	for _, ip := range ips {
		if g, ok := geoCache[ip]; ok && time.Since(g.fetched) < geoCacheTTL {
			out[ip] = g
		} else {
			need = append(need, ip)
		}
	}
	geoMu.Unlock()
	if len(need) == 0 {
		return out
	}

	type reqItem struct {
		Query  string `json:"query"`
		Fields string `json:"fields"`
	}
	items := make([]reqItem, len(need))
	for i, ip := range need {
		items[i] = reqItem{Query: ip, Fields: "status,country,city,query"}
	}
	payload, err := json.Marshal(items)
	if err != nil {
		return out
	}

	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Post("http://ip-api.com/batch", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return out
	}
	defer resp.Body.Close()

	var results []struct {
		Status  string `json:"status"`
		Country string `json:"country"`
		City    string `json:"city"`
		Query   string `json:"query"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return out
	}
	geoMu.Lock()
	for _, r := range results {
		if r.Status != "success" {
			continue
		}
		g := geoResult{country: r.Country, city: r.City, fetched: time.Now()}
		geoCache[r.Query] = g
		out[r.Query] = g
	}
	geoMu.Unlock()
	return out
}

// ---- Recent (not-yet-banned) attempts ---------------------------------

// IPAttempt is one source IP currently accumulating failures within the
// findtime window, below f2bMaxRetry (fail2ban itself only exposes an
// aggregate "Currently failed" count via `status`, not a per-IP
// breakdown — this reconstructs it, best-effort, straight from sshd's
// own recent log lines rather than fail2ban's internal state).
type IPAttempt struct {
	IP       string `json:"ip"`
	Attempts int    `json:"attempts"`
	Country  string `json:"country,omitempty"`
	City     string `json:"city,omitempty"`
}

// sshFailureKeywords are the log-line substrings that mean "this was a
// failed/rejected connection attempt" for OpenSSH in aggressive-filter
// terms — invalid users, wrong passwords/keys, and the pre-auth
// disconnects that happen when this box's own Match-Address SSH
// restriction (see restrictSSHToTunnel/lockdownSSH in main.go) or
// DenyUsers rejects a source outright. Best-effort: a line matching one
// of these is counted as one attempt from whatever IPv4 address appears
// in it; this is for operator visibility, not a banning decision —
// fail2ban's own regex matching remains the actual authority on that.
var sshFailureKeywords = []string{
	"Failed password",
	"Invalid user",
	// Bare "Connection closed/reset by <IP> ... [preauth]" (no "invalid
	// user"/"authenticating user" prefix) happens for a raw pre-auth
	// disconnect — e.g. a port scanner that never even sends a username.
	// Broad enough to also cover the invalid-user/authenticating-user
	// variants as a subset, so those don't need their own separate entries.
	"Connection closed by",
	"Connection reset by",
	"Disconnected from invalid user",
	"Disconnected from authenticating user",
	"not allowed because",
	// A non-SSH client (or garbage) hitting the port before the protocol
	// handshake even starts, e.g. "banner exchange: ... invalid format".
	"banner exchange",
}

var ipv4Re = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)

// parseRecentSSHAttempts tallies failure-looking log lines by source IP,
// sorted most-attempts-first.
func parseRecentSSHAttempts(journalOutput string) []IPAttempt {
	counts := map[string]int{}
	var order []string
	sc := bufio.NewScanner(strings.NewReader(journalOutput))
	for sc.Scan() {
		line := sc.Text()
		matched := false
		for _, kw := range sshFailureKeywords {
			if strings.Contains(line, kw) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		ip := ipv4Re.FindString(line)
		if ip == "" {
			continue
		}
		if _, seen := counts[ip]; !seen {
			order = append(order, ip)
		}
		counts[ip]++
	}
	attempts := make([]IPAttempt, 0, len(order))
	for _, ip := range order {
		attempts = append(attempts, IPAttempt{IP: ip, Attempts: counts[ip]})
	}
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].Attempts > attempts[j].Attempts })
	return attempts
}

// ---- Ban history (all-time, as far as the current log covers) --------

// BanEvent is one ban or unban action straight from fail2ban's own log —
// unlike the live "currently banned" list, this includes bans that have
// since expired or been lifted, so "Total banned: N lifetime" in the
// jail's own counter has something concrete behind it instead of just a
// number. Bounded by f2bLogHistoryLines and whatever fail2ban.log itself
// still has on disk (logrotate eventually ages entries out) — not a
// literal unlimited history.
type BanEvent struct {
	IP      string `json:"ip"`
	Jail    string `json:"jail"`
	Action  string `json:"action"` // "ban" | "unban"
	At      int64  `json:"at,omitempty"`
	Country string `json:"country,omitempty"`
	City    string `json:"city,omitempty"`
}

var banLogLineRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}),\d+\s+\S+\s+\[\d+\]:\s+NOTICE\s+\[(\w+)\]\s+(Ban|Unban)\s+(\S+)`)

// parseFail2banLogHistory parses fail2ban.log lines shaped like:
//
//	2026-07-21 13:27:42,123 fail2ban.actions [1234]: NOTICE [sshd] Ban 91.92.47.37
//
// Returned newest-first.
func parseFail2banLogHistory(logText string) []BanEvent {
	var events []BanEvent
	sc := bufio.NewScanner(strings.NewReader(logText))
	for sc.Scan() {
		m := banLogLineRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		var at int64
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.UTC); err == nil {
			at = t.Unix()
		}
		events = append(events, BanEvent{IP: m[4], Jail: m[2], Action: strings.ToLower(m[3]), At: at})
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events
}
