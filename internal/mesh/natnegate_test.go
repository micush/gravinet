package mesh

import (
	"net/netip"
	"testing"
)

// Negation inverts which packets a rule claims, and the three shapes an
// operator asked for are the three asserted here. The third — source not A
// AND dest not B — is the one that motivated the feature: it cannot be
// expressed as any number of positive rules, because rules OR together and
// there is no no-NAT action to carve an exclusion with.
func TestNATNegationMatching(t *testing.T) {
	inA := netip.MustParseAddr("10.1.1.5")  // inside 10.1.1.0/24
	outA := netip.MustParseAddr("10.2.2.5") // outside it
	inB := netip.MustParseAddr("10.3.3.7")  // inside 10.3.3.0/24
	outB := netip.MustParseAddr("10.4.4.7") // outside it

	cases := []struct {
		name           string
		src, dst       string
		srcNeg, dstNeg bool
		want           map[[2]netip.Addr]bool // src,dst -> should the rule match
	}{{
		name: "source not A", src: "10.1.1.0/24", srcNeg: true,
		want: map[[2]netip.Addr]bool{
			{inA, inB}:  false, // source inside the negated prefix: excluded
			{outA, inB}: true,
		},
	}, {
		name: "dest not B", dst: "10.3.3.0/24", dstNeg: true,
		want: map[[2]netip.Addr]bool{
			{inA, inB}:  false,
			{inA, outB}: true,
		},
	}, {
		name: "source not A and dest not B",
		src:  "10.1.1.0/24", srcNeg: true, dst: "10.3.3.0/24", dstNeg: true,
		want: map[[2]netip.Addr]bool{
			{inA, inB}:   false, // both inside: excluded
			{inA, outB}:  false, // source inside: still excluded (AND, not OR)
			{outA, inB}:  false, // dest inside: still excluded
			{outA, outB}: true,  // outside both: the only match
		},
	}, {
		// The un-negated rule, so the table above is read against a control
		// rather than against an assumption.
		name: "positive source", src: "10.1.1.0/24",
		want: map[[2]netip.Addr]bool{
			{inA, inB}:  true,
			{outA, inB}: false,
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := NATRuleSpec{
				Source: tc.src, Dest: tc.dst,
				SourceNegate: tc.srcNeg, DestNegate: tc.dstNeg,
				Translate: "192.0.2.1",
			}
			r, ok := spec.toRule()
			if !ok {
				t.Fatalf("spec did not build: %+v", spec)
			}
			for pair, want := range tc.want {
				got := prefixMatchNeg(r.src, pair[0], r.srcNeg) &&
					prefixMatchNeg(r.dst, pair[1], r.dstNeg)
				if got != want {
					t.Errorf("src=%v dst=%v: matched=%v, want %v", pair[0], pair[1], got, want)
				}
			}
		})
	}
}

// A blank prefix means "any" and stays true whether or not negation is set.
// Config refuses to save that pairing, and this pins the data plane's own
// behavior so a stale config can't turn into a rule that silently never
// fires — the failure mode negation is most likely to produce.
func TestPrefixMatchNegBlankIsAlwaysAny(t *testing.T) {
	var blank netip.Prefix
	a := netip.MustParseAddr("10.1.1.5")
	if !prefixMatchNeg(blank, a, false) {
		t.Error("blank prefix must match anything")
	}
	if !prefixMatchNeg(blank, a, true) {
		t.Error("a negated blank prefix must still match anything, not nothing")
	}
}

// Negation is a match-side concept: it must not leak into the rule's address
// family, which still comes from the prefixes and translation target.
func TestNATNegationDoesNotAffectFamily(t *testing.T) {
	for _, neg := range []bool{false, true} {
		r, ok := NATRuleSpec{Source: "fd00:203::/64", SourceNegate: neg, Translate: "fd00:203::1"}.toRule()
		if !ok {
			t.Fatalf("neg=%v: spec did not build", neg)
		}
		if !r.is6 {
			t.Errorf("neg=%v: a v6 source must still yield a v6 rule", neg)
		}
		if r.srcNeg != neg {
			t.Errorf("neg=%v: srcNeg not carried", neg)
		}
	}
}
