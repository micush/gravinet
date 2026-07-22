package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestFirewallAddDeleteLive proves that adding and removing rules through the
// engine API the web/CLI actually call (FirewallAdd / FirewallDelete) takes
// effect on real traffic immediately — no restart, no firewall toggle. This is
// the exact path the user reported as broken.
func TestFirewallAddDeleteLive(t *testing.T) {
	const netID = uint64(0xF1255)
	key, _ := crypto.GenerateKey()

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.31.0.1"))

	ks, _ := crypto.NewKeySet([]string{key})
	devB := newFakeDev("B")
	engB := NewEngine(Options{
		NodeID: "B", Hostname: "B",
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: devB, Self4: netip.MustParseAddr("10.31.0.2"),
			FirewallEnabled: true, // firewall ON, but no rules yet => allow-all
		}},
	})
	trB, err := openTestTransport(engB)
	if err != nil {
		t.Fatal(err)
	}
	engB.Attach(trB)
	engB.Start()
	B := &testNode{engB, trB, devB}
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
	if !waitUntil(15*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}

	src := netip.MustParseAddr("10.31.0.1")
	dst := netip.MustParseAddr("10.31.0.2")
	drain := func() {
		for {
			select {
			case <-B.dev.out:
			default:
				return
			}
		}
	}
	probe := func(what string) bool {
		drain()
		A.dev.in <- makeL4Addr(src, dst, 6, 80)
		A.dev.in <- makeL4Addr(src, dst, 17, 53)
		sawTCP := false
		deadline := time.After(5 * time.Second)
		for {
			select {
			case got := <-B.dev.out:
				switch got[9] {
				case 6:
					sawTCP = true
				case 17:
					return sawTCP
				}
			case <-deadline:
				t.Fatalf("%s: sentinel never arrived", what)
			}
		}
	}

	// Baseline: no rules => tcp/80 passes.
	if !probe("no rules") {
		t.Fatal("with no rules tcp/80 should pass")
	}

	// Add a deny rule via the ENGINE API (what the web/CLI call). Must drop live.
	added, err := B.eng.FirewallAdd(netID, FirewallRule{Action: "deny", Direction: "in", Proto: "tcp", DstPortMin: 80, DstPortMax: 80}, -1)
	if err != nil {
		t.Fatalf("FirewallAdd: %v", err)
	}
	if probe("after FirewallAdd") {
		t.Fatal("adding a deny rule via FirewallAdd should drop tcp/80 live (no restart)")
	}

	// Delete it via the engine API. Must pass again live.
	if err := B.eng.FirewallDelete(netID, []uint64{added.ID}); err != nil {
		t.Fatalf("FirewallDelete: %v", err)
	}
	if !probe("after FirewallDelete") {
		t.Fatal("deleting the rule via FirewallDelete should let tcp/80 through live")
	}
}
