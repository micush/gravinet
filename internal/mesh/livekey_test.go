package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestLiveKeyReload proves authentication keys are dynamic: a node that shares
// no key with a peer can be made to trust it by reloading its key set at
// runtime, with no engine restart.
func TestLiveKeyReload(t *testing.T) {
	const netID = uint64(0xC0FFEE)
	k1, _ := crypto.GenerateKey()
	k2, _ := crypto.GenerateKey()

	A := spinNode(t, "A", netID, k1, netip.MustParseAddr("10.9.0.1"))
	B := spinNode(t, "B", netID, k2, netip.MustParseAddr("10.9.0.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	port := func(n *testNode) netip.AddrPort { return netip.AddrPortFrom(lo, uint16(n.tr.Port())) }
	A.eng.AddSeed(netID, port(B))
	B.eng.AddSeed(netID, port(A))

	// Distinct keys: no session should ever form.
	if waitUntil(2*time.Second, func() bool { return len(A.eng.PeerEndpoints(netID)) > 0 }) {
		t.Fatal("A and B share no key, yet a session formed")
	}

	// Add k2 to A's set at runtime — the operation key management performs.
	ks, _ := crypto.NewKeySet([]string{k1, k2})
	if err := A.eng.ReloadRuntime(netID, NetSpec{ID: netID, Keys: ks}); err != nil {
		t.Fatalf("live reload: %v", err)
	}

	// Without restarting A, B must now authenticate.
	if !waitUntil(10*time.Second, func() bool {
		return len(A.eng.PeerEndpoints(netID)) > 0 && len(B.eng.PeerEndpoints(netID)) > 0
	}) {
		t.Fatal("after live key reload, the session did not come up without a restart")
	}
}
