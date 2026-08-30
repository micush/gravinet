package tui

// Traffic group actions. Every argv below was built from the exact usage
// string cmd/gravinet's own leaf prints on a bad invocation (cli_config.go's
// cmdNAT/cmdQoS/cmdBandwidth/cmdRoute, cli_groups.go's cmdTrafficBGP and
// cmdFW), read from source rather than guessed — the same discipline
// actions_mesh.go documents at its own top.
//
// Rule identity: NAT and QoS rules have no stable id of their own — the CLI
// addresses NAT rules by their position in cfg.NAT.Rules and QoS rules by
// their own match (protocol+port, or a service list) rather than a
// position. Row selection here uses whichever of those the CLI itself uses,
// so a delete or toggle can never target the wrong rule: NAT rows are keyed
// by index (matching cmdNAT's own delete/enable-rule), QoS rows are keyed
// by an encoded copy of the row's own match fields (matching cmdQoS's
// MATCH argument), reconstructed from the row's own data rather than
// re-parsed from a display string.

import (
	"strconv"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

func init() {
	registerActions("nat", natActions)
	registerActions("qos", qosActions)
	registerActions("bandwidth", bandwidthActions)
	registerActions("routes", routesActions)
	registerActions("bgp", bgpActions)
	registerActions("firewall", firewallActions)
	registerActions("ipv6ra", ipv6raActions)
}

// ---- nat --------------------------------------------------------------

var natActions = sectionActionSet{
	add: func(m *model) formSpec {
		return formSpec{
			title: "add NAT rule",
			fields: []formField{
				{key: "iface", label: "iface (shorthand)", kind: fieldText,
					help: "fill this ALONE for plain masquerade on an interface; leave blank to use the fields below instead"},
				{key: "source", label: "source", kind: fieldText},
				{key: "dest", label: "dest", kind: fieldText},
				{key: "dest_port", label: "dest-port", kind: fieldText, help: "N or N-M"},
				{key: "proto", label: "proto", kind: fieldText, help: "tcp | udp"},
				{key: "translate", label: "translate", kind: fieldText,
					help: "an address, \"masquerade\", or \"port-forward:ADDR[:PORT]\""},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				only := v["iface"] != "" && v["source"] == "" && v["dest"] == "" && v["dest_port"] == "" &&
					v["proto"] == "" && v["translate"] == ""
				if only {
					return runLeaf(m.cliArgs("nat", "add", v["iface"])...)
				}
				args := []string{"nat", "add"}
				for _, kw := range []struct{ key, flag string }{
					{"source", "source"}, {"dest", "dest"}, {"dest_port", "dest-port"},
					{"proto", "proto"}, {"translate", "translate"}, {"iface", "iface"},
				} {
					if val := v[kw.key]; val != "" {
						args = append(args, kw.flag, val)
					}
				}
				if len(args) == 2 {
					return mutationResult{ok: false, detail: "fill in either the iface shorthand alone, or at least one of the other fields"}
				}
				return runLeaf(m.cliArgs(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		idx, err := strconv.Atoi(row.id)
		if err != nil {
			return nil
		}
		r := natRuleAt(m, idx)
		label := "disable"
		if r != nil && !r.Enabled {
			label = "enable"
		}
		return map[rune]rowAction{
			'd': {label: "delete", confirm: "Delete NAT rule [" + row.id + "]?",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("nat", "delete", row.id)...)
				}},
			' ': {label: label, run: func(m *model, row selRow) mutationResult {
				verb := "disable-rule"
				if r != nil && !r.Enabled {
					verb = "enable-rule"
				}
				return runLeaf(m.cliArgs("nat", verb, row.id)...)
			}},
		}
	},
}

func natRuleAt(m *model, idx int) *config.NATRule {
	if m.snap == nil || m.snap.cfg == nil || idx < 0 || idx >= len(m.snap.cfg.NAT.Rules) {
		return nil
	}
	return &m.snap.cfg.NAT.Rules[idx]
}

// ---- qos ----------------------------------------------------------------

var qosActions = sectionActionSet{
	add: func(m *model) formSpec {
		return formSpec{
			title: "add QoS rule",
			fields: []formField{
				{key: "proto", label: "proto", kind: fieldText, help: "tcp | udp — leave blank if using services below"},
				{key: "port", label: "port", kind: fieldText},
				{key: "services", label: "services", kind: fieldText, help: "comma-separated, from the firewall's service catalog — alternative to proto/port"},
				{key: "priority", label: "priority", kind: fieldSelect, value: "high", options: []string{"highest", "high", "normal", "low", "lowest"}},
				{key: "scope", label: "scope (network)", kind: fieldText, help: "optional; blank applies to any network"},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				args := []string{"qos", "add"}
				if v["services"] != "" {
					args = append(args, "service", v["services"])
				} else if v["proto"] != "" && v["port"] != "" {
					args = append(args, v["proto"], v["port"])
				} else {
					return mutationResult{ok: false, detail: "fill in either proto+port, or a services list"}
				}
				args = append(args, "priority", v["priority"])
				if v["scope"] != "" {
					args = append(args, "scope", v["scope"])
				}
				return runLeaf(m.cliArgs(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		r := qosRuleAt(m, row.id)
		if r == nil {
			return nil
		}
		matchArgs := qosMatchArgs(*r)
		label := "disable"
		if r.Disabled {
			label = "enable"
		}
		return map[rune]rowAction{
			'd': {label: "delete", confirm: "Delete QoS rule " + qosRuleDisplayLabel(*r) + "?",
				run: func(m *model, row selRow) mutationResult {
					args := append([]string{"qos", "delete"}, matchArgs...)
					if r.Scope != "" {
						args = append(args, "scope", r.Scope)
					}
					return runLeaf(m.cliArgs(args...)...)
				}},
			' ': {label: label, run: func(m *model, row selRow) mutationResult {
				verb := "disable-rule"
				if r.Disabled {
					verb = "enable-rule"
				}
				args := append([]string{"qos", verb}, matchArgs...)
				if r.Scope != "" {
					args = append(args, "scope", r.Scope)
				}
				return runLeaf(m.cliArgs(args...)...)
			}},
		}
	},
}

// qosRuleDisplayLabel is a display-only echo of cmd/gravinet's own
// qosRuleMatchLabel (unreachable from here — different package), used only
// to word a confirm dialog; it carries no validation and isn't part of the
// argv any mutation sends, so duplicating the formatting (not the logic)
// doesn't create the kind of drift risk a second validator would.
func qosRuleDisplayLabel(r config.QoSRule) string {
	var parts []string
	if r.Protocol != "" || r.PortMin != 0 || r.PortMax != 0 {
		port := strconv.Itoa(r.PortMin)
		if r.PortMax != r.PortMin {
			port = strconv.Itoa(r.PortMin) + "-" + strconv.Itoa(r.PortMax)
		}
		proto := r.Protocol
		if proto == "" {
			proto = "any"
		}
		parts = append(parts, proto+" port "+port)
	}
	if len(r.Services) > 0 {
		parts = append(parts, "services "+strings.Join(r.Services, ","))
	}
	if len(parts) == 0 {
		return "any"
	}
	return strings.Join(parts, " + ")
}
// there is no simpler identity, since the CLI itself addresses a rule by
// its match, not by position (rules can be reordered by a config edit made
// elsewhere without changing what any of them mean).
func qosRuleID(r config.QoSRule) string {
	if len(r.Services) > 0 {
		return "svc" + idSep + strings.Join(r.Services, ",") + idSep + r.Scope
	}
	return "pp" + idSep + r.Protocol + idSep + strconv.Itoa(r.PortMin) + idSep + strconv.Itoa(r.PortMax) + idSep + r.Scope
}

func qosRuleAt(m *model, id string) *config.QoSRule {
	if m.snap == nil || m.snap.cfg == nil {
		return nil
	}
	for i := range m.snap.cfg.QoS.Rules {
		if qosRuleID(m.snap.cfg.QoS.Rules[i]) == id {
			return &m.snap.cfg.QoS.Rules[i]
		}
	}
	return nil
}

// qosMatchArgs rebuilds the MATCH argument(s) cmdQoS expects, from the
// rule's own stored fields rather than any display text — the same
// distinction rows.go draws for why re-deriving from real data beats
// re-parsing a rendered string.
func qosMatchArgs(r config.QoSRule) []string {
	if len(r.Services) > 0 {
		return []string{"service", strings.Join(r.Services, ",")}
	}
	proto := r.Protocol
	if proto == "" {
		proto = "tcp"
	}
	return []string{proto, strconv.Itoa(r.PortMin)}
}

// ---- bandwidth / shaping --------------------------------------------------

var bandwidthActions = sectionActionSet{
	add: func(m *model) formSpec {
		return formSpec{
			title:  "shape an interface",
			fields: []formField{{key: "iface", label: "iface", kind: fieldText}},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["iface"] == "" {
					return mutationResult{ok: false, detail: "an interface name is required"}
				}
				return runLeaf(m.cliArgs("bandwidth", "add", "-iface", v["iface"])...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		sh := shapingFor(m, row.id)
		label := "disable"
		if sh != nil && !sh.Enabled {
			label = "enable"
		}
		return map[rune]rowAction{
			'e': {label: "set rates", edit: func(m *model, row selRow) formSpec { return bandwidthEditForm(m, row.id) }},
			'd': {label: "remove", confirm: "Remove the shaping entry for " + row.id + "? It becomes unshaped, not just disabled.",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("bandwidth", "del", "-iface", row.id)...)
				}},
			' ': {label: label, run: func(m *model, row selRow) mutationResult {
				verb := "disable"
				if sh != nil && !sh.Enabled {
					verb = "enable"
				}
				return runLeaf(m.cliArgs("bandwidth", verb, "-iface", row.id)...)
			}},
		}
	},
}

func shapingFor(m *model, iface string) *config.IfaceShaping {
	if m.snap == nil || m.snap.cfg == nil {
		return nil
	}
	for i := range m.snap.cfg.Shaping {
		if m.snap.cfg.Shaping[i].Iface == iface {
			return &m.snap.cfg.Shaping[i]
		}
	}
	return nil
}

func bandwidthEditForm(m *model, iface string) formSpec {
	sh := shapingFor(m, iface)
	up, down := "unlimited", "unlimited"
	if sh != nil {
		up, down = config.RateString(sh.UpBytesPerSec), config.RateString(sh.DownBytesPerSec)
	}
	return formSpec{
		title: "shaping on " + iface,
		fields: []formField{
			// The stored value is bytes/sec but the CLI's own rate parser
			// (config.ParseRate) takes a *bits*-per-second string with a
			// unit suffix — "unlimited" prints, but re-entering that same
			// number bare would be reinterpreted as bits/sec, silently
			// landing 8x too small. Pre-filling with config.RateString's
			// own output keeps what's shown and what's re-typed in the one
			// format the CLI actually parses.
			{key: "up", label: "up", kind: fieldText, value: up, help: "e.g. 10Mbps, 1Gbps, or \"unlimited\""},
			{key: "down", label: "down", kind: fieldText, value: down},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			var results []mutationResult
			if v["up"] != up {
				results = append(results, runLeaf(m.cliArgs("bandwidth", "up", rateArg(v["up"]), "-iface", iface)...))
			}
			if v["down"] != down {
				results = append(results, runLeaf(m.cliArgs("bandwidth", "down", rateArg(v["down"]), "-iface", iface)...))
			}
			return combineResults(results)
		},
	}
}

// rateArg turns the form's "unlimited" spelling into what cmdBandwidth
// actually expects for it — 0bps parses cleanly through config.ParseRate
// and Config.ShapingSet treats a zero rate as unlimited, the same meaning
// RateString gives 0 when it renders a value back.
func rateArg(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "unlimited") {
		return "0"
	}
	return v
}

// ---- routes ---------------------------------------------------------------

var routesActions = sectionActionSet{
	add: func(m *model) formSpec {
		net := currentNetworkHint(m)
		return formSpec{
			title: "advertise a route" + netHintSuffix(net),
			fields: []formField{
				{key: "net", label: "network", kind: fieldText, value: net},
				{key: "cidr", label: "cidr", kind: fieldText},
				{key: "metric", label: "metric", kind: fieldText, value: "0"},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["cidr"] == "" {
					return mutationResult{ok: false, detail: "a CIDR is required"}
				}
				args := []string{"route", "add", v["cidr"]}
				if v["net"] != "" {
					args = append(args, "-net", v["net"])
				}
				if v["metric"] != "" && v["metric"] != "0" {
					args = append(args, "-metric", v["metric"])
				}
				return runLeaf(m.cliArgs(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		netName, cidr, ok := splitSeedRowID(row.id)
		if !ok {
			return nil
		}
		r := routeFor(m, netName, cidr)
		label := "disable"
		if r != nil && !r.Enabled {
			label = "enable"
		}
		return map[rune]rowAction{
			'd': {label: "delete", confirm: "Stop advertising " + cidr + " on " + netName + "?",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("route", "delete", cidr, "-net", netName)...)
				}},
			' ': {label: label, run: func(m *model, row selRow) mutationResult {
				verb := "disable"
				if r != nil && !r.Enabled {
					verb = "enable"
				}
				return runLeaf(m.cliArgs("route", verb, cidr, "-net", netName)...)
			}},
		}
	},
}

func routeFor(m *model, netName, cidr string) *config.Route {
	n := findNetwork(m, netName)
	if n == nil {
		return nil
	}
	for i := range n.Routes {
		if n.Routes[i].CIDR == cidr {
			return &n.Routes[i]
		}
	}
	return nil
}

// ---- bgp --------------------------------------------------------------

var bgpActions = sectionActionSet{
	row: func(m *model, row selRow) map[rune]rowAction {
		n := bgpNeighborFor(m, row.id)
		if n == nil {
			return nil
		}
		label := "shut down"
		if n.Shutdown {
			label = "bring up"
		}
		return map[rune]rowAction{
			'e': {label: "edit", edit: func(m *model, row selRow) formSpec { return bgpNeighborEditForm(m, row.id) }},
			'd': {label: "remove", confirm: "Remove BGP neighbor " + row.id + "?",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("traffic", "bgp", "neighbor", "del", row.id)...)
				}},
			' ': {label: label, run: func(m *model, row selRow) mutationResult {
				nn := *n
				nn.Shutdown = !nn.Shutdown
				return bgpNeighborUpsert(m, nn)
			}},
		}
	},
}

func bgpNeighborFor(m *model, peer string) *config.BGPNeighbor {
	if m.snap == nil || m.snap.cfg == nil {
		return nil
	}
	for i := range m.snap.cfg.BGP.Neighbors {
		if m.snap.cfg.BGP.Neighbors[i].Peer == peer {
			return &m.snap.cfg.BGP.Neighbors[i]
		}
	}
	return nil
}

func bgpNeighborEditForm(m *model, peer string) formSpec {
	n := bgpNeighborFor(m, peer)
	desc, bfd := "", "false"
	if n != nil {
		desc, bfd = n.Description, onOffBool(n.BFD)
	}
	return formSpec{
		title: "bgp neighbor " + peer,
		fields: []formField{
			{key: "description", label: "description", kind: fieldText, value: desc},
			{key: "bfd", label: "bfd", kind: fieldBool, value: bfd},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			cur := bgpNeighborFor(m, peer)
			if cur == nil {
				return mutationResult{ok: false, detail: "neighbor no longer exists"}
			}
			nn := *cur
			nn.Description, nn.BFD = v["description"], v["bfd"] == "true"
			return bgpNeighborUpsert(m, nn)
		},
	}
}

// bgpNeighborUpsert shells out to "neighbor add", which cmdTrafficBGPNeighborAdd
// documents as an upsert (replaces an existing peer's entry in place) —
// exactly what's needed to change description/bfd/shutdown on an existing
// neighbor without a separate "neighbor set" verb, which doesn't exist.
func bgpNeighborUpsert(m *model, n config.BGPNeighbor) mutationResult {
	args := []string{"traffic", "bgp", "neighbor", "add", n.Peer, strconv.FormatUint(uint64(n.RemoteAS), 10)}
	if n.Description != "" {
		args = append(args, "-description", n.Description)
	}
	if n.BFD {
		args = append(args, "-bfd")
	}
	if n.Shutdown {
		args = append(args, "-shutdown")
	}
	return runLeaf(m.cliArgs(args...)...)
}

// ---- firewall ---------------------------------------------------------
//
// Firewall rules are live control-socket state, not a config-file field —
// confirmed against cmd/gravinet/cli_groups.go's cmdFW, which sends every
// verb (add/del/move/copy/cut/paste) through control.Do rather than
// openCfg/commitCfg. The read side (pageFirewall, sections.go) was
// rewritten to match in the same pass that added this — it used to read
// cfg.Firewall.Rules, which is not what's actually enforced.

var firewallActions = sectionActionSet{
	add: func(m *model) formSpec {
		net := currentNetworkHint(m)
		return formSpec{
			title: "add firewall rule" + netHintSuffix(net),
			fields: []formField{
				{key: "scope", label: "network (scope)", kind: fieldText, value: net, help: "blank applies to every network"},
				{key: "action", label: "action", kind: fieldSelect, value: "allow", options: []string{"allow", "deny"}},
				{key: "direction", label: "direction", kind: fieldSelect, value: "both", options: []string{"in", "out", "both"}},
				{key: "proto", label: "proto", kind: fieldText, help: "tcp | udp | icmp | any; blank = any"},
				{key: "source", label: "source", kind: fieldText, help: "blank = any"},
				{key: "dest", label: "dest", kind: fieldText, help: "blank = any"},
				{key: "dport", label: "dest port", kind: fieldText, help: "N or N-M; blank = any"},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				// Every field on "fw add" is a flag — there is no
				// positional form at all (confirmed against cmd/gravinet's
				// own flag.NewFlagSet call for it), unlike "fw del" below.
				args := []string{"fw", "add", "-action", v["action"], "-dir", v["direction"]}
				if v["scope"] != "" {
					args = append(args, "-scope", v["scope"])
				}
				if v["proto"] != "" {
					args = append(args, "-proto", v["proto"])
				}
				if v["source"] != "" {
					args = append(args, "-src", v["source"])
				}
				if v["dest"] != "" {
					args = append(args, "-dst", v["dest"])
				}
				if v["dport"] != "" {
					args = append(args, "-dport", v["dport"])
				}
				return runLeaf(m.cliArgsSock(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		r := firewallRuleFor(m, row.id)
		return map[rune]rowAction{
			'd': {label: "delete", confirm: "Delete firewall rule [" + row.id + "]?",
				run: func(m *model, row selRow) mutationResult {
					// "fw del" takes the id positionally (splitIDs reads it
					// off rest directly, no flag) — the one part of this
					// leaf that isn't all-flags, confirmed the same way.
					args := []string{"fw", "del", row.id}
					if r != nil && r.Scope != "" {
						args = append(args, "-scope", r.Scope)
					}
					return runLeaf(m.cliArgsSock(args...)...)
				}},
		}
	},
}

func firewallRuleFor(m *model, id string) *mesh.FirewallRule {
	if m.snap == nil {
		return nil
	}
	for i := range m.snap.firewall {
		if strconv.FormatUint(m.snap.firewall[i].ID, 10) == id {
			return &m.snap.firewall[i]
		}
	}
	return nil
}

// ---- ipv6ra -----------------------------------------------------------
//
// Deliberately enable/disable only. cmd/gravinet's own cmdIPv6RA comment
// explains why a full interface entry (prefix, DNS, search list) isn't a
// CLI leaf: the web admin's editor validates those together and there is no
// single reusable config.Config setter to call instead, so reproducing it
// here would be exactly the second, weaker implementation this whole
// package exists to avoid. Same boundary, same reason, kept here too.

var ipv6raActions = sectionActionSet{
	row: func(m *model, row selRow) map[rune]rowAction {
		iface := row.id
		ra := ra6InterfaceFor(m, iface)
		label := "disable"
		if ra != nil && ra.Disabled {
			label = "enable"
		}
		return map[rune]rowAction{
			' ': {label: label, run: func(m *model, row selRow) mutationResult {
				verb := "disable"
				if ra != nil && ra.Disabled {
					verb = "enable"
				}
				return runLeaf(m.cliArgs("traffic", "ipv6ra", verb, iface)...)
			}},
		}
	},
}

func ra6InterfaceFor(m *model, iface string) *config.RAInterface {
	if m.snap == nil || m.snap.cfg == nil {
		return nil
	}
	for i := range m.snap.cfg.RouterAdvert.Interfaces {
		if m.snap.cfg.RouterAdvert.Interfaces[i].Iface == iface {
			return &m.snap.cfg.RouterAdvert.Interfaces[i]
		}
	}
	return nil
}
