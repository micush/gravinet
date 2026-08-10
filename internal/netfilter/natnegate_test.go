package netfilter

import (
	"net/netip"
	"strings"
	"testing"
)

// Each backend spells negation differently and a wrong spelling is worse than
// no support: nft, iptables and pf would each reject or, worse, silently
// reinterpret a malformed match. These pin the exact rendered syntax.
func TestNegationRendering(t *testing.T) {
	snat := Rule{
		Kind: Masquerade, Source: netip.MustParsePrefix("10.1.1.0/24"),
		SourceNeg: true, OutIface: "eth0",
	}
	dnat := Rule{
		Kind: DNAT, Dest: netip.MustParsePrefix("10.3.3.0/24"), DestNeg: true,
		To: netip.MustParseAddr("192.168.5.10"), Proto: "tcp", DPortLo: 8080, DPortHi: 8080,
	}

	nft := nftScript([]Rule{snat, dnat})
	// nft wants a space after the operator: "saddr != 10.1.1.0/24".
	if !strings.Contains(nft, "ip saddr != 10.1.1.0/24") {
		t.Errorf("nft source negation missing or malformed:\n%s", nft)
	}
	if !strings.Contains(nft, "ip daddr != 10.3.3.0/24") {
		t.Errorf("nft dest negation missing or malformed:\n%s", nft)
	}

	// iptables puts the bang as its own argument, immediately before the
	// option it negates.
	args := strings.Join(iptablesRuleArgs(snat), " ")
	if !strings.Contains(args, "! -s 10.1.1.0/24") {
		t.Errorf("iptables source negation: %s", args)
	}
	if a := strings.Join(iptablesRuleArgs(dnat), " "); !strings.Contains(a, "! -d 10.3.3.0/24") {
		t.Errorf("iptables dest negation: %s", a)
	}

	pf := pfScript([]Rule{snat, dnat})
	if !strings.Contains(pf, "from ! 10.1.1.0/24") {
		t.Errorf("pf source negation:\n%s", pf)
	}
	if !strings.Contains(pf, "to ! 10.3.3.0/24") {
		t.Errorf("pf dest negation:\n%s", pf)
	}
}

// Without the flags set, nothing changes — the negation work must not alter
// how an ordinary rule renders on any backend.
func TestPositiveRulesUnchangedByNegationSupport(t *testing.T) {
	r := Rule{Kind: Masquerade, Source: netip.MustParsePrefix("10.1.1.0/24"), OutIface: "eth0"}
	if nft := nftScript([]Rule{r}); strings.Contains(nft, "!") {
		t.Errorf("un-negated rule should render no bang:\n%s", nft)
	}
	if a := strings.Join(iptablesRuleArgs(r), " "); strings.Contains(a, "!") {
		t.Errorf("un-negated iptables args: %s", a)
	}
	if pf := pfScript([]Rule{r}); strings.Contains(pf, "!") {
		t.Errorf("un-negated pf rule:\n%s", pf)
	}
}

// A blank prefix is "any" and pf has no "! any" — negation on it must be
// ignored rather than emitted as invalid syntax that pfctl would reject,
// taking the whole anchor down with it.
func TestPfBlankPrefixIgnoresNegation(t *testing.T) {
	var blank netip.Prefix
	if got := pfAddr(blank, true); got != "any" {
		t.Errorf("pfAddr(blank, negated) = %q, want \"any\"", got)
	}
}

// WinNAT matches one concrete prefix with no inverse. Rendering the positive
// twin would translate exactly the traffic the operator excluded — the worst
// possible outcome — so a negated rule must land in the unsupported list.
func TestWinNATRejectsNegatedRules(t *testing.T) {
	neg := Rule{Kind: Masquerade, Source: netip.MustParsePrefix("10.1.1.0/24"), SourceNeg: true}
	pos := Rule{Kind: Masquerade, Source: netip.MustParsePrefix("10.2.2.0/24")}

	script, unsupported := winNATScript([]Rule{neg, pos})
	if len(unsupported) != 1 || !unsupported[0].SourceNeg {
		t.Fatalf("the negated rule should be reported unsupported, got %+v", unsupported)
	}
	if strings.Contains(script, "10.1.1.0/24") {
		t.Errorf("a negated rule must not be rendered as its positive twin:\n%s", script)
	}
	if !strings.Contains(script, "10.2.2.0/24") {
		t.Errorf("the ordinary rule should still render:\n%s", script)
	}
}
