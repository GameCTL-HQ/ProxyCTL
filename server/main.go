package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// //go:embed reads the Vite-built React bundle. kubeUI/ holds the
// source (React + Vite); `npm run build` (driven by build.sh /
// Dockerfile) outputs the static bundle into web/dist, which is what
// gets baked into the binary. Same shape as GameCTL's server/internal/ui/dist.

//go:embed all:web/dist
var webFS embed.FS

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func main() {
	// Structured JSON logging on stderr (matches GameCTL's shape). Lets
	// install.sh / clusterdeploy.sh grep the one-time BOOTSTRAP TOKEN
	// out of `kubectl logs` with the same `"token":"…"` regex both apps
	// use. Go's slog.SetDefault also routes the standard log package
	// (log.Printf, etc.) through the same handler, so the whole log
	// stream is JSON — no mixed text/JSON output.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	addr := flag.String("addr", "127.0.0.1:8080", "address for the admin GUI/API")
	dbPath := flag.String("db", "entries.json", "path to the entries store")
	// Auth is JWT + Kubernetes Secret (see auth.go / authstore.go).
	// The only meaningful value for `-token` / PROXYCTL_TOKEN now is the
	// literal "DISABLE_AUTH", which skips auth entirely. Refused below
	// when the listen address is not loopback (production safety).
	envTok := os.Getenv("PROXYCTL_TOKEN")
	if envTok == "" {
		envTok = os.Getenv("PROXYCTL_TOKEN")
	}
	token := flag.String("token", envTok, "auth bypass — only \"DISABLE_AUTH\" is honored (loopback-gated)")
	mode := flag.String("apply-mode", "ssh", "apply mechanism: \"ssh\" (operator's ambient ssh+kubectl at click-time) or \"manual\" (render+runbook only)")
	dropletSSH := flag.String("droplet-ssh", "root@PLACEHOLDER-DROPLET", "ssh target for the droplet (user@host); ssh mode only")
	kubeCtx := flag.String("kube-context", "", "kubectl --context for the home cluster (empty = current context); ssh mode only")
	render := flag.String("render", "", "one-shot: print rendered config to stdout and exit — \"gateway\", \"droplet\", or \"all\". No server, no token, no ssh/kube creds (pure render). e.g. proxyctl -render gateway | kubectl apply -f -")
	flag.Parse()

	// One-shot render mode: the drop-in install path. Lets an operator pipe
	// the gateway manifest straight into the cluster without running the
	// server, SSH-tunnelling in, or holding any credential — render funcs
	// are pure and only read the entries store.
	if *render != "" {
		store, err := NewStore(*dbPath)
		if err != nil {
			log.Fatalf("store: %v", err)
		}
		rb := Render(DefaultInfra(), store.List())
		switch *render {
		case "gateway":
			fmt.Print(rb.GatewayYAML)
		case "droplet":
			fmt.Print(rb.DropletWG0Conf)
		case "all":
			fmt.Print(rb.DropletWG0Conf)
			fmt.Print("\n# ===== gateway (kubectl apply -f -) =====\n")
			fmt.Print(rb.GatewayYAML)
		default:
			log.Fatalf("unknown -render %q (want \"gateway\", \"droplet\", or \"all\")", *render)
		}
		return
	}

	// Public-bind safety check: only refuse if auth is explicitly
	// disabled. The default path always has auth — pre-claim the
	// browser sees a bootstrap-token gate, post-claim it's username +
	// password (with HTTP Basic for API). The DISABLE_AUTH escape hatch
	// is dev-only and is gated to loopback.
	loopback := addrIsLoopback(*addr)
	if !loopback && *token == "DISABLE_AUTH" {
		log.Fatalf("refusing to bind public address %q with DISABLE_AUTH. "+
			"Remove -token=DISABLE_AUTH or bind to 127.0.0.1.", *addr)
	}

	store, err := NewStore(*dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	domains, err := NewDomainStore(filepath.Join(filepath.Dir(*dbPath), "domains.json"))
	if err != nil {
		log.Fatalf("domains store: %v", err)
	}
	// Operator-chosen NFS folder for per-gateway key PVCs (default ProxyCTL/Keys).
	keysStore, err := NewKeysStore(filepath.Join(filepath.Dir(*dbPath), "keys.json"))
	if err != nil {
		log.Fatalf("keys store: %v", err)
	}
	// Web/Ingress tunnels (L7) — hostname → Service routes rendered as
	// Traefik IngressRoutes. Lives next to entries.json/domains.json.
	webroutes, err := NewWebRouteStore(filepath.Join(filepath.Dir(*dbPath), "webroutes.json"))
	if err != nil {
		log.Fatalf("webroutes store: %v", err)
	}

	// Pick the Applier. Both stand behind the same interface; the store,
	// API and UI are identical regardless of which is selected. proxyctl
	// holds ZERO standing credentials in either mode — ssh mode borrows the
	// operator's ambient ssh-agent/kubeconfig only at click-time.
	// v2 setup-wizard storage: lives next to entries.json/domains.json.
	// Empty until the operator visits the Setup → Droplet wizard. When
	// configured, takes precedence over the legacy -droplet-ssh flag.
	dropletStore, err := NewFileDropletStore(filepath.Dir(*dbPath))
	if err != nil {
		log.Fatalf("droplet store: %v", err)
	}

	var applier Applier
	switch *mode {
	case "manual":
		applier = ManualApplier{}
	case "ssh":
		applier = SSHApplier{
			DropletSSH:  *dropletSSH,
			KubeContext: *kubeCtx,
			Timeout:     90 * time.Second,
			Droplet:     dropletStore.Get(),
		}
	default:
		log.Fatalf("unknown -apply-mode %q (want \"ssh\" or \"manual\")", *mode)
	}

	// Read-only, on-demand Kubernetes target picker. Shares the security
	// model of the SSHApplier: ZERO stored credentials — it borrows the
	// operator's ambient kubeconfig / in-cluster SA only when the operator
	// opens the picker, and only ever issues read (get) calls. Behind the
	// same loopback + token auth as everything else.
	kube := KubeBrowser{KubeContext: *kubeCtx, Timeout: 15 * time.Second}

	cf := NewCF()
	// Cloudflare token resolution order: env (CF_API_TOKEN /
	// CLOUDFLARE_API_TOKEN) — read by NewCF — wins. If neither set, fall
	// back to the wizard-managed PVC file (cf.token, 0600). Either way
	// the token is never returned by the API or written to logs.
	cfTokenPath := filepath.Join(filepath.Dir(*dbPath), "cf.token")
	if !cf.Configured() {
		if b, err := os.ReadFile(cfTokenPath); err == nil {
			cf.SetToken(strings.TrimSpace(string(b)))
		}
	}
	log.Printf("cloudflare: %s", map[bool]string{true: "configured", false: "not configured (use the Setup wizard or set CF_API_TOKEN)"}[cf.Configured()])
	api := &API{store: store, domains: domains, keys: keysStore, webroutes: webroutes, cf: cf, kube: kube, infra: DefaultInfra(), applier: applier,
		cfTokenPath: cfTokenPath,
		droplet:     dropletStore,
		appliedPath: filepath.Join(filepath.Dir(*dbPath), "applied.hash"),
		jobPath:     filepath.Join(filepath.Dir(*dbPath), "apply-state.json")}
	// Apply any wizard-saved droplet overlay on first boot too.
	api.refreshInfra()
	api.loadJob()

	// Auth: GameCTL-shape JWT + K8s Secret. The signing key + bcrypt
	// user record live in `proxyctl-auth` in the ProxyCTL namespace
	// (auto-detected via the downward API POD_NAMESPACE, with a
	// PROXYCTL_NAMESPACE / -auth-namespace override). On a fresh install
	// the Secret is absent → setup mode: ephemeral key + one-time
	// bootstrap token in memory, logged once for install.sh to scrape.
	// Recovery: `kubectl delete secret proxyctl-auth` + rollout restart.
	authNS := os.Getenv("POD_NAMESPACE")
	if authNS == "" {
		authNS = os.Getenv("PROXYCTL_NAMESPACE")
	}
	if authNS == "" {
		authNS = "proxyctl"
	}
	authSecret := os.Getenv("PROXYCTL_AUTH_SECRET_NAME")
	if authSecret == "" {
		authSecret = "proxyctl-auth"
	}
	// In-app update notify + one-click self-update (mirrors GameCTL): poll the
	// public repo's releases, and roll our own Deployment on apply. selfNS is
	// the namespace we run in; selfDeploy defaults to "proxyctl".
	selfDeploy := os.Getenv("PROXYCTL_DEPLOYMENT_NAME")
	if selfDeploy == "" {
		selfDeploy = "proxyctl"
	}
	api.upd = newUpdateChecker(updateRepo, version)
	api.selfNS = authNS
	api.selfDeploy = selfDeploy
	api.kubectl = "kubectl"
	authStore := NewAuthStore(authNS, authSecret)
	authn, err := newAuthn(authStore, *token == "DISABLE_AUTH")
	if err != nil {
		log.Fatalf("auth: %v", err)
	}

	// Protected routes: every /api/* endpoint that mutates or reads
	// operator state requires Authorization: Bearer <JWT>.
	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/kube/namespaces", kube.namespaces)
	protected.HandleFunc("GET /api/kube/services", kube.services)
	protected.HandleFunc("GET /api/entries", api.list)
	protected.HandleFunc("POST /api/entries", api.create)
	protected.HandleFunc("PUT /api/entries/{id}", api.update)
	protected.HandleFunc("DELETE /api/entries/{id}", api.delete)
	protected.HandleFunc("POST /api/entries/{id}/toggle", api.toggle)
	protected.HandleFunc("POST /api/entries/{id}/rebind", api.rebind)
	protected.HandleFunc("GET /api/rendered", api.rendered)
	protected.HandleFunc("POST /api/apply", api.apply)
	protected.HandleFunc("GET /api/apply/status", api.applyStatus)
	protected.HandleFunc("GET /api/droplet", api.getDroplet)
	protected.HandleFunc("POST /api/droplet/generate", api.generateDroplet)
	protected.HandleFunc("POST /api/droplet/regenerate", api.regenerateDroplet)
	protected.HandleFunc("PUT /api/droplet", api.saveDroplet)
	protected.HandleFunc("POST /api/droplet/test", api.testDroplet)
	protected.HandleFunc("POST /api/droplet/bootstrap", api.bootstrapDroplet)
	protected.HandleFunc("GET /api/droplet/egress-ip", api.detectEgressIP)
	protected.HandleFunc("POST /api/droplet/lockdown-ssh", api.lockdownSSH)
	protected.HandleFunc("POST /api/droplet/unlock-ssh", api.unlockSSH)
	protected.HandleFunc("GET /api/cf/status", api.cfStatus)
	protected.HandleFunc("POST /api/cf/token", api.cfSaveToken)
	protected.HandleFunc("POST /api/cf/test", api.cfTest)
	protected.HandleFunc("DELETE /api/cf/token", api.cfDeleteToken)
	protected.HandleFunc("GET /api/domains", api.listDomains)
	protected.HandleFunc("POST /api/domains", api.addDomain)
	protected.HandleFunc("DELETE /api/domains/{domain}", api.delDomain)
	protected.HandleFunc("GET /api/keys-config", api.getKeysConfig)
	protected.HandleFunc("PUT /api/keys-config", api.setKeysConfig)
	protected.HandleFunc("POST /api/keys-config/migrate", api.migrateKeys)
	protected.HandleFunc("GET /api/storage/share-setup", api.getShareSetup)
	protected.HandleFunc("POST /api/storage/share-setup/ack", api.ackShareNodes)
	protected.HandleFunc("GET /api/storage/status", api.getStorageStatus)
	protected.HandleFunc("POST /api/storage/test", api.testStorageShare)
	protected.HandleFunc("POST /api/storage/adopt", api.adoptStorage)
	protected.HandleFunc("GET /api/webroutes", api.listWebRoutes)
	protected.HandleFunc("POST /api/webroutes", api.createWebRoute)
	protected.HandleFunc("PUT /api/webroutes/{id}", api.updateWebRoute)
	protected.HandleFunc("DELETE /api/webroutes/{id}", api.deleteWebRoute)
	protected.HandleFunc("POST /api/webroutes/{id}/toggle", api.toggleWebRoute)
	protected.HandleFunc("GET /api/tunnel/status", api.tunnelStatus)
	protected.HandleFunc("POST /api/tunnel/setup", api.tunnelSetup)
	protected.HandleFunc("GET /api/stats", api.stats)
	protected.HandleFunc("GET /api/dns/records", api.dnsRecords)
	protected.HandleFunc("POST /api/dns/records", api.createDNS)
	// In-app version + update notify/self-update.
	protected.HandleFunc("GET /api/version", api.version)
	protected.HandleFunc("GET /api/update/check", api.updateCheck)
	protected.HandleFunc("POST /api/update/apply", api.updateApply)
	protected.HandleFunc("GET /api/release-notes", api.releaseNotes)
	authedAPI := authn.middleware(protected)

	// Public routes: static UI + auth handshake. The UI is always served
	// (no server-side login wall); the JS reads /api/auth/state on load
	// and renders the claim form, login form, or main app accordingly
	// — same client-side flow as GameCTL.
	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	root.HandleFunc("GET /api/auth/state", authn.state)
	root.HandleFunc("POST /api/auth/setup", authn.setup)
	root.HandleFunc("POST /api/token", authn.tokenHandler)
	root.Handle("/api/", authedAPI)
	sub, _ := fs.Sub(webFS, "web/dist")
	root.Handle("/", http.FileServer(http.FS(sub)))
	srv := &http.Server{Addr: *addr, Handler: root}
	go func() {
		log.Printf("proxyctl admin GUI on http://%s  (store: %s, apply-mode: %s)",
			*addr, *dbPath, applier.Mode())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
}

type API struct {
	store       *Store
	domains     *DomainStore
	keys        *KeysStore     // operator-chosen NFS folder for gateway key PVCs
	webroutes   *WebRouteStore // L7 host routes → Traefik IngressRoutes
	cf          *CF
	cfTokenPath string // 0600 file the CF wizard writes the token to
	kube        KubeBrowser
	infra       Infra
	applier     Applier
	droplet     DropletStore // wizard-managed droplet config + SSH key (v2)
	appliedPath string       // file holding the config hash of the last successful apply

	upd        *updateChecker // in-app update notify (polls GitHub releases)
	selfNS     string         // ProxyCTL's own namespace (self-rollout target)
	selfDeploy string         // ProxyCTL's own Deployment name
	kubectl    string         // kubectl binary (uses the in-cluster SA)

	jobMu   sync.Mutex
	job     *ApplyJob
	jobPath string // persisted last apply job (survives a proxyctl restart)
}

// ApplyJob is a server-side, pollable apply. The apply runs in a
// background goroutine appending Steps as they complete; the UI polls
// GET /api/apply/status so a browser refresh reconnects to live
// progress instead of losing it (GameCTL-style).
type ApplyJob struct {
	Running   bool   `json:"running"`
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt"`
	Steps     []Step `json:"steps"`
}

func (a *API) saveJob() {
	a.jobMu.Lock()
	b, _ := json.MarshalIndent(a.job, "", "  ")
	a.jobMu.Unlock()
	if b != nil {
		_ = os.WriteFile(a.jobPath, b, 0o644)
	}
}

// loadJob restores the last job at startup. A goroutine can't survive a
// restart, so a job still marked Running was interrupted — record that.
func (a *API) loadJob() {
	b, err := os.ReadFile(a.jobPath)
	if err != nil {
		return
	}
	var j ApplyJob
	if json.Unmarshal(b, &j) != nil {
		return
	}
	if j.Running {
		j.Running = false
		j.OK = false
		j.Message = "interrupted by a proxyctl restart — re-run Apply"
		j.EndedAt = time.Now().Unix()
	}
	a.job = &j
}

// applyStatus is polled by the UI (and on page load) so progress and the
// final result survive a refresh.
func (a *API) applyStatus(w http.ResponseWriter, r *http.Request) {
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	if a.job == nil {
		a.writeJSON(w, http.StatusOK, map[string]any{"idle": true, "running": false})
		return
	}
	a.writeJSON(w, http.StatusOK, a.job)
}

// configHash is a stable fingerprint of the current entry set. "Pending
// changes" = this differs from the hash persisted at the last successful
// apply. Persisted (not in-memory) so the indicator survives a browser
// refresh AND a proxyctl restart.
func (a *API) configHash() string {
	// Both stable-sorted; hashing entries + web routes together means a
	// web-route change also lights the "pending changes" indicator.
	b, _ := json.Marshal(a.store.List())
	wb, _ := json.Marshal(a.webroutes.List())
	s := sha256.Sum256(append(b, wb...))
	return hex.EncodeToString(s[:])
}

func (a *API) appliedHash() string {
	b, _ := os.ReadFile(a.appliedPath)
	return strings.TrimSpace(string(b))
}

func (a *API) markApplied() {
	_ = os.WriteFile(a.appliedPath, []byte(a.configHash()), 0o644)
	if b, err := json.Marshal(a.store.List()); err == nil {
		_ = os.WriteFile(a.appliedSnapPath(), b, 0o644)
	}
}

// pending is true when current entries differ from the last successful
// apply. A virgin install (no entries AND no prior apply) is clean —
// not pending — so the action bar / banner stays quiet on a brand-new
// instance until the operator actually has something to push.
func (a *API) pending() bool {
	if len(a.store.List()) == 0 && a.appliedHash() == "" {
		return false
	}
	return a.configHash() != a.appliedHash()
}

// setupProgress is the server-computed checklist that drives the
// first-run nudge strip + the dismissibility of the modal wizard.
// Scope is JUST first-time setup — adding tunnel entries / Apply are
// everyday operations, not onboarding, so they are NOT included here.
// Items resolve to one of:
//
//	"done"     — finished, show ✓
//	"current"  — the next thing the operator should do, highlighted
//	"later"    — not yet relevant, dimmed (or optional + skipped)
//
// allDone is true once all REQUIRED steps are done; an optional step
// left undone never blocks the user from leaving the wizard.
func (a *API) setupProgress(entryCount int) map[string]any {
	_ = entryCount
	cfg := a.droplet.Get()
	dropletDone := cfg.Configured() && cfg != nil && cfg.Bootstrapped
	cfDone := a.cf.Configured()

	steps := []map[string]any{
		{"key": "droplet", "title": "Set up the droplet (SSH key + IP + bootstrap)", "done": dropletDone},
		{"key": "cloudflare", "title": "Connect Cloudflare (DNS automation)", "done": cfDone, "optional": true},
	}
	// "current" = first not-done step regardless of optional. allDone
	// only requires that no REQUIRED step is left.
	currentSet := false
	requiredLeft := false
	for _, s := range steps {
		if s["done"].(bool) {
			s["state"] = "done"
			continue
		}
		if !currentSet {
			s["state"] = "current"
			currentSet = true
		} else {
			s["state"] = "later"
		}
		if s["optional"] != true {
			requiredLeft = true
		}
	}
	return map[string]any{
		"steps":   steps,
		"allDone": !requiredLeft,
	}
}

// appliedSnapPath holds the full entry set captured at the last successful
// apply, so the UI can show WHAT changed (not just "something did").
func (a *API) appliedSnapPath() string {
	return filepath.Join(filepath.Dir(a.appliedPath), "applied.snapshot.json")
}

func (a *API) appliedSnap() ([]Entry, bool) {
	b, err := os.ReadFile(a.appliedSnapPath())
	if err != nil {
		return nil, false
	}
	var es []Entry
	if json.Unmarshal(b, &es) != nil {
		return nil, false
	}
	return es, true
}

func samePorts(a, b []PortSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// changedFields lists the user-meaningful fields that differ between the
// applied snapshot of an entry and its current state. Applier-managed
// fields (TunnelIP / GatewayPubKey) are intentionally ignored so a
// post-apply pubkey fill never looks like an operator edit.
func changedFields(old, cur Entry) []string {
	var f []string
	if old.Name != cur.Name {
		f = append(f, "name")
	}
	if old.Subdomain != cur.Subdomain {
		f = append(f, "DNS name")
	}
	if !samePorts(old.Ports, cur.Ports) {
		f = append(f, "ports")
	}
	if old.TargetIP != cur.TargetIP {
		f = append(f, "target IP")
	}
	if old.Service != cur.Service {
		f = append(f, "service")
	}
	if old.Enabled != cur.Enabled {
		if cur.Enabled {
			f = append(f, "re-enabled")
		} else {
			f = append(f, "disabled")
		}
	}
	return f
}

// diff compares the current entry set to the last-applied snapshot and
// returns per-entry status (new / changed) plus the list of entries that
// would be REMOVED on the next Apply. detail=false when there is no
// snapshot yet (pre-feature or never applied) — the UI then falls back to
// the coarse pending flag.
func (a *API) diff() map[string]any {
	old, have := a.appliedSnap()
	res := map[string]any{"detail": have}
	if !have {
		return res
	}
	byID := map[string]Entry{}
	for _, e := range old {
		byID[e.ID] = e
	}
	perEntry := map[string]any{}
	curIDs := map[string]bool{}
	for _, e := range a.store.List() {
		curIDs[e.ID] = true
		o, ok := byID[e.ID]
		if !ok {
			perEntry[e.ID] = map[string]any{"status": "new"}
			continue
		}
		if cf := changedFields(o, *e); len(cf) > 0 {
			perEntry[e.ID] = map[string]any{"status": "changed", "fields": cf}
		}
	}
	removed := []map[string]any{}
	for _, o := range old {
		if !curIDs[o.ID] {
			removed = append(removed, map[string]any{
				"name": o.Name, "subdomain": o.Subdomain})
		}
	}
	res["perEntry"] = perEntry
	res["removed"] = removed
	return res
}

// dnsRecords lists the A records in a domain's Cloudflare zone (read-only)
// so the UI can offer the operator's REAL subdomains. Degrades gracefully
// when no Cloudflare token is in the environment.
func (a *API) dnsRecords(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"configured": a.cf.Configured()}
	if !a.cf.Configured() {
		out["error"] = "Cloudflare not configured: set CF_API_TOKEN in proxyctl's environment"
		a.writeJSON(w, http.StatusOK, out)
		return
	}
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		out["error"] = "missing ?domain="
		a.writeJSON(w, http.StatusOK, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	recs, err := a.cf.ListA(ctx, domain)
	if err != nil {
		out["error"] = err.Error()
		a.writeJSON(w, http.StatusOK, out)
		return
	}
	out["records"] = recs
	a.writeJSON(w, http.StatusOK, out)
}

// createDNS upserts a grey-cloud A record (fqdn -> droplet IP). Explicit,
// confirmed operator action — an outward-facing mutation.
func (a *API) createDNS(w http.ResponseWriter, r *http.Request) {
	if !a.cf.Configured() {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Cloudflare not configured: set CF_API_TOKEN in proxyctl's environment"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	action, rec, err := a.cf.UpsertA(ctx, body.Name, a.infra.DropletPublicIP)
	if err != nil {
		a.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"action": action, "record": rec, "ip": a.infra.DropletPublicIP})
}

// refreshApplier rebuilds the live applier so the next Apply / stats
// call uses the freshly-saved droplet config. Called after every wizard
// mutation (save IP, generate, regenerate, bootstrap). The legacy
// -droplet-ssh fallback is preserved on the new SSHApplier too.
func (a *API) refreshApplier() {
	if sa, ok := a.applier.(SSHApplier); ok {
		sa.Droplet = a.droplet.Get()
		a.applier = sa
	}
	a.refreshInfra()
}

// refreshInfra overlays the wizard-managed droplet values (IP, WG
// pubkey) on top of DefaultInfra so every render reflects the actual
// droplet the operator just configured / bootstrapped. It also pulls
// the gateway namespace from the downward-API POD_NAMESPACE env, so
// the wg-gw-* Deployments + Secrets render under the live install
// namespace even when ProxyCTL is installed under a renamed ns.
func (a *API) refreshInfra() {
	in := DefaultInfra()
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		in.K8sNamespace = ns
	}
	if cfg := a.droplet.Get(); cfg != nil {
		if cfg.IP != "" {
			in.DropletPublicIP = cfg.IP
		}
		if cfg.WGPublicKey != "" {
			in.DropletPubKey = cfg.WGPublicKey
		}
		// Detected public egress interface (bootstrap + every Apply).
		// Without it the render falls back to DefaultInfra's eth0, which
		// is only right on providers that name it that.
		if cfg.WANIface != "" {
			in.WANIface = cfg.WANIface
		}
	}
	if a.keys != nil {
		in.KeysBasePath = a.keys.BasePath()
		// Empty unless the operator named a share — that's what selects
		// share mode over the derived-StorageClass default.
		in.KeysNFSServer, in.KeysNFSExport, _ = a.keys.Share()
	}
	a.infra = in
}

// getDroplet returns the stored config (never the private key) + a
// derived "configured" flag the wizard uses to switch between its
// install-flow and its summary-flow states.
func (a *API) getDroplet(w http.ResponseWriter, r *http.Request) {
	cfg := a.droplet.Get()
	out := map[string]any{
		"configured": cfg.Configured(),
		"hasKey":     cfg != nil && cfg.PublicKey != "",
	}
	if cfg != nil {
		out["ip"] = cfg.IP
		out["port"] = cfg.port()
		out["user"] = cfg.User
		if out["user"] == "" {
			out["user"] = "root"
		}
		out["publicKey"] = cfg.PublicKey
		out["updatedAt"] = cfg.UpdatedAt
		out["bootstrapped"] = cfg.Bootstrapped
		out["wgPublicKey"] = cfg.WGPublicKey
		out["sshLockedDown"] = cfg.SSHLockedDown
		out["sshAllowedIPs"] = cfg.SSHAllowedIPs
	} else {
		out["user"] = "root"
		out["port"] = 22
	}
	// Legacy CLI fallback still works — surface it so the operator can
	// see why ssh works "for free" before they've touched the wizard.
	if sa, ok := a.applier.(SSHApplier); ok {
		if sa.DropletSSH != "" && !strings.Contains(sa.DropletSSH, "PLACEHOLDER") {
			out["legacyFlag"] = sa.DropletSSH
		}
	}
	a.writeJSON(w, http.StatusOK, out)
}

// generateDroplet creates a fresh keypair if none exists and returns the
// public half so the operator can paste it into the droplet's
// authorized_keys. Idempotent: re-calling without an existing key would
// generate one, but EnsureKeypair short-circuits if one's already there.
// Use regenerateDroplet to force-rotate.
func (a *API) generateDroplet(w http.ResponseWriter, r *http.Request) {
	pub, _, err := a.droplet.EnsureKeypair()
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.refreshApplier()
	a.writeJSON(w, http.StatusOK, map[string]any{"publicKey": pub})
}

// regenerateDroplet rotates the keypair. After this the public key
// installed on the droplet is STALE — the wizard must re-display the new
// pubkey for the operator to re-install. ssh will fail until it's done.
func (a *API) regenerateDroplet(w http.ResponseWriter, r *http.Request) {
	pub, _, err := a.droplet.Regenerate()
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.refreshApplier()
	a.writeJSON(w, http.StatusOK, map[string]any{"publicKey": pub})
}

// saveDroplet persists IP/user/port. The wizard typically calls this
// AFTER generateDroplet, so by the time it's configured the keypair
// already exists and Apply works immediately.
func (a *API) saveDroplet(w http.ResponseWriter, r *http.Request) {
	var body DropletConfig
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if body.Port == 0 {
		body.Port = 22
	}
	if body.User == "" {
		body.User = "root"
	}
	if err := a.droplet.Save(body); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.refreshApplier()
	a.getDroplet(w, r)
}

// testDroplet runs a non-mutating ssh probe (`echo OK; whoami; uname -n`)
// using exactly the args the real Apply will use. Returns the captured
// stdout/stderr so the wizard can show the operator a green check + the
// host's identity (or the real error if the key isn't installed yet).
func (a *API) testDroplet(w http.ResponseWriter, r *http.Request) {
	cfg := a.droplet.Get()
	if !cfg.Configured() {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Droplet not configured: generate a key and save IP first."})
		return
	}
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ssh apply-mode required for Test"})
		return
	}
	// Force the just-saved config (refreshApplier may not have fired yet
	// for this request if a save+test came in racing order from the UI).
	sa.Droplet = cfg
	// Raw (unelevated) on purpose: this step reports what the box IS, so
	// it must show the real login user rather than a sudo'd "root".
	//
	// The privilege line is the useful part. Every later step needs root;
	// DigitalOcean gives it directly, while OVH/AWS/Hetzner log you in as
	// an unprivileged user and expect sudo. Probing it here turns a
	// missing-sudo box into a clear message now, instead of a cryptic
	// "could not get lock /var/lib/dpkg/lock-frontend" mid-bootstrap.
	st := sa.sshDropletRaw("test", "", `echo OK
echo "login-user=$(whoami)"
if [ "$(id -u)" = "0" ]; then
  echo "privilege=root (direct login)"
elif sudo -n true 2>/dev/null; then
  echo "privilege=sudo (passwordless — commands will be elevated)"
else
  echo "privilege=NONE: not root, and passwordless sudo is unavailable."
  echo "  Fix: give this user a NOPASSWD sudoers rule, or log in as root."
fi
uname -n; uptime`)
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok":       st.OK,
		"exitCode": st.ExitCode,
		"stdout":   st.Stdout,
		"stderr":   st.Stderr,
	})
}

// bootstrapDroplet runs the one-time droplet-side prep over SSH:
// installs WireGuard / iptables / conntrack, persists the kernel
// sysctls (ip_forward + the UDP conntrack timeouts that bit
// Satisfactory), generates a WG keypair the FIRST time only, writes
// a minimal /etc/wireguard/wg0.conf, and enables wg-quick@wg0. The
// remote script is fully idempotent — re-running on an already-set-up
// droplet only re-asserts state, never overwrites a live wg0.conf or
// regenerates the WG key. Captures the droplet's WG pubkey from a
// marker line in stdout and persists it via SetWGPubKey so every per-
// game gateway's [Peer] points at the right key on the next Apply.
func (a *API) bootstrapDroplet(w http.ResponseWriter, r *http.Request) {
	cfg := a.droplet.Get()
	if !cfg.Configured() {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Droplet not configured: complete Steps 1–3 first."})
		return
	}
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ssh apply-mode required"})
		return
	}
	sa.Droplet = cfg
	// Optional body { upgrade: true } adds a conservative `apt-get
	// upgrade` step. "Conservative" because plain `upgrade` will NOT
	// install new packages or remove anything — packages that need
	// either are held back. That's deliberate: avoids surprise kernel
	// jumps (reboot needed) and surprise package removals.
	var body struct {
		Upgrade bool `json:"upgrade"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	upgradeBlock := ""
	if body.Upgrade {
		upgradeBlock = `
echo "[2/5] apt upgrade (conservative — held-back packages stay on prior version)..."
apt-get -y -o Dpkg::Options::='--force-confold' upgrade >/dev/null
`
	}

	// One self-contained bash script. Quoting: passed via -c shq() so
	// $-substitutions resolve REMOTE-side. Idempotent everywhere.
	remote := `set -e
echo "=== ProxyCTL droplet bootstrap (idempotent) ==="
export DEBIAN_FRONTEND=noninteractive

echo "[1/5] apt update + install wireguard + iptables + conntrack..."
if command -v apt-get >/dev/null; then
  apt-get update -y >/dev/null
  apt-get install -y --no-install-recommends \
    wireguard wireguard-tools iptables conntrack ca-certificates >/dev/null
else
  echo "  (non-apt distro; assuming wireguard is already installed)"
fi
` + upgradeBlock + `
echo "[3/5] kernel sysctls (ip_forward + udp conntrack timeouts)..."
cat > /etc/sysctl.d/99-proxyctl.conf <<'SYS'
net.ipv4.ip_forward=1
net.netfilter.nf_conntrack_udp_timeout=180
net.netfilter.nf_conntrack_udp_timeout_stream=600
SYS
sysctl --system >/dev/null 2>&1 || true
sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1 || true

echo "[4/5] WireGuard wg0 ..."
mkdir -p /etc/wireguard
chmod 700 /etc/wireguard
if [ ! -f /etc/wireguard/wg0.conf ]; then
  umask 077
  wg genkey > /etc/wireguard/wg0.privkey
  wg pubkey < /etc/wireguard/wg0.privkey > /etc/wireguard/wg0.pubkey
  PRIV=$(cat /etc/wireguard/wg0.privkey)
  cat > /etc/wireguard/wg0.conf <<WG
[Interface]
PrivateKey = ${PRIV}
Address    = 10.8.0.1/24
ListenPort = 51820
WG
  echo "  wg0.conf created"
else
  echo "  wg0.conf already exists — leaving untouched (idempotent re-run)"
  if [ ! -f /etc/wireguard/wg0.pubkey ]; then
    awk '/^PrivateKey/{print $3}' /etc/wireguard/wg0.conf \
      | wg pubkey > /etc/wireguard/wg0.pubkey
  fi
fi

echo "[5/5] enabling wg-quick@wg0..."
systemctl enable --now wg-quick@wg0 >/dev/null
wg show wg0 2>/dev/null | head -5 || true

echo "=== DONE ==="
echo "PROXYCTL_WG_PUBKEY=$(cat /etc/wireguard/wg0.pubkey)"
echo "PROXYCTL_WAN_IFACE=$(ip -o route get 1.1.1.1 2>/dev/null | sed -n 's/.*dev \([^ ]*\).*/\1/p')"
`

	// 10 min upper bound: apt upgrade on a stale image can take a few
	// minutes; the wg-quick + sysctl work is < 5 sec.
	st := sa.sshDropletLong("droplet bootstrap (one-time)", "",
		"bash -c "+shq(remote), 10*time.Minute)

	// Parse the marker lines so the wizard can show the WG pubkey AND
	// the renderer can write it into every gateway's [Peer] block —
	// plus the droplet's detected public interface for the NAT rules.
	pub, wan := "", ""
	for _, ln := range strings.Split(st.Stdout, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "PROXYCTL_WG_PUBKEY="); ok {
			pub = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "PROXYCTL_WAN_IFACE="); ok {
			wan = strings.TrimSpace(v)
		}
	}
	if st.OK && wan != "" {
		_ = a.droplet.SetWANIface(wan)
	}
	if st.OK && pub != "" {
		if err := a.droplet.SetWGPubKey(pub); err != nil {
			st.OK = false
			st.Stderr += "\npersist wgPubKey: " + err.Error()
		}
		a.refreshApplier() // picks up new infra overlay + applier droplet
	}

	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok":          st.OK,
		"exitCode":    st.ExitCode,
		"stdout":      st.Stdout,
		"stderr":      st.Stderr,
		"wgPublicKey": pub,
	})
}

// detectEgressIP discovers the public IP THIS process appears as on the
// internet — i.e., the IP the droplet will see future Apply SSH
// connections coming from. The wizard pre-fills the SSH lockdown list
// with this so the operator doesn't accidentally lock ProxyCTL itself
// out. ipify.org is used because it's free, no-auth, plain text, and
// CORS-friendly. Falls back gracefully on network errors.
func (a *API) detectEgressIP(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		out["error"] = err.Error()
		a.writeJSON(w, http.StatusOK, out)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip) == nil {
		out["error"] = "ipify returned non-IP: " + ip
		a.writeJSON(w, http.StatusOK, out)
		return
	}
	out["egressIP"] = ip
	a.writeJSON(w, http.StatusOK, out)
}

// lockdownSSH restricts the droplet's sshd to ONLY accept connections
// from the specified IPs. Implemented as an sshd_config drop-in (NOT
// iptables) so it persists across reboots and survives the firewall
// rebuilds the WireGuard PostUp does on every wg-quick up.
//
// Safety rails:
//   - Validates every IP locally before touching the droplet.
//   - Refuses if the ProxyCTL egress IP is NOT in the list (would
//     immediately lock the app out of its own droplet — explicit
//     `force=true` in the request body overrides for power users).
//   - Validates the new sshd config with `sshd -t` BEFORE reload.
//   - After reload, re-tests the existing SSH connection: if it dies,
//     the drop-in is removed and sshd reloaded again. Worst case:
//     wizard step fails loudly, droplet is exactly where it started.
func (a *API) lockdownSSH(w http.ResponseWriter, r *http.Request) {
	cfg := a.droplet.Get()
	if !cfg.Configured() {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Droplet not configured."})
		return
	}
	var body struct {
		IPs   []string `json:"ips"`
		Force bool     `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	// Validate each IP / CIDR.
	cleaned := make([]string, 0, len(body.IPs))
	for _, raw := range body.IPs {
		ip := strings.TrimSpace(raw)
		if ip == "" {
			continue
		}
		if !strings.Contains(ip, "/") {
			if net.ParseIP(ip) == nil {
				a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a valid IP/CIDR: " + ip})
				return
			}
		} else if _, _, err := net.ParseCIDR(ip); err != nil {
			a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a valid IP/CIDR: " + ip})
			return
		}
		cleaned = append(cleaned, ip)
	}
	if len(cleaned) == 0 {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one IP/CIDR required"})
		return
	}
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ssh apply-mode required"})
		return
	}
	sa.Droplet = cfg

	// Render the drop-in. Match by negative-IP list: deny when the
	// source is NOT one of our allow-listed IPs. (`!` only works on
	// per-IP entries — globs/CIDR negatives are limited, so for CIDR
	// we use the inverse approach: allow by default then a Match block
	// for non-listed addresses denies.)
	allow := strings.Join(cleaned, ",")
	notAllow := make([]string, 0, len(cleaned))
	for _, ip := range cleaned {
		notAllow = append(notAllow, "!"+ip)
	}
	dropin := "# Managed by ProxyCTL. Restricts SSH to the operator-listed IPs.\n" +
		"# Remove this file (or rerun the lockdown step) to revert.\n" +
		"#\n" +
		"# Allowed: " + allow + "\n" +
		"Match Address " + strings.Join(notAllow, ",") + "\n" +
		"    DenyUsers *\n"

	remote := "set -e\n" +
		"mkdir -p /etc/ssh/sshd_config.d\n" +
		"cat > /etc/ssh/sshd_config.d/99-proxyctl-lockdown.conf <<'PROXYCTLEOF'\n" +
		dropin +
		"PROXYCTLEOF\n" +
		"chmod 600 /etc/ssh/sshd_config.d/99-proxyctl-lockdown.conf\n" +
		"# Validate BEFORE reload — sshd -t exits non-zero if the merged config is bad.\n" +
		"sshd -t\n" +
		"systemctl reload ssh 2>/dev/null || systemctl reload sshd\n" +
		"echo 'PROXYCTL_LOCKDOWN_APPLIED'\n"

	st := sa.sshDropletLong("ssh lockdown: write + reload", "",
		"bash -c "+shq(remote), 45*time.Second)

	if !st.OK {
		a.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "exitCode": st.ExitCode,
			"stdout": st.Stdout, "stderr": st.Stderr,
		})
		return
	}

	// Verify a fresh ssh connection still works from this process.
	// If it doesn't, revert immediately — we'd otherwise be locked out.
	verify := sa.sshDroplet("ssh lockdown: verify our own access", "", "true")
	if !verify.OK && !body.Force {
		rev := sa.sshDroplet("ssh lockdown: REVERT (verify failed)", "",
			"rm -f /etc/ssh/sshd_config.d/99-proxyctl-lockdown.conf && (systemctl reload ssh 2>/dev/null || systemctl reload sshd)")
		a.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "reverted": rev.OK,
			"error":  "Verification SSH failed after lockdown — ProxyCTL is not on the allow-list. Reverted (no change to droplet). Re-run with the egress IP included.",
			"stdout": st.Stdout + "\n\n[verify]\n" + verify.Stdout,
			"stderr": st.Stderr + "\n\n[verify]\n" + verify.Stderr,
		})
		return
	}

	_ = a.droplet.SetSSHLockdown(true, cleaned)
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "ips": cleaned,
		"stdout": st.Stdout,
	})
}

// unlockSSH removes the lockdown drop-in. Idempotent. Useful from the
// admin Setup page if the operator's home IP changes.
func (a *API) unlockSSH(w http.ResponseWriter, r *http.Request) {
	cfg := a.droplet.Get()
	if !cfg.Configured() {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Droplet not configured."})
		return
	}
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ssh apply-mode required"})
		return
	}
	sa.Droplet = cfg
	st := sa.sshDroplet("ssh lockdown: remove", "",
		"rm -f /etc/ssh/sshd_config.d/99-proxyctl-lockdown.conf && (systemctl reload ssh 2>/dev/null || systemctl reload sshd) && echo 'PROXYCTL_LOCKDOWN_REMOVED'")
	if st.OK {
		_ = a.droplet.SetSSHLockdown(false, nil)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok": st.OK, "stdout": st.Stdout, "stderr": st.Stderr,
	})
}

// cfStatus is the wizard-facing read-only view of the Cloudflare
// connection: configured / not, and (if configured) what zones the
// token can see + its raw verify status. NEVER includes the token.
func (a *API) cfStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"configured": a.cf.Configured()}
	if !a.cf.Configured() {
		a.writeJSON(w, http.StatusOK, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if v, err := a.cf.Verify(ctx); err == nil {
		out["verified"] = true
		out["status"] = v.Status
		if v.ExpiresOn != "" {
			out["expiresOn"] = v.ExpiresOn
		}
	} else {
		out["verified"] = false
		out["error"] = err.Error()
	}
	if zs, err := a.cf.AccessibleZones(ctx); err == nil {
		zones := make([]string, 0, len(zs))
		for _, z := range zs {
			zones = append(zones, z.Name)
		}
		out["zones"] = zones
	}
	a.writeJSON(w, http.StatusOK, out)
}

// cfSaveToken accepts a freshly-pasted token, VERIFIES it against
// Cloudflare's /user/tokens/verify, and only persists if active. The
// token is written 0600 to the data PVC and immediately swapped in
// for live use (no restart needed). It is NEVER returned by any GET.
func (a *API) cfSaveToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	tok := strings.TrimSpace(body.Token)
	if tok == "" {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty token"})
		return
	}
	// Verify BEFORE persisting — try it on a temp CF client so a bad
	// token doesn't replace a good one already in memory.
	probe := &CF{token: tok, http: a.cf.http, zoneID: map[string]string{}}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	v, err := probe.Verify(ctx)
	if err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Cloudflare rejected the token: " + err.Error()})
		return
	}
	if v.Status != "active" {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Token verify returned status=" + v.Status})
		return
	}
	if err := os.WriteFile(a.cfTokenPath, []byte(tok), 0o600); err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.cf.SetToken(tok)
	a.cfStatus(w, r) // return the fresh status (zones, expiry, etc.)
}

// cfTest is "verify the live token now" — used as a re-check button
// after the operator's network or token has potentially changed.
func (a *API) cfTest(w http.ResponseWriter, r *http.Request) {
	if !a.cf.Configured() {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no token set"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	v, err := a.cf.Verify(ctx)
	if err != nil {
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok": v.Status == "active", "status": v.Status, "expiresOn": v.ExpiresOn,
	})
}

// cfDeleteToken removes the persisted token and deconfigures the live
// client. Apply will silently stop touching DNS until a new token is
// saved (same graceful-degrade as the no-token case).
func (a *API) cfDeleteToken(w http.ResponseWriter, r *http.Request) {
	_ = os.Remove(a.cfTokenPath)
	a.cf.SetToken("")
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "configured": false})
}

// listDomains is the unified domain list the entry-form's DNS dropdown
// reads from. Sources, in priority order:
//   - Cloudflare zones the configured CF token can see (auto-discovered)
//   - Manually-added domains in domains.json (for non-CF DNS providers)
//
// Deduped; CF-sourced entries are labeled so the UI can show "auto"
// vs "manual" and hide the delete button on auto ones.
func (a *API) listDomains(w http.ResponseWriter, r *http.Request) {
	manual := a.domains.List()
	out := map[string]any{
		"manual": manual,
		"auto":   []string{},
	}
	if a.cf.Configured() {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if zs, err := a.cf.AccessibleZones(ctx); err == nil {
			auto := make([]string, 0, len(zs))
			for _, z := range zs {
				auto = append(auto, z.Name)
			}
			out["auto"] = auto
		}
	}
	// Merge (auto first, then manual, dedup).
	seen := map[string]bool{}
	merged := []string{}
	for _, ds := range [][]string{out["auto"].([]string), manual} {
		for _, d := range ds {
			if !seen[d] {
				seen[d] = true
				merged = append(merged, d)
			}
		}
	}
	out["domains"] = merged
	a.writeJSON(w, http.StatusOK, out)
}

func (a *API) addDomain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if err := a.domains.Add(body.Domain); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"domains": a.domains.List()})
}

func (a *API) delDomain(w http.ResponseWriter, r *http.Request) {
	if err := a.domains.Delete(r.PathValue("domain")); err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"domains": a.domains.List()})
}

func (a *API) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// liveClusterIP resolves an entry's "name.namespace" Service label to its
// CURRENT ClusterIP via the read-only ambient kubectl. Used to detect
// "target drift" — e.g. the operator recreated the Service so its
// ClusterIP changed and the entry/gateway now point at a dead IP.
func (a *API) liveClusterIP(svc string) (ip string, found bool) {
	i := strings.Index(svc, ".")
	if i <= 0 || !a.kube.available() {
		return "", false
	}
	name, ns := svc[:i], svc[i+1:]
	b, err := a.kube.kubectl("get", "svc", name, "-n", ns, "-o",
		"jsonpath={.spec.clusterIP}")
	cip := strings.TrimSpace(string(b))
	if err != nil || cip == "" || cip == "None" {
		return "", false
	}
	return cip, true
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	entries := a.store.List()
	d := a.diff()
	perEntry, _ := d["perEntry"].(map[string]any)
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		row := map[string]any{"entry": e}
		if ch, ok := perEntry[e.ID]; ok {
			row["change"] = ch
		}
		if strings.Contains(e.Service, ".") {
			if cip, ok := a.liveClusterIP(e.Service); ok {
				row["drift"] = map[string]any{
					"live": cip, "found": true, "mismatch": cip != e.TargetIP}
			} else {
				// Service label set but not resolvable → it's gone/renamed.
				row["drift"] = map[string]any{"found": false, "mismatch": true}
			}
		}
		out = append(out, row)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"entries": out,
		"mode":    a.applier.Mode(),
		"pending": a.pending(),
		"cfReady": a.cf.Configured(),
		"diff":    d,
		"setup":   a.setupProgress(len(entries)),
	})
}

// rebind re-resolves an entry's Service to its current ClusterIP and
// updates the entry's targetIP (marks pending → Apply rebuilds just that
// game's gateway). Fixes the "I recreated the kube Service/pod and the
// proxy now points at a dead ClusterIP" case.
func (a *API) rebind(w http.ResponseWriter, r *http.Request) {
	e, ok := a.store.Get(r.PathValue("id"))
	if !ok {
		a.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	cip, found := a.liveClusterIP(e.Service)
	if !found {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "could not resolve a live ClusterIP for Service " +
				strconv.Quote(e.Service) + " — check the Service exists / set it via the cluster picker"})
		return
	}
	from := e.TargetIP
	if from == cip {
		a.writeJSON(w, http.StatusOK, map[string]any{
			"entry": e, "rebound": false, "message": "already on the live ClusterIP " + cip})
		return
	}
	e.TargetIP = cip
	if err := a.store.Put(e); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"entry": e, "rebound": true,
		"from": from, "to": cip, "message": "rebound " + e.Name + " " + from + " → " + cip + " — Apply to push"})
}

// rendered returns the review bundle (droplet wg0.conf + gateway manifest +
// runbook + summary) for the current entry set. Pure, no side effects.
func (a *API) rendered(w http.ResponseWriter, r *http.Request) {
	out := a.applier.Render(a.infra, a.store.List())
	a.writeJSON(w, http.StatusOK, out)
}

// apply is the explicit operator action. In ssh mode it shells out to the
// operator's ambient ssh + kubectl; in manual mode it just echoes the
// runbook. Never runs in the background — only on this request.
// apply kicks off the apply in the background and returns immediately.
// Progress is observed via GET /api/apply/status (polled by the UI), so
// it survives a page refresh.
func (a *API) apply(w http.ResponseWriter, r *http.Request) {
	a.jobMu.Lock()
	if a.job != nil && a.job.Running {
		a.jobMu.Unlock()
		a.writeJSON(w, http.StatusConflict, map[string]any{"running": true, "message": "an apply is already in progress"})
		return
	}
	a.job = &ApplyJob{Running: true, StartedAt: time.Now().Unix(), Message: "applying…"}
	a.jobMu.Unlock()
	a.saveJob()

	// onStep appends each completed step to the live job state so the
	// poller (and a refreshed page) sees progress as it happens.
	onStep := func(st Step) {
		a.jobMu.Lock()
		if a.job != nil {
			a.job.Steps = append(a.job.Steps, st)
		}
		a.jobMu.Unlock()
		a.saveJob()
	}

	go func() {
		var res ApplyResult
		if sa, ok := a.applier.(SSHApplier); ok {
			// Refresh the droplet's detected public egress interface before
			// rendering: the NAT rules bind to it, and a provider-renamed
			// iface (ens3/enX0 vs eth0) silently keeps public traffic out
			// of the PROXYCTL chains. Best-effort — an ssh hiccup keeps the
			// stored value and the apply proceeds.
			if sa.dropletReady() {
				st := sa.sshDropletRaw("detect public interface", "",
					"ip -o route get 1.1.1.1 2>/dev/null | sed -n 's/.*dev \\([^ ]*\\).*/\\1/p'")
				cur := a.droplet.Get()
				if wan := strings.TrimSpace(st.Stdout); st.OK && wan != "" &&
					(cur == nil || wan != cur.WANIface) {
					_ = a.droplet.SetWANIface(wan)
					a.refreshInfra()
				}
				onStep(st)
			}
			persist := func(id, pk string) {
				if e, ok := a.store.Get(id); ok {
					e.GatewayPubKey = pk
					_ = a.store.Put(e)
				}
			}
			res = sa.ApplyPerGame(a.infra, a.store.List(), persist, onStep)
		} else {
			rb := a.applier.Render(a.infra, a.store.List())
			res = a.applier.Apply(a.infra, rb)
			for _, st := range res.Steps {
				onStep(st)
			}
		}

		// Fold in Cloudflare DNS (idempotent grey-cloud upsert per entry).
		if res.OK && a.cf.Configured() {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			for _, e := range a.store.List() {
				if !e.Enabled || !strings.Contains(e.Subdomain, ".") {
					continue
				}
				st := Step{Name: "dns: " + e.Subdomain, Cmd: "cloudflare upsert A"}
				action, rec, err := a.cf.UpsertA(ctx, e.Subdomain, a.infra.DropletPublicIP)
				if err != nil {
					st.OK, st.ExitCode, st.Stderr = false, 1, err.Error()
				} else {
					st.OK, st.Stdout = true, action+" "+rec.Name+" -> "+rec.Content+" (DNS only)"
				}
				onStep(st)
			}
			cancel()
		}

		// Web apps: reconcile the Cloudflare Tunnel — ensure the tunnel,
		// (re)deploy the cloudflared connector, push the ingress rules,
		// and upsert a proxied CNAME per hostname. Independent of the
		// droplet/gateway apply above; web apps never touch the droplet.
		if routes := a.webroutes.List(); len(routes) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			for _, st := range applyWebRoutes(ctx, a.cf, a.infra.K8sNamespace, routes) {
				onStep(st)
				if !st.OK {
					res.OK = false
				}
			}
			cancel()
		}

		if res.OK {
			a.markApplied()
		}
		a.jobMu.Lock()
		if a.job != nil {
			a.job.Running = false
			a.job.OK = res.OK
			a.job.Message = res.Message
			a.job.EndedAt = time.Now().Unix()
		}
		a.jobMu.Unlock()
		a.saveJob()
	}()

	a.writeJSON(w, http.StatusAccepted, map[string]any{"running": true, "started": true})
}

// stats returns per-entry live tunnel usage: cumulative bytes + last
// handshake from `wg show wg0 dump` (keyed by the entry's gateway public
// key) and an active-connection count from /proc/net/nf_conntrack on the
// droplet (no extra tooling needed). The UI polls this and derives a live
// rate from successive samples.
func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	sa, ok := a.applier.(SSHApplier)
	if !ok || !sa.dropletReady() {
		a.writeJSON(w, http.StatusOK, map[string]any{"stats": []any{}, "note": "droplet not configured — open Setup → Droplet"})
		return
	}
	st := sa.sshDroplet("stats", "",
		"wg show wg0 dump; echo '==CT=='; (conntrack -L 2>/dev/null || cat /proc/net/nf_conntrack 2>/dev/null || echo __NOCT__)")
	dump, ct, _ := strings.Cut(st.Stdout, "==CT==")
	// Connection tracking isn't always observable: this droplet's kernel
	// has no /proc/net/nf_conntrack and no `conntrack` CLI. Detect that
	// and report connections as unknown (-1) rather than a misleading 0.
	ctOK := !strings.Contains(ct, "__NOCT__") && strings.TrimSpace(ct) != ""
	type wgp struct{ rx, tx, hs int64 }
	peers := map[string]wgp{}
	for i, ln := range strings.Split(dump, "\n") {
		f := strings.Split(ln, "\t")
		if i == 0 || len(f) < 7 { // line 0 = interface
			continue
		}
		hs, _ := strconv.ParseInt(f[4], 10, 64)
		rx, _ := strconv.ParseInt(f[5], 10, 64)
		tx, _ := strconv.ParseInt(f[6], 10, 64)
		peers[f[0]] = wgp{rx, tx, hs}
	}
	now := time.Now().Unix()
	out := []map[string]any{}
	for _, e := range a.store.List() {
		conns := -1 // -1 = unknown (no conntrack visibility)
		if ctOK {
			conns = 0
			for _, p := range e.Ports {
				conns += strings.Count(ct, fmt.Sprintf("dport=%d ", p.Port))
			}
		}
		m := map[string]any{"id": e.ID, "name": e.Name, "connections": conns,
			"rxBytes": 0, "txBytes": 0, "lastHandshake": 0, "online": false}
		if p, ok := peers[e.GatewayPubKey]; ok && e.GatewayPubKey != "" {
			m["rxBytes"], m["txBytes"], m["lastHandshake"] = p.rx, p.tx, p.hs
			m["online"] = p.hs > 0 && now-p.hs < 200
		}
		out = append(out, m)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"stats": out})
}

func (a *API) decode(w http.ResponseWriter, r *http.Request) (*Entry, bool) {
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return nil, false
	}
	e.Name = strings.TrimSpace(e.Name)
	e.Subdomain = strings.TrimSpace(e.Subdomain)
	e.TargetIP = strings.TrimSpace(e.TargetIP)
	e.Service = strings.TrimSpace(e.Service)
	return &e, true
}

// respondSaved persists the change and returns the entry plus the freshly
// rendered configs. It does NOT push — the operator clicks Apply for that.
func (a *API) respondSaved(w http.ResponseWriter, e *Entry) {
	a.writeJSON(w, http.StatusOK, map[string]any{
		"entry":    e,
		"rendered": a.applier.Render(a.infra, a.store.List()),
	})
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	e, ok := a.decode(w, r)
	if !ok {
		return
	}
	e.ID = newID()
	if err := a.store.Put(e); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.respondSaved(w, e)
}

func (a *API) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.store.Get(id); !ok {
		a.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	e, ok := a.decode(w, r)
	if !ok {
		return
	}
	e.ID = id
	if err := a.store.Put(e); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.respondSaved(w, e)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e, ok := a.store.Get(id)
	if !ok {
		a.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err := a.store.Delete(id); err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Optional, explicit (?cf=1): also delete the Cloudflare A record.
	// The UI gates this behind its own confirmation — it's an outward
	// mutation of the operator's live zone.
	dns := ""
	if r.URL.Query().Get("cf") == "1" && a.cf.Configured() && strings.Contains(e.Subdomain, ".") {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if del, err := a.cf.DeleteA(ctx, e.Subdomain); err != nil {
			dns = "DNS record NOT removed: " + err.Error()
		} else if del {
			dns = "DNS record " + e.Subdomain + " removed from Cloudflare"
		} else {
			dns = "no DNS record for " + e.Subdomain + " (already absent)"
		}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"status": "deleted",
		"dns":    dns,
	})
}

func (a *API) toggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e, ok := a.store.Get(id)
	if !ok {
		a.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	e.Enabled = !e.Enabled
	if err := a.store.Put(e); err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.respondSaved(w, e)
}
