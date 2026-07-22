package mesh

import (
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gravinet/internal/logx"
)

// Firewall: an ordered, per-network rulebase evaluated top-to-bottom, first
// match wins. The default policy is ALLOW — an empty rulebase, or a packet that
// matches no rule, is permitted. Rules are managed live (add/reorder/delete and
// cut/copy/paste) over the control plane.
//
// Reads (the data path) are lock-free: the active ruleset is an immutable
// snapshot swapped atomically on mutation. A mutex serialises mutations and the
// clipboard.

type fwAction uint8

const (
	fwAllow fwAction = iota
	fwDeny
)

type fwDir uint8

const (
	fwBoth fwDir = iota // applies to both directions
	fwIn                // mesh -> local TUN
	fwOut               // local TUN -> mesh
)

// fwLeg is one protocol/port "leg" of a rule's service match. A named service
// (e.g. DNS) may contribute several legs (udp/53 and tcp/53); a raw proto+ports
// rule contributes exactly one. proto 0 means any; a zero port bound means any.
type fwLeg struct {
	proto    uint8
	sportMin uint16
	sportMax uint16
	dportMin uint16
	dportMax uint16
}

func (l fwLeg) match(proto, sp, dp uint16) bool {
	if l.proto != 0 && uint16(l.proto) != proto {
		return false
	}
	if (l.sportMin != 0 || l.sportMax != 0) && (sp < l.sportMin || sp > l.sportMax) {
		return false
	}
	if (l.dportMin != 0 || l.dportMax != 0) && (dp < l.dportMin || dp > l.dportMax) {
		return false
	}
	return true
}

// ruleCounters holds a rule's hit tally. It's a separate heap allocation (not an
// inline field) so fwRule stays copyable by value — cloneRule does `c := *r`,
// which would trip the copylocks vet check on an inlined atomic. A pointer also
// lets the counter survive rule reordering and catalog recompiles: the same
// *ruleCounters is carried onto the freshly compiled rule, so the tally keeps
// climbing across an object edit or an FQDN refresh instead of resetting.
type ruleCounters struct {
	pkts  atomic.Uint64
	bytes atomic.Uint64
	// log-rate gate: lastLogNanos is the unix-nano time of the last emitted log
	// line for this rule; suppressed counts matches dropped since then. Keeping
	// the gate on the (persistent) counters means the rate limit survives a
	// recompile the same way the tally does.
	lastLogNanos atomic.Int64
	suppressed   atomic.Uint64
}

// logGate reports whether a log line should be emitted now for a matching rule,
// and if so how many matches were suppressed since the last line. It emits at
// most once per interval per rule.
func (c *ruleCounters) logGate(now time.Time, interval time.Duration) (emit bool, suppressed uint64) {
	last := c.lastLogNanos.Load()
	if last != 0 && now.UnixNano()-last < int64(interval) {
		c.suppressed.Add(1)
		return false, 0
	}
	c.lastLogNanos.Store(now.UnixNano())
	return true, c.suppressed.Swap(0)
}

func newRuleCounters() *ruleCounters { return &ruleCounters{} }

// fwRule is the compiled, hot-path form of one firewall rule. spec is the
// authored form (raw text / object & service references) kept verbatim so the
// rule round-trips back to config and the UI exactly as written; everything
// else is derived from spec against the object/service catalogs by compileRule.
// An empty src/dst/legs slice means "any" for that dimension.
type fwRule struct {
	id       uint64
	spec     FirewallRule // authored form; source of truth for export/persist
	disabled bool         // when true, rule is stored but skipped during evaluation
	dir      fwDir
	// src/dst hold the resolved prefixes; anySrc/anyDst record whether the
	// authored field was a wildcard ("any"/empty). The distinction matters for
	// object references: a rule constrained to an object that currently resolves
	// to nothing (an unresolved FQDN, an empty group) must match NOTHING, not
	// everything — so we consult the any-flag, not merely len(set)==0.
	src, dst             []netip.Prefix
	anySrc, anyDst       bool
	srcNegate, dstNegate bool    // flip src/dst matching: "anything except this" (see FirewallRule doc)
	legs                 []fwLeg // resolved service legs
	anyLeg               bool    // no proto, ports, or services named: match any service
	legsNegate           bool    // flip service matching: "any service except this" (see FirewallRule doc)
	action               fwAction
	logMatch             bool // emit a (rate-limited) log line when this rule matches
	notes                string
	cnt                  *ruleCounters
}

func anyContains(set []netip.Prefix, a netip.Addr) bool {
	for _, p := range set {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (r *fwRule) match(dir fwDir, src, dst netip.Addr, proto uint8, sp, dp uint16) bool {
	if r.dir != fwBoth && r.dir != dir {
		return false
	}
	srcOK := r.anySrc || anyContains(r.src, src)
	if r.srcNegate {
		srcOK = !srcOK
	}
	if !srcOK {
		return false
	}
	dstOK := r.anyDst || anyContains(r.dst, dst)
	if r.dstNegate {
		dstOK = !dstOK
	}
	if !dstOK {
		return false
	}
	svcOK := r.anyLeg
	if !svcOK {
		for _, l := range r.legs {
			if l.match(uint16(proto), sp, dp) {
				svcOK = true
				break
			}
		}
	}
	if r.legsNegate {
		svcOK = !svcOK
	}
	if !svcOK {
		return false
	}
	return true
}

type firewall struct {
	mu        sync.Mutex
	snap      atomic.Pointer[[]*fwRule]
	clipboard []*fwRule
	nextID    uint64

	enabled  atomic.Bool  // false => allow-all (filter off); toggled live, no restart
	stateful atomic.Bool  // any rule matches on connection state
	ct       *fwConntrack // connection tracking (used only when stateful)

	// mgmtPort is this node's web-admin port; an exemption with Mgmt set matches
	// it. Written once at build time (before the netState is published).
	mgmtPort uint16

	// exempts is the always-allowed allowlist, swapped atomically so edits apply
	// live without blocking the data path. Nil/empty means "filter everything per
	// the rulebase" (no exemptions).
	exempts atomic.Pointer[[]FirewallExempt]

	// Object/service catalog and FQDN state. objects/services are the authored
	// catalog; fqdn holds the current resolved address set for fqdn-kind objects
	// (keyed by lowercased object name), populated by the periodic literal-name
	// resolver (firewall_fqdn.go); fqdnWild is the same shape but populated by
	// the passive DNS-response sniffer (firewall_dns_sniff.go) for wildcard
	// ("*.example.com") entries, which can't be resolved by a direct lookup —
	// only observed as real traffic happens to name them. cat is the compiled
	// lookup built from objects/services and the *union* of fqdn and fqdnWild
	// (see rebuildCatalogLocked/mergedFQDNLocked) — a single object may mix
	// literal and wildcard entries and gets contributions from both paths. All
	// are guarded by mu (rebuilt only on mutation, never on the data path), and
	// every rule is recompiled against cat whenever any changes.
	objects  []FirewallObject
	services []FirewallService
	fqdn     map[string][]netip.Prefix
	fqdnWild map[string][]netip.Prefix
	cat      *fwCatalog

	// wcCache holds the sniffer's raw, per-address TTL bookkeeping for
	// wildcard fqdn entries — see firewall_dns_sniff.go. It's deliberately
	// its own mutex, separate from mu: the hot packet path only ever touches
	// this cache (a cheap map update keyed by object name), never mu or
	// rebuildCatalogLocked directly. Only the periodic sweep (sweepWildcardFQDN,
	// driven off the maintenance tick in control.go) promotes cache contents
	// into fqdnWild and triggers a recompile.
	wcCache *wildcardFQDNCache

	// wildcardPatterns is a lock-free snapshot of {object name -> its wildcard
	// address entries}, refreshed by rebuildCatalogLocked whenever objects
	// change. The packet path reads this instead of taking mu, matching the
	// rest of the data path's lock-free-reads discipline; nil or empty means
	// "no wildcard fqdn objects configured", the common case, checked with a
	// single pointer load before any packet is even glanced at.
	wildcardPatterns atomic.Pointer[map[string][]string]

	// log/logInterval drive per-rule match logging (rule.logMatch). nil log =>
	// logging silently disabled. logInterval bounds the per-rule log rate.
	log         *logx.Logger
	logInterval time.Duration
}

func newFirewall(rules []*fwRule) *firewall {
	f := &firewall{ct: newConntrack()}
	f.enabled.Store(true) // a freshly built rulebase enforces; callers toggle as needed
	f.fqdn = map[string][]netip.Prefix{}
	f.wcCache = &wildcardFQDNCache{}
	f.cat = emptyCatalog()
	f.logInterval = defaultFWLogInterval
	for _, r := range rules {
		f.nextID++
		r.id = f.nextID
		if r.cnt == nil {
			r.cnt = newRuleCounters()
		}
	}
	f.store(rules)
	return f
}

// defaultFWLogInterval bounds how often a single logging rule emits a line, so a
// match on a high-rate flow can't flood the log; suppressed matches are counted
// and reported on the next emitted line.
const defaultFWLogInterval = 2 * time.Second

// setLogger wires the match-logging sink. Called once at network build time.
func (f *firewall) setLogger(l *logx.Logger) { f.log = l }

// setCatalog swaps in a new object/service catalog and recompiles every rule
// against it, live. Existing rules keep their id and hit counters.
func (f *firewall) setCatalog(objs []FirewallObject, svcs []FirewallService) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects = append([]FirewallObject(nil), objs...)
	f.services = append([]FirewallService(nil), svcs...)
	f.rebuildCatalogLocked()
	f.store(f.current()) // recompiles from each rule's spec against the new catalog
}

// setFQDN updates the resolved address set for one fqdn object (keyed by
// lowercased name) and, if it changed, rebuilds the catalog and recompiles.
// Returns whether anything changed. This is the literal-name (periodic
// DNS-poll) path; see setFQDNWildcard for the sniffer-driven counterpart
// that covers "*.example.com"-style entries the same object may also carry.
func (f *firewall) setFQDN(name string, prefixes []netip.Prefix) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	f.mu.Lock()
	defer f.mu.Unlock()
	if prefixEqual(f.fqdn[key], prefixes) {
		return false
	}
	if f.fqdn == nil {
		f.fqdn = map[string][]netip.Prefix{}
	}
	f.fqdn[key] = append([]netip.Prefix(nil), prefixes...)
	f.rebuildCatalogLocked()
	f.store(f.current())
	return true
}

// setFQDNWildcard is the sniffer-driven counterpart to setFQDN: it replaces
// one wildcard fqdn object's currently-live (unexpired) sniffed address set
// and recompiles if it changed. Kept in its own map (fqdnWild) rather than
// writing into fqdn directly so the two independent writers — the periodic
// literal-name poll and the passive sniffer's TTL sweep — never race to
// overwrite each other's contribution to an object that mixes both entry
// kinds; rebuildCatalogLocked unions the two per object.
func (f *firewall) setFQDNWildcard(name string, prefixes []netip.Prefix) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	f.mu.Lock()
	defer f.mu.Unlock()
	if prefixEqual(f.fqdnWild[key], prefixes) {
		return false
	}
	if f.fqdnWild == nil {
		f.fqdnWild = map[string][]netip.Prefix{}
	}
	if len(prefixes) == 0 {
		delete(f.fqdnWild, key) // nothing sniffed (yet, or anymore) for this object
	} else {
		f.fqdnWild[key] = append([]netip.Prefix(nil), prefixes...)
	}
	f.rebuildCatalogLocked()
	f.store(f.current())
	return true
}

// fqdnNames returns the lowercased names of all fqdn-kind objects, so the
// resolver knows what to look up.
func (f *firewall) fqdnObjects() []FirewallObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FirewallObject
	for _, o := range f.objects {
		if strings.EqualFold(strings.TrimSpace(o.Kind), "fqdn") {
			out = append(out, o)
		}
	}
	return out
}

// mergedFQDNLocked returns the address sets buildCatalog should resolve
// fqdn-kind objects against: fqdn (literal, poll-resolved) unioned per
// object name with fqdnWild (wildcard, sniffer-resolved). Must be called
// with mu held. The common case — no wildcard fqdn objects configured, so
// fqdnWild is empty — returns fqdn directly with no copy.
func (f *firewall) mergedFQDNLocked() map[string][]netip.Prefix {
	if len(f.fqdnWild) == 0 {
		return f.fqdn
	}
	merged := make(map[string][]netip.Prefix, len(f.fqdn)+len(f.fqdnWild))
	for k, v := range f.fqdn {
		merged[k] = v
	}
	for k, wv := range f.fqdnWild {
		if len(wv) == 0 {
			continue
		}
		merged[k] = mergePrefixes(merged[k], wv)
	}
	return merged
}

// mergePrefixes returns the sorted, de-duplicated union of a and b. Both
// inputs are assumed already free of internal duplicates (true of every
// caller here — resolveNames and the wildcard sweep each already
// de-duplicate their own output); only cross-set duplicates need catching.
func mergePrefixes(a, b []netip.Prefix) []netip.Prefix {
	if len(a) == 0 {
		return append([]netip.Prefix(nil), b...)
	}
	if len(b) == 0 {
		return append([]netip.Prefix(nil), a...)
	}
	seen := make(map[netip.Prefix]bool, len(a)+len(b))
	out := make([]netip.Prefix, 0, len(a)+len(b))
	for _, p := range a {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range b {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sortPrefixes(out)
	return out
}

// refreshWildcardPatternsLocked recomputes the lock-free wildcard-pattern
// snapshot the packet path reads (see wildcardPatterns) from the current
// object list. Must be called with mu held; rebuildCatalogLocked does this
// on every catalog rebuild, i.e. every object edit — cheap and infrequent
// relative to the packet path that reads the result.
func (f *firewall) refreshWildcardPatternsLocked() {
	var pats map[string][]string
	for _, o := range f.objects {
		if !strings.EqualFold(strings.TrimSpace(o.Kind), "fqdn") {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(o.Name))
		if key == "" {
			continue
		}
		for _, a := range fqdnNames(o.Addresses) {
			if isWildcardFQDN(a) {
				if pats == nil {
					pats = map[string][]string{}
				}
				pats[key] = append(pats[key], strings.ToLower(strings.TrimSpace(a)))
			}
		}
	}
	f.wildcardPatterns.Store(&pats)
}

func (f *firewall) rebuildCatalogLocked() {
	f.refreshWildcardPatternsLocked()
	f.cat = buildCatalog(f.objects, f.services, f.mergedFQDNLocked())
}

// compile resolves an authored rule against the live catalog (mutation path).
func (f *firewall) compile(fr FirewallRule) (*fwRule, error) {
	f.mu.Lock()
	cat := f.cat
	f.mu.Unlock()
	return compileRule(fr, cat)
}

func prefixEqual(a, b []netip.Prefix) bool {
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

// setMgmtPort records this node's web-admin port so a Mgmt exemption matches it.
// Called once at network build time.
func (f *firewall) setMgmtPort(p uint16) { f.mgmtPort = p }

// getObjects / getServices return copies of the authored catalogs for listing.
func (f *firewall) getObjects() []FirewallObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FirewallObject(nil), f.objects...)
}

func (f *firewall) getServices() []FirewallService {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FirewallService(nil), f.services...)
}

// setObjects / setServices replace one catalog half and recompile every rule
// against the rebuilt catalog, live. The other half is left untouched.
func (f *firewall) setObjects(objs []FirewallObject) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects = append([]FirewallObject(nil), objs...)
	f.rebuildCatalogLocked()
	f.store(f.current())
}

func (f *firewall) setServices(svcs []FirewallService) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services = append([]FirewallService(nil), svcs...)
	f.rebuildCatalogLocked()
	f.store(f.current())
}

// resetCounters zeroes the hit tallies (and the log-rate gate) of the named
// rules; an empty id list resets all of them.
func (f *firewall) resetCounters(ids []uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	all := len(ids) == 0
	for _, r := range f.current() {
		if (all || want[r.id]) && r.cnt != nil {
			r.cnt.pkts.Store(0)
			r.cnt.bytes.Store(0)
			r.cnt.suppressed.Store(0)
			r.cnt.lastLogNanos.Store(0)
		}
	}
}

// FirewallExempt is the spec form of one always-allowed traffic class. Proto is
// the IP protocol number (0 = any), already resolved from the operator's token.
// A packet matches when the protocol matches and the port (compared against both
// the source and destination port) matches; a zero Port matches any port, which
// is what port-less protocols like OSPF want. When Mgmt is set the port is this
// node's live web-admin port instead of Port.
type FirewallExempt struct {
	Name  string
	Proto uint8
	Port  uint16
	Mgmt  bool
}

// setExempts swaps in a new allowlist atomically (live, no restart). A copy is
// stored so the caller's slice can't mutate the active set underfoot.
func (f *firewall) setExempts(es []FirewallExempt) {
	cp := append([]FirewallExempt(nil), es...)
	f.exempts.Store(&cp)
}

// ExemptInfo is the reporting form of an exemption (with the management port
// resolved and the protocol rendered for display).
type ExemptInfo struct {
	Name  string `json:"name"`
	Proto string `json:"proto"`
	Port  uint16 `json:"port"`
	Mgmt  bool   `json:"mgmt"`
}

// exemptProtoName renders an IP protocol number for display.
func exemptProtoName(p uint8) string {
	switch p {
	case 0:
		return "any"
	case 1:
		return "icmp"
	case 6:
		return "tcp"
	case 17:
		return "udp"
	case 89:
		return "ospf"
	default:
		return strconv.Itoa(int(p))
	}
}

// FirewallExemptsFor reports the always-allowed list currently applied to a
// network's firewall, for status/UI display.
func (e *Engine) FirewallExemptsFor(networkID uint64) []ExemptInfo {
	ns := e.network(networkID)
	if ns == nil || ns.fw == nil {
		return nil
	}
	ep := ns.fw.exempts.Load()
	if ep == nil {
		return nil
	}
	out := make([]ExemptInfo, 0, len(*ep))
	for _, x := range *ep {
		out = append(out, ExemptInfo{Name: x.Name, Proto: exemptProtoName(x.Proto), Port: x.Port, Mgmt: x.Mgmt})
	}
	return out
}

// exempt reports whether a packet carries traffic on the always-allowed list,
// which the rulebase can never override. This is what stops an operator from
// accidentally firewalling off their own remote management or the routing
// protocols (BGP/OSPF/RIP) that keep the overlay glued together — the list is
// operator-configurable and defaults to exactly those (see config).
//
// (The mesh's own control plane — route and hostname/peer advertisements — never
// reaches the firewall at all: it travels as encrypted control frames, not as
// overlay IP packets through the TUN, so it is structurally unblockable.)
func (f *firewall) exempt(proto uint8, sp, dp uint16) bool {
	ep := f.exempts.Load()
	if ep == nil {
		return false
	}
	for _, e := range *ep {
		if e.Proto != 0 && e.Proto != proto {
			continue
		}
		port := e.Port
		if e.Mgmt {
			if f.mgmtPort == 0 {
				continue // no admin port configured: nothing to match
			}
			port = f.mgmtPort
		}
		if port == 0 || sp == port || dp == port {
			return true
		}
	}
	return false
}

// setEnabled turns filtering on or off live. When off, allow() short-circuits to
// allow-all, so enable/disable needs no restart and races nothing on the hot path.
func (f *firewall) setEnabled(on bool) {
	f.enabled.Store(on)
	// Toggling the filter re-evaluates everything: drop tracked state so flows
	// established under the old state don't bypass the rules after the toggle.
	if f.ct != nil {
		f.ct.reset()
	}
}

func (f *firewall) store(rules []*fwRule) {
	// Recompile each rule from its authored spec against the current catalog, so
	// object/service edits and FQDN refreshes take effect here in one place. The
	// rule's identity and hit counters carry across the recompile; if a rule no
	// longer compiles (e.g. it names an object that was just deleted) we keep its
	// previous compiled form rather than dropping or widening it, and note it.
	cp := make([]*fwRule, 0, len(rules))
	for _, r := range rules {
		if r.spec.Action == "" && len(r.spec.Src) == 0 && len(r.spec.Dst) == 0 {
			// No authored spec to recompile from (constructed directly): keep as-is.
			cp = append(cp, r)
			continue
		}
		nc, err := compileRule(r.spec, f.cat)
		if err != nil {
			if f.log != nil {
				f.log.Infof("mesh: firewall rule %d kept previous form (recompile failed: %v)", r.id, err)
			}
			cp = append(cp, r)
			continue
		}
		nc.id = r.id
		if r.cnt != nil {
			nc.cnt = r.cnt // preserve the hit tally across the recompile
		}
		cp = append(cp, nc)
	}
	f.snap.Store(&cp)
	// Connection tracking is only meaningful when some rule can deny: it exists
	// so replies to already-allowed flows aren't blocked by a deny rule.
	stateful := false
	for _, r := range cp {
		if r.action == fwDeny {
			stateful = true
			break
		}
	}
	f.stateful.Store(stateful)
	// A rulebase change must take effect on EXISTING connections too, not just
	// new ones. Without this, a flow already marked "established" in the conntrack
	// table keeps being allowed regardless of a newly added deny rule — which is
	// exactly why live edits looked like they needed a service restart. Dropping
	// the tracked state forces every flow to be re-evaluated against the new
	// rules on its next packet.
	if f.ct != nil {
		f.ct.reset()
	}
}

func (f *firewall) current() []*fwRule {
	if p := f.snap.Load(); p != nil {
		return *p
	}
	return nil
}

// allow evaluates a packet for the given direction. The default policy is allow.
// The firewall is stateful by default: once a flow is permitted, return traffic
// for it is allowed automatically, so rules effectively govern new connections
// (e.g. a "deny in" rule blocks unsolicited inbound but not replies to
// connections this node initiated). Tracking only runs when a deny rule exists.
func (f *firewall) allow(dir fwDir, pkt []byte) bool {
	if f == nil || !f.enabled.Load() {
		return true
	}
	// Passive DNS-response sniffing for wildcard fqdn objects (see
	// firewall_dns_sniff.go) runs before the rules-empty check below: an
	// object's address set should already be warm by the time a rule
	// referencing it is added, not start from empty. It never affects the
	// allow/deny decision anywhere in this function and is cheap to skip —
	// a proto/port comparison — for every packet that isn't a DNS response,
	// which is nearly all of them.
	proto, sp, dp, _, l4ok := parseL4(pkt)
	if l4ok && sp == 53 && (proto == 17 || proto == 6) {
		f.observeDNSResponse(pkt, proto)
	}
	rules := f.current()
	if len(rules) == 0 {
		return true
	}
	src, dst, ok := parseAddrs(pkt)
	if !ok {
		return true // unparseable: fail open, matching the default-allow policy
	}

	// Control/management traffic is never filtered — see exempt().
	if f.exempt(proto, sp, dp) {
		return true
	}

	stateful := f.stateful.Load()
	if stateful {
		// Return traffic for an already-established flow is allowed regardless
		// of the rules, and the flow is refreshed.
		if f.ct.readState(proto, src, sp, dst, dp) == ctEstablished {
			f.ct.track(proto, src, sp, dst, dp, time.Now())
			return true
		}
	}

	allowed := true // default policy
	for _, r := range rules {
		if r.disabled {
			continue
		}
		if r.match(dir, src, dst, proto, sp, dp) {
			allowed = r.action == fwAllow
			f.recordHit(r, len(pkt), dir, src, dst, proto, sp, dp)
			break
		}
	}
	if stateful && allowed {
		f.ct.track(proto, src, sp, dst, dp, time.Now())
	}
	return allowed
}

// recordHit bumps a rule's packet/byte counters and, if the rule has logging
// enabled, emits a rate-limited one-line record of the match.
func (f *firewall) recordHit(r *fwRule, n int, dir fwDir, src, dst netip.Addr, proto uint8, sp, dp uint16) {
	if r.cnt != nil {
		r.cnt.pkts.Add(1)
		r.cnt.bytes.Add(uint64(n))
	}
	if !r.logMatch || f.log == nil || r.cnt == nil {
		return
	}
	iv := f.logInterval
	if iv <= 0 {
		iv = defaultFWLogInterval
	}
	emit, suppressed := r.cnt.logGate(time.Now(), iv)
	if !emit {
		return
	}
	extra := ""
	if suppressed > 0 {
		extra = fmt.Sprintf(" (+%d more suppressed)", suppressed)
	}
	f.log.Infof("mesh: firewall %s rule %d %s %s %s:%d -> %s:%d%s",
		r.action.string(), r.id, dirName(dir), protoName(proto), src, sp, dst, dp, extra)
}

func (f *firewall) sweepConntrack(now time.Time) {
	if f != nil && f.stateful.Load() {
		f.ct.sweep(now)
	}
}

// ---- management ----

// cloneRule copies a rule for the clipboard. A pasted rule is a new rule, so it
// gets its own fresh hit counters rather than sharing the source rule's tally.
func cloneRule(r *fwRule) *fwRule {
	c := *r
	c.cnt = newRuleCounters()
	return c.ptr()
}
func (r fwRule) ptr() *fwRule { return &r }

func insertAt(rules []*fwRule, r *fwRule, at int) []*fwRule {
	if at < 0 || at > len(rules) {
		at = len(rules)
	}
	out := make([]*fwRule, 0, len(rules)+1)
	out = append(out, rules[:at]...)
	out = append(out, r)
	out = append(out, rules[at:]...)
	return out
}

func insertSliceAt(rules, ins []*fwRule, at int) []*fwRule {
	if at < 0 || at > len(rules) {
		at = len(rules)
	}
	out := make([]*fwRule, 0, len(rules)+len(ins))
	out = append(out, rules[:at]...)
	out = append(out, ins...)
	out = append(out, rules[at:]...)
	return out
}

func indexOf(rules []*fwRule, id uint64) int {
	for i, r := range rules {
		if r.id == id {
			return i
		}
	}
	return -1
}

func (f *firewall) add(r *fwRule, at int) *fwRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	r.id = f.nextID
	f.store(insertAt(f.current(), r, at))
	return r
}

func (f *firewall) remove(ids []uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.removeLocked(ids)
}

func (f *firewall) removeLocked(ids []uint64) int {
	want := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	cur := f.current()
	nw := make([]*fwRule, 0, len(cur))
	removed := 0
	for _, r := range cur {
		if want[r.id] {
			removed++
		} else {
			nw = append(nw, r)
		}
	}
	f.store(nw)
	return removed
}

func (f *firewall) move(id uint64, to int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.current()
	idx := indexOf(cur, id)
	if idx < 0 {
		return false
	}
	r := cur[idx]
	without := make([]*fwRule, 0, len(cur)-1)
	without = append(without, cur[:idx]...)
	without = append(without, cur[idx+1:]...)
	f.store(insertAt(without, r, to))
	return true
}

func (f *firewall) copy(ids []uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.copyLocked(ids)
}

func (f *firewall) copyLocked(ids []uint64) int {
	want := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var clip []*fwRule
	for _, r := range f.current() { // preserve current order
		if want[r.id] {
			clip = append(clip, cloneRule(r))
		}
	}
	f.clipboard = clip
	return len(clip)
}

func (f *firewall) cut(ids []uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.copyLocked(ids)
	f.removeLocked(ids)
	return n
}

func (f *firewall) paste(at int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.clipboard) == 0 {
		return 0
	}
	ins := make([]*fwRule, 0, len(f.clipboard))
	for _, r := range f.clipboard {
		f.nextID++
		c := cloneRule(r)
		c.id = f.nextID
		ins = append(ins, c)
	}
	f.store(insertSliceAt(f.current(), ins, at))
	return len(ins)
}

func (f *firewall) clipboardLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.clipboard)
}

// ---- connection tracking (stateful firewalling) ----

type ctState uint8

const (
	ctNew ctState = iota
	ctEstablished
)

type flowKey struct {
	proto  uint8
	a, b   netip.Addr
	ap, bp uint16
}

type flowEntry struct {
	initiator   netip.Addr
	initPort    uint16
	established bool
	lastSeen    time.Time
}

type fwConntrack struct {
	mu    sync.Mutex
	flows map[flowKey]*flowEntry
}

func newConntrack() *fwConntrack { return &fwConntrack{flows: map[flowKey]*flowEntry{}} }

// reset drops all tracked flows, so subsequent packets are re-evaluated against
// the current rulebase. Called when the rules change or the firewall is toggled.
func (c *fwConntrack) reset() {
	c.mu.Lock()
	c.flows = map[flowKey]*flowEntry{}
	c.mu.Unlock()
}

const (
	ctTTLNew = 60 * time.Second
	ctTTLEst = 300 * time.Second
)

// canon builds a direction-independent key so a packet and its reply collide.
func canon(proto uint8, s netip.Addr, sp uint16, d netip.Addr, dp uint16) flowKey {
	if c := s.Compare(d); c < 0 || (c == 0 && sp <= dp) {
		return flowKey{proto, s, d, sp, dp}
	}
	return flowKey{proto, d, s, dp, sp}
}

// readState classifies a packet without mutating state.
func (c *fwConntrack) readState(proto uint8, src netip.Addr, sp uint16, dst netip.Addr, dp uint16) ctState {
	k := canon(proto, src, sp, dst, dp)
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.flows[k]
	if e == nil {
		return ctNew
	}
	if e.established {
		return ctEstablished
	}
	if src == e.initiator && sp == e.initPort {
		return ctNew // original direction, not yet confirmed by a reply
	}
	return ctEstablished // first reply confirms the connection
}

// track records an allowed flow, establishing it once a reply is seen.
func (c *fwConntrack) track(proto uint8, src netip.Addr, sp uint16, dst netip.Addr, dp uint16, now time.Time) {
	k := canon(proto, src, sp, dst, dp)
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.flows[k]
	if e == nil {
		c.flows[k] = &flowEntry{initiator: src, initPort: sp, lastSeen: now}
		return
	}
	if !e.established && (src != e.initiator || sp != e.initPort) {
		e.established = true
	}
	e.lastSeen = now
}

func (c *fwConntrack) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.flows {
		ttl := ctTTLNew
		if e.established {
			ttl = ctTTLEst
		}
		if now.Sub(e.lastSeen) > ttl {
			delete(c.flows, k)
		}
	}
}

// ---- IP address parsing ----

func parseAddrs(pkt []byte) (src, dst netip.Addr, ok bool) {
	if len(pkt) < 1 {
		return netip.Addr{}, netip.Addr{}, false
	}
	switch pkt[0] >> 4 {
	case 4:
		if len(pkt) < 20 {
			return netip.Addr{}, netip.Addr{}, false
		}
		var s, d [4]byte
		copy(s[:], pkt[12:16])
		copy(d[:], pkt[16:20])
		return netip.AddrFrom4(s), netip.AddrFrom4(d), true
	case 6:
		if len(pkt) < 40 {
			return netip.Addr{}, netip.Addr{}, false
		}
		var s, d [16]byte
		copy(s[:], pkt[8:24])
		copy(d[:], pkt[24:40])
		return netip.AddrFrom16(s), netip.AddrFrom16(d), true
	}
	return netip.Addr{}, netip.Addr{}, false
}

// ---- exported, config/control-friendly form ----

// FirewallRule is the JSON-friendly representation used by config and the
// control plane. Empty Src/Dst (or "any") match any address; zero ports match
// any port.
//
// Src and Dst may each be a raw CIDR/host ("10.0.0.0/24", "fd00::1"), the word
// "any"/"" for wildcard, or the name of an address object from the catalog
// (host/subnet/range/group/fqdn — see FirewallObject). Service matching may be
// given inline via Proto + the port bounds, and/or by naming one or more
// entries from the service catalog in Services; the two are unioned.
//
// SrcNegate, DstNegate, and ServicesNegate each flip what their dimension's
// match means: "matches the field as written" becomes "matches anything
// except what the field describes". Applied after Src/Dst/Proto+ports+
// Services are otherwise resolved exactly as normal — an object reference,
// "any", literal CIDRs, and named services all negate the same way,
// uniformly. Negating an "any" (or otherwise empty) dimension is accepted
// as written rather than special-cased: the universal set negated is the
// empty set, so that dimension then matches nothing — a rare thing to want
// on purpose, and the UI warns before saving a rule shaped that way, but
// it's not rejected outright, since "match nothing" is sometimes exactly
// how an operator temporarily neuters a rule without deleting it.
type FirewallRule struct {
	ID             uint64   `json:"id,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`  // true = rule is skipped; active by default
	Action         string   `json:"action"`              // allow|deny
	Direction      string   `json:"direction,omitempty"` // in|out|both (default both)
	Proto          string   `json:"proto,omitempty"`     // tcp|udp|icmp|any
	Src            string   `json:"src,omitempty"`       // CIDR, host, "any", or object name
	Dst            string   `json:"dst,omitempty"`
	SrcNegate      bool     `json:"src_negate,omitempty"` // match anything EXCEPT Src
	DstNegate      bool     `json:"dst_negate,omitempty"` // match anything EXCEPT Dst
	SrcPortMin     int      `json:"sport_min,omitempty"`
	SrcPortMax     int      `json:"sport_max,omitempty"`
	DstPortMin     int      `json:"dport_min,omitempty"`
	DstPortMax     int      `json:"dport_max,omitempty"`
	Services       []string `json:"services,omitempty"`        // named service-catalog entries (unioned with Proto/ports)
	ServicesNegate bool     `json:"services_negate,omitempty"` // match any service EXCEPT the Proto/ports+Services union
	Log            bool     `json:"log,omitempty"`             // log a line whenever this rule matches
	Notes          string   `json:"notes,omitempty"`
	// Packets/Bytes are export-only hit counters (ignored on input). They are the
	// running tally of traffic this rule has matched since it was created or the
	// counters were last reset.
	Packets uint64 `json:"packets,omitempty"`
	Bytes   uint64 `json:"bytes,omitempty"`
}

// FirewallObject is a named, reusable address object. kind is one of:
//
//	host   — one or more literal addresses (Addresses)
//	subnet — one or more CIDR prefixes (Addresses)
//	range  — a start-end address range, given as "a-b" entries in Addresses
//	fqdn   — one or more domain names (Addresses), resolved live (see fqdn.go)
//	group  — a bundle of other object names (Members), resolved recursively
//
// A rule references an object by putting its Name in Src or Dst. Defining the
// membership once and referencing it by name everywhere is the whole point:
// edit the object and every rule that names it updates at the next compile.
type FirewallObject struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`                // host|subnet|range|fqdn|group
	Addresses []string `json:"addresses,omitempty"` // literals/CIDRs/ranges/fqdns (non-group kinds)
	Members   []string `json:"members,omitempty"`   // member object names (group kind)
	Notes     string   `json:"notes,omitempty"`
}

// FirewallServicePort is one protocol/port leg of a named service.
type FirewallServicePort struct {
	Proto   string `json:"proto"`              // tcp|udp|icmp|<number>|any
	PortMin int    `json:"port_min,omitempty"` // destination port low bound (0 = any)
	PortMax int    `json:"port_max,omitempty"` // destination port high bound (0 = same as min, or any if min 0)
}

// FirewallService is a named, reusable bundle of protocol/port legs — e.g. a
// "DNS" service carrying udp/53 and tcp/53, referenced from a rule by name.
type FirewallService struct {
	Name  string                `json:"name"`
	Ports []FirewallServicePort `json:"ports"`
	Notes string                `json:"notes,omitempty"`
}

func parsePrefixField(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") {
		return netip.Prefix{}, nil
	}
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func protoNum(name string) uint8 {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "any":
		return 0
	case "tcp":
		return 6
	case "udp":
		return 17
	case "icmp":
		return 1
	case "icmpv6", "ipv6-icmp":
		return 58
	case "ospf":
		return 89
	default:
		// Accept a raw IP protocol number too (e.g. "47" for GRE).
		if n, err := strconv.Atoi(strings.TrimSpace(name)); err == nil && n > 0 && n < 256 {
			return uint8(n)
		}
		return 0
	}
}

func protoName(n uint8) string {
	switch n {
	case 6:
		return "tcp"
	case 17:
		return "udp"
	case 1:
		return "icmp"
	default:
		return "any"
	}
}

func dirFromString(s string) fwDir {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "in":
		return fwIn
	case "out":
		return fwOut
	default:
		return fwBoth
	}
}

func dirName(d fwDir) string {
	switch d {
	case fwIn:
		return "in"
	case fwOut:
		return "out"
	default:
		return "both"
	}
}

// ---- object & service catalogs ----

// fwCatalog is the compiled address-object and service catalog a rule resolves
// its Src/Dst/Services references against. Names are matched case-insensitively.
type fwCatalog struct {
	objects  map[string][]netip.Prefix
	services map[string][]fwLeg
	// names records which object names exist (even if they resolve to an empty
	// set, e.g. an FQDN not yet resolved) so a rule referencing one is treated as
	// "constrained to <possibly empty>", not "any".
	names map[string]bool
}

func emptyCatalog() *fwCatalog {
	return &fwCatalog{
		objects:  map[string][]netip.Prefix{},
		services: map[string][]fwLeg{},
		names:    map[string]bool{},
	}
}

// buildCatalog resolves the authored object/service lists into fast lookup maps.
// Groups are flattened recursively (with cycle protection); ranges are expanded
// to covering prefixes. fqdnResolved (keyed by lowercased object name) supplies
// the current address set for fqdn-kind objects; nil means "none resolved yet".
func buildCatalog(objs []FirewallObject, svcs []FirewallService, fqdnResolved map[string][]netip.Prefix) *fwCatalog {
	c := emptyCatalog()
	byName := make(map[string]FirewallObject, len(objs))
	for _, o := range objs {
		byName[strings.ToLower(strings.TrimSpace(o.Name))] = o
	}
	var resolve func(name string, seen map[string]bool) []netip.Prefix
	resolve = func(name string, seen map[string]bool) []netip.Prefix {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" || seen[key] {
			return nil
		}
		seen[key] = true
		o, ok := byName[key]
		if !ok {
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(o.Kind)) {
		case "group":
			var out []netip.Prefix
			for _, m := range o.Members {
				out = append(out, resolve(m, seen)...)
			}
			return out
		case "fqdn":
			return append([]netip.Prefix(nil), fqdnResolved[key]...)
		case "range":
			var out []netip.Prefix
			for _, a := range o.Addresses {
				out = append(out, parseRangeField(a)...)
			}
			return out
		default: // host, subnet
			var out []netip.Prefix
			for _, a := range o.Addresses {
				if p, err := parsePrefixField(a); err == nil && p.IsValid() {
					out = append(out, p)
				}
			}
			return out
		}
	}
	for _, o := range objs {
		key := strings.ToLower(strings.TrimSpace(o.Name))
		if key == "" {
			continue
		}
		c.names[key] = true
		c.objects[key] = resolve(o.Name, map[string]bool{})
	}
	for _, s := range svcs {
		key := strings.ToLower(strings.TrimSpace(s.Name))
		if key == "" {
			continue
		}
		c.services[key] = svcLegs(s)
	}
	return c
}

func svcLegs(s FirewallService) []fwLeg {
	var legs []fwLeg
	for _, p := range s.Ports {
		lo := uint16(p.PortMin)
		hi := uint16(p.PortMax)
		if hi == 0 {
			hi = lo // a single port, or "any" when both are 0
		}
		legs = append(legs, fwLeg{proto: protoNum(p.Proto), dportMin: lo, dportMax: hi})
	}
	return legs
}

// parseRangeField expands an "a-b" address range into a minimal set of covering
// CIDR prefixes. A bare address (no dash) is treated as a /32 or /128.
func parseRangeField(s string) []netip.Prefix {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '-')
	if i < 0 {
		if p, err := parsePrefixField(s); err == nil && p.IsValid() {
			return []netip.Prefix{p}
		}
		return nil
	}
	lo, err1 := netip.ParseAddr(strings.TrimSpace(s[:i]))
	hi, err2 := netip.ParseAddr(strings.TrimSpace(s[i+1:]))
	if err1 != nil || err2 != nil || lo.Is4() != hi.Is4() || lo.BitLen() != hi.BitLen() {
		return nil
	}
	return rangeToPrefixes(lo, hi)
}

// resolveAddrField turns a rule's Src/Dst text into (prefixes, isAny, error).
// Precedence: a catalog object name wins over literal parsing, then a raw
// CIDR/host, then the wildcard. An unknown, non-parseable token is an error so a
// typo surfaces at commit time rather than silently widening to "any".
func resolveAddrField(s string, cat *fwCatalog) (pfx []netip.Prefix, isAny bool, err error) {
	t := strings.TrimSpace(s)
	if t == "" || strings.EqualFold(t, "any") {
		return nil, true, nil
	}
	if cat != nil {
		if key := strings.ToLower(t); cat.names[key] {
			return cat.objects[key], false, nil
		}
	}
	p, perr := parsePrefixField(t)
	if perr != nil {
		return nil, false, fmt.Errorf("%q is not a known object or a valid address/CIDR", s)
	}
	return []netip.Prefix{p}, false, nil
}

// resolveLegs unions a rule's inline Proto/ports leg (if any) with the legs of
// every named service it references.
func resolveLegs(fr FirewallRule, cat *fwCatalog) (legs []fwLeg, anyLeg bool, err error) {
	hasInline := strings.TrimSpace(fr.Proto) != "" && !strings.EqualFold(strings.TrimSpace(fr.Proto), "any")
	hasPorts := fr.SrcPortMin != 0 || fr.SrcPortMax != 0 || fr.DstPortMin != 0 || fr.DstPortMax != 0
	if hasInline || hasPorts {
		legs = append(legs, fwLeg{
			proto:    protoNum(fr.Proto),
			sportMin: uint16(fr.SrcPortMin),
			sportMax: uint16(fr.SrcPortMax),
			dportMin: uint16(fr.DstPortMin),
			dportMax: uint16(fr.DstPortMax),
		})
	}
	for _, name := range fr.Services {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		sl, ok := cat.services[key]
		if !ok {
			return nil, false, fmt.Errorf("unknown service %q", name)
		}
		legs = append(legs, sl...)
	}
	if len(legs) == 0 {
		return nil, true, nil
	}
	return legs, false, nil
}

// compileRule resolves an authored rule against a catalog into its hot-path
// form. The authored spec is kept verbatim on the rule for faithful export.
func compileRule(fr FirewallRule, cat *fwCatalog) (*fwRule, error) {
	if cat == nil {
		cat = emptyCatalog()
	}
	src, anySrc, err := resolveAddrField(fr.Src, cat)
	if err != nil {
		return nil, fmt.Errorf("src: %w", err)
	}
	dst, anyDst, err := resolveAddrField(fr.Dst, cat)
	if err != nil {
		return nil, fmt.Errorf("dst: %w", err)
	}
	legs, anyLeg, err := resolveLegs(fr, cat)
	if err != nil {
		return nil, err
	}
	act := fwAllow
	if strings.EqualFold(fr.Action, "deny") {
		act = fwDeny
	}
	return &fwRule{
		spec:       fr,
		disabled:   fr.Disabled,
		dir:        dirFromString(fr.Direction),
		src:        src,
		dst:        dst,
		anySrc:     anySrc,
		anyDst:     anyDst,
		srcNegate:  fr.SrcNegate,
		dstNegate:  fr.DstNegate,
		legs:       legs,
		anyLeg:     anyLeg,
		legsNegate: fr.ServicesNegate,
		action:     act,
		logMatch:   fr.Log,
		notes:      fr.Notes,
		cnt:        newRuleCounters(),
	}, nil
}

// toRule compiles a rule with no object/service catalog — raw CIDR/host and
// inline proto/ports only. Used by config paths and tests that carry no catalog.
func (fr FirewallRule) toRule() (*fwRule, error) { return compileRule(fr, emptyCatalog()) }

// ruleToExport returns the authored form, refreshed with the live id and hit
// counters, so a listing reflects exactly what was written (object/service names
// intact) plus current traffic.
func ruleToExport(r *fwRule) FirewallRule {
	out := r.spec
	out.ID = r.id
	if r.cnt != nil {
		out.Packets = r.cnt.pkts.Load()
		out.Bytes = r.cnt.bytes.Load()
	}
	return out
}

// rangeToPrefixes decomposes an inclusive [lo,hi] address range into the minimal
// set of aligned CIDR prefixes that exactly covers it. lo and hi must be the
// same family (the caller guarantees this).
func rangeToPrefixes(lo, hi netip.Addr) []netip.Prefix {
	bits := lo.BitLen()
	loI, hiI := addrToBig(lo), addrToBig(hi)
	if loI.Cmp(hiI) > 0 {
		return nil
	}
	one := big.NewInt(1)
	cur := new(big.Int).Set(loI)
	var out []netip.Prefix
	for cur.Cmp(hiI) <= 0 {
		// Largest aligned block that starts at cur: bounded by cur's alignment
		// (its trailing-zero-bit count) and by how much of the range remains.
		maxByAlign := bits
		if cur.Sign() != 0 {
			maxByAlign = int(cur.TrailingZeroBits())
		}
		span := new(big.Int).Sub(hiI, cur)
		span.Add(span, one) // hi - cur + 1
		maxBySpan := span.BitLen() - 1
		sz := maxByAlign
		if maxBySpan < sz {
			sz = maxBySpan
		}
		out = append(out, netip.PrefixFrom(bigToAddr(cur, lo.Is4()), bits-sz))
		cur.Add(cur, new(big.Int).Lsh(one, uint(sz)))
	}
	return out
}

func addrToBig(a netip.Addr) *big.Int {
	if a.Is4() {
		b := a.As4()
		return new(big.Int).SetBytes(b[:])
	}
	b := a.As16()
	return new(big.Int).SetBytes(b[:])
}

func bigToAddr(v *big.Int, v4 bool) netip.Addr {
	if v4 {
		var b [4]byte
		v.FillBytes(b[:])
		return netip.AddrFrom4(b)
	}
	var b [16]byte
	v.FillBytes(b[:])
	return netip.AddrFrom16(b)
}

// ---- engine API (driven by the control plane) ----

func (e *Engine) fwOf(networkID uint64) (*firewall, error) {
	ns := e.network(networkID)
	if ns == nil {
		return nil, errors.New("unknown network")
	}
	if ns.fw == nil {
		return nil, errors.New("firewall not initialized on this network")
	}
	return ns.fw, nil
}

// loadRules compiles authored rules against the current catalog and installs
// them as the rulebase, assigning fresh ids. Invalid rules are skipped with a
// warning (matching the build-time tolerance) rather than failing the network.
func (f *firewall) loadRules(rules []FirewallRule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var nr []*fwRule
	for _, fr := range rules {
		r, err := compileRule(fr, f.cat)
		if err != nil {
			if f.log != nil {
				f.log.Warnf("mesh: skipping invalid firewall rule: %v", err)
			}
			continue
		}
		f.nextID++
		r.id = f.nextID
		nr = append(nr, r)
	}
	f.store(nr)
}

// ReloadFirewallRules replaces the live rulebase for a network from a fresh
// config. It only applies when the firewall is already active; enabling or
// disabling the firewall entirely (or adding/removing networks) needs a restart.
func (e *Engine) ReloadFirewallRules(networkID uint64, rules []FirewallRule) error {
	f, err := e.fwOf(networkID)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var nr []*fwRule
	for _, fr := range rules {
		r, err := compileRule(fr, f.cat)
		if err != nil {
			return err
		}
		f.nextID++
		r.id = f.nextID
		nr = append(nr, r)
	}
	f.store(nr)
	return nil
}

// FirewallRules returns the ordered rulebase for a network.
func (e *Engine) FirewallRules(networkID uint64) ([]FirewallRule, error) {
	f, err := e.fwOf(networkID)
	if err != nil {
		return nil, err
	}
	cur := f.current()
	out := make([]FirewallRule, 0, len(cur))
	for _, r := range cur {
		out = append(out, ruleToExport(r))
	}
	return out, nil
}

// FirewallAdd inserts a rule at position at (-1 = append) and returns it.
func (e *Engine) FirewallAdd(networkID uint64, fr FirewallRule, at int) (FirewallRule, error) {
	f, err := e.fwOf(networkID)
	if err != nil {
		return FirewallRule{}, err
	}
	r, err := f.compile(fr)
	if err != nil {
		return FirewallRule{}, err
	}
	added := f.add(r, at)
	e.log.Infof("mesh: firewall rule %d added on net %x (%s)", added.id, networkID, added.action.string())
	e.notifyChange(networkID)
	return ruleToExport(added), nil
}

func (e *Engine) FirewallDelete(networkID uint64, ids []uint64) error {
	f, err := e.fwOf(networkID)
	if err != nil {
		return err
	}
	n := f.remove(ids)
	e.log.Infof("mesh: firewall deleted %d rule(s) on net %x", n, networkID)
	e.notifyChange(networkID)
	return nil
}

func (e *Engine) FirewallMove(networkID, id uint64, to int) error {
	f, err := e.fwOf(networkID)
	if err != nil {
		return err
	}
	if !f.move(id, to) {
		return fmt.Errorf("no rule with id %d", id)
	}
	e.notifyChange(networkID)
	return nil
}

func (e *Engine) FirewallCopy(networkID uint64, ids []uint64) error {
	f, err := e.fwOf(networkID)
	if err != nil {
		return err
	}
	f.copy(ids)
	return nil
}

func (e *Engine) FirewallCut(networkID uint64, ids []uint64) error {
	f, err := e.fwOf(networkID)
	if err != nil {
		return err
	}
	f.cut(ids)
	e.notifyChange(networkID)
	return nil
}

func (e *Engine) FirewallPaste(networkID uint64, at int) (int, error) {
	f, err := e.fwOf(networkID)
	if err != nil {
		return 0, err
	}
	n := f.paste(at)
	e.notifyChange(networkID)
	return n, nil
}

func (a fwAction) string() string {
	if a == fwDeny {
		return "deny"
	}
	return "allow"
}

// ---- object / service catalog & counters (control plane) ----

// FirewallObjectsList returns the node's named address objects — shared by
// every network (see the fwCatalogMu field group's doc comment).
func (e *Engine) FirewallObjectsList() ([]FirewallObject, error) {
	objs, _ := e.firewallCatalogSnapshot()
	return objs, nil
}

// SetFirewallObjects replaces the node-global address-object catalog and
// recompiles every network's rulebase against it, live. FQDN objects are
// resolved promptly afterwards, network by network.
func (e *Engine) SetFirewallObjects(objs []FirewallObject) error {
	e.fwCatalogMu.Lock()
	e.fwObjects = append([]FirewallObject(nil), objs...)
	e.fwCatalogMu.Unlock()
	var notifyID uint64
	notified := false
	for id, ns := range e.netSnapshot() {
		if ns.fw == nil {
			continue
		}
		ns.fw.setObjects(objs)
		e.refreshFirewallFQDN(id)
		notifyID, notified = id, true
	}
	e.log.Infof("mesh: firewall objects updated (%d)", len(objs))
	// notifyChange needs some networkID to locate the config file's network
	// entry for the *other* things it persists (rules, name/subnet, keys) —
	// any currently-running network does, since the persist hook syncs the
	// node-global catalog unconditionally regardless of which one triggered
	// it. If no network is up yet, there's nothing to notify (and nothing
	// waiting on this UI-side; see secFwObjects/secFwServices's "no
	// networks" early return).
	if notified {
		e.notifyChange(notifyID)
	}
	return nil
}

// FirewallServicesList returns the node's named service catalog — shared by
// every network (see the fwCatalogMu field group's doc comment).
func (e *Engine) FirewallServicesList() ([]FirewallService, error) {
	_, svcs := e.firewallCatalogSnapshot()
	return svcs, nil
}

// SetFirewallServices replaces the node-global service catalog and
// recompiles every network's rulebase against it, live.
func (e *Engine) SetFirewallServices(svcs []FirewallService) error {
	e.fwCatalogMu.Lock()
	e.fwServices = append([]FirewallService(nil), svcs...)
	e.fwCatalogMu.Unlock()
	var notifyID uint64
	notified := false
	for id, ns := range e.netSnapshot() {
		if ns.fw == nil {
			continue
		}
		ns.fw.setServices(svcs)
		notifyID, notified = id, true
	}
	e.log.Infof("mesh: firewall services updated (%d)", len(svcs))
	if notified {
		e.notifyChange(notifyID)
	}
	return nil
}

// FirewallResetCounters zeroes the hit tallies of the given rules (empty = all).
func (e *Engine) FirewallResetCounters(networkID uint64, ids []uint64) error {
	f, err := e.fwOf(networkID)
	if err != nil {
		return err
	}
	f.resetCounters(ids)
	return nil
}
