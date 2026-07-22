package mesh

import (
	"net/netip"
	"testing"
)

// mkFlow builds an IPv4 packet with explicit addresses and L4 ports, so a
// forward packet and its reply can be constructed to exercise conntrack.
func mkFlow(src, dst netip.Addr, proto uint8, sp, dp uint16) []byte {
	p := make([]byte, 24)
	p[0] = 0x45
	p[2], p[3] = 0, 24
	p[8] = 64
	p[9] = proto
	s := src.As4()
	d := dst.As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	p[20], p[21] = byte(sp>>8), byte(sp)
	p[22], p[23] = byte(dp>>8), byte(dp)
	return p
}

// TestRuleChangeAppliesToEstablishedFlows reproduces the reported bug: with a
// stateful firewall, a connection that has been established (seen in both
// directions) is allowed from the conntrack table regardless of the rulebase.
// A newly added deny rule therefore appears to "need a restart" — the live edit
// has no effect on the established flow. Rule changes must re-evaluate flows.
func TestRuleChangeAppliesToEstablishedFlows(t *testing.T) {
	a := netip.MustParseAddr("10.5.0.1")
	b := netip.MustParseAddr("10.5.0.2")

	// A deny rule on an unrelated port makes the firewall stateful.
	fw := newFirewall([]*fwRule{mustRule(t, "deny", "both", "tcp", 9999)})

	fwd := mkFlow(a, b, 6, 40000, 80) // client -> server:80
	rev := mkFlow(b, a, 6, 80, 40000) // server reply
	// Establish the flow: forward then reply.
	if !fw.allow(fwIn, fwd) {
		t.Fatal("tcp/80 should be allowed before any deny rule")
	}
	if !fw.allow(fwIn, rev) {
		t.Fatal("reply should be allowed")
	}

	// Operator adds a deny rule for tcp/80 (the engine path the web/CLI use).
	fw.add(mustRule(t, "deny", "both", "tcp", 80), -1)

	// The new rule must take effect immediately, even on the established flow.
	if fw.allow(fwIn, fwd) {
		t.Fatal("after adding a deny rule, tcp/80 must be blocked live (established flow must be re-evaluated)")
	}
}

// TestEnableToggleClearsState confirms toggling the firewall also re-evaluates
// flows (the user's working workaround), keeping behavior consistent with edits.
func TestEnableToggleClearsState(t *testing.T) {
	a := netip.MustParseAddr("10.5.0.1")
	b := netip.MustParseAddr("10.5.0.2")
	fw := newFirewall([]*fwRule{mustRule(t, "deny", "both", "tcp", 9999)})
	fw.allow(fwIn, mkFlow(a, b, 6, 40000, 80))
	fw.allow(fwIn, mkFlow(b, a, 6, 80, 40000)) // establish
	fw.add(mustRule(t, "deny", "both", "tcp", 80), -1)
	fw.setEnabled(false)
	fw.setEnabled(true)
	if fw.allow(fwIn, mkFlow(a, b, 6, 40000, 80)) {
		t.Fatal("after a firewall toggle, the deny rule must apply to the flow")
	}
}
