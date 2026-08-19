package mesh

import (
	"net/netip"
	"sync"
	"time"
)

// candStore holds one network's dial candidates and the pacing state for each.
//
// It replaces four maps that were all keyed by netip.AddrPort — seedBackoff,
// tcpBackoff, tcpAttempt and dialing — plus seedTCP, which
// mapped each seed to the single derived address it turned into. Keying by
// AddrPort is the same conflation the Candidate type exists to fix, one level
// down: udp/65432 and tcp/65432 at one address are separate NAT mappings that
// can reach different hosts, so pacing them as one entry means a failure
// against one peer suppresses dials to another.
//
// Splitting pacing across two maps had its own cost, documented in v780: the
// escalating backoff was recorded against the *derived* TCP address while
// the only reader checked the *seed*, so the ladder climbed 30s → 10m purely
// for the log's benefit and a flat 30s cooldown remained the real pace. One
// store keyed by what is actually dialed removes the class of bug rather than
// that instance of it.
//
// Pacing is per candidate and deliberately not per peer. A peer with four
// candidates should keep trying the other three at full speed while one is
// cooling down — that is the whole point of having a set.
type candStore struct {
	mu   sync.Mutex
	cand map[CandKey]Candidate
	pace map[CandKey]*candPace
}

// candPace is one candidate's retry state.
//
// wait is the current backoff step, retained while cooling so the ladder can
// escalate; until is when the next attempt is allowed. inFlight replaces the
// dialing map: several seed entries could expand onto one candidate, and
// initLoop fires them synchronously in a single pass while the dials run in
// goroutines, so without a claim every one of them dials the same address
// before the first has finished.
type candPace struct {
	wait     time.Duration
	until    time.Time
	inFlight bool
	lastTry  time.Time
	fails    int
}

func newCandStore() *candStore {
	return &candStore{cand: map[CandKey]Candidate{}, pace: map[CandKey]*candPace{}}
}

// Add records a candidate, keeping the most authoritative copy when the same
// address arrives from more than one source (a seed the peer also advertises).
// Pacing state is keyed separately and survives a re-Add, so re-learning a
// candidate can't reset a backoff and turn a cooling-down address back into a
// dial-every-tick one.
func (s *candStore) Add(c Candidate) {
	if !c.Addr.IsValid() || c.Port == 0 {
		return
	}
	c.Addr = c.Addr.Unmap()
	k := c.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.cand[k]; ok {
		// Lower Src is more authoritative. Take the better source, and take an
		// owner from either — learning who a candidate belongs to is strictly
		// new information and never a reason to discard the rest.
		if c.Src > old.Src {
			if old.Owner == "" && c.Owner != "" {
				old.Owner = c.Owner
				s.cand[k] = old
			}
			return
		}
		if c.Owner == "" {
			c.Owner = old.Owner
		}
	}
	s.cand[k] = c
}

// AddAll records a batch.
func (s *candStore) AddAll(cs []Candidate) {
	for _, c := range cs {
		s.Add(c)
	}
}

// Remove drops a candidate and its pacing state.
func (s *candStore) Remove(k CandKey) {
	s.mu.Lock()
	delete(s.cand, k)
	delete(s.pace, k)
	s.mu.Unlock()
}

// All returns every candidate in dial order.
func (s *candStore) All() []Candidate {
	s.mu.Lock()
	out := make([]Candidate, 0, len(s.cand))
	for _, c := range s.cand {
		out = append(out, c)
	}
	s.mu.Unlock()
	return SortCandidates(out)
}

// ForOwner returns the candidates known to belong to nodeID, in dial order,
// plus the unowned ones — a cold seed has no owner yet and is often the only
// way to reach the peer it was configured for.
func (s *candStore) ForOwner(nodeID string) []Candidate {
	s.mu.Lock()
	var out []Candidate
	for _, c := range s.cand {
		if c.Owner == nodeID || c.Owner == "" {
			out = append(out, c)
		}
	}
	s.mu.Unlock()
	return SortCandidates(out)
}

// Seeds returns the operator-configured candidates, used by ConflictsWith to
// disqualify a candidate that would land on another peer's listener.
func (s *candStore) Seeds() []Candidate {
	s.mu.Lock()
	var out []Candidate
	for _, c := range s.cand {
		if c.Src == SrcSeed {
			out = append(out, c)
		}
	}
	s.mu.Unlock()
	return SortCandidates(out)
}

// Claim marks a candidate as being dialed, reporting false if it already is or
// if it is still cooling down. The two checks are together deliberately: every
// caller needs both, and separating them is what let concurrent callers race
// past a due-check before the first dial had updated anything.
func (s *candStore) Claim(k CandKey, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pace[k]
	if p == nil {
		p = &candPace{}
		s.pace[k] = p
	}
	if p.inFlight || now.Before(p.until) {
		return false
	}
	p.inFlight = true
	p.lastTry = now
	return true
}

// Release clears the in-flight claim without judging the outcome. Used when a
// dial is abandoned before it says anything — the transport doesn't support
// it, a connection already exists — so an unattempted candidate isn't
// penalised as if it had failed.
func (s *candStore) Release(k CandKey) {
	s.mu.Lock()
	if p := s.pace[k]; p != nil {
		p.inFlight = false
	}
	s.mu.Unlock()
}

// Fail records an attempt that didn't produce a session and escalates the
// backoff. The ladder doubles from candBackoffMin to candBackoffMax.
func (s *candStore) Fail(k CandKey, now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pace[k]
	if p == nil {
		p = &candPace{}
		s.pace[k] = p
	}
	p.inFlight = false
	p.fails++
	switch {
	case p.wait == 0:
		p.wait = candBackoffMin
	case p.wait >= candBackoffMax:
		p.wait = candBackoffMax
	default:
		p.wait *= 2
		if p.wait > candBackoffMax {
			p.wait = candBackoffMax
		}
	}
	p.until = now.Add(p.wait)
	return p.wait
}

// Succeed clears a candidate's backoff. A working address must return to full
// speed immediately: a peer that reconnects after an outage should not inherit
// a ten-minute cooldown from the outage that just ended.
func (s *candStore) Succeed(k CandKey) {
	s.mu.Lock()
	if p := s.pace[k]; p != nil {
		p.inFlight, p.wait, p.fails = false, 0, 0
		p.until = time.Time{}
	}
	s.mu.Unlock()
}

// Due reports whether a candidate may be dialed now, without claiming it.
func (s *candStore) Due(k CandKey, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pace[k]
	return p == nil || (!p.inFlight && !now.Before(p.until))
}

// Stats exposes one candidate's pacing state for the peer/seed info panel and
// for tests. Zero values mean "never tried".
func (s *candStore) Stats(k CandKey) (fails int, wait time.Duration, until, lastTry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pace[k]; p != nil {
		return p.fails, p.wait, p.until, p.lastTry
	}
	return 0, 0, time.Time{}, time.Time{}
}

// Len is the number of candidates held.
func (s *candStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cand)
}

const (
	// candBackoffMin/Max are the retry ladder, carried over from the TCP
	// path's tcpBackoffMin/Max. The values are unchanged; what changes is
	// that the entry recording them and the entry read before dialing are now
	// the same entry (see candStore's doc comment on v780).
	candBackoffMin = 30 * time.Second
	candBackoffMax = 10 * time.Minute
)

// candFromEndpoint builds an observed candidate — where a peer's packets were
// last seen coming from. Correct for NAT traversal, and stale the moment a
// mapping rebinds, which is why SrcObserved sorts below what a peer advertised
// about itself.
func candFromEndpoint(ep netip.AddrPort, proto Proto, owner string) Candidate {
	return Candidate{Addr: ep.Addr().Unmap(), Port: ep.Port(), Proto: proto, Src: SrcObserved, Owner: owner}
}

// candFromAdvertised builds a candidate from a port a peer announced for
// itself, at an address already known to reach it.
func candFromAdvertised(addr netip.Addr, port uint16, proto Proto, owner string) Candidate {
	return Candidate{Addr: addr.Unmap(), Port: port, Proto: proto, Src: SrcAdvertised, Owner: owner}
}
