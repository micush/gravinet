//go:build linux

package tun

// Physical default-gateway detection. This is the foundation the mesh's
// full-tunnel feature builds on (see internal/mesh/routes.go's special-casing
// of an accepted 0.0.0.0/0 or ::/0): before a peer-bypass host route can be
// installed via "the real gateway," something has to know what that gateway
// currently is. Reads the kernel's route table directly via rtnetlink — an
// RTM_GETROUTE dump, the read counterpart to route_linux.go's
// RTM_NEWROUTE/RTM_DELROUTE writes — rather than shelling out to `ip route`.
//
// Once gravinet's own full-tunnel default is installed, the kernel will
// happily report *two* default routes: the original physical one and
// gravinet's own "dev <tun>" one. Asking "what route would the kernel use
// for destination X" (a single RTM_GETROUTE lookup — the way `ip route get`
// works) can't tell them apart once that's happened: by then gravinet's own
// entry is the best match, so that query would just report itself back.
// DefaultGateway instead dumps every default-route entry in the main table
// and discards any whose outgoing interface is the tun device gravinet
// itself manages, keeping the lowest-metric survivor — the real, independent
// physical default, findable the same way whether or not gravinet has
// already installed its own.

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"syscall"
)

// GatewaySupported is true here: this is the real, rtnetlink-based
// implementation. False in gateway_unsupported.go on platforms that don't
// have one yet — see internal/mesh/routes.go's syncFullTunnelRoute, which
// checks this before ever installing the default route: the default route
// alone (a plain on-link route, working on every platform via the ordinary
// AddRoute) is the *dangerous* half of full-tunnel; the peer/seed bypass
// routes this flag gates are the safety net, and the two must never be
// allowed to activate independently of each other.
const GatewaySupported = true

// RouteDemotionNeeded is true here, same as every other platform gravinet
// has a real gateway backend for. Not because Linux actually needs it — the
// kernel is perfectly happy to keep two default routes at different
// metrics and prefer the lower one, which is exactly what let
// syncFullTunnelRoute simply install its own route "alongside" the
// physical one right up through v316. But from v317 on, every platform
// goes through the same demote-then-install sequence rather than Linux
// being the one platform that skips a step the others can't: one code path
// in internal/mesh to reason about, one behavior to document, and the
// physical default route ends up looking the same way after activation
// everywhere gravinet runs — deprioritized to a very high metric rather
// than left at whatever it started at, whether or not the kernel actually
// required that. See DemoteDefaultRoute below for how this is done for
// real on Linux specifically.
const RouteDemotionNeeded = true

// DemoteDefaultRoute reprograms the current physical (non-tunnel) default
// route's metric to newMetric, using the same selection DefaultGateway
// performs (lowest-metric physical candidate, excludeIfIndex ignored the
// same way) to decide which route that is. Returns the metric it had
// before the change, so a caller can restore it later.
//
// Unlike the BSD/Windows platforms this mirrors, Linux's kernel doesn't
// actually need the physical route moved out of the way for gravinet's own
// default route to install and win — see RouteDemotionNeeded's doc comment
// for why this exists here anyway. That has one real consequence for how
// it's implemented: on Linux, a route's kernel identity includes its
// metric (RTA_PRIORITY) as part of the key, the same way route_linux.go's
// DelRoute doc comment already describes for gravinet's own routes — so an
// RTM_NEWROUTE|NLM_F_REPLACE at a different metric doesn't update the
// existing entry in place, it adds a second, independent one alongside it.
// Actually changing the metric is therefore add-the-new-one,
// delete-the-old-one, not a single "change" call the way
// gateway_freebsd.go's RTM_CHANGE (a genuinely different, in-place
// operation FreeBSD's rt_msghdr supports and Linux's rtnetlink doesn't) is.
// Deliberately in that order — add before delete — so a failure or crash
// between the two steps never leaves this host with zero physical default
// routes, even for an instant: at every point either the original, the
// demoted copy, or both are present, never neither.
func DemoteDefaultRoute(family int, excludeIfIndex int32, newMetric int) (int, error) {
	routes, err := dumpDefaultRoutes(family)
	if err != nil {
		return 0, err
	}
	var best *rtEntry
	for i := range routes {
		r := &routes[i]
		if r.oif == excludeIfIndex {
			continue
		}
		if !r.gateway.IsValid() {
			continue
		}
		if best == nil || r.metric < best.metric {
			best = r
		}
	}
	if best == nil {
		return 0, fmt.Errorf("no physical default route found to demote (family %#x)", family)
	}
	if best.metric == newMetric {
		return best.metric, nil // already at the target metric; nothing to do
	}

	var dst []byte
	if family == syscall.AF_INET {
		dst = make([]byte, 4)
	} else {
		dst = make([]byte, 16)
	}
	var gw []byte
	if best.gateway.Is4() {
		b := best.gateway.As4()
		gw = b[:]
	} else {
		b := best.gateway.As16()
		gw = b[:]
	}

	if err := sendRouteMsg(syscall.RTM_NEWROUTE, syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE,
		family, dst, 0, syscall.RT_TABLE_MAIN, syscall.RT_SCOPE_UNIVERSE, best.oif, gw, newMetric); err != nil {
		return 0, fmt.Errorf("install demoted-metric default route: %w", err)
	}
	if err := sendRouteMsg(syscall.RTM_DELROUTE, 0,
		family, dst, 0, syscall.RT_TABLE_MAIN, syscall.RT_SCOPE_UNIVERSE, best.oif, gw, best.metric); err != nil {
		// Not fatal: the original route lingering alongside the new
		// demoted-metric copy just leaves two default routes at different
		// metrics rather than one — untidy, but the lower-metric one (the
		// new copy, then shortly gravinet's own literal /0) still wins
		// either way, so traffic still routes correctly.
		return best.metric, nil
	}
	return best.metric, nil
}

// DefaultGateway returns the best physical (non-tunnel) default route for
// family (syscall.AF_INET or syscall.AF_INET6), ignoring any default route
// whose outgoing interface is excludeIfIndex — pass the tun device's own
// ifindex there so gravinet never mistakes its own installed default for the
// physical one; pass 0 (never a real ifindex) to exclude nothing, e.g. when
// checking before any tun-routed default has been installed at all. Returns
// an error if no physical default route exists (genuinely offline, or the
// only default present is gravinet's own).
func DefaultGateway(family int, excludeIfIndex int32) (Gateway, error) {
	routes, err := dumpDefaultRoutes(family)
	if err != nil {
		return Gateway{}, err
	}
	var best Gateway
	found := false
	for _, r := range routes {
		if r.oif == excludeIfIndex {
			continue
		}
		if !r.gateway.IsValid() {
			continue // an on-link default with no gateway isn't useful here
		}
		if !found || r.metric < best.Metric {
			best = Gateway{Addr: r.gateway, IfIndex: r.oif, Metric: r.metric}
			found = true
		}
	}
	if !found {
		return Gateway{}, fmt.Errorf("no physical default route found (family %#x)", family)
	}
	return best, nil
}

// rtEntry is one parsed RTM_NEWROUTE dump entry, holding just the fields
// DefaultGateway needs.
type rtEntry struct {
	oif     int32
	gateway netip.Addr
	metric  int
}

// dumpDefaultRoutes issues an RTM_GETROUTE|NLM_F_DUMP request for family and
// returns every default-route (dst-len 0) entry in the main table, following
// the kernel's multi-message dump convention: keep reading datagrams until
// NLMSG_DONE, since a full table dump rarely fits in one recvfrom.
func dumpDefaultRoutes(family int) ([]rtEntry, error) {
	// SOCK_CLOEXEC: this fd is always closed (defer) well before returning,
	// but a concurrent exec.Command on another goroutine could still fork+exec
	// in the brief window it's open and inherit it otherwise — see
	// tun_linux.go's New for the concrete bug this class of leak caused with
	// the (long-lived) tun device fd.
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, err
	}

	rtm := make([]byte, syscall.SizeofRtMsg)
	rtm[0] = byte(family) // rtm_family; rest zeroed (dst_len 0 = "dump everything", filtered below)

	total := syscall.SizeofNlMsghdr + len(rtm)
	msg := make([]byte, syscall.SizeofNlMsghdr)
	binary.NativeEndian.PutUint32(msg[0:4], uint32(total))
	binary.NativeEndian.PutUint16(msg[4:6], uint16(syscall.RTM_GETROUTE))
	binary.NativeEndian.PutUint16(msg[6:8], uint16(syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP))
	binary.NativeEndian.PutUint32(msg[8:12], 1) // seq
	msg = append(msg, rtm...)

	if err := syscall.Sendto(fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, err
	}

	var out []rtEntry
	buf := make([]byte, 65536)
readLoop:
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, err
		}
		msgs := buf[:n]
		for len(msgs) >= syscall.SizeofNlMsghdr {
			msgLen := binary.NativeEndian.Uint32(msgs[0:4])
			msgType := binary.NativeEndian.Uint16(msgs[4:6])
			if msgLen < syscall.SizeofNlMsghdr || int(msgLen) > len(msgs) {
				break readLoop // malformed; stop rather than misparse
			}
			switch msgType {
			case syscall.NLMSG_DONE:
				break readLoop
			case syscall.NLMSG_ERROR:
				errno := int32(binary.NativeEndian.Uint32(msgs[syscall.SizeofNlMsghdr : syscall.SizeofNlMsghdr+4]))
				if errno != 0 {
					return nil, fmt.Errorf("rtnetlink route dump: %w", syscall.Errno(-errno))
				}
			case syscall.RTM_NEWROUTE:
				if e, ok := parseDefaultRoute(msgs[:msgLen]); ok {
					out = append(out, e)
				}
			}
			msgs = msgs[rtaAlign(int(msgLen)):]
		}
	}
	return out, nil
}

// parseDefaultRoute extracts oif/gateway/metric from one RTM_NEWROUTE dump
// message, if it's a main-table, default (dst-len 0) entry — every other
// entry (specific prefixes, other tables) is reported as !ok so the caller
// skips it without needing its own filtering logic.
func parseDefaultRoute(m []byte) (rtEntry, bool) {
	if len(m) < syscall.SizeofNlMsghdr+syscall.SizeofRtMsg {
		return rtEntry{}, false
	}
	rtm := m[syscall.SizeofNlMsghdr:]
	family := rtm[0]
	dstLen := rtm[1]
	table := rtm[4]
	if dstLen != 0 || table != syscall.RT_TABLE_MAIN {
		return rtEntry{}, false
	}

	var e rtEntry
	attrs := rtm[syscall.SizeofRtMsg:]
	for len(attrs) >= syscall.SizeofRtAttr {
		alen := binary.NativeEndian.Uint16(attrs[0:2])
		atyp := binary.NativeEndian.Uint16(attrs[2:4])
		if alen < syscall.SizeofRtAttr || int(alen) > len(attrs) {
			break // malformed attribute; stop parsing this message
		}
		data := attrs[syscall.SizeofRtAttr:alen]
		switch atyp {
		case syscall.RTA_OIF:
			if len(data) >= 4 {
				e.oif = int32(binary.NativeEndian.Uint32(data))
			}
		case syscall.RTA_GATEWAY:
			switch {
			case family == syscall.AF_INET && len(data) == 4:
				e.gateway = netip.AddrFrom4([4]byte(data))
			case family == syscall.AF_INET6 && len(data) == 16:
				e.gateway = netip.AddrFrom16([16]byte(data))
			}
		case syscall.RTA_PRIORITY:
			if len(data) >= 4 {
				e.metric = int(binary.NativeEndian.Uint32(data))
			}
		}
		attrs = attrs[rtaAlign(int(alen)):]
	}
	return e, true
}
