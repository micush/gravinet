//go:build linux

package hostnet

import (
	"net"
	"net/netip"

	"gravinet/internal/tun"
)

// ensureLinkLocal6 gives the interface a link-local address if it has none,
// and reports whether it added one. Adding is over rtnetlink, the same path
// every other address on this interface takes.
//
// Best-effort by design, and the caller does not fail an address edit on it.
// The operator asked for a global address and got one; a link-local that
// could not be derived — a device with no usable hardware address — is not a
// reason to reject the edit that succeeded. The condition does not go
// unreported either way: the IPv6 RA preflight names an interface with no
// link-local wherever it matters, which is the page where the consequence
// actually shows up.
func ensureLinkLocal6(ifName string) (bool, error) {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return false, err
	}
	if hasLinkLocal6(ifi) {
		return false, nil
	}
	ll, err := linkLocalFor(ifi.HardwareAddr)
	if err != nil {
		return false, err
	}
	// /64 is the only prefix length a link-local is configured with; the
	// kernel narrows the address's scope on its own from the fe80:: prefix,
	// which is why AddAddr can send the same RT_SCOPE_UNIVERSE it sends for
	// a global address.
	if err := tun.AddAddr(ifName, netip.PrefixFrom(ll, 64)); err != nil {
		return false, err
	}
	return true, nil
}

// ensureForwarding6 turns on IPv6 forwarding for one interface.
//
// Only ever on. An interface whose forwarding is off might have been turned
// off deliberately, but gravinet has no record of having done it and no way
// to tell that from a default — so this asserts the setting it needs and
// never clears one it did not set.
//
// Goes through writeIfaceSysctl6, which is the hardened path: it refuses an
// interface name that is not a single path component, and treats a missing
// knob as "no IPv6 on this interface" rather than as a failure.
func ensureForwarding6(ifName string) error {
	return writeIfaceSysctl6(ifName, "forwarding", "1")
}
