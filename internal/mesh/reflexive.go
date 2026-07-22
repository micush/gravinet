package mesh

import (
	"net"
	"net/netip"
	"time"
)

// Reflexive NAT discovery. Each node tells every directly-connected peer the
// underlay address it observes that peer at (a STUN-style server-reflexive
// report). Aggregating the reports peers send back about *this* node lets it
// learn its own public endpoint and classify its NAT:
//
//   - open      reflexive address is one of our own interface addresses (no NAT)
//   - cone      ≥2 peers agree on one stable mapped address (hole-punchable)
//   - nat       exactly one report, mapped (NAT confirmed, type not yet known)
//   - symmetric peers disagree → per-destination mapping (relay usually needed)
//   - unknown   no reports yet
//
// The exchange is backward-compatible: ctrlReflexive is an unknown control type
// to older peers, which ignore it, so only nodes on this build learn a status.

const reflexiveTTL = 2 * time.Minute

type reflexiveObs struct {
	addr netip.AddrPort
	at   time.Time
}

// sendReflexive tells each directly-connected peer the address we see it at.
// Relayed peers are skipped: we don't directly observe their source, so any
// report would describe the relay, not them.
func (e *Engine) sendReflexive(ns *netState) {
	ns.mu.RLock()
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		peers = append(peers, ps)
	}
	ns.mu.RUnlock()
	for _, ps := range peers {
		if ps.getRelay() != nil {
			continue
		}
		ep := ps.ep()
		if !ep.IsValid() {
			continue
		}
		e.sendControl(ps, appendEndpoint([]byte{ctrlReflexive}, ep))
	}
}

// onReflexive records a peer's report of the address it observes us at.
func (e *Engine) onReflexive(ps *peerSession, body []byte) {
	r := reader{b: body}
	ep, ok := r.endpoint()
	if !ok || !ep.IsValid() {
		return
	}
	e.reflexiveMu.Lock()
	e.reflexive[ps.nodeID] = reflexiveObs{addr: ep, at: time.Now()}
	e.reflexiveMu.Unlock()
}

// NATStatus classifies this node's reachability from the reflexive reports peers
// have sent. public is the observed public endpoint when peers agree on one.
func (e *Engine) NATStatus() (class string, public netip.AddrPort) {
	now := time.Now()
	e.reflexiveMu.Lock()
	obs := make([]netip.AddrPort, 0, len(e.reflexive))
	for id, o := range e.reflexive {
		if now.Sub(o.at) > reflexiveTTL {
			delete(e.reflexive, id)
			continue
		}
		obs = append(obs, o.addr)
	}
	e.reflexiveMu.Unlock()

	if len(obs) == 0 {
		return "unknown", netip.AddrPort{}
	}
	distinct := map[netip.AddrPort]struct{}{}
	for _, a := range obs {
		distinct[a] = struct{}{}
	}
	if len(distinct) > 1 {
		// Different vantage points see different mappings → symmetric NAT.
		return "symmetric", netip.AddrPort{}
	}
	public = obs[0]
	if e.isLocalUnderlayAddr(public.Addr()) {
		return "open", public
	}
	if len(obs) < 2 {
		return "nat", public // single report: NAT confirmed, type unconfirmed
	}
	return "cone", public
}

// NATStatusStrings is NATStatus with the endpoint pre-formatted (empty when not
// yet known or inconsistent), for the control and web surfaces.
func (e *Engine) NATStatusStrings() (class, public string) {
	c, p := e.NATStatus()
	if p.IsValid() {
		return c, p.String()
	}
	return c, ""
}

// isLocalUnderlayAddr reports whether ip is one of this host's own interface
// addresses — used to tell "no NAT" (reflexive == local) from a mapped address.
func (e *Engine) isLocalUnderlayAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if na, ok := netip.AddrFromSlice(ipn.IP); ok && na.Unmap() == ip {
				return true
			}
		}
	}
	return false
}
