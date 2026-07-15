package control

import (
	"os"
	"path/filepath"
	"testing"
)

// Serve creates the socket's own directory if it's missing — but only under a
// parent that already exists.
func TestServeCreatesLeafDirUnderExistingParent(t *testing.T) {
	base := t.TempDir() // exists; stands in for /var/run
	sock := filepath.Join(base, "gravinet", "gravinet.sock")

	srv, err := Serve(sock, nil, nil)
	if err != nil {
		t.Fatalf("Serve(%q) = %v, want it to create the leaf dir and listen", sock, err)
	}
	defer srv.Close()

	if _, err := os.Stat(sock); err != nil {
		t.Errorf("socket not created at %s: %v", sock, err)
	}
}

// The behavior that entrenched the original bug: given a stale
// "/run/gravinet.sock", the old unconditional MkdirAll manufactured a top-level
// /run on FreeBSD (writable /), inventing a directory the OS doesn't ship and
// making the bad path "work" — while the identical call failed on macOS. Serve
// must not create a directory whose parent doesn't exist; it should fail loudly
// and identically everywhere instead.
func TestServeDoesNotFabricateTopLevelDir(t *testing.T) {
	base := t.TempDir()
	missingParent := filepath.Join(base, "nonexistent") // stands in for a missing /
	dir := filepath.Join(missingParent, "run")
	sock := filepath.Join(dir, "gravinet.sock")

	srv, err := Serve(sock, nil, nil)
	if err == nil {
		srv.Close()
		t.Fatalf("Serve(%q) succeeded; it must not create %q under a parent that doesn't exist", sock, dir)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Errorf("Serve fabricated directory %s under a nonexistent parent", dir)
	}
}
