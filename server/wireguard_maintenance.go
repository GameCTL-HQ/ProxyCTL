package main

// wireguard — first-party digest-pinned image maintenance for in-cluster
// WireGuard gateways.
//
// The WireGuard data plane is the host kernel; the image carries userspace
// tools only. Its reference is digest-pinned in rendered manifests so a
// re-pointed registry tag cannot move the runtime. Upstream "latest" is read
// from GHCR for maintenance visibility, but an operator must explicitly start
// the update operation, which only re-points the labeled gateway Deployments.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	// wireguardImageDigest pins the reviewed GHCR *index* digest. Kubernetes
	// resolves digest-pinned images at the index level (picking the amd64
	// child), so the pin must be the index digest, not a child manifest
	// digest (the index's second entry is a SLSA provenance attestation,
	// platform "unknown"). Verified 2026-10-27: from-source build of
	// WireGuard/wireguard-tools v1.0.20260223 on Debian 13, amd64 child
	// 9b5e0a70…, in-cluster gateways rolled to it with keys preserved.
	wireguardImageRepo    = "ghcr.io/gamectl-hq/wireguard-kube"
	wireguardImageDigest  = "ghcr.io/gamectl-hq/wireguard-kube@sha256:92bcef1246f573c82d15a055b40932703a9f9ad96d32ace1e6e6ff2bba749b42"
	wireguardGatewayLabel = "proxyctl=gateway"
)

// ghcrBaseURL is the registry endpoint for the upstream "latest" lookup. It
// is a package var so tests can point it at an offline stub.
var ghcrBaseURL = "https://ghcr.io"

// WireguardImageStatus reports the embedded reviewed digest and the latest
// GHCR upstream reference, when that latest lookup succeeds.
type WireguardImageStatus struct {
	CurrentDigest   string `json:"currentDigest"`
	UpstreamRef     string `json:"upstreamRef,omitempty"`
	UpstreamChecked int64  `json:"upstreamChecked,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	LookupError     string `json:"lookupError,omitempty"`
	// BehindDeployments is the number of live gateway Deployments whose
	// wireguard image does not match the expected image. It lets a manual
	// digest update and an upstream update both remain actionable.
	BehindDeployments int `json:"behindDeployments,omitempty"`

	// liveImagesOK reports whether the live gateway image check actually
	// ran (not part of the API payload). The maintenance monitor uses it
	// to avoid treating a failed kubectl check as "nothing behind".
	liveImagesOK bool
}

// wireguardImageStatus safely reports the current digest and best-effort
// upstream state without blocking UI status when GHCR is unavailable. ns
// and kubeContext must match the update path (a.infra + SSHApplier) so
// BehindDeployments is computed against the cluster that would actually be
// rolled.
func wireguardImageStatus(ns, kubeContext string) WireguardImageStatus {
	st := WireguardImageStatus{CurrentDigest: wireguardImageDigest}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ref, err := resolveWireguardLatestRef(ctx)
	if err != nil {
		st.LookupError = err.Error()
		// Without a live lookup, compare the embedded current pin against
		// live Deployments. This catches manually applied image updates
		// even when GHCR is unreachable.
		st.UpstreamRef = st.CurrentDigest
	} else {
		st.UpstreamRef = ref
		st.UpstreamChecked = time.Now().Unix()
		st.UpdateAvailable = ref != wireguardImageDigest
	}
	liveImages, ok := liveWireguardGatewayImages(ns, kubeContext)
	applyLiveWireguardImages(&st, liveImages, ok)
	return st
}

// liveWireguardGatewayImages returns the configured wireguard image for each
// current gateway Deployment. Failures return ok=false; status then
// reports the embedded/upstream image without pretending to have checked
// the cluster.
func liveWireguardGatewayImages(ns, kubeContext string) ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", kubectlContextArgs(kubeContext, "-n", ns, "get", "deployments", "-l", wireguardGatewayLabel,
		"-o", `jsonpath={range .items[*]}{.spec.template.spec.containers[?(@.name=="wireguard")].image}{"\n"}{end}`)...)
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	images := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			images = append(images, line)
		}
	}
	return images, true
}

// applyLiveWireguardImages marks the status as actionable when any live
// gateway Deployment still runs an older image than the expected digest.
// ok=false (kubectl failed) records that the check did not run.
func applyLiveWireguardImages(st *WireguardImageStatus, liveImages []string, ok bool) {
	st.liveImagesOK = ok
	expected := st.UpstreamRef
	if expected == "" {
		expected = st.CurrentDigest
	}
	if expected == "" {
		return
	}
	for _, image := range liveImages {
		if image != expected && image != strings.TrimPrefix(expected, wireguardImageRepo+"@") {
			st.BehindDeployments++
		}
	}
	st.UpdateAvailable = st.UpdateAvailable || st.BehindDeployments > 0
}

// resolveWireguardUpdateRef decides which image reference an update
// operation applies. It prefers the live GHCR "latest" (so pushing a new
// build — e.g. after bumping the upstream wireguard-tools tag — needs no
// ProxyCTL redeploy to roll out); when the lookup fails it falls back to
// the embedded reviewed digest so an unreachable registry cannot block an
// update. The returned source names which reference won, for the UI and
// the operation log.
func resolveWireguardUpdateRef(ctx context.Context) (ref, source string) {
	latest, err := resolveWireguardLatestRef(ctx)
	if err != nil {
		return wireguardImageDigest, "embedded-pin (lookup failed: " + err.Error() + ")"
	}
	return latest, "upstream-latest"
}

type ghcrTokenResponse struct {
	Token string `json:"token"`
}

type ghcrImageIndex struct {
	Manifests []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Platform  struct {
			Architecture string `json:"architecture"`
		} `json:"platform"`
	} `json:"manifests"`
}

var ghcrDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// resolveWireguardLatestRef resolves the latest GHCR manifest to an immutable
// digest reference. It fails closed on ambiguous or malformed indexes.
func resolveWireguardLatestRef(ctx context.Context) (string, error) {
	// Both registry calls share the caller's ctx so the whole lookup is
	// bounded by its deadline (the status endpoint's 10s budget) — there is
	// no independent client timeout that can outlive it.
	client := &http.Client{}
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ghcrBaseURL+"/token?scope=repository:gamectl-hq/wireguard-kube:pull", nil)
	if err != nil {
		return "", err
	}
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return "", fmt.Errorf("ghcr token: %w", err)
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ghcr token status %d", tokenResp.StatusCode)
	}
	var tok ghcrTokenResponse
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil || tok.Token == "" {
		return "", fmt.Errorf("ghcr token response malformed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ghcrBaseURL+"/v2/gamectl-hq/wireguard-kube/manifests/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
	}, ", "))
	manifestResp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ghcr manifest: %w", err)
	}
	defer manifestResp.Body.Close()
	if manifestResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ghcr manifest status %d", manifestResp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(manifestResp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("ghcr manifest read: %w", err)
	}
	// Kubernetes resolves digest-pinned images at the index level, so the
	// returned reference must be the *index* digest. Prefer the registry's
	// authoritative Docker-Content-Digest; fall back to hashing the body.
	digest := manifestResp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	if !ghcrDigestPattern.MatchString(digest) {
		return "", fmt.Errorf("ghcr manifest digest malformed: %q", digest)
	}
	// Fail closed unless the index carries an amd64 image the cluster can run.
	var index ghcrImageIndex
	if err := json.Unmarshal(body, &index); err != nil {
		return "", fmt.Errorf("ghcr manifest decode: %w", err)
	}
	if len(index.Manifests) == 0 {
		// Single-arch manifest (no index children): the digest is that
		// manifest itself; nothing further to validate.
		return wireguardImageRepo + "@" + digest, nil
	}
	hasAMD64 := false
	for _, m := range index.Manifests {
		if m.Platform.Architecture == "amd64" {
			hasAMD64 = true
			break
		}
	}
	if !hasAMD64 {
		return "", fmt.Errorf("ghcr latest index has no amd64 manifest")
	}
	return wireguardImageRepo + "@" + digest, nil
}

func kubectlContextArgs(kubeContext string, args ...string) []string {
	if kubeContext == "" {
		return args
	}
	return append([]string{"--context", kubeContext}, args...)
}

// reconcileWireguardGateways moves every existing ProxyCTL gateway Deployment
// to the embedded reviewed image. It patches only the wireguard/keygen images
// with a strategic merge patch, never touching keys, secrets, selectors, or
// security settings, then waits for each rollout.
func reconcileWireguardGateways(ctx context.Context, kubeContext, ns, image string) (string, error) {
	get := exec.CommandContext(ctx, "kubectl", kubectlContextArgs(kubeContext, "-n", ns, "get", "deployments", "-l", wireguardGatewayLabel, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")...)
	getOut, err := get.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("list wireguard gateways: %w: %s", err, strings.TrimSpace(string(getOut)))
	}
	names := strings.Fields(string(getOut))
	if len(names) == 0 {
		return "no wireguard gateways configured; skipped", nil
	}
	patch := `{
  "spec": {
    "template": {
      "spec": {
        "initContainers": [
          {"name": "keygen", "image": "` + image + `"}
        ],
        "containers": [
          {"name": "wireguard", "image": "` + image + `"}
        ]
      }
    }
  }
}`
	var updated []string
	for _, name := range names {
		apply := exec.CommandContext(ctx, "kubectl", kubectlContextArgs(kubeContext, "-n", ns, "patch", "deployment", name, "--type=strategic", "-p", patch)...)
		out, err := apply.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("patch %s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
		roll := exec.CommandContext(ctx, "kubectl", kubectlContextArgs(kubeContext, "-n", ns, "rollout", "status", "deployment/"+name, "--timeout=180s")...)
		rollOut, err := roll.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("rollout %s: %w: %s", name, err, strings.TrimSpace(string(rollOut)))
		}
		updated = append(updated, name)
	}
	return fmt.Sprintf("updated wireguard gateways: %s", strings.Join(updated, ", ")), nil
}
