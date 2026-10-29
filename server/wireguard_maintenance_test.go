package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWireguardImageIsPinnedToReviewedDigest(t *testing.T) {
	// Pinned to the reviewed GHCR *index* digest (k8s resolves index-level).
	want := "ghcr.io/gamectl-hq/wireguard-kube@sha256:92bcef1246f573c82d15a055b40932703a9f9ad96d32ace1e6e6ff2bba749b42"
	if wireguardImageDigest != want {
		t.Fatalf("wireguardImageDigest = %q, want %q", wireguardImageDigest, want)
	}
}

func TestRenderGatewayUsesPinnedWireguardImage(t *testing.T) {
	in := DefaultInfra()
	e := &Entry{Name: "Test Game", TunnelIP: "10.8.0.2"}
	manifest := renderGatewayManifest(in, e)
	if !strings.Contains(manifest, wireguardImageDigest) {
		t.Fatalf("gateway manifest missing %q", wireguardImageDigest)
	}
	if strings.Contains(manifest, "ghcr.io/gamectl-hq/wireguard-kube:latest") {
		t.Fatal("gateway manifest used a mutable WireGuard tag")
	}
}

func TestReconcileWireguardGatewaysPatchesImagesOnly(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kubectl.log")
	script := `#!/bin/sh
echo "$*" >> "$KUBECTL_TEST_LOG"
case " $* " in
  *"-o jsonpath="*) printf 'wg-gw-a\nwg-gw-b\n' ;;
  *) echo ok ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECTL_TEST_LOG", logPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := reconcileWireguardGateways(ctx, "lapstack", "proxyctl", "ghcr.io/gamectl-hq/wireguard-kube@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "wg-gw-a") || !strings.Contains(got, "wg-gw-b") {
		t.Fatalf("unexpected reconciliation result %q", got)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	for _, want := range []string{
		`--context lapstack -n proxyctl get deployments -l proxyctl=gateway`,
		`"name": "keygen"`,
		`"name": "wireguard"`,
		`"image": "ghcr.io/gamectl-hq/wireguard-kube@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`,
		`--context lapstack -n proxyctl rollout status deployment/wg-gw-a --timeout=180s`,
		`--context lapstack -n proxyctl rollout status deployment/wg-gw-b --timeout=180s`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("kubectl log missing %q:\n%s", want, log)
		}
	}
	if !strings.Contains(log, `patch deployment wg-gw-a --type=strategic -p`) || !strings.Contains(log, `patch deployment wg-gw-b --type=strategic -p`) {
		t.Errorf("strategic merge patch missing:\n%s", log)
	}
}

func TestWireguardImageStatusFailsOpenForLookups(t *testing.T) {
	// Offline: point the lookup at a stub that always fails. The status must
	// still report the embedded reviewed digest and flag the lookup error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	old := ghcrBaseURL
	ghcrBaseURL = srv.URL
	defer func() { ghcrBaseURL = old }()

	// Fake kubectl on PATH so the live gateway check cannot reach a real
	// cluster from a unit test; the stub reports no gateways (empty output).
	binDir := t.TempDir()
	script := `#!/bin/sh
echo "$*" >> "$KUBECTL_TEST_LOG"
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECTL_TEST_LOG", filepath.Join(binDir, "kubectl.log"))
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	status := wireguardImageStatus("proxyctl", "")
	if status.CurrentDigest != wireguardImageDigest {
		t.Fatalf("current = %q, want pinned digest", status.CurrentDigest)
	}
	if status.UpdateAvailable {
		t.Fatal("updateAvailable must be false when the lookup fails")
	}
	if status.LookupError == "" {
		t.Fatal("lookupError must be set when the registry is unreachable")
	}
	if status.UpstreamRef != wireguardImageDigest {
		t.Fatalf("upstreamRef = %q, want current digest after a failed lookup", status.UpstreamRef)
	}
	if !status.liveImagesOK {
		t.Fatal("liveImagesOK must be true when the kubectl check ran")
	}
	klog, err := os.ReadFile(filepath.Join(binDir, "kubectl.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(klog), "-n proxyctl get deployments -l proxyctl=gateway") {
		t.Errorf("kubectl check did not query the requested namespace:\n%s", klog)
	}
}

func TestApplyLiveWireguardImagesDetectsGatewayBehindExpectedDigest(t *testing.T) {
	status := WireguardImageStatus{
		CurrentDigest: "sha256:new",
		UpstreamRef:   "ghcr.io/gamectl-hq/wireguard-kube@sha256:new",
	}
	applyLiveWireguardImages(&status, []string{
		"sha256:new",
		"sha256:old",
	}, true)
	if !status.UpdateAvailable || status.BehindDeployments != 1 {
		t.Fatalf("status = %#v", status)
	}
	if !status.liveImagesOK {
		t.Fatal("liveImagesOK must be true when the check ran")
	}
}

func TestApplyLiveWireguardImagesRecordsFailedCheck(t *testing.T) {
	status := WireguardImageStatus{
		CurrentDigest: "sha256:new",
		UpstreamRef:   "ghcr.io/gamectl-hq/wireguard-kube@sha256:new",
	}
	applyLiveWireguardImages(&status, nil, false)
	if status.liveImagesOK {
		t.Fatal("liveImagesOK must be false when kubectl failed")
	}
	if status.UpdateAvailable || status.BehindDeployments != 0 {
		t.Fatalf("failed check must not fabricate behind deployments: %#v", status)
	}
}

func TestResolveWireguardLatestRefOffline(t *testing.T) {
	// Offline: stub GHCR token + a canned multi-arch OCI index. The resolver
	// must return the *index* digest (from Docker-Content-Digest), not a
	// child manifest digest, and fail closed if no amd64 child is present.
	const indexDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const amd64Digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"stub"}`)
		case r.URL.Path == "/v2/gamectl-hq/wireguard-kube/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", indexDigest)
			fmt.Fprintf(w, `{"manifests":[
				{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%[1]s","platform":{"architecture":"amd64"}},
				{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210","platform":{"architecture":"arm64"}}
			]}`, amd64Digest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := ghcrBaseURL
	ghcrBaseURL = srv.URL
	defer func() { ghcrBaseURL = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, err := resolveWireguardLatestRef(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := wireguardImageRepo + "@" + indexDigest; ref != want {
		t.Fatalf("ref = %q, want index digest ref %q", ref, want)
	}
	if strings.Contains(ref, amd64Digest) {
		t.Fatalf("ref %q used the amd64 child digest, not the index digest", ref)
	}
}

func TestResolveWireguardUpdateRefPrefersUpstream(t *testing.T) {
	const indexDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			fmt.Fprint(w, `{"token":"stub"}`)
		case r.URL.Path == "/v2/gamectl-hq/wireguard-kube/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", indexDigest)
			fmt.Fprint(w, `{"manifests":[
				{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","platform":{"architecture":"amd64"}}
			]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := ghcrBaseURL
	ghcrBaseURL = srv.URL
	defer func() { ghcrBaseURL = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, source := resolveWireguardUpdateRef(ctx)
	if want := wireguardImageRepo + "@" + indexDigest; ref != want {
		t.Fatalf("ref = %q, want upstream index ref %q", ref, want)
	}
	if source != "upstream-latest" {
		t.Fatalf("source = %q, want upstream-latest", source)
	}
}

func TestResolveWireguardUpdateRefFallsBackToPin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	old := ghcrBaseURL
	ghcrBaseURL = srv.URL
	defer func() { ghcrBaseURL = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, source := resolveWireguardUpdateRef(ctx)
	if ref != wireguardImageDigest {
		t.Fatalf("ref = %q, want the embedded reviewed pin after a failed lookup", ref)
	}
	if !strings.HasPrefix(source, "embedded-pin") {
		t.Fatalf("source = %q, want embedded-pin fallback label", source)
	}
}

func TestResolveWireguardLatestRefFailsClosedWithoutAMD64(t *testing.T) {
	const indexDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			fmt.Fprint(w, `{"token":"stub"}`)
		case r.URL.Path == "/v2/gamectl-hq/wireguard-kube/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", indexDigest)
			fmt.Fprint(w, `{"manifests":[
				{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210","platform":{"architecture":"arm64"}}
			]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := ghcrBaseURL
	ghcrBaseURL = srv.URL
	defer func() { ghcrBaseURL = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := resolveWireguardLatestRef(ctx); err == nil {
		t.Fatal("expected an error when the index has no amd64 manifest")
	}
}
