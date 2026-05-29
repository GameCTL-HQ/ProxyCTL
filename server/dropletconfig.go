package main

// Droplet setup wizard storage + helpers.
//
// History: proxyctl v1 stored ZERO credentials — Apply borrowed the
// operator's ambient ssh-agent / kubeconfig at click-time. The v2 install
// wizard makes proxyCTL self-contained: it now owns its OWN ssh keypair
// for the droplet so the operator can install once and walk away. The
// private key NEVER leaves disk and is NEVER returned by any API — the
// wizard only ever returns the public half.
//
// Storage is deliberately behind a small interface so the inevitable
// move to an in-cluster kube Secret is a single backend swap, not a
// rewrite. Today's backend is a 0600 file pair next to entries.json
// (droplet.json + droplet.key), matching how entries/domains persist.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DropletConfig is the operator-facing droplet target. The private key
// itself is intentionally NOT a field — it lives at PrivateKeyPath on
// disk (0600) and is only ever passed to `ssh -i`; nothing in the API
// surface returns it.
type DropletConfig struct {
	IP             string `json:"ip"`             // public IPv4 of the droplet
	Port           int    `json:"port"`           // SSH port (default 22)
	User           string `json:"user"`           // SSH user (default "root")
	PublicKey      string `json:"publicKey"`      // authorized_keys line; safe to display
	PrivateKeyPath string `json:"privateKeyPath"` // local file mode 0600 (not exposed in API)
	UpdatedAt      int64  `json:"updatedAt"`

	// WGPublicKey is the droplet's WireGuard interface public key, captured
	// by the bootstrap step after `wg genkey | wg pubkey` runs on the box.
	// Every per-game gateway renders this into its [Peer] for the droplet,
	// so without it Apply on a fresh droplet would write the wrong peer key.
	// Empty until the operator runs "Prepare droplet" in the wizard.
	WGPublicKey   string `json:"wgPublicKey,omitempty"`
	Bootstrapped  bool   `json:"bootstrapped,omitempty"` // marks Step-4 done
	BootstrappedAt int64 `json:"bootstrappedAt,omitempty"`

	// SSHLockedDown is true once the operator has applied the optional
	// "restrict SSH to these IPs" step. SSHAllowedIPs is the live list
	// (so the wizard can show / let them edit / remove later).
	SSHLockedDown  bool     `json:"sshLockedDown,omitempty"`
	SSHAllowedIPs  []string `json:"sshAllowedIPs,omitempty"`
	SSHLockedAt    int64    `json:"sshLockedAt,omitempty"`
}

// Configured is true once the operator has saved an IP AND a keypair
// has been generated. The applier uses this to decide whether to prefer
// the stored config over the legacy -droplet-ssh CLI flag.
func (d *DropletConfig) Configured() bool {
	return d != nil && d.IP != "" && d.PrivateKeyPath != "" && d.PublicKey != ""
}

func (d *DropletConfig) target() string {
	user := d.User
	if user == "" {
		user = "root"
	}
	return user + "@" + d.IP
}

func (d *DropletConfig) port() int {
	if d.Port == 0 {
		return 22
	}
	return d.Port
}

// DropletStore is the persistence seam. Today: a JSON file + a key file.
// Tomorrow: a kube Secret reader. Keep this interface small.
type DropletStore interface {
	Get() *DropletConfig
	Save(cfg DropletConfig) error
	// EnsureKeypair returns the existing keypair's public key if one is
	// stored; otherwise it generates a fresh ed25519 keypair, persists
	// the private half (0600), and returns the public half.
	EnsureKeypair() (publicKey, privateKeyPath string, err error)
	// Regenerate forces a NEW keypair, replacing the existing one. The
	// previous private key is overwritten; the public key the operator
	// installed on the droplet must be re-installed.
	Regenerate() (publicKey, privateKeyPath string, err error)
	// SetWGPubKey records the droplet's WireGuard public key after
	// bootstrap. Marks the droplet as Bootstrapped so the wizard can
	// collapse Step 4 and Apply knows the gateway peer config is valid.
	SetWGPubKey(pubkey string) error
	// SetSSHLockdown records that the operator has restricted SSH to a
	// specific allow-list (or, with locked=false, that they've removed
	// the restriction). The IP list is persisted so the wizard can
	// show / edit it later.
	SetSSHLockdown(locked bool, ips []string) error
}

// fileDropletStore is the v1 backend: a JSON config + an OpenSSH-format
// private key file on local disk, both 0600. Matches entries.json /
// domains.json. The whole struct is lock-protected because the wizard
// can trigger generate + save concurrently with an Apply reading IP.
type fileDropletStore struct {
	mu      sync.RWMutex
	path    string // droplet.json
	keyPath string // droplet.key (OpenSSH-format private key)
	cfg     *DropletConfig
}

func NewFileDropletStore(dir string) (*fileDropletStore, error) {
	s := &fileDropletStore{
		path:    filepath.Join(dir, "droplet.json"),
		keyPath: filepath.Join(dir, "droplet.key"),
	}
	if b, err := os.ReadFile(s.path); err == nil {
		var c DropletConfig
		if json.Unmarshal(b, &c) == nil {
			// Re-pin the key path in case the directory moved.
			c.PrivateKeyPath = s.keyPath
			s.cfg = &c
		}
	}
	return s, nil
}

func (s *fileDropletStore) Get() *DropletConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg == nil {
		return nil
	}
	c := *s.cfg
	return &c
}

// validate guards what the operator types in the wizard. We reject early
// so a bad IP doesn't sit in storage waiting to fail at Apply time.
func (cfg DropletConfig) validate() error {
	if net.ParseIP(strings.TrimSpace(cfg.IP)) == nil {
		return errors.New("droplet IP must be a valid IPv4 / IPv6 address")
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return fmt.Errorf("port %d out of range", cfg.Port)
	}
	if u := strings.TrimSpace(cfg.User); u != "" {
		if strings.ContainsAny(u, " \t\n@:/") {
			return errors.New("user contains illegal characters")
		}
	}
	return nil
}

func (s *fileDropletStore) Save(cfg DropletConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg.PrivateKeyPath = s.keyPath
	if s.cfg != nil {
		// Preserve fields the API caller doesn't get to set: the public
		// key only comes from EnsureKeypair, never from the wizard form.
		cfg.PublicKey = s.cfg.PublicKey
	}
	cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		return err
	}
	c := cfg
	s.cfg = &c
	return nil
}

// genKeypair shells out to the system `ssh-keygen` to produce an
// OpenSSH-format ed25519 keypair. Using ssh-keygen (already required
// to USE ssh in the first place) sidesteps an external Go crypto/ssh
// dep AND guarantees the on-disk format is byte-perfect for `ssh -i`.
func (s *fileDropletStore) genKeypair() (string, error) {
	// Atomic-replace via a sibling tmp path so a half-written key never
	// becomes visible at the canonical path on a crash mid-generation.
	tmp := s.keyPath + ".new"
	_ = os.Remove(tmp)
	_ = os.Remove(tmp + ".pub")
	cmd := exec.Command("ssh-keygen",
		"-t", "ed25519",
		"-N", "", // no passphrase — at-rest protection is filesystem perms
		"-C", "proxyctl@proxyctl",
		"-q",
		"-f", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ssh-keygen failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", err
	}
	pubBytes, err := os.ReadFile(tmp + ".pub")
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp, s.keyPath); err != nil {
		return "", err
	}
	_ = os.Remove(tmp + ".pub")
	return strings.TrimSpace(string(pubBytes)), nil
}

func (s *fileDropletStore) EnsureKeypair() (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != nil && s.cfg.PublicKey != "" {
		if _, err := os.Stat(s.keyPath); err == nil {
			return s.cfg.PublicKey, s.keyPath, nil
		}
	}
	pub, err := s.genKeypair()
	if err != nil {
		return "", "", err
	}
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.PublicKey = pub
	s.cfg.PrivateKeyPath = s.keyPath
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	_ = os.WriteFile(s.path, b, 0o600)
	return pub, s.keyPath, nil
}

func (s *fileDropletStore) SetWGPubKey(pubkey string) error {
	pubkey = strings.TrimSpace(pubkey)
	if pubkey == "" {
		return errors.New("empty wg pubkey")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.WGPublicKey = pubkey
	s.cfg.Bootstrapped = true
	s.cfg.BootstrappedAt = time.Now().Unix()
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) SetSSHLockdown(locked bool, ips []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.SSHLockedDown = locked
	s.cfg.SSHAllowedIPs = ips
	if locked {
		s.cfg.SSHLockedAt = time.Now().Unix()
	}
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) Regenerate() (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pub, err := s.genKeypair()
	if err != nil {
		return "", "", err
	}
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.PublicKey = pub
	s.cfg.PrivateKeyPath = s.keyPath
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	_ = os.WriteFile(s.path, b, 0o600)
	return pub, s.keyPath, nil
}

// sshArgs returns the `ssh -i ...` argv prefix the applier should use
// when a stored droplet config is present. Empty means "no stored
// config — fall back to whatever the operator supplied via CLI flag".
func (cfg *DropletConfig) sshArgs() []string {
	if !cfg.Configured() {
		return nil
	}
	return []string{
		"-i", cfg.PrivateKeyPath,
		"-p", strconv.Itoa(cfg.port()),
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(filepath.Dir(cfg.PrivateKeyPath), "known_hosts"),
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
	}
}
