package tui

// The snapshot: everything a page might read, gathered in one place so a page
// builder is a pure function of it.
//
// There are two sources, and the distinction between them is the one thing
// worth being careful about. The config file is what this node has been told
// to do; the daemon is what it is actually doing. They are frequently not the
// same — a config edited five minutes ago and not reloaded, a network that is
// enabled in the file and failed to bring its interface up — and a console
// that silently blends them is a console that will one day show an operator a
// firewall rule that is not in force.
//
// So pages that show configuration say so, pages that show live state say so,
// and where a page has both (Networks, which pairs each configured network
// with its live interface) the two are separate columns. When the daemon is
// unreachable, live pages say that in as many words rather than rendering an
// empty table, because an empty table and a dead daemon look identical and
// mean opposite things.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/control"
	"gravinet/internal/hosts"
	"gravinet/internal/mesh"
	"gravinet/internal/resolver"
	"gravinet/internal/service"
	"gravinet/internal/webadmin"
)

// caps is internal/webadmin's Caps, kept as its own type here so nav.go's
// sectionVisible reads the way ui.go's does rather than reaching through a
// package name for every field.
type caps struct {
	bgp, ipv6ra, dhcp, snmp, lldp, syslog bool
}

// detectCaps probes the host through webadmin.Capabilities — the same call
// /api/config makes, so the rail hides the same pages in both clients.
func detectCaps() caps {
	c := webadmin.Capabilities()
	return caps{bgp: c.BGP, ipv6ra: c.IPv6RA, dhcp: c.DHCP, snmp: c.SNMP, lldp: c.LLDP, syslog: c.Syslog}
}

// livePeer/liveBan/liveRoute pair one of the control protocol's own structs
// with which network it was fetched under — see snapshot's own comment on
// peers/bans/routes for why that pairing is necessary rather than
// decorative: those three structs carry no network field of their own, so
// without this, a node running more than one network would have no way to
// tell two peers with the same NodeID-on-different-networks apart, and no
// way to say which network's ban to lift.
type livePeer struct {
	net string
	mesh.PeerInfo
}

type liveBan struct {
	net string
	mesh.BanInfo
}

type liveRoute struct {
	net string
	mesh.RouteInfo
}

// snapshot is one gathering of state. Pages read it and never fetch anything
// themselves: a page that did its own I/O would be a page that cannot be
// tested without the thing it fetches from, and would block the event loop
// while it did.
type snapshot struct {
	// Configuration.
	cfg     *config.Config
	cfgPath string
	cfgErr  error

	// Live daemon state. daemonErr non-nil means every field below it is
	// empty because the daemon could not be reached, which is a different
	// statement from "the daemon reports nothing".
	//
	// peers/bans/routes are gathered per network rather than in one
	// untargeted call — see loadLive's own comment for why that distinction
	// is not optional: the control protocol's peers/bans/routes commands
	// resolve to exactly one network when no -net is given, and *fail* if a
	// node runs more than one. livePeer/liveBan/liveRoute carry which
	// network each entry came from, which read pages use to label rows and
	// which the mesh group's row actions use to target the right -net.
	daemonErr error
	sockPath  string
	peers     []livePeer
	bans      []liveBan
	routes    []liveRoute
	ifaces    []mesh.IfaceInfo
	natClass  string
	natPublic string

	caps  caps
	taken time.Time

	// version/commit are the running binary's build identity, passed in by
	// cmd/gravinet rather than read here: they are ldflags-set variables in
	// package main, and a copy in this package would be a second version
	// number that could disagree with "gravinet version".
	version, commit string
}

// loadSnapshot gathers everything. Nothing here is fatal: a missing config
// and a dead daemon are both ordinary states for this console to be opened
// in — arguably the states it is most useful in — so each failure is recorded
// and the rest of the gather continues.
func loadSnapshot(cfgPath, sockPath, version, commit string) *snapshot {
	s := &snapshot{cfgPath: cfgPath, sockPath: sockPath, taken: time.Now(), version: version, commit: commit}
	s.caps = detectCaps()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		s.cfgErr = err
	} else {
		s.cfg = cfg
		// The config's own control_socket wins over the caller's default,
		// matching how every CLI command resolves it: a node that moved its
		// socket said so in the file, and the default is only a default.
		if cfg.ControlSocket != "" && sockPath == "" {
			s.sockPath = cfg.ControlSocket
		}
	}
	if s.sockPath == "" {
		s.sockPath = sockPath
	}

	s.loadLive()
	return s
}

// loadLive fills in the daemon-sourced half. Four round trips on a unix
// socket; the first failure stands for all of them, since they all go to the
// same process and a daemon that cannot answer "peers" will not answer
// "bans" either.
// loadLive fills in the daemon-sourced half.
//
// This starts with "nets" — the daemon's own live list of which networks it
// is actually running, by id — rather than reading s.cfg.Networks for the
// same list. Two reasons: it works even when this process's own config
// failed to load (a live page has no reason to depend on a config read
// succeeding, any more than the read pages already don't — see noConfig vs.
// noDaemon), and it can never disagree with the daemon about which networks
// exist, which a config read from a possibly-stale copy of the file could.
//
// Every one of peers/bans/routes is then fetched once per network, because
// the control protocol's own routing (Server.resolveNet, internal/control)
// requires a specific -net whenever a node runs more than one — an
// untargeted call doesn't mean "give me everything," it means "there had
// better be exactly one," and errors otherwise. A node running two networks
// calling this the old way got "multiple networks; specify -net" back as
// its daemon error on every live page, which is a real bug this rewrite
// fixes rather than a hypothetical one.
func (s *snapshot) loadLive() {
	resp, err := control.Do(s.sockPath, control.Request{Cmd: "nets"})
	if err != nil {
		s.daemonErr = err
		return
	}
	if !resp.OK {
		s.daemonErr = errors.New(resp.Error)
		return
	}
	s.peers, s.bans, s.routes = nil, nil, nil
	for _, id := range resp.Nets {
		name := s.networkNameFor(id)
		if r, err := control.Do(s.sockPath, control.Request{Cmd: "peers", Net: id}); err == nil && r.OK {
			for _, p := range r.Peers {
				s.peers = append(s.peers, livePeer{net: name, PeerInfo: p})
			}
			// NAT class/public address are properties of this node's own
			// underlay position, not of any one overlay network, so every
			// network reports the same pair — last one written is as good
			// as any other, and simpler than asserting they all agree.
			s.natClass, s.natPublic = r.NATClass, r.Public
		}
		if r, err := control.Do(s.sockPath, control.Request{Cmd: "bans", Net: id}); err == nil && r.OK {
			for _, b := range r.Bans {
				s.bans = append(s.bans, liveBan{net: name, BanInfo: b})
			}
		}
		if r, err := control.Do(s.sockPath, control.Request{Cmd: "routes", Net: id}); err == nil && r.OK {
			for _, rt := range r.Routes {
				s.routes = append(s.routes, liveRoute{net: name, RouteInfo: rt})
			}
		}
	}
	if r, err := control.Do(s.sockPath, control.Request{Cmd: "ifaces"}); err == nil && r.OK {
		s.ifaces = r.Ifaces
	}
}

// networkNameFor resolves a hex network id (as "nets" and every per-network
// response report it) to this node's own label for it, falling back to the
// hex id itself when the config that would name it isn't loaded — a live
// page has to keep working even when the config read failed (see
// loadLive's own comment), just with less friendly labels.
func (s *snapshot) networkNameFor(hexID string) string {
	if s.cfg != nil {
		for _, n := range s.cfg.Networks {
			if strings.EqualFold(n.ID, hexID) {
				return n.Name
			}
		}
	}
	return hexID
}

// daemonUp reports whether live state is available.
func (s *snapshot) daemonUp() bool { return s.daemonErr == nil }

// refreshedLive returns a new snapshot with the live half re-read, leaving s
// itself untouched.
//
// This exists because loadLive mutates its receiver's fields in place, which
// is fine at construction time — loadSnapshot builds s and calls it before s
// is visible to anything else — but is not fine on the periodic re-read in
// app.go's event loop. By the time that tick fires, a page may have started a
// lazy fetch (lazy.go) that captured the *current* snapshot pointer and is
// reading s.ifaces or s.peers from a goroutine of its own; mutating those
// same fields out from under it on the main goroutine is a data race even
// though it is a rare and short one, and the kind that a race detector only
// catches when the timing lines up.
//
// So a refresh builds a shallow copy and mutates *that*, and the caller
// (app.run) swaps its pointer to the new snapshot rather than reusing the
// old one. Any fetch still holding the old pointer keeps reading a value that
// is no longer being written to, which is safe by construction rather than
// by timing. cfg is shared between old and new (it is only ever replaced
// wholesale, on the r key's full reload, never mutated), so the copy is
// cheap: a handful of fields and three pointer-sized slices reassigned, not
// a deep copy of the config tree.
func (s *snapshot) refreshedLive() *snapshot {
	cp := *s
	cp.taken = time.Now()
	cp.daemonErr = nil
	cp.loadLive()
	return &cp
}

// ifaceFor returns the live kernel interface for a configured network, and
// whether the network is up at all. This is the join between the two halves,
// and the only place a page should be doing it.
func (s *snapshot) ifaceFor(netName string) (string, bool) {
	for _, ifc := range s.ifaces {
		if ifc.Name == netName {
			return ifc.Iface, true
		}
	}
	return "", false
}

// ---- host readers -------------------------------------------------------
//
// These are the things a page needs that are neither config nor daemon state:
// files and OS queries. Each delegates to the same exported reader the CLI
// leaf for that page uses, named in its comment so the pairing is checkable.

// readHostsFile is Monitor > Hosts File, the same read cmdMonitorHostsFile does.
func readHostsFile() ([]string, error) {
	path := hosts.DefaultPath()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return splitLines(string(b)), nil
}

// readRouteTable is Monitor > Route Table, via webadmin.LocalRouteTableText —
// the same call cmdMonitorRouteTable makes.
func readRouteTable() ([]string, error) {
	text, err := webadmin.LocalRouteTableText()
	if err != nil {
		return nil, err
	}
	return splitLines(text), nil
}

// readBGPPeers is Monitor > BGP Peers, via the same hardened vtysh call the
// web page and cmdMonitorBGPPeers both use.
func readBGPPeers() ([]string, error) {
	out, ok := webadmin.RunVtysh("show bgp summary")
	if !ok {
		return nil, errors.New("FRR/vtysh is not installed, not running, or didn't answer in time")
	}
	return splitLines(string(out)), nil
}

// readL2Peers is Monitor > L2 Peers, via service.LLDPNeighbors — the same
// call the web handler and cmdMonitorL2Peers make.
func readL2Peers() ([]service.LLDPNeighbor, error) {
	n, ok, hint := service.LLDPNeighbors()
	if !ok {
		return nil, errors.New(hint)
	}
	return n, nil
}

// readDNSState is Monitor > DNS State: what is registered with the OS
// resolver right now, per live network. Needs the live interface mapping
// first, which is why it takes the snapshot — see cmdMonitorDNSState's own
// comment on why config alone cannot answer this.
func readDNSState(s *snapshot) []dnsStateEntry {
	out := make([]dnsStateEntry, 0, len(s.ifaces))
	for _, ifc := range s.ifaces {
		// Must match dnssync.go's tag derivation, as handleLocalDNS and the
		// CLI both note: DNSTag is never set from config today, so it always
		// falls through to this form.
		tag := fmt.Sprintf("%016x", ifc.NetworkID)
		text, err := resolver.Dump(tag, ifc.Iface)
		e := dnsStateEntry{network: ifc.Name, iface: ifc.Iface}
		if err != nil {
			e.err = err
		} else {
			e.lines = splitLines(text)
		}
		out = append(out, e)
	}
	return out
}

type dnsStateEntry struct {
	network, iface string
	lines          []string
	err            error
}

// readLogTail returns the last n lines of the daemon's log file. Same file,
// same resolution (cfg.LogFilePath) and the same 1 MiB read cap as
// cmdMonitorLogs and the web admin's own Logs page.
func readLogTail(cfg *config.Config, cfgPath string, n int) ([]string, string, error) {
	if cfg == nil {
		return nil, "", errors.New("no config loaded")
	}
	path := cfg.LogFilePath(cfgPath)
	if path == "" {
		return nil, "", errors.New("file logging is not enabled in this config")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, path, err
	}
	defer f.Close()

	const maxRead = 1 << 20
	var buf []byte
	if fi, ferr := f.Stat(); ferr == nil {
		start := int64(0)
		if fi.Size() > maxRead {
			start = fi.Size() - maxRead
		}
		if _, serr := f.Seek(start, io.SeekStart); serr == nil {
			buf, _ = io.ReadAll(f)
		}
	}
	lines := splitLines(strings.TrimRight(string(buf), "\n"))
	if len(buf) == maxRead && len(lines) > 1 {
		lines = lines[1:] // drop the partial line seeking mid-file produced
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, path, nil
}

// readDocFile reads one of the Info pages' documents. Path resolution is the
// config's own method (ReadmePath and friends), which is how cmdRun wires
// these into the web admin and how cmdInfoReadme reads them — so all three
// find the same file or all three fail to.
func readDocFile(cfg *config.Config, cfgPath string, resolve func(*config.Config, string, string) string) ([]string, string, error) {
	if cfg == nil {
		return nil, "", errors.New("no config loaded")
	}
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = dirOf(exe)
	}
	path := resolve(cfg, cfgPath, exeDir)
	if path == "" {
		return nil, "", errors.New("not found next to the binary or the config, and no path is set in the config")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, path, err
	}
	return splitLines(string(b)), path, nil
}

// readInterfaces is System > Interfaces: this host's interfaces and
// addresses, read directly. Same source and same reasoning as
// cmdSystemInterfaces — an interface list is exactly what is wanted when the
// daemon is the thing that will not start.
func readInterfaces() ([]ifaceRow, error) {
	ifis, err := netInterfaces()
	if err != nil {
		return nil, err
	}
	sort.Slice(ifis, func(i, j int) bool { return ifis[i].name < ifis[j].name })
	return ifis, nil
}

type ifaceRow struct {
	name  string
	state string
	mtu   int
	mac   string
	addrs []string
}

// hostSnapshot is Monitor > Metrics, via webadmin.TakeHostSnapshot. Blocks
// for about a second — CPU and throughput are rates and need two samples —
// which is why the metrics page fetches it on a worker rather than in the
// draw path. See app.go's metricsWorker.
func hostSnapshot(ifaces []mesh.IfaceInfo) webadmin.HostSnapshot {
	return webadmin.TakeHostSnapshot(ifaces)
}

// ---- small shared helpers ----------------------------------------------

// splitLines splits on newlines and drops a trailing empty element, so a file
// ending in a newline does not render one blank line at the bottom of every
// document.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// dirOf is filepath.Dir without the import, which this file needs for exactly
// one call.
func dirOf(p string) string {
	sep := "/"
	if runtime.GOOS == "windows" {
		sep = "\\"
		p = strings.ReplaceAll(p, "/", "\\")
	}
	if i := strings.LastIndex(p, sep); i > 0 {
		return p[:i]
	}
	return "."
}
