package mesh

// Full-tunnel peer-bypass host routes. See routes.go's doc comments on the
// default RouteRej entry for the problem this solves: a peer-advertised
// 0.0.0.0/0 (or ::/0), once accepted, would otherwise loop the mesh's own
// underlay traffic into the very tunnel that traffic is what keeps up in the
// first place. A /32 (or /128) host route to a peer's real underlay endpoint
// always wins longest-prefix-match against any less specific route —
// including a literal /0 or a /1 split — so keeping one live for every
// underlay address the mesh actually dials is what keeps this node's own
// mesh traffic out of its own tunnel, on every platform, using the same
// AddRoute-style mechanism the mesh already relies on for everything else it
// puts in the OS routing table.
//
// Two independent things need a bypass route to the same address at
// different times, sometimes overlapping: a seed (dialed before any session
// exists, see syncSeedBypassRoutes) and the peer session that seed
// eventually becomes (see syncPeerBypassRoute) — and while both are live at
// once (a session just completed but the now-stale seed entry hasn't been
// pruned yet), neither should be able to delete the other's still-needed
// route. Engine.bypassRefs is the shared, reference-counted source of truth
// that makes that safe: acquireBypassRoute/releaseBypassRoute, not a direct
// Add/DelGatewayRoute call, are the only way anything in this package
// touches one of these routes.
//
// Acquisition is gated (see acquireBypassRoute) on something actually
// threatening to capture underlay traffic to the address: ns.fullTunnel, or
// — since v552 — a mesh-installed kernel route covering it
// (netState.meshRouteCovers). With neither in play every acquire call here
// is a no-op, so wiring these calls into the session and seed lifecycle
// doesn't change behavior for a network with no capturing routes. Release always proceeds regardless of fullTunnel's current value — a
// route acquired while it was on must still be releasable after it's turned
// off. The trigger that actually turns fullTunnel on — accepting a
// peer-advertised default route and installing it (literally, at its
// advertised metric — see routes.go's syncFullTunnelRoute for why a literal
// /0 is fine now that this file's /32 bypass routes exist) — is separate,
// later work; this is the machinery that keeps that safe once it exists.
//
// Swappable via package vars (rather than a new Engine-level injected
// interface, unlike Device/Sender) so tests can fake gateway/route behavior
// without a bigger constructor-injection refactor. internal/tun is
// otherwise never imported by this package — this file is the one
// exception, kept to a narrow surface (three functions) for exactly that
// reason.

import (
	"fmt"
	"net/netip"
	"syscall"

	"gravinet/internal/tun"
)

var (
	defaultGatewayFn  = tun.DefaultGateway
	addGatewayRouteFn = tun.AddGatewayRoute
	delGatewayRouteFn = tun.DelGatewayRoute
	// gatewaySupported mirrors tun.GatewaySupported by default (true on
	// Linux, false everywhere else today) but as a package var, not the
	// constant directly, so a test running on any one platform can still
	// exercise both the "supported" and "not supported yet" branches of
	// syncFullTunnelRoute's hard guard (routes.go) without needing to
	// actually run on four different operating systems.
	gatewaySupported = tun.GatewaySupported
	// demoteDefaultRouteFn and routeDemotionNeeded back
	// demotePhysicalDefaultRoute/restorePhysicalDefaultRoute below — package
	// vars, not tun.DemoteDefaultRoute/tun.RouteDemotionNeeded directly, for
	// the same test-swappability reason as defaultGatewayFn/gatewaySupported
	// above.
	demoteDefaultRouteFn = tun.DemoteDefaultRoute
	routeDemotionNeeded  = tun.RouteDemotionNeeded
)

// demotedDefaultMetric is the metric demotePhysicalDefaultRoute reprograms
// a platform's pre-existing physical default route to, immediately before
// syncFullTunnelRoute installs the mesh's own literal default route
// alongside it. Gated on routeDemotionNeeded, which every platform gravinet
// has a real gateway backend for sets true as of v317 (see each platform's
// gateway_*.go RouteDemotionNeeded doc comment — Linux's explains why it
// opted in even though its kernel doesn't strictly require it); chosen high
// enough that any plausible peer-advertised metric for the mesh's own
// default route — see syncFullTunnelRoute's advertised metric, ordinarily a
// small number — will sit below it once installed.
const demotedDefaultMetric = 1000

// bypassMetric is the metric a peer-bypass host route is installed with.
// Irrelevant to correctness — a /32 or /128 always wins longest-prefix-match
// against any less specific route regardless of metric — but
// AddGatewayRoute needs some value, and 0 (kernel/OS default) is as good as
// any.
const bypassMetric = 0

// bypassRef is one address's entry in Engine.bypassRefs: how many
// independent trackers currently need a route to it, and — captured once,
// at the moment the route was actually installed — exactly which
// gateway/interface it was installed via. Release reuses that same
// gateway/ifIndex rather than re-resolving the physical gateway fresh, so a
// gateway change between acquire and release (the host's network changed
// mid-session) can't produce a delete that targets a different route than
// the one actually in the table — it would just fail to match and leave a
// stale route behind instead, a strictly safer failure than deleting the
// wrong thing.
type bypassRef struct {
	count   int
	gateway netip.Addr
	ifIndex int32
}

// acquireBypassRoute records one more reference to addr's bypass route,
// installing it for real (via AddGatewayRoute) only on the first reference.
// A no-op unless something on this network actually threatens to capture
// underlay traffic to addr: ns.fullTunnel (the original trigger — an
// accepted default route captures *every* underlay address), or, since
// v552, any narrower mesh-installed kernel route that covers addr
// specifically (see meshRouteCovers in routes.go — a redistributed /24
// containing a peer's real endpoint loops that peer's underlay traffic into
// the tunnel exactly the way an unprotected /0 would, and needs exactly the
// same /32 escape hatch). ns supplies the gateway-lookup context (which tun
// device's ifindex to exclude) only when this is the first reference —
// later callers acquiring an address another tracker already holds don't
// re-resolve anything.
func (e *Engine) acquireBypassRoute(ns *netState, addr netip.Addr) {
	if !addr.IsValid() {
		return
	}
	if !ns.fullTunnel.Load() && !ns.meshRouteCovers(addr) {
		return
	}
	if !gatewaySupported {
		// Reachable only via the meshRouteCovers path: full-tunnel refuses
		// to activate at all on a platform with no gateway backend (see
		// syncFullTunnelRoute's hard guard), but an ordinary redistributed
		// route installs everywhere. No bypass is possible here — the
		// dataplane loop guard (isUnderlayLoop) is what protects such a
		// platform, and it logs its own WARN with the diagnosis.
		e.log.Debugf("mesh: underlay endpoint %s on net %x is covered by a mesh-installed route, but this platform has no bypass-route backend (see internal/tun/gateway_unsupported.go)", addr, ns.spec.ID)
		return
	}
	e.bypassMu.Lock()
	defer e.bypassMu.Unlock()
	if ref, ok := e.bypassRefs[addr]; ok {
		ref.count++
		e.bypassRefs[addr] = ref
		return
	}
	gw, ifIndex, err := e.physicalGateway(ns, addr)
	if err != nil {
		e.log.Debugf("mesh: full-tunnel bypass route for %s on net %x: %v", addr, ns.spec.ID, err)
		return
	}
	p := netip.PrefixFrom(addr, addr.BitLen())
	if err := addGatewayRouteFn(p, gw, ifIndex, bypassMetric); err != nil {
		e.log.Warnf("mesh: install full-tunnel bypass route %s via %s on net %x: %v", p, gw, ns.spec.ID, err)
		return // don't record a reference to a route that isn't actually there
	}
	e.bypassRefs[addr] = bypassRef{count: 1, gateway: gw, ifIndex: ifIndex}
}

// releaseBypassRoute drops one reference to addr's full-tunnel bypass
// route, removing it for real (via DelGatewayRoute) only once every tracker
// that acquired it has released it. A no-op for an address with no
// recorded reference (including one acquireBypassRoute silently declined to
// install in the first place, e.g. because the gateway couldn't be
// resolved) — always safe to call speculatively. Proceeds regardless of
// ns.fullTunnel's current value, by design (see package doc comment).
func (e *Engine) releaseBypassRoute(addr netip.Addr) {
	if !addr.IsValid() {
		return
	}
	e.bypassMu.Lock()
	ref, ok := e.bypassRefs[addr]
	if !ok {
		e.bypassMu.Unlock()
		return
	}
	ref.count--
	if ref.count > 0 {
		e.bypassRefs[addr] = ref
		e.bypassMu.Unlock()
		return
	}
	delete(e.bypassRefs, addr)
	e.bypassMu.Unlock()

	p := netip.PrefixFrom(addr, addr.BitLen())
	if err := delGatewayRouteFn(p, ref.gateway, ref.ifIndex, bypassMetric); err != nil {
		e.log.Debugf("mesh: remove full-tunnel bypass route %s via %s: %v", p, ref.gateway, err)
	}
}

// syncPeerBypassRoute acquires or releases ps's peer-bypass host route to
// match its current reachability: a reference on ps.endpoint if ps is
// reached directly (ps.relay == nil), its endpoint is known, and something
// on this network would otherwise capture underlay traffic to it
// (full-tunnel, or since v552 any mesh-installed route covering the
// endpoint — see acquireBypassRoute); none otherwise. A relayed peer isn't dialed at its own advertised endpoint, so
// there's nothing of its own to bypass — the relay session it goes through
// gets its own reference via this same function, since a relay is itself
// always a direct peer session. Safe to call from any point in the
// peer-session lifecycle (install, roam, teardown): it always reconciles
// against ps.bypassAddr rather than assuming what's currently held, so
// calling it when nothing actually changed is a cheap no-op.
func (e *Engine) syncPeerBypassRoute(ns *netState, ps *peerSession) {
	ps.mu.Lock()
	var ep netip.AddrPort
	if ps.relay == nil {
		ep = ps.endpoint
	}
	have := ps.bypassAddr
	ps.mu.Unlock()

	// The coverage check runs after ps.mu is released: meshRouteCovers takes
	// ns.osMu, and nothing should hold a session lock across a route-table
	// lock. Since v552 a bypass is wanted not only under full-tunnel but
	// also whenever a narrower mesh-installed route covers this peer's
	// endpoint — see acquireBypassRoute's doc comment for the loop that
	// guards against.
	var want netip.Addr
	if ep.IsValid() && (ns.fullTunnel.Load() || ns.meshRouteCovers(ep.Addr())) {
		want = ep.Addr().Unmap()
	}

	if have == want {
		return
	}
	if have.IsValid() {
		e.releaseBypassRoute(have)
	}
	if want.IsValid() {
		e.acquireBypassRoute(ns, want)
	}

	ps.mu.Lock()
	ps.bypassAddr = want
	ps.mu.Unlock()
}

// removePeerBypassRoute releases whatever bypass reference ps currently
// holds — called on teardown (localDisconnect, pruneDead).
func (e *Engine) removePeerBypassRoute(ns *netState, ps *peerSession) {
	ps.mu.Lock()
	have := ps.bypassAddr
	ps.bypassAddr = netip.Addr{}
	ps.mu.Unlock()
	if have.IsValid() {
		e.releaseBypassRoute(have)
	}
}

// resyncAllBypassRoutes re-syncs every live peer session's bypass route on
// ns against the current value of ns.fullTunnel — called by
// syncFullTunnelRoute right after that flag flips, in either direction, so
// already-connected peers don't have to wait for a roam or re-handshake to
// pick up a newly-turned-on full-tunnel default, and don't keep a
// now-pointless route lingering once it's turned back off.
func (e *Engine) resyncAllBypassRoutes(ns *netState) {
	ns.mu.RLock()
	sessions := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		sessions = append(sessions, ps)
	}
	ns.mu.RUnlock()
	for _, ps := range sessions {
		e.syncPeerBypassRoute(ns, ps)
	}
}

// syncSeedBypassRoutes reconciles ns's seed-driven bypass references against
// its current seed lists (ns.seeds and ns.tcpSeeds — both dialed directly,
// before any session exists to cover them the way syncPeerBypassRoute does)
// so a seed address is never dialed while a full-tunnel default could
// swallow that very dial. Only the address matters, not the port — a TCP
// fallback dial and its UDP seed share one /32 automatically, and a
// resolved fallback address (same IP, different port; see seedFallback's
// doc comment) needs nothing extra here for the same reason. Meant to be
// called once per initLoop tick, right before that tick's seeds are dialed.
func (e *Engine) syncSeedBypassRoutes(ns *netState) {
	ns.mu.RLock()
	want := make(map[netip.Addr]bool, len(ns.seeds)+len(ns.tcpSeeds))
	for _, s := range ns.seeds {
		if s.Addr().IsValid() {
			want[s.Addr()] = true
		}
	}
	for _, s := range ns.tcpSeeds {
		if s.Addr().IsValid() {
			want[s.Addr()] = true
		}
	}
	ns.mu.RUnlock()

	ns.seedBypassMu.Lock()
	if ns.seedBypassHeld == nil {
		ns.seedBypassHeld = map[netip.Addr]bool{}
	}
	var toAcquire, toRelease []netip.Addr
	for a := range want {
		if !ns.seedBypassHeld[a] {
			toAcquire = append(toAcquire, a)
		}
	}
	for a := range ns.seedBypassHeld {
		if !want[a] {
			toRelease = append(toRelease, a)
		}
	}
	for _, a := range toAcquire {
		ns.seedBypassHeld[a] = true
	}
	for _, a := range toRelease {
		delete(ns.seedBypassHeld, a)
	}
	ns.seedBypassMu.Unlock()

	for _, a := range toAcquire {
		e.acquireBypassRoute(ns, a)
	}
	for _, a := range toRelease {
		e.releaseBypassRoute(a)
	}
}

// physicalGWCache is what physicalGateway caches per address family once
// resolved for a network's current full-tunnel activation — see its doc
// comment for why a fresh live-table lookup on every call isn't safe here.
type physicalGWCache struct {
	addr    netip.Addr
	ifIndex int32
}

// physicalGateway resolves the real (non-tunnel) gateway/interface to route
// addr's bypass host route through, excluding ns's own tun device so a
// lookup after the full-tunnel default is already installed never finds
// gravinet's own route instead of the genuine physical one.
//
// Cached per address family in ns.physicalGW after the first successful
// resolution, rather than re-querying the live routing table on every
// call: on every routeDemotionNeeded platform (FreeBSD, OpenBSD, macOS,
// Windows), demotePhysicalDefaultRoute doesn't just deprioritize the
// physical default route, it actually removes it from the table (see each
// platform's gateway_*.go DemoteDefaultRoute doc comment for why a
// dst/mask key can't just be reprogrammed in place on those kernels). Once
// that's happened, a live lookup excluding this network's own tun finds
// nothing at all — the physical route genuinely isn't there anymore — so
// re-resolving fresh on every acquireBypassRoute call would silently fail
// every bypass-route request that comes after the very first one:
// exactly the shape of bug a field report caught (default route
// correctly captured, but no peer bypass routes at all, breaking
// connectivity, because every acquisition after the initial demotion
// found no physical gateway to route through). The cache is what makes
// this safe: demotePhysicalDefaultRoute warms it (see that function's own
// doc comment) before the physical route is removed, and every later
// acquireBypassRoute call for this network, for the remainder of this
// full-tunnel activation, reuses that same cached value instead of
// re-asking a table that can no longer answer. restorePhysicalDefaultRoute
// clears it on withdrawal, so a later re-activation resolves fresh rather
// than reusing a gateway from however long ago full-tunnel was last on —
// the one real cost of caching instead of re-resolving: a physical
// gateway that changes (a roam to different Wi-Fi, a new DHCP lease) while
// full-tunnel stays continuously active won't be picked up until the next
// deactivate/reactivate cycle. That's judged an acceptable trade-off
// against the alternative of bypass routes not working at all.
//
// Known limitation, unrelated to caching: if more than one network on
// this node is running full-tunnel at once, only the calling network's
// own tun is excluded from the live lookup that seeds the cache — a
// second network's own full-tunnel default route could still be mistaken
// for the physical default in that specific multi-network configuration.
func (e *Engine) physicalGateway(ns *netState, addr netip.Addr) (gw netip.Addr, ifIndex int32, err error) {
	family := syscall.AF_INET
	if addr.Is6() {
		family = syscall.AF_INET6
	}

	ns.osMu.Lock()
	if c, ok := ns.physicalGW[family]; ok {
		ns.osMu.Unlock()
		return c.addr, c.ifIndex, nil
	}
	ns.osMu.Unlock()

	if ns.spec.Dev == nil {
		return netip.Addr{}, 0, fmt.Errorf("no overlay device configured on this network")
	}
	tunIdx, err := ns.dev().IfIndex()
	if err != nil {
		return netip.Addr{}, 0, err
	}
	g, err := defaultGatewayFn(family, tunIdx)
	if err != nil {
		return netip.Addr{}, 0, err
	}
	ns.osMu.Lock()
	ns.physicalGW[family] = physicalGWCache{addr: g.Addr, ifIndex: g.IfIndex}
	ns.osMu.Unlock()
	return g.Addr, g.IfIndex, nil
}

// demotePhysicalDefaultRoute deprioritizes ns's pre-existing physical
// default route (the same one physicalGateway resolves — i.e. not
// gravinet's own tun) to demotedDefaultMetric, and records the metric it
// had before that in ns.demotedGatewayMetric so restorePhysicalDefaultRoute
// can put it back once the mesh's own default route is later withdrawn. A
// no-op unless routeDemotionNeeded is true for this platform — every
// platform gravinet has a real gateway backend for, as of v317 (see each
// platform's gateway_*.go RouteDemotionNeeded doc comment; only
// gateway_unsupported.go's forward-compatibility stub leaves it false, and
// that's moot since GatewaySupported being false there already keeps
// full-tunnel from activating at all). FreeBSD, OpenBSD, macOS, and Windows
// need this because installing gravinet's own default route "alongside"
// the physical one isn't enough on its own to make it win there — see each
// platform's gateway_*.go DemoteDefaultRoute doc comment for why. Linux's
// kernel doesn't have that problem (two default routes at different
// metrics already coexist fine, lower metric winning, the way
// syncFullTunnelRoute's own doc comment describes), but demotes the
// physical route here too anyway, for one consistent sequence across every
// platform rather than Linux being the one exception — see
// gateway_linux.go's RouteDemotionNeeded doc comment.
//
// Called only once per activation (syncFullTunnelRoute gates this on
// !installed, i.e. only on the transition into full-tunnel, not on every
// later metric change) — and, since v323, only after confirming against the
// live routing table that the physical default route isn't already sitting
// at demotedDefaultMetric. That live check (not just "is anything recorded
// for p") is what lets this stay correct across both cases that call it
// again after the first activation: a resume replay (reassertOSState
// re-driving syncRoute after sleep/wake) finds the exact same route still
// demoted from before and must leave ns.demotedGatewayMetric alone, while a
// genuine network change (Wi-Fi roam, new DHCP lease, cellular failover)
// presents a brand-new, undemoted route — trusting the old presence-only
// check on that path meant it silently skipped forever, since nothing short
// of a restart ever cleared the stale record, leaving that fresh physical
// route to compete with the mesh's own default indefinitely. See v323's
// changelog entry for the field report this came from.
//
// Failure here is only logged, not fatal: leaving the physical route at
// whatever metric it already had just means this platform is back to
// undefined dual-default-route behavior, the same as if this function
// didn't exist — not a state any worse than before full-tunnel was
// attempted.
func (e *Engine) demotePhysicalDefaultRoute(ns *netState, p netip.Prefix) {
	if !routeDemotionNeeded {
		return
	}
	if ns.spec.Dev == nil {
		return
	}
	tunIdx, err := ns.dev().IfIndex()
	if err != nil {
		e.log.Warnf("mesh: full-tunnel default route on net %x: resolve tun ifindex for route demotion: %v", ns.spec.ID, err)
		return
	}
	family := syscall.AF_INET
	if p.Addr().Is6() {
		family = syscall.AF_INET6
	}

	// Live check, not a presence-only one: see the doc comment above for why.
	gw, gerr := defaultGatewayFn(family, tunIdx)
	if gerr != nil {
		// Can't confirm current state — don't risk recording an
		// already-demoted metric as if it were the original. Whatever
		// called us (activation, or a resync after a detected network
		// change) will get another chance on its own next trigger; this
		// isn't the only caller and isn't a one-shot opportunity.
		e.log.Debugf("mesh: full-tunnel default route on net %x: check current physical default route before demoting: %v", ns.spec.ID, gerr)
		return
	}
	if gw.Metric == demotedDefaultMetric {
		return // same route, already demoted — leave the recorded original alone
	}

	// The physical route here is either one we've never seen or one that
	// replaced whatever we last demoted (a network change) — either way,
	// physicalGateway's cache for this family is now describing a gateway
	// that's stale or gone, so drop it before re-warming below rather than
	// handing acquireBypassRoute a bypass route to nowhere.
	ns.osMu.Lock()
	delete(ns.physicalGW, family)
	ns.osMu.Unlock()

	// Warm physicalGateway's cache before the physical route is removed
	// below — this is the one point in the activation sequence guaranteed
	// to run regardless of whether any live peer or seed has actually
	// called acquireBypassRoute yet (resyncAllBypassRoutes/
	// syncSeedBypassRoutes normally already have, since syncFullTunnelRoute
	// runs them first — but relying on that alone leaves a gap for a
	// network with no live peer or seed at the exact moment full-tunnel
	// activates). Best-effort: if this fails, acquireBypassRoute's own
	// later attempt will hit the same failure and log it there instead,
	// same as before this cache existed.
	if _, _, err := e.physicalGateway(ns, p.Addr()); err != nil {
		e.log.Debugf("mesh: full-tunnel default route on net %x: pre-resolve physical gateway ahead of demotion: %v", ns.spec.ID, err)
	}
	old, err := demoteDefaultRouteFn(family, tunIdx, demotedDefaultMetric)
	if err != nil {
		e.log.Warnf("mesh: full-tunnel default route on net %x: demote existing default route to metric %d: %v", ns.spec.ID, demotedDefaultMetric, err)
		return
	}
	ns.osMu.Lock()
	ns.demotedGatewayMetric[p] = old
	ns.osMu.Unlock()
	e.log.Infof("mesh: existing default route on net %x demoted to metric %d ahead of full-tunnel install (was %d)", ns.spec.ID, demotedDefaultMetric, old)
}

// restorePhysicalDefaultRoute reverses demotePhysicalDefaultRoute once
// gravinet's own full-tunnel default route for p is withdrawn, putting the
// physical default route back at whatever metric it had before it was
// demoted, rather than leaving it pinned at demotedDefaultMetric
// permanently. A no-op if nothing was ever recorded as demoted for p —
// routeDemotionNeeded false (only gateway_unsupported.go's platforms,
// where full-tunnel never activates in the first place), or the original
// demotion attempt itself failed and never recorded one.
func (e *Engine) restorePhysicalDefaultRoute(ns *netState, p netip.Prefix) {
	ns.osMu.Lock()
	old, ok := ns.demotedGatewayMetric[p]
	if ok {
		delete(ns.demotedGatewayMetric, p)
	}
	ns.osMu.Unlock()
	if !ok {
		return
	}
	if ns.spec.Dev == nil {
		return
	}
	tunIdx, err := ns.dev().IfIndex()
	if err != nil {
		e.log.Warnf("mesh: full-tunnel default route on net %x: resolve tun ifindex to restore demoted route: %v", ns.spec.ID, err)
		return
	}
	family := syscall.AF_INET
	if p.Addr().Is6() {
		family = syscall.AF_INET6
	}
	ns.osMu.Lock()
	delete(ns.physicalGW, family)
	ns.osMu.Unlock()
	if _, err := demoteDefaultRouteFn(family, tunIdx, old); err != nil {
		e.log.Warnf("mesh: full-tunnel default route on net %x: restore demoted default route to metric %d: %v", ns.spec.ID, old, err)
		return
	}
	e.log.Infof("mesh: existing default route on net %x restored to metric %d after full-tunnel withdrawal", ns.spec.ID, old)
}
