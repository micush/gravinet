package hostnet

import (
	"fmt"
	"net"
	"net/netip"
)

// GlobalAddrs lists the interface's routable addresses — the set an edit is
// allowed to change. Link-local and loopback are kernel-managed and excluded,
// so submitting an edited list can never strip fe80::.
func GlobalAddrs(ifName string) ([]netip.Prefix, error) {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if addr.IsLinkLocalUnicast() || addr.IsLoopback() || addr.IsInterfaceLocalMulticast() {
			continue
		}
		ones, _ := ipn.Mask.Size()
		out = append(out, netip.PrefixFrom(addr, ones))
	}
	return out, nil
}

// currentMTU reads an interface's MTU, so an unchanged value is not written
// again — writing it is harmless on most drivers and bounces the link on
// some, which is not a thing to do for no reason.
func currentMTU(ifName string) (int, error) {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return 0, err
	}
	return ifi.MTU, nil
}

// Apply makes an interface's live global addresses and default gateways match
// the spec, and returns what changed. The counterpart to Persist: this is the
// running system, that is the boot-time configuration, and a change normally
// wants both.
//
// Additions happen before removals. Re-addressing an interface then leaves
// the old address up until the new one is in place; the other order drops the
// interface's only address for the duration, which on the link carrying the
// request is the difference between a change and an outage.
//
// Only global addresses are touched. Link-local and loopback are
// kernel-managed and are neither reported nor removed — a spec that omits
// fe80:: has not asked for it to go.
//
// Removal happens only when Prune is set. See its doc comment: the difference
// between "this is the whole set" and "these must be present" is the
// difference between an editor and a reconciler, and getting it wrong strips
// addresses on every reload.
func Apply(s Spec) (added, removed int, err error) {
	if !safeIface(s.Iface) {
		return 0, 0, fmt.Errorf("refusing to configure interface name %q", s.Iface)
	}
	want := map[netip.Prefix]bool{}
	for _, p := range s.Addrs {
		want[netip.PrefixFrom(p.Addr().Unmap(), p.Bits())] = true
	}
	have, err := GlobalAddrs(s.Iface)
	if err != nil {
		return 0, 0, err
	}
	haveSet := map[netip.Prefix]bool{}
	for _, p := range have {
		haveSet[p] = true
	}

	for p := range want {
		if haveSet[p] {
			continue
		}
		if err := addAddr(s.Iface, p); err != nil {
			return added, removed, fmt.Errorf("adding %s: %w", p, err)
		}
		added++
	}
	if s.Prune {
		for _, p := range have {
			if want[p] {
				continue
			}
			if err := delAddr(s.Iface, p); err != nil {
				return added, removed, fmt.Errorf("removing %s: %w", p, err)
			}
			removed++
		}
	}
	// MTU before addresses would be tidier, but an MTU change can bounce the
	// link on some drivers, and doing that before the new address is on the
	// interface would drop the very session carrying the request.
	if s.MTU > 0 {
		if cur, err := currentMTU(s.Iface); err != nil || cur != s.MTU {
			if err := setMTU(s.Iface, s.MTU); err != nil {
				return added, removed, fmt.Errorf("setting MTU %d: %w", s.MTU, err)
			}
		}
	}
	for _, gw := range []netip.Addr{s.GW4, s.GW6} {
		if !gw.IsValid() {
			continue
		}
		if err := setGateway(gw, s.Iface); err != nil {
			return added, removed, fmt.Errorf("setting default gateway %s: %w", gw, err)
		}
	}
	return added, removed, nil
}
