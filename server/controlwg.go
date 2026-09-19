package main

// controlwg.go — ProxyCTL's OWN WireGuard identity for its dedicated
// control tunnel to the droplet.
//
// Background: every per-game gateway tunnel dials OUT to the droplet's
// static public IP over WireGuard, so it re-handshakes on its own when the
// operator's home network gets a new IP — proven during the 2026-07-21
// incident, when all six game tunnels recovered on their own within
// minutes of a router restart. The ONE thing that did NOT recover was
// ProxyCTL's own SSH connection to the droplet: it rides over the raw
// public internet, and the operator's home IP was hardcoded into an sshd
// allow-list (see lockdownSSH/unlockSSH in main.go), which locked them out
// until they used DigitalOcean's web console to fix it by hand.
//
// The control tunnel gives ProxyCTL's own management traffic the exact
// same resilience the game tunnels already have: a WireGuard peer, self-
// generated here, that dials the droplet directly — never the operator's
// home IP. It is scoped to reach ONLY the droplet's own tunnel address
// (see RenderControlTunnelWG0), nothing else on 10.8.0.0/24.
//
// The private key never leaves this file (0600, next to droplet.key) —
// EnsureKeypair only ever returns the derived PUBLIC key, for embedding in
// the droplet's [Peer] section (RenderDropletWG0).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ControlWGStore owns ProxyCTL's private WireGuard key for the control
// tunnel. Generation shells out to `wg genkey`/`wg pubkey` (wireguard-tools,
// bundled in the ProxyCTL image) — pure userspace Curve25519, no kernel
// module and no NET_ADMIN needed just to generate a keypair; that's only
// required later, to actually bring the interface up, which is the
// control-wg SIDECAR container's job (k8s/proxyctl.yaml), not this
// process's.
type ControlWGStore struct {
	mu      sync.Mutex
	keyPath string // controlwg.key — raw wg private key, 0600
}

func NewControlWGStore(dir string) *ControlWGStore {
	return &ControlWGStore{keyPath: filepath.Join(dir, "controlwg.key")}
}

// EnsureKeypair returns the control tunnel's public key, generating a
// private key on first call. Idempotent — a private key already on disk
// is reused as-is (regenerating would orphan the peer entry the operator
// already installed on the droplet).
func (s *ControlWGStore) EnsureKeypair() (pubkey string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, statErr := os.Stat(s.keyPath); statErr != nil {
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		if err := s.genKeyLocked(); err != nil {
			return "", err
		}
	}
	return s.pubkeyLocked()
}

// genKeyLocked writes a fresh private key via a sibling tmp path so a
// half-written key never becomes visible at the canonical path on a crash
// mid-generation — same atomic-replace shape as fileDropletStore.genKeypair.
func (s *ControlWGStore) genKeyLocked() error {
	out, err := exec.Command("wg", "genkey").Output()
	if err != nil {
		return fmt.Errorf("wg genkey: %w", err)
	}
	tmp := s.keyPath + ".new"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.keyPath)
}

func (s *ControlWGStore) pubkeyLocked() (string, error) {
	priv, err := os.ReadFile(s.keyPath)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("wg", "pubkey")
	cmd.Stdin = strings.NewReader(string(priv))
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("wg pubkey: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// PrivateKey returns the raw private key text, for rendering into the
// wg0.conf handed to the control-wg sidecar (RenderControlTunnelWG0). It
// never leaves this process — main.go writes the rendered conf straight
// to a shared emptyDir file it and the sidecar mount, never to the data
// PVC and never returned by any API response.
func (s *ControlWGStore) PrivateKey() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.keyPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
