package cephfsplugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyFor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "east"), []byte("FILEKEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withDir := &Backend{keyDir: dir}
	noDir := &Backend{keyDir: ""}

	if k, err := withDir.keyFor("east", map[string]string{"userKey": "S"}); err != nil || k != "FILEKEY" {
		t.Fatalf("file wins: %q %v", k, err)
	}
	if k, err := withDir.keyFor("west", map[string]string{"userKey": "S"}); err != nil || k != "S" {
		t.Fatalf("secret fallback: %q %v", k, err)
	}
	if _, err := withDir.keyFor("west", nil); err == nil {
		t.Fatal("expected error: keyDir set, no file, no secret")
	}
	if k, err := noDir.keyFor("x", nil); err != nil || k != "" {
		t.Fatalf("no keyDir, no secret: %q %v", k, err)
	}
}

func TestSubvolName(t *testing.T) {
	if subvolName(subvolNamePrefix, "pvc-1") != subvolName(subvolNamePrefix, "pvc-1") {
		t.Fatal("not deterministic")
	}
	if subvolName(subvolNamePrefix, "a") == subvolName(subvolNamePrefix, "b") {
		t.Fatal("collision")
	}
	if n := subvolName(subvolNamePrefix, "some-long-pvc-name"); len(n) > 40 {
		t.Fatalf("name too long: %q", n)
	}
}
