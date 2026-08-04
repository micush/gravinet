package mesh

import (
	"reflect"
	"testing"
	"time"
)

func cand(addr string, port uint16, proto Proto, src CandSource, owner string) Candidate {
	return Candidate{Addr: ap(addr), Port: port, Proto: proto, Src: src, Owner: owner}
}

// TestCandStorePacesPerCandidateNotPerPeer is the property the set exists for.
// A peer with several candidates must keep trying the rest at full speed while
// one cools down; pacing per peer would throw away the redundancy.
func TestCandStorePacesPerCandidateNotPerPeer(t *testing.T) {
	s := newCandStore()
	a := cand("198.51.100.7", 65432, ProtoUDP, SrcSeed, "p")
	b := cand("198.51.100.7", 443, ProtoTCP, SrcSeed, "p")
	s.AddAll([]Candidate{a, b})

	now := time.Now()
	if !s.Claim(a.Key(), now) {
		t.Fatal("first claim refused")
	}
	s.Fail(a.Key(), now)

	if s.Due(a.Key(), now) {
		t.Error("failed candidate is due immediately")
	}
	if !s.Due(b.Key(), now) {
		t.Error("a sibling candidate was suppressed by its peer's other failure")
	}
}

// TestCandStoreProtocolSeparatesPacing: udp/65432 and tcp/65432 at one address
// are different NAT mappings that can reach different hosts, so a failure
// against one must not throttle the other. This is the cush1/cush2 shape.
func TestCandStoreProtocolSeparatesPacing(t *testing.T) {
	s := newCandStore()
	u := cand("174.64.247.165", 65432, ProtoUDP, SrcSeed, "cush2")
	c := cand("174.64.247.165", 65432, ProtoTCP, SrcSeed, "cush1")
	s.AddAll([]Candidate{u, c})
	if s.Len() != 2 {
		t.Fatalf("stored %d candidates; udp and tcp at one address are distinct", s.Len())
	}
	now := time.Now()
	s.Claim(c.Key(), now)
	s.Fail(c.Key(), now)
	if !s.Due(u.Key(), now) {
		t.Fatal("cush1's TCP failure suppressed cush2's UDP candidate")
	}
}

// TestCandStoreClaimBlocksConcurrentDials: several seed entries can expand
// onto one candidate, and initLoop fires them synchronously while the dials
// run in goroutines. Without an atomic claim they all dial the same address.
func TestCandStoreClaimBlocksConcurrentDials(t *testing.T) {
	s := newCandStore()
	c := cand("198.51.100.7", 65432, ProtoUDP, SrcSeed, "p")
	s.Add(c)
	now := time.Now()
	if !s.Claim(c.Key(), now) {
		t.Fatal("first claim refused")
	}
	if s.Claim(c.Key(), now) {
		t.Fatal("second claim granted while a dial is in flight")
	}
	s.Release(c.Key())
	if !s.Claim(c.Key(), now) {
		t.Fatal("claim refused after release")
	}
}

// Release must not penalise: a dial abandoned before it said anything is not
// a failure, and treating it as one would back off addresses never tried.
func TestCandStoreReleaseDoesNotBackOff(t *testing.T) {
	s := newCandStore()
	c := cand("198.51.100.7", 65432, ProtoUDP, SrcSeed, "p")
	s.Add(c)
	now := time.Now()
	s.Claim(c.Key(), now)
	s.Release(c.Key())
	if fails, wait, _, _ := s.Stats(c.Key()); fails != 0 || wait != 0 {
		t.Fatalf("release recorded fails=%d wait=%v; it is not a failure", fails, wait)
	}
}

// TestCandStoreBackoffLadder: the ladder escalates and caps. The point of
// v780 was that the entry written and the entry read must be the same one, so
// this reads back through Due rather than through a second map.
func TestCandStoreBackoffLadder(t *testing.T) {
	s := newCandStore()
	c := cand("198.51.100.7", 65432, ProtoUDP, SrcSeed, "p")
	s.Add(c)
	now := time.Now()

	var last time.Duration
	for i := 0; i < 12; i++ {
		s.Claim(c.Key(), now)
		last = s.Fail(c.Key(), now)
		if s.Due(c.Key(), now) {
			t.Fatalf("attempt %d: due immediately after failing", i)
		}
		if !s.Due(c.Key(), now.Add(last+time.Second)) {
			t.Fatalf("attempt %d: not due after its own backoff of %v elapsed", i, last)
		}
		now = now.Add(last + time.Second)
	}
	if last != candBackoffMax {
		t.Fatalf("ladder settled at %v, want the cap %v", last, candBackoffMax)
	}
}

// A working address must return to full speed at once — a peer reconnecting
// after an outage should not inherit a ten-minute cooldown from that outage.
func TestCandStoreSucceedClearsBackoff(t *testing.T) {
	s := newCandStore()
	c := cand("198.51.100.7", 65432, ProtoUDP, SrcSeed, "p")
	s.Add(c)
	now := time.Now()
	for i := 0; i < 5; i++ {
		s.Claim(c.Key(), now)
		s.Fail(c.Key(), now)
	}
	s.Succeed(c.Key())
	if !s.Due(c.Key(), now) {
		t.Fatal("a candidate that just worked is still cooling down")
	}
	if fails, wait, _, _ := s.Stats(c.Key()); fails != 0 || wait != 0 {
		t.Fatalf("success left fails=%d wait=%v", fails, wait)
	}
}

// Re-learning a candidate must not reset its pacing, or a gossip refresh would
// turn a cooling-down address back into a dial-every-tick one.
func TestCandStoreReAddKeepsPacing(t *testing.T) {
	s := newCandStore()
	c := cand("198.51.100.7", 65432, ProtoUDP, SrcObserved, "p")
	s.Add(c)
	now := time.Now()
	s.Claim(c.Key(), now)
	s.Fail(c.Key(), now)

	s.Add(c) // heard about it again
	if s.Due(c.Key(), now) {
		t.Fatal("re-adding a candidate cleared its backoff")
	}
}

// The most authoritative copy wins, but an owner learned later is kept — that
// is new information, not a reason to discard the better source.
func TestCandStoreMergeKeepsBestSourceAndLearnsOwner(t *testing.T) {
	s := newCandStore()
	s.Add(cand("198.51.100.7", 65432, ProtoUDP, SrcSeed, ""))
	s.Add(cand("198.51.100.7", 65432, ProtoUDP, SrcObserved, "peer-a"))

	all := s.All()
	if len(all) != 1 {
		t.Fatalf("got %d candidates, want 1", len(all))
	}
	if all[0].Src != SrcSeed {
		t.Errorf("source = %v, want the seed to win", all[0].Src)
	}
	if all[0].Owner != "peer-a" {
		t.Errorf("owner = %q, want the later-learned owner kept", all[0].Owner)
	}
}

// ForOwner includes unowned candidates: a cold seed has no owner yet and is
// often the only way to reach the peer it was configured for. Excluding them
// would mean no connectivity, which is worse than a wrong guess.
func TestCandStoreForOwnerIncludesUnowned(t *testing.T) {
	s := newCandStore()
	s.AddAll([]Candidate{
		cand("198.51.100.7", 65432, ProtoUDP, SrcSeed, "peer-a"),
		cand("198.51.100.8", 65432, ProtoUDP, SrcSeed, ""),
		cand("198.51.100.9", 65432, ProtoUDP, SrcSeed, "peer-b"),
	})
	got := s.ForOwner("peer-a")
	want := []string{"udp://198.51.100.7:65432", "udp://198.51.100.8:65432"}
	if !reflect.DeepEqual(keys(got), want) {
		t.Fatalf("got %q, want %q", keys(got), want)
	}
}

// Seeds() feeds ConflictsWith, so it must return only operator-configured
// candidates — an observed endpoint shared behind a NAT is not authoritative
// about who answers.
func TestCandStoreSeedsOnlyReturnsSeeds(t *testing.T) {
	s := newCandStore()
	s.AddAll([]Candidate{
		cand("174.64.247.165", 65432, ProtoTCP, SrcSeed, "cush1"),
		cand("174.64.247.165", 5128, ProtoUDP, SrcObserved, "cush1"),
	})
	got := s.Seeds()
	if len(got) != 1 || got[0].Proto != ProtoTCP {
		t.Fatalf("got %q, want only the configured seed", keys(got))
	}
	// And that seed disqualifies another peer's derived candidate.
	derived := cand("174.64.247.165", 65432, ProtoTCP, SrcObserved, "cush2")
	if !derived.ConflictsWith(got) {
		t.Fatal("store's seed set did not disqualify the colliding candidate")
	}
}
