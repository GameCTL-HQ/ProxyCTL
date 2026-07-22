package main

import (
	"os/exec"
	"strings"
	"testing"
)

func sshAppl(user string) SSHApplier {
	return SSHApplier{Droplet: &DropletConfig{
		IP: "192.0.2.10", User: user,
		PrivateKeyPath: "/tmp/k", PublicKey: "ssh-ed25519 AAAA",
	}}
}

// Root logins (DigitalOcean's default) must stay byte-for-byte as they
// were in v1 — no sudo, no re-quoting.
func TestWrapPrivileged_RootUnchanged(t *testing.T) {
	cmd := "apt-get update; echo 'hi' && wg show"
	for _, user := range []string{"root", ""} {
		if got := sshAppl(user).wrapPrivileged(cmd); got != cmd {
			t.Errorf("user %q: want unchanged %q, got %q", user, cmd, got)
		}
	}
}

// Non-root logins (OVH/AWS/Hetzner) must be elevated.
func TestWrapPrivileged_NonRootElevates(t *testing.T) {
	got := sshAppl("ubuntu").wrapPrivileged("apt-get update")
	want := `sudo -n bash -c 'apt-get update'`
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

// The bug this guards: these commands are compound, so a bare `sudo `
// prefix would elevate only the first segment and run the rest
// unprivileged — silently, since the failure surfaces much later.
// Assert the whole command reaches `bash -c` as ONE argument, quoting
// intact, by having a real shell parse it.
func TestWrapPrivileged_CompoundArrivesAsSingleArg(t *testing.T) {
	cmd := `umask 077; key=$(grep -m1 '^PrivateKey' /etc/wireguard/wg0.conf); echo "$key" && wg show`
	wrapped := sshAppl("ubuntu").wrapPrivileged(cmd)

	// Let bash do the parsing the remote login shell would do, and print
	// each resulting argv on its own line.
	script := strings.Replace(wrapped, "sudo -n ", "", 1)
	script = strings.Replace(script, "bash -c ", `printf '%s\n' `, 1)
	out, err := exec.Command("bash", "-c", script).Output()
	if err != nil {
		t.Fatalf("shell parse failed: %v", err)
	}
	if got := strings.TrimRight(string(out), "\n"); got != cmd {
		t.Errorf("command did not survive quoting as a single arg:\n want: %q\n got:  %q", cmd, got)
	}
}

// controlTarget must stay empty (and every ssh call must therefore keep
// using the public IP) unless the tunnel has been explicitly verified —
// this is the guard that stops a half-configured control tunnel from
// ever becoming the ONLY way in.
func TestControlTarget_EmptyUntilVerified(t *testing.T) {
	sa := sshAppl("root")
	if got := sa.controlTarget(); got != "" {
		t.Errorf("want empty controlTarget before ControlTunnelReady, got %q", got)
	}
	sa.Droplet.ControlTunnelReady = true
	if got, want := sa.controlTarget(), "root@"+dropletControlIP; got != want {
		t.Errorf("want %q once ready, got %q", want, got)
	}
}

// A legacy -droplet-ssh target with no "user@" must not start sudo-ing.
func TestDropletUser_LegacyFlag(t *testing.T) {
	for target, want := range map[string]string{
		"root@192.0.2.10":   "root",
		"ubuntu@192.0.2.10": "ubuntu",
		"192.0.2.10":        "root",
		"":                  "root",
	} {
		if got := (SSHApplier{DropletSSH: target}).dropletUser(); got != want {
			t.Errorf("target %q: want user %q, got %q", target, want, got)
		}
	}
}
