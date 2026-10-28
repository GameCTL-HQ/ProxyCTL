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
		"cloudflared update",
		"dpkg-query -W",
		"api.github.com/repos/cloudflare/cloudflared/releases/latest",
		"cloudflared-linux-${CLOUDFLARED_ARCH}.deb",
		"CLOUDFLARED_PACKAGE_DIGEST",
		"sha256sum -c -",
		"apt-get install -y --no-install-recommends \"$CLOUDFLARED_PACKAGE\"",
		`if [ "$CLOUDFLARED_INSTALLED_VERSION" != "$CLOUDFLARED_LATEST" ]`,
		`CLOUDFLARED_UPDATE_RC" != "11"`,
		"systemctl restart cloudflared",
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
