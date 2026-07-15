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
				fmt.Fprintf(&b, "add rule %s %s prerouting%s%s dnat to %s\n", fam, tableName, nftDaddr(r), nftIif(r), r.To)
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
				fmt.Fprintf(&b, "rdr on %s %s from any to %s -> %s\n", r.InIface, fam, pfAddr(r.Dest), r.To)
			} else {
				fmt.Fprintf(&b, "rdr %s from any to %s -> %s\n", fam, pfAddr(r.Dest), r.To)
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

// winNATScript renders the PowerShell script that (re)programs Windows'
// built-in NAT (WinNAT) to match the given ruleset, and separately reports
// which rules WinNAT's model cannot express.
//
// WinNAT is fundamentally a single-address PAT/masquerade engine keyed by an
// internal address prefix (New-NetNat -InternalIPInterfaceAddressPrefix): it
// always translates to whichever address the outbound interface currently
// holds, the same shape as Masquerade here. It has no equivalent of "SNAT to
// a fixed, arbitrary address" (iptables SNAT / pf "nat ... -> <addr>"), and
// its only redirect primitive (Add-NetNatStaticMapping) requires an explicit
// protocol and port pair per mapping, so it cannot express our address-only,
// all-ports DNAT. Both are reported back as unsupported rather than silently
// dropped or half-applied.
//
// The script is idempotent and replaces the prior gravinet-owned NetNat
// objects wholesale: it removes any NetNat (and static mappings) whose name
// starts with the gravinetNATPrefix, then recreates one NetNat per
// expressible rule. OutIface is not passed to WinNAT: unlike nft/pf/iptables,
// WinNAT has no oifname-style match — it always follows the routing table.
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
		default:
			unsupported = append(unsupported, r) // SNAT-to-fixed-address and DNAT: no WinNAT equivalent
		}
	}
	return b.String(), unsupported
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
		return append(a, "-j", "DNAT", "--to-destination", r.To.String())
	}
	return nil
}
