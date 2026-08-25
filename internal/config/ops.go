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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/crypto"

	"gravinet/internal/protocol"
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

// NetworkSetSelfSeed toggles this node's own SelfSeed declaration for a
// network — see Network.SelfSeed's doc comment. Not currently hot-reloadable
// (mirrors mesh.NetSpec.AllowRelay, set only at network construction, never
// on live reload — see cmd/gravinet's buildOneNetSpec); a restart is needed
// for a change here to be advertised to peers.
func (c *Config) NetworkSetSelfSeed(name string, on bool) error {
	n := c.FindNetwork(name)
	if n == nil {
		return fmt.Errorf("no network named %q", name)
	}
	n.SelfSeed = on
	return nil
}

// NetworkSetMesh sets a network's connectivity topology — see Network.Mesh's
// doc comment. mode must be "full" or "partial" (case-insensitive); an empty
// string is accepted too and stored as "full" so the field always reads back
// unambiguously once an operator has touched it at all. Not hot-reloadable —
// mirrors NetworkSetSelfSeed/NetworkSetAllowRelay, set only at network
// construction (mesh.NetSpec.PartialMesh, cmd/gravinet's buildOneNetSpec); a
// restart is needed before the new topology actually takes effect.
func (c *Config) NetworkSetMesh(name, mode string) error {
	n := c.FindNetwork(name)
	if n == nil {
		return fmt.Errorf("no network named %q", name)
	}
	switch {
	case mode == "" || strings.EqualFold(mode, "full"):
		n.Mesh = "full"
	case strings.EqualFold(mode, "partial"):
		n.Mesh = "partial"
	default:
		return fmt.Errorf("mesh mode %q must be \"full\" or \"partial\"", mode)
	}
	return nil
}

// NetworkSetAllowRelay toggles whether this node is willing to relay other
// peers' traffic on a network — see Network.AllowRelay's doc comment. Not
// currently hot-reloadable (mirrors mesh.NetSpec.AllowRelay, which
// cmd/gravinet only reads at network construction, not on live reload); a
// restart is needed for a change here to take effect.
func (c *Config) NetworkSetAllowRelay(name string, on bool) error {
	n := c.FindNetwork(name)
	if n == nil {
		return fmt.Errorf("no network named %q", name)
	}
	n.AllowRelay = on
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

// MaxUnfragmentedTunnelMTU returns the largest overlay MTU that still fits in
// one underlay datagram at the given path-MTU-discovery ceiling — the same
// arithmetic mesh.computeMaxInnerFrag performs, duplicated here (rather than
// imported) because config must not depend on mesh. protocol.DefaultTunnelMTU
// documents the derivation, and mesh's TestDefaultTunnelMTUFitsDefaultUnderlay
// pins the two against each other so they cannot drift.
//
//	ceiling − 48 (IPv6 40 + UDP 8) − 31 (data header 14 + type 1 + tag 16) − 6 (frag header)
func MaxUnfragmentedTunnelMTU(ceiling int) int {
	m := ceiling - 48 - 31 - 6
	if m < 1 {
		return 1
	}
	return m
}

// NetworkSetMTU sets a network's overlay interface MTU.
//
// Returns an advisory alongside the error when the value is accepted but will
// fragment every full-size packet — i.e. when it exceeds what the configured
// path-MTU ceiling could ever carry whole. That is a legitimate thing to want
// (an operator may be mid-migration, or deliberately trading fragmentation for
// a larger overlay MSS), so it is not rejected; but it is precisely the state
// that produced silent, invisible throughput loss in the field, so the caller
// is given something concrete to print rather than leaving the operator to
// discover it from a startup warning after the next restart.
//
// The advisory is returned rather than logged so the CLI and the web admin can
// each present it in their own idiom from one implementation.
func (c *Config) NetworkSetMTU(ref string, mtu int) (advice string, err error) {
	n := c.FindNetwork(ref)
	if n == nil {
		return "", fmt.Errorf("no network named %q", ref)
	}
	if mtu < protocol.MinTunnelMTU || mtu > 65535 {
		return "", fmt.Errorf("mtu %d out of range (%d-65535)", mtu, protocol.MinTunnelMTU)
	}
	n.MTU = mtu
	if fits := MaxUnfragmentedTunnelMTU(c.UnderlayMTUMaxValue()); mtu > fits {
		advice = fmt.Sprintf("mtu %d is larger than any path on this node can carry whole (%d at an underlay_mtu_max of %d), so every full-size packet will be split into %d fragments and reassembled; %d avoids that",
			mtu, fits, c.UnderlayMTUMaxValue(), (mtu+fits-1)/fits, fits)
	}
	return advice, nil
}

// NetworkSetRedistributeBGPRoutes sets a network's BGP-into-mesh
// redistribution selection and the metric every such route carries — see
// Network.RedistributeBGPRoutes' doc comment for what this actually
// gossips. Unlike NetworkSetSubnets/NetworkSetAddress, this needs no
// restart and no validation beyond finding the network: it doesn't touch
// addressing or re-key anything, only what gets fed to the mesh's gossip
// (applied live by webadmin's bgpMeshRedistributor, the same way editing
// the Advertise table already applies live via reloadRoutes). routes is
// stored as given — not validated against the live BGP RIB here, since a
// selection naming a route not currently in it isn't an error, just
// something bgpMeshRedistributor's own intersection contributes nothing
// for until/unless it reappears (same non-auto-pruning behavior as
// BGPConfig's own redistribute selections).
func (c *Config) NetworkSetRedistributeBGPRoutes(ref string, routes []string, metric int) error {
	n := c.FindNetwork(ref)
	if n == nil {
		return fmt.Errorf("no network named %q", ref)
	}
	n.RedistributeBGPRoutes = routes
	n.RedistributeBGPMetric = metric
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
// ("udp", "host"). A tcp:// seed is dialed over TCP/TLS directly
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
// built-in port set; if a port is given it must be numeric and in range.
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
//
// Re-adding an address that is already present but disabled re-enables it,
// matching HostRejectAdd/DNSRejectAdd. Without that, "add the seed" on a
// disabled seed would be a silent no-op — the operator asks for the address
// to be in service, and the address stays parked with nothing said about it.
// Its notes and position survive, since the entry is not replaced.
func (c *Config) SeedAdd(netName, addr string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	addr = strings.TrimSpace(addr)
	if err := validateSeedAddr(addr); err != nil {
		return err
	}
	for i := range n.Seeds {
		if n.Seeds[i].Address == addr {
			n.Seeds[i].Disabled = false // re-adding re-enables
			return nil
		}
	}
	n.Seeds = append(n.Seeds, Seed{Address: addr})
	return nil
}

// SeedSetNode records the node id confirmed to be behind a seed address, or
// clears it with "". Learned state, not operator input: called from the
// reconciler that reads the engine's own handshake attribution, never from a
// form field. Unknown addresses are ignored rather than erroring — the
// reconciler runs against whatever the engine currently knows, which routinely
// includes addresses no longer configured here.
func (c *Config) SeedSetNode(netName, addr, nodeID string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	addr = strings.TrimSpace(addr)
	for i := range n.Seeds {
		if n.Seeds[i].Address == addr {
			n.Seeds[i].Node = nodeID
			return nil
		}
	}
	return nil
}

// SeedSetEnabled enables or disables a single bootstrap seed by address. A
// disabled seed keeps its address, notes, and row position (so re-enabling
// costs one double-click and loses nothing) but is dropped from
// SeedList.EnabledAddrs, which means it is not resolved into the dial set,
// not counted among this node's configured seeds for upgrade sequencing, and
// not embedded in a join token.
//
// Applied live in both directions, unlike SeedRemove. Disabling is the one
// subtractive operation on the seed set: the reload carries the address in
// NetSpec.RetiredSeeds, which drops it from the live dial set and tears down
// any session standing on it (mesh.applyRetiredSeeds); enabling returns it
// and it is dialed on the next tick, about a second later.
//
// On its own this is only an address-level control, and an address-level
// control cannot keep a node away: on a full mesh the peer behind a disabled
// seed is an ordinary member and another peer gossips it back within a gossip
// round, reconnecting as a learned candidate rather than as this node's seed.
// So the callers that own an operator's intent — the web admin's seed handler
// today — mirror this onto PeerSetEnabled for the node recorded in Seed.Node,
// and mirror the reverse when a peer's state changes, in every direction. See
// webadmin/seedpeercouple.go.
//
// The pairing lives there, not here, because it needs to know which node is
// behind an address — a runtime fact this package only ever sees second-hand,
// cached in Seed.Node by a reconciler reading the engine's own attribution.
func (c *Config) SeedSetEnabled(netName, addr string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	addr = strings.TrimSpace(addr)
	for i := range n.Seeds {
		if n.Seeds[i].Address == addr {
			n.Seeds[i].Disabled = !on
			return nil
		}
	}
	return fmt.Errorf("no seed %q on network %q", addr, netName)
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

// NATSetEnabled turns this node's address translation on or off. Node-global
// since v953: NAT is a statement about packets rather than about an overlay,
// so there is one switch rather than one per mesh network.
func (c *Config) NATSetEnabled(on bool) error {
	c.NAT.Enabled = on
	return nil
}

// FirewallSetEnabled turns the packet filter on or off for a network. When off,
// all traffic is allowed; when on with no rules, the default policy is allow.
// FirewallSetEnabled turns this node's firewall on or off. Node-global since
// v957; the flag gates enforcement only, never whether rules are loaded.
func (c *Config) FirewallSetEnabled(on bool) error {
	c.Firewall.Enabled = on
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
// FirewallRuleSetEnabled toggles one rule, addressed by its stable id rather
// than by position: an index would silently move under a concurrent reorder.
func (c *Config) FirewallRuleSetEnabled(id uint64, on bool) error {
	i := c.firewallIndexOf(id)
	if i < 0 {
		return fmt.Errorf("no firewall rule with id %d", id)
	}
	c.Firewall.Rules[i].Disabled = !on
	return nil
}

// firewallIndexOf locates a rule by id, or -1.
func (c *Config) firewallIndexOf(id uint64) int {
	for i := range c.Firewall.Rules {
		if c.Firewall.Rules[i].ID == id {
			return i
		}
	}
	return -1
}

// FirewallRuleAdd inserts a rule at position idx (-1 = append).
// FirewallRuleAdd inserts a rule at position at (-1 appends) and gives it a
// fresh id. Order matters — first match wins — so position is meaningful.
func (c *Config) FirewallRuleAdd(r FirewallRule, at int) error {
	if err := c.checkFirewallScope(r.Scope); err != nil {
		return err
	}
	c.assignFirewallIDs() // keeps NextID ahead of anything already present
	r.ID = c.Firewall.NextID
	c.Firewall.NextID++
	r.Scope = strings.TrimSpace(r.Scope)
	if at < 0 || at > len(c.Firewall.Rules) {
		c.Firewall.Rules = append(c.Firewall.Rules, r)
		return nil
	}
	c.Firewall.Rules = append(c.Firewall.Rules[:at], append([]FirewallRule{r}, c.Firewall.Rules[at:]...)...)
	return nil
}

// FirewallRuleUpdate replaces a rule in place, keeping its id and position.
func (c *Config) FirewallRuleUpdate(id uint64, r FirewallRule) error {
	i := c.firewallIndexOf(id)
	if i < 0 {
		return fmt.Errorf("no firewall rule with id %d", id)
	}
	if err := c.checkFirewallScope(r.Scope); err != nil {
		return err
	}
	r.ID = id
	r.Scope = strings.TrimSpace(r.Scope)
	c.Firewall.Rules[i] = r
	return nil
}

// FirewallRuleDelete removes rules by their 0-based position indices.
// Indices are processed high-to-low so earlier removals don't shift later ones.
// FirewallRuleDelete removes rules by id. Ids are never reused, so a delete
// cannot hand a live rule's identity to a later one.
func (c *Config) FirewallRuleDelete(ids []uint64) error {
	if len(ids) == 0 {
		return fmt.Errorf("no firewall rule ids given")
	}
	want := map[uint64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := c.Firewall.Rules[:0]
	n := 0
	for _, r := range c.Firewall.Rules {
		if want[r.ID] {
			n++
			continue
		}
		out = append(out, r)
	}
	if n == 0 {
		return fmt.Errorf("no firewall rule with those ids")
	}
	c.Firewall.Rules = out
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

// FirewallRuleMove moves a rule to a new position in the node's ordered list.
// First match wins, so this is a change in meaning, not presentation.
func (c *Config) FirewallRuleMove(id uint64, to int) error {
	from := c.firewallIndexOf(id)
	if from < 0 {
		return fmt.Errorf("no firewall rule with id %d", id)
	}
	if to < 0 || to >= len(c.Firewall.Rules) {
		return fmt.Errorf("position %d out of range (0..%d)", to, len(c.Firewall.Rules)-1)
	}
	r := c.Firewall.Rules[from]
	rest := append(c.Firewall.Rules[:from:from], c.Firewall.Rules[from+1:]...)
	c.Firewall.Rules = append(rest[:to:to], append([]FirewallRule{r}, rest[to:]...)...)
	return nil
}

// NATAdd adds a masquerade (overlay→underlay) rule out the given interface.
func (c *Config) NATAdd(iface, scope string) error {
	if iface == "" {
		return fmt.Errorf("NAT rule needs an interface")
	}
	for _, r := range c.NAT.Rules {
		if r.Interface == iface && strings.EqualFold(r.Scope, scope) {
			return fmt.Errorf("NAT rule for %s already exists", iface)
		}
	}
	if err := c.checkNATScope(scope); err != nil {
		return err
	}
	c.NAT.Enabled = true
	c.NAT.Rules = append(c.NAT.Rules, NATRule{
		Translate: "masquerade",
		Interface: iface,
		Scope:     scope,
		Enabled:   true,
	})
	return nil
}

// checkNATScope refuses a scope that names no mesh network. Empty is always
// valid — that is the ordinary router rule, enforced in the kernel only, and
// is what a node with no mesh networks writes.
func (c *Config) checkNATScope(scope string) error {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return nil
	}
	for i := range c.Networks {
		if strings.EqualFold(c.Networks[i].Name, scope) || strings.EqualFold(c.Networks[i].ID, scope) {
			return nil
		}
	}
	return fmt.Errorf("no mesh network named %q — leave the scope blank for a rule about traffic crossing this host's own interfaces", scope)
}

func (c *Config) NATDelete(iface string) error {
	out := c.NAT.Rules[:0]
	found := false
	for _, r := range c.NAT.Rules {
		if r.Interface == iface {
			found = true
			continue
		}
		out = append(out, r)
	}
	c.NAT.Rules = out
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
		if ip.Is4In6() {
			return "", fmt.Errorf("%s %q: write an IPv4-mapped address in plain IPv4 form (%s)", field, s, ip.Unmap())
		}
		return s, nil
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		if p.Addr().Is4In6() {
			return "", fmt.Errorf("%s %q: write an IPv4-mapped prefix in plain IPv4 form", field, s)
		}
		return s, nil
	}
	return "", fmt.Errorf("%s %q: not an IP address or CIDR", field, s)
}

// natFieldFamily reports the address family of an already-normalized NAT
// field, and whether the field names an address at all. A blank field (the
// "any" match) has no family of its own and contributes nothing to the
// agreement check below.
//
// IPv4-mapped forms never reach here — validNATCIDR and buildNATRule reject
// them at the door, because netip.Addr.Is6 answers true for ::ffff:a.b.c.d
// while the address is semantically IPv4. Left alone it would be sorted into
// the ip6 table and emitted as an "ip6 saddr ::ffff:..." match that can never
// fire, which is a silently dead rule rather than an error.
func natFieldFamily(s string) (is6, set bool) {
	if s == "" {
		return false, false
	}
	if ip, err := netip.ParseAddr(s); err == nil {
		return !ip.Is4(), true
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return !p.Addr().Is4(), true
	}
	return false, false
}

// natFamilyAgreement checks that every address-bearing field of one rule names
// the same family. fields is (label, value) pairs in the order they should be
// reported.
//
// This has to be enforced here because nothing downstream can catch it. A
// Rule carries a single V6 flag, and cmd/gravinet's kernelNATRules derives it
// from one field only — the source for masquerade, the translate target for
// SNAT and DNAT. Every other field is then rendered with that family's
// keyword. So a v4 source with a v6 translate target does not fail; it
// produces "ip6 saddr 192.168.203.0/24", which nft rejects outright, or worse
// under the iptables backend where the v4 match is simply passed to ip6tables.
// Neither is a diagnosable failure by the time it gets there.
func natFamilyAgreement(fields ...[2]string) (is6 bool, err error) {
	var have bool
	var firstLabel, firstValue string
	for _, f := range fields {
		label, value := f[0], f[1]
		v6, set := natFieldFamily(value)
		if !set {
			continue
		}
		if !have {
			is6, have, firstLabel, firstValue = v6, true, label, value
			continue
		}
		if v6 != is6 {
			return false, fmt.Errorf("%s %q is IPv%s but %s %q is IPv%s — one NAT rule cannot mix address families; write one rule per family",
				firstLabel, firstValue, natFamilyDigit(is6), label, value, natFamilyDigit(v6))
		}
	}
	return is6, nil
}

func natFamilyDigit(is6 bool) string {
	if is6 {
		return "6"
	}
	return "4"
}

// validNATPortSpec parses a DestPort value: "" (any), "N" (a single port,
// 1-65535), or "N-M" (an inclusive range, N <= M). Returns the bounds (0,0
// for "any"); a single port has lo == hi.
func validNATPortSpec(s string) (lo, hi uint16, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	badRange := fmt.Errorf("dest-port %q: must be 1-65535 or a range like 8000-8010", s)
	a, b, isRange := strings.Cut(s, "-")
	if !isRange {
		p, perr := strconv.Atoi(a)
		if perr != nil || p < 1 || p > 65535 {
			return 0, 0, badRange
		}
		return uint16(p), uint16(p), nil
	}
	lo64, lerr := strconv.Atoi(a)
	hi64, herr := strconv.Atoi(b)
	if lerr != nil || herr != nil || lo64 < 1 || lo64 > 65535 || hi64 < 1 || hi64 > 65535 || lo64 > hi64 {
		return 0, 0, badRange
	}
	return uint16(lo64), uint16(hi64), nil
}

// natPortForwardPrefix marks a translate value as DNAT — see NATRule's doc
// comment. Kept as its own constant (rather than importing mesh's copy) since
// config intentionally doesn't depend on mesh; the two packages just happen
// to agree on the same short keyword, the same way they already agree on
// "masquerade".
const natPortForwardPrefix = "port-forward:"

// cutNATPortForwardPrefix is strings.CutPrefix's case-insensitive
// counterpart, scoped to natPortForwardPrefix — matched case-insensitively
// so a hand-edited config using "Port-Forward:" still parses the same as
// the lowercase form the admin UI and CLI always write.
func cutNATPortForwardPrefix(s string) (target string, ok bool) {
	if len(s) < len(natPortForwardPrefix) || !strings.EqualFold(s[:len(natPortForwardPrefix)], natPortForwardPrefix) {
		return s, false
	}
	return s[len(natPortForwardPrefix):], true
}

// source/dest are empty ("any") or IPv4 addresses/CIDRs. translate is either
// "masquerade" (requires iface, whose primary IPv4 is used), a literal IPv4
// target (static SNAT), or "port-forward:<ipv4>[:<port>]" (DNAT to that
// address, optionally remapping to <port> — see NATRule's doc comment for
// why the mode lives in translate rather than a separate field).
// destPort/proto scope a port-forward rule to a specific port or range —
// see NATRule.DestPort's doc comment; both must be blank for a
// masquerade/static-SNAT rule, since neither means anything there.
// buildNATRule validates and normalizes the user-supplied fields of a NAT
// rule into a NATRule (with Enabled left false for the caller to set). It
// is shared by NATRuleAdd and NATRuleUpdateAt so adding and editing enforce
// identical rules.
func buildNATRule(source, dest, destPort, proto, translate, iface string) (NATRule, error) {
	src, err := validNATCIDR("source", source)
	if err != nil {
		return NATRule{}, err
	}
	dst, err := validNATCIDR("dest", dest)
	if err != nil {
		return NATRule{}, err
	}
	destPort = strings.TrimSpace(destPort)
	dpLo, dpHi, err := validNATPortSpec(destPort)
	if err != nil {
		return NATRule{}, err
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto != "" && proto != "tcp" && proto != "udp" {
		return NATRule{}, fmt.Errorf("proto %q: must be \"tcp\" or \"udp\"", proto)
	}
	if destPort != "" && proto == "" {
		return NATRule{}, fmt.Errorf("dest-port %q needs proto tcp or udp — a port only means something for one of those", destPort)
	}
	translate = strings.TrimSpace(translate)
	iface = strings.TrimSpace(iface)
	if rest, ok := cutNATPortForwardPrefix(translate); ok {
		addrPart, portPart, hasPort, perr := SplitNATTarget(rest)
		if perr != nil {
			return NATRule{}, perr
		}
		ip, aerr := netip.ParseAddr(addrPart)
		if aerr != nil {
			return NATRule{}, fmt.Errorf("port-forward target %q: must be an IP address", addrPart)
		}
		if ip.Is4In6() {
			return NATRule{}, fmt.Errorf("port-forward target %q: write an IPv4-mapped address in plain IPv4 form (%s)", addrPart, ip.Unmap())
		}
		out := natPortForwardPrefix + natTargetText(ip, 0)
		if hasPort {
			toPort, nerr := strconv.Atoi(strings.TrimSpace(portPart))
			if nerr != nil || toPort < 1 || toPort > 65535 {
				return NATRule{}, fmt.Errorf("port-forward remap port %q: must be 1-65535", portPart)
			}
			if dpLo == 0 || dpLo != dpHi {
				return NATRule{}, fmt.Errorf("port-forward remap (%s:%d) needs a single dest-port, not a range or \"any\" — a range/any can't remap to one fixed port", addrPart, toPort)
			}
			out = natPortForwardPrefix + natTargetText(ip, uint16(toPort))
		}
		if _, ferr := natFamilyAgreement([2]string{"source", src}, [2]string{"dest", dst}, [2]string{"port-forward target", addrPart}); ferr != nil {
			return NATRule{}, ferr
		}
		// Port-forwarding is a fixed rewrite target, not a per-interface
		// masquerade, so it carries no interface — same as any other literal
		// translate address.
		return NATRule{Source: src, Dest: dst, DestPort: destPort, Proto: proto, Translate: out}, nil
	}
	if destPort != "" {
		return NATRule{}, fmt.Errorf("dest-port only applies to a port-forward (DNAT) rule, not masquerade/static-SNAT")
	}
	masq := translate == "" || strings.EqualFold(translate, "masquerade")
	if masq {
		if iface == "" {
			return NATRule{}, fmt.Errorf("masquerade needs an interface (translate=masquerade requires iface)")
		}
		translate = "masquerade"
		// Masquerade has no target address, so the source prefix is the only
		// thing that can name a family — see kernelNATRules, which sets V6
		// from it alone. A blank source therefore cannot mean "both": it
		// resolves to IPv4, exactly as it always has. Say so rather than
		// programming half of what the operator asked for in silence.
		if _, set := natFieldFamily(src); !set {
			return NATRule{}, fmt.Errorf("masquerade with source \"any\" covers IPv4 only — a rule takes its family from its source, and there is nothing else here to take it from; give an explicit source prefix (one rule per family) if you want IPv6 masqueraded too")
		}
	} else {
		ip, perr := netip.ParseAddr(translate)
		if perr != nil {
			return NATRule{}, fmt.Errorf("translate %q: must be an IP address, \"masquerade\", or \"port-forward:<addr>[:<port>]\" (bracket an IPv6 address when a port follows)", translate)
		}
		if ip.Is4In6() {
			return NATRule{}, fmt.Errorf("translate %q: write an IPv4-mapped address in plain IPv4 form (%s)", translate, ip.Unmap())
		}
		iface = ""
	}
	if _, err := natFamilyAgreement([2]string{"source", src}, [2]string{"dest", dst}, [2]string{"translate", translate}); err != nil {
		return NATRule{}, err
	}
	return NATRule{Source: src, Dest: dst, Translate: translate, Interface: iface}, nil
}

// SplitNATTarget separates a port-forward target into its address and optional
// remap port. Exported because cmd/gravinet's kernelNATRules has to reach the
// identical decomposition when it turns stored config back into kernel rules,
// and this is too easy to get subtly wrong twice: the naive strings.Cut on the
// first colon that both sides used while NAT was IPv4-only silently truncates
// "fd00:203::5" to "fd00" and hands "203::5" over as a port.
//
// An IPv6 address must be bracketed when a remap port follows it, since its
// own colons are otherwise indistinguishable from the ":<port>" separator.
// Without a port the brackets are optional: an unbracketed string that parses
// whole as an address is unambiguous, so "port-forward:fd00:203::5" is
// accepted and means what it looks like.
func SplitNATTarget(s string) (addr, port string, hasPort bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false, fmt.Errorf("port-forward target is empty")
	}
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", false, fmt.Errorf("port-forward target %q: unclosed \"[\"", s)
		}
		addr = strings.TrimSpace(s[1:end])
		switch remainder := strings.TrimSpace(s[end+1:]); {
		case remainder == "":
			return addr, "", false, nil
		case strings.HasPrefix(remainder, ":"):
			return addr, strings.TrimSpace(remainder[1:]), true, nil
		default:
			return "", "", false, fmt.Errorf("port-forward target %q: expected \":<port>\" after \"]\"", s)
		}
	}
	// Unbracketed: a string that parses whole as an address carries no port,
	// which is what lets an IPv6 target skip the brackets when it doesn't
	// need one.
	if _, e := netip.ParseAddr(s); e == nil {
		return s, "", false, nil
	}
	a, p, ok := strings.Cut(s, ":")
	if !ok {
		return s, "", false, nil
	}
	return strings.TrimSpace(a), strings.TrimSpace(p), true, nil
}

// natTargetText renders a port-forward target for storage, bracketing an IPv6
// address whenever a port follows so SplitNATTarget can read it back.
func natTargetText(ip netip.Addr, port uint16) string {
	if port == 0 {
		return ip.String()
	}
	if ip.Is6() {
		return fmt.Sprintf("[%s]:%d", ip, port)
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

// applyNATNegate sets a rule's negation flags after buildNATRule has validated
// the addresses, refusing the one combination that is always a mistake:
// negation on a blank (any) field. "Everything except any" matches nothing, so
// such a rule would sit in the table looking active while never firing once.
// The firewall editor rejects the same pairing rather than saving it.
func applyNATNegate(r *NATRule, srcNeg, dstNeg bool) error {
	if srcNeg && r.Source == "" {
		return fmt.Errorf("source is negated but empty (any): that matches nothing — name a prefix, or turn the negation off")
	}
	if dstNeg && r.Dest == "" {
		return fmt.Errorf("dest is negated but empty (any): that matches nothing — name a prefix, or turn the negation off")
	}
	r.SourceNegate, r.DestNegate = srcNeg, dstNeg
	return nil
}

func (c *Config) NATRuleAdd(source, dest, destPort, proto, translate, iface, scope string) error {
	return c.NATRuleAddNeg(source, dest, destPort, proto, translate, iface, scope, false, false)
}

// NATRuleAddNeg is NATRuleAdd with the source/dest negation flags. Kept as a
// separate entry point so the six-argument form stays valid for every existing
// caller and test.
func (c *Config) NATRuleAddNeg(source, dest, destPort, proto, translate, iface, scope string, srcNeg, dstNeg bool) error {
	rule, err := buildNATRule(source, dest, destPort, proto, translate, iface)
	if err != nil {
		return err
	}
	if err := applyNATNegate(&rule, srcNeg, dstNeg); err != nil {
		return err
	}
	if err := c.checkNATScope(scope); err != nil {
		return err
	}
	rule.Scope = strings.TrimSpace(scope)
	rule.Enabled = true
	c.NAT.Enabled = true
	c.NAT.Rules = append(c.NAT.Rules, rule)
	return nil
}

// NATRuleUpdateAt replaces the rule at index idx (as shown by NAT list / the UI)
// in place, preserving its enabled/disabled state and its position. It backs the
// click-to-edit rule fields in the UI. Validation matches NATRuleAdd.
func (c *Config) NATRuleUpdateAt(idx int, source, dest, destPort, proto, translate, iface, scope string) error {
	return c.NATRuleUpdateAtNeg(idx, source, dest, destPort, proto, translate, iface, scope, false, false)
}

// NATRuleUpdateAtNeg is NATRuleUpdateAt with the negation flags.
func (c *Config) NATRuleUpdateAtNeg(idx int, source, dest, destPort, proto, translate, iface, scope string, srcNeg, dstNeg bool) error {
	if idx < 0 || idx >= len(c.NAT.Rules) {
		return fmt.Errorf("no NAT rule at index %d (have %d)", idx, len(c.NAT.Rules))
	}
	rule, err := buildNATRule(source, dest, destPort, proto, translate, iface)
	if err != nil {
		return err
	}
	if err := applyNATNegate(&rule, srcNeg, dstNeg); err != nil {
		return err
	}
	if err := c.checkNATScope(scope); err != nil {
		return err
	}
	rule.Scope = strings.TrimSpace(scope)
	rule.Enabled = c.NAT.Rules[idx].Enabled // preserve current state
	c.NAT.Rules[idx] = rule
	return nil
}

// NATRuleDeleteAt removes the rule at index idx (as shown by NAT list / the UI).
func (c *Config) NATRuleDeleteAt(idx int) error {
	if idx < 0 || idx >= len(c.NAT.Rules) {
		return fmt.Errorf("no NAT rule at index %d (have %d)", idx, len(c.NAT.Rules))
	}
	c.NAT.Rules = append(c.NAT.Rules[:idx], c.NAT.Rules[idx+1:]...)
	return nil
}

// NATRuleSetEnabled enables or disables the NAT rule at index idx (as shown by
// NAT list / the UI). A disabled rule stays in config (match intact for
// re-enabling) but is skipped when translating. This mirrors the per-rule
// enable/disable used for firewall rules.
func (c *Config) NATRuleSetEnabled(idx int, on bool) error {
	if idx < 0 || idx >= len(c.NAT.Rules) {
		return fmt.Errorf("no NAT rule at index %d (have %d)", idx, len(c.NAT.Rules))
	}
	c.NAT.Rules[idx].Enabled = on
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

// WebAdminLoginBanSet sets the web admin login lockout policy: how many
// failed attempts from one source trigger a lockout, and how long that
// lockout lasts. 0 for either restores its default (3 attempts, 900s/15min —
// see BanPolicy.EffectiveMaxFailures/EffectiveBanSeconds).
//
// Unlike NATStateTimeoutSet just above, this needs a restart to take effect:
// the throttle that actually tracks failures is built once, from these
// values, when Server.New runs — the same "captured at startup" shape as
// AuthMode/Users/GeoIPLookup (see config.WebAdmin's doc comments on those).
func (c *Config) WebAdminLoginBanSet(maxFailures, banSeconds int) error {
	if maxFailures < 0 || maxFailures > 100 {
		return fmt.Errorf("lockout attempts must be 0..100 (0 restores the default of 3)")
	}
	if banSeconds < 0 || banSeconds > 86400 {
		return fmt.Errorf("lockout duration must be 0..86400 seconds (0 restores the default of 900)")
	}
	c.WebAdmin.LoginBan.MaxFailures = maxFailures
	c.WebAdmin.LoginBan.BanSeconds = banSeconds
	return nil
}

// WebAdminTLSPaths returns where an uploaded (not self-signed) cert/key pair
// is written, next to the config — same directory as selfSignedPaths' own
// webadmin-cert.pem/webadmin-key.pem, but named distinctly so the two never
// collide and switching back to self-signed (WebAdminTLSCertReset) never
// risks deleting or overwriting the uploaded pair still sitting on disk.
func (c *Config) WebAdminTLSPaths() (certPath, keyPath string) {
	return filepath.Join(c.dir(), "webadmin-cert-custom.pem"), filepath.Join(c.dir(), "webadmin-key-custom.pem")
}

// WebAdminTLSCertSet points the config at an uploaded cert/key pair already
// written to disk (see WebAdminTLSPaths) — validating that they actually
// parse and pair together is the caller's job (internal/webadmin's handler
// does it with crypto/tls, before anything is written to disk at all), this
// just records the paths. Needs a restart: like the self-signed path it
// replaces, the certificate is loaded once when the HTTPS listener starts.
func (c *Config) WebAdminTLSCertSet(certPath, keyPath string) error {
	if certPath == "" || keyPath == "" {
		return fmt.Errorf("both a certificate path and a key path are required")
	}
	c.WebAdmin.TLSCert = certPath
	c.WebAdmin.TLSKey = keyPath
	return nil
}

// WebAdminTLSCertReset clears any uploaded cert/key paths, reverting to the
// auto-generated self-signed certificate (selfSignedCert) on next restart.
// Does not delete the uploaded files themselves — reverting is not the same
// as discarding, and someone who reset by mistake can point tls_cert/tls_key
// back at them without re-uploading.
func (c *Config) WebAdminTLSCertReset() error {
	c.WebAdmin.TLSCert = ""
	c.WebAdmin.TLSKey = ""
	return nil
}

// ConfigHistoryLimitSet sets how many config history snapshots (see
// history.go) are kept, FIFO. 0 restores the default (250). Applied live —
// unlike TLS cert/login lockout, nothing captures this at startup; every
// snapshot call reads it fresh off the just-loaded config (see
// EffectiveConfigHistoryLimit's call sites), so a change here takes effect
// on the very next commit.
func (c *Config) ConfigHistoryLimitSet(limit int) error {
	if limit < 0 || limit > 10000 {
		return fmt.Errorf("config history limit must be 0..10000 (0 restores the default of 250)")
	}
	c.ConfigHistoryLimit = limit
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

// QoSSetEnabled turns this node's classifier on or off. Node-global since
// v954 — one switch, not one per mesh network.
func (c *Config) QoSSetEnabled(on bool) error {
	c.QoS.Enabled = on
	return nil
}

// QoSAdd adds a classification rule mapping proto/port and/or named services
// (resolved against Config.FirewallServices at reload time, unioned exactly
// like FirewallRule.Services — see QoSRule's doc comment) to a class index.
// services may be nil/empty for a plain proto/port rule, matching the
// pre-Services behavior exactly.
func (c *Config) QoSAdd(proto string, port int, services []string, class int, scope string) error {
	if c.QoS.Classes < 5 {
		c.QoS.Classes = 5
	}
	if c.QoS.DefaultClass <= 0 {
		c.QoS.DefaultClass = 3
	}
	if class < 0 || class >= c.QoS.Classes {
		return fmt.Errorf("class %d out of range (0..%d)", class, c.QoS.Classes-1)
	}
	if err := c.checkQoSScope(scope); err != nil {
		return err
	}
	proto = strings.ToLower(proto)
	if proto != "tcp" && proto != "udp" && proto != "icmp" && proto != "any" && proto != "" {
		return fmt.Errorf("protocol must be tcp, udp, icmp, or any")
	}
	if port < 0 || port > 65535 {
		return fmt.Errorf("port %d out of range", port)
	}
	c.QoS.Enabled = true
	c.QoS.Rules = append(c.QoS.Rules, QoSRule{
		Protocol: proto, PortMin: port, PortMax: port, Services: cloneQoSServices(services), Class: class,
		Scope: strings.TrimSpace(scope),
	})
	return nil
}

// checkQoSScope refuses a scope naming no mesh network. Empty is always valid
// and means every network — see QoSRule.Scope.
func (c *Config) checkQoSScope(scope string) error {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return nil
	}
	for i := range c.Networks {
		if strings.EqualFold(c.Networks[i].Name, scope) || strings.EqualFold(c.Networks[i].ID, scope) {
			return nil
		}
	}
	return fmt.Errorf("no mesh network named %q — leave the scope blank to classify on every network", scope)
}

// QoSDelete removes every QoS rule matching proto/port/services exactly (the
// same key QoSAdd wrote it under — see qosRuleKeyMatches).
func (c *Config) QoSDelete(proto string, port int, services []string, scope string) error {
	proto = strings.ToLower(proto)
	out := c.QoS.Rules[:0]
	found := false
	for _, r := range c.QoS.Rules {
		if qosRuleKeyMatches(r, proto, port, services, scope) {
			found = true
			continue
		}
		out = append(out, r)
	}
	c.QoS.Rules = out
	if !found {
		return fmt.Errorf("no QoS rule for %s", qosRuleKeyLabel(proto, port, services))
	}
	return nil
}

// QoSRuleSetEnabled enables or disables the classification rule(s) matching
// proto/port/services. A disabled rule stays in config (match intact for
// re-enabling) but is skipped by the classifier. It is keyed the same way as
// QoSDelete, so it toggles every rule sharing that key. This mirrors the
// per-rule enable/disable used for firewall rules.
func (c *Config) QoSRuleSetEnabled(proto string, port int, services []string, scope string, on bool) error {
	proto = strings.ToLower(proto)
	found := false
	for i := range c.QoS.Rules {
		if qosRuleKeyMatches(c.QoS.Rules[i], proto, port, services, scope) {
			c.QoS.Rules[i].Disabled = !on
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no QoS rule for %s", qosRuleKeyLabel(proto, port, services))
	}
	return nil
}

// qosRuleKeyMatches reports whether r was authored with the given
// proto/port/services key — the same fields QoSAdd stores a rule under.
// services is compared case-insensitively and order-independently, so a
// round trip through the UI (which may reorder a comma-separated list)
// still finds the rule it means to.
// Scope is part of the key from v954. Without it, two rules that differ only
// by the network they classify on — the ordinary result of hoisting a config
// that had the same rule on two networks — would be indistinguishable, and
// deleting or toggling one would silently take the other with it.
func qosRuleKeyMatches(r QoSRule, proto string, port int, services []string, scope string) bool {
	return r.Protocol == proto && r.PortMin == port && sameServiceSet(r.Services, services) &&
		strings.EqualFold(strings.TrimSpace(r.Scope), strings.TrimSpace(scope))
}

// sameServiceSet reports whether a and b name the same set of services,
// ignoring case and order.
func sameServiceSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	norm := func(in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = strings.ToLower(strings.TrimSpace(s))
		}
		sort.Strings(out)
		return out
	}
	na, nb := norm(a), norm(b)
	for i := range na {
		if na[i] != nb[i] {
			return false
		}
	}
	return true
}

// cloneQoSServices copies a services slice, normalizing nil/empty to nil so
// a rule with no services round-trips through JSON without an empty array.
func cloneQoSServices(services []string) []string {
	if len(services) == 0 {
		return nil
	}
	return append([]string(nil), services...)
}

// qosRuleKeyLabel renders a proto/port/services key for error messages.
func qosRuleKeyLabel(proto string, port int, services []string) string {
	if len(services) == 0 {
		return fmt.Sprintf("%s port %d", proto, port)
	}
	if proto == "" && port == 0 {
		return fmt.Sprintf("services %s", strings.Join(services, ","))
	}
	return fmt.Sprintf("%s port %d + services %s", proto, port, strings.Join(services, ","))
}

// QoSSetClassDSCP overrides class's outbound DSCP mark. Every class already
// marks its traffic with a standard-codepoint default (see
// mesh.DefaultClassDSCP); this is only needed to match a specific
// organization's existing Diffserv policy instead of that default.
func (c *Config) QoSSetClassDSCP(class, dscp int) error {
	if c.QoS.Classes < 5 {
		c.QoS.Classes = 5
	}
	if class < 0 || class >= c.QoS.Classes {
		return fmt.Errorf("class %d out of range (0..%d)", class, c.QoS.Classes-1)
	}
	if dscp < 0 || dscp > 63 {
		return fmt.Errorf("dscp %d out of range (0..63)", dscp)
	}
	for len(c.QoS.ClassDSCP) <= class {
		c.QoS.ClassDSCP = append(c.QoS.ClassDSCP, -1)
	}
	c.QoS.ClassDSCP[class] = dscp
	return nil
}

// QoSClearClassDSCP removes a class's DSCP override, reverting it to the
// standard-codepoint default.
func (c *Config) QoSClearClassDSCP(class int) error {
	if class < 0 || class >= len(c.QoS.ClassDSCP) || c.QoS.ClassDSCP[class] < 0 {
		return fmt.Errorf("no DSCP override for class %d", class)
	}
	c.QoS.ClassDSCP[class] = -1
	return nil
}

// ---- shaping -----------------------------------------------------------------

// ShapingAdd creates an entry for an interface, unshaped and switched off:
// adding a row and setting a rate on it are separate acts, and a new entry
// that immediately started capping traffic would be a rate nobody chose.
//
// The interface need not exist. See IfaceShaping's doc comment for why that
// is allowed and how both front ends report it.
func (c *Config) ShapingAdd(iface string) error {
	iface = strings.TrimSpace(iface)
	if iface == "" {
		return fmt.Errorf("name the interface to shape")
	}
	if strings.ContainsAny(iface, " \t/") {
		return fmt.Errorf("%q is not an interface name", iface)
	}
	if c.ShapingFor(iface) != nil {
		return fmt.Errorf("%s already has a shaping entry", iface)
	}
	c.Shaping = append(c.Shaping, IfaceShaping{Iface: iface})
	return nil
}

// ShapingDelete removes an interface's entry, leaving it unshaped.
func (c *Config) ShapingDelete(iface string) error {
	iface = strings.TrimSpace(iface)
	for i := range c.Shaping {
		if c.Shaping[i].Iface == iface {
			c.Shaping = append(c.Shaping[:i], c.Shaping[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%s has no shaping entry", iface)
}

// ShapingSet sets the up/down/both rate (bytes/s) on an interface's entry. It
// changes only the rate, never the on/off state — turning the limiter on or
// off is the job of ShapingSetEnabled (the web toggle / CLI enable|disable).
// Keeping these independent means editing a rate can't flip the enabled state
// out from under the operator: state stays consistent through editing.
//
// The entry has to exist. Creating one here would turn a mistyped interface
// name into a silent new row rather than an error, and that row would be the
// one place the mistake did not show.
func (c *Config) ShapingSet(iface, dir string, bps int) error {
	s := c.ShapingFor(iface)
	if s == nil {
		return fmt.Errorf("%s has no shaping entry — add one first", strings.TrimSpace(iface))
	}
	if bps < 0 {
		return fmt.Errorf("a rate cannot be negative")
	}
	switch dir {
	case "up":
		s.UpBytesPerSec = bps
	case "down":
		s.DownBytesPerSec = bps
	case "both":
		s.UpBytesPerSec = bps
		s.DownBytesPerSec = bps
	default:
		return fmt.Errorf("direction must be up, down, or both")
	}
	return nil
}

// ShapingSetEnabled turns an interface's limit on or off without changing its
// configured rates, so a cap can be lifted temporarily and later restored.
func (c *Config) ShapingSetEnabled(iface string, on bool) error {
	s := c.ShapingFor(iface)
	if s == nil {
		return fmt.Errorf("%s has no shaping entry — add one first", strings.TrimSpace(iface))
	}
	s.Enabled = on
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

// RoutePrefer sets (or replaces) the ordered origin preference for a prefix.
// Origins are node IDs, most preferred first. Passing an empty list removes the
// entry, so selection reverts to plain lowest-metric.
//
// Replaces rather than merges: the list is positional, and appending to an
// existing one would silently change what everything after the insertion point
// ranks against. An operator setting a preference is stating the whole order.
func (c *Config) RoutePrefer(netName, cidr string, origins []string) error {
	n, err := c.routeTarget(netName, cidr)
	if err != nil {
		return err
	}
	clean := make([]string, 0, len(origins))
	seen := make(map[string]bool, len(origins))
	for _, o := range origins {
		o = strings.TrimSpace(o)
		if o == "" {
			return fmt.Errorf("route_prefer %s: empty origin node id", cidr)
		}
		if seen[o] {
			return fmt.Errorf("route_prefer %s: origin %q listed twice", cidr, o)
		}
		seen[o] = true
		clean = append(clean, o)
	}
	if len(clean) == 0 {
		n.RoutePrefer = removePrefer(n.RoutePrefer, cidr)
		return nil
	}
	for i := range n.RoutePrefer {
		if n.RoutePrefer[i].CIDR == cidr {
			n.RoutePrefer[i].Origins = clean
			return nil
		}
	}
	n.RoutePrefer = append(n.RoutePrefer, PreferRoute{CIDR: cidr, Origins: clean})
	return nil
}

// RoutePreferRemove drops the preference for a prefix entirely.
func (c *Config) RoutePreferRemove(netName, cidr string) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	n.RoutePrefer = removePrefer(n.RoutePrefer, cidr)
	return nil
}

// RoutePreferSetEnabled enables or disables a preference entry by CIDR. A
// disabled entry stays in config with its order intact but stops applying, so
// selection falls back to lowest-metric — the same convention as reject
// entries and firewall rules.
func (c *Config) RoutePreferSetEnabled(netName, cidr string, on bool) error {
	n, err := c.PickNetwork(netName)
	if err != nil {
		return err
	}
	for i := range n.RoutePrefer {
		if n.RoutePrefer[i].CIDR == cidr {
			n.RoutePrefer[i].Disabled = !on
			return nil
		}
	}
	return fmt.Errorf("no route_prefer entry for %s", cidr)
}

func removePrefer(s []PreferRoute, cidr string) []PreferRoute {
	out := s[:0]
	for _, x := range s {
		if x.CIDR != cidr {
			out = append(out, x)
		}
	}
	return out
}
