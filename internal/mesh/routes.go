package mesh

import (
	"net/netip"
	"time"
)

// redistRoute resolves a destination to a peer session via a redistributed
// route (longest-prefix match whose origin currently has a session). Used as a
// fallback when no exact host route matches.
func (e *Engine) redistRoute(ns *netState, dst netip.Addr) *peerSession {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	var best *peerSession
	bestBits := -1
	bestMetric := 0
	for _, re := range ns.redist {
		if !re.prefix.Contains(dst) {
			continue
		}
		bits := re.prefix.Bits()
		// Prefer the most specific prefix; among equally specific ones, the
		// lowest metric wins.
		if bits < bestBits {
			continue
		}
		if bits == bestBits && best != nil && re.metric >= bestMetric {
			continue
		}
		if ps := ns.byNode[re.origin]; ps != nil {
			best = ps
			bestBits = bits
			bestMetric = re.metric
		}
	}
	return best
}

// rejected reports whether an advertised prefix is refused by a reject rule. By
// default a rule matches only the exact prefix; an Inclusive rule also matches
// every more-specific route contained within it. (A non-inclusive /0 therefore
// blocks only a learned default route, never the routes it nominally contains.)
func (ns *netState) rejected(p netip.Prefix) bool {
	rej := ns.advReject.Load()
	if rej == nil {
		return false
	}
	pm := p.Masked()
	for _, r := range *rej {
		if r.Inclusive {
			if r.Prefix.Bits() <= p.Bits() && r.Prefix.Contains(p.Addr()) {
				return true
			}
		} else if r.Prefix.Masked() == pm {
			return true
		}
	}
	return false
}

// cloneMetricMap copies a prefix→metric map (nil-safe).
func cloneMetricMap(m map[netip.Prefix]int) map[netip.Prefix]int {
	out := make(map[netip.Prefix]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// metricFor returns the advertised metric for prefix p (0 if unset).
func (ns *netState) metricFor(p netip.Prefix) int {
	if mp := ns.advMetric.Load(); mp != nil {
		return (*mp)[p]
	}
	return 0
}

// advertiseRoutes floods this node's configured routes to the mesh, plus
// (see bgpRedistSet's doc comment) anything currently redistributed from
// BGP — the two live in separate state but are combined here, since a
// newly-connected peer needs the complete picture in one flood, not just
// whichever source it happens to ask about.
func (e *Engine) advertiseRoutes(ns *netState) {
	rs := ns.advRoutes.Load()
	if rs != nil {
		for _, p := range *rs {
			e.floodControl(ns, encodeRouteAdd(e.nodeID, p, ns.metricFor(p)), nil)
		}
	}
	if br := ns.bgpRoutes.Load(); br != nil {
		for _, p := range br.routes {
			e.floodControl(ns, encodeRouteAdd(e.nodeID, p, br.metric), nil)
		}
	}
}

// withdrawRoutes floods a withdrawal for each prefix so peers drop it from their
// redistributed-route tables. Local-origin withdrawal, mirroring an unban.
func (e *Engine) withdrawRoutes(ns *netState, prefixes []netip.Prefix) {
	for _, p := range prefixes {
		e.floodControl(ns, encodeRouteDel(e.nodeID, p), nil)
		e.log.Infof("mesh: withdrawing route %s on net %x", p, ns.spec.ID)
	}
}

// reloadRoutes swaps the advertised route and reject sets live and floods the
// delta: newly-added routes are advertised, removed ones are withdrawn. It also
// purges any already-learned route the new reject set now covers, so rejecting a
// route takes effect immediately rather than only blocking future
// advertisements. This is what makes redistributing (or un-redistributing, or
// rejecting) a route apply without a restart.
func (e *Engine) reloadRoutes(ns *netState, newRoutes []netip.Prefix, newReject []RejectRule, newMetric map[netip.Prefix]int) {
	var old []netip.Prefix
	if oldp := ns.advRoutes.Load(); oldp != nil {
		old = *oldp
	}
	var oldMetric map[netip.Prefix]int
	if omp := ns.advMetric.Load(); omp != nil {
		oldMetric = *omp
	}
	nr := append([]netip.Prefix(nil), newRoutes...)
	ns.advRoutes.Store(&nr)
	rj := append([]RejectRule(nil), newReject...)
	ns.advReject.Store(&rj)
	mm := cloneMetricMap(newMetric)
	ns.advMetric.Store(&mm)

	// Purge any already-learned routes the (possibly new) reject set now covers,
	// so rejecting a route drops it immediately — removed from the forwarding
	// table and uninstalled from the OS routing table, the same way a withdrawn
	// redistributed route is. Clearing knownRoute lets the route be re-learned if
	// the reject is later lifted and the origin re-advertises.
	ns.mu.Lock()
	var purged []netip.Prefix
	if len(ns.redist) > 0 {
		kept := ns.redist[:0]
		for _, re := range ns.redist {
			if ns.rejected(re.prefix) {
				delete(ns.knownRoute, re.origin+"|"+re.prefix.String())
				purged = append(purged, re.prefix)
				continue
			}
			kept = append(kept, re)
		}
		ns.redist = kept
	}
	ns.mu.Unlock()
	for _, p := range purged {
		e.syncRoute(ns, p)
	}

	inSet := func(set []netip.Prefix, p netip.Prefix) bool {
		for _, q := range set {
			if q == p {
				return true
			}
		}
		return false
	}
	var added, removed []netip.Prefix
	for _, p := range newRoutes {
		if !inSet(old, p) {
			added = append(added, p)
		}
	}
	for _, p := range old {
		if !inSet(newRoutes, p) {
			removed = append(removed, p)
		}
	}
	for _, p := range added {
		e.floodControl(ns, encodeRouteAdd(e.nodeID, p, ns.metricFor(p)), nil)
		e.log.Infof("mesh: advertising route %s on net %x", p, ns.spec.ID)
	}
	// Re-advertise routes whose metric changed (same prefix, different metric) so
	// peers pick up the new value live.
	for _, p := range newRoutes {
		if inSet(old, p) && oldMetric[p] != newMetric[p] {
			e.floodControl(ns, encodeRouteAdd(e.nodeID, p, newMetric[p]), nil)
			e.log.Infof("mesh: route %s metric -> %d on net %x", p, newMetric[p], ns.spec.ID)
		}
	}
	if len(removed) > 0 {
		e.withdrawRoutes(ns, removed)
	}
}

// bgpRedistSet is a network's current BGP-into-mesh redistribution set: the
// CIDRs pulled from FRR's RIB (see webadmin's bgpMeshRedistributor) plus the
// single metric config.Network.RedistributeBGPMetric assigns to all of them.
// Bundled into one struct behind one atomic pointer — rather than a separate
// slice and int the way advRoutes/advMetric are two pointers — so a
// concurrent reader (advertiseRoutes, mid-flood, racing a poll's update) can
// never observe routes from one update paired with the metric from another;
// advRoutes/advMetric can only tear that way across a genuinely simultaneous
// reload, which reloadRoutes already treats as acceptable (see its own
// sequential Store calls), but bgpRoutes updates on every poll tick even when
// nothing changed, so the same assumption held less comfortably here.
type bgpRedistSet struct {
	routes []netip.Prefix
	metric int
}

// reloadBGPRoutes swaps this node's BGP-into-mesh redistribution set live and
// floods the delta — the BGP-sourced counterpart to reloadRoutes, kept
// entirely separate (own atomic state, own diff-and-flood) so a BGP RIB poll
// and a config-driven Advertise-route reload can never race to clobber each
// other; see bgpRoutes's doc comment on netState. Called from SetBGPRoutes
// below, which is what webadmin's poller actually calls — every poll tick,
// whether or not the RIB changed, since there's no cheaper way for the
// caller to know that in advance than to just call this and let the diff
// below do nothing when nothing changed.
func (e *Engine) reloadBGPRoutes(ns *netState, newRoutes []netip.Prefix, metric int) {
	var old []netip.Prefix
	oldMetric := 0
	if oldp := ns.bgpRoutes.Load(); oldp != nil {
		old, oldMetric = oldp.routes, oldp.metric
	}
	nr := append([]netip.Prefix(nil), newRoutes...)
	ns.bgpRoutes.Store(&bgpRedistSet{routes: nr, metric: metric})

	inSet := func(set []netip.Prefix, p netip.Prefix) bool {
		for _, q := range set {
			if q == p {
				return true
			}
		}
		return false
	}
	var added, removed []netip.Prefix
	for _, p := range newRoutes {
		if !inSet(old, p) {
			added = append(added, p)
		}
	}
	for _, p := range old {
		if !inSet(newRoutes, p) {
			removed = append(removed, p)
		}
	}
	for _, p := range added {
		e.floodControl(ns, encodeRouteAdd(e.nodeID, p, metric), nil)
		e.log.Infof("mesh: advertising BGP-redistributed route %s on net %x", p, ns.spec.ID)
		// bestRedistMetric now treats p as self-redistributed (ns.bgpRoutes
		// was already updated above), so this drops any mesh-sourced kernel
		// route a sibling's gossiped copy had installed for it before this
		// node started redistributing p itself — see bestRedistMetric's own
		// doc comment. Without this call nothing re-evaluates p until some
		// unrelated event touches its ns.redist entry (a peer re-advertising,
		// a metric change, a stale sweep), which could be an arbitrarily long
		// wait; this makes the switch to the node's own BGP path immediate.
		e.syncRoute(ns, p)
	}
	// Unlike reloadRoutes, this is a single metric for the whole batch, so a
	// metric change re-advertises every currently-held prefix at once (no
	// per-prefix comparison needed) rather than hunting for which ones
	// changed — there's only ever one value that could have.
	if oldMetric != metric {
		for _, p := range newRoutes {
			if inSet(old, p) {
				e.floodControl(ns, encodeRouteAdd(e.nodeID, p, metric), nil)
			}
		}
		if len(old) > 0 || len(newRoutes) > 0 {
			e.log.Infof("mesh: BGP-redistributed routes metric -> %d on net %x", metric, ns.spec.ID)
		}
	}
	if len(removed) > 0 {
		e.withdrawRoutes(ns, removed)
		// The mirror of the added-side call above: p just left ns.bgpRoutes
		// (this node stopped redistributing it — BGP went down, the operator
		// unchecked it, whatever the cause), so bestRedistMetric will now
		// fall through to ns.redist again. If a sibling exit node is still
		// gossiping p, this is what actually installs its route as this
		// node's fallback — immediately, rather than only after that
		// sibling's next unrelated re-advertisement.
		for _, p := range removed {
			e.syncRoute(ns, p)
		}
	}
}

// SetBGPRoutes updates the CIDRs this node is currently redistributing from
// its BGP RIB into networkID's mesh gossip (config.Network.RedistributeBGPRoutes),
// tagged with metric. It's webadmin's bgpMeshRedistributor that calls this —
// gravinet's mesh engine never talks to FRR itself, it just accepts whatever
// route set the caller currently has and reconciles the mesh side (see
// reloadBGPRoutes). Passing an empty routes slice clears redistribution for
// this network (withdrawing anything previously sent), which is exactly what
// the poller does the moment RedistributeBGPRoutes empties out, BGP itself goes
// down, or the network is disabled — this function has no opinion on why the
// set is empty, only that it now is.
//
// ok is false if networkID isn't configured on this node; the caller should
// treat that as "nothing to do" rather than a error worth logging.
func (e *Engine) SetBGPRoutes(networkID uint64, routes []netip.Prefix, metric int) (ok bool) {
	ns := e.network(networkID)
	if ns == nil {
		return false
	}
	e.reloadBGPRoutes(ns, routes, metric)
	return true
}

func (e *Engine) onRouteAdd(ps *peerSession, body []byte) {
	origin, prefix, metric, ok := decodeRouteAdd(body)
	if !ok || origin == e.nodeID {
		return
	}
	ns := ps.net
	if ns.rejected(prefix) {
		e.log.Debugf("mesh: rejecting advertised route %s from %q", prefix, origin)
		return
	}
	key := origin + "|" + prefix.String()
	now := time.Now()
	ns.mu.Lock()
	if ns.knownRoute[key] {
		// Already known — refresh its freshness so it isn't swept as stale, and
		// pick up any metric change.
		metricChanged := false
		for i := range ns.redist {
			if ns.redist[i].origin == origin && ns.redist[i].prefix == prefix {
				ns.redist[i].lastSeen = now
				if ns.redist[i].metric != metric {
					ns.redist[i].metric = metric
					metricChanged = true
				}
				break
			}
		}
		ns.mu.Unlock()
		if metricChanged {
			e.log.Infof("mesh: route %s via %q metric -> %d on net %x", prefix, origin, metric, ns.spec.ID)
			// The best metric for this prefix may have changed; re-program the OS
			// route so `ip route` reflects the new value.
			e.syncRoute(ns, prefix)
			e.floodControl(ns, encodeRouteAdd(origin, prefix, metric), ps)
		}
		return // no new route, just possibly an updated metric; stop the flood
	}
	ns.knownRoute[key] = true
	ns.redist = append(ns.redist, routeEntry{origin: origin, prefix: prefix, metric: metric, lastSeen: now})
	ns.mu.Unlock()

	e.log.Infof("mesh: learned route %s via %q on net %x", prefix, origin, ns.spec.ID)
	// Program the host routing table so the kernel actually hands packets for
	// this prefix to the TUN; without it the route exists only inside the engine
	// and never appears in `ip route`, so the OS never sends the traffic in. The
	// route is installed with its advertised metric (RTA_PRIORITY on Linux).
	e.syncRoute(ns, prefix)
	e.floodControl(ns, encodeRouteAdd(origin, prefix, metric), ps)
}

// encodeRouteDel builds a route-withdrawal control message (origin + prefix).
func encodeRouteDel(origin string, p netip.Prefix) []byte {
	out := []byte{ctrlRouteDel}
	out = appendLenStr(out, origin)
	out = append(out, encodeAddr(p.Addr())...)
	out = append(out, byte(p.Bits()))
	return out
}

func decodeRouteDel(b []byte) (origin string, prefix netip.Prefix, ok bool) {
	r := reader{b: b}
	origin, ok = r.lenStr()
	if !ok {
		return "", netip.Prefix{}, false
	}
	fam, ok := r.byte()
	if !ok {
		return "", netip.Prefix{}, false
	}
	var addr netip.Addr
	switch fam {
	case 4:
		v, ok := r.take(4)
		if !ok {
			return "", netip.Prefix{}, false
		}
		addr = netip.AddrFrom4([4]byte{v[0], v[1], v[2], v[3]})
	case 6:
		v, ok := r.take(16)
		if !ok {
			return "", netip.Prefix{}, false
		}
		var a [16]byte
		copy(a[:], v)
		addr = netip.AddrFrom16(a)
	default:
		return "", netip.Prefix{}, false
	}
	bits, ok := r.byte()
	if !ok {
		return "", netip.Prefix{}, false
	}
	pfx, err := addr.Prefix(int(bits))
	if err != nil {
		return "", netip.Prefix{}, false
	}
	return origin, pfx, true
}

// onRouteDel removes a withdrawn route from the redistributed table and keeps the
// withdrawal flooding, mirroring an unban. Only the origin's own entry is
// removed, so two nodes advertising the same CIDR don't clobber each other.
func (e *Engine) onRouteDel(ps *peerSession, body []byte) {
	origin, prefix, ok := decodeRouteDel(body)
	if !ok || origin == e.nodeID {
		return
	}
	ns := ps.net
	key := origin + "|" + prefix.String()
	ns.mu.Lock()
	had := ns.knownRoute[key]
	if had {
		delete(ns.knownRoute, key)
		kept := ns.redist[:0]
		for _, re := range ns.redist {
			if re.origin == origin && re.prefix == prefix {
				continue
			}
			kept = append(kept, re)
		}
		ns.redist = kept
	}
	ns.mu.Unlock()
	if !had {
		return // already gone; stop the flood
	}
	e.log.Infof("mesh: route %s withdrawn by %q on net %x", prefix, origin, ns.spec.ID)
	// Reconcile the OS route: removed if no origin still advertises it, otherwise
	// re-programmed in case the withdrawing origin held the best metric.
	e.syncRoute(ns, prefix)
	e.floodControl(ns, encodeRouteDel(origin, prefix), ps)
}

// bestRedistMetric returns the lowest advertised metric among the redistributed
// entries for this exact prefix, and whether any advertiser exists. That lowest
// value is the metric we program into the OS route (consistent with the
// lowest-metric-wins forwarding choice in redistRoute).
//
// A prefix this node is itself currently redistributing from its own BGP RIB
// (ns.bgpRoutes — see reloadBGPRoutes/SetBGPRoutes) is always reported as not
// advertised here, regardless of what ns.redist holds for it — even if a
// peer is advertising it at a numerically better metric. This node already
// has a direct, authoritative path to that prefix via FRR's own BGP-learned
// route; a peer's gossiped copy exists so *other* nodes can reach the prefix
// through one of possibly several redistributing exit nodes, not so an exit
// node can second-guess its own live BGP session. Without this check, a pair
// of redundant exit nodes each redistributing the same external prefix (the
// intended, ordinary multi-exit setup — see docs/changelog.md v542) would
// each treat the other's gossiped copy as just another route, install it as
// a plain kernel route via syncRoute, and since FRR treats any pre-existing
// kernel route as distance 0 — unconditionally ahead of any BGP-learned
// route, no matter its distance — silently loop that node's own transit
// traffic for the prefix back out over the mesh to its sibling instead of
// out its own working BGP session. bestRedistMetric can't see ns.redist's
// own self-origin exclusion (onRouteAdd already refuses to record this
// node's own advertisements there) helping here: the colliding entry
// legitimately belongs to a *different* origin, so that filter doesn't
// apply. This is the same failure mode, just via a sibling's advertisement
// rather than this node's own looped-back one.
func (ns *netState) bestRedistMetric(p netip.Prefix) (int, bool) {
	if br := ns.bgpRoutes.Load(); br != nil {
		for _, q := range br.routes {
			if q == p {
				return 0, false
			}
		}
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	best, found := 0, false
	for _, re := range ns.redist {
		if re.prefix == p {
			if !found || re.metric < best {
				best, found = re.metric, true
			}
		}
	}
	return best, found
}

// MeshRouteMetricFloor is added to every advertised metric before it's
// programmed into the OS routing table by syncRoute (not
// syncFullTunnelRoute — a full-tunnel default route is a deliberate
// "route everything through the mesh" opt-in and needs to be able to
// compete with a local default route on its own terms, the opposite of
// what this is for).
//
// Without it, a route this node learned from a mesh peer and one it
// already has locally (a directly-connected interface, or one DHCP/RA
// handed it) can end up as two entries for the very same prefix in the
// kernel's own table, and the kernel picks between same-prefix entries by
// metric alone — it has no notion of "connected beats mesh-learned" the
// way a routing daemon's RIB does. A peer that happens to advertise a low
// metric (0 is the Advertise table's own default) could then win over a
// route this host would otherwise use directly, silently detouring locally
// deliverable traffic out through the mesh tunnel instead. 9000 is
// comfortably above any metric a normal connected/DHCP/static route is
// likely to carry (DHCP-assigned metrics in the low thousands aren't
// unusual, but nowhere near this) without needing this node to actually
// enumerate what those routes currently are — it's cheap insurance, not a
// promise those routes are ordered by name.
//
// Applied uniformly to whatever bestRedistMetric already picked as the
// lowest-advertised value, so relative preference *among* mesh peers
// advertising the same prefix — lower advertised metric still wins — is
// unaffected; only where the result lands relative to this node's own
// non-mesh routes changes.
//
// Exported and reused as-is by internal/webadmin's own distance-bgp
// rendering (see frr.go's renderFRR) — a BGP-learned route risks the exact
// same collision with a local route, just installed by FRR/zebra instead
// of gravinet's own syncRoute, and deserves the same numeric floor rather
// than an independently-chosen one that could drift from this one over
// time for no real reason.
const MeshRouteMetricFloor = 9000

// syncRoute reconciles the host routing-table entry for prefix p with the
// current best advertised metric: it installs the route if missing, re-programs
// it if the best metric changed, or removes it once no origin advertises p any
// more. The installed metric is tracked because a route's kernel identity
// includes its priority (metric) — so a metric change is a delete-then-add, and
// a delete must target the exact metric that was installed. Failures are logged,
// not fatal: overlay forwarding still works from the engine's own table.
func (e *Engine) syncRoute(ns *netState, p netip.Prefix) {
	if ns.spec.Dev == nil {
		return
	}
	// A default route (0.0.0.0/0 or ::/0) only ever reaches here at all if the
	// operator removed it from RouteRej's default (see config.NewNetworkDefaults'
	// doc comment) — this is a deliberate full-tunnel opt-in, not an ordinary
	// redistributed prefix, and installing it needs the /1-split plus the
	// peer-bypass machinery in fulltunnel.go, not a literal "default dev <tun>".
	if isDefaultRoute(p) {
		e.syncFullTunnelRoute(ns, p)
		return
	}
	metric, advertised := ns.bestRedistMetric(p)
	metric += MeshRouteMetricFloor
	ns.osMu.Lock()
	defer ns.osMu.Unlock()
	old, installed := ns.osMetric[p]

	if !advertised {
		if installed {
			if err := ns.dev().DelRoute(p, old); err != nil {
				e.log.Debugf("mesh: remove route %s from OS table (net %x): %v", p, ns.spec.ID, err)
			}
			delete(ns.osMetric, p)
		}
		return
	}
	if installed && old == metric {
		return // already programmed at this metric
	}
	if installed && old != metric {
		// Priority is part of the route key; drop the stale-metric entry first so
		// we don't leave two routes for the same prefix.
		if err := ns.dev().DelRoute(p, old); err != nil {
			e.log.Debugf("mesh: replace route %s (old metric %d) on net %x: %v", p, old, ns.spec.ID, err)
		}
	}
	if err := ns.dev().AddRoute(p, metric); err != nil {
		e.log.Warnf("mesh: install route %s (metric %d) into OS table (net %x): %v", p, metric, ns.spec.ID, err)
		return
	}
	ns.osMetric[p] = metric
}

// isDefaultRoute reports whether p is a default route in either address
// family (0.0.0.0/0 or ::/0) — the only prefixes syncRoute ever redirects to
// syncFullTunnelRoute instead of installing directly.
func isDefaultRoute(p netip.Prefix) bool { return p.Bits() == 0 }

// syncFullTunnelRoute is syncRoute's counterpart for an accepted default
// route: installs p (0.0.0.0/0 or ::/0) literally, at the peer-advertised
// metric. As of v317 this is preceded, on every platform, by
// demotePhysicalDefaultRoute deprioritizing whatever default route already
// exists (see that function's doc comment in fulltunnel.go) — not because
// every platform's kernel actually requires it: Linux in particular is
// entirely happy to keep two default routes at different metrics and
// simply prefer whichever is lower, which is exactly what let this
// function get away with installing "alongside" the physical route,
// undemoted, through v316. From v317 on it demotes there too anyway, so
// there's one sequence to reason about instead of Linux being the one
// platform that skips a step FreeBSD/OpenBSD/macOS/Windows can't.
//
// This used to install a /1 split (0.0.0.0/1 + 128.0.0.0/1) instead of the
// literal prefix, on the theory that it was needed to keep the mesh's own
// underlay traffic from looping into the tunnel. That theory doesn't hold
// once fulltunnel.go's /32-per-peer bypass routes exist: those win
// longest-prefix-match against *any* less specific route, /1 split or
// literal /0 alike, so the split bought nothing for loop-safety once the
// bypass mechanism was in place — a literal /0 is exactly as safe.
//
// The split's actual remaining justification — the reason wg-quick still
// uses it — is different and unrelated to peer safety: a literal /0 is
// visible to tooling that specifically looks for dst-length==0 (DHCP
// clients, NetworkManager, systemd-networkd), and something like a lease
// renewal doing its own default-route management could contest or
// periodically reassert against it. A /1 split is invisible to that class
// of check, so nothing else on the box ever touches it. That's a real
// trade-off, but it's an operational one for whoever's running this node to
// weigh, not a safety requirement — this installs the literal route by
// design.
//
// ns.fullTunnel is flipped before the route is installed (so peer/seed
// bypass routes exist first — no window where the /0 is up but its escape
// hatch isn't) and after it's removed (so a route acquired while it was on
// stays cleanable).
func (e *Engine) syncFullTunnelRoute(ns *netState, p netip.Prefix) {
	metric, advertised := ns.bestRedistMetric(p)

	ns.osMu.Lock()
	old, installed := ns.osMetric[p]
	ns.osMu.Unlock()

	// A literal default route is still the *dangerous* half of full-tunnel —
	// a plain on-link route, already installable on every platform via the
	// ordinary AddRoute below, regardless of whether this platform's
	// peer/seed bypass-route machinery (the safety net that keeps it from
	// looping the mesh's own traffic into itself) actually exists yet. So
	// this is a hard prerequisite, checked before touching anything: if the
	// platform can't back the bypass routes, it doesn't get the default
	// route either — refusing outright is the only safe option, not a
	// partial activation with a silently-absent safety net
	// (acquireBypassRoute would just fail quietly, on a platform where that
	// failure means something very different than "no seed to acquire yet").
	if advertised && !installed && !gatewaySupported {
		e.log.Warnf("mesh: net %x: peer advertised a full-tunnel default route, but this platform has no bypass-route backend yet (see internal/tun/gateway_unsupported.go) — refusing to activate rather than install an unprotected default", ns.spec.ID)
		return
	}

	if !advertised {
		if !installed {
			return
		}
		ns.osMu.Lock()
		delete(ns.osMetric, p)
		ns.osMu.Unlock()
		if err := ns.dev().DelRoute(p, old); err != nil {
			e.log.Debugf("mesh: remove full-tunnel default route %s from OS table (net %x): %v", p, ns.spec.ID, err)
		}
		e.restorePhysicalDefaultRoute(ns, p)
		e.log.Infof("mesh: full-tunnel default route withdrawn on net %x", ns.spec.ID)
		ns.fullTunnel.Store(false)
		e.resyncAllBypassRoutes(ns) // release now-orphaned peer bypass routes
		return
	}
	if installed && old == metric {
		return // already active at this metric; fullTunnel already true from when this first installed
	}

	// Bypass routing has to exist before the default route does, not after —
	// turn it on and backfill every live session/seed first, so there's no
	// window where the /0 could capture the mesh's own dial/keepalive
	// traffic before its escape hatch is in place.
	ns.fullTunnel.Store(true)
	e.resyncAllBypassRoutes(ns)
	e.syncSeedBypassRoutes(ns)

	if installed && old != metric {
		if err := ns.dev().DelRoute(p, old); err != nil {
			e.log.Debugf("mesh: replace full-tunnel default route %s (old metric %d) on net %x: %v", p, old, ns.spec.ID, err)
		}
	}
	if !installed {
		// Demote the pre-existing physical default route once, before the
		// very first AddRoute below — see demotePhysicalDefaultRoute's doc
		// comment (fulltunnel.go) for why this now runs on every platform,
		// including Linux, as of v317. A metric-only update to an
		// already-active full-tunnel route (the branch above) never repeats
		// this: the physical route was already demoted when this first went
		// active.
		e.demotePhysicalDefaultRoute(ns, p)
	}
	if err := ns.dev().AddRoute(p, metric); err != nil {
		e.log.Warnf("mesh: install full-tunnel default route %s (metric %d) into OS table (net %x): %v", p, metric, ns.spec.ID, err)
		return // leave osMetric unset so the next advertisement retries the install
	}
	ns.osMu.Lock()
	ns.osMetric[p] = metric
	ns.osMu.Unlock()
	e.log.Infof("mesh: full-tunnel default route active on net %x (metric %d)", ns.spec.ID, metric)
}

// dropNodeRoutes forgets every redistributed route learned from nodeID and
// uninstalls the OS route for any prefix no longer advertised by another origin.
// Called when a node's session goes away (timeout, ban, disable) so its routes
// don't linger here after it's gone — they're re-learned if the node returns and
// re-advertises. Safe to call without holding ns.mu.
func (e *Engine) dropNodeRoutes(ns *netState, nodeID string) {
	ns.mu.Lock()
	if len(ns.redist) == 0 {
		ns.mu.Unlock()
		return
	}
	var gone []netip.Prefix
	kept := ns.redist[:0]
	for _, re := range ns.redist {
		if re.origin == nodeID {
			delete(ns.knownRoute, re.origin+"|"+re.prefix.String())
			gone = append(gone, re.prefix)
			continue
		}
		kept = append(kept, re)
	}
	ns.redist = kept
	ns.mu.Unlock()
	if len(gone) == 0 {
		return
	}
	// Reconcile each affected prefix: removed if this node was its only origin,
	// otherwise re-programmed with the best metric still on offer.
	for _, p := range gone {
		e.syncRoute(ns, p)
	}
	e.log.Infof("mesh: dropped %d route(s) learned from %q on net %x", len(gone), nodeID, ns.spec.ID)
}

// routeHoldMultiple sets how many re-advertisement intervals a learned route may
// go unrefreshed before it is withdrawn as stale. minRouteHold floors it so a very
// short cadence can't cause flapping on normal jitter.
const (
	routeHoldMultiple = 2
	minRouteHold      = 15 * time.Second
)

// routeTTL is the maximum silence from a route's origin before peers withdraw it.
// With the default 10s re-advertisement cadence this is ~20s, so a route leaves
// the mesh within ~20s of its origin going offline rather than waiting out the
// much longer dead-session timeout.
func (e *Engine) routeTTL() time.Duration {
	ttl := time.Duration(routeHoldMultiple) * e.routeAdvInterval()
	if ttl < minRouteHold {
		ttl = minRouteHold
	}
	return ttl
}

// sweepStaleRoutes withdraws learned routes whose origin has stopped
// re-advertising them — a fast, independent signal that the origin has gone
// offline. Each node ages routes off its own copy as the periodic
// re-advertisements stop arriving; nothing is flooded, since every peer reaches
// the same conclusion from the same missing updates.
func (e *Engine) sweepStaleRoutes(ns *netState, now time.Time) {
	ttl := e.routeTTL()
	ns.mu.Lock()
	if len(ns.redist) == 0 {
		ns.mu.Unlock()
		return
	}
	var gone []netip.Prefix
	kept := ns.redist[:0]
	for _, re := range ns.redist {
		if !re.lastSeen.IsZero() && now.Sub(re.lastSeen) > ttl {
			delete(ns.knownRoute, re.origin+"|"+re.prefix.String())
			gone = append(gone, re.prefix)
			continue
		}
		kept = append(kept, re)
	}
	ns.redist = kept
	ns.mu.Unlock()
	if len(gone) == 0 {
		return
	}
	for _, p := range gone {
		e.syncRoute(ns, p)
	}
	e.log.Infof("mesh: withdrew %d stale route(s) on net %x (origin silent > %s)", len(gone), ns.spec.ID, ttl)
}

// Routes returns the redistributed routes known on a network (control API).
func (e *Engine) Routes(networkID uint64) []RouteInfo {
	ns := e.network(networkID)
	if ns == nil {
		return nil
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	out := make([]RouteInfo, 0, len(ns.redist))
	for _, re := range ns.redist {
		out = append(out, RouteInfo{CIDR: re.prefix.String(), Via: re.origin, Metric: re.metric})
	}
	return out
}

// RouteInfo is a redistributed route as reported to the control API.
type RouteInfo struct {
	CIDR   string `json:"cidr"`
	Via    string `json:"via"`
	Metric int    `json:"metric"`
}

func encodeRouteAdd(origin string, p netip.Prefix, metric int) []byte {
	out := []byte{ctrlRouteAdd}
	out = appendLenStr(out, origin)
	out = append(out, encodeAddr(p.Addr())...)
	out = append(out, byte(p.Bits()))
	out = append(out, byte(metric))
	return out
}

func decodeRouteAdd(b []byte) (origin string, prefix netip.Prefix, metric int, ok bool) {
	r := reader{b: b}
	origin, ok = r.lenStr()
	if !ok {
		return "", netip.Prefix{}, 0, false
	}
	fam, ok := r.byte()
	if !ok {
		return "", netip.Prefix{}, 0, false
	}
	var addr netip.Addr
	switch fam {
	case 4:
		v, ok := r.take(4)
		if !ok {
			return "", netip.Prefix{}, 0, false
		}
		addr = netip.AddrFrom4([4]byte{v[0], v[1], v[2], v[3]})
	case 6:
		v, ok := r.take(16)
		if !ok {
			return "", netip.Prefix{}, 0, false
		}
		var a [16]byte
		copy(a[:], v)
		addr = netip.AddrFrom16(a)
	default:
		return "", netip.Prefix{}, 0, false
	}
	bits, ok := r.byte()
	if !ok {
		return "", netip.Prefix{}, 0, false
	}
	m, ok := r.byte()
	if !ok {
		return "", netip.Prefix{}, 0, false
	}
	pfx, err := addr.Prefix(int(bits))
	if err != nil {
		return "", netip.Prefix{}, 0, false
	}
	return origin, pfx, int(m), true
}

// shouldReadvertise gates periodic route re-flooding.
func (ns *netState) shouldReadvertiseHosts(now time.Time, interval time.Duration) bool {
	hs := ns.advHosts.Load()
	if hs == nil || len(*hs) == 0 {
		return false
	}
	if now.Sub(ns.lastHostAdv) < interval {
		return false
	}
	ns.lastHostAdv = now
	return true
}

func (ns *netState) shouldReadvertiseDNS(now time.Time, interval time.Duration) bool {
	ds := ns.advDNS.Load()
	if ds == nil || len(*ds) == 0 {
		return false
	}
	if now.Sub(ns.lastDNSAdv) < interval {
		return false
	}
	ns.lastDNSAdv = now
	return true
}

// shouldReadvertise reports whether it's time to re-flood this network's
// currently-advertised routes — both the config-driven set (advRoutes) and
// anything currently redistributed from BGP (bgpRoutes, see bgpRedistSet's
// doc comment) — as a guard against a lost flood packet quietly leaving a
// peer without a route it should have. False when there is nothing at all to
// (re)advertise from either source, so a node with no redistributed routes
// isn't paying for the interval check every tick for nothing.
func (ns *netState) shouldReadvertise(now time.Time, interval time.Duration) bool {
	rs := ns.advRoutes.Load()
	br := ns.bgpRoutes.Load()
	haveAdv := rs != nil && len(*rs) > 0
	haveBGP := br != nil && len(br.routes) > 0
	if !haveAdv && !haveBGP {
		return false
	}
	if now.Sub(ns.lastRouteAdv) < interval {
		return false
	}
	ns.lastRouteAdv = now
	return true
}
