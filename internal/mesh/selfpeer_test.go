package mesh

import (
	"net/netip"
	"testing"
)

// TestSelfPeer verifies SelfPeer reports this node's own identity (hostname,
// node id, overlay address) for a configured network, in the same PeerInfo
// shape ListPeers uses — this is what lets the admin UI's peers table show
// the current node alongside the peers it actually connects to (previously
// the node was invisible in its own peer list). It should also report
// ok=false for a network id this node doesn't have.
func TestSelfPeer(t *testing.T) {
	e := NewEngine(Options{NodeID: "self-id-123", Hostname: "myhost", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]

	// Before an address is assigned, SelfPeer should still report identity
	// with empty overlay fields rather than failing.
	pi, ok := e.SelfPeer(1)
	if !ok {
		t.Fatalf("SelfPeer(1) ok = false, want true")
	}
	if pi.NodeID != "self-id-123" || pi.Hostname != "myhost" {
		t.Fatalf("SelfPeer(1) = %+v, want NodeID=self-id-123 Hostname=myhost", pi)
	}
	if pi.Overlay4 != "" || pi.Overlay6 != "" {
		t.Fatalf("SelfPeer(1) overlay = %q/%q before assignment, want both empty", pi.Overlay4, pi.Overlay6)
	}

	// Once an address is assigned (normally via DAD), it should show up.
	ns.mu.Lock()
	ns.self4 = netip.MustParseAddr("10.0.0.5")
	ns.mu.Unlock()

	pi, ok = e.SelfPeer(1)
	if !ok {
		t.Fatalf("SelfPeer(1) ok = false after address assignment, want true")
	}
	if pi.Overlay4 != "10.0.0.5" {
		t.Fatalf("SelfPeer(1).Overlay4 = %q, want 10.0.0.5", pi.Overlay4)
	}

	// A network id that isn't configured on this node should report ok=false
	// rather than a zero-value row that could be mistaken for a real self
	// entry on that network.
	if _, ok := e.SelfPeer(999); ok {
		t.Fatalf("SelfPeer(999) ok = true for an unconfigured network, want false")
	}
}

// TestSelfPeerCarriesItsOwnNote covers the read path that makes a note on this
// node's own row possible in the admin UI's peers table.
//
// The write path never needed anything: PeerNotes is a map keyed by node id and
// PeerSetNotes has always accepted any non-empty id, this node's own included.
// What was missing was a way to read one back — self is not in ListPeers and not
// in DisabledPeers, the two places a note reaches the UI from, so a note stored
// under this node's id existed in config and was visible nowhere. The peers
// table, seeing nowhere to show it, declined to offer the edit at all.
func TestSelfPeerCarriesItsOwnNote(t *testing.T) {
	e := NewEngine(Options{NodeID: "self-id-123", Hostname: "myhost", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]

	// No note set: the field is empty, not absent or defaulted to something.
	if pi, ok := e.SelfPeer(1); !ok || pi.Notes != "" {
		t.Fatalf("SelfPeer(1).Notes = %q with no note set, want empty", pi.Notes)
	}

	// A note keyed by this node's own id is exactly a peerNotes entry, which
	// is how config reload delivers every other peer's note (applyPeerNotes).
	e.applyPeerNotes(ns, map[string]string{
		"self-id-123": "rack 3, do not reboot in hours",
		"other-node":  "someone else's box",
	})

	pi, ok := e.SelfPeer(1)
	if !ok {
		t.Fatal("SelfPeer(1) ok = false, want true")
	}
	if pi.Notes != "rack 3, do not reboot in hours" {
		t.Fatalf("SelfPeer(1).Notes = %q, want this node's own note", pi.Notes)
	}
	// The other node's note must not leak onto the self row.
	if pi.Notes == "someone else's box" {
		t.Fatal("SelfPeer(1) picked up another node's note")
	}
	// Clearing it (config drops the key entirely, per PeerSetNotes) empties
	// the field rather than leaving the previous value behind.
	e.applyPeerNotes(ns, map[string]string{"other-node": "someone else's box"})
	if pi, _ := e.SelfPeer(1); pi.Notes != "" {
		t.Fatalf("SelfPeer(1).Notes = %q after the note was cleared, want empty", pi.Notes)
	}
}
