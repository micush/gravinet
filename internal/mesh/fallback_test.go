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

// fallbackHandshakeGrace defaults to 10s in production; shortened once here
// for the whole test binary so watchFallbackHandshake's background goroutines
// (spawned by every successful ensureFallback call, including in tests that
// aren't specifically testing this) resolve quickly rather than lingering for
// the real 10s past their own test's return — which would otherwise race
// against any later test's use of the same package-level var.
func init() {
	fallbackHandshakeGrace = 50 * time.Millisecond
}

// fakeFallback is a Sender that also implements fallbackDialer, recording dials.
type fakeFallback struct {
	mu     sync.Mutex
	dialed []netip.AddrPort
	has    map[netip.AddrPort]bool
}

func (f *fakeFallback) Send(netip.AddrPort, []byte) error { return nil }

func (f *fakeFallback) DialFallback(to netip.AddrPort) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialed = append(f.dialed, to)
	if f.has == nil {
		f.has = map[netip.AddrPort]bool{}
	}
	f.has[to] = true
	return nil
}

func (f *fakeFallback) HasFallback(to netip.AddrPort) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.has[to]
}

func (f *fakeFallback) dials() []netip.AddrPort {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netip.AddrPort(nil), f.dialed...)
}

func fallbackEngine(t *testing.T, port int) (*Engine, *fakeFallback, *netState) {
	t.Helper()
	e := NewEngine(Options{
		NodeID:          "self",
		TCPFallbackPort: port,
		Nets:            []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	f := &fakeFallback{has: map[netip.AddrPort]bool{}}
	e.Attach(f)
	ns := e.netSnapshot()[1]
	if ns == nil {
		t.Fatal("network not created")
	}
	return e, f, ns
}

// TestEnsureFallbackDialsAndSeeds: when UDP to a seed is failing, the engine
// dials the peer's :443 fallback and registers it as a seed so the next init
// tick hands the handshake to the TLS path.
func TestEnsureFallbackDialsAndSeeds(t *testing.T) {
	e, f, ns := fallbackEngine(t, 443)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	fb := netip.MustParseAddrPort("203.0.113.7:443")

	e.ensureFallback(ns, seed)

	// The dial runs off the init loop; wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if d := f.dials(); len(d) != 1 || d[0] != fb {
		t.Fatalf("expected one dial to %s, got %v", fb, d)
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
		t.Fatalf("fallback endpoint %s not added as seed; seeds=%v", fb, seeds)
	}

	// Idempotent: with a fallback conn already up (HasFallback true), no re-dial.
	e.ensureFallback(ns, seed)
	time.Sleep(100 * time.Millisecond)
	if d := f.dials(); len(d) != 1 {
		t.Fatalf("expected no second dial, got %d", len(d))
	}
}

// TestEnsureFallbackSamePortDials: when the fallback port equals the seed's port
// (the default, both 65432), the engine still dials the TLS fallback at that
// endpoint — and does not add a duplicate seed, since fb == seed.
func TestEnsureFallbackSamePortDials(t *testing.T) {
	e, f, ns := fallbackEngine(t, 65432)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")

	ns.mu.RLock()
	seedsBefore := len(ns.seeds)
	ns.mu.RUnlock()

	e.ensureFallback(ns, seed)
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
		t.Fatalf("same-port fallback should not add a seed: before=%d after=%d", seedsBefore, seedsAfter)
	}
}

// TestEnsureFallbackDisabled: with the fallback port at 0, no dialing happens.
func TestEnsureFallbackDisabled(t *testing.T) {
	e, f, ns := fallbackEngine(t, 0)
	e.ensureFallback(ns, netip.MustParseAddrPort("203.0.113.7:65432"))
	time.Sleep(100 * time.Millisecond)
	if d := f.dials(); len(d) != 0 {
		t.Fatalf("expected no dial when fallback disabled, got %v", d)
	}
}

// TestEnsureFallbackNoDialerIsNoop: a UDP-only transport (no fallbackDialer)
// must be tolerated without panic.
func TestEnsureFallbackNoDialerIsNoop(t *testing.T) {
	e := NewEngine(Options{
		NodeID:          "self",
		TCPFallbackPort: 443,
		Nets:            []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	e.Attach(nopSender{}) // implements Sender but not fallbackDialer
	ns := e.netSnapshot()[1]
	e.ensureFallback(ns, netip.MustParseAddrPort("203.0.113.7:65432")) // must not panic
}

// slowFallback wraps fakeFallback with an artificial delay before
// DialFallback completes, opening a window during which concurrent callers
// could race past the already-connected/already-has-fallback checks if
// nothing coalesces them.
type slowFallback struct {
	*fakeFallback
	delay time.Duration
}

func (f *slowFallback) DialFallback(to netip.AddrPort) error {
	time.Sleep(f.delay)
	return f.fakeFallback.DialFallback(to)
}

// TestEnsureFallbackCoalescesConcurrentDials reproduces the real-world
// failure mode directly: a peer whose seed list has accumulated many stale
// duplicate entries (same IP, different historically-observed ports — see
// AddSeed's exact-match-only dedup) all resolve to the same fallback address,
// and initLoop fires ensureFallback for every one of them in a single
// synchronous pass while each dial runs asynchronously. Without coalescing,
// many of these race past the checks and each independently dial, producing
// a burst of duplicate "established tcp fallback" log lines within the same
// tick — exactly the pattern seen in production logs. With the fix, only one
// dial should ever be in flight for a given fallback address at a time.
func TestEnsureFallbackCoalescesConcurrentDials(t *testing.T) {
	inner := &fakeFallback{has: map[netip.AddrPort]bool{}}
	f := &slowFallback{fakeFallback: inner, delay: 100 * time.Millisecond}
	e := NewEngine(Options{
		NodeID:          "self",
		TCPFallbackPort: 443,
		Nets:            []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	e.Attach(f)
	ns := e.netSnapshot()[1]

	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.ensureFallback(ns, seed)
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.dials()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // let any (wrongly) racing extra dials land
	if d := f.dials(); len(d) != 1 {
		t.Fatalf("expected exactly 1 dial despite 30 concurrent callers for the same fallback address, got %d: %v", len(d), d)
	}
}

// TestEnsureFallbackPropagatesSeedOwnerToFb checks that when seed's owner is
// known (via AddSeedFor — see the gossip loop in control.go), the
// fallback-derived fb address inherits that same ownership. Without this, fb
// is added to ns.seeds unowned and can never be pruned by install()'s
// stale-seed cleanup even after the real peer connects via a completely
// different path, leaving it to be retried by initLoop forever.
func TestEnsureFallbackPropagatesSeedOwnerToFb(t *testing.T) {
	e, f, ns := fallbackEngine(t, 443)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	fb := netip.MustParseAddrPort("203.0.113.7:443")

	ns.mu.Lock()
	ns.seedOwner[seed] = "peer1"
	ns.mu.Unlock()

	e.ensureFallback(ns, seed)
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

// TestWatchFallbackHandshakeWarnsWhenNoSessionForms is the core diagnostic
// this fix adds: DialFallback succeeding only confirms a raw socket
// connected, not that a gravinet peer is on the other end. If no mesh
// session forms over fb within the grace window, a warning should be logged
// — otherwise an address that isn't running gravinet (or fails its handshake
// for any reason) produces a log line that reads as success every time its
// socket reconnects, with nothing distinguishing it from a genuinely healthy
// reconnect.
func TestWatchFallbackHandshakeWarnsWhenNoSessionForms(t *testing.T) {
	var buf bytes.Buffer
	log := logx.New(&buf, logx.LevelInfo)
	e := NewEngine(Options{
		NodeID: "self", Log: log,
		Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	ns := e.netSnapshot()[1]
	fb := netip.MustParseAddrPort("203.0.113.7:443")

	e.watchFallbackHandshake(ns, fb)

	if !strings.Contains(buf.String(), "no mesh session formed") {
		t.Fatalf("expected a warning about no mesh session forming, got log:\n%s", buf.String())
	}
}

// TestWatchFallbackHandshakeSilentWhenSessionForms checks the converse: no
// warning when a session genuinely forms over fb before the grace window
// elapses.
func TestWatchFallbackHandshakeSilentWhenSessionForms(t *testing.T) {
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

	e.watchFallbackHandshake(ns, fb)

	if strings.Contains(buf.String(), "no mesh session formed") {
		t.Fatalf("should not warn when a session already exists at fb, got log:\n%s", buf.String())
	}
}
