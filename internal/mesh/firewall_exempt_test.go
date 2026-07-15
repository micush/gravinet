package mesh

import (
	"net/netip"
	"testing"
)

// mkL4Ports builds a minimal IPv4 packet with both source and destination L4
// ports set (makeL4Addr only sets the destination port).
func mkL4Ports(proto uint8, sport, dport uint16) []byte {
	p := make([]byte, 24)
	p[0] = 0x45
	p[2], p[3] = 0, 24
	p[8] = 64
	p[9] = proto
	src := netip.MustParseAddr("10.42.0.1").As4()
	dst := netip.MustParseAddr("10.42.0.2").As4()
	copy(p[12:16], src[:])
	copy(p[16:20], dst[:])
	p[20], p[21] = byte(sport>>8), byte(sport)
	p[22], p[23] = byte(dport>>8), byte(dport)
	return p
}

// TestFirewallExemptsControlTraffic proves a blanket deny rule cannot cut off
// remote management or the BGP/OSPF/RIP routing protocols, in either direction,
// while still blocking ordinary traffic. It is written so it would FAIL if the
// exemption were removed (the ordinary-packet assertion confirms deny-all is
// really in force).
func TestFirewallExemptsControlTraffic(t *testing.T) {
	const mgmt = 8443
	fw := newFirewall([]*fwRule{mustRule(t, "deny", "both", "any", 0)}) // deny everything
	fw.setMgmtPort(mgmt)
	// The default allowlist (config.DefaultFirewallExempts), resolved to numbers.
	fw.setExempts([]FirewallExempt{
		{Name: "remote management", Proto: 6, Mgmt: true},
		{Name: "BGP", Proto: 6, Port: 179},
		{Name: "OSPF", Proto: 89},
		{Name: "RIP", Proto: 17, Port: 520},
		{Name: "RIPng", Proto: 17, Port: 521},
	})

	// Control: an ordinary packet must be dropped, both directions.
	if fw.allow(fwIn, mkL4Ports(6, 5000, 8080)) || fw.allow(fwOut, mkL4Ports(6, 5000, 8080)) {
		t.Fatal("deny-all rule should block ordinary TCP traffic")
	}

	cases := []struct {
		name string
		pkt  []byte
	}{
		{"mgmt dport", mkL4Ports(6, 5000, mgmt)},
		{"mgmt sport", mkL4Ports(6, mgmt, 5000)},
		{"BGP dport 179", mkL4Ports(6, 5000, 179)},
		{"BGP sport 179", mkL4Ports(6, 179, 5000)},
		{"OSPF proto 89", mkL4Ports(89, 0, 0)},
		{"RIP udp 520", mkL4Ports(17, 520, 520)},
		{"RIPng udp 521 dport", mkL4Ports(17, 5000, 521)},
		{"RIPng udp 521 sport", mkL4Ports(17, 521, 5000)},
	}
	for _, c := range cases {
		if !fw.allow(fwIn, c.pkt) {
			t.Errorf("%s: should be exempt inbound, was blocked", c.name)
		}
		if !fw.allow(fwOut, c.pkt) {
			t.Errorf("%s: should be exempt outbound, was blocked", c.name)
		}
	}

	// With no management port configured, a Mgmt exemption must NOT match port 0,
	// and a non-exempt port stays blocked.
	fw0 := newFirewall([]*fwRule{mustRule(t, "deny", "both", "any", 0)})
	fw0.setExempts([]FirewallExempt{{Name: "remote management", Proto: 6, Mgmt: true}})
	if fw0.allow(fwIn, mkL4Ports(6, 0, 0)) {
		t.Error("mgmtPort unset: TCP port 0 must not be exempt")
	}
	if fw.allow(fwIn, mkL4Ports(17, 5000, 9999)) {
		t.Error("ordinary UDP 9999 must remain blocked")
	}

	// An explicitly empty allowlist disables all exemptions — even management.
	fwNone := newFirewall([]*fwRule{mustRule(t, "deny", "both", "any", 0)})
	fwNone.setMgmtPort(mgmt)
	fwNone.setExempts([]FirewallExempt{})
	if fwNone.allow(fwIn, mkL4Ports(6, 5000, mgmt)) {
		t.Error("empty allowlist should exempt nothing, including management")
	}
}
