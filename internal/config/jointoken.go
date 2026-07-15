package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gravinet/internal/crypto"
)

// A join token bundles everything a fresh node needs to join a network — the
// network id, its enabled key(s), the overlay subnets, and one or more bootstrap
// seeds — into a single pasteable string (cf. Proxmox's cluster join info). It
// is BEARER CREDENTIAL: anyone holding it can join the network, because it
// carries the network key. Share it over a secure channel and prefer a short
// expiry. The format is "grav1." + base64url(JSON); versioned so older nodes
// reject what they can't read.

const joinTokenPrefix = "grav1."

type joinTokenKey struct {
	Key     string `json:"k"`
	Label   string `json:"l,omitempty"`
	Expires string `json:"x,omitempty"` // per-key expiry (RFC3339), preserved for rotation
}

type joinToken struct {
	V         int            `json:"v"`
	ID        string         `json:"id"`
	Name      string         `json:"name,omitempty"`
	Subnet4   string         `json:"s4,omitempty"`
	Subnet6   string         `json:"s6,omitempty"`
	Keys      []joinTokenKey `json:"keys"`
	Seeds     []string       `json:"seeds,omitempty"`
	PeerCache []string       `json:"peer_cache,omitempty"` // see NetworkToken's doc: kept apart from Seeds so it lands in the joiner's PeerCache, not its Seeds
	TCPPort   int            `json:"tcp,omitempty"`        // mesh TCP/TLS fallback port, to dial the seeds when UDP is blocked
	Exp       string         `json:"exp,omitempty"`        // token expiry (RFC3339); "" = never
}

// IsJoinToken reports whether s looks like a gravinet join token.
func IsJoinToken(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), joinTokenPrefix)
}

// NetworkToken builds a join token for an existing network. extraSeeds are
// prepended to the embedded seed list (use this to advertise the reachable
// underlay endpoint of the node minting the token, which it can't always learn
// on its own). The token carries two separate groups of bootstrap candidates:
// extraSeeds plus this network's own configured Seeds travel as the token's
// Seeds group, and the joiner adopts them as its own explicit seeds (exactly
// as if it had typed them in itself); this node's recently-seen peers
// (PeerCache) travel as a separate group and land in the joiner's PeerCache
// instead — just as many candidates to bootstrap from, without permanently
// mislabeling someone else's transient peer list as seeds the joiner's own
// operator configured. ttl > 0 stamps the token with an expiry.
func (c *Config) NetworkToken(ref string, extraSeeds []string, ttl time.Duration) (string, error) {
	n := c.FindNetwork(ref)
	if n == nil {
		return "", fmt.Errorf("network %q not found", ref)
	}
	jt := joinToken{V: 1, ID: n.ID, Name: n.Name, Subnet4: n.Subnet4, Subnet6: n.Subnet6}
	if c.TCPFallbackEnabled() {
		jt.TCPPort = c.TCPFallbackPortValue() // let joiners reach the seeds over TCP if UDP is blocked
	}
	now := time.Now()
	for _, k := range n.Keys {
		if k.Key == "" || !k.Enabled || k.Expired(now) {
			continue
		}
		jt.Keys = append(jt.Keys, joinTokenKey{Key: k.Key, Label: k.Label, Expires: k.Expires})
	}
	if len(jt.Keys) == 0 {
		return "", fmt.Errorf("network %q has no enabled, unexpired key to share", n.Name)
	}
	seen := map[string]bool{}
	addSeed := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		jt.Seeds = append(jt.Seeds, s)
	}
	addCached := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return // already carried as a genuine seed (or a dup within PeerCache itself)
		}
		seen[s] = true
		jt.PeerCache = append(jt.PeerCache, s)
	}
	// Embed every endpoint this host knows for the network — explicitly-passed
	// addresses and this network's own configured seeds go in Seeds (the
	// joiner will treat these as its own explicit, operator-curated seeds,
	// exactly as if it had typed them in itself); this node's recently-seen
	// peers (PeerCache) go in a separate group so the joiner has just as many
	// bootstrap candidates without mistaking someone else's transient peer
	// list for its own permanent configuration. Before this split, a peer
	// address that only ever lived in n.PeerCache (e.g. a LAN peer discovered
	// via gossip, never an address anyone configured as a seed) was flattened
	// into the same Seeds list and, once applied on the joiner, written
	// straight into *its* Seeds — permanently mixing "peers I happen to have
	// seen" into "seeds I explicitly configured," on a node whose operator
	// never added any such seed.
	for _, s := range extraSeeds {
		addSeed(s)
	}
	for _, s := range n.Seeds {
		addSeed(s.Address)
	}
	for _, s := range n.PeerCache {
		addCached(s)
	}
	if ttl > 0 {
		jt.Exp = now.Add(ttl).UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(jt)
	if err != nil {
		return "", err
	}
	return joinTokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// TokenSeedCount reports how many distinct bootstrap seeds a token for ref would
// carry given extra seeds, so callers can warn when it would carry none.
func (c *Config) TokenSeedCount(ref string, extra []string) int {
	n := c.FindNetwork(ref)
	if n == nil {
		return 0
	}
	seen := map[string]bool{}
	for _, group := range [][]string{extra, n.Seeds.Addrs(), n.PeerCache} {
		for _, s := range group {
			if s = strings.TrimSpace(s); s != "" {
				seen[s] = true
			}
		}
	}
	return len(seen)
}

func parseJoinToken(s string) (joinToken, error) {
	var jt joinToken
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, joinTokenPrefix) {
		return jt, fmt.Errorf("not a gravinet join token (expected %q prefix)", joinTokenPrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, joinTokenPrefix))
	if err != nil {
		return jt, fmt.Errorf("malformed join token: %w", err)
	}
	if err := json.Unmarshal(raw, &jt); err != nil {
		return jt, fmt.Errorf("malformed join token: %w", err)
	}
	if jt.V != 1 {
		return jt, fmt.Errorf("unsupported join-token version %d (upgrade gravinet)", jt.V)
	}
	return jt, nil
}

// NetworkJoinToken joins (or updates) the network described by a join token,
// returning the canonical id and learned name. Keys are merged into free slots
// without disturbing keys already present. The token's Seeds group is merged
// into this node's own Seeds (the joiner's explicit, operator-facing seed
// list); its PeerCache group — the issuer's recently-seen peers, not
// anyone's configured seeds — is merged into this node's PeerCache instead,
// so it's treated as the same kind of auto-managed, self-pruning bootstrap
// candidate here that it was on the issuing node, not promoted to a
// permanent seed this node's operator never configured.
func (c *Config) NetworkJoinToken(token string) (id, name string, err error) {
	jt, err := parseJoinToken(token)
	if err != nil {
		return "", "", err
	}
	canon, err := canonNetworkID(jt.ID)
	if err != nil {
		return "", "", fmt.Errorf("join token has an invalid network id: %w", err)
	}
	if jt.Exp != "" {
		t, perr := time.Parse(time.RFC3339, jt.Exp)
		if perr != nil {
			return "", "", fmt.Errorf("join token has an invalid expiry")
		}
		if time.Now().After(t) {
			return "", "", fmt.Errorf("join token expired at %s", t.Local().Format(time.RFC3339))
		}
	}
	if len(jt.Keys) == 0 {
		return "", "", fmt.Errorf("join token carries no keys")
	}
	for _, k := range jt.Keys {
		if _, derr := crypto.DecodeKey(k.Key); derr != nil {
			return "", "", fmt.Errorf("join token contains an invalid key: %w", derr)
		}
	}
	if jt.Subnet4 != "" {
		if verr := validV4CIDR(jt.Subnet4); verr != nil {
			return "", "", verr
		}
	}
	if jt.Subnet6 != "" {
		if verr := validV6CIDR(jt.Subnet6); verr != nil {
			return "", "", verr
		}
	}

	n := c.FindNetwork(canon)
	if n == nil {
		nn := NewNetworkDefaults()
		nn.ID = canon
		nn.Name = jt.Name
		nn.Subnet4, nn.Subnet6 = jt.Subnet4, jt.Subnet6
		c.Networks = append(c.Networks, nn)
		n = &c.Networks[len(c.Networks)-1]
	} else {
		if jt.Subnet4 != "" {
			n.Subnet4 = jt.Subnet4
		}
		if jt.Subnet6 != "" {
			n.Subnet6 = jt.Subnet6
		}
	}

	// Merge keys into free slots, skipping material already present.
	have := map[string]bool{}
	for _, k := range n.Keys {
		if k.Key != "" {
			have[k.Key] = true
		}
	}
	for _, jk := range jt.Keys {
		if have[jk.Key] {
			continue
		}
		slot := -1
		for i := range n.Keys {
			if n.Keys[i].Key == "" {
				slot = i
				break
			}
		}
		if slot < 0 {
			return "", "", fmt.Errorf("no free key slot to import all token keys (max 8)")
		}
		label := jk.Label
		if label == "" {
			label = fmt.Sprintf("key%d", slot)
		}
		n.Keys[slot] = KeySlot{Key: jk.Key, Label: label, Enabled: true, Expires: jk.Expires}
		have[jk.Key] = true
	}

	n.Enabled = true
	if jt.TCPPort > 0 && n.SeedTCPPort == 0 {
		n.SeedTCPPort = jt.TCPPort // dial the token's seeds on this port if UDP is blocked
	}
	for _, s := range jt.Seeds {
		s = strings.TrimSpace(s)
		if s != "" && !containsSeedAddr(n.Seeds, s) {
			n.Seeds = append(n.Seeds, Seed{Address: s})
		}
	}
	for _, s := range jt.PeerCache {
		s = strings.TrimSpace(s)
		// Skip anything already present as a genuine seed (either just merged
		// above, or already configured here independently) — a bootstrap
		// candidate doesn't need to be tracked in both places at once.
		if s != "" && !containsSeedAddr(n.Seeds, s) && !containsStr(n.PeerCache, s) {
			n.PeerCache = append(n.PeerCache, s)
		}
	}
	return canon, n.Name, nil
}
