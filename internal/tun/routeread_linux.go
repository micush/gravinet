//go:build linux

package tun

// Route *reading* is the counterpart to the route installation above: it dumps
// the kernel's routing table so gravinet can discover prefixes some other
// routing daemon (FRR/BGP, OSPF, a hand-written static route) has pointed at a
// mesh peer, and forward them without the operator having to restate those
// prefixes to gravinet.
//
// This exists because a TUN carries no next-hop. When the kernel decides
// "10.3.3.0/24 via 10.255.255.248 dev mesh0" and hands the packet over, the
// data plane sees only the destination address; the gateway that made the
// decision — which is exactly the peer's overlay address, the one piece of
// information needed to pick a session — is discarded at the device boundary.
// Reading it back out of the routing table is the only way to recover it.

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"syscall"
)

// OSRoute is one kernel route whose traffic leaves via a given interface, with
// the gateway the kernel would have used. Only routes that have both a
// destination prefix and a resolvable gateway are reported: a route with no
// gateway is on-link and tells us nothing about which peer owns it.
type OSRoute struct {
	Prefix  netip.Prefix
	Gateway netip.Addr
}

// rtaNhID is RTA_NH_ID, the attribute carrying a nexthop-object id. Not in
// Go's syscall package (it postdates it), so it's named here.
//
// This attribute is the whole reason this file is more than a simple dump
// parser. Modern FRR installs routes referencing a *nexthop object* rather
// than embedding the gateway in the route itself, so the route arrives with
// RTA_NH_ID set and RTA_GATEWAY absent entirely — `ip route` renders it as
// "nhid 81 via 10.255.255.248", which reads as though the gateway were right
// there in the route when it is not. A parser that only looked for
// RTA_GATEWAY would silently see no gateway on precisely the routes a BGP
// daemon installs, which is the case this whole feature is for.
const rtaNhID = 27

// rtmGetNextHop / nhaID / nhaGateway are the nexthop-object dump message and
// its attributes (RTM_GETNEXTHOP, NHA_ID, NHA_GATEWAY), also absent from
// syscall.
const (
	rtmGetNextHop = 108
	nhaID         = 1
	nhaGateway    = 6
)

// ListRoutesVia returns every kernel route for the given address family whose
// output interface is ifIndex and which has a usable gateway, resolving
// nexthop objects as needed.
//
// Errors are returned rather than swallowed, but callers are expected to treat
// a failure as "no routes learned this cycle" and carry on: this is an
// enrichment path, and a mesh that forwards only what it was explicitly told
// is strictly better than one that stops forwarding because a netlink dump
// failed.
func ListRoutesVia(family int, ifIndex int32) ([]OSRoute, error) {
	routes, err := dumpRoutes(family)
	if err != nil {
		return nil, err
	}
	var need bool
	for _, r := range routes {
		if r.oif == ifIndex && !r.gw.IsValid() && r.nhID != 0 {
			need = true
			break
		}
	}
	// Only pay for the second dump when a route actually referenced a nexthop
	// object — on a box whose routes carry their gateway inline this is pure
	// overhead, and it runs on a timer.
	var nh map[uint32]netip.Addr
	if need {
		if nh, err = dumpNextHops(family); err != nil {
			return nil, err
		}
	}

	var out []OSRoute
	for _, r := range routes {
		if r.oif != ifIndex || !r.dst.IsValid() {
			continue
		}
		gw := r.gw
		if !gw.IsValid() && r.nhID != 0 {
			gw = nh[r.nhID]
		}
		if !gw.IsValid() {
			continue // on-link, or a nexthop object we couldn't resolve
		}
		out = append(out, OSRoute{Prefix: r.dst, Gateway: gw})
	}
	return out, nil
}

type rawRoute struct {
	dst  netip.Prefix
	gw   netip.Addr
	oif  int32
	nhID uint32
}

func dumpRoutes(family int) ([]rawRoute, error) {
	rtm := make([]byte, syscall.SizeofRtMsg)
	rtm[0] = byte(family)
	msgs, err := netlinkDump(syscall.RTM_GETROUTE, rtm)
	if err != nil {
		return nil, err
	}
	var out []rawRoute
	for _, m := range msgs {
		if len(m) < syscall.SizeofRtMsg {
			continue
		}
		fam, dstLen, table, typ := int(m[0]), int(m[1]), m[4], m[7]
		if fam != family || typ != syscall.RTN_UNICAST {
			continue
		}
		// Skip the local and broadcast tables: they describe this host's own
		// addresses, never a path to a peer, and including them would let a
		// local address masquerade as a learned prefix.
		if table == syscall.RT_TABLE_LOCAL {
			continue
		}
		var r rawRoute
		var dstRaw []byte
		forEachAttr(m[syscall.SizeofRtMsg:], func(typ uint16, data []byte) {
			switch typ {
			case syscall.RTA_DST:
				dstRaw = data
			case syscall.RTA_GATEWAY:
				if a, ok := netip.AddrFromSlice(data); ok {
					r.gw = a.Unmap()
				}
			case syscall.RTA_OIF:
				if len(data) >= 4 {
					r.oif = int32(binary.NativeEndian.Uint32(data))
				}
			case rtaNhID:
				if len(data) >= 4 {
					r.nhID = binary.NativeEndian.Uint32(data)
				}
			}
		})
		if dstRaw == nil {
			// No RTA_DST means the default route (0.0.0.0/0 or ::/0). It is
			// deliberately kept here and filtered by the caller, which knows
			// whether a default via a peer is wanted; dropping it silently
			// this far down would hide a real routing decision.
			var zero netip.Addr
			if family == syscall.AF_INET {
				zero = netip.AddrFrom4([4]byte{})
			} else {
				zero = netip.AddrFrom16([16]byte{})
			}
			r.dst = netip.PrefixFrom(zero, 0)
		} else if a, ok := netip.AddrFromSlice(dstRaw); ok {
			r.dst = netip.PrefixFrom(a.Unmap(), dstLen)
		} else {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func dumpNextHops(family int) (map[uint32]netip.Addr, error) {
	// struct nhmsg: family, scope, protocol, resvd, flags — 8 bytes.
	nhm := make([]byte, 8)
	nhm[0] = byte(family)
	msgs, err := netlinkDump(rtmGetNextHop, nhm)
	if err != nil {
		return nil, err
	}
	out := map[uint32]netip.Addr{}
	for _, m := range msgs {
		if len(m) < 8 {
			continue
		}
		var id uint32
		var gw netip.Addr
		forEachAttr(m[8:], func(typ uint16, data []byte) {
			switch typ {
			case nhaID:
				if len(data) >= 4 {
					id = binary.NativeEndian.Uint32(data)
				}
			case nhaGateway:
				if a, ok := netip.AddrFromSlice(data); ok {
					gw = a.Unmap()
				}
			}
		})
		if id != 0 && gw.IsValid() {
			out[id] = gw
		}
	}
	return out, nil
}

// forEachAttr walks a run of rtnetlink attributes, calling fn for each. A
// malformed length stops the walk rather than panicking: this parses whatever
// the kernel hands back, and a truncated attribute must not take the daemon
// down.
func forEachAttr(b []byte, fn func(typ uint16, data []byte)) {
	for len(b) >= syscall.SizeofRtAttr {
		l := int(binary.NativeEndian.Uint16(b[0:2]))
		typ := binary.NativeEndian.Uint16(b[2:4])
		if l < syscall.SizeofRtAttr || l > len(b) {
			return
		}
		fn(typ, b[syscall.SizeofRtAttr:l])
		b = b[rtaAlign(l):]
	}
}

// netlinkDump sends a NLM_F_DUMP request carrying body and returns each
// response message's payload (everything after the nlmsghdr).
func netlinkDump(msgType int, body []byte) ([][]byte, error) {
	s, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	defer syscall.Close(s)
	if err := syscall.Bind(s, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("netlink bind: %w", err)
	}

	total := syscall.SizeofNlMsghdr + len(body)
	req := make([]byte, syscall.SizeofNlMsghdr, total)
	binary.NativeEndian.PutUint32(req[0:4], uint32(total))
	binary.NativeEndian.PutUint16(req[4:6], uint16(msgType))
	binary.NativeEndian.PutUint16(req[6:8], uint16(syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP))
	binary.NativeEndian.PutUint32(req[8:12], 1) // sequence
	req = append(req, body...)

	if err := syscall.Sendto(s, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("netlink send: %w", err)
	}

	var out [][]byte
	buf := make([]byte, 64*1024)
	for {
		n, _, err := syscall.Recvfrom(s, buf, 0)
		if err != nil {
			return nil, fmt.Errorf("netlink recv: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("netlink parse: %w", err)
		}
		for _, m := range msgs {
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return out, nil
			case syscall.NLMSG_ERROR:
				if len(m.Data) >= 4 {
					if e := int32(binary.NativeEndian.Uint32(m.Data[0:4])); e != 0 {
						return nil, fmt.Errorf("netlink error: %w", syscall.Errno(-e))
					}
				}
				return out, nil
			default:
				out = append(out, append([]byte(nil), m.Data...))
			}
		}
	}
}
