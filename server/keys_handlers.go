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
	out := map[string]any{
		"basePath":        base,
		"defaultBasePath": defaultKeysBasePath,
		"storageClass":    scName,
		// The nfs-subdir provisioner backs the SSD export; show the resolved
		// on-disk hint for the UI (informational).
		"locationHint": "/mnt/1TBSSD/" + base + "  (NFS SSD)",
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
	if err := a.keys.Set(req.BasePath); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	a.refreshInfra() // so renders + apply use the new KeysBasePath

	// Create the StorageClass now (idempotent: skip if it already exists), so a
	// subsequent gateway apply can bind without waiting.
	base := a.keys.BasePath()
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
		"storageClass": scName,
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
