package mesh

import (
	"sort"
)

// DisabledPeerInfo is a locally-disabled peer as reported to the control API.
type DisabledPeerInfo struct {
	NodeID   string `json:"node_id"`
	Hostname string `json:"hostname,omitempty"`
	// Notes is an operator-authored, local-only note attached to this peer's
	// node id (see Config.PeerSetNotes) — never gossiped, purely for display.
	Notes string `json:"notes,omitempty"`
}

// isPeerDisabled reports whether nodeID is on this node's local blocklist. This
// is local-only state (never flooded), the disable counterpart to isBanned.
func (ns *netState) isPeerDisabled(nodeID string) bool {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.disabledPeers[nodeID]
}

// localDisconnect tears down any active session to target and removes it from
// routing, without flooding anything to the mesh and without forgetting the
// peer's learned endpoint — so re-enabling lets the maintenance loop redial it.
// This is the disable counterpart to applyBan (which is mesh-wide and also drops
// the peer from the seed list).
func (e *Engine) localDisconnect(ns *netState, target string) {
	if target == e.nodeID {
		return
	}
	var victim *peerSession
	ns.mu.Lock()
	if ps := ns.byNode[target]; ps != nil {
		victim = ps
		delete(ns.byNode, target)
		if ps.overlay4.IsValid() && ns.routes4[ps.overlay4] == ps {
			delete(ns.routes4, ps.overlay4)
		}
		if ps.overlay6.IsValid() && ns.routes6[ps.overlay6] == ps {
			delete(ns.routes6, ps.overlay6)
		}
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
		e.removePeerBypassRoute(ns, victim)
	}
	// Drop routes learned from a peer we just disabled; re-learned on re-enable.
	e.dropNodeRoutes(ns, target)
}

// applyDisabledPeers replaces the local blocklist with the given set and
// disconnects any currently-connected peer that just became disabled. Called
// from ReloadRuntime, so peer enable/disable applies live (no restart). The
// handshake/relay/control guards keep a disabled peer from reconnecting; a
// re-enabled peer is redialed automatically by the maintenance loop from its
// retained endpoint.
func (e *Engine) applyDisabledPeers(ns *netState, ids []string) {
	next := make(map[string]bool, len(ids))
	for _, id := range ids {
		next[id] = true
	}
	var nowBlocked []string
	ns.mu.Lock()
	ns.disabledPeers = next
	for nid := range ns.byNode {
		if next[nid] {
			nowBlocked = append(nowBlocked, nid)
		}
	}
	ns.mu.Unlock()
	for _, nid := range nowBlocked {
		e.log.Infof("mesh: locally disabled peer %q on net %x — disconnecting", nid, ns.spec.ID)
		e.localDisconnect(ns, nid)
	}
}

// applyPeerNotes replaces the local, operator-authored peer-notes map (node id
// -> note) with the given set. Purely informational — unlike
// applyDisabledPeers, this never disconnects anyone, so there's no need to
// diff against currently-connected peers first.
func (e *Engine) applyPeerNotes(ns *netState, notes map[string]string) {
	next := make(map[string]string, len(notes))
	for k, v := range notes {
		next[k] = v
	}
	ns.mu.Lock()
	ns.peerNotes = next
	ns.mu.Unlock()
}

// DisabledPeers lists the peers locally disabled on a network, with their
// last-known hostname where one was learned.
func (e *Engine) DisabledPeers(networkID uint64) []DisabledPeerInfo {
	ns := e.network(networkID)
	if ns == nil {
		return nil
	}
	ns.mu.RLock()
	out := make([]DisabledPeerInfo, 0, len(ns.disabledPeers))
	for id := range ns.disabledPeers {
		out = append(out, DisabledPeerInfo{NodeID: id, Hostname: ns.hostnameOf(id), Notes: ns.peerNotes[id]})
	}
	ns.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}
