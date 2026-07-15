package mesh

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"time"
)

// ipResolver is the subset of *net.Resolver the firewall FQDN path uses.
type ipResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// netResolver is the DNS resolver used for firewall fqdn objects. It's a package
// var (not a hardcoded call) so tests can stub name resolution deterministically.
var netResolver ipResolver = net.DefaultResolver

// fwFQDNRefresh is how often the firewall re-resolves its fqdn-kind address
// objects. A change in the resolved set triggers a live rulebase recompile so
// rules that reference the object start matching the new addresses. This mirrors
// the DNS TTL horizon a name-based policy is expected to track without demanding
// a restart.
const fwFQDNRefresh = 60 * time.Second

// fwFQDNTimeout bounds a single object's DNS lookup so a slow resolver can't
// stall the maintenance tick.
const fwFQDNTimeout = 5 * time.Second

// resolveFirewallFQDN re-resolves every fqdn-kind object on a network's firewall
// and folds the results back in. Each object may list several names (comma- or
// space-separated per entry, or several entries); the union of all resolved
// addresses becomes the object's address set. Lookups that fail leave that
// object's previous set in place via setFQDN only being called with what we
// successfully resolved — a transient DNS failure shouldn't blank a policy.
//
// Wildcard entries ("*.example.com") are skipped here — there is no DNS
// query that means "every subdomain of example.com", so attempting one
// would just be a guaranteed-failed lookup every tick. Those are populated
// instead by the passive sniffer in firewall_dns_sniff.go, which observes
// real traffic rather than asking a question DNS has no way to answer. An
// object naming both a literal and a wildcard entry gets contributions from
// both paths independently (see mergedFQDNLocked); this function only ever
// touches the literal ones.
func (e *Engine) resolveFirewallFQDN(ns *netState) {
	if ns == nil || ns.fw == nil {
		return
	}
	objs := ns.fw.fqdnObjects()
	if len(objs) == 0 {
		return
	}
	for _, o := range objs {
		names := literalFQDNNames(o.Addresses)
		if len(names) == 0 {
			continue
		}
		prefixes, ok := resolveNames(names)
		if !ok {
			// Total failure across all names: keep the last known set rather than
			// clearing the object (which would widen or void the referencing rule).
			continue
		}
		if ns.fw.setFQDN(o.Name, prefixes) {
			e.log.Debugf("mesh: firewall fqdn %q now resolves to %d address(es)", o.Name, len(prefixes))
		}
	}
}

// refreshFirewallFQDN resolves FQDN objects for one network immediately, used on
// a config reload so a freshly added name-based rule doesn't wait a full tick.
func (e *Engine) refreshFirewallFQDN(networkID uint64) {
	if ns := e.network(networkID); ns != nil {
		e.resolveFirewallFQDN(ns)
		ns.lastFWFQDN = time.Now()
	}
}

// fqdnNames splits an object's Addresses entries into individual domain names,
// accepting comma- or whitespace-separated names within an entry. Includes
// wildcard entries ("*.example.com") as-is — callers that only want literal
// names to resolve via DNS should use literalFQDNNames instead; this
// function is also used to build the wildcard-pattern snapshot the sniffer
// reads (refreshWildcardPatternsLocked), which needs to see both kinds.
func fqdnNames(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		for _, f := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
	}
	return out
}

// literalFQDNNames is fqdnNames filtered down to non-wildcard entries — what
// the periodic DNS-poll resolver should actually attempt to look up.
func literalFQDNNames(addrs []string) []string {
	all := fqdnNames(addrs)
	out := make([]string, 0, len(all))
	for _, n := range all {
		if !isWildcardFQDN(n) {
			out = append(out, n)
		}
	}
	return out
}

// resolveNames resolves a set of domain names to a de-duplicated, sorted list of
// prefixes (each address as a full-length prefix). ok is false only when every
// name failed to resolve, so the caller can distinguish "empty on purpose" from
// "DNS is down".
func resolveNames(names []string) (pfx []netip.Prefix, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), fwFQDNTimeout)
	defer cancel()
	seen := map[netip.Addr]bool{}
	anyOK := false
	for _, n := range names {
		addrs, err := netResolver.LookupNetIP(ctx, "ip", n)
		if err != nil {
			continue
		}
		anyOK = true
		for _, a := range addrs {
			a = a.Unmap()
			if seen[a] {
				continue
			}
			seen[a] = true
			pfx = append(pfx, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	sortPrefixes(pfx)
	return pfx, anyOK
}

func sortPrefixes(p []netip.Prefix) {
	// Simple insertion sort by string form; sets are tiny and this keeps the
	// resolved order stable so prefixEqual doesn't churn on reordering.
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].String() < p[j-1].String(); j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}
