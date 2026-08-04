package config

import "testing"

// A node listens on a set of UDP ports and a set of TCP ports. "Off" for a
// transport is an empty list — not a zero port, which is how the pre-v789
// shape spelled it (primary_port == 0, plus a separate disable_tcp_fallback
// bool for the other side). Two spellings of "off" for two transports was one
// of the smaller symptoms of modelling them as a hierarchy rather than as two
// lists; there is one spelling now, and it is the same for both.
//
// The only combination Validate refuses outright is both empty: a node with no
// transport could never be reached, and no amount of other configuration
// rescues it.
func TestValidateTransportPortLists(t *testing.T) {
	// UDP off, TCP on.
	c := &Config{EnableIPv4: true, TCPPorts: []int{65432}}
	if err := c.Validate(); err != nil {
		t.Fatalf("udp off with tcp on should validate: %v", err)
	}

	// TCP off, UDP on. Symmetric with the above, which it was not before:
	// turning UDP off meant a sentinel port value, turning TCP off meant a
	// boolean field.
	c = &Config{EnableIPv4: true, UDPPorts: []int{51820}}
	if err := c.Validate(); err != nil {
		t.Fatalf("tcp off with udp on should validate: %v", err)
	}

	// Both on, the ordinary case.
	c = &Config{EnableIPv4: true, UDPPorts: []int{51820, 443}, TCPPorts: []int{65432, 80}}
	if err := c.Validate(); err != nil {
		t.Fatalf("both transports on should validate: %v", err)
	}

	// Both off — the one refusal.
	c = &Config{EnableIPv4: true}
	if err := c.Validate(); err == nil {
		t.Fatal("both port lists empty should fail validation — the node could never be reached")
	}

	// 0 is not a sentinel any more, it is just out of range. Accepting it
	// would resurrect the ambiguity the empty list exists to remove.
	c = &Config{EnableIPv4: true, UDPPorts: []int{0}, TCPPorts: []int{65432}}
	if err := c.Validate(); err == nil {
		t.Fatal("port 0 should fail validation; an empty list is how a transport is turned off")
	}
	c = &Config{EnableIPv4: true, UDPPorts: []int{-1}, TCPPorts: []int{65432}}
	if err := c.Validate(); err == nil {
		t.Fatal("negative port should fail validation")
	}
	c = &Config{EnableIPv4: true, UDPPorts: []int{70000}, TCPPorts: []int{65432}}
	if err := c.Validate(); err == nil {
		t.Fatal("port above 65535 should fail validation")
	}
}

// TestPortListAccessors: the first entry is what this node advertises, and
// duplicates are dropped. Binding the same port twice fails the second time
// and reads as a configuration error in the log; a list is meant to be safe to
// widen without checking what is already in it.
func TestPortListAccessors(t *testing.T) {
	c := &Config{UDPPorts: []int{51820, 443, 51820, 0, 70000}, TCPPorts: []int{65432, 80}}
	if got, want := c.UDPPortList(), []int{51820, 443}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("UDPPortList = %v, want %v (deduped, out-of-range dropped, order kept)", got, want)
	}
	if got := c.AdvertisedUDPPort(); got != 51820 {
		t.Errorf("AdvertisedUDPPort = %d, want the first entry 51820", got)
	}
	if got := c.AdvertisedTCPPort(); got != 65432 {
		t.Errorf("AdvertisedTCPPort = %d, want the first entry 65432", got)
	}
	if !c.UDPEnabled() || !c.TCPEnabled() {
		t.Error("both transports should read as enabled")
	}

	empty := &Config{}
	if empty.UDPEnabled() || empty.TCPEnabled() {
		t.Error("empty lists should read as disabled")
	}
	if empty.AdvertisedUDPPort() != 0 || empty.AdvertisedTCPPort() != 0 {
		t.Error("a disabled transport advertises port 0")
	}
}
