package mesh

import (
	"net/netip"
	"time"
)

// Conditional-forwarding domain advertisement. A node can advertise "route
// queries under this domain to these servers" records that flood mesh-wide,
// same as custom hosts records (hostadv.go) — origin-stamped, re-advertised on
// the route cadence, flooded to all peers, swept when the origin goes silent,
// and explicitly withdrawn on a config change rather than waiting out the TTL.
//
// Unlike hosts records, an accepting peer doesn't write these directly into a
// file; syncDNS (dnssync.go) hands the accumulated set to internal/resolver,
// which registers them with the OS's native split-DNS mechanism.

// dnsForward is a domain -> servers forward this node advertises (from config).
type dnsForward struct {
	domain  string
	servers []netip.Addr
}

// DNSForwardSpec is the exported config-facing form of a conditional-forward.
type DNSForwardSpec struct {
	Domain  string
	Servers []netip.Addr
}

func toDNSForwards(specs []DNSForwardSpec) []dnsForward {
	out := make([]dnsForward, 0, len(specs))
	for _, s := range specs {
		if s.Domain == "" || len(s.Servers) == 0 {
			continue
		}
		servers := make([]netip.Addr, 0, len(s.Servers))
		for _, a := range s.Servers {
			if a.IsValid() {
				servers = append(servers, a)
			}
		}
		if len(servers) == 0 {
			continue
		}
		out = append(out, dnsForward{domain: s.Domain, servers: servers})
	}
	return out
}

// learnedDNS is a forward record learned from a peer, kept until its origin
// goes quiet.
type learnedDNS struct {
	origin   string
	domain   string
	servers  []netip.Addr
	lastSeen time.Time
}

func dnsKey(origin, domain string) string { return origin + "|" + domain }

// addr decodes one [fam:1][addr] pair (same wire shape as encodeAddr) and
// advances the cursor, unlike the free decodeAddr function which is only safe
// as a message's last field since it doesn't track an offset.
func (r *reader) addr() (netip.Addr, bool) {
	fam, ok := r.byte()
	if !ok {
		return netip.Addr{}, false
	}
	switch fam {
	case 4:
		b, ok := r.take(4)
		if !ok {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte(b)), true
	case 6:
		b, ok := r.take(16)
		if !ok {
			return netip.Addr{}, false
		}
		return netip.AddrFrom16([16]byte(b)), true
	}
	return netip.Addr{}, false
}

func encodeDNSAdd(origin, domain string, servers []netip.Addr) []byte {
	out := []byte{ctrlDNSAdd}
	out = appendLenStr(out, origin)
	out = appendLenStr(out, domain)
	if len(servers) > 255 {
		servers = servers[:255]
	}
	out = append(out, byte(len(servers)))
	for _, s := range servers {
		out = append(out, encodeAddr(s)...)
	}
	return out
}

func encodeDNSDel(origin, domain string) []byte {
	out := []byte{ctrlDNSDel}
	out = appendLenStr(out, origin)
	out = appendLenStr(out, domain)
	return out
}

func decodeDNSAdd(b []byte) (origin, domain string, servers []netip.Addr, ok bool) {
	r := reader{b: b}
	origin, ok = r.lenStr()
	if !ok {
		return
	}
	domain, ok = r.lenStr()
	if !ok {
		return
	}
	n, ok := r.byte()
	if !ok {
		return "", "", nil, false
	}
	servers = make([]netip.Addr, 0, n)
	for i := 0; i < int(n); i++ {
		a, ok2 := r.addr()
		if !ok2 {
			return "", "", nil, false
		}
		servers = append(servers, a)
	}
	if domain == "" || len(servers) == 0 {
		return "", "", nil, false
	}
	return origin, domain, servers, true
}

func decodeDNSDel(b []byte) (origin, domain string, ok bool) {
	r := reader{b: b}
	origin, ok = r.lenStr()
	if !ok {
		return
	}
	domain, ok = r.lenStr()
	if !ok || domain == "" {
		return "", "", false
	}
	return origin, domain, true
}

// advertiseDNS floods this node's configured conditional-forward domains to
// the mesh.
func (e *Engine) advertiseDNS(ns *netState) {
	ds := ns.advDNS.Load()
	if ds == nil {
		return
	}
	for _, d := range *ds {
		e.floodControl(ns, encodeDNSAdd(e.nodeID, d.domain, d.servers), nil)
	}
}

// onDNSAdd records a peer-advertised forward and re-floods it onward.
func (e *Engine) onDNSAdd(ps *peerSession, body []byte) {
	origin, domain, servers, ok := decodeDNSAdd(body)
	if !ok || origin == e.nodeID {
		return
	}
	ns := ps.net
	key := dnsKey(origin, domain)
	now := time.Now()
	ns.mu.Lock()
	cur, known := ns.learnedDNS[key]
	changed := !known || !sameAddrs(cur.servers, servers)
	if known {
		cur.lastSeen = now
		cur.servers = servers
	} else {
		ns.learnedDNS[key] = &learnedDNS{origin: origin, domain: domain, servers: servers, lastSeen: now}
	}
	ns.mu.Unlock()

	if !known {
		e.log.Infof("mesh: learned dns forward %s -> %v via %q on net %x", domain, servers, origin, ns.spec.ID)
	}
	if changed {
		// New record or a changed server set: propagate and refresh the resolver.
		e.floodControl(ns, encodeDNSAdd(origin, domain, servers), ps)
		ns.lastDNSSig = "" // force a rewrite on the next sync
		e.syncDNS(ns, now)
	}
}

// onDNSDel removes a withdrawn forward and propagates the withdrawal.
func (e *Engine) onDNSDel(ps *peerSession, body []byte) {
	origin, domain, ok := decodeDNSDel(body)
	if !ok || origin == e.nodeID {
		return
	}
	ns := ps.net
	key := dnsKey(origin, domain)
	ns.mu.Lock()
	_, had := ns.learnedDNS[key]
	if had {
		delete(ns.learnedDNS, key)
	}
	ns.mu.Unlock()
	if had {
		e.log.Infof("mesh: dns forward %s via %q withdrawn on net %x", domain, origin, ns.spec.ID)
		e.floodControl(ns, encodeDNSDel(origin, domain), ps)
		ns.lastDNSSig = ""
		e.syncDNS(ns, time.Now())
	}
}

// withdrawDNS floods withdrawals for forwards removed from config.
func (e *Engine) withdrawDNS(ns *netState, fwds []dnsForward) {
	for _, d := range fwds {
		e.floodControl(ns, encodeDNSDel(e.nodeID, d.domain), nil)
		e.log.Infof("mesh: withdrawing dns forward %s on net %x", d.domain, ns.spec.ID)
	}
}

// sweepStaleDNS drops learned forwards whose origin has gone silent, reusing
// the same TTL as route/host staleness (a peer that's stopped re-advertising
// anything is equally stale across all three, so one cadence covers all).
func (e *Engine) sweepStaleDNS(ns *netState, now time.Time) {
	ttl := e.routeTTL()
	var dropped bool
	ns.mu.Lock()
	for k, d := range ns.learnedDNS {
		if now.Sub(d.lastSeen) > ttl {
			delete(ns.learnedDNS, k)
			dropped = true
		}
	}
	ns.mu.Unlock()
	if dropped {
		ns.lastDNSSig = ""
		e.syncDNS(ns, now)
	}
}

// reloadDNS swaps the advertised forward set live and floods the delta:
// newly-added forwards are advertised, removed ones withdrawn.
func (e *Engine) reloadDNS(ns *netState, fwds []dnsForward, reject []string) {
	var old []dnsForward
	if p := ns.advDNS.Load(); p != nil {
		old = *p
	}
	nf := append([]dnsForward(nil), fwds...)
	ns.advDNS.Store(&nf)
	drj := lowerAll(reject)
	ns.advDNSReject.Store(&drj)

	inNew := make(map[string][]netip.Addr, len(fwds))
	for _, d := range fwds {
		inNew[d.domain] = d.servers
	}
	// Withdraw forwards that are gone (or whose servers changed — re-advertised below).
	var gone []dnsForward
	for _, d := range old {
		if servers, ok := inNew[d.domain]; !ok || !sameAddrs(servers, d.servers) {
			gone = append(gone, d)
		}
	}
	if len(gone) > 0 {
		e.withdrawDNS(ns, gone)
	}
	e.advertiseDNS(ns)
	ns.lastDNSSig = ""
	e.syncDNS(ns, time.Now())
}

func sameAddrs(a, b []netip.Addr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
