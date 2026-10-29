# ProxyCTL container image.
#
# Three stages, matching GameCTL's pipeline:
#   1. Node — build the React+Vite UI bundle (kubeUI/ → server/web/dist)
#   2. Go   — compile the binary in server/, //go:embed-ing web/dist
#   3. Runtime — small Alpine carrying ssh / ssh-keygen / kubectl / wg
#      (wg genkey+pubkey only — this container never brings a WireGuard
#      interface up itself; that's the control-wg sidecar's job, see
#      k8s/proxyctl.yaml and server/controlwg.go)
#
# Built with:   docker build -t proxyctl:dev .
# Or:           docker build --build-arg VERSION=$(git describe --tags --always) ...

# --- Stage 1: build the React UI bundle ---
FROM node:22-alpine@sha256:fe4f6d74e20370acb04ad889077d14d0f64a9c8ea6a42bc60cddf1e844699914 AS ui
WORKDIR /src/kubeUI
COPY kubeUI/package.json kubeUI/package-lock.json* ./
RUN npm ci --no-audit --no-fund
COPY kubeUI/ ./
RUN npm run build
# vite.config.js writes to ../server/web/dist — i.e. /src/server/web/dist.

# --- Stage 2: build the Go binary with the embedded UI ---
FROM golang:1.26.10-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS go
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

# --- Stage 3: build a version-skew-compatible kubectl with patched Go deps ---
# Kubernetes v1.35 is within kubectl's supported +/-1 minor skew for the v1.34
# clusters ProxyCTL manages. The upstream v1.35.9 binary was built before
# several Go fixes landed, so build the exact release source with fixed modules.
FROM golang:1.26.10-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS kubectl
ARG KUBECTL_VERSION=v1.35.9
ARG KUBECTL_COMMIT=329000efd0b26bd78155e51e4df449130f0fca00
ARG KUBECTL_SOURCE_SHA256=3020026c65ea6701c42113ca092bb20bff71099028f0cf596354e95d53f7e0a6
ENV GOTOOLCHAIN=local
RUN apk add --no-cache ca-certificates curl
WORKDIR /src/kubernetes
RUN curl -fsSL -o /tmp/kubernetes.tar.gz \
        "https://codeload.github.com/kubernetes/kubernetes/tar.gz/${KUBECTL_COMMIT}" \
    && echo "${KUBECTL_SOURCE_SHA256}  /tmp/kubernetes.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/kubernetes.tar.gz --strip-components=1 \
    && rm -f /tmp/kubernetes.tar.gz
RUN GOWORK=off go mod edit \
        -require=golang.org/x/net@v0.60.0 \
        -require=golang.org/x/text@v0.41.0 \
    && GOWORK=off CGO_ENABLED=0 go build -mod=mod -trimpath \
        -ldflags="-s -w \
          -X k8s.io/client-go/pkg/version.gitVersion=${KUBECTL_VERSION} \
          -X k8s.io/client-go/pkg/version.gitCommit=${KUBECTL_COMMIT} \
          -X k8s.io/client-go/pkg/version.gitTreeState=clean \
          -X k8s.io/component-base/version.gitVersion=${KUBECTL_VERSION} \
          -X k8s.io/component-base/version.gitCommit=${KUBECTL_COMMIT} \
          -X k8s.io/component-base/version.gitTreeState=clean" \
        -o /out/kubectl ./cmd/kubectl

# --- Stage 4: minimal runtime ---
FROM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f
RUN apk upgrade --no-cache \
    && apk add --no-cache \
        ca-certificates openssh-client iptables bash curl tini wireguard-tools \
    && adduser -D -u 1000 proxyctl \
    && mkdir -p /data \
    && chown proxyctl:proxyctl /data
COPY --from=kubectl /out/kubectl /usr/local/bin/kubectl
COPY --from=go /out/proxyctl /usr/local/bin/proxyctl
USER proxyctl
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/proxyctl"]
CMD ["-addr", "0.0.0.0:8080", "-db", "/data/entries.json", "-apply-mode", "ssh"]
