#!/usr/bin/env bash
#
# ProxyCTL installer — deploys ProxyCTL into the cluster your current
# kubectl context points at, and wires up how you'll reach the web UI.
#
# Mirrors GameCTL's install.sh shape: prompts for image + namespace +
# exposure mode (auto / ingress / loadbalancer / nodeport / portforward),
# generates a fresh admin token, applies a single manifest, prints how to
# reach the UI.
#
# Once installed, the SETUP WIZARDS in the UI handle the rest:
#   - Droplet:    generate SSH key → install pubkey on droplet → Test → Save
#   - Cloudflare: paste API token (phase 2)
# This script never asks for any of those — it only handles deployment
# and UI networking. ProxyCTL stores no credentials until you choose to,
# via the wizards, on the PVC.
#
# Usage:
#   ./scripts/install.sh
#   PROXYCTL_IMAGE=registry.git.example.com/admin/proxyctl:v1 ./scripts/install.sh
#
# Honoured environment variables (skip the matching prompt when set):
#   PROXYCTL_IMAGE            container image the cluster can pull
#                             (default: registry.example.com:5000/proxyctl:dev — the
#                              homelab registry. Override for any other env.)
#   PROXYCTL_NAMESPACE        install namespace            (default: proxyctl).
#                             Also where wg-gw-* gateways live — one
#                             ProxyCTL fronts Services in any other ns
#                             via cross-namespace ClusterIP.
#   PROXYCTL_STORAGE_CLASS    PVC storage class            (default: nfs-ssd)
#   PROXYCTL_HOST             UI hostname for Ingress mode
#   PROXYCTL_INGRESS_CLASS    ingress class                (default: detected/traefik)
#   PROXYCTL_EXPOSE           ingress | loadbalancer | nodeport | portforward (default: auto)
#   PROXYCTL_MANIFEST         path to the manifest         (default: $repo/k8s/proxyctl.yaml)
#   PROXYCTL_ASSUME_YES       set to 1 for non-interactive (fails if input needed)
#
# NOTE: the admin BOOTSTRAP TOKEN is NOT taken from env. ProxyCTL
# generates it freshly in memory on first start, logs it, and consumes
# it on claim — this script scrapes it from `kubectl logs` after the
# pod is up so the operator only ever sees it once.
#
set -euo pipefail

# --- Defaults / env ---------------------------------------------------------
NS="${PROXYCTL_NAMESPACE:-proxyctl}"
SC="${PROXYCTL_STORAGE_CLASS:-nfs-ssd}"
# Default to the homelab registry image so `./scripts/install.sh` Just
# Works without flags during testing. Override with `PROXYCTL_IMAGE=…`
# for any other registry (the prompt below also lets you change it).
IMAGE="${PROXYCTL_IMAGE:-registry.example.com:5000/proxyctl:dev}"
HOST="${PROXYCTL_HOST:-}"
INGRESS_CLASS="${PROXYCTL_INGRESS_CLASS:-}"
EXPOSE="${PROXYCTL_EXPOSE:-auto}"
ASSUME_YES="${PROXYCTL_ASSUME_YES:-0}"

HERE="$(cd "$(dirname "$0")/.." && pwd)"
MANIFEST="${PROXYCTL_MANIFEST:-$HERE/k8s/proxyctl.yaml}"

# Color helpers — same shape as GameCTL's install.sh so the two installers
# render identically: blue ==>, green ok, yellow warn, red fail.
c_blue=$'\033[1;34m'; c_grn=$'\033[1;32m'; c_yel=$'\033[1;33m'
c_red=$'\033[1;31m'; c_rst=$'\033[0m'
say()  { printf '%s==>%s %s\n' "$c_blue" "$c_rst" "$*"; }
ok()   { printf '%s ok %s %s\n' "$c_grn"  "$c_rst" "$*"; }
warn() { printf '%swarn%s %s\n' "$c_yel"  "$c_rst" "$*" >&2; }
err()  { printf '%sfail%s %s\n' "$c_red"  "$c_rst" "$*" >&2; }
die()  { err "$*"; exit 1; }

# --- Tiny prompt helpers (TTY-bound; fail loud under -e if no TTY) ---------
ask(){
  local prompt="$1" def="${2:-}" envname="${3:-}" ans=""
  [ "$ASSUME_YES" = "1" ] && { printf '%s\n' "$def"; return; }
  if [ -n "$def" ]; then read -r -p "$prompt [$def]: " ans </dev/tty || true
  else                   read -r -p "$prompt: " ans </dev/tty || true; fi
  ans="${ans:-$def}"
  [ -n "$envname" ] && export "$envname=$ans"
  printf '%s\n' "$ans"
}
ask_required(){
  local prompt="$1" def="${2:-}" envname="${3:-}" ans=""
  ans="$(ask "$prompt" "$def" "$envname")"
  while [ -z "$ans" ]; do
    [ "$ASSUME_YES" = "1" ] && { err "missing required: $prompt"; exit 1; }
    read -r -p "$prompt: " ans </dev/tty || true
  done
  printf '%s\n' "$ans"
}
confirm_yes(){
  # Strict whitelist (matches GameCTL's confirm_proceed): only empty
  # input (the [Y] default) or an explicit y/yes is "yes". Anything
  # else — typos like "t", "x", "asdf", etc. — counts as no and the
  # caller aborts. Prevents misclicks from blindly applying the
  # manifest.
  [ "$ASSUME_YES" = "1" ] && return 0
  local ans; read -r -p "$1 [Y/n]: " ans </dev/tty || true
  case "$ans" in ""|y|Y|yes|Yes|YES) return 0 ;; *) return 1 ;; esac
}

# --- Preflight: kubectl + cluster reachable --------------------------------
command -v kubectl >/dev/null || { err "kubectl not on PATH"; exit 1; }
if ! kubectl version --request-timeout=5s >/dev/null 2>&1; then
  err "kubectl cannot reach the cluster (current context: $(kubectl config current-context 2>/dev/null || echo none))"
  exit 1
fi
ok "Cluster reachable: $(kubectl config current-context)"

# --- Image (defaults to the homelab registry for fast testing) -------------
# The prompt defaults to the env value (or our homelab default if unset).
# Hit Enter to accept; type a different image to override.
IMAGE="$(ask "Container image the cluster can pull" "$IMAGE")"
say "Image: ${IMAGE}"

# --- Detect cluster features for exposure-mode auto-selection ---------------
HAS_INGRESS=0; HAS_METALLB=0
kubectl get ingressclass -o name >/dev/null 2>&1 && HAS_INGRESS=1
kubectl -n metallb-system get cm config >/dev/null 2>&1 || \
  kubectl get ipaddresspool.metallb.io -A >/dev/null 2>&1 && HAS_METALLB=1
INGRESS_CLASSES="$(kubectl get ingressclass -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)"

if [ "$EXPOSE" = "auto" ]; then
  if   [ "$HAS_INGRESS" -eq 1 ]; then default_expose="ingress"
  elif [ "$HAS_METALLB" -eq 1 ]; then default_expose="loadbalancer"
  else                                 default_expose="portforward"; fi
  if [ "$ASSUME_YES" = "1" ]; then
    EXPOSE="$default_expose"
  else
    echo
    echo "How would you like to reach the ProxyCTL UI?"
    echo "  1) Ingress       — via a hostname through your ingress controller"
    [ "$HAS_INGRESS" -eq 1 ] && echo "                     (detected ingress classes: $INGRESS_CLASSES)" \
                             || echo "                     (no ingress controller detected — needs one)"
    echo "  2) LoadBalancer  — an external IP via MetalLB or your cloud LB"
    [ "$HAS_METALLB" -eq 1 ] && echo "                     (MetalLB detected)" \
                             || echo "                     (no MetalLB / cloud LB detected)"
    echo "  3) NodePort      — a high port on every node"
    echo "  4) Port-forward  — Service is ClusterIP-only; reach it via"
    echo "                     'kubectl -n $NS port-forward svc/proxyctl 8080:80'"
    echo "                     (no cluster networking changes; safest for an admin app)"
    case "$default_expose" in ingress) d=1;; loadbalancer) d=2;; nodeport) d=3;; portforward) d=4;; esac
    sel="$(ask "Pick one" "$d")"
    case "$sel" in
      1) EXPOSE="ingress" ;;
      2) EXPOSE="loadbalancer" ;;
      3) EXPOSE="nodeport" ;;
      4) EXPOSE="portforward" ;;
      *) EXPOSE="$default_expose" ;;
    esac
  fi
fi
say "UI exposure mode: ${EXPOSE}"

# --- Ingress hostname (only when ingress is selected) ----------------------
if [ "$EXPOSE" = "ingress" ]; then
  HOST="$(ask_required "Hostname the UI will be reached on (e.g. proxyctl.example.com)" "$HOST" PROXYCTL_HOST)"
  if [ -z "$INGRESS_CLASS" ]; then
    INGRESS_CLASS="$(printf '%s\n' "$INGRESS_CLASSES" | awk '{print $1}')"
    [ -z "$INGRESS_CLASS" ] && INGRESS_CLASS="traefik"
  fi
fi

# --- Map EXPOSE to Service type --------------------------------------------
case "$EXPOSE" in
  loadbalancer) SVC_TYPE="LoadBalancer" ;;
  nodeport)     SVC_TYPE="NodePort" ;;
  *)            SVC_TYPE="ClusterIP" ;;
esac

# --- Render the manifest into a tempfile -----------------------------------
TMP_MANIFEST="$(mktemp)"
trap 'rm -f "$TMP_MANIFEST"' EXIT
esc(){ sed -e 's/[\/&]/\\&/g' <<<"$1"; }
sed -e "s/__IMAGE__/$(esc "$IMAGE")/g" \
    -e "s/__NAMESPACE__/$(esc "$NS")/g" \
    -e "s/__STORAGE_CLASS__/$(esc "$SC")/g" \
    -e "s/__SERVICE_TYPE__/$(esc "$SVC_TYPE")/g" \
    "$MANIFEST" > "$TMP_MANIFEST"

say "About to apply ProxyCTL (namespace: ${NS}, image: ${IMAGE}, expose: ${EXPOSE})"
confirm_yes "Proceed?" || { err "aborted"; exit 1; }

kubectl apply -f "$TMP_MANIFEST"

# --- Optional Ingress (rendered inline so the manifest stays one file) -----
if [ "$EXPOSE" = "ingress" ]; then
  cat <<EOF | kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: proxyctl
  namespace: ${NS}
spec:
  ingressClassName: ${INGRESS_CLASS}
  rules:
    - host: ${HOST}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: proxyctl
                port: { number: 80 }
EOF
fi

say "Waiting for rollout"
kubectl -n "$NS" rollout status deploy/proxyctl --timeout=120s || \
  warn "Rollout not complete yet — check: kubectl -n $NS get pods"

# --- Resolve the actual UI URL ----------------------------------------------
UI_URL=""; LB_PENDING=0
case "$EXPOSE" in
  loadbalancer)
    say "Waiting for an external IP (up to 30s)"
    for _ in $(seq 1 15); do
      ip="$(kubectl -n "$NS" get svc proxyctl -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
      [ -n "$ip" ] && { UI_URL="http://${ip}/"; break; }
      sleep 2
    done
    [ -z "$UI_URL" ] && LB_PENDING=1
    ;;
  nodeport)
    np="$(kubectl -n "$NS" get svc proxyctl -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)"
    nodeip="$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)"
    [ -n "$np" ] && [ -n "$nodeip" ] && UI_URL="http://${nodeip}:${np}/"
    ;;
esac

# --- Next steps: hand off to first-run setup --------------------------------
# Scrape the one-time bootstrap token straight out of the pod log so the
# operator never sees raw JSON. Both apps now emit the same slog-shaped
# `"msg":"BOOTSTRAP TOKEN","token":"…"` line, so the extraction is the
# identical pattern used by GameCTL's installer. Retry briefly — the
# line is emitted at startup, just after rollout completes.
echo
ok "ProxyCTL deployed."
echo
TOKEN=""
for _ in $(seq 1 10); do
  TOKEN="$(kubectl -n "$NS" logs deploy/proxyctl 2>/dev/null \
            | grep -i 'BOOTSTRAP TOKEN' \
            | grep -oE '"token":"[A-Za-z0-9]+"' | tail -1 | cut -d'"' -f4)"
  [ -n "$TOKEN" ] && break
  sleep 2
done

bar="==============================================================="
echo "${c_blue}${bar}${c_rst}"
echo "${c_blue}  Finish setup — ProxyCTL writes its own proxyctl-auth Secret${c_rst}"
echo "${c_blue}  (in the ${NS} namespace). No manual kubectl secrets needed.${c_rst}"
echo "${c_blue}${bar}${c_rst}"
echo
if [ -n "$TOKEN" ]; then
  echo "  ${c_grn}Your Bootstrap Token is:${c_rst}  ${c_yel}${TOKEN}${c_rst}"
else
  echo "  ${c_yel}No bootstrap token in the log${c_rst} — setup is likely already"
  echo "  completed for this instance. Just log in with your admin account."
  echo "  (Fresh install? Re-check: kubectl -n ${NS} logs deploy/proxyctl | grep -i 'BOOTSTRAP TOKEN')"
  echo "  (Recover: kubectl -n ${NS} delete secret proxyctl-auth"
  echo "           then kubectl -n ${NS} rollout restart deploy/proxyctl)"
fi
echo
echo "  1) Open the UI:"
case "$EXPOSE" in
  ingress)
    echo "       ${c_grn}http://${HOST}/${TOKEN:+?token=${TOKEN}}${c_rst}   (point DNS/hosts for ${HOST} at your ingress controller)"
    ;;
  loadbalancer)
    if [ "$LB_PENDING" -eq 1 ]; then
      warn "No external IP assigned — every pool address is likely in use."
      echo "       • free an IP / widen a pool, then: kubectl -n ${NS} get svc proxyctl -w"
      echo "       • or re-run install.sh and choose Ingress or NodePort"
      echo "       • immediate access: kubectl -n ${NS} port-forward svc/proxyctl 8080:80  → http://127.0.0.1:8080/${TOKEN:+?token=${TOKEN}}"
    else
      [ -n "$UI_URL" ] && echo "       ${c_grn}${UI_URL%/}/${TOKEN:+?token=${TOKEN}}${c_rst}"
      echo "       Always works: kubectl -n ${NS} port-forward svc/proxyctl 8080:80  → http://127.0.0.1:8080/${TOKEN:+?token=${TOKEN}}"
    fi
    ;;
  nodeport)
    [ -n "$UI_URL" ] && echo "       ${c_grn}${UI_URL%/}/${TOKEN:+?token=${TOKEN}}${c_rst}"
    echo "       Always works: kubectl -n ${NS} port-forward svc/proxyctl 8080:80  → http://127.0.0.1:8080/${TOKEN:+?token=${TOKEN}}"
    ;;
  portforward|*)
    echo "       Run: kubectl -n ${NS} port-forward svc/proxyctl 8080:80"
    echo "       Then: ${c_grn}http://localhost:8080/${TOKEN:+?token=${TOKEN}}${c_rst}"
    ;;
esac
echo "  2) Enter the token above + choose your admin username/password. Done."
echo "${c_blue}${bar}${c_rst}"
echo
echo "Once admin is claimed, droplet + Cloudflare setup are driven from the"
echo "in-app wizards — this script's job is done."
