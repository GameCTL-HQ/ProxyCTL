package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// extractWGPrivateKey pulls the PrivateKey value out of a wg0.conf. The
// input is the base64 the kube Secret stores under data."wg0.conf"; if it
// is not valid base64 it is treated as already-decoded text.
func extractWGPrivateKey(secretData string) string {
	conf := strings.TrimSpace(secretData)
	if dec, err := base64.StdEncoding.DecodeString(conf); err == nil {
		conf = string(dec)
	}
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PrivateKey") {
			if i := strings.Index(line, "="); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

// Applier is the seam between "proxyctl computed the desired state" and
// "that state reaches the droplet + cluster". The store, API and web UI
// never change when the apply *mechanism* changes — only the chosen Applier.
//
// SECURITY MODEL (important): proxyctl holds ZERO standing credentials. It
// stores no SSH private key and no kubeconfig of its own and persists
// nothing. The SSHApplier borrows the OPERATOR'S AMBIENT credentials at
// click-time — the ssh-agent / ~/.ssh / kubeconfig already present in the
// process environment of whoever launched proxyctl. The admin UI itself
// stays loopback-bound + token-protected (auth.go), so "click Apply" can
// only ever be triggered by someone who already tunnelled in as the
// operator. Nothing is automated in the background.
//
// Implementations:
//   - SSHApplier   — v1 PRIMARY. Shells out to system `ssh` + `kubectl`
//     using ambient creds. Surfaces stdout/stderr/exit in the UI.
//   - ManualApplier — fallback. Render + runbook only, never executes.
type Applier interface {
	// Render computes the review bundle (always safe, no side effects).
	Render(in Infra, entries []*Entry) Rendered
	// Apply pushes the rendered state to the droplet + cluster. For
	// ManualApplier this is a no-op that just echoes the runbook.
	Apply(in Infra, r Rendered) ApplyResult
	// Mode is a short identifier shown in the UI ("ssh" / "manual").
	Mode() string
}

// ApplyResult is what the API hands back to the UI.
type ApplyResult struct {
	Mode      string   `json:"mode"`      // matches Applier.Mode()
	Automated bool     `json:"automated"` // true if Apply actually executed
	OK        bool     `json:"ok"`        // overall success
	Message   string   `json:"message"`   // human status / next step
	Steps     []Step   `json:"steps"`     // per-command output (ssh mode)
	Rendered  Rendered `json:"rendered"`  // the configs to review/apply
}

// Step is one executed command's result, surfaced verbatim in the UI.
type Step struct {
	Name     string `json:"name"`
	Cmd      string `json:"cmd"`
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	OK       bool   `json:"ok"`
}

// ---- ManualApplier: render-only, never executes, zero credentials. ----

type ManualApplier struct{}

func (ManualApplier) Mode() string { return "manual" }

func (ManualApplier) Render(in Infra, entries []*Entry) Rendered {
	return Render(in, entries)
}

func (ManualApplier) Apply(in Infra, r Rendered) ApplyResult {
	return ApplyResult{
		Mode:      "manual",
		Automated: false,
		OK:        true,
		Message: "Manual mode: proxyctl executed nothing. Review the droplet " +
			"wg0.conf and gateway manifest, then run the apply script by hand.",
		Rendered: r,
	}
}

// ---- SSHApplier: v1 primary. Borrows the operator's ambient ssh/kubectl
// at click-time; stores nothing. ----

type SSHApplier struct {
	DropletSSH  string // user@host for the droplet, e.g. root@203.0.113.10 (legacy flag fallback)
	KubeContext string // kubectl --context (empty = current context)
	Timeout     time.Duration

	// Droplet, when configured by the setup wizard, takes precedence over
	// DropletSSH: ssh uses its OWN -i <key>, -p <port>, user, with a
	// dedicated known_hosts so first-run TOFU stays in proxyCTL's data
	// dir instead of polluting the operator's. Nil = legacy ambient path.
	Droplet *DropletConfig
}

// dropletTarget returns the effective user@host the ssh applier should
// hit, preferring the stored wizard config over the legacy CLI flag.
func (s SSHApplier) dropletTarget() string {
	if s.Droplet != nil && s.Droplet.Configured() {
		return s.Droplet.target()
	}
	return s.DropletSSH
}

// dropletReady is true when EITHER the wizard has configured a droplet
// OR the operator supplied -droplet-ssh. The placeholder is treated as
// unset.
func (s SSHApplier) dropletReady() bool {
	if s.Droplet != nil && s.Droplet.Configured() {
		return true
	}
	return s.DropletSSH != "" && !strings.Contains(s.DropletSSH, "PLACEHOLDER")
}

// dropletUser returns the effective SSH login user, mirroring
// dropletTarget's precedence (wizard config over the legacy flag). A
// legacy target with no "user@" prefix reports "root" so its behaviour
// is unchanged.
func (s SSHApplier) dropletUser() string {
	if s.Droplet != nil && s.Droplet.Configured() {
		return s.Droplet.user()
	}
	if u, _, ok := strings.Cut(s.DropletSSH, "@"); ok {
		if u = strings.TrimSpace(u); u != "" {
			return u
		}
	}
	return "root"
}

// wrapPrivileged elevates a droplet-bound remote command when the login
// user isn't already root.
//
// Every remote command here writes /etc, installs packages, or drives
// systemd — i.e. all of them need root. DigitalOcean images log you in
// as root, so v1 wrote them unprefixed. Other providers (OVH, AWS,
// Hetzner) hand you an unprivileged user with passwordless sudo instead,
// where bootstrap dies on the first apt-get with "could not get lock
// /var/lib/dpkg/lock-frontend (13: Permission denied)".
//
// The command is re-wrapped rather than prefixed because these are
// compound ("a; b && c") — a bare `sudo ` would elevate only the first
// segment and silently run the rest unprivileged. -n keeps it
// non-interactive so a box without NOPASSWD fails loudly instead of
// hanging on a password prompt no one can answer. sudo passes stdin
// through, so the `bash -s` steps still read their piped script.
func (s SSHApplier) wrapPrivileged(remote string) string {
	if s.dropletUser() == "root" {
		return remote
	}
	return "sudo -n bash -c " + shq(remote)
}

// sshDropletArgs builds the argv every droplet-bound ssh funnels through.
// With a wizard config it uses -i <key>, a proxyCTL-owned known_hosts,
// and IdentitiesOnly to refuse the agent. Without, it falls back to -o
// BatchMode=yes against the legacy flag target. Privilege is the
// caller's business — see sshDroplet vs sshDropletRaw.
func (s SSHApplier) sshDropletArgs(remote string) []string {
	var args []string
	if s.Droplet != nil && s.Droplet.Configured() {
		args = append(args, s.Droplet.sshArgs()...)
		args = append(args, s.Droplet.target(), remote)
	} else {
		args = append(args, "-o", "BatchMode=yes", s.DropletSSH, remote)
	}
	return args
}

// sshDroplet runs a droplet-bound command with root privileges (see
// wrapPrivileged). This is the default because every real droplet
// command needs root.
func (s SSHApplier) sshDroplet(name, stdin, remote string) Step {
	return s.run(name, "ssh", stdin, s.sshDropletArgs(s.wrapPrivileged(remote))...)
}

// sshDropletRaw runs a command as the login user, WITHOUT elevating.
// Only for probing the box's own state — e.g. the Test step, which has
// to report the real login user and whether root is actually reachable.
func (s SSHApplier) sshDropletRaw(name, stdin, remote string) Step {
	return s.run(name, "ssh", stdin, s.sshDropletArgs(remote)...)
}

// sshDropletLong runs a single droplet-bound ssh command with a custom
// (typically longer) timeout — for bootstrap, which can spend minutes
// inside apt-get install on a fresh box.
func (s SSHApplier) sshDropletLong(name, stdin, remote string, timeout time.Duration) Step {
	long := s
	long.Timeout = timeout
	return long.run(name, "ssh", stdin, long.sshDropletArgs(long.wrapPrivileged(remote))...)
}

func (SSHApplier) Mode() string { return "ssh" }

func (s SSHApplier) Render(in Infra, entries []*Entry) Rendered {
	return Render(in, entries)
}

// run executes a command with a hard timeout and captures everything for the
// UI. It inherits the process environment on purpose — that is how the
// operator's ambient ssh-agent / kubeconfig is "borrowed" without proxyctl
// ever holding a credential.
func (s SSHApplier) run(name, bin string, stdin string, args ...string) Step {
	to := s.Timeout
	if to == 0 {
		to = 90 * time.Second
	}
	st := Step{Name: name, Cmd: bin + " " + strings.Join(args, " ")}
	cmd := exec.Command(bin, args...)
	cmd.Env = os.Environ()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		st.Stderr = err.Error()
		st.ExitCode = -1
		return st
	}
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(to):
		_ = cmd.Process.Kill()
		st.Stderr = fmt.Sprintf("timed out after %s", to)
		st.ExitCode = -1
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			st.ExitCode = ee.ExitCode()
		} else if err != nil {
			st.ExitCode = -1
			errb.WriteString(err.Error())
		}
	}
	st.Stdout = out.String()
	if st.Stderr == "" {
		st.Stderr = errb.String()
	}
	st.OK = st.ExitCode == 0
	return st
}

// ApplyPerGame is the isolated, incremental data-plane apply. Each enabled
// entry gets its own gateway pod that SELF-GENERATES its WireGuard key; we
// read back only the PUBLIC key, persist it via `persist`, then reconcile
// the droplet peers LIVE with `wg syncconf` (no interface bounce) so adding,
// changing, or removing one game never disturbs the others. Removed games'
// gateways are deleted. proxyctl never sees a private key.
func (s SSHApplier) ApplyPerGame(in Infra, entries []*Entry, persist func(id, pubkey string), onStep func(Step)) ApplyResult {
	res := ApplyResult{Mode: "ssh", Automated: true}
	// add records a completed step AND streams it to the live job so a
	// polling/refreshed UI sees progress as it happens.
	add := func(st Step) Step {
		res.Steps = append(res.Steps, st)
		if onStep != nil {
			onStep(st)
		}
		return st
	}
	if !s.dropletReady() {
		res.OK = false
		res.Message = "Droplet not configured — open Setup → Droplet to install proxyCTL's SSH key on the droplet, then Apply."
		return res
	}
	ns := in.K8sNamespace
	kctx := func(a ...string) []string {
		if s.KubeContext != "" {
			return append([]string{"--context", s.KubeContext}, a...)
		}
		return a
	}
	var enabled []*Entry
	for _, e := range entries {
		if e.Enabled && e.TunnelIP != "" {
			enabled = append(enabled, e)
		}
	}

	// Phase 0: make sure the keys storage the gateways will claim exists.
	// Share mode needs NOTHING provisioned: gateways mount the operator's
	// export inline (volumes.nfs + subPath) — no PV, no PVC, no
	// StorageClass, so there is no cluster-scoped object to create and no
	// RBAC beyond the namespaced writes the SA already has.
	if !in.keysShareMode() {
		// Provisioner mode: ensure the keys StorageClass for the configured
		// base folder exists (create-if-missing). Each gateway's key PVC binds
		// via this class, whose pathPattern nests the dir under KeysBasePath.
		// A pathPattern is fixed at creation, so a changed base folder is a new
		// class name — created here on the next apply; existing gateways move
		// via the Setup → keys "move" action (delete PVC + re-key).
		scName := keysSCName(in.KeysBasePath)
		if chk := s.run("keys StorageClass: check "+scName, "kubectl", "",
			kctx("get", "storageclass", scName)...); !chk.OK {
			add(s.run("keys StorageClass: create "+scName, "kubectl",
				renderKeysStorageClass(in.KeysBasePath), kctx("create", "-f", "-")...))
		}
	}

	// Phase 1: apply each game's isolated gateway (Secret tmpl + PVC +
	// Deployment). Independent objects — one failing doesn't touch others.
	for _, e := range enabled {
		add(s.run("game "+e.Name+": kubectl apply gateway",
			"kubectl", renderGatewayManifest(in, e), kctx("apply", "-f", "-")...))
	}
	// Phase 2: wait for each game's pod (it self-generates its key on boot).
	//
	// The step's own timeout must exceed kubectl's, or our context kills
	// kubectl first and reports a bare "timed out after 1m30s" — swallowing
	// the reason kubectl was about to print. The default 90s step timeout did
	// exactly that to the 120s below, and also capped a first-boot gateway at
	// 90s: less than the ~30-90s a new one needs AFTER pulling the WireGuard
	// image, so a cold node could never finish.
	rollout := s
	rollout.Timeout = 210 * time.Second
	for _, e := range enabled {
		add(rollout.run("game "+e.Name+": rollout",
			"kubectl", "", kctx("-n", ns, "rollout", "status",
				"deploy/wg-gw-"+e.Slug(), "--timeout=180s")...))
	}
	// Phase 3: read back the self-generated PUBLIC key, persist it.
	for _, e := range enabled {
		st := s.run("game "+e.Name+": read public key", "kubectl", "",
			kctx("-n", ns, "exec", "deploy/wg-gw-"+e.Slug(), "-c", "wireguard",
				"--", "cat", "/keys/publickey")...)
		pk := strings.TrimSpace(st.Stdout)
		if st.OK && len(pk) >= 40 {
			e.GatewayPubKey = pk
			persist(e.ID, pk)
			st.Stdout = pk
		} else {
			st.OK = false
			if st.Stderr == "" {
				st.Stderr = "no public key read from pod"
			}
		}
		add(st)
	}
	// Phase 4: delete gateways for games no longer present (clean removal).
	want := map[string]bool{}
	for _, e := range enabled {
		want["wg-gw-"+e.Slug()] = true
	}
	lst := s.run("cleanup: list managed gateways", "kubectl", "",
		kctx("-n", ns, "get", "deploy", "-l", "proxyctl=gateway",
			"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")...)
	add(lst)
	for _, name := range strings.Fields(lst.Stdout) {
		if !strings.HasPrefix(name, "wg-gw-") || want[name] {
			continue
		}
		add(s.run("cleanup: delete "+name, "kubectl", "",
			kctx("-n", ns, "delete", "deploy,secret,pvc", "-l", "app="+name,
				"--ignore-not-found", "--wait=false")...))
	}

	// Phase 5: droplet — only peers whose pod key we actually have. Write
	// wg0.conf for reboot-persistence, then `wg syncconf` LIVE (adds/
	// removes/updates peers without tearing the interface), then rebuild
	// the idempotent iptables chains. Other games keep running throughout.
	var ready []*Entry
	for _, e := range enabled {
		if e.GatewayPubKey != "" {
			ready = append(ready, e)
		}
	}
	wg0 := RenderDropletWG0(in, ready)
	remote := "umask 077; new=$(cat); " +
		"key=$(grep -m1 '^PrivateKey' /etc/wireguard/wg0.conf | cut -d= -f2- | tr -d ' '); " +
		"[ -n \"$key\" ] || { echo 'no droplet PrivateKey' >&2; exit 1; }; " +
		"printf '%s\\n' \"$new\" | sed \"s|__DROPLET_PRIVATE_KEY__|$key|\" > /etc/wireguard/wg0.conf; " +
		"(wg show wg0 >/dev/null 2>&1 && wg syncconf wg0 <(wg-quick strip wg0)) || wg-quick up wg0; " +
		"wg show wg0 | grep -E 'interface|peer|latest' | head -40"
	add(s.sshDroplet("droplet: install wg0.conf + wg syncconf (live, no bounce)",
		wg0, "bash -c "+shq(remote)))
	add(s.sshDroplet("droplet: rebuild NAT chains",
		RenderDropletNATScript(in, ready), "bash -s"))

	res.OK = true
	for _, st := range res.Steps {
		if !st.OK {
			res.OK = false
		}
	}
	if res.OK {
		res.Message = "Per-game apply OK — each game on its own isolated tunnel; " +
			"droplet peers reconciled live (others never dropped)."
	} else {
		res.Message = "One or more steps failed — see output. Per-game: a failing " +
			"game does not affect the others; safe to re-run after fixing."
	}
	return res
}

// shq single-quotes a string for safe embedding in `bash -c '...'`.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (s SSHApplier) Apply(in Infra, r Rendered) ApplyResult {
	res := ApplyResult{Mode: "ssh", Automated: true, Rendered: r}

	if !s.dropletReady() {
		res.OK = false
		res.Message = "Droplet not configured — open Setup → Droplet to install proxyCTL's SSH key on the droplet, then Apply."
		return res
	}

	// 1. Droplet: stream the rendered wg0.conf in over ssh stdin. The
	//    rendered file carries a __DROPLET_PRIVATE_KEY__ placeholder (so no
	//    key material ever leaves the box / enters proxyctl); the remote
	//    one-liner splices the EXISTING PrivateKey out of the live
	//    wg0.conf back in before installing. Uses ambient ssh creds.
	remote := "umask 077 && new=$(cat) && " +
		"key=$(grep -m1 '^PrivateKey' /etc/wireguard/wg0.conf | cut -d= -f2- | tr -d ' ') && " +
		"[ -n \"$key\" ] || { echo 'no existing PrivateKey on droplet' >&2; exit 1; } && " +
		"printf '%s\\n' \"$new\" | sed \"s|__DROPLET_PRIVATE_KEY__|$key|\" > /etc/wireguard/wg0.conf && " +
		"systemctl restart wg-quick@wg0 && wg show wg0"
	res.Steps = append(res.Steps, s.sshDroplet(
		"droplet: install wg0.conf + restart wg-quick@wg0",
		r.DropletWG0Conf, remote))

	// 2. Cluster: the rendered manifest carries a __GATEWAY_PRIVATE_KEY__
	//    placeholder. Pull the existing key out of the live Secret, splice
	//    it in locally, then kubectl apply from stdin — using the
	//    operator's ambient kubeconfig / context. No key is persisted.
	ctxArgs := []string{}
	if s.KubeContext != "" {
		ctxArgs = append(ctxArgs, "--context", s.KubeContext)
	}
	keyStep := s.run("cluster: read existing wg-gateway key", "kubectl", "",
		append(append([]string{}, ctxArgs...),
			"-n", in.K8sNamespace, "get", "secret", "wg-gateway",
			"-o", "go-template={{index .data \"wg0.conf\"}}")...)
	res.Steps = append(res.Steps, keyStep)
	manifest := r.GatewayYAML
	if keyStep.OK {
		if k := extractWGPrivateKey(keyStep.Stdout); k != "" {
			manifest = strings.ReplaceAll(manifest, "__GATEWAY_PRIVATE_KEY__", k)
		}
	}
	if strings.Contains(manifest, "__GATEWAY_PRIVATE_KEY__") {
		res.OK = false
		res.Message = "Could not recover the existing gateway PrivateKey from " +
			"the live Secret; aborting kubectl apply to avoid breaking the " +
			"tunnel. Apply the gateway manifest by hand (Manual mode)."
		return res
	}
	kargs := append(append([]string{}, ctxArgs...), "apply", "-f", "-")
	res.Steps = append(res.Steps, s.run(
		"cluster: kubectl apply wg-gateway", "kubectl", manifest, kargs...,
	))

	// Force the gateway pod to recreate so it re-reads the updated Secret.
	// The wg0.conf is a subPath Secret mount — those NEVER update in a
	// running pod, and wg-quick only reads its config at start anyway. So
	// `kubectl apply` of a Secret-only change (a new/edited entry that
	// doesn't alter the Deployment spec) silently does NOT take effect
	// until the pod restarts. rollout restart is idempotent (stamps a
	// restartedAt annotation) and, with strategy:Recreate, gives a clean
	// reload — without it, "add entry → Apply" looks OK but the new
	// proxy is dead on the gateway.
	restart := append(append([]string{}, ctxArgs...),
		"-n", in.K8sNamespace, "rollout", "restart", "deploy/wg-gateway")
	res.Steps = append(res.Steps, s.run(
		"cluster: rollout restart wg-gateway", "kubectl", "", restart...,
	))

	rollout := append(append([]string{}, ctxArgs...),
		"-n", in.K8sNamespace, "rollout", "status",
		"deploy/wg-gateway", "--timeout=90s")
	res.Steps = append(res.Steps, s.run(
		"cluster: rollout status", "kubectl", "", rollout...,
	))

	res.OK = true
	for _, st := range res.Steps {
		if !st.OK {
			res.OK = false
		}
	}
	if res.OK {
		res.Message = "Applied via the operator's ambient ssh + kubectl. " +
			"proxyctl stored no credentials. Verify in-game."
	} else {
		res.Message = "One or more steps failed — see output below. " +
			"Nothing was persisted by proxyctl; safe to re-run after fixing."
	}
	return res
}
