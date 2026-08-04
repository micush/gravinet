package mesh

import (
	"net/netip"
	"reflect"
	"testing"
)

func ap(s string) netip.Addr { return netip.MustParseAddr(s) }

func keys(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.String())
	}
	return out
}

// TestSeedCandidatesKeepsSchemeAndPorts is the property the old path lacked.
// The seed syntax has always carried a protocol and a port list; expansion
// must be a parse, never an inference, because every inference in the old code
// was a guess at a fact only the peer knew.
func TestSeedCandidatesKeepsSchemeAndPorts(t *testing.T) {
	got, err := SeedCandidates("tcp://198.51.100.7:65432,443,23", []netip.Addr{ap("198.51.100.7")}, []uint16{9999}, "peer-a")
	if err != nil {
		t.Fatalf("SeedCandidates: %v", err)
	}
	want := []string{"tcp://198.51.100.7:65432", "tcp://198.51.100.7:443", "tcp://198.51.100.7:23"}
	if !reflect.DeepEqual(keys(got), want) {
		t.Fatalf("got %q, want %q", keys(got), want)
	}
	for _, c := range got {
		if c.Owner != "peer-a" || c.Src != SrcSeed {
			t.Fatalf("candidate %v lost its owner/source", c)
		}
	}
}

// A bare seed is UDP, matching config.SeedParts.
func TestSeedCandidatesDefaultsToUDP(t *testing.T) {
	got, _ := SeedCandidates("198.51.100.7:65432", []netip.Addr{ap("198.51.100.7")}, nil, "")
	if len(got) != 1 || got[0].Proto != ProtoUDP {
		t.Fatalf("got %q, want a single udp candidate", keys(got))
	}
}

// The no-port case is the ONLY place this node's own ports may enter, and it
// must stay confined there — that confinement is what stops one peer's port
// becoming another's.
func TestSeedCandidatesDefaultPortsOnlyWhenSeedHasNone(t *testing.T) {
	got, _ := SeedCandidates("198.51.100.7", []netip.Addr{ap("198.51.100.7")}, []uint16{65432, 443}, "")
	if want := []string{"udp://198.51.100.7:65432", "udp://198.51.100.7:443"}; !reflect.DeepEqual(keys(got), want) {
		t.Fatalf("got %q, want %q", keys(got), want)
	}
	// With a port given, defaults must not appear.
	got, _ = SeedCandidates("198.51.100.7:7777", []netip.Addr{ap("198.51.100.7")}, []uint16{65432, 443}, "")
	if want := []string{"udp://198.51.100.7:7777"}; !reflect.DeepEqual(keys(got), want) {
		t.Fatalf("got %q, want %q — defaults leaked into a seed that named its port", keys(got), want)
	}
}

func TestSeedCandidatesResolvesToEveryAddress(t *testing.T) {
	got, _ := SeedCandidates("tcp://seed.example:443", []netip.Addr{ap("198.51.100.7"), ap("2001:db8::1")}, nil, "")
	want := []string{"tcp://198.51.100.7:443", "tcp://[2001:db8::1]:443"}
	if !reflect.DeepEqual(keys(got), want) {
		t.Fatalf("got %q, want %q", keys(got), want)
	}
}

func TestSeedCandidatesRejectsBadPort(t *testing.T) {
	if _, err := SeedCandidates("198.51.100.7:70000", []netip.Addr{ap("198.51.100.7")}, nil, ""); err == nil {
		t.Fatal("accepted an out-of-range port")
	}
}

// TestProtocolIsPartOfIdentity is the crux. tcp/65432 and udp/65432 at one
// address are independent NAT mappings that can reach *different hosts*.
// Collapsing them is exactly what put two peers on one socket.
func TestProtocolIsPartOfIdentity(t *testing.T) {
	u := Candidate{Addr: ap("174.64.247.165"), Port: 65432, Proto: ProtoUDP}
	c := Candidate{Addr: ap("174.64.247.165"), Port: 65432, Proto: ProtoTCP}
	if u.Key() == c.Key() {
		t.Fatal("udp and tcp at the same address:port share a key; they are different mappings to possibly different hosts")
	}
}

// TestTwoPeersBehindOneNAT reconstructs the live failure. mcfed's seed list:
//
//	tcp://174.64.247.165:65432   "cox gn-cush1 nat phoenix"
//	     174.64.247.165:65432    "cox-gn-cush2 nat phoenix"
//
// Same IP, same port number, different protocol — an ordinary NAT setup, and
// the operator had already said which was which. The old engine derived a TCP
// candidate for the UDP seed, took its port from whichever live session shared
// the IP, and dialed into the other peer's listener every tick.
func TestTwoPeersBehindOneNAT(t *testing.T) {
	nat := []netip.Addr{ap("174.64.247.165")}
	c1, err := SeedCandidates("tcp://174.64.247.165:65432", nat, nil, "cush1")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := SeedCandidates("174.64.247.165:65432", nat, nil, "cush2")
	if err != nil {
		t.Fatal(err)
	}
	if len(c1) != 1 || len(c2) != 1 {
		t.Fatalf("expected one candidate each, got %q and %q", keys(c1), keys(c2))
	}
	if c1[0].Key() == c2[0].Key() {
		t.Fatal("the two peers collapsed onto one candidate — the exact bug")
	}
	if c1[0].Proto != ProtoTCP || c2[0].Proto != ProtoUDP {
		t.Fatalf("protocols lost: %v / %v", c1[0], c2[0])
	}
	// Merging both peers' seeds must keep them distinct.
	all := SortCandidates(append(append([]Candidate{}, c1...), c2...))
	if len(all) != 2 {
		t.Fatalf("merged to %q; two peers behind one NAT must stay two candidates", keys(all))
	}
}

// TestConflictsWithAnotherPeersSeed: a candidate that lands exactly on another
// peer's operator-configured seed is known-wrong before a socket is opened.
func TestConflictsWithAnotherPeersSeed(t *testing.T) {
	cush1Seed, _ := SeedCandidates("tcp://174.64.247.165:65432", []netip.Addr{ap("174.64.247.165")}, nil, "cush1")

	// What the old derivation manufactured for cush2: a TCP candidate at the
	// same address, wearing cush2's owner.
	derived := Candidate{Addr: ap("174.64.247.165"), Port: 65432, Proto: ProtoTCP, Src: SrcObserved, Owner: "cush2"}
	if !derived.ConflictsWith(cush1Seed) {
		t.Fatal("did not flag a candidate that lands on another peer's configured seed")
	}
	// cush1's own seed obviously doesn't conflict with itself.
	if cush1Seed[0].ConflictsWith(cush1Seed) {
		t.Fatal("a peer's own seed flagged as conflicting")
	}
	// An unowned candidate has nothing to contradict; it must stay dialable,
	// since refusing to dial is worse than a wrong guess — no answer means no
	// connectivity.
	unowned := Candidate{Addr: ap("174.64.247.165"), Port: 65432, Proto: ProtoTCP, Src: SrcObserved}
	if unowned.ConflictsWith(cush1Seed) {
		t.Fatal("an unowned candidate was disqualified; it has no owner to contradict")
	}
	// A shared *observed* endpoint is ordinary NAT behaviour and says nothing
	// about who answers — only seeds are authoritative enough to disqualify.
	obs := []Candidate{{Addr: ap("174.64.247.165"), Port: 65432, Proto: ProtoTCP, Src: SrcObserved, Owner: "cush1"}}
	if derived.ConflictsWith(obs) {
		t.Fatal("an observed endpoint was treated as authoritative")
	}
}

// TestOrderIsPreferenceNotTiers: "fallback" reduced to what it actually was.
func TestOrderIsPreferenceNotTiers(t *testing.T) {
	in := []Candidate{
		{Addr: ap("10.0.0.1"), Port: 65432, Proto: ProtoUDP, Src: SrcHostCand},
		{Addr: ap("198.51.100.7"), Port: 65432, Proto: ProtoTCP, Src: SrcSeed},
		{Addr: ap("198.51.100.7"), Port: 65432, Proto: ProtoUDP, Src: SrcObserved},
		{Addr: ap("198.51.100.7"), Port: 443, Proto: ProtoUDP, Src: SrcAdvertised},
	}
	got := keys(SortCandidates(in))
	want := []string{
		"tcp://198.51.100.7:65432", // seed wins despite being TCP
		"udp://198.51.100.7:443",   // advertised
		"udp://198.51.100.7:65432", // observed
		"udp://10.0.0.1:65432",     // host candidate last
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Merging sources is normal; the highest-authority copy of a duplicate wins.
func TestSortCandidatesDedupesKeepingBestSource(t *testing.T) {
	in := []Candidate{
		{Addr: ap("198.51.100.7"), Port: 65432, Proto: ProtoUDP, Src: SrcObserved, Owner: "p"},
		{Addr: ap("198.51.100.7"), Port: 65432, Proto: ProtoUDP, Src: SrcSeed, Owner: "p"},
	}
	got := SortCandidates(in)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].Src != SrcSeed {
		t.Fatalf("kept the %v copy; the operator's seed should win", got[0].Src)
	}
}

func TestSeedHost(t *testing.T) {
	for in, want := range map[string]string{
		"tcp://174.64.247.165:65432,443": "174.64.247.165",
		"174.64.247.165:65432":           "174.64.247.165",
		"seed.example":                   "seed.example",
		"udp://[2001:db8::1]:65432":      "2001:db8::1",
	} {
		if got := SeedHost(in); got != want {
			t.Errorf("SeedHost(%q) = %q, want %q", in, got, want)
		}
	}
}
