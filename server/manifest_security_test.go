package main

import (
	"os"
	"strings"
	"testing"
)

func TestProxyCTLManifestHardensRuntimeContainers(t *testing.T) {
	b, err := os.ReadFile("../k8s/proxyctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(b)
	if strings.Contains(manifest, "hostPath: { path: /lib/modules }") || strings.Contains(manifest, "mountPath: /lib/modules") {
		t.Fatal("ProxyCTL manifest must not mount host kernel modules")
	}
	for _, want := range []string{
		"seccompProfile: { type: RuntimeDefault }",
		"runAsNonRoot: true",
		"runAsUser: 1000",
		"runAsGroup: 1000",
		"allowPrivilegeEscalation: false",
		`capabilities: { drop: ["ALL"] }`,
		`capabilities: { drop: ["ALL"], add: ["NET_ADMIN"] }`,
		`- { name: HOME, value: "/tmp" }`,
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("ProxyCTL manifest missing %q", want)
		}
	}
}

func TestControlTunnelConfigIsGroupReadableByHardenedSidecar(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainSrc), "os.WriteFile(tmp, []byte(conf), 0o640)") {
		t.Fatal("control tunnel config must be written 0640 so the NET_ADMIN-only sidecar can read it through fsGroup 1000")
	}
	manifest, err := os.ReadFile("../k8s/proxyctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `runAsUser: 0, runAsGroup: 1000`) {
		t.Fatal("control-wg sidecar must join group 1000 to read the 0640 shared config")
	}
}
