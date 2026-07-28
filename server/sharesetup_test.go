package main

import (
	"strings"
	"testing"
)

func TestExportsLineFor(t *testing.T) {
	got := exportsLineFor("/mnt/ssd/ProxyCTL", []string{"10.0.0.101", "10.0.0.102"})
	want := "/mnt/ssd/ProxyCTL " +
		"10.0.0.101(rw,sync,no_subtree_check,no_root_squash) " +
		"10.0.0.102(rw,sync,no_subtree_check,no_root_squash)"
	if got != want {
		t.Fatalf("exportsLineFor:\n got %q\nwant %q", got, want)
	}
	// Exports entries are line-scoped — the whole thing must stay on one line.
	if strings.Contains(got, "\n") {
		t.Fatalf("exports line contains a newline: %q", got)
	}
}

func TestRenderShareCommands(t *testing.T) {
	out := renderShareCommands("/mnt/ssd/ProxyCTL", []string{"10.0.0.1"})
	for _, want := range []string{
		"mkdir -p /mnt/ssd/ProxyCTL\n",
		"chmod 700 /mnt/ssd/ProxyCTL\n",
		"/mnt/ssd/ProxyCTL 10.0.0.1(rw,sync,no_subtree_check,no_root_squash)\n",
		"exportfs -ra\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("commands missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderShareCommandsNoNodes(t *testing.T) {
	// No node list (e.g. RBAC not yet re-applied): the placeholder keeps the
	// command shape honest instead of rendering an exports line with no hosts.
	out := renderShareCommands("/mnt/ssd/ProxyCTL", nil)
	if !strings.Contains(out, "<node-ip>(rw,sync,no_subtree_check,no_root_squash)") {
		t.Errorf("placeholder exports clause missing in:\n%s", out)
	}
}

func TestNodesNotCovered(t *testing.T) {
	live := []clusterNode{
		{Name: "n1", IP: "10.0.0.1"},
		{Name: "n2", IP: "10.0.0.2"},
		{Name: "n3", IP: "10.0.0.3"},
	}
	missing := nodesNotCovered(live, []string{"10.0.0.1", "10.0.0.3"})
	if len(missing) != 1 || missing[0].Name != "n2" {
		t.Fatalf("want [n2], got %+v", missing)
	}
	// Empty snapshot = exports line never generated: nothing to diff against.
	if got := nodesNotCovered(live, nil); got != nil {
		t.Fatalf("nil snapshot should yield nil, got %+v", got)
	}
	if got := nodesNotCovered(live, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}); len(got) != 0 {
		t.Fatalf("fully covered should yield none, got %+v", got)
	}
}

func TestValidateNFSExportPath(t *testing.T) {
	for _, ok := range []string{"/mnt/ssd", "/mnt/ssd/ProxyCTL", "/", "/mnt/ssd", "/srv/nfs/games-01"} {
		if err := validateNFSExportPath(ok); err != nil {
			t.Errorf("%q should validate: %v", ok, err)
		}
	}
	// Shell metacharacters must be rejected: the path is interpolated into the
	// /etc/exports line applied over SSH on the droplet as root, so an escape
	// here is a root-command-injection footgun.
	for _, bad := range []string{
		"", "mnt/ssd", "/mnt/../etc", "/mnt/my share",
		"/mnt/ssd;reboot",
		"/mnt/$(touch pwned)",
		"/mnt/`id`",
		"/mnt/ssd|nc",
		"/mnt/a&b",
		"/mnt/x'y",
	} {
		if err := validateNFSExportPath(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestAppShareEnvDefaults(t *testing.T) {
	// StorageClass install: no env → provisioner mode, legacy default folder.
	t.Setenv("PROXYCTL_DATA_NFS_SERVER", "")
	t.Setenv("PROXYCTL_DATA_NFS_EXPORT", "")
	if _, _, ok := appShareEnv(); ok {
		t.Fatal("empty env should not be an app share")
	}
	if got := defaultKeysBase(); got != defaultKeysBasePath {
		t.Fatalf("default base = %q, want %q", got, defaultKeysBasePath)
	}

	// Unsubstituted manifest placeholders must not count as a share.
	t.Setenv("PROXYCTL_DATA_NFS_SERVER", "__DATA_NFS_SERVER__")
	t.Setenv("PROXYCTL_DATA_NFS_EXPORT", "__DATA_NFS_EXPORT__")
	if _, _, ok := appShareEnv(); ok {
		t.Fatal("placeholder env should not be an app share")
	}

	// Share install: env set → keys default to the app share, folder "Keys".
	t.Setenv("PROXYCTL_DATA_NFS_SERVER", "10.0.0.5")
	t.Setenv("PROXYCTL_DATA_NFS_EXPORT", "/mnt/ssd/ProxyCTL/")
	srv, exp, ok := appShareEnv()
	if !ok || srv != "10.0.0.5" || exp != "/mnt/ssd/ProxyCTL" {
		t.Fatalf("appShareEnv = %q %q %v", srv, exp, ok)
	}
	if got := defaultKeysBase(); got != "Keys" {
		t.Fatalf("default base = %q, want Keys", got)
	}

	// A store with nothing saved falls back to the app share + Keys/ —
	// and an explicitly saved share still wins over the env.
	s, err := NewKeysStore(t.TempDir() + "/keys.json")
	if err != nil {
		t.Fatal(err)
	}
	if srv, exp, ok := s.Share(); !ok || srv != "10.0.0.5" || exp != "/mnt/ssd/ProxyCTL" {
		t.Fatalf("unsaved Share() = %q %q %v, want the app share", srv, exp, ok)
	}
	if got := s.BasePath(); got != "Keys" {
		t.Fatalf("unsaved BasePath() = %q, want Keys", got)
	}
	if err := s.SetAll("Other/Keys", "10.0.0.9", "/mnt/hdd"); err != nil {
		t.Fatal(err)
	}
	if srv, _, _ := s.Share(); srv != "10.0.0.9" {
		t.Fatalf("saved share should win over env, got server %q", srv)
	}
}

func TestKeysStoreNodeIPsSnapshot(t *testing.T) {
	p := t.TempDir() + "/keys.json"
	s, err := NewKeysStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAll("ProxyCTL/Keys", "10.0.0.5", "/mnt/ssd/ProxyCTL"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeIPs([]string{"10.0.0.1", "10.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	// Snapshot survives a reload.
	s2, err := NewKeysStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.NodeIPs(); len(got) != 2 || got[0] != "10.0.0.1" {
		t.Fatalf("snapshot not persisted, got %v", got)
	}
	// Re-saving the SAME share keeps the snapshot…
	if err := s2.SetAll("ProxyCTL/Keys", "10.0.0.5", "/mnt/ssd/ProxyCTL"); err != nil {
		t.Fatal(err)
	}
	if got := s2.NodeIPs(); len(got) != 2 {
		t.Fatalf("snapshot dropped on same-share save, got %v", got)
	}
	// …but a DIFFERENT share invalidates it: the old exports line describes
	// nothing about the new share.
	if err := s2.SetAll("ProxyCTL/Keys", "10.0.0.6", "/mnt/hdd/ProxyCTL"); err != nil {
		t.Fatal(err)
	}
	if got := s2.NodeIPs(); len(got) != 0 {
		t.Fatalf("snapshot should reset on share change, got %v", got)
	}
}
