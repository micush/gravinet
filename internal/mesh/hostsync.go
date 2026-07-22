package mesh

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"gravinet/internal/hosts"
)

// syncHosts reflects currently-connected peers into the OS hosts file, writing
// a per-network managed block. It debounces by content signature so the file is
// only rewritten when the set of host→address mappings actually changes. A peer
// that goes silent is pruned from the session table, so it naturally falls out
// of the hosts file (TTL-by-liveness).
func (e *Engine) syncHosts(ns *netState, now time.Time) {
	if !ns.spec.HostsSync {
		return
	}
	path := ns.spec.HostsPath
	if path == "" {
		path = hosts.DefaultPath()
	}

	ns.mu.RLock()
	entries := make([]hosts.Entry, 0, len(ns.byNode))
	for _, ps := range ns.byNode {
		if ps.hostname == "" {
			continue
		}
		if !ps.overlay4.IsValid() && !ps.overlay6.IsValid() {
			continue
		}
		entries = append(entries, hosts.Entry{Hostname: ps.hostname, V4: ps.overlay4, V6: ps.overlay6})
	}
	// Custom records learned from peers (name -> arbitrary IP), minus any whose
	// hostname this node rejects (a local filter; mirrors route-reject).
	for _, h := range ns.learnedHosts {
		if ns.hostRejected(h.name) {
			continue
		}
		entries = append(entries, hostEntryFor(h.name, h.ip))
	}
	ns.mu.RUnlock()

	// This node's own advertised records, so it resolves them too.
	if p := ns.advHosts.Load(); p != nil {
		for _, h := range *p {
			entries = append(entries, hostEntryFor(h.name, h.ip))
		}
	}

	sig := hostsSignature(entries)
	if sig == ns.lastHostsSig {
		return // nothing changed; avoid rewriting the file
	}
	tag := fmt.Sprintf("%016x", ns.spec.ID)
	if err := hosts.Sync(path, tag, entries); err != nil {
		e.log.Debugf("mesh: hosts sync (%s): %v", path, err)
		return
	}
	ns.lastHostsSig = sig
	e.log.Debugf("mesh: hosts file updated with %d entries (net %x)", len(entries), ns.spec.ID)
}

// hostEntryFor builds a hosts.Entry for a custom name -> IP record, placing the
// address in the v4 or v6 slot by family.
func hostEntryFor(name string, ip netip.Addr) hosts.Entry {
	e := hosts.Entry{Hostname: name}
	if ip.Is4() {
		e.V4 = ip
	} else {
		e.V6 = ip
	}
	return e
}

func hostsSignature(entries []hosts.Entry) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Hostname+"|"+e.V4.String()+"|"+e.V6.String())
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// hostRejected reports whether a peer-advertised host record for name is refused
// by this node's local reject filter. Matching is case-insensitive.
func (ns *netState) hostRejected(name string) bool {
	p := ns.advHostReject.Load()
	if p == nil || len(*p) == 0 {
		return false
	}
	name = strings.ToLower(name)
	for _, r := range *p {
		if r == name {
			return true
		}
	}
	return false
}

// lowerAll returns a copy of names with each entry lowercased, for
// case-insensitive hostname matching.
func lowerAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.ToLower(n)
	}
	return out
}
