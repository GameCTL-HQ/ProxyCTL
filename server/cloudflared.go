package main

// cloudflared — the in-cluster connector that runs the Cloudflare Tunnel
// carrying web-app traffic. ProxyCTL renders AND applies this itself: a
// Deployment + token Secret in ProxyCTL's own namespace, both covered by
// the existing namespaced Role. cloudflared needs zero kube RBAC — it
// only dials outbound to Cloudflare's edge and reaches Service
// ClusterIPs, which any pod can do.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
)

// cloudflaredImage is the connector image, pinned by digest so a re-pointed
// tag (or a compromised registry) can't swap the binary ProxyCTL runs.
const (
	cloudflaredVersion = "2026.10.4"
	cloudflaredImage   = "cloudflare/cloudflared@sha256:c6c7ae1d5dfdcb094ad344ed68ccffc26635c9c6be80965664c33baf064b0515"
	cloudflaredName    = "proxyctl-cloudflared"
)

func renderCloudflaredSecret(ns, token string) string {
	tok64 := base64.StdEncoding.EncodeToString([]byte(token))
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %[1]s
  namespace: %[2]s
  labels: { app.kubernetes.io/managed-by: proxyctl }
type: Opaque
data:
  token: %[3]s
`, cloudflaredName, ns, tok64)
}

// renderCloudflaredDeployment is intentionally separate from the token Secret:
// maintenance can roll the connector image without reading or rewriting tunnel
// credentials.
func renderCloudflaredDeployment(ns string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
  labels: { app.kubernetes.io/name: %[1]s, app.kubernetes.io/managed-by: proxyctl }
spec:
  # Two connectors → the tunnel keeps serving across a pod restart.
  replicas: 2
  selector:
    matchLabels: { app.kubernetes.io/name: %[1]s }
  template:
    metadata:
      labels: { app.kubernetes.io/name: %[1]s }
    spec:
      automountServiceAccountToken: false
      securityContext:
        seccompProfile: { type: RuntimeDefault }
      containers:
        - name: cloudflared
          image: %[3]s
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            runAsGroup: 65532
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          args: ["tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run"]
          env:
            - name: TUNNEL_TOKEN
              valueFrom:
                secretKeyRef: { name: %[1]s, key: token }
          readinessProbe:
            httpGet: { path: /ready, port: 2000 }
            initialDelaySeconds: 5
          livenessProbe:
            httpGet: { path: /ready, port: 2000 }
            initialDelaySeconds: 15
          resources:
            requests: { cpu: 25m, memory: 32Mi }
            limits:   { cpu: 200m, memory: 128Mi }
`, cloudflaredName, ns, cloudflaredImage)
}

// renderCloudflared returns the token Secret plus connector Deployment.
func renderCloudflared(ns, token string) string {
	return renderCloudflaredSecret(ns, token) + "---\n" + renderCloudflaredDeployment(ns)
}

// ensureCloudflared applies the cloudflared Secret + Deployment into ns.
// Idempotent — `kubectl apply` reconciles, and a changed token rolls the
// pods on the next apply.
func ensureCloudflared(ctx context.Context, ns, token string) error {
	_, err := kubectlApplyStdin(ctx, renderCloudflared(ns, token))
	return err
}

// reconcileCloudflaredDeployment upgrades an existing in-cluster connector to
// this ProxyCTL build's reviewed, digest-pinned cloudflared release. It never
// reads or rewrites the tunnel token Secret. A cluster with no connector is a
// valid configuration and is left untouched.
func reconcileCloudflaredDeployment(ctx context.Context, kubeContext, ns string) (string, error) {
	kubectlArgs := func(args ...string) []string {
		if kubeContext == "" {
			return args
		}
		return append([]string{"--context", kubeContext}, args...)
	}
	get := exec.CommandContext(ctx, "kubectl", kubectlArgs("-n", ns, "get", "deployment", cloudflaredName)...)
	getOut, err := get.CombinedOutput()
	if err != nil {
		getText := string(getOut)
		missingConnector := strings.Contains(getText, `deployments.apps "`+cloudflaredName+`" not found`) ||
			strings.Contains(getText, `deployment.apps "`+cloudflaredName+`" not found`)
		if missingConnector {
			return "cloudflared connector not configured; skipped", nil
		}
		return "", fmt.Errorf("find cloudflared deployment: %w: %s", err, strings.TrimSpace(getText))
	}
	apply := exec.CommandContext(ctx, "kubectl", kubectlArgs("apply", "-f", "-")...)
	apply.Stdin = strings.NewReader(renderCloudflaredDeployment(ns))
	applyOut, err := apply.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("apply cloudflared %s: %w: %s", cloudflaredVersion, err, strings.TrimSpace(string(applyOut)))
	}
	roll := exec.CommandContext(ctx, "kubectl", kubectlArgs("-n", ns, "rollout", "status",
		"deployment/"+cloudflaredName, "--timeout=120s")...)
	rollOut, err := roll.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("cloudflared rollout: %w: %s", err, strings.TrimSpace(string(rollOut)))
	}
	return strings.TrimSpace(string(applyOut) + "\n" + string(rollOut)), nil
}
