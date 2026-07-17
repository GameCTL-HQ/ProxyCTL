# ProxyCTL container image.
#
# Three stages, matching GameCTL's pipeline:
#   1. Node — build the React+Vite UI bundle (kubeUI/ → server/web/dist)
#   2. Go   — compile the binary in server/, //go:embed-ing web/dist
#   3. Runtime — small Alpine carrying ssh / ssh-keygen / kubectl
#
# Built with:   docker build -t proxyctl:dev .
# Or:           docker build --build-arg VERSION=$(git describe --tags --always) ...

# --- Stage 1: build the React UI bundle ---
FROM node:22-alpine AS ui
WORKDIR /src/kubeUI
COPY kubeUI/package.json kubeUI/package-lock.json* ./
RUN npm ci --no-audit --no-fund || npm install --no-audit --no-fund
COPY kubeUI/ ./
RUN npm run build
# vite.config.js writes to ../server/web/dist — i.e. /src/server/web/dist.

# --- Stage 2: build the Go binary with the embedded UI ---
FROM golang:1.25-alpine AS go
WORKDIR /src/server
COPY server/go.mod server/go.sum* ./
RUN go mod download
COPY server/ ./
# Drop in the UI bundle the Node stage just built (//go:embed web/dist).
COPY --from=ui /src/server/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -trimpath -o /out/proxyctl ./

# --- Stage 2: minimal runtime ---
FROM alpine:3.20
ARG KUBECTL_VERSION=v1.30.5
RUN apk add --no-cache \
        ca-certificates openssh-client iptables bash curl tini \
    && curl -fsSL -o /usr/local/bin/kubectl \
        "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl \
    && adduser -D -u 1000 proxyctl \
    && mkdir -p /data \
    && chown proxyctl:proxyctl /data
COPY --from=go /out/proxyctl /usr/local/bin/proxyctl
USER proxyctl
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/proxyctl"]
CMD ["-addr", "0.0.0.0:8080", "-db", "/data/entries.json", "-apply-mode", "ssh"]
