package tui

// The monitor, system, info and settings pages. Split from sections.go only
// for length; the contract is identical — snapshot in, []card out, no I/O in
// the builder itself (see lazy.go for how the slow reads are arranged).

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

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
		items = append(items, kv{rows: []kvRow{
			{"state dir", dash(s.cfg.UpgradeStateDir()), "mut"},
			{"confirm window", strconv.Itoa(s.cfg.UpgradeConfirmSeconds()) + "s", ""},
			{"accept pushes from a manager", onOff(s.cfg.Upgrade.AcceptManagerUpgrades),
				enabledTone(s.cfg.Upgrade.AcceptManagerUpgrades)},
		}})
	}
	return []card{
		{title: "upgrade", items: items},
		card{title: "note", items: []item{para{
			text: "Checking for and applying a new binary replaces the running process and restarts the service. " +
				"That is not something to do from a screen that is only meant to be read, so it is not wired to a " +
				"key here: \"gravinet upgrade check\" and \"gravinet upgrade apply\" are the same operations the web " +
				"admin's buttons call.", tone: "mut"}}},
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
	live := kv{rows: []kvRow{
		{"hostname", dash(info.Hostname), ""},
		{"dns servers", joinOr(info.DNSServers, "\u2014"), ""},
		{"search domain", dash(info.SearchDomain), ""},
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
	return append(cards,
		card{title: "note", items: []item{para{
			text: "changing the hostname needs a gravinet restart before mesh peers see the new name: the advertised " +
				"name is read once at startup.", tone: "mut"}}},
		editHint("gravinet system resolver -h"))
}

func pageTime(c pageCtx) []card {
	v, _, ready := c.lazy.need("time", func() (any, error) { return service.HostTime(), nil })
	if !ready {
		return []card{{title: "time", items: []item{reading("clock, timezone and NTP state")}}}
	}
	info, _ := v.(service.TimeInfo)
	return []card{
		{title: "time", items: []item{kv{rows: timeRows(info)}}},
		editHint("gravinet system time -h"),
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
	items := []item{kv{rows: []kvRow{{"mode (configured)", mode, enabledTone(mode != "off")}}}}
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
				"reader cannot see its sockets and guessing would put a confident wrong answer on the screen.",
			tone: "mut"}}},
		editHint("gravinet system dhcp mode <off|relay>"))
}

func pageSNMP(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	sn := s.cfg.SNMP
	items := []item{kv{rows: []kvRow{
		{"state", onOff(sn.Enabled), enabledTone(sn.Enabled)},
		{"listen", dash(sn.ListenAddr), ""},
		{"location", dash(sn.Location), ""},
		{"contact", dash(sn.Contact), ""},
		{"runnable", yesNo(sn.IsRunnable()), enabledTone(sn.IsRunnable())},
	}}}
	if len(sn.Communities) > 0 {
		// The community string is a credential and is not printed, for the
		// same reason key material is not printed on the Keys page: this
		// screen ends up in a scrollback buffer.
		t := table{head: []string{"community", "state"}}
		for _, cm := range sn.Communities {
			row := tableRow{cells: []string{"(set)", onOff(!cm.Disabled)}, cellTone: map[int]string{1: enabledTone(!cm.Disabled)}}
			if cm.Disabled {
				row.tone = "dim"
			}
			t.rows = append(t.rows, row)
		}
		items = append(items, t)
	}
	return []card{
		{title: "snmp", items: items},
		card{title: "note", items: []item{para{
			text: "community strings are credentials and are not printed here.", tone: "mut"}}},
		editHint("gravinet system snmp -h"),
	}
}

func pageLLDP(c pageCtx) []card {
	s := c.snap
	if s.cfg == nil {
		return []card{noConfig(s)}
	}
	d := s.cfg.Discovery
	items := []item{kv{rows: []kvRow{
		{"state", onOff(!d.Disabled), enabledTone(!d.Disabled)},
		{"runnable", yesNo(d.IsRunnable()), enabledTone(d.IsRunnable())},
		{"any cdp", yesNo(d.AnyCDP()), ""},
	}}}
	if len(d.Interfaces) > 0 {
		t := table{head: []string{"iface", "lldp", "cdp"}}
		for _, i := range d.Interfaces {
			t.rows = append(t.rows, tableRow{cells: []string{i.Name, onOff(i.LLDP), onOff(i.CDP)},
				cellTone: map[int]string{1: enabledTone(i.LLDP), 2: enabledTone(i.CDP)}})
		}
		items = append(items, t)
	} else {
		items = append(items, empty{"no interfaces run discovery"})
	}
	return []card{
		{title: "lldp", items: items},
		card{title: "note", items: []item{para{
			text: "this is which interfaces run discovery. The neighbors they find are under Monitor \u203a L2 Peers.",
			tone: "mut"}}},
		editHint("gravinet system lldp -h"),
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
		editHint("gravinet system syslog -h"),
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
		if s.cfg.HostSettings != nil && len(s.cfg.HostSettings.Users) > 0 {
			t := table{head: []string{"user", "expires"}}
			for _, u := range s.cfg.HostSettings.Users {
				exp := "never"
				if u.ExpiresUnix > 0 {
					exp = time.Unix(u.ExpiresUnix, 0).UTC().Format("2006-01-02")
				}
				t.rows = append(t.rows, tableRow{cells: []string{u.Name, exp}})
			}
			items = append(items, t)
		}
	} else {
		items = append(items, para{text: fmt.Sprintf("could not read %s: %v", s.cfgPath, s.cfgErr), tone: "danger"})
	}
	return []card{
		{title: "users", items: items},
		card{title: "note", items: []item{para{
			text: "credentials are never printed. \"gravinet genpass\" produces a new one.", tone: "mut"}}},
		editHint("gravinet system users -h"),
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
		t := table{head: []string{"id", "when", "by", "summary"}}
		for _, e := range entries {
			t.rows = append(t.rows, tableRow{cells: []string{
				e.ID, dash(e.Stamp), dash(e.User), dash(e.Summary),
			}})
		}
		items = append(items, t)
	} else {
		items = append(items, empty{"no snapshots"})
	}
	return []card{
		{title: "config history", items: items},
		editHint("gravinet system config-history <list|diff|restore|snapshot>"),
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
		items = append(items, kv{rows: []kvRow{
			{"supported on this host", yesNo(ok), enabledTone(ok)},
		}})
		if note != "" {
			items = append(items, para{text: note, tone: "mut"})
		}
	} else {
		items = append(items, reading("power support"))
	}
	items = append(items, para{
		text: "Restarting or shutting down the host is not bound to a key here, deliberately. This page is read " +
			"from a terminal that is frequently the only way back into the machine, and a keystroke that takes it " +
			"down does not belong on a screen somebody is scrolling through. \"gravinet system power\" asks first " +
			"and is where this lives.", tone: "warn"})
	return []card{{title: "power", items: items}}
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

	console := kv{rows: []kvRow{
		{"web admin", onOff(wa.Enabled), enabledTone(wa.Enabled)},
		{"listen", joinOr(cfg.ListenAddrsRaw(), "default (loopback + mesh)"), ""},
		{"login lockout", fmt.Sprintf("%d attempts, %ds", wa.LoginBan.EffectiveMaxFailures(), wa.LoginBan.EffectiveBanSeconds()), ""},
		{"config history limit", strconv.Itoa(cfg.EffectiveConfigHistoryLimit()), ""},
		{"remote shell", onOff(wa.AllowRemoteShell), enabledTone(wa.AllowRemoteShell)},
		{"geoip lookup", onOff(wa.GeoIPEnabled()), enabledTone(wa.GeoIPEnabled())},
	}}

	cluster := kv{rows: []kvRow{
		{"managed", onOff(cfg.Managed), enabledTone(cfg.Managed)},
		{"manager", onOff(cfg.Manager), enabledTone(cfg.Manager)},
		{"accept manager upgrades", onOff(cfg.Upgrade.AcceptManagerUpgrades), enabledTone(cfg.Upgrade.AcceptManagerUpgrades)},
	}}

	logging := kv{rows: []kvRow{
		{"log level", dash(cfg.LogLevel), ""},
		{"log size cap", cfg.LogMaxSizeString(), ""},
		{"log file", dash(cfg.LogFilePath(s.cfgPath)), "mut"},
	}}

	mesh := kv{rows: []kvRow{
		{"udp ports", portsOrOff(cfg.UDPPortList()), portsTone(cfg.UDPPortList())},
		{"tcp ports", portsOrOff(cfg.TCPPortList()), portsTone(cfg.TCPPortList())},
		{"keepalive", cfg.KeepaliveDuration().String(), ""},
		{"peer timeout", cfg.PeerTimeoutDuration().String(), ""},
		{"route re-advertise", cfg.RouteAdvDuration().String(), ""},
		{"nat state timeout", natTimeout(cfg.NATStateTimeout), ""},
		{"upnp", onOff(cfg.EnableUPnP), enabledTone(cfg.EnableUPnP)},
	}}

	host := kv{rows: []kvRow{
		{"ip forwarding", onOff(cfg.ForwardingEnabled()), enabledTone(cfg.ForwardingEnabled())},
		{"icmp redirects suppressed", onOff(cfg.RedirectsDisabled()), enabledTone(cfg.RedirectsDisabled())},
	}}

	perf := kv{rows: []kvRow{
		{"worker threads", zeroAsDefault(cfg.WorkerThreads, "per core"), ""},
		{"tun queues", zeroAsDefault(cfg.TunQueues, "off (single queue)"), ""},
		{"socket buffer", zeroAsDefault(cfg.SocketBufferMB(), "default") + mbSuffix(cfg.SocketBufferMB()), ""},
		{"udp gso", onOff(cfg.UDPGSOEnabled()), enabledTone(cfg.UDPGSOEnabled())},
	}}

	ddns := cfg.DDNS
	ddnsRows := kv{rows: []kvRow{
		{"dynamic dns", onOff(ddns.Active()), enabledTone(ddns.Active())},
		{"interval", ddnsInterval(ddns), ""},
		{"ttl", strconv.Itoa(ddns.TTL) + "s", ""},
		{"reverse records", onOff(ddns.ReverseEnabled()), enabledTone(ddns.ReverseEnabled())},
		{"tsig key", tsigState(ddns.TSIGKey), tsigTone(ddns.TSIGKey)},
	}}

	return []card{
		{title: "console", items: []item{console}},
		{title: "cluster", items: []item{cluster}},
		{title: "logging", items: []item{logging}},
		{title: "mesh", items: []item{mesh}},
		{title: "host", items: []item{host}},
		{title: "performance", items: []item{perf}},
		{title: "dynamic dns", items: []item{ddnsRows}},
		{title: "not shown", items: []item{para{
			text: "Two rows on the web admin's Settings page have nothing here to show. Dark mode is a per-browser " +
				"preference with nothing in config.json behind it — this console has its own theme, on the t key. " +
				"The TLS certificate upload wants two PEM files pasted in, and is safer done where the browser you " +
				"would lock out is the one asking.", tone: "mut"}}},
		editHint("gravinet settings -h \u2014 every row above has a leaf under it"),
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
