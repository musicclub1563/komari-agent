package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteAutoDiscoveryConfigRestrictsPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auto-discovery.json")
	data := []byte("{\"uuid\":\"agent-uuid\",\"token\":\"secret-token\"}")

	if err := writeAutoDiscoveryConfig(path, data); err != nil {
		t.Fatalf("writeAutoDiscoveryConfig: %v", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(stored) != string(data) {
		t.Fatalf("content = %q, want %q", stored, data)
	}
	assertOwnerOnly(t, path)
}

func TestWriteAutoDiscoveryConfigTightensExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not model POSIX permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "auto-discovery.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := writeAutoDiscoveryConfig(path, []byte("{\"token\":\"secret-token\"}")); err != nil {
		t.Fatalf("writeAutoDiscoveryConfig: %v", err)
	}
	assertOwnerOnly(t, path)
}

func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %04o, want 0600", perm)
	}
}
