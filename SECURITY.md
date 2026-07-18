# ProxyCTL — security model

ProxyCTL is a **single-operator** control plane. An authenticated admin is
*intentionally* trusted with `ssh` + `kubectl`-equivalent power — that's the
job. The protections below are about who *becomes* that operator, and about
not over-exposing the cluster to the public internet.

## Auth

- First run generates a one-time **bootstrap token** in memory, logged once.
  It only unlocks the `/api/auth/setup` claim — it never authenticates a
  request.
- Claim writes a JWT signing key + bcrypt admin record into the
  `proxyctl-auth` Kubernetes Secret. Thereafter auth is a JWT (HS256, 8h).
- Recovery: `kubectl delete secret proxyctl-auth` + rollout restart → a
  fresh bootstrap token on the next pod log.
- The `DISABLE_AUTH` dev escape hatch refuses to start on a non-loopback
  bind.

## RBAC footprint

ProxyCTL holds the minimum it needs, and no more:

- **ClusterRole, read-only** — `namespaces`, `services`, `pods` (the UI's
  target picker, cluster-wide).
- **ClusterRole, write, one CRD type** — `traefik.io/ingressroutes`. Web
  routes drop an IngressRoute next to each backend app; that needs
  cluster-wide write, but scoped to exactly one resource type.
- **Namespaced Role** (ProxyCTL's own namespace) — the `wg-gw-*` gateway
  lifecycle (Deployments/Secrets/PVCs) + the single `proxyctl-auth` Secret.

ProxyCTL never holds standing droplet credentials (its SSH keypair lives
0600 on the PVC) and never holds cluster-admin. Anything that *would*
require cluster-admin — e.g. deploying the public Traefik below — is
**rendered** by ProxyCTL and applied by the operator with their own
credentials, exactly once.

## Web Routes — use a dedicated public Traefik

**Recommended:** run a separate Traefik instance for the public tunnel,
distinct from whatever Traefik serves your internal (`.lan`) routes. This
keeps public and internal routing cleanly isolated — only the routes you
explicitly publish are reachable from the internet.

The Setup wizard's **Public Traefik** step renders a ready-to-apply
manifest for it. The manifest is scoped so this Traefik only picks up the
IngressRoutes ProxyCTL renders:

```yaml
--providers.kubernetescrd.labelselector=app.kubernetes.io/managed-by=proxyctl
```

Every IngressRoute ProxyCTL renders carries
`app.kubernetes.io/managed-by: proxyctl`, so the public Traefik serves
*only* ProxyCTL-managed web routes and your internal Traefik is never in
the tunnel path.

Recommended deployment:

1. `traefik-public` — its own namespace, label-selector-scoped to
   `managed-by=proxyctl`, its own ClusterIP. (Wizard renders this.)
2. A Port/Game entry: droplet `80:tcp, 443:tcp` → `traefik-public`'s
   ClusterIP. This is the shared web tunnel.
3. Web Routes — each becomes an IngressRoute next to its backend Service.

Only what you explicitly add as a web route is internet-reachable.

## Reporting

This is a homelab side project. Open an issue for anything that looks like a
real boundary crossing (unauthenticated access, cross-tenant escape, secret
disclosure).
