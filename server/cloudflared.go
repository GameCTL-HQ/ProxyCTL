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
)

// cloudflaredImage is the connector image. cloudflared is a tiny
// single-binary utility; bump this to pin a newer release.
const cloudflaredImage = "cloudflare/cloudflared:latest"

const cloudflaredName = "proxyctl-cloudflared"

// renderCloudflared returns the Secret (tunnel connector token) +
// Deployment for the cloudflared connector in namespace ns. The token is
// base64-encoded into the Secret's data so no YAML quoting can bite.
func renderCloudflared(ns, token string) string {
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
---
apiVersion: apps/v1
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
      containers:
        - name: cloudflared
          image: %[4]s
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
`, cloudflaredName, ns, tok64, cloudflaredImage)
}

// ensureCloudflared applies the cloudflared Secret + Deployment into ns.
// Idempotent — `kubectl apply` reconciles, and a changed token rolls the
// pods on the next apply.
func ensureCloudflared(ctx context.Context, ns, token string) error {
	_, err := kubectlApplyStdin(ctx, renderCloudflared(ns, token))
	return err
}
