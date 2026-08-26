package webadmin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The fixtures in testdata are not hand-written. They are the actual output of
// Kea 2.4.1 configured by gravinet's own renderKea, driven with real
// DISCOVER/REQUEST/RELEASE/DECLINE exchanges:
//
//	kea-leases4-journal.csv    the live memfile after that run (9 rows)
//	kea-leases4-compacted.csv  the same file after kea-lfc ran over it (4 rows)
//
// Both are in the repo because the interesting behavior of this format is not
// in its documentation — a declined lease loses its client identity, a release
// writes two rows, and LFC drops some states while keeping others. Fixtures
// generated from the real server are the only way those stay honest.

// leaseNow is a timestamp inside the lifetime of the fixture leases (they were
// issued with a 600s lifetime and expire around 1787713907-1787713910). Fixed
// rather than time.Now() so these tests do not start failing on their own.
var leaseNow = time.Unix(1787713800, 0)

func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

func byAddr(ls []dhcpLease) map[string]dhcpLease {
	m := make(map[string]dhcpLease, len(ls))
	for _, l := range ls {
		m[l.Address] = l
	}
	return m
}

// The headline property: a journal and its LFC compaction describe the same
// world, so the page must not appear to change contents when LFC fires.
func TestJournalAndCompactedFileAgree(t *testing.T) {
	j, err := readKeaLeases(fixture(t, "kea-leases4-journal.csv"), leaseNow)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	c, err := readKeaLeases(fixture(t, "kea-leases4-compacted.csv"), leaseNow)
	if err != nil {
		t.Fatalf("compacted: %v", err)
	}
	if len(j) != len(c) {
		t.Fatalf("journal gave %d leases, its own compaction gave %d:\n%+v\n%+v", len(j), len(c), j, c)
	}
	jm, cm := byAddr(j), byAddr(c)
	for addr, want := range cm {
		got, ok := jm[addr]
		if !ok {
			t.Errorf("%s is in the compacted file but not read from the journal", addr)
			continue
		}
		if got != want {
			t.Errorf("%s differs between journal and compaction:\n got %+v\nwant %+v", addr, got, want)
		}
	}
}

// A renewal appends a second row for one address. The later row wins, and the
// address appears once — not twice, which is what a naive reader produces and
// what an operator would see as a duplicate client.
func TestRenewalCollapsesToOneRow(t *testing.T) {
	ls, err := readKeaLeases(fixture(t, "kea-leases4-journal.csv"), leaseNow)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	var got dhcpLease
	for _, l := range ls {
		if l.Address == "10.1.1.10" {
			n++
			got = l
		}
	}
	if n != 1 {
		t.Fatalf("10.1.1.10 appears %d times; the journal has two rows for it and the later one wins", n)
	}
	// The journal's two rows differ only in expiry: 1787713907 then 1787713910.
	if got.Expire != 1787713910 {
		t.Errorf("expire = %d, want the later row's 1787713910 — an earlier row won", got.Expire)
	}
	if got.Hostname != "laptop" {
		t.Errorf("hostname = %q, want laptop", got.Hostname)
	}
}

// A released address is gone. Kea writes it twice — lifetime 0, then state 2 —
// and neither is a lease anyone holds.
func TestReleasedLeaseIsNotShown(t *testing.T) {
	ls, err := readKeaLeases(fixture(t, "kea-leases4-journal.csv"), leaseNow)
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := byAddr(ls)["10.1.1.14"]; ok {
		t.Fatalf("a released address is still listed as leased: %+v", l)
	}
}

// The finding that could not have come from the docs: Kea blanks hwaddr,
// client_id and hostname on a declined lease. The row is kept — the address is
// genuinely unavailable and an operator hunting a missing address needs to see
// why — but it carries no client, and this must not invent one.
func TestDeclinedLeaseIsKeptWithoutClientIdentity(t *testing.T) {
	ls, err := readKeaLeases(fixture(t, "kea-leases4-journal.csv"), leaseNow)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := byAddr(ls)["10.1.1.13"]
	if !ok {
		t.Fatal("the declined address is missing; it is unavailable and must be visible")
	}
	if !l.Declined || l.State != keaStateDeclined {
		t.Errorf("not flagged as declined: %+v", l)
	}
	if l.HWAddr != "" || l.ClientID != "" || l.Hostname != "" {
		t.Errorf("declined lease carries client identity %+v — Kea blanks these, so any value here is invented", l)
	}
	// The lifetime on a declined row is the decline probation period, not the
	// subnet's 600s. Callers must be able to tell, or they will render it as
	// a day-long lease.
	if l.ValidLifetime != 86400 {
		t.Errorf("valid_lifetime = %d, want the 86400 probation period from the real server", l.ValidLifetime)
	}
}

// The ordinary case, and that the client fields survive for it.
func TestActiveLeasesAreReported(t *testing.T) {
	ls, err := readKeaLeases(fixture(t, "kea-leases4-journal.csv"), leaseNow)
	if err != nil {
		t.Fatal(err)
	}
	m := byAddr(ls)
	for addr, host := range map[string]string{
		"10.1.1.10": "laptop", "10.1.1.11": "printer", "10.1.1.12": "nas",
	} {
		l, ok := m[addr]
		if !ok {
			t.Errorf("%s missing from the lease table", addr)
			continue
		}
		if l.Hostname != host {
			t.Errorf("%s: hostname %q, want %q", addr, l.Hostname, host)
		}
		if l.HWAddr == "" || l.ClientID == "" {
			t.Errorf("%s: lost its client identity: %+v", addr, l)
		}
		if l.SubnetID != 1 {
			t.Errorf("%s: subnet_id %d, want 1", addr, l.SubnetID)
		}
	}
}

// Time moves. Once the fixtures' 600s leases lapse, they stop being current
// even though Kea has not yet rewritten a thing — the file is unchanged and
// the answer still has to be right.
func TestExpiredLeasesDropOutWithoutAFileChange(t *testing.T) {
	later := time.Unix(1787713910+1, 0)
	ls, err := readKeaLeases(fixture(t, "kea-leases4-journal.csv"), later)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if !l.Declined {
			t.Errorf("%s is still listed after its lease expired: %+v", l.Address, l)
		}
	}
	// The declined address stays: its probation runs to 1787799711, well past
	// this point, and it is still not available.
	if _, ok := byAddr(ls)["10.1.1.13"]; !ok {
		t.Error("the declined address vanished; it is still unavailable")
	}
}

// A node that has never run Kea has no file. That is a normal state, not a
// failure, and must not render as an error.
func TestMissingLeaseFileIsNotAnError(t *testing.T) {
	ls, err := readKeaLeases(filepath.Join(t.TempDir(), "nope.csv"), leaseNow)
	if err != nil {
		t.Fatalf("a missing lease file reported an error: %v", err)
	}
	if len(ls) != 0 {
		t.Fatalf("expected no leases, got %+v", ls)
	}
}

// LFC renames the live file aside and writes a fresh one, so a read can land
// in a window where the main path does not exist. Falling back to the
// compacted copy beats telling an operator their fully-leased LAN is empty.
func TestFallsBackToCompactedFileDuringLFC(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "kea-leases4.csv")
	src, err := os.ReadFile(fixture(t, "kea-leases4-compacted.csv"))
	if err != nil {
		t.Fatal(err)
	}
	// Only the ".2" exists: mid-LFC, the live file has been renamed away.
	if err := os.WriteFile(live+".2", src, 0o644); err != nil {
		t.Fatal(err)
	}
	ls, err := readKeaLeases(live, leaseNow)
	if err != nil {
		t.Fatalf("mid-LFC read failed: %v", err)
	}
	if len(ls) == 0 {
		t.Fatal("read nothing mid-LFC; an operator would see an empty lease table for a fully leased LAN")
	}
}

// Kea appends while this reads, so the final row can be half-written. The
// torn row is dropped; every complete row before it survives.
func TestTornTrailingRowDoesNotLoseTheTable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kea-leases4.csv")
	src, err := os.ReadFile(fixture(t, "kea-leases4-compacted.csv"))
	if err != nil {
		t.Fatal(err)
	}
	torn := append(append([]byte{}, src...), []byte("10.1.1.99,0c:13:9e:27:00:99,01:0c:1")...)
	if err := os.WriteFile(p, torn, 0o644); err != nil {
		t.Fatal(err)
	}
	ls, err := readKeaLeases(p, leaseNow)
	if err != nil {
		t.Fatalf("a torn trailing row failed the whole read: %v", err)
	}
	full, err := readKeaLeases(fixture(t, "kea-leases4-compacted.csv"), leaseNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) < len(full) {
		t.Fatalf("lost complete rows to a torn write: got %d, want at least %d", len(ls), len(full))
	}
}

// Header-driven indexing, not fixed positions: pool_id was added to this file
// in Kea 2.3, and a version that inserts a column must not shift every field.
func TestColumnsResolveByHeaderNotPosition(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kea-leases4.csv")
	// Same data, columns reordered and an unknown one inserted up front.
	body := "extra_new_column,state,address,hostname,expire,valid_lifetime,subnet_id,hwaddr,client_id\n" +
		"xyz,0,10.1.1.10,laptop,1787713910,600,1,0c:13:9e:27:00:11,01:0c:13:9e:27:00:11\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ls, err := readKeaLeases(p, leaseNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 {
		t.Fatalf("want 1 lease, got %+v", ls)
	}
	got := ls[0]
	if got.Address != "10.1.1.10" || got.Hostname != "laptop" || got.HWAddr != "0c:13:9e:27:00:11" || got.ValidLifetime != 600 {
		t.Fatalf("fields resolved by position rather than by header name: %+v", got)
	}
}

// A file that is not a lease file at all is an error worth reporting, rather
// than silently rendering as "no leases".
func TestNonLeaseFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kea-leases4.csv")
	if err := os.WriteFile(p, []byte("some,other,file\n1,2,3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readKeaLeases(p, leaseNow); err == nil {
		t.Fatal("a file with no address column was accepted as a lease file")
	}
}

// Kea writes the header the moment it starts, before anyone leases. That is
// "no leases yet", not a broken file.
func TestHeaderOnlyFileIsEmptyNotAnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kea-leases4.csv")
	hdr := "address,hwaddr,client_id,valid_lifetime,expire,subnet_id,fqdn_fwd,fqdn_rev,hostname,state,user_context,pool_id\n"
	if err := os.WriteFile(p, []byte(hdr), 0o644); err != nil {
		t.Fatal(err)
	}
	ls, err := readKeaLeases(p, leaseNow)
	if err != nil {
		t.Fatalf("a freshly-started Kea's lease file reported an error: %v", err)
	}
	if len(ls) != 0 {
		t.Fatalf("want no leases, got %+v", ls)
	}
}

// Row order is stable across reads of an unchanged file, so the table does not
// reshuffle under an operator between refreshes.
func TestOrderIsStable(t *testing.T) {
	var first []string
	for i := 0; i < 5; i++ {
		ls, err := readKeaLeases(fixture(t, "kea-leases4-journal.csv"), leaseNow)
		if err != nil {
			t.Fatal(err)
		}
		var order []string
		for _, l := range ls {
			order = append(order, l.Address)
		}
		if i == 0 {
			first = order
			continue
		}
		if len(order) != len(first) {
			t.Fatalf("read %d returned %d rows, first returned %d", i, len(order), len(first))
		}
		for j := range order {
			if order[j] != first[j] {
				t.Fatalf("row order changed between reads: %v then %v", first, order)
			}
		}
	}
}
