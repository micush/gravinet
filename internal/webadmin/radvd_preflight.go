package webadmin

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"

	"gravinet/internal/config"
)

// Preflight: the conditions radvd needs at runtime that nothing else here
// checks.
//
// Everything else on the apply path validates the *configuration* — the
// prefix is a /64, the interface is not the mesh device, the file is ours to
// write, the unit came up. All of it can pass while the host advertises
// nothing, because radvd's own requirements are about the interface rather
// than about the file.
//
// The one that matters is the link-local address. RFC 4861 §6.1.2 requires a
// router advertisement to be sourced from the sending interface's link-local
// address, and radvd implements that literally: get_iface_addrs() returns -1
// when it finds no fe80:: address on the interface, setup_iface() gives up at
// that point, and iface->state_info.ready is never set. An interface that is
// never ready is never sent on — not slowly, never.
//
// What makes it worth code rather than a line in the docs is that radvd says
// nothing. The complaint is gated on IgnoreIfMissing, which defaults to on
// (defaults.h: DFLT_IgnoreIfMissing 1), so it goes to dlog at debug level 4
// and is suppressed at the default level. radvd parses the config, forks,
// drops privileges and runs. `systemctl status` is green, `journalctl -u
// radvd` is clean, /etc/radvd.conf is correct, the page says enabled — and
// tcpdump sees zero packets forever. Every surface an operator would think to
// check agrees that this is working.
//
// So gravinet checks it instead. This does not fix the interface: a
// link-local is the kernel's to create, hostnet treats it as such, and
// HostIface.Validate refuses to let one be configured as a static address.
// Reporting is the whole contribution, and it is enough — the condition is
// trivial to fix once someone knows it is the condition.

// ifaceRAState is what the preflight needs to know about an interface. A
// struct rather than three lookups so the interface is read once, and so the
// tests have one seam to fake instead of a live host to arrange.
type ifaceRAState struct {
	present   bool
	up        bool
	linkLocal bool
}

// lookupIfaceRAState is a var so tests can substitute interfaces that do not
// exist on the machine running them — there is no portable way to produce a
// NIC with a global address and no link-local, which is precisely the case
// worth testing.
var lookupIfaceRAState = liveIfaceRAState

func liveIfaceRAState(name string) ifaceRAState {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return ifaceRAState{}
	}
	st := ifaceRAState{present: true, up: ifi.Flags&net.FlagUp != 0}
	addrs, err := ifi.Addrs()
	if err != nil {
		// The interface is there and the addresses could not be read. Claiming
		// it has no link-local would be inventing a fault out of a failed
		// lookup, so report it as fine and let the daemon be the judge.
		st.linkLocal = true
		return st
	}
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
			st.linkLocal = true
			break
		}
	}
	return st
}

// radvdIfaceProblem reports why an interface will not carry advertisements,
// or "" when nothing is in the way. The order is most-actionable first: an
// interface that is absent or down explains the missing link-local rather
// than being explained by it.
func radvdIfaceProblem(name string) string {
	st := lookupIfaceRAState(name)
	switch {
	case !st.present:
		return fmt.Sprintf("%s: no interface by that name on this host, so nothing is advertised on it", name)
	case !st.up:
		return fmt.Sprintf("%s is down, so nothing is advertised on it", name)
	case !st.linkLocal:
		return fmt.Sprintf("%s has no IPv6 link-local address: a router advertisement has to be sourced from one, "+
			"so radvd will run and advertise nothing, silently (check "+
			"`sysctl net.ipv6.conf.%s.addr_gen_mode net.ipv6.conf.%s.disable_ipv6`)", name, name, name)
	}
	return ""
}

// radvdProblems maps each enabled interface that cannot advertise to the
// reason. Only enabled entries: a parked row is not supposed to be
// advertising, so reporting that it is not would be reporting the request
// back as a fault.
func radvdProblems(c config.RAConfig) map[string]string {
	out := map[string]string{}
	for _, e := range c.EnabledInterfaces() {
		iface := strings.TrimSpace(e.Iface)
		if p := radvdIfaceProblem(iface); p != "" {
			out[iface] = p
		}
	}
	return out
}

// radvdProblemNote renders the preflight for the apply note, or "" when there
// is nothing wrong. Sorted, because a note assembled from a map would
// otherwise reorder itself between two saves that changed nothing.
//
// The trailing "; " is noteworthy's separator convention: it joins its parts
// with nothing and trims one off the end.
func radvdProblemNote(c config.RAConfig) string {
	probs := radvdProblems(c)
	if len(probs) == 0 {
		return ""
	}
	names := make([]string, 0, len(probs))
	for n := range probs {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(probs[n])
		b.WriteString("; ")
	}
	return b.String()
}
