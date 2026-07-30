package netfilter

import (
	"net/netip"
	"strings"
	"testing"
)

func TestNftScriptDNATPortMatchAndRemap(t *testing.T) {
	rules := []Rule{
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), InIface: "eth0", To: netip.MustParseAddr("10.0.0.5"),
			Proto: "tcp", DPortLo: 32400, DPortHi: 32400},
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.6"),
			Proto: "udp", DPortLo: 8000, DPortHi: 8010},
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.7"),
			Proto: "tcp", DPortLo: 8443, DPortHi: 8443, ToPort: 443},
	}
	got := nftScript(rules)
	for _, want := range []string{
		`add rule ip gravinet_nat prerouting ip daddr 203.0.113.5/32 iifname "eth0" tcp dport 32400 dnat to 10.0.0.5`,
		`add rule ip gravinet_nat prerouting ip daddr 203.0.113.5/32 udp dport 8000-8010 dnat to 10.0.0.6`,
		`add rule ip gravinet_nat prerouting ip daddr 203.0.113.5/32 tcp dport 8443 dnat to 10.0.0.7:443`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("nft script missing line:\n  %s\nfull script:\n%s", want, got)
		}
	}
}

func TestNftDnatToBracketsIPv6Remap(t *testing.T) {
	r := Rule{Kind: DNAT, To: netip.MustParseAddr("2001:db8::5"), ToPort: 443, V6: true}
	if got, want := nftDnatTo(r), "[2001:db8::5]:443"; got != want {
		t.Errorf("nftDnatTo = %q, want %q", got, want)
	}
}

func TestPfScriptDNATPortMatchAndRemap(t *testing.T) {
	rules := []Rule{
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), InIface: "em0", To: netip.MustParseAddr("10.0.0.5"),
			Proto: "tcp", DPortLo: 32400, DPortHi: 32400},
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.6"),
			Proto: "udp", DPortLo: 8000, DPortHi: 8010},
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.7"),
			Proto: "tcp", DPortLo: 8443, DPortHi: 8443, ToPort: 443},
	}
	got := pfScript(rules)
	for _, want := range []string{
		"rdr on em0 inet proto tcp from any to 203.0.113.5/32 port 32400 -> 10.0.0.5",
		"rdr inet proto udp from any to 203.0.113.5/32 port 8000:8010 -> 10.0.0.6",
		"rdr inet proto tcp from any to 203.0.113.5/32 port 8443 -> 10.0.0.7 port 443",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("pf script missing line:\n  %s\nfull script:\n%s", want, got)
		}
	}
}

func TestIptablesRuleArgsDNATPortMatchAndRemap(t *testing.T) {
	cases := []struct {
		name string
		r    Rule
		want []string
	}{
		{"single port, no remap",
			Rule{Kind: DNAT, Dest: mustP("203.0.113.5/32"), InIface: "eth0", To: netip.MustParseAddr("10.0.0.5"), Proto: "tcp", DPortLo: 32400, DPortHi: 32400},
			[]string{"-t", "nat", "-A", "GRAVINET_NAT_PRE", "-d", "203.0.113.5/32", "-i", "eth0", "-p", "tcp", "--dport", "32400", "-j", "DNAT", "--to-destination", "10.0.0.5"}},
		{"range",
			Rule{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.6"), Proto: "udp", DPortLo: 8000, DPortHi: 8010},
			[]string{"-t", "nat", "-A", "GRAVINET_NAT_PRE", "-d", "203.0.113.5/32", "-p", "udp", "--dport", "8000:8010", "-j", "DNAT", "--to-destination", "10.0.0.6"}},
		{"remap",
			Rule{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.7"), Proto: "tcp", DPortLo: 8443, DPortHi: 8443, ToPort: 443},
			[]string{"-t", "nat", "-A", "GRAVINET_NAT_PRE", "-d", "203.0.113.5/32", "-p", "tcp", "--dport", "8443", "-j", "DNAT", "--to-destination", "10.0.0.7:443"}},
	}
	for _, c := range cases {
		got := iptablesRuleArgs(c.r)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s:\n  got  %v\n  want %v", c.name, got, c.want)
		}
	}
}

// TestWinNATScriptDNATSinglePortSupported checks the case PAT specifically
// unlocks: a single-port DNAT with an explicit protocol and a concrete
// external address now gets its own NetNat + static mapping, instead of
// falling into "unsupported" the way every DNAT rule used to (see
// winNATScript's own doc comment for why WinNAT could never express the
// old address-only, all-ports form).
func TestWinNATScriptDNATSinglePortSupported(t *testing.T) {
	rules := []Rule{
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.5"),
			Proto: "tcp", DPortLo: 32400, DPortHi: 32400},
	}
	script, unsupported := winNATScript(rules)
	if len(unsupported) != 0 {
		t.Fatalf("expected the single-port DNAT to be supported, got unsupported: %+v", unsupported)
	}
	if !strings.Contains(script, `New-NetNat -Name "gravinet_nat_0" -InternalIPInterfaceAddressPrefix "10.0.0.5/32"`) {
		t.Errorf("winNAT script missing the per-rule NetNat scoped to the internal target:\n%s", script)
	}
	if !strings.Contains(script, `Add-NetNatStaticMapping -NatName "gravinet_nat_0" -Protocol TCP -ExternalIPAddress "203.0.113.5" -ExternalPort 32400 -InternalIPAddress "10.0.0.5" -InternalPort 32400`) {
		t.Errorf("winNAT script missing the static mapping:\n%s", script)
	}
}

// TestWinNATScriptDNATSinglePortRemapSupported checks the internal port in
// the static mapping is the remapped (ToPort) value, not the externally
// matched one, when they differ.
func TestWinNATScriptDNATSinglePortRemapSupported(t *testing.T) {
	rules := []Rule{
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.7"),
			Proto: "udp", DPortLo: 8443, DPortHi: 8443, ToPort: 443},
	}
	script, unsupported := winNATScript(rules)
	if len(unsupported) != 0 {
		t.Fatalf("expected the remapped single-port DNAT to be supported, got unsupported: %+v", unsupported)
	}
	if !strings.Contains(script, `-ExternalPort 8443 -InternalIPAddress "10.0.0.7" -InternalPort 443`) {
		t.Errorf("winNAT script should use the external port for -ExternalPort and the remapped port for -InternalPort:\n%s", script)
	}
	if !strings.Contains(script, "-Protocol UDP") {
		t.Errorf("winNAT script should render proto \"udp\" as -Protocol UDP:\n%s", script)
	}
}

// TestWinNATScriptDNATStillUnsupportedCases checks the shapes that remain
// genuinely inexpressible in WinNAT even after PAT: a port *range* (a
// static mapping is always exactly one port) and address-only/all-ports (no
// port information at all — the original, pre-PAT limitation).
func TestWinNATScriptDNATStillUnsupportedCases(t *testing.T) {
	rules := []Rule{
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.5"), Proto: "tcp", DPortLo: 8000, DPortHi: 8010}, // range
		{Kind: DNAT, Dest: mustP("203.0.113.5/32"), To: netip.MustParseAddr("10.0.0.6")},                                            // address-only, no port at all
		{Kind: DNAT, Dest: mustP("203.0.113.0/24"), To: netip.MustParseAddr("10.0.0.7"), Proto: "tcp", DPortLo: 80, DPortHi: 80},     // Dest isn't a /32
	}
	_, unsupported := winNATScript(rules)
	if len(unsupported) != len(rules) {
		t.Fatalf("expected all %d rules to remain unsupported, got %d: %+v", len(rules), len(unsupported), unsupported)
	}
}
