package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// KubeBrowser is the READ-ONLY, on-demand cluster picker behind the kube
// endpoints. It shells out to the system `kubectl` exactly like SSHApplier
// shells out to ssh/kubectl — using the OPERATOR'S AMBIENT kubeconfig /
// in-cluster service account already in proxyctl's process environment.
//
// SECURITY MODEL (unchanged): proxyctl stores ZERO credentials. It never
// holds a kubeconfig of its own, never persists cluster data, and never
// talks to the cluster in the background. Every call here happens only
// because the operator opened the picker in the (loopback + token) UI, and
// every kubectl invocation is a read verb (get) only. If kubectl or ambient
// access is missing the handlers degrade gracefully so the UI can fall back
// to the manual ClusterIP field.
//
// INTERNAL-ONLY (hard constraint): the Kubernetes API is reached ONLY via
// whatever internal kubeconfig / in-cluster context is already ambient
// where proxyctl runs (operator workstation on the LAN, or an in-cluster
// host). proxyctl never requires, assumes, or creates external/public
// reachability to the kube API and never tunnels/exposes/punches kube
// access outward. The picker is purely an operator convenience: when
// proxyctl is run somewhere WITHOUT internal cluster access it must
// degrade to the manual target-IP field, never try to reach the API across
// the internet. This is wholly separate from the droplet/WireGuard data
// path — the public droplet only ever sees game UDP/TCP forwarded into the
// tunnel and never talks to the kube API. proxyctl is an internal
// operator tool and is NOT designed to run on the public droplet.
type KubeBrowser struct {
	// KubeContext mirrors SSHApplier.KubeContext (-kube-context). Empty =
	// the ambient current context / in-cluster config.
	KubeContext string
	Timeout     time.Duration
}

func (k KubeBrowser) timeout() time.Duration {
	if k.Timeout == 0 {
		return 15 * time.Second
	}
	return k.Timeout
}

// kubectl runs a read-only kubectl command with the operator's ambient
// environment (that is how the kubeconfig / in-cluster SA is "borrowed"
// without proxyctl ever holding a credential). Returns stdout, or an error
// whose message is safe to surface to the operator-only UI.
func (k KubeBrowser) kubectl(args ...string) ([]byte, error) {
	full := []string{}
	if k.KubeContext != "" {
		full = append(full, "--context", k.KubeContext)
	}
	full = append(full, args...)

	ctx, cancel := context.WithTimeout(context.Background(), k.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "kubectl", full...)
	cmd.Env = os.Environ() // ambient creds, never stored
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("kubectl timed out after %s", k.timeout())
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return []byte(out.String()), nil
}

// available reports whether a kubectl binary is even on PATH. Used to give
// the UI a clear "kube picker unavailable, use manual IP" signal instead of
// a confusing per-call error.
func (k KubeBrowser) available() bool {
	_, err := exec.LookPath("kubectl")
	return err == nil
}

// ---- response shapes (kept minimal; only what the picker needs) ----

type kubePort struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"` // TCP | UDP
}

type kubeService struct {
	Name      string     `json:"name"`
	Namespace string     `json:"namespace"`
	Type      string     `json:"type"`
	ClusterIP string     `json:"clusterIP"`
	Ports     []kubePort `json:"ports"`
	Selector  string     `json:"selector"`  // human-readable label selector
	Ready     string     `json:"ready"`     // e.g. "2/2 ready", "0/1 ready", "no selector"
	ReadyOK   bool       `json:"readyOK"`   // all backing pods ready & >0
}

func kubeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error":     msg,
		"kubeReady": false,
	})
}

// namespaces: GET /api/kube/namespaces -> {"namespaces":[...]}
func (k KubeBrowser) namespaces(w http.ResponseWriter, r *http.Request) {
	if !k.available() {
		kubeErr(w, http.StatusServiceUnavailable,
			"kubectl not found on PATH — kube picker unavailable; use the manual ClusterIP field.")
		return
	}
	out, err := k.kubectl("get", "namespaces", "-o", "json")
	if err != nil {
		kubeErr(w, http.StatusBadGateway,
			"cluster not reachable from here — internal access required. "+
				"proxyctl only uses the ambient internal kubeconfig/in-cluster "+
				"context and never reaches the kube API over the internet. "+
				"Run proxyctl somewhere with LAN/in-cluster access, or use the "+
				"manual ClusterIP field. ("+err.Error()+")")
		return
	}
	var lst struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &lst); err != nil {
		kubeErr(w, http.StatusBadGateway, "unexpected kubectl output: "+err.Error())
		return
	}
	names := make([]string, 0, len(lst.Items))
	for _, it := range lst.Items {
		names = append(names, it.Metadata.Name)
	}
	sort.Strings(names)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"namespaces": names,
		"kubeReady":  true,
	})
}

// services: GET /api/kube/services?ns=NS -> {"services":[...]}
// For each Service we also resolve backing pod readiness so the operator can
// see it is a live workload before pinning a tunnel at it.
func (k KubeBrowser) services(w http.ResponseWriter, r *http.Request) {
	ns := strings.TrimSpace(r.URL.Query().Get("ns"))
	if ns == "" {
		kubeErr(w, http.StatusBadRequest, "missing ?ns= namespace")
		return
	}
	if !k.available() {
		kubeErr(w, http.StatusServiceUnavailable,
			"kubectl not found on PATH — kube picker unavailable; use the manual ClusterIP field.")
		return
	}

	out, err := k.kubectl("get", "services", "-n", ns, "-o", "json")
	if err != nil {
		kubeErr(w, http.StatusBadGateway,
			"cluster not reachable from here — internal access required. "+
				"Could not list services in "+ns+" via the ambient internal "+
				"kubeconfig/in-cluster context: "+err.Error()+
				" — use the manual ClusterIP field.")
		return
	}
	var svcList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Type      string            `json:"type"`
				ClusterIP string            `json:"clusterIP"`
				Selector  map[string]string `json:"selector"`
				Ports     []struct {
					Name     string `json:"name"`
					Port     int    `json:"port"`
					Protocol string `json:"protocol"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &svcList); err != nil {
		kubeErr(w, http.StatusBadGateway, "unexpected kubectl output: "+err.Error())
		return
	}

	// One pod list for the namespace; match each Service's selector against
	// it locally so we make exactly one extra read call, not one per svc.
	pods := k.podsByLabels(ns)

	services := make([]kubeService, 0, len(svcList.Items))
	for _, it := range svcList.Items {
		sel := it.Spec.Selector
		svc := kubeService{
			Name:      it.Metadata.Name,
			Namespace: ns,
			Type:      it.Spec.Type,
			ClusterIP: it.Spec.ClusterIP,
			Selector:  joinSelector(sel),
		}
		for _, p := range it.Spec.Ports {
			proto := p.Protocol
			if proto == "" {
				proto = "TCP"
			}
			svc.Ports = append(svc.Ports, kubePort{
				Name: p.Name, Port: p.Port, Protocol: proto,
			})
		}
		if len(sel) == 0 {
			svc.Ready = "no selector"
		} else {
			total, ready := matchPodReadiness(pods, sel)
			svc.Ready = fmt.Sprintf("%d/%d ready", ready, total)
			svc.ReadyOK = total > 0 && ready == total
		}
		services = append(services, svc)
	}
	sort.Slice(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"services":  services,
		"namespace": ns,
		"kubeReady": true,
	})
}

type podInfo struct {
	labels map[string]string
	ready  bool
}

// podsByLabels reads the namespace's pods once (read-only) and projects just
// labels + an aggregate Ready condition. Best-effort: on any failure we
// return nil and Services still render (readiness shows as "?").
func (k KubeBrowser) podsByLabels(ns string) []podInfo {
	out, err := k.kubectl("get", "pods", "-n", ns, "-o", "json")
	if err != nil {
		return nil
	}
	var pl struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &pl); err != nil {
		return nil
	}
	pods := make([]podInfo, 0, len(pl.Items))
	for _, p := range pl.Items {
		ready := false
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" {
				ready = c.Status == "True"
			}
		}
		pods = append(pods, podInfo{labels: p.Metadata.Labels, ready: ready})
	}
	return pods
}

// matchPodReadiness returns total/ready pod counts whose labels are a
// superset of the Service selector.
func matchPodReadiness(pods []podInfo, sel map[string]string) (total, ready int) {
	for _, p := range pods {
		if labelsMatch(p.labels, sel) {
			total++
			if p.ready {
				ready++
			}
		}
	}
	return
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func joinSelector(sel map[string]string) string {
	if len(sel) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sel))
	for k, v := range sel {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
