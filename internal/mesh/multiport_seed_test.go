package mesh

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// freeUDPPorts reserves n ephemeral UDP ports on loopback and immediately
// releases them, returning the numbers so a transport can be opened on them
// explicitly. transport.Options.ExtraPorts takes literal port numbers (unlike
// PrimaryPort, which accepts 0 for "pick one"), so a test that needs a node
// listening on several known ports has to choose them itself. There is a
// theoretical race between releasing a port here and rebinding it below; in a
// test container with nothing else competing for the ephemeral range it does
// not happen in practice, and if it ever did, the transport logs
// "extra listen port N not bound ... skipping" and the assertions below fail
// loudly rather than silently testing less than they claim to.
func freeUDPPorts(t *testing.T, n int) []int {
	t.Helper()
	conns := make([]*net.UDPConn, 0, n)
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatalf("reserve ephemeral udp port %d/%d: %v", i+1, n, err)
		}
		conns = append(conns, c)
		ports = append(ports, c.LocalAddr().(*net.UDPAddr).Port)
	}
	for _, c := range conns {
		c.Close()
	}
	return ports
}

// spinNodeMultiPort is spinNode with two additions this test needs: extra
// inbound listen ports (the peer side of config's extra_listen_ports), and
// seeds supplied through NetSpec.Seeds rather than AddSeed.
//
// The NetSpec.Seeds path is the point, not a convenience. It is exactly how
// cmd/gravinet delivers config-file seeds — buildOneNetSpec/resolveSeeds into
// newNetState — which means these seeds arrive without ever passing through
// addSeed, and are marked explicit by explicitSeedSet. Both properties matter
// here: bypassing addSeed is the original reason a configured seed had no
// seedOwner entry, and being explicit is what exempts it from install()'s
// stale-seed pruning. A test that used AddSeed instead would converge for an
// entirely different reason (install() would delete the surplus seeds outright
// rather than attributing them) and would therefore not test the deployed
// arrangement at all.
func spinNodeMultiPort(t *testing.T, name string, netID uint64, key string, self netip.Addr, extra []int, seeds []netip.AddrPort) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID:   name,
		Hostname: name,
		Nets:     []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self, Seeds: seeds}},
	})
	tr, err := transport.Open(transport.Options{
		BindAddr:    "127.0.0.1",
		PrimaryPort: 0,
		ExtraPorts:  extra,
		EnableV4:    true,
		Workers:     1,
		Handler:     eng.OnPacket,
	})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	eng.Attach(tr)
	eng.Start()
	return &testNode{eng, tr, dev}
}

// TestMultiPortSeedsConvergeAndStopRedialing is the deployment-shaped
// counterpart to TestOutboundHandshakeAttributesSeedOwner. That test proves
// the mechanism on a single seed; this one proves the property that actually
// matters in the field, on the configuration that made the bug catastrophic
// rather than cosmetic.
//
// The field case: resolveSeeds expands "host:port,port,..." into one seed per
// port (TestResolveSeedsMultiPort), and a peer running extra_listen_ports
// genuinely answers on every one of them, so every dial succeeds. One peer
// given 13 ports across two address families is 26 distinct seed addresses for
// a single node. Gossip attributes at most the one endpoint it reports; before
// the handshake path recorded attribution itself, the rest stayed unowned
// permanently. connectedTo matches only the live session's exact endpoint and
// connectedToSeedOwner had nothing to consult, so initSeedTick re-dialed every
// unattributed address on every 1s tick, forever — each dial completing a real
// handshake and reinstalling the session.
//
// So the assertion is convergence, in two parts. First: every seed address
// ends up attributed, which is bounded work — each address must be dialed once
// to be attributed, so a burst of len(seeds) handshakes at startup is expected
// and correct. Second, and the part that distinguishes a fix from the bug: once
// converged it stays quiet. The session object for the peer must survive
// several full initLoop ticks unreplaced, because install() overwrites
// ns.byNode on every completed handshake — so a surviving pointer is direct
// evidence that no re-handshake occurred, and a replaced one is direct
// evidence of exactly the churn this closes.
func TestMultiPortSeedsConvergeAndStopRedialing(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x9017)

	// B listens on its primary port plus five more, standing in for a peer
	// with extra_listen_ports set. Six rather than the field's twenty-six:
	// the mechanism is per-address and does not change with the count, and a
	// smaller number keeps the startup burst quick.
	const extraCount = 5
	extra := freeUDPPorts(t, extraCount)
	B := spinNodeMultiPort(t, "B", netID, key, netip.MustParseAddr("10.11.0.2"), extra, nil)

	// A is seeded with one address per port B answers on — the expansion
	// resolveSeeds would produce from a single "127.0.0.1:p1,p2,..." line.
	lo := netip.MustParseAddr("127.0.0.1")
	seeds := make([]netip.AddrPort, 0, extraCount+1)
	seeds = append(seeds, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	for _, p := range extra {
		seeds = append(seeds, netip.AddrPortFrom(lo, uint16(p)))
	}
	A := spinNodeMultiPort(t, "A", netID, key, netip.MustParseAddr("10.11.0.1"), nil, seeds)

	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	ns := A.eng.netSnapshot()[netID]
	if ns == nil {
		t.Fatal("network not started on A")
	}

	attributed := func() int {
		ns.mu.RLock()
		defer ns.mu.RUnlock()
		n := 0
		for _, s := range seeds {
			if ns.seedOwner[s] == "B" {
				n++
			}
		}
		return n
	}

	// Convergence. Every address must be dialed once to be attributed, so this
	// is a bounded burst, not a steady state.
	if !waitUntil(20*time.Second, func() bool { return attributed() == len(seeds) }) {
		t.Fatalf("only %d of %d seed addresses were attributed to B: every unattributed address is one initSeedTick re-dials on every tick, forever", attributed(), len(seeds))
	}

	// The predicate initSeedTick actually consults. Attribution is only useful
	// insofar as this comes back true for every address.
	for _, s := range seeds {
		if !A.eng.connectedToSeedOwner(ns, s) {
			t.Errorf("connectedToSeedOwner(%s) is false despite B being connected — initSeedTick would re-dial this address every tick", s)
		}
	}

	sessionForB := func() *peerSession {
		ns.mu.RLock()
		defer ns.mu.RUnlock()
		return ns.byNode["B"]
	}
	// Sessions for this network held engine-wide, including ones orphaned by a
	// re-handshake: install() adds each new session to e.sessions and leaves
	// older indices valid (glare handling — see its doc comment), so churn
	// shows up here as growth well before pruneDead reaps the orphans at
	// peerTimeout.
	countSessions := func() int {
		A.eng.mu.Lock()
		defer A.eng.mu.Unlock()
		n := 0
		for _, ps := range A.eng.sessions {
			if ps.net == ns {
				n++
			}
		}
		return n
	}

	before := sessionForB()
	if before == nil {
		t.Fatal("no session for B after convergence")
	}
	beforeCount := countSessions()

	// Quiescence across several full initLoop ticks (1s each).
	const quietTicks = 4
	time.Sleep(quietTicks*time.Second + 500*time.Millisecond)

	if after := sessionForB(); after != before {
		t.Fatalf("B's session was replaced during %d idle ticks: initLoop is still re-handshaking a peer it is already connected to, which is the churn this is meant to stop", quietTicks)
	}
	// Orphans being reaped would lower this, which is fine; growth is what
	// indicates fresh handshakes.
	if after := countSessions(); after > beforeCount {
		t.Errorf("session count for the network grew from %d to %d while idle — new handshakes are still completing", beforeCount, after)
	}
}
