package mesh

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Control message types carried inside the encrypted session (innerCtrl frames).
const (
	ctrlPeerList      = 0x01 // a list of known peers, for auto full-mesh
	ctrlPing          = 0x02 // NAT keepalive / liveness probe
	ctrlPong          = 0x03 // reply to ping
	ctrlDADQuery      = 0x04 // "is this overlay address in use?"
	ctrlDADDefend     = 0x05 // "yes, that address is taken"
	ctrlAddrNotify    = 0x06 // "my overlay address is now X"
	ctrlBanAdd        = 0x07 // distributed ban (flooded)
	ctrlBanDel        = 0x08 // distributed unban (flooded; honored from origin)
	ctrlRouteAdd      = 0x09 // redistributed route (flooded)
	ctrlRouteDel      = 0x0a // route withdrawal (flooded; honored from origin)
	ctrlReflexive     = 0x0b // "I observe you at address X" (STUN-style NAT discovery)
	ctrlHostAdd       = 0x0c // advertised custom hosts-file record (flooded)
	ctrlHostDel       = 0x0d // withdrawal of a custom hosts record (flooded)
	ctrlDNSAdd        = 0x0e // advertised conditional DNS-forward domain (flooded)
	ctrlDNSDel        = 0x0f // withdrawal of a conditional DNS-forward domain (flooded)
	ctrlKeyAdd        = 0x10 // rotated/distributed network key sent to members (flooded)
	ctrlKeyDel        = 0x11 // retraction of a previously-distributed key (flooded; honored by identity)
	ctrlPeerAdd       = 0x12 // single-peer gossip announcement (see announcePeerChange)
	ctrlClusterNotify = 0x13 // "my managed/manager status just changed" (see announceClusterState)
)

// peerEntry is one node advertised in a gossip message.
type peerEntry struct {
	nodeID   string
	hostname string
	overlay4 netip.Addr
	overlay6 netip.Addr
	endpoint netip.AddrPort
	managed  bool   // peer advertises remote management (propagated mesh-wide via gossip)
	manager  bool   // peer advertises Manager mode (propagated mesh-wide via gossip)
	webPort  uint16 // peer's web-admin port, so non-neighbors can manage it too
	tcpPort  uint16 // peer's TCP/TLS fallback port, so non-neighbors can dial it when UDP fails
	// extraTCPPorts/extraUDPPorts are the peer's additional listen ports
	// (config extra_tcp_listen_ports/extra_listen_ports), propagated the
	// same way tcpPort is so non-neighbors learn about them too — see
	// peerListExtraTCPBlock/peerListExtraUDPBlock.
	extraTCPPorts []uint16
	extraUDPPorts []uint16
	// localEndpoints are the peer's self-declared host candidates (its own LAN
	// interface addresses), relayed onward from what it advertised in its
	// handshake — see hsPayload.LocalEndpoints. This is the leg that matters:
	// two nodes behind one NAT never observe each other at all, so neither can
	// ever learn the other's LAN address first-hand. It can only reach them
	// through a mutual peer's gossip, which is this field.
	localEndpoints []netip.AddrPort
	// version is the peer's build version, relayed onward from what it
	// advertised in its handshake — see hsPayload.Version. Propagated
	// through gossip (unlike bgpASN) specifically because ManagedPeers
	// includes peers known only indirectly, and those are exactly the rows
	// that would otherwise show a blank version.
	version string
}

// ---- control dispatch ----

func (e *Engine) onControl(ps *peerSession, body []byte) {
	if len(body) == 0 {
		return
	}
	switch body[0] {
	case ctrlPing:
		e.sendControl(ps, []byte{ctrlPong})
	case ctrlPong:
		// liveness already recorded by touch() on receipt; also close out the
		// RTT measurement armed by sendKeepalive. sent==0 means we never
		// pinged this session (shouldn't happen post-install, but a pong from
		// a session that hasn't sent a ping yet is more likely a stray/replay
		// than a real sample) — skip rather than record garbage. A negative
		// delta (clock oddity) is discarded the same way.
		if sent := ps.pingSentNanos.Load(); sent != 0 {
			if rtt := time.Now().UnixNano() - sent; rtt > 0 {
				ps.rttNanos.Store(rtt)
			}
		}
	case ctrlPeerList:
		entries, err := decodePeerList(body[1:])
		if err != nil {
			return
		}
		e.learnPeers(ps, entries)
	case ctrlPeerAdd:
		// Same wire shape as ctrlPeerList (a peerEntry slice — just always
		// length 1 in practice), so it reuses decodePeerList/learnPeers
		// unchanged: a single-entry "list" upserts exactly like a full one.
		// An older peer that doesn't recognize this message type falls
		// through the switch below with no default case and simply ignores
		// it, relying solely on ctrlPeerList's periodic broadcast — no
		// version negotiation needed for this to degrade safely.
		entries, err := decodePeerList(body[1:])
		if err != nil {
			return
		}
		e.learnPeers(ps, entries)
	case ctrlDADQuery:
		e.onDADQuery(ps, body[1:])
	case ctrlDADDefend:
		e.onDADDefend(ps, body[1:])
	case ctrlAddrNotify:
		e.onAddrNotify(ps, body[1:])
	case ctrlBanAdd:
		e.onBanAdd(ps, body[1:])
	case ctrlBanDel:
		e.onBanDel(ps, body[1:])
	case ctrlRouteAdd:
		e.onRouteAdd(ps, body[1:])
	case ctrlRouteDel:
		e.onRouteDel(ps, body[1:])
	case ctrlReflexive:
		e.onReflexive(ps, body[1:])
	case ctrlHostAdd:
		e.onHostAdd(ps, body[1:])
	case ctrlHostDel:
		e.onHostDel(ps, body[1:])
	case ctrlDNSAdd:
		e.onDNSAdd(ps, body[1:])
	case ctrlDNSDel:
		e.onDNSDel(ps, body[1:])
	case ctrlKeyAdd:
		e.onKeyAdd(ps, body[1:])
	case ctrlKeyDel:
		e.onKeyDel(ps, body[1:])
	case ctrlClusterNotify:
		e.onClusterNotify(ps, body[1:])
	}
}

// floodControl re-broadcasts a control message to every peer except the one it
// arrived on (used for epidemic propagation of bans and routes).
func (e *Engine) floodControl(ns *netState, ctrl []byte, except *peerSession) {
	ns.mu.RLock()
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		if ps != except {
			peers = append(peers, ps)
		}
	}
	ns.mu.RUnlock()
	for _, ps := range peers {
		e.sendControl(ps, ctrl)
	}
}

func (e *Engine) sendControl(ps *peerSession, ctrl []byte) {
	e.sealAndSend(ps, innerCtrl, ctrl)
}

// learnPeers records advertised nodes and dials any we are not yet connected to,
// driving the mesh toward full connectivity. The advertising peer (ps) is noted
// as a relay candidate for each node it reports.
func (e *Engine) learnPeers(ps *peerSession, entries []peerEntry) {
	ns := ps.net
	reported := make([]string, 0, len(entries))
	for _, en := range entries {
		if en.nodeID != "" {
			reported = append(reported, en.nodeID)
		}
	}
	ps.markReported(reported)

	now := time.Now()
	for _, en := range entries {
		if en.nodeID == "" || en.nodeID == e.nodeID {
			continue // never connect to ourselves
		}
		if ns.isBanned(en.nodeID) || ns.isPeerDisabled(en.nodeID) {
			continue // don't dial banned or locally-disabled nodes
		}
		ns.mu.Lock()
		ni := ns.nodes[en.nodeID]
		if ni == nil {
			ni = &nodeInfo{nodeID: en.nodeID}
			ns.nodes[en.nodeID] = ni
		}
		ni.hostname = en.hostname
		if en.overlay4.IsValid() {
			ni.overlay4 = en.overlay4
		}
		if en.overlay6.IsValid() {
			ni.overlay6 = en.overlay6
		}
		if en.endpoint.IsValid() {
			ni.endpoint = en.endpoint
		}
		ni.lastSeen = now
		existing, connected := ns.byNode[en.nodeID]
		// Distinct from plain "connected": true only once we hold a session
		// that isn't relayed. A relay-only connection still wants fresh
		// endpoint candidates below, so initLoop keeps getting a chance to
		// upgrade to direct — see the seed-registration block's own comment.
		directlyConnected := connected && existing.getRelay() == nil

		// Managed status propagates mesh-wide via gossip so peers we aren't
		// directly connected to are still manageable. For a direct neighbor the
		// handshake is authoritative, so don't let second-hand gossip override it.
		if !connected {
			ni.managed = en.managed
			ni.manager = en.manager
			ni.webPort = en.webPort
			// Only overwrite with a version actually reported: a peer
			// relaying an entry it learned before the version block
			// existed sends "", and blanking a version we already knew
			// from a fresher source would be a regression, not an
			// update. Same shape as the tcpPort/extraPorts guards below.
			if en.version != "" {
				ni.version = en.version
			}
			if en.tcpPort != 0 {
				ni.tcpPort = en.tcpPort
			}
			if len(en.extraTCPPorts) > 0 {
				ni.extraTCPPorts = en.extraTCPPorts
			}
			if len(en.extraUDPPorts) > 0 {
				ni.extraUDPPorts = en.extraUDPPorts
			}
		}

		// Address-conflict backstop: if a peer claims an overlay address we hold
		// and wins the deterministic tie-break (lower node id), relinquish ours
		// and re-pick. DAD prevents most conflicts; this catches races during
		// mesh formation.
		conflict := false
		if en.overlay4.IsValid() && en.overlay4 == ns.self4 && en.nodeID < e.nodeID {
			ns.self4 = netip.Addr{}
			conflict = true
		}
		if en.overlay6.IsValid() && en.overlay6 == ns.self6 && en.nodeID < e.nodeID {
			ns.self6 = netip.Addr{}
			conflict = true
		}
		ns.mu.Unlock()

		if conflict {
			e.log.Warnf("mesh: overlay address conflict with %q on net %x; re-assigning", en.nodeID, ns.spec.ID)
			e.maybeAssignAddress(ns)
		}
		if !directlyConnected && en.endpoint.IsValid() {
			e.AddSeedFor(ns.spec.ID, en.endpoint, en.nodeID) // initLoop dials it next tick
			// Extra UDP ports (config extra_listen_ports on the peer's end)
			// ride the same seed pool rather than a separate dial mechanism:
			// each becomes its own seed candidate at the peer's address, so
			// initLoop's existing retry/backoff/dedup already tries all of
			// them, no new parallel-dial logic needed the way ensureFallback
			// (TCP) required — UDP's primary is learned automatically by
			// observing which port a packet arrives from, but an extra port
			// nobody's dialed yet has no such signal, hence this explicit
			// seeding. Skipped once directly connected: its working port is
			// already known and extra candidates for this peer add nothing
			// — but kept going while only relayed, same as the primary
			// endpoint just above, so a relay-only peer's extra ports stay
			// available as direct-upgrade candidates too.
			for _, extra := range en.extraUDPPorts {
				if extra != en.endpoint.Port() {
					e.AddSeedFor(ns.spec.ID, netip.AddrPortFrom(en.endpoint.Addr(), extra), en.nodeID)
				}
			}
		}
		// Host candidates (the peer's own LAN addresses, self-declared and
		// relayed to us by whoever is connected to it — see localcand.go).
		// Registered even when we're *directly* connected already, unlike the
		// observed-endpoint seeding above: that block skips a directly-connected
		// peer because its working address is already known and more candidates
		// for it add nothing. These are different. A node behind the same NAT as
		// us is exactly the case where the observed endpoint is the shared public
		// address and the LAN candidate is the only one that can ever produce a
		// direct path — and it is also the case where we are most likely to be
		// sitting on a working *relayed* session, which counts as "connected" and
		// would skip the candidate that fixes it. So: always register, and let
		// initSeedTick's own connectedTo/connectedToSeedOwner checks decide
		// whether there's anything left to try (they no-op cheaply when the peer
		// is already direct).
		if len(en.localEndpoints) > 0 {
			e.addLocalCandidates(ns.spec.ID, en.nodeID, en.localEndpoints)
		}
	}
}

// ---- gossip ----

// announcePeerChange floods a single-peer ctrlPeerAdd to every other directly
// connected peer on ns, and queues peerAddResends extra redundant re-floods
// (flushPendingPeerAdds, driven from maintLoop) so this specific change
// doesn't depend on one UDP datagram arriving. Called from install() on every
// (re)connection of ps — new peer, roam, or reconnect — since any of those is
// exactly when this node's own knowledge of ps (hostname, overlay addrs,
// endpoint, managed/manager/webPort/tcpPort) could have just changed. No field-by-
// field diff against the previous state is needed: install() itself is rare
// compared to a maintenance tick, so treating every install() as gossip-
// worthy costs little and can't miss a real change by mis-comparing.
func (e *Engine) announcePeerChange(ns *netState, ps *peerSession) {
	e.floodSinglePeer(ns, ps)
	ns.mu.Lock()
	ns.pendingPeerAdds[ps.nodeID] = peerAddResends
	ns.mu.Unlock()
}

// floodSinglePeer sends everyone but ps itself a ctrlPeerAdd carrying ps's
// current state, read fresh at call time rather than a snapshot captured when
// the change was first queued — so a redundant resend a few seconds later
// (see flushPendingPeerAdds) reflects whatever's true now, not stale data
// replayed if ps changed again in the meantime. A no-op if ps has since
// disconnected: nothing to announce, and its entry is dropped from
// pendingPeerAdds by the caller.
func (e *Engine) floodSinglePeer(ns *netState, ps *peerSession) {
	ns.mu.RLock()
	_, stillConnected := ns.byNode[ps.nodeID]
	entry := peerEntry{
		nodeID: ps.nodeID, hostname: ps.hostname, overlay4: ps.overlay4, overlay6: ps.overlay6,
		endpoint: ps.ep(), managed: ps.managed, manager: ps.manager, webPort: ps.webPort, tcpPort: ps.tcpPort,
		extraTCPPorts: ps.extraTCPPorts, extraUDPPorts: ps.extraUDPPorts,
	}
	ns.mu.RUnlock()
	if !stillConnected {
		return
	}
	out := []byte{ctrlPeerAdd}
	out = append(out, encodePeerList([]peerEntry{entry})...)
	e.floodControl(ns, out, ps)
}

// flushPendingPeerAdds re-floods each still-pending single-peer announcement
// once per maintenance tick and decrements its remaining count, dropping it
// once exhausted. Run unconditionally every tick (unlike broadcastGossip,
// which is gated to gossipInterval) so the redundant copies land a few
// seconds apart as intended — see peerAddResends' doc comment.
func (e *Engine) flushPendingPeerAdds(ns *netState) {
	ns.mu.Lock()
	nodeIDs := make([]string, 0, len(ns.pendingPeerAdds))
	for nid := range ns.pendingPeerAdds {
		nodeIDs = append(nodeIDs, nid)
	}
	ns.mu.Unlock()

	for _, nid := range nodeIDs {
		ns.mu.RLock()
		ps, ok := ns.byNode[nid]
		ns.mu.RUnlock()
		if ok {
			e.floodSinglePeer(ns, ps)
		}
		ns.mu.Lock()
		ns.pendingPeerAdds[nid]--
		if ns.pendingPeerAdds[nid] <= 0 || !ok {
			delete(ns.pendingPeerAdds, nid)
		}
		ns.mu.Unlock()
	}
}

// buildPeerList encodes the currently-connected peers, excluding the recipient
// (who already knows itself). Only verified, active sessions are advertised.
func (e *Engine) buildPeerList(ns *netState, exceptNodeID string) []byte {
	ns.mu.RLock()
	entries := make([]peerEntry, 0, len(ns.byNode))
	for nid, p := range ns.byNode {
		if nid == exceptNodeID {
			continue
		}
		entries = append(entries, peerEntry{
			nodeID:         nid,
			hostname:       p.hostname,
			overlay4:       p.overlay4,
			overlay6:       p.overlay6,
			endpoint:       p.ep(),
			managed:        p.managed,
			manager:        p.manager,
			webPort:        p.webPort,
			tcpPort:        p.tcpPort,
			extraTCPPorts:  p.extraTCPPorts,
			extraUDPPorts:  p.extraUDPPorts,
			localEndpoints: p.localEndpoints,
			version:        p.version,
		})
	}
	ns.mu.RUnlock()

	out := make([]byte, 0, 1+len(entries)*48)
	out = append(out, ctrlPeerList)
	out = append(out, encodePeerList(entries)...)
	return out
}

// peerListSig returns a deterministic summary of every directly-connected
// peer and the exact fields buildPeerList gossips about each one (hostname,
// overlay addresses, endpoint, managed/manager/webPort/tcpPort, host
// candidates, build version). broadcastGossip
// floods the full peer list to every connected peer — at N peers that's
// O(N) recipients times O(N) entries per message, unconditionally, every
// gossipInterval. Most of those ticks change nothing: nobody joined, nobody
// roamed, no managed flag flipped. Comparing this signature to the last one
// sent lets the maintenance loop skip the resend on every tick where the
// content is identical, which is where nearly all of that O(N^2) cost was
// going. gossipFullRefresh still forces a resend periodically regardless, so
// a peer that missed an earlier change self-heals without needing its own
// trigger; a newly-connected peer gets its own immediate copy from install()
// rather than waiting for either.
//
// Field access here mirrors buildPeerList exactly (plain reads under
// ns.mu, ps.ep() for the mutex-guarded endpoint) so the signature reflects
// precisely what would be sent, not an approximation of it.
func (ns *netState) peerListSig() string {
	ns.mu.RLock()
	ids := make([]string, 0, len(ns.byNode))
	for nid := range ns.byNode {
		ids = append(ids, nid)
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, nid := range ids {
		p := ns.byNode[nid]
		fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%s\x00%s\x00%t\x00%t\x00%d\x00%d\x00%v\x00%s\n",
			nid, p.hostname, p.overlay4, p.overlay6, p.ep(), p.managed, p.manager, p.webPort, p.tcpPort,
			p.localEndpoints, // host candidates are gossiped too, so a change in them must re-flood
			p.version)        // ...as is the build version: a peer restarting onto a new build
		// otherwise keeps an identical signature (same hostname, overlay and
		// endpoint), so without this the new version would only propagate at
		// the next gossipFullRefresh rather than promptly.
	}
	ns.mu.RUnlock()
	return b.String()
}

func (e *Engine) gossipPeerTo(ns *netState, ps *peerSession) {
	e.sendControl(ps, e.buildPeerList(ns, ps.nodeID))
}

func (e *Engine) broadcastGossip(ns *netState) {
	ns.mu.RLock()
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		peers = append(peers, ps)
	}
	ns.mu.RUnlock()
	for _, ps := range peers {
		e.gossipPeerTo(ns, ps)
	}
}

// PokePeers sends an immediate keepalive to every connected peer on every
// network. Used after an underlay port change so peers observe our new source
// endpoint and roam to it at once, instead of waiting for the next keepalive.
func (e *Engine) PokePeers() {
	for _, ns := range e.netSnapshot() {
		e.sendKeepalive(ns)
	}
}

func (e *Engine) sendKeepalive(ns *netState) {
	ns.mu.RLock()
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		peers = append(peers, ps)
	}
	ns.mu.RUnlock()
	for _, ps := range peers {
		ps.pingSentNanos.Store(time.Now().UnixNano())
		e.sendControl(ps, []byte{ctrlPing})
	}
}

// ---- maintenance ----

func (e *Engine) maintLoop(ns *netState) {
	defer ns.wg.Done()
	t := time.NewTicker(maintInterval)
	defer t.Stop()
	lastMono := time.Now()
	lastWall := lastMono.Round(0) // Round(0) strips the monotonic reading → pure wall clock
	for {
		select {
		case <-e.stop:
			return
		case <-ns.done:
			return
		case <-t.C:
		}
		now := time.Now()
		// Suspend/resume detection: while the host sleeps its monotonic clock
		// freezes, so silence-based liveness never fires and we keep using a
		// session the peer dropped long ago. Compare wall-clock elapsed to
		// monotonic elapsed since the last tick; a large excess means we were
		// suspended (or badly stalled) — force a clean reconnect before doing the
		// rest of maintenance, so this tick's pruneDead tears the sessions down.
		wall := now.Round(0)
		if suspended(wall.Sub(lastWall), now.Sub(lastMono)) {
			e.log.Infof("mesh: clock jumped ~%v (suspend/resume?); reconnecting peers on net %x", wall.Sub(lastWall).Round(time.Second), ns.spec.ID)
			e.onResume(ns, now)
			// onResume is a best-effort immediate mitigation; the real fix is a
			// clean process restart (see SetSuspendResumeHook's doc) — request
			// one now rather than leaving the network in whatever partially-
			// recovered state onResume alone can manage.
			e.notifySuspendResume()
		}
		lastMono, lastWall = now, wall
		// Re-enumerate our own host candidates here rather than on the
		// handshake path: it's a syscall, and buildHSInit holds ns.mu (see
		// localEndpoints). This picks up an interface coming up or going down,
		// a DHCP lease change, or a port change, within one maintenance tick.
		e.refreshLocalCandidates()
		e.maybeAssignAddress(ns)
		e.reconcileDataplane(ns, now)
		e.pruneDead(ns, now)
		e.sweepStaleRoutes(ns, now)
		e.sweepStaleHosts(ns, now)
		e.sweepStaleDNS(ns, now)
		e.sweepDeadSeeds(ns, now)
		e.sweepBans(ns, now)
		if nat := ns.nat.Load(); nat != nil {
			nat.sweep(now)
		}
		ns.fw.sweepConntrack(now)
		ns.fw.sweepWildcardFQDN(now)
		if now.Sub(ns.lastFWFQDN) >= fwFQDNRefresh {
			e.resolveFirewallFQDN(ns)
			ns.lastFWFQDN = now
		}
		e.flushPendingPeerAdds(ns)
		if now.Sub(ns.lastBanRefresh) >= banRefresh {
			e.refreshBans(ns, now)
			ns.lastBanRefresh = now
		}
		if now.Sub(ns.lastGossip) >= gossipInterval {
			if sig := ns.peerListSig(); sig != ns.lastGossipSig || now.Sub(ns.lastGossipFull) >= gossipFullRefresh {
				e.broadcastGossip(ns)
				ns.lastGossipSig = sig
				ns.lastGossipFull = now
			}
			e.sendReflexive(ns)
			ns.lastGossip = now
		}
		if now.Sub(ns.lastKeepalive) >= e.keepaliveInterval() {
			e.sendKeepalive(ns)
			ns.lastKeepalive = now
		}
		ns.mu.Lock()
		readv := ns.shouldReadvertise(now, e.routeAdvInterval())
		hadv := ns.shouldReadvertiseHosts(now, e.routeAdvInterval())
		dadv := ns.shouldReadvertiseDNS(now, e.routeAdvInterval())
		ns.mu.Unlock()
		if readv {
			e.advertiseRoutes(ns)
		}
		if hadv {
			e.advertiseHosts(ns)
		}
		if dadv {
			e.advertiseDNS(ns)
		}
		e.syncHosts(ns, now)
		e.syncDNS(ns, now)
		e.tryRelays(ns)
		e.maybePersistPeers(ns)
	}
}

// maybePersistPeers triggers a config write of the bootstrap peer cache when the
// connected-peer set changes, so a restart has many seed candidates rather than
// the single configured one. Debounced by signature; the actual write (union +
// cap) happens in the persist hook.
func (e *Engine) maybePersistPeers(ns *netState) {
	ns.mu.RLock()
	eps := make([]string, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		if ep := ps.ep(); ep.IsValid() {
			eps = append(eps, ep.String())
		}
	}
	ns.mu.RUnlock()
	sort.Strings(eps)
	sig := strings.Join(eps, ",")
	ns.mu.Lock()
	changed := sig != ns.lastPeerSig
	ns.lastPeerSig = sig
	ns.mu.Unlock()
	if changed {
		e.notifyChange(ns.spec.ID)
	}
}

// suspended reports whether the wall-clock vs monotonic elapsed gap between two
// consecutive maintenance ticks indicates the host was suspended (its monotonic
// clock froze) or severely stalled. A backward wall step yields a negative skew
// and is ignored.
func suspended(wallGap, monoGap time.Duration) bool {
	return wallGap-monoGap > suspendSkew
}

func (ps *peerSession) setLastRx(t time.Time) {
	ps.mu.Lock()
	ps.lastRx = t
	ps.mu.Unlock()
}

// onResume forces a clean reconnect after the host wakes from suspend. The peer
// has very likely dropped our session during the sleep (no keepalives), and our
// underlay address may have changed, so the reliable path back is a fresh
// handshake: age every session on the network so this tick's pruneDead tears it
// down (freeing the endpoint for initLoop to re-dial), and re-run path-MTU
// discovery since the path may now differ.
//
// Re-dialing peers is not sufficient on its own: when the link bounced during
// sleep the OS frequently flushes our overlay interface address and the routes
// we installed, which black-holes traffic until a restart. So we also re-assert
// that OS state here (see reassertOSState) and invalidate the cached underlay
// source so the next underlay check re-baselines to the post-wake address.
func (e *Engine) onResume(ns *netState, now time.Time) {
	e.reconnectAllPeers(ns)
	e.reassertOSState(ns)
}

// reconnectAllPeers ages every session on the network past the peer timeout so
// the current maintenance tick's pruneDead tears them all down — freeing each
// peer's endpoint for initLoop to re-dial from a clean slate — and drops each
// peer's path-MTU state so it re-discovers on the fresh path. This is the
// in-process reconnect shared by onResume (suspend/resume) and
// checkUnderlayChange (a live underlay roam): both leave every existing
// session pointed at an endpoint that may no longer be reachable from the new
// underlay, and for a peer that isn't a configured seed a stale session is
// never retried until it's first pruned — so without this teardown, only
// seeds (and peers that happen to re-handshake toward us on their own) ever
// come back, which is the "every peer reads 'no reply' after a roam, only
// sometimes recovering" symptom. Also invalidates the cached underlay source
// so the next check re-baselines to the post-roam address rather than
// re-triggering on it.
//
// Critically, each peer's last-known endpoint is re-armed as a redial target
// (via AddSeedFor, node-tagged so install() can prune it cleanly once the peer
// reconnects) BEFORE the session is aged out. pruneDead deletes a reaped
// session outright — node, routes, and endpoint — so without re-arming, a
// non-seed peer that was only ever learned via gossip has *nothing* left to
// dial once every session is pruned: recovery then depends entirely on a
// configured seed being reachable on the new underlay, and if the seeds are
// momentarily unreachable during a lossy roam, the session table empties and
// stays empty (every subsequent roam ages an already-empty table and does
// nothing) until a restart re-reads the seeds from scratch. That is the
// reported terminal state — "once it fails, no roam brings it back, only a
// restart." Re-arming the endpoints turns every former peer, not just the
// configured seeds, into a standing redial target that initLoop keeps
// retrying on whatever network we land on, so recovery no longer hinges on a
// seed happening to be reachable at the instant of the roam.
func (e *Engine) reconnectAllPeers(ns *netState) {
	aged := time.Now().Add(-e.peerTimeoutDuration() - time.Second)
	type redialTarget struct {
		ep     netip.AddrPort
		nodeID string
	}
	var peers []*peerSession
	var redials []redialTarget
	e.mu.RLock()
	for _, ps := range e.sessions {
		if ps.net == ns {
			peers = append(peers, ps)
			if ep := ps.ep(); ep.IsValid() {
				redials = append(redials, redialTarget{ep: ep, nodeID: ps.nodeID})
			}
		}
	}
	e.mu.RUnlock()
	for _, ps := range peers {
		ps.setLastRx(aged)
		ps.resetPMTU()
	}

	// Re-arm each former peer's endpoint as a node-tagged redial target so
	// initLoop keeps dialing it after pruneDead reaps the session — the
	// endpoint would otherwise be forgotten entirely. Clear any seed backoff
	// so the dial happens on the next tick rather than waiting out a cooldown
	// from earlier failed handshakes on the old underlay.
	for _, rt := range redials {
		ns.mu.Lock()
		delete(ns.seedBackoff, rt.ep)
		ns.mu.Unlock()
		e.AddSeedFor(ns.spec.ID, rt.ep, rt.nodeID)
	}

	e.underlayMu.Lock()
	e.localUnderlay = netip.Addr{}
	e.underlayMu.Unlock()
}

// reassertOSState re-applies the overlay interface address and every installed
// redistributed route after a resume, where the OS may have torn down the
// interface's address and routing entries when the underlay link bounced.
//
// The interface-address adds are idempotent: if the address survived the sleep
// the device's AddIPv4/AddIPv6 is a no-op. Routes need more care — syncRoute
// treats ns.osMetric as ground truth and skips a prefix it believes is already
// programmed, but after a resume that belief is stale (the kernel dropped the
// route while osMetric still records it). Clearing osMetric first makes each
// syncRoute re-program the prefix from scratch.
//
// The base overlay subnet's own connected route needs the same explicit
// treatment, not just the address re-add above: that route isn't tracked in
// osMetric (it was never installed via AddRoute in the first place — it's a
// side effect of assigning the interface's address with the subnet's netmask,
// see tun_darwin.go's AddIPv4). On macOS in particular, a sleep/resume cycle
// can drop just that derived network route while leaving the interface's own
// address configuration untouched, in which case re-running the identical
// AddIPv4 call is a genuine no-op — same address, same mask, nothing for the
// OS to reconfigure — and the route never comes back on its own, even though
// every peer-advertised route (reinstalled explicitly via syncRoute, below)
// does. The symptom is a peer that never recovers after the laptop wakes,
// while every other peer and every redistributed route keeps working, because
// only the interface's own subnet lost its path to the tun device. Explicitly
// re-adding the subnet route here, the same way any other route is
// (re-)installed, doesn't depend on the OS deriving it as a side effect: it's
// a no-op (logged, ignored) if the route already exists, and a real fix if it
// doesn't.
func (e *Engine) reassertOSState(ns *netState) {
	if ns.dev() == nil {
		return
	}

	ns.mu.RLock()
	a4, p4 := ns.self4, ns.subnet4
	a6, p6 := ns.self6, ns.subnet6
	ns.mu.RUnlock()
	if a4.IsValid() && p4.IsValid() {
		if err := ns.dev().AddIPv4(a4, p4.Bits()); err != nil {
			e.log.Debugf("mesh: resume: re-add %s on %s (net %x): %v", a4, ns.spec.Dev.Name(), ns.spec.ID, err)
		}
		if err := ns.dev().AddRoute(p4, 0); err != nil {
			e.log.Debugf("mesh: resume: re-add base route %s on %s (net %x): %v", p4, ns.spec.Dev.Name(), ns.spec.ID, err)
		}
	}
	if a6.IsValid() && p6.IsValid() {
		if err := ns.dev().AddIPv6(a6, p6.Bits()); err != nil {
			e.log.Debugf("mesh: resume: re-add %s on %s (net %x): %v", a6, ns.spec.Dev.Name(), ns.spec.ID, err)
		}
		if err := ns.dev().AddRoute(p6, 0); err != nil {
			e.log.Debugf("mesh: resume: re-add base route %s on %s (net %x): %v", p6, ns.spec.Dev.Name(), ns.spec.ID, err)
		}
	}

	// Snapshot the installed prefixes and drop the cache so syncRoute re-adds.
	ns.osMu.Lock()
	prefixes := make([]netip.Prefix, 0, len(ns.osMetric))
	for p := range ns.osMetric {
		prefixes = append(prefixes, p)
	}
	ns.osMetric = make(map[netip.Prefix]int, len(prefixes))
	ns.osMu.Unlock()

	// syncRoute takes ns.osMu itself, so call it only after releasing the lock.
	for _, p := range prefixes {
		e.syncRoute(ns, p)
	}
}

// ResetNetwork immediately drops every active peer session on a network and
// clears seed backoff and in-flight handshake state, so the maintenance loop
// redials every known peer and configured seed from a clean slate on its next
// tick (about a second later) instead of waiting out any existing retry
// backoff. This is the admin UI's on-demand "reset" action for a network
// whose connectivity looks stuck — the on-demand counterpart to onResume,
// which does the same teardown automatically after a detected suspend/resume.
func (e *Engine) ResetNetwork(networkID uint64) error {
	ns := e.network(networkID)
	if ns == nil {
		return fmt.Errorf("no such network %016x", networkID)
	}
	ns.mu.RLock()
	targets := make([]string, 0, len(ns.byNode))
	for nid := range ns.byNode {
		targets = append(targets, nid)
	}
	ns.mu.RUnlock()
	for _, nid := range targets {
		e.localDisconnect(ns, nid) // tears down the session; endpoint is retained so it's redialed
	}
	ns.mu.Lock()
	ns.pending = make(map[uint32]*pendingHS)
	ns.seedBackoff = make(map[netip.AddrPort]time.Time)
	ns.mu.Unlock()
	e.log.Infof("mesh: network %016x reset by admin — reconnecting to %d peer(s) and seeds", networkID, len(targets))
	return nil
}

// pruneDead drops sessions that have gone silent past peerTimeout, freeing the
// endpoint so the seed can be re-dialed.
func (e *Engine) pruneDead(ns *netState, now time.Time) {
	var dead []*peerSession
	e.mu.Lock()
	for idx, ps := range e.sessions {
		if ps.net != ns {
			continue
		}
		if now.Sub(ps.lastRxTime()) > e.peerTimeoutDuration() {
			delete(e.sessions, idx)
			dead = append(dead, ps)
		}
	}
	e.mu.Unlock()
	if len(dead) == 0 {
		return
	}
	ns.mu.Lock()
	var goneNodes []string
	for _, ps := range dead {
		if ns.byNode[ps.nodeID] == ps {
			delete(ns.byNode, ps.nodeID)
			goneNodes = append(goneNodes, ps.nodeID)
		}
		if ps.overlay4.IsValid() && ns.routes4[ps.overlay4] == ps {
			delete(ns.routes4, ps.overlay4)
		}
		if ps.overlay6.IsValid() && ns.routes6[ps.overlay6] == ps {
			delete(ns.routes6, ps.overlay6)
		}
	}
	ns.publishFwd()
	ns.mu.Unlock()
	for _, ps := range dead {
		e.removePeerBypassRoute(ns, ps)
	}
	// A node that has gone silent takes its redistributed routes with it; without
	// this they'd linger in the OS table on every peer until a restart.
	for _, id := range goneNodes {
		e.dropNodeRoutes(ns, id)
	}
	for _, ps := range dead {
		e.log.Infof("mesh: pruned dead session to %q on net %x", ps.nodeID, ns.spec.ID)
	}
}

func (ps *peerSession) lastRxTime() time.Time {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.lastRx
}

// connectedToNode reports whether we hold a session to a given node id.
func (e *Engine) connectedToNode(ns *netState, nodeID string) bool {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	_, ok := ns.byNode[nodeID]
	return ok
}

// ---- peer list wire codec ----
//
//	[count:2] then per entry:
//	  [nidLen:1][nid][hnLen:1][hn][flags:1]([v4:4])([v6:16])
//	  [epFam:1]( [4]+[port:2] | [16]+[port:2] | nothing )

func encodePeerList(entries []peerEntry) []byte {
	b := make([]byte, 0, 2+len(entries)*48)
	var cnt [2]byte
	binary.BigEndian.PutUint16(cnt[:], uint16(len(entries)))
	b = append(b, cnt[:]...)
	for _, en := range entries {
		b = appendLenStr(b, en.nodeID)
		b = appendLenStr(b, en.hostname)
		var flags byte
		if en.overlay4.Is4() {
			flags |= flagHasV4
		}
		if en.overlay6.Is6() && !en.overlay6.Is4In6() {
			flags |= flagHasV6
		}
		if en.managed {
			flags |= flagManaged
		}
		if en.manager {
			flags |= flagManager
		}
		b = append(b, flags)
		if flags&flagHasV4 != 0 {
			a := en.overlay4.As4()
			b = append(b, a[:]...)
		}
		if flags&flagHasV6 != 0 {
			a := en.overlay6.As16()
			b = append(b, a[:]...)
		}
		b = appendEndpoint(b, en.endpoint)
		if flags&flagManaged != 0 {
			var wp [2]byte
			binary.BigEndian.PutUint16(wp[:], en.webPort)
			b = append(b, wp[:]...)
		}
	}
	// TCP/TLS fallback ports are appended as one trailing block, in entry order,
	// after all entries. Older decoders stop after the entries and ignore these
	// bytes; newer decoders read them to learn each peer's fallback port. Adding
	// it here (rather than per-entry) keeps the format backward-compatible.
	if len(entries) > 0 {
		b = append(b, peerListTCPBlock)
		for _, en := range entries {
			var tp [2]byte
			binary.BigEndian.PutUint16(tp[:], en.tcpPort)
			b = append(b, tp[:]...)
		}
		// Extra TCP/UDP ports are further trailing blocks, each its own marker
		// byte so an old decoder (which only ever checks for peerListTCPBlock,
		// once, then stops) never reaches them — see decodePeerList. Per-entry
		// count-prefixed, unlike the single TCP block above, since the extra
		// list length varies per peer. Only emitted at all if at least one
		// entry actually has something to say, so the common case (nobody
		// using extra ports) doesn't grow every gossip message by two blocks
		// of all-zero counts.
		if peerListHasExtraTCP(entries) {
			b = append(b, peerListExtraTCPBlock)
			for _, en := range entries {
				b = appendPortList(b, en.extraTCPPorts)
			}
		}
		if peerListHasExtraUDP(entries) {
			b = append(b, peerListExtraUDPBlock)
			for _, en := range entries {
				b = appendPortList(b, en.extraUDPPorts)
			}
		}
		// Host candidates: one more trailing block, same rules. Emitted only
		// when somebody actually has candidates to advertise, so a mesh of
		// older peers costs nothing. NOTE this block must be emitted after the
		// extra-port blocks, and decoded in the same order, because a decoder
		// that predates it stops at the first marker it doesn't recognize.
		if peerListHasLocal(entries) {
			b = append(b, peerListLocalBlock)
			for _, en := range entries {
				b = appendEndpointList(b, en.localEndpoints)
			}
		}
		// Peer build versions: one more trailing block, same rules again,
		// and — like every block above — emitted after the ones that came
		// before it and decoded in the same order, since a decoder
		// predating it stops at the first marker it doesn't recognize.
		// Only emitted when at least one entry actually has a version to
		// report, so a mesh of peers that all predate the field costs
		// nothing. See hsPayload.Version.
		if peerListHasVersion(entries) {
			b = append(b, peerListVersionBlock)
			for _, en := range entries {
				b = appendLenStr(b, en.version)
			}
		}
	}
	return b
}

// peerListHasExtraTCP/peerListHasExtraUDP report whether any entry has extra
// ports worth encoding a trailing block for — see encodePeerList.
func peerListHasExtraTCP(entries []peerEntry) bool {
	for _, en := range entries {
		if len(en.extraTCPPorts) > 0 {
			return true
		}
	}
	return false
}
func peerListHasExtraUDP(entries []peerEntry) bool {
	for _, en := range entries {
		if len(en.extraUDPPorts) > 0 {
			return true
		}
	}
	return false
}
func peerListHasLocal(entries []peerEntry) bool {
	for _, en := range entries {
		if len(en.localEndpoints) > 0 {
			return true
		}
	}
	return false
}

// peerListHasVersion reports whether any entry has a build version worth
// encoding a trailing block for — see encodePeerList.
func peerListHasVersion(entries []peerEntry) bool {
	for _, en := range entries {
		if en.version != "" {
			return true
		}
	}
	return false
}

// peerListTCPBlock marks the optional trailing block of per-entry TCP fallback
// ports in an encoded peer list. peerListExtraTCPBlock/peerListExtraUDPBlock
// mark further optional trailing blocks of per-entry extra ports — see
// decodePeerList for how an unrecognized marker (a block from a version this
// decoder predates) stops parsing rather than misreading it.
const (
	peerListTCPBlock      = 0x01
	peerListExtraTCPBlock = 0x02
	peerListExtraUDPBlock = 0x03
	peerListLocalBlock    = 0x04
	peerListVersionBlock  = 0x05
)

func appendLenStr(b []byte, s string) []byte {
	if len(s) > 255 {
		s = s[:255]
	}
	b = append(b, byte(len(s)))
	return append(b, s...)
}

func appendEndpoint(b []byte, ep netip.AddrPort) []byte {
	a := ep.Addr().Unmap()
	switch {
	case a.Is4():
		b = append(b, 4)
		v := a.As4()
		b = append(b, v[:]...)
	case a.Is6():
		b = append(b, 6)
		v := a.As16()
		b = append(b, v[:]...)
	default:
		return append(b, 0)
	}
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], ep.Port())
	return append(b, p[:]...)
}

func decodePeerList(b []byte) ([]peerEntry, error) {
	r := reader{b: b}
	count, ok := r.u16()
	if !ok {
		return nil, errBadPayload
	}
	// Cap the preallocation: count is attacker-controlled (up to 65535), and the
	// loop fails fast on a short body, so don't let a tiny packet reserve megabytes.
	prealloc := int(count)
	if prealloc > 1024 {
		prealloc = 1024
	}
	entries := make([]peerEntry, 0, prealloc)
	for i := 0; i < int(count); i++ {
		var en peerEntry
		nid, ok := r.lenStr()
		if !ok {
			return nil, errBadPayload
		}
		en.nodeID = nid
		hn, ok := r.lenStr()
		if !ok {
			return nil, errBadPayload
		}
		en.hostname = hn
		flags, ok := r.byte()
		if !ok {
			return nil, errBadPayload
		}
		if flags&flagHasV4 != 0 {
			v, ok := r.take(4)
			if !ok {
				return nil, errBadPayload
			}
			en.overlay4 = netip.AddrFrom4([4]byte{v[0], v[1], v[2], v[3]})
		}
		if flags&flagHasV6 != 0 {
			v, ok := r.take(16)
			if !ok {
				return nil, errBadPayload
			}
			var a [16]byte
			copy(a[:], v)
			en.overlay6 = netip.AddrFrom16(a)
		}
		ep, ok := r.endpoint()
		if !ok {
			return nil, errBadPayload
		}
		en.endpoint = ep
		if flags&flagManaged != 0 {
			wp, ok := r.take(2)
			if !ok {
				return nil, errBadPayload
			}
			en.managed = true
			en.webPort = binary.BigEndian.Uint16(wp)
		}
		en.manager = flags&flagManager != 0
		entries = append(entries, en)
	}
	// Optional trailing blocks: per-entry TCP fallback ports, then per-entry
	// extra TCP/UDP ports (see encodePeerList). Each is its own marker byte,
	// read in a loop rather than a single check so newer blocks can follow
	// the original one — an older decoder here (one that predates this loop
	// entirely) still works correctly regardless, since it only ever
	// performs its own single one-shot check for peerListTCPBlock and never
	// reads anything past it. A marker this decoder doesn't recognize (a
	// block from a version newer than it) can't be safely skipped — its
	// length isn't self-describing — so parsing just stops there rather
	// than risk misreading unrelated bytes as a block it misunderstands.
blocks:
	for {
		marker, ok := r.byte()
		if !ok {
			break
		}
		switch marker {
		case peerListTCPBlock:
			for i := range entries {
				tp, ok := r.take(2)
				if !ok {
					break blocks // truncated block: keep whatever was parsed, stop entirely
				}
				entries[i].tcpPort = binary.BigEndian.Uint16(tp)
			}
		case peerListExtraTCPBlock:
			for i := range entries {
				ports, ok := readPortList(&r)
				if !ok {
					break blocks
				}
				entries[i].extraTCPPorts = ports
			}
		case peerListExtraUDPBlock:
			for i := range entries {
				ports, ok := readPortList(&r)
				if !ok {
					break blocks
				}
				entries[i].extraUDPPorts = ports
			}
		case peerListLocalBlock:
			for i := range entries {
				eps, ok := readEndpointList(&r)
				if !ok {
					break blocks
				}
				entries[i].localEndpoints = eps
			}
		case peerListVersionBlock:
			for i := range entries {
				v, ok := r.lenStr()
				if !ok {
					break blocks
				}
				entries[i].version = v
			}
		default:
			break blocks // unrecognized block: stop rather than misparse
		}
	}
	return entries, nil
}

// reader helpers specific to the control codec.

func (r *reader) u16() (uint16, bool) {
	s, ok := r.take(2)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint16(s), true
}

func (r *reader) lenStr() (string, bool) {
	n, ok := r.byte()
	if !ok {
		return "", false
	}
	s, ok := r.take(int(n))
	if !ok {
		return "", false
	}
	return string(s), true
}

func (r *reader) endpoint() (netip.AddrPort, bool) {
	fam, ok := r.byte()
	if !ok {
		return netip.AddrPort{}, false
	}
	switch fam {
	case 0:
		return netip.AddrPort{}, true // valid-but-empty
	case 4:
		v, ok := r.take(4)
		if !ok {
			return netip.AddrPort{}, false
		}
		p, ok := r.u16()
		if !ok {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte{v[0], v[1], v[2], v[3]}), p), true
	case 6:
		v, ok := r.take(16)
		if !ok {
			return netip.AddrPort{}, false
		}
		var a [16]byte
		copy(a[:], v)
		p, ok := r.u16()
		if !ok {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(netip.AddrFrom16(a), p), true
	}
	return netip.AddrPort{}, false
}
