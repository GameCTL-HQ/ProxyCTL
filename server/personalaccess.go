package main

// personalaccess.go — lets one or more humans SSH into the droplet over
// the same resilient WireGuard tunnel ProxyCTL's own control tunnel uses,
// instead of losing all direct access once SSH is restricted to
// tunnel-only (see restrictSSHToTunnel in main.go).
//
// Each named device (laptop, phone, ...) gets two independent things:
//   - Its own WireGuard peer, with its own reserved address (see
//     personalAccessIPLow/High in render.go). ProxyCTL generates this
//     keypair and hands the private half back ONCE as a downloadable
//     client config — never stored anywhere. Losing the file means
//     adding a fresh device (a new peer; the old one must be separately
//     revoked), same posture as Regenerate() for the droplet's own key.
//   - Its own SSH key, but BYOK: the operator generates that keypair
//     locally (ssh-keygen) and pastes only the PUBLIC half in, same as
//     adding a key to GitHub. ProxyCTL only ever appends that public key
//     to authorized_keys — a private key never exists on ProxyCTL or the
//     droplet, let alone transits the network, for this half at all.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// genWGKeypair generates a one-shot WireGuard keypair via wireguard-tools
// (bundled in the image) — same mechanism as ControlWGStore, just never
// persisted afterward.
func genWGKeypair() (priv, pub string, err error) {
	out, err := exec.Command("wg", "genkey").Output()
	if err != nil {
		return "", "", fmt.Errorf("wg genkey: %w", err)
	}
	priv = strings.TrimSpace(string(out))
	cmd := exec.Command("wg", "pubkey")
	cmd.Stdin = strings.NewReader(priv)
	pubOut, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("wg pubkey: %w", err)
	}
	return priv, strings.TrimSpace(string(pubOut)), nil
}

// sshPubKeyTypes are the OpenSSH public-key line prefixes validateSSHPubKey
// accepts — anything else (including a private key pasted by mistake) is
// rejected before it ever reaches the droplet.
var sshPubKeyTypes = []string{
	"ssh-ed25519 ", "ssh-rsa ", "ssh-dss ",
	"ecdsa-sha2-nistp256 ", "ecdsa-sha2-nistp384 ", "ecdsa-sha2-nistp521 ",
}

// validateSSHPubKey checks that s looks like exactly one OpenSSH
// authorized_keys line — the operator generates their OWN keypair locally
// (see the "Add device" copy) and pastes only the public half here; the
// private key never touches ProxyCTL or the droplet at all. Rejects
// anything multi-line (a stray newline could otherwise inject an extra
// authorized_keys entry) or that doesn't start with a recognized key
// type, which also catches someone pasting a private key by mistake.
func validateSSHPubKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("paste this device's SSH public key (e.g. the contents of id_ed25519.pub)")
	}
	if strings.ContainsAny(s, "\n\r") {
		return "", fmt.Errorf("that looks like more than one line — paste just the single public key line")
	}
	ok := false
	for _, t := range sshPubKeyTypes {
		if strings.HasPrefix(s, t) {
			ok = true
			break
		}
	}
	if !ok {
		return "", fmt.Errorf(`doesn't look like an SSH public key (should start with "ssh-ed25519 ", "ssh-rsa ", etc.) — ` +
			`make sure you pasted the .pub file's contents, not the private key`)
	}
	return s, nil
}

// authorizeSSHKey appends the operator-supplied public key (already
// validated by validateSSHPubKey) to /root/.ssh/authorized_keys.
// Append-only: existing lines (the operator's own keys, ProxyCTL's
// bootstrap key, anything else already there) are never touched.
func authorizeSSHKey(sa SSHApplier, pubKey string) Step {
	remote := "set -e\n" +
		"mkdir -p /root/.ssh && chmod 700 /root/.ssh\n" +
		"touch /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys\n" +
		"printf '%s\\n' " + shq(pubKey) + " >> /root/.ssh/authorized_keys\n"
	return sa.sshDroplet("personal access: authorize SSH key", "", "bash -c "+shq(remote))
}

// deauthorizeSSHKey removes EXACTLY the given authorized_keys line
// (matched as a fixed string, never a pattern) — every other line,
// including the operator's own keys and ProxyCTL's bootstrap key, is left
// untouched. A no-op (not an error) if the file doesn't exist or the line
// is already gone, so revoking a pre-this-feature peer (empty SSHPubKey —
// callers should skip calling this at all in that case) or double-revoking
// can't fail the whole request.
func deauthorizeSSHKey(sa SSHApplier, sshPubKeyLine string) Step {
	remote := "set -e\n" +
		"[ -f /root/.ssh/authorized_keys ] || exit 0\n" +
		"grep -vF -- " + shq(sshPubKeyLine) + " /root/.ssh/authorized_keys > /root/.ssh/authorized_keys.new || true\n" +
		"mv /root/.ssh/authorized_keys.new /root/.ssh/authorized_keys\n" +
		"chmod 600 /root/.ssh/authorized_keys\n"
	return sa.sshDroplet("personal access: remove SSH key from authorized_keys", "", "bash -c "+shq(remote))
}

// allocPersonalAccessIP picks the lowest free address in the reserved
// personal-access range that isn't already used by another named device.
func allocPersonalAccessIP(existing []PersonalAccessPeer) (string, error) {
	taken := map[string]bool{}
	for _, p := range existing {
		taken[p.IP] = true
	}
	for h := personalAccessIPLow; h <= personalAccessIPHigh; h++ {
		ip := fmt.Sprintf("10.8.0.%d", h)
		if !taken[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("personal-access IP pool exhausted (10.8.0.%d-%d) — revoke an unused device first",
		personalAccessIPLow, personalAccessIPHigh)
}

// generatePersonalAccess: POST /api/droplet/personal-access/generate.
// Allocates a fresh address + keypair for a NAMED device, pushes the
// PUBLIC half to the droplet as a new WireGuard peer (pushDropletWG0
// always re-renders the full current peer set from live state, so no
// existing peer — game gateway, control tunnel, or another personal-access
// device — is ever dropped), and — if SSH is already restricted to
// tunnel-only — extends the sshd allow-list to include it too. Returns
// the WireGuard client config exactly once; its private key is never
// stored. The SSH side is BYOK: the operator generates their own keypair
// locally and pastes only the public half in (SSHPubKey in the request) —
// same as adding a key to GitHub — so a private key never exists on
// ProxyCTL or the droplet, let alone transits the network.
func (a *API) generatePersonalAccess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		SSHPubKey string `json:"sshPubKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a name for this device is required (e.g. \"Laptop\")"})
		return
	}
	sshPub, err := validateSSHPubKey(body.SSHPubKey)
	if err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	cfg := a.droplet.Get()
	if !cfg.Configured() {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Droplet not configured."})
		return
	}
	if cfg.WGPublicKey == "" {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": `Droplet not bootstrapped yet — run "Prepare droplet" first.`})
		return
	}
	for _, p := range cfg.PersonalAccessPeers {
		if strings.EqualFold(p.Name, name) {
			a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a device named " + name + " already exists — pick a different name or revoke it first."})
			return
		}
	}
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ssh apply-mode required"})
		return
	}
	sa.Droplet = cfg

	ip, err := allocPersonalAccessIP(cfg.PersonalAccessPeers)
	if err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	priv, pub, err := genWGKeypair()
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "generating WireGuard keypair: " + err.Error()})
		return
	}
	// Authorize the operator's OWN public key before the peer is recorded,
	// so a failure here aborts cleanly with nothing to unwind.
	if st := authorizeSSHKey(sa, sshPub); !st.OK {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "authorizing this device's SSH key: " + strings.TrimSpace(st.Stderr),
		})
		return
	}

	peer := PersonalAccessPeer{Name: name, PubKey: pub, IP: ip, CreatedAt: time.Now().Unix(), SSHPubKey: sshPub}
	if err := a.droplet.AddPersonalAccessPeer(peer); err != nil {
		_ = deauthorizeSSHKey(sa, sshPub) // don't leave an authorized_keys entry for a peer that was never recorded
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.refreshInfra() // picks up the new peer for rendering

	push := sa.pushDropletWG0(a.infra, a.readyEntries())
	if !push.OK {
		_ = a.droplet.RemovePersonalAccessPeer(pub) // don't record a peer that was never actually pushed
		_ = deauthorizeSSHKey(sa, sshPub)           // ...or leave its SSH key authorized either
		a.refreshInfra()
		a.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "step": "push",
			"stdout": push.Stdout, "stderr": push.Stderr,
		})
		return
	}

	cfg = a.droplet.Get() // refreshed with the new peer persisted
	if cfg.SSHTunnelOnly {
		result, applyOK := a.applyTunnelOnlySSHDropin(sa, tunnelAllowedIPs(cfg))
		if !applyOK {
			// The peer exists on the droplet's WireGuard interface, but
			// sshd doesn't allow it yet — say so plainly rather than
			// silently handing out a config that won't actually SSH. The
			// WireGuard config is the only secret ProxyCTL ever holds here
			// (the SSH key was the operator's own, never ProxyCTL's to
			// lose), so it's still worth returning even on this failure path.
			result["warning"] = "The WireGuard peer was added, but updating sshd's allow-list to include it failed " +
				"— see details below. SSH access is unchanged from before this (still safe)."
			result["config"] = RenderPersonalAccessWG0(a.infra, priv, ip)
			result["pubKey"], result["ip"], result["name"], result["sshPubKey"] = pub, ip, name, sshPub
			a.writeJSON(w, http.StatusOK, result)
			return
		}
		if _, fwOK := a.applyTunnelOnlyFirewall(sa, a.infra); !fwOK {
			a.writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "config": RenderPersonalAccessWG0(a.infra, priv, ip), "pubKey": pub, "ip": ip, "name": name, "sshPubKey": sshPub,
				"message": name + " added and can SSH in (sshd allow-list updated + verified). Save the WireGuard config now.",
				"warning": "The network-level firewall gate couldn't be extended to include this device and was reverted — " +
					"sshd's own restriction still covers it, so this isn't urgent, but the extra firewall layer is missing for now.",
			})
			return
		}
	}

	conf := RenderPersonalAccessWG0(a.infra, priv, ip)
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "config": conf, "pubKey": pub, "ip": ip, "name": name, "sshPubKey": sshPub,
		"message": name + " added. Save the WireGuard config now — it's shown only this once.",
	})
}

// revokePersonalAccess: POST /api/droplet/personal-access/revoke (body:
// {"pubKey": "..."}). If SSH is restricted to tunnel-only, tightens
// sshd's allow-list back down FIRST (excluding just this peer, while
// ProxyCTL's own control-tunnel peer and every other remaining device
// still prove the remaining path works) and only then removes the
// WireGuard peer itself — the reverse order would leave a brief window
// where sshd still expects a peer that's already gone.
func (a *API) revokePersonalAccess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PubKey string `json:"pubKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	pubKey := strings.TrimSpace(body.PubKey)
	if pubKey == "" {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pubKey required"})
		return
	}
	cfg := a.droplet.Get()
	if !cfg.Configured() {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Droplet not configured."})
		return
	}
	var target *PersonalAccessPeer
	for i := range cfg.PersonalAccessPeers {
		if cfg.PersonalAccessPeers[i].PubKey == pubKey {
			target = &cfg.PersonalAccessPeers[i]
			break
		}
	}
	if target == nil {
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "No such personal-access peer."})
		return
	}
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ssh apply-mode required"})
		return
	}
	sa.Droplet = cfg

	if cfg.SSHTunnelOnly {
		remaining := []string{controlTunnelIP}
		var remainingPeers []PersonalAccessPeerInfo
		for _, p := range cfg.PersonalAccessPeers {
			if p.PubKey != pubKey {
				remaining = append(remaining, p.IP)
				remainingPeers = append(remainingPeers, PersonalAccessPeerInfo{Name: p.Name, PubKey: p.PubKey, IP: p.IP})
			}
		}
		result, applyOK := a.applyTunnelOnlySSHDropin(sa, remaining)
		if !applyOK {
			a.writeJSON(w, http.StatusOK, result)
			return
		}
		// Rebuild the firewall gate with the REDUCED peer set. a.infra
		// isn't refreshed yet at this point (the peer is only removed
		// from the store just below) — pass a scoped copy rather than
		// mutate shared state before the removal is actually persisted.
		fwInfra := a.infra
		fwInfra.PersonalAccessPeers = remainingPeers
		if _, fwOK := a.applyTunnelOnlyFirewall(sa, fwInfra); !fwOK {
			a.writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "step": "firewall",
				"error": "sshd's allow-list no longer includes this device, but updating the firewall gate failed. " +
					"The device still cannot SSH in (sshd already blocks it) — retry to fully clean up the firewall layer.",
			})
			return
		}
	}

	_ = a.droplet.RemovePersonalAccessPeer(pubKey)
	a.refreshInfra()
	push := sa.pushDropletWG0(a.infra, a.readyEntries())
	msg := "Personal access peer revoked."
	if target.SSHPubKey != "" {
		// Best-effort, same posture as unlockSSH's firewall teardown: the
		// WireGuard peer is already gone by this point (the actual access
		// boundary), so a hiccup removing the now-orphaned authorized_keys
		// line doesn't need to fail the whole revoke — it just means one
		// harmless unused line lingers until a retry cleans it up.
		if dk := deauthorizeSSHKey(sa, target.SSHPubKey); !dk.OK {
			msg += " Its dedicated SSH key's authorized_keys entry couldn't be removed automatically (see stderr) — harmless " +
				"since it can no longer reach sshd at all without the WireGuard peer, but worth retrying to fully clean up."
		} else {
			msg += " Its dedicated SSH key was also de-authorized."
		}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok": push.OK, "stdout": push.Stdout, "stderr": push.Stderr,
		"message": msg,
	})
}
