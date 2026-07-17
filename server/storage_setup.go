package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// GUI-first storage: a fresh install boots on an emptyDir ("setup mode") and
// the wizard's Storage step — not the installer — decides where everything
// lives. The operator names an NFS share, TESTS it (a probe pod mounts it
// inline and writes a file), then adopts it: ProxyCTL patches its OWN
// Deployment from emptyDir to an inline nfs volume (subPath app/), Kubernetes
// restarts the pod onto the share, and the admin login survives in the
// proxyctl-auth Secret. Gateways mount the same share inline at Keys/ —
// no PV, no PVC, no StorageClass, so the whole flow needs only namespaced
// RBAC the ServiceAccount already has (plus pods create/delete for the probe).
//
// A cluster-storage fallback (adopt a StorageClass instead) keeps NFS-less
// clusters viable: that path creates the classic proxyctl-data PVC and
// repoints the Deployment at it.

// dataVolumeState is what the running Deployment says about /data.
type dataVolumeState struct {
	Mode      string `json:"mode"` // "ephemeral" | "share" | "pvc" | "unknown"
	NFSServer string `json:"nfsServer,omitempty"`
	NFSExport string `json:"nfsExport,omitempty"`
	ClaimName string `json:"claimName,omitempty"`
	SubPath   string `json:"subPath,omitempty"`
}

// selfDataVolume reads ProxyCTL's own Deployment and classifies the "data"
// volume. The Deployment spec — not env — is the truth after an adopt.
func (a *API) selfDataVolume(ctx context.Context) (dataVolumeState, error) {
	out, err := exec.CommandContext(ctx, a.kubectl, "-n", a.selfNS,
		"get", "deploy/"+a.selfDeploy, "-o", "json").CombinedOutput()
	if err != nil {
		return dataVolumeState{Mode: "unknown"}, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name     string    `json:"name"`
						EmptyDir *struct{} `json:"emptyDir"`
						NFS      *struct {
							Server string `json:"server"`
							Path   string `json:"path"`
						} `json:"nfs"`
						PVC *struct {
							ClaimName string `json:"claimName"`
						} `json:"persistentVolumeClaim"`
					} `json:"volumes"`
					Containers []struct {
						VolumeMounts []struct {
							Name    string `json:"name"`
							SubPath string `json:"subPath"`
						} `json:"volumeMounts"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &dep); err != nil {
		return dataVolumeState{Mode: "unknown"}, err
	}
	st := dataVolumeState{Mode: "unknown"}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name != "data" {
			continue
		}
		switch {
		case v.NFS != nil:
			st = dataVolumeState{Mode: "share", NFSServer: v.NFS.Server, NFSExport: v.NFS.Path}
		case v.PVC != nil:
			st = dataVolumeState{Mode: "pvc", ClaimName: v.PVC.ClaimName}
		case v.EmptyDir != nil:
			st = dataVolumeState{Mode: "ephemeral"}
		}
	}
	if len(dep.Spec.Template.Spec.Containers) > 0 {
		for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
			if m.Name == "data" {
				st.SubPath = m.SubPath
			}
		}
	}
	return st, nil
}

// getStorageStatus: GET /api/storage/status — how /data is provisioned right
// now. mode "ephemeral" is what gates the wizard: nothing durable exists yet.
func (a *API) getStorageStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.selfDataVolume(r.Context())
	out := map[string]any{
		"mode":      st.Mode,
		"nfsServer": st.NFSServer,
		"nfsExport": st.NFSExport,
		"claimName": st.ClaimName,
		"subPath":   st.SubPath,
	}
	if err != nil {
		out["error"] = err.Error()
	}
	// A legacy PVC install may still keep KEYS on a share via env/config.
	if srv, exp, ok := a.keys.Share(); ok {
		out["keysShare"] = map[string]string{"server": srv, "export": exp}
	}
	a.writeJSON(w, http.StatusOK, out)
}

type storageShareReq struct {
	NFSServer    string `json:"nfsServer"`
	NFSExport    string `json:"nfsExport"`
	StorageClass string `json:"storageClass"` // adopt only: PVC fallback
}

// testStorageShare: POST /api/storage/test — mount the named share from a
// throwaway probe pod (inline nfs volume, ProxyCTL's own image) and write +
// read back a file. Failures come back with the pod's events, which is where
// NFS mount errors actually surface ("mount(2): timed out", access denied…).
func (a *API) testStorageShare(w http.ResponseWriter, r *http.Request) {
	var req storageShareReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	srv, exp := strings.TrimSpace(req.NFSServer), strings.TrimSpace(req.NFSExport)
	if err := validateNFSShare(srv, exp); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	exp = normalizeNFSExport(exp)

	_, image, err := a.selfContainerImage(r.Context())
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "couldn't read ProxyCTL's own image for the probe: " + err.Error()})
		return
	}

	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	podName := "proxyctl-storage-probe-" + hex.EncodeToString(rnd[:])

	// Mount the PARENT of the entered path and reach the leaf via subPath:
	// NFS refuses to mount a directory that doesn't exist, but kubelet
	// CREATES a missing subPath on demand. So testing "10.0.0.5:/mnt/x/ProxyCTL"
	// mounts /mnt/x and materializes ProxyCTL/ — the Test button itself
	// brings the directory into existence, and the later adopt (which mounts
	// the full path directly) finds it there. Only if the PARENT is missing
	// too does the mount fail — that error is honest and actionable.
	mountPath, subPath := exp, ""
	if exp != "/" {
		if i := strings.LastIndex(exp, "/"); i >= 0 {
			mountPath, subPath = exp[:i], exp[i+1:]
			if mountPath == "" {
				mountPath = "/"
			}
		}
	}
	subPathLine := ""
	if subPath != "" {
		subPathLine = fmt.Sprintf(", subPath: %s", subPath)
	}

	// activeDeadlineSeconds bounds a probe whose mount hangs; the file is
	// removed after the read so repeated tests leave the share clean.
	pod := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels: { proxyctl: storage-probe }
spec:
  restartPolicy: Never
  activeDeadlineSeconds: 40
  terminationGracePeriodSeconds: 3
  containers:
    - name: probe
      image: %s
      # Root, to mirror the storage-init initContainer that will prepare the
      # dir on adopt: with no_root_squash both act as root; on an all_squash
      # export both get squashed the same way. Either way the probe's verdict
      # predicts what adopt will actually experience.
      securityContext: { runAsUser: 0 }
      command: ["sh", "-c", "echo proxyctl-probe > /probe/.proxyctl-probe && cat /probe/.proxyctl-probe && rm -f /probe/.proxyctl-probe"]
      volumeMounts:
        - { name: probe, mountPath: /probe%s }
      resources:
        requests: { cpu: 10m, memory: 16Mi }
        limits: { cpu: 100m, memory: 32Mi }
  volumes:
    - name: probe
      nfs: { server: %s, path: %s }
`, podName, a.selfNS, image, subPathLine, srv, mountPath)

	if out, err := runKubectlKeys(pod, "-n", a.selfNS, "apply", "-f", "-"); err != nil {
		a.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "couldn't create the probe pod: " + strings.TrimSpace(out)})
		return
	}
	defer func() {
		_, _ = runKubectlKeys("", "-n", a.selfNS, "delete", "pod", podName,
			"--ignore-not-found", "--wait=false", "--grace-period=1")
	}()

	// Poll to a verdict. A mount that can't complete keeps the pod Pending
	// (stuck in ContainerCreating) — that's the common failure, and the
	// reason we read events rather than logs for the error text.
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		phase, _ := runKubectlKeys("", "-n", a.selfNS, "get", "pod", podName,
			"-o", "jsonpath={.status.phase}")
		switch strings.TrimSpace(phase) {
		case "Succeeded":
			a.writeJSON(w, http.StatusOK, map[string]any{
				"ok":      true,
				"message": fmt.Sprintf("✓ mounted %s:%s from the cluster and wrote + read a test file", srv, exp),
			})
			return
		case "Failed":
			logs, _ := runKubectlKeys("", "-n", a.selfNS, "logs", podName, "--tail=5")
			a.writeJSON(w, http.StatusOK, map[string]any{
				"ok":    false,
				"error": "the probe mounted the share but couldn't write to it: " + strings.TrimSpace(logs),
			})
			return
		}
		time.Sleep(2 * time.Second)
	}
	events, _ := runKubectlKeys("", "-n", a.selfNS, "get", "events",
		"--field-selector", "involvedObject.name="+podName,
		"-o", "jsonpath={range .items[*]}{.reason}: {.message}{\"\\n\"}{end}")
	detail := strings.TrimSpace(events)
	if len(detail) > 600 {
		detail = detail[len(detail)-600:]
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok": false,
		"error": "the share didn't mount within 45s — usually the export doesn't " +
			"admit the node IPs, or the path doesn't exist on the server.",
		"detail": detail,
	})
}

// adoptStorage: POST /api/storage/adopt — repoint ProxyCTL's own /data.
// Share: emptyDir → inline nfs volume at subPath app/, plus the share env so
// keys default onto it. StorageClass: create the classic proxyctl-data PVC
// and mount that. Either way the Deployment restarts (Recreate) and the UI
// reconnects; the admin login lives in a Secret and survives.
func (a *API) adoptStorage(w http.ResponseWriter, r *http.Request) {
	var req storageShareReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	sc := strings.TrimSpace(req.StorageClass)
	srv, exp := strings.TrimSpace(req.NFSServer), strings.TrimSpace(req.NFSExport)

	var volume, subPath, envSrv, envExp string
	switch {
	case srv != "" || exp != "":
		if err := validateNFSShare(srv, exp); err != nil {
			a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		exp = normalizeNFSExport(exp)
		volume = fmt.Sprintf(`{"name":"data","nfs":{"server":%q,"path":%q}}`, srv, exp)
		subPath, envSrv, envExp = "app", srv, exp
	case sc != "":
		// A nonexistent class would leave the PVC Pending and the restarted
		// pod stuck in ContainerCreating — with the UI down. Refuse upfront.
		if out, err := runKubectlKeys("", "get", "storageclass", sc); err != nil {
			a.writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "StorageClass " + sc + " doesn't exist in this cluster: " + strings.TrimSpace(out)})
			return
		}
		pvc := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: proxyctl-data
  namespace: %s
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: %s
  resources: { requests: { storage: 1Gi } }
`, a.selfNS, sc)
		if _, err := runKubectlKeys("", "-n", a.selfNS, "get", "pvc", "proxyctl-data"); err != nil {
			if out, err := runKubectlKeys(pvc, "-n", a.selfNS, "create", "-f", "-"); err != nil {
				a.writeJSON(w, http.StatusBadGateway, map[string]any{
					"error": "creating the proxyctl-data PVC failed: " + strings.TrimSpace(out)})
				return
			}
		}
		volume = `{"name":"data","persistentVolumeClaim":{"claimName":"proxyctl-data"}}`
	default:
		a.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "give either an NFS share (server + export) or a storageClass"})
		return
	}

	_, image, err := a.selfContainerImage(r.Context())
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "couldn't read ProxyCTL's own image: " + err.Error()})
		return
	}
	ops, err := a.dataVolumePatchOps(r.Context(), volume, subPath, envSrv, envExp, image)
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if out, err := runKubectlKeys("", "-n", a.selfNS, "patch", "deploy/"+a.selfDeploy,
		"--type=json", "-p", ops); err != nil {
		a.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "patching the Deployment failed: " + strings.TrimSpace(out)})
		return
	}
	a.refreshInfra()
	mode := "share"
	msg := fmt.Sprintf("Moving in: ProxyCTL is restarting onto %s:%s (app/ beside Keys/). This page will reconnect in a few seconds.", srv, exp)
	if sc != "" {
		mode = "pvc"
		msg = "Moving in: ProxyCTL is restarting onto a PVC on StorageClass " + sc + ". This page will reconnect in a few seconds."
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode, "message": msg})
}

// dataVolumePatchOps builds the JSON-patch that swaps the data volume,
// sets/clears the mount subPath, sets/clears the share env vars, and keeps
// the storage-init initContainer pointed at the right dir — with indices
// read from the LIVE spec, since a patch by position must match it.
func (a *API) dataVolumePatchOps(ctx context.Context, volumeJSON, subPath, envSrv, envExp, image string) (string, error) {
	out, err := exec.CommandContext(ctx, a.kubectl, "-n", a.selfNS,
		"get", "deploy/"+a.selfDeploy, "-o", "json").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("reading the Deployment: %s", strings.TrimSpace(string(out)))
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name string `json:"name"`
					} `json:"volumes"`
					InitContainers []struct {
						Name string `json:"name"`
					} `json:"initContainers"`
					Containers []struct {
						VolumeMounts []struct {
							Name string `json:"name"`
						} `json:"volumeMounts"`
						Env []struct {
							Name string `json:"name"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &dep); err != nil {
		return "", err
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return "", fmt.Errorf("deployment has no containers")
	}
	volIdx, mntIdx := -1, -1
	for i, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "data" {
			volIdx = i
		}
	}
	c := dep.Spec.Template.Spec.Containers[0]
	for i, m := range c.VolumeMounts {
		if m.Name == "data" {
			mntIdx = i
		}
	}
	if volIdx < 0 || mntIdx < 0 {
		return "", fmt.Errorf("deployment has no 'data' volume/mount to repoint")
	}

	var ops []string
	ops = append(ops, fmt.Sprintf(`{"op":"replace","path":"/spec/template/spec/volumes/%d","value":%s}`, volIdx, volumeJSON))

	// storage-init: root, creates the data dir and hands it to uid 1000.
	// kubelet creates a missing NFS subPath dir root:root 755 and fsGroup
	// doesn't apply to nfs volumes — without this, the app's first write
	// (the droplet SSH key) dies with Permission denied.
	dir := "/share/" + subPath
	initJSON := fmt.Sprintf(`{"name":"storage-init","image":%q,`+
		`"securityContext":{"runAsUser":0},`+
		`"command":["sh","-c","mkdir -p %s && chown 1000:1000 %s"],`+
		`"volumeMounts":[{"name":"data","mountPath":"/share"}]}`, image, dir, dir)
	initIdx := -1
	for i, ic := range dep.Spec.Template.Spec.InitContainers {
		if ic.Name == "storage-init" {
			initIdx = i
		}
	}
	switch {
	case initIdx >= 0:
		ops = append(ops, fmt.Sprintf(`{"op":"replace","path":"/spec/template/spec/initContainers/%d","value":%s}`, initIdx, initJSON))
	case len(dep.Spec.Template.Spec.InitContainers) > 0:
		ops = append(ops, fmt.Sprintf(`{"op":"add","path":"/spec/template/spec/initContainers/-","value":%s}`, initJSON))
	default:
		ops = append(ops, fmt.Sprintf(`{"op":"add","path":"/spec/template/spec/initContainers","value":[%s]}`, initJSON))
	}
	// subPath: always set explicitly ("" = volume root) — add if the live
	// mount predates the field.
	ops = append(ops, fmt.Sprintf(`{"op":"add","path":"/spec/template/spec/containers/0/volumeMounts/%d/subPath","value":%q}`, mntIdx, subPath))
	for name, val := range map[string]string{
		"PROXYCTL_DATA_NFS_SERVER": envSrv,
		"PROXYCTL_DATA_NFS_EXPORT": envExp,
	} {
		envIdx := -1
		for i, e := range c.Env {
			if e.Name == name {
				envIdx = i
			}
		}
		if envIdx >= 0 {
			ops = append(ops, fmt.Sprintf(`{"op":"replace","path":"/spec/template/spec/containers/0/env/%d/value","value":%q}`, envIdx, val))
		} else {
			ops = append(ops, fmt.Sprintf(`{"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":%q,"value":%q}}`, name, val))
		}
	}
	return "[" + strings.Join(ops, ",") + "]", nil
}
