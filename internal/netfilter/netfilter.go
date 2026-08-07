// Package netfilter programs a dedicated, gravinet-owned kernel/OS-level NAT
// ruleset so the host masquerades/translates gateway traffic as it's forwarded
// out a physical interface. This is the part the userspace overlay NAT cannot
// do: SNAT/masquerade to the gateway's own interface only works if the OS's
// own connection tracking reverse-translates the replies (which arrive
// addressed to that interface and are never handed to the TUN). So
// overlay->underlay (and underlay->overlay DNAT) must live here;
// overlay<->overlay translation stays in the userspace path (internal/mesh).
//
// There's no cross-platform API for this, so each platform drives whatever
// it actually has: nft or iptables on Linux, pf on macOS/FreeBSD/OpenBSD, and
// WinNAT on Windows (see the platform-specific files, and
// docs/ARCHITECTURE.md, for what each one can and can't express). Every
// backend owns a dedicated, gravinet-only slice of its platform's NAT
// state — an nft table, an iptables chain pair, a pf anchor, or a set of
// named NetNat objects — so we never touch the operator's own rules, and
// Clear removes exactly what Apply added.
//
// The rule *generators* here (nftScript, iptablesRuleArgs, pfScript,
// winNATScript) are pure and platform-neutral so they can be unit tested
// without root or any of these tools actually installed; only the
// application/teardown (which shells out to nft, iptables, pfctl, or
// powershell) lives in the platform files.
package netfilter

import (
	"fmt"
	"net/netip"
	"strings"
)

// Kind is the translation a Rule performs.
type Kind uint8

const (
	Masquerade Kind = iota // SNAT to the out-interface's address (kernel picks it)
	SNAT                   // SNAT to a fixed address (To)
	DNAT                   // rewrite destination to To
)

// Rule is one kernel NAT rule. Source/Dest are optional matches (invalid = any).
type Rule struct {
	Kind     Kind
	Source   netip.Prefix // ip saddr match (optional)
	Dest     netip.Prefix // ip daddr match (optional)
	OutIface string       // oifname, for Masquerade/SNAT
	InIface  string       // iifname, for DNAT
	To       netip.Addr   // target for SNAT/DNAT (unused by Masquerade)
	V6       bool         // address family: false = IPv4 (nft "ip"/iptables), true = IPv6 (nft "ip6"/ip6tables)

	// Proto/DPortLo/DPortHi scope a DNAT rule to a specific destination
	// port or range on the original (pre-translation) packet — port
	// address translation (PAT) — instead of matching every port on Dest.
	// DPortLo == 0 means every port (the original address-only DNAT
	// behavior). Proto must be "tcp" or "udp" whenever DPortLo is set (a
	// port only means something for those two). Unused by Masquerade/SNAT.
	Proto   string
	DPortLo uint16
	DPortHi uint16
	// ToPort, if nonzero, additionally remaps the destination port to this
	// value instead of preserving the original one. Only ever set
	// alongside DPortLo == DPortHi (a single matched port) — see
	// config.NATRule.DestPort's doc comment for why a range can't remap
	// this way.
	ToPort uint16
}

// family returns the nft family keyword for the rule ("ip" or "ip6").
func (r Rule) family() string {
	if r.V6 {
		return "ip6"
	}
	return "ip"
}

// anyFamily reports whether any rule uses the given nft family ("ip"/"ip6").
func anyFamily(rules []Rule, fam string) bool {
	for _, r := range rules {
		if r.family() == fam {
			return true
		}
	}
	return false
}

// splitFamily partitions rules into IPv4 and IPv6 sets.
func splitFamily(rules []Rule) (v4, v6 []Rule) {
	for _, r := range rules {
		if r.V6 {
			v6 = append(v6, r)
		} else {
			v4 = append(v4, r)
		}
	}
	return v4, v6
}

const (
	tableName    = "gravinet_nat"
	iptPostChain = "GRAVINET_NAT_POST"
	iptPreChain  = "GRAVINET_NAT_PRE"
)

// nftScript renders the full ruleset as an `nft -f -` script. It is a single
// transaction. Rules are grouped by address family into the gravinet-owned
// table in the matching nft family ("ip" or "ip6"); a family with no rules gets
// no table here (the Manager deletes any stale one). Applying twice is safe.
func nftScript(rules []Rule) string {
	var b strings.Builder
	for _, fam := range []string{"ip", "ip6"} {
		var fr []Rule
		for _, r := range rules {
			if r.family() == fam {
				fr = append(fr, r)
			}
		}
		if len(fr) == 0 {
			continue
		}
		fmt.Fprintf(&b, "add table %s %s\n", fam, tableName)
		fmt.Fprintf(&b, "flush table %s %s\n", fam, tableName)
		fmt.Fprintf(&b, "add chain %s %s postrouting { type nat hook postrouting priority 100 ; }\n", fam, tableName)
		fmt.Fprintf(&b, "add chain %s %s prerouting { type nat hook prerouting priority -100 ; }\n", fam, tableName)
		for _, r := range fr {
			switch r.Kind {
			case Masquerade:
				fmt.Fprintf(&b, "add rule %s %s postrouting%s%s masquerade\n", fam, tableName, nftSaddr(r), nftOif(r))
			case SNAT:
				fmt.Fprintf(&b, "add rule %s %s postrouting%s%s snat to %s\n", fam, tableName, nftSaddr(r), nftOif(r), r.To)
			case DNAT:
				fmt.Fprintf(&b, "add rule %s %s prerouting%s%s%s dnat to %s\n", fam, tableName, nftDaddr(r), nftIif(r), nftDport(r), nftDnatTo(r))
			}
		}
	}
	return b.String()
}

func nftSaddr(r Rule) string {
	if r.Source.IsValid() {
		return " " + r.family() + " saddr " + r.Source.String()
	}
	return ""
}
func nftDaddr(r Rule) string {
	if r.Dest.IsValid() {
		return " " + r.family() + " daddr " + r.Dest.String()
	}
	return ""
}
func nftOif(r Rule) string {
	if r.OutIface != "" {
		return fmt.Sprintf(" oifname %q", r.OutIface)
	}
	return ""
}
func nftIif(r Rule) string {
	if r.InIface != "" {
		return fmt.Sprintf(" iifname %q", r.InIface)
	}
	return ""
}

// nftDport renders the DNAT port match — "" (every port) when DPortLo is
// unset, else e.g. " tcp dport 32400" or " tcp dport 8000-8010".
func nftDport(r Rule) string {
	if r.DPortLo == 0 {
		return ""
	}
	if r.DPortHi == r.DPortLo {
		return fmt.Sprintf(" %s dport %d", r.Proto, r.DPortLo)
	}
	return fmt.Sprintf(" %s dport %d-%d", r.Proto, r.DPortLo, r.DPortHi)
}

// nftDnatTo renders the dnat target: the bare address, or "address:port"
// when ToPort remaps it (IPv6 targets get bracketed, nft's own requirement
// for disambiguating the address's colons from the port separator).
func nftDnatTo(r Rule) string {
	if r.ToPort == 0 {
		return r.To.String()
	}
	if r.To.Is6() {
		return fmt.Sprintf("[%s]:%d", r.To, r.ToPort)
	}
	return fmt.Sprintf("%s:%d", r.To, r.ToPort)
}

// pfScript renders the ruleset as pf.conf-syntax nat/rdr lines, suitable for
// loading into a dedicated gravinet-owned anchor via `pfctl -a <anchor> -f -`
// on the pf-based platforms (macOS, FreeBSD, OpenBSD). Unlike nft, pf mixes
// address families in one ruleset (each line carries its own inet/inet6
// keyword), so v4 and v6 rules are emitted together, in input order.
//
// Masquerade with no OutIface uses pf's "egress" interface group (the
// interface currently holding the default route) so a dynamic outbound
// address can still be resolved, mirroring nft/iptables masquerade with no
// oifname (kernel picks the egress interface at forward time).
func pfScript(rules []Rule) string {
	var b strings.Builder
	for _, r := range rules {
		fam := "inet"
		if r.V6 {
			fam = "inet6"
		}
		switch r.Kind {
		case Masquerade:
			iface := r.OutIface
			if iface == "" {
				iface = "egress"
			}
			fmt.Fprintf(&b, "nat on %s %s from %s to any -> (%s)\n", iface, fam, pfAddr(r.Source), iface)
		case SNAT:
			if r.OutIface != "" {
				fmt.Fprintf(&b, "nat on %s %s from %s to any -> %s\n", r.OutIface, fam, pfAddr(r.Source), r.To)
			} else {
				fmt.Fprintf(&b, "nat %s from %s to any -> %s\n", fam, pfAddr(r.Source), r.To)
			}
		case DNAT:
			if r.InIface != "" {
				fmt.Fprintf(&b, "rdr on %s %s%s from any to %s%s -> %s%s\n", r.InIface, fam, pfProto(r), pfAddr(r.Dest), pfDport(r), r.To, pfToPort(r))
			} else {
				fmt.Fprintf(&b, "rdr %s%s from any to %s%s -> %s%s\n", fam, pfProto(r), pfAddr(r.Dest), pfDport(r), r.To, pfToPort(r))
			}
		}
	}
	return b.String()
}

// pfAddr renders a prefix match for pf syntax, or "any" when unset.
func pfAddr(p netip.Prefix) string {
	if p.IsValid() {
		return p.String()
	}
	return "any"
}

// pfProto renders the DNAT protocol match — "" (any protocol, every port)
// when DPortLo is unset, else " proto tcp"/" proto udp". pf requires an
// explicit protocol whenever a port is matched, same as nft/iptables.
func pfProto(r Rule) string {
	if r.DPortLo == 0 {
		return ""
	}
	return " proto " + r.Proto
}

// pfDport renders the DNAT destination port match on the "to" (pre-
// translation) side — "" (every port) when DPortLo is unset, else
// " port 32400", or a range using pf's own colon syntax (not a dash):
// " port 8000:8010".
func pfDport(r Rule) string {
	if r.DPortLo == 0 {
		return ""
	}
	if r.DPortHi == r.DPortLo {
		return fmt.Sprintf(" port %d", r.DPortLo)
	}
	return fmt.Sprintf(" port %d:%d", r.DPortLo, r.DPortHi)
}

// pfToPort renders the redirect target's port when ToPort remaps it — ""
// when ToPort is unset, relying on pf's own default of preserving the
// original destination port when the target side names no port.
func pfToPort(r Rule) string {
	if r.ToPort == 0 {
		return ""
	}
	return fmt.Sprintf(" port %d", r.ToPort)
}

// winNATScript renders the PowerShell script that (re)programs Windows'
// built-in NAT (WinNAT) to match the given ruleset, and separately reports
// which rules WinNAT's model cannot express.
//
// WinNAT is fundamentally a single-address PAT/masquerade engine keyed by an
// internal address prefix (New-NetNat -InternalIPInterfaceAddressPrefix): it
// always translates to whichever address the outbound interface currently
// holds, the same shape as Masquerade here. It has no equivalent of "SNAT to
// a fixed, arbitrary address" (iptables SNAT / pf "nat ... -> <addr>").
//
// DNAT is expressible too, but only for a single matched port with an
// explicit protocol and a concrete external address (Dest as a /32) — the
// exact shape Add-NetNatStaticMapping needs (protocol, external
// address+port, internal address+port). The original address-only,
// all-ports DNAT, and a DNAT rule matching a port *range*, both stay
// unsupported: neither has a WinNAT equivalent (a static mapping is
// necessarily one port at a time). Both are reported back as unsupported
// rather than silently dropped or half-applied.
//
// The script is idempotent and replaces the prior gravinet-owned NetNat
// objects wholesale: it removes any NetNat (and static mappings) whose name
// starts with the gravinetNATPrefix, then recreates one NetNat per
// expressible rule — including, for each qualifying DNAT rule, a dedicated
// NetNat scoped to the internal target as a /32 (WinNAT has no notion of a
// static mapping without a NAT context to attach it to via -NatName, so
// each DNAT rule gets its own rather than trying to share one across
// unrelated internal targets). OutIface is not passed to WinNAT: unlike
// nft/pf/iptables, WinNAT has no oifname-style match — it always follows
// the routing table.
func winNATScript(rules []Rule) (script string, unsupported []Rule) {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	fmt.Fprintf(&b, "Get-NetNatStaticMapping -ErrorAction SilentlyContinue | Where-Object { $_.NatName -like '%s*' } | Remove-NetNatStaticMapping -Confirm:$false -ErrorAction SilentlyContinue\n", winNATPrefix)
	fmt.Fprintf(&b, "Get-NetNat -ErrorAction SilentlyContinue | Where-Object { $_.Name -like '%s*' } | Remove-NetNat -Confirm:$false -ErrorAction SilentlyContinue\n", winNATPrefix)

	n := 0
	for _, r := range rules {
		if r.V6 {
			unsupported = append(unsupported, r) // WinNAT here is scoped to IPv4 only
			continue
		}
		switch r.Kind {
		case Masquerade:
			if !r.Source.IsValid() {
				unsupported = append(unsupported, r) // WinNAT requires a concrete internal prefix
				continue
			}
			fmt.Fprintf(&b, "New-NetNat -Name %q -InternalIPInterfaceAddressPrefix %q | Out-Null\n",
				fmt.Sprintf("%s%d", winNATPrefix, n), r.Source.String())
			n++
		case DNAT:
			if !winNATDNATSupported(r) {
				unsupported = append(unsupported, r) // address-only/all-ports, or a port range: no WinNAT equivalent
				continue
			}
			name := fmt.Sprintf("%s%d", winNATPrefix, n)
			n++
			fmt.Fprintf(&b, "New-NetNat -Name %q -InternalIPInterfaceAddressPrefix %q | Out-Null\n", name, r.To.String()+"/32")
			internalPort := r.DPortLo
			if r.ToPort != 0 {
				internalPort = r.ToPort
			}
			fmt.Fprintf(&b, "Add-NetNatStaticMapping -NatName %q -Protocol %s -ExternalIPAddress %q -ExternalPort %d -InternalIPAddress %q -InternalPort %d | Out-Null\n",
				name, winNATProto(r.Proto), r.Dest.Addr().String(), r.DPortLo, r.To.String(), internalPort)
		default:
			unsupported = append(unsupported, r) // SNAT-to-fixed-address: no WinNAT equivalent
		}
	}
	return b.String(), unsupported
}

// winNATDNATSupported reports whether r's DNAT can be expressed as a
// WinNAT static mapping: a single matched port (not a range, not "every
// port"), an explicit tcp/udp protocol, and a concrete external address
// (Dest as a single /32 — Add-NetNatStaticMapping needs one specific
// ExternalIPAddress, not a CIDR range or "any").
func winNATDNATSupported(r Rule) bool {
	return r.DPortLo != 0 && r.DPortLo == r.DPortHi &&
		(r.Proto == "tcp" || r.Proto == "udp") &&
		r.Dest.IsValid() && r.Dest.Bits() == 32
}

// winNATProto renders a protocol the way Add-NetNatStaticMapping's
// -Protocol parameter expects it (title case).
func winNATProto(proto string) string {
	if proto == "udp" {
		return "UDP"
	}
	return "TCP"
}

// winNATPrefix names every NetNat object gravinet owns, so Apply can find and
// replace them wholesale without touching anything else on the host.
const winNATPrefix = "gravinet_nat_"

// iptablesRuleArgs renders one rule as the argv that follows the `iptables`
// binary (an `-A` into our custom chain). Used by the iptables fallback backend.
func iptablesRuleArgs(r Rule) []string {
	switch r.Kind {
	case Masquerade:
		a := []string{"-t", "nat", "-A", iptPostChain}
		if r.Source.IsValid() {
			a = append(a, "-s", r.Source.String())
		}
		if r.OutIface != "" {
			a = append(a, "-o", r.OutIface)
		}
		return append(a, "-j", "MASQUERADE")
	case SNAT:
		a := []string{"-t", "nat", "-A", iptPostChain}
		if r.Source.IsValid() {
			a = append(a, "-s", r.Source.String())
		}
		if r.OutIface != "" {
			a = append(a, "-o", r.OutIface)
		}
		return append(a, "-j", "SNAT", "--to-source", r.To.String())
	case DNAT:
		a := []string{"-t", "nat", "-A", iptPreChain}
		if r.Dest.IsValid() {
			a = append(a, "-d", r.Dest.String())
		}
		if r.InIface != "" {
			a = append(a, "-i", r.InIface)
		}
		if r.DPortLo != 0 {
			a = append(a, "-p", r.Proto, "--dport", iptDportArg(r))
		}
		return append(a, "-j", "DNAT", "--to-destination", iptToDestination(r))
	}
	return nil
}

// iptDportArg renders --dport's value: "32400", or a range using
// iptables' own colon syntax (not a dash): "8000:8010".
func iptDportArg(r Rule) string {
	if r.DPortHi == r.DPortLo {
		return fmt.Sprintf("%d", r.DPortLo)
	}
	return fmt.Sprintf("%d:%d", r.DPortLo, r.DPortHi)
}

// iptToDestination renders --to-destination's value: the bare address, or
// "address:port" when ToPort remaps it (IPv6 targets get bracketed, same
// as nftDnatTo — see that function's doc comment).
func iptToDestination(r Rule) string {
	if r.ToPort == 0 {
		return r.To.String()
	}
	if r.To.Is6() {
		return fmt.Sprintf("[%s]:%d", r.To, r.ToPort)
	}
	return fmt.Sprintf("%s:%d", r.To, r.ToPort)
}
