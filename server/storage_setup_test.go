package main

import "testing"

func TestParseExistingDataProbe(t *testing.T) {
	// Realistic multi-line pod log: the write/read check's own output comes
	// first, the marker line last.
	logs := "proxyctl-probe\nPROXYCTL_EXISTING app_files=3 key_dirs=2\n"
	appFiles, keyDirs := parseExistingDataProbe(logs)
	if appFiles != 3 || keyDirs != 2 {
		t.Fatalf("want appFiles=3 keyDirs=2, got appFiles=%d keyDirs=%d", appFiles, keyDirs)
	}
}

func TestParseExistingDataProbe_Empty(t *testing.T) {
	appFiles, keyDirs := parseExistingDataProbe("PROXYCTL_EXISTING app_files=0 key_dirs=0\n")
	if appFiles != 0 || keyDirs != 0 {
		t.Fatalf("want both 0, got appFiles=%d keyDirs=%d", appFiles, keyDirs)
	}
}

// Missing/garbled marker (e.g. an older probe image without this check, or
// a pod log truncated mid-line) must fall back to "found nothing" rather
// than error — this check is advisory on top of the pass/fail mount test.
func TestParseExistingDataProbe_MissingMarker(t *testing.T) {
	appFiles, keyDirs := parseExistingDataProbe("proxyctl-probe\n")
	if appFiles != 0 || keyDirs != 0 {
		t.Fatalf("want both 0 for missing marker, got appFiles=%d keyDirs=%d", appFiles, keyDirs)
	}
}
