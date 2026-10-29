package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseDropletManagementStatus(t *testing.T) {
	raw := strings.Join([]string{
		"PROXYCTL_HOSTNAME=game-edge-1",
		"PROXYCTL_OS=Ubuntu 24.04.3 LTS",
		"PROXYCTL_KERNEL=6.8.0-88-generic",
		"PROXYCTL_UPTIME_SECONDS=86461",
		"PROXYCTL_LOAD=0.12 0.34 0.56",
		"PROXYCTL_CPU_CORES=2",
		"PROXYCTL_CPU_USAGE_PCT=17.5",
		"PROXYCTL_MEM_TOTAL=4194304000",
		"PROXYCTL_MEM_AVAILABLE=3145728000",
		"PROXYCTL_DISK_TOTAL=53687091200",
		"PROXYCTL_DISK_USED=10737418240",
		"PROXYCTL_DISK_AVAILABLE=42949672960",
		"PROXYCTL_WIREGUARD_ACTIVE=true",
		"PROXYCTL_SSH_ACTIVE=true",
		"PROXYCTL_UNATTENDED_ACTIVE=true",
		"PROXYCTL_REBOOT_REQUIRED=true",
		"PROXYCTL_PENDING_UPDATES=3",
		"PROXYCTL_WG_VERSION=wireguard-tools v1.0.20210914",
		"PROXYCTL_CLOUDFLARED_ACTIVE=true",
		"PROXYCTL_CLOUDFLARED_INSTALLED=true",
		"PROXYCTL_CLOUDFLARED_VERSION=cloudflared version 2026.10.4 (built 2026-10-26)",
	}, "\n")

	got, complete := parseDropletManagementStatus(raw)
	if !complete {
		t.Fatalf("expected complete status to parse cleanly: %+v", got)
	}
	if got.Hostname != "game-edge-1" || got.OS != "Ubuntu 24.04.3 LTS" || got.Kernel != "6.8.0-88-generic" {
		t.Fatalf("identity fields parsed incorrectly: %+v", got)
	}
	if got.UptimeSeconds != 86461 || got.Load1 != 0.12 || got.Load5 != 0.34 || got.Load15 != 0.56 {
		t.Fatalf("uptime/load parsed incorrectly: %+v", got)
	}
	if got.CPUCores != 2 || got.CPUUsagePct != 17.5 {
		t.Fatalf("CPU parsed incorrectly: %+v", got)
	}
	if got.MemoryTotal != 4194304000 || got.MemoryAvailable != 3145728000 || got.MemoryUsed != 1048576000 {
		t.Fatalf("memory parsed incorrectly: %+v", got)
	}
	if got.DiskTotal != 53687091200 || got.DiskUsed != 10737418240 || got.DiskAvailable != 42949672960 {
		t.Fatalf("disk parsed incorrectly: %+v", got)
	}
	if !got.WireGuardActive || !got.SSHActive || !got.UnattendedActive || !got.RebootRequired || got.PendingUpdates != 3 {
		t.Fatalf("service/update fields parsed incorrectly: %+v", got)
	}
	if got.WGVersion != "wireguard-tools v1.0.20210914" {
		t.Fatalf("wg version = %q", got.WGVersion)
	}
	if !got.CloudflaredInstalled || !got.CloudflaredActive || !strings.Contains(got.CloudflaredVersion, "2026.10.4") {
		t.Fatalf("cloudflared status parsed incorrectly: %+v", got)
	}
}

func TestParseDropletManagementStatusReportsIncompletePayload(t *testing.T) {
	_, complete := parseDropletManagementStatus("PROXYCTL_HOSTNAME=game-edge-1\nPROXYCTL_CPU_USAGE_PCT=not-a-number\n")
	if complete {
		t.Fatal("incomplete/malformed payload reported complete")
	}
}

func TestDropletManagementProbeIncludesRequiredSignals(t *testing.T) {
	for _, marker := range []string{
		"PROXYCTL_CPU_USAGE_PCT=",
		"PROXYCTL_MEM_TOTAL=",
		"PROXYCTL_DISK_TOTAL=",
		"PROXYCTL_WIREGUARD_ACTIVE=",
		"PROXYCTL_UNATTENDED_ACTIVE=",
		"PROXYCTL_REBOOT_REQUIRED=",
		"PROXYCTL_PENDING_UPDATES=",
		"PROXYCTL_CLOUDFLARED_ACTIVE=",
		"PROXYCTL_CLOUDFLARED_INSTALLED=",
		"PROXYCTL_CLOUDFLARED_VERSION=",
	} {
		if !strings.Contains(dropletManagementProbe, marker) {
			t.Errorf("management probe missing marker %q", marker)
		}
	}
}

func TestDropletRebootCommandSchedulesAfterSSHReturns(t *testing.T) {
	if !strings.Contains(dropletRebootCommand, "systemd-run") || !strings.Contains(dropletRebootCommand, "--on-active=2s") {
		t.Fatalf("reboot command must schedule reboot after the SSH request returns: %q", dropletRebootCommand)
	}
	if strings.Contains(dropletRebootCommand, "shutdown -r now") {
		t.Fatalf("reboot command must not synchronously tear down its own SSH response: %q", dropletRebootCommand)
	}
}

func TestDropletMaintenanceGuardSerializesMutatingOperations(t *testing.T) {
	a := &API{}
	if busy, ok := a.beginDropletMaintenance("update", 0); !ok || busy != "" {
		t.Fatalf("first operation rejected: busy=%q ok=%v", busy, ok)
	}
	if busy, ok := a.beginDropletMaintenance("reboot", 2*time.Minute); ok || busy != "update" {
		t.Fatalf("overlapping reboot was not rejected: busy=%q ok=%v", busy, ok)
	}
	a.endDropletMaintenance("update")
	if busy, ok := a.beginDropletMaintenance("reboot", 2*time.Minute); !ok || busy != "" {
		t.Fatalf("reboot rejected after update released lock: busy=%q ok=%v", busy, ok)
	}
	if busy, ok := a.beginDropletMaintenance("apply", 0); ok || busy != "reboot" {
		t.Fatalf("apply was not blocked during reboot window: busy=%q ok=%v", busy, ok)
	}
}

func TestExpiredDropletMaintenanceWindowClearsAutomatically(t *testing.T) {
	a := &API{dropletMaintenance: "reboot", dropletMaintenanceUntil: time.Now().Add(-time.Second)}
	if busy, ok := a.beginDropletMaintenance("apply", 0); !ok || busy != "" {
		t.Fatalf("expired maintenance lock remained active: busy=%q ok=%v", busy, ok)
	}
}

func TestDropletManagementBusyReportClearsExpiredHold(t *testing.T) {
	a := &API{dropletMaintenance: "reboot", dropletMaintenanceUntil: time.Now().Add(-time.Second)}
	if busy := a.busyDropletMaintenance(); busy != "" {
		t.Fatalf("expired reboot hold still reported busy: %q", busy)
	}
}

func TestDropletManagementStateAlwaysIncludesBusyOperation(t *testing.T) {
	a := &API{dropletMaintenance: "reboot"}
	got := a.dropletManagementState(true, false)
	if got["configured"] != true || got["reachable"] != false {
		t.Fatalf("unexpected base state: %#v", got)
	}
	if got["busy"] != "reboot" {
		t.Fatalf("busy = %#v, want reboot", got["busy"])
	}
}
