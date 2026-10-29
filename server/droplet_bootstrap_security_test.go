package main

import (
	"os"
	"strings"
	"testing"
)

func TestDropletBootstrapMaintainsHostSecurityUpdates(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	for _, want := range []string{
		"wireguard wireguard-tools iptables conntrack ca-certificates curl jq unattended-upgrades",
		"zzzz-proxyctl-security",
		`APT::Periodic::Update-Package-Lists "1";`,
		`APT::Periodic::Unattended-Upgrade "1";`,
		`Unattended-Upgrade::Automatic-Reboot "false";`,
		"apt-config dump",
		`if [ "$AUTO_REBOOT" != "false" ]`,
		"PROXYCTL_REBOOT_REQUIRED=true",
		"PROXYCTL_CLOUDFLARED_VERSION=",
		"reconcileCloudflaredDeployment",
		"cloudflaredVersion",
		"SetWriteDeadline(time.Now().Add(14 * time.Minute))",
		"context.WithTimeout(context.Background(), 13*time.Minute)",
		"nativeExitCode",
		"clusterCloudflaredOK",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("droplet bootstrap is missing security-maintenance requirement %q", want)
		}
	}
	if strings.Contains(src, "cat > /etc/apt/apt.conf.d/20auto-upgrades") {
		t.Fatal("droplet bootstrap must not overwrite the host's existing 20auto-upgrades policy")
	}
}

func TestDropletBootstrapNeverManagesNativeCloudflared(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	start := strings.Index(src, "func (a *API) runDropletBootstrapUpgrade")
	end := strings.Index(src, "// detectEgressIP")
	if start < 0 || end <= start {
		t.Fatal("could not isolate runDropletBootstrapUpgrade source")
	}
	bootstrap := src[start:end]
	for _, forbidden := range []string{
		"cloudflared update",
		"api.github.com/repos/cloudflare/cloudflared/releases/latest",
		"CLOUDFLARED_PACKAGE",
		"systemctl restart cloudflared",
	} {
		if strings.Contains(bootstrap, forbidden) {
			t.Errorf("standard droplet maintenance must not manage optional native cloudflared: found %q", forbidden)
		}
	}
	if !strings.Contains(bootstrap, "reconcileCloudflaredDeployment") {
		t.Fatal("standard maintenance must still reconcile the Kubernetes Cloudflare connector")
	}
}
