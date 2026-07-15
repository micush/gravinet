package mesh

import (
	"net/netip"
	"time"
)

// Custom hosts-record advertisement. Beyond the automatic peer-hostname ->
// overlay-address entries, a node can advertise arbitrary "name -> IP" records
// (e.g. web.local -> 192.168.5.5) that flood mesh-wide and land in every peer's
// hosts file. Propagation mirrors route redistribution exactly: each record is
// origin-stamped, re-advertised on the route cadence, flooded to all peers, and
// swept when its origin goes silent. A config change floods an explicit
// withdrawal so removals apply promptly rather than waiting for the TTL.

// hostRecord is a name -> IP this node advertises (from config).
type hostRecord struct {
	name string
	ip   netip.Addr
}

// HostRecordSpec is the exported config-facing form of a custom hosts record.
type HostRecordSpec struct {
	Name string
	IP   netip.Addr
}

func toHostRecords(specs []HostRecordSpec) []hostRecord {
	out := make([]hostRecord, 0, len(specs))
	for _, s := range specs {
		if s.Name == "" || !s.IP.IsValid() {
			continue
		}
		out = append(out, hostRecord{name: s.Name, ip: s.IP})
	}
	return out
}

// learnedHost is a record learned from a peer, kept until its origin goes quiet.
type learnedHost struct {
	origin   string
	name     string
	ip       netip.Addr
	lastSeen time.Time
}

func hostKey(origin, name string) string { return origin + "|" + name }

func encodeHostAdd(origin, name string, ip netip.Addr) []byte {
	out := []byte{ctrlHostAdd}
	out = appendLenStr(out, origin)
	out = appendLenStr(out, name)
	out = append(out, encodeAddr(ip)...)
	return out
}

func encodeHostDel(origin, name string) []byte {
	out := []byte{ctrlHostDel}
	out = appendLenStr(out, origin)
	out = appendLenStr(out, name)
	return out
}

func decodeHostAdd(b []byte) (origin, name string, ip netip.Addr, ok bool) {
	r := reader{b: b}
	origin, ok = r.lenStr()
	if !ok {
		return
	}
	name, ok = r.lenStr()
	if !ok {
		return
	}
	ip, ok = decodeAddr(r.b[r.off:])
	if !ok || name == "" {
		return "", "", netip.Addr{}, false
	}
	return origin, name, ip, true
}

func decodeHostDel(b []byte) (origin, name string, ok bool) {
	r := reader{b: b}
	origin, ok = r.lenStr()
	if !ok {
		return
	}
	name, ok = r.lenStr()
	if !ok || name == "" {
		return "", "", false
	}
	return origin, name, true
}

// advertiseHosts floods this node's configured custom records to the mesh.
func (e *Engine) advertiseHosts(ns *netState) {
	hs := ns.advHosts.Load()
	if hs == nil {
		return
	}
	for _, h := range *hs {
		e.floodControl(ns, encodeHostAdd(e.nodeID, h.name, h.ip), nil)
	}
}

// onHostAdd records a peer-advertised custom record and re-floods it onward.
func (e *Engine) onHostAdd(ps *peerSession, body []byte) {
	origin, name, ip, ok := decodeHostAdd(body)
	if !ok || origin == e.nodeID {
		return
	}
	ns := ps.net
	key := hostKey(origin, name)
	now := time.Now()
	ns.mu.Lock()
	cur, known := ns.learnedHosts[key]
	changed := !known || cur.ip != ip
	if known {
		cur.lastSeen = now
		cur.ip = ip
	} else {
		ns.learnedHosts[key] = &learnedHost{origin: origin, name: name, ip: ip, lastSeen: now}
	}
	ns.mu.Unlock()

	if !known {
		e.log.Infof("mesh: learned host %s -> %s via %q on net %x", name, ip, origin, ns.spec.ID)
	}
	if changed {
		// New record or a changed address: propagate and refresh the hosts file.
		e.floodControl(ns, encodeHostAdd(origin, name, ip), ps)
		ns.lastHostsSig = "" // force a rewrite on the next sync
		e.syncHosts(ns, now)
	}
}

// onHostDel removes a withdrawn record and propagates the withdrawal.
func (e *Engine) onHostDel(ps *peerSession, body []byte) {
	origin, name, ok := decodeHostDel(body)
	if !ok || origin == e.nodeID {
		return
	}
	ns := ps.net
	key := hostKey(origin, name)
	ns.mu.Lock()
	_, had := ns.learnedHosts[key]
	if had {
		delete(ns.learnedHosts, key)
	}
	ns.mu.Unlock()
	if had {
		e.log.Infof("mesh: host %s via %q withdrawn on net %x", name, origin, ns.spec.ID)
		e.floodControl(ns, encodeHostDel(origin, name), ps)
		ns.lastHostsSig = ""
		e.syncHosts(ns, time.Now())
	}
}

// withdrawHosts floods withdrawals for records removed from config.
func (e *Engine) withdrawHosts(ns *netState, recs []hostRecord) {
	for _, h := range recs {
		e.floodControl(ns, encodeHostDel(e.nodeID, h.name), nil)
		e.log.Infof("mesh: withdrawing host %s on net %x", h.name, ns.spec.ID)
	}
}

// sweepStaleHosts drops learned records whose origin has gone silent.
func (e *Engine) sweepStaleHosts(ns *netState, now time.Time) {
	ttl := e.routeTTL()
	var dropped bool
	ns.mu.Lock()
	for k, h := range ns.learnedHosts {
		if now.Sub(h.lastSeen) > ttl {
			delete(ns.learnedHosts, k)
			dropped = true
		}
	}
	ns.mu.Unlock()
	if dropped {
		ns.lastHostsSig = ""
		e.syncHosts(ns, now)
	}
}

// reloadHosts swaps the advertised record set live and floods the delta:
// newly-added records are advertised, removed ones withdrawn.
func (e *Engine) reloadHosts(ns *netState, recs []hostRecord, reject []string) {
	var old []hostRecord
	if p := ns.advHosts.Load(); p != nil {
		old = *p
	}
	nr := append([]hostRecord(nil), recs...)
	ns.advHosts.Store(&nr)
	hrj := lowerAll(reject)
	ns.advHostReject.Store(&hrj)

	inNew := make(map[string]netip.Addr, len(recs))
	for _, h := range recs {
		inNew[h.name] = h.ip
	}
	// Withdraw records that are gone (or whose IP changed — re-advertised below).
	var gone []hostRecord
	for _, h := range old {
		if ip, ok := inNew[h.name]; !ok || ip != h.ip {
			gone = append(gone, h)
		}
	}
	if len(gone) > 0 {
		e.withdrawHosts(ns, gone)
	}
	e.advertiseHosts(ns)
	ns.lastHostsSig = ""
	e.syncHosts(ns, time.Now())
}
