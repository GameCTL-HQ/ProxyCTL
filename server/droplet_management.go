package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// dropletManagementProbe emits one machine-readable marker per signal. Values
// never contain newlines, so the parser can ignore unrelated command output and
// remain backward-compatible as the page grows.
const dropletManagementProbe = `set -u
printf 'PROXYCTL_HOSTNAME='; hostname 2>/dev/null || true
printf 'PROXYCTL_OS='; (awk -F= '$1=="PRETTY_NAME" {gsub(/^"|"$/, "", $2); print $2; exit}' /etc/os-release 2>/dev/null || true)
printf 'PROXYCTL_KERNEL='; uname -r 2>/dev/null || true
printf 'PROXYCTL_UPTIME_SECONDS='; awk '{printf "%d\n", $1}' /proc/uptime 2>/dev/null || echo 0
printf 'PROXYCTL_LOAD='; awk '{print $1, $2, $3}' /proc/loadavg 2>/dev/null || echo '0 0 0'
printf 'PROXYCTL_CPU_CORES='; (getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null || echo 0)
read cpu u n s i w irq sirq steal rest < /proc/stat
idle1=$((i+w)); total1=$((u+n+s+i+w+irq+sirq+steal)); sleep 1
read cpu u n s i w irq sirq steal rest < /proc/stat
idle2=$((i+w)); total2=$((u+n+s+i+w+irq+sirq+steal)); dt=$((total2-total1)); di=$((idle2-idle1))
printf 'PROXYCTL_CPU_USAGE_PCT='; awk -v dt="$dt" -v di="$di" 'BEGIN { if (dt > 0) printf "%.1f\n", (dt-di)*100/dt; else print "0.0" }'
printf 'PROXYCTL_MEM_TOTAL='; awk '/^MemTotal:/ {print $2*1024}' /proc/meminfo
printf 'PROXYCTL_MEM_AVAILABLE='; awk '/^MemAvailable:/ {print $2*1024}' /proc/meminfo
set -- $(df -B1 --output=size,used,avail / 2>/dev/null | tail -1)
echo "PROXYCTL_DISK_TOTAL=${1:-0}"
echo "PROXYCTL_DISK_USED=${2:-0}"
echo "PROXYCTL_DISK_AVAILABLE=${3:-0}"
printf 'PROXYCTL_WIREGUARD_ACTIVE='; systemctl is-active --quiet wg-quick@wg0 && echo true || echo false
printf 'PROXYCTL_SSH_ACTIVE='; (systemctl is-active --quiet ssh || systemctl is-active --quiet sshd) && echo true || echo false
printf 'PROXYCTL_UNATTENDED_ACTIVE='; systemctl is-active --quiet unattended-upgrades && echo true || echo false
printf 'PROXYCTL_REBOOT_REQUIRED='; [ -e /var/run/reboot-required ] && echo true || echo false
printf 'PROXYCTL_PENDING_UPDATES='; if command -v apt >/dev/null; then apt list --upgradable 2>/dev/null | sed '1d' | awk 'NF {n++} END {print n+0}'; else echo 0; fi
printf 'PROXYCTL_WG_VERSION='; wg --version 2>/dev/null | head -1 || true
printf 'PROXYCTL_CLOUDFLARED_INSTALLED='; command -v cloudflared >/dev/null 2>&1 && echo true || echo false
printf 'PROXYCTL_CLOUDFLARED_ACTIVE='; systemctl is-active --quiet cloudflared && echo true || echo false
printf 'PROXYCTL_CLOUDFLARED_VERSION='; cloudflared --version 2>/dev/null | head -1 || true
`

// Delay the reboot so systemd-run can return success over SSH before the host
// tears down its network. A timestamped transient unit also makes retries safe.
const dropletRebootCommand = `systemd-run --unit="proxyctl-reboot-$(date +%s)" --on-active=2s /usr/bin/systemctl reboot`

// beginDropletMaintenance is the server-side gate for all operations that can
// mutate or restart the remote host. A timed hold is used after a reboot is
// scheduled because the request itself returns before the host actually drops.
func (a *API) beginDropletMaintenance(kind string, hold time.Duration) (busy string, ok bool) {
	a.dropletMaintenanceMu.Lock()
	defer a.dropletMaintenanceMu.Unlock()
	if a.dropletMaintenance != "" && !a.dropletMaintenanceUntil.IsZero() && time.Now().After(a.dropletMaintenanceUntil) {
		a.dropletMaintenance = ""
		a.dropletMaintenanceUntil = time.Time{}
	}
	if a.dropletMaintenance != "" {
		return a.dropletMaintenance, false
	}
	a.dropletMaintenance = kind
	if hold > 0 {
		a.dropletMaintenanceUntil = time.Now().Add(hold)
	} else {
		a.dropletMaintenanceUntil = time.Time{}
	}
	return "", true
}

func (a *API) endDropletMaintenance(kind string) {
	a.dropletMaintenanceMu.Lock()
	defer a.dropletMaintenanceMu.Unlock()
	if a.dropletMaintenance == kind {
		a.dropletMaintenance = ""
		a.dropletMaintenanceUntil = time.Time{}
	}
}

// busyDropletMaintenance returns the active operation kind, lazily clearing an
// expired reboot hold so status readers never report a stale lock.
func (a *API) busyDropletMaintenance() string {
	a.dropletMaintenanceMu.Lock()
	defer a.dropletMaintenanceMu.Unlock()
	if a.dropletMaintenance != "" && !a.dropletMaintenanceUntil.IsZero() && time.Now().After(a.dropletMaintenanceUntil) {
		a.dropletMaintenance = ""
		a.dropletMaintenanceUntil = time.Time{}
		return ""
	}
	return a.dropletMaintenance
}

type DropletManagementStatus struct {
	Hostname             string  `json:"hostname"`
	OS                   string  `json:"os"`
	Kernel               string  `json:"kernel"`
	UptimeSeconds        int64   `json:"uptimeSeconds"`
	Load1                float64 `json:"load1"`
	Load5                float64 `json:"load5"`
	Load15               float64 `json:"load15"`
	CPUCores             int64   `json:"cpuCores"`
	CPUUsagePct          float64 `json:"cpuUsagePct"`
	MemoryTotal          int64   `json:"memoryTotal"`
	MemoryUsed           int64   `json:"memoryUsed"`
	MemoryAvailable      int64   `json:"memoryAvailable"`
	DiskTotal            int64   `json:"diskTotal"`
	DiskUsed             int64   `json:"diskUsed"`
	DiskAvailable        int64   `json:"diskAvailable"`
	WireGuardActive      bool    `json:"wireguardActive"`
	SSHActive            bool    `json:"sshActive"`
	UnattendedActive     bool    `json:"unattendedActive"`
	RebootRequired       bool    `json:"rebootRequired"`
	PendingUpdates       int64   `json:"pendingUpdates"`
	WGVersion            string  `json:"wgVersion"`
	CloudflaredInstalled bool    `json:"cloudflaredInstalled"`
	CloudflaredActive    bool    `json:"cloudflaredActive"`
	CloudflaredVersion   string  `json:"cloudflaredVersion"`
}

func parseDropletManagementStatus(stdout string) (DropletManagementStatus, bool) {
	var out DropletManagementStatus
	values := make(map[string]string)
	for _, line := range strings.Split(stdout, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.HasPrefix(key, "PROXYCTL_") {
			values[key] = strings.TrimSpace(value)
		}
	}
	complete := true
	intValue := func(key string) int64 {
		v, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil {
			complete = false
		}
		return v
	}
	floatValue := func(key string) float64 {
		v, err := strconv.ParseFloat(values[key], 64)
		if err != nil {
			complete = false
		}
		return v
	}
	boolValue := func(key string) bool { return values[key] == "true" }

	for _, key := range []string{
		"PROXYCTL_HOSTNAME", "PROXYCTL_KERNEL", "PROXYCTL_UPTIME_SECONDS", "PROXYCTL_LOAD",
		"PROXYCTL_CPU_CORES", "PROXYCTL_CPU_USAGE_PCT", "PROXYCTL_MEM_TOTAL", "PROXYCTL_MEM_AVAILABLE",
		"PROXYCTL_DISK_TOTAL", "PROXYCTL_DISK_USED", "PROXYCTL_DISK_AVAILABLE",
		"PROXYCTL_WIREGUARD_ACTIVE", "PROXYCTL_SSH_ACTIVE", "PROXYCTL_UNATTENDED_ACTIVE",
		"PROXYCTL_REBOOT_REQUIRED", "PROXYCTL_PENDING_UPDATES",
		"PROXYCTL_CLOUDFLARED_INSTALLED", "PROXYCTL_CLOUDFLARED_ACTIVE", "PROXYCTL_CLOUDFLARED_VERSION",
	} {
		if _, ok := values[key]; !ok {
			complete = false
			break
		}
	}

	out.Hostname = values["PROXYCTL_HOSTNAME"]
	out.OS = values["PROXYCTL_OS"]
	out.Kernel = values["PROXYCTL_KERNEL"]
	out.UptimeSeconds = intValue("PROXYCTL_UPTIME_SECONDS")
	if _, err := fmt.Sscanf(values["PROXYCTL_LOAD"], "%f %f %f", &out.Load1, &out.Load5, &out.Load15); err != nil {
		complete = false
	}
	out.CPUCores = intValue("PROXYCTL_CPU_CORES")
	out.CPUUsagePct = floatValue("PROXYCTL_CPU_USAGE_PCT")
	out.MemoryTotal = intValue("PROXYCTL_MEM_TOTAL")
	out.MemoryAvailable = intValue("PROXYCTL_MEM_AVAILABLE")
	out.MemoryUsed = out.MemoryTotal - out.MemoryAvailable
	if out.MemoryUsed < 0 {
		out.MemoryUsed = 0
	}
	out.DiskTotal = intValue("PROXYCTL_DISK_TOTAL")
	out.DiskUsed = intValue("PROXYCTL_DISK_USED")
	out.DiskAvailable = intValue("PROXYCTL_DISK_AVAILABLE")
	out.WireGuardActive = boolValue("PROXYCTL_WIREGUARD_ACTIVE")
	out.SSHActive = boolValue("PROXYCTL_SSH_ACTIVE")
	out.UnattendedActive = boolValue("PROXYCTL_UNATTENDED_ACTIVE")
	out.RebootRequired = boolValue("PROXYCTL_REBOOT_REQUIRED")
	out.PendingUpdates = intValue("PROXYCTL_PENDING_UPDATES")
	out.WGVersion = values["PROXYCTL_WG_VERSION"]
	out.CloudflaredInstalled = boolValue("PROXYCTL_CLOUDFLARED_INSTALLED")
	out.CloudflaredActive = boolValue("PROXYCTL_CLOUDFLARED_ACTIVE")
	out.CloudflaredVersion = values["PROXYCTL_CLOUDFLARED_VERSION"]
	return out, complete
}

func (a *API) dropletManagementState(configured, reachable bool) map[string]any {
	return map[string]any{
		"configured": configured,
		"reachable":  reachable,
		"busy":       a.busyDropletMaintenance(),
	}
}

func (a *API) readDropletManagementStatus() (DropletManagementStatus, bool, error) {
	cfg := a.droplet.Get()
	if cfg == nil || !cfg.Configured() {
		return DropletManagementStatus{}, false, errDropletNotConfigured
	}
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		return DropletManagementStatus{}, false, fmt.Errorf("ssh apply-mode required")
	}
	sa.Droplet = cfg
	st := sa.sshDropletLong("droplet management: status", "", "bash -c "+shq(dropletManagementProbe), 20*time.Second)
	if !st.OK {
		return DropletManagementStatus{}, false, fmt.Errorf("%s", strings.TrimSpace(stepError(st)))
	}
	status, complete := parseDropletManagementStatus(st.Stdout)
	return status, complete, nil
}

// errDropletNotConfigured lets background callers tell "no droplet exists"
// (a normal state) apart from a real probe failure (which must never be
// treated as an all-clear).
var errDropletNotConfigured = fmt.Errorf("droplet not configured")

func (a *API) dropletManagementStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := a.droplet.Get()
	if cfg == nil || !cfg.Configured() {
		a.writeJSON(w, http.StatusOK, a.dropletManagementState(false, false))
		return
	}
	status, complete, err := a.readDropletManagementStatus()
	if err != nil {
		resp := a.dropletManagementState(true, false)
		resp["checkedAt"] = time.Now().Unix()
		resp["error"] = err.Error()
		a.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp := a.dropletManagementState(true, true)
	resp["complete"] = complete
	resp["checkedAt"] = time.Now().Unix()
	resp["ip"] = cfg.IP
	resp["status"] = status
	resp["clusterCloudflaredVersion"] = cloudflaredVersion
	if !complete {
		resp["warning"] = "Some droplet signals could not be parsed."
	}
	a.writeJSON(w, http.StatusOK, resp)
}

// wireguardImageContext returns the namespace + kube context the WireGuard
// gateways live in, exactly as the update operation resolves them.
func (a *API) wireguardImageContext() (ns, kubeContext string) {
	ns = a.infra.K8sNamespace
	if sa, ok := a.applier.(SSHApplier); ok {
		kubeContext = sa.KubeContext
	}
	return
}

func (a *API) wireguardImageStatus(w http.ResponseWriter, _ *http.Request) {
	ns, kubeContext := a.wireguardImageContext()
	st := wireguardImageStatus(ns, kubeContext)
	a.writeJSON(w, http.StatusOK, map[string]any{
		"currentDigest":     st.CurrentDigest,
		"upstreamRef":       st.UpstreamRef,
		"upstreamChecked":   st.UpstreamChecked,
		"updateAvailable":   st.UpdateAvailable,
		"lookupError":       st.LookupError,
		"behindDeployments": st.BehindDeployments,
		// Best-effort droplet side: neutral state (not an error) when the
		// droplet is unconfigured or unreachable.
		"droplet": a.dropletWireguardStatus(),
	})
}

func (a *API) updateWireguardImage(w http.ResponseWriter, _ *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))
	resp := a.runWireguardImageUpdate(context.Background())
	if _, isBusy := resp["busy"].(string); isBusy {
		a.writeJSON(w, http.StatusConflict, resp)
		return
	}
	a.writeJSON(w, http.StatusOK, resp)
}

// runWireguardImageUpdate is the operation core behind POST
// /api/wireguard/image/update AND the background runner: resolve the target
// image (upstream "latest" when reachable, embedded reviewed pin
// otherwise), roll the cluster gateways, then sync the droplet's native
// tools. Busy-gated like the UI path so a background run and a manual run
// can never interleave.
func (a *API) runWireguardImageUpdate(ctx context.Context) map[string]any {
	if busy, acquired := a.beginDropletMaintenance("wireguard-image", 0); !acquired {
		return map[string]any{"ok": false, "busy": busy, "error": "cannot update WireGuard image while " + busy + " is in progress"}
	}
	defer a.endDropletMaintenance("wireguard-image")
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		return map[string]any{"ok": false, "error": "ssh apply-mode required"}
	}
	opCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()
	// Resolve the target image up front (own 10s budget): upstream "latest"
	// when reachable, embedded reviewed pin otherwise. The rollout below
	// then applies exactly that reference.
	lookupCtx, lookupCancel := context.WithTimeout(opCtx, 10*time.Second)
	image, source := resolveWireguardUpdateRef(lookupCtx)
	lookupCancel()
	result, err := reconcileWireguardGateways(opCtx, sa.KubeContext, a.infra.K8sNamespace, image)
	if err != nil {
		return map[string]any{
			"ok":     false,
			"error":  err.Error(),
			"image":  image,
			"source": source,
		}
	}
	// Phase 2: sync the droplet's native wireguard-tools to the same pinned
	// upstream source (builds on the droplet, shadows the distro package in
	// /usr/local, leaves it as rollback). Per-phase reporting: a droplet
	// failure does not undo or hide the completed cluster rollout.
	dropletResp, dropletNote, dropletErr := a.syncDropletWireguardPhase(sa)
	resp := map[string]any{
		"image":   image,
		"source":  source,
		"result":  result + "; " + dropletNote,
		"droplet": dropletResp,
	}
	if dropletErr != nil {
		resp["ok"] = false
		resp["error"] = dropletErr.Error() + " — cluster gateways were updated to " + image
	} else {
		resp["ok"] = true
	}
	return resp
}

// syncDropletWireguardPhase runs the droplet half of a WireGuard update and
// returns its response fragment, a human line for the result string, and an
// error ONLY when the sync was attempted and failed (an unconfigured
// droplet is a neutral skip, not an error).
func (a *API) syncDropletWireguardPhase(sa SSHApplier) (map[string]any, string, error) {
	cfg := a.droplet.Get()
	if cfg == nil || !cfg.Configured() {
		return map[string]any{"ok": false, "skipped": true, "error": "droplet not configured"}, "droplet wireguard skipped (not configured)", nil
	}
	got, err := syncDropletWireguard(sa, cfg)
	if err != nil {
		return map[string]any{"ok": false, "expected": got.Expected, "error": err.Error()},
			"droplet wireguard sync FAILED", err
	}
	return map[string]any{"ok": true, "version": got.Version, "path": got.Path},
		"droplet wireguard " + got.Version + " (synced)", nil
}

func (a *API) rebootDroplet(w http.ResponseWriter, _ *http.Request) {
	cfg := a.droplet.Get()
	if !cfg.Configured() {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "droplet not configured"})
		return
	}
	sa, ok := a.applier.(SSHApplier)
	if !ok {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "ssh apply-mode required"})
		return
	}
	if busy, acquired := a.beginDropletMaintenance("reboot", 2*time.Minute); !acquired {
		a.writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "busy": busy, "error": "cannot reboot while droplet " + busy + " is in progress"})
		return
	}
	sa.Droplet = cfg
	st := sa.sshDroplet("droplet management: schedule reboot", "", dropletRebootCommand)
	if !st.OK {
		a.endDropletMaintenance("reboot")
		a.writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": strings.TrimSpace(st.Stderr), "exitCode": st.ExitCode})
		return
	}
	a.writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "scheduled": true, "requestedAt": time.Now().Unix(),
	})
}
