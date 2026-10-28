package main

import (
	"strings"
	"testing"
)

func TestParseDropletWireguardProbeSynced(t *testing.T) {
	st, err := parseDropletWireguardProbe(strings.Join([]string{
		"WG_PATH=/usr/local/bin/wg",
		"WG_VERSION=wireguard-tools v1.0.20260223 - https://git.zx2c4.com/wireguard-tools/",
		"WGQUICK_PATH=/usr/local/bin/wg-quick",
	}, "\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if st.Version != "v1.0.20260223" {
		t.Fatalf("version = %q, want v1.0.20260223", st.Version)
	}
	if st.Path != "/usr/local/bin/wg" || st.WgQuickPath != "/usr/local/bin/wg-quick" {
		t.Fatalf("paths = %q / %q", st.Path, st.WgQuickPath)
	}
	if !st.Synced {
		t.Fatal("probe at the pinned upstream version must report synced")
	}
}

func TestParseDropletWireguardProbeBehind(t *testing.T) {
	st, err := parseDropletWireguardProbe(strings.Join([]string{
		"WG_PATH=/usr/bin/wg",
		"WG_VERSION=wireguard-tools v1.0.20210914 - https://git.zx2c4.com/wireguard-tools/",
		"WGQUICK_PATH=/usr/bin/wg-quick",
	}, "\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if st.Synced {
		t.Fatal("distro package version must not report synced")
	}
	if st.Version != "v1.0.20210914" {
		t.Fatalf("version = %q, want v1.0.20210914", st.Version)
	}
}

func TestParseDropletWireguardProbeGarbage(t *testing.T) {
	if _, err := parseDropletWireguardProbe("wg: command not found\n"); err == nil {
		t.Fatal("missing version line must fail the parse")
	}
	if _, err := parseDropletWireguardProbe(""); err == nil {
		t.Fatal("empty output must fail the parse")
	}
}

func TestDropletWireguardProbeAsksForBothTools(t *testing.T) {
	for _, marker := range []string{
		"command -v wg",
		"WG_VERSION=",
		"command -v wg-quick",
	} {
		if !strings.Contains(dropletWireguardProbe, marker) {
			t.Errorf("probe missing marker %q", marker)
		}
	}
}

func TestDropletWireguardSyncScriptPinned(t *testing.T) {
	s := dropletWireguardSyncScript()
	// Pinned upstream source: tag AND tarball SHA-256 must be embedded.
	if !strings.Contains(s, dropletWireguardRef) {
		t.Errorf("sync script missing pinned upstream ref %q", dropletWireguardRef)
	}
	if !strings.Contains(s, dropletWireguardSHA256) {
		t.Error("sync script missing pinned tarball SHA-256")
	}
	if !strings.Contains(s, "sha256sum -c") {
		t.Error("sync script must verify the tarball hash before building")
	}
	// Install to /usr/local so PATH shadows the distro binary; the distro
	// package stays as the fallback and rollback.
	if !strings.Contains(s, "install PREFIX=/usr/local WITH_WGQUICK=yes") {
		t.Error("sync script must install to /usr/local with wg-quick")
	}
	// The systemd unit hardcodes /usr/bin/wg-quick; the drop-in override is
	// what makes boot-time wg0 come up with the upstream script.
	if !strings.Contains(s, "wg-quick@.service.d") || !strings.Contains(s, "ExecStart=/usr/local/bin/wg-quick up %i") {
		t.Error("sync script must install the wg-quick@ systemd drop-in override")
	}
	if !strings.Contains(s, "set -euo pipefail") {
		t.Error("sync script must fail hard (set -euo pipefail)")
	}
}
