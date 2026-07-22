package tun

import (
	"net/netip"
	"reflect"
	"testing"
)

// TestPtpIfconfigArgsIncludesNetmask is the regression test for the actual
// macOS bug: AddIPv4 used to build "inet <addr> <addr>" with no netmask at
// all. On a point-to-point utun interface with identical local/dest
// addresses, that leaves macOS to default the mask to /32 — a route to this
// node's own single address and nothing else, so packets addressed to any
// other overlay peer never even reached the tun device. This asserts the
// exact argument list ifconfig receives, so a regression back to "no
// netmask" fails here instead of only being discoverable on a real Mac.
func TestPtpIfconfigArgsIncludesNetmask(t *testing.T) {
	tests := []struct {
		name       string
		addr       string
		prefixLen  int
		wantMaskIP string
	}{
		{"slash-16 overlay subnet", "10.42.0.7", 16, "255.255.0.0"},
		{"slash-24 overlay subnet", "10.42.0.7", 24, "255.255.255.0"},
		{"slash-32 single host", "10.42.0.7", 32, "255.255.255.255"},
		{"slash-8 broad subnet", "10.0.0.5", 8, "255.0.0.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.addr)
			got := ptpIfconfigArgs(addr, tc.prefixLen)
			want := []string{"inet", tc.addr, tc.addr, "netmask", tc.wantMaskIP}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ptpIfconfigArgs(%s, /%d) = %v, want %v", tc.addr, tc.prefixLen, got, want)
			}
		})
	}
}

// TestPtpIfconfigArgsLocalEqualsDest proves the point-to-point local/dest
// pair stays identical (that part of the original design was correct and
// intentional — only the missing netmask was the bug) — both positional
// address arguments must be the assigned address itself.
func TestPtpIfconfigArgsLocalEqualsDest(t *testing.T) {
	addr := netip.MustParseAddr("10.99.0.1")
	args := ptpIfconfigArgs(addr, 16)
	if len(args) < 3 || args[1] != args[2] {
		t.Fatalf("expected local and dest addresses to match, got args=%v", args)
	}
}
