package main

import (
	"strings"
	"testing"
)

func shareInfra() Infra {
	in := DefaultInfra()
	in.K8sNamespace = "proxyctl"
	in.KeysBasePath = "ProxyCTL/Keys"
	in.KeysNFSServer = "10.0.0.5"
	in.KeysNFSExport = "/mnt/ssd"
	return in
}

func gwEntry() *Entry {
	return &Entry{Name: "cs2", Enabled: true, TunnelIP: "10.8.0.3",
		TargetIP: "10.43.0.10", Service: "cs2.gamectl",
		Ports: []PortSpec{{Port: 27015, Proto: "both"}}}
}

func TestKeysShareMode_OnlyWhenBothHalvesSet(t *testing.T) {
	in := DefaultInfra()
	if in.keysShareMode() {
		t.Error("default Infra must be provisioner mode")
	}
	in.KeysNFSServer = "10.0.0.5"
	if in.keysShareMode() {
		t.Error("server alone must NOT enable share mode — it would render an unmountable PV")
	}
	in.KeysNFSExport = "/mnt/ssd"
	if !in.keysShareMode() {
		t.Error("server+export must enable share mode")
	}
}

// The property that keeps existing tunnels alive: share mode's subPath must
// reproduce the nfs-subdir provisioner's own dir naming
// (${.PVC.namespace}-${.PVC.name}), so pointing a share at an export the
// provisioner already used finds the SAME keypairs. Diverging silently re-keys
// every gateway and breaks the droplet's [Peer] until re-apply.
func TestKeysSubPath_MatchesProvisionerDirNaming(t *testing.T) {
	got := keysSubPath("ProxyCTL/Keys", "proxyctl", "wg-gw-cs2")
	want := "ProxyCTL/Keys/proxyctl-wg-gw-cs2-keys" // == the live NFS dir today
	if got != want {
		t.Errorf("subPath would orphan existing keys:\n want %q\n got  %q", want, got)
	}
}

func TestRenderGateway_ShareMode(t *testing.T) {
	got := renderGatewayManifest(shareInfra(), gwEntry())

	// Binds the shared claim, not a per-gateway one.
	shared := keysShareVolName("10.0.0.5", "/mnt/ssd")
	if !strings.Contains(got, "claimName: "+shared) {
		t.Errorf("share mode must bind the shared claim %q:\n%s", shared, got)
	}
	// And must NOT declare its own PVC (they'd collide on the shared PV).
	if strings.Contains(got, "kind: PersistentVolumeClaim") {
		t.Errorf("share mode must not render a per-gateway PVC:\n%s", got)
	}
	// Both /keys mounts carve the gateway's own subPath.
	sub := "subPath: ProxyCTL/Keys/proxyctl-wg-gw-cs2-keys"
	if n := strings.Count(got, sub); n != 2 {
		t.Errorf("want 2 subPath mounts (keygen + wireguard), got %d:\n%s", n, got)
	}
	// The wireguard container keeps its read-only view.
	if !strings.Contains(got, "mountPath: /keys, subPath: "+
		"ProxyCTL/Keys/proxyctl-wg-gw-cs2-keys, readOnly: true") {
		t.Errorf("wireguard mount lost readOnly:\n%s", got)
	}
}

// Provisioner mode is the default and must be untouched by all of this.
func TestRenderGateway_ProvisionerModeUnchanged(t *testing.T) {
	in := DefaultInfra()
	in.K8sNamespace = "proxyctl"
	got := renderGatewayManifest(in, gwEntry())

	if !strings.Contains(got, "kind: PersistentVolumeClaim") {
		t.Errorf("provisioner mode must still render a per-gateway PVC:\n%s", got)
	}
	if !strings.Contains(got, "claimName: wg-gw-cs2-keys") {
		t.Errorf("provisioner mode must bind its own PVC:\n%s", got)
	}
	if strings.Contains(got, "subPath") {
		t.Errorf("provisioner mode must not use subPath (the class's pathPattern does the nesting):\n%s", got)
	}
}

func TestRenderKeysSharePV(t *testing.T) {
	got := renderKeysSharePV("proxyctl", "10.0.0.5", "/mnt/ssd")

	// The PV is the export ROOT — kubelet creates the per-gateway subPath under
	// it, which a static NFS PV can't do for a path that doesn't exist.
	if !strings.Contains(got, "nfs: { server: 10.0.0.5, path: /mnt/ssd }") {
		t.Errorf("PV must point at the export root:\n%s", got)
	}
	// storageClassName "" keeps every provisioner's hands off it.
	if strings.Count(got, `storageClassName: ""`) != 2 {
		t.Errorf("PV and PVC must both pin storageClassName to \"\":\n%s", got)
	}
	// Shared by all gateways.
	if !strings.Contains(got, "accessModes: [ReadWriteMany]") {
		t.Errorf("must be RWX — every gateway shares this claim:\n%s", got)
	}
	// Losing the keys re-keys every tunnel.
	if !strings.Contains(got, "persistentVolumeReclaimPolicy: Retain") {
		t.Errorf("keys PV must Retain:\n%s", got)
	}
	// Must NOT pin an NFS version: a server that doesn't speak the pinned one
	// fails the mount, and the pod then hangs before its init containers —
	// which surfaces as an unexplained rollout timeout, not an NFS error.
	// mount.nfs negotiates 4.2 -> 4.1 -> 4.0 -> 3 on its own.
	if strings.Contains(got, "nfsvers") {
		t.Errorf("PV must not pin an NFS version (breaks NFSv3-only servers):\n%s", got)
	}
}

// A changed share must be a NEW PV: nfs.server/path are immutable, so reusing
// the name would make apply fail rather than move the keys.
func TestKeysShareVolName_ChangesWithShare(t *testing.T) {
	a := keysShareVolName("10.0.0.5", "/mnt/ssd")
	if b := keysShareVolName("10.0.0.6", "/mnt/ssd"); a == b {
		t.Error("different server must yield a different PV name")
	}
	if b := keysShareVolName("10.0.0.5", "/mnt/other"); a == b {
		t.Error("different export must yield a different PV name")
	}
	if b := keysShareVolName("10.0.0.5", "/mnt/ssd"); a != b {
		t.Error("same share must be stable across calls")
	}
}

func TestValidateNFSShare(t *testing.T) {
	ok := []struct{ srv, exp string }{
		{"10.0.0.5", "/mnt/ssd"},
		{"nas.example.com", "/export/keys"},
		{"10.0.0.5", "/mnt/ssd/"}, // trailing slash normalized
	}
	for _, c := range ok {
		if err := validateNFSShare(c.srv, c.exp); err != nil {
			t.Errorf("validateNFSShare(%q,%q) = %v, want nil", c.srv, c.exp, err)
		}
	}
	bad := []struct{ srv, exp, why string }{
		{"", "/mnt/ssd", "missing server"},
		{"10.0.0.5", "", "missing export"},
		{"10.0.0.5", "mnt/ssd", "relative export"},
		{"10.0.0.5:/mnt", "/mnt/ssd", "server carrying a path"},
		{"nfs://10.0.0.5", "/mnt/ssd", "server carrying a scheme"},
		{"10.0.0.5", "/mnt/../etc", "traversal"},
	}
	for _, c := range bad {
		if err := validateNFSShare(c.srv, c.exp); err == nil {
			t.Errorf("validateNFSShare(%q,%q) accepted %s", c.srv, c.exp, c.why)
		}
	}
}

func TestNormalizeNFSExport(t *testing.T) {
	for in, want := range map[string]string{
		"/mnt/ssd/":    "/mnt/ssd",
		"  /mnt/ssd  ": "/mnt/ssd",
		"/mnt//ssd":    "/mnt/ssd",
		"/mnt/ssd/./x": "/mnt/ssd/x",
		"/":            "/",
		"":             "",
	} {
		if got := normalizeNFSExport(in); got != want {
			t.Errorf("normalizeNFSExport(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeysLocationHintFor_ShareIsAuthoritative(t *testing.T) {
	// A named share must not consult cluster discovery at all.
	got := keysLocationHintFor("ProxyCTL/Keys", "10.0.0.5", "/mnt/ssd/")
	want := "/mnt/ssd/ProxyCTL/Keys  (NFS 10.0.0.5)"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}
