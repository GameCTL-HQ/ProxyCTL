package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// Runtime discovery of the storage this install actually sits on.
//
// ProxyCTL generates a StorageClass for the per-gateway keypair PVCs
// (see render.go: renderKeysStorageClass). That class must name the SAME
// provisioner as the StorageClass the install was given — a provisioner
// name is cluster-specific (the nfs-subdir chart builds it from the Helm
// release name), so a hardcoded one is only ever right on the cluster it
// was copied from. Everywhere else the class would reference a
// provisioner that does not exist and every keys PVC would sit Pending
// forever.
//
// The backing NFS export is discovered the same way, purely so the UI can
// tell the operator where their keys really land instead of printing a
// path invented at build time.
//
// Both are read through the ServiceAccount's EXISTING permissions:
// storageclasses (get) and pods (list). The nfs-subdir provisioner's env
// lives on its Pod as much as on its Deployment, and pods are already
// granted — reading it there avoids widening RBAC to cluster-wide
// deployments just for a display string.

// legacyBaseStorageClass is the pre-env default. Installs from before
// PROXYCTL_STORAGE_CLASS existed have no env to read, and their manifest
// used this value, so it stays the fallback rather than a hard failure.
const legacyBaseStorageClass = "nfs-ssd"

// legacyKeysProvisioner is the last-resort provisioner for renderKeysStorageClass
// when discovery turns up nothing. It is the nfs-subdir chart's name for a
// release called "nfs-ssd" — i.e. correct only for the legacy default install.
const legacyKeysProvisioner = "cluster.local/" + legacyBaseStorageClass + "-nfs-subdir-external-provisioner"

// baseStorageClass is the StorageClass this install was deployed with,
// passed by the manifest (substituted from __STORAGE_CLASS__). The "__"
// guard catches a manifest applied without substitution.
func baseStorageClass() string {
	sc := strings.TrimSpace(os.Getenv("PROXYCTL_STORAGE_CLASS"))
	if sc == "" || strings.HasPrefix(sc, "__") {
		return legacyBaseStorageClass
	}
	return sc
}

// storageFacts is what we could learn about the backing storage. Every
// field is best-effort: a cluster whose provisioner isn't nfs-subdir (or
// whose RBAC is trimmed) yields zero values, and callers degrade to
// something honest rather than inventing a path.
type storageFacts struct {
	StorageClass string // the class this install runs on
	Provisioner  string // its provisioner, e.g. cluster.local/<release>-nfs-subdir-external-provisioner
	NFSServer    string // e.g. 10.0.0.100  (empty if not an nfs-subdir provisioner)
	NFSPath      string // e.g. /mnt/ssd    (empty if not an nfs-subdir provisioner)
}

// NFSRoot is the export root, or "" when undiscoverable.
func (f storageFacts) NFSRoot() string { return f.NFSPath }

var (
	sfMu    sync.Mutex
	sfCache *storageFacts
)

// discoverStorage resolves (and caches) the storage facts. Cached for the
// process lifetime: a StorageClass's provisioner is immutable, and the
// provisioner's NFS export only changes on a redeploy of the provisioner
// itself, which restarts nothing here — a stale hint is corrected by the
// next ProxyCTL restart.
func discoverStorage() storageFacts {
	sfMu.Lock()
	if sfCache != nil {
		f := *sfCache
		sfMu.Unlock()
		return f
	}
	sfMu.Unlock()

	f := storageFacts{StorageClass: baseStorageClass()}

	// 1. The class names its provisioner.
	if out, err := runKubectlKeys("", "get", "sc", f.StorageClass,
		"-o", "jsonpath={.provisioner}"); err == nil {
		f.Provisioner = strings.TrimSpace(out)
	}

	// 2. Find the pod running that provisioner and read its NFS env. Only
	//    the nfs-subdir provisioner publishes NFS_SERVER / NFS_PATH; any
	//    other provisioner simply leaves these empty.
	if f.Provisioner != "" {
		if srv, path, ok := nfsEnvForProvisioner(f.Provisioner); ok {
			f.NFSServer, f.NFSPath = srv, path
		}
	}

	sfMu.Lock()
	sfCache = &f
	sfMu.Unlock()
	return f
}

// nfsEnvForProvisioner scans pods for the one whose PROVISIONER_NAME
// matches, returning its NFS_SERVER / NFS_PATH.
func nfsEnvForProvisioner(prov string) (server, path string, ok bool) {
	out, err := runKubectlKeys("", "get", "pods", "--all-namespaces", "-o", "json")
	if err != nil {
		return "", "", false
	}
	var list struct {
		Items []struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return "", "", false
	}
	for _, p := range list.Items {
		for _, c := range p.Spec.Containers {
			var name, srv, pth string
			for _, e := range c.Env {
				switch e.Name {
				case "PROVISIONER_NAME":
					name = e.Value
				case "NFS_SERVER":
					srv = e.Value
				case "NFS_PATH":
					pth = e.Value
				}
			}
			if name == prov && srv != "" && pth != "" {
				return srv, pth, true
			}
		}
	}
	return "", "", false
}

// keysLocationHintFor describes where the keys for basePath land on disk, for
// display only. An operator-named share is authoritative — it IS the answer, no
// discovery needed. Otherwise fall back to resolving the install's own
// StorageClass.
func keysLocationHintFor(basePath, nfsServer, nfsExport string) string {
	if nfsServer != "" && nfsExport != "" {
		bp := normalizeKeysBasePath(basePath)
		if bp == "" {
			bp = defaultKeysBasePath
		}
		return strings.TrimRight(normalizeNFSExport(nfsExport), "/") + "/" + bp +
			"  (NFS " + nfsServer + ")"
	}
	return keysLocationHint(basePath)
}

// keysLocationHint describes where the keys for basePath land on disk in
// provisioner mode. Returns the real resolved path when the backing export is
// discoverable, and an honest placeholder when it isn't — never a guess.
func keysLocationHint(basePath string) string {
	bp := normalizeKeysBasePath(basePath)
	if bp == "" {
		bp = defaultKeysBasePath
	}
	f := discoverStorage()
	if root := f.NFSRoot(); root != "" {
		hint := strings.TrimRight(root, "/") + "/" + bp
		if f.NFSServer != "" {
			return hint + "  (NFS " + f.NFSServer + ")"
		}
		return hint
	}
	// Not an nfs-subdir provisioner (or not discoverable): the subpath is
	// still true, the root isn't ours to claim.
	return bp + "  (under the " + f.StorageClass + " volume root)"
}
