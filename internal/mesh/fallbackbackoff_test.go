package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// DialFallback succeeding only means the socket connected. When the address
// isn't running gravinet — or is a gateway that accepts connections to
// anything, which is what made this visible — HasFallback then reports it as
// connected, so nothing suppresses it and it is redialled on every tick
// forever, each attempt burning a 10s handshake grace.
//
// The failure is standing, not transient, so it earns a cooldown.

func TestFallbackBackoffDoublesAndCaps(t *testing.T) {
	ns := &netState{}
	fb := netip.MustParseAddrPort("192.168.5.104:65432")

	if got := ns.noteFallbackFailure(fb); got != fallbackBackoffMin {
		t.Fatalf("first failure waits %s, want %s", got, fallbackBackoffMin)
	}
	if !ns.fallbackInBackoff(fb) {
		t.Fatal("address is not in backoff immediately after a failure")
	}
	prev := fallbackBackoffMin
	for i := 0; i < 20; i++ {
		got := ns.noteFallbackFailure(fb)
		if got < prev {
			t.Fatalf("backoff went backwards: %s after %s", got, prev)
		}
		if got > fallbackBackoffMax {
			t.Fatalf("backoff %s exceeded the cap %s", got, fallbackBackoffMax)
		}
		prev = got
	}
	if prev != fallbackBackoffMax {
		t.Fatalf("backoff settled at %s, want the cap %s", prev, fallbackBackoffMax)
	}
}

// A transient failure must cost one extra dial, not a permanent penalty: the
// moment a session forms the address is clean again.
func TestFallbackBackoffClearsOnSuccess(t *testing.T) {
	ns := &netState{}
	fb := netip.MustParseAddrPort("192.168.5.104:65432")
	ns.noteFallbackFailure(fb)
	ns.noteFallbackFailure(fb)
	if !ns.fallbackInBackoff(fb) {
		t.Fatal("expected backoff after two failures")
	}
	ns.clearFallbackBackoff(fb)
	if ns.fallbackInBackoff(fb) {
		t.Fatal("a completed handshake must clear the cooldown entirely, so a peer that recovers is dialled at full rate again")
	}
	if got := ns.noteFallbackFailure(fb); got != fallbackBackoffMin {
		t.Fatalf("after clearing, the next failure waits %s, want to start over at %s", got, fallbackBackoffMin)
	}
}

// Backoff is per address: one bad candidate must not silence a different one.
func TestFallbackBackoffIsPerAddress(t *testing.T) {
	ns := &netState{}
	bad := netip.MustParseAddrPort("192.168.5.104:65432")
	good := netip.MustParseAddrPort("192.168.5.108:65432")
	ns.noteFallbackFailure(bad)
	if !ns.fallbackInBackoff(bad) {
		t.Fatal("bad address should be cooling down")
	}
	if ns.fallbackInBackoff(good) {
		t.Fatal("an unrelated address was suppressed by another's failure")
	}
}

// And it must expire, or a temporarily-down peer is never retried.
func TestFallbackBackoffExpires(t *testing.T) {
	ns := &netState{}
	fb := netip.MustParseAddrPort("192.168.5.104:65432")
	ns.noteFallbackFailure(fb)
	ns.mu.Lock()
	e := ns.fallbackBackoff[fb]
	e.until = time.Now().Add(-time.Second)
	ns.fallbackBackoff[fb] = e
	ns.mu.Unlock()
	if ns.fallbackInBackoff(fb) {
		t.Fatal("backoff past its deadline must not still suppress the address")
	}
}
