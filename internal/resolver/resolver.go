// Package resolver registers gravinet-owned conditional-forwarding domains
// with the OS's native split-DNS mechanism, so fully-qualified queries under a
// mesh domain are routed to mesh DNS servers without touching the machine's
// default resolver configuration. It is the DNS analog of internal/hosts:
// where hosts writes a delimited block into the hosts file for plain
// name -> address mappings, resolver registers routing (not search) domains so
// only queries under those domains are affected — bare hostnames and every
// other domain resolve exactly as before.
//
// Because every supported OS checks hosts before DNS, any name internal/hosts
// already answers never reaches this path at all; resolver only ever sees
// queries for names the hosts file doesn't have an entry for. The two are
// complementary layers, not alternatives.
//
// The underlying OS primitive differs by platform:
//   - Linux: systemd-resolved per-link routing domains (resolvectl). This is
//     the one platform where a link takes a single shared DNS server set, so
//     Sync unions the Servers across all Entries onto the network's tun
//     interface rather than keeping each domain's servers separate. A future
//     local forwarder process is the escape hatch if genuinely independent
//     per-domain server sets are ever needed on Linux; nothing here forecloses
//     adding one later.
//   - macOS: one /etc/resolver/<domain> file per domain, each with its own
//     nameserver list — true per-domain server sets, no compromise needed.
//   - Windows: one NRPT rule per domain (namespace), same per-domain fidelity
//     as macOS.
//   - FreeBSD: one local-unbound forward zone per domain, added/removed at
//     runtime via local-unbound-control, again with its own independent
//     server set per zone. Unlike the other three, the underlying mechanism
//     has no field to stash gravinet's ownership tag in, so this backend
//     keeps its own on-disk record of what it applied instead of reading
//     ownership back out of local-unbound's state; see resolver_freebsd.go's
//     package doc for why that's still restart-safe.
//   - OpenBSD: identical to FreeBSD in mechanism — one unbound forward zone
//     per domain via unbound-control (unbound ships in the OpenBSD base
//     system), with the same on-disk ownership record. The only difference is
//     operational: OpenBSD doesn't route the system through unbound by
//     default, so the installer's --unbound step makes unbound the resolver;
//     see resolver_openbsd.go's package doc.
//
// All platform implementations are restart-safe: they identify what they
// previously wrote via an embedded gravinet+tag marker and reconcile against
// it on every Sync, rather than relying on in-memory state that a daemon
// restart would lose (mirroring internal/hosts's read-existing-then-rewrite
// discipline, and internal/netfilter's "own table, exact teardown" discipline).
//
// Sync also takes an optional list of search domains — plain suffixes that
// should be tried automatically for an unqualified (single-label) query, as
// opposed to the routing domains above, which only affect fully-qualified
// queries under them. This is only genuinely interface-scoped (touching
// nothing but gravinet's own tun interface, the same isolation principle as
// routing domains) on two of the four platforms:
//   - Linux: systemd-resolved's per-link domain list already accepts both
//     kinds side by side — a "~"-prefixed entry is a routing domain, a bare
//     one is a search domain. Since resolvectl domain replaces a link's whole
//     domain list in one call, Sync always combines both kinds into a single
//     invocation rather than risking one clobbering the other.
//   - Windows: the per-adapter connection-specific DNS suffix, set via
//     PowerShell's Set-DnsClient, is scoped to gravinet's own adapter the
//     same way NRPT rules are. It's a single value, not a list, so only the
//     first configured search domain is applied; Sync returns an error naming
//     the rest as unapplied rather than silently dropping them.
//   - macOS and FreeBSD have no equivalent interface-scoped primitive: a
//     search domain there is a property of a network *service*'s
//     configuration (macOS: networksetup -setsearchdomains; FreeBSD/local-
//     unbound: not a resolver-daemon concept at all, it's a stub-resolver/
//     resolv.conf setting), not of gravinet's own tun interface — applying it
//     there would mean reaching into the machine's primary network config,
//     a materially more invasive change than anything else this package does.
//     Sync returns a clear, distinct error there instead of doing that
//     silently or pretending to support it.
package resolver

import (
	"errors"
	"net/netip"
	"sort"
	"strings"
)

// ErrSearchDomainsUnsupported is wrapped into the error Sync returns when
// some or all of searchDomains couldn't be applied for a permanent,
// structural platform reason (no per-interface mechanism at all on macOS/
// FreeBSD, only one suffix per adapter on Windows) — never because of a
// transient failure like a missing binary or a permission error. Routing
// domains (entries) still applied successfully in every case that returns
// this: only the search-suffix part is affected.
//
// This distinction matters to callers with their own retry/debounce logic
// (see internal/mesh/dnssync.go): a transient error is worth retrying and
// re-logging on the next sync, since it might clear on its own or after an
// operator fixes something. This one never will — the same searchDomains
// input will produce the same error every time, forever, on this platform.
// Treating it identically to a transient failure means a caller that skips
// its own debounce bookkeeping on any non-nil error retries and re-logs this
// one on every cycle indefinitely, for a condition no retry can ever fix.
// errors.Is against this value lets a caller apply routing-domain-succeeded
// bookkeeping anyway and log the limitation without that.
var ErrSearchDomainsUnsupported = errors.New("resolver: search domains not fully supported on this platform")

// Entry is one conditional-forwarding rule to apply: queries under Domain
// should be routed to Servers. Domain has no leading "." or "~" — platform
// code adds whatever the underlying mechanism requires.
type Entry struct {
	Domain  string
	Servers []netip.Addr
}

// normalizeDomain trims a trailing root dot and lowercases a domain name.
// Domain names are case-insensitive by definition (RFC 4343), but nothing
// upstream of here enforced that — two nodes (or the same admin, at
// different times) spelling the same domain with different case were
// otherwise treated as genuinely different entries. That's mostly cosmetic on
// Linux (Sync just unions everything into one routing-domain set), but
// silently destructive on macOS and Windows, whose per-domain mechanisms key
// off this string directly: macOS's default APFS and Windows' registry are
// both case-insensitive-but-preserving, so "Example.com" and "example.com"
// resolve to the very same underlying /etc/resolver file or NRPT value, and
// whichever synced most recently silently wins — no error, no indication
// either side's admin mismatched anything, just a live state that
// intermittently doesn't match what either config says.
func normalizeDomain(d string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
}

// linuxRoutingArgs computes the resolvectl arguments for entries: domains
// prefixed "~" (systemd-resolved's routing-domain marker) and the union of
// all Servers, both sorted for a stable, diff-friendly command line. Kept
// platform-neutral (no os/exec) so it's testable without a Linux host or a
// resolvectl binary, mirroring how internal/netfilter keeps its rule
// generators pure and only the exec/apply step platform-locked.
func linuxRoutingArgs(entries []Entry) (domains, servers []string) {
	domainSet := map[string]struct{}{}
	serverSet := map[string]struct{}{}
	for _, e := range entries {
		d := normalizeDomain(e.Domain)
		if d == "" {
			continue
		}
		domainSet["~"+d] = struct{}{}
		for _, s := range e.Servers {
			if s.IsValid() {
				serverSet[s.String()] = struct{}{}
			}
		}
	}
	domains = make([]string, 0, len(domainSet))
	for d := range domainSet {
		domains = append(domains, d)
	}
	servers = make([]string, 0, len(serverSet))
	for s := range serverSet {
		servers = append(servers, s)
	}
	sort.Strings(domains)
	sort.Strings(servers)
	return domains, servers
}

// searchDomainArgs normalizes, deduplicates, and sorts a list of search
// domains — shared by every platform backend that applies them (Linux via
// resolvectl, Windows via the per-adapter connection-specific suffix). Kept
// platform-neutral for the same testability reason as linuxRoutingArgs.
func searchDomainArgs(domains []string) []string {
	set := map[string]struct{}{}
	for _, d := range domains {
		d = normalizeDomain(d)
		if d != "" {
			set[d] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
