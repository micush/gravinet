package mesh

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/protocol"
)

// splitSizes runs the real fragPlan and expands its decision into the actual
// per-fragment payload sizes, mirroring sendFragmented's loop. It deliberately
// calls the production function rather than reimplementing the arithmetic, so
// these invariants cannot drift away from what the hot path really does.
func splitSizes(length, per int) []int {
	count, size := fragPlan(length, per)
	var out []int
	for i := 0; i < count; i++ {
		start := i * size
		end := start + size
		if end > length {
			end = length
		}
		if start >= length {
			out = append(out, 0) // an empty fragment: the receiver rejects these
			continue
		}
		out = append(out, end-start)
	}
	return out
}

// The three properties the balanced split has to hold for every plausible
// combination of overlay packet size and path MTU, checked exhaustively rather
// than at a couple of hand-picked points: nothing exceeds what the path can
// carry, nothing is empty, and the pieces still add up to the whole packet.
func TestFragmentSplitInvariants(t *testing.T) {
	for length := 1; length <= 9300; length++ {
		for _, per := range []int{1, 2, 37, 300, 1195, 1317, 1435, 4608, 8915, 9103, 9131} {
			sizes := splitSizes(length, per)
			if len(sizes) == 0 {
				t.Fatalf("length=%d per=%d produced no fragments", length, per)
			}
			sum := 0
			for i, s := range sizes {
				if s <= 0 {
					t.Fatalf("length=%d per=%d: fragment %d is empty (%v)", length, per, i, sizes)
				}
				if s > per {
					t.Fatalf("length=%d per=%d: fragment %d is %d bytes, over the path limit", length, per, i, s)
				}
				sum += s
			}
			if sum != length {
				t.Fatalf("length=%d per=%d: fragments sum to %d (%v)", length, per, sum, sizes)
			}
			// Balancing must not cost an extra datagram: the count has to match
			// what the greedy split at the path limit would have produced.
			if want := (length + per - 1) / per; len(sizes) != want {
				t.Fatalf("length=%d per=%d: %d fragments, greedy would use %d", length, per, len(sizes), want)
			}
		}
	}
}

// The property the whole change exists for: fragments are near-uniform, so the
// GSO send path can coalesce them. gsoRunLen takes the first datagram's length
// as the stride and stops at anything *larger*, tolerating one shorter piece at
// the end — so what matters is that every fragment after the first is <= the
// first, and that the spread is small enough to be worth coalescing at all.
func TestFragmentSplitIsUniformEnoughForGSO(t *testing.T) {
	cases := []struct {
		name        string
		length, per int
	}{
		{"9216 overlay on a 9000 path", 9216, 8915},
		{"9216 overlay on a 9216 path", 9216, 9131},
		{"9216 overlay on a 1520 path", 9216, 1435},
		{"9216 overlay on a 1280 path", 9216, 1195},
		{"1500 overlay on a 1280 path", 1500, 1195},
	}
	for _, c := range cases {
		sizes := splitSizes(c.length, c.per)
		first := sizes[0]
		for i, s := range sizes[1:] {
			if s > first {
				t.Fatalf("%s: fragment %d (%d B) is larger than the first (%d B) — breaks the GSO stride", c.name, i+1, s, first)
			}
		}
		smallest := sizes[len(sizes)-1]
		// The old greedy split produced a final runt (9216/8915 gave 301 bytes,
		// 3% of the first fragment). Balanced, the last piece must be within a
		// hair of the others; anything under half is the pathology returning.
		if smallest*2 < first {
			t.Fatalf("%s: last fragment %d B is a runt next to %d B (%v)", c.name, smallest, first, sizes)
		}
	}
}

// The specific regression from the field: a 9216-byte overlay packet on a
// 9000-byte path used to go out as 8915 + 301. It must now be two even halves.
func TestNineKOverlayOnNineKPathSplitsEvenly(t *testing.T) {
	per := computeMaxInnerFrag(9000)
	if per != 8915 {
		t.Fatalf("computeMaxInnerFrag(9000)=%d, want 8915 — overhead accounting changed, revisit this test", per)
	}
	sizes := splitSizes(9216, per)
	if len(sizes) != 2 {
		t.Fatalf("got %d fragments, want 2: %v", len(sizes), sizes)
	}
	if sizes[0] != 4608 || sizes[1] != 4608 {
		t.Fatalf("got %v, want two 4608-byte halves", sizes)
	}
}

// The invariant that was violated for the whole life of the project before
// v659, and the reason this file exists at all: the default overlay MTU must
// fit inside ONE underlay datagram at the default path-MTU-discovery ceiling.
//
// These are three numbers owned by three different packages — config's default
// underlay_mtu_max, mesh's per-fragment overhead accounting, and protocol's
// default tunnel MTU — and nothing connected them, so 9216 sat above a ceiling
// of 9000 indefinitely with the only symptom being a fragment counter nobody
// had reason to read. Asserting the relationship rather than the literal means
// changing any one of the three fails here instead of silently reintroducing
// fragmentation on every packet.
func TestDefaultTunnelMTUFitsDefaultUnderlay(t *testing.T) {
	var c config.Config // zero value: every default in force
	ceiling := c.UnderlayMTUMaxValue()
	if ceiling != 9000 {
		t.Fatalf("default underlay_mtu_max is %d, expected 9000 — if this moved deliberately, DefaultTunnelMTU must move with it", ceiling)
	}
	fits := computeMaxInnerFrag(ceiling)
	if protocol.DefaultTunnelMTU > fits {
		t.Fatalf("default tunnel MTU %d exceeds what a %d-byte underlay carries unfragmented (%d): every full-size packet would be split",
			protocol.DefaultTunnelMTU, ceiling, fits)
	}
	// Also assert it isn't left needlessly small: an overlay MTU below the
	// limit is throughput given away for nothing.
	if protocol.DefaultTunnelMTU != fits {
		t.Fatalf("default tunnel MTU %d wastes %d bytes of every packet; the limit at a %d-byte underlay is %d",
			protocol.DefaultTunnelMTU, fits-protocol.DefaultTunnelMTU, ceiling, fits)
	}
	// And the payoff: a full-size default packet must need exactly one piece.
	if count, _ := fragPlan(protocol.DefaultTunnelMTU, fits); count != 1 {
		t.Fatalf("a full-size default overlay packet still needs %d fragments", count)
	}
}

// Lowering the default must not have made anything worse on smaller underlays.
// ceil is monotonic in the numerator, so a smaller overlay MTU can only reduce
// a packet's fragment count — checked here across the path MTUs a real mesh
// actually sees rather than argued from the formula alone.
func TestLowerDefaultNeverIncreasesFragmentCount(t *testing.T) {
	const previousDefault = 9216
	for _, underlay := range []int{590, 1280, 1500, 1520, 4000, 9000, 9216} {
		per := computeMaxInnerFrag(underlay)
		was, _ := fragPlan(previousDefault, per)
		now, _ := fragPlan(protocol.DefaultTunnelMTU, per)
		if now > was {
			t.Fatalf("underlay %d: new default needs %d fragments where the old one needed %d", underlay, now, was)
		}
		t.Logf("underlay %d (per=%d): %d fragments -> %d", underlay, per, was, now)
	}
}

// A packet that already fits must not be touched, and one that needs exactly
// one fragment must not be rebalanced into something smaller than it needs.
func TestFragmentSplitLeavesSinglePieceAlone(t *testing.T) {
	for _, length := range []int{1, 100, 8914, 8915} {
		sizes := splitSizes(length, 8915)
		if len(sizes) != 1 || sizes[0] != length {
			t.Fatalf("length=%d: got %v, want a single %d-byte piece", length, sizes, length)
		}
	}
}
