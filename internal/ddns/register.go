package ddns

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Logf is the log sink, a plain function so the caller can hand over logx.Warnf
// directly and a test can hand over nothing.
type Logf func(format string, args ...any)

// Params is one registration run's inputs. Assembled by the caller from the
// host's live resolver settings, so this package neither reads the OS nor
// knows what a gravinet config looks like.
type Params struct {
	// Hostname is the short name to register. A name that already carries the
	// domain is accepted and not doubled up.
	Hostname string
	// Domain is the zone to register into — the host's search domain.
	Domain string
	// Servers are the DNS servers to ask. The first that answers a SOA lookup
	// decides where the update goes; the rest are fallbacks for that lookup,
	// not additional update targets, because the update belongs to whichever
	// server is authoritative rather than to all of them.
	Servers []string
	// TTL for the records published. 0 takes the default.
	TTL uint32
	// Key signs the updates, or nil for unsigned.
	Key *Key
	// SkipIfaces are interfaces whose addresses must not be published —
	// gravinet's own overlay devices. See Registrar.Run.
	SkipIfaces []string
	// Reverse also publishes a PTR for every address published forward.
	Reverse bool
	// PublishMesh is informational: it records that SkipIfaces was left empty
	// deliberately rather than by omission. This package never reads it for a
	// decision — the skip list is the decision — but a diagnosis that cannot
	// tell "no overlay addresses configured" from "overlay addresses excluded"
	// is missing the only fact that distinguishes them.
	PublishMesh bool
}

// A note on TTL: Params.TTL is taken literally, including zero.
//
// Zero is a real DNS answer — it tells resolvers not to cache the record at all
// — so this package does not spend it as a stand-in for "unset". A caller that
// wants the ordinary lifetime asks for it by name; config.DefaultDDNSTTL is
// where that lives, alongside every other default gravinet ships, and it is
// written into the config file rather than inferred here.
//
// It used to mean "use 900", which made zero unreachable and made two adjacent
// fields on the same settings card disagree about what zero meant: the interval
// switches registration off with it, and the TTL switched a default on. See
// v995.

// Result is what one run did, for the log and the page.
type Result struct {
	// Published are the names now confirmed correct, whether this run wrote
	// them or found them already right.
	Published []string
	// Updated counts the names this run actually changed. Zero is the steady
	// state and the reason this is worth reporting separately: a run that
	// updates something every time is a symptom.
	Updated int
	// Errors are per-name failures. A failure on one name does not stop the
	// others — a node with a working LAN address and a broken second interface
	// should still have its primary name published.
	Errors []string
}

// recordSet is one name, one record type, and every address that name should
// carry of that type.
//
// A set rather than a record because that is the unit an update writes. A name
// with two IPv4 addresses is one delete-RRset followed by two adds, in one
// message; treating each address separately would mean the second update's
// delete threw away what the first had just published, and the name would end
// up with whichever address happened to be enumerated last.
type recordSet struct {
	fqdn  string
	rtype int
	addrs []netip.Addr
	// primary marks the bare hostname.domain sets, as against the
	// per-interface aliases. Only these get a PTR: a reverse lookup has one
	// answer and it should be the name a human would use.
	primary bool
}

// Register performs one registration pass and returns what it did.
//
// The shape is the same for every name: work out where the zone's updates go,
// ask what is published now, and send a delete-then-add only if that disagrees
// with what this host currently has. On a node whose addresses are stable —
// which a gateway's are — every run after the first is a pair of queries and no
// writes at all.
func Register(p Params, log Logf) (Result, error) {
	var res Result
	if log == nil {
		log = func(string, ...any) {}
	}
	host := strings.TrimSpace(p.Hostname)
	domain := strings.Trim(strings.TrimSpace(p.Domain), ".")
	if host == "" || domain == "" || len(p.Servers) == 0 {
		// Not an error. This is the "not configured yet" state, and the caller
		// checks for it before calling — but a node can lose its search domain
		// between the check and the run.
		return res, fmt.Errorf("need a hostname, a search domain and at least one DNS server")
	}
	// A hostname the operator typed as an FQDN already carries the domain.
	host = strings.TrimSuffix(strings.TrimSuffix(host, "."+domain), ".")
	if host == "" || strings.Contains(host, ".") && strings.HasSuffix(host, domain) {
		return res, fmt.Errorf("hostname %q and domain %q do not combine into a name", p.Hostname, p.Domain)
	}
	ttl := p.TTL

	records, err := collectRecords(host, domain, p.SkipIfaces)
	if err != nil {
		return res, err
	}
	if len(records) == 0 {
		return res, fmt.Errorf("no address on this host is worth publishing (every interface is loopback, link-local, or one of gravinet's own)")
	}

	// Where the zone's updates go, and what that zone is actually called.
	// Asked once and reused for every name in it.
	//
	// The apex is taken from the server's answer rather than assumed to be the
	// search domain, because those are two different things: a host in
	// eng.corp.internal whose records live in the corp.internal zone has to
	// name corp.internal in the update, and naming the search domain would be
	// refused by a server that does not serve a zone by that name.
	master, zone, err := findMaster(domain, p.Servers)
	if err != nil {
		return res, err
	}
	if zone == "" {
		zone = domain
	}

	for _, rs := range records {
		label := fmt.Sprintf("%s %s", rs.fqdn, recordTypeName(rs.rtype))

		// Read before writing. An update that is already true costs a delete
		// and an add, bumps the zone serial, and on a secondary triggers a
		// transfer — for nothing. The query is one packet and turns the steady
		// state, which is almost every run, into a read-only operation.
		//
		// Compared as a set, because that is what is written: a name with two
		// addresses is correct only when both are published and nothing else
		// is. A subset match would leave a stale third address in place
		// forever, since every later run would agree the record was fine.
		//
		// A query that failed is treated as "unknown" rather than "absent", and
		// the update is sent. Asserting on no information is the safe
		// direction: the cost is a redundant write, where the cost of assuming
		// the record is fine would be a node whose name never resolves because
		// one lookup timed out.
		current, qerr := currentRecords(master, rs.fqdn, rs.rtype)
		if qerr == nil && sameAddrSet(current, rs.addrs) {
			res.Published = append(res.Published, label)
			continue
		}

		// One delete for the type, then every address. In one message, so the
		// name never resolves to nothing in between.
		upd := []rr{deleteRRset(rs.fqdn, rs.rtype)}
		for _, a := range rs.addrs {
			_, rdata := rdataAddr(a)
			upd = append(upd, addRecord(rs.fqdn, rs.rtype, ttl, rdata))
		}
		if err := send(master, zone, upd, p.Key); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", label, err))
			// No PTR for a forward record that did not land: a pointer to a
			// name that does not resolve is worse than no pointer.
			continue
		}
		log("ddns: published %s %s", label, joinAddrs(rs.addrs))
		res.Published = append(res.Published, label)
		res.Updated++
	}

	// The reverse records, checked on their own every run whether or not the
	// forward ones needed anything.
	//
	// They used to be attempted only when the forward record had just changed,
	// which meant a node whose A record was already correct never looked at its
	// PTR at all — so a PTR deleted by hand, or one that failed on the run that
	// created the A record, stayed missing forever. The two live in different
	// zones, quite often on different servers, and there is no reason for the
	// state of one to be evidence about the other.
	//
	// One per published address, pointing at the name that carries it. Through
	// v1002 only the primary name's address got one, on the reasoning that a
	// reverse lookup has a single answer and it should be the name a human
	// would use. That is true of an address, and the mistake was applying it to
	// a *host*: a multi-homed node's other addresses are not aliases of the
	// primary one, they are separate addresses on separate networks, and the
	// name a human wants back for 10.1.1.1 is the name that resolves to
	// 10.1.1.1. Leaving them out meant a gateway with three LANs published one
	// PTR and left every reverse zone the operator actually ran untouched.
	//
	// Where an address appears under both the primary name and a per-interface
	// alias, the primary wins: that is the single-answer rule, applied per
	// address where it belongs.
	if p.Reverse {
		for _, t := range ptrTargets(records) {
			if failedForward(res.Errors, t.fqdn, t.rtype) {
				continue
			}
			changed, err := syncPTR(master, p.Servers, t.addr, t.fqdn, ttl, p.Key)
			label := fmt.Sprintf("%s PTR", t.addr)
			switch {
			case err != nil:
				// Reported, never fatal. A forward record that resolves is the
				// job; a missing PTR is a lesser problem and very often means
				// the reverse zone simply is not delegated here, which is not
				// this node's to fix.
				res.Errors = append(res.Errors, fmt.Sprintf("PTR for %s: %v", t.addr, err))
			case changed:
				log("ddns: published PTR %s -> %s", t.addr, t.fqdn)
				res.Published = append(res.Published, label)
				res.Updated++
			default:
				// Already correct. Counted as published for the same reason the
				// forward sets are: the summary line is meant to say what this
				// node has in DNS, and a reverse record that is right is not
				// less published than one written a moment ago. Leaving it out
				// made a run with working PTRs and a run with none produce the
				// same numbers.
				res.Published = append(res.Published, label)
			}
		}
	}
	return res, nil
}

// ptrTarget is one address and the name its PTR should answer with.
type ptrTarget struct {
	addr  netip.Addr
	fqdn  string
	rtype int
}

// ptrTargets is one entry per distinct address, carrying the name that should
// be returned for it.
//
// First-seen wins, and collectRecords builds the primary set for an address
// before the per-interface alias that shares it, so the primary name is
// preferred without this needing to know which is which. An address that only
// ever appears under an alias — every interface after the first of its family —
// gets that alias, which is the only name that resolves to it.
func ptrTargets(records []recordSet) []ptrTarget {
	seen := map[netip.Addr]bool{}
	var out []ptrTarget
	for _, rs := range records {
		for _, a := range rs.addrs {
			if seen[a] {
				continue
			}
			seen[a] = true
			out = append(out, ptrTarget{addr: a, fqdn: rs.fqdn, rtype: rs.rtype})
		}
	}
	return out
}

// collectRecords is every address on this host worth publishing, grouped into
// the sets that will be written.
//
// Both families, always. A dual-stack node publishes an A and an AAAA under the
// same name, which is the whole point of being dual-stack — a client picks the
// family it can use. The two are separate record types, so each is its own set
// and neither delete touches the other.
//
// The bare hostname.domain gets the first usable address *of each family*, and
// only the first: on a gateway with three LAN interfaces, publishing all three
// under one name would hand clients a round-robin across networks most of them
// cannot reach. The per-interface alias is where the rest live, and it carries
// every address of its family on that interface.
//
// Skipped: loopback, IPv6 link-local, and gravinet's own overlay devices. The
// last is the one this project has to decide for itself — an overlay address
// published into LAN DNS is reachable only by mesh peers, who already resolve
// each other through the hosts-file sync, so it would answer queries from hosts
// that cannot use it while adding nothing for the hosts that can.
func collectRecords(host, domain string, skip []string) ([]recordSet, error) {
	skipSet := map[string]bool{}
	for _, n := range skip {
		if n = strings.TrimSpace(n); n != "" {
			skipSet[n] = true
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("reading this host's interfaces: %w", err)
	}
	// Stable order, so the address that becomes the primary is the same one
	// from run to run rather than whatever the kernel listed first today.
	sort.Slice(ifaces, func(i, j int) bool { return ifaces[i].Index < ifaces[j].Index })

	// Keyed by name+type, with the order they were first seen kept separately:
	// map iteration order is random, and a set of updates that reordered
	// itself between runs would make the log impossible to diff.
	sets := map[string]*recordSet{}
	var order []string
	add := func(fqdn string, rtype int, primary bool, a netip.Addr) {
		k := fmt.Sprintf("%s/%d", fqdn, rtype)
		rs, ok := sets[k]
		if !ok {
			rs = &recordSet{fqdn: fqdn, rtype: rtype, primary: primary}
			sets[k], order = rs, append(order, k)
		}
		for _, have := range rs.addrs {
			if have == a {
				return
			}
		}
		rs.addrs = append(rs.addrs, a)
	}

	primaryTaken := map[int]bool{} // by record type, so v4 and v6 each get one
	for _, ifi := range ifaces {
		if skipSet[ifi.Name] || ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			addr = addr.Unmap()
			if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified() {
				continue
			}
			rtype, _ := rdataAddr(addr)
			if !primaryTaken[rtype] {
				add(host+"."+domain, rtype, true, addr)
				primaryTaken[rtype] = true
			}
			add(fmt.Sprintf("%s-%s.%s", host, sanitizeLabel(ifi.Name), domain), rtype, false, addr)
		}
	}

	out := make([]recordSet, 0, len(order))
	for _, k := range order {
		out = append(out, *sets[k])
	}
	return out, nil
}

// sanitizeLabel makes an interface name usable as a DNS label. A tagged
// interface is "eth1.22", whose dot would otherwise make the alias a name in a
// subdomain that does not exist.
func sanitizeLabel(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// findMaster is the server an update for a name has to be sent to, and the zone
// that update has to name.
//
// The zone's SOA names its primary master, and that is where RFC 2136 updates
// go — not to whichever resolver this host happens to use, which may be a
// forwarder with no authority and will answer REFUSED or NOTAUTH. Asking is one
// query and removes a whole class of "it works from my laptop" confusion.
//
// The same answer settles the zone name. An UPDATE's zone section has to carry
// the apex — the name the server knows the zone by — and the caller very often
// has only a name from inside it. A server asked for the SOA of a name below an
// apex returns the apex's SOA, owned by the apex, in the authority section of a
// negative answer, so one lookup yields both facts. Taking the owner rather than
// echoing the queried name is what lets this work against a zone delegated
// somewhere the caller did not predict.
//
// An empty zone means the server answered without an SOA anywhere. The caller
// decides what to do with that, because the sensible fallback differs: for a
// forward name it is the search domain, and for a reverse name it is the
// classful guess.
//
// Falls back to the configured server when the SOA names something that does
// not resolve. On a small network the resolver and the authoritative server are
// usually the same box, so the fallback is right far more often than it is a
// guess.
func findMaster(name string, servers []string) (master, zone string, err error) {
	var lastErr error
	for _, s := range servers {
		id, q, err := buildQuery(name, typeSOA, true)
		if err != nil {
			return "", "", err
		}
		reply, err := exchange(s, id, q)
		if err != nil {
			lastErr = err
			continue
		}
		rcode, answers, ok := parseResponse(reply)
		if !ok {
			lastErr = fmt.Errorf("%s sent a reply that could not be read", s)
			continue
		}
		if rcode != rcodeNoError && rcode != rcodeNXDomain {
			lastErr = fmt.Errorf("%s answered %s for the SOA of %s", s, rcodeText(rcode), name)
			continue
		}
		for _, a := range answers {
			if a.rtype != typeSOA || a.text == "" {
				continue
			}
			if strings.Contains(a.text, "root-servers") {
				// The root's SOA means the resolver went all the way up and
				// found nothing: this name is not in a zone anybody here serves.
				return "", "", fmt.Errorf("%s is not a zone on any configured server (the lookup reached the root), so there is nothing to register into", name)
			}
			apex := zoneFromSOA(a.name, name)
			if ip := resolveHost(a.text, s); ip != "" {
				return ip, apex, nil
			}
			// The MNAME did not resolve, so it is not an address anything can
			// be sent to. The server that just answered is: on a small network
			// it is the same box, and on a larger one it is at least reachable
			// and will answer NOTAUTH rather than nothing at all.
			//
			// Returning the unresolvable name instead — which this did through
			// v1002 — produced an update dialled at a hostname with no address,
			// failing in the socket layer with a DNS error about the zone's own
			// name. The common way to land there is the `@ IN SOA @ ...` idiom,
			// where MNAME expands to the zone apex: extremely ordinary in a
			// hand-written reverse zone, and fatal to every update sent to it.
			return s, apex, nil
		}
		// Answered, no SOA. The configured server is the best remaining guess,
		// and there is nothing to say about the zone name.
		return s, "", nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no configured DNS server answered")
	}
	return "", "", fmt.Errorf("could not find the server authoritative for %s: %w", name, lastErr)
}

// zoneFromSOA is the zone apex an SOA answer establishes, given the name that
// was asked about.
//
// The owner of the SOA is the apex. The check is that it encloses the queried
// name, which it does in every well-formed answer; anything else is a reply
// this code does not understand well enough to build an update from, and the
// queried name is the more conservative of the two guesses left.
func zoneFromSOA(owner, queried string) string {
	o := strings.ToLower(strings.Trim(strings.TrimSpace(owner), "."))
	q := strings.ToLower(strings.Trim(strings.TrimSpace(queried), "."))
	if o == "" {
		return q
	}
	if o != q && !strings.HasSuffix(q, "."+o) {
		return q
	}
	return o
}

// usableReverseZone reports whether an apex is one a PTR for rev could actually
// live in.
//
// The arpa apexes are the interesting rejects. A resolver asked about a reverse
// name nobody here serves walks up and answers with the SOA of in-addr.arpa or
// ip6.arpa, which is a real zone and would be accepted by the checks above —
// and an update naming it is refused by every server on earth. Catching it here
// turns that into a sentence about a missing delegation, which is what it is.
func usableReverseZone(zone, rev string) bool {
	z := strings.ToLower(strings.Trim(strings.TrimSpace(zone), "."))
	switch z {
	case "", "arpa", "in-addr.arpa", "ip6.arpa":
		return false
	}
	r := strings.ToLower(strings.Trim(strings.TrimSpace(rev), "."))
	return r == z || strings.HasSuffix(r, "."+z)
}

// resolveHost turns a nameserver's own name into an address, using the resolver
// that just told us about it. Empty when it does not resolve, which the caller
// treats as "use the name and let the dialer try".
func resolveHost(name, server string) string {
	for _, qt := range []int{typeA, typeAAAA} {
		id, q, err := buildQuery(name, qt, true)
		if err != nil {
			continue
		}
		reply, err := exchange(server, id, q)
		if err != nil {
			continue
		}
		_, answers, ok := parseResponse(reply)
		if !ok {
			continue
		}
		for _, a := range answers {
			if (a.rtype == typeA || a.rtype == typeAAAA) && a.text != "" {
				return a.text
			}
		}
	}
	return ""
}

// currentRecord is what the authoritative server publishes for a name right
// now, or "" if nothing. Asked before every update so a steady node writes
// nothing.
func currentRecord(server, fqdn string, rtype int) (string, error) {
	id, q, err := buildQuery(fqdn, rtype, false)
	if err != nil {
		return "", err
	}
	reply, err := exchange(server, id, q)
	if err != nil {
		return "", err
	}
	rcode, answers, ok := parseResponse(reply)
	if !ok || rcode != rcodeNoError {
		return "", fmt.Errorf("lookup of %s answered %s", fqdn, rcodeText(rcode))
	}
	for _, a := range answers {
		if a.rtype == rtype && a.text != "" {
			return a.text, nil
		}
	}
	return "", nil
}

// send signs (if there is a key) and sends one update, and turns a non-zero
// rcode into an error that says what to do about it.
func send(server, zone string, updates []rr, key *Key) error {
	id, msg, err := buildUpdate(zone, updates)
	if err != nil {
		return err
	}
	if key != nil {
		if msg, err = sign(msg, id, *key, time.Now()); err != nil {
			return err
		}
	}
	reply, err := exchange(server, id, msg)
	if err != nil {
		return err
	}
	rcode, _, ok := parseResponse(reply)
	if !ok {
		return fmt.Errorf("the server's reply could not be read")
	}
	if rcode != rcodeNoError {
		return fmt.Errorf("the server answered %s", rcodeText(rcode))
	}
	return nil
}

// syncPTR brings the reverse record for an address into line, and reports
// whether it had to write anything.
//
// Same read-before-write as the forward half, and for the same reason, with one
// difference worth stating: the comparison is on the name the PTR points at,
// not on an address, so a PTR that already names this host is left alone even
// though it lives in a different zone from the record that produced it.
//
// The reverse zone gets its own SOA lookup, because it is very often served by
// something other than the forward zone's master — or by nothing at all, which
// is why the caller treats a failure here as a note rather than an error.
//
// The lookup asks about the full PTR name rather than a guessed zone. That is
// the whole difference between this working and not: the answer to "what is the
// SOA for 2.2.0.192.in-addr.arpa" names the zone that actually holds that name,
// whatever boundary it was delegated on, and that is the name the update has to
// carry in its zone section. Deriving the zone arithmetically instead — the /24,
// the /64 — is right only for sites that happen to delegate there, and every
// other site gets an update naming a zone their server has never heard of,
// which is refused. See v1001.
func syncPTR(forwardMaster string, servers []string, addr netip.Addr, fqdn string, ttl uint32, key *Key) (bool, error) {
	rev, guess := reverseName(addr)
	if rev == "" {
		return false, fmt.Errorf("no reverse name for %s", addr)
	}
	master, zone, err := findMaster(rev, servers)
	if err != nil || zone == "" {
		// The reverse zone may not be delegated to these servers at all. Try
		// the forward zone's master, which on a single-server network is the
		// same box and does hold it, and fall back to the classful boundary
		// for the zone name because nothing better was offered.
		master, zone = forwardMaster, guess
	}
	if !usableReverseZone(zone, rev) {
		// The closest enclosing zone is one of the arpa apexes, which means no
		// reverse zone for this address is served here. Sending the update
		// anyway would produce a refusal and an operator hunting through TSIG
		// settings for a problem that is a missing delegation.
		return false, fmt.Errorf("no reverse zone covering %s is served here (the nearest zone is %s, which is not a delegation this node can update)", addr, zone)
	}
	// Trailing dots are not significant and the wire form carries none, so the
	// comparison is made on the bare names.
	if cur, qerr := currentRecord(master, rev, typePTR); qerr == nil {
		if strings.EqualFold(strings.TrimSuffix(cur, "."), strings.TrimSuffix(fqdn, ".")) {
			return false, nil
		}
	}
	name, err := encodeName(fqdn)
	if err != nil {
		return false, err
	}
	if err := send(master, zone, []rr{
		deleteRRset(rev, typePTR),
		addRecord(rev, typePTR, ttl, name),
	}, key); err != nil {
		return false, err
	}
	return true, nil
}

// reverseName is an address's PTR name, and a guess at the zone that holds it.
//
// The name is exact. The zone is the /24 for IPv4 and the /64 for IPv6, which
// is a guess and is used only when the servers cannot be asked — syncPTR
// discovers the real apex from the SOA and falls back to this only when the
// lookup itself fails.
//
// It was the primary source of the zone name through v1000, and it was wrong
// for every site that delegates anywhere other than those two boundaries: a
// 192.168.0.0/16 held as one reverse zone, a 10/8 held as one, an RFC 2317
// slice of a /24. All of them got an update naming a zone that does not exist,
// all of them answered NOTAUTH, and none of them ever got a PTR — while the
// forward records, whose zone name came from the search domain and was
// therefore usually right, published normally.
func reverseName(a netip.Addr) (name, zone string) {
	if a.Is4() {
		b := a.As4()
		return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", b[3], b[2], b[1], b[0]),
			fmt.Sprintf("%d.%d.%d.in-addr.arpa", b[2], b[1], b[0])
	}
	if !a.Is6() {
		return "", ""
	}
	b := a.As16()
	nibbles := make([]string, 0, 32)
	for i := len(b) - 1; i >= 0; i-- {
		nibbles = append(nibbles, fmt.Sprintf("%x", b[i]&0x0F), fmt.Sprintf("%x", b[i]>>4))
	}
	name = strings.Join(nibbles, ".") + ".ip6.arpa"
	// The /64: the top 64 bits are the last 16 nibbles of the reversed name.
	zone = strings.Join(nibbles[16:], ".") + ".ip6.arpa"
	return name, zone
}

func recordTypeName(t int) string {
	switch t {
	case typeA:
		return "A"
	case typeAAAA:
		return "AAAA"
	case typePTR:
		return "PTR"
	}
	return fmt.Sprintf("type%d", t)
}

// currentRecords is everything the authoritative server publishes for a name
// at one type, in no particular order. Empty means the name has no records of
// that type, which is a normal first run and not an error.
func currentRecords(server, fqdn string, rtype int) ([]netip.Addr, error) {
	id, q, err := buildQuery(fqdn, rtype, false)
	if err != nil {
		return nil, err
	}
	reply, err := exchange(server, id, q)
	if err != nil {
		return nil, err
	}
	rcode, answers, ok := parseResponse(reply)
	if !ok {
		return nil, fmt.Errorf("the reply for %s could not be read", fqdn)
	}
	// NXDOMAIN is a real answer here: the name does not exist yet, which is
	// exactly the state a first run is in, and reporting it as a failure would
	// make every new record look like an error before it was created.
	if rcode == rcodeNXDomain {
		return nil, nil
	}
	if rcode != rcodeNoError {
		return nil, fmt.Errorf("lookup of %s answered %s", fqdn, rcodeText(rcode))
	}
	var out []netip.Addr
	for _, a := range answers {
		if a.rtype != rtype || a.text == "" {
			continue
		}
		if addr, err := netip.ParseAddr(a.text); err == nil {
			out = append(out, addr)
		}
	}
	return out, nil
}

// sameAddrSet reports whether two address lists hold the same members,
// disregarding order. DNS answers arrive in whatever order the server felt like
// — quite often deliberately rotated — so comparing them positionally would
// rewrite a correct record on most runs.
func sameAddrSet(a, b []netip.Addr) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[netip.Addr]int, len(a))
	for _, x := range a {
		seen[x]++
	}
	for _, y := range b {
		if seen[y] == 0 {
			return false
		}
		seen[y]--
	}
	return true
}

// failedForward reports whether this run already failed to write the forward
// set a PTR would point at.
func failedForward(errs []string, fqdn string, rtype int) bool {
	prefix := fmt.Sprintf("%s %s:", fqdn, recordTypeName(rtype))
	for _, e := range errs {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// joinAddrs renders an address set for the log.
func joinAddrs(addrs []netip.Addr) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ", ")
}
