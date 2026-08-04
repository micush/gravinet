package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// DialTCP succeeding only means the socket connected. When the address
// isn't running gravinet — or is a gateway that accepts connections to
// anything, which is what made this visible — HasTCP then reports it as
// connected, so nothing suppresses it and it is redialled on every tick
// forever, each attempt burning a 10s handshake grace.
//
// The failure is standing, not transient, so it earns a cooldown.

func TestFallbackBackoffDoublesAndCaps(t *testing.T) {
	ns := &netState{cands: newCandStore()}
	fb := netip.MustParseAddrPort("192.168.5.104:65432")

	if got := ns.noteFallbackFailure(fb); got != candBackoffMin {
		t.Fatalf("first failure waits %s, want %s", got, candBackoffMin)
	}
	if !ns.fallbackInBackoff(fb) {
		t.Fatal("address is not in backoff immediately after a failure")
	}
	prev := candBackoffMin
	for i := 0; i < 20; i++ {
		got := ns.noteFallbackFailure(fb)
		if got < prev {
			t.Fatalf("backoff went backwards: %s after %s", got, prev)
		}
		if got > candBackoffMax {
			t.Fatalf("backoff %s exceeded the cap %s", got, candBackoffMax)
		}
		prev = got
	}
	if prev != candBackoffMax {
		t.Fatalf("backoff settled at %s, want the cap %s", prev, candBackoffMax)
	}
}

// A transient failure must cost one extra dial, not a permanent penalty: the
// moment a session forms the address is clean again.
func TestFallbackBackoffClearsOnSuccess(t *testing.T) {
	ns := &netState{cands: newCandStore()}
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
	if got := ns.noteFallbackFailure(fb); got != candBackoffMin {
		t.Fatalf("after clearing, the next failure waits %s, want to start over at %s", got, candBackoffMin)
	}
}

// Backoff is per address: one bad candidate must not silence a different one.
func TestFallbackBackoffIsPerAddress(t *testing.T) {
	ns := &netState{cands: newCandStore()}
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
	ns := &netState{cands: newCandStore()}
	fb := netip.MustParseAddrPort("192.168.5.104:65432")
	wait := ns.noteFallbackFailure(fb)
	if !ns.fallbackInBackoff(fb) {
		t.Fatal("a just-failed address should be cooling down")
	}
	// Read back through the same store the writer used. That the entry
	// written and the entry read are the same entry is the whole point of
	// consolidating these — v780 was exactly that split.
	if ns.cands.Due(tcpCandKey(fb), time.Now().Add(wait+time.Second)) == false {
		t.Fatal("backoff past its deadline must not still suppress the address")
	}
}
