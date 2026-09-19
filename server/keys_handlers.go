package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Keys-location settings: let the operator choose (and later move) the NFS
// folder where per-gateway WireGuard keypair PVCs are stored. The folder drives
// a StorageClass pathPattern (see render.go: keysSCName / renderKeysStorageClass);
// ProxyCTL creates the class on demand and can re-key existing gateways into the
// new location.

// runKubectlKeys runs a kubectl command (optionally with stdin) using the
// in-cluster ServiceAccount, with a short timeout. Used for StorageClass
// create + gateway PVC cleanup during a keys move.
func runKubectlKeys(stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// getKeysConfig: GET /api/keys-config — the current keys folder + derived
// StorageClass, plus how many live gateways still sit on a different class
// (i.e. would be moved by a re-key).
func (a *API) getKeysConfig(w http.ResponseWriter, r *http.Request) {
	base := a.keys.BasePath()
	scName := keysSCName(base)
	sf := discoverStorage()
	srv, exp, shareMode := a.keys.Share()
	// The install's own share (when the whole app lives on one) beats
	// provisioner discovery as the wizard's suggested share: it's where the
	// operator already decided ProxyCTL's data goes.
	discSrv, discExp := sf.NFSServer, sf.NFSRoot()
	if s, e, ok := appShareEnv(); ok {
		discSrv, discExp = s, e
	}
	out := map[string]any{
		"basePath":        base,
		"defaultBasePath": defaultKeysBase(),
		"storageClass":    scName,
		// "share" = keys go on the operator-named export via a static PV.
		// "provisioner" = they ride the install's own StorageClass.
		"mode": map[bool]string{true: "share", false: "provisioner"}[shareMode],
		// The operator's share, echoed back so the wizard can prefill.
		"nfsServer": srv,
		"nfsExport": exp,
		// Resolved from THIS cluster's provisioner (storagediscover.go), not
		// assumed: the export behind the install's StorageClass is the only
		// thing that knows where keys land when no share is named.
		"locationHint": keysLocationHintFor(base, srv, exp),
		// The DISCOVERED export, offered as the example/prefill so the operator
		// starts from what their cluster actually uses rather than a value
		// invented here.
		"discoveredNfsServer": discSrv,
		"discoveredNfsExport": discExp,
		"baseStorageClass":    sf.StorageClass,
	}

	// Share mode: diff the live node list against the snapshot of IPs the
	// operator's exports line was generated for. A node added since then can't
	// mount the share — surface it, with a regenerated exports line, before a
	// gateway scheduled there hangs in ContainerCreating. Best-effort: a
	// kubectl hiccup (or pre-nodes-RBAC install) just omits the warning.
	if shareMode {
		if live, err := listClusterNodes(); err == nil {
			if missing := nodesNotCovered(live, a.keys.NodeIPs()); len(missing) > 0 {
				ips := make([]string, len(live))
				for i, n := range live {
					ips[i] = n.IP
				}
				out["uncoveredNodes"] = missing
				out["updatedExportsLine"] = exportsLineFor(exp, ips)
			}
		}
	}

	// Count gateways whose keys PVC is NOT on the current class (candidates to
	// move). Best-effort — a kubectl hiccup just omits the count.
	ns := a.infra.K8sNamespace
	if o, err := runKubectlKeys("", "-n", ns, "get", "pvc", "-l", "proxyctl=gateway",
		"-o", "jsonpath={range .items[*]}{.spec.storageClassName}{\"\\n\"}{end}"); err == nil {
		total, onOther := 0, 0
		for _, line := range strings.Split(strings.TrimSpace(o), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			total++
			if line != scName {
				onOther++
			}
		}
		out["gatewayKeysTotal"] = total
		out["gatewayKeysOnOtherPath"] = onOther
	}
	a.writeJSON(w, http.StatusOK, out)
}

type keysConfigReq struct {
	BasePath string `json:"basePath"`
	// Optional explicit NFS share. Both empty = keep keys on the install's own
	// StorageClass (provisioner mode). Both set = put them on this export via a
	// static PV, which is the only way to use a share the provisioner doesn't
	// mount. Sending both as "" clears a previously-set share.
	NFSServer string `json:"nfsServer"`
	NFSExport string `json:"nfsExport"`
}

// setKeysConfig: PUT /api/keys-config — validate + persist the keys folder,
// refresh the render Infra, and create the StorageClass now so new gateways can
// bind immediately. Existing gateways keep their current keys until a move.
func (a *API) setKeysConfig(w http.ResponseWriter, r *http.Request) {
	var req keysConfigReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if err := a.keys.SetAll(req.BasePath, req.NFSServer, req.NFSExport); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	a.refreshInfra() // so renders + apply use the new folder/share

	base := a.keys.BasePath()

	// Share mode: nothing to provision — gateways mount the export inline
	// (volumes.nfs + subPath) on the next apply, so saving the share is
	// pure configuration.
	if srv, exp, ok := a.keys.Share(); ok {
		// Snapshot the node IPs the operator's exports line covers as of this
		// save, so later node-adds can be detected and warned about.
		// Best-effort: without nodes RBAC the diff is simply never armed.
		if live, err := listClusterNodes(); err == nil {
			ips := make([]string, len(live))
			for i, n := range live {
				ips[i] = n.IP
			}
			_ = a.keys.SetNodeIPs(ips)
		}
		a.writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"basePath":  base,
			"mode":      "share",
			"nfsServer": srv, "nfsExport": exp,
			"locationHint": keysLocationHintFor(base, srv, exp),
		})
		return
	}

	// Provisioner mode: create the StorageClass now (idempotent: skip if it
	// already exists), so a subsequent gateway apply can bind without waiting.
	scName := keysSCName(base)
	if _, err := runKubectlKeys("", "get", "storageclass", scName); err != nil {
		if out, err := runKubectlKeys(renderKeysStorageClass(base), "create", "-f", "-"); err != nil {
			a.writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": "saved the folder, but creating the StorageClass failed: " + strings.TrimSpace(out),
			})
			return
		}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"basePath":     base,
		"mode":         "provisioner",
		"storageClass": scName,
		"locationHint": keysLocationHint(base),
	})
}

// migrateKeys: POST /api/keys-config/migrate — move EXISTING gateways to the
// current keys folder. A StorageClass pathPattern is fixed at creation, so a
// PVC can't be re-pointed in place: this deletes each managed gateway's
// Deployment + key PVC, then runs the normal apply, which recreates them under
// the current class (regenerating each key and re-syncing the droplet peer
// live). Brief per-tunnel downtime; other state is untouched.
func (a *API) migrateKeys(w http.ResponseWriter, r *http.Request) {
	ns := a.infra.K8sNamespace
	// Delete gateways + their key PVCs (reclaimPolicy Delete cleans the old
	// dirs). Secrets are left and re-applied by the apply below.
	if out, err := runKubectlKeys("", "-n", ns, "delete", "deploy,pvc",
		"-l", "proxyctl=gateway", "--ignore-not-found", "--wait=true", "--timeout=120s"); err != nil {
		a.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "failed to remove old gateway key volumes: " + strings.TrimSpace(out),
		})
		return
	}
	// Recreate everything via the standard apply (creates new PVCs under the
	// current keys class, re-keys, re-registers the droplet peers). Reuses the
	// pollable apply job + UI progress.
	a.apply(w, r)
}
