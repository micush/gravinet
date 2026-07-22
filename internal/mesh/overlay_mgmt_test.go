package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func spinMgmtSubnet(t *testing.T, name string, netID uint64, key string, self netip.Addr, subnet netip.Prefix, webPort uint16) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID: name, Hostname: name, Managed: true, WebPort: webPort,
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self, Subnet4: subnet}},
	})
	tr, err := transport.Open(transport.Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	eng.Attach(tr)
	eng.Start()
	return &testNode{eng, tr, dev}
}

// TestRemoteMgmtAuthorization reproduces the "no networks from remote peers" bug.
// For the management proxy + overlay-source auth to work, each node's
// OverlayContains must accept a connected peer's overlay address. This checks the
// realistic case (subnet configured) AND documents the broken case (no subnet),
// which is what join-by-id nodes hit until restart.
func TestRemoteMgmtAuthorization(t *testing.T) {
	const netID = uint64(0x5150)
	key, _ := crypto.GenerateKey()
	subnet := netip.MustParsePrefix("10.50.0.0/24")
	aAddr := netip.MustParseAddr("10.50.0.1")
	bAddr := netip.MustParseAddr("10.50.0.2")

	A := spinMgmtSubnet(t, "A", netID, key, aAddr, subnet, 8443)
	B := spinMgmtSubnet(t, "B", netID, key, bAddr, subnet, 8443)
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

	if !waitUntil(8*time.Second, func() bool {
		mp := A.eng.ManagedPeers(time.Minute)
		return len(mp) == 1 && mp[0].NodeID == "B" && mp[0].Overlay4 == bAddr && mp[0].WebPort == 8443
	}) {
		t.Fatalf("A did not discover B as manageable; got %+v", A.eng.ManagedPeers(time.Minute))
	}

	// With a subnet configured, A must accept B's overlay address (proxy target)
	// and B must accept A's (overlay-source auth). This is what the web proxy and
	// authed() now depend on.
	if !A.eng.OverlayContains(bAddr) {
		t.Error("A.OverlayContains(B overlay) = false: proxy to B would be 403'd")
	}
	if !B.eng.OverlayContains(aAddr) {
		t.Error("B.OverlayContains(A overlay) = false: B would reject A's proxied request (401)")
	}
}

// TestOverlayContainsNoSubnetIsBroken documents the regression directly: without
// a subnet on the netState (join-by-id before restart), OverlayContains rejects
// even a node's own legitimate overlay address — breaking all remote management.
func TestOverlayContainsNoSubnetIsBroken(t *testing.T) {
	const netID = uint64(0x5151)
	key, _ := crypto.GenerateKey()
	N := spinManaged(t, "N", netID, key, netip.MustParseAddr("10.60.0.1"), true, 8443)
	defer func() { N.dev.Close(); N.eng.Stop(); N.tr.Close() }()

	// No Subnet4 was set (as in join-by-id). OverlayContains can't authorize.
	if N.eng.OverlayContains(netip.MustParseAddr("10.60.0.2")) {
		t.Skip("subnet inference already works; regression not present")
	}
	t.Log("confirmed: with no subnet on the netState, OverlayContains rejects valid overlay peers (root cause of 'no networks')")
}
