package webadmin

// autoBGPReconciler is BGPConfig.AutoBGP's engine: a self-numbering,
// self-peering BGP speaker instead of a hand-maintained one. Structurally
// this is bgpMeshRedistributor's sibling (bgp_redistribute.go) — same
// reason for polling rather than reacting to an event (a peer connecting or
// disconnecting isn't a config edit gravinet gets told about, it's live
// mesh state this package has to go check), same shape (a ticker, a sync()
// method callable directly for an immediate resync after a config save,
// a close() for shutdown) — but where that one only ever pushes into the
// mesh (Backend.SetBGPRoutes) and never touches gravinet's own config, this
// one edits BGPConfig itself (ASN, RouterID, Enabled, Neighbors) via
// mutateConfig, then pushes that to FRR either the same way a manual save
// through the web UI does (applyBGP, for the rare cases that actually
// change the router bgp block's own identity) or via a lighter incremental
// vtysh path of its own for routine neighbor churn (applyBGPIncremental) —
// see sync()'s doc comment for which is which and why.
//
// A peer's remote AS is never derived from its address. It's gossiped: see
// mesh.Engine.SetBGPASN / hsPayload.BGPASN's doc comment for why deriving it
// independently on both ends can't be made to agree in general (two nodes
// with different partial views of each other's network memberships can
// each compute a different address as "a peer's first tunnel address"), and
// why gossiping the value that peer already knows about itself — its own
// current effective ASN, AutoBGP-derived or hand-typed — sidesteps that
// entirely, and also makes a peer with a real manually-assigned ASN
// interoperate correctly instead of getting a fabricated one.
//
// See BGPConfig's doc comment (internal/config/config.go) for exactly what
// AutoBGP derives and maintains, and why Password=="autobgp" is both the
// fixed MD5 password it sets and the marker it uses to recognize a Neighbor
// entry as its own on a later pass.

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"sort"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// autoBGPPollInterval mirrors bgpRedistributePollInterval's reasoning: a
// peer connecting or disconnecting should show up as a BGP neighbor
// appearing/disappearing within a handful of seconds, not minutes, and
// there's no config-edit event to react to instead — ListPeers is polled
// the same way bgpLearnedRoutesFn is. Tighter than bgpRedistributePollInterval
// (10s vs 15s): the common case here is a no-op comparison against an
// in-memory peer list, and even the uncommon case is now a handful of cheap
// incremental vtysh commands rather than a full restart (see
// applyBGPIncremental), so there's less reason to space it out.
const autoBGPPollInterval = 10 * time.Second

// autoBGPPassword is the fixed MD5 password AutoBGP sets on every neighbor
// it creates, and — doubling as its own management marker — the sole
// signal it uses to recognize a Neighbor entry as one of its own on a later
// pass. A real neighbor configured by hand is exceedingly unlikely to
// coincidentally carry this exact password; if one somehow did, AutoBGP
// would treat it as its own from that point on — the same trade-off any
// convention-based ownership marker makes, and the only one available
// here since Description is spoken for (spec: "the description will be
// the peer name") and there is nowhere else on a BGPNeighbor to hide a tag.
const autoBGPPassword = "autobgp"

// autoBGPPrivateASNBase/Size bound the 4-byte private-use ASN range (RFC
// 6996: 4200000000-4294967294 inclusive), the space deriveASNFromIPv4 maps
// into. Deliberately not the whole uint32 range: 4294967295 is reserved
// (RFC 7300's "last" AS) and everything below 4200000000 is either a real
// public/legacy AS or the 2-byte-compatible range — using any of that would
// risk colliding with a genuine AS number somewhere.
const (
	autoBGPPrivateASNBase = 4200000000
	autoBGPPrivateASNSize = 4294967294 - autoBGPPrivateASNBase + 1 // 94,967,295
)

type autoBGPReconciler struct {
	s    *Server
	stop chan struct{}
}

func newAutoBGPReconciler(s *Server) *autoBGPReconciler {
	return &autoBGPReconciler{s: s, stop: make(chan struct{})}
}

func (r *autoBGPReconciler) run() {
	t := time.NewTicker(autoBGPPollInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.sync()
		}
	}
}

func (r *autoBGPReconciler) close() { close(r.stop) }

// deriveASNFromIPv4 maps an IPv4 address into the 4-byte private ASN range
// by folding its 32-bit value into autoBGPPrivateASNSize's width via modulo
// and offsetting into the range. Deterministic and collision-free for any
// realistically-sized overlay subnet (a /8 or smaller has, at most, ~16.8M
// addresses — well under the ~95M-wide range this maps into); two addresses
// only ever collide if the mesh's actual overlay subnet were larger than
// the private ASN range itself, an extreme case this doesn't try to guard
// against.
//
// Only ever used for this node's own local ASN, derived from its own first
// tunnel IPv4 (see autoBGPSelfTunnel4) — never for a peer's. A peer's
// remote AS is gossiped, not derived (see this file's own doc comment for
// why); this function existing at all is purely a "what does *this* node
// call itself" question, which has none of the cross-node-agreement problem
// deriving a peer's AS would.
//
// ip must be an IPv4 (or IPv4-in-6) address — the only caller already knows
// it's v4 — so there's no error return; an unexpected v6 address maps to 0,
// a value Validate() already rejects as "no AS number" rather than silently
// accepting a bogus one.
func deriveASNFromIPv4(ip netip.Addr) uint32 {
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if !ip.Is4() {
		return 0
	}
	b := ip.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return autoBGPPrivateASNBase + (v % autoBGPPrivateASNSize)
}

// autoBGPPeer is one mesh peer's identity, tunnel addresses, and gossiped
// BGP ASN, as gathered across every network this node belongs to (see
// gatherAutoBGPPeers) — the per-peer input to building the desired Neighbor
// set.
type autoBGPPeer struct {
	nodeID   string
	hostname string
	v4, v6   netip.Addr
	asn      uint32 // gossiped effective ASN (mesh.PeerInfo.BGPASN); 0 = no BGP there
}

// autoBGPSelfTunnel4 returns this node's own first tunnel IPv4 address —
// SelfPeer's Overlay4 from the first (lowest network id, NetworkIDs' own
// sorted order) network that has one assigned. The zero Addr if none of
// this node's networks have assigned it a v4 overlay address yet (DAD still
// pending, or every network here is v6-only).
func autoBGPSelfTunnel4(be Backend) netip.Addr {
	for _, id := range be.NetworkIDs() {
		pi, ok := be.SelfPeer(id)
		if !ok || pi.Overlay4 == "" {
			continue
		}
		if a, err := netip.ParseAddr(pi.Overlay4); err == nil {
			return a
		}
	}
	return netip.Addr{}
}

// gatherAutoBGPPeers collects every currently-connected mesh peer across
// every network this node belongs to, deduplicated by node id (the same
// peer can be a member of more than one shared network, and AutoBGP
// manages exactly one neighbor pair for it either way) — each with its
// first tunnel IPv4 and IPv6 address ("first" meaning the earliest network,
// NetworkIDs' sorted order, on which that peer has one assigned — every
// network the peer's on is considered for each address family
// independently, not "the first network it appears on, whole") and its
// gossiped BGP ASN (the first non-zero value seen across those same
// networks — a peer's ASN is one fact about itself, not something that
// legitimately differs per network, so any network it's advertised on
// should agree). Sorted by node id, so the result — and therefore the
// desired Neighbor list built from it — doesn't reorder from one poll to
// the next just because ListPeers' own per-network ordering happened to
// shuffle.
func gatherAutoBGPPeers(be Backend) []autoBGPPeer {
	byID := map[string]*autoBGPPeer{}
	for _, netID := range be.NetworkIDs() {
		for _, p := range be.ListPeers(netID) {
			if p.NodeID == "" {
				continue
			}
			ag, ok := byID[p.NodeID]
			if !ok {
				ag = &autoBGPPeer{nodeID: p.NodeID}
				byID[p.NodeID] = ag
			}
			if ag.hostname == "" {
				ag.hostname = p.Hostname
			}
			if !ag.v4.IsValid() && p.Overlay4 != "" {
				if a, err := netip.ParseAddr(p.Overlay4); err == nil {
					ag.v4 = a
				}
			}
			if !ag.v6.IsValid() && p.Overlay6 != "" {
				if a, err := netip.ParseAddr(p.Overlay6); err == nil {
					ag.v6 = a
				}
			}
			if ag.asn == 0 {
				ag.asn = p.BGPASN
			}
		}
	}
	out := make([]autoBGPPeer, 0, len(byID))
	for _, ag := range byID {
		out = append(out, *ag)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].nodeID < out[j].nodeID })
	return out
}

// desiredAutoBGPNeighbors builds the Neighbor set AutoBGP wants right now
// from peers (gatherAutoBGPPeers' output): one entry for a peer's tunnel
// IPv4 address if it has one, and a second for its tunnel IPv6 if it has
// one — a v4-only, v6-only, or dual-stack peer all get exactly the entries
// their own tunnel addresses support, both sharing that peer's one
// gossiped ASN. A peer whose gossiped ASN is 0 (no BGP configured there —
// or a peer too old to gossip it at all, indistinguishable from genuinely
// having none) has nothing to peer with it as, and one with neither tunnel
// address has nowhere to point a neighbor regardless of its ASN; either
// way, it's skipped entirely rather than half-configured. Sorted by peer
// address, so the result — and mergeAutoBGPNeighbors' diff against the
// existing config — is stable across polls when the peer set hasn't
// actually changed.
func desiredAutoBGPNeighbors(peers []autoBGPPeer) []config.BGPNeighbor {
	var out []config.BGPNeighbor
	for _, p := range peers {
		if p.asn == 0 || (!p.v4.IsValid() && !p.v6.IsValid()) {
			continue
		}
		name := p.hostname
		if name == "" {
			name = p.nodeID
		}
		if p.v4.IsValid() {
			out = append(out, config.BGPNeighbor{Peer: p.v4.String(), RemoteAS: p.asn, Description: name, Password: autoBGPPassword, BFD: true})
		}
		if p.v6.IsValid() {
			out = append(out, config.BGPNeighbor{Peer: p.v6.String(), RemoteAS: p.asn, Description: name, Password: autoBGPPassword, BFD: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Peer < out[j].Peer })
	return out
}

// mergeAutoBGPNeighbors reconciles the AutoBGP-managed subset of existing
// against desired (desiredAutoBGPNeighbors' output, already sorted by
// Peer). Every entry whose Password isn't exactly autoBGPPassword is
// carried through completely untouched, in its original position, no
// matter what its Peer address is — AutoBGP never adds, edits, or removes
// anything it didn't create itself (see BGPConfig.AutoBGP's doc comment).
// If a manual entry happens to share a peer address with something
// desired would otherwise create there, the manual entry wins outright:
// desired's entry for that address is simply never added, rather than
// coexisting as a duplicate. An autobgp-managed entry whose Peer is still
// in desired is refreshed to desired's current shape — covers a peer's
// hostname or gossiped ASN having changed, or its Shutdown/BFD having
// drifted from what AutoBGP wants (e.g. a manual double-click on the
// neighbor's state tag); "maintain" means enforced on every pass, not set
// once and left alone. One whose Peer fell out of desired — the peer
// disconnected, or lost the tunnel address that produced it — is dropped.
// Anything left in desired with no existing entry at all (a newly-online
// peer) is appended.
//
// added and removed are exactly the entries that changed — a refreshed
// entry counts as neither, since sync()'s incremental apply path (see
// applyBGPIncremental) only needs to *add* a neighbor that wasn't there at
// all or *remove* one that's gone; a refresh (e.g. just its description
// changing) doesn't need either a live `no neighbor` retraction or a fresh
// `neighbor ... remote-as` line, and there's no vtysh incantation for
// "patch the description in place" worth the complexity when a full
// re-add achieves the same end state. removed is peer addresses only (all
// `no neighbor <peer>` needs); added is full Neighbor values (everything
// `neighbor <peer> remote-as ...` and its follow-up lines need). changed
// reports whether result actually differs from existing at all, so sync()
// can skip writing/reloading FRR entirely when a poll finds nothing to do —
// the common case once a mesh has settled, the same "skip the round-trip
// entirely" shape bgpMeshRedistributor.sync() uses for its own no-op case.
func mergeAutoBGPNeighbors(existing, desired []config.BGPNeighbor) (result, added []config.BGPNeighbor, removed []string, changed bool) {
	desiredByPeer := make(map[string]config.BGPNeighbor, len(desired))
	for _, d := range desired {
		desiredByPeer[d.Peer] = d
	}
	// handled marks every peer address existing already has a slot for,
	// managed or not — a manual entry occupying the same address as a
	// desired one still means that address is spoken for, and the desired
	// loop below must not also append its own entry alongside it.
	handled := make(map[string]bool, len(existing))
	result = make([]config.BGPNeighbor, 0, len(existing)+len(desired))
	for _, nb := range existing {
		handled[nb.Peer] = true
		if nb.Password != autoBGPPassword {
			result = append(result, nb)
			continue
		}
		if d, ok := desiredByPeer[nb.Peer]; ok {
			result = append(result, d)
			if d != nb {
				changed = true
			}
		} else {
			changed = true
			removed = append(removed, nb.Peer) // dropped: peer no longer online/managed
		}
	}
	for _, d := range desired {
		if handled[d.Peer] {
			continue
		}
		result = append(result, d)
		added = append(added, d)
		changed = true
	}
	return result, added, removed, changed
}

// runVtyshConfig runs cmds as one atomic vtysh config session —
// "configure terminal" followed by each of cmds in order, exactly what an
// operator typing them by hand at the vtysh prompt would produce. Same
// hard wall-clock bound and abandon-on-wedge behavior as runVtysh (frr.go);
// ok is false if vtysh is absent, the invocation errored, or it exceeded
// the bound. Used only by applyBGPIncremental — everything else in this
// package that talks to vtysh is a read (runVtysh itself, or the show-*
// queries built on it).
func runVtyshConfig(cmds []string) (ok bool) {
	bin, present := vtyshPath()
	if !present {
		return false
	}
	args := make([]string, 0, 2*(len(cmds)+1))
	args = append(args, "-c", "configure terminal")
	for _, c := range cmds {
		args = append(args, "-c", c)
	}
	ch := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), bgpQueryTimeout)
		defer cancel()
		_, err := exec.CommandContext(ctx, bin, args...).Output()
		ch <- err
	}()
	select {
	case err := <-ch:
		return err == nil
	case <-time.After(bgpQueryTimeout + 2*time.Second):
		return false
	}
}

// autoBGPAddNeighborCmds is the incremental-vtysh equivalent of the lines
// renderFRR (frr.go) writes to frr.conf for one neighbor — remote-as,
// description, password, bfd, then activating it in the right
// address-family — issued live instead of only to the file. Kept in exact
// lockstep with renderFRR's own per-neighbor block on purpose: this is the
// same neighbor, applied through a different mechanism, and any drift
// between the two would mean the live daemon and the persisted config
// disagree about what a "correctly configured" neighbor looks like.
func autoBGPAddNeighborCmds(asn uint32, n config.BGPNeighbor) []string {
	cmds := []string{fmt.Sprintf("router bgp %d", asn)}
	cmds = append(cmds, fmt.Sprintf("neighbor %s remote-as %d", n.Peer, n.RemoteAS))
	if d := filterInline(strings.TrimSpace(n.Description), 60, false); d != "" {
		cmds = append(cmds, fmt.Sprintf("neighbor %s description %s", n.Peer, d))
	}
	if n.Password != "" {
		if pw := filterInline(n.Password, 80, true); pw != "" {
			cmds = append(cmds, fmt.Sprintf("neighbor %s password %s", n.Peer, pw))
		}
	}
	if n.BFD {
		cmds = append(cmds, fmt.Sprintf("neighbor %s bfd", n.Peer))
	}
	if n.Shutdown {
		cmds = append(cmds, fmt.Sprintf("neighbor %s shutdown", n.Peer))
	}
	if isIPv6Peer(n.Peer) {
		// FRR defaults every neighbor active in ipv4 unicast, v6 peers
		// included; explicitly deactivate a v6 peer there so it isn't left
		// running an IPv4 unicast exchange over its v6 session, then
		// activate it in ipv6 unicast instead — exactly what renderFRR does
		// for a full config write (frr.go), just issued as two live
		// address-family blocks instead of one written to file.
		cmds = append(cmds, "address-family ipv4 unicast", fmt.Sprintf("no neighbor %s activate", n.Peer), "exit-address-family")
		cmds = append(cmds, "address-family ipv6 unicast", fmt.Sprintf("neighbor %s activate", n.Peer), "exit-address-family")
	} else {
		cmds = append(cmds, "address-family ipv4 unicast", fmt.Sprintf("neighbor %s activate", n.Peer), "exit-address-family")
	}
	return cmds
}

// autoBGPRemoveNeighborCmds retracts one neighbor entirely — FRR's `no
// neighbor <peer>` removes its address-family activation along with it, so
// there's no separate "no activate" step needed the way adding one has an
// explicit activate step.
func autoBGPRemoveNeighborCmds(asn uint32, peer string) []string {
	return []string{fmt.Sprintf("router bgp %d", asn), fmt.Sprintf("no neighbor %s", peer)}
}

// applyBGPIncremental is AutoBGP's own FRR-apply path for routine neighbor
// churn: unlike applyBGP (frr.go), it never restarts or reloads the
// daemon — it writes the freshly-rendered frr.conf (so a *future* real
// restart starts from the correct state) and then issues the exact
// incremental vtysh commands an operator would type by hand for just the
// neighbors that changed, live, on the already-running daemon. Every other
// BGP session on the box — including any manually-configured one entirely
// unrelated to AutoBGP — is left completely undisturbed, which a restart
// cannot promise; that matters a great deal here specifically, since
// neighbor churn (a peer connecting or disconnecting) is the routine,
// expected case for this feature, not a rare event the way it is for a
// hand-edited BGP config.
//
// Only ever used when the ASN/router-id/Enabled state itself didn't change
// (see sync()) — those are rare, one-time changes to the router bgp
// block's own identity and go through applyBGP's normal restart/reload
// path instead, which is also what establishes the router bgp block in the
// first place (there's nothing to incrementally patch before that's run
// at least once).
//
// A vtysh command that fails (daemon momentarily unresponsive, etc.) is
// logged and left there — not retried, and never escalated to a forced
// restart just to compensate. frr.conf has already been updated to the
// correct state by the time this runs, so the next real restart or reload
// (whenever one next happens, for any reason) reconciles it regardless.
func applyBGPIncremental(next config.BGPConfig, added []config.BGPNeighbor, removed []string, meshRoutes []string, log *logx.Logger) {
	if !frrInstalled() {
		return
	}
	if err := writeAtomicFile(frrConf, renderFRR(next, meshRoutes...)); err != nil {
		if log != nil {
			log.Warnf("autobgp: writing frr.conf: %v", err)
		}
		return
	}
	for _, peer := range removed {
		if !runVtyshConfig(autoBGPRemoveNeighborCmds(next.ASN, peer)) && log != nil {
			log.Warnf("autobgp: vtysh could not remove neighbor %s live; frr.conf is updated and a future restart/reload will pick it up", peer)
		}
	}
	for _, n := range added {
		if !runVtyshConfig(autoBGPAddNeighborCmds(next.ASN, n)) && log != nil {
			log.Warnf("autobgp: vtysh could not add neighbor %s live; frr.conf is updated and a future restart/reload will pick it up", n.Peer)
		}
	}
}

// sync is one reconcile pass. Always gossips this node's current effective
// BGP ASN first (Backend.SetBGPASN), whether or not AutoBGP itself is what
// produced it — a manually-configured real ASN deserves the same
// visibility to any AutoBGP peer as a derived one. Then, only if AutoBGP is
// on: derives this node's own ASN/router-id if it needs to, gathers who's
// actually online right now, diffs that against the stored Neighbors list,
// and — only if something actually needs to change — persists it and
// pushes it to FRR, via applyBGP's full restart/reload path if the ASN,
// router-id, or Enabled itself changed (rare — the router bgp block's own
// identity), or via applyBGPIncremental's live vtysh commands otherwise
// (the routine case: just a peer connecting or disconnecting). A no-op,
// cheap enough to run on every poll, whenever AutoBGP is off, or the
// gossiped ASN is unchanged (mesh.Engine.SetBGPASN's own guard) and the
// mesh has settled into a neighbor set that already matches.
func (r *autoBGPReconciler) sync() {
	if r.s.configPath == "" {
		return
	}
	cfg, err := config.Load(r.s.configPath)
	if err != nil {
		return
	}

	if !cfg.BGP.AutoBGP {
		effectiveASN := uint32(0)
		if cfg.BGP.Enabled {
			effectiveASN = cfg.BGP.ASN
		}
		r.s.be.SetBGPASN(effectiveASN)
		return
	}

	selfV4 := autoBGPSelfTunnel4(r.s.be)
	asn := cfg.BGP.ASN
	if asn == 0 {
		if !selfV4.IsValid() {
			r.s.be.SetBGPASN(0) // nothing to derive a local AS from yet (DAD still pending)
			return
		}
		asn = deriveASNFromIPv4(selfV4)
	}
	routerID := cfg.BGP.RouterID
	if routerID == "" {
		if !selfV4.IsValid() {
			r.s.be.SetBGPASN(0)
			return
		}
		routerID = selfV4.String()
	}
	// Gossip now, using the value this pass has settled on — even on the
	// very first activation pass, where cfg.BGP.ASN was still 0 a moment
	// ago and asn is a freshly-derived value that hasn't been persisted yet.
	r.s.be.SetBGPASN(asn)

	identityChanged := !cfg.BGP.Enabled || cfg.BGP.ASN != asn || cfg.BGP.RouterID != routerID

	desired := desiredAutoBGPNeighbors(gatherAutoBGPPeers(r.s.be))
	nextNeighbors, added, removed, changed := mergeAutoBGPNeighbors(cfg.BGP.Neighbors, desired)
	if !changed && !identityChanged {
		return // already exactly what AutoBGP wants — nothing to write or reload
	}

	var prev config.BGPConfig
	var meshRoutes []string
	var next config.BGPConfig
	if err := r.s.mutateConfig(func(c *config.Config) error {
		prev = c.BGP
		meshRoutes = meshRouteCIDRs(c)
		c.BGP.Enabled = true
		c.BGP.ASN = asn
		c.BGP.RouterID = routerID
		c.BGP.Neighbors = nextNeighbors
		next = c.BGP
		return nil
	}); err != nil {
		r.s.log.Warnf("autobgp: %v", err)
		return
	}

	if identityChanged {
		if _, err := applyBGP(next, prev, meshRoutes, meshRoutes, r.s.log); err != nil {
			r.s.log.Warnf("autobgp: applying to FRR: %v", err)
		}
		return
	}
	applyBGPIncremental(next, added, removed, meshRoutes, r.s.log)
}
