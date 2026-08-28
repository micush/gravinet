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
	// Reverse also publishes a PTR for the primary name.
	Reverse bool
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

	// Where the zone's updates go. Asked once and reused for every name in it.
	master, err := findMaster(domain, p.Servers)
	if err != nil {
		return res, err
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
		if err := send(master, domain, upd, p.Key); err != nil {
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
	// One per primary address, which on a dual-stack node means one in
	// in-addr.arpa and one in ip6.arpa, both pointing at the same name.
	if p.Reverse {
		for _, rs := range records {
			if !rs.primary || len(rs.addrs) == 0 {
				continue
			}
			if failedForward(res.Errors, rs.fqdn, rs.rtype) {
				continue
			}
			addr := rs.addrs[0]
			changed, err := syncPTR(master, p.Servers, addr, rs.fqdn, ttl, p.Key)
			switch {
			case err != nil:
				// Reported, never fatal. A forward record that resolves is the
				// job; a missing PTR is a lesser problem and very often means
				// the reverse zone simply is not delegated here, which is not
				// this node's to fix.
				res.Errors = append(res.Errors, fmt.Sprintf("PTR for %s: %v", addr, err))
			case changed:
				log("ddns: published PTR %s -> %s", addr, rs.fqdn)
				res.Updated++
			}
		}
	}
	return res, nil
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

// findMaster is the server an update for this zone has to be sent to.
//
// The zone's SOA names its primary master, and that is where RFC 2136 updates
// go — not to whichever resolver this host happens to use, which may be a
// forwarder with no authority and will answer REFUSED or NOTAUTH. Asking is one
// query and removes a whole class of "it works from my laptop" confusion.
//
// Falls back to the configured server when the SOA names something that does
// not resolve. On a small network the resolver and the authoritative server are
// usually the same box, so the fallback is right far more often than it is a
// guess.
func findMaster(domain string, servers []string) (string, error) {
	var lastErr error
	for _, s := range servers {
		id, q, err := buildQuery(domain, typeSOA, true)
		if err != nil {
			return "", err
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
			lastErr = fmt.Errorf("%s answered %s for the SOA of %s", s, rcodeText(rcode), domain)
			continue
		}
		for _, a := range answers {
			if a.rtype != typeSOA || a.text == "" {
				continue
			}
			if strings.Contains(a.text, "root-servers") {
				// The root's SOA means the resolver went all the way up and
				// found nothing: this domain is not a zone anybody here serves.
				return "", fmt.Errorf("%s is not a zone on any configured server (the lookup reached the root), so there is nothing to register into", domain)
			}
			if ip := resolveHost(a.text, s); ip != "" {
				return ip, nil
			}
			return a.text, nil
		}
		// Answered, no SOA. The configured server is the best remaining guess.
		return s, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no configured DNS server answered")
	}
	return "", fmt.Errorf("could not find the server authoritative for %s: %w", domain, lastErr)
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
func syncPTR(forwardMaster string, servers []string, addr netip.Addr, fqdn string, ttl uint32, key *Key) (bool, error) {
	rev, zone := reverseName(addr)
	if rev == "" {
		return false, fmt.Errorf("no reverse name for %s", addr)
	}
	master, err := findMaster(zone, servers)
	if err != nil {
		// The reverse zone may not be delegated to these servers at all. Try
		// the forward zone's master, which on a single-server network is the
		// same box and does hold it.
		master = forwardMaster
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

// reverseName is an address's PTR name and the zone that holds it.
//
// The zone is the /24 for IPv4 and the /64 for IPv6 — the classful boundaries
// reverse delegation actually happens on. A network delegated on some other
// boundary (RFC 2317 style) will refuse the update, which is reported and is
// the right outcome: guessing at a delegation shape would send updates to a
// zone that does not exist.
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
