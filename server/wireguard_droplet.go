package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Upstream source pin for the droplet's native wireguard-tools. MUST stay
// in lockstep with the WIREGUARD_TOOLS_REF / WIREGUARD_TOOLS_SHA256 build
// args of GameCTL-HQ/WireGuard-Kube's Dockerfile — bumping upstream is a
// two-line change in each place (this file + the image Dockerfile).
const (
	dropletWireguardRef      = "v1.0.20260223"
	dropletWireguardSHA256   = "859f8af03702db5e5c43f8ece77f5ebef40a2f4627c3e03997a031d4940ea9bc"
	dropletWireguardProbeTO  = 15 * time.Second
	dropletWireguardSyncTO   = 5 * time.Minute
)

// DropletWireguardStatus is the droplet side of the WireGuard maintenance
// card: which wg the droplet resolves, its version, and whether it matches
// the pinned upstream source.
type DropletWireguardStatus struct {
	Version     string `json:"version,omitempty"`
	Path        string `json:"path,omitempty"`
	WgQuickPath string `json:"wgQuickPath,omitempty"`
	Expected    string `json:"expected"`
	Synced      bool   `json:"synced"`
	Error       string `json:"error,omitempty"`
}

// dropletWireguardProbe reports which wg/wg-quick the droplet resolves
// (PATH order decides: /usr/local shadows the distro /usr/bin) and the
// version the active wg reports.
const dropletWireguardProbe = `P=$(command -v wg); V=$($P --version 2>/dev/null); Q=$(command -v wg-quick 2>/dev/null || echo ""); echo "WG_PATH=$P"; echo "WG_VERSION=$V"; echo "WGQUICK_PATH=$Q"`

var wgVersionRE = regexp.MustCompile(`wireguard-tools (v[0-9]+\.[0-9]+\.[0-9]+[0-9a-z]*)`)

// stepError picks the most useful failure text from a Step: stderr when it
// has anything, stdout otherwise (some ssh failures land on stdout).
func stepError(st Step) string {
	if s := strings.TrimSpace(st.Stderr); s != "" {
		return s
	}
	return strings.TrimSpace(st.Stdout)
}

func truncateProbe(out string, n int) string {
	s := strings.TrimSpace(out)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func parseDropletWireguardProbe(out string) (DropletWireguardStatus, error) {
	st := DropletWireguardStatus{Expected: dropletWireguardRef}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "WG_PATH="):
			st.Path = strings.TrimPrefix(line, "WG_PATH=")
		case strings.HasPrefix(line, "WG_VERSION="):
			v := strings.TrimPrefix(line, "WG_VERSION=")
			if m := wgVersionRE.FindStringSubmatch(v); m != nil {
				st.Version = m[1]
			}
		case strings.HasPrefix(line, "WGQUICK_PATH="):
			st.WgQuickPath = strings.TrimPrefix(line, "WGQUICK_PATH=")
		}
	}
	if st.Version == "" {
		return st, fmt.Errorf("droplet wg version not reported (output: %q)", truncateProbe(out, 200))
	}
	st.Synced = st.Version == dropletWireguardRef
	return st, nil
}

// dropletWireguardSyncScript compiles wireguard-tools FROM the pinned
// upstream source on the droplet itself (glibc portability: the image's
// binary is built against a newer libc than the droplet's Ubuntu ships)
// and installs to /usr/local so PATH shadows the distro package, which is
// left in place as the fallback/rollback. Idempotent: re-running rebuilds
// the same pinned source and re-applies the systemd drop-in.
func dropletWireguardSyncScript() string {
	return `set -euo pipefail
REF=` + dropletWireguardRef + `
SHA=` + dropletWireguardSHA256 + `
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends build-essential pkg-config ca-certificates curl >/dev/null
cd /tmp
curl -fsSL -o wg-tools.tar.gz "https://github.com/WireGuard/wireguard-tools/archive/refs/tags/${REF}.tar.gz"
echo "${SHA}  wg-tools.tar.gz" | sha256sum -c -
rm -rf /tmp/wgtools-src && mkdir /tmp/wgtools-src
tar -xzf wg-tools.tar.gz -C /tmp/wgtools-src --strip-components=1
make -C /tmp/wgtools-src/src -j"$(nproc)"
make -C /tmp/wgtools-src/src install PREFIX=/usr/local WITH_WGQUICK=yes
rm -rf /tmp/wgtools-src /tmp/wg-tools.tar.gz
mkdir -p /etc/systemd/system/wg-quick@.service.d
cat > /etc/systemd/system/wg-quick@.service.d/upstream-tools.conf <<'CFG'
# Use the from-upstream-source wg-quick (` + dropletWireguardRef + `) instead of the
# distro script. Managed by ProxyCTL WireGuard maintenance.
[Service]
ExecStart=
ExecStart=/usr/local/bin/wg-quick up %i
ExecStop=
ExecStop=/usr/local/bin/wg-quick down %i
CFG
systemctl daemon-reload
hash -r
P=$(command -v wg); V=$($P --version 2>/dev/null); Q=$(command -v wg-quick 2>/dev/null || echo "")
echo "WG_PATH=$P"
echo "WG_VERSION=$V"
echo "WGQUICK_PATH=$Q"
`
}

// dropletWireguardStatus probes the droplet best-effort: an unreachable or
// unconfigured droplet reports its error neutrally instead of failing the
// UI status (same stance as the GHCR upstream check).
func (a *API) dropletWireguardStatus() *DropletWireguardStatus {
	cfg := a.droplet.Get()
	if cfg == nil || !cfg.Configured() {
		return &DropletWireguardStatus{Expected: dropletWireguardRef, Error: "droplet not configured"}
	}
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		return &DropletWireguardStatus{Expected: dropletWireguardRef, Error: "ssh apply-mode required"}
	}
	sa.Droplet = cfg
	st := sa.sshDropletLong("wireguard droplet: probe", "", "bash -c "+shq(dropletWireguardProbe), dropletWireguardProbeTO)
	if !st.OK {
		return &DropletWireguardStatus{Expected: dropletWireguardRef, Error: strings.TrimSpace(stepError(st))}
	}
	got, err := parseDropletWireguardProbe(st.Stdout)
	if err != nil {
		return &DropletWireguardStatus{Expected: dropletWireguardRef, Error: err.Error()}
	}
	return &got
}

// syncDropletWireguard builds the pinned upstream source on the droplet and
// installs it to /usr/local (see dropletWireguardSyncScript). It returns the
// resulting status on success so the caller can report the exact version.
func syncDropletWireguard(sa SSHApplier, cfg *DropletConfig) (DropletWireguardStatus, error) {
	sa.Droplet = cfg
	st := sa.sshDropletLong("wireguard droplet: sync upstream source", dropletWireguardSyncScript(), "bash -s", dropletWireguardSyncTO)
	if !st.OK {
		return DropletWireguardStatus{Expected: dropletWireguardRef}, fmt.Errorf("%s (exit %d)", strings.TrimSpace(stepError(st)), st.ExitCode)
	}
	got, err := parseDropletWireguardProbe(st.Stdout)
	if err != nil {
		return got, err
	}
	return got, nil
}
