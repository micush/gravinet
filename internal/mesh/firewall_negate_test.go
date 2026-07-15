package mesh

import (
	"net/netip"
	"testing"
)

func TestFirewallSrcNegate(t *testing.T) {
	a := netip.MustParseAddr
	// Deny everything EXCEPT traffic sourced from 10.5.0.0/16.
	r, err := FirewallRule{Action: "deny", Src: "10.5.0.0/16", SrcNegate: true}.toRule()
	if err != nil {
		t.Fatal(err)
	}
	f := newFirewall([]*fwRule{r})

	if !f.allow(fwOut, makeL4Addr(a("10.5.1.1"), a("8.8.8.8"), 6, 1)) {
		t.Fatal("traffic sourced from within 10.5.0.0/16 should be allowed (negated src excludes it)")
	}
	if f.allow(fwOut, makeL4Addr(a("10.6.1.1"), a("8.8.8.8"), 6, 1)) {
		t.Fatal("traffic sourced from outside 10.5.0.0/16 should be denied (matches the negated src)")
	}
}

func TestFirewallDstNegate(t *testing.T) {
	a := netip.MustParseAddr
	// Deny everything EXCEPT traffic destined to 10.5.0.0/16.
	r, err := FirewallRule{Action: "deny", Dst: "10.5.0.0/16", DstNegate: true}.toRule()
	if err != nil {
		t.Fatal(err)
	}
	f := newFirewall([]*fwRule{r})

	if !f.allow(fwOut, makeL4Addr(a("1.1.1.1"), a("10.5.9.9"), 6, 1)) {
		t.Fatal("traffic to 10.5.0.0/16 should be allowed (negated dst excludes it)")
	}
	if f.allow(fwOut, makeL4Addr(a("1.1.1.1"), a("10.6.9.9"), 6, 1)) {
		t.Fatal("traffic outside 10.5.0.0/16 should be denied (matches the negated dst)")
	}
}

func TestFirewallServicesNegate(t *testing.T) {
	// Deny every service EXCEPT tcp/80.
	r, err := FirewallRule{Action: "deny", Proto: "tcp", DstPortMin: 80, DstPortMax: 80, ServicesNegate: true}.toRule()
	if err != nil {
		t.Fatal(err)
	}
	f := newFirewall([]*fwRule{r})

	if !f.allow(fwOut, makeL4(6, 5000, 80, 0)) {
		t.Fatal("tcp/80 should be allowed (negated service excludes it)")
	}
	if f.allow(fwOut, makeL4(6, 5000, 443, 0)) {
		t.Fatal("tcp/443 should be denied (matches the negated service)")
	}
	if f.allow(fwOut, makeL4(17, 5000, 80, 0)) {
		t.Fatal("udp/80 should be denied (proto doesn't match the un-negated leg, so it's outside the excluded set)")
	}
}

// TestFirewallNegateAnyMatchesNothing confirms the documented edge-case
// behavior: negating an "any"/empty dimension logically means "the
// universal set, negated" — the empty set — so that dimension then matches
// nothing at all, rather than being special-cased into some other meaning.
func TestFirewallNegateAnyMatchesNothing(t *testing.T) {
	a := netip.MustParseAddr
	r, err := FirewallRule{Action: "deny", Src: "", SrcNegate: true}.toRule() // Src "any", negated
	if err != nil {
		t.Fatal(err)
	}
	f := newFirewall([]*fwRule{r})
	if !f.allow(fwOut, makeL4Addr(a("1.2.3.4"), a("5.6.7.8"), 6, 1)) {
		t.Fatal("negating an 'any' src should match nothing, i.e. traffic should fall through to the default allow")
	}
}

// TestFirewallNegateWithObject confirms negation applies uniformly whether
// the field is a raw CIDR or a named address-object reference — negation is
// applied to whatever the field resolves to, not to its literal text.
func TestFirewallNegateWithObject(t *testing.T) {
	a := netip.MustParseAddr
	f := newFirewall(nil)
	f.setCatalog([]FirewallObject{{Name: "trusted", Kind: "subnet", Addresses: []string{"10.9.0.0/16"}}}, nil)
	f.loadRules([]FirewallRule{{Action: "deny", Dst: "trusted", DstNegate: true}})

	if !f.allow(fwOut, makeL4Addr(a("1.1.1.1"), a("10.9.5.5"), 6, 1)) {
		t.Fatal("traffic to the object's own range should be allowed (negated)")
	}
	if f.allow(fwOut, makeL4Addr(a("1.1.1.1"), a("10.10.5.5"), 6, 1)) {
		t.Fatal("traffic outside the object's range should be denied (matches the negated object)")
	}
}

// TestFirewallRuleNegateRoundTrip mirrors TestFirewallRuleNotesRoundTrip:
// the negate flags must survive toRule -> ruleToExport unchanged, the same
// way Action/Src/Dst/Notes already do, since ruleToExport returns the
// authored spec verbatim.
func TestFirewallRuleNegateRoundTrip(t *testing.T) {
	in := FirewallRule{Action: "deny", Src: "10.0.0.0/8", SrcNegate: true, Dst: "10.1.0.0/16",
		Proto: "tcp", DstPortMin: 80, DstPortMax: 80, ServicesNegate: true}
	r, err := in.toRule()
	if err != nil {
		t.Fatal(err)
	}
	out := ruleToExport(r)
	if out.SrcNegate != true || out.DstNegate != false || out.ServicesNegate != true {
		t.Fatalf("negate flags did not round trip: got %+v", out)
	}
}

// TestFirewallSrcAndDstNegateTogether confirms the two dimensions combine
// with ordinary AND semantics (as every dimension always has, negated or
// not) rather than negation somehow changing how dimensions combine with
// each other. With both src and dst negated, the rule — by De Morgan's law
// — matches (denies) only when a packet is OUTSIDE *both* exemptions
// simultaneously; being inside either one alone is enough for the rule not
// to match at all.
func TestFirewallSrcAndDstNegateTogether(t *testing.T) {
	a := netip.MustParseAddr
	r, err := FirewallRule{
		Action: "deny",
		Src:    "10.0.0.0/24", SrcNegate: true, // src dimension excludes 10.0.0.0/24
		Dst: "10.0.1.0/24", DstNegate: true, // dst dimension excludes 10.0.1.0/24
	}.toRule()
	if err != nil {
		t.Fatal(err)
	}
	f := newFirewall([]*fwRule{r})

	// Inside both exemptions: src dimension alone already fails to match, so
	// the rule doesn't match regardless of dst. Allowed.
	if !f.allow(fwOut, makeL4Addr(a("10.0.0.5"), a("10.0.1.5"), 6, 1)) {
		t.Fatal("src inside its exemption: rule shouldn't match regardless of dst, traffic allowed")
	}
	// src inside its exemption (dimension fails), dst outside its exemption:
	// the src dimension alone is enough to keep the rule from matching.
	if !f.allow(fwOut, makeL4Addr(a("10.0.0.5"), a("8.8.8.8"), 6, 1)) {
		t.Fatal("src inside its exemption: rule shouldn't match even though dst is outside its own exemption")
	}
	// src outside its exemption (dimension holds), dst inside its exemption
	// (dimension fails): the dst dimension alone keeps the rule from matching.
	if !f.allow(fwOut, makeL4Addr(a("9.9.9.9"), a("10.0.1.5"), 6, 1)) {
		t.Fatal("dst inside its exemption: rule shouldn't match even though src is outside its own exemption")
	}
	// Outside both exemptions simultaneously: only now do both dimensions
	// hold, so the rule matches and denies.
	if f.allow(fwOut, makeL4Addr(a("9.9.9.9"), a("8.8.8.8"), 6, 1)) {
		t.Fatal("outside both exemptions: rule should match (deny)")
	}
}
