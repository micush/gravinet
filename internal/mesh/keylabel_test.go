package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestPeerInfoKeyLabel proves ListPeers reports which of this node's own key
// labels authenticated a given peer's session — the admin UI's peers-table
// "key" column. It builds nodes directly (rather than via spinNode, which
// doesn't set KeyLabels) so each side's NetSpec.KeyLabels can be set
// independently, mirroring how the two ends of a real mesh may label the very
// same shared key differently — labels are local display metadata only, never
// compared between peers.
func TestPeerInfoKeyLabel(t *testing.T) {
	keyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	raw, err := crypto.DecodeKey(keyB64)
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	id := crypto.DeriveKeyID(raw)
	const netID = uint64(0x7E7A1)

	spin := func(name, label string, self netip.Addr) *testNode {
		ks, err := crypto.NewKeySet([]string{keyB64})
		if err != nil {
			t.Fatalf("keyset: %v", err)
		}
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID:   name,
			Hostname: name,
			Nets: []NetSpec{{
				ID: netID, Name: "n", Keys: ks, KeyLabels: map[crypto.KeyID]string{id: label},
				Dev: dev, Self4: self,
			}},
		})
		tr, err := transport.Open(transport.Options{
			BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket,
		})
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		eng.Attach(tr)
		eng.Start()
		return &testNode{eng, tr, dev}
	}

	// The two sides deliberately use different labels for the identical shared
	// key, to prove each side reports its own.
	A := spin("A", "prod-2026", netip.MustParseAddr("10.77.0.1"))
	B := spin("B", "b-side-label", netip.MustParseAddr("10.77.0.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	if !waitUntil(15*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatal("A-B did not connect")
	}

	var gotA, gotB string
	if !waitUntil(5*time.Second, func() bool {
		for _, p := range A.eng.ListPeers(netID) {
			gotA = p.KeyLabel
		}
		for _, p := range B.eng.ListPeers(netID) {
			gotB = p.KeyLabel
		}
		return gotA != "" && gotB != ""
	}) {
		t.Fatalf("KeyLabel not populated in time: A saw %q, B saw %q", gotA, gotB)
	}
	if gotA != "prod-2026" {
		t.Fatalf("A's ListPeers KeyLabel = %q, want %q (A's own label for the key)", gotA, "prod-2026")
	}
	if gotB != "b-side-label" {
		t.Fatalf("B's ListPeers KeyLabel = %q, want %q (B's own label for the key)", gotB, "b-side-label")
	}
}
