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

// The v824 gap: a masquerade or SNAT rule's dest match was dropped by every
// backend, so an exemption like "masquerade 10/8 except when going to 10/8"
// rendered as "masquerade 10/8" and translated exactly the traffic it was
// written to leave alone. Silent, and visible only by reading the installed
// ruleset against the config that produced it.
func TestMasqueradeAndSNATCarryDestMatch(t *testing.T) {
	// The rule from the reported bundle.
	masq := Rule{
		Kind:   Masquerade,
		Source: netip.MustParsePrefix("10.0.0.0/8"),
		Dest:   netip.MustParsePrefix("10.0.0.0/8"), DestNeg: true,
		OutIface: "eth0",
	}
	snat := Rule{
		Kind:   SNAT,
		Source: netip.MustParsePrefix("10.0.0.0/8"),
		Dest:   netip.MustParsePrefix("192.168.0.0/16"),
		To:     netip.MustParseAddr("203.0.113.9"), OutIface: "eth0",
	}

	nft := nftScript([]Rule{masq, snat})
	if !strings.Contains(nft, "ip saddr 10.0.0.0/8 ip daddr != 10.0.0.0/8 oifname \"eth0\" masquerade") {
		t.Errorf("nft masquerade lost its dest match:\n%s", nft)
	}
	if !strings.Contains(nft, "ip daddr 192.168.0.0/16") {
		t.Errorf("nft snat lost its dest match:\n%s", nft)
	}

	if a := strings.Join(iptablesRuleArgs(masq), " "); !strings.Contains(a, "-s 10.0.0.0/8 ! -d 10.0.0.0/8") {
		t.Errorf("iptables masquerade: %s", a)
	}
	if a := strings.Join(iptablesRuleArgs(snat), " "); !strings.Contains(a, "-d 192.168.0.0/16") {
		t.Errorf("iptables snat: %s", a)
	}

	pf := pfScript([]Rule{masq, snat})
	if !strings.Contains(pf, "from 10.0.0.0/8 to ! 10.0.0.0/8") {
		t.Errorf("pf masquerade:\n%s", pf)
	}
	if !strings.Contains(pf, "to 192.168.0.0/16") {
		t.Errorf("pf snat:\n%s", pf)
	}
}

// A rule with no dest still renders exactly as before — "to any" on pf, no
// daddr clause on nft, no -d on iptables.
func TestNoDestRendersUnchanged(t *testing.T) {
	r := Rule{Kind: Masquerade, Source: netip.MustParsePrefix("10.0.0.0/8"), OutIface: "eth0"}
	if nft := nftScript([]Rule{r}); !strings.Contains(nft, "ip saddr 10.0.0.0/8 oifname \"eth0\" masquerade") {
		t.Errorf("nft:\n%s", nft)
	}
	if a := strings.Join(iptablesRuleArgs(r), " "); strings.Contains(a, "-d ") {
		t.Errorf("iptables should emit no -d: %s", a)
	}
	if pf := pfScript([]Rule{r}); !strings.Contains(pf, "to any") {
		t.Errorf("pf should keep \"to any\":\n%s", pf)
	}
}

// WinNAT has no destination selector on its masquerade equivalent, so a
// dest-scoped rule must be reported rather than rendered without the scope —
// which is the bug this release fixes, and would be worse on Windows because
// there is no syntax to fix it with.
func TestWinNATRejectsDestScopedMasquerade(t *testing.T) {
	r := Rule{Kind: Masquerade, Source: netip.MustParsePrefix("10.0.0.0/8"), Dest: netip.MustParsePrefix("10.0.0.0/8"), DestNeg: true}
	script, unsupported := winNATScript([]Rule{r})
	if len(unsupported) != 1 {
		t.Fatalf("dest-scoped masquerade should be unsupported, got %+v", unsupported)
	}
	if strings.Contains(script, "10.0.0.0/8") {
		t.Errorf("must not render the rule without its dest scope:\n%s", script)
	}
}
