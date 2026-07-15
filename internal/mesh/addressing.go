package mesh

import (
	"crypto/rand"
	"encoding/binary"
	"net/netip"
	"time"
)

const (
	dadWait     = 800 * time.Millisecond // how long to listen for a defense
	dadAttempts = 8                      // candidate picks before giving up
)

// selfAddrs returns this node's overlay addresses on the network.
func (ns *netState) selfAddrs() (netip.Addr, netip.Addr) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.self4, ns.self6
}

// subnets returns the network's known overlay subnets.
func (ns *netState) subnets() (netip.Prefix, netip.Prefix) {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.subnet4, ns.subnet6
}

// netName returns the network's current (possibly learned) name.
func (ns *netState) netName() string {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.name
}

// absorbIdentity adopts peer-advertised network properties we don't have yet —
// the subnet and the name. A node that joined by id alone learns both this way.
// Returns true if anything was newly learned, so the caller can persist it.
func (ns *netState) absorbIdentity(pl hsPayload) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	changed := false
	if !ns.subnet4.IsValid() && pl.Subnet4.IsValid() {
		ns.subnet4 = pl.Subnet4
		changed = true
	}
	if !ns.subnet6.IsValid() && pl.Subnet6.IsValid() {
		ns.subnet6 = pl.Subnet6
		changed = true
	}
	if ns.name == "" && pl.Name != "" {
		ns.name = pl.Name
		changed = true
	}
	return changed
}

// maybeAssignAddress kicks off dynamic addressing if we still lack an overlay
// address but know our subnet. It runs even with no peers: the first node in a
// network must be able to bootstrap its own address (with no peers there is
// nothing to collide with), and dadProbe degrades correctly to "free" when there
// is no one to defend. As peers appear, DAD probes them for conflicts normally.
// Cheap to call repeatedly (from startup, handshake completion, and maintenance).
func (e *Engine) maybeAssignAddress(ns *netState) {
	ns.mu.Lock()
	if ns.assigning {
		ns.mu.Unlock()
		return
	}
	need4 := !ns.self4.IsValid() && ns.subnet4.IsValid()
	need6 := !ns.self6.IsValid() && ns.subnet6.IsValid()
	if !need4 && !need6 {
		ns.mu.Unlock()
		return
	}
	ns.assigning = true
	ns.mu.Unlock()

	ns.wg.Add(1)
	go e.runDAD(ns)
}

func (e *Engine) runDAD(ns *netState) {
	defer ns.wg.Done()
	defer func() {
		ns.mu.Lock()
		ns.assigning = false
		ns.mu.Unlock()
	}()

	ns.mu.RLock()
	need4 := !ns.self4.IsValid() && ns.subnet4.IsValid()
	sub4 := ns.subnet4
	need6 := !ns.self6.IsValid() && ns.subnet6.IsValid()
	sub6 := ns.subnet6
	ns.mu.RUnlock()

	if need4 {
		if addr, ok := e.dadPick(ns, sub4); ok {
			e.assignAddr(ns, addr, sub4.Bits())
		} else {
			e.log.Warnf("mesh: could not find a free IPv4 in %s on net %x", sub4, ns.spec.ID)
		}
	}
	if need6 {
		if addr, ok := e.dadPick(ns, sub6); ok {
			e.assignAddr(ns, addr, sub6.Bits())
		} else {
			e.log.Warnf("mesh: could not find a free IPv6 in %s on net %x", sub6, ns.spec.ID)
		}
	}
}

// dadPick repeatedly proposes a random address and probes the mesh until one is
// free or attempts are exhausted.
func (e *Engine) dadPick(ns *netState, prefix netip.Prefix) (netip.Addr, bool) {
	for i := 0; i < dadAttempts; i++ {
		cand := randomHost(prefix)
		if !cand.IsValid() {
			return netip.Addr{}, false
		}
		if e.dadProbe(ns, cand) {
			return cand, true
		}
		e.log.Debugf("mesh: DAD candidate %s is taken, retrying", cand)
	}
	return netip.Addr{}, false
}

// dadProbe returns true if cand appears free: it isn't known locally, and no
// peer defends it within dadWait.
func (e *Engine) dadProbe(ns *netState, cand netip.Addr) bool {
	if e.addrInUse(ns, cand) {
		return false
	}
	ns.mu.Lock()
	ns.dadCandidate = cand
	ns.dadDefended = false
	ns.mu.Unlock()

	q := append([]byte{ctrlDADQuery}, encodeAddr(cand)...)
	e.broadcastControl(ns, q)

	select {
	case <-e.stop:
		return false
	case <-ns.done:
		return false
	case <-time.After(dadWait):
	}

	ns.mu.Lock()
	defended := ns.dadDefended
	ns.dadCandidate = netip.Addr{}
	ns.mu.Unlock()
	return !defended
}

// addrInUse checks local knowledge: our own address, an active peer route, or
// a node in the registry.
func (e *Engine) addrInUse(ns *netState, a netip.Addr) bool {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if a == ns.self4 || a == ns.self6 {
		return true
	}
	if a.Is4() {
		if _, ok := ns.routes4[a]; ok {
			return true
		}
	} else if _, ok := ns.routes6[a]; ok {
		return true
	}
	for _, ni := range ns.nodes {
		if ni.overlay4 == a || ni.overlay6 == a {
			return true
		}
	}
	return false
}

// assignAddr configures the interface, records the address, and announces it.
//
// Beyond the address itself, this also explicitly installs the connected
// route for the whole subnet via AddRoute — not just relying on AddIPv4's
// netmask side effect to produce it. That side effect is what tun_darwin.go's
// point-to-point address trick is supposed to guarantee, but a live report
// (base subnet route missing on macOS, present for every other prefix
// including this same network's IPv6 /64, surviving both a restart and a
// sleep/resume cycle) shows it isn't reliable in practice. AddRoute is the
// same mechanism every redistributed route already goes through, and is a
// harmless no-op if the connected route is already there.
func (e *Engine) assignAddr(ns *netState, addr netip.Addr, bits int) {
	var err error
	if addr.Is4() {
		err = ns.dev().AddIPv4(addr, bits)
	} else {
		err = ns.dev().AddIPv6(addr, bits)
	}
	if err != nil {
		e.log.Errorf("mesh: assign %s on net %x: %v", addr, ns.spec.ID, err)
		return
	}
	ns.mu.Lock()
	if addr.Is4() {
		ns.self4 = addr
	} else {
		ns.self6 = addr
	}
	self4, self6 := ns.self4, ns.self6
	subnet4, subnet6 := ns.subnet4, ns.subnet6
	ns.mu.Unlock()

	if subnet := subnet4; addr.Is4() && subnet.IsValid() {
		if rerr := ns.dev().AddRoute(subnet, 0); rerr != nil {
			e.log.Debugf("mesh: assign: base route %s on %s (net %x): %v", subnet, ns.spec.Dev.Name(), ns.spec.ID, rerr)
		}
	} else if subnet := subnet6; !addr.Is4() && subnet.IsValid() {
		if rerr := ns.dev().AddRoute(subnet, 0); rerr != nil {
			e.log.Debugf("mesh: assign: base route %s on %s (net %x): %v", subnet, ns.spec.Dev.Name(), ns.spec.ID, rerr)
		}
	}

	e.log.Infof("mesh: assigned overlay address %s on net %x (%s)", addr, ns.spec.ID, ns.spec.Dev.Name())
	e.announceAddr(ns, self4, self6)
	e.notifyChange(ns.spec.ID) // pin the address in config for restart stability
}

// announceAddr tells every peer our current overlay address(es).
func (e *Engine) announceAddr(ns *netState, v4, v6 netip.Addr) {
	body := []byte{ctrlAddrNotify}
	body = append(body, encodeOptAddr(v4)...)
	body = append(body, encodeOptAddr(v6)...)
	e.broadcastControl(ns, body)
}

func (e *Engine) broadcastControl(ns *netState, ctrl []byte) {
	ns.mu.RLock()
	peers := make([]*peerSession, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		peers = append(peers, ps)
	}
	ns.mu.RUnlock()
	for _, ps := range peers {
		e.sendControl(ps, ctrl)
	}
}

// ---- control handlers ----

func (e *Engine) onDADQuery(ps *peerSession, body []byte) {
	a, ok := decodeAddr(body)
	if !ok {
		return
	}
	if e.addrInUse(ps.net, a) {
		e.sendControl(ps, append([]byte{ctrlDADDefend}, encodeAddr(a)...))
	}
}

func (e *Engine) onDADDefend(ps *peerSession, body []byte) {
	a, ok := decodeAddr(body)
	if !ok {
		return
	}
	ns := ps.net
	ns.mu.Lock()
	if a == ns.dadCandidate {
		ns.dadDefended = true
	}
	ns.mu.Unlock()
}

func (e *Engine) onAddrNotify(ps *peerSession, body []byte) {
	r := reader{b: body}
	v4, _ := r.optAddr()
	v6, _ := r.optAddr()
	ns := ps.net

	ns.mu.Lock()
	// repoint routes from any old address of this peer to the new one
	if ps.overlay4.IsValid() && ns.routes4[ps.overlay4] == ps {
		delete(ns.routes4, ps.overlay4)
	}
	if ps.overlay6.IsValid() && ns.routes6[ps.overlay6] == ps {
		delete(ns.routes6, ps.overlay6)
	}
	if v4.IsValid() {
		ps.overlay4 = v4
		ns.routes4[v4] = ps
	}
	if v6.IsValid() {
		ps.overlay6 = v6
		ns.routes6[v6] = ps
	}
	if ni := ns.nodes[ps.nodeID]; ni != nil {
		if v4.IsValid() {
			ni.overlay4 = v4
		}
		if v6.IsValid() {
			ni.overlay6 = v6
		}
	}
	ns.mu.Unlock()
	e.log.Debugf("mesh: peer %q announced overlay v4=%s v6=%s", ps.nodeID, v4, v6)
}

// ---- address codecs ----

// encodeAddr writes [fam:1][addr]; fam is 4 or 6.
func encodeAddr(a netip.Addr) []byte {
	a = a.Unmap()
	if a.Is4() {
		v := a.As4()
		return append([]byte{4}, v[:]...)
	}
	v := a.As16()
	return append([]byte{6}, v[:]...)
}

func decodeAddr(b []byte) (netip.Addr, bool) {
	if len(b) < 1 {
		return netip.Addr{}, false
	}
	switch b[0] {
	case 4:
		if len(b) < 5 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte{b[1], b[2], b[3], b[4]}), true
	case 6:
		if len(b) < 17 {
			return netip.Addr{}, false
		}
		var a [16]byte
		copy(a[:], b[1:17])
		return netip.AddrFrom16(a), true
	}
	return netip.Addr{}, false
}

// encodeOptAddr writes [fam:1]([addr]) where fam 0 means "none".
func encodeOptAddr(a netip.Addr) []byte {
	if !a.IsValid() {
		return []byte{0}
	}
	return encodeAddr(a)
}

func (r *reader) optAddr() (netip.Addr, bool) {
	fam, ok := r.byte()
	if !ok {
		return netip.Addr{}, false
	}
	switch fam {
	case 0:
		return netip.Addr{}, true
	case 4:
		v, ok := r.take(4)
		if !ok {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte{v[0], v[1], v[2], v[3]}), true
	case 6:
		v, ok := r.take(16)
		if !ok {
			return netip.Addr{}, false
		}
		var a [16]byte
		copy(a[:], v)
		return netip.AddrFrom16(a), true
	}
	return netip.Addr{}, false
}

// randomHost picks a uniformly-random usable host address inside prefix,
// avoiding the network and (for IPv4) broadcast addresses.
func randomHost(prefix netip.Prefix) netip.Addr {
	prefix = prefix.Masked()
	if prefix.Addr().Is4() {
		return randomHost4(prefix)
	}
	return randomHost6(prefix)
}

func randomHost4(prefix netip.Prefix) netip.Addr {
	bits := prefix.Bits()
	hostBits := 32 - bits
	netUint := binary.BigEndian.Uint32(asSlice4(prefix.Addr()))
	if hostBits <= 1 {
		// /31 or /32: just return the network address (point-to-point/host)
		return prefix.Addr()
	}
	span := uint32(1) << hostBits // number of addresses in the block
	for i := 0; i < 16; i++ {
		h := randUint32() % span
		if h == 0 || h == span-1 {
			continue // skip network and broadcast
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], netUint|h)
		return netip.AddrFrom4(b)
	}
	return netip.Addr{}
}

func randomHost6(prefix netip.Prefix) netip.Addr {
	bits := prefix.Bits()
	base := prefix.Addr().As16()
	var rnd [16]byte
	_, _ = rand.Read(rnd[:])
	out := base
	for i := 0; i < 128; i++ {
		if i < bits {
			continue // network portion fixed
		}
		byteIdx := i / 8
		bit := byte(1 << (7 - uint(i%8)))
		if rnd[byteIdx]&bit != 0 {
			out[byteIdx] |= bit
		} else {
			out[byteIdx] &^= bit
		}
	}
	a := netip.AddrFrom16(out)
	if a == prefix.Addr() { // avoid the all-zero host (subnet anycast)
		out[15] |= 1
		a = netip.AddrFrom16(out)
	}
	return a
}

func asSlice4(a netip.Addr) []byte {
	v := a.As4()
	return v[:]
}

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
