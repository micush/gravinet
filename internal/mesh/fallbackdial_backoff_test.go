package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes. The dial itself
// runs off the init loop in its own goroutine (see dialFallbackCandidate), so
// asserting on f.dials() immediately after the call would race; the existing
// fallback tests spell this loop out inline each time.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// watchFallbackHandshake has recorded an escalating backoff against a fallback
// address that connects but never handshakes since v713 — and logged "not
// retrying for <wait>" while doing it. Nothing on the dial path ever read it
// back: fallbackInBackoff's only caller was primeTCPSeeds' explicit-TCP-seed
// loop, keyed on the seed, while the address being penalized is fb, the
// derived candidate port, which for any advertised extra port isn't the seed
// at all. So the ladder climbed 30s → 10m purely for the log's benefit while
// fallbackDialCooldown's flat 30s stayed the real pace, and the warning
// overstated its own suppression window by more than an order of magnitude at
// the top of the ladder. Observed in the field as 16,549 of these warnings
// across a twelve-node mesh in three hours, on every node, each with a fresh
// socket and TLS handshake behind it. See v780.

// TestDialFallbackCandidateHonorsBackoff: an fb still cooling down is not
// redialled.
func TestDialFallbackCandidateHonorsBackoff(t *testing.T) {
	e, f, ns := fallbackEngine(t, 443)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	fb := netip.MustParseAddrPort("203.0.113.7:443")

	// The first dial connects, the handshake never lands, and
	// watchFallbackHandshake penalizes fb — reproduced directly rather than
	// waiting out the grace timer.
	ns.noteFallbackFailure(fb)
	if !ns.fallbackInBackoff(fb) {
		t.Fatal("fb should be in backoff after a recorded failure")
	}

	e.dialFallbackCandidate(ns, f, seed, 443)

	if got := f.dials(); len(got) != 0 {
		t.Fatalf("dialed %v while fb was in backoff; the recorded cooldown is being ignored", got)
	}
	// The claim must be released, not leaked — a stuck ns.dialing entry would
	// suppress the dial permanently rather than for the backoff window.
	ns.mu.RLock()
	stuck := ns.dialing[fb]
	ns.mu.RUnlock()
	if stuck {
		t.Fatal("dialFallbackCandidate left fb claimed after declining to dial")
	}
}

// TestDialFallbackCandidateDialsOnceBackoffCleared: the suppression is a
// cooldown, not a permanent ban. A session forming clears it
// (watchFallbackHandshake's success path) and the address becomes dialable
// again.
func TestDialFallbackCandidateDialsOnceBackoffCleared(t *testing.T) {
	e, f, ns := fallbackEngine(t, 443)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	fb := netip.MustParseAddrPort("203.0.113.7:443")

	ns.noteFallbackFailure(fb)
	e.dialFallbackCandidate(ns, f, seed, 443)
	if got := f.dials(); len(got) != 0 {
		t.Fatalf("dialed %v while in backoff", got)
	}

	ns.clearFallbackBackoff(fb)
	e.dialFallbackCandidate(ns, f, seed, 443)

	waitFor(t, func() bool { return len(f.dials()) == 1 }, "fb not dialed after its backoff was cleared")
}

// TestDialFallbackCandidateBackoffIsPerAddress: penalizing one candidate port
// must not suppress the others. The whole point of a multi-port candidate list
// is that one port getting through when another doesn't is the expected case.
func TestDialFallbackCandidateBackoffIsPerAddress(t *testing.T) {
	e, f, ns := fallbackEngine(t, 443)
	seed := netip.MustParseAddrPort("203.0.113.7:65432")
	dead := netip.MustParseAddrPort("203.0.113.7:443")

	ns.noteFallbackFailure(dead)
	e.dialFallbackCandidate(ns, f, seed, 23)

	waitFor(t, func() bool {
		for _, d := range f.dials() {
			if d == netip.MustParseAddrPort("203.0.113.7:23") {
				return true
			}
		}
		return false
	}, "an unpenalized candidate port was suppressed by a different port's backoff")
}
