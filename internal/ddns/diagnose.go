package ddns

import (
	"fmt"
	"net/netip"
	"strings"
)

// Diagnose reports what a registration run would do, without sending anything.
//
// # Why this exists
//
// Every decision a run makes was invisible. Register's log sink is the caller's
// debug logger, so a published PTR wrote a debug line; a PTR that was never
// attempted wrote nothing at all; and Result carried no reverse entries, so the
// timer loop's summary counted forward names and nothing else. An operator with
// no PTRs therefore saw exactly the same output as an operator whose PTRs were
// all correct: silence.
//
// That silence covers at least five different causes — reverse publishing
// switched off, an address whose reverse zone nobody local serves, a zone that
// refuses the key, a PTR already present and correct, and an address that is
// simply never considered because it is not the one the primary name carries.
// Each wants a different fix and none of them announces itself. See v1002.
//
// This sends the same queries a run sends and skips the updates, so it is safe
// to run against production at any time, and prints the verdict for every name
// including the ones a run would silently pass over.
type Diagnosis struct {
	// Inputs, echoed because a wrong answer here explains everything after it.
	Hostname string
	Domain   string
	Servers  []string
	Reverse  bool

	// The forward zone, as discovered rather than as assumed.
	ForwardZone   string
	ForwardMaster string
	ForwardErr    string

	Forward []ForwardVerdict
	// PTR carries one entry per address this host publishes — including the
	// addresses no PTR is attempted for, which is the case that had no way of
	// being seen before.
	PTR []ReverseVerdict
}

// ForwardVerdict is one name and type, and what a run would do about it.
type ForwardVerdict struct {
	FQDN      string
	Type      string
	Primary   bool
	Want      []netip.Addr
	Have      []netip.Addr
	LookupErr string
	Action    string // "write" or "already correct"
}

// ReverseVerdict is one address and what a run would do about its PTR.
type ReverseVerdict struct {
	Addr netip.Addr
	// Name is the address's PTR name. Always set.
	Name string
	// Zone is the zone that actually holds that name, discovered from the SOA.
	Zone   string
	Master string
	// Have is the name the PTR currently points at, or "" for none.
	Have string
	// Want is the name it should point at, or "" when no PTR is attempted.
	Want string
	// Note is a qualifier on how the zone was arrived at, kept separate from
	// the verdict so one does not overwrite the other.
	Note string
	// Action is the verdict, and the reason when nothing would happen.
	Action string
}

// Diagnose runs the queries and reports. Never writes.
func Diagnose(p Params) (Diagnosis, error) {
	var d Diagnosis
	host := strings.TrimSpace(p.Hostname)
	domain := strings.Trim(strings.TrimSpace(p.Domain), ".")
	if host == "" || domain == "" || len(p.Servers) == 0 {
		return d, fmt.Errorf("need a hostname, a search domain and at least one DNS server")
	}
	host = strings.TrimSuffix(strings.TrimSuffix(host, "."+domain), ".")
	d.Hostname, d.Domain, d.Servers, d.Reverse = host, domain, p.Servers, p.Reverse
	records, err := collectRecords(host, domain)
	if err != nil {
		return d, err
	}
	if len(records) == 0 {
		return d, fmt.Errorf("no address on this host is worth publishing (every interface is loopback, link-local, or one of gravinet's own)")
	}

	master, zone, err := findMaster(domain, p.Servers)
	if err != nil {
		d.ForwardErr = err.Error()
		return d, nil
	}
	if zone == "" {
		zone = domain
	}
	d.ForwardZone, d.ForwardMaster = zone, master

	for _, rs := range records {
		v := ForwardVerdict{
			FQDN:    rs.fqdn,
			Type:    recordTypeName(rs.rtype),
			Primary: rs.primary,
			Want:    rs.addrs,
			Action:  "write",
		}
		have, qerr := currentRecords(master, rs.fqdn, rs.rtype)
		switch {
		case qerr != nil:
			v.LookupErr = qerr.Error()
		default:
			v.Have = have
			if sameAddrSet(have, rs.addrs) {
				v.Action = "already correct"
			}
		}
		d.Forward = append(d.Forward, v)
	}

	// The reverse verdicts, one per distinct address, using the same target
	// selection the run uses so the two cannot disagree. An address that gets
	// no PTR still gets a line saying why: that case produces no query, no
	// update, no error and no log line, and was the hardest thing here to see
	// from outside.
	targets := ptrTargets(records)
	if !p.Reverse {
		for _, t := range targets {
			v := ReverseVerdict{Addr: t.addr, Want: t.fqdn}
			v.Name, _ = reverseName(t.addr)
			v.Action = "no PTR: reverse publishing is switched off (`gravinet settings ddns reverse on`)"
			d.PTR = append(d.PTR, v)
		}
		return d, nil
	}
	for _, t := range targets {
		d.PTR = append(d.PTR, diagnosePTR(master, p, t.addr, t.fqdn))
	}
	return d, nil
}

// diagnosePTR is the reverse half of one address's verdict: the same lookups
// syncPTR makes, without the update it would follow them with.
func diagnosePTR(forwardMaster string, p Params, addr netip.Addr, fqdn string) ReverseVerdict {
	v := ReverseVerdict{Addr: addr, Want: fqdn}
	rev, guess := reverseName(addr)
	v.Name = rev
	if rev == "" {
		v.Action = "no PTR: this address has no reverse name"
		return v
	}
	master, zone, err := findMaster(rev, p.Servers)
	if err != nil || zone == "" {
		// Kept as a note rather than as the verdict, because the verdict is
		// still to come and would overwrite it. This line is often the whole
		// answer: a fallback means the servers did not name a zone for this
		// address, and the classful guess after it is exactly the assumption
		// that produced no PTRs at all before v1001.
		master, zone = forwardMaster, guess
		if err != nil {
			v.Note = fmt.Sprintf("no zone discovered (%v); falling back to the classful guess %s via the forward zone's master", err, guess)
		} else {
			v.Note = fmt.Sprintf("the servers answered without an SOA for %s, so the zone is the classful guess %s", rev, guess)
		}
	}
	v.Zone, v.Master = zone, master
	if !usableReverseZone(zone, rev) {
		v.Action = fmt.Sprintf("no PTR: the nearest zone above %s is %s, so no reverse zone for this address is served here", rev, zone)
		return v
	}
	cur, qerr := currentRecord(master, rev, typePTR)
	if qerr == nil {
		v.Have = cur
		if strings.EqualFold(strings.TrimSuffix(cur, "."), strings.TrimSuffix(fqdn, ".")) {
			v.Action = "already correct"
			return v
		}
	}
	v.Action = fmt.Sprintf("would write PTR in zone %s via %s", zone, master)
	return v
}

// String renders a diagnosis for a terminal.
func (d Diagnosis) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "name:     %s.%s\n", d.Hostname, d.Domain)
	fmt.Fprintf(&b, "servers:  %s\n", strings.Join(d.Servers, ", "))
	fmt.Fprintf(&b, "reverse:  %s\n", onOffLabel(d.Reverse))
	if d.ForwardErr != "" {
		fmt.Fprintf(&b, "\nforward zone: FAILED — %s\n", d.ForwardErr)
		return b.String()
	}
	fmt.Fprintf(&b, "zone:     %s (updates go to %s)\n", d.ForwardZone, d.ForwardMaster)

	b.WriteString("\nforward\n")
	for _, v := range d.Forward {
		fmt.Fprintf(&b, "  %-40s %-4s %s\n", v.FQDN, v.Type, joinAddrs(v.Want))
		switch {
		case v.LookupErr != "":
			fmt.Fprintf(&b, "  %-40s      lookup failed: %s (would write anyway)\n", "", v.LookupErr)
		case v.Action == "already correct":
			fmt.Fprintf(&b, "  %-40s      already correct\n", "")
		default:
			fmt.Fprintf(&b, "  %-40s      would write (published now: %s)\n", "", orNone(joinAddrs(v.Have)))
		}
	}

	b.WriteString("\nreverse\n")
	if len(d.PTR) == 0 {
		b.WriteString("  nothing to do\n")
	}
	for _, v := range d.PTR {
		if v.Want != "" {
			fmt.Fprintf(&b, "  %-40s -> %s\n", v.Addr.String(), v.Want)
		} else {
			fmt.Fprintf(&b, "  %-40s %s\n", v.Addr.String(), v.Name)
		}
		if v.Note != "" {
			fmt.Fprintf(&b, "  %-40s      %s\n", "", v.Note)
		}
		fmt.Fprintf(&b, "  %-40s      %s\n", "", v.Action)
		if v.Have != "" {
			fmt.Fprintf(&b, "  %-40s      points at: %s\n", "", v.Have)
		}
	}
	return b.String()
}

func onOffLabel(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}
