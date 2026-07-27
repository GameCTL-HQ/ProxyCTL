package main

// updatemanifest.go — the manifest-aware half of updateApply (update.go).
//
// `kubectl set image` alone misses manifest drift: a release that adds a
// container, a volume, or an env var (e.g. the control-wg sidecar) needs a
// real `kubectl apply`, or the pod comes up without whatever the new code
// expects and the failure looks unrelated (see setupControlTunnel's
// "no sidecar" check in main.go, added for exactly that symptom).
//
// The running pod only carries the manifest its OWN (older) image shipped
// with, so the manifest to reapply is fetched fresh from GitHub at the
// target release's tag — the same file clusterdeploy.sh applies from a
// local checkout, just reachable from inside the cluster.
//
// Deliberately NOT self-applied: Namespace, ServiceAccount, ClusterRole,
// ClusterRoleBinding, Role, RoleBinding. ProxyCTL's own ServiceAccount has
// no RBAC to modify any of those kinds (see k8s/proxyctl.yaml's "Security
// model" comment) and it stays that way on purpose — letting a running
// build rewrite its own permissions would mean a compromised or buggy
// release could grant itself arbitrary new cluster access. A release that
// changes those objects still gets its Deployment/Service/Secret reapplied
// automatically; the RBAC piece is surfaced as a warning asking a human
// with kubectl access to apply it once, out of band.
import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
)

// manifestSelfApplicableKinds are the object kinds ProxyCTL's own
// ServiceAccount already has RBAC to create/update/patch/delete in its own
// namespace (proxyctl-gateway-mgr's Role covers deployments and secrets;
// "services" is also granted).
var manifestSelfApplicableKinds = map[string]bool{
	"Deployment": true,
	"Service":    true,
	"Secret":     true,
}

// splitYAMLDocs splits a multi-document manifest on "---" separator lines.
func splitYAMLDocs(manifest string) []string {
	lines := strings.Split(manifest, "\n")
	var docs []string
	cur := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "---" {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = cur[:0]
			continue
		}
		cur = append(cur, ln)
	}
	return append(docs, strings.Join(cur, "\n"))
}

// docKind returns a YAML document's top-level "kind:" value, or "" if it
// has none (a pure-comment document, e.g. the header before the first ---).
func docKind(doc string) string {
	for _, ln := range strings.Split(doc, "\n") {
		if t := strings.TrimSpace(ln); strings.HasPrefix(t, "kind:") {
			return strings.TrimSpace(strings.TrimPrefix(t, "kind:"))
		}
	}
	return ""
}

// partitionManifest splits a manifest into the documents ProxyCTL can safely
// reapply against itself (selfApplicable) and the rest (elevated), which
// need a human with a real kubeconfig.
func partitionManifest(manifest string) (selfApplicable, elevated []string) {
	for _, doc := range splitYAMLDocs(manifest) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		if manifestSelfApplicableKinds[docKind(doc)] {
			selfApplicable = append(selfApplicable, doc)
		} else {
			elevated = append(elevated, doc)
		}
	}
	return
}

func joinDocs(docs []string) string { return strings.Join(docs, "\n---\n") }

// manifestRelPath is where the install manifest lives in the repo, at any
// tag — same path clusterdeploy.sh applies from a local checkout.
const manifestRelPath = "k8s/proxyctl.yaml"

func rawManifestURL(repo, tag string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", repo, tag, manifestRelPath)
}

// fetchManifest downloads the install manifest as it shipped at the given
// tag. Used both for the release being upgraded TO (required) and the
// release currently running (best-effort, for drift comparison only).
func (c *updateChecker) fetchManifest(ctx context.Context, tag string) (string, error) {
	if strings.TrimSpace(tag) == "" {
		return "", fmt.Errorf("no tag to fetch a manifest for")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawManifestURL(c.repo, tag), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "proxyctl-update-check")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d fetching manifest for %s", resp.StatusCode, tag)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// substituteManifest applies the same __PLACEHOLDER__ substitutions
// clusterdeploy.sh's sed step does.
func substituteManifest(doc string, subs map[string]string) string {
	for k, v := range subs {
		doc = strings.ReplaceAll(doc, k, v)
	}
	return doc
}

// manifestSubstitutions reads this install's OWN live settings — namespace,
// StorageClass, Service type, and how /data is provisioned — so a reapplied
// manifest preserves them exactly, the same detect-from-live-Deployment
// approach clusterdeploy.sh uses for DATA_VOLUME_SPEC and friends.
func (a *API) manifestSubstitutions(ctx context.Context, image string) (map[string]string, error) {
	dv, err := a.selfDataVolume(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the current data volume: %w", err)
	}
	var volSpec string
	switch dv.Mode {
	case "share":
		volSpec = fmt.Sprintf("nfs: { server: %s, path: %s }", dv.NFSServer, dv.NFSExport)
	case "pvc":
		volSpec = fmt.Sprintf("persistentVolumeClaim: { claimName: %s }", dv.ClaimName)
	default:
		volSpec = "emptyDir: {}"
	}
	svcType, err := a.selfServiceType(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the current Service type: %w", err)
	}
	return map[string]string{
		"__IMAGE__":            image,
		"__NAMESPACE__":        a.selfNS,
		"__STORAGE_CLASS__":    baseStorageClass(),
		"__SERVICE_TYPE__":     svcType,
		"__DATA_VOLUME_SPEC__": volSpec,
		"__DATA_SUBPATH__":     dv.SubPath,
		"__DATA_NFS_SERVER__":  dv.NFSServer,
		"__DATA_NFS_EXPORT__":  dv.NFSExport,
	}, nil
}

// selfServiceType reads the live Service's spec.type (ClusterIP / NodePort /
// LoadBalancer) so a reapplied manifest doesn't reset an operator's choice.
func (a *API) selfServiceType(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, a.kubectl, "-n", a.selfNS,
		"get", "svc/"+a.selfDeploy, "-o", "jsonpath={.spec.type}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", fmt.Errorf("empty Service type from %s/%s", a.selfNS, a.selfDeploy)
	}
	return t, nil
}

func (a *API) kubectlSetImage(ctx context.Context, containerName, image string) error {
	out, err := exec.CommandContext(ctx, a.kubectl, "-n", a.selfNS,
		"set", "image", "deploy/"+a.selfDeploy, containerName+"="+image).CombinedOutput()
	if err != nil {
		return fmt.Errorf("set image failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *API) kubectlApplyManifest(ctx context.Context, manifest string) error {
	cmd := exec.CommandContext(ctx, a.kubectl, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kubectl apply failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// releaseApplyResult is what applyRelease actually did, for updateApply's
// HTTP response.
type releaseApplyResult struct {
	message     string
	rbacWarning string // non-empty when the release also needs an out-of-band RBAC apply
}

// applyRelease moves this install to newImage, doing whatever that release
// actually needs: a plain `set image` when nothing structural changed
// between the running tag and latest, or a real `kubectl apply` of the
// Deployment/Service/Secret documents (image + namespace + storage +
// service-type substituted from THIS install's live settings) when it did.
//
// Manifest-fetch failures for the running tag (curTag unparsable, a "dev"
// build, transient network trouble) are NOT treated as "no drift" — they
// fall back to assuming drift and doing the real apply, the same
// full-apply-by-default safety margin clusterdeploy.sh already uses.
func (a *API) applyRelease(ctx context.Context, containerName, newImage, curTag, latest string) (releaseApplyResult, error) {
	target, err := a.upd.fetchManifest(ctx, latest)
	if err != nil {
		return releaseApplyResult{}, fmt.Errorf("couldn't fetch the %s release manifest to check for drift: %w", latest, err)
	}
	targetSelf, targetElevated := partitionManifest(target)

	driftSelf, driftElevated := true, true
	if cur, curErr := a.upd.fetchManifest(ctx, curTag); curErr == nil {
		curSelf, curElevated := partitionManifest(cur)
		driftSelf = joinDocs(curSelf) != joinDocs(targetSelf)
		driftElevated = joinDocs(curElevated) != joinDocs(targetElevated)
	}

	if !driftSelf && !driftElevated {
		if err := a.kubectlSetImage(ctx, containerName, newImage); err != nil {
			return releaseApplyResult{}, err
		}
		return releaseApplyResult{
			message: "Update started — ProxyCTL is rolling to " + latest + ". This page will reconnect in a few seconds.",
		}, nil
	}

	subs, err := a.manifestSubstitutions(ctx, newImage)
	if err != nil {
		return releaseApplyResult{}, fmt.Errorf("couldn't read this install's current settings to reapply the manifest: %w", err)
	}
	applyDocs := make([]string, len(targetSelf))
	for i, d := range targetSelf {
		applyDocs[i] = substituteManifest(d, subs)
	}
	if err := a.kubectlApplyManifest(ctx, joinDocs(applyDocs)); err != nil {
		return releaseApplyResult{}, err
	}

	res := releaseApplyResult{
		message: "Update started — ProxyCTL is reapplying its manifest (picking up any new containers/volumes/env) " +
			"and rolling to " + latest + ". This page will reconnect in a few seconds.",
	}
	if driftElevated {
		res.rbacWarning = latest + " also changes cluster permissions (RBAC/ServiceAccount) that ProxyCTL can't grant " +
			"itself — its Deployment/Service/Secret were updated, but ask someone with kubectl access to also run: " +
			"kubectl apply -f " + rawManifestURL(a.upd.repo, latest) + " (or scripts/clusterdeploy.sh) once."
	}
	return res, nil
}
