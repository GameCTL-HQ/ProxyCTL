package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// defaultKeysStorageClass is the StorageClass for the default keys base folder
// (ProxyCTL/Keys); it matches the bundled k8s/storageclass-proxyctl-keys.yaml.
// Any other operator-chosen folder gets its own derived class (see keysSCName).
const defaultKeysStorageClass = "nfs-ssd-proxyctl-keys"

// controlTunnelIP is ProxyCTL's OWN fixed address on the droplet's
// WireGuard network — reserved out of the per-game allocator's pool (see
// Store.allocTunnelIPLocked) so a game entry can never be handed this
// address. It backs the dedicated "control tunnel" ProxyCTL uses to reach
// the droplet's sshd (see server/controlwg.go) instead of riding over the
// operator's raw public-internet IP, which is the thing that broke on the
// 2026-07-21 router-restart incident.
const controlTunnelIP = "10.8.0.254"

// personalAccessIPLow/High reserve a small range for the operator's OWN
// personal-access WireGuard peers — excluded from the per-game
// allocator's pool the same way controlTunnelIP is, so a game entry can
// never be handed one of these addresses either. Each named device (see
// server/personalaccess.go) gets the lowest free address in this range.
// Lets a human SSH into the droplet over the same resilient tunnel
// ProxyCTL itself uses, instead of losing all direct access once SSH is
// restricted to tunnel-only (see restrictSSHToTunnel in main.go). Ten
// slots is plenty for a homelab operator's own devices.
const (
	personalAccessIPLow  = 244 // 10.8.0.244
	personalAccessIPHigh = 253 // 10.8.0.253
)

// reservedTunnelIPs are addresses the per-game allocator
// (Store.allocTunnelIPLocked) must never hand out: ProxyCTL's own
// control tunnel plus the whole personal-access range above.
func reservedTunnelIPs() map[string]bool {
	taken := map[string]bool{controlTunnelIP: true}
	for h := personalAccessIPLow; h <= personalAccessIPHigh; h++ {
		taken[fmt.Sprintf("10.8.0.%d", h)] = true
	}
	return taken
}

// keysSCName returns the StorageClass name for a keys base folder. The default
// folder keeps the bundled class name; any other folder gets a stable per-path
// suffix, because a StorageClass pathPattern is fixed at creation — distinct
// folders therefore need distinct classes.
func keysSCName(basePath string) string {
	bp := normalizeKeysBasePath(basePath)
	if bp == "" || bp == defaultKeysBasePath {
		return defaultKeysStorageClass
	}
	sum := sha256.Sum256([]byte(bp))
	return defaultKeysStorageClass + "-" + hex.EncodeToString(sum[:])[:8]
}

// renderKeysStorageClass renders the StorageClass that nests per-gateway key
// dirs under basePath via the nfs-subdir provisioner's pathPattern, reclaiming
// the dir on delete so stale gateways don't accumulate. ProxyCTL applies this
// (create-if-missing) before rendering gateways.
//
// The provisioner is discovered from the StorageClass this install runs on
// (see storagediscover.go), never hardcoded: the nfs-subdir chart derives its
// provisioner name from the Helm release name, so a literal would only match
// the cluster it was copied from and would leave every keys PVC Pending
// against a non-existent provisioner anywhere else.
func renderKeysStorageClass(basePath string) string {
	return renderKeysStorageClassWith(basePath, discoverStorage().Provisioner)
}

// renderKeysStorageClassWith is the pure form — provisioner injected, so the
// rendering is testable without a cluster.
func renderKeysStorageClassWith(basePath, provisioner string) string {
	bp := normalizeKeysBasePath(basePath)
	if bp == "" {
		bp = defaultKeysBasePath
	}
	if provisioner == "" {
		// Discovery failed (trimmed RBAC / missing class). The bundled
		// default is the best remaining guess; a wrong provisioner surfaces
		// immediately as a Pending PVC, which is easier to diagnose than
		// rendering a class with no provisioner at all (rejected by the API).
		provisioner = legacyKeysProvisioner
	}
	var b strings.Builder
	b.WriteString("apiVersion: storage.k8s.io/v1\nkind: StorageClass\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n", keysSCName(bp))
	b.WriteString("  labels: { app.kubernetes.io/managed-by: proxyctl }\n")
	fmt.Fprintf(&b, "provisioner: %s\n", provisioner)
	b.WriteString("parameters:\n")
	fmt.Fprintf(&b, "  pathPattern: \"%s/${.PVC.namespace}-${.PVC.name}\"\n", bp)
	b.WriteString("  archiveOnDelete: \"false\"\n")
	b.WriteString("reclaimPolicy: Delete\nvolumeBindingMode: Immediate\nallowVolumeExpansion: true\n")
	return b.String()
}

// ---- Share mode: keys on an operator-named NFS export ----------------------
//
// An nfs-subdir provisioner mounts ONE fixed export, and a StorageClass has no
// field to point it elsewhere — so honouring a typed share means bypassing the
// provisioner with a static PV.
//
// Every gateway mounts the export ROOT inline (volumes.nfs) and carves its
// own dir via subPath. That is deliberate: an NFS mount of a path that
// doesn't exist yet fails, and nothing here can mkdir on a remote NAS — but
// kubelet CREATES a subPath on demand. Mounting the root doesn't expose it:
// a subPath mount shows the container only its own subdir.

// keysSubPath is the per-gateway dir under the export.
//
// It reproduces the nfs-subdir provisioner's own naming
// (`${.PVC.namespace}-${.PVC.name}` -> "proxyctl-wg-gw-cs2-keys") on purpose:
// pointing share mode at the export a provisioner was already using then finds
// the EXISTING keypairs in place, so gateways keep their identity and the
// droplet's [Peer] stays valid. Diverging here would silently re-key every
// tunnel.
func keysSubPath(basePath, ns, gwName string) string {
	bp := normalizeKeysBasePath(basePath)
	if bp == "" {
		bp = defaultKeysBase()
	}
	return bp + "/" + ns + "-" + gwName + "-keys"
}

// Base WireGuard infrastructure. These are SHARED, one-time facts about the
// already-deployed tunnel (see wireguard/WIREGUARD.md) — proxyctl never
// changes them; it only renders the *per-entry* DNAT/FORWARD rules that ride
// on top. Keys are NOT held here: the gateway manifest is rendered as a
// template with a PrivateKey placeholder so no secret material lives in the
// store or in proxyctl's memory.
type Infra struct {
	DropletPublicIP string // public droplet IP, e.g. 192.0.2.123
	WGPort          int    // WireGuard UDP port, e.g. 51820
	DropletWGIP     string // droplet tunnel IP, e.g. 10.8.0.1
	GatewayWGIP     string // in-cluster gateway tunnel IP, e.g. 10.8.0.2
	WGSubnet        string // e.g. 10.8.0.0/24
	GatewayPubKey   string // droplet's view of the gateway peer public key
	DropletPubKey   string // gateway's view of the droplet peer public key
	K8sNamespace    string // ProxyCTL's own namespace — where per-entry wg-gw-*
	// Deployments/Secrets live. Single home for every gateway so one
	// ProxyCTL can front Services in any cluster namespace without
	// needing a Role grant in each target namespace.
	WGImageDigest string // pinned WireGuard image ref (first-party wireguard-kube)
	WANIface      string // droplet public interface, e.g. eth0
	// KeysBasePath is the operator-chosen relative folder under the export
	// where per-gateway keypair dirs are nested (default "ProxyCTL/Keys").
	KeysBasePath string
	// KeysNFSServer / KeysNFSExport are the operator-named NFS share for
	// gateway keys. Both empty = provisioner mode (derive a StorageClass on
	// the install's own class). Both set = share mode (static PV on that
	// export) — see renderKeysSharePV.
	KeysNFSServer string
	KeysNFSExport string

	// ControlPubKey is ProxyCTL's own WireGuard public key for its
	// dedicated control tunnel (see controlwg.go). Empty until the
	// operator runs "Set up control tunnel" in Setup → Droplet — with it
	// empty, RenderDropletWG0 emits no control [Peer] stanza at all, so
	// every install that hasn't opted in renders byte-for-byte as before.
	ControlPubKey string

	// PersonalAccessPeers are the operator's own personal-access WireGuard
	// peers (see server/personalaccess.go) — empty until they generate
	// one, same "no stanza at all if unused" behavior as ControlPubKey
	// above, just N stanzas instead of at most one.
	PersonalAccessPeers []PersonalAccessPeerInfo

	// SSHTunnelOnly mirrors DropletConfig.SSHTunnelOnly — when true,
	// dropletRules ALSO gates port 22 itself at the firewall (PROXYCTL-SSH
	// chain in INPUT), not just at the sshd layer (see restrictSSHToTunnel
	// in main.go). Kept as a plain bool on Infra (not re-derived from
	// DropletConfig inside render.go) so this file stays free of a
	// dependency on DropletStore.
	SSHTunnelOnly bool
}

// PersonalAccessPeerInfo is the minimal per-peer info RenderDropletWG0
// needs to emit a [Peer] stanza — decoupled from DropletConfig's storage
// shape (PersonalAccessPeer) so render.go doesn't need to import it.
type PersonalAccessPeerInfo struct {
	Name   string // rendered only into a comment, for readability
	PubKey string
	IP     string
}

// keysShareMode is true when the operator named an explicit NFS share.
func (in Infra) keysShareMode() bool {
	return in.KeysNFSServer != "" && in.KeysNFSExport != ""
}

// DefaultInfra mirrors the live deployment documented in
// wireguard/WIREGUARD.md so a fresh run renders something correct to review.
func DefaultInfra() Infra {
	return Infra{
		DropletPublicIP: "192.0.2.123",
		WGPort:          51820,
		DropletWGIP:     "10.8.0.1",
		GatewayWGIP:     "10.8.0.2",
		WGSubnet:        "10.8.0.0/24",
		// Documented in wireguard/WIREGUARD.md: the droplet's [Peer] view of
		// the gateway. The gateway's [Peer] view of the droplet's public key
		// is NOT in the runbook — left as a placeholder for the operator to
		// fill from the live config (proxyctl never derives keys).
		// Public keys only (safe to embed — DefaultInfra mirrors the live
		// deployment; private keys are never held, only placeholders).
		// droplet's [Peer] points at the gateway → gateway's pubkey:
		GatewayPubKey: "__GATEWAY_PUBLIC_KEY__",
		// gateway's [Peer] points at the droplet → droplet's pubkey:
		DropletPubKey: "__DROPLET_PUBLIC_KEY__",
		// Gateways live alongside ProxyCTL itself. Overridden by
		// main.go from the downward-API POD_NAMESPACE on real runs;
		// "proxyctl" is the fallback for local renders.
		K8sNamespace: "proxyctl",
		// First-party from-scratch image (GameCTL-HQ/WireGuard-Kube): Debian
		// official base + Debian wireguard-tools, host-kernel data plane,
		// same /config/wg_confs contract the manifests already use.
		WGImageDigest: "ghcr.io/gamectl-hq/wireguard-kube@sha256:baba2d49d8928f2d85e98ca58c27b4bbc208b62a00bf620bf95a15a8b245b785",
		WANIface:      "eth0",
		KeysBasePath:  defaultKeysBasePath,
	}
}

// Rendered is the full review bundle produced from the enabled entries.
type Rendered struct {
	DropletWG0Conf string `json:"dropletWg0Conf"` // full droplet /etc/wireguard/wg0.conf
	GatewayYAML    string `json:"gatewayYaml"`    // wg-gateway Secret+Deployment manifest
	ApplyScript    string `json:"applyScript"`    // copy/paste runbook
	Summary        string `json:"summary"`        // per-entry port → target map
	DNSGuide       string `json:"dnsGuide"`       // exact DNS records to create
}

func multiport(ports []int) string {
	ss := make([]string, len(ports))
	for i, p := range ports {
		ss[i] = strconv.Itoa(p)
	}
	return strings.Join(ss, ",")
}

func enabled(entries []*Entry) []*Entry {
	out := make([]*Entry, 0, len(entries))
	for _, e := range entries {
		if e.Enabled {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// dropletRules renders the iptables PostUp/PostDown lines for the droplet's
// wg0.conf: public ports are DNATed to the gateway tunnel IP, MASQUERADEd so
// replies return, and FORWARD is allowed in both directions for the tunnel.
//
// All rules live in dedicated chains (nat PROXYCTL + PROXYCTL-SNAT, filter
// PROXYCTL) that PostUp CREATES then FLUSHES every apply, so the desired
// set is rebuilt from scratch each time — adding, editing, or deleting an
// entry can never leave stale/duplicate rules behind. wg-quick's PostDown
// can't be relied on for cleanup: the applier overwrites wg0.conf before
// restarting, so PostDown would delete the *new* rules, not the old ones.
// The flush-on-PostUp is what actually guarantees idempotency; PostDown is
// just best-effort unhook for a clean `wg-quick down`.
func dropletRules(in Infra, entries []*Entry) (postUp, postDown []string) {
	wan := in.WANIface
	if wan == "" {
		wan = "eth0"
	}
	var up, down []string
	// Kernel forwarding (also persisted in /etc/sysctl.conf on the box).
	up = append(up, "sysctl -w net.ipv4.ip_forward=1")
	// Create+flush the managed chains and ensure exactly one jump into each.
	up = append(up,
		"iptables -t nat -N PROXYCTL 2>/dev/null || true; iptables -t nat -F PROXYCTL",
		"iptables -t nat -N PROXYCTL-SNAT 2>/dev/null || true; iptables -t nat -F PROXYCTL-SNAT",
		"iptables -N PROXYCTL 2>/dev/null || true; iptables -F PROXYCTL",
		fmt.Sprintf("iptables -t nat -C PREROUTING -i %s -j PROXYCTL 2>/dev/null || iptables -t nat -A PREROUTING -i %s -j PROXYCTL", wan, wan),
		"iptables -t nat -C POSTROUTING -j PROXYCTL-SNAT 2>/dev/null || iptables -t nat -A POSTROUTING -j PROXYCTL-SNAT",
		"iptables -C FORWARD -j PROXYCTL 2>/dev/null || iptables -A FORWARD -j PROXYCTL",
	)
	// Per-game DNAT: each entry's public ports go to ITS OWN tunnel IP
	// (its isolated gateway pod), and replies are MASQUERADEd per /32.
	for _, e := range entries {
		tip := e.TunnelIP
		if tip == "" {
			continue
		}
		if tp := e.tcpPorts(); len(tp) > 0 {
			mp := multiport(tp)
			up = append(up,
				fmt.Sprintf("# %s (%s) tcp -> %s", e.Name, e.Subdomain, tip),
				fmt.Sprintf("iptables -t nat -A PROXYCTL -p tcp -m multiport --dports %s -j DNAT --to-destination %s", mp, tip),
				fmt.Sprintf("iptables -A PROXYCTL -i %s -o wg0 -p tcp -m multiport --dports %s -j ACCEPT", wan, mp),
			)
		}
		if pu := e.udpPorts(); len(pu) > 0 {
			mp := multiport(pu)
			up = append(up,
				fmt.Sprintf("# %s (%s) udp -> %s", e.Name, e.Subdomain, tip),
				fmt.Sprintf("iptables -t nat -A PROXYCTL -p udp -m multiport --dports %s -j DNAT --to-destination %s", mp, tip),
				fmt.Sprintf("iptables -A PROXYCTL -i %s -o wg0 -p udp -m multiport --dports %s -j ACCEPT", wan, mp),
			)
		}
		up = append(up,
			fmt.Sprintf("iptables -t nat -A PROXYCTL-SNAT -d %s/32 -o wg0 -j MASQUERADE", tip))
		// Egress entries: the peer is the game pod itself, and its OUTBOUND
		// traffic exits here so matchmaking backends record the droplet IP.
		// Scoped to this entry's /32 — only this one game can egress.
		if e.IsEgress() {
			up = append(up,
				fmt.Sprintf("# %s egress (outbound via droplet)", e.Name),
				fmt.Sprintf("iptables -A PROXYCTL -i wg0 -o %s -s %s/32 -j ACCEPT", wan, tip),
				fmt.Sprintf("iptables -t nat -A PROXYCTL-SNAT -s %s/32 -o %s -j MASQUERADE", tip, wan),
			)
		}
	}
	// Shared ESTABLISHED/RELATED return path for all tunnels.
	up = append(up,
		"iptables -A PROXYCTL -i wg0 -m state --state ESTABLISHED,RELATED -j ACCEPT",
	)

	// Port 22 firewall gate (network layer, on top of the sshd_config
	// gate restrictSSHToTunnel already applies at the application layer —
	// see main.go). The chain is ALWAYS created+flushed, same
	// always-rebuild idempotency as PROXYCTL/PROXYCTL-SNAT above, so
	// turning tunnel-only OFF cleanly empties it out on the very next
	// apply; it's only POPULATED (accept the tunnel peers, drop
	// everything else) while SSHTunnelOnly is actually on. An empty
	// custom chain is an inert pass-through in iptables, never a DROP —
	// so every install that hasn't opted into tunnel-only sees this add
	// nothing but a harmless empty chain.
	up = append(up,
		"iptables -N PROXYCTL-SSH 2>/dev/null || true; iptables -F PROXYCTL-SSH",
	)
	if in.SSHTunnelOnly {
		allowed := []string{controlTunnelIP}
		for _, p := range in.PersonalAccessPeers {
			allowed = append(allowed, p.IP)
		}
		up = append(up,
			"iptables -C INPUT -p tcp --dport 22 -m state --state NEW -j PROXYCTL-SSH 2>/dev/null || "+
				"iptables -I INPUT 1 -p tcp --dport 22 -m state --state NEW -j PROXYCTL-SSH",
		)
		for _, ip := range allowed {
			up = append(up, fmt.Sprintf("iptables -A PROXYCTL-SSH -s %s/32 -j RETURN", ip))
		}
		up = append(up, "iptables -A PROXYCTL-SSH -j DROP")
	} else {
		// Remove a stale jump from a previous apply — harmless either way
		// (an empty chain is a pass-through) but keeps INPUT free of
		// dead references once tunnel-only is off.
		up = append(up,
			"iptables -D INPUT -p tcp --dport 22 -m state --state NEW -j PROXYCTL-SSH 2>/dev/null || true",
		)
	}

	down = append(down,
		fmt.Sprintf("iptables -t nat -D PREROUTING -i %s -j PROXYCTL 2>/dev/null || true", wan),
		"iptables -t nat -D POSTROUTING -j PROXYCTL-SNAT 2>/dev/null || true",
		"iptables -D FORWARD -j PROXYCTL 2>/dev/null || true",
		"iptables -D INPUT -p tcp --dport 22 -m state --state NEW -j PROXYCTL-SSH 2>/dev/null || true",
		"iptables -t nat -F PROXYCTL 2>/dev/null || true; iptables -t nat -X PROXYCTL 2>/dev/null || true",
		"iptables -t nat -F PROXYCTL-SNAT 2>/dev/null || true; iptables -t nat -X PROXYCTL-SNAT 2>/dev/null || true",
		"iptables -F PROXYCTL 2>/dev/null || true; iptables -X PROXYCTL 2>/dev/null || true",
		"iptables -F PROXYCTL-SSH 2>/dev/null || true; iptables -X PROXYCTL-SSH 2>/dev/null || true",
	)
	return up, down
}

// RenderDropletNATScript returns just the droplet iptables PROXYCTL
// chain build as a shell script (no wg-quick, no comments). The applier
// runs this directly so the chains are rebuilt idempotently WITHOUT
// bouncing the wg0 interface — established flows for other games keep
// running while one game's rules change.
func RenderDropletNATScript(in Infra, entries []*Entry) string {
	up, _ := dropletRules(in, entries)
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, l := range up {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// indentLines renders each line with the given directive prefix (e.g.
// "PostUp = "). Lines that are WireGuard comments (start with '#') are
// emitted as bare comment lines instead: prefixing them with "PostUp = "
// made wg-quick run them via `bash -c` as a shell no-op, which works but
// litters the live config. We keep only the leading whitespace of the
// prefix so the annotation stays valid inside the gateway YAML block
// scalar (a '#' line is a valid wg.conf comment in either renderer).
func indentLines(prefix string, lines []string) string {
	indent := prefix[:len(prefix)-len(strings.TrimLeft(prefix, " \t"))]
	var b strings.Builder
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			b.WriteString(indent)
		} else {
			b.WriteString(prefix)
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// RenderDropletWG0 builds the full /etc/wireguard/wg0.conf for the droplet.
func RenderDropletWG0(in Infra, entries []*Entry) string {
	up, down := dropletRules(in, entries)
	var b strings.Builder
	b.WriteString("# RENDERED BY proxyctl — review before applying. Do not hand-edit on\n")
	b.WriteString("# the droplet; change entries in proxyctl and re-render instead.\n")
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "Address = %s/24\n", in.DropletWGIP)
	fmt.Fprintf(&b, "ListenPort = %d\n", in.WGPort)
	b.WriteString("# PrivateKey lives only in /etc/wireguard/ on the droplet — keep this line\n")
	b.WriteString("# as-is on the box (proxyctl never sees or stores keys).\n")
	b.WriteString("PrivateKey = __DROPLET_PRIVATE_KEY__\n")
	b.WriteString(indentLines("PostUp = ", up))
	b.WriteString(indentLines("PostDown = ", down))
	// One [Peer] per game: each isolated gateway pod dials out to us with
	// its own self-generated key, pinned to its own /32. The applier
	// normally adds/updates these LIVE via `wg set` (no interface bounce);
	// this rendered file is the faithful full-rebuild / review artifact.
	for _, e := range entries {
		if e.TunnelIP == "" {
			continue
		}
		pk := e.GatewayPubKey
		if pk == "" {
			pk = "__PUBKEY_" + e.Slug() + "__ # filled after the pod self-generates"
		}
		b.WriteString("\n[Peer]\n")
		fmt.Fprintf(&b, "# %s gateway (%s)\n", e.Name, e.TunnelIP)
		fmt.Fprintf(&b, "PublicKey = %s\n", pk)
		fmt.Fprintf(&b, "AllowedIPs = %s/32\n", e.TunnelIP)
		b.WriteString("PersistentKeepalive = 25\n")
	}
	// The control tunnel: ProxyCTL's own dedicated peer, present only once
	// the operator has generated it a keypair. Scoped to its own /32 like
	// every other peer here — it gets no more of the subnet than a game
	// gateway would.
	if in.ControlPubKey != "" {
		b.WriteString("\n[Peer]\n")
		b.WriteString("# proxyctl control tunnel (SSH management — see controlwg.go)\n")
		fmt.Fprintf(&b, "PublicKey = %s\n", in.ControlPubKey)
		fmt.Fprintf(&b, "AllowedIPs = %s/32\n", controlTunnelIP)
		b.WriteString("PersistentKeepalive = 25\n")
	}
	// The operator's own personal-access peers: one per generated device
	// (see server/personalaccess.go). Each scoped to its own /32 exactly
	// like every other peer — this grants reaching the droplet over the
	// tunnel, nothing on the rest of 10.8.0.0/24.
	for _, p := range in.PersonalAccessPeers {
		b.WriteString("\n[Peer]\n")
		fmt.Fprintf(&b, "# operator personal access: %s (SSH — see personalaccess.go)\n", p.Name)
		fmt.Fprintf(&b, "PublicKey = %s\n", p.PubKey)
		fmt.Fprintf(&b, "AllowedIPs = %s/32\n", p.IP)
		b.WriteString("PersistentKeepalive = 25\n")
	}
	return b.String()
}

// RenderControlTunnelWG0 renders ProxyCTL's OWN client-side wg0.conf for
// its dedicated control tunnel to the droplet: dial out to the droplet's
// public IP just like a per-game gateway does, but scoped to reach ONLY
// the droplet's tunnel address (in.DropletWGIP/32) — this interface exists
// solely to give SSH a path to the droplet that survives the operator's
// home IP changing, never to reach anything else on 10.8.0.0/24.
//
// privateKey is ProxyCTL's own control-tunnel private key (see
// controlwg.go — never persisted anywhere but that store's 0600 file).
// The rendered text is written to an emptyDir shared with the control-wg
// sidecar container in ProxyCTL's own pod (k8s/proxyctl.yaml); it never
// touches the ProxyCTL data PVC.
func RenderControlTunnelWG0(in Infra, privateKey string) string {
	var b strings.Builder
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "Address = %s/32\n", controlTunnelIP)
	fmt.Fprintf(&b, "PrivateKey = %s\n", privateKey)
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", in.DropletPubKey)
	fmt.Fprintf(&b, "Endpoint = %s:%d\n", in.DropletPublicIP, in.WGPort)
	fmt.Fprintf(&b, "AllowedIPs = %s/32\n", in.DropletWGIP)
	b.WriteString("PersistentKeepalive = 25\n")
	return b.String()
}

// RenderPersonalAccessWG0 renders the operator's OWN client config for one
// personal-access device: a normal WireGuard client profile (import into
// the WireGuard app on a laptop/phone) that dials the droplet's public IP
// directly, scoped to reach ONLY the droplet's tunnel address — same
// shape and same scoping as RenderControlTunnelWG0, just for a human
// instead of ProxyCTL's own sidecar. privateKey is generated fresh each
// time a device is added and never persisted (see personalaccess.go) —
// this rendered text IS the one and only copy the operator gets. ip is
// that device's own allocated address (see allocPersonalAccessIP).
func RenderPersonalAccessWG0(in Infra, privateKey, ip string) string {
	var b strings.Builder
	b.WriteString("# ProxyCTL personal access — import into WireGuard on your device.\n")
	b.WriteString("# Reaches ONLY the droplet (for SSH); nothing else on this network.\n")
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "Address = %s/32\n", ip)
	fmt.Fprintf(&b, "PrivateKey = %s\n", privateKey)
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", in.DropletPubKey)
	fmt.Fprintf(&b, "Endpoint = %s:%d\n", in.DropletPublicIP, in.WGPort)
	fmt.Fprintf(&b, "AllowedIPs = %s/32\n", in.DropletWGIP)
	b.WriteString("PersistentKeepalive = 25\n")
	return b.String()
}

// gatewayRulesForEntry renders the in-cluster gateway PostUp/PostDown for
// ONE game's pod: tunnel traffic to this entry's tunnel IP is DNATed to its
// target ClusterIP, MASQUERADEd back out wg0, and FORWARD is locked so the
// flow can ONLY reach this one target (no lateral movement). One pod = one
// game, so the lockdown is naturally per-game.
func gatewayRulesForEntry(in Infra, e *Entry) (postUp, postDown []string) {
	var up, down []string
	tip := e.TunnelIP
	if tp := e.tcpPorts(); len(tp) > 0 {
		mp := multiport(tp)
		up = append(up,
			fmt.Sprintf("# %s tcp -> %s", e.Name, e.TargetIP),
			fmt.Sprintf("iptables -t nat -A PREROUTING -d %s -p tcp -m multiport --dports %s -j DNAT --to-destination %s", tip, mp, e.TargetIP),
			fmt.Sprintf("iptables -A FORWARD -i wg0 -o eth0 -d %s -p tcp -m multiport --dports %s -j ACCEPT", e.TargetIP, mp),
		)
	}
	if pu := e.udpPorts(); len(pu) > 0 {
		mp := multiport(pu)
		up = append(up,
			fmt.Sprintf("# %s udp -> %s", e.Name, e.TargetIP),
			fmt.Sprintf("iptables -t nat -A PREROUTING -d %s -p udp -m multiport --dports %s -j DNAT --to-destination %s", tip, mp, e.TargetIP),
			fmt.Sprintf("iptables -A FORWARD -i wg0 -o eth0 -d %s -p udp -m multiport --dports %s -j ACCEPT", e.TargetIP, mp),
		)
	}
	up = append(up,
		fmt.Sprintf("iptables -t nat -A POSTROUTING -d %s -j MASQUERADE", e.TargetIP),
		"iptables -A FORWARD -i eth0 -o wg0 -m state --state ESTABLISHED,RELATED -j ACCEPT",
		"iptables -A FORWARD -j DROP",
	)
	down = append(down, "iptables -D FORWARD -j DROP || true")
	return up, down
}

// renderGatewayManifest emits ONE game's isolated gateway: a per-game
// Secret (wg0.conf TEMPLATE — no private key in it), a small PVC that
// persists the key across pod restarts, and a Deployment whose init
// containers (1) raise sysctls and (2) self-generate a WireGuard keypair
// on the PVC if absent, then assemble the live wg0.conf by splicing the
// private key into the template. proxyctl NEVER sees the private key —
// it only later reads back the public key. wg-gw-<slug> per entry, so
// adding/removing one game never disturbs the others.
func renderGatewayManifest(in Infra, e *Entry) string {
	up, down := gatewayRulesForEntry(in, e)
	name := "wg-gw-" + e.Slug()
	var b strings.Builder
	fmt.Fprintf(&b, "# ===== %s (%s) — isolated per-game gateway =====\n", e.Name, e.TunnelIP)

	// Secret: the wg0.conf TEMPLATE. __PRIVATEKEY__ is filled in-pod from
	// the PVC by the keygen init — it is never present in the Secret.
	b.WriteString("apiVersion: v1\nkind: Secret\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n  namespace: %s\n", name, in.K8sNamespace)
	fmt.Fprintf(&b, "  labels: { app: %s, proxyctl: gateway }\n", name)
	// Build the wg0.tmpl body once so we can both embed it and hash it.
	var tb strings.Builder
	tb.WriteString("    [Interface]\n")
	fmt.Fprintf(&tb, "    Address = %s/32\n", e.TunnelIP)
	tb.WriteString("    PrivateKey = __PRIVATEKEY__\n")
	tb.WriteString(indentLines("    PostUp = ", up))
	tb.WriteString(indentLines("    PostDown = ", down))
	tb.WriteString("\n    [Peer]\n")
	fmt.Fprintf(&tb, "    PublicKey = %s\n", in.DropletPubKey)
	fmt.Fprintf(&tb, "    Endpoint = %s:%d\n", in.DropletPublicIP, in.WGPort)
	fmt.Fprintf(&tb, "    AllowedIPs = %s/32\n", in.DropletWGIP)
	tb.WriteString("    PersistentKeepalive = 25\n")
	tmpl := tb.String()
	sum := sha256.Sum256([]byte(tmpl))
	cfgHash := hex.EncodeToString(sum[:])[:16]
	b.WriteString("stringData:\n  wg0.tmpl: |\n")
	b.WriteString(tmpl)
	b.WriteString("---\n")

	// PVC: persists the self-generated keypair across pod restarts so the
	// droplet peer stays valid (no re-key churn on reschedule). Pinned to
	// NFS — NOT Longhorn — to keep proxyctl off the Longhorn replica-sync
	// path that overloads the mini cluster.
	//
	// Share mode has no per-gateway PVC: every gateway shares the one RWX
	// claim on the named export and separates via subPath (see below).
	if !in.keysShareMode() {
		b.WriteString("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n")
		fmt.Fprintf(&b, "  name: %s-keys\n  namespace: %s\n", name, in.K8sNamespace)
		fmt.Fprintf(&b, "  labels: { app: %s, proxyctl: gateway }\n", name)
		fmt.Fprintf(&b, "spec:\n  storageClassName: %s\n  accessModes: [ReadWriteOnce]\n  resources: { requests: { storage: 8Mi } }\n", keysSCName(in.KeysBasePath))
		b.WriteString("---\n")
	}

	// Deployment.
	b.WriteString("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n  namespace: %s\n", name, in.K8sNamespace)
	fmt.Fprintf(&b, "  labels: { app: %s, proxyctl: gateway }\n", name)
	b.WriteString("spec:\n  replicas: 1\n  strategy: { type: Recreate }\n")
	fmt.Fprintf(&b, "  selector: { matchLabels: { app: %s } }\n", name)
	// config-hash annotation: any change to this game's rendered config
	// (ports/target/tunnel IP/peer) changes the hash → kubectl apply
	// recreates ONLY this game's pod so the edit takes effect, while
	// every other game's pod is left running untouched.
	fmt.Fprintf(&b, "  template:\n    metadata:\n      labels: { app: %s }\n", name)
	fmt.Fprintf(&b, "      annotations: { proxyctl/config-hash: \"%s\" }\n", cfgHash)
	b.WriteString("    spec:\n      automountServiceAccountToken: false\n")
	b.WriteString("      initContainers:\n")
	// 1. sysctls (privileged): forwarding + conntrack timeouts so idle
	//    game UDP flows aren't dropped (Satisfactory 8888 etc.).
	b.WriteString("        - name: sysctl\n          image: busybox:1.36\n")
	b.WriteString("          command: [\"sh\",\"-c\",\"sysctl -w net.ipv4.ip_forward=1 && sysctl -w net.ipv4.conf.all.rp_filter=2 && sysctl -w net.netfilter.nf_conntrack_udp_timeout=180 && sysctl -w net.netfilter.nf_conntrack_udp_timeout_stream=600\"]\n")
	b.WriteString("          securityContext: { privileged: true }\n")
	// 2. keygen (wg image): self-generate the keypair on the PVC if
	//    absent, expose the PUBLIC key, then splice the PRIVATE key into
	//    the wg0.conf the main container will bring up. Private key never
	//    leaves the pod; proxyctl only ever reads /keys/publickey back.
	fmt.Fprintf(&b, "        - name: keygen\n          image: %s\n", in.WGImageDigest)
	b.WriteString("          command: [\"sh\",\"-c\",\"set -e; umask 077; ")
	b.WriteString("if [ ! -s /keys/privatekey ]; then wg genkey > /keys/privatekey; fi; ")
	b.WriteString("wg pubkey < /keys/privatekey > /keys/publickey; ")
	b.WriteString("mkdir -p /wg; sed \\\"s|__PRIVATEKEY__|$(cat /keys/privatekey)|\\\" /tmpl/wg0.tmpl > /wg/wg0.conf; chmod 600 /wg/wg0.conf\"]\n")
	b.WriteString("          volumeMounts:\n")
	fmt.Fprintf(&b, "            %s\n", keysMountLine(in, name, false))
	b.WriteString("            - { name: tmpl, mountPath: /tmpl }\n")
	b.WriteString("            - { name: wgconf, mountPath: /wg }\n")
	b.WriteString("      containers:\n        - name: wireguard\n")
	fmt.Fprintf(&b, "          image: %s\n", in.WGImageDigest)
	b.WriteString("          securityContext: { capabilities: { add: [\"NET_ADMIN\"] } }\n")
	b.WriteString("          env:\n            - { name: PUID, value: \"1000\" }\n")
	b.WriteString("            - { name: PGID, value: \"1000\" }\n            - { name: TZ, value: \"UTC\" }\n")
	b.WriteString("          volumeMounts:\n            - { name: config, mountPath: /config }\n")
	b.WriteString("            - { name: wgconf, mountPath: /config/wg_confs }\n")
	fmt.Fprintf(&b, "            %s\n", keysMountLine(in, name, true))
	b.WriteString("            - { name: modules, mountPath: /lib/modules, readOnly: true }\n")
	b.WriteString("          readinessProbe:\n")
	b.WriteString("            exec: { command: [\"sh\",\"-c\",\"wg show wg0 >/dev/null\"] }\n")
	b.WriteString("            initialDelaySeconds: 12\n            periodSeconds: 15\n")
	b.WriteString("      volumes:\n        - { name: config, emptyDir: {} }\n")
	b.WriteString("        - { name: wgconf, emptyDir: {} }\n")
	b.WriteString("        - { name: modules, hostPath: { path: /lib/modules } }\n")
	fmt.Fprintf(&b, "        - { name: tmpl, secret: { secretName: %s, items: [ { key: wg0.tmpl, path: wg0.tmpl } ] } }\n", name)
	fmt.Fprintf(&b, "        %s\n", keysVolumeLine(in, name))
	return b.String()
}

// keysVolumeLine renders a gateway's keys volume. Share mode mounts the
// operator's export INLINE (volumes.nfs) — no PV, no PVC, no StorageClass,
// nothing cluster-scoped to create; the per-gateway dir is carved by the
// mount's subPath (see keysMountLine). Provisioner mode keeps the classic
// per-gateway PVC on the derived StorageClass.
func keysVolumeLine(in Infra, gwName string) string {
	if in.keysShareMode() {
		return fmt.Sprintf("- { name: keys, nfs: { server: %s, path: %s } }",
			in.KeysNFSServer, in.KeysNFSExport)
	}
	return fmt.Sprintf("- { name: keys, persistentVolumeClaim: { claimName: %s-keys } }", gwName)
}

// keysMountLine renders a gateway's /keys volumeMount. In share mode the one
// shared claim is carved per-gateway with subPath — which also makes kubelet
// create the dir, the thing a static NFS PV can't do for itself.
func keysMountLine(in Infra, gwName string, readOnly bool) string {
	s := "- { name: keys, mountPath: /keys"
	if in.keysShareMode() {
		s += ", subPath: " + keysSubPath(in.KeysBasePath, in.K8sNamespace, gwName)
	}
	if readOnly {
		s += ", readOnly: true"
	}
	return s + " }"
}

// RenderGatewayManifests concatenates the isolated per-game gateway
// manifests for every enabled entry.
func RenderGatewayManifests(in Infra, entries []*Entry) string {
	en := []*Entry{}
	for _, e := range entries {
		// Egress entries have no wg-gw pod: their WireGuard peer is a
		// sidecar inside the game pod, managed by the orchestrator.
		if e.Enabled && e.TunnelIP != "" && !e.IsEgress() {
			en = append(en, e)
		}
	}
	if len(en) == 0 {
		return "# (no enabled entries — no per-game gateways to render)\n"
	}
	var b strings.Builder
	b.WriteString("# RENDERED BY proxyctl — one isolated gateway per game.\n")
	b.WriteString("# Each pod self-generates its WireGuard key (PVC-persisted);\n")
	b.WriteString("# proxyctl never sees a private key. kubectl apply per game.\n")
	for i, e := range en {
		if i > 0 {
			b.WriteString("---\n")
		}
		b.WriteString(renderGatewayManifest(in, e))
	}
	return b.String()
}

// RenderSummary is a human map of which port hits which target.
func RenderSummary(in Infra, entries []*Entry) string {
	var b strings.Builder
	if len(entries) == 0 {
		return "(no enabled entries — rendered configs carry base infra only)\n"
	}
	for _, e := range entries {
		specs := make([]string, len(e.Ports))
		for i, p := range e.Ports {
			specs[i] = fmt.Sprintf("%d/%s", p.Port, p.Proto)
		}
		fmt.Fprintf(&b, "%-20s %s\n", e.Name, e.Subdomain)
		if e.IsEgress() {
			fmt.Fprintf(&b, "  %s:%s  <->  tunnel  <->  game pod %s (egress: outbound exits via droplet)",
				in.DropletPublicIP, strings.Join(specs, ","), e.TunnelIP)
		} else {
			fmt.Fprintf(&b, "  %s:%s  ->  tunnel  ->  gateway  ->  %s",
				in.DropletPublicIP, strings.Join(specs, ","), e.TargetIP)
		}
		if e.Service != "" {
			fmt.Fprintf(&b, "  (%s)", e.Service)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RenderDNSGuide tells the operator exactly which DNS records to create so
// players can reach each game by name. Every name points at the one droplet
// IP — routing is by PORT (the DNS name is a friendly alias, not a routing
// key), so each game needs a unique public port. The Cloudflare proxy
// (orange cloud) only handles HTTP/HTTPS; game UDP/TCP MUST be a plain
// "DNS only" (grey-cloud) A record or it will not connect.
func RenderDNSGuide(in Infra, entries []*Entry) string {
	var b strings.Builder
	b.WriteString("Create these in your DNS provider. Cloudflare: set Proxy\n")
	b.WriteString("status = \"DNS only\" (grey cloud) — game traffic is not HTTP\n")
	b.WriteString("and will NOT pass through the orange-cloud proxy.\n\n")
	fmt.Fprintf(&b, "%-6s %-32s %-16s %s\n", "TYPE", "NAME", "VALUE", "PROXY")
	named := false
	for _, e := range entries {
		if strings.TrimSpace(e.Subdomain) == "" {
			continue
		}
		named = true
		specs := make([]string, len(e.Ports))
		for i, p := range e.Ports {
			specs[i] = strconv.Itoa(p.Port)
		}
		fmt.Fprintf(&b, "%-6s %-32s %-16s %s\n", "A", e.Subdomain, in.DropletPublicIP, "DNS only")
		fmt.Fprintf(&b, "   players connect to:  %s:%s\n\n",
			e.Subdomain, strings.Join(specs, ","))
	}
	if !named {
		b.WriteString("(no entries have a DNS name yet — set the DNS label on an\n")
		b.WriteString(" entry to get its exact record here)\n\n")
	}
	fmt.Fprintf(&b, "All names point at the same droplet IP (%s). Routing is by\n", in.DropletPublicIP)
	b.WriteString("PORT, so every game needs a unique public port; the DNS name\n")
	b.WriteString("is a friendly alias, not a routing key. Subdomains can't be\n")
	b.WriteString("auto-discovered via DNS — they're managed per entry here.\n")
	return b.String()
}

// Render produces the full review bundle for the enabled entries.
func Render(in Infra, all []*Entry) Rendered {
	en := enabled(all)
	r := Rendered{
		DropletWG0Conf: RenderDropletWG0(in, en),
		GatewayYAML:    RenderGatewayManifests(in, en),
		Summary:        RenderSummary(in, en),
		DNSGuide:       RenderDNSGuide(in, en),
	}
	r.ApplyScript = renderApplyScript(in, r)
	return r
}

func renderApplyScript(in Infra, r Rendered) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
# proxyctl apply runbook (v1: ManualApplier — you run these by hand).
# proxyctl holds NO ssh key and NO kubeconfig; nothing here is automated.
# Review the two rendered configs first, then:
set -euo pipefail

# 1. DROPLET — drop in the new wg0.conf and restart WireGuard.
#    Copy the "droplet wg0.conf" panel into /tmp/wg0.conf locally, then:
scp /tmp/wg0.conf root@%s:/etc/wireguard/wg0.conf
ssh root@%s 'systemctl restart wg-quick@wg0 && wg show wg0'
#    NOTE: keep the real PrivateKey line in /etc/wireguard/wg0.conf on the
#    droplet — the rendered file uses a __DROPLET_PRIVATE_KEY__ placeholder.

# 2. CLUSTER — apply the wg-gateway Secret+Deployment.
#    Copy the "gateway manifest" panel into /tmp/wg-gateway.yaml, replace
#    __GATEWAY_PRIVATE_KEY__ with the existing key:
#      kubectl -n %s get secret wg-gateway -o jsonpath='{.data.wg0\.conf}' \
#        | base64 -d | grep PrivateKey
kubectl apply -f /tmp/wg-gateway.yaml
kubectl -n %s rollout status deploy/wg-gateway

# 3. Verify a player port end-to-end (handshake + a connect).
echo "done — verify in-game"
`, in.DropletPublicIP, in.DropletPublicIP, in.K8sNamespace, in.K8sNamespace)
}
