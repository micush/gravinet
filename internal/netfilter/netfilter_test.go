package netfilter

import (
	"net/netip"
	"strings"
	"testing"
)

func mustP(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func TestNftScript(t *testing.T) {
	rules := []Rule{
		{Kind: Masquerade, Source: mustP("10.0.0.0/24"), OutIface: "eth0"},
		{Kind: SNAT, Source: mustP("10.0.1.0/24"), OutIface: "eth0", To: netip.MustParseAddr("203.0.113.5")},
		{Kind: DNAT, Dest: mustP("198.51.100.10/32"), InIface: "eth0", To: netip.MustParseAddr("10.0.0.9")},
		{Kind: Masquerade, OutIface: "wan0"}, // no source = any
	}
	got := nftScript(rules)

	// Scaffolding: one transaction that creates, flushes, and repopulates.
	for _, want := range []string{
		"add table ip gravinet_nat",
		"flush table ip gravinet_nat",
		"add chain ip gravinet_nat postrouting { type nat hook postrouting priority 100 ; }",
		"add chain ip gravinet_nat prerouting { type nat hook prerouting priority -100 ; }",
		`add rule ip gravinet_nat postrouting ip saddr 10.0.0.0/24 oifname "eth0" masquerade`,
		`add rule ip gravinet_nat postrouting ip saddr 10.0.1.0/24 oifname "eth0" snat to 203.0.113.5`,
		`add rule ip gravinet_nat prerouting ip daddr 198.51.100.10/32 iifname "eth0" dnat to 10.0.0.9`,
		`add rule ip gravinet_nat postrouting oifname "wan0" masquerade`, // no saddr clause
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("nft script missing line:\n  %s\nfull script:\n%s", want, got)
		}
	}
	// The flush must precede the rules so re-apply never leaves stale entries.
	if strings.Index(got, "flush table") > strings.Index(got, "add rule") {
		t.Error("flush must come before add rule")
	}
}

func TestIptablesRuleArgs(t *testing.T) {
	cases := []struct {
		name string
		r    Rule
		want []string
	}{
		{"masquerade", Rule{Kind: Masquerade, Source: mustP("10.0.0.0/24"), OutIface: "eth0"},
			[]string{"-t", "nat", "-A", "GRAVINET_NAT_POST", "-s", "10.0.0.0/24", "-o", "eth0", "-j", "MASQUERADE"}},
		{"masquerade-any", Rule{Kind: Masquerade, OutIface: "wan0"},
			[]string{"-t", "nat", "-A", "GRAVINET_NAT_POST", "-o", "wan0", "-j", "MASQUERADE"}},
		{"snat", Rule{Kind: SNAT, Source: mustP("10.0.1.0/24"), OutIface: "eth0", To: netip.MustParseAddr("203.0.113.5")},
			[]string{"-t", "nat", "-A", "GRAVINET_NAT_POST", "-s", "10.0.1.0/24", "-o", "eth0", "-j", "SNAT", "--to-source", "203.0.113.5"}},
		{"dnat", Rule{Kind: DNAT, Dest: mustP("198.51.100.10/32"), InIface: "eth0", To: netip.MustParseAddr("10.0.0.9")},
			[]string{"-t", "nat", "-A", "GRAVINET_NAT_PRE", "-d", "198.51.100.10/32", "-i", "eth0", "-j", "DNAT", "--to-destination", "10.0.0.9"}},
	}
	for _, c := range cases {
		got := iptablesRuleArgs(c.r)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s:\n  got  %v\n  want %v", c.name, got, c.want)
		}
	}
}

func TestNftScriptIPv6(t *testing.T) {
	rules := []Rule{
		{Kind: Masquerade, Source: mustP("fd00:42::/64"), OutIface: "eth0", V6: true},
		{Kind: SNAT, Source: mustP("fd00:43::/64"), OutIface: "eth0", To: netip.MustParseAddr("2001:db8::5"), V6: true},
		{Kind: DNAT, Dest: mustP("2001:db8::10/128"), InIface: "eth0", To: netip.MustParseAddr("fd00:42::9"), V6: true},
		{Kind: Masquerade, Source: mustP("10.0.0.0/24"), OutIface: "eth0"}, // a v4 rule alongside
	}
	got := nftScript(rules)
	for _, want := range []string{
		"add table ip6 gravinet_nat",
		"add chain ip6 gravinet_nat postrouting { type nat hook postrouting priority 100 ; }",
		`add rule ip6 gravinet_nat postrouting ip6 saddr fd00:42::/64 oifname "eth0" masquerade`,
		`add rule ip6 gravinet_nat postrouting ip6 saddr fd00:43::/64 oifname "eth0" snat to 2001:db8::5`,
		`add rule ip6 gravinet_nat prerouting ip6 daddr 2001:db8::10/128 iifname "eth0" dnat to fd00:42::9`,
		// the v4 rule lands in the ip table, not ip6
		"add table ip gravinet_nat",
		`add rule ip gravinet_nat postrouting ip saddr 10.0.0.0/24 oifname "eth0" masquerade`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("nft script missing line:\n  %s\nfull script:\n%s", want, got)
		}
	}
	// A v4 saddr must never leak into an ip6 rule and vice-versa.
	if strings.Contains(got, "ip6 saddr 10.0.0.0") || strings.Contains(got, "add rule ip gravinet_nat postrouting ip saddr fd00") {
		t.Error("address family mismatch in generated rules")
	}
}

func TestNftScriptOnlyEmitsUsedFamilies(t *testing.T) {
	// IPv4-only ruleset must not create an ip6 table (avoids touching kernels
	// without ip6 NAT support).
	v4 := nftScript([]Rule{{Kind: Masquerade, OutIface: "wan0"}})
	if strings.Contains(v4, "ip6") {
		t.Errorf("v4-only script should not mention ip6:\n%s", v4)
	}
	v6 := nftScript([]Rule{{Kind: Masquerade, OutIface: "wan0", V6: true}})
	if strings.Contains(v6, "add table ip gravinet_nat") {
		t.Errorf("v6-only script should not create the ip table:\n%s", v6)
	}
}

func TestPfScript(t *testing.T) {
	rules := []Rule{
		{Kind: Masquerade, Source: mustP("10.0.0.0/24"), OutIface: "em0"},
		{Kind: Masquerade, OutIface: ""}, // no interface = pf "egress" group
		{Kind: SNAT, Source: mustP("10.0.1.0/24"), OutIface: "em0", To: netip.MustParseAddr("203.0.113.5")},
		{Kind: SNAT, Source: mustP("10.0.2.0/24"), To: netip.MustParseAddr("203.0.113.6")}, // no OutIface
		{Kind: DNAT, Dest: mustP("198.51.100.10/32"), InIface: "em0", To: netip.MustParseAddr("10.0.0.9")},
		{Kind: DNAT, Dest: mustP("198.51.100.11/32"), To: netip.MustParseAddr("10.0.0.10")}, // no InIface
	}
	got := pfScript(rules)
	for _, want := range []string{
		"nat on em0 inet from 10.0.0.0/24 to any -> (em0)",
		"nat on egress inet from any to any -> (egress)",
		"nat on em0 inet from 10.0.1.0/24 to any -> 203.0.113.5",
		"nat inet from 10.0.2.0/24 to any -> 203.0.113.6",
		"rdr on em0 inet from any to 198.51.100.10/32 -> 10.0.0.9",
		"rdr inet from any to 198.51.100.11/32 -> 10.0.0.10",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("pf script missing line:\n  %s\nfull script:\n%s", want, got)
		}
	}
}

func TestPfScriptIPv6(t *testing.T) {
	got := pfScript([]Rule{
		{Kind: Masquerade, Source: mustP("fd00:42::/64"), OutIface: "em0", V6: true},
		{Kind: DNAT, Dest: mustP("2001:db8::10/128"), InIface: "em0", To: netip.MustParseAddr("fd00:42::9"), V6: true},
	})
	for _, want := range []string{
		"nat on em0 inet6 from fd00:42::/64 to any -> (em0)",
		"rdr on em0 inet6 from any to 2001:db8::10/128 -> fd00:42::9",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("pf script missing line:\n  %s\nfull script:\n%s", want, got)
		}
	}
}

func TestWinNATScript(t *testing.T) {
	rules := []Rule{
		{Kind: Masquerade, Source: mustP("10.0.0.0/24"), OutIface: "eth0"},                 // OutIface ignored: WinNAT has no equivalent
		{Kind: SNAT, Source: mustP("10.0.1.0/24"), To: netip.MustParseAddr("203.0.113.5")}, // no WinNAT equivalent
		{Kind: DNAT, Dest: mustP("198.51.100.10/32"), To: netip.MustParseAddr("10.0.0.9")}, // no WinNAT equivalent
		{Kind: Masquerade, Source: mustP("fd00:42::/64"), V6: true},                        // WinNAT here is v4-only
	}
	script, unsupported := winNATScript(rules)

	if !strings.Contains(script, `New-NetNat -Name "gravinet_nat_0" -InternalIPInterfaceAddressPrefix "10.0.0.0/24"`) {
		t.Errorf("winNAT script missing New-NetNat line:\n%s", script)
	}
	if !strings.Contains(script, "Remove-NetNatStaticMapping") || !strings.Contains(script, "Remove-NetNat ") {
		t.Errorf("winNAT script should tear down prior gravinet_nat_* objects first:\n%s", script)
	}
	if len(unsupported) != 3 {
		t.Fatalf("expected 3 unsupported rules (fixed SNAT, DNAT, v6), got %d: %+v", len(unsupported), unsupported)
	}
}

func TestWinNATScriptEmptyRulesStillTearsDown(t *testing.T) {
	// Clear() reuses winNATScript(nil) purely for its teardown lines.
	script, unsupported := winNATScript(nil)
	if len(unsupported) != 0 {
		t.Errorf("nil rules should report no unsupported rules, got %+v", unsupported)
	}
	if !strings.Contains(script, "Remove-NetNat ") {
		t.Errorf("expected teardown of prior gravinet objects even with no rules:\n%s", script)
	}
	if strings.Contains(script, "New-NetNat -Name") {
		t.Errorf("nil rules should not create any NetNat objects:\n%s", script)
	}
}

func TestIptablesArgsIPv6(t *testing.T) {
	// ip6tables takes the same args as iptables; the binary differs, not the argv.
	got := iptablesRuleArgs(Rule{Kind: SNAT, Source: mustP("fd00:42::/64"), OutIface: "eth0", To: netip.MustParseAddr("2001:db8::5"), V6: true})
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-s fd00:42::/64") || !strings.Contains(joined, "--to-source 2001:db8::5") {
		t.Errorf("unexpected ip6tables args: %v", got)
	}
}
