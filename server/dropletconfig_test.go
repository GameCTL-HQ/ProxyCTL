package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewFileDropletStoreRepairsPrivateKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "droplet.key")
	want := []byte("existing-private-key-must-not-change\n")
	if err := os.WriteFile(keyPath, want, 0o660); err != nil {
		t.Fatal(err)
	}

	if _, err := NewFileDropletStore(dir); err != nil {
		t.Fatalf("NewFileDropletStore: %v", err)
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("private key mode = %04o, want 0600", got)
	}
	got, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("private key contents changed: got %q, want %q", got, want)
	}
}
