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
	WGPublicKey    string `json:"wgPublicKey,omitempty"`
	Bootstrapped   bool   `json:"bootstrapped,omitempty"` // marks Step-4 done
	BootstrappedAt int64  `json:"bootstrappedAt,omitempty"`

	// WANIface is the droplet's public egress interface the NAT/DNAT rules
	// bind to. Auto-detected (`ip route get 1.1.1.1` → dev) at bootstrap and
	// refreshed on every Apply — a hardcoded "eth0" is only right on
	// providers that name it that (DigitalOcean yes; Hetzner/AWS-style
	// ens3/enX0 no), and a wrong iface silently keeps public traffic out of
	// the PROXYCTL chains.
	WANIface string `json:"wanIface,omitempty"`
	// WANIfaceManual pins the operator's explicit choice: auto-detection
	// stops overwriting WANIface and Apply only WARNS when detection
	// disagrees. Cleared to return to auto.
	WANIfaceManual bool `json:"wanIfaceManual,omitempty"`
	// WANIfaces is the droplet's global-scope interface list ("name addr"),
	// captured at bootstrap so the wizard can offer a picker when the box
	// has more than one candidate.
	WANIfaces []string `json:"wanIfaces,omitempty"`

	// SSHLockedDown is true once the operator has applied the optional
	// "restrict SSH to these IPs" step. SSHAllowedIPs is the live list
	// (so the wizard can show / let them edit / remove later).
	SSHLockedDown bool     `json:"sshLockedDown,omitempty"`
	SSHAllowedIPs []string `json:"sshAllowedIPs,omitempty"`
	SSHLockedAt   int64    `json:"sshLockedAt,omitempty"`

	// SSHTunnelOnly is true once SSH has been restricted to ONLY
	// ProxyCTL's own verified control-tunnel peer (see
	// restrictSSHToTunnel in main.go) — the default, recommended
	// end-state of the conversion: it replaces a public-IP allow-list
	// entirely (SSHAllowedIPs is cleared), since the tunnel survives a
	// home-IP change and an allow-list doesn't. SSHLockedDown stays true
	// alongside this — it means "SSH is restricted", this field says HOW.
	SSHTunnelOnly bool `json:"sshTunnelOnly,omitempty"`

	// SSHFirewallGate is true once the network-level iptables gate on
	// port 22 (PROXYCTL-SSH chain, see dropletRules in render.go) has
	// been applied and verified, on top of SSHTunnelOnly's sshd-level
	// restriction. Optional hardening layer — SSHTunnelOnly alone is
	// already safe; this just means a non-tunnel connection gets
	// dropped at the firewall instead of merely refused by sshd.
	SSHFirewallGate bool `json:"sshFirewallGate,omitempty"`

	// ControlTunnelReady is true once the operator has run "Set up control
	// tunnel" AND a live SSH round-trip over it has been verified (see
	// setupControlTunnel in main.go). SSHApplier only ever PREFERS the
	// control-tunnel address when this is true; it always falls back to
	// the public IP on any failure, so a stale/unready tunnel never blocks
	// an apply — it only stops being tried as the fast path.
	ControlTunnelReady bool  `json:"controlTunnelReady,omitempty"`
	ControlTunnelAt    int64 `json:"controlTunnelAt,omitempty"`

	// ControlTunnelNudgeDismissed silences the main-app banner that urges an
	// already-locked-down install (SSHLockedDown true, from before the
	// control tunnel existed) to set one up. Sticky once dismissed — a
	// one-time nudge, not a nag on every page load — and irrelevant once
	// ControlTunnelReady is true, since the banner's trigger already
	// excludes that case.
	ControlTunnelNudgeDismissed bool `json:"controlTunnelNudgeDismissed,omitempty"`

	// PersonalAccessPeers are named WireGuard peers letting a human SSH
	// into the droplet over the same tunnel, independent of ProxyCTL's
	// own control-tunnel peer (see server/personalaccess.go) — one per
	// device (laptop, phone, ...). Each PRIVATE key is never stored here
	// (or anywhere) — generated in memory, handed to the operator once as
	// a downloadable client config, and forgotten; losing it means
	// generating a fresh one for that device (same posture as
	// Regenerate() for the droplet's own SSH keypair), not re-serving an
	// old secret.
	PersonalAccessPeers []PersonalAccessPeer `json:"personalAccessPeers,omitempty"`

	// F2BPolicy is the operator's fail2ban ban policy, when they've changed
	// it from the defaults in fail2ban.go (nil = defaults). Persisted so
	// "Re-apply fail2ban config" and the status endpoint keep describing
	// the policy that's actually rendered onto the droplet.
	F2BPolicy *F2BPolicy `json:"f2bPolicy,omitempty"`
}

// F2BPolicy is an operator-tuned fail2ban ban policy. Zero-value fields
// fall back to the defaults in fail2ban.go (see effectiveF2BPolicy).
type F2BPolicy struct {
	MaxRetry int    `json:"maxRetry,omitempty"` // failures before a ban
	FindTime string `json:"findTime,omitempty"` // window the failures must land in, fail2ban time format ("10m")
	BanTime  string `json:"banTime,omitempty"`  // ban duration; "-1" = permanent
}

// PersonalAccessPeer is one operator-managed personal-access peer.
type PersonalAccessPeer struct {
	Name      string `json:"name"`
	PubKey    string `json:"pubKey"` // WireGuard public key — network-layer reachability only
	IP        string `json:"ip"`
	CreatedAt int64  `json:"createdAt"`
	// SSHPubKey is this device's OWN dedicated SSH public key (the exact
	// line appended to the droplet's authorized_keys) — a second,
	// independent secret on top of the WireGuard key above, so a leaked
	// WireGuard config alone still isn't enough to log in, AND revoking
	// one device never requires touching the shared original droplet key
	// every other device/human might still be using. Safe to store/show
	// (public key). Empty for peers created before this field existed —
	// those devices still authenticate with whatever key they always
	// used; nothing to revoke here for them.
	SSHPubKey string `json:"sshPubKey,omitempty"`
}

// Configured is true once the operator has saved an IP AND a keypair
// has been generated. The applier uses this to decide whether to prefer
// the stored config over the legacy -droplet-ssh CLI flag.
func (d *DropletConfig) Configured() bool {
	return d != nil && d.IP != "" && d.PrivateKeyPath != "" && d.PublicKey != ""
}

// user is the effective SSH login user. Defaults to "root" to match
// DigitalOcean images, which is what v1 assumed everywhere.
func (d *DropletConfig) user() string {
	if u := strings.TrimSpace(d.User); u != "" {
		return u
	}
	return "root"
}

func (d *DropletConfig) target() string {
	return d.user() + "@" + d.IP
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
	// SetWANIface records the droplet's public egress interface. manual=true
	// pins an operator choice (auto-detection stops overwriting it);
	// manual=true with iface="" clears the pin, returning to auto.
	// manual=false is the auto-detection path and is a no-op while a manual
	// pin is in place.
	SetWANIface(iface string, manual bool) error
	// SetWANIfaces records the droplet's interface list for the picker.
	SetWANIfaces(list []string) error
	// SetSSHLockdown records that the operator has restricted SSH to a
	// specific allow-list (or, with locked=false, that they've removed
	// the restriction). The IP list is persisted so the wizard can
	// show / edit it later. Always clears SSHTunnelOnly — this is the
	// IP-allow-list mode, not the tunnel-only mode.
	SetSSHLockdown(locked bool, ips []string) error
	// SetControlTunnelReady records whether ProxyCTL's dedicated control
	// tunnel to the droplet has been verified working end-to-end.
	SetControlTunnelReady(ready bool) error
	// SetSSHTunnelOnly records that SSH has been restricted to ONLY
	// ProxyCTL's own control-tunnel peer, replacing any public-IP
	// allow-list (SSHAllowedIPs is cleared). Implies SSHLockedDown=true.
	SetSSHTunnelOnly(on bool) error
	// SetSSHFirewallGate records whether the network-level port-22
	// iptables gate is currently applied and verified.
	SetSSHFirewallGate(on bool) error
	// SetControlTunnelNudgeDismissed records that the operator has
	// dismissed the "move to the control tunnel" banner, so it doesn't
	// reappear on every page load.
	SetControlTunnelNudgeDismissed(dismissed bool) error
	// SetF2BPolicy stores an operator-tuned fail2ban policy (nil returns
	// to the built-in defaults). Caller is responsible for re-applying
	// the jail config on the droplet — this only records intent.
	SetF2BPolicy(p *F2BPolicy) error
	// AddPersonalAccessPeer appends a new named personal-access peer.
	AddPersonalAccessPeer(peer PersonalAccessPeer) error
	// RemovePersonalAccessPeer removes the peer with the given public key.
	// A pubkey not currently present is a no-op, not an error.
	RemovePersonalAccessPeer(pubKey string) error
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

func (s *fileDropletStore) SetWANIface(iface string, manual bool) error {
	iface = strings.TrimSpace(iface)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	switch {
	case manual && iface == "":
		// Clear the pin: auto-detection resumes; keep the last value so
		// renders stay stable until the next detection runs.
		s.cfg.WANIfaceManual = false
	case manual:
		s.cfg.WANIface = iface
		s.cfg.WANIfaceManual = true
	default:
		if iface == "" {
			return errors.New("empty interface name")
		}
		if s.cfg.WANIfaceManual {
			return nil // operator's pin wins over auto-detection
		}
		s.cfg.WANIface = iface
	}
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) SetWANIfaces(list []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.WANIfaces = list
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
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
	s.cfg.SSHTunnelOnly = false
	s.cfg.SSHFirewallGate = false
	if locked {
		s.cfg.SSHLockedAt = time.Now().Unix()
	}
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) SetSSHTunnelOnly(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.SSHTunnelOnly = on
	if on {
		s.cfg.SSHLockedDown = true
		s.cfg.SSHAllowedIPs = nil
		s.cfg.SSHLockedAt = time.Now().Unix()
	}
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) SetSSHFirewallGate(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.SSHFirewallGate = on
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) SetControlTunnelReady(ready bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.ControlTunnelReady = ready
	if ready {
		s.cfg.ControlTunnelAt = time.Now().Unix()
	}
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) SetF2BPolicy(p *F2BPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.F2BPolicy = p
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) SetControlTunnelNudgeDismissed(dismissed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.ControlTunnelNudgeDismissed = dismissed
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) AddPersonalAccessPeer(peer PersonalAccessPeer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &DropletConfig{}
	}
	s.cfg.PersonalAccessPeers = append(s.cfg.PersonalAccessPeers, peer)
	s.cfg.UpdatedAt = time.Now().Unix()
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *fileDropletStore) RemovePersonalAccessPeer(pubKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		return nil
	}
	kept := make([]PersonalAccessPeer, 0, len(s.cfg.PersonalAccessPeers))
	for _, p := range s.cfg.PersonalAccessPeers {
		if p.PubKey != pubKey {
			kept = append(kept, p)
		}
	}
	s.cfg.PersonalAccessPeers = kept
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
