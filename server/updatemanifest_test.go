package main

import (
	"os"
	"strings"
	"testing"
)

func TestSplitYAMLDocsAndDocKind(t *testing.T) {
	manifest := "# header comment\n---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: x\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: y\n"
	docs := splitYAMLDocs(manifest)
	if len(docs) != 3 {
		t.Fatalf("want 3 docs (header + 2), got %d: %#v", len(docs), docs)
	}
	if k := docKind(docs[0]); k != "" {
		t.Errorf("header doc: want no kind, got %q", k)
	}
	if k := docKind(docs[1]); k != "Namespace" {
		t.Errorf("doc 1: want Namespace, got %q", k)
	}
	if k := docKind(docs[2]); k != "Deployment" {
		t.Errorf("doc 2: want Deployment, got %q", k)
	}
}

// partitionManifest is the load-bearing security boundary for the in-app
// update path: only Deployment/Service/Secret may ever be self-applied by
// ProxyCTL's own ServiceAccount, since that's all its RBAC covers. Assert
// this against the REAL shipped manifest, not a fixture, so an edit to
// k8s/proxyctl.yaml that adds a new object kind gets caught here instead of
// silently either (a) never being reapplied by updateApply or (b) being
// self-applied against RBAC that doesn't actually permit it.
func TestPartitionManifest_RealManifest(t *testing.T) {
	b, err := os.ReadFile("../k8s/proxyctl.yaml")
	if err != nil {
		t.Fatalf("reading k8s/proxyctl.yaml: %v", err)
	}
	self, elevated := partitionManifest(string(b))

	wantSelf := map[string]int{"Deployment": 1, "Service": 1, "Secret": 1}
	gotSelf := map[string]int{}
	for _, d := range self {
		gotSelf[docKind(d)]++
	}
	for k, want := range wantSelf {
		if gotSelf[k] != want {
			t.Errorf("self-applicable kind %s: want %d, got %d", k, want, gotSelf[k])
		}
	}
	for k := range gotSelf {
		if !manifestSelfApplicableKinds[k] {
			t.Errorf("doc kind %s ended up in the self-applicable set but isn't in manifestSelfApplicableKinds", k)
		}
	}

	wantElevatedKinds := []string{"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"}
	gotElevated := map[string]bool{}
	for _, d := range elevated {
		gotElevated[docKind(d)] = true
	}
	for _, k := range wantElevatedKinds {
		if !gotElevated[k] {
			t.Errorf("expected elevated kind %s not found in manifest partition", k)
		}
		if manifestSelfApplicableKinds[k] {
			t.Errorf("kind %s is marked self-applicable but grants/scopes permissions — must stay elevated", k)
		}
	}
}

func TestSubstituteManifest(t *testing.T) {
	doc := "image: __IMAGE__\nnamespace: __NAMESPACE__\ndata:\n  __DATA_VOLUME_SPEC__\nsubPath: \"__DATA_SUBPATH__\""
	subs := map[string]string{
		"__IMAGE__":            "ghcr.io/x/proxyctl:v1.2.3",
		"__NAMESPACE__":        "proxyctl",
		"__DATA_VOLUME_SPEC__": "nfs: { server: 10.0.0.5, path: /share }",
		"__DATA_SUBPATH__":     "app",
	}
	got := substituteManifest(doc, subs)
	for _, want := range []string{
		"image: ghcr.io/x/proxyctl:v1.2.3",
		"namespace: proxyctl",
		"nfs: { server: 10.0.0.5, path: /share }",
		`subPath: "app"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("substituted manifest missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "__") {
		t.Errorf("substituted manifest still has an unreplaced placeholder:\n%s", got)
	}
}

func TestJoinDocsRoundTrip(t *testing.T) {
	docs := []string{"a: 1", "b: 2"}
	joined := joinDocs(docs)
	if joined != "a: 1\n---\nb: 2" {
		t.Errorf("unexpected join: %q", joined)
	}
}
