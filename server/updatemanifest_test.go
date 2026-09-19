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

func TestContainerImages(t *testing.T) {
	doc := `
spec:
  initContainers:
    - name: init
      image: __IMAGE__   # storage-init
  containers:
    - name: main
      image: "ghcr.io/x/proxyctl:v1"
    - name: sidecar
      image: ghcr.io/gamectl-hq/wireguard-kube@sha256:deadbeef
  volumes:
    - name: data
      claimName: proxyctl-data
`
	got := containerImages(doc)
	want := []string{"__IMAGE__", "ghcr.io/x/proxyctl:v1", "ghcr.io/gamectl-hq/wireguard-kube@sha256:deadbeef"}
	if len(got) != len(want) {
		t.Fatalf("want %d images, got %d: %#v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("image[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestVerifyDeploymentImages(t *testing.T) {
	const newImage = "ghcr.io/gamectl-hq/proxyctl:v0.6.20"
	deploy := func(body string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n" + body
	}
	cases := []struct {
		name    string
		docs    []string
		wantErr bool
	}{
		{
			name: "newImage plus digest-pinned sidecar is allowed",
			docs: []string{deploy(
				"      initContainers:\n        - { name: init, image: __IMG__ }\n" +
					"      containers:\n" +
					"        - { name: main, image: __IMG__ }\n" +
					"        - { name: wg, image: ghcr.io/gamectl-hq/wireguard-kube@sha256:deadbeef }")},
			wantErr: false,
		},
		{
			name: "floating-tag sidecar is refused",
			docs: []string{deploy(
				"      containers:\n" +
					"        - { name: main, image: __IMG__ }\n" +
					"        - { name: evil, image: evil.example/payload:latest }")},
			wantErr: true,
		},
		{
			name: "unpinned version tag from another registry is refused",
			docs: []string{deploy(
				"      containers:\n" +
					"        - { name: main, image: __IMG__ }\n" +
					"        - { name: evil, image: evil.example/payload:v1 }")},
			wantErr: true,
		},
		{
			name: "non-Deployment docs carry no image policy",
			docs: []string{"apiVersion: v1\nkind: Service\nspec: { ports: [{ port: 80 }] }",
				"apiVersion: v1\nkind: Secret\ndata: { a: b }"},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// resolve the __IMG__ placeholder to newImage the way the real
			// apply path does before verifying.
			resolved := make([]string, len(tc.docs))
			for i, d := range tc.docs {
				resolved[i] = strings.ReplaceAll(d, "__IMG__", newImage)
			}
			err := verifyDeploymentImages(resolved, newImage)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got: %v", err)
			}
		})
	}
}

// TestVerifyDeploymentImages_RealManifest pins the shipped manifest to the
// image policy: it must reference only the release image (via __IMAGE__) and
// digest-pinned first-party images. Catches a future manifest edit that slips
// in a floating-tag container before it reaches the self-apply path.
func TestVerifyDeploymentImages_RealManifest(t *testing.T) {
	b, err := os.ReadFile("../k8s/proxyctl.yaml")
	if err != nil {
		t.Fatalf("reading k8s/proxyctl.yaml: %v", err)
	}
	newImage := "ghcr.io/gamectl-hq/proxyctl:v0.0.0-test"
	doc := substituteManifest(string(b), map[string]string{"__IMAGE__": newImage})
	self, _ := partitionManifest(doc)
	if err := verifyDeploymentImages(self, newImage); err != nil {
		t.Fatalf("shipped manifest should pass the image policy, got: %v", err)
	}

	deploy := ""
	for _, d := range self {
		if docKind(d) == "Deployment" {
			deploy = d
		}
	}
	images := containerImages(deploy)
	if len(images) != 3 {
		t.Fatalf("want 3 container images in the Deployment, got %d: %#v", len(images), images)
	}
	if images[0] != newImage || images[1] != newImage {
		t.Errorf("init+main containers should be newImage %q, got %#v", newImage, images)
	}
	if !strings.Contains(images[2], "@sha256:") {
		t.Errorf("control-wg sidecar should be digest-pinned, got %q", images[2])
	}
}
