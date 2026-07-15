package mesh

import (
	"net/netip"
	"time"

	"gravinet/internal/crypto"
)

// Relay lets two nodes that cannot reach each other directly communicate
// through a willing third node. The relay forwards opaque outer packets without
// the session keys, so A↔B traffic stays end-to-end encrypted; the relay only
// sees ciphertext.
//
// Envelope: [srcLen:1][src][dstLen:1][dst][opaque...]
//
// Candidate scoring (see bestRelay): among connected peers that have ever
// reported knowing the target, a relay reached directly always beats one
// that's itself already relayed (using it would stack a second hop onto
// every packet), and within the same tier the one with the lowest recently
// measured RTT wins. That RTT is this node's own round trip to the
// candidate (from the ctrlPing/ctrlPong keepalive) — not the candidate's
// path to the target, which isn't visible here: the relay forwards opaque
// ciphertext it never decrypts or attributes a round trip to, so there's no
// way to learn "candidate's RTT to target" without the candidate explicitly
// gossiping it, which nothing does today. This optimizes the half of the
// path that's measurable and keeps chains short, not true end-to-end
// latency to the target.

func encodeRelay(src, dst string, opaque []byte) []byte {
	out := make([]byte, 0, 2+len(src)+len(dst)+len(opaque))
	out = appendLenStr(out, src)
	out = appendLenStr(out, dst)
	return append(out, opaque...)
}

func decodeRelay(b []byte) (src, dst string, opaque []byte, ok bool) {
	r := reader{b: b}
	src, ok = r.lenStr()
	if !ok {
		return "", "", nil, false
	}
	dst, ok = r.lenStr()
	if !ok {
		return "", "", nil, false
	}
	return src, dst, r.b[r.off:], true
}

// onRelay handles a relay envelope that arrived on session ps. If we are the
// destination, the opaque packet is processed as if it came from src via ps. If
// we are an intermediary, we forward it to the destination's session (only when
// configured to relay).
func (e *Engine) onRelay(ps *peerSession, body []byte) {
	src, dst, opaque, ok := decodeRelay(body)
	if !ok {
		return
	}
	ns := ps.net
	if dst == e.nodeID {
		// Destination: process the opaque packet; replies route back via ps.
		e.dispatch(opaque, netip.AddrPort{}, ps)
		return
	}
	// Intermediary.
	if !ns.spec.AllowRelay {
		// Previously a bare, silent return: a node with allow_relay off would
		// drop every forwarded packet without a word, while the peer that
		// picked it as a relay retried indefinitely. Both ends stayed quiet,
		// so the only visible symptom was a peer that never came up. Say it
		// out loud on this end too — an operator reading either node's log can
		// now see it immediately. (An upgraded initiator won't even get here:
		// it learns our refusal from the handshake and skips us as a candidate
		// — see bestRelay/willRelay. This still fires for one that predates
		// that advertisement.)
		e.logRelayDeclined(ns, src, dst)
		return
	}
	if ns.isBanned(src) || ns.isBanned(dst) || ns.isPeerDisabled(src) || ns.isPeerDisabled(dst) {
		return
	}
	ns.mu.RLock()
	target := ns.byNode[dst]
	ns.mu.RUnlock()
	if target == nil || target == ps {
		return // no path, or would bounce back
	}
	e.sealAndSend(target, innerRelay, encodeRelay(src, dst, opaque))
}

// ---- reported-peer tracking (relay candidate discovery) ----

func (ps *peerSession) markReported(ids []string) {
	ps.reportedMu.Lock()
	if ps.reported == nil {
		ps.reported = make(map[string]bool, len(ids))
	}
	for _, id := range ids {
		ps.reported[id] = true
	}
	ps.reportedMu.Unlock()
}

func (ps *peerSession) reports(id string) bool {
	ps.reportedMu.Lock()
	defer ps.reportedMu.Unlock()
	return ps.reported[id]
}

// ---- relay discovery ----

const relayPendingTTL = 12 * time.Second

// tryRelays looks for nodes we know about but cannot reach directly, and
// starts a relayed handshake through the best-scoring connected peer that
// reports knowing them (see bestRelay).
func (e *Engine) tryRelays(ns *netState) {
	now := time.Now()

	// Snapshot what we need under the lock.
	ns.mu.Lock()
	// prune stale relay pendings so we can retry / pick another relay
	for idx, p := range ns.pending {
		if p.relay != nil && now.Sub(p.started) > relayPendingTTL {
			delete(ns.pending, idx)
		}
	}
	type want struct {
		nodeID   string
		endpoint netip.AddrPort
	}
	var wants []want
	for nid, ni := range ns.nodes {
		if nid == e.nodeID {
			continue
		}
		if _, connected := ns.byNode[nid]; connected {
			continue
		}
		wants = append(wants, want{nid, ni.endpoint})
	}
	// relay pending set
	relayPending := make(map[string]bool)
	for _, p := range ns.pending {
		if p.targetNode != "" {
			relayPending[p.targetNode] = true
		}
	}
	// candidate relays: connected peers
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		peers = append(peers, ps)
	}
	backoff := ns.seedBackoff
	hasBackoff := func(ep netip.AddrPort) bool {
		if !ep.IsValid() {
			return true // no direct endpoint at all
		}
		_, ok := backoff[ep]
		return ok
	}
	ns.mu.Unlock()

	for _, w := range wants {
		if relayPending[w.nodeID] || e.connectedToNode(ns, w.nodeID) {
			continue
		}
		if ns.isBanned(w.nodeID) || ns.isPeerDisabled(w.nodeID) {
			continue
		}
		// Only relay once direct has demonstrably failed (in backoff or no endpoint).
		if !hasBackoff(w.endpoint) {
			continue
		}
		// Best connected peer that reports knowing the target — see bestRelay.
		relay, refused := bestRelay(peers, w.nodeID)
		if relay == nil {
			if refused > 0 {
				// Every peer that knows this target has allow_relay turned
				// off. Before this log existed the whole situation was
				// completely silent: bestRelay returned nil, tryRelays moved
				// on, and the target simply stayed unreachable forever with
				// nothing anywhere saying why — indistinguishable, from the
				// outside, from "no peer knows it" or "the relayed handshake
				// is failing." That silence is at its worst in exactly the
				// case that matters most: a node whose remaining reachable
				// peers are all relay-refusers (e.g. after moving onto a
				// network where only the public seeds answer) loses every
				// peer behind them at once, with no diagnosis available.
				e.logRelayRefused(ns, w.nodeID, refused)
			}
			continue
		}
		e.startRelayHandshake(ns, w.nodeID, relay)
	}
}

// relayRefusedLogEvery throttles logRelayRefused: tryRelays runs every
// maintInterval (5s) and would otherwise re-log an unchanged, persistent
// misconfiguration for every unreachable target on every tick.
const relayRefusedLogEvery = 5 * time.Minute

// logRelayRefused reports, at most once per relayRefusedLogEvery per target,
// that target cannot be reached directly and every connected peer that knows
// it has declined to relay.
func (e *Engine) logRelayRefused(ns *netState, target string, refused int) {
	now := time.Now()
	ns.mu.Lock()
	if last, ok := ns.relayRefusedLog[target]; ok && now.Sub(last) < relayRefusedLogEvery {
		ns.mu.Unlock()
		return
	}
	ns.relayRefusedLog[target] = now
	ns.mu.Unlock()
	e.log.Warnf("mesh: cannot reach %q on net %x — no direct path, and all %d connected peer(s) that know it have allow_relay disabled, so none will forward for us; enable allow_relay on at least one of them (or restore a direct path) or %q stays unreachable", target, ns.spec.ID, refused, target)
}

// bestRelay picks the best of peers to use as a relay for target, among those
// who have ever reported (via gossip) knowing it *and* have not explicitly
// told us they will not relay (hsPayload.AllowRelay — see willRelay). Returns
// nil if none qualify, plus the number of otherwise-suitable candidates that
// were skipped purely because they refuse to relay, which is what lets
// tryRelays tell "nobody knows this target" apart from "every peer that knows
// it has allow_relay turned off" — two situations that look identical from
// here and demand completely different fixes. See the package doc above for
// what "best" does and doesn't account for.
func bestRelay(peers []*peerSession, target string) (best *peerSession, refused int) {
	for _, ps := range peers {
		if ps.nodeID == target || !ps.reports(target) {
			continue
		}
		if !ps.willRelay() {
			refused++
			continue
		}
		if best == nil || relayBetter(ps, best) {
			best = ps
		}
	}
	return best, refused
}

// willRelay reports whether ps is usable as a relay. A peer that predates the
// advertisement (relayKnown false) is assumed willing: that's exactly how this
// worked before the flag existed, and assuming otherwise would break relaying
// through every not-yet-upgraded node for the whole duration of a rolling
// upgrade. Only an explicit "no" is honored.
func (ps *peerSession) willRelay() bool {
	return !ps.relayKnown || ps.allowRelay
}

// relayBetter reports whether a is a better relay candidate than b: reached
// directly beats reached via a relay regardless of RTT (stacking hops is
// worse than a slightly slower single hop), and within the same tier, a
// lower measured RTT wins. An unmeasured RTT (0 — no ctrlPong round trip
// completed yet, e.g. a peer connected within the last keepaliveInterval)
// never beats anything, including another unmeasured candidate, so ties
// among fresh, same-tier candidates simply keep whichever bestRelay saw
// first rather than flapping between them.
func relayBetter(a, b *peerSession) bool {
	aDirect, bDirect := a.getRelay() == nil, b.getRelay() == nil
	if aDirect != bDirect {
		return aDirect
	}
	aRTT, bRTT := a.rttNanos.Load(), b.rttNanos.Load()
	if aRTT == 0 {
		return false
	}
	if bRTT == 0 {
		return true
	}
	return aRTT < bRTT
}

// logRelayDeclined reports, at most once per relayRefusedLogEvery per
// src→dst pair, that this node dropped a relay request because its own
// allow_relay is disabled.
func (e *Engine) logRelayDeclined(ns *netState, src, dst string) {
	key := src + "\x00" + dst
	now := time.Now()
	ns.mu.Lock()
	if last, ok := ns.relayDeclinedLog[key]; ok && now.Sub(last) < relayRefusedLogEvery {
		ns.mu.Unlock()
		return
	}
	ns.relayDeclinedLog[key] = now
	ns.mu.Unlock()
	e.log.Warnf("mesh: declining to relay %q → %q on net %x: allow_relay is disabled on this node, so %q cannot reach %q through us", src, dst, ns.spec.ID, src, dst)
}

func (e *Engine) startRelayHandshake(ns *netState, target string, relay *peerSession) {
	ns.mu.Lock()
	for _, p := range ns.pending {
		if p.targetNode == target {
			ns.mu.Unlock()
			return // already trying
		}
	}
	eph, err := crypto.NewEphemeral()
	if err != nil {
		ns.mu.Unlock()
		return
	}
	idxI := e.allocIndex()
	p := &pendingHS{
		idxI:       idxI,
		eph:        eph,
		keyCursor:  0,
		started:    time.Now(),
		relay:      relay,
		targetNode: target,
	}
	ns.pending[idxI] = p
	pkt := e.buildHSInit(ns, p)
	ns.mu.Unlock()

	if pkt == nil {
		return
	}
	e.log.Infof("mesh: attempting relayed handshake to %q via %q on net %x", target, relay.nodeID, ns.spec.ID)
	e.sealAndSend(relay, innerRelay, encodeRelay(e.nodeID, target, pkt))
}
