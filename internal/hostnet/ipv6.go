package hostnet

import (
	"fmt"
	"net"
	"net/netip"
)

// Making an interface that gravinet has just addressed actually usable for
// IPv6 on a router. Two things do not come with the address:
//
//   - A link-local address. Normally the kernel makes one when the interface
//     comes up, but it does not always: addr_gen_mode set to none, or IPv6
//     disabled on the interface at the moment it appeared, both leave an
//     interface that carries a perfectly good global /64 and no fe80::. That
//     interface cannot source a router advertisement (RFC 4861 §6.1.2), cannot
//     be a next hop for a neighbour, and cannot answer neighbour solicitation
//     the way anything on the link expects.
//
//   - Forwarding. gravinet turns IPv6 forwarding on host-wide at startup, next
//     to the IPv4 knob, and on Linux that write does propagate to the default
//     and to every interface. What it cannot cover is an interface whose own
//     forwarding knob is set afterwards — by a sysctl.d drop-in applied when
//     the link comes up, or by a network manager that writes per-interface
//     settings of its own. The per-interface value wins, and a router
//     interface with forwarding off is a black hole that advertises itself.
//
// Both are asserted only when gravinet is itself assigning a global IPv6
// address to the interface. That is the point at which the operator has said
// this interface carries routed IPv6, and it is the only point at which
// turning forwarding on for one interface is unambiguously what they meant.

// linkLocalFor derives an interface's link-local address from its hardware
// address by the modified EUI-64 rule (RFC 4291 appendix A, RFC 2464 §4).
//
// Deriving rather than picking something arbitrary like fe80::1 is what makes
// this safe to do automatically. This is the exact address the kernel would
// have generated itself under the default addr_gen_mode, so it is stable
// across reboots, identical on both sides of a restore, and unique on the link
// for the same reason a MAC is. If the kernel later does generate its own —
// someone clears addr_gen_mode and bounces the link — it generates this one,
// and the interface ends up with the address it already had rather than a
// second one.
func linkLocalFor(hw net.HardwareAddr) (netip.Addr, error) {
	var eui [8]byte
	switch len(hw) {
	case 6: // EUI-48: ff:fe goes in the middle
		copy(eui[0:3], hw[0:3])
		eui[3], eui[4] = 0xff, 0xfe
		copy(eui[5:8], hw[3:6])
	case 8: // already an EUI-64
		copy(eui[:], hw)
	default:
		// Interfaces with no hardware address, or one of a length this rule
		// does not describe — tunnels, some point-to-point devices. There is
		// no derivation to make, and inventing one risks handing two nodes
		// the same link-local, which is worse than leaving the interface as
		// it is.
		return netip.Addr{}, fmt.Errorf("interface has no EUI-48 or EUI-64 hardware address to derive a link-local from")
	}
	// A group-bit address is a multicast or broadcast MAC and was never this
	// interface's own; an all-zero one is the placeholder several virtual
	// devices carry, and would give every such interface fe80::200:ff:fe00:0.
	if hw[0]&0x01 != 0 {
		return netip.Addr{}, fmt.Errorf("interface hardware address %s is a group address", hw)
	}
	if allZero(hw) {
		return netip.Addr{}, fmt.Errorf("interface has an all-zero hardware address")
	}
	// The "modified" in modified EUI-64: invert the universal/local bit.
	eui[0] ^= 0x02

	var b [16]byte
	b[0], b[1] = 0xfe, 0x80
	copy(b[8:], eui[:])
	return netip.AddrFrom16(b), nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// hasLinkLocal6 reports whether the interface already carries an IPv6
// link-local address. An interface whose addresses cannot be read is reported
// as having one, so a failed lookup does not become a reason to add an
// address to an interface that may already have one.
func hasLinkLocal6(ifi *net.Interface) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return true
	}
	return anyLinkLocal6(addrs)
}

// anyLinkLocal6 is the predicate itself, split out from the lookup so it can
// be tested against address lists a test can construct — a host running the
// suite may have no IPv6 at all, and there is no portable way to arrange an
// interface that has a global address and no link-local, which is the case
// that matters.
func anyLinkLocal6(addrs []net.Addr) bool {
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		if addr.Is6() && !addr.Is4In6() && addr.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// assignsV6 reports whether this spec puts a global IPv6 address on the
// interface itself. False for a SLAAC or DHCPv6 family, where the address
// comes from the network: gravinet has not been asked to make that interface
// a router, and turning forwarding on under it would change how the kernel
// reads the very advertisements it is relying on for its own address.
func (s Spec) assignsV6() bool {
	if !s.Mode6.Or(ModeStatic).IsStatic() {
		return false
	}
	for _, p := range s.Addrs {
		a := p.Addr().Unmap()
		if a.Is6() && !a.Is4In6() && !a.IsLinkLocalUnicast() && !a.IsLoopback() {
			return true
		}
	}
	return false
}
