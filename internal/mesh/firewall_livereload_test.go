package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestFirewallLiveReload proves the firewall responds to config changes LIVE —
// no restart — through the same ReloadRuntime path the web API and control
// socket use. It exercises all four behaviors the user cares about with real
// packets flowing across the mesh:
//  1. an enabled deny rule actually drops matching traffic,
//  2. disabling that rule (live) lets the traffic through,
//  3. re-enabling the rule (live) drops it again,
//  4. disabling the whole firewall (live) lets everything through,
//     and re-enabling it restores filtering.
func TestFirewallLiveReload(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xF1244)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.21.0.1"))

	ks, _ := crypto.NewKeySet([]string{key})
	devB := newFakeDev("B")
	denyTCP80 := FirewallRule{Action: "deny", Direction: "in", Proto: "tcp", DstPortMin: 80, DstPortMax: 80}
	engB := NewEngine(Options{
		NodeID: "B", Hostname: "B",
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: devB, Self4: netip.MustParseAddr("10.21.0.2"),
			FirewallEnabled: true, FirewallRules: []FirewallRule{denyTCP80},
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

	src := netip.MustParseAddr("10.21.0.1")
	dst := netip.MustParseAddr("10.21.0.2")

	drain := func() {
		for {
			select {
			case <-B.dev.out:
			default:
				return
			}
		}
	}
	// probe sends the filtered packet (tcp/80) followed by a sentinel (udp/53)
	// that is never filtered. Packets preserve order across the tunnel, so if the
	// tcp/80 arrives before the sentinel the rule let it through. Returns true if
	// tcp/80 passed.
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
					return sawTCP // sentinel arrived; verdict is in
				}
			case <-deadline:
				t.Fatalf("%s: sentinel packet never arrived", what)
			}
		}
	}

	reload := func(enabled bool, rules []FirewallRule) {
		if err := B.eng.ReloadRuntime(netID, NetSpec{
			ID: netID, FirewallEnabled: enabled, FirewallRules: rules,
		}); err != nil {
			t.Fatalf("reload: %v", err)
		}
	}

	// 1. enabled + deny rule => tcp/80 dropped.
	if probe("rule active") {
		t.Fatal("enabled deny rule should drop tcp/80")
	}
	// 2. disable the rule live => tcp/80 passes (no restart).
	reload(true, []FirewallRule{{Action: "deny", Direction: "in", Proto: "tcp", DstPortMin: 80, DstPortMax: 80, Disabled: true}})
	if !probe("rule disabled live") {
		t.Fatal("disabling the rule live should let tcp/80 through")
	}
	// 3. re-enable the rule live => dropped again.
	reload(true, []FirewallRule{denyTCP80})
	if probe("rule re-enabled live") {
		t.Fatal("re-enabling the rule live should drop tcp/80 again")
	}
	// 4. disable the whole firewall live => everything passes.
	reload(false, []FirewallRule{denyTCP80})
	if !probe("firewall disabled live") {
		t.Fatal("disabling the firewall live should let tcp/80 through")
	}
	// 5. re-enable the firewall live => filtering restored.
	reload(true, []FirewallRule{denyTCP80})
	if probe("firewall re-enabled live") {
		t.Fatal("re-enabling the firewall live should drop tcp/80 again")
	}
}
