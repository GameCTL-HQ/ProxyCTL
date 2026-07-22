package main

import (
	"os/exec"
	"testing"
)

// requireWG skips the test on a machine without wireguard-tools installed
// (e.g. a bare dev box) rather than failing — the real target is the
// proxyctl container image, which the Dockerfile now installs it into.
func requireWG(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("wg"); err != nil {
		t.Skip("wg (wireguard-tools) not installed — skipping; the proxyctl image carries it")
	}
}

func TestControlWGStore_EnsureKeypairIsIdempotent(t *testing.T) {
	requireWG(t)
	s := NewControlWGStore(t.TempDir())
	pub1, err := s.EnsureKeypair()
	if err != nil {
		t.Fatalf("first EnsureKeypair: %v", err)
	}
	if pub1 == "" {
		t.Fatal("expected a non-empty public key")
	}
	pub2, err := s.EnsureKeypair()
	if err != nil {
		t.Fatalf("second EnsureKeypair: %v", err)
	}
	if pub1 != pub2 {
		t.Errorf("EnsureKeypair must not rotate an existing key: got %q then %q", pub1, pub2)
	}
}

func TestControlWGStore_PrivateKeyNeverEmptyOnceGenerated(t *testing.T) {
	requireWG(t)
	s := NewControlWGStore(t.TempDir())
	if _, err := s.EnsureKeypair(); err != nil {
		t.Fatal(err)
	}
	priv, err := s.PrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if priv == "" {
		t.Fatal("expected a non-empty private key after EnsureKeypair")
	}
}
