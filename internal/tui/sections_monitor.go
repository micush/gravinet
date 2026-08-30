package tui

// The monitor, system, info and settings pages. Split from sections.go only
// for length; the contract is identical — snapshot in, []card out, no I/O in
// the builder itself (see lazy.go for how the slow reads are arranged).

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/service"
	"gravinet/internal/webadmin"
)

// ---- monitor ------------------------------------------------------------

func pageMetrics(c pageCtx) []card {
	s := c.snap
	v, err, ready := c.lazy.need("metrics", func() (any, error) {
		return hostSnapshot(s.ifaces), nil
	})
	if !ready {
		return []card{{title: "metrics", items: []item{
			reading("CPU, memory, disk and interface throughput"),
			para{text: "CPU and throughput are rates, so this takes about a second: they need two samples to mean anything.", tone: "mut"},
		}}}
	}
	if err != nil {
		return []card{{title: "metrics", items: []item{para{text: err.Error(), tone: "danger"}}}}
	}
	snap, _ := v.(webadmin.HostSnapshot)
	cards := []card{{title: "metrics", items: hostSnapshotItems(snap)}}
	if !s.daemonUp() {
		cards = append(cards, card{title: "note", items: []item{para{
			text: "host figures are read directly and are correct. The per-interface throughput table is empty " +
				"because the overlay interface list comes from the daemon, which is not reachable.", tone: "warn"}}})
	}
	cards = append(cards, card{title: "note", items: []item{para{
		text: "one instantaneous reading, not a graph. The web admin's version keeps a rolling history in the daemon; " +
			"press r to take another sample.", tone: "mut"}}})
	return cards
}

func pageMeshPeers(c pageCtx) []card {
	s := c.snap
	if !s.daemonUp() {
		return []card{noDaemon(s)}
	}
	if len(s.peers) == 0 {
		return []card{{title: "mesh peers", items: []item{empty{"no peers are currently connected"}}}}
	}

	// The same field set "gravinet monitor mesh-peers" prints, which is
	// itself the web admin's Mesh Peers page field for field, plus the wire
	// byte counters that page does not show.
	t := table{head: []string{"peer", "key", "session", "transport", "path", "pmtu", "frags tx/rx", "health", "tx", "rx"}}
	for _, p := range s.peers {
		path, pathTone := "direct", "ok"
		if p.Relayed {
			path, pathTone = "relay", "warn"
		}
		pmtu := "\u2014"
		if p.PathMTU > 0 {
			pmtu = strconv.Itoa(p.PathMTU)
		}
		health, healthTone := "clean", "ok"
		if p.FragSendDrop > 0 || p.ReasmDrop > 0 {
			health = fmt.Sprintf("drops send %d / reasm %d", p.FragSendDrop, p.ReasmDrop)
			healthTone = "danger"
		}
		t.rows = append(t.rows, tableRow{cells: []string{
			peerName(p.Hostname, p.NodeID), dash(p.KeyLabel), formatAge(p.EstablishedAt),
			dash(p.Transport), path, pmtu,
			fmt.Sprintf("%d/%d", p.FragsSent, p.FragsRcvd), health,
			formatBytes(p.TxBytes), formatBytes(p.RxBytes),
		}, cellTone: map[int]string{4: pathTone, 7: healthTone}})
	}
	cards := []card{{title: "mesh peers", items: []item{t}}}

	// Drop counters, separately, because they are the thing worth noticing
	// and they are lost in a wide table. Only rendered when there is
	// something to say: a table of zeroes on every page load trains people
	// to stop reading it.
	dt := table{head: []string{"peer", "spoof", "replay", "auth", "fw in", "police", "tun write", "egress q"}}
	for _, p := range s.peers {
		total := p.SpoofDrop + p.ReplayDrop + p.AuthDrop + p.FwInDrop + p.PoliceDrop + p.TunWriteDrop + p.EgressQDrop
		if total == 0 {
			continue
		}
		dt.rows = append(dt.rows, tableRow{cells: []string{
			peerName(p.Hostname, p.NodeID),
			strconv.FormatUint(p.SpoofDrop, 10), strconv.FormatUint(p.ReplayDrop, 10),
			strconv.FormatUint(p.AuthDrop, 10), strconv.FormatUint(p.FwInDrop, 10),
			strconv.FormatUint(p.PoliceDrop, 10), strconv.FormatUint(p.TunWriteDrop, 10),
			strconv.FormatUint(p.EgressQDrop, 10),
		}, tone: "warn"})
	}
	if len(dt.rows) > 0 {
		cards = append(cards, card{title: "drops", items: []item{
			para{text: "peers with a nonzero drop counter. A climbing count localizes loss to one peer's underlay path.", tone: "mut"},
			dt,
		}})
	}
	return cards
}

// pageCapture is the terminal counterpart of the CLI's own decision here.
// "gravinet monitor capture" runs tcpdump rather than reimplementing a
// capture pipeline, for the reasons set out at length in cli_groups.go; this
// page says the same thing rather than pretending to a live packet view it
// would have to build a second pcap encoder to back.
func pageCapture(c pageCtx) []card {
	s := c.snap
	items := []item{
		para{text: "Live capture is not drawn here. On a terminal the tool for this already exists, is installed " +
			"nearly everywhere, and does filtering, -w output and rotation better than a reimplementation would — " +
			"which is the same call \"gravinet monitor capture\" makes, and it runs tcpdump for you.", tone: "mut"},
	}
	if len(s.ifaces) > 0 {
		t := table{head: []string{"network", "iface", "command"}}
		for _, i := range s.ifaces {
			t.rows = append(t.rows, tableRow{cells: []string{
				i.Name, i.Iface, "gravinet monitor capture " + i.Iface,
			}})
		}
		items = append(items, t)
	} else if !s.daemonUp() {
		items = append(items, para{text: "the overlay interface list needs the daemon, which is not reachable", tone: "warn"})
	} else {
		items = append(items, empty{"no overlay interfaces are up"})
	}
	return []card{{title: "packet capture", items: items}}
}

// pageSpeedtest mirrors the one gap the CLI also has, and for the same
// reason — see speedtestNotYetInCLI in cli_groups.go. Saying so is better
// than a page that looks like it should work.
func pageSpeedtest(c pageCtx) []card {
	return []card{{title: "speedtest", items: []item{
		para{text: "Not available outside the web admin. Unlike everything else under Monitor this is not a local " +
			"read: it coordinates an active throughput test between two live peers over the mesh, which only the " +
			"running daemon can start. Doing it from here needs new asynchronous control-socket protocol — a " +
			"start-job/poll-status shape rather than the single request/response everything else here uses — so it " +
			"is absent for the same reason it is absent from the CLI, rather than being drawn as a button that " +
			"does nothing.", tone: "mut"},
	}}}
}

func pageLatency(c pageCtx) []card {
	s := c.snap
	if !s.daemonUp() {
		return []card{noDaemon(s)}
	}
	if len(s.peers) == 0 {
		return []card{{title: "latency", items: []item{empty{"no peers to measure"}}}}
	}
	// The RTT the daemon already measures on its own keepalive, rather than a
	// fresh ICMP sweep. It is the same number the web admin's latency column
	// shows, it costs nothing, and it is measured over the tunnel — which is
	// the path being asked about, unlike a ping to the peer's underlay
	// address.
	t := table{head: []string{"peer", "overlay4", "rtt", "path"}}
	for _, p := range s.peers {
		rtt, tone := "\u2014", "mut"
		if p.RTTMs > 0 {
			rtt = fmt.Sprintf("%.1f ms", p.RTTMs)
			switch {
			case p.RTTMs >= 250:
				tone = "danger"
			case p.RTTMs >= 100:
				tone = "warn"
			default:
				tone = "ok"
			}
		}
		path := "direct"
		if p.Relayed {
			path = "relay " + dash(p.RelayVia)
		}
		t.rows = append(t.rows, tableRow{cells: []string{
			peerName(p.Hostname, p.NodeID), dash(p.Overlay4), rtt, path,
		}, cellTone: map[int]string{2: tone}})
	}
	return []card{
		{title: "latency", items: []item{t}},
		card{title: "note", items: []item{para{
			text: "round-trip time over the tunnel, from the daemon's own keepalive — not a fresh ICMP sweep. " +
				"\"gravinet latency\" reports the same figures.", tone: "mut"}}},
	}
}

func pageRouteTable(c pageCtx) []card {
	lines, err, ready := lazyLines(c.lazy, "route-table", readRouteTable)
	return monoPage("route table", "the kernel routing table", lines, err, ready)
}

func pageBGPPeers(c pageCtx) []card {
	lines, err, ready := lazyLines(c.lazy, "bgp-peers", readBGPPeers)
	cards := monoPage("bgp peers", "BGP session state from FRR", lines, err, ready)
	return append(cards, card{title: "note", items: []item{para{
		text: "read-only, straight from vtysh. The editor is under Traffic \u203a BGP.", tone: "mut"}}})
}

func pageL2Peers(c pageCtx) []card {
	v, err, ready := c.lazy.need("l2-peers", func() (any, error) { return readL2Peers() })
	if !ready {
		return []card{{title: "l2 peers", items: []item{reading("LLDP/CDP neighbors")}}}
	}
	if err != nil {
		return []card{{title: "l2 peers", items: []item{para{text: err.Error(), tone: "danger"}}}}
	}
	ns, _ := v.([]service.LLDPNeighbor)
	if len(ns) == 0 {
		return []card{{title: "l2 peers", items: []item{empty{"no neighbors seen"}}}}
	}
	t := table{head: []string{"local iface", "neighbor", "port", "protocol", "mgmt ip"}}
	for _, n := range ns {
		t.rows = append(t.rows, tableRow{cells: l2Cells(n)})
	}
	return []card{
		{title: "l2 peers", items: []item{t}},
		card{title: "note", items: []item{para{
			text: "read-only. Which interfaces run LLDP/CDP is under System \u203a LLDP.", tone: "mut"}}},
	}
}

func pageHostsFile(c pageCtx) []card {
	lines, err, ready := lazyLines(c.lazy, "hosts-file", readHostsFile)
	cards := monoPage("hosts file", "this host's hosts file", lines, err, ready)
	return append(cards, card{title: "note", items: []item{para{
		text: "the live file, including whatever gravinet has written into it. What this node advertises is under " +
			"Naming \u203a Hosts.", tone: "mut"}}})
}

func pageDNSState(c pageCtx) []card {
	s := c.snap
	if !s.daemonUp() {
		return []card{noDaemon(s)}
	}
	v, _, ready := c.lazy.need("dns-state", func() (any, error) { return readDNSState(s), nil })
	if !ready {
		return []card{{title: "dns state", items: []item{reading("what is registered with the OS resolver")}}}
	}
	entries, _ := v.([]dnsStateEntry)
	if len(entries) == 0 {
		return []card{{title: "dns state", items: []item{empty{"no networks are up"}}}}
	}
	var cards []card
	for _, e := range entries {
		items := []item{}
		switch {
		case e.err != nil:
			items = append(items, para{text: e.err.Error(), tone: "danger"})
		case len(e.lines) == 0:
			items = append(items, empty{"nothing registered"})
		default:
			items = append(items, mono{lines: e.lines})
		}
		cards = append(cards, card{title: fmt.Sprintf("%s (%s)", e.network, e.iface), items: items})
	}
	return append(cards, card{title: "note", items: []item{para{
		text: "read live from the OS resolver, not from what gravinet remembers applying — so this reflects reality " +
			"even if a sync failed quietly.", tone: "mut"}}})
}

func pageLogs(c pageCtx) []card {
	s := c.snap
	type logResult struct {
		lines []string
		path  string
	}
	v, err, ready := c.lazy.need("logs", func() (any, error) {
		lines, path, err := readLogTail(s.cfg, s.cfgPath, 500)
		return logResult{lines, path}, err
	})
	if !ready {
		return []card{{title: "logs", items: []item{reading("the daemon's log")}}}
	}
	res, _ := v.(logResult)
	if err != nil {
		items := []item{para{text: err.Error(), tone: "danger"}}
		if res.path != "" {
			items = append(items, kv{rows: []kvRow{{"path", res.path, "mut"}}})
		}
		return []card{{title: "logs", items: items}}
	}
	return []card{
		{title: "logs \u2014 " + res.path, items: logItems(res.lines)},
		card{title: "note", items: []item{para{
			text: "the last 500 lines. Press r to re-read, or use \"gravinet monitor logs -n N\" for more.", tone: "mut"}}},
	}
}

// logItems renders log lines, coloured by level the way the web admin's Logs
// page colours them: errors red, warnings amber, everything else plain. A
// mono item carries one tone for the whole block, so this splits the tail
// into runs of consecutive same-level lines — which layout draws back to back
// with no separator, so the result reads as one block.
//
// Level detection is a substring match on the bracketed level logx writes,
// deliberately narrow: a line whose *message* happens to contain the word
// error is not an error line, and colouring it red is how a log page ends up
// looking alarming when nothing is wrong.
func logItems(lines []string) []item {
	var out []item
	var run []string
	runTone := ""
	flush := func() {
		if len(run) > 0 {
			out = append(out, mono{lines: run, tone: runTone})
			run = nil
		}
	}
	for _, l := range lines {
		tone := logTone(l)
		if tone != runTone {
			flush()
			runTone = tone
		}
		run = append(run, l)
	}
	flush()
	if len(out) == 0 {
		out = append(out, empty{"(empty)"})
	}
	return out
}

// logTone classifies one log line. Matches the level tags logx emits.
func logTone(l string) string {
	switch {
	case strings.Contains(l, "[error]") || strings.Contains(l, "ERROR"):
		return "danger"
	case strings.Contains(l, "[warn]") || strings.Contains(l, "WARN"):
		return "warn"
	}
	return ""
}

// monoPage is the shape shared by every page that is "some text, read from
// the host": route table, BGP summary, hosts file, and the four documents.
func monoPage(title, what string, lines []string, err error, ready bool) []card {
	if !ready {
		return []card{{title: title, items: []item{reading(what)}}}
	}
	if err != nil {
		return []card{{title: title, items: []item{para{text: err.Error(), tone: "danger"}}}}
	}
	if len(lines) == 0 {
		return []card{{title: title, items: []item{empty{"(empty)"}}}}
	}
	return []card{{title: title, items: []item{mono{lines: lines}}}}
}

// l2Cells extracts the columns from an LLDP neighbor. Split out because the
// struct's field names are not obviously column names and the mapping is
// worth being able to read in one place.
func l2Cells(n service.LLDPNeighbor) []string {
	return []string{dash(n.LocalIface), dash(n.SystemName), dash(n.Port), dash(n.Protocol), dash(n.MgmtIP)}
}

// ---- system -------------------------------------------------------------

func pageUpgrade(c pageCtx) []card {
	s := c.snap
	items := []item{kv{rows: []kvRow{
		{"running", s.version + " (" + s.commit + ")", ""},
		{"platform", runtime.GOOS + "/" + runtime.GOARCH, ""},
	}}}
	if s.cfg != nil {
		items = append(items, editableKV{rows: []editableKVRow{
			{k: "state dir", v: dash(s.cfg.UpgradeStateDir()), tone: "mut"},
			{k: "confirm window", v: strconv.Itoa(s.cfg.UpgradeConfirmSeconds()) + "s"},
			{k: "accept pushes from a manager", v: onOff(s.cfg.Upgrade.AcceptManagerUpgrades),
				tone: enabledTone(s.cfg.Upgrade.AcceptManagerUpgrades),
				edit: func(m *model) formSpec {
					return boolSettingForm("accept manager upgrades", "accept-mgr-upgrades", s.cfg.Upgrade.AcceptManagerUpgrades)
				}},
			{k: "rollback to the previous binary", v: "\u2014", edit: func(m *model) formSpec { return upgradeRollbackForm() }},
			{k: "clear saved rollback state", v: "\u2014", edit: func(m *model) formSpec { return upgradeClearForm() }},
		}})
	}
	return []card{
		{title: "upgrade", items: items},
		card{title: "note", items: []item{para{
			text: "Checking for and applying a new binary is not wired to a key here: it runs a full build that " +
				"can take minutes, and this console's mutations are built around commands that answer in seconds — " +
				"forcing that mismatch under time pressure risked corrupting an in-progress binary swap, which is a " +
				"worse outcome than leaving it out. \"gravinet upgrade ARCHIVE.tgz\" and \"gravinet upgrade status\" " +
				"are the same operations the web admin's buttons call.", tone: "mut"}}},
	}
}

func upgradeRollbackForm() formSpec {
	return formSpec{
		title:  "rollback to the previous binary",
		fields: []formField{{key: "confirm", label: "roll back now", kind: fieldBool, value: "true"}},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["confirm"] != "true" {
				return mutationResult{ok: true, detail: "nothing changed"}
			}
			return runLeaf(m.cliArgsSock("upgrade", "rollback")...)
		},
	}
}

func upgradeClearForm() formSpec {
	return formSpec{
		title:  "clear saved rollback state",
		fields: []formField{{key: "confirm", label: "clear it", kind: fieldBool, value: "true"}},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["confirm"] != "true" {
				return mutationResult{ok: true, detail: "nothing changed"}
			}
			return runLeaf(m.cliArgsSock("upgrade", "clear")...)
		},
	}
}

func pageInterfaces(c pageCtx) []card {
	v, err, ready := c.lazy.need("interfaces", func() (any, error) { return readInterfaces() })
	if !ready {
		return []card{{title: "interfaces", items: []item{reading("this host's interfaces")}}}
	}
	if err != nil {
		return []card{{title: "interfaces", items: []item{para{text: err.Error(), tone: "danger"}}}}
	}
	rows, _ := v.([]ifaceRow)
	t := table{head: []string{"iface", "state", "mtu", "mac", "addresses"}}
	for _, r := range rows {
		tone := ""
		switch r.state {
		case "down":
			tone = "dim"
		case "up, no carrier":
			tone = "warn"
		}
		t.rows = append(t.rows, tableRow{cells: []string{
			r.name, r.state, strconv.Itoa(r.mtu), dash(r.mac), joinOr(r.addrs, "(none)"),
		}, tone: tone})
	}
	return []card{
		{title: "interfaces", items: []item{t}},
		card{title: "note", items: []item{para{
			text: "read from the host directly, so this works when the daemon does not — which is when an interface " +
				"list is most worth having. Read-only here and in the CLI; addressing is edited in the web admin.",
			tone: "mut"}}},
	}
}

func pageResolver(c pageCtx) []card {
	v, _, ready := c.lazy.need("resolver", func() (any, error) { return service.HostResolver(), nil })
	if !ready {
		return []card{{title: "resolver", items: []item{reading("hostname and DNS servers")}}}
	}
	info, _ := v.(service.ResolverInfo)
	live := editableKV{rows: []editableKVRow{
		{k: "hostname", v: dash(info.Hostname), edit: func(m *model) formSpec { return resolverHostnameForm(info.Hostname) }},
		{k: "dns servers", v: joinOr(info.DNSServers, "\u2014"), edit: func(m *model) formSpec { return resolverDNSForm(info) }},
		{k: "search domain", v: dash(info.SearchDomain), edit: func(m *model) formSpec { return resolverDNSForm(info) }},
	}}
	cards := []card{{title: "resolver (live)", items: []item{live}}}
	if s := c.snap; s.cfg != nil && s.cfg.HostSettings != nil && s.cfg.HostSettings.Resolver != nil {
		r := s.cfg.HostSettings.Resolver
		cards = append(cards, card{title: "resolver (configured)", items: []item{kv{rows: []kvRow{
			{"hostname", dash(r.Hostname), "mut"},
			{"dns servers", joinOr(r.DNSServers, "\u2014"), "mut"},
			{"search domain", dash(r.SearchDomain), "mut"},
		}}}})
	}
	return append(cards, card{title: "note", items: []item{para{
		text: "changing the hostname needs a gravinet restart before mesh peers see the new name: the advertised " +
			"name is read once at startup.", tone: "mut"}}})
}

func resolverHostnameForm(current string) formSpec {
	return formSpec{
		title:  "hostname",
		fields: []formField{{key: "v", label: "hostname", kind: fieldText, value: current}},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["v"] == "" {
				return mutationResult{ok: false, detail: "a hostname is required"}
			}
			res := runLeaf(m.cliArgsBare("system", "resolver", "hostname", v["v"])...)
			if res.ok {
				res.detail += "\nrestart gravinet for mesh peers to see the new name"
			}
			return res
		},
	}
}

func resolverDNSForm(info service.ResolverInfo) formSpec {
	servers := joinOr(info.DNSServers, "")
	return formSpec{
		title: "dns servers",
		fields: []formField{
			{key: "servers", label: "servers", kind: fieldText, value: servers, help: "comma-separated"},
			{key: "search", label: "search domain", kind: fieldText, value: info.SearchDomain},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["servers"] == "" {
				return mutationResult{ok: false, detail: "at least one server is required"}
			}
			args := []string{"system", "resolver", "dns"}
			for _, s := range strings.Split(v["servers"], ",") {
				if s = strings.TrimSpace(s); s != "" {
					args = append(args, s)
				}
			}
			if v["search"] != "" {
				args = append(args, "-search", v["search"])
			}
			return runLeaf(m.cliArgsBare(args...)...)
		},
	}
}

func pageTime(c pageCtx) []card {
	v, _, ready := c.lazy.need("time", func() (any, error) { return service.HostTime(), nil })
	if !ready {
		return []card{{title: "time", items: []item{reading("clock, timezone and NTP state")}}}
	}
	info, _ := v.(service.TimeInfo)
	rows := timeRows(info)
	erows := make([]editableKVRow, len(rows))
	for i, r := range rows {
		erows[i] = editableKVRow{k: r.k, v: r.v, tone: r.tone}
	}
	for i := range erows {
		switch erows[i].k {
		case "timezone":
			erows[i].edit = func(m *model) formSpec { return timeTimezoneForm(info) }
		case "ntp":
			erows[i].edit = func(m *model) formSpec { return timeNTPForm(info) }
		case "clock":
			erows[i].edit = func(m *model) formSpec { return timeClockForm() }
		}
	}
	return []card{
		{title: "time", items: []item{editableKV{rows: erows}}},
		card{title: "note", items: []item{para{
			text: "setting the clock by hand only matters while NTP is off — it corrects itself right back the " +
				"next sync otherwise.", tone: "mut"}}},
	}
}

func timeTimezoneForm(info service.TimeInfo) formSpec {
	return formSpec{
		title:  "timezone",
		fields: []formField{{key: "v", label: "timezone", kind: fieldText, value: info.Timezone, help: "e.g. America/Phoenix"}},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["v"] == "" {
				return mutationResult{ok: false, detail: "a timezone is required"}
			}
			return runLeaf(m.cliArgsBare("system", "time", "timezone", v["v"])...)
		},
	}
}

func timeNTPForm(info service.TimeInfo) formSpec {
	return formSpec{
		title: "ntp",
		fields: []formField{
			{key: "on", label: "on", kind: fieldBool, value: onOffBool(info.NTPEnabled)},
			{key: "servers", label: "servers", kind: fieldText, value: joinOr(info.Servers, ""),
				help: "comma-separated; only used when turning NTP on"},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["on"] != "true" {
				return runLeaf(m.cliArgsBare("system", "time", "ntp", "off")...)
			}
			args := []string{"system", "time", "ntp", "on"}
			for _, s := range strings.Split(v["servers"], ",") {
				if s = strings.TrimSpace(s); s != "" {
					args = append(args, s)
				}
			}
			return runLeaf(m.cliArgsBare(args...)...)
		},
	}
}

func timeClockForm() formSpec {
	return formSpec{
		title:  "set clock",
		fields: []formField{{key: "v", label: "clock (RFC3339)", kind: fieldText, help: "e.g. 2026-08-02T15:04:05 \u2014 only takes effect while NTP is off"}},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["v"] == "" {
				return mutationResult{ok: false, detail: "a timestamp is required"}
			}
			return runLeaf(m.cliArgsBare("system", "time", "clock", v["v"])...)
		},
	}
}

func pageDHCP(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	d := s.cfg.DHCP
	mode := string(d.Mode)
	if mode == "" {
		mode = "off"
	}
	items := []item{editableKV{rows: []editableKVRow{
		{k: "mode (configured)", v: mode, tone: enabledTone(mode != "off"), edit: dhcpModeForm},
	}}}
	if len(d.Relay.Links) > 0 {
		t := table{head: []string{"iface", "state", "servers", "max hops"}}
		for _, l := range d.Relay.Links {
			hops := "default"
			if l.MaxHops > 0 {
				hops = strconv.Itoa(l.MaxHops)
			}
			row := tableRow{cells: []string{l.Iface, onOff(!l.Disabled), joinOr(l.Servers, "\u2014"), hops},
				cellTone: map[int]string{1: enabledTone(!l.Disabled)}}
			if l.Disabled {
				row.tone = "dim"
			}
			t.rows = append(t.rows, row)
		}
		items = append(items, t)
	} else {
		items = append(items, empty{"no relay links configured"})
	}
	cards := []card{{title: "dhcp relay", items: items}}
	if d.RetiredServerMode() {
		cards = append(cards, card{title: "warning", items: []item{para{
			text: "this config still has this node serving DHCP through Kea, a role removed in v988. gravinet has " +
				"not touched the Kea service — if it was running it still is, still enabled at boot, and serving a " +
				"config nothing manages now. The served subnets are in this node's config history if they are worth " +
				"recreating elsewhere.", tone: "warn"}}})
	}
	return append(cards,
		card{title: "note", items: []item{para{
			text: "this is intent, not what is running: the relay lives inside the daemon process, so a separate " +
				"reader cannot see its sockets and guessing would put a confident wrong answer on the screen. Relay " +
				"links themselves need the web admin's validation and aren't editable here — the same boundary " +
				"\"gravinet system dhcp\" itself draws.",
			tone: "mut"}}})
}

func dhcpModeForm(m *model) formSpec {
	current := "off"
	if m.snap != nil && m.snap.cfg != nil && string(m.snap.cfg.DHCP.Mode) != "" {
		current = string(m.snap.cfg.DHCP.Mode)
	}
	return formSpec{
		title:  "dhcp relay mode",
		fields: []formField{{key: "v", label: "mode", kind: fieldSelect, value: current, options: []string{"off", "relay"}}},
		submit: func(m *model, v map[string]string) mutationResult {
			return runLeaf(m.cliArgs("system", "dhcp", "mode", v["v"])...)
		},
	}
}

func pageSNMP(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	sn := s.cfg.SNMP
	items := []item{editableKV{rows: []editableKVRow{
		{k: "state", v: onOff(sn.Enabled), tone: enabledTone(sn.Enabled),
			edit: func(m *model) formSpec { return verbBoolForm("snmp", []string{"system", "snmp"}, "on", "off", sn.Enabled) }},
		{k: "listen", v: dash(sn.ListenAddr), edit: func(m *model) formSpec { return snmpListenForm(sn) }},
		{k: "location", v: dash(sn.Location), edit: func(m *model) formSpec { return snmpLocationForm(sn) }},
		{k: "contact", v: dash(sn.Contact), edit: func(m *model) formSpec { return snmpContactForm(sn) }},
		{k: "runnable", v: yesNo(sn.IsRunnable()), tone: enabledTone(sn.IsRunnable())},
	}}}
	if len(sn.Communities) > 0 {
		// The community string is a credential and is never displayed, for
		// the same reason key material is not printed on the Keys page:
		// this screen ends up in a scrollback buffer. The real value still
		// has to be the row's identity (cmdSystemSNMP's community del takes
		// the string itself, there is no separate name), so it lives only
		// in the row's id, which is never rendered — only cells are drawn.
		t := table{selectKey: "snmp-communities", head: []string{"community", "state"}}
		for _, cm := range sn.Communities {
			row := tableRow{cells: []string{"(set)", onOff(!cm.Disabled)}, cellTone: map[int]string{1: enabledTone(!cm.Disabled)}}
			if cm.Disabled {
				row.tone = "dim"
			}
			t.rows = append(t.rows, row)
			t.ids = append(t.ids, cm.Community)
		}
		items = append(items, t)
	}
	return []card{
		{title: "snmp", items: items},
		card{title: "note", items: []item{para{
			text: "community strings are credentials and are not printed here. a add  d delete, on the selected " +
				"community. There is no individual enable/disable for one \u2014 remove it and add it back.",
			tone: "mut"}}},
	}
}

func snmpListenForm(sn config.SNMPConfig) formSpec {
	return formSpec{
		title:  "snmp listen address",
		fields: []formField{{key: "v", label: "listen", kind: fieldText, value: sn.ListenAddr, help: "ADDR:PORT"}},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["v"] == "" {
				return mutationResult{ok: false, detail: "a listen address is required"}
			}
			return runLeaf(m.cliArgs("system", "snmp", "listen", v["v"])...)
		},
	}
}

func snmpLocationForm(sn config.SNMPConfig) formSpec {
	return formSpec{
		title:  "snmp location",
		fields: []formField{{key: "v", label: "location", kind: fieldText, value: sn.Location}},
		submit: func(m *model, v map[string]string) mutationResult {
			return runLeaf(m.cliArgs("system", "snmp", "location", v["v"])...)
		},
	}
}

func snmpContactForm(sn config.SNMPConfig) formSpec {
	return formSpec{
		title:  "snmp contact",
		fields: []formField{{key: "v", label: "contact", kind: fieldText, value: sn.Contact}},
		submit: func(m *model, v map[string]string) mutationResult {
			return runLeaf(m.cliArgs("system", "snmp", "contact", v["v"])...)
		},
	}
}

func pageLLDP(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	d := s.cfg.Discovery
	items := []item{editableKV{rows: []editableKVRow{
		{k: "state", v: onOff(!d.Disabled), tone: enabledTone(!d.Disabled),
			edit: func(m *model) formSpec { return verbBoolForm("lldp", []string{"system", "lldp"}, "on", "off", !d.Disabled) }},
		{k: "runnable", v: yesNo(d.IsRunnable()), tone: enabledTone(d.IsRunnable())},
		{k: "any cdp", v: yesNo(d.AnyCDP())},
	}}}
	if len(d.Interfaces) > 0 {
		t := table{selectKey: "lldp-iface", head: []string{"iface", "lldp", "cdp"}}
		for _, i := range d.Interfaces {
			t.rows = append(t.rows, tableRow{cells: []string{i.Name, onOff(i.LLDP), onOff(i.CDP)},
				cellTone: map[int]string{1: enabledTone(i.LLDP), 2: enabledTone(i.CDP)}})
			t.ids = append(t.ids, i.Name)
		}
		items = append(items, t)
	} else {
		items = append(items, empty{"no interfaces run discovery"})
	}
	return []card{
		{title: "lldp", items: items},
		card{title: "note", items: []item{para{
			text: "a add an interface  d remove. Adding turns on both LLDP and CDP together \u2014 there is no CLI verb " +
				"for one without the other on an existing entry, remove and re-add to change that. The neighbors " +
				"found are under Monitor \u203a L2 Peers.", tone: "mut"}}},
	}
}

func pageSyslog(c pageCtx) []card {
	v, _, ready := c.lazy.need("syslog", func() (any, error) { return service.HostSyslog(), nil })
	if !ready {
		return []card{{title: "syslog", items: []item{reading("syslog forwarding state")}}}
	}
	info, _ := v.(service.SyslogInfo)
	items := []item{}
	if t := syslogTable(info); t != nil {
		items = append(items, t)
	} else {
		items = append(items, empty{"no remote targets configured"})
	}
	return []card{
		{title: "syslog", items: items},
		card{title: "note", items: []item{para{
			text: "a add  d remove, on the selected collector. There is no individual enable/disable \u2014 remove " +
				"it and add it back.", tone: "mut"}}},
	}
}

func pageUsers(c pageCtx) []card {
	s := c.snap
	var items []item
	if s.cfg != nil {
		wa := s.cfg.WebAdmin
		items = append(items, kv{rows: []kvRow{
			{"auth mode", dash(wa.AuthMode), ""},
			{"pam service", dash(wa.PAMService), "mut"},
			{"allowed OS users", joinOr(wa.AllowUsers, "\u2014"), ""},
			{"local credentials", strconv.Itoa(len(wa.Users)), ""},
		}})
	} else {
		items = append(items, para{text: fmt.Sprintf("could not read %s: %v", s.cfgPath, s.cfgErr), tone: "danger"})
	}

	// The live OS group membership — what "gravinet system users" actually
	// lists — not HostSettings.Users, a config-side cache that can lag
	// behind an account added or removed some other way.
	v, err, ready := c.lazy.need("sys-users", func() (any, error) { return service.ListSystemUsers(), nil })
	switch {
	case !ready:
		items = append(items, reading("console users"))
	case err != nil:
		items = append(items, para{text: err.Error(), tone: "danger"})
	default:
		info, _ := v.(service.UsersInfo)
		if len(info.Users) > 0 {
			t := table{selectKey: "sys-users", head: []string{"user", "exists", "expires"}}
			for _, u := range info.Users {
				exp := "never"
				switch {
				case !u.ExpiryKnown:
					exp = "unknown"
				case !u.Expires.IsZero():
					exp = u.Expires.Format("2006-01-02")
				}
				if u.Expired {
					exp += " (expired)"
				}
				tone := ""
				if !u.Exists {
					tone = "warn"
				}
				t.rows = append(t.rows, tableRow{cells: []string{u.Name, yesNo(u.Exists), exp}, tone: tone})
				t.ids = append(t.ids, u.Name)
			}
			items = append(items, t)
		} else {
			items = append(items, empty{"no console users"})
		}
		if !info.CanManage && info.ManageHint != "" {
			items = append(items, para{text: info.ManageHint, tone: "warn"})
		}
	}
	return []card{
		{title: "users", items: items},
		card{title: "note", items: []item{para{
			text: "a add  e set expiry  d delete, on the selected user. Credentials are never printed \u2014 " +
				"\"gravinet genpass\" produces a new one, and a password is always asked for here, never blank.",
			tone: "mut"}}},
	}
}

func pageConfigHistory(c pageCtx) []card {
	s := c.snap
	v, err, ready := c.lazy.need("config-history", func() (any, error) {
		return config.List(s.cfgPath)
	})
	if !ready {
		return []card{{title: "config history", items: []item{reading("config snapshots")}}}
	}
	if err != nil {
		return []card{{title: "config history", items: []item{para{text: err.Error(), tone: "danger"}}}}
	}
	entries, _ := v.([]config.SnapshotMeta)
	limit := 0
	if s.cfg != nil {
		limit = s.cfg.EffectiveConfigHistoryLimit()
	}
	items := []item{kv{rows: []kvRow{
		{"snapshots", strconv.Itoa(len(entries)), ""},
		{"limit", strconv.Itoa(limit), ""},
	}}}
	if len(entries) > 0 {
		t := table{selectKey: "config-history", head: []string{"id", "when", "by", "summary"}}
		for _, e := range entries {
			t.rows = append(t.rows, tableRow{cells: []string{
				e.ID, dash(e.Stamp), dash(e.User), dash(e.Summary),
			}})
			t.ids = append(t.ids, e.ID)
		}
		items = append(items, t)
	} else {
		items = append(items, empty{"no snapshots"})
	}
	return []card{
		{title: "config history", items: items},
		card{title: "note", items: []item{para{
			text: "a snapshot now  e view diff against the config on disk  d restore (overwrites the current " +
				"config), on the selected snapshot.", tone: "mut"}}},
	}
}

func pagePower(c pageCtx) []card {
	v, _, ready := c.lazy.need("power", func() (any, error) {
		ok, note := service.HostPowerSupported()
		return [2]any{ok, note}, nil
	})
	items := []item{}
	if ready {
		pair, _ := v.([2]any)
		ok, _ := pair[0].(bool)
		note, _ := pair[1].(string)
		items = append(items, editableKV{rows: []editableKVRow{
			{k: "supported on this host", v: yesNo(ok), tone: enabledTone(ok)},
			{k: "restart host", v: "\u2014", edit: func(m *model) formSpec { return powerForm("reboot", "restart") }},
			{k: "shutdown host", v: "\u2014", edit: func(m *model) formSpec { return powerForm("shutdown", "shut down") }},
			{k: "cancel pending action", v: "\u2014", edit: func(m *model) formSpec { return powerCancelForm() }},
		}})
		if note != "" {
			items = append(items, para{text: note, tone: "mut"})
		}
	} else {
		items = append(items, reading("power support"))
	}
	items = append(items, para{
		text: "This takes the whole HOST down, not just the gravinet service — and this console is frequently " +
			"the only way back into the machine. Each action here still asks for confirmation first, the same as " +
			"\"gravinet system power\" does, but there is no undo once it runs; a delay gives a window to reach " +
			"the host another way if this was a mistake.", tone: "warn"})
	return []card{{title: "power", items: items}}
}

func powerForm(verb, label string) formSpec {
	return formSpec{
		title: label + " host",
		fields: []formField{
			{key: "delay", label: "delay (minutes)", kind: fieldText, value: "1",
				help: "a window to reach the host another way if this was a mistake"},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			delay := strings.TrimSpace(v["delay"])
			if delay == "" {
				delay = "0"
			}
			return runLeaf(m.cliArgsBare("system", "power", verb, "-delay", delay)...)
		},
	}
}

func powerCancelForm() formSpec {
	return formSpec{
		title:  "cancel pending restart/shutdown",
		fields: []formField{{key: "confirm", label: "cancel it", kind: fieldBool, value: "true"}},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["confirm"] != "true" {
				return mutationResult{ok: true, detail: "nothing changed"}
			}
			return runLeaf(m.cliArgsBare("system", "power", "cancel")...)
		},
	}
}

// ---- info ---------------------------------------------------------------

func pageReadme(c pageCtx) []card {
	return docPage(c, "readme", "the README", (*config.Config).ReadmePath)
}

func pageGettingStarted(c pageCtx) []card {
	return docPage(c, "getting-started", "the walkthrough", (*config.Config).GettingStartedPath)
}

func pageAPIDoc(c pageCtx) []card {
	return docPage(c, "api", "the API reference", (*config.Config).APIDocPath)
}

func pageLicense(c pageCtx) []card {
	return docPage(c, "license", "the license", (*config.Config).LicensePath)
}

// docPage renders one of the Info documents. Markdown, unrendered — the same
// call "gravinet info readme" makes, and for the same reason it gives: on a
// terminal the raw source is the more useful form, and this one is scrollable
// and searchable, which is most of what a renderer would have added.
func docPage(c pageCtx, key, what string, resolve func(*config.Config, string, string) string) []card {
	s := c.snap
	type docResult struct {
		lines []string
		path  string
	}
	v, err, ready := c.lazy.need("doc:"+key, func() (any, error) {
		lines, path, err := readDocFile(s.cfg, s.cfgPath, resolve)
		return docResult{lines, path}, err
	})
	if !ready {
		return []card{{title: key, items: []item{reading(what)}}}
	}
	res, _ := v.(docResult)
	if err != nil {
		return []card{{title: key, items: []item{para{text: err.Error(), tone: "danger"}}}}
	}
	title := key
	if res.path != "" {
		title = key + " \u2014 " + res.path
	}
	return []card{{title: title, items: []item{mono{lines: res.lines}}}}
}

func pageAbout(c pageCtx) []card {
	s := c.snap
	pam := "no"
	if webadmin.PAMCompiledIn {
		pam = "yes"
	}
	rows := []kvRow{
		{"version", s.version, ""},
		{"commit", s.commit, ""},
		{"platform", runtime.GOOS + "/" + runtime.GOARCH, ""},
		{"go", runtime.Version(), ""},
		{"pam", pam, ""},
		{"config", s.cfgPath, "mut"},
		{"control socket", s.sockPath, "mut"},
	}
	if s.cfg != nil {
		rows = append(rows,
			kvRow{"node id", dash(s.cfg.NodeID), ""},
			kvRow{"hostname", dash(s.cfg.Hostname), ""},
		)
	}
	daemon, tone := "reachable", "ok"
	if !s.daemonUp() {
		daemon, tone = "not reachable: "+s.daemonErr.Error(), "danger"
	}
	rows = append(rows, kvRow{"daemon", daemon, tone})
	return []card{{title: "about", items: []item{kv{rows: rows}}}}
}

// ---- settings -----------------------------------------------------------

// pageSettings mirrors the gear page: every row there that has a config field
// behind it, in the same order, with the CLI leaf that sets it. The two rows
// with no CLI form are listed too, saying why — the same two
// cmd/gravinet/navparity_test.go names in its noCLIForm map, for the same
// reasons.
func pageSettings(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	cfg := s.cfg
	wa := cfg.WebAdmin

	console := editableKV{rows: []editableKVRow{
		{k: "web admin", v: onOff(wa.Enabled), tone: enabledTone(wa.Enabled)},
		{k: "listen", v: joinOr(cfg.ListenAddrsRaw(), "default (loopback + mesh)"),
			edit: listenAddrsForm},
		{k: "login lockout", v: fmt.Sprintf("%d attempts, %ds", wa.LoginBan.EffectiveMaxFailures(), wa.LoginBan.EffectiveBanSeconds()),
			edit: loginBanForm},
		{k: "config history limit", v: strconv.Itoa(cfg.EffectiveConfigHistoryLimit()),
			edit: func(m *model) formSpec {
				return intSettingForm("config history limit", "history-limit", cfg.EffectiveConfigHistoryLimit(), "how many snapshots to keep before pruning")
			}},
		{k: "remote shell", v: onOff(wa.AllowRemoteShell), tone: enabledTone(wa.AllowRemoteShell),
			edit: func(m *model) formSpec {
				return boolSettingForm("remote shell (needs a restart)", "shell", wa.AllowRemoteShell)
			}},
		{k: "geoip lookup", v: onOff(wa.GeoIPEnabled()), tone: enabledTone(wa.GeoIPEnabled()),
			edit: func(m *model) formSpec {
				return boolSettingForm("geoip lookup (needs a restart)", "geoip", wa.GeoIPEnabled())
			}},
	}}

	cluster := editableKV{rows: []editableKVRow{
		{k: "managed", v: onOff(cfg.Managed), tone: enabledTone(cfg.Managed),
			edit: func(m *model) formSpec { return topLevelBoolForm("managed mode", "managed", cfg.Managed) }},
		{k: "manager", v: onOff(cfg.Manager), tone: enabledTone(cfg.Manager),
			edit: func(m *model) formSpec { return topLevelBoolForm("manager mode", "manager", cfg.Manager) }},
		{k: "accept manager upgrades", v: onOff(cfg.Upgrade.AcceptManagerUpgrades), tone: enabledTone(cfg.Upgrade.AcceptManagerUpgrades),
			edit: func(m *model) formSpec {
				return boolSettingForm("accept manager upgrades", "accept-mgr-upgrades", cfg.Upgrade.AcceptManagerUpgrades)
			}},
	}}

	logging := editableKV{rows: []editableKVRow{
		{k: "log level", v: dash(cfg.LogLevel),
			edit: func(m *model) formSpec {
				return textSettingForm("log level", "log-level", cfg.LogLevel, "one of error, warn, info, debug")
			}},
		{k: "log size cap", v: cfg.LogMaxSizeString(),
			edit: func(m *model) formSpec {
				return textSettingForm("log size cap", "log-size", cfg.LogMaxSizeString(), "e.g. 200M, 1G, or a bare byte count")
			}},
		{k: "log file", v: dash(cfg.LogFilePath(s.cfgPath)), tone: "mut"},
	}}

	meshSettings := editableKV{rows: []editableKVRow{
		{k: "udp ports", v: portsOrOff(cfg.UDPPortList()), tone: portsTone(cfg.UDPPortList()),
			edit: func(m *model) formSpec { return portListForm("UDP ports", "udp-port", cfg.UDPPortList()) }},
		{k: "tcp ports", v: portsOrOff(cfg.TCPPortList()), tone: portsTone(cfg.TCPPortList()),
			edit: func(m *model) formSpec { return portListForm("TCP ports", "tcp-port", cfg.TCPPortList()) }},
		{k: "keepalive", v: cfg.KeepaliveDuration().String(),
			edit: func(m *model) formSpec {
				return intSettingForm("keepalive interval (seconds)", "keepalive", cfg.KeepaliveInterval, "0..86400; 0 = default")
			}},
		{k: "peer timeout", v: cfg.PeerTimeoutDuration().String(),
			edit: func(m *model) formSpec {
				return intSettingForm("peer timeout (seconds)", "peer-timeout", cfg.PeerTimeout, "0..86400; 0 = default; raised to at least the keepalive interval")
			}},
		{k: "route re-advertise", v: cfg.RouteAdvDuration().String(),
			edit: func(m *model) formSpec {
				return intSettingForm("route re-advertisement interval (seconds)", "route-adv", cfg.RouteAdvInterval, "0..86400; 0 = default")
			}},
		{k: "nat state timeout", v: natTimeout(cfg.NATStateTimeout),
			edit: func(m *model) formSpec {
				return intSettingForm("NAT state timeout (seconds)", "nat-state", cfg.NATStateTimeout, "0 = default 120s")
			}},
		{k: "upnp", v: onOff(cfg.EnableUPnP), tone: enabledTone(cfg.EnableUPnP),
			edit: func(m *model) formSpec { return boolSettingForm("upnp (needs a restart)", "upnp", cfg.EnableUPnP) }},
	}}

	host := editableKV{rows: []editableKVRow{
		{k: "ip forwarding", v: onOff(cfg.ForwardingEnabled()), tone: enabledTone(cfg.ForwardingEnabled()),
			edit: func(m *model) formSpec {
				return boolSettingForm("ip forwarding (needs a restart)", "ip-forwarding", cfg.ForwardingEnabled())
			}},
		{k: "icmp redirects suppressed", v: onOff(cfg.RedirectsDisabled()), tone: enabledTone(cfg.RedirectsDisabled()),
			edit: func(m *model) formSpec {
				return boolSettingForm("suppress icmp redirects (needs a restart)", "ip-redirects", cfg.RedirectsDisabled())
			}},
	}}

	perf := editableKV{rows: []editableKVRow{
		{k: "worker threads", v: zeroAsDefault(cfg.WorkerThreads, "per core"),
			edit: func(m *model) formSpec {
				return intSettingForm("worker threads (needs a restart)", "worker-threads", cfg.WorkerThreads, "0 = per core")
			}},
		{k: "tun queues", v: zeroAsDefault(cfg.TunQueues, "off (single queue)"),
			edit: func(m *model) formSpec {
				return intSettingForm("tun queues (Linux only; needs a restart)", "tun-queues", cfg.TunQueues, "0 = single queue")
			}},
		{k: "socket buffer", v: zeroAsDefault(cfg.SocketBufferMB(), "default") + mbSuffix(cfg.SocketBufferMB()),
			edit: func(m *model) formSpec {
				return intSettingForm("socket buffer, MB (needs a restart)", "socket-buffer", cfg.SocketBufferMB(), "0 = default")
			}},
		{k: "udp gso", v: onOff(cfg.UDPGSOEnabled()), tone: enabledTone(cfg.UDPGSOEnabled()),
			edit: func(m *model) formSpec { return boolSettingForm("udp gso (needs a restart)", "udp-gso", cfg.UDPGSOEnabled()) }},
	}}

	ddns := cfg.DDNS
	ddnsRows := editableKV{rows: []editableKVRow{
		{k: "dynamic dns", v: onOff(ddns.Active()), tone: enabledTone(ddns.Active()), edit: ddnsForm},
		{k: "interval", v: ddnsInterval(ddns), edit: ddnsForm},
		{k: "ttl", v: strconv.Itoa(ddns.TTL) + "s", edit: ddnsForm},
		{k: "reverse records", v: onOff(ddns.ReverseEnabled()), tone: enabledTone(ddns.ReverseEnabled()), edit: ddnsForm},
		{k: "tsig key", v: tsigState(ddns.TSIGKey), tone: tsigTone(ddns.TSIGKey), edit: ddnsKeyForm},
	}}

	return []card{
		{title: "console", items: []item{console}},
		{title: "cluster", items: []item{cluster}},
		{title: "logging", items: []item{logging}},
		{title: "mesh", items: []item{meshSettings}},
		{title: "host", items: []item{host}},
		{title: "performance", items: []item{perf}},
		{title: "dynamic dns", items: []item{ddnsRows}},
		{title: "not shown", items: []item{para{
			text: "Two rows on the web admin's Settings page have nothing here to show. Dark mode is a per-browser " +
				"preference with nothing in config.json behind it — this console has its own theme, on the t key. " +
				"The TLS certificate upload wants two PEM files pasted in, and is safer done where the browser you " +
				"would lock out is the one asking.", tone: "mut"}}},
	}
}

func portsOrOff(ports []int) string {
	if len(ports) == 0 {
		return "off"
	}
	return intsToStr(ports)
}

func portsTone(ports []int) string {
	if len(ports) == 0 {
		return "warn"
	}
	return ""
}

func zeroAsDefault(n int, what string) string {
	if n == 0 {
		return what
	}
	return strconv.Itoa(n)
}

func mbSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return " MB"
}

func ddnsInterval(d config.DDNSConfig) string {
	if !d.Active() {
		return "off"
	}
	return d.Interval().String()
}

func tsigState(key string) string {
	if strings.TrimSpace(key) == "" {
		return "unset (updates are sent unsigned)"
	}
	return "set"
}

func tsigTone(key string) string {
	if strings.TrimSpace(key) == "" {
		return "warn"
	}
	return "ok"
}
