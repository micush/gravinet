package hostnet

import "fmt"

// Addressing mode: where an interface's addresses come from, per family.
//
// Per family because that is how every backend expresses it and how operators
// actually deploy: a static IPv4 address alongside SLAAC IPv6 on the same NIC
// is ordinary, not a corner case. A single per-interface mode would have been
// less code here and unable to describe the common configuration.
//
// The IPv6 trio is not arbitrary. DHCPv6 and SLAAC differ in where the
// *address* comes from, not in whether router advertisements are honoured: a
// v6 host takes its default route from the RA either way, and a DHCPv6 server
// does not supply one. So both accept RAs and differ only in whether an
// address is derived from the advertised prefix — which on Linux is exactly
// the accept_ra/autoconf sysctl pair, and is why the split lands cleanly on
// every platform rather than only on the one it was designed against.
type Mode string

const (
	// ModeStatic is addresses this configuration names, and nothing else.
	ModeStatic Mode = "static"
	// ModeDHCP is IPv4 from a DHCP client. IPv4 only: there is no IPv4
	// equivalent of an RA, so this is the only non-static IPv4 answer.
	ModeDHCP Mode = "dhcp"
	// ModeDHCP6 is IPv6 addresses from DHCPv6, with the default route still
	// from the RA.
	ModeDHCP6 Mode = "dhcp6"
	// ModeSLAAC is IPv6 addresses derived from the advertised prefix.
	ModeSLAAC Mode = "slaac"
)

// Or supplies a default for an unset mode.
//
// Empty reads as static, and that is a compatibility rule rather than a
// preference: `host_interfaces` records written before this release exist only
// because an operator set a static address through gravinet, so reading an
// absent mode as static is what keeps those records meaning what they meant.
// A config restored from a v871 backup therefore behaves identically.
func (m Mode) Or(d Mode) Mode {
	if m == "" {
		return d
	}
	return m
}

// IsStatic reports whether this family's addresses are gravinet's to manage.
// Only a static family has an intended address set, is pruned to match one, or
// may carry a gateway from this configuration.
func (m Mode) IsStatic() bool { return m.Or(ModeStatic) == ModeStatic }

// AcceptsRA reports whether router advertisements should be honoured. True for
// both non-static IPv6 modes: see the type comment.
func (m Mode) AcceptsRA() bool {
	e := m.Or(ModeStatic)
	return e == ModeSLAAC || e == ModeDHCP6
}

// Autoconf reports whether an address should be derived from the advertised
// prefix. SLAAC only — under DHCPv6 the RA is read for the route and the
// address comes from the server.
func (m Mode) Autoconf() bool { return m.Or(ModeStatic) == ModeSLAAC }

// ValidMode4 checks an IPv4 mode. The families have disjoint vocabularies on
// purpose: "slaac" as an IPv4 mode is not a typo to be tolerated, it is a
// misunderstanding worth reporting.
func ValidMode4(m Mode) error {
	switch m {
	case "", ModeStatic, ModeDHCP:
		return nil
	case ModeDHCP6, ModeSLAAC:
		return fmt.Errorf("%q is an IPv6 mode; IPv4 is static or dhcp", string(m))
	}
	return fmt.Errorf("unknown IPv4 addressing mode %q: want static or dhcp", string(m))
}

// ValidMode6 checks an IPv6 mode.
func ValidMode6(m Mode) error {
	switch m {
	case "", ModeStatic, ModeDHCP6, ModeSLAAC:
		return nil
	case ModeDHCP:
		return fmt.Errorf("%q is the IPv4 mode; IPv6 DHCP is spelled dhcp6", string(m))
	}
	return fmt.Errorf("unknown IPv6 addressing mode %q: want static, dhcp6 or slaac", string(m))
}

// errNoDHCPv6 is returned by the backends on platforms whose base system has
// no DHCPv6 client. Refused rather than quietly downgraded to SLAAC: an
// operator who picked DHCPv6 would get addresses, from the wrong place, and
// nothing would say so.
func errNoDHCPv6(platform, alternative string) error {
	return fmt.Errorf("%s has no DHCPv6 client in the base system, so gravinet cannot configure dhcp6 on this host; %s", platform, alternative)
}
