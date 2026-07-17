#!/usr/bin/env bash
# Build the ProxyCTL binary. Two stages, mirroring GameCTL's layout:
#   1. Build the React+Vite UI bundle in kubeUI/ → server/web/dist/
#   2. Build the Go binary in server/, which //go:embed-s web/dist
#
# Override ARCH=arm64 if needed; set SKIP_UI=1 to reuse the existing bundle.
set -euo pipefail
cd "$(dirname "$0")"

ARCH="${ARCH:-amd64}"
OUT="dist/proxyctl"

if [ "${SKIP_UI:-0}" != "1" ]; then
  echo ">> building UI bundle (kubeUI → server/web/dist)…"
  ( cd kubeUI && [ -d node_modules ] || npm install --no-audit --no-fund )
  ( cd kubeUI && npm run build )
fi

if [ ! -f server/web/dist/index.html ]; then
  echo "!! server/web/dist/index.html missing — run without SKIP_UI=1 to build the UI" >&2
  exit 1
fi

mkdir -p dist
# Build from server/; output back up to the repo-root dist/ so the
# localstart.sh / localdev.sh / clusterdeploy paths are unchanged.
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go -C server build -trimpath -ldflags "-s -w" -o "../$OUT" .

echo "built: $OUT"
file "$OUT" 2>/dev/null || true
ls -lh "$OUT"
