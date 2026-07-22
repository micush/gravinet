package mesh

import (
	"net/netip"
	"testing"
)

// FuzzUDPPorts is a correctness/safety fuzz target for udpPorts, not a
// behavioral one: the function's contract on arbitrary bytes is "never
// panic", since it parses attacker-controlled packet bytes off the wire
// before any authentication has happened (isUnderlayLoop runs on outbound
// TUN reads, pre-encryption). A malformed extension-header chain — a
// claimed length that runs past the buffer, an unrecognised next-header
// value, a header that chains into itself — must resolve to ok=false, never
// a panic. The corpus seeds from the shapes TestUDPPorts already covers as
// known-good/known-bad, since a fuzzer converges faster from real packet
// structure than from pure random bytes.
//
// This does not separately test for hangs: the extension-header walk in
// udpPorts is provably bounded independent of input — every branch that
// advances `off` does so by at least 8 bytes (the smallest possible header),
// and the loop itself is additionally capped at maxIPv6ExtHeaders — so no
// input can make it loop unboundedly. That's a static property of the code,
// not something worth spending fuzz budget re-discovering per input.
func FuzzUDPPorts(f *testing.F) {
	a4 := netip.MustParseAddr("10.0.0.1")
	b4 := netip.MustParseAddr("10.0.0.2")
	a6 := netip.MustParseAddr("fd00::1")
	b6 := netip.MustParseAddr("fd00::2")
	f.Add(buildUDP4(a4, b4, 1, 2, 4))
	f.Add(buildUDP6(a6, b6, 1, 2, 4))
	f.Add(buildUDP6Ext(a6, b6, 1, 2, 4, 0, extHdr6(17, 0)))
	f.Add(buildUDP6Ext(a6, b6, 1, 2, 4, 44, fragHdr(17, 8)))
	f.Add([]byte{})
	f.Add([]byte{0x60})
	f.Add(make([]byte, 40)) // all-zero v6 header: next=0, hop-by-hop chained into itself
	self := make([]byte, 0, 300)
	self = append(self, buildUDP6(a6, b6, 1, 2, 4)[:40]...)
	self[6] = 0
	for i := 0; i < 30; i++ {
		self = append(self, 0, 0, 0, 0, 0, 0, 0, 0) // next=0, len=0: chains into another hop-by-hop
	}
	f.Add(self)
	trunc := buildUDP6Ext(a6, b6, 1, 2, 4, 0, extHdr6(17, 0))
	trunc[41] = 255 // maximal claimed extension length, well past the buffer
	f.Add(trunc)
	f.Fuzz(func(t *testing.T, p []byte) {
		udpPorts(p) // must not panic; return value isn't asserted here
	})
}
