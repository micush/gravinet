package mesh

import (
	"net/netip"
	"testing"

	"gravinet/internal/crypto"
)

// TestOverlayContains verifies the structural overlay-address check used to
// authorize overlay-sourced management and constrain the proxy. It must accept
// in-subnet addresses and reject everything a malicious peer might advertise to
// escape the overlay (loopback, link-local/metadata, multicast, unspecified,
// and any address outside the configured subnet such as the LAN or a public IP).
func TestOverlayContains(t *testing.T) {
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev("A")
	eng := NewEngine(Options{
		NodeID: "A", Hostname: "A",
		Nets: []NetSpec{{
			ID: 0x1, Name: "n", Keys: ks, Dev: dev,
			Self4:   netip.MustParseAddr("10.42.0.1"),
			Subnet4: netip.MustParsePrefix("10.42.0.0/16"),
		}},
	})

	cases := []struct {
		ip   string
		want bool
	}{
		{"10.42.3.7", true},        // legitimate overlay address
		{"10.42.0.1", true},        // self, in subnet
		{"10.99.0.1", false},       // valid unicast but outside the overlay subnet
		{"127.0.0.1", false},       // loopback (poison → localhost SSRF / auth bypass)
		{"169.254.169.254", false}, // link-local: cloud metadata
		{"192.168.1.1", false},     // LAN, not in overlay subnet
		{"203.0.113.5", false},     // public, not in overlay subnet
		{"0.0.0.0", false},         // unspecified
		{"224.0.0.1", false},       // multicast
		{"::1", false},             // ipv6 loopback
	}
	for _, c := range cases {
		if got := eng.OverlayContains(netip.MustParseAddr(c.ip)); got != c.want {
			t.Errorf("OverlayContains(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
	if eng.OverlayContains(netip.Addr{}) {
		t.Error("OverlayContains(invalid) should be false")
	}
}

// TestOverlayContains4in6 reproduces the remote-management bug: a dual-stack web
// admin sees inbound IPv4 connections as 4-in-6 mapped addresses (::ffff:a.b.c.d),
// and netip.Prefix.Contains does NOT match those against an IPv4 prefix. So the
// overlay-source auth check rejected every legitimate proxied request.
func TestOverlayContains4in6(t *testing.T) {
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev("A")
	eng := NewEngine(Options{
		NodeID: "A", Hostname: "A",
		Nets: []NetSpec{{
			ID: 0x1, Name: "n", Keys: ks, Dev: dev,
			Self4:   netip.MustParseAddr("10.50.0.1"),
			Subnet4: netip.MustParsePrefix("10.50.0.0/24"),
		}},
	})
	// The same overlay address, but 4-in-6 mapped as a dual-stack listener reports it.
	mapped := netip.AddrFrom16(netip.MustParseAddr("10.50.0.2").As16())
	if !mapped.Is4In6() {
		t.Fatal("test setup: expected a 4-in-6 mapped address")
	}
	if !eng.OverlayContains(mapped) {
		t.Fatal("4-in-6 mapped overlay address must be recognized (this is why remote management showed 'no networks')")
	}
}
