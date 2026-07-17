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
# Like GameCTL it also: self-heals a fresh-k3s kubeconfig (copies the
# root-only /etc/rancher/k3s/k3s.yaml to ~/.kube/config + sets KUBECONFIG);
# offers to install MetalLB + a pool when you pick LoadBalancer on a cluster
# without one; lets you bind a specific MetalLB IP; and can additionally put
# a hostname Ingress in front of a LoadBalancer/NodePort install. These are
# opt-in prompts, all declined automatically under PROXYCTL_ASSUME_YES=1.
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
#   PROXYCTL_MANIFEST         path or URL to the manifest  (default: bundled/raw)
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
# Default to the published public image. In the private repo this is the
# `OWNER` placeholder; scripts/sync-public.sh rewrites it to the real GHCR
# image (ghcr.io/gamectl-hq/proxyctl:latest) in the public snapshot, so a
# fresh `curl … | bash` Just Works. Override with `PROXYCTL_IMAGE=…` for a
# private/homelab registry (the prompt below also lets you change it).
IMAGE="${PROXYCTL_IMAGE:-ghcr.io/gamectl-hq/proxyctl:latest}"
HOST="${PROXYCTL_HOST:-}"
INGRESS_CLASS="${PROXYCTL_INGRESS_CLASS:-}"
EXPOSE="${PROXYCTL_EXPOSE:-auto}"
ASSUME_YES="${PROXYCTL_ASSUME_YES:-0}"

# Where to fetch the manifest from when not run inside a checkout. The OWNER
# placeholder is rewritten to the public slug by scripts/sync-public.sh, so a
# `curl … | bash` install pulls the manifest straight from the public repo.
RAW_URL_DEFAULT="https://raw.githubusercontent.com/GameCTL-HQ/ProxyCTL/main/k8s/proxyctl.yaml"
MANIFEST="${PROXYCTL_MANIFEST:-}"
# BASH_SOURCE[0] is unset when the script is run via `curl … | bash` (there is
# no file on disk). Guard it under `set -u`; leave SCRIPT_DIR empty in that
# case so the manifest lookup below falls through to the URL instead of
# resolving a bogus path like /home/k8s/proxyctl.yaml.
_src="${BASH_SOURCE[0]:-}"
if [ -n "$_src" ] && [ -f "$_src" ]; then
  SCRIPT_DIR="$(cd "$(dirname "$_src")" && pwd)"
else
  SCRIPT_DIR=""
fi

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
confirm_invasive(){
  # For irreversible / cluster-wide actions (installing MetalLB, copying the
  # k3s kubeconfig, creating an extra Ingress). Default is NO and it is
  # NEVER auto-confirmed — non-interactive runs always decline. Mirrors
  # GameCTL's confirm_invasive.
  [ "$ASSUME_YES" = "1" ] && return 1
  local ans; read -r -p "$1 [y/N]: " ans </dev/tty || true
  [[ "${ans:-}" =~ ^[Yy]$ ]]
}

# --- Preflight: kubectl + cluster reachable --------------------------------
command -v kubectl >/dev/null 2>&1 || die "kubectl not on PATH"
kubectl version --client --output=yaml >/dev/null 2>&1 || true
if ! kubectl cluster-info >/dev/null 2>&1; then
  # Two stacked k3s footguns on a fresh box:
  #   1) /etc/rancher/k3s/k3s.yaml is root:root mode 600 by default — a
  #      normal user gets "permission denied" reading it.
  #   2) `kubectl` on a k3s box is usually a symlink to the `k3s` binary
  #      (`k3s kubectl`), which **hardcodes** /etc/rancher/k3s/k3s.yaml
  #      unless $KUBECONFIG is set. So a plain ~/.kube/config alone is
  #      ignored — the wrapper still tries the root-only file.
  # Fix: copy the kubeconfig to ~/.kube/config (if not already), then
  # export $KUBECONFIG for this session AND persist it to ~/.bashrc so the
  # user doesn't hit this again on the next shell. Mirrors GameCTL.
  if [ -e /etc/rancher/k3s/k3s.yaml ] && [ "$(id -u)" -ne 0 ]; then
    if [ ! -e "$HOME/.kube/config" ]; then
      warn "Detected k3s on this host but /etc/rancher/k3s/k3s.yaml isn't"
      warn "readable as $USER — that's why kubectl can't reach the cluster."
      if confirm_invasive "Copy it to ~/.kube/config (chown $USER, mode 0600) and set KUBECONFIG?"; then
        mkdir -p "$HOME/.kube"
        sudo install -m 0600 -o "$USER" -g "$(id -gn)" /etc/rancher/k3s/k3s.yaml "$HOME/.kube/config" \
          || die "couldn't copy /etc/rancher/k3s/k3s.yaml — re-run with sudo or do it by hand:
       sudo install -m 0600 -o \$USER -g \$USER /etc/rancher/k3s/k3s.yaml ~/.kube/config
       export KUBECONFIG=\$HOME/.kube/config"
        ok "Wrote $HOME/.kube/config"
      else
        die "kubectl can't reach a cluster. The k3s fix is:
       sudo install -m 0600 -o \$USER -g \$USER /etc/rancher/k3s/k3s.yaml ~/.kube/config
       export KUBECONFIG=\$HOME/.kube/config
   (or pass --write-kubeconfig-mode=644 at k3s install time)"
      fi
    else
      ok "Found existing ~/.kube/config — using it"
    fi
    # The k3s-wrapped kubectl ignores ~/.kube/config unless $KUBECONFIG is
    # set, so set it for the rest of this run…
    export KUBECONFIG="$HOME/.kube/config"
    # …and persist for future shells if we can.
    if [ -f "$HOME/.bashrc" ] && [ -w "$HOME/.bashrc" ] \
       && ! grep -qE '^[[:space:]]*export[[:space:]]+KUBECONFIG=' "$HOME/.bashrc"; then
      {
        printf '\n# Added by ProxyCTL installer — the k3s-wrapped kubectl\n'
        printf '# hardcodes /etc/rancher/k3s/k3s.yaml unless KUBECONFIG is set.\n'
        printf 'export KUBECONFIG="$HOME/.kube/config"\n'
      } >> "$HOME/.bashrc"
      ok "Added 'export KUBECONFIG=…' to ~/.bashrc for future shells"
    fi
    if ! kubectl cluster-info >/dev/null 2>&1; then
      die "still can't reach the cluster after setting KUBECONFIG=$KUBECONFIG — check 'kubectl config current-context'"
    fi
  else
    die "kubectl can't reach a cluster. Check your kubeconfig / current-context:
       kubectl config current-context"
  fi
fi
ok "Cluster reachable: $(kubectl config current-context 2>/dev/null || echo '?')"

# --- Image -----------------------------------------------------------------
# Non-interactive: use the published image automatically. Override without a
# prompt by setting PROXYCTL_IMAGE=… before running (e.g. for a private
# registry). Default is the public GHCR image (set above / rewritten by
# sync-public for the public installer).
#
# Pin a moving :latest default to the newest immutable release tag. This is
# what stops unprompted version jumps: with imagePullPolicy: Always, a pod that
# restarts on a :latest tag re-pulls whatever :latest now points at — so a node
# drain or eviction during setup silently updates ProxyCTL. Pinning a fixed tag
# means restarts re-pull the SAME version; only the in-app "Update" button (which
# sets a new tag) moves it. A user-supplied PROXYCTL_IMAGE is respected as-is.
case "$IMAGE" in
  *:latest)
    _repo="${IMAGE%:latest}"
    # GameCTL-HQ/ProxyCTL is the release repo (matches updateRepo in server/update.go).
    _tag="$(curl -fsSL --max-time 10 \
      https://api.github.com/repos/GameCTL-HQ/ProxyCTL/releases/latest 2>/dev/null \
      | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
    if [ -n "$_tag" ]; then
      IMAGE="${_repo}:${_tag}"
      ok "Pinned image to released tag: ${IMAGE}"
    else
      warn "Couldn't resolve the latest release tag — staying on ${IMAGE}."
      warn "A pod restart on a :latest tag can change versions unprompted;"
      warn "set PROXYCTL_IMAGE=<repo>:<version> to pin explicitly."
    fi
    ;;
esac
say "Image: ${IMAGE}  (override with PROXYCTL_IMAGE=…)"

# --- Storage: where the WHOLE install lives ---------------------------------
# One question decides it: name an NFS share (server:/path) and ProxyCTL's
# app data (/data -> subPath app/) AND the per-gateway WireGuard keys
# (Keys/) all live in that one directory — simple to find, simple to back
# up, and it survives a reinstall (static PV, reclaimPolicy Retain).
# Press Enter to keep the classic behavior instead: a PVC on the
# StorageClass ($SC), keys handled from the in-app Storage step.
#
# Advisory, never enforced: keys are private key material — ideally use a
# directory only the cluster nodes can mount (export it to the node IPs;
# NFS mounts come from the nodes, not pods). An open share works too.
DATA_NFS="${PROXYCTL_DATA_NFS:-}"
DATA_NFS_SERVER=""; DATA_NFS_EXPORT=""; DATA_PVC="proxyctl-data"; DATA_SUBPATH=""
if [ -z "$DATA_NFS" ] && [ "$ASSUME_YES" != "1" ]; then
  echo
  echo "Where should ProxyCTL keep its data (app state + WireGuard keys)?"
  echo "  Give an NFS share as server:/path (e.g. 10.0.0.5:/mnt/ssd/ProxyCTL)"
  echo "  and everything lives in that one directory (recommended: a dir only"
  echo "  your cluster nodes can mount). Enter = use StorageClass '$SC'."
  DATA_NFS="$(ask "NFS share for ProxyCTL (server:/path, Enter = StorageClass)" "")"
fi
if [ -n "$DATA_NFS" ]; then
  case "$DATA_NFS" in
    *:/*) DATA_NFS_SERVER="${DATA_NFS%%:*}"; DATA_NFS_EXPORT="${DATA_NFS#*:}" ;;
    *) die "NFS share must look like server:/path (got: $DATA_NFS)" ;;
  esac
  case "$DATA_NFS_SERVER" in *[/:@" "]*|"") die "bad NFS server '$DATA_NFS_SERVER' — bare hostname or IP" ;; esac
  case "$DATA_NFS_EXPORT" in
    *..*) die "NFS path must not contain .." ;;
    *" "*) die "NFS path must not contain spaces" ;;
  esac
  DATA_NFS_EXPORT="${DATA_NFS_EXPORT%/}"
  # Same name the app derives for this share (server/render.go
  # keysShareVolName): sha256("server:export"), first 8 hex chars. Keeping
  # them identical means the app finds this PV/PVC already in place and
  # never needs cluster-scoped persistentvolumes RBAC.
  if command -v sha256sum >/dev/null 2>&1; then
    _hash="$(printf '%s' "${DATA_NFS_SERVER}:${DATA_NFS_EXPORT}" | sha256sum | cut -c1-8)"
  else
    _hash="$(printf '%s' "${DATA_NFS_SERVER}:${DATA_NFS_EXPORT}" | shasum -a 256 | cut -c1-8)"
  fi
  DATA_PVC="proxyctl-keys-${_hash}"
  DATA_SUBPATH="app"
  say "Install storage: NFS ${DATA_NFS_SERVER}:${DATA_NFS_EXPORT}  (volume ${DATA_PVC})"
else
  say "Install storage: StorageClass ${SC} (PVC proxyctl-data)"
fi

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

# --- Optional: install MetalLB (explicit opt-in only) ----------------------
# Only when LoadBalancer is the chosen exposure and MetalLB is absent.
# Installing it is cluster-wide infra and needs an unused IP range only you
# know — strictly opt-in, never silent. Mirrors GameCTL's MetalLB step (but
# gated on the LB choice, since ProxyCTL has no game servers that need it).
if [ "$EXPOSE" = "loadbalancer" ] && [ "$HAS_METALLB" -eq 0 ]; then
  echo
  warn "LoadBalancer was chosen but no MetalLB / cloud LB is present. On a"
  warn "bare-metal cluster the Service will stay <pending> without one."
  if confirm_invasive "Install MetalLB now and create a dedicated 'proxyctl' pool?"; then
    echo "MetalLB hands out real IPs on your LAN. Enter a range that is NOT"
    echo "used by DHCP, other pools, or real hosts (e.g. 10.0.0.240-10.0.0.250)."
    RANGE="$(ask "Unused IP range for the 'proxyctl' MetalLB pool" "")"
    [ -n "$RANGE" ] || die "no IP range provided; aborting MetalLB install"
    say "Installing MetalLB (native manifest)"
    kubectl apply -f https://raw.githubusercontent.com/metallb/metallb/v0.14.8/config/manifests/metallb-native.yaml
    say "Waiting for MetalLB controller to be ready"
    kubectl -n metallb-system rollout status deploy/controller --timeout=120s
    say "Creating IPAddressPool 'proxyctl' + L2Advertisement"
    kubectl apply -f - <<EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: proxyctl
  namespace: metallb-system
spec:
  addresses:
    - ${RANGE}
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: proxyctl
  namespace: metallb-system
spec:
  ipAddressPools:
    - proxyctl
EOF
    HAS_METALLB=1
    ok "MetalLB installed with pool 'proxyctl' (${RANGE})"
  else
    warn "Skipping MetalLB. The LoadBalancer Service will stay <pending> until"
    warn "you install MetalLB (or re-run and pick Ingress / NodePort / Port-forward)."
  fi
fi

# --- Optionally ALSO put an Ingress (hostname) in front --------------------
# When the primary exposure is LB/NodePort and an ingress controller exists,
# offer to additionally create a hostname Ingress (mirrors GameCTL). Not
# offered for port-forward, where an Ingress would contradict the choice.
ALSO_INGRESS=0
if { [ "$EXPOSE" = "loadbalancer" ] || [ "$EXPOSE" = "nodeport" ]; } \
   && [ "$HAS_INGRESS" -eq 1 ]; then
  confirm_invasive "Also create an Ingress (hostname) in addition?" && ALSO_INGRESS=1
fi

# --- Ingress hostname (when Ingress is primary OR an add-on) ----------------
if [ "$EXPOSE" = "ingress" ] || [ "$ALSO_INGRESS" -eq 1 ]; then
  HOST="$(ask_required "Hostname the UI will be reached on (e.g. proxyctl.example.com)" "$HOST" PROXYCTL_HOST)"
  if [ -z "$INGRESS_CLASS" ]; then
    INGRESS_CLASS="$(printf '%s\n' "$INGRESS_CLASSES" | awk '{print $1}')"
    [ -z "$INGRESS_CLASS" ] && INGRESS_CLASS="traefik"
  fi
fi

# --- MetalLB IP helpers -----------------------------------------------------
# Mirrors GameCTL's installer: when the UI is exposed as a LoadBalancer and
# MetalLB is present, let the operator pick a pool and BIND A SPECIFIC IP
# (or press Enter to let MetalLB auto-assign). The choice is applied as a
# Service annotation after the manifest lands (see post-apply block below).
_ip2int() { local a b c d; IFS=. read -r a b c d <<<"$1"; echo $(( (a<<24)+(b<<16)+(c<<8)+d )); }
_int2ip() { local n=$1; echo "$(((n>>24)&255)).$(((n>>16)&255)).$(((n>>8)&255)).$((n&255))"; }

# expand_addr: print every IPv4 in a MetalLB address token (a-b range,
# single IP, or CIDR). Bounded to 4096 addrs so a wide CIDR can't hang.
expand_addr() {
  local tok="$1" s e i base pre bi mask net bc
  if [[ "$tok" == */* ]]; then
    base="${tok%/*}"; pre="${tok#*/}"
    [ "$pre" -ge 20 ] 2>/dev/null || { warn "pool entry ${tok}: CIDR too wide to list — auto-assign only"; return; }
    bi="$(_ip2int "$base")"; mask=$(( (0xffffffff << (32-pre)) & 0xffffffff ))
    net=$(( bi & mask )); bc=$(( net | (~mask & 0xffffffff) ))
    for ((i=net; i<=bc; i++)); do _int2ip "$i"; done
  elif [[ "$tok" == *-* ]]; then
    s="$(_ip2int "${tok%-*}")"; e="$(_ip2int "${tok#*-}")"
    [ $(( e - s )) -le 4096 ] || { warn "pool range ${tok} too large to list"; return; }
    for ((i=s; i<=e; i++)); do _int2ip "$i"; done
  elif [[ "$tok" =~ ^[0-9.]+$ ]]; then
    echo "$tok"
  fi
}

# For LoadBalancer + MetalLB: pick a pool (auto if single), list FREE IPs,
# let the user take one or press Enter to let MetalLB auto-assign from it.
LB_IP=""; LB_POOL=""
if [ "$EXPOSE" = "loadbalancer" ] && [ "$HAS_METALLB" -eq 1 ]; then
  pool_lines="$(kubectl get ipaddresspools.metallb.io -A \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.spec.addresses[*]}{"\n"}{end}' 2>/dev/null || true)"
  pool_names="$(printf '%s\n' "$pool_lines" | sed '/^$/d' | cut -d'|' -f1)"
  n_pools="$(printf '%s\n' "$pool_names" | sed '/^$/d' | wc -l | tr -d ' ')"

  if [ "${n_pools:-0}" -eq 0 ]; then
    warn "No MetalLB IPAddressPools found — the Service may stay <pending>."
    LB_IP="$(ask "Pin a specific IP for the UI (blank = let MetalLB decide)" "")"
  else
    if [ "$n_pools" -eq 1 ]; then
      LB_POOL="$(printf '%s\n' "$pool_names" | sed '/^$/d' | head -1)"
      say "Using the only MetalLB pool: ${LB_POOL}"
    else
      echo "MetalLB pools: $(printf '%s ' $pool_names)"
      LB_POOL="$(ask "Pool to use" "$(printf '%s\n' "$pool_names" | sed '/^$/d' | head -1)")"
    fi

    addrs="$(printf '%s\n' "$pool_lines" | awk -F'|' -v p="$LB_POOL" '$1==p{print $2}')"
    used="$(kubectl get svc -A -o jsonpath='{range .items[*]}{.status.loadBalancer.ingress[0].ip}{"\n"}{end}' 2>/dev/null | sed '/^$/d')"
    free=""
    for tok in $addrs; do
      while read -r ip; do
        [ -z "$ip" ] && continue
        printf '%s\n' "$used" | grep -qxF "$ip" || free="${free}${ip}\n"
      done < <(expand_addr "$tok")
    done
    free="$(printf '%b' "$free" | sed '/^$/d')"
    if [ -n "$free" ]; then
      echo "Free IPs in '${LB_POOL}': $(printf '%s\n' "$free" | head -10 | tr '\n' ' ')$([ "$(printf '%s\n' "$free" | wc -l)" -gt 10 ] && echo '…')"
    else
      warn "Pool '${LB_POOL}' has no free IPs — auto-assign will stay <pending> until one frees."
    fi
    LB_IP="$(ask "Specific IP from '${LB_POOL}' (Enter = let MetalLB auto-assign from it)" "")"
  fi
fi

# --- Map EXPOSE to Service type --------------------------------------------
case "$EXPOSE" in
  loadbalancer) SVC_TYPE="LoadBalancer" ;;
  nodeport)     SVC_TYPE="NodePort" ;;
  *)            SVC_TYPE="ClusterIP" ;;
esac

# --- Resolve + render the manifest into a tempfile -------------------------
# Source order: explicit $PROXYCTL_MANIFEST file → bundled file from a
# checkout → the published raw URL (the `curl … | bash` path, where there is
# no file on disk).
SRC_MANIFEST="$(mktemp)"
TMP_MANIFEST="$(mktemp)"
trap 'rm -f "$SRC_MANIFEST" "$TMP_MANIFEST"' EXIT
if [ -n "$MANIFEST" ] && [ -f "$MANIFEST" ]; then
  cp "$MANIFEST" "$SRC_MANIFEST"; say "Using manifest: $MANIFEST"
elif [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/../k8s/proxyctl.yaml" ]; then
  cp "$SCRIPT_DIR/../k8s/proxyctl.yaml" "$SRC_MANIFEST"
  say "Using bundled manifest from checkout"
else
  url="${MANIFEST:-$RAW_URL_DEFAULT}"
  command -v curl >/dev/null 2>&1 || die "curl needed to fetch the manifest"
  say "Fetching manifest: $url"
  curl -fsSL "$url" -o "$SRC_MANIFEST" || die "failed to download manifest"
fi

esc(){ sed -e 's/[\/&]/\\&/g' <<<"$1"; }
# In NFS-share mode the proxyctl-data PVC document is stripped — /data rides
# the share's static PV/PVC (created below) at subPath app/ instead.
STRIP_PVC="cat"
[ -n "$DATA_NFS_SERVER" ] && STRIP_PVC="sed /__APP_PVC_START__/,/__APP_PVC_END__/d"
$STRIP_PVC "$SRC_MANIFEST" | sed \
    -e "s/__IMAGE__/$(esc "$IMAGE")/g" \
    -e "s/__NAMESPACE__/$(esc "$NS")/g" \
    -e "s/__STORAGE_CLASS__/$(esc "$SC")/g" \
    -e "s/__SERVICE_TYPE__/$(esc "$SVC_TYPE")/g" \
    -e "s/__DATA_PVC__/$(esc "$DATA_PVC")/g" \
    -e "s/__DATA_SUBPATH__/$(esc "$DATA_SUBPATH")/g" \
    -e "s/__DATA_NFS_SERVER__/$(esc "$DATA_NFS_SERVER")/g" \
    -e "s/__DATA_NFS_EXPORT__/$(esc "$DATA_NFS_EXPORT")/g" \
    > "$TMP_MANIFEST"

say "About to apply ProxyCTL (namespace: ${NS}, image: ${IMAGE}, expose: ${EXPOSE}$( [ "$ALSO_INGRESS" -eq 1 ] && echo '+ingress' ))"
confirm_yes "Proceed?" || { err "aborted"; exit 1; }

kubectl apply -f "$TMP_MANIFEST"

# NFS-share install: create the share's static PV + RWX PVC (field-for-field
# what the app itself would render — server/render.go renderKeysSharePV — so
# later in-app applies are no-ops). Retain: keys + app data must survive the
# claim. Applied after the manifest so the namespace exists.
if [ -n "$DATA_NFS_SERVER" ]; then
  say "Creating static PV + PVC ${DATA_PVC} on ${DATA_NFS_SERVER}:${DATA_NFS_EXPORT}"
  cat <<PVEOF | kubectl apply -f -
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${DATA_PVC}
  labels: { proxyctl: keys }
spec:
  capacity: { storage: 1Gi }
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  nfs: { server: ${DATA_NFS_SERVER}, path: ${DATA_NFS_EXPORT} }
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${DATA_PVC}
  namespace: ${NS}
  labels: { proxyctl: keys }
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: ${DATA_PVC}
  resources: { requests: { storage: 1Gi } }
PVEOF
  # Advisory only: show the node IPs so the operator can restrict the export.
  _node_ips="$(kubectl get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{" "}{end}' 2>/dev/null || true)"
  [ -n "$_node_ips" ] && say "Tip: the share only needs to admit the node IPs: ${_node_ips}"
fi

# --- Bind the UI Service to the chosen MetalLB IP / pool --------------------
# Same mechanism GameCTL uses: a specific IP wins via loadBalancerIPs; else
# pin the pool so MetalLB auto-assigns from it. No-op when neither was set
# (plain auto-assign / non-MetalLB LoadBalancer).
if [ "$EXPOSE" = "loadbalancer" ]; then
  if [ -n "$LB_IP" ]; then
    say "Requesting specific MetalLB IP ${LB_IP} for the UI Service"
    kubectl -n "$NS" annotate svc proxyctl "metallb.universe.tf/loadBalancerIPs=${LB_IP}" --overwrite >/dev/null
  elif [ -n "$LB_POOL" ]; then
    say "Binding the UI Service to MetalLB pool '${LB_POOL}' (auto-assign)"
    kubectl -n "$NS" annotate svc proxyctl "metallb.universe.tf/address-pool=${LB_POOL}" --overwrite >/dev/null
  fi
fi

# --- Optional Ingress (rendered inline so the manifest stays one file) -----
# Created when Ingress is the primary exposure OR an explicit add-on.
if [ "$EXPOSE" = "ingress" ] || [ "$ALSO_INGRESS" -eq 1 ]; then
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
if [ "$ALSO_INGRESS" -eq 1 ]; then
  echo "       Also via Ingress: ${c_grn}http://${HOST}/${TOKEN:+?token=${TOKEN}}${c_rst} (needs DNS → ingress controller)"
fi
echo "  2) Enter the token above + choose your admin username/password. Done."
echo "${c_blue}${bar}${c_rst}"
echo
echo "Once admin is claimed, droplet + Cloudflare setup are driven from the"
echo "in-app wizards — this script's job is done."
