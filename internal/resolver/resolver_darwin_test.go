//go:build darwin

package resolver

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withTempResolverDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := resolverDir
	resolverDir = dir
	t.Cleanup(func() { resolverDir = old })
	return dir
}

func addrs(t *testing.T, ss ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestSyncWritesOwnedFilesPerDomain(t *testing.T) {
	dir := withTempResolverDir(t)
	tag := "deadbeef"

	err := Sync(tag, "", []Entry{
		{Domain: "domain1", Servers: addrs(t, "1.1.1.1")},
		{Domain: "domain2", Servers: addrs(t, "2.2.2.2", "3.3.3.3")},
	}, nil)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	d1, err := os.ReadFile(filepath.Join(dir, "domain1"))
	if err != nil {
		t.Fatalf("read domain1: %v", err)
	}
	if !strings.Contains(string(d1), "nameserver 1.1.1.1") {
		t.Fatalf("domain1 missing its server:\n%s", d1)
	}
	if !strings.HasPrefix(string(d1), marker(tag)) {
		t.Fatalf("domain1 missing ownership marker:\n%s", d1)
	}

	d2, err := os.ReadFile(filepath.Join(dir, "domain2"))
	if err != nil {
		t.Fatalf("read domain2: %v", err)
	}
	if !strings.Contains(string(d2), "nameserver 2.2.2.2") || !strings.Contains(string(d2), "nameserver 3.3.3.3") {
		t.Fatalf("domain2 missing its servers (kept independent per domain, unlike Linux):\n%s", d2)
	}
}

func TestSyncRemovesStaleDomainsButKeepsUnowned(t *testing.T) {
	dir := withTempResolverDir(t)
	tag := "deadbeef"

	// An operator's own unrelated file, and a different tag's file, must
	// survive every Sync call regardless of what this tag's entries are.
	if err := os.WriteFile(filepath.Join(dir, "operator-own"), []byte("nameserver 9.9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeResolverFile("othertag", "other-domain", []string{"8.8.8.8"}); err != nil {
		t.Fatal(err)
	}

	if err := Sync(tag, "", []Entry{
		{Domain: "domain1", Servers: addrs(t, "1.1.1.1")},
		{Domain: "domain2", Servers: addrs(t, "2.2.2.2")},
	}, nil); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Shrink to just domain1: domain2's file (owned by tag) must be removed.
	if err := Sync(tag, "", []Entry{
		{Domain: "domain1", Servers: addrs(t, "1.1.1.1")},
	}, nil); err != nil {
		t.Fatalf("Sync (shrink): %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "domain2")); !os.IsNotExist(err) {
		t.Fatalf("stale domain2 file not removed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "domain1")); err != nil {
		t.Fatalf("domain1 file unexpectedly gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "operator-own")); err != nil {
		t.Fatalf("unrelated operator file was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other-domain")); err != nil {
		t.Fatalf("another tag's file was touched: %v", err)
	}
}

func TestClearRemovesAllOwnedFiles(t *testing.T) {
	dir := withTempResolverDir(t)
	tag := "deadbeef"

	if err := Sync(tag, "", []Entry{
		{Domain: "domain1", Servers: addrs(t, "1.1.1.1")},
		{Domain: "domain2", Servers: addrs(t, "2.2.2.2")},
	}, nil); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := Clear(tag, ""); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Clear left files behind: %v", entries)
	}
}

// TestSyncCollapsesCaseVariantDomains proves two case-variant spellings of the
// same domain (e.g. from two peers, or the same admin at different times)
// land in exactly one file, not two that silently fight over the same
// case-insensitive-but-preserving APFS path. Before normalizeDomain existed,
// Sync would treat "Example.com" and "example.com" as different entries and
// write them independently — which on real APFS resolves to the same
// on-disk file, so whichever was written second silently won with no error.
// This temp-dir test can't reproduce the filesystem collision itself (Linux's
// temp dirs are case-sensitive), but it does prove the entries are collapsed
// to one logical domain before Sync ever gets to writing anything, which is
// what actually prevents the collision on a real Mac.
func TestSyncCollapsesCaseVariantDomains(t *testing.T) {
	dir := withTempResolverDir(t)
	tag := "deadbeef"

	if err := Sync(tag, "", []Entry{
		{Domain: "Example.com", Servers: addrs(t, "1.1.1.1")},
		{Domain: "example.com", Servers: addrs(t, "2.2.2.2")},
	}, nil); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("got %d files (%v), want exactly 1 — case-variant domains should collapse to one", len(entries), names)
	}
	if entries[0].Name() != "example.com" {
		t.Fatalf("file name = %q, want the lowercase form %q", entries[0].Name(), "example.com")
	}
}

func TestSyncOverwritesChangedServers(t *testing.T) {
	dir := withTempResolverDir(t)
	tag := "deadbeef"

	if err := Sync(tag, "", []Entry{{Domain: "domain1", Servers: addrs(t, "1.1.1.1")}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Sync(tag, "", []Entry{{Domain: "domain1", Servers: addrs(t, "9.9.9.9")}}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "domain1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "1.1.1.1") {
		t.Fatalf("stale server not overwritten:\n%s", data)
	}
	if !strings.Contains(string(data), "9.9.9.9") {
		t.Fatalf("new server missing:\n%s", data)
	}
}

// TestDumpReportsOwnedDomainsWithContent verifies Dump reads real file content
// back from disk (not cached state), includes only domains owned by the given
// tag, and strips the ownership marker line since it's bookkeeping, not
// something a reader trying to see what's registered needs to see.
func TestDumpReportsOwnedDomainsWithContent(t *testing.T) {
	withTempResolverDir(t)
	tag := "deadbeef"

	if err := Sync(tag, "", []Entry{
		{Domain: "domain1", Servers: addrs(t, "1.1.1.1")},
		{Domain: "domain2", Servers: addrs(t, "2.2.2.2", "3.3.3.3")},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeResolverFile("othertag", "other-domain", []string{"8.8.8.8"}); err != nil {
		t.Fatal(err)
	}

	out, err := Dump(tag, "")
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if !strings.Contains(out, "domain1") || !strings.Contains(out, "1.1.1.1") {
		t.Fatalf("dump missing domain1's content:\n%s", out)
	}
	if !strings.Contains(out, "domain2") || !strings.Contains(out, "2.2.2.2") || !strings.Contains(out, "3.3.3.3") {
		t.Fatalf("dump missing domain2's content:\n%s", out)
	}
	if strings.Contains(out, "other-domain") || strings.Contains(out, "8.8.8.8") {
		t.Fatalf("dump leaked another tag's entry:\n%s", out)
	}
	if strings.Contains(out, marker(tag)) {
		t.Fatalf("dump should strip the ownership marker line, not display it:\n%s", out)
	}

	// A tag that owns nothing gets a clear empty-state message, not an error
	// or blank output.
	empty, err := Dump("no-such-tag", "")
	if err != nil {
		t.Fatalf("Dump for unowned tag should not error: %v", err)
	}
	if empty == "" {
		t.Fatal("Dump for unowned tag should return a clear empty-state message, not blank output")
	}
}

// TestSyncSearchDomainsUnsupportedButRoutingStillApplies checks the contract
// documented on Sync: macOS has no interface-scoped mechanism for search
// domains, so a non-empty searchDomains argument must produce a clear,
// distinct error — but routing domains (entries) must still be applied
// normally rather than being skipped because of it.
func TestSyncSearchDomainsUnsupportedButRoutingStillApplies(t *testing.T) {
	dir := withTempResolverDir(t)
	tag := "deadbeef"

	err := Sync(tag, "", []Entry{{Domain: "domain1", Servers: addrs(t, "1.1.1.1")}}, []string{"corp.internal"})
	if err == nil {
		t.Fatal("expected an error naming search domains as unsupported")
	}
	if !errors.Is(err, ErrSearchDomainsUnsupported) {
		t.Fatalf("error should wrap ErrSearchDomainsUnsupported (callers like dnssync.go rely on errors.Is to avoid retrying a permanent condition forever), got: %v", err)
	}
	if !strings.Contains(err.Error(), "search domain") {
		t.Fatalf("error should mention search domains, got: %v", err)
	}

	// Routing domain must have been written despite the search-domain error.
	got, rerr := os.ReadFile(filepath.Join(dir, "domain1"))
	if rerr != nil {
		t.Fatalf("routing domain should still be applied: %v", rerr)
	}
	if !strings.Contains(string(got), "1.1.1.1") {
		t.Fatalf("domain1 file missing its server: %s", got)
	}

	// No search domains and no entries: no error, nothing to apply.
	if err := Sync(tag, "", nil, nil); err != nil {
		t.Fatalf("Sync with nothing to apply should not error: %v", err)
	}
}
