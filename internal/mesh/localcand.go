package mesh

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Local (host) endpoint candidates.
//
// A node's underlay endpoint was historically only ever *observed*: a peer
// recorded the source address a packet arrived from, and gossiped that
// observation onward. Nothing a node knew about its own addresses ever reached
// the mesh. Across the internet that's fine — the observed (server-reflexive)
// address is exactly the one everyone else must dial. Between two nodes behind
// the same NAT it fails completely: every outside observer sees both of them at
// the single shared public address, so the only candidate either one ever learns
// for the other is that public address. Dialing it from inside needs NAT hairpin
// (loopback), which many gateways don't implement — so the handshake fails, the
// seed goes into backoff, and tryRelays quite correctly concludes "direct has
// demonstrably failed" and relays them through a node on the far side of the
// internet. Two machines on the same switch, with no firewall between them,
// end up relaying — while the LAN address that would have connected instantly
// was known only to themselves, because nothing ever advertised it.
//
// Host candidates close that gap the same way ICE does: a node self-declares its
// own interface addresses, they ride the handshake (hsPayload.LocalEndpoints) and
// are re-gossiped by neighbors (peerEntry.localEndpoints), and the receiver adds
// each as an ordinary seed. initLoop's existing per-seed dial/retry/backoff/dedup
// machinery then tries them all in parallel with everything else — no new dial
// path, exactly the argument the extraUDPPorts seeding already makes. A candidate
// that's unreachable (a peer's LAN address seen from outside that LAN) just fails
// its handshake and cools down like any other dead seed; one that works upgrades
// the pair to direct.

// localEndpoints returns this node's current host candidates. This is a pure
// atomic load — no syscall, no lock — because it is called from buildHSInit,
// which runs while planHandshake holds ns.mu, on every single handshake packet
// this node builds.
//
// It used to do the interface enumeration inline, which meant a netlink syscall
// (net.InterfaceAddrs) under a network-wide write lock, once per handshake
// build. That was always wrong, but it stayed survivable only because host
// candidates were being destroyed as fast as they were learned (see v377's
// prune fix), so ns.seeds stayed tiny. The moment candidates began to persist,
// ns.seeds grew to hold every peer's LAN addresses — dozens of entries, most
// unreachable from here and so cycling handshakes forever — and initLoop began
// issuing a syscall per seed per tick with ns.mu held throughout. The engine
// still forwarded packets (the data path takes ns.mu only briefly, and ICMP is
// sparse enough to slip through), but anything needing the lock for longer —
// the web admin's peer listing above all — was starved out. The node pinged
// fine and could not be managed at all. The enumeration now happens in
// refreshLocalCandidates, off the handshake path entirely.
func (e *Engine) localEndpoints() []netip.AddrPort {
	if p := e.localCands.Load(); p != nil {
		return *p
	}
	return nil
}

// refreshLocalCandidates re-enumerates this node's host candidates and
// publishes them for localEndpoints. Called from maintLoop and once at startup
// — never with ns.mu held, since it makes a syscall and takes e.mu (via
// isOverlayAddr), neither of which belongs under a network's write lock.
//
// Filtering is deliberately conservative. Loopback is useless to anyone but us
// and actively harmful to advertise (a peer would dial *its own* loopback and,
// on a shared host in tests, reach the wrong node entirely). Link-local (v4
// 169.254/16, v6 fe80::/10) is only meaningful with a zone/scope this encoding
// doesn't carry, and a peer dialing it would reach whatever holds that address
// on *its* link. Multicast/unspecified aren't endpoints at all. Our own overlay
// addresses are dropped too: a receiver refuses to dial into an overlay subnet
// anyway, so advertising them is pure waste, and they crowd real LAN addresses
// out of the maxLocalEndpoints budget.
//
// Everything else — private LAN ranges, CGNAT, and public addresses alike — is
// advertised. Private ranges are the entire point (they're what makes same-LAN
// pairs work), and a public address here is simply a correct, useful candidate
// for an un-NATed host.
//
// The port is the primary UDP port when UDP is enabled. When UDP is off
// (PrimaryPort 0 — the '-' setting) the addresses are still perfectly dialable
// over the TCP/TLS fallback, so they're advertised at the fallback port instead
// rather than suppressed: a node with UDP disabled has *more* need of a LAN path
// to its same-NAT neighbours, not less, since a relay is its only alternative.
// Only when both are off is there genuinely nothing to offer.
func (e *Engine) refreshLocalCandidates() {
	var out []netip.AddrPort
	port := uint16(e.primaryPort.Load())
	if port == 0 {
		port = uint16(e.fallbackPort.Load()) // UDP off: still reachable over TCP/TLS
	}
	if port != 0 {
		// Enumerate per-interface, not via net.InterfaceAddrs(): that flattens
		// away the interface each address belongs to, and the interface is
		// exactly what distinguishes a real uplink from a virtual bridge this
		// host owns. See virtualBridgeIface.
		ifaces, err := net.Interfaces()
		if err == nil {
			out = make([]netip.AddrPort, 0, 8)
			own := make(map[netip.Addr]bool, 8)
			for _, ifi := range ifaces {
				addrs, err := ifi.Addrs()
				if err != nil {
					continue
				}
				skip := ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 || virtualBridgeIface(ifi.Name)
				for _, a := range addrs {
					ipn, ok := a.(*net.IPNet)
					if !ok {
						continue
					}
					ip, ok := netip.AddrFromSlice(ipn.IP)
					if !ok {
						continue
					}
					ip = ip.Unmap()
					// Record EVERY address this host holds, including the ones we
					// refuse to advertise (bridges, down interfaces) — that set is
					// what lets us reject a peer's claim to live at an address we
					// occupy ourselves. See ownAddrs.
					own[ip] = true
					if skip || !usableLocalCandidate(ip) || e.isOverlayAddr(ip) {
						continue
					}
					out = append(out, netip.AddrPortFrom(ip, port))
				}
			}
			e.ownAddrs.Store(&own)
			// Which address families we can actually originate traffic in. A
			// usable address here means a routable source for that family — the
			// same filter usableLocalCandidate applies (loopback and link-local
			// excluded: neither can source a packet to a peer's global or ULA
			// address). See canSourceFamily.
			var v4, v6 bool
			for ip := range own {
				if !usableLocalCandidate(ip) {
					continue
				}
				if ip.Is4() {
					v4 = true
				} else if ip.Is6() {
					v6 = true
				}
			}
			e.haveV4.Store(v4)
			e.haveV6.Store(v6)
		}
	}
	// Deterministic order, so an unchanged set of interfaces produces an
	// unchanged advertisement — peerListSig compares gossip content to decide
	// whether a re-broadcast is even needed, and map/interface iteration order
	// wobbling would otherwise make every node look permanently "changed" and
	// re-flood its peer list on every single gossip tick.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Addr() != out[j].Addr() {
			return out[i].Addr().Less(out[j].Addr())
		}
		return out[i].Port() < out[j].Port()
	})
	if len(out) > maxLocalEndpoints {
		out = out[:maxLocalEndpoints]
	}
	// Report the set whenever it changes (boot, an interface coming up, a port
	// change, a roam). The other half of the diagnosis addLocalCandidates' log
	// gives on the receiving side: if a peer is stuck relayed and never learns
	// our LAN address, this says whether we ever offered one. Silence means we
	// advertised nothing — no usable interface, or UDP and TCP/TLS both off —
	// which is a different problem from the candidate being advertised and then
	// ignored.
	sig := fmt.Sprint(out)
	if prev := e.lastLocalCandSig.Swap(&sig); prev == nil || *prev != sig {
		e.log.Infof("mesh: advertising %d host candidate(s) to peers: %v", len(out), out)
		if prev != nil {
			// Not the first call: our own attachment to the network actually
			// changed. Every "that candidate is unreachable from here" verdict
			// was made against the old attachment and is now worthless — see
			// clearDeadHostCands.
			e.clearDeadHostCands()
		}
	}
	e.localCands.Store(&out)
}

// hostCandGrace is how long a host candidate is retried before being written
// off as unreachable (contrast deadSeedGrace, an hour, for a real seed). A
// candidate is a same-link address: if it works at all it works immediately,
// with none of the reasons a real seed deserves patience (a peer still
// booting, a NAT yet to open, an operator-configured anchor that must never be
// abandoned). Meanwhile most candidates in a mesh of any size are unreachable
// from any given node by construction, so patience here is paid for in useless
// dialing.
//
// It MUST stay comfortably longer than directUpgradeInterval, and that is not
// a stylistic preference — it's the difference between this mechanism working
// and not working at all. The single most valuable host candidate is the one
// belonging to a peer we currently reach only via relay: that is the entire
// case host candidates exist for. But an upgrade attempt toward a relay-only
// peer is throttled to once per directUpgradeInterval (5 minutes) unless the
// peer is an explicit seed. Set this shorter than that — it was 90s, briefly —
// and such a candidate gets exactly ONE dial attempt, then gets swept and
// permanently marked hostCandDead before the throttle would even permit a
// second. One shot at the only job it has, then written off forever, with
// gossip barred from resurrecting it. Long enough for several throttled
// attempts, and still far below deadSeedGrace.
const hostCandGrace = 20 * time.Minute

// canSourceFamily reports whether this host can originate traffic to addr's
// family at all — i.e. whether it holds any routable address of that family.
//
// Without it, every send and every dial into an unusable family is a guaranteed
// ENETUNREACH, and gravinet retried each one on every cycle, forever. On a link
// with no IPv6 (a phone tether, most cellular data), a mesh whose peers advertise
// IPv6 endpoints and IPv6 host candidates produces a steady stream of
//
//	send: write udp6 [::]:65432->[fdf5:...]:65432: sendto: network is unreachable
//	tcp fallback dial [fdf5:...]:65432: connect: network is unreachable
//
// — dozens of pointless syscalls per tick, drowning the log and consuming the
// dial budget the addresses that *can* work are competing for. Precisely when the
// node is trying to recover from a roam and has the least room to waste.
//
// ENETUNREACH is a synchronous, definitive verdict from the kernel, in the same
// class as the EMSGSIZE handled in engine.send: not a transient to retry, but a
// statement that this path does not exist from here. Rather than react per-packet,
// ask the question once per maintenance tick, which is where the answer actually
// changes — refreshLocalCandidates already enumerates every address this host
// holds, so it knows which families are live, and a roam that gains or loses IPv6
// flips it within one tick.
//
// Conservative on purpose: unknown (nothing enumerated yet) reads as reachable, so
// a failure to enumerate can never wedge the node into refusing to dial anything.
func (e *Engine) canSourceFamily(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	if e.ownAddrs.Load() == nil {
		return true // no evidence yet: never refuse to dial on no evidence
	}
	if addr.Is4() || addr.Is4In6() {
		return e.haveV4.Load()
	}
	return e.haveV6.Load()
}

// isOwnAddr reports whether ip is an address this host itself holds — on any
// interface, including the virtual bridges and down interfaces we would never
// advertise ourselves.
//
// This is the receive-side counterpart to virtualBridgeIface, and it is precise
// where that one is heuristic. We cannot see a peer's interface names, so we
// cannot tell whether the 192.168.122.1 it advertised is a real uplink or its
// libvirt bridge. But we do not need to: if we hold that address too, then
// dialing it reaches *ourselves*, and it is worthless as a path to that peer no
// matter what it is on their end. Zero false positives — an address we own is
// never a way to reach somebody else — and it keeps working against peers on
// older builds that still advertise their bridges.
func (e *Engine) isOwnAddr(ip netip.Addr) bool {
	if m := e.ownAddrs.Load(); m != nil {
		return (*m)[ip]
	}
	return false
}

// clearDeadHostCands forgets every written-off host candidate on every network.
// Called when this node's own candidate set changes — a new DHCP lease, an
// interface up/down, a move from Wi-Fi to cellular. Reachability is a property
// of *both* ends, so our own attachment changing invalidates every previous
// "unreachable from here" verdict; a peer's LAN address that was hopeless a
// moment ago may be on our doorstep now.
func (e *Engine) clearDeadHostCands() {
	for _, ns := range e.netSnapshot() {
		ns.mu.Lock()
		if len(ns.hostCandDead) > 0 {
			ns.hostCandDead = make(map[netip.AddrPort]bool)
		}
		ns.mu.Unlock()
	}
}

// virtualBridgeNamePrefixes are interfaces belonging to host-local virtual
// networks — VM and container plumbing that this host itself owns and is the
// gateway for.
//
// Their addresses must never be advertised as host candidates, because they are
// *the same address on every host running the same stack*. libvirt puts
// 192.168.122.1 on virbr0 on every machine it's installed on; Docker puts
// 172.17.0.1 on docker0 on every machine. So when this node tells a peer "you
// can reach me at 192.168.122.1", any peer that also runs libvirt dials its own
// virbr0 and talks to itself. Observed in production exactly that way: mcfed
// advertised 192.168.122.1, and gn-cush1 — which has its own libvirt bridge at
// the identical address — dutifully logged "learned host candidate
// 192.168.122.1:65432 for peer 0916a3a70b1d5f4c" and dialed it. That handshake
// can only ever fail (it lands on cush1's own daemon, or on nothing), while
// consuming one of the peer's maxLocalEndpoints slots and a dial every cycle.
//
// Note what is deliberately NOT here: "br0"/"bridge0". A bare br0 is very often
// the *real* LAN bridge on a hypervisor host — the actual uplink, carrying the
// address a peer genuinely should dial. Excluding it would break the very
// same-LAN discovery this whole mechanism exists for. Docker's per-network
// bridges are "br-<12 hex digits>", which the "br-" prefix catches without
// touching br0.
//
// Other VPN/tunnel interfaces (wg*, tun*, tailscale*, zt*) are also absent on
// purpose: unlike a host-local bridge, a peer genuinely may be reachable across
// another VPN, and that is a legitimate path worth advertising.
var virtualBridgeNamePrefixes = []string{
	"virbr",   // libvirt
	"docker",  // Docker default bridge
	"br-",     // Docker user-defined networks (br-<hex>); does NOT match br0
	"veth",    // container veth pairs
	"vboxnet", // VirtualBox host-only
	"vmnet",   // VMware
	"lxcbr",   // LXC
	"lxdbr",   // LXD
	"podman",  // Podman
	"cni",     // CNI plugins
	"flannel", // Flannel
	"cali",    // Calico
	"weave",   // Weave
	"kube",    // various Kubernetes bridges
	"virbr-nic",
}

// virtualBridgeIface reports whether an interface name belongs to a host-local
// virtual network whose addresses are ambiguous across hosts — see
// virtualBridgeNamePrefixes.
func virtualBridgeIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range virtualBridgeNamePrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// usableLocalCandidate reports whether ip is worth advertising as a host
// candidate — see localEndpoints for why each class is excluded.
func usableLocalCandidate(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() {
		return false
	}
	if ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return true
}

// addLocalCandidates registers a peer's advertised host candidates as seeds for
// it, so initLoop dials them alongside its observed endpoint. Attributed to the
// node via AddSeedFor, so install()'s stale-seed pruning can clean up the ones
// that lose out once the peer is connected — the same lifecycle every other
// learned endpoint already has. addSeed drops any that land inside an overlay
// subnet.
func (e *Engine) addLocalCandidates(netID uint64, nodeID string, eps []netip.AddrPort) {
	if nodeID == "" || nodeID == e.nodeID {
		return
	}
	ns := e.network(netID)
	if ns == nil {
		return
	}
	for i, ep := range eps {
		if i >= maxLocalEndpoints {
			return
		}
		if !ep.IsValid() || !usableLocalCandidate(ep.Addr()) {
			continue // don't trust a peer's list any further than our own
		}
		if e.isOverlayAddr(ep.Addr()) {
			continue // addSeed would drop it anyway; don't mark or log it
		}
		if e.isOwnAddr(ep.Addr()) {
			// The peer is advertising an address we hold ourselves — almost
			// always a virtual-bridge address that is identical on both hosts
			// (libvirt's 192.168.122.1, Docker's 172.17.0.1). Dialing it reaches
			// this very daemon, never the peer. See isOwnAddr.
			continue
		}
		// Mark before seeding. ns.hostCand is what exempts this address from
		// install()'s stale-seed prune — without it the prune deletes every
		// host candidate on the owner's next handshake, since a candidate is
		// by definition not the observed endpoint. It's also the "have we ever
		// seen this" record that keeps the log below to genuinely new
		// information rather than re-announcing an address that was destroyed
		// and re-learned.
		ns.mu.Lock()
		if ns.hostCandDead[ep] {
			// Already given its chance and never connected. Gossip re-delivers
			// every peer's full candidate list on every interval, so without
			// this the sweep and the re-add would chase each other forever.
			ns.mu.Unlock()
			continue
		}
		firstTime := !ns.hostCand[ep]
		ns.hostCand[ep] = true
		ns.mu.Unlock()

		e.addSeed(netID, ep, nodeID, false)
		if firstTime {
			e.log.Infof("mesh: learned host candidate %s for peer %q on net %016x — will try it for a direct path", ep, nodeID, netID)
		}
	}
}
