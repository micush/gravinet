package mesh

import (
	"encoding/binary"
	"net"
	"net/netip"
	"time"

	"gravinet/internal/protocol"
)

// Path-MTU discovery, probe-based (PLPMTUD-style, RFC 8899 in spirit).
//
// Classic PMTUD relies on ICMP "fragmentation needed" replies, which mobile/5G
// and many middleboxes silently drop — the very failure that motivated overlay
// fragmentation. Instead we *probe*: send a padded packet of a candidate size to
// the peer and wait for it to echo an ack. Because a probe is an ordinary sealed
// datagram travelling the same path with the same treatment as real data, an
// acked probe proves a datagram of that size reaches the peer intact (the AEAD
// tag guarantees it arrived whole, not truncated/refragmented). No ICMP, no DF.
//
// Each peer's path is searched independently between a floor (the safe fallback,
// config underlay_mtu) and a ceiling (config underlay_mtu_max). The largest acked
// size becomes the per-peer underlay MTU we fragment to. If probes above the
// floor never ack — an MTU black hole — the peer simply stays at the floor.

const (
	pmtuProbeTimeout = 700 * time.Millisecond // per-probe ack wait
	pmtuMaxTries     = 2                      // resends of a candidate before it's declared failed
	pmtuRevalidate   = 2 * time.Minute        // once settled, recheck/grow this often
	pmtuProbeHdrLen  = 4 + 2                  // [probe_id:4][size:2] inside the sealed body
)

type pmtuPhase uint8

const (
	phaseSearch   pmtuPhase = iota // climbing toward the largest working size
	phaseSettled                   // converged; eff is the discovered PMTU
	phaseValidate                  // re-checking that eff still works
)

// pmtuState is a per-peer discovery state machine. step() is driven on a ticker
// and ack() on probe replies; both run under the peer's pmtuMu. eff is the
// discovered working outer MTU (mirrored to peerSession atomics for the hot path).
type pmtuState struct {
	floor, ceil int
	eff         int

	phase    pmtuPhase
	low      int // largest confirmed-working size in this search
	high     int // current upper bound to search toward
	awaiting bool
	cand     int
	probeID  uint32
	tries    int
	deadline time.Time
	revalAt  time.Time
}

func newPMTUState(floor, ceil int, now time.Time) *pmtuState {
	p := &pmtuState{floor: floor, ceil: ceil, eff: floor, low: floor, high: ceil}
	if ceil <= floor {
		p.phase = phaseSettled
		p.revalAt = now.Add(100 * 365 * 24 * time.Hour) // discovery off: never revalidate
	}
	return p
}

// step advances the machine at time now. When it returns send=true the caller
// must transmit a probe of `size` outer bytes carrying `id`.
func (p *pmtuState) step(now time.Time, nextID func() uint32) (size int, id uint32, send bool) {
	if p.ceil <= p.floor {
		return 0, 0, false // discovery disabled
	}
	if p.awaiting {
		if now.Before(p.deadline) {
			return 0, 0, false // still waiting on an ack
		}
		p.tries++
		if p.tries <= pmtuMaxTries {
			p.probeID = nextID()
			p.deadline = now.Add(pmtuProbeTimeout)
			return p.cand, p.probeID, true // resend same candidate
		}
		p.awaiting = false
		p.onCandFailed()
		// fall through and possibly start the next probe
	}
	switch p.phase {
	case phaseSettled:
		if now.Before(p.revalAt) {
			return 0, 0, false
		}
		p.phase = phaseValidate
		return p.startProbe(p.eff, now, nextID)
	case phaseValidate:
		return 0, 0, false // a validate probe is always in flight once entered
	default: // phaseSearch
		if p.low >= p.high {
			if p.low > p.eff {
				p.eff = p.low
			}
			p.phase = phaseSettled
			p.revalAt = now.Add(pmtuRevalidate)
			return 0, 0, false
		}
		cand := p.low + (p.high-p.low+1)/2 // midpoint, biased up
		return p.startProbe(cand, now, nextID)
	}
}

func (p *pmtuState) startProbe(size int, now time.Time, nextID func() uint32) (int, uint32, bool) {
	p.cand = size
	p.probeID = nextID()
	p.tries = 0
	p.awaiting = true
	p.deadline = now.Add(pmtuProbeTimeout)
	return size, p.probeID, true
}

// onCandFailed handles a candidate that exhausted its retries.
func (p *pmtuState) onCandFailed() {
	switch p.phase {
	case phaseValidate:
		// The size we were using stopped working — the path got worse. Fall back
		// to the floor and rediscover from scratch.
		p.eff = p.floor
		p.low = p.floor
		p.high = p.ceil
		p.phase = phaseSearch
	default: // phaseSearch
		p.high = p.cand - 1
		if p.high < p.low {
			p.high = p.low
		}
	}
}

// tooBig folds a definitive, synchronous "that packet exceeds the path MTU"
// verdict from the kernel (EMSGSIZE from sendto) into the search. Returns true
// if any state changed.
//
// This is strictly better information than a probe timeout, and it arrives for
// free. A timeout is *inference*: the ack didn't come back, so the size
// probably didn't fit — and it costs pmtuMaxTries × pmtuProbeTimeout to reach
// that guess. EMSGSIZE is the kernel stating, before the packet ever left the
// host, that the size definitely does not fit the outgoing interface. Throwing
// that away and waiting to time out instead is what turned a jumbo-to-cellular
// roam into a total blackout: with ceil at 9000 (a jumbo LAN) and the new link
// at ~1400, every probe was rejected locally, every rejection was logged at
// Debug and dropped, and the search crawled down through the whole range on
// timeouts alone — while nothing else could go out either, because eff was
// still sized for a link that no longer existed.
//
// Note this also handles the case a probe timeout cannot: EMSGSIZE on a
// *non-probe* packet (a keepalive, a handshake, real traffic) means eff itself
// is now too large. There is no probe in flight to time out, so the search
// would never learn it, and the peer would be blackholed indefinitely. Falling
// back to the floor is always safe — the floor is the configured
// known-good underlay MTU.
func (p *pmtuState) tooBig(size int, now time.Time) bool {
	if size <= p.floor {
		// Even the floor doesn't fit. Nothing this state machine can do; the
		// operator's configured underlay_mtu is wrong for this path. Don't
		// thrash the search over it.
		return false
	}
	changed := false
	if p.awaiting && p.cand == size {
		// The in-flight probe was rejected outright. Fail the candidate now
		// rather than after its retries expire.
		p.awaiting = false
		p.tries = 0
		p.onCandFailed()
		changed = true
	} else if size > p.floor && size-1 < p.high {
		// A non-probe packet (or a stale probe) was rejected. Pull the upper
		// bound down to just under what the kernel refused.
		p.high = size - 1
		if p.high < p.low {
			p.high = p.low
		}
		changed = true
	}
	if p.eff >= size {
		// Whatever we're currently sending at is too big for this path. Drop to
		// the floor immediately and re-search upward — do not keep emitting
		// packets the kernel will keep refusing.
		p.eff = p.floor
		p.low = p.floor
		p.phase = phaseSearch
		p.awaiting = false
		p.tries = 0
		changed = true
	}
	if changed {
		p.revalAt = now.Add(pmtuRevalidate)
	}
	return changed
}

// ack records a successful probe reply.
func (p *pmtuState) ack(id uint32, now time.Time) {
	if !p.awaiting || id != p.probeID {
		return
	}
	p.awaiting = false
	switch p.phase {
	case phaseValidate:
		if p.eff < p.ceil {
			p.low, p.high, p.phase = p.eff, p.ceil, phaseSearch // still good — try to grow
		} else {
			p.phase, p.revalAt = phaseSettled, now.Add(pmtuRevalidate)
		}
	default: // phaseSearch
		if p.cand > p.low {
			p.low = p.cand
		}
		if p.cand > p.eff {
			p.eff = p.cand
		}
	}
}

// reset abandons the current discovery result and re-searches from the floor.
// eff drops to the floor immediately so the send path shrinks to a size that is
// safe on any path right now (restoring connectivity through a freshly-lowered
// path MTU), then discovery climbs back toward the new path's true maximum. Used
// when the underlay path changes (a peer roams, or our local address changes).
func (p *pmtuState) reset(now time.Time) {
	if p.ceil <= p.floor {
		return // discovery disabled; nothing to search
	}
	p.eff = p.floor
	p.low = p.floor
	p.high = p.ceil
	p.phase = phaseSearch
	p.awaiting = false
	p.tries = 0
	p.cand = 0
}

// ---- engine glue ----

// initPMTU sets up a peer's discovery state when its session is installed.
func (ps *peerSession) initPMTU(floor, ceil int) {
	ps.pmtuMu.Lock()
	ps.pmtu = newPMTUState(floor, ceil, time.Now())
	ps.pmtuMu.Unlock()
	ps.setEff(floor)
}

// resetPMTU re-runs path-MTU discovery for this peer, immediately dropping the
// published outer MTU to the floor so traffic keeps flowing across a path whose
// MTU just shrank, then re-climbing. Safe to call from the receive path.
func (ps *peerSession) resetPMTU() {
	ps.pmtuMu.Lock()
	if ps.pmtu == nil {
		ps.pmtuMu.Unlock()
		return
	}
	ps.pmtu.reset(time.Now())
	eff := ps.pmtu.eff
	ps.pmtuMu.Unlock()
	ps.setEff(eff)
}

// setEff publishes a newly-discovered outer MTU to the lock-free send path.
func (ps *peerSession) setEff(mtu int) {
	ps.effMTU.Store(int32(mtu))
	ps.maxFrag.Store(int32(computeMaxInnerFrag(mtu)))
}

// pmtuTick drives one peer's discovery: issue/resend/expire probes as due.
func (e *Engine) pmtuTick(ps *peerSession) {
	ps.pmtuMu.Lock()
	if ps.pmtu == nil {
		ps.pmtuMu.Unlock()
		return
	}
	size, id, send := ps.pmtu.step(time.Now(), func() uint32 { return e.pmtuSeq.Add(1) })
	eff := ps.pmtu.eff
	ps.pmtuMu.Unlock()
	if int32(eff) != ps.effMTU.Load() {
		ps.setEff(eff)
		e.log.Debugf("mesh: path mtu to %s now %d bytes", ps.nodeID, eff)
	}
	if send && e.probeReachable(ps) {
		e.sendProbe(ps, size, id)
	}
}

// probeReachable reports whether a PMTU probe to this peer can plausibly leave
// the host right now. A direct peer is probed at its underlay endpoint; if this
// host holds no routable address in that endpoint's family — e.g. we just roamed
// onto an IPv4-only tether and the peer's endpoint is IPv6 — the probe is a
// guaranteed ENETUNREACH, emitted every second for as long as the session lasts.
// That is the same wasted, log-drowning syscall class canSourceFamily was added
// (v388) to suppress on the handshake and seed paths; the probe path was the one
// sender it never covered. A relayed peer is reached through its relay's endpoint,
// whose own reachability the relay session already governs, so it isn't gated here.
// Fail-open, exactly like canSourceFamily: with nothing enumerated yet, or no valid
// endpoint to judge, probing proceeds rather than wedging discovery shut.
func (e *Engine) probeReachable(ps *peerSession) bool {
	if ps.getRelay() != nil {
		return true // sent via the relay; the relay path governs its own reachability
	}
	ep := ps.ep()
	if !ep.IsValid() {
		return true // nothing to judge against: don't suppress on no evidence
	}
	return e.canSourceFamily(ep.Addr())
}

// sendProbe transmits a single sealed probe padded so the outer datagram is
// `size` bytes (using the conservative IPv6 outer overhead). It is sent directly,
// bypassing fragmentation, so the wire datagram really is that size.
func (e *Engine) sendProbe(ps *peerSession, size int, id uint32) {
	bodyLen, ok := probeBodyLen(size)
	if !ok {
		return
	}
	body := make([]byte, bodyLen) // zero padding after the header
	binary.BigEndian.PutUint32(body[0:4], id)
	binary.BigEndian.PutUint16(body[4:6], uint16(size))
	e.sealAndSend(ps, innerMTUProbe, body)
}

// onMTUProbe echoes an ack so the sender learns this size got through.
func (e *Engine) onMTUProbe(ps *peerSession, body []byte) {
	if len(body) < pmtuProbeHdrLen {
		return
	}
	ack := make([]byte, pmtuProbeHdrLen)
	copy(ack, body[:pmtuProbeHdrLen]) // echo [probe_id:4][size:2]
	e.sealAndSend(ps, innerMTUAck, ack)
}

// onMTUAck confirms a probed size and lets the search advance/grow.
func (e *Engine) onMTUAck(ps *peerSession, body []byte) {
	if len(body) < pmtuProbeHdrLen {
		return
	}
	id := binary.BigEndian.Uint32(body[0:4])
	ps.pmtuMu.Lock()
	if ps.pmtu == nil {
		ps.pmtuMu.Unlock()
		return
	}
	ps.pmtu.ack(id, time.Now())
	eff := ps.pmtu.eff
	ps.pmtuMu.Unlock()
	if int32(eff) != ps.effMTU.Load() {
		ps.setEff(eff)
		e.log.Debugf("mesh: path mtu to %s now %d bytes", ps.nodeID, eff)
	}
}

// localSourceIP returns the local address the kernel would use to reach dst.
// A UDP "connect" performs a route lookup and binds a source without sending any
// packet, so this is cheap and reflects the current default path.
func localSourceIP(dst netip.AddrPort) (netip.Addr, bool) {
	c, err := net.Dial("udp", dst.String())
	if err != nil {
		return netip.Addr{}, false
	}
	defer c.Close()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		if a, ok2 := netip.AddrFromSlice(ua.IP); ok2 {
			return a.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// connectedEndpoint returns a directly-connected peer's underlay endpoint to
// probe against, plus the node ID it came from. If preferID names a peer
// that's still directly connected, it's reused so repeated calls keep probing
// the same destination. Otherwise selection falls back to the
// lexicographically smallest node ID among currently connected peers — a
// deterministic tie-break, not Go's randomized map iteration order, so
// consecutive calls with no preference don't arbitrarily bounce between
// different peers. That matters because on a multi-homed host with two or
// more directly-connected peers reachable via different local interfaces,
// bouncing between them would make localSourceIP legitimately return a
// different address each time — not because this host's own underlay
// changed, but because a different peer (reachable via a different local
// path) was sampled. See checkUnderlayChange, which relies on preferID to
// keep the comparison anchored to one fixed destination.
func (e *Engine) connectedEndpoint(preferID string) (netip.AddrPort, string) {
	for _, ns := range e.netSnapshot() {
		ns.mu.RLock()
		if preferID != "" {
			if ps, ok := ns.byNode[preferID]; ok && ps.getRelay() == nil {
				if ep := ps.ep(); ep.IsValid() {
					ns.mu.RUnlock()
					return ep, preferID
				}
			}
		}
		var bestID string
		var bestEP netip.AddrPort
		for id, ps := range ns.byNode {
			if ps.getRelay() != nil {
				continue
			}
			ep := ps.ep()
			if !ep.IsValid() {
				continue
			}
			if bestID == "" || id < bestID {
				bestID = id
				bestEP = ep
			}
		}
		ns.mu.RUnlock()
		if bestID != "" {
			return bestEP, bestID
		}
	}
	return netip.AddrPort{}, ""
}

// resetAllPMTU re-runs path-MTU discovery for every peer on every network.
func (e *Engine) resetAllPMTU() {
	for _, ns := range e.netSnapshot() {
		ns.mu.RLock()
		peers := make([]*peerSession, 0, len(ns.byNode))
		for _, ps := range ns.byNode {
			peers = append(peers, ps)
		}
		ns.mu.RUnlock()
		for _, ps := range peers {
			ps.resetPMTU()
		}
	}
}

// checkUnderlayChange detects a change in the local underlay source address
// (e.g. the user switched Wi-Fi networks or failed over to cellular) and,
// when it changes, re-runs path-MTU discovery for all peers (the new path may
// have a smaller MTU, which would otherwise black-hole large packets until
// the slow periodic revalidation noticed) and re-drives reassertOSState for
// every network. The latter is what makes full-tunnel's default-route
// demotion (see fulltunnel.go's demotePhysicalDefaultRoute) self-correcting
// across a network change instead of only across a sleep/resume cycle: a
// plain Wi-Fi roam can leave the tun interface itself completely undisturbed
// (reconcileDataplane's kernel-truth check finds nothing wrong and never
// calls reassertOSState on its own), while the *physical* default route
// underneath it is now a different route entirely — this is the trigger that
// notices and asks every network to re-verify. Safe to call unconditionally:
// reassertOSState is a no-op for a network with nothing to fix, and
// demotePhysicalDefaultRoute (as of v323) checks the live routing table
// before touching anything, so calling it more often than strictly necessary
// no longer risks the state corruption it used to. Rate-limited to roughly
// once per second by the caller-side check below.
func (e *Engine) checkUnderlayChange(now time.Time) {
	e.underlayMu.Lock()
	if now.Sub(e.lastUnderlayCheck) < time.Second {
		e.underlayMu.Unlock()
		return
	}
	e.lastUnderlayCheck = now
	prevRefNode := e.underlayRefNode
	e.underlayMu.Unlock()

	dst, refNode := e.connectedEndpoint(prevRefNode)
	if !dst.IsValid() {
		return
	}
	cur, ok := localSourceIP(dst)
	if !ok {
		return
	}

	e.underlayMu.Lock()
	prev := e.localUnderlay
	// Only a same-reference-peer comparison can tell us our own underlay
	// changed. If the reference peer itself changed (first check, or the
	// previous one disconnected), rebase silently instead: a different local
	// source address for a *different* destination is expected on a
	// multi-homed host and isn't evidence anything here actually moved.
	sameRef := prevRefNode != "" && refNode == prevRefNode
	changed := sameRef && prev.IsValid() && prev != cur
	e.localUnderlay = cur
	e.underlayRefNode = refNode
	e.underlayMu.Unlock()

	if changed {
		e.log.Infof("mesh: local underlay address changed %s -> %s; re-running path MTU discovery for all peers", prev, cur)
		e.resetAllPMTU()
		for _, ns := range e.netSnapshot() {
			e.reassertOSState(ns)
		}
		// In-process recovery above (drop every path MTU to the floor, re-assert
		// each network's OS state) is a best-effort mitigation. A roam can also
		// leave peers pinned to endpoints on the network we just left and OS
		// routes pointing at the old gateway — state only a clean restart rebuilds
		// reliably. If a restart hook is installed, request one; it's grace-gated
		// and one-shot so a flapping link can't spin the service.
		e.notifyUnderlayChange()
	}
}

// pmtuLoop drives discovery for every peer on this network on a fast cadence so
// the path MTU converges within seconds (data flows at the floor meanwhile),
// and — regardless of whether discovery itself is enabled — is what drives
// checkUnderlayChange, since it's the only per-second tick a netState owns.
//
// It used to return immediately, before ever ticking, when discovery was
// disabled (pmtuCeil <= pmtuFloor, e.g. an operator-set pmtu_discovery:false
// or underlay_mtu_max <= underlay_mtu). That silently took checkUnderlayChange
// down with it: no roam detection, no resetAllPMTU/reassertOSState recovery,
// and — because SetUnderlayChangeHook's restart-on-underlay-change callback is
// only ever invoked from inside checkUnderlayChange — no automatic restart on
// a Wi-Fi/cellular roam either, even though restart_on_underlay_change is a
// separate config knob a operator may have deliberately left on while turning
// discovery off (e.g. to avoid probe traffic on a metered link — precisely
// the kind of link most likely to roam). None of that coupling was
// documented anywhere an operator could have discovered it short of reading
// this function. The tick now always runs; only the per-peer pmtuTick calls
// below are skipped when discovery is off.
func (e *Engine) pmtuLoop(ns *netState) {
	defer ns.wg.Done()
	discoveryEnabled := e.pmtuCeil > e.pmtuFloor
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ns.done:
			return
		case <-t.C:
		}
		e.checkUnderlayChange(time.Now())
		if !discoveryEnabled {
			continue
		}
		ns.mu.RLock()
		peers := make([]*peerSession, 0, len(ns.byNode))
		for _, ps := range ns.byNode {
			peers = append(peers, ps)
		}
		ns.mu.RUnlock()
		for _, ps := range peers {
			e.pmtuTick(ps)
		}
	}
}

// probeBodyLen returns the sealed-body length whose resulting outer datagram is
// `size` bytes, and whether a probe of that size is representable.
func probeBodyLen(size int) (int, bool) {
	sealed := size - fragOverheadV6 // target sealed-datagram size (conservative v6)
	bodyLen := sealed - protocol.DataHeaderLen - 1 - protocol.GCMOverhead
	if bodyLen < pmtuProbeHdrLen {
		return 0, false
	}
	return bodyLen, true
}
