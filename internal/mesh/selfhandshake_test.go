package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestSelfHandshakeIsRejected simulates the NAT-hairpin case a symmetric-NAT
// node can hit through a relay/hub: it dials what it believes is a different
// peer's advertised endpoint, but the packet loops back to its own listener.
// The resulting handshake authenticates cleanly (it's really our own PSK and
// our own ephemeral keys) and claims our own node id — before this fix,
// nothing checked for that, so install() registered a live peer session with
// ourselves, and the admin UI's peers table showed the current node twice:
// once as the inert "this node" row, once as an ordinary connected peer.
//
// A single seed pointed at the node's own listening address reproduces this
// deterministically without needing real NAT hardware: both the outbound
// handshake (onHSResp, initiator role) and any handshake it happens to
// receive first (onHSInit, responder role) see a claimed node id equal to
// their own, and PeerCount must stay at zero either way.
func TestSelfHandshakeIsRejected(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatalf("NewKeySet: %v", err)
	}
	const netID = uint64(0x5EF0)

	dev := newFakeDev("hairpin")
	eng := NewEngine(Options{
		NodeID:   "hairpin-node",
		Hostname: "hairpin",
		Nets: []NetSpec{{
			ID: netID, Name: "m", Keys: ks, Dev: dev,
			Self4: netip.MustParseAddr("10.9.0.1"),
		}},
	})
	tr, err := transport.Open(transport.Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1,
		Handler: eng.OnPacket,
	})
	if err != nil {
		t.Fatalf("transport.Open: %v", err)
	}
	eng.Attach(tr)
	eng.Start()
	defer func() {
		dev.Close()
		eng.Stop()
		tr.Close()
	}()

	// Point the node's own seed list at its own listening port — the wire-level
	// symptom of a hairpin: a dial that comes back to the same process instead
	// of reaching a distinct peer.
	self := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(tr.Port()))
	eng.AddSeed(netID, self)

	// Give initLoop (1s ticker) several chances to dial and complete a
	// handshake with itself, and confirm it never becomes a registered peer.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := eng.PeerCount(netID); got != 0 {
			t.Fatalf("PeerCount(netID) = %d, want 0 (installed a session with our own node id)", got)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if peers := eng.ListPeers(netID); len(peers) != 0 {
		t.Fatalf("ListPeers(netID) = %+v, want empty", peers)
	}
}
