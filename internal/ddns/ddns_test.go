package ddns

import (
	"encoding/base64"
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncodeNameWireFormat(t *testing.T) {
	got, err := encodeName("node7.corp.internal")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := []byte{5, 'n', 'o', 'd', 'e', '7', 4, 'c', 'o', 'r', 'p', 8, 'i', 'n', 't', 'e', 'r', 'n', 'a', 'l', 0}
	if string(got) != string(want) {
		t.Errorf("encodeName = %v, want %v", got, want)
	}
	// A trailing dot is the same name, not an empty final label.
	dotted, err := encodeName("node7.corp.internal.")
	if err != nil || string(dotted) != string(want) {
		t.Errorf("a trailing root dot changed the encoding: %v (%v)", dotted, err)
	}
	if root, err := encodeName("."); err != nil || len(root) != 1 || root[0] != 0 {
		t.Errorf("the root is not a single zero byte: %v (%v)", root, err)
	}
	// An empty label is a malformed name and must not encode to a truncated
	// one: "a..b" silently becoming "a" would publish a record at the wrong
	// name, which is worse than refusing.
	if _, err := encodeName("a..b"); err == nil {
		t.Error("a name with an empty label was accepted")
	}
	if _, err := encodeName(strings.Repeat("x", 64) + ".example"); err == nil {
		t.Error("a label longer than 63 bytes was accepted")
	}
}

// The update message has to be shaped the way a server expects to read it, and
// almost every field here is one nothing else in this package would catch: a
// wrong opcode or a count in the wrong slot produces a message that is refused
// with no explanation of which byte was wrong.
func TestBuildUpdateHeaderAndSections(t *testing.T) {
	addr := netip.MustParseAddr("10.1.1.7")
	rtype, rdata := rdataAddr(addr)
	id, msg, err := buildUpdate("corp.internal", []rr{
		deleteRRset("node7.corp.internal", rtype),
		addRecord("node7.corp.internal", rtype, 900, rdata),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if binary.BigEndian.Uint16(msg[0:2]) != id {
		t.Error("the header id is not the one returned for matching the reply")
	}
	flags := binary.BigEndian.Uint16(msg[2:4])
	if op := (flags >> 11) & 0xF; op != opcodeUpdate {
		t.Errorf("opcode = %d, want %d (UPDATE)", op, opcodeUpdate)
	}
	if zo := binary.BigEndian.Uint16(msg[4:6]); zo != 1 {
		t.Errorf("ZOCOUNT = %d, want 1", zo)
	}
	if pr := binary.BigEndian.Uint16(msg[6:8]); pr != 0 {
		t.Errorf("PRCOUNT = %d, want 0 — a prerequisite would make this fail rather than converge", pr)
	}
	// The updates go in the section counted by NSCOUNT, not ANCOUNT. Putting
	// them in the wrong one is the classic way to write an UPDATE that every
	// server ignores.
	if up := binary.BigEndian.Uint16(msg[8:10]); up != 2 {
		t.Errorf("UPCOUNT = %d, want 2 (delete + add)", up)
	}
	if ad := binary.BigEndian.Uint16(msg[10:12]); ad != 0 {
		t.Errorf("ARCOUNT = %d, want 0 on an unsigned update", ad)
	}
	// The zone section carries type SOA, which is what selects the zone.
	zname, _ := encodeName("corp.internal")
	if want := append(append([]byte{}, zname...), 0, byte(typeSOA), 0, classIN); string(msg[12:12+len(want)]) != string(want) {
		t.Error("the zone section is not <zone> SOA IN")
	}
}

// Delete-then-add, not add alone. A node that changed address must not end up
// with two A records, one of which is a lie.
func TestDeleteRRsetIsClassAnyWithNoRdata(t *testing.T) {
	d := deleteRRset("node7.corp.internal", typeA)
	if d.class != classAny {
		t.Errorf("delete class = %d, want ANY(255) so every record of the type goes", d.class)
	}
	if len(d.rdata) != 0 || d.ttl != 0 {
		t.Error("a delete-RRset carries no rdata and no TTL")
	}
	b, err := d.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := binary.BigEndian.Uint16(b[len(b)-2:]); got != 0 {
		t.Errorf("rdlength = %d, want 0", got)
	}
}

// TSIG is the part with no forgiving failure mode: a MAC computed over the
// wrong bytes is refused by every server with BADSIG and nothing else to go on.
func TestTSIGSigning(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("hunter2-hunter2-hunter2-hunter2!"))
	k, err := ParseKey("gravinet:" + secret)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if k.Algorithm != tsigSHA256 {
		t.Errorf("default algorithm = %q, want hmac-sha256", k.Algorithm)
	}
	id, msg, err := buildUpdate("corp.internal", []rr{deleteRRset("a.corp.internal", typeA)})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	before := binary.BigEndian.Uint16(msg[10:12])
	signed, err := sign(msg, id, *k, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if len(signed) <= len(msg) {
		t.Fatal("signing appended nothing")
	}
	// ARCOUNT counts the TSIG record.
	if got := binary.BigEndian.Uint16(signed[10:12]); got != before+1 {
		t.Errorf("ARCOUNT = %d, want %d — the server will not look for the TSIG record", got, before+1)
	}
	// The message ahead of the TSIG record is untouched: the MAC covers it as
	// sent, so a signer that rewrote any of it would sign one message and
	// transmit another.
	if string(signed[12:len(msg)]) != string(msg[12:]) {
		t.Error("signing altered the message body")
	}
	// The record is the last thing in the message, is class ANY, TTL 0, and
	// carries the original id so a server can restore it.
	tail := signed[len(msg):]
	kname, _ := encodeName("gravinet")
	if !strings.HasPrefix(string(tail), string(kname)) {
		t.Error("the TSIG record is not owned by the key name")
	}
	off := len(kname)
	if rt := binary.BigEndian.Uint16(tail[off : off+2]); rt != typeTSIG {
		t.Errorf("TSIG type = %d, want 250", rt)
	}
	if cl := binary.BigEndian.Uint16(tail[off+2 : off+4]); cl != classAny {
		t.Errorf("TSIG class = %d, want ANY(255)", cl)
	}
	if ttl := binary.BigEndian.Uint32(tail[off+4 : off+8]); ttl != 0 {
		t.Errorf("TSIG TTL = %d, want 0 — it must never be cached", ttl)
	}
	// Deterministic for a fixed time: same input, same MAC. This is what makes
	// a signature verifiable at all.
	again, _ := sign(msg, id, *k, time.Unix(1700000000, 0))
	if string(again) != string(signed) {
		t.Error("signing the same message twice produced different bytes")
	}
	// And the MAC actually depends on the message.
	_, other, _ := buildUpdate("corp.internal", []rr{deleteRRset("b.corp.internal", typeA)})
	otherSigned, _ := sign(other, id, *k, time.Unix(1700000000, 0))
	if string(otherSigned[len(other):]) == string(tail) {
		t.Error("two different messages produced the same MAC")
	}
}

func TestParseKeyForms(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	if k, err := ParseKey(""); err != nil || k != nil {
		t.Errorf("an empty key spec is not an error, it is 'unsigned': %v %v", k, err)
	}
	k, err := ParseKey("kname:" + secret + ":hmac-sha512")
	if err != nil || k.Algorithm != tsigSHA512 {
		t.Errorf("explicit algorithm not honoured: %v %v", k, err)
	}
	// The spellings that turn up in key files and documentation.
	for _, spelling := range []string{"HMAC-SHA256", "sha256", "hmac_sha256", " hmac-sha256 "} {
		if got := normalizeAlgorithm(spelling); got != tsigSHA256 {
			t.Errorf("normalizeAlgorithm(%q) = %q", spelling, got)
		}
	}
	if _, err := ParseKey("kname:not-base64!!"); err == nil {
		t.Error("a secret that is not base64 was accepted")
	}
	if _, err := ParseKey("kname:" + secret + ":hmac-md5"); err == nil {
		t.Error("md5 was accepted; RFC 8945 lists it as optional and nothing needs it")
	}
	if _, err := ParseKey("no-colon-here"); err == nil {
		t.Error("a spec that is neither a file nor name:secret was accepted")
	}

	// The BIND key-file form, which is what tsig-keygen writes.
	dir := t.TempDir()
	path := filepath.Join(dir, "k.key")
	os.WriteFile(path, []byte("key \"tech\" {\n\talgorithm hmac-sha384;\n\tsecret \""+secret+"\";\n};\n"), 0o600)
	fk, err := ParseKey(path)
	if err != nil {
		t.Fatalf("key file: %v", err)
	}
	if fk.Name != "tech" || fk.Algorithm != tsigSHA384 {
		t.Errorf("key file parsed as %+v", fk)
	}
}

// The reverse name is the one place an off-by-one produces a PTR in a zone that
// exists, for an address that is not this one.
func TestReverseName(t *testing.T) {
	name, zone := reverseName(netip.MustParseAddr("192.0.2.37"))
	if name != "37.2.0.192.in-addr.arpa" {
		t.Errorf("reverse name = %q", name)
	}
	if zone != "2.0.192.in-addr.arpa" {
		t.Errorf("reverse zone = %q, want the /24", zone)
	}
	name6, zone6 := reverseName(netip.MustParseAddr("2001:db8::1"))
	if !strings.HasSuffix(name6, ".ip6.arpa") || !strings.HasPrefix(name6, "1.0.0.0") {
		t.Errorf("v6 reverse name = %q", name6)
	}
	// The /64 is the top 64 bits, which is the last 16 of the 32 reversed
	// nibbles. Spelled out rather than counted: the count is 16 nibbles but 17
	// dots once ".ip6.arpa" is on the end, and a test that has to explain its
	// own arithmetic is not checking the thing it claims to.
	if want := "0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa"; zone6 != want {
		t.Errorf("v6 reverse zone = %q, want %q", zone6, want)
	}
}

// Interface names are not DNS labels. A tagged interface is "eth1.22", whose
// dot would put the alias in a subdomain that does not exist.
func TestSanitizeLabel(t *testing.T) {
	for in, want := range map[string]string{
		"eth0":    "eth0",
		"eth1.22": "eth1-22",
		"br-lan":  "br-lan",
		"ETH0":    "eth0",
		"_weird_": "weird",
	} {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// Register refuses rather than half-working when the three things it needs are
// not all there. This is the state a node sits in before anyone fills the
// resolver page in, so the message has to say which piece is missing.
func TestRegisterNeedsAllThreeInputs(t *testing.T) {
	for name, p := range map[string]Params{
		"no hostname": {Domain: "corp.internal", Servers: []string{"10.0.0.1"}},
		"no domain":   {Hostname: "node7", Servers: []string{"10.0.0.1"}},
		"no servers":  {Hostname: "node7", Domain: "corp.internal"},
	} {
		if _, err := Register(p, nil); err == nil {
			t.Errorf("%s: registered anyway", name)
		}
	}
}

// The rcodes an operator will actually hit have to explain themselves. "update
// failed: rcode 5" sends somebody to a packet capture; REFUSED with the reason
// sends them to their zone's update policy.
func TestRcodeTextExplainsTheConfigurationCases(t *testing.T) {
	for rc, want := range map[int]string{
		rcodeRefused: "dynamic updates",
		rcodeNotAuth: "TSIG",
		16:           "signature",
		17:           "key name",
		18:           "clock",
	} {
		if got := rcodeText(rc); !strings.Contains(got, want) {
			t.Errorf("rcodeText(%d) = %q, want it to mention %q", rc, got, want)
		}
	}
}

// A response has to survive being read: a malformed or truncated one is a
// remote input, and this parser runs on whatever a DNS server sent.
func TestParseResponseToleratesRubbish(t *testing.T) {
	for _, junk := range [][]byte{
		nil,
		{1, 2, 3},
		make([]byte, 12),
		append(make([]byte, 12), 0xC0, 0x0C), // a compression pointer loop
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("parseResponse panicked on %d bytes: %v", len(junk), r)
				}
			}()
			parseResponse(junk)
		}()
	}
}

// The reverse records are checked and maintained on their own schedule, not as
// a side effect of a forward record changing.
//
// Checked as structure rather than over a socket: standing up a fake
// authoritative server to prove this would test the fake. What went wrong was
// control flow — the PTR sat inside the branch taken only when the forward
// record had just been written — and control flow is what this pins.
func TestPTRIsNotConditionalOnTheForwardRecordChanging(t *testing.T) {
	src := mustReadSource(t, "register.go")

	// The reverse pass is its own loop over the records, after the forward
	// loop has finished, rather than a branch inside it.
	fwd := strings.Index(src, "for _, rs := range records {")
	rev := strings.Index(src, "if p.Reverse {")
	if fwd < 0 || rev < 0 || rev < fwd {
		t.Fatal("the reverse pass is not a separate step after the forward one")
	}
	if strings.Contains(between(t, src, "for _, rs := range records {", "\n\t}\n"), "syncPTR(") {
		t.Error("the PTR is written from inside the forward loop again")
	}

	// And the reverse path reads before it writes, like the forward one.
	sync := between(t, src, "func syncPTR(", "\n}\n")
	cur := strings.Index(sync, "currentRecord(master, rev, typePTR)")
	send := strings.Index(sync, "send(master, zone,")
	if cur < 0 {
		t.Fatal("syncPTR does not read the published PTR before writing one")
	}
	if send < 0 || cur > send {
		t.Fatal("syncPTR writes before it reads")
	}
	if !strings.Contains(sync, "return false, nil") {
		t.Error("syncPTR has no path that reports 'already correct, wrote nothing'")
	}
}

// A forward set that failed to land must not get a pointer aimed at it.
func TestNoPTRWhenTheForwardRecordFailed(t *testing.T) {
	src := mustReadSource(t, "register.go")
	if !strings.Contains(src, "failedForward(res.Errors, t.fqdn, t.rtype)") {
		t.Error("the reverse pass no longer checks whether the forward set failed this run")
	}
}

// Reading before writing is the difference between a run that costs a query
// and one that bumps the zone serial every interval on every gateway.
func TestForwardRecordIsReadBeforeItIsWritten(t *testing.T) {
	loop := between(t, mustReadSource(t, "register.go"), "for _, rs := range records {", "\n\t}\n")
	cur := strings.Index(loop, "currentRecords(master, rs.fqdn, rs.rtype)")
	send := strings.Index(loop, "send(master, zone, upd, p.Key)")
	if cur < 0 || send < 0 || cur > send {
		t.Fatal("the forward set is written without being read first")
	}
	// A failed lookup asserts rather than assumes: the cost of a redundant
	// write is one update, the cost of assuming the record is fine is a name
	// that never resolves because a lookup timed out once.
	if !strings.Contains(loop, "qerr == nil && sameAddrSet(current, rs.addrs)") {
		t.Error("a failed lookup is no longer treated as 'unknown, write anyway'")
	}
}

// Both families, under the same name. This is what a dual-stack node is for,
// and the bug it replaces gave hostname.domain whichever family the kernel
// happened to enumerate first and nothing of the other.
func TestPrimaryNameGetsOneRecordPerFamily(t *testing.T) {
	src := mustReadSource(t, "register.go")
	if !strings.Contains(src, "primaryTaken := map[int]bool{}") {
		t.Fatal("the primary name is claimed once for the whole host again, so a dual-stack node publishes only one family under it")
	}
	if !strings.Contains(src, "primaryTaken[rtype]") {
		t.Error("the primary claim is not per record type")
	}
}

// A name with two addresses of one family is one delete and two adds, in one
// message. Written as two separate updates, the second one's delete throws away
// what the first just published and the name keeps only the last address.
func TestOneUpdatePerNameAndType(t *testing.T) {
	loop := between(t, mustReadSource(t, "register.go"), "for _, rs := range records {", "\n\t}\n")
	del := strings.Count(loop, "deleteRRset(rs.fqdn, rs.rtype)")
	if del != 1 {
		t.Errorf("the update carries %d deletes, want exactly 1 for the whole set", del)
	}
	if !strings.Contains(loop, "for _, a := range rs.addrs {") {
		t.Error("the update does not add every address in the set")
	}
}

// Order is not identity. DNS answers arrive in whatever order the server felt
// like, quite often deliberately rotated, so a positional comparison would
// rewrite a correct record on most runs.
func TestSameAddrSetIgnoresOrder(t *testing.T) {
	a := netip.MustParseAddr("10.0.0.1")
	b := netip.MustParseAddr("10.0.0.2")
	if !sameAddrSet([]netip.Addr{a, b}, []netip.Addr{b, a}) {
		t.Error("a reordered answer was treated as a difference")
	}
	if sameAddrSet([]netip.Addr{a}, []netip.Addr{a, b}) {
		t.Error("a subset was treated as equal, so a stale extra address would never be removed")
	}
	if sameAddrSet([]netip.Addr{a, b}, []netip.Addr{a, a}) {
		t.Error("different members compared equal")
	}
	if !sameAddrSet(nil, nil) {
		t.Error("two empty sets differ")
	}
}

func mustReadSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("could not find %q", start)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("could not find %q after %q", end, start)
	}
	return rest[:j]
}

// The TTL is published as given, including zero. This package used to swap a
// zero for 900, which made an uncacheable record impossible to ask for and put
// the default in a place the config file could not show.
func TestTTLIsTakenLiterally(t *testing.T) {
	src := mustReadSource(t, "register.go")
	if strings.Contains(src, "ttl = DefaultTTL") {
		t.Error("Register still substitutes a default for a zero TTL")
	}
	body := between(t, src, "func Register(", "\n}\n")
	if !strings.Contains(body, "ttl := p.TTL") {
		t.Fatal("Register no longer takes the TTL from its params")
	}
	// Nothing between reading it and using it may change it.
	seg := between(t, body, "ttl := p.TTL", "collectRecords(")
	if strings.Contains(seg, "ttl =") {
		t.Errorf("the TTL is rewritten after being read: %s", seg)
	}
}

// A spec that is plainly not an inline key is refused with the form that is,
// rather than with a grammar listing one the help no longer mentions.
//
// The last case is why this matches on separators rather than on a drive
// letter: "k:secret" is a legal inline key, not a Windows path.
func TestParseKeyRefusesAPathWithTheInlineForm(t *testing.T) {
	for _, spec := range []string{"/etc/gravinet/tsig.key", `C:\keys\tsig.key`, "./tsig.key", "~/tsig.key"} {
		if _, err := ParseKey(spec); err == nil || !strings.Contains(err.Error(), "is not a TSIG key") {
			t.Errorf("ParseKey(%q) = %v, want it to name the missing file", spec, err)
		}
	}
	k, err := ParseKey("k:c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0Cg==")
	if err != nil || k.Name != "k" {
		t.Errorf("ParseKey with a one-character name = %v, %v; want it read as an inline key", k, err)
	}
}
