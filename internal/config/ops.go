package config

// ops.go holds the config-mutation primitives shared by the CLI (cmd/gravinet)
// and the web admin GUI (internal/webadmin). Both surfaces drive these exact
// methods so the two never drift: anything you can do from one you can do from
// the other. Each returns an error instead of exiting, and none of them persist
// — the caller validates, saves, and reloads.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/crypto"
)

// ---- network resolution ------------------------------------------------------

// FindNetwork returns the network matching ref — by Name, by exact hex ID, or by
// numerically-equal hex ID (so the zero-trimmed form shown in `status` and the
// web UI also matches the zero-padded form stored in config). Returns nil if none.
func (c *Config) FindNetwork(ref string) *Network {
	qv, qok := parseHexID(ref)
	for i := range c.Networks {
		n := &c.Networks[i]
		if n.Name == ref || n.ID == ref {
			return n
		}
		if qok {
			if nv, ok := parseHexID(n.ID); ok && nv == qv {
				return n
			}
		}
	}
	return nil
}

// NetworkID resolves a network reference (name or hex ID) to its numeric engine
// ID. Used by the control socket so live commands (ban/unban/fw) accept a network
// name, matching the config commands.
func (c *Config) NetworkID(ref string) (uint64, bool) {
	n := c.FindNetwork(ref)
	if n == nil {
		return 0, false
	}
	v, err := strconv.ParseUint(n.ID, 16, 64)
	return v, err == nil
}

func parseHexID(s string) (uint64, bool) {
	v, err := strconv.ParseUint(s, 16, 64)
	return v, err == nil
}

// PickNetwork resolves a network by name; with an empty name it returns the sole
// network if there's exactly one, else an error asking the caller to choose.
func (c *Config) PickNetwork(name string) (*Network, error) {
	if name != "" {
		n := c.FindNetwork(name)
		if n == nil {
			return nil, fmt.Errorf("no network named %q", name)
		}
		return n, nil
	}
	switch len(c.Networks) {
	case 0:
		return nil, fmt.Errorf("no networks configured")
	case 1:
		return &c.Networks[0], nil
	default:
		return nil, fmt.Errorf("multiple networks configured; specify which one")
	}
}

// NextFreeSubnets picks the next non-overlapping overlay pair (10.N.0.0/16 and
// fd00:N::/64), N starting at 42, skipping any second octet already used by an
// existing 10.x network. This is what lets one host hold several networks
// without their overlays colliding.
func (c *Config) NextFreeSubnets() (string, string) {
	used := map[int]bool{}
	for _, n := range c.Networks {
		if ip, _, err := net.ParseCIDR(n.Subnet4); err == nil {
			if v4 := ip.To4(); v4 != nil && v4[0] == 10 {
				used[int(v4[1])] = true
			}
		}
	}
	n := 42
	for n < 255 && used[n] {
		n++
	}
	return fmt.Sprintf("10.%d.0.0/16", n), fmt.Sprintf("fd00:%d::/64", n)
}

// ---- networks ----------------------------------------------------------------

// NetworkAdd creates a network with a freshly generated key. Empty v4/v6 means
// auto-assign a dual-stack pair; giving one family makes a single-family network.
func (c *Config) NetworkAdd(name, v4, v6 string) (*Network, error) {
	if name == "" {
		return nil, fmt.Errorf("network name required")
	}
	if c.FindNetwork(name) != nil {
		return nil, fmt.Errorf("network %q already exists", name)
	}
	v4, v6, err := resolveSubnets(c, v4, v6)
	if err != nil {
		return nil, err
	}
	n := NewNetworkDefaults()
	n.ID = randomNetworkID()
	n.Name = name
	n.Subnet4, n.Subnet6 = v4, v6
	k, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	n.Keys[0] = KeySlot{Key: k, Label: "key0", Enabled: true}
	c.Networks = append(c.Networks, n)
	return &c.Networks[len(c.Networks)-1], nil
}

// NetworkDelete removes a network. Returns an error if it doesn't exist.
func (c *Config) NetworkDelete(ref string) error {
	// Prefer an ID match: IDs are unique, so the web UI can target one of
	// several same-named networks without deleting the others. Matched the
	// same way FindNetwork matches — exact string, then numerically-equal hex
	// ID — so a zero-trimmed or differently-cased id from the web UI/API
	// still matches the zero-padded, lowercase form stored in config. This
	// used to be a plain n.ID == ref check, unlike every other Network*
	// method here (NetworkSetEnabled, NetworkRename, NetworkSetSubnets,
	// NetworkSetAddress), which all go through FindNetwork already — that
	// inconsistency was the bug: enable/disable/rename/subnet edits on a
	// network would work fine while deleting that exact same network failed
	// with "no network named", for any ID FindNetwork's numeric fallback
	// would have matched but exact-string comparison wouldn't.
	qv, qok := parseHexID(ref)
	for i, n := range c.Networks {
		if n.ID == ref {
			c.Networks = append(c.Networks[:i:i], c.Networks[i+1:]...)
			return nil
		}
		if qok {
			if nv, ok := parseHexID(n.ID); ok && nv == qv {
				c.Networks = append(c.Networks[:i:i], c.Networks[i+1:]...)
				return nil
			}
		}
	}
	out := c.Networks[:0]
	found := false
	for _, n := range c.Networks {
		if n.Name == ref {
			found = true
			continue
		}
		out = append(out, n)
	}
	c.Networks = out
	if !found {
		return fmt.Errorf("no network named %q", ref)
	}
	return nil
}

// NetworkSetEnabled enables or disables a network.
func (c *Config) NetworkSetEnabled(name string, on bool) error {
	n := c.FindNetwork(name)
	if n == nil {
		return fmt.Errorf("no network named %q", name)
	}
	n.Enabled = on
	return nil
}

// NetworkRename changes a network's local label. The name is config-only metadata
// (the engine identifies networks by their immutable ID), so this is safe and does
// not need a restart.
func (c *Config) NetworkRename(ref, newName string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("new network name required")
	}
	n := c.FindNetwork(ref)
	if n == nil {
		return fmt.Errorf("no network named %q", ref)
	}
	if n.Name == newName {
		return nil
	}
	for i := range c.Networks {
		if &c.Networks[i] != n && c.Networks[i].Name == newName {
			return fmt.Errorf("network %q already exists", newName)
		}
	}
	n.Name = newName
	return nil
}

// NetworkSetNotes replaces a network's free-form operator note. Config-only
// metadata, like the name — safe and needs no restart.
func (c *Config) NetworkSetNotes(ref, notes string) error {
	n := c.FindNetwork(ref)
	if n == nil {
		return fmt.Errorf("no network named %q", ref)
	}
	n.Notes = strings.TrimSpace(notes)
	return nil
}

// NetworkSetSubnets replaces a network's overlay subnet(s). An empty v4/v6 leaves
// that family unchanged; the literal "none" clears it. At least one family must
// remain. Changing a subnet re-homes dynamic addressing on restart and must be
// applied on every node in the network, so callers treat it as structural.
func (c *Config) NetworkSetSubnets(ref, v4, v6 string) error {
	n := c.FindNetwork(ref)
	if n == nil {
		return fmt.Errorf("no network named %q", ref)
	}
	nv4, nv6 := n.Subnet4, n.Subnet6
	if v4 != "" {
		if strings.EqualFold(v4, "none") {
			nv4 = ""
		} else {
			if err := validV4CIDR(v4); err != nil {
				return err
			}
			nv4 = v4
		}
	}
	if v6 != "" {
		if strings.EqualFold(v6, "none") {
			nv6 = ""
		} else {
			if err := validV6CIDR(v6); err != nil {
				return err
			}
			nv6 = v6
		}
	}
	if nv4 == "" && nv6 == "" {
		return fmt.Errorf("a network needs at least one subnet (v4 or v6)")
	}
	n.Subnet4, n.Subnet6 = nv4, nv6
	return nil
}

// NetworkSetAddress sets this node's own overlay address on the given network
// (CIDR, e.g. "10.42.0.5/16"). An empty value leaves a family unchanged; "none"
// clears it, restoring DAD self-assignment. Each address must be a valid host
// CIDR for its family, fall inside the network's subnet for that family (when
// one is set), and — critically — carry that subnet's own prefix length, not
// just any length that happens to contain the address.
//
// That last requirement isn't pedantry: gravinet assigns the overlay address
// to the interface as a point-to-point pair (local == dest) with this exact
// prefix length standing in for the netmask, specifically so the OS derives a
// connected route to the *entire* subnet rather than to just this one host
// (see tun_darwin.go's AddIPv4 for why). Typing "10.42.0.5/32" here — the
// natural instinct when thinking of "my address" as a single host — silently
// produces exactly that: a working address with no route to any other peer,
// which looks identical to a mesh outage for every peer except ones reachable
// through some other route entirely. Rejecting the mismatch up front, with a
// message naming the subnet's actual prefix length, catches it at entry
// instead of as a support case days later.
//
// Note on liveness: this persists immediately, but a running node only adopts a
// changed address on its next (re)start of the network — the hot reload does not
// re-address an already-configured interface. Callers should surface that.
func (c *Config) NetworkSetAddress(ref, v4, v6 string) error {
	n := c.FindNetwork(ref)
	if n == nil {
		return fmt.Errorf("no network named %q", ref)
	}
	a4, a6 := n.Address4, n.Address6
	if v4 != "" {
		if strings.EqualFold(strings.TrimSpace(v4), "none") {
			a4 = ""
		} else {
			p, err := netip.ParsePrefix(strings.TrimSpace(v4))
			if err != nil || !p.Addr().Is4() {
				return fmt.Errorf("address4 %q: must be an IPv4 CIDR (e.g. 10.42.0.5/16)", v4)
			}
			if n.Subnet4 != "" {
				sub, serr := netip.ParsePrefix(n.Subnet4)
				if serr == nil {
					if !sub.Contains(p.Addr()) {
						return fmt.Errorf("address4 %q is not inside subnet4 %s", v4, n.Subnet4)
					}
					if p.Bits() != sub.Bits() {
						return fmt.Errorf("address4 %q must use subnet4's own prefix length /%d (e.g. %s/%d), not /%d — "+
							"a shorter or /32-style length here breaks this node's route to the rest of the overlay",
							v4, sub.Bits(), p.Addr(), sub.Bits(), p.Bits())
					}
				}
			}
			a4 = p.String()
		}
	}
	if v6 != "" {
		if strings.EqualFold(strings.TrimSpace(v6), "none") {
			a6 = ""
		} else {
			p, err := netip.ParsePrefix(strings.TrimSpace(v6))
			if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() {
				return fmt.Errorf("address6 %q: must be an IPv6 CIDR (e.g. fd00:42::5/64)", v6)
			}
			if n.Subnet6 != "" {
				sub, serr := netip.ParsePrefix(n.Subnet6)
				if serr == nil {
					if !sub.Contains(p.Addr()) {
						return fmt.Errorf("address6 %q is not inside subnet6 %s", v6, n.Subnet6)
					}
					if p.Bits() != sub.Bits() {
						return fmt.Errorf("address6 %q must use subnet6's own prefix length /%d (e.g. %s/%d), not /%d — "+
							"a shorter or /128-style length here breaks this node's route to the rest of the overlay",
							v6, sub.Bits(), p.Addr(), sub.Bits(), p.Bits())
					}
				}
			}
			a6 = p.String()
		}
	}
	n.Address4, n.Address6 = a4, a6
	return nil
}

// NetworkJoin sets the network's key (creating the network if needed) and adds an
// optional seed peer. Empty v4/v6 on creation auto-assign as in NetworkAdd.
// NetworkJoin joins an existing network by its id (the on-the-wire identity that
// must match the rest of the mesh). The name and subnet are left blank and learned
// from the seed once the node peers, unless a subnet override is given.
func (c *Config) NetworkJoin(id, key, peer, v4, v6 string) error {
	canon, err := canonNetworkID(id)
	if err != nil {
		return fmt.Errorf("join requires a valid network id: %w", err)
	}
	if key == "" {
		return fmt.Errorf("join requires a key")
	}
	if _, err := crypto.DecodeKey(key); err != nil {
		return fmt.Errorf("invalid key: %w", err)
	}
	var sub4, sub6 string
	if v4 != "" {
		if err := validV4CIDR(v4); err != nil {
			return err
		}
		sub4 = v4
	}
	if v6 != "" {
		if err := validV6CIDR(v6); err != nil {
			return err
		}
		sub6 = v6
	}
	n := c.FindNetwork(canon)
	if n == nil {
		nn := NewNetworkDefaults()
		nn.ID = canon
		nn.Name = "" // learned from the network on first handshake
		nn.Subnet4, nn.Subnet6 = sub4, sub6
		c.Networks = append(c.Networks, nn)
		n = &c.Networks[len(c.Networks)-1]
	} else if sub4 != "" || sub6 != "" {
		n.Subnet4, n.Subnet6 = sub4, sub6
	}
	n.Keys[0] = KeySlot{Key: key, Label: "key0", Enabled: true}
	n.Enabled = true
	if peer = strings.TrimSpace(peer); peer != "" && !containsSeedAddr(n.Seeds, peer) {
		n.Seeds = append(n.Seeds, Seed{Address: peer})
	}
	return nil
}

// canonNetworkID validates a hex network id and returns it zero-padded to 16 chars.
func canonNetworkID(s string) (string, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 16, 64)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%016x", v), nil
}

// ---- routes ------------------------------------------------------------------

// RouteAdd redistributes a CIDR on a network (ensuring it's enabled).
func (c *Config) RouteAdd(netName, cidr string, metric int) error {
	n, err := c.routeTarget(netName, cidr)
	if err != nil {
		return err
	}
	for i := range n.Routes {
		if n.Routes[i].CIDR == cidr {
			n.Routes[i].Enabled = true
			n.Routes[i].Metric = metric
			return nil
		}
	}
	n.Routes = append(n.Routes, Route{CIDR: cidr, Metric: metric, Enabled: true})
	return nil
}

// RouteDelete removes a redistributed or rejected route.
func (c *Config) RouteDelete(netName, cidr string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	out := n.Routes[:0]
	for _, r := range n.Routes {
		if r.CIDR != cidr {
			out = append(out, r)
		}
	}
	n.Routes = out
	n.RouteRej = removeReject(n.RouteRej, cidr)
	return nil
}

// RouteReject adds (or updates) a CIDR in the reject list. inclusive controls
// whether the entry also rejects more-specific networks contained within the
// CIDR; when false (the default) it matches only the exact advertised prefix.
func (c *Config) RouteReject(netName, cidr string, inclusive bool) error {
	n, err := c.routeTarget(netName, cidr)
	if err != nil {
		return err
	}
	for i := range n.RouteRej {
		if n.RouteRej[i].CIDR == cidr {
			n.RouteRej[i].Inclusive = inclusive
			return nil
		}
	}
	n.RouteRej = append(n.RouteRej, RejectRoute{CIDR: cidr, Inclusive: inclusive})
	return nil
}

func removeReject(s []RejectRoute, cidr string) []RejectRoute {
	out := s[:0]
	for _, x := range s {
		if x.CIDR != cidr {
			out = append(out, x)
		}
	}
	return out
}

// RouteSetEnabled enables or disables an advertised route by CIDR. A disabled
// route stays in config (CIDR/metric intact for re-enabling) but is not
// advertised into the mesh. This mirrors the per-rule enable/disable used for
// firewall rules.
func (c *Config) RouteSetEnabled(netName, cidr string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	for i := range n.Routes {
		if n.Routes[i].CIDR == cidr {
			n.Routes[i].Enabled = on
			return nil
		}
	}
	return fmt.Errorf("no advertised route for %s", cidr)
}

// RouteRejectSetEnabled enables or disables a reject entry by CIDR. A disabled
// entry stays in config but is not applied, so routes it would have refused are
// accepted again. This mirrors the per-rule enable/disable used for firewall
// rules.
func (c *Config) RouteRejectSetEnabled(netName, cidr string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	for i := range n.RouteRej {
		if n.RouteRej[i].CIDR == cidr {
			n.RouteRej[i].Disabled = !on
			return nil
		}
	}
	return fmt.Errorf("no reject entry for %s", cidr)
}

// SeedParts splits an optional transport scheme from a seed address.
// "tcp://host:port" -> ("tcp", "host:port"); "udp://host" or a bare host ->
// ("udp", "host"). A tcp:// seed is dialed over the TCP/TLS fallback directly
// (cold bootstrap when UDP is blocked end to end); everything else is UDP.
func SeedParts(addr string) (transport, hostport string) {
	addr = strings.TrimSpace(addr)
	low := strings.ToLower(addr)
	switch {
	case strings.HasPrefix(low, "tcp://"):
		return "tcp", addr[len("tcp://"):]
	case strings.HasPrefix(low, "udp://"):
		return "udp", addr[len("udp://"):]
	}
	return "udp", addr
}

// validateSeedAddr checks a bootstrap endpoint string. A bare host (or IP) is
// allowed — the node falls back to the primary port and gravinet's own
// built-in fallback set; if a port is given it must be numeric and in range.
// More than one port may be given as a comma-separated list
// ("host:port,port,..."), each tried as its own dial candidate against the
// same host — same idea as the built-in no-port expansion, but an
// operator-chosen list instead of the built-in one (e.g. ports a restrictive
// firewall is known to pass). An optional "tcp://" / "udp://" scheme is
// accepted.
func validateSeedAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fmt.Errorf("seed address required")
	}
	_, addr = SeedParts(addr)
	if addr == "" {
		return fmt.Errorf("seed address required")
	}
	if strings.ContainsAny(addr, " \t") {
		return fmt.Errorf("seed address %q must not contain spaces", addr)
	}
	if host, ports, err := net.SplitHostPort(addr); err == nil {
		if host == "" {
			return fmt.Errorf("seed %q: missing host", addr)
		}
		for _, p := range strings.Split(ports, ",") {
			p = strings.TrimSpace(p)
			if pn, perr := strconv.Atoi(p); perr != nil || pn < 1 || pn > 65535 {
				return fmt.Errorf("seed %q: port %q must be 1-65535", addr, p)
			}
		}
	}
	return nil
}

// SeedAdd appends an underlay bootstrap endpoint (host or host:port) to a
// network, de-duplicated. Seeds persist in config regardless of whether any peer
// is currently connected. The new entry starts with empty Notes; use
// SeedSetNotes to attach one.
func (c *Config) SeedAdd(netName, addr string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	addr = strings.TrimSpace(addr)
	if err := validateSeedAddr(addr); err != nil {
		return err
	}
	if !containsSeedAddr(n.Seeds, addr) {
		n.Seeds = append(n.Seeds, Seed{Address: addr})
	}
	return nil
}

// SeedRemove deletes a bootstrap endpoint from a network. The running daemon
// keeps an already-dialed endpoint until its next restart.
func (c *Config) SeedRemove(netName, addr string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	n.Seeds = removeSeedAddr(n.Seeds, strings.TrimSpace(addr))
	return nil
}

// SeedSetNotes attaches (or clears, if notes is empty) an operator note to an
// already-configured seed by address. Local-only — never dialed, matched, or
// carried in a join token.
func (c *Config) SeedSetNotes(netName, addr, notes string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	addr = strings.TrimSpace(addr)
	for i := range n.Seeds {
		if n.Seeds[i].Address == addr {
			n.Seeds[i].Notes = strings.TrimSpace(notes)
			return nil
		}
	}
	return fmt.Errorf("no seed %q on network %q", addr, netName)
}

// SeedUpdateAddr changes an already-configured seed's address in place —
// used when the web UI edits a seed's host/port or flips its udp/tcp
// transport — preserving that seed's Notes and its position in the list.
//
// This used to be done client-side as SeedAdd-then-SeedRemove. SeedAdd always
// starts a brand-new entry with empty Notes and appends it at the end of the
// slice, so that sequence silently wiped the seed's notes and moved its row
// to the bottom of the table on every single address or transport edit. An
// in-place rename has the exact same live effect — ReloadRuntime's seed
// handling is additive-only (ranges over spec.Seeds and dials whatever isn't
// already dialed; a stale address is simply left dialed until restart either
// way), so the new address still gets dialed on the next reload — without
// either side effect.
func (c *Config) SeedUpdateAddr(netName, oldAddr, newAddr string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	oldAddr = strings.TrimSpace(oldAddr)
	newAddr = strings.TrimSpace(newAddr)
	if err := validateSeedAddr(newAddr); err != nil {
		return err
	}
	idx := -1
	for i := range n.Seeds {
		if n.Seeds[i].Address == oldAddr {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("no seed %q on network %q", oldAddr, netName)
	}
	if newAddr != oldAddr && containsSeedAddr(n.Seeds, newAddr) {
		return fmt.Errorf("seed %q already exists on network %q", newAddr, netName)
	}
	n.Seeds[idx].Address = newAddr
	return nil
}

func (c *Config) routeTarget(netName, cidr string) (*Network, error) {
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		return nil, fmt.Errorf("invalid CIDR %q: %v", cidr, err)
	}
	return c.PickNetwork(netName)
}

// ---- NAT ---------------------------------------------------------------------

func (c *Config) NATSetEnabled(netName string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	n.NAT.Enabled = on
	return nil
}

// FirewallSetEnabled turns the packet filter on or off for a network. When off,
// all traffic is allowed; when on with no rules, the default policy is allow.
func (c *Config) FirewallSetEnabled(netName string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	n.Firewall.Enabled = on
	return nil
}

// FirewallMarkObjectsCatalogSeeded / FirewallMarkServicesCatalogSeeded record
// that this node's well-known object/service catalog has been populated
// once, node-wide (see Config.ObjectsCatalogSeeded's doc comment).
// Idempotent — safe to call again once already marked.
func (c *Config) FirewallMarkObjectsCatalogSeeded() error {
	c.ObjectsCatalogSeeded = true
	return nil
}
func (c *Config) FirewallMarkServicesCatalogSeeded() error {
	c.ServicesCatalogSeeded = true
	return nil
}

// PeerSetEnabled enables or disables a peer locally by node id. Disabling adds
// the id to the network's local DisabledPeers blocklist (this node refuses to
// connect to it); enabling removes it. This is local-only and never floods to
// the mesh — unlike a ban.
func (c *Config) PeerSetEnabled(netName, nodeID string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if nodeID == "" {
		return fmt.Errorf("empty peer node id")
	}
	kept := make([]string, 0, len(n.DisabledPeers))
	for _, id := range n.DisabledPeers {
		if id != nodeID {
			kept = append(kept, id)
		}
	}
	if !on {
		kept = append(kept, nodeID)
	}
	n.DisabledPeers = kept
	return nil
}

// PeerSetNotes attaches (or clears, if notes is empty) an operator note to a
// peer by node id. Local-only and purely informational — like PeerSetEnabled,
// never gossiped, but unlike it, never consulted by the engine for anything
// but display. The peer itself isn't persisted here; only the note survives
// across the peer's connect/disconnect cycles and node restarts.
func (c *Config) PeerSetNotes(netName, nodeID, notes string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if nodeID == "" {
		return fmt.Errorf("empty peer node id")
	}
	notes = strings.TrimSpace(notes)
	if notes == "" {
		delete(n.PeerNotes, nodeID)
		return nil
	}
	if n.PeerNotes == nil {
		n.PeerNotes = map[string]string{}
	}
	n.PeerNotes[nodeID] = notes
	return nil
}

// FirewallRuleSetEnabled enables or disables a single firewall rule by its
// position index (0-based). Disabled rules are skipped during evaluation.
func (c *Config) FirewallRuleSetEnabled(netName string, idx int, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if idx < 0 || idx >= len(n.Firewall.Rules) {
		return fmt.Errorf("rule index %d out of range", idx)
	}
	n.Firewall.Rules[idx].Disabled = !on
	return nil
}

// FirewallRuleAdd inserts a rule at position idx (-1 = append).
func (c *Config) FirewallRuleAdd(netName string, r FirewallRule, at int) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	r.Disabled = false // new rules are active by default
	if at < 0 || at >= len(n.Firewall.Rules) {
		n.Firewall.Rules = append(n.Firewall.Rules, r)
	} else {
		n.Firewall.Rules = append(n.Firewall.Rules, FirewallRule{})
		copy(n.Firewall.Rules[at+1:], n.Firewall.Rules[at:])
		n.Firewall.Rules[at] = r
	}
	return nil
}

// FirewallRuleDelete removes rules by their 0-based position indices.
// Indices are processed high-to-low so earlier removals don't shift later ones.
func (c *Config) FirewallRuleDelete(netName string, idxs []int) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	// sort descending so we can splice without index drift
	for i := 0; i < len(idxs)-1; i++ {
		for j := i + 1; j < len(idxs); j++ {
			if idxs[j] > idxs[i] {
				idxs[i], idxs[j] = idxs[j], idxs[i]
			}
		}
	}
	for _, idx := range idxs {
		if idx < 0 || idx >= len(n.Firewall.Rules) {
			return fmt.Errorf("rule index %d out of range", idx)
		}
		n.Firewall.Rules = append(n.Firewall.Rules[:idx], n.Firewall.Rules[idx+1:]...)
	}
	return nil
}

// ---- firewall exempt allowlist ----------------------------------------------

// ParseExemptProto resolves an exemption protocol token to its IP protocol
// number. It accepts the named protocols, "ospf", "any"/"" (0 = any), and a raw
// decimal number. The bool reports whether the token was understood.
func ParseExemptProto(s string) (uint8, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "any":
		return 0, true
	case "tcp":
		return 6, true
	case "udp":
		return 17, true
	case "icmp":
		return 1, true
	case "ospf":
		return 89, true
	}
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 && n <= 255 {
		return uint8(n), true
	}
	return 0, false
}

func validateExempt(e FirewallExempt) error {
	if _, ok := ParseExemptProto(e.Proto); !ok {
		return fmt.Errorf("invalid proto %q (use tcp|udp|icmp|ospf|<number>|any)", e.Proto)
	}
	if e.Port < 0 || e.Port > 65535 {
		return fmt.Errorf("port %d out of range", e.Port)
	}
	return nil
}

// FirewallExemptList returns the node-global effective exemption list — the
// built-in defaults when none has been configured — plus whether it is the
// (unmodified) default set.
func (c *Config) FirewallExemptList() ([]FirewallExempt, bool) {
	return c.EffectiveFirewallExempt(), c.FirewallExempts == nil
}

// FirewallExemptSet replaces the entire node-global allowlist after validating
// every entry. Passing an empty (non-nil) slice means "no exemptions", which is
// distinct from the unset/default state. This is the one mutator the editable
// UI needs: add, remove, and edit all reduce to setting the whole list.
func (c *Config) FirewallExemptSet(list []FirewallExempt) error {
	for i, e := range list {
		if err := validateExempt(e); err != nil {
			return fmt.Errorf("exempt[%d]: %w", i, err)
		}
	}
	if list == nil {
		list = []FirewallExempt{}
	}
	c.FirewallExempts = list
	return nil
}

// FirewallExemptAdd appends one exemption to the global list, materializing the
// built-in defaults first so adding a custom entry never drops the protective
// defaults.
func (c *Config) FirewallExemptAdd(e FirewallExempt) error {
	if err := validateExempt(e); err != nil {
		return err
	}
	if c.FirewallExempts == nil {
		c.FirewallExempts = DefaultFirewallExempts()
	}
	c.FirewallExempts = append(c.FirewallExempts, e)
	return nil
}

// FirewallExemptReset reverts the global allowlist to the built-in defaults.
func (c *Config) FirewallExemptReset() {
	c.FirewallExempts = nil
}

// FirewallExemptDelete removes global exemptions by 0-based index, materializing
// the built-in defaults first so a delete from the default set takes effect. An
// emptied list stays non-nil ("no exemptions"), distinct from the default state.
func (c *Config) FirewallExemptDelete(idxs []int) error {
	if c.FirewallExempts == nil {
		c.FirewallExempts = DefaultFirewallExempts()
	}
	for i := 0; i < len(idxs)-1; i++ {
		for j := i + 1; j < len(idxs); j++ {
			if idxs[j] > idxs[i] {
				idxs[i], idxs[j] = idxs[j], idxs[i]
			}
		}
	}
	for _, idx := range idxs {
		if idx < 0 || idx >= len(c.FirewallExempts) {
			return fmt.Errorf("exempt index %d out of range", idx)
		}
		c.FirewallExempts = append(c.FirewallExempts[:idx], c.FirewallExempts[idx+1:]...)
	}
	if c.FirewallExempts == nil {
		c.FirewallExempts = []FirewallExempt{}
	}
	return nil
}

// FirewallExemptSetEnabled enables or disables the global allowlist entry at the
// given 0-based index (as shown by 'fw exempt list' / the UI). It materializes
// the built-in defaults first so toggling a default entry takes effect. A
// disabled entry stays in the list but is not applied, so its traffic class is
// once again subject to the rulebase. This mirrors the per-rule enable/disable
// used for firewall rules.
func (c *Config) FirewallExemptSetEnabled(idx int, on bool) error {
	if c.FirewallExempts == nil {
		c.FirewallExempts = DefaultFirewallExempts()
	}
	if idx < 0 || idx >= len(c.FirewallExempts) {
		return fmt.Errorf("exempt index %d out of range", idx)
	}
	c.FirewallExempts[idx].Disabled = !on
	return nil
}

func (c *Config) FirewallRuleMove(netName string, fromIdx, toIdx int) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	rules := n.Firewall.Rules
	if fromIdx < 0 || fromIdx >= len(rules) || toIdx < 0 || toIdx >= len(rules) {
		return fmt.Errorf("rule index out of range")
	}
	r := rules[fromIdx]
	rules = append(rules[:fromIdx], rules[fromIdx+1:]...)
	newRules := make([]FirewallRule, 0, len(rules)+1)
	newRules = append(newRules, rules[:toIdx]...)
	newRules = append(newRules, r)
	newRules = append(newRules, rules[toIdx:]...)
	n.Firewall.Rules = newRules
	return nil
}

// NATAdd adds a masquerade (overlay→underlay) rule out the given interface.
func (c *Config) NATAdd(netName, iface string) error {
	if iface == "" {
		return fmt.Errorf("NAT rule needs an interface")
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	for _, r := range n.NAT.Rules {
		if r.Interface == iface {
			return fmt.Errorf("NAT rule for %s already exists", iface)
		}
	}
	n.NAT.Enabled = true
	n.NAT.Rules = append(n.NAT.Rules, NATRule{
		Direction: NATOverlayToUnderlay,
		Translate: "masquerade",
		Interface: iface,
		Enabled:   true,
	})
	return nil
}

func (c *Config) NATDelete(netName, iface string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	out := n.NAT.Rules[:0]
	found := false
	for _, r := range n.NAT.Rules {
		if r.Interface == iface {
			found = true
			continue
		}
		out = append(out, r)
	}
	n.NAT.Rules = out
	if !found {
		return fmt.Errorf("no NAT rule for interface %s", iface)
	}
	return nil
}

// validNATCIDR accepts an empty string (meaning "any"), a bare IPv4 address
// (treated as /32), or an IPv4 CIDR. It returns a normalized form for storage.
func validNATCIDR(field, s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") {
		return "", nil
	}
	if ip, err := netip.ParseAddr(s); err == nil {
		if !ip.Is4() {
			return "", fmt.Errorf("%s %q: NAT is IPv4-only", field, s)
		}
		return s, nil
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		if !p.Addr().Is4() {
			return "", fmt.Errorf("%s %q: NAT is IPv4-only", field, s)
		}
		return s, nil
	}
	return "", fmt.Errorf("%s %q: not an IPv4 address or CIDR", field, s)
}

// NATRuleAdd appends a full NAT rule. direction is one of overlay2underlay,
// underlay2overlay, overlay2overlay (default overlay2underlay). source/dest are
// empty ("any") or IPv4 addresses/CIDRs. translate is either "masquerade" (which
// requires iface, whose primary IPv4 is used) or a literal IPv4 target.
// buildNATRule validates and normalizes the user-supplied fields of a NAT rule
// into a NATRule (with Enabled left false for the caller to set). It is shared by
// NATRuleAdd and NATRuleUpdateAt so adding and editing enforce identical rules:
// a recognized direction, valid source/dest CIDRs, and a translate target that
// is either "masquerade" (which requires an egress interface) or a literal IPv4
// (which clears the interface).
func buildNATRule(direction, source, dest, translate, iface string) (NATRule, error) {
	dir := NATDirection(strings.ToLower(strings.TrimSpace(direction)))
	switch dir {
	case "":
		dir = NATOverlayToUnderlay
	case NATOverlayToUnderlay, NATUnderlayToOverlay, NATOverlayToOverlay:
	default:
		return NATRule{}, fmt.Errorf("direction must be overlay2underlay, underlay2overlay, or overlay2overlay")
	}
	src, err := validNATCIDR("source", source)
	if err != nil {
		return NATRule{}, err
	}
	dst, err := validNATCIDR("dest", dest)
	if err != nil {
		return NATRule{}, err
	}
	translate = strings.TrimSpace(translate)
	iface = strings.TrimSpace(iface)
	masq := translate == "" || strings.EqualFold(translate, "masquerade")
	if masq {
		if iface == "" {
			return NATRule{}, fmt.Errorf("masquerade needs an interface (translate=masquerade requires iface)")
		}
		translate = "masquerade"
	} else {
		ip, perr := netip.ParseAddr(translate)
		if perr != nil || !ip.Is4() {
			return NATRule{}, fmt.Errorf("translate %q: must be an IPv4 address or \"masquerade\"", translate)
		}
		iface = ""
	}
	return NATRule{Direction: dir, Source: src, Dest: dst, Translate: translate, Interface: iface}, nil
}

func (c *Config) NATRuleAdd(netName, direction, source, dest, translate, iface string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	rule, err := buildNATRule(direction, source, dest, translate, iface)
	if err != nil {
		return err
	}
	rule.Enabled = true
	n.NAT.Enabled = true
	n.NAT.Rules = append(n.NAT.Rules, rule)
	return nil
}

// NATRuleUpdateAt replaces the rule at index idx (as shown by NAT list / the UI)
// in place, preserving its enabled/disabled state and its position. It backs the
// click-to-edit rule fields in the UI. Validation matches NATRuleAdd.
func (c *Config) NATRuleUpdateAt(netName string, idx int, direction, source, dest, translate, iface string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if idx < 0 || idx >= len(n.NAT.Rules) {
		return fmt.Errorf("no NAT rule at index %d (have %d)", idx, len(n.NAT.Rules))
	}
	rule, err := buildNATRule(direction, source, dest, translate, iface)
	if err != nil {
		return err
	}
	rule.Enabled = n.NAT.Rules[idx].Enabled // preserve current state
	n.NAT.Rules[idx] = rule
	return nil
}

// NATRuleDeleteAt removes the rule at index idx (as shown by NAT list / the UI).
func (c *Config) NATRuleDeleteAt(netName string, idx int) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if idx < 0 || idx >= len(n.NAT.Rules) {
		return fmt.Errorf("no NAT rule at index %d (have %d)", idx, len(n.NAT.Rules))
	}
	n.NAT.Rules = append(n.NAT.Rules[:idx], n.NAT.Rules[idx+1:]...)
	return nil
}

// NATRuleSetEnabled enables or disables the NAT rule at index idx (as shown by
// NAT list / the UI). A disabled rule stays in config (match intact for
// re-enabling) but is skipped when translating. This mirrors the per-rule
// enable/disable used for firewall rules.
func (c *Config) NATRuleSetEnabled(netName string, idx int, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if idx < 0 || idx >= len(n.NAT.Rules) {
		return fmt.Errorf("no NAT rule at index %d (have %d)", idx, len(n.NAT.Rules))
	}
	n.NAT.Rules[idx].Enabled = on
	return nil
}

// NATStateTimeoutSet sets the global idle lifetime (seconds) of tracked NAT
// connections before their mappings are reclaimed. 0 = default (120s).
func (c *Config) NATStateTimeoutSet(seconds int) error {
	if seconds < 0 || seconds > 86400 {
		return fmt.Errorf("state timeout must be 0..86400 seconds")
	}
	c.NATStateTimeout = seconds
	return nil
}

// ---- Custom hosts records ----------------------------------------------------

// HostAdd adds (or updates) a custom name -> IP record this node advertises.
func (c *Config) HostAdd(netName, name, ip string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("host name required")
	}
	if _, err := netip.ParseAddr(strings.TrimSpace(ip)); err != nil {
		return fmt.Errorf("invalid ip %q", ip)
	}
	ip = strings.TrimSpace(ip)
	for i := range n.HostsAdvertise {
		if n.HostsAdvertise[i].Name == name {
			n.HostsAdvertise[i].IP = ip // update existing
			return nil
		}
	}
	n.HostsAdvertise = append(n.HostsAdvertise, HostRecord{Name: name, IP: ip})
	return nil
}

// HostUpdate edits the record currently named oldName in place: it can rename it
// (newName) and/or change its IP, preserving the record's enabled/disabled state
// and its position in the list. Renaming onto a different existing record is
// rejected. This backs the click-to-edit name/IP cells in the UI.
func (c *Config) HostUpdate(netName, oldName, newName, ip string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("host name required")
	}
	if _, err := netip.ParseAddr(strings.TrimSpace(ip)); err != nil {
		return fmt.Errorf("invalid ip %q", ip)
	}
	ip = strings.TrimSpace(ip)
	idx := -1
	for i := range n.HostsAdvertise {
		if n.HostsAdvertise[i].Name == oldName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no host record named %q", oldName)
	}
	if newName != oldName {
		for i := range n.HostsAdvertise {
			if i != idx && n.HostsAdvertise[i].Name == newName {
				return fmt.Errorf("host record %q already exists", newName)
			}
		}
	}
	n.HostsAdvertise[idx].Name = newName
	n.HostsAdvertise[idx].IP = ip
	return nil
}

// HostDelete removes a custom record by name.
func (c *Config) HostDelete(netName, name string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	out := n.HostsAdvertise[:0]
	found := false
	for _, h := range n.HostsAdvertise {
		if h.Name == name {
			found = true
			continue
		}
		out = append(out, h)
	}
	if !found {
		return fmt.Errorf("no host record named %q", name)
	}
	n.HostsAdvertise = out
	return nil
}

// HostSetEnabled enables or disables a single advertised host record by name.
// A disabled record is kept in config (so it can be re-enabled with its IP
// intact) but is withheld from the mesh advertisement. This mirrors the
// per-rule enable/disable used for firewall rules.
func (c *Config) HostSetEnabled(netName, name string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	for i := range n.HostsAdvertise {
		if n.HostsAdvertise[i].Name == name {
			n.HostsAdvertise[i].Disabled = !on
			return nil
		}
	}
	return fmt.Errorf("no host record named %q", name)
}

// HostRejectAdd adds (or re-enables) a hostname this node refuses to accept from
// the mesh — peers' advertised records for that name are not written into this
// node's hosts file. Re-adding an existing entry clears its disabled flag.
func (c *Config) HostRejectAdd(netName, name string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("host name required")
	}
	for i := range n.HostsReject {
		if strings.EqualFold(n.HostsReject[i].Name, name) {
			n.HostsReject[i].Disabled = false // re-adding re-enables
			return nil
		}
	}
	n.HostsReject = append(n.HostsReject, HostReject{Name: name})
	return nil
}

// HostRejectDelete removes a reject entry by name.
func (c *Config) HostRejectDelete(netName, name string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	out := n.HostsReject[:0]
	found := false
	for _, h := range n.HostsReject {
		if strings.EqualFold(h.Name, name) {
			found = true
			continue
		}
		out = append(out, h)
	}
	if !found {
		return fmt.Errorf("no host reject named %q", name)
	}
	n.HostsReject = out
	return nil
}

// HostRejectSetEnabled enables or disables a reject entry by name. A disabled
// entry stays in config but stops filtering, so the affected records are
// accepted again. This mirrors the per-rule enable/disable used elsewhere.
func (c *Config) HostRejectSetEnabled(netName, name string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	for i := range n.HostsReject {
		if strings.EqualFold(n.HostsReject[i].Name, name) {
			n.HostsReject[i].Disabled = !on
			return nil
		}
	}
	return fmt.Errorf("no host reject named %q", name)
}

// ---- Conditional DNS forwarding ------------------------------------------------

// parseServerList splits a comma-separated server list, trims whitespace
// around each entry, and validates every one as an IP. Used by both
// DNSForwardAdd and DNSForwardUpdate so the two share one error message.
func parseServerList(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if _, err := netip.ParseAddr(p); err != nil {
			return nil, fmt.Errorf("invalid server %q", p)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one server is required")
	}
	return out, nil
}

// DNSForwardAdd adds (or updates) a conditional-forwarding domain this node
// advertises: queries under domain are routed to servers (comma-separated)
// mesh-wide via the OS's native split-DNS mechanism. See internal/resolver.
func (c *Config) DNSForwardAdd(netName, domain, servers string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return fmt.Errorf("domain required")
	}
	list, err := parseServerList(servers)
	if err != nil {
		return err
	}
	for i := range n.DNSAdvertise {
		if n.DNSAdvertise[i].Domain == domain {
			n.DNSAdvertise[i].Servers = list // update existing
			return nil
		}
	}
	n.DNSAdvertise = append(n.DNSAdvertise, DNSForward{Domain: domain, Servers: list})
	return nil
}

// DNSForwardUpdate edits the forward currently named oldDomain in place: it can
// rename the domain and/or change its server list, preserving the record's
// enabled/disabled state and its position in the list. This backs the
// click-to-edit domain/servers cells in the UI, mirroring HostUpdate.
func (c *Config) DNSForwardUpdate(netName, oldDomain, newDomain, servers string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	oldDomain = strings.TrimSpace(oldDomain)
	newDomain = strings.TrimSpace(newDomain)
	if newDomain == "" {
		return fmt.Errorf("domain required")
	}
	list, err := parseServerList(servers)
	if err != nil {
		return err
	}
	idx := -1
	for i := range n.DNSAdvertise {
		if n.DNSAdvertise[i].Domain == oldDomain {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no dns forward for domain %q", oldDomain)
	}
	if newDomain != oldDomain {
		for i := range n.DNSAdvertise {
			if i != idx && n.DNSAdvertise[i].Domain == newDomain {
				return fmt.Errorf("dns forward for domain %q already exists", newDomain)
			}
		}
	}
	n.DNSAdvertise[idx].Domain = newDomain
	n.DNSAdvertise[idx].Servers = list
	return nil
}

// DNSForwardDelete removes a conditional-forward by domain.
func (c *Config) DNSForwardDelete(netName, domain string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	domain = strings.TrimSpace(domain)
	out := n.DNSAdvertise[:0]
	found := false
	for _, d := range n.DNSAdvertise {
		if d.Domain == domain {
			found = true
			continue
		}
		out = append(out, d)
	}
	if !found {
		return fmt.Errorf("no dns forward for domain %q", domain)
	}
	n.DNSAdvertise = out
	return nil
}

// DNSForwardSetEnabled enables or disables a single advertised forward by
// domain. A disabled forward is kept in config (so it can be re-enabled with
// its servers intact) but is withheld from the mesh advertisement — mirrors
// HostSetEnabled.
func (c *Config) DNSForwardSetEnabled(netName, domain string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	domain = strings.TrimSpace(domain)
	for i := range n.DNSAdvertise {
		if n.DNSAdvertise[i].Domain == domain {
			n.DNSAdvertise[i].Disabled = !on
			return nil
		}
	}
	return fmt.Errorf("no dns forward for domain %q", domain)
}

// DNSRejectAdd adds (or re-enables) a domain this node refuses to accept a
// conditional-forward for from the mesh — mirrors HostRejectAdd.
func (c *Config) DNSRejectAdd(netName, domain string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return fmt.Errorf("domain required")
	}
	for i := range n.DNSReject {
		if strings.EqualFold(n.DNSReject[i].Domain, domain) {
			n.DNSReject[i].Disabled = false // re-adding re-enables
			return nil
		}
	}
	n.DNSReject = append(n.DNSReject, DNSReject{Domain: domain})
	return nil
}

// DNSRejectDelete removes a reject entry by domain.
func (c *Config) DNSRejectDelete(netName, domain string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	domain = strings.TrimSpace(domain)
	out := n.DNSReject[:0]
	found := false
	for _, d := range n.DNSReject {
		if strings.EqualFold(d.Domain, domain) {
			found = true
			continue
		}
		out = append(out, d)
	}
	if !found {
		return fmt.Errorf("no dns reject for domain %q", domain)
	}
	n.DNSReject = out
	return nil
}

// DNSRejectSetEnabled enables or disables a reject entry by domain — mirrors
// HostRejectSetEnabled.
func (c *Config) DNSRejectSetEnabled(netName, domain string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	domain = strings.TrimSpace(domain)
	for i := range n.DNSReject {
		if strings.EqualFold(n.DNSReject[i].Domain, domain) {
			n.DNSReject[i].Disabled = !on
			return nil
		}
	}
	return fmt.Errorf("no dns reject for domain %q", domain)
}

// ---- QoS ---------------------------------------------------------------------

func (c *Config) QoSSetEnabled(netName string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	n.QoS.Enabled = on
	return nil
}

// QoSAdd adds a classification rule mapping proto/port to a class index.
func (c *Config) QoSAdd(netName, proto string, port, class int) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if n.QoS.Classes < 5 {
		n.QoS.Classes = 5
	}
	if n.QoS.DefaultClass <= 0 {
		n.QoS.DefaultClass = 3
	}
	if class < 0 || class >= n.QoS.Classes {
		return fmt.Errorf("class %d out of range (0..%d)", class, n.QoS.Classes-1)
	}
	proto = strings.ToLower(proto)
	if proto != "tcp" && proto != "udp" && proto != "icmp" && proto != "any" && proto != "" {
		return fmt.Errorf("protocol must be tcp, udp, icmp, or any")
	}
	if port < 0 || port > 65535 {
		return fmt.Errorf("port %d out of range", port)
	}
	n.QoS.Enabled = true
	n.QoS.Rules = append(n.QoS.Rules, QoSRule{
		Protocol: proto, PortMin: port, PortMax: port, Class: class,
	})
	return nil
}

func (c *Config) QoSDelete(netName, proto string, port int) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	proto = strings.ToLower(proto)
	out := n.QoS.Rules[:0]
	found := false
	for _, r := range n.QoS.Rules {
		if r.Protocol == proto && r.PortMin == port {
			found = true
			continue
		}
		out = append(out, r)
	}
	n.QoS.Rules = out
	if !found {
		return fmt.Errorf("no QoS rule for %s port %d", proto, port)
	}
	return nil
}

// QoSRuleSetEnabled enables or disables the classification rule(s) matching
// proto/port. A disabled rule stays in config (match intact for re-enabling) but
// is skipped by the classifier. It is keyed the same way as QoSDelete, so it
// toggles every rule sharing that proto/port. This mirrors the per-rule
// enable/disable used for firewall rules.
func (c *Config) QoSRuleSetEnabled(netName, proto string, port int, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	proto = strings.ToLower(proto)
	found := false
	for i := range n.QoS.Rules {
		if n.QoS.Rules[i].Protocol == proto && n.QoS.Rules[i].PortMin == port {
			n.QoS.Rules[i].Disabled = !on
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no QoS rule for %s port %d", proto, port)
	}
	return nil
}

// QoSSetClassDSCP overrides class's outbound DSCP mark. Every class already
// marks its traffic with a standard-codepoint default (see
// mesh.DefaultClassDSCP); this is only needed to match a specific
// organization's existing Diffserv policy instead of that default.
func (c *Config) QoSSetClassDSCP(netName string, class, dscp int) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if n.QoS.Classes < 5 {
		n.QoS.Classes = 5
	}
	if class < 0 || class >= n.QoS.Classes {
		return fmt.Errorf("class %d out of range (0..%d)", class, n.QoS.Classes-1)
	}
	if dscp < 0 || dscp > 63 {
		return fmt.Errorf("dscp %d out of range (0..63)", dscp)
	}
	for len(n.QoS.ClassDSCP) <= class {
		n.QoS.ClassDSCP = append(n.QoS.ClassDSCP, -1)
	}
	n.QoS.ClassDSCP[class] = dscp
	return nil
}

// QoSClearClassDSCP removes a class's DSCP override, reverting it to the
// standard-codepoint default.
func (c *Config) QoSClearClassDSCP(netName string, class int) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if class < 0 || class >= len(n.QoS.ClassDSCP) || n.QoS.ClassDSCP[class] < 0 {
		return fmt.Errorf("no DSCP override for class %d", class)
	}
	n.QoS.ClassDSCP[class] = -1
	return nil
}

// ---- bandwidth ---------------------------------------------------------------

// ThrottleSet sets the up/down/both rate (bytes/s) on a network. It changes only
// the rate, never the on/off state — turning the limiter on or off is the job of
// ThrottleSetEnabled (the web toggle / CLI enable|disable). Keeping these
// independent means editing a rate can't flip the enabled state out from under
// the operator: state stays consistent through editing.
func (c *Config) ThrottleSet(netName, dir string, bps int) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	switch dir {
	case "up":
		n.Throttle.UpBytesPerSec = bps
	case "down":
		n.Throttle.DownBytesPerSec = bps
	case "both":
		n.Throttle.UpBytesPerSec = bps
		n.Throttle.DownBytesPerSec = bps
	default:
		return fmt.Errorf("direction must be up, down, or both")
	}
	return nil
}

// ThrottleSetEnabled turns a network's bandwidth limit on or off without changing
// the configured rates, so a cap can be lifted temporarily and later restored.
func (c *Config) ThrottleSetEnabled(netName string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	n.Throttle.Enabled = on
	return nil
}

// ---- shared parsing/format helpers (used by CLI and web) ---------------------

// PriorityToClass maps a priority name or numeric level to a class index
// (0 = highest priority).
func PriorityToClass(level string, classes int) (int, error) {
	if classes <= 0 {
		classes = 3
	}
	if n, err := strconv.Atoi(level); err == nil {
		return clampInt(n, 0, classes-1), nil
	}
	switch strings.ToLower(level) {
	case "highest", "":
		return 0, nil
	case "high":
		return clampInt(1, 0, classes-1), nil
	case "normal", "medium", "mid":
		return classes / 2, nil
	case "low":
		return clampInt(classes-2, 0, classes-1), nil
	case "lowest":
		return classes - 1, nil
	default:
		return 0, fmt.Errorf("unknown priority %q (highest|high|normal|low|lowest or 0..%d)", level, classes-1)
	}
}

// ClassName renders a class index back to a human label.
func ClassName(class, classes int) string {
	switch {
	case class == 0:
		return "highest"
	case class == classes-1:
		return "lowest"
	case class == classes/2:
		return "normal"
	default:
		return fmt.Sprintf("class %d", class)
	}
}

// dscpNames maps the standard Diffserv codepoints this package's default
// marking ladder (mesh.DefaultClassDSCP) actually uses to their conventional
// names, for display. A DSCP value outside this set (e.g. a custom
// ClassDSCP override) just prints as a bare number.
var dscpNames = map[int]string{
	0:  "CS0",
	8:  "CS1",
	10: "AF11",
	18: "AF21",
	26: "AF31",
	34: "AF41",
	46: "EF",
}

// DSCPName renders a DSCP codepoint as "NAME(N)" when it's one of the
// standard names [gravinet] marks with by default, else just "N".
func DSCPName(dscp int) string {
	if name, ok := dscpNames[dscp]; ok {
		return fmt.Sprintf("%s(%d)", name, dscp)
	}
	return fmt.Sprintf("%d", dscp)
}

// ParseRate parses "150mbps", "1gbps", "512kbps", "1000000" (bits/s) into bytes/s.
func ParseRate(s string) (int, error) {
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "gbps"):
		mult, s = 1e9, strings.TrimSuffix(s, "gbps")
	case strings.HasSuffix(s, "mbps"):
		mult, s = 1e6, strings.TrimSuffix(s, "mbps")
	case strings.HasSuffix(s, "kbps"):
		mult, s = 1e3, strings.TrimSuffix(s, "kbps")
	case strings.HasSuffix(s, "bps"):
		mult, s = 1, strings.TrimSuffix(s, "bps")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid rate %q (try 150mbps, 1gbps, 512kbps)", orig)
	}
	return int(v * mult / 8.0), nil
}

// RateString renders bytes/s back to a human rate.
func RateString(bytesPerSec int) string {
	if bytesPerSec <= 0 {
		return "unlimited"
	}
	bits := float64(bytesPerSec) * 8
	switch {
	case bits >= 1e9:
		return fmt.Sprintf("%.3gGbps", bits/1e9)
	case bits >= 1e6:
		return fmt.Sprintf("%.3gMbps", bits/1e6)
	case bits >= 1e3:
		return fmt.Sprintf("%.3gKbps", bits/1e3)
	default:
		return fmt.Sprintf("%.0fbps", bits)
	}
}

// ---- internal helpers --------------------------------------------------------

func resolveSubnets(c *Config, v4, v6 string) (string, string, error) {
	if v4 == "" && v6 == "" {
		a, b := c.NextFreeSubnets()
		return a, b, nil
	}
	if v4 != "" {
		if err := validV4CIDR(v4); err != nil {
			return "", "", err
		}
	}
	if v6 != "" {
		if err := validV6CIDR(v6); err != nil {
			return "", "", err
		}
	}
	return v4, v6, nil
}

func validV4CIDR(s string) error {
	ip, _, err := net.ParseCIDR(s)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("subnet %q must be an IPv4 CIDR (e.g. 10.50.0.0/16); use subnet6 for IPv6", s)
	}
	return nil
}

func validV6CIDR(s string) error {
	ip, _, err := net.ParseCIDR(s)
	if err != nil || ip.To4() != nil {
		return fmt.Errorf("subnet6 %q must be an IPv6 CIDR (e.g. fd00:80::/64)", s)
	}
	return nil
}

func randomNetworkID() string {
	var b [8]byte // 16 hex chars, matching the initial-config and node-id width
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func removeStr(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// containsSeedAddr reports whether any seed in the list has the given
// address (Notes ignored — addresses are still the de-duplication key).
func containsSeedAddr(s SeedList, addr string) bool {
	for _, x := range s {
		if x.Address == addr {
			return true
		}
	}
	return false
}

// removeSeedAddr deletes every seed with the given address from the list.
func removeSeedAddr(s SeedList, addr string) SeedList {
	out := s[:0]
	for _, x := range s {
		if x.Address != addr {
			out = append(out, x)
		}
	}
	return out
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---- key slots (join / rotation) --------------------------------------------
//
// Each network has a fixed array of key slots (len(Network.Keys)). All enabled,
// non-empty keys authenticate joiners, so rotation is: generate a new key, let
// both run, distribute it, then disable/delete the old one. Key changes take
// effect on restart (keys are bound into the engine's key set at startup).

// KeySlots is the number of key slots per network.
const KeySlots = 8

// KeyFingerprint is a short, non-secret identifier for a key (so a slot can be
// referred to without revealing the key itself).
func KeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

func validSlot(slot int) error {
	if slot < 0 || slot >= KeySlots {
		return fmt.Errorf("slot %d out of range (0–%d)", slot, KeySlots-1)
	}
	return nil
}

// IsLastEnabledKey reports whether slot holds the only enabled, non-empty key —
// used to refuse changes that would leave a network with no way to authenticate.
func IsLastEnabledKey(n *Network, slot int) bool {
	if slot < 0 || slot >= len(n.Keys) || !n.Keys[slot].Enabled || n.Keys[slot].Key == "" {
		return false
	}
	for i := range n.Keys {
		if i != slot && n.Keys[i].Enabled && n.Keys[i].Key != "" {
			return false
		}
	}
	return true
}

// KeyGenerate mints a fresh key into the first free slot and returns its index
// and value (the value is shown once so it can be distributed).
func (c *Config) KeyGenerate(netName, label string) (int, string, error) {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return 0, "", err
	}
	slot := -1
	for i := range n.Keys {
		if n.Keys[i].Key == "" {
			slot = i
			break
		}
	}
	if slot < 0 {
		return 0, "", fmt.Errorf("all %d key slots are full; delete one first", KeySlots)
	}
	k, err := crypto.GenerateKey()
	if err != nil {
		return 0, "", fmt.Errorf("generate key: %w", err)
	}
	if label == "" {
		label = fmt.Sprintf("key%d", slot)
	}
	n.Keys[slot] = KeySlot{Key: k, Label: label, Enabled: true}
	return slot, k, nil
}

// KeyGenerateInto generates a fresh key directly into a specific (empty) slot,
// for the web UI's "select an empty slot, then Generate" flow.
func (c *Config) KeyGenerateInto(netName string, slot int, label string) (string, error) {
	if err := validSlot(slot); err != nil {
		return "", err
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return "", err
	}
	if n.Keys[slot].Key != "" {
		return "", fmt.Errorf("slot %d is not empty; delete it first", slot)
	}
	k, err := crypto.GenerateKey()
	if err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	if label == "" {
		label = fmt.Sprintf("key%d", slot)
	}
	n.Keys[slot] = KeySlot{Key: k, Label: label, Enabled: true}
	return k, nil
}

// KeySet imports an existing key into a slot (e.g. to join a network someone
// else created, or to pin a specific rotation key).
func (c *Config) KeySet(netName string, slot int, key, label string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	if _, err := crypto.DecodeKey(key); err != nil {
		return fmt.Errorf("invalid key: %w", err)
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if label == "" {
		if n.Keys[slot].Label != "" {
			label = n.Keys[slot].Label
		} else {
			label = fmt.Sprintf("key%d", slot)
		}
	}
	n.Keys[slot] = KeySlot{Key: key, Label: label, Enabled: true}
	return nil
}

// KeySetEnabled enables or disables a slot. It refuses to disable the last
// enabled key (which would lock the network).
// KeySetLabel changes only a slot's label (config metadata; the engine never
// uses labels, so this needs no restart).
func (c *Config) KeySetLabel(netName string, slot int, label string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if n.Keys[slot].Key == "" {
		return fmt.Errorf("slot %d is empty", slot)
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = fmt.Sprintf("key%d", slot)
	}
	n.Keys[slot].Label = label
	return nil
}

// KeySetNotes changes only a slot's notes (config metadata; unlike Label, this
// is never part of the distributed-key flood payload — a Distributed slot's
// notes stay local to this node even when its label or expiry gets pushed to
// every peer holding a copy).
func (c *Config) KeySetNotes(netName string, slot int, notes string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if n.Keys[slot].Key == "" {
		return fmt.Errorf("slot %d is empty", slot)
	}
	n.Keys[slot].Notes = strings.TrimSpace(notes)
	return nil
}

// KeySetExpiry sets (or clears, when expires is empty) a slot's expiry. The value
// must be RFC3339; past it the key stops authenticating and its sessions drop.
func (c *Config) KeySetExpiry(netName string, slot int, expires string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if n.Keys[slot].Key == "" {
		return fmt.Errorf("slot %d is empty", slot)
	}
	expires = strings.TrimSpace(expires)
	if expires != "" {
		if _, perr := time.Parse(time.RFC3339, expires); perr != nil {
			return fmt.Errorf("bad expiry %q (want RFC3339, e.g. 2026-12-31T23:59:59Z)", expires)
		}
	}
	n.Keys[slot].Expires = expires
	return nil
}

func (c *Config) KeySetEnabled(netName string, slot int, on bool) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if n.Keys[slot].Key == "" {
		return fmt.Errorf("slot %d is empty", slot)
	}
	if !on && IsLastEnabledKey(n, slot) {
		return fmt.Errorf("slot %d is the only enabled key; add another before disabling it", slot)
	}
	n.Keys[slot].Enabled = on
	return nil
}

// KeySetDistributed sets a slot's Distributed bookkeeping flag — purely local
// display/tracking state (no safety check needed, unlike enable/delete: it
// doesn't affect this node's own ability to authenticate anyone). The actual
// mesh-wide push or retraction this flag tracks is a separate engine call
// (FloodKey / RetractKey); this just records that it happened.
func (c *Config) KeySetDistributed(netName string, slot int, on bool) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if n.Keys[slot].Key == "" {
		return fmt.Errorf("slot %d is empty", slot)
	}
	n.Keys[slot].Distributed = on
	return nil
}

// KeyDelete clears a slot. It refuses to delete the last enabled key.
func (c *Config) KeyDelete(netName string, slot int) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	if n.Keys[slot].Key == "" {
		return fmt.Errorf("slot %d is already empty", slot)
	}
	if IsLastEnabledKey(n, slot) {
		return fmt.Errorf("slot %d is the only enabled key; add another before deleting it", slot)
	}
	n.Keys[slot] = KeySlot{}
	return nil
}

// KeyReveal returns the full key in a slot (for distribution).
func (c *Config) KeyReveal(netName string, slot int) (string, error) {
	if err := validSlot(slot); err != nil {
		return "", err
	}
	n, err := c.PickNetwork(netName)
	if err != nil {
		return "", err
	}
	if n.Keys[slot].Key == "" {
		return "", fmt.Errorf("slot %d is empty", slot)
	}
	return n.Keys[slot].Key, nil
}
