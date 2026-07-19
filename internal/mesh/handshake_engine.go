package mesh

import (
	"net/netip"
	"sync/atomic"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/protocol"
)

// transcript binds both ephemerals and the network id into the key derivation.
// Both peers compute it identically: initiator ephemeral first, then responder.
func transcript(initEph, respEph []byte, network uint64) []byte {
	t := make([]byte, 0, len(initEph)+len(respEph)+8)
	t = append(t, initEph...)
	t = append(t, respEph...)
	t = append(t,
		byte(network>>56), byte(network>>48), byte(network>>40), byte(network>>32),
		byte(network>>24), byte(network>>16), byte(network>>8), byte(network))
	return t
}

// install registers a completed session for inbound demux and overlay routing.
// The newest session for a node becomes the outbound route, but older inbound
// session indices are kept valid: with simultaneous initiation ("glare") both
// peers briefly hold two sessions and may each keep a different one as primary,
// so dropping an inbound index here would blackhole the peer's traffic. Demux
// is O(1) by index, so keeping a couple of extra inbound sessions is cheap.
// (Stale-session reaping arrives with rekeying in a later step.)
func (e *Engine) install(ns *netState, ps *peerSession) {
	if ps.nodeID == e.nodeID {
		// Belt-and-suspenders: onHSInit/onHSResp already reject a handshake
		// that claims our own node id before reaching here, but install() is
		// the one place a byNode/routes entry actually gets created, so it
		// stays guarded too rather than trusting every future caller to
		// remember the check — registering ourselves here would be worse
		// than a duplicate peers-table row, since routes4/routes6 would then
		// route our own overlay traffic back into our own tunnel.
		e.log.Warnf("mesh: refusing to install a peer session for our own node id %q on net %x", ps.nodeID, ns.spec.ID)
		return
	}
	ps.initPMTU(e.pmtuFloor, e.pmtuCeil)
	ns.mu.Lock()
	// A re-handshake for a peer we're already connected to (e.g. its endpoint
	// roamed off the configured seed, so initLoop re-dialed it, or the peer
	// re-initiated toward us) installs a fresh session over a live one. Carry
	// the prior session's establishment time forward so "established" reflects
	// continuous peer uptime, not the age of the latest crypto session — the
	// data path never dropped. A genuine teardown deletes the byNode entry
	// first (see pruneDead/removePeer), so after a real reconnect there is no
	// prev and the timestamp correctly advances.
	prev := ns.byNode[ps.nodeID] // nil for a genuinely new peer, not a re-handshake
	if prev != nil && prev.established.Before(ps.established) {
		ps.established = prev.established
	}
	ns.byNode[ps.nodeID] = ps
	if ps.overlay4.IsValid() {
		ns.routes4[ps.overlay4] = ps
	}
	if ps.overlay6.IsValid() {
		ns.routes6[ps.overlay6] = ps
	}
	ni := ns.nodes[ps.nodeID]
	if ni == nil {
		ni = &nodeInfo{nodeID: ps.nodeID}
		ns.nodes[ps.nodeID] = ni
	}
	ni.hostname = ps.hostname
	if ps.overlay4.IsValid() {
		ni.overlay4 = ps.overlay4
	}
	if ps.overlay6.IsValid() {
		ni.overlay6 = ps.overlay6
	}
	ni.endpoint = ps.endpoint
	ni.managed = ps.managed
	ni.manager = ps.manager
	ni.bgpASN = ps.bgpASN
	ni.webPort = ps.webPort
	ni.tcpPort = ps.tcpPort
	ni.extraTCPPorts = ps.extraTCPPorts
	ni.extraUDPPorts = ps.extraUDPPorts
	ni.lastSeen = time.Now()
	if ps.endpoint.IsValid() {
		ns.everConnected[ps.endpoint] = true
	}
	// Register the peer's advertised host candidates (see localcand.go). Done
	// even though we're connected right now: this session may be a *relayed*
	// one, in which case its LAN address is precisely the candidate that could
	// upgrade it to direct, and seedOwnerNeedsUpgrade/initSeedTick will try it
	// on the next tick. Deferred until after ns.mu is released — addSeed takes
	// the same lock.
	localCands := ps.localEndpoints
	// Clear this peer's backoff — reachable again. Matched against the exact
	// endpoint, or (for a fallback-established session) against seedFallback's
	// record of which seed this fallback address resolves to. An exact-key-only
	// delete here was a silent no-op for every fallback-established session,
	// since ps.endpoint is the fallback port, never the original (blocked) UDP
	// seed address:port the backoff entry is actually keyed under — leaving it
	// to linger until it happened to expire on its own. Matching by IP alone
	// (an earlier version of this fix) would be wrong here for the same reason
	// it's wrong in connectedTo: it could clear a different peer's backoff
	// entry just for sharing an IP.
	for seed := range ns.seedBackoff {
		if seed == ps.endpoint || ns.seedFallback[seed] == ps.endpoint {
			delete(ns.seedBackoff, seed)
		}
	}
	// Prune this node's other seed entries now that it's connected — but only
	// ones we can positively attribute to it via seedOwner (see AddSeedFor),
	// never by matching on IP alone, and never an operator-configured seed
	// (explicitSeed). A peer behind a NAT that rotates its source port
	// accumulates a new seed entry every time a different peer gossips its
	// latest observed endpoint (AddSeed's dedup is exact-match only), and
	// nothing else ever removes the old ones — left unchecked, the list grows
	// without bound and initLoop keeps re-attempting every stale entry on
	// every tick forever. Pruning by owner is precise: a seed with no recorded
	// owner, or owned by a different node, is left untouched, even if it
	// happens to share an IP with ps (see connectedTo's doc comment for why
	// IP-based matching is unsafe — several distinct peers can share one
	// address, e.g. behind the same NAT gateway).
	//
	// The explicitSeed exemption is what keeps a node able to find its way
	// back after *its own* underlay changes (Wi-Fi → cellular, a new DHCP
	// lease, a VPN flap). ns.seeds is the only list initLoop dials, and the
	// unbounded-growth problem this pruning exists to solve is purely a
	// gossip-learned one: NetSpec.Seeds is a small, fixed, operator-authored
	// set that never grows on its own. But this loop couldn't tell the two
	// apart, so a configured seed got dropped like any other the moment
	// gossip attributed it to a node (AddSeedFor) and that node was later
	// seen at a different endpoint — a NAT rebind or roam on *its* end, which
	// over a long uptime happens to essentially every peer eventually. The
	// entry never came back (only a config reload's AddExplicitSeed merge or
	// a restart re-adds it), so ns.seeds would drift into holding nothing but
	// dynamically-learned endpoints. That's invisible while the local node
	// stays put — those endpoints work fine — and catastrophic the moment it
	// moves: every learned endpoint is now unreachable from the new network,
	// pruneDead clears the sessions after peerTimeout, and initLoop is left
	// re-dialing a list of addresses that can no longer answer, with the one
	// set of addresses the operator specifically chose as reachable anchors
	// long since deleted. Result: the node sits fully dead until it's
	// restarted, exactly the symptom that made "gravinet used to renegotiate
	// when I changed networks and now doesn't" a real regression rather than
	// a topology problem. Keeping explicit seeds pinned costs a handful of
	// entries and restores the bootstrap path that's supposed to always be
	// there.
	// A relayed session has NO observed endpoint: onRelay dispatches the inner
	// packet with a zero netip.AddrPort, because the source we actually saw was
	// the relay, not this peer. So ps.endpoint is invalid here, and the entire
	// premise of this pruning — "these seeds are superseded by the address the
	// peer is really at" — has nothing to stand on. Run it anyway and
	// `s != ps.endpoint` is trivially true for every seed, so it deletes every
	// candidate this node has: its gossip-learned endpoints, and (since v374)
	// its host candidates — the LAN addresses that exist for the sole purpose
	// of upgrading a relayed peer to direct. A relayed install would quietly
	// destroy the only addresses the upgrade path had to dial, on every
	// (re)handshake, guaranteeing the peer stayed relayed forever. Only prune
	// when there is a real endpoint doing the superseding.
	if ps.endpoint.IsValid() && len(ns.seeds) > 0 {
		kept := ns.seeds[:0]
		for _, s := range ns.seeds {
			if s != ps.endpoint && ns.seedOwner[s] == ps.nodeID && !ns.explicitSeed[s] && !ns.hostCand[s] {
				delete(ns.seedOwner, s)
				delete(ns.seedBackoff, s)
				delete(ns.seedFallback, s)
				continue // stale: superseded by ps.endpoint, drop it
			}
			kept = append(kept, s)
		}
		ns.seeds = kept
	}
	ns.mu.Unlock()

	e.addLocalCandidates(ns.spec.ID, ps.nodeID, localCands)

	e.mu.Lock()
	e.sessions[ps.localIdx] = ps
	e.mu.Unlock()

	// A re-handshake replaces prev's *peerSession object outright, so any
	// bypass route tracked on prev (see fulltunnel.go) doesn't carry over to
	// ps automatically — withdraw it before installing ps's own, in case the
	// two ever differ (a roamed endpoint across the re-handshake).
	if prev != nil && prev != ps {
		e.removePeerBypassRoute(ns, prev)
	}
	e.syncPeerBypassRoute(ns, ps)

	// Push our redistributed routes to the (possibly new) peer right away —
	// both config Advertise routes (advRoutes) and anything currently
	// redistributed from BGP (bgpRoutes, see bgpRedistSet's doc comment):
	// advertiseRoutes floods both, so the gate here has to consider both too,
	// or a node with only BGP-into-mesh redistribution active (no config
	// Advertise routes at all) would never flood its BGP routes to a newly
	// connected peer until something else happened to trigger a call.
	rs := ns.advRoutes.Load()
	br := ns.bgpRoutes.Load()
	if (rs != nil && len(*rs) > 0) || (br != nil && len(br.routes) > 0) {
		e.advertiseRoutes(ns)
	}

	// Give the (possibly new) peer its own immediate copy of the peer list,
	// addressed to just it — not a full broadcast. broadcastGossip's periodic
	// resend to everyone is now change-gated (see peerListSig) and can
	// legitimately skip ticks where nothing changed from the perspective of
	// peers who already have current info; a peer connecting for the first
	// time has no prior info at all and shouldn't have to wait up to
	// gossipFullRefresh to learn who else is on the mesh.
	e.gossipPeerTo(ns, ps)

	// Tell everyone else about this (re)connection right away too, via the
	// small single-peer path rather than waiting for broadcastGossip's next
	// eligible tick — see announcePeerChange's doc comment.
	e.announcePeerChange(ns, ps)
}

// onHSInit handles an inbound handshake initiation (responder role).
func (e *Engine) onHSInit(payload []byte, from netip.AddrPort, via *peerSession) {
	hdr, aad, body, err := protocol.DecodeHSInit(payload)
	if err != nil {
		return
	}
	ns := e.network(hdr.Network)
	if ns == nil {
		return
	}
	src := from.Addr().String()
	if via != nil {
		src = "relay:" + via.nodeID // throttle relayed inits by relay identity
	}
	if ns.throttle.Banned(src) {
		return
	}
	psk, ok := ns.keys.Load().Lookup(hdr.KeyID)
	if !ok {
		if ns.throttle.Fail(src) {
			e.log.Warnf("mesh: banned %s on net %x (no matching key)", src, hdr.Network)
		}
		return
	}
	pt, err := crypto.OpenWithKey(psk, body, aad)
	if err != nil {
		if ns.throttle.Fail(src) {
			e.log.Warnf("mesh: banned %s on net %x (auth failure)", src, hdr.Network)
		}
		return
	}
	pl, err := decodeHSPayload(pt)
	if err != nil || len(pl.Ephemeral) != ephemeralLen {
		ns.throttle.Fail(src)
		return
	}
	if !freshTimestamp(pl.TimeNano) {
		// Unlike dispatch's version-mismatch case, this is post-authentication
		// — the PSK already opened this payload, so whoever sent it knows a
		// real key for this network, not anonymous unauthenticated traffic.
		// Safe to Warn unconditionally: retries of a genuinely clock-skewed
		// peer are naturally bounded by the existing handshake retry/seed
		// backoff cadence (seconds, not a tight loop), not amplified by
		// adding this log. Reporting both the direction and magnitude (not
		// just "rejected") is the point — "their clock reads earlier/later
		// than ours by X" is what actually lets an operator tell which side
		// to go fix.
		skew := time.Duration(time.Now().UnixNano() - pl.TimeNano)
		dir := "behind"
		if skew < 0 {
			skew = -skew
			dir = "ahead of"
		}
		e.log.Warnf("mesh: rejecting handshake from %q on net %x: its clock appears %s %s ours (tolerance is %s) — check NTP/system time on both nodes", pl.NodeID, hdr.Network, skew, dir, clockSkew)
		return // stale/replayed or large clock skew
	}
	if ns.isBanned(pl.NodeID) {
		e.log.Warnf("mesh: rejecting handshake from banned node %q on net %x", pl.NodeID, hdr.Network)
		return
	}
	if ns.isPeerDisabled(pl.NodeID) {
		e.log.Debugf("mesh: rejecting handshake from locally-disabled peer %q on net %x", pl.NodeID, hdr.Network)
		return
	}
	if pl.NodeID == e.nodeID {
		// A handshake claiming our own node id didn't come from a peer — the
		// packet looped back to us, most often a symmetric-NAT node hairpinning
		// off a relay/hub while dialing what it believed was someone else's
		// advertised endpoint. The auth already passed (it's a real, validly
		// sealed session — just with ourselves), so this is the only point
		// that can still tell it apart from a legitimate peer: skip responding
		// rather than let install() add a live "enabled" row for our own node
		// id right alongside the inert "this node" row in the peers table.
		e.log.Debugf("mesh: dropping handshake init on net %x that claims our own node id %q (from %s)", hdr.Network, pl.NodeID, from)
		return
	}
	// Replay guard: the ±clockSkew timestamp window above still admits a captured
	// HS_INIT re-sent within it. The initiator's ephemeral public key is unique
	// per legitimate attempt, so reject a second appearance of the same one —
	// this is checked only after auth so the cache never fills with junk.
	if ns.hsReplay(pl.Ephemeral, time.Now()) {
		e.log.Debugf("mesh: dropping replayed handshake from %q on net %x", pl.NodeID, hdr.Network)
		return
	}
	ns.throttle.Reset(src) // valid auth clears the source's failure history

	respEph, err := crypto.NewEphemeral()
	if err != nil {
		return
	}
	shared, err := respEph.Shared(pl.Ephemeral)
	if err != nil {
		return
	}
	tr := transcript(pl.Ephemeral, respEph.Public(), hdr.Network)
	keys := crypto.DeriveSessionKeys(shared, psk, tr, false)
	sess, err := crypto.NewSession(keys)
	if err != nil {
		return
	}

	idxR := e.allocIndex()
	ps := &peerSession{
		sess:           sess,
		localIdx:       idxR,
		remoteIdx:      pl.Index,
		endpoint:       from,
		relay:          via,
		nodeID:         pl.NodeID,
		hostname:       pl.Hostname,
		overlay4:       pl.Overlay4,
		overlay6:       pl.Overlay6,
		net:            ns,
		keyID:          hdr.KeyID,
		managed:        pl.Managed,
		manager:        pl.Manager,
		allowRelay:     pl.AllowRelay,
		relayKnown:     pl.RelayKnown,
		localEndpoints: pl.LocalEndpoints,
		webPort:        pl.WebPort,
		tcpPort:        pl.TCPPort,
		extraTCPPorts:  pl.ExtraTCPPorts,
		extraUDPPorts:  pl.ExtraUDPPorts,
		bgpASN:         pl.BGPASN,
		lastRx:         time.Now(),
		established:    time.Now(),
	}
	e.install(ns, ps)
	if ns.absorbIdentity(pl) {
		e.notifyChange(ns.spec.ID)
	}

	self4, self6 := ns.selfAddrs()
	sub4, sub6 := ns.subnets()
	resp := hsPayload{
		Index:          idxR,
		TimeNano:       time.Now().UnixNano(),
		Ephemeral:      respEph.Public(),
		Overlay4:       self4,
		Overlay6:       self6,
		NodeID:         e.nodeID,
		Hostname:       e.hostname,
		Subnet4:        sub4,
		Subnet6:        sub6,
		Name:           ns.netName(),
		Managed:        e.managed.Load(),
		Manager:        e.manager.Load(),
		AllowRelay:     ns.spec.AllowRelay,
		LocalEndpoints: e.localEndpoints(),
		WebPort:        e.webPort,
		TCPPort:        uint16(e.fallbackPort.Load()),
		ExtraTCPPorts:  loadPortList(&e.extraTCPPorts),
		ExtraUDPPorts:  loadPortList(&e.extraUDPPorts),
		BGPASN:         e.bgpASN.Load(),
	}
	hrespHdr := make([]byte, protocol.HSRespHeaderLen)
	protocol.EncodeHSResp(hrespHdr, protocol.HSRespHeader{Network: hdr.Network, RecvSession: pl.Index})
	sealed, err := crypto.SealWithKey(psk, encodeHSPayload(resp), hrespHdr)
	if err != nil {
		return
	}
	out := append(hrespHdr, sealed...)
	if via != nil {
		e.sealAndSend(via, innerRelay, encodeRelay(e.nodeID, pl.NodeID, out))
		e.log.Infof("mesh: inbound tunnel up with %q (%s) on net %x via relay %q",
			pl.NodeID, pl.Hostname, hdr.Network, via.nodeID)
	} else {
		e.send(from, out)
		e.log.Infof("mesh: inbound tunnel up with %q (%s) on net %x via %s",
			pl.NodeID, pl.Hostname, hdr.Network, from)
	}
	e.maybeAssignAddress(ns)
}

// onHSResp completes an outbound handshake (initiator role).
func (e *Engine) onHSResp(payload []byte, from netip.AddrPort, via *peerSession) {
	hdr, aad, body, err := protocol.DecodeHSResp(payload)
	if err != nil {
		return
	}
	ns := e.network(hdr.Network)
	if ns == nil {
		return
	}
	ns.mu.Lock()
	p := ns.pending[hdr.RecvSession]
	ns.mu.Unlock()
	if p == nil {
		return // unknown or already-completed handshake
	}

	// The response is sealed under whichever key the responder matched; try our
	// keys (≤8) to open it. This is robust to key cycling on our side.
	psk, keyID, pt, ok := e.tryOpenWithAnyKey(ns, body, aad)
	if !ok {
		return
	}
	pl, err := decodeHSPayload(pt)
	if err != nil || len(pl.Ephemeral) != ephemeralLen {
		return
	}
	if ns.isBanned(pl.NodeID) || ns.isPeerDisabled(pl.NodeID) {
		ns.mu.Lock()
		delete(ns.pending, p.idxI)
		ns.mu.Unlock()
		return
	}
	if pl.NodeID == e.nodeID {
		// We dialed a seed believing it was a peer, and got a response
		// claiming our own node id — the same NAT-hairpin loopback as in
		// onHSInit, just discovered from the initiator side instead of the
		// responder side. Drop the pending handshake so seed backoff can
		// retry normally instead of installing a session with ourselves.
		ns.mu.Lock()
		delete(ns.pending, p.idxI)
		ns.mu.Unlock()
		e.log.Debugf("mesh: dropping handshake response on net %x that claims our own node id %q (from %s)", hdr.Network, pl.NodeID, from)
		return
	}
	shared, err := p.eph.Shared(pl.Ephemeral)
	if err != nil {
		return
	}
	tr := transcript(p.eph.Public(), pl.Ephemeral, hdr.Network)
	keys := crypto.DeriveSessionKeys(shared, psk, tr, true)
	sess, err := crypto.NewSession(keys)
	if err != nil {
		return
	}

	relay := via
	if relay == nil {
		relay = p.relay
	}
	ps := &peerSession{
		sess:           sess,
		localIdx:       p.idxI, // we told the peer to send to idxI; that's our inbound index
		remoteIdx:      pl.Index,
		endpoint:       from,
		relay:          relay,
		nodeID:         pl.NodeID,
		hostname:       pl.Hostname,
		overlay4:       pl.Overlay4,
		overlay6:       pl.Overlay6,
		net:            ns,
		keyID:          keyID,
		managed:        pl.Managed,
		manager:        pl.Manager,
		allowRelay:     pl.AllowRelay,
		relayKnown:     pl.RelayKnown,
		localEndpoints: pl.LocalEndpoints,
		webPort:        pl.WebPort,
		tcpPort:        pl.TCPPort,
		extraTCPPorts:  pl.ExtraTCPPorts,
		extraUDPPorts:  pl.ExtraUDPPorts,
		bgpASN:         pl.BGPASN,
		lastRx:         time.Now(),
		established:    time.Now(),
	}
	ns.mu.Lock()
	delete(ns.pending, p.idxI)
	// p.endpoint is the literal address we dialed to reach this peer, and a
	// completed, authenticated handshake proves it belongs to pl.NodeID —
	// record that now, regardless of how this seed was added. Without this,
	// only seeds added via AddSeedFor (control.go's gossip re-dial hint, and
	// reload's live-seed merge) ever get an owner; a statically configured
	// seed from the config file never did, since ns.seeds is populated
	// directly in newNetState, bypassing addSeed entirely. That left
	// connectedToSeedOwner unable to recognize the common case — a peer
	// reachable at its configured seed address but whose live session
	// endpoint (ps.endpoint, below) has since roamed to a different
	// NAT-mapped port or interface — so initLoop kept re-dialing and
	// re-handshaking a peer that was, in fact, already connected: one full
	// handshake every tick, forever, each one reinstalling the session (see
	// install()'s "tunnel up" logging) and disrupting the data path each time
	// it did, which is what produced the once-a-second "tunnel up" churn for
	// a subset of peers rather than a one-time connect.
	ns.seedOwner[p.endpoint] = pl.NodeID
	ns.mu.Unlock()
	e.install(ns, ps)
	if ns.absorbIdentity(pl) {
		e.notifyChange(ns.spec.ID)
	}
	if relay != nil {
		e.log.Infof("mesh: outbound tunnel up with %q (%s) on net %x via relay %q",
			pl.NodeID, pl.Hostname, hdr.Network, relay.nodeID)
	} else {
		e.log.Infof("mesh: outbound tunnel up with %q (%s) on net %x via %s",
			pl.NodeID, pl.Hostname, hdr.Network, from)
	}
	e.maybeAssignAddress(ns)
}

// tryOpenWithAnyKey attempts to open a sealed body with each configured key.
func (e *Engine) tryOpenWithAnyKey(ns *netState, body, aad []byte) ([]byte, crypto.KeyID, []byte, bool) {
	ks := ns.keys.Load()
	for _, id := range ks.Order() {
		psk, ok := ks.Lookup(id)
		if !ok {
			continue
		}
		if pt, err := crypto.OpenWithKey(psk, body, aad); err == nil {
			return psk, id, pt, true
		}
	}
	return nil, crypto.KeyID{}, nil, false
}

func freshTimestamp(ns int64) bool {
	d := time.Now().UnixNano() - ns
	if d < 0 {
		d = -d
	}
	return time.Duration(d) <= clockSkew
}

// maxHSSeen caps the handshake replay cache. A legitimate mesh sees far fewer
// distinct in-flight handshakes than this within a skew window; the bound just
// stops a flood of unique (already authenticated) inits from growing it without
// limit. Oldest entries are evicted first.
const maxHSSeen = 4096

// hsReplay reports whether an HS_INIT bearing this ephemeral public key has
// already been accepted within the skew window (i.e. it's a replay), and if
// not, records it. A fresh initiator uses a new ephemeral per attempt, so the
// pubkey is effectively a single-use nonce. Only call this for handshakes that
// already passed authentication and freshTimestamp, so the cache holds only
// real, decrypted inits — never attacker-chosen garbage.
func (ns *netState) hsReplay(ephemeral []byte, now time.Time) bool {
	key := string(ephemeral)
	ns.hsSeenMu.Lock()
	defer ns.hsSeenMu.Unlock()
	if exp, ok := ns.hsSeen[key]; ok && now.Before(exp) {
		return true // already seen and not yet lapsed → replay
	}
	// Opportunistically evict lapsed entries; if still over the cap after that,
	// drop the oldest to make room (bounded work: one full pass only when full).
	for k, exp := range ns.hsSeen {
		if !now.Before(exp) {
			delete(ns.hsSeen, k)
		}
	}
	if len(ns.hsSeen) >= maxHSSeen {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, exp := range ns.hsSeen {
			if first || exp.Before(oldest) {
				oldestKey, oldest, first = k, exp, false
			}
		}
		delete(ns.hsSeen, oldestKey)
	}
	// Keep it for one skew window past now — the same horizon freshTimestamp
	// would still accept a replay within, so the cache covers exactly that span.
	ns.hsSeen[key] = now.Add(clockSkew)
	return false
}

// initLoop drives outbound handshakes to seeds, cycling through keys on retry.
func (e *Engine) initLoop(ns *netState) {
	defer ns.wg.Done()
	if ns.keys.Load().Len() == 0 {
		return // nothing to authenticate with
	}
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ns.done:
			return
		case <-t.C:
		}
		e.syncSeedBypassRoutes(ns)
		e.primeTCPSeeds(ns) // dial/redial any explicit TCP/TLS seeds
		ns.mu.RLock()
		seeds := append([]netip.AddrPort(nil), ns.seeds...)
		backoff := make(map[netip.AddrPort]time.Time, len(ns.seedBackoff))
		for k, v := range ns.seedBackoff {
			backoff[k] = v
		}
		ns.mu.RUnlock()
		now := time.Now()
		for _, seed := range seeds {
			e.initSeedTick(ns, seed, backoff, now)
		}
	}
}

// initSeedTick is initLoop's per-seed decision, factored out so it's directly
// unit-testable without waiting on the real 1s ticker (see
// TestUpgradeAttemptAlsoTriesFallback). backoff is initLoop's already-taken
// snapshot of ns.seedBackoff for this tick.
func (e *Engine) initSeedTick(ns *netState, seed netip.AddrPort, backoff map[netip.AddrPort]time.Time, now time.Time) {
	if !e.canSourceFamily(seed.Addr()) {
		// No routable address in this family, so every send and every fallback
		// dial to it is a guaranteed ENETUNREACH. Don't spend the tick learning
		// that again — see canSourceFamily. Re-evaluated each maintenance tick,
		// so a roam that gains IPv6 picks these straight back up.
		return
	}
	if e.connectedTo(ns, seed) {
		return // this seed's own address is already our live session's endpoint
	}
	if e.connectedToSeedOwner(ns, seed) {
		if !e.seedOwnerNeedsUpgrade(ns, seed, now) {
			return // owner already connected directly, or too soon to retry
		}
		// A fresh, independent handshake attempt toward a peer already
		// reached via relay. On success this upgrades the existing session
		// in place — install() already overwrites ns.byNode[nodeID] cleanly
		// regardless of what was there before, carrying the original
		// establishment time forward — so nothing extra is needed here for
		// the upgrade itself. On failure the working relay session is
		// completely untouched: this is a new, separate pending handshake
		// (see planHandshake), not a teardown of anything. See
		// directUpgradeInterval's doc comment for why this exists and why
		// it's throttled so much more gently than a genuinely failing seed.
		//
		// Also try the TCP/TLS fallback in parallel, the same way the
		// not-yet-connected branch below already does for a seed that's
		// merely cooling down. Without this, a peer that only ever became
		// relay-connected — because UDP to it doesn't work, up to and
		// including UDP being turned off entirely (config.PrimaryPort == 0,
		// the '-' port setting added in v366) — can never upgrade: this
		// upgrade attempt is otherwise the *only* remaining retry path once
		// connectedToSeedOwner is true (the backoff branch below, which is
		// what normally calls ensureFallback, is unreachable from here for
		// as long as the peer stays relayed, since every UDP-endpoint
		// variant gossip re-registers for the same owner routes back into
		// this same branch instead). With UDP disabled, e.send's plain
		// planHandshake attempt just below will keep failing immediately
		// (Dual.Send returns errNoUDP) forever, on every throttled retry,
		// and nothing would ever try the path that could actually work.
		// ensureFallback already no-ops once a fallback connection exists
		// or one is already being dialed, so calling it on every throttled
		// upgrade attempt costs nothing extra when UDP is working fine.
		e.ensureFallback(ns, seed)
	} else if until, ok := backoff[seed]; ok && now.Before(until) {
		// UDP to this seed is cooling down after a failed handshake — try
		// reaching the same peer over the TCP/TLS fallback in parallel.
		e.ensureFallback(ns, seed)
		return // unreachable seed cooling down
	}
	pkt, to, send := e.planHandshake(ns, seed)
	if send {
		e.send(to, pkt)
	}
}

// planHandshake decides, under lock, whether to (re)send a handshake to seed,
// builds the packet, then releases the lock before any network I/O.
func (e *Engine) planHandshake(ns *netState, seed netip.AddrPort) ([]byte, netip.AddrPort, bool) {
	ns.mu.Lock()
	defer ns.mu.Unlock()

	var p *pendingHS
	for _, pp := range ns.pending {
		if pp.endpoint == seed {
			p = pp
			break
		}
	}
	now := time.Now()
	order := ns.keys.Load().Order()

	if p == nil {
		// fresh attempt with the first key
		eph, err := crypto.NewEphemeral()
		if err != nil {
			return nil, seed, false
		}
		idxI := e.allocIndex()
		p = &pendingHS{idxI: idxI, eph: eph, keyCursor: 0, endpoint: seed, started: now}
		ns.pending[idxI] = p
		return e.buildHSInit(ns, p), seed, true
	}

	if now.Sub(p.started) <= handshakeRetry {
		return nil, seed, false // give the current try time to land
	}
	// retry: advance to the next key, or give up this cycle after the last
	if p.keyCursor+1 < len(order) {
		p.keyCursor++
		p.started = now
		return e.buildHSInit(ns, p), seed, true
	}
	// exhausted all keys for this attempt; drop pending and cool the seed down
	delete(ns.pending, p.idxI)
	ns.seedBackoff[seed] = now.Add(seedRetryBackoff)
	return nil, seed, false
}

// buildHSInit constructs an HS_INIT packet for the pending handshake's current
// key cursor. Caller holds ns.mu.
func (e *Engine) buildHSInit(ns *netState, p *pendingHS) []byte {
	ks := ns.keys.Load()
	order := ks.Order()
	keyID := order[p.keyCursor]
	psk, _ := ks.Lookup(keyID)

	pl := hsPayload{
		Index:          p.idxI,
		TimeNano:       time.Now().UnixNano(),
		Ephemeral:      p.eph.Public(),
		Overlay4:       ns.self4,
		Overlay6:       ns.self6,
		NodeID:         e.nodeID,
		Hostname:       e.hostname,
		Subnet4:        ns.subnet4,
		Subnet6:        ns.subnet6,
		Name:           ns.name, // planHandshake already holds ns.mu; don't re-lock
		Managed:        e.managed.Load(),
		Manager:        e.manager.Load(),
		AllowRelay:     ns.spec.AllowRelay,
		LocalEndpoints: e.localEndpoints(),
		WebPort:        e.webPort,
		TCPPort:        uint16(e.fallbackPort.Load()),
		ExtraTCPPorts:  loadPortList(&e.extraTCPPorts),
		ExtraUDPPorts:  loadPortList(&e.extraUDPPorts),
		BGPASN:         e.bgpASN.Load(),
	}
	hdr := make([]byte, protocol.HSInitHeaderLen)
	protocol.EncodeHSInit(hdr, protocol.HSInitHeader{Network: ns.spec.ID, KeyID: keyID})
	sealed, err := crypto.SealWithKey(psk, encodeHSPayload(pl), hdr)
	if err != nil {
		return nil
	}
	return append(hdr, sealed...)
}

// connectedTo reports whether we already have a session to seed's endpoint.
// connectedTo reports whether ns has a live session reachable at seed's exact
// address, or at the specific fallback address ensureFallback last resolved
// for this seed. The second check is necessary because when UDP to a seed is
// blocked, ensureFallback deliberately reconnects the same peer on a
// different port (the TCP/TLS fallback port) — that is the intended,
// successful outcome, not a different peer, and matching on the exact
// original port would mean a peer that's fully connected via its fallback
// path is never recognized as such, so initLoop keeps calling ensureFallback
// on every tick forever even though the peer is already up.
//
// This is intentionally more precise than "any live session sharing this
// seed's IP": several distinct peers can share one IP (behind the same NAT
// gateway, or in tests, several local peers on 127.0.0.1), and matching by IP
// alone would treat connecting to any one of them as satisfying every other
// seed at that address — which is exactly the regression an earlier, broader
// version of this fix caused (see TestConnectedToDoesNotFalsePositiveAcrossPeersOnSameIP).
func (e *Engine) connectedTo(ns *netState, seed netip.AddrPort) bool {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	fb, hasFallback := ns.seedFallback[seed]
	for _, ps := range ns.byNode {
		ep := ps.ep()
		if ep == seed || (hasFallback && ep == fb) {
			return true
		}
	}
	return false
}

// seedOwnerNeedsUpgrade reports whether seed is worth a fresh handshake
// attempt despite its owner already having a live session — true only when
// that session is relay-only (see peerSession.getRelay), and enough time
// has passed since the last such attempt (directUpgradeInterval) — unless
// the owner is a node the operator explicitly configured as a seed
// (explicitSeedNode), in which case there's no throttle at all: an explicit
// seed is a deliberate statement that this address should be directly
// reachable, worth retrying at initSeedTick's normal pace rather than
// backing off just because a relay happens to be covering for it. Recording
// the attempt time is folded into a true result rather than left to a
// separate call, so every caller that proceeds also marks it — no separate
// step to remember.
func (e *Engine) seedOwnerNeedsUpgrade(ns *netState, seed netip.AddrPort, now time.Time) bool {
	ns.mu.RLock()
	owner, ok := ns.seedOwner[seed]
	if !ok || owner == "" {
		ns.mu.RUnlock()
		return false
	}
	ps, live := ns.byNode[owner]
	if !live || ps.getRelay() == nil {
		ns.mu.RUnlock()
		return false // no session, or already direct — nothing to improve
	}
	if ns.explicitSeedNode[owner] || ns.hostCand[seed] {
		// No directUpgradeInterval throttle for an explicit seed, nor for a
		// host candidate — but still paced like a not-yet-connected peer would
		// be (handshakeRetry per key, then seedRetryBackoff between full
		// attempt cycles), not hammered once a tick forever: planHandshake sets
		// ns.seedBackoff on this exact seed whenever an attempt cycle exhausts,
		// the same as it does for any other seed, so reading it back here is
		// what keeps this at that pace instead of sending a fresh handshake
		// every second indefinitely.
		//
		// The host-candidate case matters as much as the explicit-seed one. A
		// candidate belonging to a peer we currently reach only via relay is
		// the single highest-value dial in the system — it is the entire reason
		// host candidates exist — and it is a same-link UDP packet, about the
		// cheapest thing this loop can do. directUpgradeInterval's 5 minutes is
		// calibrated for speculative retries across a WAN against a peer a relay
		// is already covering for; applying it here throttled the LAN probe that
		// escapes the relay down to one attempt per five minutes. Worse, it made
		// the mechanism work only by accident of bookkeeping: explicitSeedNode is
		// keyed by node ID and is populated only when gossip happens to attribute
		// a node's *configured seed address* to it, so whether a co-located peer
		// ever got a timely LAN probe depended on whether the address in someone's
		// config matched the endpoint gossip reported. Two nodes on the same switch
		// should not need to be configured seeds of each other to find each other.
		// Paced per *node*, not merely per seed. A peer commonly owns many seeds
		// — gn-ionos2 has twelve configured TCP seed ports, plus its UDP
		// endpoint, plus host candidates — and every one of them lands here
		// independently, on every tick, with no directUpgradeInterval to hold it
		// back. Keyed only by seed, a single relay-connected peer therefore fired
		// a dozen concurrent upgrade handshakes per second. Each that succeeded
		// installed a fresh session and displaced the last, and the displaced
		// ones (still valid receive indices, so they cannot simply be deleted —
		// the peer may be addressing packets to them) piled up until pruneDead
		// reaped them ~20s later. That is the "pruned dead session to
		// 5f87d03fdff7b708" storm: twenty-five identical lines every five
		// seconds, indefinitely.
		//
		// One upgrade in flight per peer is all that is ever useful: they all
		// reach the same node, and the first to land makes the rest redundant.
		// Serializing costs nothing — a seed that loses the race is retried on a
		// later tick, and because planHandshake pushes a failing seed into
		// seedBackoff, the turn naturally rotates through a peer's addresses
		// rather than sticking on the first one.
		until, cooling := ns.seedBackoff[seed]
		dueNow := !cooling || !now.Before(until)
		ns.mu.RUnlock()
		if !dueNow {
			return false
		}
		ns.mu.Lock()
		if last, ok := ns.upgradeNodeAt[owner]; ok && now.Sub(last) < upgradeNodeInterval {
			ns.mu.Unlock()
			return false // another of this peer's seeds is already trying
		}
		ns.upgradeNodeAt[owner] = now
		ns.mu.Unlock()
		return true
	}
	last, tried := ns.directUpgradeAttempt[seed]
	ns.mu.RUnlock()
	if tried && now.Sub(last) < directUpgradeInterval {
		return false // tried recently, give it time
	}
	ns.mu.Lock()
	ns.directUpgradeAttempt[seed] = now
	ns.mu.Unlock()
	return true
}

// connectedToSeedOwner reports whether we already hold a live session to the
// node this seed is known to belong to. connectedTo only recognizes a session
// by its current endpoint, which misses a peer whose endpoint has roamed off
// the configured seed address (the common NAT case, where touch() follows the
// observed source port) — leaving initLoop to re-dial and re-handshake a peer
// that is in fact connected, which reinstalls its session every cycle and
// resets its "established" time without connectivity ever dropping. seedOwner
// is set only when a seed can be positively attributed to a node (AddSeedFor /
// gossip), so this stays owner-precise and never conflates distinct peers that
// merely share an IP — the same reason connectedTo refuses to match by IP.
func (e *Engine) connectedToSeedOwner(ns *netState, seed netip.AddrPort) bool {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	owner, ok := ns.seedOwner[seed]
	if !ok || owner == "" {
		return false
	}
	_, live := ns.byNode[owner]
	return live
}

// tcpPortForEndpoint returns the TCP/TLS fallback port advertised by the node
// whose underlay endpoint matches ep (learned via handshake or gossip), or 0 if
// unknown. This lets the fallback dial a peer's actual port without a mesh-wide
// agreement on a single port.
func (ns *netState) tcpPortForEndpoint(ep netip.AddrPort) uint16 {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	// A connected session is the most authoritative source. Matched by IP,
	// not the exact port: this is what lets a stale seed (e.g. an
	// operator-configured bootstrap address whose port is no longer the
	// peer's actual TCP fallback port — the peer's live session settled on a
	// different one) get unstuck. Once ANY live session exists for this
	// address's IP, its advertised port is a trustworthy port to try,
	// regardless of which port the seed being resolved happens to use.
	//
	// Unlike connectedTo (which gates whether to dial/retry at all, and must
	// stay address-precise — matching by IP there risks conflating distinct
	// peers that happen to share an address, e.g. behind one NAT gateway; see
	// its doc comment), this is purely a "which port should I try" choice: a
	// wrong guess here just fails to complete a handshake, same as today, so
	// an occasional same-IP collision is self-correcting, not destructive.
	for _, ps := range ns.byNode {
		if ps.ep().Addr() == ep.Addr() && ps.tcpPort != 0 {
			return ps.tcpPort
		}
	}
	// Next, the seed's owning node, when we know it (AddSeedFor records it —
	// gossip-learned endpoints and host candidates both carry a node ID). This
	// is what makes a host candidate (localcand.go) resolvable at all: a peer's
	// LAN address is by definition NOT its observed endpoint, so the exact-match
	// on ni.endpoint just below can never hit for one, and without this step
	// every LAN candidate would silently fall through to *our own* fallback port
	// — right only by coincidence, whenever both nodes happen to use the same
	// one. Going through the owner instead reads the port that node actually
	// advertised, for the exact node the candidate belongs to. Node-keyed, so
	// it carries none of the same-IP collision risk of matching on address.
	if owner, ok := ns.seedOwner[ep]; ok && owner != "" {
		if ni := ns.nodes[owner]; ni != nil && ni.tcpPort != 0 {
			return ni.tcpPort
		}
	}
	// Otherwise fall back to gossip/handshake-learned node info. Kept
	// exact-match: this can be stale (learned about a peer no longer
	// connected), so matching by IP alone here has less certain payoff than
	// the live-session case above.
	for _, ni := range ns.nodes {
		if ni.endpoint == ep && ni.tcpPort != 0 {
			return ni.tcpPort
		}
	}
	return 0
}

// extraTCPPortsForEndpoint is tcpPortForEndpoint's counterpart for the extra
// TCP ports a peer advertises — same matching priority (live session by IP
// first, then gossip/handshake-learned node info by exact endpoint), same
// self-correcting tolerance for an occasional same-IP collision. Used by
// ensureFallback to dial every advertised candidate in parallel, not just
// the primary.
func (ns *netState) extraTCPPortsForEndpoint(ep netip.AddrPort) []uint16 {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	for _, ps := range ns.byNode {
		if ps.ep().Addr() == ep.Addr() && len(ps.extraTCPPorts) > 0 {
			return ps.extraTCPPorts
		}
	}
	for _, ni := range ns.nodes {
		if ni.endpoint == ep && len(ni.extraTCPPorts) > 0 {
			return ni.extraTCPPorts
		}
	}
	return nil
}

// primeTCPSeeds dials each configured TCP/TLS seed over the fallback directly,
// rather than waiting for a UDP handshake to fail. This is what lets a node cold-
// bootstrap onto the mesh when UDP is blocked end to end: once the TLS connection
// is up, the seed is registered so the next init tick runs the handshake over it.
// Dial is idempotent and dedupes in-flight dials, so calling this every tick just
// retries unreachable seeds until they connect.
func (e *Engine) primeTCPSeeds(ns *netState) {
	ns.mu.RLock()
	seeds := append([]netip.AddrPort(nil), ns.tcpSeeds...)
	ns.mu.RUnlock()
	if len(seeds) == 0 {
		return
	}
	e.mu.RLock()
	tr := e.tr
	e.mu.RUnlock()
	fd, ok := tr.(fallbackDialer)
	if !ok {
		return // UDP-only transport: can't dial a TCP seed
	}
	netID := ns.spec.ID
	for _, seed := range seeds {
		if !seed.Addr().IsValid() || fd.HasFallback(seed) {
			if fd.HasFallback(seed) {
				e.AddSeed(netID, seed) // connected; make sure the handshake loop dials it
			}
			continue
		}
		s := seed
		go func() {
			if err := fd.DialFallback(s); err != nil {
				e.log.Debugf("mesh: tcp seed dial %s: %v", s, err)
				return
			}
			e.AddSeed(netID, s) // route the handshake to this seed over the new TLS conn
			e.log.Infof("mesh: dialed tcp seed %s", s)
		}()
	}
}

// addTCPSeed merges a TCP/TLS seed into the live set (de-duplicated), so one added
// at runtime is primed on the next init tick.
func (e *Engine) addTCPSeed(networkID uint64, seed netip.AddrPort) {
	ns := e.network(networkID)
	if ns == nil || !seed.Addr().IsValid() {
		return
	}
	ns.mu.Lock()
	for _, s := range ns.tcpSeeds {
		if s == seed {
			ns.mu.Unlock()
			return
		}
	}
	ns.tcpSeeds = append(ns.tcpSeeds, seed)
	ns.mu.Unlock()
}

// ensureFallback opens a TCP/TLS fallback connection to a peer when UDP to it has
// failed (the seed is in backoff). It dials the peer's assumed fallback port
// (default 443) — and, if the peer has advertised any extra TCP ports, those
// too, all in parallel — and registers whichever succeeds as a seed, so the
// next init tick sends the handshake there and the Dual sender, seeing a live
// TLS connection, routes it over TCP. The peer answers over the same
// connection, so the session settles on the TLS path. Dialing runs off the
// init loop so a slow TLS handshake can't stall handshakes to other seeds.
//
// Candidates are tried in parallel, not sequentially with a per-candidate
// timeout: the whole point of an extra port existing is not knowing in
// advance which one gets through a restrictive firewall, which is exactly
// the case where the primary is most likely to be the one that fails —
// paying its full timeout before trying alternates would work against the
// feature's own purpose. Multiple candidates succeeding is harmless, not
// something this cancels for: each resolves to its own distinct fb address
// dialFallbackCandidate independently claims and dials, so they can't
// collide with each other the way retrying the *same* fb concurrently
// would, and an extra live connection that ends up unused simply sits idle
// rather than causing any conflicting state.
// fallbackDialCooldown paces repeat TCP/TLS fallback dials to the same seed.
// Long enough that dozens of unreachable seeds cost a trickle rather than a
// storm, short enough that a peer which only just became reachable over TCP
// (its UDP listener went away, a firewall opened) is picked up promptly.
const fallbackDialCooldown = 30 * time.Second

// fallbackDialDue reports whether seed is due a TCP/TLS fallback dial, and
// records the attempt if so. The first attempt for any seed is immediate; only
// retries are paced by fallbackDialCooldown.
func (e *Engine) fallbackDialDue(ns *netState, seed netip.AddrPort, now time.Time) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if last, tried := ns.fallbackAttempt[seed]; tried && now.Sub(last) < fallbackDialCooldown {
		return false
	}
	ns.fallbackAttempt[seed] = now
	return true
}

func (e *Engine) ensureFallback(ns *netState, seed netip.AddrPort) {
	if !seed.Addr().IsValid() {
		return
	}
	if !e.canSourceFamily(seed.Addr()) {
		return // guaranteed ENETUNREACH; see canSourceFamily
	}
	// Rate-limit, never suppress. The cost problem this addresses is real: the
	// backoff branch of initSeedTick calls this for every cooling-down seed on
	// every tick, and once host candidates persist there can be dozens, so an
	// unthrottled dial-per-tick-per-seed becomes a continuous storm of sockets,
	// TLS handshakes and goroutines — enough on its own to starve the web admin
	// and delay keepalives until healthy sessions time out.
	//
	// The first attempt for a seed is always immediate; only retries are paced.
	//
	// This replaces a guard that skipped the dial entirely for host candidates
	// whenever *this* node had UDP enabled. That was wrong in the one way that
	// matters, and it cost real debugging: whether a TCP dial is worth making
	// depends on whether the PEER can be reached over UDP, not on whether WE
	// can speak it. A peer with UDP disabled (PrimaryPort 0 — the '-' setting)
	// advertises its LAN address at its TCP/TLS port and can be reached over
	// nothing else. Under the old guard, any peer that still had UDP enabled
	// would probe that address over UDP, get silence (there is no UDP listener
	// there), and then decline to try TCP — because ITS OWN UDP worked fine. So
	// two machines on the same switch, one of them TCP-only, would fail every
	// direct attempt and relay to each other across the internet, with the LAN
	// path suppressed by the very code meant to be conserving effort.
	if !e.fallbackDialDue(ns, seed, time.Now()) {
		return
	}
	fp := int(e.fallbackPort.Load())
	if fp <= 0 {
		return
	}
	e.mu.RLock()
	tr := e.tr
	e.mu.RUnlock()
	// Primary port resolution, most-authoritative first:
	//  1. the peer's advertised TCP port (learned via handshake or gossip),
	//  2. the join-token seed hint (lets a cold bootstrap reach the seeds over TCP
	//     on a non-default port before any advertisement is known),
	//  3. our own port, as the last-resort assumption.
	// This is what lets nodes run different TCP ports without a mesh-wide agreement.
	port := fp
	if ns.spec.SeedTCPPort > 0 {
		port = ns.spec.SeedTCPPort
	}
	if adv := ns.tcpPortForEndpoint(seed); adv != 0 {
		port = int(adv)
	}
	e.dialFallbackCandidate(ns, tr, seed, port)
	// Any advertised extra ports beyond the primary, each its own parallel
	// attempt — see this function's own doc comment for why parallel, not
	// sequential.
	for _, extra := range ns.extraTCPPortsForEndpoint(seed) {
		if int(extra) != port {
			e.dialFallbackCandidate(ns, tr, seed, int(extra))
		}
	}
}

// dialFallbackCandidate is ensureFallback's per-candidate-port body — claim,
// check, and dial exactly one fallback address. Factored out so the primary
// port and any extra ports all go through the identical logic rather than
// duplicating it once per candidate.
func (e *Engine) dialFallbackCandidate(ns *netState, tr Sender, seed netip.AddrPort, port int) {
	// Dial the peer at the chosen fallback port. When it equals the seed's port
	// (the default — both 65432), fb == seed and the existing seed simply starts
	// routing over TLS once the connection is up; when they differ (e.g. the peer
	// advertises 443), fb is registered as an additional seed so the next init
	// tick hands the handshake to the TLS path.
	fb := netip.AddrPortFrom(seed.Addr(), uint16(port))

	// Claim fb before doing anything else: several stale duplicate seed
	// entries for one peer (same IP, different historically-observed ports —
	// see AddSeed) all resolve to this same fb, and initLoop fires
	// ensureFallback for every one of them in a single synchronous pass while
	// this function's dial runs asynchronously in a goroutine. Without this
	// claim, many callers can race past the checks below before the first
	// one's dial has completed and updated shared state, each independently
	// dialing and logging "established tcp fallback" for the exact same
	// address within the same tick.
	ns.mu.Lock()
	ns.seedFallback[seed] = fb
	if ns.dialing[fb] {
		ns.mu.Unlock()
		return // another call is already resolving this exact fallback address
	}
	ns.dialing[fb] = true
	ns.mu.Unlock()
	release := func() {
		ns.mu.Lock()
		delete(ns.dialing, fb)
		ns.mu.Unlock()
	}

	if e.connectedTo(ns, fb) {
		release()
		return // already connected over the fallback
	}
	fd, ok := tr.(fallbackDialer)
	if !ok || fd.HasFallback(fb) {
		release()
		return // transport has no fallback, or a connection is already up
	}
	netID := ns.spec.ID
	go func() {
		defer release()
		if err := fd.DialFallback(fb); err != nil {
			e.log.Debugf("mesh: tcp fallback dial %s: %v", fb, err)
			return
		}
		if fb != seed {
			// Propagate seed's known owner (if any — see AddSeedFor) to the
			// derived fb entry. Without this, fb is added unowned and can
			// never be pruned by install()'s stale-seed cleanup even after
			// its peer connects via a completely different path (a stale
			// UDP-then-TCP-fallback address left behind by NAT rebinding, an
			// address change, etc.) — it would sit in ns.seeds and get
			// retried by initLoop forever, exactly like the original seed
			// would have without this propagation.
			ns.mu.RLock()
			owner := ns.seedOwner[seed]
			ns.mu.RUnlock()
			if owner != "" {
				e.AddSeedFor(netID, fb, owner)
			} else {
				e.AddSeed(netID, fb)
			}
		}
		e.log.Infof("mesh: udp to %s appears blocked; established tcp/%d fallback", seed.Addr(), port)
		go e.watchFallbackHandshake(ns, fb)
	}()
}

// fallbackHandshakeGrace is how long watchFallbackHandshake waits after a TCP/
// TLS fallback socket connects before concluding the handshake isn't going to
// complete. Generous enough to cover a full handshake retry cycle
// (handshakeRetry between attempts, cycling through all configured keys)
// without false-triggering on a merely slow handshake. A var (not const) so
// tests can shorten it rather than waiting out the real duration.
var fallbackHandshakeGrace = 10 * time.Second

// watchFallbackHandshake warns if fb never yields a working mesh session
// within fallbackHandshakeGrace of its raw TCP/TLS socket connecting.
// DialFallback succeeding only confirms the socket connected — it says
// nothing about whether a gravinet peer is actually on the other end. Without
// this, an address that isn't running gravinet (or isn't reachable as the
// peer expected, or fails the handshake for some other reason — wrong key,
// banned, version mismatch) produces "established tcp fallback" every time
// its socket reconnects, which reads as success and gives no signal that
// nothing is actually working.
func (e *Engine) watchFallbackHandshake(ns *netState, fb netip.AddrPort) {
	t := time.NewTimer(fallbackHandshakeGrace)
	defer t.Stop()
	select {
	case <-e.stop:
		return
	case <-ns.done:
		return
	case <-t.C:
	}
	if e.connectedTo(ns, fb) {
		return // handshake completed; nothing to warn about
	}
	e.log.Warnf("mesh: tcp fallback to %s connected but no mesh session formed within %s — the address may not be running gravinet, may not be the peer expected, or the handshake may be failing (wrong key, banned, version mismatch)", fb, fallbackHandshakeGrace)
}

// deadSeedGrace is how long a seed entry that's never once connected keeps
// being retried before sweepDeadSeeds gives up on it and removes it from
// ns.seeds. Mirrors peer_cache's own staleness window (see
// peerCacheStaleGrace in cmd/gravinet) for the same reasoning: long enough
// that routine downtime isn't mistaken for permanent staleness, short enough
// that a wrong or decommissioned address doesn't get retried — and logged —
// for the entire lifetime of a long-running process. Without this, a dead
// seed that nothing else ever prunes (an operator-configured or otherwise
// unowned entry — see AddSeed/AddSeedFor's doc comments on that gap) would be
// retried, and logged, for as long as the process stays up, restart or not.
const deadSeedGrace = 1 * time.Hour

// sweepDeadSeeds removes seeds that have been in ns.seeds for at least
// deadSeedGrace and have never once resulted in a connection — checked
// against everConnected (this seed's own address, or its resolved fallback
// address, per install()), not against whether a session happens to be live
// at this exact instant. The two are different questions: a seed that
// connected fine yesterday and is simply unreachable right now — a peer
// that's banned, restarting, or behind a transient network blip — has very
// much "resulted in a connection" and must keep being retried for as long as
// it takes to come back, no matter how long that is; only a seed that has
// truly never once worked, in deadSeedGrace or more of trying, is actually
// dead weight. (An earlier version of this check used live-connection state
// instead of everConnected, which meant any sustained outage on an
// already-proven seed — most reliably, an admin ban lasting more than a
// sweep tick — looked identical to "never worked" and got the seed silently
// and permanently evicted, with no way back short of a restart re-reading it
// from config. Checking everConnected instead of the instantaneous state is
// the fix: nothing here now depends on whether the reason for the current
// gap is a ban, a restart, or anything else temporary.)
//
// This is address-precise, matching connectedTo and install()'s stale-seed
// pruning: it only ever judges the exact address in question, never anything
// else sharing its IP, so it carries none of the same-IP collision risk a
// broader heuristic would (see connectedTo's doc comment for why that
// matters).
//
// This is the runtime counterpart to the peer_cache staleness pruning done at
// persist time (cmd/gravinet's mergePeerCache): that one stops a dead address
// from reappearing in the config on the next restart; this one stops the
// current, already-running process from retrying — and logging — it in the
// meantime, which is what actually matters for a process that stays up for
// months between restarts.
// This never evicts an operator-configured seed (ns.explicitSeed, see v370's
// AddExplicitSeed), no matter how long it has failed for. everConnected alone
// isn't enough to protect those: it only ever gets set by a *successful*
// session, so a seed that has never once worked in this process is fair game
// for eviction — which is right for a gossip-learned address (it was only
// ever a guess, and if it's still valid it will simply be re-learned) and
// badly wrong for a configured one. A daemon that cold-starts while its
// configured seeds happen to be unreachable — booting on cellular, before
// the VPN or Wi-Fi comes up, during an upstream outage, on a laptop opened
// somewhere new — would evict them all after deadSeedGrace and then never
// retry them again, sitting permanently dead with an empty seed list even
// once the network came back, and no way out short of a config reload or a
// restart. An explicit seed is the operator's standing instruction that this
// address is the way back to the mesh; the whole point is that it stays
// dialable when nothing else is. Its retries are already paced by
// seedBackoff, so pinning it costs a handshake attempt every
// seedRetryBackoff, not a busy loop — and unlike a learned entry there is
// nothing else that could ever re-add it.
func (e *Engine) sweepDeadSeeds(ns *netState, now time.Time) {
	ns.mu.Lock()
	var dead []netip.AddrPort
	kept := ns.seeds[:0]
	for _, s := range ns.seeds {
		first, known := ns.seedFirstSeen[s]
		// A host candidate gets a much shorter chance than a real seed. It is a
		// *speculative* LAN address, gossiped to us about a peer we may share no
		// network with at all, and in a mesh of N peers each advertising several,
		// most of them are unreachable from here by construction. If one is going
		// to work it works essentially instantly — it's a same-link address, not
		// something waiting on a peer to boot or a NAT to open. Giving each an
		// hour of deadSeedGrace meant every node ground continuously through
		// dozens of dud addresses: a UDP handshake cycle every seedRetryBackoff
		// plus a TCP/TLS dial on top, for an hour, per candidate. That starved
		// the web admin outright and delayed keepalives enough that healthy
		// direct sessions timed out (peerTimeout is 20s) and were replaced by
		// relayed ones — a node that looked fine at startup and quietly rotted
		// into relaying over the next few minutes.
		grace := deadSeedGrace
		if ns.hostCand[s] {
			grace = hostCandGrace
		}
		stillYoung := !known || now.Sub(first) < grace
		hadConnection := ns.everConnected[s]
		if !hadConnection {
			if fb, hasFallback := ns.seedFallback[s]; hasFallback {
				hadConnection = ns.everConnected[fb]
			}
		}
		if stillYoung || hadConnection || ns.explicitSeed[s] {
			kept = append(kept, s)
			continue
		}
		if ns.hostCand[s] {
			// Remember it's a dud, or the next gossip re-adds it and we do this
			// all over again, forever — see hostCandDead.
			ns.hostCandDead[s] = true
		}
		dead = append(dead, s)
		delete(ns.seedFirstSeen, s)
		delete(ns.seedBackoff, s)
		delete(ns.seedFallback, s)
		delete(ns.seedOwner, s)
	}
	ns.seeds = kept
	ns.mu.Unlock()
	for _, s := range dead {
		e.log.Infof("mesh: giving up on seed %s on net %016x — never connected within %s; it will not be retried unless learned again", s, ns.spec.ID, deadSeedGrace)
	}
}

// SetFallbackPort updates this node's TCP/TLS fallback port at runtime (so a live
// config change takes effect for both advertising and outbound dialing).
func (e *Engine) SetFallbackPort(port int) {
	e.fallbackPort.Store(int64(port))
	e.refreshLocalCandidates() // candidates carry a port; re-publish on the new one
}

// SetPrimaryPort updates this node's primary UDP listen port at runtime, so a
// live config change (including turning UDP off, port 0) is reflected in the
// host candidates it advertises — see localEndpoints. The counterpart to
// SetFallbackPort.
func (e *Engine) SetPrimaryPort(port int) {
	e.primaryPort.Store(int64(port))
	e.refreshLocalCandidates() // candidates carry a port; re-publish on the new one
}

// SetExtraTCPPorts/SetExtraUDPPorts update this node's own additional
// advertised listen ports at runtime — the counterpart to SetFallbackPort
// for extra_tcp_listen_ports/extra_listen_ports, so a live config change to
// either takes effect for advertising immediately, same as the primary port
// changing already did. Also pushes the change to already-connected peers
// right away (announceClusterStateAll, the same live-push SetManaged/
// SetManager use) rather than waiting for their next reconnect to notice.
func (e *Engine) SetExtraTCPPorts(ports []uint16) {
	p := append([]uint16(nil), ports...) // defensive copy: caller's slice may be reused
	e.extraTCPPorts.Store(&p)
	e.announceClusterStateAll()
}
func (e *Engine) SetExtraUDPPorts(ports []uint16) {
	p := append([]uint16(nil), ports...)
	e.extraUDPPorts.Store(&p)
	e.announceClusterStateAll()
}

// loadPortList reads an atomic.Pointer[[]uint16] such as extraTCPPorts,
// normalizing the zero value (never explicitly Store'd — e.g. a test
// constructing an Engine by hand rather than via NewEngine) to an empty
// slice rather than a nil-pointer dereference.
func loadPortList(p *atomic.Pointer[[]uint16]) []uint16 {
	v := p.Load()
	if v == nil {
		return nil
	}
	return *v
}
