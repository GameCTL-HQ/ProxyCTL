package main

import "testing"

// splitImageRepoTag must keep the registry/repo intact so updateApply swaps
// ONLY the tag. The traps: a registry host:port colon that must not be mistaken
// for the tag separator, and an @sha256 digest that must be dropped.
func TestSplitImageRepoTag(t *testing.T) {
	cases := []struct {
		image    string
		wantRepo string
		wantTag  string
	}{
		{"ghcr.io/gamectl-hq/proxyctl:v0.4.1", "ghcr.io/gamectl-hq/proxyctl", "v0.4.1"},
		{"ghcr.io/gamectl-hq/proxyctl:latest", "ghcr.io/gamectl-hq/proxyctl", "latest"},
		// Registry host:port — the port colon precedes the last '/', so it is
		// NOT the tag separator.
		{"registry.example.com:5000/proxyctl:7258dc4", "registry.example.com:5000/proxyctl", "7258dc4"},
		{"registry.example.com:5000/proxyctl", "registry.example.com:5000/proxyctl", ""},
		// No tag at all.
		{"ghcr.io/gamectl-hq/proxyctl", "ghcr.io/gamectl-hq/proxyctl", ""},
		// Digest is stripped; a tag+digest keeps the tag.
		{"ghcr.io/gamectl-hq/proxyctl@sha256:abc123", "ghcr.io/gamectl-hq/proxyctl", ""},
		{"ghcr.io/gamectl-hq/proxyctl:v0.4.1@sha256:abc123", "ghcr.io/gamectl-hq/proxyctl", "v0.4.1"},
	}
	for _, c := range cases {
		repo, tag := splitImageRepoTag(c.image)
		if repo != c.wantRepo || tag != c.wantTag {
			t.Errorf("splitImageRepoTag(%q) = (%q,%q), want (%q,%q)",
				c.image, repo, tag, c.wantRepo, c.wantTag)
		}
	}
}
