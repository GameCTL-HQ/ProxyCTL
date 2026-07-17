package main

import (
	"os"
	"strings"
	"testing"
)

func TestBaseStorageClass_EnvAndFallbacks(t *testing.T) {
	old, had := os.LookupEnv("PROXYCTL_STORAGE_CLASS")
	t.Cleanup(func() {
		if had {
			os.Setenv("PROXYCTL_STORAGE_CLASS", old)
		} else {
			os.Unsetenv("PROXYCTL_STORAGE_CLASS")
		}
	})

	for env, want := range map[string]string{
		"longhorn": "longhorn",
		"  ssd  ":  "ssd",
		// Unset, or a manifest applied without substitution, must fall back
		// rather than produce a class literally named "__STORAGE_CLASS__".
		"":                  legacyBaseStorageClass,
		"__STORAGE_CLASS__": legacyBaseStorageClass,
	} {
		os.Setenv("PROXYCTL_STORAGE_CLASS", env)
		if got := baseStorageClass(); got != want {
			t.Errorf("env %q: want %q, got %q", env, want, got)
		}
	}
}

// The bug this guards: a hardcoded provisioner only matches the cluster it was
// copied from. The rendered class must name whatever provisioner was
// discovered, or every keys PVC sits Pending against one that doesn't exist.
func TestRenderKeysStorageClass_UsesDiscoveredProvisioner(t *testing.T) {
	got := renderKeysStorageClassWith("ProxyCTL/Keys", "cluster.local/acme-nfs-subdir-external-provisioner")
	if !strings.Contains(got, "provisioner: cluster.local/acme-nfs-subdir-external-provisioner\n") {
		t.Errorf("discovered provisioner not used:\n%s", got)
	}
	if strings.Contains(got, "nfs-ssd-nfs-subdir") {
		t.Errorf("maintainer's provisioner leaked into the rendered class:\n%s", got)
	}
}

// Discovery can legitimately fail (trimmed RBAC). Rendering a class with an
// empty provisioner would be rejected by the API server, so fall back.
func TestRenderKeysStorageClass_EmptyProvisionerFallsBack(t *testing.T) {
	got := renderKeysStorageClassWith("ProxyCTL/Keys", "")
	if !strings.Contains(got, "provisioner: "+legacyKeysProvisioner+"\n") {
		t.Errorf("want fallback provisioner, got:\n%s", got)
	}
}

func TestRenderKeysStorageClass_PathPatternHonoursFolder(t *testing.T) {
	got := renderKeysStorageClassWith("Custom/Folder", "p")
	if !strings.Contains(got, `pathPattern: "Custom/Folder/${.PVC.namespace}-${.PVC.name}"`) {
		t.Errorf("folder not honoured:\n%s", got)
	}
}
