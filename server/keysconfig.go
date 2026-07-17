package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
)

// defaultKeysBasePath is where per-gateway WireGuard keypairs land on the NFS
// SSD share by default. It matches the pathPattern of the bundled
// nfs-ssd-proxyctl-keys StorageClass (k8s/storageclass-proxyctl-keys.yaml).
const defaultKeysBasePath = "ProxyCTL/Keys"

// KeysStore persists the operator's chosen base folder for gateway key PVCs.
// The folder is a relative path under the NFS SSD export root (e.g.
// "ProxyCTL/Keys"); ProxyCTL derives a StorageClass whose nfs-subdir
// pathPattern nests the per-gateway dirs under it. Persisted as a small JSON
// file next to entries.json, mirroring DomainStore / the droplet config.
type KeysStore struct {
	path string
	mu   sync.RWMutex
	base string
}

func NewKeysStore(p string) (*KeysStore, error) {
	s := &KeysStore{path: p, base: defaultKeysBasePath}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var saved struct {
		BasePath string `json:"basePath"`
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	if v := normalizeKeysBasePath(saved.BasePath); v != "" {
		s.base = v
	}
	return s, nil
}

// BasePath returns the configured keys base folder (never empty).
func (s *KeysStore) BasePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.base == "" {
		return defaultKeysBasePath
	}
	return s.base
}

// Set validates and persists a new base folder.
func (s *KeysStore) Set(p string) error {
	v := normalizeKeysBasePath(p)
	if err := validateKeysBasePath(v); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.base = v
	b, err := json.MarshalIndent(struct {
		BasePath string `json:"basePath"`
	}{v}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// normalizeKeysBasePath trims slashes/space and cleans the path so
// "/ProxyCTL/Keys/" and "ProxyCTL/Keys" both become "ProxyCTL/Keys".
func normalizeKeysBasePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

// validateKeysBasePath keeps the folder a safe relative path: at least one
// segment, no traversal, each segment limited to letters/digits/. _ - (it is
// interpolated into a StorageClass pathPattern + an NFS dir name).
func validateKeysBasePath(p string) error {
	if p == "" {
		return fmt.Errorf("keys folder is required")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("keys folder must be a relative path under the share (no leading /)")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("invalid keys folder %q", p)
		}
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '.' || r == '_' || r == '-':
			default:
				return fmt.Errorf("keys folder segment %q may only contain letters, digits, . _ -", seg)
			}
		}
	}
	return nil
}
