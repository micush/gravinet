package tui

// One builder per page. Each takes the snapshot and returns []card; none of
// them touches the terminal, computes a width, or does I/O. See content.go
// for why that split matters.
//
// Every page that has an editor ends with an editHint naming the command
// that reaches it, because the answer to "how do I change this" belongs on
// the page raising the question. The strings are checked against the CLI's
// own leaf tables by sections_test.go, so a renamed command does not leave
// forty-two pages advertising a command that no longer exists.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/webadmin"
)

// pageCtx is what a builder gets: the snapshot, plus the things that are
// fetched lazily because they are slow or platform-dependent (see app.go's
// worker). A builder reads lazy data through the accessors on this rather
// than blocking, so a page whose data has not arrived yet renders a "reading"
// state instead of freezing the console.
type pageCtx struct {
	snap *snapshot
	lazy *lazyState
}

// buildPage renders one section. Unknown sections cannot happen (the rail is
// built from the same table) but are handled rather than panicking: a console
// is the wrong place to discover an unhandled case.
func buildPage(sec string, c pageCtx) []card {
	if fn, ok := pageBuilders[sec]; ok {
		return fn(c)
	}
	return []card{{title: sectionHeading(sec), items: []item{
		empty{"this page has no builder — this is a bug in internal/tui"},
	}}}
}

// pageBuilders is the dispatch table, the terminal counterpart of
// renderSection's object literal in ui.go. Keyed by the same section keys, so
// a section in navGroups with no entry here is caught by sections_test.go
// walking every key.
var pageBuilders = map[string]func(pageCtx) []card{
	// mesh
	"networks": pageNetworks,
	"keys":     pageKeys,
	"seeds":    pageSeeds,
	"peers":    pagePeers,
	"bans":     pageBans,
	// traffic
	"firewall":  pageFirewall,
	"nat":       pageNAT,
	"qos":       pageQoS,
	"ipv6ra":    pageIPv6RA,
	"bandwidth": pageBandwidth,
	"routes":    pageRoutes,
	"bgp":       pageBGP,
	// naming
	"dns":   pageDNS,
	"hosts": pageHosts,
	// monitor
	"metrics":     pageMetrics,
	"mesh-peers":  pageMeshPeers,
	"capture":     pageCapture,
	"speedtest":   pageSpeedtest,
	"latency":     pageLatency,
	"route-table": pageRouteTable,
	"bgp-peers":   pageBGPPeers,
	"l2-peers":    pageL2Peers,
	"hosts-file":  pageHostsFile,
	"dns-state":   pageDNSState,
	"logs":        pageLogs,
	// system
	"upgrade":        pageUpgrade,
	"interfaces":     pageInterfaces,
	"resolver":       pageResolver,
	"time":           pageTime,
	"dhcp":           pageDHCP,
	"snmp":           pageSNMP,
	"lldp":           pageLLDP,
	"syslog":         pageSyslog,
	"users":          pageUsers,
	"config-history": pageConfigHistory,
	"power":          pagePower,
	// info
	"readme":          pageReadme,
	"getting-started": pageGettingStarted,
	"api":             pageAPIDoc,
	"license":         pageLicense,
	"about":           pageAbout,
	// gear
	"settings": pageSettings,
}

// ---- shared page furniture ---------------------------------------------

// editHint is the footer every configuration page carries. Phrased as what
// to run rather than as an apology for not being a form: the operator is
// already at a shell, and the command is the useful half of the sentence.
// editHint used to be the footer every still-read-only config page carried
// ("this console reads; it does not write — to change this, run: ..."). It
// has no callers left: every page that used to show it now has a real
// action reaching the same command instead. Removed rather than left as
// dead code, so its disappearance is itself the signal that the read/write
// migration finished — a future page that goes back to read-only for a
// documented reason (the same boundary IPv6RA and DHCP relay links already
// draw) writes its own explanatory note card instead of resurrecting a
// generic one.

// noConfig is what every config-backed page shows when the file could not be
// read. Names the path and the error, because "no networks" and "I could not
// open /etc/gravinet/config.json" are very different findings and only one of
// them is about networks.
func noConfig(s *snapshot) card {
	return card{title: "config", items: []item{
		para{text: fmt.Sprintf("could not read %s: %v", s.cfgPath, s.cfgErr), tone: "danger"},
	}}
}

// noDaemon is the live-page counterpart. The distinction it draws — the
// daemon is unreachable, as opposed to reporting nothing — is the entire
// reason this exists rather than an empty table.
func noDaemon(s *snapshot) card {
	return card{title: "daemon", items: []item{
		para{text: fmt.Sprintf("the daemon is not reachable on %s: %v", s.sockPath, s.daemonErr), tone: "danger"},
		para{text: "This page shows live state, so there is nothing to show until the daemon answers. " +
			"Configuration pages still work — they read the file directly.", tone: "mut"},
	}}
}

// ---- small formatters ---------------------------------------------------

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func enabledTone(b bool) string {
	if b {
		return "ok"
	}
	return "mut"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// dash renders an empty value as an em dash rather than as nothing, so a
// blank cell always means "no value" and never "the renderer dropped it".
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "\u2014"
	}
	return s
}

func joinOr(list []string, empty string) string {
	if len(list) == 0 {
		return empty
	}
	return strings.Join(list, ", ")
}

func intsToStr(v []int) string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, ",")
}

// formatBytes renders a byte count the way an operator reads one.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// formatRate matches cmd/gravinet's own formatRate, so the metrics page and
// "gravinet monitor metrics" report throughput identically.
func formatRate(bps float64) string {
	units := []string{"B/s", "KB/s", "MB/s", "GB/s"}
	v := bps
	for _, u := range units {
		if v < 1024 || u == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", v, u)
		}
		v /= 1024
	}
	return fmt.Sprintf("%.1f B/s", bps)
}

// formatUptime matches cmd/gravinet's formatUptime, for the same reason.
func formatUptime(secs uint64) string {
	d := secs / 86400
	h := (secs % 86400) / 3600
	m := (secs % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// formatAge renders how long ago a nanosecond timestamp was. Coarse on
// purpose: a session established eleven hours ago is "11h", and the extra
// precision would only make the column wider.
func formatAge(unixNano int64) string {
	if unixNano == 0 {
		return "\u2014"
	}
	d := time.Since(time.Unix(0, unixNano))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// shortID trims a node id to its first eight characters, the same prefix the
// web admin falls back to when a peer has no hostname yet.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// peerName is the display-name rule used everywhere peers are listed:
// hostname if there is one, an id prefix otherwise.
func peerName(hostname, nodeID string) string {
	if hostname != "" {
		return hostname
	}
	return shortID(nodeID)
}

// ---- mesh ---------------------------------------------------------------

func pageNetworks(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	if len(s.cfg.Networks) == 0 {
		return []card{
			{title: "networks", items: []item{empty{"no overlay networks are configured"}}},
		}
	}

	// Configured on the left, live on the right. The interface column is the
	// join with the daemon and is the column that answers "why can I not
	// reach anything on this network" more often than the rest together: a
	// network enabled in the file with no interface is a network that failed
	// to come up.
	t := table{
		selectKey: "networks",
		head:      []string{"name", "id", "state", "subnet4", "subnet6", "address4", "address6", "mtu", "iface", "mesh"},
	}
	for _, n := range s.cfg.Networks {
		iface, up := s.ifaceFor(n.Name)
		ifaceCell, ifaceTone := iface, "ok"
		switch {
		case !s.daemonUp():
			ifaceCell, ifaceTone = "?", "mut"
		case !up && n.Enabled:
			ifaceCell, ifaceTone = "not up", "danger"
		case !up:
			ifaceCell, ifaceTone = "\u2014", "mut"
		}
		meshMode := n.Mesh
		if meshMode == "" {
			meshMode = "full"
		}
		row := tableRow{cells: []string{
			n.Name, n.ID, onOff(n.Enabled),
			dash(n.Subnet4), dash(n.Subnet6),
			dash(n.Address4), dash(n.Address6),
			strconv.Itoa(n.MTU), ifaceCell, meshMode,
		}, cellTone: map[int]string{2: enabledTone(n.Enabled), 8: ifaceTone}}
		if !n.Enabled {
			row.tone = "dim"
		}
		t.rows = append(t.rows, row)
		t.ids = append(t.ids, n.Name)
	}

	cards := []card{{title: "networks", items: []item{t}}}
	if !s.daemonUp() {
		cards[0].items = append(cards[0].items,
			para{text: "the iface column reads ? because the daemon is not reachable — everything else here is from the config file", tone: "warn"})
	}
	return append(cards, card{title: "note", items: []item{para{
		text: "The advanced form (E) changes this node's address, mesh mode, relay willingness, and self-seeding \u2014 " +
			"those need a restart to take effect. Joining an existing network onto a new node, or minting an invite " +
			"token for one, is a separate flow from editing a row here: \"gravinet network join\" / \"gravinet network token\".",
		tone: "mut"}}})
}

func pageKeys(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	var cards []card
	for _, n := range s.cfg.Networks {
		// Every slot is shown now, empty ones included: an empty slot is a
		// selectable row too (its 'e' action opens the generate/import
		// form), which is why this no longer skips them the way the
		// read-only version of this page did.
		t := table{selectKey: "keys", head: []string{"slot", "state", "label", "key", "distributed", "expires", "notes"}}
		for i, k := range n.Keys {
			id := keyRowID(n.Name, i)
			if k.Key == "" && k.Label == "" {
				t.rows = append(t.rows, tableRow{cells: []string{
					strconv.Itoa(i), "\u2014", "(empty)", "\u2014", "\u2014", "\u2014", "\u2014",
				}, tone: "mut"})
				t.ids = append(t.ids, id)
				continue
			}
			// The key itself is never shown. The web admin does not show it
			// either without a deliberate reveal, and a terminal is a place
			// where the last screenful lives in a scrollback buffer and
			// frequently in somebody's terminal recording.
			row := tableRow{cells: []string{
				strconv.Itoa(i), onOff(k.Enabled), dash(k.Label), "(set)",
				yesNo(k.Distributed), dash(k.Expires), dash(k.Notes),
			}, cellTone: map[int]string{1: enabledTone(k.Enabled)}}
			if !k.Enabled {
				row.tone = "dim"
			}
			t.rows = append(t.rows, row)
			t.ids = append(t.ids, id)
		}
		cards = append(cards, card{title: "keys \u2014 " + n.Name, items: []item{t}})
	}
	if len(cards) == 0 {
		cards = append(cards, card{title: "keys", items: []item{empty{"no networks are configured"}}})
	}
	return append(cards,
		card{title: "note", items: []item{para{
			text: "Key material is never displayed here; \"gravinet mesh keys reveal\" prints one deliberately, " +
				"which is the right shape for something that ends up in a scrollback buffer, and \"gravinet mesh keys " +
				"distribute\" pushes a rotated key to every current member over the live mesh.", tone: "mut"}}})
}

func pageSeeds(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	var cards []card
	for _, n := range s.cfg.Networks {
		t := table{selectKey: "seeds", head: []string{"address", "state", "node", "notes"}}
		for _, sd := range n.Seeds {
			row := tableRow{cells: []string{
				sd.Address, onOff(!sd.Disabled), dash(shortID(sd.Node)), dash(sd.Notes),
			}, cellTone: map[int]string{1: enabledTone(!sd.Disabled)}}
			if sd.Disabled {
				row.tone = "dim"
			}
			t.rows = append(t.rows, row)
			t.ids = append(t.ids, n.Name+idSep+sd.Address)
		}
		items := []item{}
		if len(t.rows) > 0 {
			items = append(items, t)
		} else {
			items = append(items, empty{"no seeds configured"})
		}
		if n.SelfSeed {
			items = append(items, para{text: "this node advertises itself as a seed on this network", tone: "ok"})
		}
		cards = append(cards, card{title: "seeds \u2014 " + n.Name, items: items})
	}
	if len(cards) == 0 {
		cards = append(cards, card{title: "seeds", items: []item{empty{"no networks are configured"}}})
	}
	return cards
}

func pagePeers(c pageCtx) []card {
	s := c.snap
	if !s.daemonUp() {
		return []card{noDaemon(s)}
	}
	if len(s.peers) == 0 {
		return []card{{title: "peers", items: []item{empty{"no peers are currently connected"}}}}
	}
	t := table{selectKey: "peers", head: []string{"peer", "network", "node id", "overlay4", "overlay6", "endpoint", "path", "rtt", "version"}}
	for _, p := range s.peers {
		path, tone := "direct", "ok"
		if p.Relayed {
			path, tone = "relay "+dash(p.RelayVia), "warn"
		}
		rtt := "\u2014"
		if p.RTTMs > 0 {
			rtt = fmt.Sprintf("%.1fms", p.RTTMs)
		}
		endpoint := p.Endpoint
		if p.Relayed {
			// A relayed session has no direct underlay address of its own;
			// showing the zero AddrPort here is how a relay gets mistaken
			// for a broken endpoint. Same treatment the web admin gives it.
			endpoint = "\u2014"
		}
		t.rows = append(t.rows, tableRow{cells: []string{
			peerName(p.Hostname, p.NodeID), p.net, shortID(p.NodeID),
			dash(p.Overlay4), dash(p.Overlay6), dash(endpoint), path, rtt, dash(p.Version),
		}, cellTone: map[int]string{6: tone}})
		t.ids = append(t.ids, p.net+idSep+p.NodeID)
	}
	return []card{
		{title: "peers", items: []item{t}},
		card{title: "note", items: []item{para{
			text: "Disabling stops this node dialing a peer but does not force a disconnect if the peer is the one " +
				"that reconnects \u2014 ban for that.", tone: "mut"}}},
	}
}

func pageBans(c pageCtx) []card {
	s := c.snap
	if !s.daemonUp() {
		return []card{noDaemon(s)}
	}
	if len(s.bans) == 0 {
		return []card{
			{title: "bans", items: []item{empty{"no nodes are banned"}}},
		}
	}
	t := table{selectKey: "bans", head: []string{"target", "network", "hostname", "issued by", "mine", "when", "notes"}}
	for _, b := range s.bans {
		t.rows = append(t.rows, tableRow{cells: []string{
			shortID(b.Target), b.net, dash(b.Hostname),
			dash(peerName(b.OriginHostname, b.Origin)), yesNo(b.Mine),
			formatAge(b.At) + " ago", dash(b.Notes),
		}, tone: "danger"})
		t.ids = append(t.ids, b.net+idSep+b.Target)
	}
	return []card{
		{title: "bans", items: []item{t}},
		card{title: "note", items: []item{para{
			text: "Only bans this node issued (\"mine\" yes) can be lifted here; a ban another node issued is " +
				"lifted on that node.", tone: "mut"}}},
	}
}

// ---- traffic ------------------------------------------------------------

func pageFirewall(c pageCtx) []card {
	s := c.snap
	if !s.daemonUp() {
		return []card{noDaemon(s)}
	}
	var cards []card

	t := table{selectKey: "firewall", head: []string{
		"id", "state", "action", "dir", "proto", "source", "destination", "ports", "services", "scope", "hits", "log", "notes"}}
	for _, r := range s.firewall {
		tone := ""
		if r.Disabled {
			tone = "dim"
		}
		actTone := "ok"
		if strings.EqualFold(r.Action, "deny") || strings.EqualFold(r.Action, "drop") || strings.EqualFold(r.Action, "reject") {
			actTone = "danger"
		}
		cellTone := map[int]string{2: actTone}
		if r.Disabled {
			cellTone = nil
		}
		t.rows = append(t.rows, tableRow{cells: []string{
			strconv.FormatUint(r.ID, 10), onOff(!r.Disabled), r.Action, dash(r.Direction), dash(r.Proto),
			negate(r.Src, r.SrcNegate), negate(r.Dst, r.DstNegate),
			portRange(r.DstPortMin, r.DstPortMax),
			negate(joinOr(r.Services, ""), r.ServicesNegate),
			dash(r.Scope), formatBytes(r.Bytes), yesNo(r.Log), dash(r.Notes),
		}, tone: tone, cellTone: cellTone})
		t.ids = append(t.ids, strconv.FormatUint(r.ID, 10))
	}
	items := []item{}
	if len(t.rows) > 0 {
		items = append(items, t)
	} else {
		items = append(items, empty{"no firewall rules"})
	}
	cards = append(cards, card{title: "firewall", items: items})

	if s.cfg != nil {
		if ex := s.cfg.EffectiveFirewallExempt(); len(ex) > 0 {
			et := table{head: []string{"name", "proto", "port", "management", "state"}}
			for _, e := range ex {
				port := "\u2014"
				if e.Port != 0 {
					port = strconv.Itoa(e.Port)
				}
				row := tableRow{cells: []string{e.Name, dash(e.Proto), port, yesNo(e.Mgmt), onOff(!e.Disabled)},
					cellTone: map[int]string{4: enabledTone(!e.Disabled)}}
				if e.Disabled {
					row.tone = "dim"
				}
				et.rows = append(et.rows, row)
			}
			cards = append(cards, card{title: "exemptions", items: []item{
				para{text: "traffic allowed regardless of the rules above — the ports that keep this node manageable " +
					"(edit with \"gravinet fw exempt -h\")", tone: "mut"},
				et,
			}})
		}
	}
	return append(cards, card{title: "note", items: []item{para{
		text: "\"scope\" is which network a rule applies to — blank means every network. Rules are node-global; " +
			"there is no per-network rulebase to switch between.", tone: "mut"}}})
}

// negate renders a rule field that may be inverted, marking the inversion
// with a leading "!" the way the web admin's own rule editor does. An
// inverted source rendered without its "!" is a rule that means the opposite
// of what it says, which is the worst possible thing for a firewall page to
// get wrong.
func negate(v string, neg bool) string {
	if strings.TrimSpace(v) == "" {
		return "any"
	}
	if neg {
		return "!" + v
	}
	return v
}

// portRange renders a min/max pair as a single cell.
func portRange(lo, hi int) string {
	switch {
	case lo == 0 && hi == 0:
		return "any"
	case hi == 0 || hi == lo:
		return strconv.Itoa(lo)
	default:
		return fmt.Sprintf("%d-%d", lo, hi)
	}
}

func pageNAT(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	cards := []card{{title: "nat", items: []item{editableKV{rows: []editableKVRow{
		{k: "state", v: onOff(s.cfg.NAT.Enabled), tone: enabledTone(s.cfg.NAT.Enabled),
			edit: func(m *model) formSpec { return verbBoolForm("nat", []string{"nat"}, "enable", "disable", s.cfg.NAT.Enabled) }},
		{k: "state timeout", v: natTimeout(s.cfg.NATStateTimeout),
			edit: func(m *model) formSpec {
				return intSettingForm("NAT state timeout (seconds)", "nat-state", s.cfg.NATStateTimeout, "0 = default 120s")
			}},
		{k: "observed class", v: dash(s.natClass), tone: natClassTone(s.natClass)},
		{k: "public address", v: dash(s.natPublic)},
	}}}}}
	if !s.daemonUp() {
		cards[0].items = append(cards[0].items,
			para{text: "observed class and public address are live readings and need the daemon", tone: "warn"})
	}

	// NAT rules live at the node level only (cfg.NAT.Rules) as of v953 —
	// the same source "gravinet nat list" reads, and the only one worth
	// matching: a per-network NAT.Rules field still exists on the struct
	// for old config migration, but the running daemon and every CLI verb
	// ignore it, so showing it here would show rules that are not in force.
	rules := s.cfg.NAT.Rules
	if len(rules) > 0 {
		t := table{selectKey: "nat", head: []string{"state", "dir", "proto", "source", "destination", "dport", "translate", "iface"}}
		for i, r := range rules {
			row := tableRow{cells: []string{
				onOff(r.Enabled), dash(r.Direction), dash(r.Proto),
				negate(r.Source, r.SourceNegate), negate(r.Dest, r.DestNegate),
				dash(r.DestPort), dash(r.Translate), dash(r.Interface),
			}, cellTone: map[int]string{0: enabledTone(r.Enabled)}}
			if !r.Enabled {
				row.tone = "dim"
			}
			t.rows = append(t.rows, row)
			t.ids = append(t.ids, strconv.Itoa(i))
		}
		cards = append(cards, card{title: "rules", items: []item{t}})
	} else {
		cards = append(cards, card{title: "rules", items: []item{empty{"no NAT rules configured"}}})
	}
	return append(cards, card{title: "note", items: []item{para{
		text: "\"add\" takes either a bare interface (masquerade shorthand) or source/dest/translate keywords — " +
			"see \"gravinet nat add -h\".", tone: "mut"}}})
}

func natTimeout(secs int) string {
	if secs == 0 {
		return "120s (default)"
	}
	return strconv.Itoa(secs) + "s"
}

// natClassTone colours the observed NAT class: a symmetric NAT is the one
// reading that predicts trouble (it is why sessions end up relayed), so it is
// worth being the colour that says so.
func natClassTone(class string) string {
	switch strings.ToLower(class) {
	case "":
		return "mut"
	case "symmetric":
		return "warn"
	case "open", "full-cone", "endpoint-independent":
		return "ok"
	}
	return ""
}

func pageQoS(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	var cards []card
	add := func(title string, q config.QoS) {
		items := []item{kv{rows: []kvRow{
			{"state", onOff(q.Enabled), enabledTone(q.Enabled)},
			{"classes", strconv.Itoa(q.Classes), ""},
			{"default class", strconv.Itoa(q.DefaultClass), ""},
		}}}
		if len(q.Rules) > 0 {
			t := table{selectKey: "qos", head: []string{"state", "proto", "ports", "services", "dscp", "class", "scope"}}
			for _, r := range q.Rules {
				dscp := "\u2014"
				if r.DSCP != nil {
					dscp = strconv.Itoa(*r.DSCP)
				}
				row := tableRow{cells: []string{
					onOff(!r.Disabled), dash(r.Protocol), portRange(r.PortMin, r.PortMax),
					joinOr(r.Services, "\u2014"), dscp, strconv.Itoa(r.Class), dash(r.Scope),
				}, cellTone: map[int]string{0: enabledTone(!r.Disabled)}}
				if r.Disabled {
					row.tone = "dim"
				}
				t.rows = append(t.rows, row)
				t.ids = append(t.ids, qosRuleID(r))
			}
			items = append(items, t)
		} else {
			items = append(items, empty{"no classification rules"})
		}
		cards = append(cards, card{title: title, items: items})
	}
	add("qos \u2014 node", s.cfg.QoS)
	for _, n := range s.cfg.Networks {
		if n.QoS.Enabled || len(n.QoS.Rules) > 0 {
			add("qos \u2014 "+n.Name, n.QoS)
		}
	}
	return cards
}

func pageIPv6RA(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	ra := s.cfg.RouterAdvert
	items := []item{kv{rows: []kvRow{{"state", onOff(ra.Enabled), enabledTone(ra.Enabled)}}}}
	if len(ra.Interfaces) > 0 {
		t := table{selectKey: "ipv6ra", head: []string{"iface", "state", "prefixes", "managed", "other", "lifetime", "pref", "dns", "search"}}
		for _, i := range ra.Interfaces {
			life := "default"
			if i.DefaultLifetime != 0 {
				life = strconv.Itoa(i.DefaultLifetime) + "s"
			}
			if i.NotDefault {
				life = "not a default router"
			}
			row := tableRow{cells: []string{
				i.Iface, onOff(!i.Disabled), joinOr(i.Prefixes, "\u2014"),
				yesNo(i.Managed), yesNo(i.OtherConfig), life, dash(i.Preference),
				joinOr(i.DNS, "\u2014"), joinOr(i.Search, "\u2014"),
			}, cellTone: map[int]string{1: enabledTone(!i.Disabled)}}
			if i.Disabled {
				row.tone = "dim"
			}
			t.rows = append(t.rows, row)
			t.ids = append(t.ids, i.Iface)
		}
		items = append(items, t)
	} else {
		items = append(items, empty{"no interfaces advertise router advertisements"})
	}
	return []card{
		{title: "ipv6 router advertisements", items: items},
		card{title: "note", items: []item{para{
			text: "Adding or editing a full entry (prefix, DNS, search list) needs the web admin's validation and " +
				"isn't reproduced here \u2014 the same boundary \"gravinet traffic ipv6ra\" itself draws.", tone: "mut"}}},
	}
}

func pageBandwidth(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	items := []item{editableKV{rows: []editableKVRow{
		{k: "feature", v: onOff(s.cfg.ShapingEnabled()), tone: enabledTone(s.cfg.ShapingEnabled()),
			edit: func(m *model) formSpec {
				return verbBoolForm("shaping (node-wide switch)", []string{"bandwidth"}, "on", "off", s.cfg.ShapingEnabled())
			}},
	}}}
	if len(s.cfg.Shaping) > 0 {
		t := table{selectKey: "bandwidth", head: []string{"iface", "kind", "state", "up", "down", "burst", "queue"}}
		for _, sh := range s.cfg.Shaping {
			th := s.cfg.ShapingThrottle(sh.Iface)
			row := tableRow{cells: []string{
				sh.Iface, s.cfg.ShapingKind(sh.Iface), onOff(th.Enabled),
				rateOrUnset(th.UpBytesPerSec), rateOrUnset(th.DownBytesPerSec),
				bytesOrUnset(th.BurstBytes), bytesOrUnset(th.QueueBytes),
			}, cellTone: map[int]string{2: enabledTone(th.Enabled)}}
			if !th.Enabled {
				row.tone = "dim"
			}
			t.rows = append(t.rows, row)
			t.ids = append(t.ids, sh.Iface)
		}
		items = append(items, t)
	} else {
		items = append(items, empty{"no interfaces are shaped"})
	}
	return []card{
		{title: "shaping", items: items},
	}
}

func rateOrUnset(bps int) string {
	if bps <= 0 {
		return "unlimited"
	}
	return formatRate(float64(bps))
}

func bytesOrUnset(n int) string {
	if n <= 0 {
		return "default"
	}
	return formatBytes(uint64(n))
}

func pageRoutes(c pageCtx) []card {
	s := c.snap
	var cards []card

	if s.cfg != nil {
		t := table{selectKey: "routes", head: []string{"network", "cidr", "metric", "state"}}
		for _, n := range s.cfg.Networks {
			for _, r := range n.Routes {
				row := tableRow{cells: []string{n.Name, r.CIDR, strconv.Itoa(r.Metric), onOff(r.Enabled)},
					cellTone: map[int]string{3: enabledTone(r.Enabled)}}
				if !r.Enabled {
					row.tone = "dim"
				}
				t.rows = append(t.rows, row)
				t.ids = append(t.ids, n.Name+idSep+r.CIDR)
			}
		}
		items := []item{}
		if len(t.rows) > 0 {
			items = append(items, t)
		} else {
			items = append(items, empty{"no routes are advertised from this node"})
		}
		cards = append(cards, card{title: "advertised (config)", items: items})
	} else {
		cards = append(cards, noConfig(s))
	}

	// The live half: what this node has actually learned from the mesh, which
	// is a different list from what it advertises and is the one that answers
	// "is the other end's route reaching me".
	if s.daemonUp() {
		t := table{head: []string{"network", "cidr", "via", "metric"}}
		for _, r := range s.routes {
			t.rows = append(t.rows, tableRow{cells: []string{r.net, r.CIDR, dash(r.Via), strconv.Itoa(r.Metric)}})
		}
		items := []item{}
		if len(t.rows) > 0 {
			items = append(items, t)
		} else {
			items = append(items, empty{"no routes learned from peers"})
		}
		cards = append(cards, card{title: "learned (live)", items: items})
	} else {
		cards = append(cards, card{title: "learned (live)", items: []item{
			para{text: "needs the daemon; not reachable", tone: "warn"}}})
	}
	return cards
}

func pageBGP(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	b := s.cfg.BGP
	items := []item{editableKV{rows: []editableKVRow{
		{k: "state", v: onOff(b.Enabled), tone: enabledTone(b.Enabled),
			edit: func(m *model) formSpec { return verbBoolForm("bgp", []string{"traffic", "bgp"}, "enable", "disable", b.Enabled) }},
		{k: "local AS", v: asnOrDash(b.ASN), edit: bgpSetForm(s)},
		{k: "router id", v: dash(b.RouterID), edit: bgpSetForm(s)},
		{k: "auto bgp", v: onOff(b.AutoBGP), tone: enabledTone(b.AutoBGP), edit: bgpSetForm(s)},
		{k: "networks", v: joinOr(b.Networks, "\u2014")},
		{k: "timers", v: fmt.Sprintf("keepalive %s, hold %s", secsOrDefault(b.KeepaliveTime), secsOrDefault(b.HoldTime)),
			edit: bgpSetForm(s)},
	}}}
	if len(b.Neighbors) > 0 {
		t := table{selectKey: "bgp-neighbors", head: []string{"peer", "remote AS", "state", "bfd", "description"}}
		for _, n := range b.Neighbors {
			st, tone := "configured", ""
			if n.Shutdown {
				st, tone = "shutdown", "dim"
			}
			t.rows = append(t.rows, tableRow{cells: []string{
				n.Peer, asnOrDash(n.RemoteAS), st, yesNo(n.BFD), dash(n.Description),
			}, tone: tone})
			t.ids = append(t.ids, n.Peer)
		}
		items = append(items, t)
	} else {
		items = append(items, empty{"no neighbors configured"})
	}
	return []card{
		{title: "bgp", items: items},
		card{title: "note", items: []item{para{
			text: "This is the configuration gravinet writes into FRR; live session state is under Monitor \u203a " +
				"BGP Peers. Advertised networks and the redistribute pickers need the web admin or " +
				"\"gravinet traffic bgp advertise\"/\"-h\" for now.", tone: "mut"}}},
	}
}

// bgpSetForm covers "gravinet traffic bgp set", the four fields that share
// one CLI verb (each flag left at its zero value means "leave unchanged" —
// cmdTrafficBGPSet's own convention, matched here by only passing a flag
// when its field actually differs from what the form opened with).
func bgpSetForm(s *snapshot) func(m *model) formSpec {
	return func(m *model) formSpec {
		b := s.cfg.BGP
		asn, routerID := strconv.FormatUint(uint64(b.ASN), 10), b.RouterID
		keepalive, hold := strconv.FormatUint(uint64(b.KeepaliveTime), 10), strconv.FormatUint(uint64(b.HoldTime), 10)
		autoBGP := onOffBool(b.AutoBGP)
		return formSpec{
			title: "bgp settings",
			fields: []formField{
				{key: "asn", label: "local AS", kind: fieldText, value: asn, help: "0 leaves it unchanged"},
				{key: "router_id", label: "router id", kind: fieldText, value: routerID},
				{key: "keepalive", label: "keepalive (seconds)", kind: fieldText, value: keepalive, help: "0 leaves it unchanged"},
				{key: "hold", label: "hold (seconds)", kind: fieldText, value: hold, help: "0 leaves it unchanged"},
				{key: "auto_bgp", label: "auto bgp", kind: fieldBool, value: autoBGP},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				args := []string{"traffic", "bgp", "set"}
				if v["asn"] != asn && v["asn"] != "0" && v["asn"] != "" {
					args = append(args, "-asn", v["asn"])
				}
				if v["router_id"] != routerID && v["router_id"] != "" {
					args = append(args, "-router-id", v["router_id"])
				}
				if v["keepalive"] != keepalive && v["keepalive"] != "0" && v["keepalive"] != "" {
					args = append(args, "-keepalive", v["keepalive"])
				}
				if v["hold"] != hold && v["hold"] != "0" && v["hold"] != "" {
					args = append(args, "-hold", v["hold"])
				}
				if v["auto_bgp"] != autoBGP {
					args = append(args, "-auto-bgp="+v["auto_bgp"])
				}
				if len(args) == 3 {
					return mutationResult{ok: true, detail: "nothing changed"}
				}
				return runLeaf(m.cliArgs(args...)...)
			},
		}
	}
}

func asnOrDash(n uint32) string {
	if n == 0 {
		return "\u2014"
	}
	return strconv.FormatUint(uint64(n), 10)
}

func secsOrDefault(n uint32) string {
	if n == 0 {
		return "default"
	}
	return strconv.FormatUint(uint64(n), 10) + "s"
}

// ---- naming -------------------------------------------------------------

func pageDNS(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	var cards []card
	for _, n := range s.cfg.Networks {
		items := []item{kv{rows: []kvRow{
			{"sync", onOff(n.DNSSync.Enabled), enabledTone(n.DNSSync.Enabled)},
			{"ttl", strconv.Itoa(n.DNSSync.TTLSeconds) + "s", ""},
			{"search domains", onOff(!n.DNSSync.DisableSearchDomains), ""},
		}}}
		if len(n.DNSAdvertise) > 0 {
			t := table{selectKey: "dns-fwd", head: []string{"domain", "servers", "state"}}
			for _, f := range n.DNSAdvertise {
				row := tableRow{cells: []string{f.Domain, joinOr(f.Servers, "\u2014"), onOff(!f.Disabled)},
					cellTone: map[int]string{2: enabledTone(!f.Disabled)}}
				if f.Disabled {
					row.tone = "dim"
				}
				t.rows = append(t.rows, row)
				t.ids = append(t.ids, n.Name+idSep+f.Domain)
			}
			items = append(items, t)
		} else {
			items = append(items, empty{"no domains advertised"})
		}
		if len(n.DNSReject) > 0 {
			rt := table{selectKey: "dns-reject", head: []string{"rejected domain", "state"}}
			for _, r := range n.DNSReject {
				row := tableRow{cells: []string{r.Domain, onOff(!r.Disabled)}, cellTone: map[int]string{1: enabledTone(!r.Disabled)}}
				if r.Disabled {
					row.tone = "dim"
				}
				rt.rows = append(rt.rows, row)
				rt.ids = append(rt.ids, n.Name+idSep+r.Domain)
			}
			items = append(items, rt)
		}
		cards = append(cards, card{title: "dns \u2014 " + n.Name, items: items})
	}
	if len(cards) == 0 {
		cards = append(cards, card{title: "dns", items: []item{empty{"no networks are configured"}}})
	}
	return append(cards, card{title: "note", items: []item{para{
		text: "Rejected domains (refused from peers) are added with \"gravinet naming dns reject -h\".", tone: "mut"}}})
}

func pageHosts(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	var cards []card
	for _, n := range s.cfg.Networks {
		items := []item{kv{rows: []kvRow{
			{"sync", onOff(n.HostsSync.Enabled), enabledTone(n.HostsSync.Enabled)},
			{"ttl", strconv.Itoa(n.HostsSync.TTLSeconds) + "s", ""},
			{"hosts file", dash(n.HostsSync.Path), "mut"},
		}}}
		if len(n.HostsAdvertise) > 0 {
			t := table{selectKey: "hosts", head: []string{"name", "address", "state"}}
			for _, h := range n.HostsAdvertise {
				row := tableRow{cells: []string{h.Name, h.IP, onOff(!h.Disabled)},
					cellTone: map[int]string{2: enabledTone(!h.Disabled)}}
				if h.Disabled {
					row.tone = "dim"
				}
				t.rows = append(t.rows, row)
				t.ids = append(t.ids, n.Name+idSep+h.Name)
			}
			items = append(items, t)
		} else {
			items = append(items, empty{"no records advertised from this node"})
		}
		cards = append(cards, card{title: "hosts \u2014 " + n.Name, items: items})
	}
	if len(cards) == 0 {
		cards = append(cards, card{title: "hosts", items: []item{empty{"no networks are configured"}}})
	}
	return append(cards, card{title: "note", items: []item{para{
		text: "This is what this node advertises; what actually landed in the host's file is under Monitor \u203a " +
			"Hosts File.", tone: "mut"}}})
}

// hostSnapshotItems renders a metrics reading. Shared by the Metrics page and
// nothing else today, but kept separate because it is the one piece of that
// page that is about formatting rather than about fetching.
func hostSnapshotItems(snap webadmin.HostSnapshot) []item {
	pct := func(v float64, ok bool) (string, string) {
		if !ok {
			return "n/a (not available on this platform)", "mut"
		}
		tone := ""
		switch {
		case v >= 90:
			tone = "danger"
		case v >= 75:
			tone = "warn"
		}
		return fmt.Sprintf("%.1f%%", v), tone
	}
	cpu, cpuT := pct(snap.CPUPercent, snap.CPUOK)
	mem, memT := pct(snap.MemPercent, snap.MemOK)
	disk, diskT := pct(snap.DiskPercent, snap.DiskOK)
	up := "n/a"
	if snap.UptimeOK {
		up = formatUptime(snap.UptimeSeconds)
	}
	items := []item{kv{rows: []kvRow{
		{"cpu", cpu, cpuT},
		{"memory", mem, memT},
		{"disk", disk, diskT},
		{"uptime", up, ""},
	}}}
	if len(snap.Ifaces) == 0 {
		return append(items, empty{"no overlay interfaces up"})
	}
	t := table{head: []string{"network", "iface", "rx", "tx"}}
	for _, i := range snap.Ifaces {
		t.rows = append(t.rows, tableRow{cells: []string{
			i.Network, i.Iface, formatRate(i.RxBytesPerSec), formatRate(i.TxBytesPerSec),
		}})
	}
	return append(items, t)
}
