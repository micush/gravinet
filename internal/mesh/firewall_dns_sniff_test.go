package mesh

import (
	"net/netip"
	"testing"
	"time"
)

func TestLiteralFQDNNamesFiltersWildcards(t *testing.T) {
	got := literalFQDNNames([]string{"example.com, *.example.com", "*.wild.net", "plain.org"})
	want := map[string]bool{"example.com": true, "plain.org": true}
	if len(got) != len(want) {
		t.Fatalf("literalFQDNNames = %v, want exactly %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected literal name %q (wildcard entries must be filtered out)", n)
		}
	}
}

// ---- pattern matching ----

func TestFqdnPatternMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.example.com", "foo.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", true},     // wildcard now also matches the bare parent
		{"*.example.com", "notexample.com", false}, // suffix match without the dot boundary must not count
		{"*.example.com", "evilexample.com", false},
		{"*.EXAMPLE.com", "foo.example.COM", true},  // case-insensitive
		{"*.example.com.", "foo.example.com", true}, // trailing root dot on the pattern ignored
		{"*.example.com", "foo.example.com.", true}, // trailing root dot on the name ignored
		{"example.com", "example.com", true},        // literal exact match
		{"example.com", "foo.example.com", false},   // literal does not match subdomains
		{"", "example.com", false},
		{"example.com", "", false},
		{"*.", "foo.com", false}, // degenerate empty-suffix wildcard matches nothing
	}
	for _, c := range cases {
		if got := fqdnPatternMatch(c.pattern, c.name); got != c.want {
			t.Errorf("fqdnPatternMatch(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestIsWildcardFQDN(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"*.example.com", true},
		{"  *.example.com  ", true},
		{"example.com", false},
		{"*example.com", false}, // no dot after the star: not a real DNS wildcard, treated as a (failing) literal
		{"sub*.example.com", false},
		{"*", false},
	}
	for _, c := range cases {
		if got := isWildcardFQDN(c.in); got != c.want {
			t.Errorf("isWildcardFQDN(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// ---- DNS wire-format parsing ----

// dnsLabelName encodes name (dot-separated, no trailing dot) as an
// uncompressed sequence of length-prefixed labels terminated by a zero
// byte — the straightforward encoding every DNS message can always fall
// back to, used here to hand-build well-formed test messages.
func dnsLabelName(name string) []byte {
	if name == "" {
		return []byte{0}
	}
	var out []byte
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			out = append(out, byte(i-start))
			out = append(out, name[start:i]...)
			start = i + 1
		}
	}
	out = append(out, 0)
	return out
}

// buildDNSResponse hand-builds a minimal, well-formed DNS response: one
// question (qname/A, or AAAA if any answer address is v6), and one A/AAAA
// answer per (name, addr) pair, each with the given ttl. Every answer name
// is encoded fully (no compression) — TestDNSParseResponseCompressed covers
// the compressed-pointer path separately, by hand, for precision.
func buildDNSResponse(qname string, answers []dnsAnswer, ttl uint32) []byte {
	msg := make([]byte, 12)
	msg[2] = 0x80 // QR=1 (response), rest of flags zero
	putU16 := func(b []byte, v uint16) { b[0], b[1] = byte(v>>8), byte(v) }
	putU16(msg[4:6], 1)                    // QDCOUNT
	putU16(msg[6:8], uint16(len(answers))) // ANCOUNT

	msg = append(msg, dnsLabelName(qname)...)
	qtype := uint16(1)
	for _, a := range answers {
		if a.addr.Is6() {
			qtype = 28
		}
	}
	qt := make([]byte, 4)
	putU16(qt[0:2], qtype)
	putU16(qt[2:4], 1) // QCLASS IN
	msg = append(msg, qt...)

	for _, a := range answers {
		msg = append(msg, dnsLabelName(a.name)...)
		rr := make([]byte, 10)
		typ := uint16(1)
		var rdata []byte
		if a.addr.Is6() {
			typ = 28
			b := a.addr.As16()
			rdata = b[:]
		} else {
			b := a.addr.As4()
			rdata = b[:]
		}
		putU16(rr[0:2], typ)
		putU16(rr[2:4], 1) // CLASS IN
		rr[4], rr[5], rr[6], rr[7] = byte(ttl>>24), byte(ttl>>16), byte(ttl>>8), byte(ttl)
		putU16(rr[8:10], uint16(len(rdata)))
		msg = append(msg, rr...)
		msg = append(msg, rdata...)
	}
	return msg
}

func TestDNSParseResponseBasic(t *testing.T) {
	addr := netip.MustParseAddr("93.184.216.34")
	msg := buildDNSResponse("example.com", []dnsAnswer{{name: "example.com", addr: addr}}, 300)
	got := dnsParseResponse(msg)
	if len(got) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(got))
	}
	if got[0].name != "example.com" {
		t.Errorf("name = %q, want example.com", got[0].name)
	}
	if got[0].addr != addr {
		t.Errorf("addr = %v, want %v", got[0].addr, addr)
	}
	if got[0].ttl != 300*time.Second {
		t.Errorf("ttl = %v, want 300s", got[0].ttl)
	}
}

func TestDNSParseResponseMultipleAnswersAndAAAA(t *testing.T) {
	a4 := netip.MustParseAddr("10.1.2.3")
	a6 := netip.MustParseAddr("2001:db8::1")
	msg := buildDNSResponse("multi.example.com", []dnsAnswer{
		{name: "multi.example.com", addr: a4},
		{name: "multi.example.com", addr: a6},
	}, 60)
	got := dnsParseResponse(msg)
	if len(got) != 2 {
		t.Fatalf("expected 2 answers, got %d: %+v", len(got), got)
	}
	if got[0].addr != a4 || got[1].addr != a6 {
		t.Errorf("addrs = %v, %v; want %v, %v", got[0].addr, got[1].addr, a4, a6)
	}
}

func TestDNSParseResponseRejectsQuery(t *testing.T) {
	msg := buildDNSResponse("example.com", []dnsAnswer{{name: "example.com", addr: netip.MustParseAddr("1.2.3.4")}}, 60)
	msg[2] &^= 0x80 // clear QR: make it look like a query
	if got := dnsParseResponse(msg); got != nil {
		t.Errorf("expected nil for a query message, got %+v", got)
	}
}

// TestDNSParseResponseCompressed hand-builds a message where the answer's
// name is a compression pointer back to the question name — the
// overwhelmingly common real-world shape (every stub/recursive resolver
// compresses this way) and the case a naive "just re-decode the label
// bytes" implementation would get wrong.
func TestDNSParseResponseCompressed(t *testing.T) {
	msg := make([]byte, 12)
	msg[2] = 0x80
	msg[4], msg[5] = 0, 1 // QDCOUNT=1
	msg[6], msg[7] = 0, 1 // ANCOUNT=1
	qnameOff := len(msg)
	msg = append(msg, dnsLabelName("cdn.example.com")...)
	msg = append(msg, 0, 1, 0, 1) // QTYPE=A, QCLASS=IN

	// Answer name: a compression pointer back to qnameOff.
	msg = append(msg, 0xc0|byte(qnameOff>>8), byte(qnameOff))
	msg = append(msg, 0, 1, 0, 1)  // TYPE=A, CLASS=IN
	msg = append(msg, 0, 0, 0, 60) // TTL=60
	msg = append(msg, 0, 4)        // RDLENGTH=4
	ip := netip.MustParseAddr("203.0.113.9").As4()
	msg = append(msg, ip[:]...)

	got := dnsParseResponse(msg)
	if len(got) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(got))
	}
	if got[0].name != "cdn.example.com" {
		t.Errorf("name = %q, want cdn.example.com (via compression pointer)", got[0].name)
	}
	if got[0].addr.String() != "203.0.113.9" {
		t.Errorf("addr = %v, want 203.0.113.9", got[0].addr)
	}
}

// TestDNSNameRejectsForwardPointer confirms a pointer that doesn't point
// strictly backward is rejected rather than followed — the case that would
// otherwise let a crafted message loop forever.
func TestDNSNameRejectsForwardPointer(t *testing.T) {
	msg := []byte{0xc0, 0x05, 0, 0, 0, 3, 'a', 'b', 'c', 0} // pointer at 0 -> offset 5 (forward)
	if _, _, ok := dnsName(msg, 0); ok {
		t.Fatal("expected a forward pointer to be rejected")
	}
	self := []byte{0xc0, 0x00} // pointer at 0 -> offset 0 (self-loop)
	if _, _, ok := dnsName(self, 0); ok {
		t.Fatal("expected a self-pointing pointer to be rejected")
	}
}

func TestDNSParseResponseMalformedSafe(t *testing.T) {
	good := buildDNSResponse("example.com", []dnsAnswer{{name: "example.com", addr: netip.MustParseAddr("1.2.3.4")}}, 60)
	// Every truncation point must fail cleanly (nil or a partial, never valid
	// answers out of thin air, and — the actual point of this test — never a
	// panic on any of these lengths).
	for n := 0; n <= len(good); n++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("dnsParseResponse panicked on a %d-byte truncation: %v", n, r)
				}
			}()
			dnsParseResponse(good[:n])
		}()
	}
	// A handful of specifically-corrupted headers.
	corrupt := [][]byte{
		nil,
		{0, 1, 2}, // far too short
		append([]byte{0, 0, 0x80, 0}, make([]byte, 8)...), // QR set, all counts zero-ish garbage follows
	}
	for _, c := range corrupt {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("dnsParseResponse panicked on corrupt input %v: %v", c, r)
				}
			}()
			dnsParseResponse(c)
		}()
	}
}

// ---- TTL cache ----

func TestWildcardFQDNCacheExpiry(t *testing.T) {
	c := &wildcardFQDNCache{}
	a1 := netip.MustParseAddr("10.0.0.1")
	a2 := netip.MustParseAddr("10.0.0.2")

	c.record("cdn", a1, 10*time.Second)
	c.record("cdn", a2, 100*time.Second)

	live := c.sweep(time.Now().Add(50 * time.Second))
	got := live["cdn"]
	if len(got) != 1 || got[0].Addr() != a2 {
		t.Fatalf("after a1's TTL elapsed, expected only a2 left, got %v", got)
	}

	live = c.sweep(time.Now().Add(200 * time.Second))
	if len(live["cdn"]) != 0 {
		t.Fatalf("expected everything expired, got %v", live["cdn"])
	}
}

func TestWildcardFQDNCacheLaterLongerTTLWins(t *testing.T) {
	c := &wildcardFQDNCache{}
	a := netip.MustParseAddr("10.0.0.1")
	c.record("obj", a, 5*time.Second)
	c.record("obj", a, 500*time.Second) // a second, longer-lived observation of the same address
	live := c.sweep(time.Now().Add(60 * time.Second))
	if len(live["obj"]) != 1 {
		t.Fatalf("expected the longer TTL to keep the address alive at +60s, got %v", live["obj"])
	}
}

// ---- end-to-end firewall integration ----

// buildUDPPacket wraps payload in a minimal IPv4 UDP packet with the given
// addresses and ports — enough for l4Payload/parseL4 to find the payload,
// which is all firewall.allow's DNS-sniff hook needs.
func buildUDPPacket(src, dst netip.Addr, srcPort, dstPort uint16, payload []byte) []byte {
	total := 20 + 8 + len(payload)
	p := make([]byte, total)
	p[0] = 0x45
	p[2], p[3] = byte(total>>8), byte(total)
	p[8] = 64
	p[9] = 17 // UDP
	s, d := src.As4(), dst.As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	p[20], p[21] = byte(srcPort>>8), byte(srcPort)
	p[22], p[23] = byte(dstPort>>8), byte(dstPort)
	udpLen := 8 + len(payload)
	p[24], p[25] = byte(udpLen>>8), byte(udpLen)
	copy(p[28:], payload)
	return p
}

func TestFirewallWildcardFQDNEndToEnd(t *testing.T) {
	a := netip.MustParseAddr
	f := newFirewall(nil)
	f.setCatalog([]FirewallObject{{Name: "cdn", Kind: "fqdn", Addresses: []string{"*.example.com"}}}, nil)
	f.loadRules([]FirewallRule{{Action: "deny", Dst: "cdn"}})

	dnsServer := a("8.8.8.8")
	client := a("10.0.0.5")
	cdnIP := a("203.0.113.9")

	// Before any DNS traffic is observed, the object resolves to nothing —
	// a rule constrained to it must match nothing, not widen to "any".
	if !f.allow(fwOut, buildUDPPacket(client, cdnIP, 55555, 443, nil)) {
		t.Fatal("before any DNS is observed, the wildcard object should match nothing (traffic allowed)")
	}

	// A DNS response naming a subdomain of example.com crosses the
	// firewall (this is the passive tap — observing it must not itself
	// affect the allow/deny decision for the DNS packet itself).
	resp := buildDNSResponse("cdn.example.com", []dnsAnswer{{name: "cdn.example.com", addr: cdnIP}}, 60)
	if !f.allow(fwIn, buildUDPPacket(dnsServer, client, 53, 55555, resp)) {
		t.Fatal("the DNS response packet itself should be allowed (no rule blocks DNS here)")
	}

	// Sweeping promotes the sniffed address into the live catalog.
	f.sweepWildcardFQDN(time.Now())

	if f.allow(fwOut, buildUDPPacket(client, cdnIP, 55555, 443, nil)) {
		t.Fatal("after the sweep, traffic to the sniffed address should be denied")
	}
	if !f.allow(fwOut, buildUDPPacket(client, a("203.0.113.99"), 55555, 443, nil)) {
		t.Fatal("an unrelated address must still be allowed")
	}

	// A DNS answer for the bare parent domain (not a subdomain) now also
	// populates the *.example.com object — verified through the whole
	// pipeline, not just the unit-level matcher (see fqdnPatternMatch's
	// doc comment: the wildcard reads as "this domain and everything
	// under it").
	bareIP := a("198.51.100.7")
	respBare := buildDNSResponse("example.com", []dnsAnswer{{name: "example.com", addr: bareIP}}, 60)
	f.allow(fwIn, buildUDPPacket(dnsServer, client, 53, 55556, respBare))
	f.sweepWildcardFQDN(time.Now())
	if f.allow(fwOut, buildUDPPacket(client, bareIP, 55555, 443, nil)) {
		t.Fatal("a DNS answer for the bare parent domain should now match *.example.com (traffic to it should be denied)")
	}

	// A malformed/spoofed packet that has the right source port (53) but
	// isn't actually a response (QR clear) must not populate anything —
	// this exercises dnsParseResponse's QR check specifically, as opposed
	// to just being filtered by allow()'s own sp==53 gate before ever
	// reaching the parser.
	queryLikePayload := buildDNSResponse("evil.example.com", []dnsAnswer{{name: "evil.example.com", addr: a("192.0.2.1")}}, 60)
	queryLikePayload[2] &^= 0x80 // clear QR
	f.allow(fwIn, buildUDPPacket(dnsServer, client, 53, 55557, queryLikePayload))
	f.sweepWildcardFQDN(time.Now())
	if !f.allow(fwOut, buildUDPPacket(client, a("192.0.2.1"), 55555, 443, nil)) {
		t.Fatal("a non-response (QR=0) packet on port 53 must not populate the wildcard object (traffic to it should be allowed)")
	}
}

// TestFirewallFQDNObjectMixesLiteralAndWildcard verifies one object naming
// both a literal entry (resolved by the periodic poller, simulated here via
// setFQDN directly) and a wildcard entry (resolved by the sniffer) ends up
// constraining a rule to the union of both — neither path clobbers the
// other's contribution.
func TestFirewallFQDNObjectMixesLiteralAndWildcard(t *testing.T) {
	a := netip.MustParseAddr
	f := newFirewall(nil)
	f.setCatalog([]FirewallObject{{Name: "mixed", Kind: "fqdn", Addresses: []string{"literal.example.org", "*.wild.example.org"}}}, nil)
	f.loadRules([]FirewallRule{{Action: "deny", Dst: "mixed"}})

	literalIP := a("10.9.9.1")
	wildIP := a("10.9.9.2")
	client := a("10.0.0.5")

	// Literal side, as the periodic poller would apply it.
	if !f.setFQDN("mixed", []netip.Prefix{netip.PrefixFrom(literalIP, 32)}) {
		t.Fatal("setFQDN should report a change")
	}
	// Wildcard side, as the sniffer would apply it.
	resp := buildDNSResponse("cdn.wild.example.org", []dnsAnswer{{name: "cdn.wild.example.org", addr: wildIP}}, 60)
	f.allow(fwIn, buildUDPPacket(a("8.8.8.8"), client, 53, 55555, resp))
	f.sweepWildcardFQDN(time.Now())

	if f.allow(fwOut, buildUDPPacket(client, literalIP, 1, 443, nil)) {
		t.Fatal("the literal-resolved address should be denied")
	}
	if f.allow(fwOut, buildUDPPacket(client, wildIP, 1, 443, nil)) {
		t.Fatal("the wildcard-sniffed address should also be denied (union, not overwrite)")
	}
	if !f.allow(fwOut, buildUDPPacket(client, a("10.9.9.3"), 1, 443, nil)) {
		t.Fatal("an unrelated address should still be allowed")
	}
}

// TestFirewallNoWildcardObjectsSniffIsNoop confirms a network with no
// wildcard fqdn objects at all takes the fast-path bail in
// observeDNSResponse — indirectly, by checking the cache stays empty even
// after DNS traffic crosses the firewall.
func TestFirewallNoWildcardObjectsSniffIsNoop(t *testing.T) {
	a := netip.MustParseAddr
	f := newFirewall(nil)
	// No fqdn objects at all.
	resp := buildDNSResponse("anything.example.com", []dnsAnswer{{name: "anything.example.com", addr: a("1.2.3.4")}}, 60)
	f.allow(fwIn, buildUDPPacket(a("8.8.8.8"), a("10.0.0.1"), 53, 55555, resp))
	live := f.wcCache.sweep(time.Now())
	if len(live) != 0 {
		t.Fatalf("expected no cache entries with no wildcard objects configured, got %v", live)
	}
}
