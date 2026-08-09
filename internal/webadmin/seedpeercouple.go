package webadmin

import (
	"net/netip"
	"strconv"
	"strings"

	"gravinet/internal/config"
)

// Seed state and peer state are two views of one decision, and they mirror
// each other in every direction:
//
//	enable/disable a seed  → the node behind it is enabled/disabled to match
//	enable/disable a node  → the seeds that reach it are enabled/disabled to match
//
// So a row's state is never a half-truth. An address-level switch on its own
// cannot keep a node away — on a full mesh any other peer gossips it back
// within a gossip round — and a node-level switch on its own leaves the
// addresses that reach it in a state contradicting it. Coupling both
// directions is what makes either switch mean what the operator reading it
// takes it to mean.
//
// One address can front several nodes behind a NAT, and one node can be
// reached at several addresses. The mapping used here is the one the engine
// has actually proven by handshake, not a guess, and it is one node per
// address — the most recent to answer there. Where that is genuinely ambiguous
// the honest outcome is the one an operator can see and correct, which is why
// an address with no proven owner couples to nothing and says so.
//
// All of this depends on knowing which node is behind an address, which is a
// runtime fact. config.Seed.Node caches it; syncSeedNodes below refreshes it
// from the engine's own handshake attribution on every seed or peer edit, so
// the link is already recorded by the time it is needed — critically, the
// handshake that establishes it cannot happen once the seed is parked, so
// learning it lazily at disable time would be too late.

// syncSeedNodes refreshes config.Seed.Node from the engine's current
// attribution, for every network. Cheap (a map lookup per seed, no I/O) and
// idempotent, so it is safe to call at the top of any config mutation.
//
// It only ever fills in or corrects an attribution, never clears one. A seed
// the engine currently knows nothing about is usually one that is simply
// disabled or not yet dialed, and forgetting its node then would destroy the
// association exactly when the re-enable path needs it.
//
// Seeds written as a hostname rather than an IP literal are skipped: resolving
// one means a DNS lookup inside a request handler, and a stale or split-horizon
// answer would attribute a seed to the wrong node — a worse failure than no
// attribution, since it would disable an unrelated peer.
func syncSeedNodes(cfg *config.Config, owners map[uint64]map[string]string) bool {
	changed := false
	for i := range cfg.Networks {
		n := &cfg.Networks[i]
		id, err := strconv.ParseUint(n.ID, 16, 64)
		if err != nil {
			continue
		}
		byAddr := owners[id]
		if len(byAddr) == 0 {
			continue
		}
		for j := range n.Seeds {
			host := seedHostKey(n.Seeds[j].Address)
			if host == "" {
				continue // hostname, or unparseable — see above
			}
			if node := byAddr[host]; node != "" && node != n.Seeds[j].Node {
				n.Seeds[j].Node = node
				changed = true
			}
		}
	}
	return changed
}

// seedHostKey reduces an operator-written seed address to the bare-IP key
// SeedNodeOwners publishes, or "" when it isn't an IP literal.
//
// It deliberately keys on the address alone, dropping any port, because the
// port a seed was configured with is frequently not the port its handshake
// completed on: a multi-port seed ("host:65432,443") offers several
// candidates, and a bare host expands across the whole built-in fallback set.
// Matching on host:port would miss all of those.
func seedHostKey(addr string) string {
	_, hostport := config.SeedParts(addr)
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return ""
	}
	// Strip a port (or comma-separated port list) if one is present. An
	// IPv6 literal is bracketed when it carries a port, unbracketed when
	// bare, so try the bare form first and fall back to splitting.
	if ip, err := netip.ParseAddr(hostport); err == nil {
		return ip.Unmap().String()
	}
	host := hostport
	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end > 0 {
			host = host[1:end]
		}
	} else if idx := strings.LastIndex(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return "" // a hostname
	}
	return ip.Unmap().String()
}

// coupleSeedState mirrors a seed's new state onto the node behind it, so
// disabling a seed can't be undone seconds later by another peer gossiping the
// node back, and enabling one doesn't leave the node still blocked. Returns
// the node it changed, or "" if the seed has no proven owner — which the
// caller must report rather than swallow, since silently doing nothing is what
// makes an operator think the toggle is broken.
func coupleSeedState(cfg *config.Config, netName, addr string, on bool) (string, error) {
	n, err := cfg.PickNetwork(netName)
	if err != nil {
		return "", err
	}
	node := n.Seeds.NodeFor(addr)
	if node == "" {
		return "", nil
	}
	if peerEnabled(n, node) == on {
		return "", nil // already matches; nothing to report
	}
	if err := cfg.PeerSetEnabled(netName, node, on); err != nil {
		return "", err
	}
	return node, nil
}

// couplePeerState is the mirror: a node's new state is applied to every seed
// address proven to reach it. Returns the addresses it actually changed, so
// the caller reports what happened rather than claiming credit for rows that
// already matched.
func couplePeerState(cfg *config.Config, netName, nodeID string, on bool) ([]string, error) {
	n, err := cfg.PickNetwork(netName)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, addr := range n.Seeds.AddrsForNode(nodeID) {
		if seedEnabled(n, addr) == on {
			continue
		}
		if err := cfg.SeedSetEnabled(netName, addr, on); err != nil {
			return changed, err
		}
		changed = append(changed, addr)
	}
	return changed, nil
}

func peerEnabled(n *config.Network, nodeID string) bool {
	for _, id := range n.DisabledPeers {
		if id == nodeID {
			return false
		}
	}
	return true
}

func seedEnabled(n *config.Network, addr string) bool {
	for i := range n.Seeds {
		if n.Seeds[i].Address == addr {
			return !n.Seeds[i].Disabled
		}
	}
	return true
}

// n0SeedOwner reports whether a seed address genuinely has no proven owner, as
// opposed to having one whose state already matched. The two produce the same
// empty result from coupleSeedState and mean opposite things: one is a gap
// worth telling the operator about, the other is a no-op worth staying quiet
// about.
func n0SeedOwner(cfg *config.Config, netName, addr string) bool {
	n, err := cfg.PickNetwork(netName)
	if err != nil {
		return false
	}
	return n.Seeds.NodeFor(addr) == ""
}
