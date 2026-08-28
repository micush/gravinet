package ddns

import (
	"strings"
	"testing"
)

// The whole point of Diagnose is that it accounts for every address, including
// the ones a run passes over in silence. An address that gets no PTR because it
// is not the one the primary name carries produces no query, no update, no
// error and no log line, and was therefore indistinguishable from an address
// whose PTR is fine.
func TestDiagnoseAccountsForEveryAddress(t *testing.T) {
	f := &fakeDNS{zones: []string{"corp.internal"}}
	server := f.start(t)

	p := Params{
		Hostname: "node7",
		Domain:   "corp.internal",
		Servers:  []string{server},
		Reverse:  true,
	}
	d, err := Diagnose(p)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}

	// Every distinct published address has a reverse verdict. Distinct, not
	// per set: an address carried by both the primary name and a per-interface
	// alias is one address and gets one PTR.
	distinct := map[string]bool{}
	for _, v := range d.Forward {
		for _, a := range v.Want {
			distinct[a.String()] = true
		}
	}
	want := len(distinct)
	if want == 0 {
		t.Skip("this machine has no address worth publishing")
	}
	if len(d.PTR) != want {
		t.Errorf("%d reverse verdicts for %d published addresses; every address has to be accounted for", len(d.PTR), want)
	}
	for _, v := range d.PTR {
		if v.Action == "" {
			t.Errorf("%s has no verdict", v.Addr)
		}
	}

	// Nothing was written.
	if accepted, refused := f.took(); len(accepted) > 0 || len(refused) > 0 {
		t.Errorf("a dry run sent %d update(s) and had %d refused", len(accepted), len(refused))
	}
}

// An address that only ever appears under a per-interface alias still gets a
// PTR, and it points at the alias — the only name that resolves to it. Through
// v1002 it got nothing, which is why a gateway with three LANs published one
// PTR and left every reverse zone its operator ran untouched.
func TestAliasOnlyAddressGetsAPTRNamingTheAlias(t *testing.T) {
	f := &fakeDNS{zones: []string{"corp.internal"}}
	server := f.start(t)

	d, err := Diagnose(Params{
		Hostname: "node7",
		Domain:   "corp.internal",
		Servers:  []string{server},
		Reverse:  true,
	})
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	// Find an address that the primary name does not carry.
	primary := map[string]bool{}
	for _, v := range d.Forward {
		if v.Primary {
			for _, a := range v.Want {
				primary[a.String()] = true
			}
		}
	}
	var checked int
	for _, v := range d.PTR {
		if primary[v.Addr.String()] {
			continue
		}
		checked++
		if v.Want == "" {
			t.Errorf("%s gets no PTR target at all", v.Addr)
		}
		if strings.Contains(v.Action, "no PTR") {
			t.Errorf("%s was passed over: %s", v.Addr, v.Action)
		}
	}
	if checked == 0 {
		t.Skip("this machine has no alias-only address to check")
	}
}

// Where one address is carried by both the primary name and an alias, the PTR
// names the primary. A reverse lookup has one answer; that rule belongs to the
// address, which is where it is applied now, rather than to the host.
func TestSharedAddressPTRPrefersThePrimaryName(t *testing.T) {
	recs, err := collectRecords("node7", "corp.internal", nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	targets := ptrTargets(recs)
	seen := map[string]bool{}
	for _, tg := range targets {
		if seen[tg.addr.String()] {
			t.Errorf("%s has more than one PTR target", tg.addr)
		}
		seen[tg.addr.String()] = true
	}
	for _, rs := range recs {
		if !rs.primary {
			continue
		}
		for _, a := range rs.addrs {
			for _, tg := range targets {
				if tg.addr == a && tg.fqdn != rs.fqdn {
					t.Errorf("PTR for %s names %s, want the primary %s", a, tg.fqdn, rs.fqdn)
				}
			}
		}
	}
}

// Reverse switched off is a configuration answer, and the run that produces no
// PTRs because of it looks exactly like every other run that produces none.
func TestDiagnoseNamesReverseBeingOff(t *testing.T) {
	f := &fakeDNS{zones: []string{"corp.internal"}}
	server := f.start(t)

	d, err := Diagnose(Params{
		Hostname: "node7",
		Domain:   "corp.internal",
		Servers:  []string{server},
		Reverse:  false,
	})
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if len(d.PTR) == 0 {
		t.Skip("this machine has no address worth publishing")
	}
	for _, v := range d.PTR {
		if !strings.Contains(v.Action, "switched off") {
			t.Errorf("verdict for %s does not say reverse is off: %s", v.Addr, v.Action)
		}
	}
	if strings.Contains(d.String(), "reverse:  on") {
		t.Error("the header claims reverse is on")
	}
}

// A missing reverse delegation is named as one, in the rendered output an
// operator actually reads.
func TestDiagnoseRendersTheMissingDelegation(t *testing.T) {
	f := &fakeDNS{zones: []string{"corp.internal", "in-addr.arpa", "ip6.arpa"}}
	server := f.start(t)

	d, err := Diagnose(Params{
		Hostname: "node7",
		Domain:   "corp.internal",
		Servers:  []string{server},
		Reverse:  true,
	})
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	out := d.String()
	if !strings.Contains(out, "no reverse zone for this address is served here") {
		t.Errorf("the rendered diagnosis does not name the missing delegation:\n%s", out)
	}
	if !strings.Contains(out, "forward") || !strings.Contains(out, "reverse") {
		t.Errorf("the rendered diagnosis is missing a section:\n%s", out)
	}
}

// A PTR that is already right is published, not absent. The summary line
// counted forward names only, so a node with working PTRs and a node with none
// reported the same figures.
func TestSteadyStatePTRIsReportedAsPublished(t *testing.T) {
	addrs := hostAddrs(t)
	var revZones []string
	for _, a := range addrs {
		if a.Is4() {
			revZones = append(revZones, shortReverseZone(a))
		}
	}
	if len(revZones) == 0 {
		t.Skip("no IPv4 address on this machine")
	}
	f := &fakeDNS{zones: append([]string{"corp.internal"}, revZones...)}
	server := f.start(t)

	res, err := Register(Params{
		Hostname: "node7",
		Domain:   "corp.internal",
		Servers:  []string{server},
		Reverse:  true,
	}, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var ptrs int
	for _, p := range res.Published {
		if strings.HasSuffix(p, " PTR") {
			ptrs++
		}
	}
	if ptrs == 0 {
		t.Errorf("no PTR appears in Published: %v", res.Published)
	}
}
