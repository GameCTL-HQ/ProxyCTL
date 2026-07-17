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

// KeysStore persists where gateway keypairs live: a relative base folder
// (e.g. "ProxyCTL/Keys") plus, optionally, an explicit NFS share to put it on.
//
// Two modes, because they are provisioned in genuinely different ways:
//
//   - Provisioner mode (share unset, the default): keys ride the install's own
//     StorageClass. ProxyCTL derives a class whose nfs-subdir pathPattern nests
//     the per-gateway dirs under the base folder. Zero config, but it can only
//     ever use the ONE export that provisioner mounts.
//
//   - Share mode (server + export set): the operator names any NFS share.
//     A provisioner can't be pointed at a different export, so this bypasses it
//     with a static PV on that share — see renderKeysSharePV. This is what lets
//     an operator put keys on a share that has no provisioner at all.
//
// Persisted as a small JSON file next to entries.json, mirroring DomainStore /
// the droplet config.
type KeysStore struct {
	path      string
	mu        sync.RWMutex
	base      string
	nfsServer string // "" = provisioner mode
	nfsExport string // absolute export path, e.g. /mnt/ssd
}

type keysSaved struct {
	BasePath  string `json:"basePath"`
	NFSServer string `json:"nfsServer,omitempty"`
	NFSExport string `json:"nfsExport,omitempty"`
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
	var saved keysSaved
	if err := json.Unmarshal(b, &saved); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	if v := normalizeKeysBasePath(saved.BasePath); v != "" {
		s.base = v
	}
	// Only honour a share when BOTH halves are present and valid — half a
	// share would render a PV that can never mount.
	if validateNFSShare(saved.NFSServer, saved.NFSExport) == nil {
		s.nfsServer = strings.TrimSpace(saved.NFSServer)
		s.nfsExport = normalizeNFSExport(saved.NFSExport)
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

// Share returns the operator-chosen NFS share, if any. ok=false means
// provisioner mode.
func (s *KeysStore) Share() (server, export string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.nfsServer == "" || s.nfsExport == "" {
		return "", "", false
	}
	return s.nfsServer, s.nfsExport, true
}

// Set validates and persists a new base folder, leaving the share as-is.
func (s *KeysStore) Set(p string) error {
	srv, exp, _ := s.Share()
	return s.SetAll(p, srv, exp)
}

// SetAll validates and persists the base folder plus the share. Empty
// server+export clears the share (back to provisioner mode).
func (s *KeysStore) SetAll(p, server, export string) error {
	v := normalizeKeysBasePath(p)
	if err := validateKeysBasePath(v); err != nil {
		return err
	}
	// Validate the raw input BEFORE normalizing — see validateNFSShare.
	server, export = strings.TrimSpace(server), strings.TrimSpace(export)
	if server != "" || export != "" {
		if err := validateNFSShare(server, export); err != nil {
			return err
		}
	}
	export = normalizeNFSExport(export)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.base, s.nfsServer, s.nfsExport = v, server, export
	b, err := json.MarshalIndent(keysSaved{v, server, export}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// normalizeNFSExport trims and cleans the export to an absolute path with no
// trailing slash: "/mnt/ssd/" -> "/mnt/ssd".
func normalizeNFSExport(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	if p == "/" {
		return p
	}
	return strings.TrimRight(p, "/")
}

// validateNFSShare keeps the share renderable into a PV: both halves present,
// the export absolute + traversal-free, and the server a bare host/IP (it is
// interpolated into the PV's nfs.server field).
//
// Takes the RAW export, deliberately: normalizeNFSExport runs path.Clean, which
// would resolve "/mnt/../etc" to "/etc" and export something the operator never
// typed. Reject that rather than silently rewrite it.
func validateNFSShare(server, export string) error {
	server = strings.TrimSpace(server)
	export = strings.TrimSpace(export)
	if server == "" || export == "" {
		return fmt.Errorf("both an NFS server and an export path are required (e.g. 10.0.0.5 and /mnt/ssd)")
	}
	if strings.ContainsAny(server, " \t\n/:@") {
		return fmt.Errorf("NFS server %q must be a bare hostname or IP — no scheme, port, or path", server)
	}
	if !strings.HasPrefix(export, "/") {
		return fmt.Errorf("NFS export %q must be an absolute path (e.g. /mnt/ssd)", export)
	}
	for _, seg := range strings.Split(export, "/") {
		if seg == ".." {
			return fmt.Errorf("NFS export %q must not contain .. — give the export's real path", export)
		}
	}
	return nil
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
