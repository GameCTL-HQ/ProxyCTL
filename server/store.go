package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
)

// hasControlChars reports whether s contains any ASCII control character
// (newline, CR, tab, NUL, …). Entry fields render into the droplet
// wg0.conf + the gateway manifest, both applied via `bash -c`/`kubectl
// apply`; a stray newline could split a rendered line. All mutation is
// authenticated-admin-only so this isn't a trust-boundary issue — it's
// defense-in-depth + stops a malformed entry corrupting a rendered config.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// PortSpec is one public port that the droplet accepts and tunnels home for
// an Entry. Proto is "tcp", "udp", or "both". Port is the number players hit
// on the droplet's public IP; it is DNATed straight through the tunnel to
// the gateway, which DNATs it on to the target Service ClusterIP.
//
// TargetPort (0 = same as Port) remaps at the GATEWAY hop: the droplet and
// the tunnel still carry the public Port, and only the gateway's final DNAT
// lands on TargetIP:TargetPort. This exists because every Source-engine
// game defaults to the same ports (27015...) — remapping lets a second game
// publish on a free public port while its own config, Service, and pod keep
// the default, no restart. The conflict check keys on the PUBLIC port only.
type PortSpec struct {
	Port       int    `json:"port"`
	Proto      string `json:"proto"`                // "tcp" | "udp" | "both"
	TargetPort int    `json:"targetPort,omitempty"` // 0 = deliver to the same port
}

// EffectiveTarget is the in-cluster port this public port delivers to.
func (p PortSpec) EffectiveTarget() int {
	if p.TargetPort != 0 {
		return p.TargetPort
	}
	return p.Port
}

// remapped reports whether this port actually changes number at the gateway.
func (p PortSpec) remapped() bool { return p.TargetPort != 0 && p.TargetPort != p.Port }

func (p PortSpec) validate() error {
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("port %d out of range", p.Port)
	}
	if p.TargetPort < 0 || p.TargetPort > 65535 {
		return fmt.Errorf("port %d: target port %d out of range", p.Port, p.TargetPort)
	}
	switch p.Proto {
	case "tcp", "udp", "both":
	default:
		return fmt.Errorf("port %d: proto must be tcp, udp, or both", p.Port)
	}
	return nil
}

// Entry is a single tunnelled game service. proxyctl renders an Entry into
// (a) the droplet wg0.conf PostUp/PostDown iptables block and (b) the
// in-cluster wg-gateway Secret's wg0.conf. v1 ships the ManualApplier (no
// stored credentials — render + copy/paste); the Applier seam lets a future
// SSHApplier push automatically without touching the store/UI.
//
// Each Entry's public Ports are DNATed on the droplet into the WireGuard
// tunnel (→ gateway 10.8.0.2), then the gateway DNATs them on to TargetIP
// (a Service ClusterIP). Name/Subdomain are cosmetic labels — game clients
// are routed by port + protocol, never by hostname.
type Entry struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`      // e.g. "satisfactory"
	Subdomain string     `json:"subdomain"` // e.g. "satisfactory.examplelabs.cc" (label only)
	Ports     []PortSpec `json:"ports"`     // public ports the droplet accepts
	TargetIP  string     `json:"targetIP"`  // target Service ClusterIP, e.g. 10.43.101.26
	Service   string     `json:"service"`   // optional "name.namespace" reminder label
	Enabled   bool       `json:"enabled"`

	// Pod-per-game data plane: each entry gets its OWN in-cluster gateway
	// pod + WireGuard tunnel, so adding/removing one game never disturbs
	// the others. TunnelIP is auto-allocated once and pinned (never moves
	// across applies). GatewayPubKey is the public key the entry's gateway
	// pod self-generates on first boot — proxyctl records only the PUBLIC
	// key (the private key never leaves the pod's Secret).
	TunnelIP      string `json:"tunnelIP,omitempty"`      // e.g. 10.8.0.2 (pinned)
	GatewayPubKey string `json:"gatewayPubKey,omitempty"` // filled by the applier post-boot

	// Mode selects the data plane for this entry.
	//
	//   "" / "inbound" — the classic per-game wg-gw pod: droplet DNATs the
	//   public ports into the tunnel, the gateway pod DNATs on to TargetIP.
	//   Outbound game traffic still leaves via the cluster's own WAN.
	//
	//   "egress" — no wg-gw pod. The WireGuard peer lives INSIDE the game
	//   pod (a sidecar the orchestrator — e.g. GameCTL — injects), and the
	//   droplet additionally SNATs the game's OUTBOUND traffic, so backends
	//   that record the server's egress address (PlayFab, master servers)
	//   see the droplet IP and player joins land on the droplet's public
	//   ports. The peer's public key is supplied by the orchestrator on the
	//   entry (proxyctl still never sees a private key — it is generated
	//   and held on the game's side of the wire).
	Mode string `json:"mode,omitempty"`
}

// IsEgress reports whether this entry uses the egress data plane.
func (e *Entry) IsEgress() bool { return e.Mode == "egress" }

// Slug is the DNS-1123 name used for this entry's per-game Kubernetes
// resources (Deployment/Secret "wg-gw-<slug>") and rule comments.
func (e *Entry) Slug() string {
	s := strings.ToLower(strings.TrimSpace(e.Name))
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r == ' ' || r == '-' || r == '_' || r == '.' {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "game"
	}
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	return out
}

func (e *Entry) validate() error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if hasControlChars(e.Name) {
		return fmt.Errorf("name contains control characters")
	}
	if hasControlChars(e.Subdomain) {
		return fmt.Errorf("subdomain contains control characters")
	}
	if hasControlChars(e.Service) {
		return fmt.Errorf("service label contains control characters")
	}
	switch e.Mode {
	case "", "inbound", "egress":
	default:
		return fmt.Errorf("mode %q is not one of \"\", \"inbound\", \"egress\"", e.Mode)
	}
	tip := strings.TrimSpace(e.TargetIP)
	if tip == "" {
		return fmt.Errorf("target ClusterIP is required")
	}
	// TargetIP renders into iptables DNAT `--to-destination`, which
	// requires a literal IP. Rejecting non-IPs here surfaces a typo'd
	// target up-front instead of producing a silently-broken rule.
	if net.ParseIP(tip) == nil {
		return fmt.Errorf("target ClusterIP %q is not a valid IP address", e.TargetIP)
	}
	if len(e.Ports) == 0 {
		return fmt.Errorf("at least one public port is required")
	}
	seen := map[string]bool{}
	for _, p := range e.Ports {
		if err := p.validate(); err != nil {
			return err
		}
		k := fmt.Sprintf("%d/%s", p.Port, p.Proto)
		if seen[k] {
			return fmt.Errorf("duplicate port %s", k)
		}
		seen[k] = true
	}
	return nil
}

// tcpPorts / udpPorts expand "both" so renderers can emit one DNAT line per
// L4 protocol (iptables -m multiport needs a single proto per rule). These
// are the PUBLIC ports — what the droplet matches — regardless of remap.
func (e *Entry) tcpPorts() []int { return e.portsFor("tcp") }
func (e *Entry) udpPorts() []int { return e.portsFor("udp") }

func (e *Entry) portsFor(proto string) []int {
	var out []int
	for _, p := range e.Ports {
		if p.Proto == proto || p.Proto == "both" {
			out = append(out, p.Port)
		}
	}
	sort.Ints(out)
	return out
}

// passthroughPortsFor / remapsFor split a proto's ports for the GATEWAY
// renderer: passthrough ports can share one multiport DNAT (destination
// carries no port), while each remapped port needs its own rule because
// `--to-destination ip:port` would collapse a multiport group onto a
// single target port.
func (e *Entry) passthroughPortsFor(proto string) []int {
	var out []int
	for _, p := range e.Ports {
		if (p.Proto == proto || p.Proto == "both") && !p.remapped() {
			out = append(out, p.Port)
		}
	}
	sort.Ints(out)
	return out
}

// remapsFor returns [public, target] pairs, public-port ordered.
func (e *Entry) remapsFor(proto string) [][2]int {
	var out [][2]int
	for _, p := range e.Ports {
		if (p.Proto == proto || p.Proto == "both") && p.remapped() {
			out = append(out, [2]int{p.Port, p.TargetPort})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// Store persists entries to a JSON file and is safe for concurrent use.
type Store struct {
	path string
	mu   sync.RWMutex
	data map[string]*Entry
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, data: map[string]*Entry{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var list []*Entry
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, e := range list {
		s.data[e.ID] = e
	}
	return s, nil
}

func (s *Store) flushLocked() error {
	list := make([]*Entry, 0, len(s.data))
	for _, e := range s.data {
		list = append(list, e)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// List returns entries sorted by name (stable order for rendered configs).
func (s *Store) List() []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Entry, 0, len(s.data))
	for _, e := range s.data {
		cp := *e
		cp.Ports = append([]PortSpec(nil), e.Ports...)
		list = append(list, &cp)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Name == list[j].Name {
			return list[i].ID < list[j].ID
		}
		return list[i].Name < list[j].Name
	})
	return list
}

func (s *Store) Get(id string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[id]
	if !ok {
		return nil, false
	}
	cp := *e
	cp.Ports = append([]PortSpec(nil), e.Ports...)
	return &cp, true
}

// protosOverlap reports whether two port protocols can collide on the
// droplet: "both" collides with everything, otherwise exact match.
func protosOverlap(a, b string) bool {
	return a == b || a == "both" || b == "both"
}

// PortConflictError is a public-port collision between enabled entries,
// carrying enough structure for the UI to offer the one-click fix:
// "publish on Suggested instead, delivered to the same in-cluster port".
type PortConflictError struct {
	Port      int    // the contested public port
	Proto     string // the claimant's proto for it
	Holder    string // name of the enabled entry that already routes it
	Suggested int    // nearest free public port (0 if none found)
}

func (e *PortConflictError) Error() string {
	msg := fmt.Sprintf(
		"public port %d/%s is already routed by enabled entry %q — one port can only reach one target",
		e.Port, e.Proto, e.Holder)
	if e.Suggested > 0 {
		return fmt.Sprintf("%s (publish on %d instead — it can still deliver to %d in-cluster — or disable that entry)", msg, e.Suggested, e.Port)
	}
	return msg + " (disable that entry or pick a different port)"
}

// suggestFreePortLocked walks upward from the contested port to the first
// public port/proto no enabled entry routes (skipping except's own ID).
// Purely advisory — the caller re-validates whatever the operator submits.
// Caller holds s.mu.
func (s *Store) suggestFreePortLocked(from int, proto, exceptID string) int {
	inUse := func(port int) bool {
		for _, other := range s.data {
			if other.ID == exceptID || !other.Enabled {
				continue
			}
			for _, op := range other.Ports {
				if op.Port == port && protosOverlap(proto, op.Proto) {
					return true
				}
			}
		}
		return false
	}
	for cand := from + 1; cand <= 65535 && cand <= from+100; cand++ {
		if cand == 51820 { // the droplet's own WireGuard listen port
			continue
		}
		if !inUse(cand) {
			return cand
		}
	}
	return 0
}

// portConflictLocked returns a *PortConflictError when an ENABLED entry e
// claims a public port/proto that another enabled entry already routes.
// Every enabled entry DNATs on the droplet's single public IP, so one
// public port can only ever reach one target — a second claim doesn't
// error at apply time, it just silently loses to iptables first-match
// (which is how two LiveKit entries ended up fighting over the media
// ports). Only the PUBLIC port is contested: two entries delivering to
// the same TargetPort on different ClusterIPs is exactly what remapping
// is for. Sharing a SUBDOMAIN across entries is fine and deliberate: DNS
// just points at the droplet, and the ports are what route. Caller holds
// s.mu.
func (s *Store) portConflictLocked(e *Entry) error {
	if !e.Enabled {
		return nil // disabled entries render nothing; conflicts re-check on enable
	}
	for _, other := range s.data {
		if other.ID == e.ID || !other.Enabled {
			continue
		}
		for _, p := range e.Ports {
			for _, op := range other.Ports {
				if p.Port == op.Port && protosOverlap(p.Proto, op.Proto) {
					return &PortConflictError{
						Port: p.Port, Proto: p.Proto, Holder: other.Name,
						Suggested: s.suggestFreePortLocked(p.Port, p.Proto, e.ID),
					}
				}
			}
		}
	}
	return nil
}

func (s *Store) Put(e *Entry) error {
	if err := e.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.portConflictLocked(e); err != nil {
		return err
	}
	if e.TunnelIP == "" {
		ip, err := s.allocTunnelIPLocked(e.ID)
		if err != nil {
			return err
		}
		e.TunnelIP = ip
	}
	_, existed := s.data[e.ID]
	s.data[e.ID] = e
	verb := "create"
	if existed {
		verb = "update"
	}
	if err := s.flushLocked(); err != nil {
		return err
	}
	log.Printf("store: %s %s (%q) tunnelIP=%s — %d entries now",
		verb, e.ID, e.Name, e.TunnelIP, len(s.data))
	return nil
}

// allocTunnelIPLocked pins the lowest free 10.8.0.H (H in 2..254) not
// already taken by another entry. .1 is the droplet; each game gets its
// own /32 so its tunnel is independent. .254 and .244-.253 are reserved
// for ProxyCTL's own control tunnel and the operator's personal-access
// peers respectively (see reservedTunnelIPs in render.go) and are never
// handed out here. Caller holds s.mu.
func (s *Store) allocTunnelIPLocked(selfID string) (string, error) {
	taken := reservedTunnelIPs()
	for id, x := range s.data {
		if id != selfID && x.TunnelIP != "" {
			taken[x.TunnelIP] = true
		}
	}
	for h := 2; h <= 254; h++ {
		ip := fmt.Sprintf("10.8.0.%d", h)
		if !taken[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("tunnel IP pool exhausted (10.8.0.2-254)")
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	nm := ""
	if e, ok := s.data[id]; ok {
		nm = e.Name
	}
	delete(s.data, id)
	if err := s.flushLocked(); err != nil {
		return err
	}
	log.Printf("store: delete %s (%q) — %d entries now", id, nm, len(s.data))
	return nil
}
