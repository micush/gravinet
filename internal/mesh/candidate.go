package mesh

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"gravinet/internal/config"
)

// A peer is reachable at a set of endpoints. Each one is an address, a
// protocol and a port, and any of them might work. That is the whole model.
//
// What this replaces is a hierarchy — a "primary" UDP port with a "fallback"
// TCP port derived from it — which the code then had to keep re-deriving,
// because the derivation was never actually knowable. A peer's TCP port is a
// fact about that peer. It is not a function of our own TCP port, nor of the
// port some other session at the same IP happens to use, and every mechanism
// that pretended otherwise was working around not having asked the right
// question:
//
//   - fallbackPort.Load() guessed the peer's port from our own config.
//   - SeedTCPPort added a second, parallel hint channel for join tokens.
//   - tcpPortForEndpoint walked live sessions for one whose endpoint shared an
//     IP and borrowed its port.
//   - seedFallback mapped each seed to the single derived address it became.
//
// That last pair is what made this worth doing rather than worth renaming. Two
// nodes behind one NAT — one reached over TCP/65432, the other over UDP/65432,
// which are independent mappings and an entirely ordinary setup — collapsed
// into each other: the UDP seed for the second peer had a TCP candidate
// manufactured for it at the same IP, tcpPortForEndpoint resolved its port
// from the first peer's live session, and the dial landed on the first peer's
// listener. Deterministically, every tick, forever. The operator's own seed
// list had said which peer was which all along, with a scheme and a port on
// each entry; the engine threw both away and re-derived them.
//
// In a flat set that collision cannot be expressed. A candidate carries its
// own protocol and its own port, from whoever supplied it, and nothing reaches
// across peers to fill in a blank.
//
// "Fallback" survives only as a *preference*: UDP is cheaper than TCP-over-TLS,
// so it is tried first. That is an ordering over one set, not two tiers, and it
// is expressed by Less rather than by a separate code path.

// Proto is a candidate's transport.
type Proto uint8

const (
	ProtoUDP Proto = iota
	ProtoTCP
)

func (p Proto) String() string {
	if p == ProtoTCP {
		return "tcp"
	}
	return "udp"
}

// CandSource records where a candidate came from, which is what decides how
// much to trust it when two disagree. Ordered most to least authoritative.
type CandSource uint8

const (
	// SrcSeed is an operator-configured seed: someone typed it, usually with
	// a note naming the peer. Nothing this node infers outranks that.
	SrcSeed CandSource = iota
	// SrcAdvertised is a port the peer itself announced, via handshake or
	// gossip. Authoritative about the peer, but only once it can be heard.
	SrcAdvertised
	// SrcObserved is where a peer's packets were last seen coming from —
	// correct for NAT traversal and wrong the moment a mapping rebinds.
	SrcObserved
	// SrcHostCand is a LAN address the peer advertised for local discovery.
	// Often unreachable from here, cheap to try when it isn't.
	SrcHostCand
)

func (s CandSource) String() string {
	switch s {
	case SrcSeed:
		return "seed"
	case SrcAdvertised:
		return "advertised"
	case SrcObserved:
		return "observed"
	default:
		return "host-candidate"
	}
}

// Candidate is one place a peer might be reachable.
//
// Owner is the node id this candidate belongs to when known — from a seed's
// recorded owner, or from the peer that advertised it. An empty Owner means
// "some peer, unknown which", which is the honest state for a cold seed and is
// exactly the case the old code papered over by borrowing a port from whatever
// session shared the IP.
type Candidate struct {
	Addr  netip.Addr
	Port  uint16
	Proto Proto
	Src   CandSource
	Owner string
}

func (c Candidate) AddrPort() netip.AddrPort {
	return netip.AddrPortFrom(c.Addr, c.Port)
}

func (c Candidate) String() string {
	return c.Proto.String() + "://" + c.AddrPort().String()
}

// Key identifies a candidate for deduplication and per-candidate backoff.
// Protocol is part of it: tcp/65432 and udp/65432 at one address are different
// NAT mappings reaching different hosts, and treating them as one entry is
// precisely the conflation this model exists to prevent.
type CandKey struct {
	Addr  netip.Addr
	Port  uint16
	Proto Proto
}

func (c Candidate) Key() CandKey {
	return CandKey{Addr: c.Addr, Port: c.Port, Proto: c.Proto}
}

func (k CandKey) String() string {
	return k.Proto.String() + "://" + netip.AddrPortFrom(k.Addr, k.Port).String()
}

// SeedCandidates expands one configured seed entry into candidates.
//
// The seed syntax already carries everything needed and always has:
// "tcp://host:65432,443,23" is a protocol and three ports. Expansion is
// therefore a parse, not an inference — no port is invented, and none is
// borrowed from anywhere else.
//
// resolved supplies the addresses for the seed's host, since a seed may name
// a DNS host rather than a literal and resolution is the caller's business
// (and must not happen on a lock). defaultPorts is used only when the seed
// gives no port at all; it is the one place a port this node didn't get from
// the operator can enter, and it stays confined to the no-port case.
//
// owner, when known, is stamped onto every candidate — a seed's note usually
// names the peer, and AddSeedFor already records that association.
func SeedCandidates(seedAddr string, resolved []netip.Addr, defaultPorts []uint16, owner string) ([]Candidate, error) {
	scheme, hostport := config.SeedParts(seedAddr)
	proto := ProtoUDP
	if scheme == "tcp" {
		proto = ProtoTCP
	}
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return nil, fmt.Errorf("seed address required")
	}

	ports, err := seedPorts(hostport, defaultPorts)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return nil, nil
	}
	out := make([]Candidate, 0, len(resolved)*len(ports))
	seen := map[CandKey]bool{}
	for _, a := range resolved {
		a = a.Unmap()
		if !a.IsValid() {
			continue
		}
		for _, p := range ports {
			c := Candidate{Addr: a, Port: p, Proto: proto, Src: SrcSeed, Owner: owner}
			if seen[c.Key()] {
				continue
			}
			seen[c.Key()] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// seedPorts pulls the port list out of a seed's host:port,port,... form.
func seedPorts(hostport string, defaults []uint16) ([]uint16, error) {
	_, portList, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port on the seed at all — the only case where this node's own
		// defaults are used, and the only inference in the whole expansion.
		return defaults, nil
	}
	var out []uint16
	for _, ps := range strings.Split(portList, ",") {
		ps = strings.TrimSpace(ps)
		if ps == "" {
			continue
		}
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("seed port %q must be 1-65535", ps)
		}
		out = append(out, uint16(n))
	}
	if len(out) == 0 {
		return defaults, nil
	}
	return out, nil
}

// SeedHost returns the host portion of a seed address, with any scheme and
// port list stripped, ready for resolution.
func SeedHost(seedAddr string) string {
	_, hostport := config.SeedParts(seedAddr)
	hostport = strings.TrimSpace(hostport)
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// Less orders a candidate set for dialing. This is the whole of what
// "fallback" used to mean, reduced to what it actually was: a preference.
//
//  1. Source. An operator's seed beats what a peer advertised, which beats
//     where its packets happened to come from, which beats a LAN address that
//     probably isn't reachable from here.
//  2. Protocol. UDP before TCP — cheaper to set up, no TLS handshake — but a
//     preference only. A TCP seed still leads a UDP host-candidate, because
//     being probably-right matters more than being cheap.
//  3. Address and port, for a stable order so a candidate set dials the same
//     way twice and logs comparably.
func (c Candidate) Less(o Candidate) bool {
	if c.Src != o.Src {
		return c.Src < o.Src
	}
	if c.Proto != o.Proto {
		return c.Proto < o.Proto
	}
	if c.Addr != o.Addr {
		return c.Addr.Less(o.Addr)
	}
	return c.Port < o.Port
}

// SortCandidates orders a set for dialing and drops exact duplicates. Callers
// merge candidates from several sources, and the same address can legitimately
// arrive from more than one — a seed the peer also advertises, say. The
// highest-authority copy wins, since Less sorts by source first.
func SortCandidates(in []Candidate) []Candidate {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Less(in[j]) })
	seen := map[CandKey]bool{}
	out := in[:0]
	for _, c := range in {
		if seen[c.Key()] {
			continue
		}
		seen[c.Key()] = true
		out = append(out, c)
	}
	return out
}

// ConflictsWith reports whether dialing c would reach a different peer than
// intended, because some other node's operator-configured seed names exactly
// this address, protocol and port.
//
// This is the check the old derivation had no way to make. It manufactured a
// TCP candidate at a UDP seed's address, resolved its port from an unrelated
// session, and dialed straight into another peer's explicitly configured
// listener — with that peer's seed sitting in the same list, saying so. Owned
// candidates are cheap to disqualify before a socket is opened.
//
// Only seeds are consulted. An advertised or observed endpoint shared with
// another peer is ordinary NAT behaviour and says nothing about who answers.
func (c Candidate) ConflictsWith(seeds []Candidate) bool {
	if c.Owner == "" {
		return false // nothing to contradict
	}
	for _, s := range seeds {
		if s.Src != SrcSeed || s.Owner == "" || s.Owner == c.Owner {
			continue
		}
		if s.Key() == c.Key() {
			return true
		}
	}
	return false
}
