package mesh

import (
	"fmt"

	"gravinet/internal/crypto"
	"gravinet/internal/protocol"
)

// buildEgress constructs the up-throttle shaper (with QoS) for a spec, or nil
// when no up-throttle is configured.
func (e *Engine) buildEgress(spec NetSpec) *shaper {
	if spec.ThrottleUp <= 0 {
		return nil
	}
	return newShaper(spec.ThrottleUp, spec.ThrottleBurst, spec.ThrottleQueue, spec.QoS,
		func(ps *peerSession, pkt []byte) { e.sendData(ps, pkt) })
}

// buildIngress constructs the down-throttle policer, or nil when unset.
func buildIngress(spec NetSpec) *tokenBucket {
	if spec.ThrottleDown <= 0 {
		return nil
	}
	burst := spec.ThrottleBurst
	// A policer with a one-packet burst shreds TCP: every normal micro-burst
	// overflows it, the sender sees loss and collapses, and the realized rate ends
	// up far below the cap. Give it ~250ms of the down-rate so ordinary bursts pass
	// while the sustained average still settles at the configured rate.
	if m := spec.ThrottleDown / 4; burst < m {
		burst = m
	}
	if burst < protocol.MaxUDPPayload {
		burst = protocol.MaxUDPPayload
	}
	return newTokenBucket(spec.ThrottleDown, burst)
}

// buildNAT constructs the NAT table for a spec, or nil when NAT is off or has no
// valid rules.
func (e *Engine) buildNAT(spec NetSpec) *natTable {
	if !spec.NATEnabled {
		return nil
	}
	var natRules []natRule
	for _, ns2 := range spec.NAT {
		if r, ok := ns2.toRule(); ok {
			natRules = append(natRules, r)
		} else {
			e.log.Warnf("mesh: skipping invalid NAT rule on net %x", spec.ID)
		}
	}
	if len(natRules) == 0 {
		return nil
	}
	return newNATTable(natRules, spec.NATStateTimeout)
}

// ReloadRuntime re-applies the hot-reloadable parts of a network's config from a
// fresh spec, without recreating the interface or sessions. It swaps NAT, the
// up-throttle shaper (and its QoS classifier), and the down-throttle policer
// atomically — including turning any of them on or off live — and toggles the
// firewall on or off and replaces its rulebase live (the firewall object always
// exists, so enabling, disabling, and per-rule changes all apply without a
// restart).
//
// Structural changes (adding/removing a network, addressing, keys, or ports) are
// NOT applied here and still need a restart. A reload may drop a handful of
// in-flight shaped packets as the old shaper is retired; TCP recovers and it is
// imperceptible in practice.
func (e *Engine) ReloadRuntime(networkID uint64, spec NetSpec) error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()

	e.mu.RLock()
	ns := e.network(networkID)
	e.mu.RUnlock()
	if ns == nil {
		return fmt.Errorf("no such network %016x", networkID)
	}

	// Firewall: toggle filtering and refresh rules live (no restart needed). The
	// pointer is stable; enable/disable flips a flag, rules swap atomically.
	if ns.fw != nil {
		ns.fw.setEnabled(spec.FirewallEnabled)
		ns.fw.setExempts(spec.FirewallExempts)
		// Catalog first so rules that reference edited objects/services recompile
		// against the new definitions; setCatalog recompiles the live rulebase,
		// then ReloadFirewallRules swaps in the (possibly reordered/edited) rules.
		ns.fw.setCatalog(spec.FirewallObjects, spec.FirewallServices)
		if err := e.ReloadFirewallRules(networkID, spec.FirewallRules); err != nil {
			e.log.Debugf("mesh: reload firewall net %016x: %v", networkID, err)
		}
		e.refreshFirewallFQDN(networkID)
	}

	// Locally-disabled peers: swap the blocklist and disconnect any peer that
	// just became disabled. Local-only, applied live (no restart).
	e.applyDisabledPeers(ns, spec.DisabledPeers)

	// Peer notes: local-only, operator-authored, purely informational — swap
	// the map live, no disconnect logic needed (unlike disabled peers, notes
	// never affect connectivity).
	e.applyPeerNotes(ns, spec.PeerNotes)

	// Seeds: merge newly-configured bootstrap endpoints so an added seed is
	// dialed without a restart. Additive only (AddSeed de-duplicates); a removed
	// seed stays in the live set until the next restart. AddExplicitSeed (not
	// plain AddSeed) because these came directly from the operator's config —
	// see explicitSeed's doc comment on netState for what that unlocks.
	for _, s := range spec.Seeds {
		e.AddExplicitSeed(networkID, s)
	}
	for _, s := range spec.TCPSeeds {
		e.addTCPSeed(networkID, s)
	}

	// Redistributed routes: swap the advertised/reject sets and flood the delta
	// (advertise new routes, withdraw removed ones) so changes apply live.
	e.reloadRoutes(ns, spec.Routes, spec.RouteReject, spec.RouteMetric)
	e.reloadHosts(ns, toHostRecords(spec.AdvHosts), spec.HostReject)
	e.reloadDNS(ns, toDNSForwards(spec.AdvDNS), spec.DNSReject)
	sd := append([]string(nil), spec.SearchDomains...)
	ns.searchDomains.Store(&sd)

	// NAT — atomic swap (nil disables; no background goroutine to retire).
	ns.nat.Store(e.buildNAT(spec))

	// Down-throttle policer — atomic swap (nil disables).
	ns.ingress.Store(buildIngress(spec))

	// Up-throttle shaper (+ QoS) — start the new one, swap it in, then retire the
	// old one so in-flight senders that already enqueued still drain.
	newEg := e.buildEgress(spec)
	if newEg != nil {
		go newEg.run()
	}
	if old := ns.egress.Swap(newEg); old != nil {
		old.close()
	}

	// Authentication keys — atomic swap so add/enable/disable/delete apply live.
	// A reload without a rebuilt set (Keys == nil) leaves the current one intact.
	if spec.Keys != nil {
		// A key learned via mesh propagation that config now carries is
		// config-owned from here on; drop its transient metadata so it isn't
		// re-surfaced for persistence (which would let a slot the operator later
		// clears be silently re-learned). Any propagated key config does NOT yet
		// carry — the narrow window before the persist hook has written it, or a
		// deployment with no persist hook at all — is re-folded so a reload
		// doesn't drop a key the mesh is actively using. Retirement can't be
		// undone this way: a retired key was removed from propagatedKeys on the
		// earlier reload that saw config still carrying it.
		ns.mu.Lock()
		labels := make(map[crypto.KeyID]string, len(spec.KeyLabels))
		for id, l := range spec.KeyLabels {
			labels[id] = l
		}
		expires := make(map[crypto.KeyID]string, len(spec.KeyExpires))
		for id, x := range spec.KeyExpires {
			expires[id] = x
		}
		ns.forgetConfiguredKeys(spec.Keys, labels, expires) // drop entries config has fully caught up with
		ns.forgetAppliedRetractions(spec.Keys)              // drop retractions config has actually applied
		merged := spec.Keys
		for id, pk := range ns.propagatedKeys {
			merged = merged.With(pk.raw)
			if _, ok := labels[id]; !ok {
				labels[id] = pk.label // not yet config-owned; label from the gossip
			}
		}
		ns.keys.Store(merged)
		ns.keyLabels.Store(&labels)
		ns.mu.Unlock()
		// Tear down sessions whose key was just retired so each peer re-handshakes
		// with a still-valid key instead of riding a disabled/expired one.
		e.dropRetiredKeySessions(ns)
	}

	e.log.Infof("mesh: runtime config reloaded on net %016x (nat=%v up=%d down=%d qos=%v)",
		networkID, spec.NATEnabled, spec.ThrottleUp, spec.ThrottleDown, spec.QoS != nil)
	return nil
}

// keyLabelFor returns the display label for a key ID (e.g. the one that
// authenticated a peer's session), or "" if it's not in the current label map
// (an unlabeled or since-forgotten key). Safe to call without ns.mu: backed by
// an atomic pointer, like ns.keys itself.
func (ns *netState) keyLabelFor(id crypto.KeyID) string {
	kl := ns.keyLabels.Load()
	if kl == nil {
		return ""
	}
	return (*kl)[id]
}

// dropRetiredKeySessions tears down sessions that authenticated with a key no
// longer in the network's enabled set (disabled, deleted, or expired), freeing
// the endpoint so the node re-handshakes with a remaining key. Mirrors pruneDead.
func (e *Engine) dropRetiredKeySessions(ns *netState) {
	ks := ns.keys.Load()
	valid := make(map[crypto.KeyID]bool, ks.Len())
	for _, id := range ks.Order() {
		valid[id] = true
	}
	var dead []*peerSession
	e.mu.Lock()
	for idx, ps := range e.sessions {
		if ps.net != ns {
			continue
		}
		if !valid[ps.keyID] {
			delete(e.sessions, idx)
			dead = append(dead, ps)
		}
	}
	e.mu.Unlock()
	if len(dead) == 0 {
		return
	}
	ns.mu.Lock()
	for _, ps := range dead {
		if ns.byNode[ps.nodeID] == ps {
			delete(ns.byNode, ps.nodeID)
		}
		if ps.overlay4.IsValid() && ns.routes4[ps.overlay4] == ps {
			delete(ns.routes4, ps.overlay4)
		}
		if ps.overlay6.IsValid() && ns.routes6[ps.overlay6] == ps {
			delete(ns.routes6, ps.overlay6)
		}
	}
	ns.mu.Unlock()
	for _, ps := range dead {
		e.log.Infof("mesh: dropped session to %q on net %x (its key was retired); re-handshaking with a current key", ps.nodeID, ns.spec.ID)
	}
}
