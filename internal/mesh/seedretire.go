package mesh

import (
	"net/netip"
)

// SeedNodeOwners reports, for one network, which node is currently known to
// sit behind each seed address — keyed both by "ip:port" and by bare "ip", so
// a caller holding an operator-written seed string can match it without
// having to reproduce this package's port-expansion rules.
//
// The bare-ip key is what makes this usable at all in practice. A seed
// configured as "203.0.113.5:65432,443" expands into several dial candidates
// and the handshake may complete on any of them; a seed reached over a
// different address family than the one configured (a v4 seed address whose
// node answers on v6, which is ordinary on a dual-stacked host) matches
// neither. Keying by address alone lets the caller ask the question it
// actually has — "who is at this host" — rather than the one the engine's
// internal maps happen to be shaped for.
//
// Sources, in increasing order of authority, later winning: a live session's
// current endpoint, then ns.seedOwner (any address a handshake has completed
// against), then configuredSeedOwnerUDP/TCP (the same proof, restricted to
// addresses the operator actually configured as seeds). Attribution beats a
// bare endpoint match because a peer that has roamed since its handshake is
// still the node that answered at the seed.
func (e *Engine) SeedNodeOwners(networkID uint64) map[string]string {
	ns := e.network(networkID)
	if ns == nil {
		return nil
	}
	out := map[string]string{}
	put := func(ap netip.AddrPort, node string) {
		if node == "" || !ap.IsValid() {
			return
		}
		addr := ap.Addr().Unmap()
		out[netip.AddrPortFrom(addr, ap.Port()).String()] = node
		out[addr.String()] = node
	}
	ns.mu.RLock()
	for nid, ps := range ns.byNode {
		ps.mu.Lock()
		ep := ps.endpoint
		ps.mu.Unlock()
		put(ep, nid)
	}
	for ap, node := range ns.seedOwner {
		put(ap, node)
	}
	for ap, node := range ns.configuredSeedOwnerUDP {
		put(ap, node)
	}
	for ap, node := range ns.configuredSeedOwnerTCP {
		put(ap, node)
	}
	ns.mu.RUnlock()
	return out
}

// applyRetiredSeeds removes the endpoints of seeds the operator has just
// disabled from the live dial set, and tears down any session currently
// standing on one. Called from ReloadRuntime, so disabling a seed in the web
// admin or the CLI takes effect immediately rather than at the next restart.
//
// This is the only subtractive step in seed handling, and it is narrow on
// purpose. It removes exactly the addresses named in NetSpec.RetiredSeeds —
// never "everything not in the configured list", which would also sweep away
// gossip-learned candidates and PeerCache addresses that no operator ever
// disabled and that the additive design exists to protect.
//
// Re-enabling needs no counterpart here. Retiring an address deletes its
// backoff and first-seen bookkeeping along with the entry, so when the next
// reload puts it back through AddExplicitSeed it is genuinely new again and
// initLoop dials it on its next tick, about a second later, rather than
// waiting out whatever retry backoff it had accumulated before.
//
// # What a teardown here does and does not mean
//
// A seed is a bootstrap address, not a node identity, so this disconnects the
// session reached over that address and nothing more. On a full mesh the peer
// behind it is an ordinary member and any other peer will gossip it back
// within gossipInterval, at which point learnPeers redials it — the session
// returns in about ten seconds and the address is relearned as an ordinary
// candidate, no longer as this node's configured seed.
//
// That is correct behavior, not a leak: a seed's off switch governs whether
// this node uses that address to bootstrap, and turning it off is not a claim
// that the node behind it should be unreachable. The control that does mean
// that is Peers → disable (applyDisabledPeers), which is keyed on node id and
// which the handshake, relay, and gossip paths all consult. The distinction is
// worth keeping precisely because one seed address can front several nodes
// behind a NAT, and one node can be reachable at several addresses.
//
// Where it does durably disconnect: a partial mesh, where peer-to-peer links
// are refused outright and the seed link is the only one there is, and a node
// whose peer is not reachable any other way.
func (e *Engine) applyRetiredSeeds(ns *netState, retired, retiredTCP []netip.AddrPort) {
	if len(retired) == 0 && len(retiredTCP) == 0 {
		return
	}
	gone := make(map[netip.AddrPort]bool, len(retired)+len(retiredTCP))
	for _, s := range retired {
		gone[s] = true
	}
	for _, s := range retiredTCP {
		gone[s] = true
	}

	// victims is collected under the same lock that removes the addresses, so
	// a session cannot be established over an address between it being
	// dropped from the dial set and being looked for here.
	victims := map[string]netip.AddrPort{}

	ns.mu.Lock()
	kept := ns.seeds[:0]
	for _, s := range ns.seeds {
		if gone[s] {
			continue
		}
		kept = append(kept, s)
	}
	ns.seeds = kept

	keptTCP := ns.tcpSeeds[:0]
	for _, s := range ns.tcpSeeds {
		if gone[s] {
			continue
		}
		keptTCP = append(keptTCP, s)
	}
	ns.tcpSeeds = keptTCP

	for s := range gone {
		// The owner attribution is the reliable mapping from a seed address
		// to the node behind it: a peer that roamed to a new endpoint after
		// the handshake still has its original seed recorded here, and would
		// not be found by matching endpoints alone.
		if owner, known := ns.seedOwner[s]; known && owner != "" {
			victims[owner] = s
		}
		delete(ns.seedOwner, s)
		delete(ns.explicitSeed, s)
		delete(ns.seedFirstSeen, s)
		delete(ns.seedBackoff, s)
		delete(ns.seedTCP, s)
		delete(ns.everConnected, s)
	}
	// The complement of the roaming case above: a session whose endpoint is
	// still the retired address but which was never attributed to it (a seed
	// dialed before any gossip arrived to name its owner).
	for nid, ps := range ns.byNode {
		if _, already := victims[nid]; already {
			continue
		}
		ps.mu.Lock()
		ep := ps.endpoint
		ps.mu.Unlock()
		if gone[ep] {
			victims[nid] = ep
		}
	}
	ns.mu.Unlock()

	for _, s := range retired {
		e.log.Infof("mesh: seed %s disabled on net %016x — removed from the dial set", s, ns.spec.ID)
	}
	for _, s := range retiredTCP {
		e.log.Infof("mesh: tcp seed %s disabled on net %016x — removed from the dial set", s, ns.spec.ID)
	}
	for nid, ep := range victims {
		e.log.Infof("mesh: disconnecting peer %q on net %016x — reached over disabled seed %s", nid, ns.spec.ID, ep)
		e.localDisconnect(ns, nid)
	}
}
