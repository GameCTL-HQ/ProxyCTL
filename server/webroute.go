package main

// Web apps — the L7 counterpart to port/game Entries.
//
// A port/game Entry is raw L4: droplet port → WireGuard tunnel → gateway
// pod → target ClusterIP. A WebRoute instead is exposed through a
// Cloudflare Tunnel: cloudflared runs in-cluster, dials out to
// Cloudflare's edge, and routes each public hostname straight to an
// in-cluster Service. Cloudflare's edge terminates TLS and runs
// WAF/DDoS — no droplet, no public IP, no certs on our side.
//
// A WebRoute apply (see applyWebRoutes): ensure the tunnel exists, deploy
// the cloudflared connector, push the ingress rules to Cloudflare, and
// upsert a proxied CNAME per hostname → the tunnel.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// WebRoute is one HTTP host → in-cluster Service mapping.
type WebRoute struct {
	ID        string `json:"id"`
	Hostname  string `json:"hostname"`  // e.g. jellyfin.examplelabs.cc
	Namespace string `json:"namespace"` // backend Service's namespace
	Service   string `json:"service"`   // backend Service name
	Port      int    `json:"port"`      // backend Service port
	Enabled   bool   `json:"enabled"`
}

func (w *WebRoute) validate() error {
	w.Hostname = strings.TrimSpace(strings.ToLower(w.Hostname))
	w.Namespace = strings.TrimSpace(w.Namespace)
	w.Service = strings.TrimSpace(w.Service)
	if w.Hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	if len(w.Hostname) > 253 || !strings.Contains(w.Hostname, ".") {
		return fmt.Errorf("hostname %q is not a valid FQDN", w.Hostname)
	}
	for _, r := range w.Hostname {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return fmt.Errorf("hostname %q has an illegal character", w.Hostname)
		}
	}
	if w.Namespace == "" {
		return fmt.Errorf("backend namespace is required")
	}
	if w.Service == "" {
		return fmt.Errorf("backend service is required")
	}
	if !dns1123(w.Namespace) || !dns1123(w.Service) {
		return fmt.Errorf("namespace/service must be valid Kubernetes names")
	}
	if w.Port < 1 || w.Port > 65535 {
		return fmt.Errorf("port %d out of range", w.Port)
	}
	return nil
}

// serviceURL is the in-cluster URL cloudflared forwards this route to.
func (w *WebRoute) serviceURL() string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", w.Service, w.Namespace, w.Port)
}

// dns1123 reports whether s is a valid lowercase DNS-1123 name (kube
// namespace + Service names).
func dns1123(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// WebRouteStore persists web routes to a JSON file (mirrors Store).
type WebRouteStore struct {
	path string
	mu   sync.RWMutex
	data map[string]*WebRoute
}

func NewWebRouteStore(path string) (*WebRouteStore, error) {
	s := &WebRouteStore{path: path, data: map[string]*WebRoute{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var list []*WebRoute
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, w := range list {
		s.data[w.ID] = w
	}
	return s, nil
}

func (s *WebRouteStore) flushLocked() error {
	list := make([]*WebRoute, 0, len(s.data))
	for _, w := range s.data {
		list = append(list, w)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Hostname < list[j].Hostname })
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

// List returns web routes sorted by hostname (stable for UI + apply).
func (s *WebRouteStore) List() []*WebRoute {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*WebRoute, 0, len(s.data))
	for _, w := range s.data {
		cp := *w
		list = append(list, &cp)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Hostname < list[j].Hostname })
	return list
}

func (s *WebRouteStore) Get(id string) (*WebRoute, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.data[id]
	if !ok {
		return nil, false
	}
	cp := *w
	return &cp, true
}

// Put validates + upserts a web route. Rejects a duplicate hostname.
func (s *WebRouteStore) Put(w *WebRoute) error {
	if err := w.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ex := range s.data {
		if id != w.ID && ex.Hostname == w.Hostname {
			return fmt.Errorf("hostname %q already has a route", w.Hostname)
		}
	}
	s.data[w.ID] = w
	return s.flushLocked()
}

func (s *WebRouteStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return s.flushLocked()
}

// --- API handlers (methods on *API; the type lives in main.go) ----------

func (a *API) listWebRoutes(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]any{
		"routes":       a.webroutes.List(),
		"cfConfigured": a.cf.Configured(),
	})
}

// decodeWebRoute reads + trims a WebRoute from the request body.
func (a *API) decodeWebRoute(w http.ResponseWriter, r *http.Request) (*WebRoute, bool) {
	var wr WebRoute
	if err := json.NewDecoder(r.Body).Decode(&wr); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return nil, false
	}
	return &wr, true
}

func (a *API) createWebRoute(w http.ResponseWriter, r *http.Request) {
	wr, ok := a.decodeWebRoute(w, r)
	if !ok {
		return
	}
	wr.ID = newID()
	if err := a.webroutes.Put(wr); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"route": wr})
}

func (a *API) updateWebRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.webroutes.Get(id); !ok {
		a.writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such web route"})
		return
	}
	wr, ok := a.decodeWebRoute(w, r)
	if !ok {
		return
	}
	wr.ID = id
	if err := a.webroutes.Put(wr); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"route": wr})
}

func (a *API) deleteWebRoute(w http.ResponseWriter, r *http.Request) {
	if err := a.webroutes.Delete(r.PathValue("id")); err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) toggleWebRoute(w http.ResponseWriter, r *http.Request) {
	wr, ok := a.webroutes.Get(r.PathValue("id"))
	if !ok {
		a.writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such web route"})
		return
	}
	wr.Enabled = !wr.Enabled
	if err := a.webroutes.Put(wr); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"route": wr})
}

// tunnelStatus: GET /api/tunnel/status — does the cloudflared connector
// exist + have ready replicas, and is Cloudflare configured. Reads only
// the Deployment status (the namespaced Role already covers that).
func (a *API) tunnelStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "-n", a.infra.K8sNamespace,
		"get", "deploy", cloudflaredName, "-o", "jsonpath={.status.readyReplicas}")
	out, _ := cmd.Output()
	ready := strings.TrimSpace(string(out))
	a.writeJSON(w, http.StatusOK, map[string]any{
		"cfConfigured":     a.cf.Configured(),
		"connectorPresent": ready != "",
		"cloudflaredReady": ready != "" && ready != "0",
	})
}

// tunnelSetup: POST /api/tunnel/setup — runs the full Cloudflare Tunnel
// reconcile on demand (ensure tunnel, deploy cloudflared, push routes,
// CNAMEs). The wizard's "Set up Cloudflare Tunnel" button calls this so
// the tunnel is live before the operator's first Apply.
func (a *API) tunnelSetup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	steps := applyWebRoutes(ctx, a.cf, a.infra.K8sNamespace, a.webroutes.List())
	ok := true
	for _, s := range steps {
		if !s.OK {
			ok = false
			// Log the real failure so it's visible in `kubectl logs`
			// too, not only in the wizard's response.
			slog.Warn("tunnel setup step failed", "step", s.Name, "stderr", s.Stderr)
		}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "steps": steps})
}

// --- apply: Cloudflare Tunnel reconcile ----------------------------------

// kubectlApplyStdin pipes a manifest into `kubectl apply -f -`.
func kubectlApplyStdin(ctx context.Context, manifest string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// applyWebRoutes reconciles the Cloudflare Tunnel for all web routes:
// ensure the tunnel, (re)deploy the cloudflared connector, push the
// ingress rules for enabled routes, and upsert a proxied CNAME per
// hostname → the tunnel. Returns one Step per action for the apply log.
func applyWebRoutes(ctx context.Context, cf *CF, ns string, routes []*WebRoute) []Step {
	if !cf.Configured() {
		return []Step{{
			Name: "web: cloudflare", Cmd: "cloudflare tunnel", OK: false, ExitCode: 1,
			Stderr: "Cloudflare not configured — web routes need a CF API token (Setup → Cloudflare).",
		}}
	}

	// 1. Ensure the tunnel exists + fetch the connector token.
	tokStep := Step{Name: "web: cloudflare tunnel", Cmd: "cloudflare cfd_tunnel"}
	ti, err := cf.EnsureTunnel(ctx)
	var token string
	if err == nil {
		token, err = cf.TunnelToken(ctx, ti)
	}
	if err != nil {
		tokStep.OK, tokStep.ExitCode, tokStep.Stderr = false, 1, err.Error()
		return []Step{tokStep}
	}
	tokStep.OK, tokStep.Stdout = true, "tunnel '"+cfTunnelName+"' ready ("+ti.ID+")"
	steps := []Step{tokStep}

	// 2. Deploy the cloudflared connector into ProxyCTL's namespace.
	cdStep := Step{Name: "web: cloudflared connector", Cmd: "kubectl apply deploy/" + cloudflaredName}
	if err := ensureCloudflared(ctx, ns, token); err != nil {
		cdStep.OK, cdStep.ExitCode, cdStep.Stderr = false, 1, err.Error()
		return append(steps, cdStep)
	}
	cdStep.OK, cdStep.Stdout = true, "connector applied in "+ns
	steps = append(steps, cdStep)

	// 3. Push the ingress rules (enabled routes only).
	var ingress []TunnelIngress
	for _, wr := range routes {
		if wr.Enabled {
			ingress = append(ingress, TunnelIngress{Hostname: wr.Hostname, Service: wr.serviceURL()})
		}
	}
	cfgStep := Step{Name: "web: tunnel routes", Cmd: "cloudflare set tunnel config"}
	if err := cf.SetTunnelConfig(ctx, ti, ingress); err != nil {
		cfgStep.OK, cfgStep.ExitCode, cfgStep.Stderr = false, 1, err.Error()
		return append(steps, cfgStep)
	}
	cfgStep.OK, cfgStep.Stdout = true, fmt.Sprintf("%d route(s) published", len(ingress))
	steps = append(steps, cfgStep)

	// 4. Proxied CNAME per enabled hostname → <tunnel-id>.cfargotunnel.com.
	for _, wr := range routes {
		if !wr.Enabled {
			continue
		}
		st := Step{Name: "dns: " + wr.Hostname, Cmd: "cloudflare upsert CNAME"}
		action, err := cf.UpsertCNAME(ctx, wr.Hostname, ti.CNAME)
		if err != nil {
			st.OK, st.ExitCode, st.Stderr = false, 1, err.Error()
		} else {
			st.OK, st.Stdout = true, action+" "+wr.Hostname+" -> "+ti.CNAME
		}
		steps = append(steps, st)
	}
	return steps
}
