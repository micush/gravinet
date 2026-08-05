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
		e.dispatch(opaque, netip.AddrPort{}, ps, ProtoUDP)
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

// reportedRTTObs is one gossiped RTT and when it arrived — see
// peerSession.reportedRTT.
type reportedRTTObs struct {
	rtt time.Duration
	at  time.Time
}

// reportedRTTTTL bounds how long a gossiped RTT is trusted. It must stay
// comfortably longer than gossipFullRefresh (180s), which is the worst-case
// interval between refreshes on a mesh where nothing else about the peer list
// changes — RTT is deliberately not part of peerListSig, so it does not
// trigger a re-flood of its own. Set below that and a quiet mesh would spend
// most of its time treating every relay's far leg as unknown, which loses the
// whole point of gossiping it.
const reportedRTTTTL = 10 * time.Minute

// noteReportedRTT records the RTTs carried by a received peer list. A zero
// rttMillis means the advertiser has no measurement (not "zero latency"), so
// it is skipped rather than stored — storing it would read back as an
// unbeatably fast far leg.
func (ps *peerSession) noteReportedRTT(entries []peerEntry) {
	now := time.Now()
	ps.reportedMu.Lock()
	for _, en := range entries {
		if en.nodeID == "" || en.rttMillis == 0 {
			continue
		}
		if ps.reportedRTT == nil {
			ps.reportedRTT = make(map[string]reportedRTTObs, len(entries))
		}
		ps.reportedRTT[en.nodeID] = reportedRTTObs{
			rtt: time.Duration(en.rttMillis) * time.Millisecond,
			at:  now,
		}
	}
	ps.reportedMu.Unlock()
}

// reportedRTTFor returns this peer's advertised round trip to id, and whether
// a fresh figure exists at all. A stale observation is reported as unknown and
// dropped, so the map cannot grow without bound on a peer that keeps gossiping
// a churning set of node ids.
func (ps *peerSession) reportedRTTFor(id string) (time.Duration, bool) {
	ps.reportedMu.Lock()
	defer ps.reportedMu.Unlock()
	obs, ok := ps.reportedRTT[id]
	if !ok {
		return 0, false
	}
	if time.Since(obs.at) > reportedRTTTTL {
		delete(ps.reportedRTT, id)
		return 0, false
	}
	return obs.rtt, true
}

// ---- relay discovery ----

const relayPendingTTL = 12 * time.Second

// Relay handshake attempts back off per target. relayPendingTTL alone is not a
// throttle: it only bounds how long an *unanswered* attempt occupies the
// pending slot, so it governs the silent-target case and nothing else. Any
// target that answers and is then rejected — partial-mesh policy, a ban, a key
// mismatch, a hairpin claiming our own node id — has its pending deleted on
// receipt, freeing the slot immediately and letting the next maintenance tick
// attempt again, forever, at maintInterval. The partial-mesh gate in tryRelays
// removes the one cause seen in the field; this bounds the whole class,
// including causes not yet found.
//
// Deliberately not reset on success: install() removes the target from the
// wants list entirely, and a later teardown re-entering the loop should not get
// a fresh unthrottled burst. relayAttemptReset drops the entry only when the
// target has been quiet long enough that a genuinely new situation is likely.
var (
	relayAttemptBase = 10 * time.Second
	relayAttemptMax  = 10 * time.Minute
	// relayAttemptReset is how long without an attempt before a target's
	// counter is forgotten. Longer than relayAttemptMax so a target sitting at
	// the cap does not silently reset itself between attempts.
	relayAttemptReset = 30 * time.Minute
)

// relayAttemptBackoff returns the delay before the next attempt for a target
// that has already had n attempts: the base doubled once per attempt *after the
// first*, capped. So n=1 is one base interval, n=2 is two, n=3 is four.
func relayAttemptBackoff(n int) time.Duration {
	d := relayAttemptBase
	for i := 1; i < n && d < relayAttemptMax; i++ {
		d *= 2
	}
	if d > relayAttemptMax {
		return relayAttemptMax
	}
	return d
}

// relayAttemptAllowed reports whether a relayed handshake to target may be
// attempted now, and records the attempt if so. Caller must hold ns.mu.
func (ns *netState) relayAttemptAllowed(target string, now time.Time) bool {
	if ns.relayAttempts == nil {
		ns.relayAttempts = make(map[string]*relayAttempt)
	}
	a := ns.relayAttempts[target]
	if a == nil {
		ns.relayAttempts[target] = &relayAttempt{n: 1, last: now}
		return true
	}
	if now.Sub(a.last) > relayAttemptReset {
		a.n, a.last = 1, now
		return true
	}
	if now.Sub(a.last) < relayAttemptBackoff(a.n) {
		return false
	}
	a.n++
	a.last = now
	return true
}

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
		// Partial mesh permits only seed-to-seed and seed-to-peer links, and
		// onHSInit/onHSResp refuse a peer-to-peer one outright — relayed or
		// not, since a relayed session is still a session between those two
		// nodes. Attempting one here is therefore not a retry that might
		// eventually succeed; it is a request guaranteed to be refused, and
		// refused *after* a round trip, which is what made it a storm rather
		// than a no-op: the response deletes the pending handshake, so
		// relayPendingTTL never gets to throttle anything and the next
		// maintenance tick starts over. One node's log carried 7825 partial-mesh
		// rejections and a relayed handshake to the same unreachable target
		// every 5 seconds without pause.
		//
		// learnPeers already applies this exact gate to gossip-driven direct
		// dials (see its !ns.spec.PartialMesh || en.selfSeed condition). This
		// is the relay path's missing counterpart.
		if ns.spec.PartialMesh && !ns.spec.SelfSeed && !ni.selfSeed {
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
	// Seeds that initSeedTick will actually dial. Needed because a missing
	// backoff entry is ambiguous on its own: it means either "direct hasn't
	// been tried yet" or "direct was tried, succeeded, and install() cleared
	// the entry" — and after a later teardown the second case leaves a node
	// with a valid endpoint, no backoff, and nothing that will ever dial it
	// again (install() also prunes the seed on connect). Treating that as
	// "still waiting on direct" wedges the node permanently: no dial means no
	// backoff entry, and no backoff entry means no relay. Peers behind
	// symmetric NAT, whose advertised endpoint can never work, never escape
	// it. See TestTryRelaysWhenNoBackoffEntryExists.
	dialable := make(map[netip.AddrPort]bool, len(ns.seeds))
	for _, s := range ns.seeds {
		dialable[s] = true
	}
	// shouldRelay reports whether to go via a relay now rather than keep
	// waiting for a direct path: no endpoint at all, a direct attempt that has
	// demonstrably failed (in backoff), or no seed that would produce an
	// attempt in the first place.
	shouldRelay := func(ep netip.AddrPort) bool {
		if !ep.IsValid() {
			return true // no direct endpoint at all
		}
		if _, cooling := backoff[ep]; cooling {
			return true // direct has demonstrably failed
		}
		return !dialable[ep] // nothing will dial it; waiting is indefinite
	}
	ns.mu.Unlock()

	for _, w := range wants {
		if relayPending[w.nodeID] || e.connectedToNode(ns, w.nodeID) {
			continue
		}
		if ns.isBanned(w.nodeID) || ns.isPeerDisabled(w.nodeID) {
			continue
		}
		// Only relay once direct has demonstrably failed, or once it is clear
		// nothing is going to attempt it.
		if !shouldRelay(w.endpoint) {
			continue
		}
		// Best connected peer that reports knowing the target — see bestRelay.
		ns.mu.Lock()
		allowed := ns.relayAttemptAllowed(w.nodeID, now)
		ns.mu.Unlock()
		if !allowed {
			continue
		}
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
//
// A peer we reach *through a relay* is never itself a candidate. Until v798
// this was a preference (relayBetter's tier rule) rather than a rule, so a
// chain was still assembled whenever nothing direct qualified — and chains are
// what took the mesh down in v797: three-plus hops whose real cost nothing
// could see, and no way to rule out that a link in the chain was routing back
// through us. Together with rttMillisOf advertising direct measurements only,
// the rule here makes every relayed path exactly two direct hops.
//
// The cost is real and worth naming: a target reachable *only* through a chain
// is now unreachable rather than reachable badly. That is the deliberate
// trade. An unreachable peer is a visible, local, stable failure; a chain is an
// invisible one that degrades every other path sharing its links.
func bestRelay(peers []*peerSession, target string) (best *peerSession, refused int) {
	for _, ps := range peers {
		if ps.nodeID == target || !ps.reports(target) {
			continue
		}
		if ps.getRelay() != nil {
			continue // reached via a relay: would make this a chain
		}
		if !ps.willRelay() {
			refused++
			continue
		}
		if best == nil || relayBetter(ps, best, target) {
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

// relayCost estimates what reaching target through cand would cost: this
// node's measured round trip to cand, plus cand's own advertised round trip to
// target (see peerEntry.rttMillis). full reports whether both legs are known.
//
// A partial cost — near leg measured, far leg unadvertised — is still the best
// available ordering signal among candidates that are equally in the dark, and
// is exactly what this scored on before the far leg was gossiped at all, so it
// is returned rather than discarded. But it is never comparable to a full one:
// a 20ms near leg with an unknown far leg is not "better" than a 60ms total
// that is actually known end to end. Callers that act on cost (rescoreRelays)
// require full; callers that merely order candidates (relayBetter) prefer full
// over partial and otherwise compare like with like.
//
// known is false when even the near leg is unmeasured, which orders below
// everything — see relayBetter.
func relayCost(cand *peerSession, target string) (cost time.Duration, full, known bool) {
	near := time.Duration(cand.rttNanos.Load())
	if near <= 0 {
		return 0, false, false
	}
	if far, ok := cand.reportedRTTFor(target); ok {
		return near + far, true, true
	}
	return near, false, true
}

// relayBetter reports whether a is a better relay candidate than b: reached
// directly beats reached via a relay regardless of RTT (stacking hops is
// worse than a slightly slower single hop), and within the same tier, the
// lower estimated end-to-end cost wins (see relayCost).
//
// Within a tier the ordering is: a candidate whose full path is known beats
// one whose far leg isn't, and within the same knowledge class the lower cost
// wins. An unmeasured near leg never beats anything, including another
// unmeasured candidate, so ties among fresh, same-tier candidates simply keep
// whichever bestRelay saw first rather than flapping between them. Note that
// bestRelay iterates a map, so "saw first" is arbitrary — on a mesh where no
// keepalive round trip has completed yet, the pick is effectively random. That
// is survivable only because rescoreRelays revisits it; before that existed,
// an arbitrary startup pick was permanent.
func relayBetter(a, b *peerSession, target string) bool {
	aDirect, bDirect := a.getRelay() == nil, b.getRelay() == nil
	if aDirect != bDirect {
		return aDirect
	}
	aCost, aFull, aKnown := relayCost(a, target)
	bCost, bFull, bKnown := relayCost(b, target)
	if !aKnown {
		return false
	}
	if !bKnown {
		return true
	}
	if aFull != bFull {
		return aFull
	}
	return aCost < bCost
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

// ---- relay re-scoring ----

// tryRelays only ever considers nodes it is *not* connected to, and a relayed
// session sits in ns.byNode exactly like a direct one. So the relay chosen when
// a path was first established was, until this existed, never revisited: it
// survived every better candidate connecting afterwards, every RTT measurement
// that arrived after the pick, and every far-leg RTT gossiped since. Combined
// with relayBetter's arbitrary ordering among not-yet-measured candidates (see
// its doc comment), a node whose direct path can never come up — two peers
// behind one NAT, say — could sit on a randomly-chosen relay indefinitely, and
// on a mesh with one distant peer that is the difference between 50ms and 350ms
// forever.
//
// These bounds keep that from becoming a different failure. Latency is noisy
// and a relay switch is not free — it costs a handshake and a brief
// interruption — so a challenger must be better by a real margin, not by
// jitter, and a path must have been in place long enough to have been measured
// properly before it can be moved off.
var (
	// relayRescoreDwell is how long a relayed session must have been
	// established before it may be moved. Deliberately several keepalive
	// intervals: rttNanos on a session installed seconds ago is one sample or
	// none, and moving a path on that basis is how the arbitrary-pick problem
	// gets reintroduced with extra steps.
	relayRescoreDwell = 90 * time.Second
	// relayRescoreInterval is the *base* throttle after a move. Each further
	// move of the same target doubles it (see relayRescoreBackoff), so a
	// target the cost model cannot settle on stops being churned instead of
	// oscillating forever. v797 had a flat interval, and paths moved on
	// precisely that cadence for as long as the log ran.
	relayRescoreInterval = 5 * time.Minute
	// relayRescoreBackoffMax caps that doubling. A target that has been moved
	// this many times is not going to be improved by moving it again; at that
	// point re-scoring has become the problem and stopping is the fix.
	relayRescoreBackoffMax = 2 * time.Hour
	// A challenger must beat the incumbent by both of these to displace it.
	// The ratio alone would chase noise on a fast path (25% of 4ms is
	// nothing); the absolute floor alone would chase noise on a slow one
	// (30ms of 400ms is well inside normal variance). Requiring both means a
	// switch only happens when the improvement is large in relative *and*
	// practical terms.
	//
	// Note what these can and cannot do. They bound how often a *wrong*
	// decision repeats; they cannot make a wrong decision right. v797's
	// margins were these exact values and the mesh still churned, because the
	// two costs being compared were not measured the same way. Margins are the
	// second line of defence, not the first.
	relayRescoreMargin  = 0.75 // challenger must be at most 75% of incumbent
	relayRescoreMinGain = 30 * time.Millisecond
)

// relayRescoreBackoff returns the throttle for a target that has already been
// moved n times: the base interval doubled per move, capped.
func relayRescoreBackoff(n int) time.Duration {
	d := relayRescoreInterval
	for i := 0; i < n && d < relayRescoreBackoffMax; i++ {
		d *= 2
	}
	if d > relayRescoreBackoffMax {
		return relayRescoreBackoffMax
	}
	return d
}

// relaySwitch is one decision to move a target's relayed path onto a different
// relay — see relaySwitches, which decides, and rescoreRelays, which acts.
type relaySwitch struct {
	target   string
	from     string        // the relay currently carrying this path
	to       *peerSession  // the challenger
	was      time.Duration // incumbent's cost, by the same estimator as `now`
	now      time.Duration // challenger's estimated end-to-end cost
	measured time.Duration // incumbent's real end-to-end RTT, for the log only
}

// relaySwitches decides which relayed paths should move, and is deliberately
// separate from performing the move: every bound that makes this safe to run on
// every maintenance tick lives here, and none of it needs a handshake, a key
// set or a socket to exercise.
//
// The comparison is estimate against estimate, both from relayCost. v797
// compared the challenger's estimate against the incumbent's *measured*
// end-to-end rttNanos, and that was the defect that made the hysteresis
// useless: an estimate is two RTT samples added together, while the measurement
// includes the relay's store-and-forward delay, its queueing, and the second
// decrypt/encrypt hop. The estimate therefore ran systematically low — by more
// than the 25%/30ms margin — so every candidate looked better than the
// incumbent, was switched to, promptly measured worse than it had estimated,
// and was itself displaced by the next candidate. Paths churned on exactly the
// relayRescoreInterval cadence. Two quantities that are not the same kind of
// thing cannot be compared and the margin cannot rescue them; measuring both
// the same way can.
//
// The incumbent's measured RTT is still used, but only as a gate that can veto
// a move, never as the thing being beaten: if the path already measures better
// than what the challenger merely estimates, there is nothing to win.
func (e *Engine) relaySwitches(ns *netState, now time.Time) []relaySwitch {
	ns.mu.RLock()
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		peers = append(peers, ps)
	}
	// A relay switch is pointless while a handshake for that target is already
	// in flight — including one an earlier tick started.
	inFlight := make(map[string]bool, len(ns.pending))
	for _, p := range ns.pending {
		if p.targetNode != "" {
			inFlight[p.targetNode] = true
		}
	}
	lastMoved := make(map[string]time.Time, len(ns.relayRescored))
	for target, at := range ns.relayRescored {
		lastMoved[target] = at
	}
	moveCount := make(map[string]int, len(ns.relayRescoreCount))
	for target, n := range ns.relayRescoreCount {
		moveCount[target] = n
	}
	ns.mu.RUnlock()

	var out []relaySwitch
	for _, ps := range peers {
		via := ps.getRelay()
		if via == nil {
			continue // direct: nothing to improve on, and never worth moving onto a relay
		}
		if now.Sub(ps.established) < relayRescoreDwell {
			continue
		}
		if last, ok := lastMoved[ps.nodeID]; ok && now.Sub(last) < relayRescoreBackoff(moveCount[ps.nodeID]) {
			continue
		}
		if inFlight[ps.nodeID] {
			continue
		}
		if ns.isBanned(ps.nodeID) || ns.isPeerDisabled(ps.nodeID) {
			continue
		}
		// The incumbent, scored by the same estimator as any challenger. If its
		// relay stopped advertising a far leg to this target, there is no
		// comparable figure and nothing to compare against — leave it be rather
		// than fall back to the measurement and reintroduce v797's mismatch.
		wasEst, wasFull, _ := relayCost(via, ps.nodeID)
		if !wasFull {
			continue
		}
		cand, _ := bestRelay(peers, ps.nodeID)
		if cand == nil || cand.nodeID == via.nodeID {
			continue
		}
		cost, full, _ := relayCost(cand, ps.nodeID)
		if !full {
			continue // far leg unknown: not a measurement, not grounds to move
		}
		if cost > time.Duration(float64(wasEst)*relayRescoreMargin) || wasEst-cost < relayRescoreMinGain {
			continue
		}
		// Veto: the path as it actually measures is already better than the
		// challenger's estimate. The estimate omits relay overhead, so a
		// challenger that cannot even beat the incumbent's real number on paper
		// will certainly not beat it in practice.
		if measured := time.Duration(ps.rttNanos.Load()); measured > 0 && measured <= cost {
			continue
		}
		out = append(out, relaySwitch{
			target: ps.nodeID, from: via.nodeID, to: cand,
			was: wasEst, now: cost, measured: time.Duration(ps.rttNanos.Load()),
		})
	}
	return out
}

// rescoreRelays applies relaySwitches' decisions.
//
// The switch reuses startRelayHandshake rather than tearing the old session
// down first: install() replaces the session for a node wholesale, so the
// existing path keeps carrying traffic until the new one completes, and a
// challenger that turns out to be unreachable costs a failed handshake rather
// than an outage. This is the same shape as the relayed→direct upgrade path.
func (e *Engine) rescoreRelays(ns *netState) {
	now := time.Now()
	for _, sw := range e.relaySwitches(ns, now) {
		ns.mu.Lock()
		if ns.relayRescored == nil {
			ns.relayRescored = make(map[string]time.Time)
		}
		if ns.relayRescoreCount == nil {
			ns.relayRescoreCount = make(map[string]int)
		}
		ns.relayRescored[sw.target] = now
		ns.relayRescoreCount[sw.target]++
		n := ns.relayRescoreCount[sw.target]
		ns.mu.Unlock()
		// Both figures, plus what the path actually measures: v797 logged only
		// the measurement and called it the incumbent's cost, which hid the
		// fact that it was being compared against something else entirely.
		e.log.Infof("mesh: moving relayed path to %q on net %x from %q (est %v, measures %v) to %q (est %v); move %d for this target, next not before %v; keeping the current path until the new one completes",
			sw.target, ns.spec.ID, sw.from, sw.was.Round(time.Millisecond), sw.measured.Round(time.Millisecond),
			sw.to.nodeID, sw.now.Round(time.Millisecond), n, relayRescoreBackoff(n))
		e.startRelayHandshake(ns, sw.target, sw.to)
	}
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

// repointRelayUsers moves every session that was relaying through old onto
// replacement (or onto nothing, when replacement is nil).
//
// peerSession.relay is a pointer to the relay's own session, and until this
// existed nothing ever updated it. A relay re-handshakes far more often than
// it disconnects — install() puts a fresh session in byNode and orphans the
// previous object — and every peer reached through that relay went on holding
// the orphan. deliver() then sealed with the dead session's keys and sent to
// its remoteIdx, an index the relay node had already discarded, so the packet
// was dropped there with no error, no counter and no log. The affected peer
// went dark outbound until its own peerTimeout reaped it and a fresh handshake
// installed a current pointer, which reads from outside as a peer that works,
// stops, and comes back a minute later.
//
// Direct sessions hold no such pointer, which is exactly why only relayed
// peers were affected.
//
// Locking: sessions are snapshotted under ns.mu and their own mutexes taken
// only after it is released, matching resyncAllBypassRoutes — no ns.mu→ps.mu
// ordering is introduced.
func (e *Engine) repointRelayUsers(ns *netState, old, replacement *peerSession) {
	if old == nil || old == replacement {
		return
	}
	ns.mu.RLock()
	sessions := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		sessions = append(sessions, ps)
	}
	ns.mu.RUnlock()

	moved := 0
	for _, ps := range sessions {
		if ps == old || ps == replacement {
			continue
		}
		ps.mu.Lock()
		if ps.relay == old {
			ps.relay = replacement
			moved++
		}
		ps.mu.Unlock()
	}
	if moved > 0 && replacement == nil {
		// The relay is gone rather than replaced: these peers have no path
		// until tryRelays picks another, which it will once their own
		// sessions are reaped. Leaving the dangling pointer instead would
		// keep them sending into a discarded session for just as long, but
		// silently and with no diagnosis available.
		e.log.Infof("mesh: relay %q on net %x went away; %d relayed peer(s) need a new path", old.nodeID, ns.spec.ID, moved)
	}
}

// relayAttempt tracks how hard we have been chasing one target with relayed
// handshakes — see relayAttemptAllowed.
type relayAttempt struct {
	n    int       // attempts made in the current run
	last time.Time // when the last attempt was made
}

// ---- partial-mesh dial suppression ----

// policyRefusedTTL bounds how long a recorded partial-mesh refusal suppresses
// dialing. The refusal reflects the far node's own SelfSeed config, which is a
// stable property, so this is deliberately long — but not permanent: an
// operator turning SelfSeed on over there should be picked up without needing a
// restart here. Gossip is the fast path for that (learnPeers clears the record
// as soon as the node is advertised as a seed); this is only the backstop for a
// node whose seed status is not being gossiped to us at all.
const policyRefusedTTL = 30 * time.Minute

// noteSeedPolicyRefused records that a handshake with node was refused because
// partial-mesh policy forbids the link, keyed by both the node and the endpoint
// it answered from.
//
// Both keys are needed. The endpoint is what initLoop iterates, so it is what
// can actually gate a dial — but the endpoint that answers is not always the one
// dialed, and a roamed peer arrives at a new address. The node id is stable and
// covers that; it also lets learnPeers clear the record when gossip says the
// node has become a seed.
func (e *Engine) noteSeedPolicyRefused(ns *netState, nodeID string, ep netip.AddrPort) {
	now := time.Now()
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if ns.policyRefusedNode == nil {
		ns.policyRefusedNode = make(map[string]time.Time)
	}
	if nodeID != "" {
		ns.policyRefusedNode[nodeID] = now
	}
	if ep.IsValid() {
		if ns.policyRefusedEP == nil {
			ns.policyRefusedEP = make(map[netip.AddrPort]time.Time)
		}
		ns.policyRefusedEP[ep] = now
		// Attribute the address so the proactive check below can gate it even
		// after the TTL lapses, without waiting for another refusal.
		if nodeID != "" && ns.seedOwner[ep] == "" {
			ns.seedOwner[ep] = nodeID
		}
	}
}

// clearSeedPolicyRefused forgets any refusal recorded against a node, called
// when gossip reports it as a seed — at which point the link is permitted and
// dialing should resume immediately rather than wait out policyRefusedTTL.
func (ns *netState) clearSeedPolicyRefused(nodeID string) {
	if nodeID == "" {
		return
	}
	if _, ok := ns.policyRefusedNode[nodeID]; !ok {
		return
	}
	delete(ns.policyRefusedNode, nodeID)
	for ep, owner := range ns.seedOwner {
		if owner == nodeID {
			delete(ns.policyRefusedEP, ep)
			delete(ns.seedBackoff, ep) // dial on the next tick, don't sit out a cooldown
		}
	}
}

// seedRefusedByPolicy reports whether dialing this seed is pointless because
// partial-mesh policy forbids the resulting link.
//
// Two independent grounds, because the information arrives at different times.
// The proactive one — this node is not a seed, the address belongs to a node we
// know about, and that node is not a seed either — catches endpoints armed by
// gossip or by a post-teardown redial, before any packet is sent. It cannot
// catch a PeerCache entry at cold start, whose owner is unknown until something
// answers; the recorded-refusal one covers exactly that case, costing a single
// handshake per node per TTL instead of one per second forever.
func (e *Engine) seedRefusedByPolicy(ns *netState, seed netip.AddrPort, now time.Time) bool {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if !ns.spec.PartialMesh || ns.spec.SelfSeed {
		return false // full mesh, or we are a seed: every link is permitted
	}
	if at, ok := ns.policyRefusedEP[seed]; ok && now.Sub(at) < policyRefusedTTL {
		return true
	}
	owner := ns.seedOwner[seed]
	if owner == "" {
		return false // unattributed: let it answer so we can learn who it is
	}
	if at, ok := ns.policyRefusedNode[owner]; ok && now.Sub(at) < policyRefusedTTL {
		return true
	}
	if ni, ok := ns.nodes[owner]; ok && !ni.selfSeed {
		return true // known peer, known not to be a seed
	}
	return false
}
