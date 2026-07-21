package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/control"
	"gravinet/internal/hosts"
	"gravinet/internal/resolver"
	"gravinet/internal/webadmin"
)

// This file adds a second, nested way to reach every existing CLI command —
// "gravinet <group> <section> ..." — mirroring internal/webadmin/ui.go's own
// NAV_GROUPS exactly (mesh/traffic/naming/monitor/info), plus a "settings"
// group for managed/manager mode, which the web admin also has but reaches
// through the gear icon rather than the left rail. It is deliberately
// additive: every flat command (network, key, route, seed, nat, host, qos,
// bandwidth, fw, ban, unban, managed, manager, upgrade, latency, ...) keeps
// working completely unchanged, args and flags identical, because most of
// them are called by name from scripts (the install-*.sh scripts, the
// upgrade preflight's own "gravinet selftest"/"gravinet version" calls) that
// have no reason to know this restructuring happened. Every leaf below that
// has an existing flat command just calls that command's function directly
// with the same args slice — there is exactly one implementation of each
// command's logic, reached two ways, not two implementations that could
// drift apart.
//
// A few NAV_GROUPS sections don't have any CLI-reachable equivalent today —
// mostly Monitor's live host-state views (metrics, capture, route-table,
// bgp-peers, hosts-file, dns-state, logs) and Traffic > BGP / Naming > DNS,
// which are web-admin/config-file-only features with no control-socket
// command behind them at all. Rather than omit those sections (which would
// make the CLI's shape silently incomplete next to the web admin, or make
// "gravinet monitor logs" a confusing "unknown subcommand") they're listed
// with notYetInCLI, which says plainly that this isn't wired up yet instead
// of pretending to be a real answer.

// groupLeaf is one runnable entry in a group's menu.
type groupLeaf struct {
	name string
	desc string
	run  func(args []string)
}

// dispatchGroup runs the leaf named args[0] under this group, or — if args
// is empty or doesn't match anything — prints what's available and exits
// nonzero, the same way the top-level "gravinet" with no/unknown command
// does in usage().
func dispatchGroup(groupPath string, leaves []groupLeaf, args []string) {
	if len(args) > 0 {
		for _, l := range leaves {
			if l.name == args[0] {
				l.run(args[1:])
				return
			}
		}
		fmt.Fprintf(os.Stderr, "gravinet %s: unknown subcommand %q\n\n", groupPath, args[0])
	}
	fmt.Fprintf(os.Stderr, "gravinet %s <subcommand>:\n", groupPath)
	for _, l := range leaves {
		fmt.Fprintf(os.Stderr, "  %-15s %s\n", l.name, l.desc)
	}
	os.Exit(2)
}

// notYetInCLI is the leaf for a NAV_GROUPS section with no control-socket
// command behind it — see this file's package comment for which ones and
// why. Reports that plainly rather than a bare "unknown subcommand", and
// exits nonzero like any other command that couldn't do what was asked.
func notYetInCLI(what string) func([]string) {
	return func(args []string) {
		fatal("%s isn't available via the CLI yet — use the web admin for this", what)
	}
}

// cmdMonitorCapture and speedtestNotYetInCLI: both of these were actually
// investigated rather than assumed out of scope, and they landed
// differently —
//
// capture (real since v554, by delegation): internal/webadmin's
// startCapture(iface, snaplen, onPacket) is genuinely daemon-independent
// (one per platform — capture_linux.go, capture_darwin.go, etc. — no
// engine state involved), so an in-process CLI capture was possible in
// principle. What was never readily reusable is everything downstream of
// it: capHandle/stop() and the pcap writer (captureState.writePcap) are
// unexported and coupled to the web admin's own in-memory ring buffer, so
// in-process parity meant either exporting a meaningful chunk of that
// machinery across six platform files or writing a second, independent
// pcap encoder — real work, with the same per-platform-raw-capture risk
// profile as the FreeBSD tun fixes got. The field answer was simpler and
// better: on a terminal, the tool for "show me packets on this interface"
// already exists, is installed nearly everywhere, and does filtering,
// -w pcap output, and rotation better than a reimplementation ever would.
// So cmdMonitorCapture just runs tcpdump on the requested overlay
// interface, passing any extra args through verbatim — gravinet's only
// added value here is knowing *which* interfaces are its overlays (the
// "ifaces" control command, same one monitor dns-state uses) so a bare
// "gravinet monitor capture" can pick the interface for you when there's
// only one, and list them when there's more.
//
// speedtest: unlike everything else under "monitor", this isn't a local
// read — handleSpeedtestRun coordinates an active throughput test between
// two live peers over the mesh itself, which only the running daemon can
// initiate. A CLI equivalent needs actual new control-socket protocol (an
// asynchronous start-job/poll-status shape, not a single request/response
// like everything else here), not just a new command reusing an existing
// local reader.
func speedtestNotYetInCLI(args []string) {
	fatal(`monitor speedtest isn't available via the CLI yet.

Unlike everything else under "monitor", this isn't a local read — it
coordinates an active throughput test between two live peers over the mesh
itself, which only the running daemon can initiate. A CLI equivalent needs
real new control-socket protocol (start a job, poll its status), not just a
new command reusing an existing reader. Use the web admin for this today.`)
}

// cmdMonitorCapture is "gravinet monitor capture [iface] [tcpdump args...]":
// live packet capture on an overlay interface, by running tcpdump — see the
// doc comment above for why delegation beat an in-process reimplementation.
// Everything after the interface is handed to tcpdump untouched, so the
// full filter/output vocabulary works exactly as documented in tcpdump(8):
//
//	gravinet monitor capture                      # sole overlay iface, or a list to pick from
//	gravinet monitor capture mesh0                # tcpdump -i mesh0
//	gravinet monitor capture mesh0 -n port 53     # plus any tcpdump flags/filter
//	gravinet monitor capture mesh0 -w out.pcap    # write a pcap
//
// tcpdump needs the same privileges here it needs anywhere (root or
// CAP_NET_RAW); it reports that itself, in its own words, if missing.
func cmdMonitorCapture(args []string) {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println(`gravinet monitor capture [iface] [tcpdump args...] — live packet capture on an
overlay interface, delegated to tcpdump.

With no interface named, asks the running daemon for its overlay interfaces
and uses the only one (or lists them if there are several). Everything after
the interface is passed to tcpdump verbatim: filters ("port 53"), -n, -v,
-w file.pcap, -c 100, anything tcpdump(8) documents. Requires tcpdump on
PATH and the privileges tcpdump itself needs (root or CAP_NET_RAW).`)
		return
	}

	var iface string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		iface, args = args[0], args[1:]
	}
	if iface == "" {
		// No interface named: the one thing gravinet knows that tcpdump
		// doesn't is which interfaces are its overlays. Ask the daemon.
		resp, err := control.Do(defaultControlSocket(), control.Request{Cmd: "ifaces"})
		if err != nil || !resp.OK {
			fatal("no interface given and the daemon isn't reachable to list overlay interfaces — name one explicitly: gravinet monitor capture <iface>")
		}
		switch len(resp.Ifaces) {
		case 0:
			fatal("no interface given and the daemon reports no overlay interfaces up — name one explicitly")
		case 1:
			iface = resp.Ifaces[0].Iface
			fmt.Fprintf(os.Stderr, "capturing on %s (network %q — the only overlay interface)\n", iface, resp.Ifaces[0].Name)
		default:
			fmt.Fprintln(os.Stderr, "several overlay interfaces are up — pick one:")
			for _, ifc := range resp.Ifaces {
				fmt.Fprintf(os.Stderr, "  gravinet monitor capture %-8s (network %q)\n", ifc.Iface, ifc.Name)
			}
			os.Exit(2)
		}
	}

	td, err := exec.LookPath("tcpdump")
	if err != nil {
		// Root's PATH often carries the sbin dirs, but a sudo-less shell's
		// may not even though the binary is right there — check the usual
		// homes before declaring it absent.
		for _, p := range []string{"/usr/sbin/tcpdump", "/sbin/tcpdump", "/usr/local/sbin/tcpdump", "/usr/bin/tcpdump"} {
			if st, serr := os.Stat(p); serr == nil && !st.IsDir() {
				td = p
				break
			}
		}
		if td == "" {
			fatal("tcpdump not found on this host — install it (e.g. apt install tcpdump / pkg install tcpdump), or use the web admin's Monitor → Capture page")
		}
	}

	cmd := exec.Command(td, append([]string{"-i", iface}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl-C should stop the capture, not this wrapper: the SIGINT goes to
	// the whole foreground process group, tcpdump catches it, prints its
	// packet counts, and exits — ignore it here so gravinet is still around
	// to relay that exit code instead of dying first mid-stream.
	signal.Ignore(os.Interrupt)
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		fatal("tcpdump: %v", err)
	}
}

var meshGroup = []groupLeaf{
	{"networks", "define overlay networks: subnets, addressing, MTU", cmdNetwork},
	{"keys", "cryptographic keys used to authenticate this network's peers", cmdKey},
	{"seeds", "bootstrap addresses used to find and reconnect to peers", cmdSeed},
	{"peers", "live peer list for a network (via the daemon)", cmdMeshPeers},
	{"bans", "nodes blocked from joining or reconnecting: list|add|del", cmdMeshBans},
}

var trafficGroup = []groupLeaf{
	{"firewall", "rules controlling which traffic is allowed through the tunnel", cmdFW},
	{"nat", "port forwarding and address translation for tunnel traffic", cmdNAT},
	{"qos", "traffic prioritization and queuing order", cmdQoS},
	{"shaping", "rate limiting per peer or network (the flat form is \"bandwidth\"/\"bw\")", cmdBandwidth},
	{"routes", "additional subnets redistributed across the mesh", cmdRoute},
	{"bgp", "BGP and BFD configuration, applied to FRR", cmdTrafficBGP},
}

var namingGroup = []groupLeaf{
	{"dns", "conditional forwarding of specific domains to mesh DNS servers", cmdNamingDNS},
	{"hosts", "custom hostname records advertised to peers", cmdHost},
}

var monitorGroup = []groupLeaf{
	{"metrics", "live CPU, memory, disk, and per-overlay-interface throughput", cmdMonitorMetrics},
	{"mesh-peers", "live connection health for every peer (partial — see -h)", cmdMonitorMeshPeers},
	{"capture", "live packet capture on an overlay interface (runs tcpdump)", cmdMonitorCapture},
	{"speedtest", "measure throughput between this node and a managed peer", speedtestNotYetInCLI},
	{"latency", "round-trip time from this host to every other mesh peer", cmdLatency},
	{"route-table", "the live kernel routing table on this host", cmdMonitorRouteTable},
	{"bgp-peers", "live BGP peer sessions reported by FRR", cmdMonitorBGPPeers},
	{"hosts-file", "the live contents of this host's hosts file", cmdMonitorHostsFile},
	{"dns-state", "what's actually registered with this host's OS resolver", cmdMonitorDNSState},
	{"logs", "the daemon's recent log output", cmdMonitorLogs},
}

var infoGroup = []groupLeaf{
	{"upgrade", "check and apply a new gravinet binary on this node; local only", cmdUpgrade},
	{"readme", "project documentation", cmdInfoReadme},
	{"getting-started", "the full onboarding walkthrough", cmdInfoGettingStarted},
	{"license", "license information", cmdInfoLicense},
	{"about", "build and host identity", cmdInfoAbout},
}

// settingsGroup isn't a NAV_GROUPS entry — it mirrors the web admin's
// Settings page, reached from the gear icon rather than the left rail. As
// of v553 it carries the whole page, not just cluster mode: every row there
// with a config field behind it has a leaf here (see cli_settings.go's
// package comment for the live-vs-restart split and why each setting lands
// where it does). The one deliberate omission is Dark mode, a per-browser
// preference stored client-side with nothing in config.json to set.
var settingsGroup = []groupLeaf{
	{"managed", "get/set managed mode (be managed by a Manager-mode peer)", cmdManaged},
	{"manager", "get/set manager mode (manage other nodes)", cmdManager},
	{"shell", "allow a Manager peer to open a real OS shell here: on|off|status", cmdSettingsShell},
	{"log-level", "how much this node logs (error|warn|info|debug); applied live", cmdSettingsLogLevel},
	{"log-size", "log-file size cap (e.g. 200M, 1G); oldest lines drop first; applied live", cmdSettingsLogSize},
	{"route-adv", "route re-advertisement interval in seconds (0 = default); applied live", cmdSettingsRouteAdv},
	{"udp-port", "UDP listen port(s), comma-separated, first is primary; \"-\" turns UDP off", cmdSettingsUDPPort},
	{"tcp-port", "TCP/TLS fallback port(s), comma-separated; \"-\" turns the fallback off", cmdSettingsTCPPort},
	{"nat-state", "idle NAT connection timeout in seconds (0 = default 120s); applied live", cmdSettingsNATState},
	{"upnp", "ask the LAN router to forward this node's ports automatically: on|off|status", cmdSettingsUPnP},
	{"geoip", "approximate peer/seed locations via ipapi.co in the web admin: on|off|status", cmdSettingsGeoIP},
}

// cmdMeshPeers is "gravinet mesh peers" — the live peer list, the same data
// cmdList's "Peers" section shows (see printPeers in main.go), just on its
// own rather than bundled with bans and routes.
func cmdMeshPeers(args []string) {
	fs := flag.NewFlagSet("mesh peers", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	netID := fs.String("net", "", "network name or hex id; optional if only one")
	fs.Parse(args)
	resp, err := control.Do(*sock, control.Request{Cmd: "peers", Net: *netID})
	if err != nil {
		fatal("control: %v%s", err, controlDialHint())
	}
	if !resp.OK {
		fatal("%s", resp.Error)
	}
	printPeers(resp.Peers)
}

// cmdMonitorMeshPeers is "gravinet monitor mesh-peers". It's the same list
// as "gravinet mesh peers" — that's genuinely the closest CLI equivalent
// this has today. What it's missing next to the actual Monitor > Mesh Peers
// page: transport (tcp/udp), tx/rx byte counters, and clean/dirty session
// state, none of which mesh.PeerInfo or the control protocol carry — that's
// not something this can paper over locally, it needs those fields added to
// the protocol first.
func cmdMonitorMeshPeers(args []string) {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Println(`gravinet monitor mesh-peers: live peer list (node id, hostname, overlay addresses, endpoint, direct/relayed).

Note: the web admin's Monitor > Mesh Peers page additionally shows transport
(tcp/udp), tx/rx byte counters, and clean/dirty session state per peer. None
of that is available over the control socket today, so it can't be shown
here — this prints the same subset "gravinet mesh peers" does.`)
			return
		}
	}
	cmdMeshPeers(args)
}

// cmdMonitorRouteTable is "gravinet monitor route-table" — the live kernel
// routing table, read directly by this process rather than asking the
// daemon: it's plain host state (readProcRoutes/nativeRouteText, see
// webadmin.LocalRouteTableText), not anything the daemon tracks internally,
// so there's nothing gained by round-tripping through the control socket
// for it.
func cmdMonitorRouteTable(args []string) {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Println("gravinet monitor route-table: the live kernel routing table on this host (reads local OS state directly; no running daemon needed).")
			return
		}
	}
	text, err := webadmin.LocalRouteTableText()
	if err != nil {
		fatal("route table: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		fmt.Println("(empty)")
		return
	}
	fmt.Print(text)
}

// cmdMonitorHostsFile is "gravinet monitor hosts-file" — this host's actual
// hosts file (the same file the daemon writes peer/advertised records
// into), read directly rather than through the daemon: it's a fixed path
// (hosts.DefaultPath), not daemon-internal state.
func cmdMonitorHostsFile(args []string) {
	fs := flag.NewFlagSet("monitor hosts-file", flag.ExitOnError)
	fs.Parse(args)
	path := hosts.DefaultPath()
	b, err := os.ReadFile(path)
	if err != nil {
		fatal("%s: %v", path, err)
	}
	os.Stdout.Write(b)
}

// cmdMonitorDNSState is "gravinet monitor dns-state" — what's actually
// registered with this host's OS resolver right now, per network. Reads
// live via internal/resolver.Dump, the same as handleLocalDNS
// (internal/webadmin/sysinfo.go) — not from anything gravinet remembers
// applying, so it reflects reality even if a Sync silently failed.
//
// Unlike hosts-file/route-table, this does need one thing from the running
// daemon first: which kernel interface (e.g. "mesh0") each network is
// actually using right now, since that assignment happens at runtime and
// isn't derivable from config alone. That's the new "ifaces" control
// command (see internal/control/control.go), fetched here, then
// resolver.Dump does the rest locally.
func cmdMonitorDNSState(args []string) {
	fs := flag.NewFlagSet("monitor dns-state", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	fs.Parse(args)
	resp, err := control.Do(*sock, control.Request{Cmd: "ifaces"})
	if err != nil {
		fatal("control: %v%s", err, controlDialHint())
	}
	if !resp.OK {
		fatal("%s", resp.Error)
	}
	if len(resp.Ifaces) == 0 {
		fmt.Println("(no networks up)")
		return
	}
	for _, ifc := range resp.Ifaces {
		// Must match dnssync.go's tag derivation exactly (DNSTag is never
		// set from config today, so it always falls through to this form)
		// — same requirement handleLocalDNS's own comment notes.
		tag := fmt.Sprintf("%016x", ifc.NetworkID)
		text, derr := resolver.Dump(tag, ifc.Iface)
		fmt.Printf("== %s (%s) ==\n", ifc.Name, ifc.Iface)
		if derr != nil {
			fmt.Printf("  error: %v\n", derr)
			continue
		}
		if strings.TrimSpace(text) == "" {
			fmt.Println("  (nothing registered)")
			continue
		}
		fmt.Println(text)
	}
}

// cmdMonitorLogs is "gravinet monitor logs" — the tail of the daemon's log
// file, read directly off disk (same file, same path resolution —
// cfg.LogFilePath — cmdRun uses to wire this into the web admin's own Logs
// page) rather than through the control socket: it's a plain file, not
// daemon-internal state, so there's nothing the daemon needs to hand back
// that reading the file directly doesn't already give.
func cmdMonitorLogs(args []string) {
	fs := flag.NewFlagSet("monitor logs", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "config path")
	n := fs.Int("n", 200, "number of trailing lines to show")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config %s: %v", *cfgPath, err)
	}
	path := cfg.LogFilePath(*cfgPath)
	if path == "" {
		fatal("file logging is not enabled in this config")
	}
	f, err := os.Open(path)
	if err != nil {
		fatal("%s: %v", path, err)
	}
	defer f.Close()
	const maxRead = 1 << 20 // last 1 MiB, same cap handleLogs uses
	var buf []byte
	if fi, ferr := f.Stat(); ferr == nil {
		size := fi.Size()
		start := int64(0)
		if size > maxRead {
			start = size - maxRead
		}
		if _, serr := f.Seek(start, io.SeekStart); serr == nil {
			buf, _ = io.ReadAll(f)
		}
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(buf) == maxRead && len(lines) > 1 {
		lines = lines[1:] // drop a partial first line from seeking mid-file
	}
	if *n > 0 && len(lines) > *n {
		lines = lines[len(lines)-*n:]
	}
	for _, l := range lines {
		fmt.Println(l)
	}
}

// cmdMonitorBGPPeers is "gravinet monitor bgp-peers" — live BGP peer
// sessions reported by FRR, queried directly via vtysh (webadmin.RunVtysh,
// the same hardened, wedge-proof call the web page's handleBGP uses)
// rather than through the daemon: FRR is its own process gravinet just
// talks to, so there's no daemon-internal state involved either way.
func cmdMonitorBGPPeers(args []string) {
	fs := flag.NewFlagSet("monitor bgp-peers", flag.ExitOnError)
	fs.Parse(args)
	out, ok := webadmin.RunVtysh("show bgp summary")
	if !ok {
		fatal("FRR/vtysh is not installed, not running, or didn't answer in time")
	}
	os.Stdout.Write(out)
}

// cmdMonitorMetrics is "gravinet monitor metrics" — a single, instantaneous
// CPU/memory/disk/uptime/per-interface-throughput reading (webadmin.
// TakeHostSnapshot, the same readers Info → Metrics' graphs use — see that
// function's doc comment). Unlike the web page, this has no history: a
// separate, short-lived CLI process can't see the daemon's own rolling
// sample buffer, so this only ever reports "right now," not a graph over
// time. Takes about a second — CPU% and interface throughput both need two
// samples a second apart to mean anything, not just one.
func cmdMonitorMetrics(args []string) {
	fs := flag.NewFlagSet("monitor metrics", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	fs.Parse(args)
	resp, err := control.Do(*sock, control.Request{Cmd: "ifaces"})
	if err != nil {
		fatal("control: %v%s", err, controlDialHint())
	}
	if !resp.OK {
		fatal("%s", resp.Error)
	}
	snap := webadmin.TakeHostSnapshot(resp.Ifaces)
	printPct := func(label string, v float64, ok bool) {
		if !ok {
			fmt.Printf("%-8s n/a (not available on this platform)\n", label+":")
			return
		}
		fmt.Printf("%-8s %.1f%%\n", label+":", v)
	}
	printPct("CPU", snap.CPUPercent, snap.CPUOK)
	printPct("Memory", snap.MemPercent, snap.MemOK)
	printPct("Disk", snap.DiskPercent, snap.DiskOK)
	if snap.UptimeOK {
		fmt.Printf("%-8s %s\n", "Uptime:", formatUptime(snap.UptimeSeconds))
	} else {
		fmt.Printf("%-8s n/a\n", "Uptime:")
	}
	if len(snap.Ifaces) == 0 {
		fmt.Println("(no overlay interfaces up)")
		return
	}
	fmt.Println("Interfaces:")
	for _, ifc := range snap.Ifaces {
		fmt.Printf("  %-12s %-8s rx=%-12s tx=%s\n", ifc.Network, ifc.Iface, formatRate(ifc.RxBytesPerSec), formatRate(ifc.TxBytesPerSec))
	}
}

// formatRate renders a bytes/sec rate the way an operator reads it, not raw
// bytes — B, KB, MB, GB per second.
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

// formatUptime renders a duration the way an operator reads it (days,
// hours, minutes) rather than a raw second count.
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

// cmdMeshBans is "gravinet mesh bans [list|add|del] ...". list is new — the
// flat CLI never had a standalone way to see bans, only ever as part of
// "status"/cmdList's combined output. add/del wrap the existing ban/unban
// commands unchanged (same flags, same behavior), so "gravinet mesh bans add
// NODE -notes X" is exactly "gravinet ban NODE -notes X".
func cmdMeshBans(args []string) {
	if len(args) == 0 {
		cmdMeshBansList(nil)
		return
	}
	switch args[0] {
	case "list":
		cmdMeshBansList(args[1:])
	case "add":
		cmdBan(args[1:])
	case "del", "remove", "rm":
		cmdUnban(args[1:])
	case "-h", "--help":
		fmt.Println(`usage: gravinet mesh bans <list|add|del> ...
  list             show current bans (default with no subcommand)
  add <node-id>    ban a node — same as "gravinet ban"
  del <node-id>    unban a node — same as "gravinet unban"`)
	default:
		fatal("usage: gravinet mesh bans <list|add|del> ... (run with -h for details)")
	}
}

func cmdMeshBansList(args []string) {
	fs := flag.NewFlagSet("mesh bans list", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	netID := fs.String("net", "", "network name or hex id; optional if only one")
	fs.Parse(args)
	resp, err := control.Do(*sock, control.Request{Cmd: "bans", Net: *netID})
	if err != nil {
		fatal("control: %v%s", err, controlDialHint())
	}
	if !resp.OK {
		fatal("%s", resp.Error)
	}
	printBans(resp.Bans)
}

// printVersion backs both "gravinet version" and "gravinet info about" — one
// implementation, not two that could quietly report different things for
// what's meant to be the same question.
func printVersion() {
	pam := "no"
	if webadmin.PAMCompiledIn {
		pam = "yes"
	}
	// The trailing "pam=yes|no" is deliberately stable/parseable: the
	// install-*.sh scripts grep for it to find out whether a resolved
	// binary actually has PAM support, rather than inferring it after
	// the fact from ldd/otool/objdump output — a heuristic that can be
	// fooled by static linking (see docs/changelog.md for the bug that
	// prompted this). Only the binary itself reliably knows what it was
	// built with.
	fmt.Printf("gravinet %s (%s) %s/%s pam=%s\n", version, commit, runtime.GOOS, runtime.GOARCH, pam)
}

// cmdInfoAbout is "gravinet info about". The web admin's About tab
// (handleAbout, internal/webadmin/sysinfo.go) additionally reports
// os_version and go_version; those two aren't included here since they'd
// need an unexported webadmin helper (osVersion()) exported just for this,
// and pam= — which this already has and About doesn't show at all — is the
// one of the set a script actually greps for. Same core identity either way.
func cmdInfoAbout(args []string) {
	printVersion()
}

// docFilePath is the shape of *config.Config's ReadmePath/LicensePath/
// GettingStartedPath methods — matched via a method expression below so
// printDocFile resolves each path exactly the way cmdRun wires it into the
// web admin (see main.go's cmdRun, the SetReadmePath/SetLicensePath/
// SetGettingStartedPath calls), rather than a second guess at the same
// logic that could disagree with it.
type docFilePath func(cfg *config.Config, configPath, exeDir string) string

func cmdInfoReadme(args []string) { printDocFile(args, "readme", (*config.Config).ReadmePath) }
func cmdInfoGettingStarted(args []string) {
	printDocFile(args, "getting-started", (*config.Config).GettingStartedPath)
}
func cmdInfoLicense(args []string) { printDocFile(args, "license", (*config.Config).LicensePath) }

// printDocFile backs "gravinet info readme/getting-started/license" — the
// CLI equivalent of those three Info pages, which are themselves just files
// read fresh off disk on every request (serveDocFile in
// internal/webadmin/edit.go). Reused here the same way: read fresh, print
// verbatim, no caching.
func printDocFile(args []string, name string, pathFn docFilePath) {
	fs := flag.NewFlagSet("info "+name, flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "config path")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config %s: %v", *cfgPath, err)
	}
	exeDir := ""
	if exe, eerr := os.Executable(); eerr == nil {
		exeDir = filepath.Dir(exe)
	}
	path := pathFn(cfg, *cfgPath, exeDir)
	if path == "" {
		fatal("%s: no file found (nothing installed alongside this binary or this config)", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fatal("%s: %v", path, err)
	}
	os.Stdout.Write(b)
}

// cmdTrafficBGP is "gravinet traffic bgp <verb> ...", the config-file editor
// for cfg.BGP (a single global field — see config.go — not per-network like
// most of the rest of this file). Covers the core of what Traffic > BGP
// does: identity (ASN/router-id/timers), the enabled toggle, neighbors, and
// this node's own advertised networks. Deliberately does not cover the
// three redistribute-picker lists (RedistributeConnectedRoutes/
// RedistributeStaticRoutes/RedistributeMeshRoutes) — each needs
// cross-referencing something live (this host's current connected/static
// routes as FRR sees them, or the Mesh Routes advertise list) that would
// need its own plumbing; "show" still displays whatever's already
// configured there, just can't add to it yet.
func cmdTrafficBGP(args []string) {
	if len(args) == 0 {
		cmdTrafficBGPShow(nil)
		return
	}
	switch args[0] {
	case "show":
		cmdTrafficBGPShow(args[1:])
	case "enable":
		cmdTrafficBGPToggle(args[1:], true)
	case "disable":
		cmdTrafficBGPToggle(args[1:], false)
	case "set":
		cmdTrafficBGPSet(args[1:])
	case "neighbor":
		cmdTrafficBGPNeighbor(args[1:])
	case "advertise":
		cmdTrafficBGPAdvertise(args[1:])
	case "-h", "--help":
		fmt.Println(`usage: gravinet traffic bgp <verb> ...
  show                                   print the current BGP config (default with no verb)
  enable / disable                       toggle BGP on/off
  set [-asn N] [-router-id IP] [-keepalive N] [-hold N] [-as-prepend] [-auto-bgp]
  neighbor add <peer> <remote-as> [-description T] [-password P] [-bfd] [-shutdown]
  neighbor del <peer>
  advertise add <cidr>                   add one of this node's own advertised networks
  advertise del <cidr>

Not covered here: the redistribute-connected/static/mesh-routes pickers —
"show" displays them, but adding to them needs the web admin for now.`)
	default:
		fatal("usage: gravinet traffic bgp <show|enable|disable|set|neighbor|advertise> ... (run with -h for details)")
	}
}

func cmdTrafficBGPShow(args []string) {
	fs := flag.NewFlagSet("traffic bgp show", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "config path")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config %s: %v", *cfgPath, err)
	}
	b := cfg.BGP
	fmt.Printf("enabled:    %v\n", b.Enabled)
	fmt.Printf("asn:        %d\n", b.ASN)
	fmt.Printf("router_id:  %s\n", orNone(b.RouterID))
	fmt.Printf("keepalive:  %s\n", secOrDefault(b.KeepaliveTime))
	fmt.Printf("hold:       %s\n", secOrDefault(b.HoldTime))
	fmt.Printf("as_prepend: %v\n", b.ASPrepend)
	fmt.Printf("auto_bgp:   %v\n", b.AutoBGP)
	fmt.Printf("neighbors (%d):\n", len(b.Neighbors))
	for _, n := range b.Neighbors {
		flags := ""
		if n.BFD {
			flags += " bfd"
		}
		if n.Shutdown {
			flags += " shutdown"
		}
		desc := ""
		if n.Description != "" {
			desc = " (" + n.Description + ")"
		}
		fmt.Printf("  %-20s remote-as %-10d%s%s\n", n.Peer, n.RemoteAS, desc, flags)
	}
	fmt.Printf("advertised networks (%d): %s\n", len(b.Networks), strings.Join(orNoneList(b.Networks), ", "))
	fmt.Printf("redistribute connected (%d): %s\n", len(b.RedistributeConnectedRoutes), strings.Join(orNoneList(b.RedistributeConnectedRoutes), ", "))
	fmt.Printf("redistribute static (%d): %s\n", len(b.RedistributeStaticRoutes), strings.Join(orNoneList(b.RedistributeStaticRoutes), ", "))
	fmt.Printf("redistribute mesh routes (%d): %s\n", len(b.RedistributeMeshRoutes), strings.Join(orNoneList(b.RedistributeMeshRoutes), ", "))
}

func cmdTrafficBGPToggle(args []string, on bool) {
	fs := flag.NewFlagSet("traffic bgp enable/disable", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "config path")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config %s: %v", *cfgPath, err)
	}
	cfg.BGP.Enabled = on
	commitCfg(cfg, *cfgPath)
}

func cmdTrafficBGPSet(args []string) {
	fs := flag.NewFlagSet("traffic bgp set", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "config path")
	asn := fs.Uint("asn", 0, "local AS number (0 = leave unchanged)")
	routerID := fs.String("router-id", "", "BGP router ID (leave unset to leave unchanged)")
	keepalive := fs.Uint("keepalive", 0, "keepalive timer, seconds (0 = leave unchanged)")
	hold := fs.Uint("hold", 0, "hold timer, seconds (0 = leave unchanged)")
	asPrepend := fs.Bool("as-prepend", false, "prepend this node's ASN twice on outbound advertisements")
	autoBGP := fs.Bool("auto-bgp", false, "self-number and self-peer with every connected mesh peer")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config %s: %v", *cfgPath, err)
	}
	if *asn != 0 {
		cfg.BGP.ASN = uint32(*asn)
	}
	if *routerID != "" {
		cfg.BGP.RouterID = *routerID
	}
	if *keepalive != 0 {
		cfg.BGP.KeepaliveTime = uint32(*keepalive)
	}
	if *hold != 0 {
		cfg.BGP.HoldTime = uint32(*hold)
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "as-prepend" {
			cfg.BGP.ASPrepend = *asPrepend
		}
		if f.Name == "auto-bgp" {
			cfg.BGP.AutoBGP = *autoBGP
		}
	})
	commitCfg(cfg, *cfgPath)
}

func cmdTrafficBGPNeighbor(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet traffic bgp neighbor <add|del> ...")
	}
	switch args[0] {
	case "add":
		cmdTrafficBGPNeighborAdd(args[1:])
	case "del", "remove", "rm":
		cmdTrafficBGPNeighborDel(args[1:])
	default:
		fatal("usage: gravinet traffic bgp neighbor <add|del> ...")
	}
}

func cmdTrafficBGPNeighborAdd(args []string) {
	peer, rest := splitPositional(args)
	remoteASStr, rest2 := splitPositional(rest)
	fs := flag.NewFlagSet("traffic bgp neighbor add", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "config path")
	desc := fs.String("description", "", "neighbor description")
	password := fs.String("password", "", "MD5 session password")
	bfd := fs.Bool("bfd", false, "run BFD on this session")
	shutdown := fs.Bool("shutdown", false, "administratively shut this neighbor down")
	fs.Parse(rest2)
	if peer == "" || remoteASStr == "" {
		fatal("usage: gravinet traffic bgp neighbor add <peer-address> <remote-as> [-description T] [-password P] [-bfd] [-shutdown]")
	}
	remoteAS, err := strconv.ParseUint(remoteASStr, 10, 32)
	if err != nil {
		fatal("remote-as %q: %v", remoteASStr, err)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config %s: %v", *cfgPath, err)
	}
	for i, n := range cfg.BGP.Neighbors {
		if n.Peer == peer {
			// Replace in place rather than append a duplicate — same
			// "add == upsert" convention cmdHost/cmdSeed already use.
			cfg.BGP.Neighbors[i] = config.BGPNeighbor{Peer: peer, RemoteAS: uint32(remoteAS), Description: *desc, Password: *password, BFD: *bfd, Shutdown: *shutdown}
			commitCfg(cfg, *cfgPath)
			return
		}
	}
	cfg.BGP.Neighbors = append(cfg.BGP.Neighbors, config.BGPNeighbor{Peer: peer, RemoteAS: uint32(remoteAS), Description: *desc, Password: *password, BFD: *bfd, Shutdown: *shutdown})
	commitCfg(cfg, *cfgPath)
}

func cmdTrafficBGPNeighborDel(args []string) {
	peer, rest := splitPositional(args)
	fs := flag.NewFlagSet("traffic bgp neighbor del", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "config path")
	fs.Parse(rest)
	if peer == "" {
		fatal("usage: gravinet traffic bgp neighbor del <peer-address>")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config %s: %v", *cfgPath, err)
	}
	out := cfg.BGP.Neighbors[:0]
	found := false
	for _, n := range cfg.BGP.Neighbors {
		if n.Peer == peer {
			found = true
			continue
		}
		out = append(out, n)
	}
	if !found {
		fatal("no neighbor %q configured", peer)
	}
	cfg.BGP.Neighbors = out
	commitCfg(cfg, *cfgPath)
}

func cmdTrafficBGPAdvertise(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet traffic bgp advertise <add|del> <cidr>")
	}
	verb, rest := args[0], args[1:]
	cidr, rest2 := splitPositional(rest)
	fs := flag.NewFlagSet("traffic bgp advertise", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "config path")
	fs.Parse(rest2)
	if cidr == "" {
		fatal("usage: gravinet traffic bgp advertise <add|del> <cidr>")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("load config %s: %v", *cfgPath, err)
	}
	switch verb {
	case "add":
		for _, c := range cfg.BGP.Networks {
			if c == cidr {
				fmt.Println("already advertised")
				return
			}
		}
		cfg.BGP.Networks = append(cfg.BGP.Networks, cidr)
	case "del", "remove", "rm":
		out := cfg.BGP.Networks[:0]
		found := false
		for _, c := range cfg.BGP.Networks {
			if c == cidr {
				found = true
				continue
			}
			out = append(out, c)
		}
		if !found {
			fatal("%q is not currently advertised", cidr)
		}
		cfg.BGP.Networks = out
	default:
		fatal("usage: gravinet traffic bgp advertise <add|del> <cidr>")
	}
	commitCfg(cfg, *cfgPath)
}

func orNoneList(l []string) []string {
	if len(l) == 0 {
		return []string{"(none)"}
	}
	return l
}

func secOrDefault(v uint32) string {
	if v == 0 {
		return "(FRR default)"
	}
	return fmt.Sprintf("%ds", v)
}

// cmdNamingDNS is "gravinet naming dns <verb> ...", the config-file editor
// for a network's DNSAdvertise/DNSReject lists — conditional-forwarding
// rules this node advertises mesh-wide, and domains this node refuses to
// accept from peers. Mirrors cmdHost's shape exactly (same verbs, same
// -net resolution, same per-network struct pair), since DNSForward/
// DNSReject are the domain analog of HostRecord/HostReject — and reuses
// the same *config.Config mutator methods (DNSForwardAdd/Delete/
// SetEnabled, DNSRejectAdd/Delete/SetEnabled, already used by the web
// admin's own DNS editor) rather than a second copy of that logic.
func cmdNamingDNS(args []string) {
	cfg, path, rest := openCfg(args)
	netName, rest := extractOpt(rest, "net")
	if len(rest) == 0 {
		fatal("usage: gravinet naming dns <list|add DOMAIN SERVERS|remove DOMAIN|enable DOMAIN|disable DOMAIN|reject DOMAIN|reject-remove DOMAIN|reject-enable DOMAIN|reject-disable DOMAIN> [-net NAME]")
	}
	sub, rest := rest[0], rest[1:]
	n := pickNetwork(cfg, netName)
	switch sub {
	case "list":
		fmt.Printf("network %s advertised forwards:\n", n.Name)
		if len(n.DNSAdvertise) == 0 {
			fmt.Println("  (none)")
		}
		for _, d := range n.DNSAdvertise {
			fmt.Printf("  %-30s -> %-30s %s\n", d.Domain, strings.Join(d.Servers, ","), onOff(!d.Disabled))
		}
		fmt.Printf("network %s rejected forwards (refused from peers):\n", n.Name)
		if len(n.DNSReject) == 0 {
			fmt.Println("  (none)")
		}
		for _, d := range n.DNSReject {
			fmt.Printf("  %-30s %s\n", d.Domain, onOff(!d.Disabled))
		}
		return
	case "add":
		if len(rest) < 2 {
			fatal("usage: gravinet naming dns add DOMAIN SERVERS (comma-separated)")
		}
		if err := cfg.DNSForwardAdd(netName, rest[0], rest[1]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("advertising %s -> %s on %s\n", rest[0], rest[1], n.Name)
	case "remove", "delete", "del":
		if len(rest) < 1 {
			fatal("usage: gravinet naming dns remove DOMAIN")
		}
		if err := cfg.DNSForwardDelete(netName, rest[0]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("stopped advertising %s on %s\n", rest[0], n.Name)
	case "enable", "disable":
		if len(rest) < 1 {
			fatal("usage: gravinet naming dns %s DOMAIN", sub)
		}
		if err := cfg.DNSForwardSetEnabled(netName, rest[0], sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd advertising %s on %s\n", sub, rest[0], n.Name)
	case "reject":
		if len(rest) < 1 {
			fatal("usage: gravinet naming dns reject DOMAIN")
		}
		if err := cfg.DNSRejectAdd(netName, rest[0]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("rejecting forward %s on %s\n", rest[0], n.Name)
	case "reject-remove":
		if len(rest) < 1 {
			fatal("usage: gravinet naming dns reject-remove DOMAIN")
		}
		if err := cfg.DNSRejectDelete(netName, rest[0]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("stopped rejecting forward %s on %s\n", rest[0], n.Name)
	case "reject-enable", "reject-disable":
		if len(rest) < 1 {
			fatal("usage: gravinet naming dns %s DOMAIN", sub)
		}
		if err := cfg.DNSRejectSetEnabled(netName, rest[0], sub == "reject-enable"); err != nil {
			fatal("%v", err)
		}
		verb := "enabled"
		if sub == "reject-disable" {
			verb = "disabled"
		}
		fmt.Printf("%s forward reject %s on %s\n", verb, rest[0], n.Name)
	default:
		fatal("unknown: gravinet naming dns %s", sub)
	}
	commitCfg(cfg, path)
}
