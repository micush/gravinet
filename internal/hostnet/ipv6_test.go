package hostnet

import (
	"net"
	"net/netip"
	"testing"
)

func mac(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	hw, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("bad test MAC %q: %v", s, err)
	}
	return hw
}

// The derivation has to produce exactly what the kernel would have produced
// under the default addr_gen_mode. That is the whole reason it is safe to add
// one automatically: if the kernel ever does generate its own, it generates
// this address, so the interface ends up holding the address it already had
// rather than a second one.
//
// The first case is the interface from the report that prompted this — MAC
// 0c:0e:41:8a:00:01, whose kernel-generated link-local would have been
// fe80::e0e:41ff:fe8a:1 had one been generated at all.
func TestLinkLocalForMatchesKernelDerivation(t *testing.T) {
	for _, tc := range []struct{ hw, want string }{
		{"0c:0e:41:8a:00:01", "fe80::e0e:41ff:fe8a:1"},
		{"00:00:00:00:00:01", "fe80::200:ff:fe00:1"},
		// The universal/local bit is inverted, not set: an address that
		// already has it set comes back with it clear.
		{"02:00:00:00:00:01", "fe80::ff:fe00:1"},
		// Already an EUI-64: used as-is, with the same bit inverted and no
		// ff:fe inserted.
		{"0c:0e:41:ff:fe:8a:00:01", "fe80::e0e:41ff:fe8a:1"},
	} {
		got, err := linkLocalFor(mac(t, tc.hw))
		if err != nil {
			t.Errorf("%s: %v", tc.hw, err)
			continue
		}
		want := netip.MustParseAddr(tc.want)
		if got != want {
			t.Errorf("%s: got %s, want %s", tc.hw, got, want)
		}
		if !got.IsLinkLocalUnicast() {
			t.Errorf("%s: derived %s is not link-local", tc.hw, got)
		}
	}
}

// Every hardware address that cannot yield a unique link-local is refused
// rather than approximated. Two nodes handed the same fe80:: on one link is a
// worse outcome than an interface left as it was found.
func TestLinkLocalForRefusesUnusableHardwareAddrs(t *testing.T) {
	for name, hw := range map[string]net.HardwareAddr{
		"no hardware address": nil,
		"all zero":            mac(t, "00:00:00:00:00:00"),
		"group bit set":       mac(t, "01:00:5e:00:00:01"),
		"wrong length":        net.HardwareAddr{0x01, 0x02, 0x03},
	} {
		if got, err := linkLocalFor(hw); err == nil {
			t.Errorf("%s: derived %s instead of refusing", name, got)
		}
	}
}

// assignsV6 is the gate on both assertions, and what it gates on matters:
// forwarding must not be turned on under an interface whose own address comes
// from a router advertisement, because enabling forwarding changes how the
// kernel reads the advertisements it is relying on.
func TestAssignsV6GatesOnStaticGlobalIPv6(t *testing.T) {
	pfx := func(s string) netip.Prefix { return netip.MustParsePrefix(s) }
	for _, tc := range []struct {
		name string
		spec Spec
		want bool
	}{
		{"static global v6", Spec{Addrs: []netip.Prefix{pfx("fd01::1/64")}}, true},
		{"explicit static mode", Spec{Mode6: ModeStatic, Addrs: []netip.Prefix{pfx("fd01::1/64")}}, true},
		{"v4 only", Spec{Addrs: []netip.Prefix{pfx("10.1.1.1/24")}}, false},
		{"no addresses at all", Spec{}, false},
		{"slaac", Spec{Mode6: ModeSLAAC}, false},
		{"dhcp6", Spec{Mode6: ModeDHCP6}, false},
		// A SLAAC interface carrying a stale static address in the record is
		// still not ours to make a router: the mode is what decides.
		{"slaac with a leftover address", Spec{Mode6: ModeSLAAC, Addrs: []netip.Prefix{pfx("fd01::1/64")}}, false},
		// A link-local is not a global address and does not by itself mean
		// the interface was given routed IPv6 — and it would not reach
		// Validate anyway.
		{"link-local only", Spec{Addrs: []netip.Prefix{pfx("fe80::1/64")}}, false},
	} {
		if got := tc.spec.assignsV6(); got != tc.want {
			t.Errorf("%s: assignsV6() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The exact case that prompted this: an interface carrying a global /64 and
// no fe80:: reads as needing one, while the same interface with a link-local
// present does not. A link-local of the wrong family, or an IPv4-mapped
// address that happens to look link-local, must not count.
func TestAnyLinkLocal6(t *testing.T) {
	addrs := func(cidrs ...string) []net.Addr {
		var out []net.Addr
		for _, c := range cidrs {
			ip, n, err := net.ParseCIDR(c)
			if err != nil {
				t.Fatalf("bad test CIDR %q: %v", c, err)
			}
			out = append(out, &net.IPNet{IP: ip, Mask: n.Mask})
		}
		return out
	}
	for _, tc := range []struct {
		name string
		in   []net.Addr
		want bool
	}{
		{"the reported interface", addrs("10.1.1.1/24", "fd01::1/64"), false},
		{"a healthy interface", addrs("10.1.1.1/24", "fd01::1/64", "fe80::e0e:41ff:fe8a:1/64"), true},
		{"link-local only", addrs("fe80::1/64"), true},
		{"nothing at all", nil, false},
		// 169.254.0.0/16 is IPv4 link-local. It is not what is being asked
		// about, and an interface carrying one still cannot source an RA.
		{"IPv4 link-local", addrs("169.254.1.1/16"), false},
	} {
		if got := anyLinkLocal6(tc.in); got != tc.want {
			t.Errorf("%s: anyLinkLocal6() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// `ip_forwarding: false` has to reach this package too. gravinet's forwarding
// story is host-level and opt-outable; an operator who declined it and then
// edited an address must not find the per-interface knob turned on by a code
// path that never mentions forwarding.
func TestSetForwarding6Default(t *testing.T) {
	if !forward6.Load() {
		t.Error("the default must be on, matching config.ForwardingEnabled for an unset field")
	}
	t.Cleanup(func() { SetForwarding6(true) })
	SetForwarding6(false)
	if forward6.Load() {
		t.Error("opting out did not reach this package")
	}
	SetForwarding6(true)
	if !forward6.Load() {
		t.Error("opting back in did not reach this package")
	}
}
