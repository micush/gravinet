package mesh

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// BanInfo is a ban as reported to the control API.
type BanInfo struct {
	Target         string `json:"target"`
	Hostname       string `json:"hostname"`        // target's hostname
	Origin         string `json:"origin"`          // node id that issued the ban
	OriginHostname string `json:"origin_hostname"` // issuing node's hostname, if known
	Notes          string `json:"notes"`
	At             int64  `json:"at_unix_nano"`
	Mine           bool   `json:"mine"` // true if this node issued it (can unban)
}

// PeerInfo is a connected peer as reported to the control API.
type PeerInfo struct {
	NodeID   string `json:"node_id"`
	Hostname string `json:"hostname"`
	Overlay4 string `json:"overlay4,omitempty"`
	Overlay6 string `json:"overlay6,omitempty"`
	Endpoint string `json:"endpoint"` // observed underlay source — a NAT'd peer's public mapping
	Relayed  bool   `json:"relayed"`  // reached via a relay (couldn't connect directly → restrictive NAT/firewall)
	// BGPASN is this peer's current effective BGP AS number, as gossiped in
	// its handshake and kept fresh live thereafter — see hsPayload.BGPASN's
	// doc comment. 0 means it has no BGP configured (or predates this
	// field). Consumed by AutoBGP (internal/webadmin/autobgp.go) to build a
	// Neighbor for this peer using the AS it actually runs, rather than
	// guessing one from its tunnel address.
	BGPASN uint32 `json:"bgp_asn,omitempty"`

	// TxBytes/RxBytes are cumulative encrypted wire bytes exchanged with this
	// peer over the life of its current session (they reset on re-handshake,
	// like EstablishedAt). Omitted when zero so old clients and idle rows stay
	// clean.
	TxBytes uint64 `json:"tx_bytes,omitempty"`
	RxBytes uint64 `json:"rx_bytes,omitempty"`

	// RelayVia is the relay's hostname (falling back to its node id if no
	// hostname is known) when Relayed is true, empty otherwise. A relayed
	// session has no direct underlay address of its own to report in
	// Endpoint — see peerSession.endpoint's doc comment — so the UI shows
	// this instead of Endpoint for a relayed row rather than the raw
	// zero-value AddrPort string.
	RelayVia string `json:"relay_via,omitempty"`

	// RTTMs is the most recent round-trip time measured via the ctrlPing/
	// ctrlPong NAT keepalive (see peerSession.rttNanos) in milliseconds; 0/
	// omitted if no keepalive round trip has completed yet (session just
	// installed — the first sample lands within one keepaliveInterval).
	// This is a passive, continuously-updated figure over the session's
	// real current path, distinct from Info → Latency's on-demand
	// ICMP-over-overlay ping, which isn't visible to (or used by) the mesh
	// engine at all.
	RTTMs float64 `json:"rtt_ms,omitempty"`

	// Notes is an operator-authored, local-only note attached to this peer's
	// node id (see Config.PeerSetNotes) — never gossiped, purely for display.
	Notes string `json:"notes,omitempty"`

	// KeyLabel is the label (from this node's own key table) of the key that
	// authenticated the current session with this peer — i.e. which of our
	// slots this peer is currently riding on. Empty if the session's key was
	// since removed from our table (rare: the session survives until its next
	// reconnect, per dropRetiredKeySessions).
	KeyLabel string `json:"key_label,omitempty"`

	Transport string `json:"transport"` // active underlay transport for this peer: "udp" or "tcp" (TLS fallback)

	// EstablishedAt is when the current session with this peer was installed
	// (see install()); it resets to now on every reconnect, so it doubles as
	// "how long this session has been up" for the admin UI.
	EstablishedAt int64 `json:"established_at_unix_nano"`

	// Transport diagnostics: discovered path MTU and fragmentation/reassembly
	// counters, so a connectivity problem can be localized to (or ruled out of)
	// the mesh from the Info page.
	PathMTU      int    `json:"path_mtu"`       // discovered underlay datagram size (outer bytes); 0 = not yet probed
	FragsSent    uint64 `json:"frags_sent"`     // fragment datagrams sent to this peer
	FragSendDrop uint64 `json:"frag_send_drop"` // packets we couldn't send (path too small / too many pieces)
	FragsRcvd    uint64 `json:"frags_rcvd"`     // fragment datagrams received from this peer
	ReasmOK      uint64 `json:"reasm_ok"`       // packets fully reassembled from this peer
	ReasmDrop    uint64 `json:"reasm_drop"`     // incomplete reassemblies dropped (lost fragments)
	SpoofDrop    uint64 `json:"spoof_drop"`     // inbound packets dropped: source not owned by this peer (anti-spoofing)
}

func banKey(origin, target string) string { return origin + "\x00" + target }

// isBanned reports whether nodeID has a live (unexpired) ban from any origin.
func (ns *netState) isBanned(nodeID string) bool {
	now := time.Now().UnixNano()
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	for _, b := range ns.bans {
		if b.target == nodeID && (b.expiresNano == 0 || now < b.expiresNano) {
			return true
		}
	}
	return false
}

// BanNode issues a ban for target on a network, applies it locally, and floods
// it to the mesh. The origin is recorded as this node.
func (e *Engine) BanNode(networkID uint64, target, notes string) error {
	ns := e.network(networkID)
	if ns == nil {
		return errors.New("unknown network")
	}
	if target == "" {
		return errors.New("empty target node id")
	}
	now := time.Now()
	rec := &banRecord{
		target:      target,
		origin:      e.nodeID,
		notes:       notes,
		atNano:      now.UnixNano(),
		expiresNano: now.Add(ns.banTTL).UnixNano(),
	}
	ns.mu.Lock()
	rec.endpoint = ns.endpointOf(target) // remember for fast re-dial on unban
	rec.hostname = ns.hostnameOf(target) // remember the name (registry is cleared by applyBan)
	ns.bans[banKey(rec.origin, rec.target)] = rec
	ns.mu.Unlock()

	e.applyBan(ns, target)
	e.floodControl(ns, encodeBanAdd(rec), nil)
	e.log.Infof("mesh: banned %q on net %x (origin=self notes=%q ttl=%s)", target, networkID, notes, ns.banTTL)
	return nil
}

// UnbanNode removes a ban that this node originated and floods the removal.
// Per spec, a ban can only be lifted by its originating node.
func (e *Engine) UnbanNode(networkID uint64, target string) error {
	ns := e.network(networkID)
	if ns == nil {
		return errors.New("unknown network")
	}
	key := banKey(e.nodeID, target)
	ns.mu.Lock()
	rec, ok := ns.bans[key]
	var ep netip.AddrPort
	if ok {
		ep = rec.endpoint
		delete(ns.bans, key)
	}
	ns.mu.Unlock()
	if !ok {
		return errors.New("no ban on that node originated here (only the originating node can unban)")
	}
	e.floodControl(ns, encodeBanDel(e.nodeID, target), nil)
	e.redial(ns, ep) // reconnect immediately instead of waiting out peerTimeout
	e.log.Infof("mesh: unbanned %q on net %x", target, networkID)
	return nil
}

// EditBanNotes changes the notes on a ban this node originated and re-floods
// it so every peer's copy updates. Like unban, only the originating node may do
// this (a node can't rewrite a ban another node issued). The ban's expiry is
// bumped to now+TTL so the update wins the receive-side freshness check
// (rec.expiresNano > existing.expiresNano); this deliberately resets the ban's
// TTL clock, which is the accepted trade-off for making the edit propagate.
func (e *Engine) EditBanNotes(networkID uint64, target, notes string) error {
	ns := e.network(networkID)
	if ns == nil {
		return errors.New("unknown network")
	}
	key := banKey(e.nodeID, target)
	ns.mu.Lock()
	rec, ok := ns.bans[key]
	if ok {
		rec.notes = notes
		rec.expiresNano = time.Now().Add(ns.banTTL).UnixNano() // bump so the edit out-freshes the old copy on peers
	}
	ns.mu.Unlock()
	if !ok {
		return errors.New("no ban on that node originated here (only the originating node can edit its notes)")
	}
	e.floodControl(ns, encodeBanAdd(rec), nil)
	e.log.Infof("mesh: edited ban notes for %q on net %x (notes=%q)", target, networkID, notes)
	return nil
}

// ForceUnban removes all bans on target regardless of origin and floods the
// removal. It is the operator break-glass for when an originating node is gone.
// Note: if the origin is actually still alive, it will re-assert the ban on its
// next refresh — which is the desired behaviour (force-unban only sticks for
// genuinely departed origins).
func (e *Engine) ForceUnban(networkID uint64, target string) error {
	ns := e.network(networkID)
	if ns == nil {
		return errors.New("unknown network")
	}
	ns.mu.Lock()
	removed := 0
	var redials []netip.AddrPort
	for k, b := range ns.bans {
		if b.target == target {
			redials = append(redials, b.endpoint)
			delete(ns.bans, k)
			removed++
		}
	}
	ns.mu.Unlock()
	e.floodControl(ns, encodeBanDel("", target), nil) // empty origin = wildcard
	for _, ep := range redials {
		e.redial(ns, ep)
	}
	e.log.Infof("mesh: force-unbanned %q on net %x (%d local record(s) cleared)", target, networkID, removed)
	return nil
}

// refreshBans re-floods this node's own bans with a bumped expiry so they stay
// alive across the mesh; bans whose origin has gone silent simply lapse.
func (e *Engine) refreshBans(ns *netState, now time.Time) {
	ns.mu.Lock()
	var toFlood []*banRecord
	for _, b := range ns.bans {
		if b.origin == e.nodeID {
			b.expiresNano = now.Add(ns.banTTL).UnixNano()
			cp := *b
			toFlood = append(toFlood, &cp)
		}
	}
	ns.mu.Unlock()
	for _, b := range toFlood {
		e.floodControl(ns, encodeBanAdd(b), nil)
	}
}

// sweepBans drops expired bans, allowing the target to reconnect.
func (e *Engine) sweepBans(ns *netState, now time.Time) {
	n := now.UnixNano()
	ns.mu.Lock()
	var expired []*banRecord
	for k, b := range ns.bans {
		if b.expiresNano != 0 && n >= b.expiresNano {
			delete(ns.bans, k)
			expired = append(expired, b)
		}
	}
	ns.mu.Unlock()
	for _, b := range expired {
		e.log.Infof("mesh: ban on %q (origin %q) on net %x expired", b.target, b.origin, ns.spec.ID)
		e.redial(ns, b.endpoint) // reconnect immediately on expiry
	}
}

// ListBans returns the bans known on a network.
func (e *Engine) ListBans(networkID uint64) []BanInfo {
	ns := e.network(networkID)
	if ns == nil {
		return nil
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	out := make([]BanInfo, 0, len(ns.bans))
	for _, b := range ns.bans {
		// Resolve the issuing node's hostname: it's us if we issued it, else
		// look it up in the learned node registry (may be empty if we've never
		// had that node's identity, in which case the UI shows just the id).
		originHost := ""
		if b.origin == e.nodeID {
			originHost = e.hostname
		} else if ni := ns.nodes[b.origin]; ni != nil {
			originHost = ni.hostname
		}
		out = append(out, BanInfo{
			Target: b.target, Hostname: b.hostname, Origin: b.origin, OriginHostname: originHost,
			Notes: b.notes, At: b.atNano, Mine: b.origin == e.nodeID,
		})
	}
	return out
}

// PeerEndpoints returns the underlay endpoints of currently-connected peers, for
// persisting a bootstrap cache so a restart isn't reliant on a single seed.
func (e *Engine) PeerEndpoints(networkID uint64) []netip.AddrPort {
	ns := e.network(networkID)
	if ns == nil {
		return nil
	}
	ns.mu.RLock()
	eps := make([]netip.AddrPort, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		if ep := ps.ep(); ep.IsValid() {
			eps = append(eps, ep)
		}
	}
	ns.mu.RUnlock()
	// Never cache an overlay/mesh address as a bootstrap endpoint (belt-and-
	// suspenders alongside the AddSeed guard, and catches a session that roamed
	// onto an overlay source). Keeps peer_cache strictly underlay.
	out := eps[:0]
	for _, ep := range eps {
		if !e.isOverlayAddr(ep.Addr()) {
			out = append(out, ep)
		}
	}
	return out
}

// ManagedPeer is one remotely-manageable node learned from the mesh.
type ManagedPeer struct {
	NodeID    string
	Hostname  string
	Network   uint64
	Overlay4  netip.Addr
	Overlay6  netip.Addr
	WebPort   uint16
	LastSeen  time.Time
	Connected bool
	Manager   bool // peer currently advertises Manager mode (see IsManagerAddr)
}

// manageable reports whether this entry can actually be proxied to: it needs an
// overlay address and an advertised web-admin port.
func (m ManagedPeer) manageable() bool {
	return (m.Overlay4.IsValid() || m.Overlay6.IsValid()) && m.WebPort != 0
}

// betterManaged reports whether candidate a is a better representative than b for
// the same node observed on more than one network. A node commonly joins several
// networks under one node id; the dropdown shows it once, so we must pick the
// entry the operator can actually reach: prefer a manageable endpoint, then a
// connected one, then the most recently seen. Choosing purely by recency (the old
// behaviour) let a fresher-but-unreachable entry on one network hide a reachable
// entry on another, so the node flickered in and out of the selector.
func betterManaged(a, b ManagedPeer) bool {
	if am, bm := a.manageable(), b.manageable(); am != bm {
		return am
	}
	if a.Connected != b.Connected {
		return a.Connected
	}
	return a.LastSeen.After(b.LastSeen)
}

// ManagedPeers returns every node, across all networks, that advertised itself
// as managed and was heard from within maxAge. Self is never included. Deduped
// by node id, keeping the most recently seen record.
//
// A currently-connected peer is exempt from the maxAge check entirely: an
// active session is a definitive liveness signal on its own, and ni.lastSeen
// does NOT track it — it's set once at handshake (install()) and afterward
// only refreshed by *third-party* gossip mentioning this node (learnPeers sets
// it unconditionally for every entry, deliberately unlike managed/manager/
// webPort which stay handshake-authoritative for a connected peer — see
// learnPeers' "for a direct neighbour the handshake stays authoritative").
// Session keepalives (ctrlPing/ctrlPong) only ever touch the session's own
// lastRx, never the registry. So in a small mesh, or any time gossip about a
// specific already-connected peer happens not to arrive again for a while,
// lastSeen goes stale on a peer that is very much still alive — and applying
// the TTL there flickered it in and out of this list on exactly that gossip
// cadence, a live report matched precisely ("goes missing, comes back after a
// bit"). The TTL still fully applies to a peer known only indirectly (gossip-
// only, no live session) — that's the multi-hop case it exists for.
func (e *Engine) ManagedPeers(maxAge time.Duration) []ManagedPeer {
	now := time.Now()
	best := map[string]ManagedPeer{}
	for id, ns := range e.netSnapshot() {
		ns.mu.RLock()
		for nodeID, ni := range ns.nodes {
			if nodeID == e.nodeID || !ni.managed {
				continue
			}
			_, connected := ns.byNode[nodeID]
			if !connected && maxAge > 0 && now.Sub(ni.lastSeen) > maxAge {
				continue
			}
			cur := ManagedPeer{
				NodeID: nodeID, Hostname: ni.hostname, Network: id,
				Overlay4: ni.overlay4, Overlay6: ni.overlay6, WebPort: ni.webPort,
				LastSeen: ni.lastSeen, Connected: connected, Manager: ni.manager,
			}
			if prev, ok := best[nodeID]; !ok || betterManaged(cur, prev) {
				best[nodeID] = cur
			}
		}
		ns.mu.RUnlock()
	}
	out := make([]ManagedPeer, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	// best is a map, and Go deliberately randomizes map iteration order on
	// every single call — without this, every consumer (the header's node
	// picker, the speedtest source/target pickers, /api/cluster) saw this
	// list reshuffle on every poll even when the managed-peer set itself
	// hadn't changed. Same fix, same rationale, as ListPeers above: sort by
	// hostname, falling back to NodeID for a peer with no known hostname yet
	// and as the tiebreaker if two peers share one.
	sort.Slice(out, func(i, j int) bool {
		hi, hj := strings.ToLower(out[i].Hostname), strings.ToLower(out[j].Hostname)
		if hi == "" {
			hi = out[i].NodeID
		}
		if hj == "" {
			hj = out[j].NodeID
		}
		if hi != hj {
			return hi < hj
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

// SetManaged toggles this node's advertised managed mode live; new handshakes
// carry the updated flag, and every already-connected peer is notified right
// away too (announceClusterStateAll) — without that, a peer meshed before the
// toggle would keep whatever was true at its last handshake until it happened
// to reconnect, which could be indefinitely on a long-lived session. Managed
// reports the current value.
func (e *Engine) SetManaged(on bool) {
	e.managed.Store(on)
	e.announceClusterStateAll()
}
func (e *Engine) Managed() bool { return e.managed.Load() }

// SetManager toggles this node's advertised manager mode live — whether it may
// manage other Managed peers — the same way SetManaged toggles being managed,
// including the same immediate push to already-connected peers. This one
// matters even more in practice: IsManagerAddr can only authorize a proxied
// request once the *target* peer's registry entry for the caller says
// Manager, so without the push, flipping Manager on and immediately trying to
// use it against an already-meshed peer would 401 until something forced a
// reconnect. Manager reports the current value.
func (e *Engine) SetManager(on bool) {
	e.manager.Store(on)
	e.announceClusterStateAll()
}
func (e *Engine) Manager() bool { return e.manager.Load() }

// SetBGPASN updates this node's advertised effective BGP ASN live — see
// hsPayload.BGPASN's doc comment for what it's for. Unlike SetManaged/
// SetManager (each only ever called on an actual UI toggle), this is called
// on every AutoBGP reconcile poll (internal/webadmin/autobgp.go) regardless
// of whether anything changed, so it skips the announce — and the resulting
// ctrlClusterNotify round trip to every connected peer on every network —
// when the value is exactly what it already was. BGPASN reports the current
// value.
func (e *Engine) SetBGPASN(asn uint32) {
	if e.bgpASN.Swap(asn) == asn {
		return
	}
	e.announceClusterStateAll()
}
func (e *Engine) BGPASN() uint32 { return e.bgpASN.Load() }

// announceClusterStateAll pushes this node's current Managed/Manager/WebPort/
// TCPPort state to every currently connected peer, across every network, via
// ctrlClusterNotify. Called from SetManaged/SetManager so a live toggle is
// visible to already-meshed peers immediately instead of waiting for their
// next handshake (reconnect, roam, or restart) — the same gap that gossip
// deliberately leaves alone for a connected peer (control.go's "for a direct
// neighbour the handshake stays authoritative"), except here the update
// genuinely does come from the authoritative party itself over its own
// already-authenticated session, so there's no third-party-forgery concern
// the way there would be trusting this over gossip.
func (e *Engine) announceClusterStateAll() {
	for _, ns := range e.netSnapshot() {
		e.announceClusterState(ns)
	}
}

// announceClusterState sends one ctrlClusterNotify to every peer currently
// connected on ns, carrying this node's current Managed/Manager/WebPort/
// TCPPort/ExtraTCPPorts/ExtraUDPPorts/BGPASN. Wire shape mirrors the
// handshake's own "Managed-cluster advertisement" trailing field exactly
// ([mflag:1][webPort:2][tcpPort:2], mflag bit 0 Managed / bit 1 Manager —
// see hsPayload's encode/decode) so there's only one bit-packing convention
// for this data in the whole package, then the same appendPortList format
// the handshake uses for the two extra-port lists, then BGPASN as a further
// fixed 4-byte trailing field (see hsPayload.BGPASN).
func (e *Engine) announceClusterState(ns *netState) {
	var mflag byte
	if e.managed.Load() {
		mflag |= 1
	}
	if e.manager.Load() {
		mflag |= 2
	}
	body := []byte{ctrlClusterNotify, mflag}
	var wp [2]byte
	binary.BigEndian.PutUint16(wp[:], e.webPort)
	body = append(body, wp[:]...)
	var tp [2]byte
	binary.BigEndian.PutUint16(tp[:], uint16(e.fallbackPort.Load()))
	body = append(body, tp[:]...)
	body = appendPortList(body, loadPortList(&e.extraTCPPorts))
	body = appendPortList(body, loadPortList(&e.extraUDPPorts))
	// This node's current effective BGP ASN — a further trailing field,
	// fixed-width like its handshake counterpart (hsPayload.BGPASN). Present
	// unconditionally (not gated behind the port lists being present) since
	// this is always freshly written by this exact build, unlike a
	// backward-compat decode which has to tolerate an older *peer's*
	// shorter payload.
	var asnB [4]byte
	binary.BigEndian.PutUint32(asnB[:], e.bgpASN.Load())
	body = append(body, asnB[:]...)
	e.broadcastControl(ns, body)
}

// onClusterNotify applies a peer's freshly-pushed Managed/Manager/WebPort/
// TCPPort/ExtraTCPPorts/ExtraUDPPorts state to both its live session (ps) and
// its registry entry (ni). Unlike learnPeers' gossip path, this needs no
// "already connected" gate: ps *is* the direct, currently-authenticated
// session for this node id, so it is inherently the authoritative source for
// it — the same trust ps's own handshake fields already carry, just
// refreshed without a full re-handshake.
func (e *Engine) onClusterNotify(ps *peerSession, body []byte) {
	if len(body) < 1 {
		return
	}
	mflag := body[0]
	managed := mflag&1 != 0
	manager := mflag&2 != 0
	var webPort, tcpPort uint16
	var bgpASN uint32
	if len(body) >= 3 {
		webPort = binary.BigEndian.Uint16(body[1:3])
	}
	var extraTCP, extraUDP []uint16
	if len(body) >= 5 {
		tcpPort = binary.BigEndian.Uint16(body[3:5])
		// Extra TCP/UDP ports are further optional trailing fields, same
		// nested-only-if-the-previous-one-was-present chain as the
		// handshake payload (see decodeHSPayload) — an older peer's shorter
		// notify just leaves these nil rather than erroring.
		r := reader{b: body[5:]}
		if ports, ok := readPortList(&r); ok {
			extraTCP = ports
			if ports2, ok := readPortList(&r); ok {
				extraUDP = ports2
				// This node's current effective BGP ASN, same nesting rule:
				// absent on an older peer's shorter notify, which just
				// leaves this 0.
				if asnB, ok := r.take(4); ok {
					bgpASN = binary.BigEndian.Uint32(asnB)
				}
			}
		}
	}

	ns := ps.net
	ns.mu.Lock()
	ps.managed = managed
	ps.manager = manager
	ps.webPort = webPort
	ps.tcpPort = tcpPort
	ps.extraTCPPorts = extraTCP
	ps.extraUDPPorts = extraUDP
	ps.bgpASN = bgpASN
	if ni := ns.nodes[ps.nodeID]; ni != nil {
		ni.managed = managed
		ni.manager = manager
		ni.webPort = webPort
		ni.tcpPort = tcpPort
		ni.extraTCPPorts = extraTCP
		ni.extraUDPPorts = extraUDP
		ni.bgpASN = bgpASN
	}
	ns.mu.Unlock()
	e.log.Debugf("mesh: peer %q updated managed=%v manager=%v webPort=%d bgpASN=%d", ps.nodeID, managed, manager, webPort, bgpASN)
}

// routeAdvInterval is the live route re-advertisement cadence.
func (e *Engine) routeAdvInterval() time.Duration {
	if v := e.routeAdvNs.Load(); v > 0 {
		return time.Duration(v)
	}
	return defaultRouteAdvInterval
}

// SetRouteAdvInterval updates the route re-advertisement cadence live. Values
// below 1s are clamped; non-positive values restore the default.
func (e *Engine) SetRouteAdvInterval(d time.Duration) {
	if d <= 0 {
		d = defaultRouteAdvInterval
	} else if d < time.Second {
		d = time.Second
	}
	e.routeAdvNs.Store(int64(d))
}

// keepaliveInterval is the live NAT-keepalive cadence.
func (e *Engine) keepaliveInterval() time.Duration {
	if v := e.keepaliveNs.Load(); v > 0 {
		return time.Duration(v)
	}
	return defaultKeepaliveInterval
}

// SetKeepaliveInterval updates the NAT-keepalive cadence live. Values below
// 1s are clamped; non-positive values restore the default.
func (e *Engine) SetKeepaliveInterval(d time.Duration) {
	if d <= 0 {
		d = defaultKeepaliveInterval
	} else if d < time.Second {
		d = time.Second
	}
	e.keepaliveNs.Store(int64(d))
}

// peerTimeoutDuration is the live dead-session timeout — how long a session
// may go without received traffic before pruneDead tears it down.
func (e *Engine) peerTimeoutDuration() time.Duration {
	if v := e.peerTimeoutNs.Load(); v > 0 {
		return time.Duration(v)
	}
	return defaultPeerTimeout
}

// SetPeerTimeout updates the dead-session timeout live. Non-positive values
// restore the default (20s). An explicit value below the current keepalive
// interval is clamped up to it: a session timing out before a single
// keepalive round trip could even complete would just cause constant
// unnecessary reconnection thrashing, not faster failure detection.
func (e *Engine) SetPeerTimeout(d time.Duration) {
	if d <= 0 {
		d = defaultPeerTimeout
	} else if min := e.keepaliveInterval(); d < min {
		d = min
	}
	e.peerTimeoutNs.Store(int64(d))
}

// plausibleOverlayAddr rejects addresses that must never legitimately appear as
// a peer's overlay address: loopback, the unspecified address, link-local, and
// multicast. Keeping these out of any overlay-trust decision stops a malicious
// peer from advertising e.g. 127.0.0.1 to reach non-overlay targets.
func plausibleOverlayAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	return !ip.IsLoopback() && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() && !ip.IsInterfaceLocalMulticast()
}

// OverlayReachable reports whether ip is a usable speedtest/transfer target for
// this node. It must be a plausible overlay address (never loopback,
// link-local, or multicast — so a malicious peer can't point us at 127.0.0.1 or
// a cloud-metadata endpoint), AND must either fall inside one of this node's
// overlay subnets OR match the overlay address of a node currently in the mesh
// registry. The subnet check alone (OverlayContains) is too strict on a client
// peer that hasn't learned the subnet for the target's address family; the
// registry check covers a known peer in that case. A target on a different
// overlay network that this node neither shares a subnet with nor knows as a
// node returns false — which is correct, since it can't be reached anyway.
func (e *Engine) OverlayReachable(ip netip.Addr) bool {
	if !plausibleOverlayAddr(ip) {
		return false
	}
	return e.OverlayContains(ip) || e.IsOverlayAddr(ip)
}

// OverlayContains reports whether ip is a structurally valid overlay address: it
// falls inside one of this node's configured overlay subnets and is not a
// loopback/link-local/multicast address. Unlike IsOverlayAddr it does NOT
// consult the node registry, whose contents are advertised by (untrusted) peers
// — so a malicious peer cannot make an arbitrary address (its own underlay IP,
// 127.0.0.1, a cloud-metadata address) "look like" an overlay address. This is
// the address check used to authorize overlay-sourced management and to
// constrain the management proxy's target.
func (e *Engine) OverlayContains(ip netip.Addr) bool {
	if !plausibleOverlayAddr(ip) {
		return false
	}
	// A dual-stack listener reports inbound IPv4 connections as 4-in-6 mapped
	// addresses (::ffff:a.b.c.d), and netip.Prefix.Contains will not match those
	// against an IPv4 prefix. Unmap so an overlay v4 address is recognized however
	// it arrived — without this, overlay-sourced management was always rejected.
	ip = ip.Unmap()
	for _, ns := range e.netSnapshot() {
		ns.mu.RLock()
		in := (ns.subnet4.IsValid() && ns.subnet4.Contains(ip)) ||
			(ns.subnet6.IsValid() && ns.subnet6.Contains(ip))
		ns.mu.RUnlock()
		if in {
			return true
		}
	}
	return false
}

// IsOverlayAddr reports whether ip is an overlay address of a node currently in
// the mesh registry (used to authorize management arriving over the overlay).
func (e *Engine) IsOverlayAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	for _, ns := range e.netSnapshot() {
		ns.mu.RLock()
		hit := (ns.self4.IsValid() && ip == ns.self4) || (ns.self6.IsValid() && ip == ns.self6)
		if !hit {
			for _, ni := range ns.nodes {
				if (ni.overlay4.IsValid() && ip == ni.overlay4) || (ni.overlay6.IsValid() && ip == ni.overlay6) {
					hit = true
					break
				}
			}
		}
		ns.mu.RUnlock()
		if hit {
			return true
		}
	}
	return false
}

// IsManagerAddr reports whether ip belongs to a node currently in the mesh
// registry (direct neighbor or gossip-learned, same as IsOverlayAddr) that has
// advertised Manager mode. This is the other half of the Manager/Managed split:
// webadmin's overlay-sourced management bypass now requires both — the local
// node must be Managed (OverlayContains: is this address structurally inside
// one of my overlay subnets) AND the caller must be a known Manager (this
// method) — so being Managed no longer means "any mesh peer may manage me,"
// only "any *Manager* peer may."
//
// This shares IsOverlayAddr's registry-trust trade-off: the manager flag for a
// non-neighbor rides untrusted gossip (see peerEntry/flagManager), so in
// principle a malicious peer could mis-tag some address inside the subnet as
// belonging to a manager. That's bounded by OverlayContains already having
// required a structurally valid overlay address first — the registry can
// mislabel an address, but it can't manufacture a connection that genuinely
// arrives from an address whose real owner isn't the entity making the
// request, since that would require holding a live mesh session for it. A
// direct neighbor's own handshake is authoritative and can't be overridden by
// a third party's gossip (see learnPeers' "if !connected" gate), so this gap
// only exists at all for a peer known solely through relay/gossip.
func (e *Engine) IsManagerAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	for _, ns := range e.netSnapshot() {
		ns.mu.RLock()
		hit := false
		for _, ni := range ns.nodes {
			if ni.manager && ((ni.overlay4.IsValid() && ip == ni.overlay4) || (ni.overlay6.IsValid() && ip == ni.overlay6)) {
				hit = true
				break
			}
		}
		ns.mu.RUnlock()
		if hit {
			return true
		}
	}
	return false
}

// ListPeers returns the connected peers on a network.
func (e *Engine) ListPeers(networkID uint64) []PeerInfo {
	ns := e.network(networkID)
	if ns == nil {
		return nil
	}
	// Grab the transport before locking ns: if it can fall back to TCP/TLS, a
	// peer with a live fallback connection is currently reached over TCP.
	e.mu.RLock()
	fd, hasFB := e.tr.(fallbackDialer)
	e.mu.RUnlock()
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	out := make([]PeerInfo, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		transport := "udp"
		if hasFB && fd.HasFallback(ps.ep()) {
			transport = "tcp"
		}
		pi := PeerInfo{NodeID: ps.nodeID, Hostname: ps.hostname, Endpoint: ps.ep().String(), Relayed: ps.getRelay() != nil,
			Transport:     transport,
			EstablishedAt: ps.established.UnixNano(),
			KeyLabel:      ns.keyLabelFor(ps.keyID),
			Notes:         ns.peerNotes[ps.nodeID],
			PathMTU:       int(ps.effMTU.Load()),
			FragsSent:     ps.fragsSent.Load(),
			FragSendDrop:  ps.fragSendDrop.Load(),
			FragsRcvd:     ps.fragsRcvd.Load(),
			ReasmOK:       ps.reasmOK.Load(),
			ReasmDrop:     ps.reasmDrop.Load(),
			SpoofDrop:     ps.spoofDrop.Load(),
			TxBytes:       ps.txBytes.Load(),
			RxBytes:       ps.rxBytes.Load(),
			BGPASN:        ps.bgpASN,
		}
		if r := ps.getRelay(); r != nil {
			if r.hostname != "" {
				pi.RelayVia = r.hostname
			} else {
				pi.RelayVia = r.nodeID
			}
		}
		if rtt := ps.rttNanos.Load(); rtt != 0 {
			pi.RTTMs = float64(rtt) / 1e6
		}
		if ps.overlay4.IsValid() {
			pi.Overlay4 = ps.overlay4.String()
		}
		if ps.overlay6.IsValid() {
			pi.Overlay6 = ps.overlay6.String()
		}
		out = append(out, pi)
	}
	// ns.byNode is a map, and Go deliberately randomizes map iteration order
	// on every single call — so without this, every consumer (the web UI's
	// peers page, `gravinet list`, the /api/status JSON) saw the list
	// reshuffle on every poll even when the peer set itself hadn't changed at
	// all. Sort by hostname (what's actually shown/meant by "peer"), falling
	// back to NodeID for a peer with no known hostname yet and as a tiebreaker
	// for two peers that happen to share one, so the order is fully
	// deterministic either way.
	sort.Slice(out, func(i, j int) bool {
		hi, hj := strings.ToLower(out[i].Hostname), strings.ToLower(out[j].Hostname)
		if hi == "" {
			hi = out[i].NodeID
		}
		if hj == "" {
			hj = out[j].NodeID
		}
		if hi != hj {
			return hi < hj
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

// NetworkIDs returns the configured network ids (for the control API).
// SelfID returns this node's own node id.
func (e *Engine) SelfID() string { return e.nodeID }

// SelfOverlay returns one of this node's overlay addresses (IPv4 preferred), or
// the zero Addr if none is assigned yet. Used so a peer can run a speedtest
// against this node without resolving it from its own registry.
func (e *Engine) SelfOverlay() netip.Addr {
	for _, ns := range e.netSnapshot() {
		s4, s6 := ns.selfAddrs()
		if s4.IsValid() {
			return s4
		}
		if s6.IsValid() {
			return s6
		}
	}
	return netip.Addr{}
}

// SelfPeer returns this node's own identity as it would appear in networkID's
// peer listing — hostname, node id, and this node's own overlay address(es)
// there — reusing PeerInfo so the admin UI's peers table can show a "this
// node" row alongside the peers it actually connects to (its endpoint,
// relay, transport, and session fields are left zero-valued: none of those
// describe a connection to yourself). ok is false if networkID isn't one of
// this node's configured networks.
func (e *Engine) SelfPeer(networkID uint64) (pi PeerInfo, ok bool) {
	ns := e.network(networkID)
	if ns == nil {
		return PeerInfo{}, false
	}
	s4, s6 := ns.selfAddrs()
	pi = PeerInfo{NodeID: e.nodeID, Hostname: e.hostname, BGPASN: e.bgpASN.Load()}
	if s4.IsValid() {
		pi.Overlay4 = s4.String()
	}
	if s6.IsValid() {
		pi.Overlay6 = s6.String()
	}
	return pi, true
}

// IfaceInfo maps an overlay network to its kernel interface, for the metrics UI.
type IfaceInfo struct {
	NetworkID uint64
	Name      string // network name
	Iface     string // kernel interface name (e.g. mesh0)
}

// Interfaces returns the live overlay-network -> kernel-interface mapping,
// sorted by network id for a stable order.
func (e *Engine) Interfaces() []IfaceInfo {
	cur := e.netSnapshot()
	out := make([]IfaceInfo, 0, len(cur))
	for id, ns := range cur {
		iface := ""
		if ns.spec.Dev != nil {
			iface = ns.spec.Dev.Name()
		}
		out = append(out, IfaceInfo{NetworkID: id, Name: ns.spec.Name, Iface: iface})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NetworkID < out[j].NetworkID })
	return out
}

// LoopDrops returns the number of this node's own underlay datagrams that were
// routed back into the tunnel and dropped by the loop guard since start.
func (e *Engine) LoopDrops() uint64 { return e.loopDrops.Load() }

func (e *Engine) NetworkIDs() []uint64 {
	cur := e.netSnapshot()
	out := make([]uint64, 0, len(cur))
	for id := range cur {
		out = append(out, id)
	}
	// Sort for a stable order across calls — a Go map ranges randomly, which made
	// the web UI's per-network cards swap places on every status poll.
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// applyBan tears down any session to target and removes it from routing/seeds so
// the initiator won't re-dial it. The handshake path rejects it by node id.
func (e *Engine) applyBan(ns *netState, target string) {
	if target == e.nodeID {
		return // can't disconnect ourselves; record only
	}
	var victim *peerSession
	var drop []netip.AddrPort
	ns.mu.Lock()
	if ni := ns.nodes[target]; ni != nil && ni.endpoint.IsValid() {
		drop = append(drop, ni.endpoint)
	}
	if ps := ns.byNode[target]; ps != nil {
		victim = ps
		drop = append(drop, ps.ep())
		delete(ns.byNode, target)
		if ps.overlay4.IsValid() && ns.routes4[ps.overlay4] == ps {
			delete(ns.routes4, ps.overlay4)
		}
		if ps.overlay6.IsValid() && ns.routes6[ps.overlay6] == ps {
			delete(ns.routes6, ps.overlay6)
		}
	}
	delete(ns.nodes, target)
	if len(drop) > 0 {
		kept := ns.seeds[:0]
		for _, s := range ns.seeds {
			drophit := false
			for _, d := range drop {
				if s == d {
					drophit = true
					break
				}
			}
			if !drophit {
				kept = append(kept, s)
			}
		}
		ns.seeds = kept
	}
	ns.publishFwd()
	ns.mu.Unlock()

	if victim != nil {
		e.mu.Lock()
		for idx, ps := range e.sessions {
			if ps.nodeID == target && ps.net == ns {
				delete(e.sessions, idx)
			}
		}
		e.mu.Unlock()
	}
	// A banned node's redistributed routes must go too — it can't carry traffic.
	e.dropNodeRoutes(ns, target)
}

// endpointOf returns the best-known underlay endpoint for a node id, or the zero
// value if we have none. Caller must hold ns.mu.
func (ns *netState) endpointOf(target string) netip.AddrPort {
	if ps := ns.byNode[target]; ps != nil {
		if ep := ps.ep(); ep.IsValid() {
			return ep
		}
	}
	if ni := ns.nodes[target]; ni != nil && ni.endpoint.IsValid() {
		return ni.endpoint
	}
	return netip.AddrPort{}
}

// hostnameOf returns the best-known hostname for a node, from a live session or
// the learned registry. Used to label bans (applyBan drops the node from the
// registry, so the name must be captured at ban time). "" if never learned.
func (ns *netState) hostnameOf(target string) string {
	if ps := ns.byNode[target]; ps != nil && ps.hostname != "" {
		return ps.hostname
	}
	if ni := ns.nodes[target]; ni != nil {
		return ni.hostname
	}
	return ""
}

// redial schedules an immediate reconnect to a formerly-banned peer's last-known
// underlay endpoint, so an unban (or ban expiry) reconnects in about a second
// instead of waiting out peerTimeout. No-op if we never recorded an endpoint.
func (e *Engine) redial(ns *netState, ep netip.AddrPort) {
	if !ep.IsValid() {
		return
	}
	ns.mu.Lock()
	delete(ns.seedBackoff, ep) // clear any cooldown so initLoop dials it now
	ns.mu.Unlock()
	e.AddSeed(ns.spec.ID, ep)
}

// ---- control handlers ----

func (e *Engine) onBanAdd(ps *peerSession, body []byte) {
	rec, ok := decodeBanAdd(body)
	if !ok {
		return
	}
	ns := ps.net
	key := banKey(rec.origin, rec.target)
	ns.mu.Lock()
	existing := ns.bans[key]
	// Accept a new ban, or a refresh that extends the expiry; ignore stale dupes.
	fresh := existing == nil || rec.expiresNano > existing.expiresNano
	if fresh {
		if existing == nil {
			rec.endpoint = ns.endpointOf(rec.target) // remember for fast re-dial on unban
			rec.hostname = ns.hostnameOf(rec.target) // capture the name before applyBan clears it
		} else {
			rec.endpoint = existing.endpoint // preserve across a refresh
			rec.hostname = existing.hostname
		}
		ns.bans[key] = rec
	}
	ns.mu.Unlock()
	if !fresh {
		return // duplicate/stale; stop the flood
	}
	if existing == nil {
		e.applyBan(ns, rec.target)
		e.log.Infof("mesh: ban received: %q banned by %q on net %x (%q)", rec.target, rec.origin, ns.spec.ID, rec.notes)
	}
	e.floodControl(ns, encodeBanAdd(rec), ps)
}

func (e *Engine) onBanDel(ps *peerSession, body []byte) {
	origin, target, ok := decodeBanDel(body)
	if !ok {
		return
	}
	ns := ps.net
	ns.mu.Lock()
	had := false
	var redials []netip.AddrPort
	if origin == "" { // wildcard force-unban: clear all origins for target
		for k, b := range ns.bans {
			if b.target == target {
				redials = append(redials, b.endpoint)
				delete(ns.bans, k)
				had = true
			}
		}
	} else {
		key := banKey(origin, target)
		if b, ok := ns.bans[key]; ok {
			redials = append(redials, b.endpoint)
			delete(ns.bans, key)
			had = true
		}
	}
	ns.mu.Unlock()
	if !had {
		return
	}
	for _, ep := range redials {
		e.redial(ns, ep) // reconnect immediately on a gossiped unban
	}
	e.log.Infof("mesh: unban received: %q (origin %q) on net %x", target, origin, ns.spec.ID)
	e.floodControl(ns, encodeBanDel(origin, target), ps)
}

// ---- codecs ----

func encodeBanAdd(b *banRecord) []byte {
	out := []byte{ctrlBanAdd}
	out = appendLenStr(out, b.origin)
	out = appendLenStr(out, b.target)
	out = appendLenStr(out, b.notes)
	var ts [8]byte
	putUint64(ts[:], uint64(b.atNano))
	out = append(out, ts[:]...)
	var exp [8]byte
	putUint64(exp[:], uint64(b.expiresNano))
	return append(out, exp[:]...)
}

func decodeBanAdd(b []byte) (*banRecord, bool) {
	r := reader{b: b}
	origin, ok := r.lenStr()
	if !ok {
		return nil, false
	}
	target, ok := r.lenStr()
	if !ok {
		return nil, false
	}
	notes, ok := r.lenStr()
	if !ok {
		return nil, false
	}
	ts, ok := r.u64()
	if !ok {
		return nil, false
	}
	exp, ok := r.u64()
	if !ok {
		return nil, false
	}
	return &banRecord{origin: origin, target: target, notes: notes, atNano: int64(ts), expiresNano: int64(exp)}, true
}

func encodeBanDel(origin, target string) []byte {
	out := []byte{ctrlBanDel}
	out = appendLenStr(out, origin)
	out = appendLenStr(out, target)
	return out
}

func decodeBanDel(b []byte) (origin, target string, ok bool) {
	r := reader{b: b}
	origin, ok = r.lenStr()
	if !ok {
		return "", "", false
	}
	target, ok = r.lenStr()
	if !ok {
		return "", "", false
	}
	return origin, target, true
}

func putUint64(b []byte, v uint64) {
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
}
