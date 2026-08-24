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
	// The interface has to exist before anything is written, not after.
	//
	// Apply always required this — GlobalAddrs below fails on a name the
	// kernel does not know — but it found out after applyMode had run, and on
	// Linux applyMode has written to /proc by then. For an ordinary typo that
	// cost nothing. For the two names the kernel keeps in among the real
	// interfaces it cost a good deal: "all" and "default" are directories in
	// /proc/sys/net/ipv6/conf, so an interface-edit request naming one of them
	// wrote accept_ra and autoconf under it — "all" meaning every interface on
	// the host, "default" meaning every interface created afterwards — and
	// then returned "no such network interface". The operator saw a request
	// that failed and a machine that had quietly stopped accepting router
	// advertisements.
	//
	// safeIface does not catch this and should not be asked to: "all" is a
	// perfectly well-formed interface name, and the reason to refuse it is not
	// its spelling but that there is no such interface. Nor is this only a
	// Linux concern — every platform's applyMode runs before this point, and a
	// name that names nothing is not something to hand any of them.
	//
	// Checking here is behaviour-preserving for every input that used to
	// succeed: all of those name a real interface, or GlobalAddrs would have
	// rejected them anyway. The only difference is that the ones that were
	// always going to fail now fail before the kernel is touched.
	if _, err := net.InterfaceByName(s.Iface); err != nil {
		return 0, 0, fmt.Errorf("no interface named %q on this host", s.Iface)
	}
	// The mode goes first, and for one family it has to. Switching IPv6 to
	// static means turning off RA acceptance and autoconfiguration; doing
	// that after pruning would let the kernel put an autoconfigured address
	// back between the two steps, on an interface that had just been told not
	// to have one.
	if err := applyMode(s); err != nil {
		return 0, 0, err
	}

	want := map[netip.Prefix]bool{}
	for _, p := range s.Addrs {
		want[netip.PrefixFrom(p.Addr().Unmap(), p.Bits())] = true
	}
	// Read the interface after applyMode, not before: on a platform where the
	// mode change itself sets an address, a snapshot taken first would be
	// stale by the time it is compared against.
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
	// Abandoned static addresses go regardless of Prune, because the mode
	// they belonged to no longer exists on this interface. Failing to remove
	// one is not fatal: it may already be gone, and the family it belonged to
	// is no longer gravinet's to account for.
	for _, p := range s.Release {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits())
		if want[p] || !haveSet[p] {
			continue
		}
		if err := delAddr(s.Iface, p); err != nil {
			continue
		}
		removed++
	}
	if s.Prune {
		for _, p := range have {
			if want[p] {
				continue
			}
			// Only a static family has an intended set to prune against.
			// The addresses of a DHCP or SLAAC family come from a client or
			// from a router advertisement, and deleting those on the
			// strength of a list that was never about them would strip the
			// addressing the operator had just asked for.
			if !s.ModeFor(p.Addr()).IsStatic() {
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

// ModeFor picks the mode governing an address's family.
func (s Spec) ModeFor(a netip.Addr) Mode {
	if a.Is4() {
		return s.Mode4.Or(ModeStatic)
	}
	return s.Mode6.Or(ModeStatic)
}

// StaticAddrs filters a prefix list to the families this spec manages
// statically. Used where a caller has the interface's live addresses and needs
// the subset that is gravinet's to record or reassert — a leased or
// autoconfigured address must not be written into the configuration as a
// static one, which is how an interface on DHCP comes back from a restore
// pinned to whatever address it happened to hold.
func (s Spec) StaticAddrs(in []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range in {
		if s.ModeFor(p.Addr()).IsStatic() {
			out = append(out, p)
		}
	}
	return out
}
