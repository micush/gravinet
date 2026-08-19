package mesh

import (
	"bytes"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"gravinet/internal/logx"
)

// tcpHandshakeGrace defaults to 10s in production; shortened once here
// for the whole test binary so watchTCPHandshake's background goroutines
// (spawned by every successful ensureTCP call, including in tests that
// aren't specifically testing this) resolve quickly rather than lingering for
// the real 10s past their own test's return — which would otherwise race
// against any later test's use of the same package-level var.
func init() {
	tcpHandshakeGrace = 50 * time.Millisecond
}

// fakeTCP is a Sender that also implements tcpDialer, recording dials.
type fakeTCP struct {
	mu     sync.Mutex
	dialed []netip.AddrPort
	has    map[netip.AddrPort]bool
}

func (f *fakeTCP) Send(netip.AddrPort, []byte) error { return nil }

func (f *fakeTCP) DialTCP(to netip.AddrPort) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialed = append(f.dialed, to)
	if f.has == nil {
		f.has = map[netip.AddrPort]bool{}
	}
	f.has[to] = true
	return nil
}

func (f *fakeTCP) HasTCP(to netip.AddrPort) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.has[to]
}

func (f *fakeTCP) dials() []netip.AddrPort {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netip.AddrPort(nil), f.dialed...)
}

func tcpEngine(t *testing.T, port int) (*Engine, *fakeTCP, *netState) {
	t.Helper()
	e := NewEngine(Options{
		NodeID:  "self",
		TCPPort: port,
		Nets:    []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	f := &fakeTCP{has: map[netip.AddrPort]bool{}}
	e.Attach(f)
	ns := e.netSnapshot()[1]
	if ns == nil {
		t.Fatal("network not created")
	}
	return e, f, ns
}

// TestEnsureTCPDialsAndSeeds: when UDP to a seed is failing, the engine
// dials the peer's :443 TCP and registers it as a seed so the next init
// tick hands the handshake to the TLS path.
func TestEnsureTCPDialsAndSeeds(t *testing.T) {
	e, f, ns := tcpEngine(t, 443)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	fb := netip.MustParseAddrPort("203.0.113.7:443")

	e.ensureTCP(ns, seed)

	// The dial runs off the init loop; wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	// Containment, not an exact count. The flat candidate model dials every
	// plausible candidate — here the seed's own port alongside the resolved
	// one — rather than choosing between them. Choosing is what forced the old
	// code to derive a port, and deriving is what let one peer's port be used
	// for another (two nodes behind one NAT, tcp/65432 and udp/65432).
	if !dialedContains(f, fb) {
		t.Fatalf("expected a dial to %s, got %v", fb, f.dials())
	}

	ns.mu.RLock()
	seeds := append([]netip.AddrPort(nil), ns.seeds...)
	ns.mu.RUnlock()
	found := false
	for _, s := range seeds {
		if s == fb {
			found = true
		}
	}
	if !found {
		t.Fatalf("TCP endpoint %s not added as seed; seeds=%v", fb, seeds)
	}

	// Idempotent: with a TCP connection already up (HasTCP true), no
	// re-dial of anything already dialed. Compared against the set from the
	// first pass rather than against a fixed count, since the candidate set
	// may legitimately hold more than one address.
	before := len(f.dials())
	e.ensureTCP(ns, seed)
	time.Sleep(100 * time.Millisecond)
	if d := f.dials(); len(d) != before {
		t.Fatalf("expected no further dials (was %d), got %d: %v", before, len(d), d)
	}
}

// TestEnsureTCPSamePortDials: when the TCP port equals the seed's port
// (the default, both 65432), the engine still dials the TLS at that
// endpoint — and does not add a duplicate seed, since fb == seed.
func TestEnsureTCPSamePortDials(t *testing.T) {
	e, f, ns := tcpEngine(t, 65432)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")

	ns.mu.RLock()
	seedsBefore := len(ns.seeds)
	ns.mu.RUnlock()

	e.ensureTCP(ns, seed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if d := f.dials(); len(d) != 1 || d[0] != seed {
		t.Fatalf("expected one dial to %s, got %v", seed, d)
	}
	ns.mu.RLock()
	seedsAfter := len(ns.seeds)
	ns.mu.RUnlock()
	if seedsAfter != seedsBefore {
		t.Fatalf("same-alternate ports should not add a seed: before=%d after=%d", seedsBefore, seedsAfter)
	}
}

// TestEnsureTCPDisabled: with the TCP port at 0, no dialing happens.
func TestEnsureTCPDisabled(t *testing.T) {
	e, f, ns := tcpEngine(t, 0)
	e.ensureTCP(ns, netip.MustParseAddrPort("203.0.113.7:65432"))
	time.Sleep(100 * time.Millisecond)
	if d := f.dials(); len(d) != 0 {
		t.Fatalf("expected no dial when TCP disabled, got %v", d)
	}
}

// TestEnsureTCPNoDialerIsNoop: a UDP-only transport (no tcpDialer)
// must be tolerated without panic.
func TestEnsureTCPNoDialerIsNoop(t *testing.T) {
	e := NewEngine(Options{
		NodeID:  "self",
		TCPPort: 443,
		Nets:    []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	e.Attach(nopSender{}) // implements Sender but not tcpDialer
	ns := e.netSnapshot()[1]
	e.ensureTCP(ns, netip.MustParseAddrPort("203.0.113.7:65432")) // must not panic
}

// slowTCP wraps fakeTCP with an artificial delay before
// DialTCP completes, opening a window during which concurrent callers
// could race past the already-connected/already-has-TCP checks if
// nothing coalesces them.
type slowTCP struct {
	*fakeTCP
	delay time.Duration
}

func (f *slowTCP) DialTCP(to netip.AddrPort) error {
	time.Sleep(f.delay)
	return f.fakeTCP.DialTCP(to)
}

// TestEnsureTCPCoalescesConcurrentDials reproduces the real-world
// failure mode directly: a peer whose seed list has accumulated many stale
// duplicate entries (same IP, different historically-observed ports — see
// AddSeed's exact-match-only dedup) all resolve to the same TCP address,
// and initLoop fires ensureTCP for every one of them in a single
// synchronous pass while each dial runs asynchronously. Without coalescing,
// many of these race past the checks and each independently dial, producing
// a burst of duplicate "established tcp" log lines within the same
// tick — exactly the pattern seen in production logs. With the fix, only one
// dial should ever be in flight for a given TCP address at a time.
func TestEnsureTCPCoalescesConcurrentDials(t *testing.T) {
	inner := &fakeTCP{has: map[netip.AddrPort]bool{}}
	f := &slowTCP{fakeTCP: inner, delay: 100 * time.Millisecond}
	e := NewEngine(Options{
		NodeID:  "self",
		TCPPort: 443,
		Nets:    []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	e.Attach(f)
	ns := e.netSnapshot()[1]

	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.ensureTCP(ns, seed)
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // let any (wrongly) racing extra dials land
	// The claim must still coalesce: each distinct candidate address is dialed
	// at most once no matter how many callers arrive together. What is no
	// longer asserted is that there is only one address — the set may hold
	// several, and each is separately coalesced.
	seen := map[netip.AddrPort]int{}
	for _, d := range f.dials() {
		seen[d]++
	}
	for addr, n := range seen {
		if n != 1 {
			t.Fatalf("address %s dialed %d times despite 30 concurrent callers; the claim did not coalesce", addr, n)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no dial at all")
	}
}

// TestEnsureTCPPropagatesSeedOwnerToFb checks that when seed's owner is
// known (via AddSeedFor — see the gossip loop in control.go), the
// TCP-derived fb address inherits that same ownership. Without this, fb
// is added to ns.seeds unowned and can never be pruned by install()'s
// stale-seed cleanup even after the real peer connects via a completely
// different path, leaving it to be retried by initLoop forever.
func TestEnsureTCPPropagatesSeedOwnerToFb(t *testing.T) {
	e, f, ns := tcpEngine(t, 443)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	fb := netip.MustParseAddrPort("203.0.113.7:443")

	ns.mu.Lock()
	ns.seedOwner[seed] = "peer1"
	ns.mu.Unlock()

	e.ensureTCP(ns, seed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	ns.mu.RLock()
	owner := ns.seedOwner[fb]
	ns.mu.RUnlock()
	if owner != "peer1" {
		t.Fatalf("fb should inherit seed's owner: got owner=%q, want peer1", owner)
	}
}

// TestWatchTCPHandshakeWarnsWhenNoSessionForms is the core diagnostic
// this fix adds: DialTCP succeeding only confirms a raw socket
// connected, not that a gravinet peer is on the other end. If no mesh
// session forms over fb within the grace window, a warning should be logged
// — otherwise an address that isn't running gravinet (or fails its handshake
// for any reason) produces a log line that reads as success every time its
// socket reconnects, with nothing distinguishing it from a genuinely healthy
// reconnect.
func TestWatchTCPHandshakeWarnsWhenNoSessionForms(t *testing.T) {
	var buf bytes.Buffer
	log := logx.New(&buf, logx.LevelInfo)
	e := NewEngine(Options{
		NodeID: "self", Log: log,
		Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	ns := e.netSnapshot()[1]
	fb := netip.MustParseAddrPort("203.0.113.7:443")

	e.watchTCPHandshake(ns, fb)

	if !strings.Contains(buf.String(), "no mesh session formed") {
		t.Fatalf("expected a warning about no mesh session forming, got log:\n%s", buf.String())
	}
}

// TestWatchTCPHandshakeSilentWhenSessionForms checks the converse: no
// warning when a session genuinely forms over fb before the grace window
// elapses.
func TestWatchTCPHandshakeSilentWhenSessionForms(t *testing.T) {
	var buf bytes.Buffer
	log := logx.New(&buf, logx.LevelInfo)
	e := NewEngine(Options{
		NodeID: "self", Log: log,
		Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	ns := e.netSnapshot()[1]
	fb := netip.MustParseAddrPort("203.0.113.7:443")

	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", endpoint: fb}
	ns.mu.Unlock()

	e.watchTCPHandshake(ns, fb)

	if strings.Contains(buf.String(), "no mesh session formed") {
		t.Fatalf("should not warn when a session already exists at fb, got log:\n%s", buf.String())
	}
}
