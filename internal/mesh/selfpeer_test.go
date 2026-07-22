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
