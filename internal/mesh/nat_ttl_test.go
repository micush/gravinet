package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// The configurable state timeout governs how long a tracked NAT mapping lives.
func TestNATStateTimeout(t *testing.T) {
	a := netip.MustParseAddr
	mk := func(ttl time.Duration) *natTable {
		nt := newNATTable([]natRule{{action: snatAction, src: netip.MustParsePrefix("192.168.1.0/24"), to: a("10.0.0.1")}}, ttl)
		nt.translateOut(makeUDP(a("192.168.1.5"), a("10.0.0.2"), 1000, 53, []byte("q")))
		return nt
	}
	conns := func(nt *natTable) int {
		nt.mu.Lock()
		defer nt.mu.Unlock()
		return len(nt.snatFwd)
	}

	// Short TTL: a sweep past the idle window reclaims the mapping.
	short := mk(1 * time.Second)
	if conns(short) != 1 {
		t.Fatal("expected a tracked connection after translateOut")
	}
	short.sweep(time.Now().Add(2 * time.Second))
	if conns(short) != 0 {
		t.Error("short TTL: mapping should have been reclaimed")
	}

	// ttl=0 falls back to the default (120s): same 2s sweep keeps it.
	def := mk(0)
	def.sweep(time.Now().Add(2 * time.Second))
	if conns(def) != 1 {
		t.Error("default TTL: mapping should still be alive after 2s")
	}
}
