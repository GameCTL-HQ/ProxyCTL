package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderCloudflaredUsesHardenedPodSecurity(t *testing.T) {
	manifest := renderCloudflared("proxyctl", "test-token")
	for _, want := range []string{
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"runAsUser: 65532",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"drop: [\"ALL\"]",
		"seccompProfile: { type: RuntimeDefault }",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("renderCloudflared manifest missing %q", want)
		}
	}
}

func TestCloudflaredImageIsCurrentPinnedDigest(t *testing.T) {
	const want = "cloudflare/cloudflared@sha256:c6c7ae1d5dfdcb094ad344ed68ccffc26635c9c6be80965664c33baf064b0515"
	if cloudflaredImage != want {
		t.Fatalf("cloudflaredImage = %q, want %q", cloudflaredImage, want)
	}
	if cloudflaredVersion != "2026.10.4" {
		t.Fatalf("cloudflaredVersion = %q", cloudflaredVersion)
	}
}

func TestRenderCloudflaredDeploymentCanReconcileImageWithoutTokenSecret(t *testing.T) {
	manifest := renderCloudflaredDeployment("proxyctl")
	if strings.Contains(manifest, "kind: Secret") || strings.Contains(manifest, "test-token") {
		t.Fatalf("deployment-only reconcile must not rewrite tunnel credentials: %s", manifest)
	}
	if !strings.Contains(manifest, cloudflaredImage) {
		t.Fatalf("deployment-only reconcile missing pinned image %q", cloudflaredImage)
	}
}

func TestReconcileCloudflaredDeploymentRollsPinnedImageWithoutSecret(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kubectl.log")
	script := `#!/bin/sh
echo "$*" >> "$KUBECTL_TEST_LOG"
case " $* " in *" apply "*) cat >> "$KUBECTL_TEST_LOG" ;; esac
echo ok
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECTL_TEST_LOG", logPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := reconcileCloudflaredDeployment(ctx, "lapstack", "proxyctl"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"--context lapstack -n proxyctl get deployment " + cloudflaredName,
		"--context lapstack apply -f -",
		"--context lapstack -n proxyctl rollout status deployment/" + cloudflaredName,
		cloudflaredImage,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kubectl reconciliation log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "kind: Secret") {
		t.Fatal("image reconciliation rewrote the tunnel Secret")
	}
}

func TestReconcileCloudflaredDeploymentSkipsOnlyMissingConnector(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$KUBECTL_TEST_ERROR" in
  deployment) echo 'Error from server (NotFound): deployments.apps "proxyctl-cloudflared" not found' >&2 ;;
  namespace) echo 'Error from server (NotFound): namespaces "proxyctl" not found' >&2 ;;
esac
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("KUBECTL_TEST_ERROR", "deployment")
	if got, err := reconcileCloudflaredDeployment(context.Background(), "lapstack", "proxyctl"); err != nil || !strings.Contains(got, "skipped") {
		t.Fatalf("missing connector should skip: got=%q err=%v", got, err)
	}
	t.Setenv("KUBECTL_TEST_ERROR", "namespace")
	if _, err := reconcileCloudflaredDeployment(context.Background(), "lapstack", "proxyctl"); err == nil {
		t.Fatal("missing namespace must fail instead of being reported as an optional connector skip")
	}
}
