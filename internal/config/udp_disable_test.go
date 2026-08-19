package config

import "testing"

// TestValidateUDPOffRequiresTCP covers the "UDP is turned off" case
// behind the web admin's UDP port "-" field, and its refusal when nothing is
// left to reach the node on.
//
// This test predates the v789 fold of primary_port/extra_listen_ports into
// the flat udp_ports/tcp_ports lists and was never migrated with it: it went
// on referencing a Config.PrimaryPort field that no longer existed, so the
// whole config package's tests stopped compiling — and therefore stopped
// running — rather than failing visibly. Rewritten here in the current shape,
// asserting the same five things it always meant to.
//
// The sentinel moved with the refactor: "UDP off" is now an empty udp_ports
// list rather than primary_port == 0, and the reachability rule generalized
// from "UDP off needs the TCP on" to "udp_ports and tcp_ports must
// not both be empty."
func TestValidateUDPOffRequiresTCP(t *testing.T) {
	// UDP off, TCP on — must validate.
	c := &Config{UDPPorts: nil, TCPPorts: []int{65432}, EnableIPv4: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("udp off with tcp on should validate: %v", err)
	}

	// UDP off, TCP also off — must be refused, since the node would have no
	// way to be reached at all.
	c = &Config{UDPPorts: nil, TCPPorts: nil, EnableIPv4: true}
	if err := c.Validate(); err == nil {
		t.Fatal("udp and tcp both off should fail validation")
	}

	// A negative port is out of range regardless of the other transport —
	// an empty list is the only valid way to say "off", not any value <= 0.
	c = &Config{UDPPorts: []int{-1}, TCPPorts: []int{65432}, EnableIPv4: true}
	if err := c.Validate(); err == nil {
		t.Fatal("negative udp port should fail validation")
	}
	// Port 0 is likewise out of range as a list entry: it is not a sentinel
	// here, and the empty-list spelling is what carries that meaning now.
	c = &Config{UDPPorts: []int{0}, TCPPorts: []int{65432}, EnableIPv4: true}
	if err := c.Validate(); err == nil {
		t.Fatal("udp port 0 should fail validation (empty list is how UDP is turned off)")
	}

	// Both on (the ordinary case) still validates.
	c = &Config{UDPPorts: []int{51820}, TCPPorts: []int{65432}, EnableIPv4: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("udp and tcp both on should validate: %v", err)
	}

	// UDP on, TCP off — also fine; only both-off is refused.
	c = &Config{UDPPorts: []int{51820}, TCPPorts: nil, EnableIPv4: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("udp on with tcp off should validate: %v", err)
	}
}
