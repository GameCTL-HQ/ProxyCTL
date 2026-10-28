package main

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfileBuildsPatchedKubectlFromPinnedSource(t *testing.T) {
	b, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"ARG KUBECTL_VERSION=v1.35.9",
		"ARG KUBECTL_COMMIT=329000efd0b26bd78155e51e4df449130f0fca00",
		"ARG KUBECTL_SOURCE_SHA256=3020026c65ea6701c42113ca092bb20bff71099028f0cf596354e95d53f7e0a6",
		"golang.org/x/net@v0.60.0",
		"golang.org/x/text@v0.41.0",
		"COPY --from=kubectl /out/kubectl /usr/local/bin/kubectl",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile missing pinned patched kubectl build marker %q", want)
		}
	}
	if strings.Contains(s, "dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl") {
		t.Error("Dockerfile still installs the upstream kubectl binary with known high-severity dependencies")
	}
}
