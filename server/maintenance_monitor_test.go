package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// =====================================================================
// Integration tests for the maintenance update monitor
// (StartMaintenanceUpdateMonitor / checkAndNotifyMaintenanceUpdates).
//
// The monitor runs against its REAL dependencies: the real
// AlertConfigStore, the real maintenance de-dup state file, the real
// SendDiscordAlert HTTP send, the real GHCR "latest" resolver, the real
// droplet probe parsers, and the real SSHApplier argument pipeline. The
// only fakes are the remote ends:
//
//   - `ssh` and `kubectl` are PATH shims. SSHApplier is a concrete
//     struct, not an interface, so this is the repo's established pattern
//     for faking the droplet (see TestReconcileWireguardGatewaysPatchesImagesOnly).
//     Every invocation is logged, and the shims serve canned probe output,
//     so the tests can prove exactly which remote commands the monitor
//     issued.
//   - GHCR is an httptest server behind the existing ghcrBaseURL seam.
//   - The Discord webhook is an httptest server; validateDiscordWebhookURL
//     is swapped for a permissive check for the test's duration because the
//     production validator only accepts https://discord[app].com URLs.
//
// Together the tests prove:
//   - the CONFIGURED Discord alert path is used (an embed is POSTed to the
//     webhook URL saved in AlertConfigStore, with the maintenance title
//     and amber color);
//   - NO SPAM: an unchanged condition notifies exactly once, cleared
//     conditions prune the de-dup state, and only a reappeared condition
//     produces a fresh notification;
//   - NO AUTOMATIC MAINTENANCE OPERATIONS: with the auto-update store
//     armed (but not due) and a fully configured SSH droplet available,
//     the monitor only issues the read-only status probes — the ssh/kubectl
//     logs contain no mutating command (apt-get, image patch/rollout,
//     reboot scheduling, WireGuard from-source sync).
// =====================================================================

// --- PATH shims for ssh / kubectl --------------------------------------

const fakeSSHShim = `#!/bin/sh
echo "ssh $*" >> "$PROXYCTL_FAKE_SSH_LOG"
for arg in "$@"; do
  case "$arg" in
    *PROXYCTL_PENDING_UPDATES*) cat "$PROXYCTL_FAKE_MGMT_PROBE"; exit 0 ;;
    *WGQUICK_PATH=*) cat "$PROXYCTL_FAKE_WG_PROBE"; exit 0 ;;
  esac
done
exit 0
`

const fakeKubectlShim = `#!/bin/sh
echo "kubectl $*" >> "$PROXYCTL_FAKE_KUBECTL_LOG"
exit 0
`

// --- Canned droplet probe payloads (same marker format the real probes emit)

const mgmtProbeWithIssues = `PROXYCTL_HOSTNAME=test-droplet
PROXYCTL_OS=Ubuntu 24.04.3 LTS
PROXYCTL_KERNEL=6.8.0-88-generic
PROXYCTL_UPTIME_SECONDS=86461
PROXYCTL_LOAD=0.12 0.34 0.56
PROXYCTL_CPU_CORES=2
PROXYCTL_CPU_USAGE_PCT=17.5
PROXYCTL_MEM_TOTAL=4194304000
PROXYCTL_MEM_AVAILABLE=3145728000
PROXYCTL_DISK_TOTAL=53687091200
PROXYCTL_DISK_USED=10737418240
PROXYCTL_DISK_AVAILABLE=42949672960
PROXYCTL_WIREGUARD_ACTIVE=true
PROXYCTL_SSH_ACTIVE=true
PROXYCTL_UNATTENDED_ACTIVE=true
PROXYCTL_REBOOT_REQUIRED=true
PROXYCTL_PENDING_UPDATES=3
PROXYCTL_WG_VERSION=wireguard-tools v1.0.20210914
PROXYCTL_CLOUDFLARED_INSTALLED=true
PROXYCTL_CLOUDFLARED_ACTIVE=true
PROXYCTL_CLOUDFLARED_VERSION=cloudflared version 2026.10.4
`

const mgmtProbeClean = `PROXYCTL_HOSTNAME=test-droplet
PROXYCTL_OS=Ubuntu 24.04.3 LTS
PROXYCTL_KERNEL=6.8.0-88-generic
PROXYCTL_UPTIME_SECONDS=86461
PROXYCTL_LOAD=0.12 0.34 0.56
PROXYCTL_CPU_CORES=2
PROXYCTL_CPU_USAGE_PCT=17.5
PROXYCTL_MEM_TOTAL=4194304000
PROXYCTL_MEM_AVAILABLE=3145728000
PROXYCTL_DISK_TOTAL=53687091200
PROXYCTL_DISK_USED=10737418240
PROXYCTL_DISK_AVAILABLE=42949672960
PROXYCTL_WIREGUARD_ACTIVE=true
PROXYCTL_SSH_ACTIVE=true
PROXYCTL_UNATTENDED_ACTIVE=true
PROXYCTL_REBOOT_REQUIRED=false
PROXYCTL_PENDING_UPDATES=0
PROXYCTL_WG_VERSION=wireguard-tools v1.0.20260223
PROXYCTL_CLOUDFLARED_INSTALLED=true
PROXYCTL_CLOUDFLARED_ACTIVE=true
PROXYCTL_CLOUDFLARED_VERSION=cloudflared version 2026.10.4
`

const wgProbeStale = `WG_PATH=/usr/bin/wg
WG_VERSION=wireguard-tools v1.0.20210914
WGQUICK_PATH=/usr/bin/wg-quick
`

const wgProbeSynced = `WG_PATH=/usr/local/bin/wg
WG_VERSION=wireguard-tools v1.0.20260223
WGQUICK_PATH=/usr/local/bin/wg-quick
`

// --- Swappable upstream digest for the GHCR stub ------------------------

type digestCell struct {
	mu sync.Mutex
	v  string
}

func (d *digestCell) get() string  { d.mu.Lock(); defer d.mu.Unlock(); return d.v }
func (d *digestCell) set(v string) { d.mu.Lock(); d.v = v; d.mu.Unlock() }

// newUpstreamDigest is a valid GHCR index digest that is NOT the embedded
// reviewed pin, so wireguardImageStatus reports an available update.
var newUpstreamDigest = "sha256:" + strings.Repeat("b", 64)

// pinnedBareDigest is the embedded pin's bare sha256 digest; a lookup that
// returns it reports no update available (ref == wireguardImageDigest).
var pinnedBareDigest = strings.TrimPrefix(wireguardImageDigest, wireguardImageRepo+"@")

func newGHCRStub(t *testing.T, digest *digestCell) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/token"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"stub"}`)
		case r.URL.Path == "/v2/gamectl-hq/wireguard-kube/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", digest.get())
			fmt.Fprint(w, `{"manifests":[
				{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","platform":{"architecture":"amd64"}}
			]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- Fake Discord webhook ----------------------------------------------

type webhookHit struct {
	Method      string
	ContentType string
	Body        discordAlert
}

type webhookCapture struct {
	mu   sync.Mutex
	hits []webhookHit
}

func (c *webhookCapture) all() []webhookHit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]webhookHit(nil), c.hits...)
}

func newWebhookCapture(t *testing.T) (*webhookCapture, *httptest.Server) {
	t.Helper()
	c := &webhookCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body discordAlert
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.hits = append(c.hits, webhookHit{
			Method:      r.Method,
			ContentType: r.Header.Get("Content-Type"),
			Body:        body,
		})
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

// --- Fixture -------------------------------------------------------------

// monitorFixture wires an API whose maintenance check runs against real
// stores with faked remote ends (see the file header).
type monitorFixture struct {
	api        *API
	autoUpdate *autoUpdateStore
	webhook    *webhookCapture
	sshLog     string
	kubectlLog string
	mgmtProbe  string
	wgProbe    string
	digest     *digestCell
}

func newMonitorFixture(t *testing.T) *monitorFixture {
	t.Helper()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &monitorFixture{
		sshLog:     filepath.Join(dir, "ssh.log"),
		kubectlLog: filepath.Join(dir, "kubectl.log"),
		mgmtProbe:  filepath.Join(dir, "mgmt-probe.txt"),
		wgProbe:    filepath.Join(dir, "wg-probe.txt"),
		digest:     &digestCell{v: newUpstreamDigest},
	}
	writeShim := func(name, script string) {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeShim("ssh", fakeSSHShim)
	writeShim("kubectl", fakeKubectlShim)
	if err := os.WriteFile(f.mgmtProbe, []byte(mgmtProbeWithIssues), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.wgProbe, []byte(wgProbeStale), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PROXYCTL_FAKE_SSH_LOG", f.sshLog)
	t.Setenv("PROXYCTL_FAKE_KUBECTL_LOG", f.kubectlLog)
	t.Setenv("PROXYCTL_FAKE_MGMT_PROBE", f.mgmtProbe)
	t.Setenv("PROXYCTL_FAKE_WG_PROBE", f.wgProbe)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ghcr := newGHCRStub(t, f.digest)
	oldBase := ghcrBaseURL
	ghcrBaseURL = ghcr.URL
	t.Cleanup(func() { ghcrBaseURL = oldBase })

	capture, webhookSrv := newWebhookCapture(t)
	f.webhook = capture

	// The production validator only accepts https://discord[app].com
	// webhooks, so the real SendDiscordAlert HTTP path cannot reach an
	// httptest server without swapping it; production keeps the default.
	oldValidate := validateDiscordWebhookURL
	validateDiscordWebhookURL = func(string) error { return nil }
	t.Cleanup(func() { validateDiscordWebhookURL = oldValidate })

	alerts, err := NewAlertConfigStore(filepath.Join(dir, "alerts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := alerts.Set(AlertConfig{Enabled: true, DiscordWebhookURL: webhookSrv.URL}); err != nil {
		t.Fatal(err)
	}

	dropletStore, err := NewFileDropletStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A fully configured droplet: every SSH-bound code path in the monitor
	// (status probe, wg probe, and any accidental maintenance operation)
	// will attempt to run and be recorded by the ssh shim.
	if err := dropletStore.Save(DropletConfig{
		IP: "10.0.0.5", User: "root", Port: 22,
		PublicKey: "ssh-ed25519 AAAATestPublicKey proxyctl@test",
	}); err != nil {
		t.Fatal(err)
	}

	// Auto-update ARMED (every 1 day) but not yet due: if the monitor's
	// not-due gate regressed, this would trigger runDropletMaintenanceUpdate
	// and the ssh/kubectl logs would show mutating operations.
	autoUpdate := newAutoUpdateStore(filepath.Join(dir, "droplet-autoupdate.json"))
	if _, err := autoUpdate.SetEveryDays(1); err != nil {
		t.Fatal(err)
	}
	autoUpdate.mu.Lock()
	autoUpdate.state.LastRun = time.Now().Unix()
	autoUpdate.mu.Unlock()
	f.autoUpdate = autoUpdate

	f.api = &API{
		alerts:               alerts,
		droplet:              dropletStore,
		applier:              SSHApplier{Timeout: 10 * time.Second},
		autoUpdate:           autoUpdate,
		maintenanceAlertPath: filepath.Join(dir, "maintenance-alerts.json"),
		lastVersionPath:      filepath.Join(dir, "last-version"),
	}
	return f
}

func (f *monitorFixture) setMgmtProbe(t *testing.T, payload string) {
	t.Helper()
	if err := os.WriteFile(f.mgmtProbe, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *monitorFixture) setWGProbe(t *testing.T, payload string) {
	t.Helper()
	if err := os.WriteFile(f.wgProbe, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *monitorFixture) notified() (map[string]bool, error) {
	return loadMaintenanceAlertState(f.api.maintenanceAlertPath)
}

func (f *monitorFixture) sshLogContent() string {
	b, err := os.ReadFile(f.sshLog)
	if err != nil {
		return ""
	}
	return string(b)
}

func (f *monitorFixture) kubectlLogContent() string {
	b, err := os.ReadFile(f.kubectlLog)
	if err != nil {
		return ""
	}
	return string(b)
}

func (f *monitorFixture) kubectlLogLines() int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(f.kubectlLogContent()), "\n") {
		if line != "" {
			n++
		}
	}
	return n
}

// assertNoMaintenanceOperations fails if the monitor issued any mutating
// command. Only the two read-only droplet probes and the read-only gateway
// listing are ever expected in the logs.
func (f *monitorFixture) assertNoMaintenanceOperations(t *testing.T) {
	t.Helper()
	sshLog := f.sshLogContent()
	kubectlLog := f.kubectlLogContent()
	for _, marker := range []string{
		"apt-get",          // droplet bootstrap/upgrade packages
		"systemd-run",      // scheduled droplet reboot
		"wg-tools-src",     // from-source WireGuard build/sync
		"wg syncconf",      // droplet peer reconciliation (Apply)
		"rollout",          // cluster Deployment rollout (WireGuard update)
		"patch deployment", // strategic image patch (WireGuard update)
	} {
		if strings.Contains(sshLog, marker) || strings.Contains(kubectlLog, marker) {
			t.Fatalf("monitor issued a maintenance operation (log contains %q):\nssh:\n%s\nkubectl:\n%s",
				marker, sshLog, kubectlLog)
		}
	}
}

// --- Tests ----------------------------------------------------------------

// TestMaintenanceUpdateCheckUsesConfiguredDiscordPathWithoutSpamOrOps is the
// core integration test: the monitor's check — the single action
// StartMaintenanceUpdateMonitor repeats on its timer — drives the real
// AlertConfigStore + SendDiscordAlert pipeline to a real HTTP webhook,
// notifies once per distinct condition (no spam), re-notifies when a cleared
// condition reappears, and performs no maintenance operations.
func TestMaintenanceUpdateCheckUsesConfiguredDiscordPathWithoutSpamOrOps(t *testing.T) {
	f := newMonitorFixture(t)

	// 1. First check: all four maintenance conditions are present (upstream
	// GHCR image newer than the pin, stale droplet wg tools, 3 pending
	// package updates, reboot required) -> exactly ONE combined Discord embed.
	f.api.checkAndNotifyMaintenanceUpdates()
	hits := f.webhook.all()
	if len(hits) != 1 {
		t.Fatalf("webhook hits after first check = %d, want 1", len(hits))
	}
	hit := hits[0]
	if hit.Method != http.MethodPost || hit.ContentType != "application/json" {
		t.Fatalf("webhook request = %q %q, want POST application/json", hit.Method, hit.ContentType)
	}
	if len(hit.Body.Embeds) != 1 {
		t.Fatalf("embeds = %#v, want exactly one combined embed", hit.Body.Embeds)
	}
	e := hit.Body.Embeds[0]
	if e.Title != "🟡 ProxyCTL maintenance updates available" {
		t.Fatalf("title = %q", e.Title)
	}
	for _, want := range []string{
		"WireGuard gateway image update available",
		"Droplet WireGuard tools v1.0.20210914 → v1.0.20260223",
		"3 droplet package updates available",
		"Droplet reboot required to finish installed updates",
	} {
		if !strings.Contains(e.Description, want) {
			t.Fatalf("description missing %q:\n%s", want, e.Description)
		}
	}
	if e.Color != 0xF59E0B {
		t.Fatalf("color = %#x, want maintenance amber 0xF59E0B", e.Color)
	}

	// The de-dup state now records every active kind.
	notified, err := f.notified()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"wireguard-image", "wireguard-droplet", "packages", "reboot"} {
		if !notified[kind] {
			t.Fatalf("notified = %#v, want %q recorded", notified, kind)
		}
	}

	// 2. No spam: an unchanged second check posts nothing.
	f.api.checkAndNotifyMaintenanceUpdates()
	if n := len(f.webhook.all()); n != 1 {
		t.Fatalf("webhook hits after unchanged re-check = %d, want 1", n)
	}

	// 3. All conditions clear -> no notification, and the state is pruned.
	f.setMgmtProbe(t, mgmtProbeClean)
	f.setWGProbe(t, wgProbeSynced)
	f.digest.set(pinnedBareDigest)
	f.api.checkAndNotifyMaintenanceUpdates()
	if n := len(f.webhook.all()); n != 1 {
		t.Fatalf("webhook hits after conditions cleared = %d, want 1", n)
	}
	notified, err = f.notified()
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 0 {
		t.Fatalf("notified = %#v, want pruned to empty", notified)
	}

	// 4. One condition reappears -> exactly one fresh notification, covering
	//    only the reappeared kind.
	f.digest.set(newUpstreamDigest)
	f.api.checkAndNotifyMaintenanceUpdates()
	hits = f.webhook.all()
	if len(hits) != 2 {
		t.Fatalf("webhook hits after reappearance = %d, want 2", len(hits))
	}
	re := hits[1].Body.Embeds
	if len(re) != 1 || !strings.Contains(re[0].Description, "WireGuard gateway image update available") {
		t.Fatalf("reappearance embeds = %#v, want the wireguard-image notice only", re)
	}
	if strings.Contains(re[0].Description, "package updates") || strings.Contains(re[0].Description, "reboot required") {
		t.Fatalf("reappearance embed leaked cleared conditions: %q", re[0].Description)
	}

	// 5. No maintenance operation was ever attempted, even though the
	//    auto-update store was armed (not yet due) and a fully configured
	//    SSH droplet was available: the only remote commands are the two
	//    read-only droplet probes and the read-only gateway listing.
	f.assertNoMaintenanceOperations(t)
	if !strings.Contains(f.sshLogContent(), "PROXYCTL_PENDING_UPDATES") ||
		!strings.Contains(f.sshLogContent(), "WGQUICK_PATH=") {
		t.Fatal("expected the read-only droplet probes in the ssh log")
	}
	if !strings.Contains(f.kubectlLogContent(), "proxyctl=gateway") {
		t.Fatal("expected the read-only gateway listing in the kubectl log")
	}
}

// TestStartMaintenanceUpdateMonitorGoroutine exercises the real goroutine
// (startup delay + check interval shrunk to milliseconds via the timing
// seams) and proves it: fires its first check shortly after start, keeps
// ticking (the read-only kubectl listing is repeated), posts exactly one
// de-duplicated notification, and stops cleanly on context cancel.
func TestStartMaintenanceUpdateMonitorGoroutine(t *testing.T) {
	f := newMonitorFixture(t)

	oldInit := maintenanceMonitorInitialDelay
	oldInterval := maintenanceUpdateCheckInterval
	maintenanceMonitorInitialDelay = time.Millisecond
	maintenanceUpdateCheckInterval = 2 * time.Millisecond
	t.Cleanup(func() {
		maintenanceMonitorInitialDelay = oldInit
		maintenanceUpdateCheckInterval = oldInterval
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.api.StartMaintenanceUpdateMonitor(ctx)

	// Wait for the first (and, by de-dup, only) notification.
	deadline := time.Now().Add(10 * time.Second)
	for len(f.webhook.all()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("monitor never posted its first notification")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Let the ticker run: the kubectl log must grow (each check lists
	// gateways read-only) while the webhook stays at exactly one hit.
	first := f.kubectlLogLines()
	time.Sleep(300 * time.Millisecond)
	if got := f.kubectlLogLines(); got <= first {
		t.Fatalf("ticker did not keep checking: kubectl lines %d -> %d", first, got)
	}
	if n := len(f.webhook.all()); n != 1 {
		t.Fatalf("webhook hits during ticker soak = %d, want 1 (no spam across ticks)", n)
	}

	// Cancel: the goroutine must stop checking.
	cancel()
	time.Sleep(100 * time.Millisecond)
	at := f.kubectlLogLines()
	time.Sleep(300 * time.Millisecond)
	if got := f.kubectlLogLines(); got != at {
		t.Fatalf("monitor kept checking after cancel: kubectl lines %d -> %d", at, got)
	}
	if n := len(f.webhook.all()); n != 1 {
		t.Fatalf("webhook hits after cancel = %d, want 1", n)
	}
	f.assertNoMaintenanceOperations(t)
}

// TestMaintenanceUpdateCheckSilentWhenAlertsDisabled proves the monitor
// respects the configured alert path: with alerts disabled it still runs the
// full check (and maintains de-dup state) but sends nothing.
func TestMaintenanceUpdateCheckSilentWhenAlertsDisabled(t *testing.T) {
	f := newMonitorFixture(t)
	if err := f.api.alerts.Set(AlertConfig{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	f.api.checkAndNotifyMaintenanceUpdates()
	if n := len(f.webhook.all()); n != 0 {
		t.Fatalf("webhook hits = %d, want 0 with alerts disabled", n)
	}
	if _, err := f.notified(); err != nil {
		t.Fatalf("de-dup state not maintained: %v", err)
	}
	f.assertNoMaintenanceOperations(t)
}
