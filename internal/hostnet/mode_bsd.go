//go:build freebsd || openbsd

package hostnet

import (
	"fmt"
	"os/exec"
	"strings"
)

// applyMode on the BSDs, through ifconfig(8) — the same route the rest of this
// package's live changes take here.
//
// IPv4 has nothing to do live, for the same reason as on Linux: a DHCP lease
// needs a client daemon, which is the backend's to run.
//
// IPv6 has one knob rather than Linux's two. accept_rtadv turns on RA handling
// and prefix autoconfiguration together, so the kernel cannot be told to read
// an advertisement for its route while declining to derive an address from it.
// That distinction is what separates dhcp6 from slaac, and it is unavailable
// here — which is consistent, because neither BSD ships a DHCPv6 client for the
// other half of dhcp6 either, and the persist backends refuse that mode
// outright rather than quietly giving the operator SLAAC.
func applyMode(s Spec) error {
	verb := "-accept_rtadv"
	if s.Mode6.AcceptsRA() {
		verb = "accept_rtadv"
	}
	out, err := exec.Command("ifconfig", s.Iface, "inet6", verb).CombinedOutput()
	if err != nil {
		// An interface with no IPv6 at all rejects this, and that is not a
		// reason to fail an edit whose IPv4 half is sound.
		if strings.Contains(strings.ToLower(string(out)), "address family not supported") {
			return nil
		}
		return fmt.Errorf("ifconfig %s inet6 %s: %v (%s)", s.Iface, verb, err, strings.TrimSpace(string(out)))
	}
	return nil
}
