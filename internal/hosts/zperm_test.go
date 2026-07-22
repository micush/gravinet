package hosts

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// TestSyncKeepsHostsReadable: syncing must leave the hosts file world/group
// readable (0644-style), even when it was previously 0600, so normal users can
// still resolve names.
func TestSyncKeepsHostsReadable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	if err := os.WriteFile(p, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	e := Entry{Hostname: "node-b", V4: netip.MustParseAddr("10.0.0.2")}
	if err := Sync(p, "gravinet", []Entry{e}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o044 != 0o044 {
		t.Fatalf("hosts not group/other-readable after sync: %o", fi.Mode().Perm())
	}

	// A brand-new hosts file defaults to 0644.
	p2 := filepath.Join(dir, "hosts2")
	if err := Sync(p2, "gravinet", []Entry{e}); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(p2)
	if fi2.Mode().Perm() != 0o644 {
		t.Fatalf("new hosts file mode = %o, want 644", fi2.Mode().Perm())
	}
}
