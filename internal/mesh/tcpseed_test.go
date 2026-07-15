package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// primeTCPSeeds must dial each explicit TCP seed over the fallback and, once up,
// register it as a seed so the handshake loop runs over the TLS connection.
func TestPrimeTCPSeedsDialsAndRegisters(t *testing.T) {
	e, f, ns := fallbackEngine(t, 65432)
	seed := netip.MustParseAddrPort("198.51.100.9:8443")
	ns.mu.Lock()
	ns.tcpSeeds = []netip.AddrPort{seed}
	ns.mu.Unlock()

	e.primeTCPSeeds(ns)

	// Wait for the off-loop dial + registration.
	deadline := time.Now().Add(2 * time.Second)
	registered := func() bool {
		ns.mu.RLock()
		defer ns.mu.RUnlock()
		for _, s := range ns.seeds {
			if s == seed {
				return true
			}
		}
		return false
	}
	for time.Now().Before(deadline) && !registered() {
		time.Sleep(10 * time.Millisecond)
	}
	if d := f.dials(); len(d) != 1 || d[0] != seed {
		t.Fatalf("expected one dial to %s, got %v", seed, d)
	}
	if !registered() {
		t.Fatal("tcp seed not registered as a seed after dial")
	}

	// Idempotent: already connected → no redial.
	e.primeTCPSeeds(ns)
	time.Sleep(50 * time.Millisecond)
	if d := f.dials(); len(d) != 1 {
		t.Fatalf("redialed an already-connected tcp seed: %v", d)
	}
}

func TestAddTCPSeedDedupes(t *testing.T) {
	e, _, ns := fallbackEngine(t, 65432)
	seed := netip.MustParseAddrPort("198.51.100.20:9000")
	e.addTCPSeed(ns.spec.ID, seed)
	e.addTCPSeed(ns.spec.ID, seed)
	ns.mu.RLock()
	n := 0
	for _, s := range ns.tcpSeeds {
		if s == seed {
			n++
		}
	}
	ns.mu.RUnlock()
	if n != 1 {
		t.Fatalf("addTCPSeed did not dedupe: %d copies", n)
	}
}
