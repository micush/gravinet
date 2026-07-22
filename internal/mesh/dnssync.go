package mesh

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"gravinet/internal/resolver"
)

// syncDNS reflects the currently-accepted set of conditional-forward domains
// (this node's own advertised forwards, plus everything learned from peers and
// not locally rejected), plus this node's own configured search domains, into
// the OS's native split-DNS mechanism via internal/resolver. Debounced by
// content signature, same discipline as syncHosts, so the OS resolver is only
// touched when the accepted set actually changes.
func (e *Engine) syncDNS(ns *netState, now time.Time) {
	if !ns.spec.DNSSync {
		return
	}
	iface := ""
	if ns.spec.Dev != nil {
		iface = ns.spec.Dev.Name()
	}

	ns.mu.RLock()
	byDomain := make(map[string][]netip.Addr)
	for _, d := range ns.learnedDNS {
		if ns.dnsRejected(d.domain) {
			continue
		}
		// Domain names are case-insensitive, so canonicalize to lowercase
		// before using one as a map key here — this is the single point every
		// platform's resolver.Sync is fed from, so normalizing here (rather
		// than, or as well as, in each platform backend) is what actually
		// stops two peers spelling the same domain with different case from
		// ever reaching an OS-level registration as if they were different
		// domains. See resolver.normalizeDomain's doc for why that matters
		// most on macOS and Windows.
		byDomain[strings.ToLower(d.domain)] = d.servers
	}
	ns.mu.RUnlock()

	// This node's own advertised forwards, so it resolves them too.
	if p := ns.advDNS.Load(); p != nil {
		for _, d := range *p {
			byDomain[strings.ToLower(d.domain)] = d.servers
		}
	}

	entries := make([]resolver.Entry, 0, len(byDomain))
	for domain, servers := range byDomain {
		entries = append(entries, resolver.Entry{Domain: domain, Servers: servers})
	}

	var configured []string
	if p := ns.searchDomains.Load(); p != nil {
		configured = *p
	}
	searchDomains := effectiveSearchDomains(configured, byDomain, ns.spec.SearchLearned)

	sig := dnsSignature(entries, searchDomains)
	if sig == ns.lastDNSSig {
		return // nothing changed; avoid an unnecessary OS resolver write
	}
	tag := ns.spec.DNSTag
	if tag == "" {
		tag = fmt.Sprintf("%016x", ns.spec.ID)
	}
	if err := resolver.Sync(tag, iface, entries, searchDomains); err != nil {
		if errors.Is(err, resolver.ErrSearchDomainsUnsupported) {
			// Routing domains still applied; only the search-suffix part hit
			// a permanent, structural platform limitation that no retry will
			// ever clear (see ErrSearchDomainsUnsupported's doc comment).
			// Log it — reaching this branch at all means sig just changed
			// from ns.lastDNSSig (the debounce above already returned early
			// otherwise) — then record sig as synced anyway. Without that,
			// sig never matches ns.lastDNSSig, the debounce above never
			// holds, and this re-fires, re-runs Sync, and re-logs the
			// identical warning every maintenance tick forever, for a
			// condition that isn't going to change until the config itself
			// does. Recording it here means it naturally logs again if the
			// domains involved change later, exactly like the success path.
			e.log.Warnf("mesh: dns sync (net %x, iface %s): %v", ns.spec.ID, iface, err)
			ns.lastDNSSig = sig
			return
		}
		// Deliberately do NOT record sig here: unlike the structural case above,
		// this failure may well be transient (systemd-resolved restarting, a link
		// not up yet), so the next tick should retry rather than treat the failed
		// state as synced.
		//
		// Retrying every tick used to mean re-logging the identical warning every
		// tick, forever, for a condition an operator has to fix by hand — e.g. on
		// RHEL/Rocky/Alma/CentOS, where systemd-resolved isn't enabled by default,
		// so every sync fails with "The name is not activatable" and nothing about
		// that changes until someone enables it. That's noise that buries the very
		// first (and only informative) instance. So: shout once, in full, then say
		// nothing further until the error actually changes or clears — the retry
		// itself continues silently either way, so a fix applied later still takes
		// effect on the next tick without needing a restart.
		if msg := err.Error(); msg != ns.lastDNSErr {
			e.log.Warnf("mesh: dns sync (net %x, iface %s): %v", ns.spec.ID, iface, err)
			ns.lastDNSErr = msg
		} else {
			e.log.Debugf("mesh: dns sync (net %x, iface %s): still failing, unchanged: %v", ns.spec.ID, iface, err)
		}
		return
	}
	// Cleared: a later failure — even an identical one — is news again, and worth
	// logging in full.
	ns.lastDNSErr = ""
	ns.lastDNSSig = sig
	e.log.Debugf("mesh: os resolver updated with %d conditional-forward domain(s) and %d search domain(s) (net %x)",
		len(entries), len(searchDomains), ns.spec.ID)
}

// effectiveSearchDomains computes the search-suffix list syncDNS hands to
// internal/resolver: configured always applies (this node's own AdvDNS
// domains — see NetSpec.SearchDomains), and, when learned is true, every
// domain currently in byDomain is added too. byDomain is this node's full
// accepted conditional-forward set — its own AdvDNS plus every
// peer-advertised forward it has accepted (learnedDNS, already filtered
// through dnsRejected by the caller) — so learned=true (the default; see
// config.DNSSync.DisableSearchDomains for the opt-out) is what lets a
// DNSSync-only consumer, one that routes a peer's forward but advertises
// none of its own, complete bare queries against it too, not just
// fully-qualified ones. Kept free of *netState/*Engine, like
// linuxRoutingArgs/searchDomainArgs in internal/resolver, so it's testable
// without an engine or a resolvectl binary; duplicates between configured
// and byDomain are harmless, since resolver.searchDomainArgs dedupes
// downstream.
func effectiveSearchDomains(configured []string, byDomain map[string][]netip.Addr, learned bool) []string {
	if !learned || len(byDomain) == 0 {
		return configured
	}
	out := make([]string, 0, len(configured)+len(byDomain))
	out = append(out, configured...)
	for d := range byDomain {
		out = append(out, d)
	}
	return out
}

func dnsSignature(entries []resolver.Entry, searchDomains []string) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		servers := make([]string, 0, len(e.Servers))
		for _, s := range e.Servers {
			servers = append(servers, s.String())
		}
		sort.Strings(servers)
		lines = append(lines, e.Domain+"|"+strings.Join(servers, ","))
	}
	sort.Strings(lines)
	search := append([]string(nil), searchDomains...)
	sort.Strings(search)
	return strings.Join(lines, "\n") + "\n--search--\n" + strings.Join(search, "\n")
}

// dnsRejected reports whether a peer-advertised forward for domain is refused
// by this node's local reject filter. Matching is case-insensitive, and by
// exact domain (not suffix), mirroring hostRejected.
func (ns *netState) dnsRejected(domain string) bool {
	p := ns.advDNSReject.Load()
	if p == nil || len(*p) == 0 {
		return false
	}
	domain = strings.ToLower(domain)
	for _, r := range *p {
		if r == domain {
			return true
		}
	}
	return false
}
