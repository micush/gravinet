package mesh

// Peer overlay host-route guard.
//
// Motivating field report: six otherwise-unrelated nodes (five Linux, one
// OpenBSD) could each be pinged by a specific peer but never replied to it.
// Every other explanation was ruled out — the OS firewall was empty, gravinet's
// own per-network ACL was disabled, echo_ignore_all was 0 on every box — before
// the actual cause turned up in the plain host routing table: on all six nodes,
// something on the LAN was advertising (or had statically configured) a route
// to that one peer's *exact* overlay address that pointed out the physical LAN
// interface instead of the mesh device. gravinet's own base subnet route (e.g.
// "fd00:203::/64 dev mesh0", installed by assignAddr in addressing.go) was
// present and correct on every node; it didn't matter. Both IPv4 and IPv6
// routing always take the most specific matching route, full stop — a single
// host route beats a /64 or /24 it's nested inside regardless of metric,
// protocol, or which of the two was installed first. So the kernel generated
// the ICMP/overlay reply exactly like it should, then handed it to this
// more-specific route instead of the tunnel, and it vanished onto a physical
// segment with nowhere to deliver it — invisible to a capture taken on the
// mesh interface, because it genuinely never went out that interface at all.
//
// The actual misconfiguration lives on whatever device was the next-hop for
// that route — not in gravinet, and not in any of the six nodes' own config —
// so there is no fix here that reaches into someone else's router. What
// gravinet *can* do, and didn't, is stop losing the tie unnecessarily. A /32
// (or /128) host route to a peer's overlay address is exactly as specific as
// the kind of route that shadowed it above, and — unlike the /64 — contesting
// that specific destination on equal terms actually matters: installed at the
// lowest usable metric, it wins outright against anything less specific, and
// wins the common case of a router-advertised or DHCP/RA-sourced competing
// route, which normally doesn't get metric 0. It doesn't win against another
// route at the same prefix length *and* the same or lower metric — nothing
// short of removing that route at its source does — which is why this is a
// mitigation for gravinet's own usability, not a substitute for finding and
// fixing the actual router. Continuously reasserted (see reconcileDataplane's
// dpRouteReassertEvery, extended below to cover this alongside the base subnet
// route it already re-adds) so a route that reclaims the destination later —
// the LAN device re-advertising, a lease renewing — doesn't win permanently
// just because it happened to arrive after gravinet's own.
//
// Installed the same way the base subnet route already is (Device.AddRoute,
// an idempotent on-link "<prefix> dev <mesh-if>" replace), so it needs no new
// platform backend: every platform gravinet runs on, including the OpenBSD
// node in the report above, already implements AddRoute/DelRoute (see
// engine.go's Device interface and each internal/tun/*.go implementation).

import "net/netip"

// overlayGuardMetric is the metric a peer overlay guard route is installed
// at. Lowest usable value, for the same reason bypassMetric in fulltunnel.go
// is 0: it maximizes the odds of winning a same-specificity tie against
// whatever installed the shadowing route, since most sources of an
// unintentional competing route (router advertisements, DHCP-derived
// routes, a static route added without a deliberate low metric) don't
// themselves use metric 0.
const overlayGuardMetric = 0

// hostGuardPrefix returns the single-address prefix (/32 for IPv4, /128 for
// IPv6) that makes a guard route exactly as specific as the kind of route
// this defends against — never wider, since a wider prefix would just be
// another subnet route with the same shadowing problem the base route
// already has.
func hostGuardPrefix(addr netip.Addr) netip.Prefix {
	return netip.PrefixFrom(addr, addr.BitLen())
}

// installOverlayGuardRoute installs (or idempotently re-installs) an on-link
// host route for addr via ns's mesh device. A no-op for an invalid address or
// a network with no live device (e.g. a test harness with no NewDevice
// wired), same guard reconcileDataplane already applies before touching
// dev(). Failure is logged at Debug, not Warn: this is defense in depth on
// top of the base subnet route that already carries the connection when no
// shadowing route exists, so losing this specifically isn't itself a
// connectivity-affecting event worth surfacing as a warning — the periodic
// reassert (see reassertOverlayGuardRoutes) gets another chance shortly
// either way.
func (e *Engine) installOverlayGuardRoute(ns *netState, addr netip.Addr) {
	if !addr.IsValid() {
		return
	}
	dev := ns.dev()
	if dev == nil {
		return
	}
	if err := dev.AddRoute(hostGuardPrefix(addr), overlayGuardMetric); err != nil {
		e.log.Debugf("mesh: install overlay guard route for %s on net %x (%s): %v", addr, ns.spec.ID, dev.Name(), err)
	}
}

// removeOverlayGuardRoute removes a previously-installed guard route for
// addr. A missing route is not an error (DelRoute's own contract); best-effort
// like every other route teardown in this package.
func (e *Engine) removeOverlayGuardRoute(ns *netState, addr netip.Addr) {
	if !addr.IsValid() {
		return
	}
	dev := ns.dev()
	if dev == nil {
		return
	}
	if err := dev.DelRoute(hostGuardPrefix(addr), overlayGuardMetric); err != nil {
		e.log.Debugf("mesh: remove overlay guard route for %s on net %x (%s): %v", addr, ns.spec.ID, dev.Name(), err)
	}
}

// installOverlayGuardRoutes installs guard routes for both address families
// of a freshly-installed peer session. Called from install() (handshake_engine.go)
// right alongside syncPeerBypassRoute — the mirror-image defense: that one
// keeps the mesh's own tunnel from swallowing a peer's *underlay* traffic,
// this one keeps a foreign LAN route from swallowing a peer's *overlay*
// traffic. Safe to call for a re-handshake over a live session (AddRoute
// replaces, not duplicates).
func (e *Engine) installOverlayGuardRoutes(ns *netState, ps *peerSession) {
	e.installOverlayGuardRoute(ns, ps.overlay4)
	e.installOverlayGuardRoute(ns, ps.overlay6)
}

// removeOverlayGuardRoutes removes both address families' guard routes for a
// session that just left ns.routes4/routes6 — called at every site that
// deletes those map entries (teardownSessions, applyBan, localDisconnect,
// dropRetiredKeySessions), mirroring how each of those already tears down
// the peer's bypass route and/or redistributed routes. Left installed after
// teardown, a guard route is not itself a leak that misroutes live traffic —
// the dataplane already has no session to hand matching packets to and drops
// them regardless of what the OS route says — but removing it promptly keeps
// the host table from accumulating routes to peers this node no longer talks
// to, the same hygiene dropNodeRoutes already applies to redistributed
// prefixes.
func (e *Engine) removeOverlayGuardRoutes(ns *netState, ps *peerSession) {
	e.removeOverlayGuardRoute(ns, ps.overlay4)
	e.removeOverlayGuardRoute(ns, ps.overlay6)
}

// reassertOverlayGuardRoutes re-installs every live peer's guard route(s) on
// ns. Called from reconcileDataplane at the same dpRouteReassertEvery cadence
// as the base subnet route's own defensive re-add, and for the same reason:
// nothing here can see whether a route was quietly stripped or shadowed
// without querying the routing table, so the cheap alternative is
// periodically re-asserting what should be there. This is what makes the
// guard route a *continuous* defense rather than a one-time install that a
// later-arriving competing route (a lease renewal, a flapping RA) could win
// away from permanently.
func (e *Engine) reassertOverlayGuardRoutes(ns *netState) {
	if ns.dev() == nil {
		return
	}
	ns.mu.RLock()
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		peers = append(peers, ps)
	}
	ns.mu.RUnlock()
	for _, ps := range peers {
		e.installOverlayGuardRoutes(ns, ps)
	}
}
