package hosts

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderPreservesAndReplaces(t *testing.T) {
	existing := []byte("127.0.0.1 localhost\n# my own note\n10.0.0.9 keepme\n")
	tag := "deadbeef"

	entries := []Entry{
		{Hostname: "node-b", V4: netip.MustParseAddr("10.42.0.2"), V6: netip.MustParseAddr("fd00:42::2")},
		{Hostname: "node-a", V4: netip.MustParseAddr("10.42.0.1")},
	}
	out := string(Render(existing, tag, entries))

	// Pre-existing lines preserved.
	for _, want := range []string{"127.0.0.1 localhost", "# my own note", "10.0.0.9 keepme"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lost pre-existing line %q:\n%s", want, out)
		}
	}
	// Managed block present with both families for node-b and v4 for node-a.
	for _, want := range []string{"# BEGIN gravinet deadbeef", "# END gravinet deadbeef",
		"10.42.0.2", "fd00:42::2", "10.42.0.1", "node-a", "node-b"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in block:\n%s", want, out)
		}
	}

	// Re-rendering with fewer entries replaces (not appends) the block.
	out2 := string(Render([]byte(out), tag, []Entry{{Hostname: "node-a", V4: netip.MustParseAddr("10.42.0.1")}}))
	if strings.Count(out2, "# BEGIN gravinet deadbeef") != 1 {
		t.Fatalf("block duplicated on re-render:\n%s", out2)
	}
	if strings.Contains(out2, "node-b") {
		t.Fatalf("stale node-b not removed:\n%s", out2)
	}

	// Empty entries removes the block entirely but keeps other content.
	out3 := string(Render([]byte(out2), tag, nil))
	if strings.Contains(out3, "gravinet deadbeef") {
		t.Fatalf("block not removed when empty:\n%s", out3)
	}
	if !strings.Contains(out3, "127.0.0.1 localhost") {
		t.Fatalf("removing block dropped other content:\n%s", out3)
	}
}

// TestSyncSerializesConcurrentNetworks deterministically reproduces the
// multi-network bug. While network A is mid-Sync (between its read and write),
// network B does a full Sync of its own block. Without serialization, A's write
// is based on a read taken before B's write and silently drops B's block — and
// because each network debounces, B never restores it. The syncMu lock must make
// A's read-modify-write atomic so B's block survives.
func TestSyncSerializesConcurrentNetworks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bDone := make(chan struct{})
	entA := []Entry{{Hostname: "host-a", V4: netip.MustParseAddr("10.1.0.1")}}
	entB := []Entry{{Hostname: "host-b", V4: netip.MustParseAddr("10.2.0.1")}}

	afterRead = func() {
		afterRead = nil // fire only on A's Sync
		go func() {
			Sync(path, "netB", entB) // network B writes its block concurrently
			close(bDone)
		}()
		select {
		case <-bDone: // B got through before A wrote (only possible without the lock)
		case <-time.After(300 * time.Millisecond): // B blocked on the lock — expected
		}
	}
	if err := Sync(path, "netA", entA); err != nil {
		t.Fatal(err)
	}
	<-bDone // ensure B's (possibly queued) write completes
	afterRead = nil

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"host-a", "host-b", "localhost"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("%q missing — a network's block was clobbered:\n%s", want, out)
		}
	}
}
