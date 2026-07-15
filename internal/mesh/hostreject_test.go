package mesh

import (
	"net/netip"
	"os"
	"strings"
	"testing"
)

// TestHostRejectFiltersLearned verifies the local host-reject filter: a rejected
// hostname is still learned and relayed (so lifting the reject restores it
// instantly) but is withheld from this node's hosts file, while allowed records
// are written. Matching is case-insensitive, and lifting the reject live makes
// the record reappear on the next sync.
func TestHostRejectFiltersLearned(t *testing.T) {
	dir := t.TempDir()
	hp := dir + "/hosts"
	if err := os.WriteFile(hp, []byte("127.0.0.1 localhost\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
		HostsSync: true, HostsPath: hp, HostReject: []string{"Blocked.local"}, // mixed case on purpose
	}}})
	ns := e.netSnapshot()[1]
	ps := &peerSession{net: ns, nodeID: "peer1"}

	// Predicate is case-insensitive.
	if !ns.hostRejected("blocked.local") || !ns.hostRejected("BLOCKED.LOCAL") {
		t.Error("reject match should be case-insensitive")
	}
	if ns.hostRejected("ok.local") {
		t.Error("ok.local should not be rejected")
	}

	// Learn one rejected and one allowed record (onHostAdd re-syncs the file).
	e.onHostAdd(ps, encodeHostAdd("peer1", "blocked.local", netip.MustParseAddr("10.0.0.9"))[1:])
	e.onHostAdd(ps, encodeHostAdd("peer1", "ok.local", netip.MustParseAddr("10.0.0.10"))[1:])

	// Both are still learned — reject is a write-time filter, not a learn drop.
	ns.mu.RLock()
	_, lb := ns.learnedHosts[hostKey("peer1", "blocked.local")]
	_, lo := ns.learnedHosts[hostKey("peer1", "ok.local")]
	ns.mu.RUnlock()
	if !lb || !lo {
		t.Fatalf("both records should be learned: blocked=%v ok=%v", lb, lo)
	}

	// The hosts file contains the allowed record but not the rejected one.
	if s := readFile(t, hp); strings.Contains(s, "blocked.local") || !strings.Contains(s, "ok.local") {
		t.Fatalf("reject not applied to hosts file:\n%s", s)
	}

	// Lift the reject live; the previously-rejected record reappears on re-sync.
	e.reloadHosts(ns, nil, nil)
	if s := readFile(t, hp); !strings.Contains(s, "blocked.local") {
		t.Fatalf("after lifting reject, host should reappear:\n%s", s)
	}

	// Re-apply the reject live; it drops out again without re-learning.
	e.reloadHosts(ns, nil, []string{"blocked.local"})
	if s := readFile(t, hp); strings.Contains(s, "blocked.local") {
		t.Fatalf("re-applied reject should remove the host:\n%s", s)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
