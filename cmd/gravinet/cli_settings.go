package main

// The rest of the "gravinet settings" group (v553) — everything on the web
// admin's Settings page beyond managed/manager (which predate this file and
// live in cli_config.go). Each command here edits the same config field the
// corresponding Settings row's handler does and applies it the same way:
// live via commitCfg (save + daemon reload) for the settings the running
// daemon picks up from reloadFn — log level/size, route advertisement,
// UDP/TCP ports, NAT state timeout — and via commitCfgStructural (save +
// service restart, with the house-standard --no-restart opt-out) for the
// three the daemon or web admin only reads at startup: remote shell, UPnP,
// and Geo-IP. Which bucket each lands in isn't decided here — it mirrors
// exactly what each setting's web-admin handler reports (restart:false vs
// restart:true) and *why*, so the two front doors can't drift apart on
// semantics; see handleLogLevel, handleLogSize, handleRouteAdv, handlePort,
// handleTCPPort, handleNATState (live) and handleShellSetting,
// handleUPnPSetting, handleGeoIPSetting (restart) in internal/webadmin.
//
// Dark mode — the one remaining Settings row — is deliberately absent: it's
// a per-browser preference stored client-side, not node config; there is
// nothing in config.json for a CLI to set.

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/ddns"
	"gravinet/internal/service"
)

// settingsLogLevels mirrors internal/webadmin/loglevel.go's logLevels.
var settingsLogLevels = []string{"error", "warn", "info", "debug"}

// settingsAction pops a leading action word (on/off/enable/disable/status)
// off args, defaulting to "status" — the same shape cmdManaged/cmdManager
// use, shared here instead of re-pasted into each toggle below.
func settingsAction(args []string) (string, []string) {
	if len(args) > 0 {
		if len(args) > 0 {
			args[0] = expandVerb(args[0], v("on", "off", "enable", "disable", "status"))
		}
		switch args[0] {
		case "on", "off", "enable", "disable", "status":
			return args[0], args[1:]
		}
	}
	return "status", args
}

// cmdSettingsShell toggles AllowRemoteShell — whether a Manager peer may
// open a real OS shell on this node through the web admin. Restart-needed:
// the flag is captured into webadmin.Server.cfg once at startup (see
// handleShellSetting's doc comment), same as Geo-IP below. Note the web
// admin deliberately refuses to let a *remote* Manager flip this on; a
// local root CLI is exactly who is allowed to.
func cmdSettingsShell(args []string) {
	noRestart, args := hasFlag(args, "no-restart")
	cfg, path, rest := openCfg(args)
	action, _ := settingsAction(rest)
	switch action {
	case "on", "enable":
		cfg.WebAdmin.AllowRemoteShell = true
		fmt.Println("remote shell ON — a Manager peer may open a real OS shell on this node (running as the daemon's user, typically root) through the web admin")
		commitCfgStructural(cfg, path, noRestart)
	case "off", "disable":
		cfg.WebAdmin.AllowRemoteShell = false
		fmt.Println("remote shell OFF")
		commitCfgStructural(cfg, path, noRestart)
	default:
		st := "off"
		if cfg.WebAdmin.AllowRemoteShell {
			st = "on"
		}
		fmt.Printf("remote shell: %s\n", st)
	}
}

// cmdSettingsUPnP toggles the UPnP IGD port-forwarding helper. Restart-
// needed: the upnp.Manager mapping this node's listen ports is only ever
// built alongside those ports' transports at daemon startup (see
// handleUPnPSetting's doc comment).
func cmdSettingsUPnP(args []string) {
	noRestart, args := hasFlag(args, "no-restart")
	cfg, path, rest := openCfg(args)
	action, _ := settingsAction(rest)
	switch action {
	case "on", "enable":
		cfg.EnableUPnP = true
		fmt.Println("UPnP ON — on startup this node asks the LAN router to forward every port it listens on (UDP, TCP, extras) from the WAN side automatically; best-effort, a silent no-op if the router doesn't offer UPnP")
		commitCfgStructural(cfg, path, noRestart)
	case "off", "disable":
		cfg.EnableUPnP = false
		fmt.Println("UPnP OFF")
		commitCfgStructural(cfg, path, noRestart)
	default:
		st := "off"
		if cfg.EnableUPnP {
			st = "on"
		}
		fmt.Printf("upnp: %s\n", st)
	}
}

// cmdSettingsGeoIP toggles the peer/seed info panel's Geo-IP lookup (a
// third-party service, ipapi.co; on by default). Restart-needed: like
// AuthMode and the admin user list, the value is captured into the web
// admin server once at startup (see handleGeoIPSetting's doc comment).
func cmdSettingsGeoIP(args []string) {
	noRestart, args := hasFlag(args, "no-restart")
	cfg, path, rest := openCfg(args)
	action, _ := settingsAction(rest)
	set := func(on bool) {
		cfg.WebAdmin.GeoIPLookup = &on
	}
	switch action {
	case "on", "enable":
		set(true)
		fmt.Println("geo-IP lookups ON — the web admin's peer/seed info panels show an approximate location, looked up from ipapi.co")
		commitCfgStructural(cfg, path, noRestart)
	case "off", "disable":
		set(false)
		fmt.Println("geo-IP lookups OFF")
		commitCfgStructural(cfg, path, noRestart)
	default:
		st := "off"
		if cfg.WebAdmin.GeoIPEnabled() {
			st = "on (default)"
			if cfg.WebAdmin.GeoIPLookup != nil {
				st = "on"
			}
		}
		fmt.Printf("geo-ip lookups: %s\n", st)
	}
}

// cmdSettingsLogLevel gets/sets the daemon's log level. Applied live —
// this exists in the web admin precisely because a restart destroys the
// mesh state you raised the level to observe (handleLogLevel's doc
// comment); the CLI keeps that property via commitCfg's reload.
func cmdSettingsLogLevel(args []string) {
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		lvl := cfg.LogLevel
		if lvl == "" {
			lvl = "info (default)"
		}
		fmt.Printf("log level: %s   (available: %s)\n", lvl, strings.Join(settingsLogLevels, ", "))
		return
	}
	want := strings.ToLower(rest[0])
	ok := false
	for _, l := range settingsLogLevels {
		if l == want {
			ok = true
		}
	}
	if !ok {
		fatal("unknown log level %q; want one of %s", rest[0], strings.Join(settingsLogLevels, ", "))
	}
	cfg.LogLevel = want
	fmt.Printf("log level -> %s (applied live; no restart)\n", want)
	commitCfg(cfg, path)
}

// cmdSettingsLogSize gets/sets the log-file size cap ("200M", "1G", "99K",
// or a bare byte count). Applied live; a shrink trims the on-disk file
// immediately (the reload calls the rotating file's SetMaxBytes — see
// handleLogSize). Setting it clears the legacy LogMaxMB/LogKeep fields the
// same way the handler does, so the single-file FIFO cap is unambiguously
// in charge.
func cmdSettingsLogSize(args []string) {
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		fmt.Printf("log size cap: %s\n", cfg.LogMaxSizeString())
		return
	}
	b, err := config.ParseSize(rest[0])
	if err != nil {
		fatal("invalid size %q: %v (try 200M, 1G, 99K)", rest[0], err)
	}
	canonical := config.FormatSize(b)
	cfg.LogMaxSize = canonical
	cfg.LogMaxMB = 0
	cfg.LogKeep = 0
	fmt.Printf("log size cap -> %s (applied live; oldest lines drop first once full)\n", canonical)
	commitCfg(cfg, path)
}

// cmdSettingsRouteAdv gets/sets the route re-advertisement interval in
// seconds (0 = default). Applied live, same range check as handleRouteAdv.
func cmdSettingsRouteAdv(args []string) {
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		fmt.Printf("route advertisement interval: %d (effective: %ds)\n",
			cfg.RouteAdvInterval, int(cfg.RouteAdvDuration().Seconds()))
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil || n < 0 || n > 86400 {
		fatal("interval must be a number of seconds between 0 and 86400 (0 = default)")
	}
	cfg.RouteAdvInterval = n
	fmt.Printf("route advertisement interval -> %d (applied live)\n", n)
	commitCfg(cfg, path)
}

// cmdSettingsNATState gets/sets the global idle lifetime of a tracked NAT
// connection before its mapping is reclaimed. 0 restores the default
// (120s). Applied live; the range check lives in NATStateTimeoutSet, the
// same helper handleNATState uses.
func cmdSettingsNATState(args []string) {
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		if cfg.NATStateTimeout == 0 {
			fmt.Println("nat state timeout: default (120s)")
		} else {
			fmt.Printf("nat state timeout: %ds\n", cfg.NATStateTimeout)
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		fatal("timeout must be a number of seconds (0 = default 120s)")
	}
	if err := cfg.NATStateTimeoutSet(n); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("nat state timeout -> %d (applied live; 0 means the 120s default)\n", n)
	commitCfg(cfg, path)
}

// parsePortList parses "65432" or "65432,443,80" into a validated []int.
func parsePortList(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a port number", part)
		}
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("port %d must be between 1 and 65535", p)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one port is required")
	}
	return out, nil
}

// fmtPortList renders a primary port plus extras the way the setters accept
// them, so status output round-trips as input.
func fmtPortList(primary int, extras []int) string {
	parts := []string{strconv.Itoa(primary)}
	for _, p := range extras {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ",")
}

// cmdSettingsUDPPort gets/sets the UDP underlay port(s): the first becomes
// a plain comma-separated list, or "-" to turn UDP off entirely. There is no
// primary and no extras: the node listens on every port in the list and a
// peer may reach it on any of them. The first entry is what this node
// advertises to peers as its canonical port, which is presentation, not
// precedence. Applied live (the daemon rebinds on reload); mirrors handlePort,
// including refusing to turn UDP off while TCP is also off.
func cmdSettingsUDPPort(args []string) {
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		printPortList("udp", cfg.UDPPortList())
		return
	}
	if rest[0] == "-" {
		if !cfg.TCPEnabled() {
			fatal("can't turn off the UDP ports while TCP is also off — at least one transport must stay on")
		}
		cfg.UDPPorts = []int{} // explicit empty, not null: see UDPPorts' doc comment
		fmt.Println("udp -> off (TCP only)")
		commitCfg(cfg, path)
		return
	}
	ports, err := parsePortList(rest[0])
	if err != nil {
		fatal("%v (comma-separated, e.g. 65432,443 — or \"-\" to turn UDP off)", err)
	}
	cfg.UDPPorts = ports
	fmt.Printf("udp port(s) -> %s (applied live, the daemon rebinds)\n", joinPorts(ports))
	commitCfg(cfg, path)
}

// cmdSettingsTCPPort is cmdSettingsUDPPort's TCP counterpart, and deliberately
// identical in shape: a list of ports, "-" to turn TCP off. Neither list is
// derived from the other, which is the point — a peer's TCP port is a fact
// about that peer, and the old primary/fallback framing kept inviting the code
// to guess it from something else (see v788).
func cmdSettingsTCPPort(args []string) {
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		printPortList("tcp", cfg.TCPPortList())
		return
	}
	if rest[0] == "-" {
		if !cfg.UDPEnabled() {
			fatal("can't turn off the TCP ports while UDP is also off — at least one transport must stay on")
		}
		cfg.TCPPorts = []int{} // explicit empty, not null: see UDPPorts' doc comment
		fmt.Println("tcp -> off (UDP only)")
		commitCfg(cfg, path)
		return
	}
	ports, err := parsePortList(rest[0])
	if err != nil {
		fatal("%v (comma-separated, e.g. 65432,443 — or \"-\" to turn TCP off)", err)
	}
	cfg.TCPPorts = ports
	fmt.Printf("tcp port(s) -> %s (applied live, the daemon rebinds)\n", joinPorts(ports))
	commitCfg(cfg, path)
}

func printPortList(proto string, ports []int) {
	if len(ports) == 0 {
		fmt.Printf("%s: off (\"-\")\n", proto)
		return
	}
	fmt.Printf("%s port(s): %s\n", proto, joinPorts(ports))
}

func joinPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------------------
// The rest of the Settings page (v781). Everything above predates this block;
// what follows closes the gap between this group and the web admin's Settings
// page, which had grown from 11 rows with a CLI leaf to 27 rows with 11. Each
// leaf below edits the same config field its Settings row's handler does, and
// commits the same way that handler reports: commitCfg for the ones the
// running daemon picks up from reloadFn, commitCfgStructural for the ones
// only read at startup. As above, which bucket each lands in is not decided
// here — it's copied from the handler's own editResult(w, err, restart) call,
// so the two front doors can't drift apart on semantics.
//
// Two Settings rows still have no CLI form, both deliberately: Dark mode (a
// per-browser preference; nothing in config.json behind it) and TLS
// certificate upload/reset (two PEM blobs, and the cert the operator's own
// browser is trusting — the web handler pointedly refuses to auto-restart for
// exactly that reason, and a flag-driven version of it is a worse place to
// make that mistake, not a better one).

// settingsIntSet is the shared body of every plain integer setting here:
// print-on-no-args, otherwise parse, range-check, assign, commit. Factored
// out because there are now eight of these and eight hand-copied versions is
// eight places for a range check to be subtly wrong. set returns an error so
// a field with a config-side setter (ConfigHistoryLimitSet) can use it as-is
// rather than duplicating its bounds here.
func settingsIntSet(args []string, label, unit string, show func(*config.Config) string, set func(*config.Config, int) error, structural bool) {
	noRestart, args := hasFlag(args, "no-restart")
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		fmt.Printf("%s: %s\n", label, show(cfg))
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		fatal("%s must be a number%s", label, unit)
	}
	if err := set(cfg, n); err != nil {
		fatal("%v", err)
	}
	if structural {
		fmt.Printf("%s -> %d (needs a restart to take effect)\n", label, n)
		commitCfgStructural(cfg, path, noRestart)
		return
	}
	fmt.Printf("%s -> %d (applied live)\n", label, n)
	commitCfg(cfg, path)
}

// settingsBoolSet is settingsIntSet's counterpart for on/off rows, including
// the *bool fields whose nil means "default" (IPForwarding,
// DisableRedirects, EnableUDPGSO) — status therefore has three states to
// report, not two, and says which way the default falls rather than folding
// an unset field into a bare "off".
func settingsBoolSet(args []string, label string, show func(*config.Config) string, set func(*config.Config, bool), structural bool) {
	noRestart, args := hasFlag(args, "no-restart")
	cfg, path, rest := openCfg(args)
	action, _ := settingsAction(rest)
	switch action {
	case "on", "enable":
		set(cfg, true)
	case "off", "disable":
		set(cfg, false)
	default:
		fmt.Printf("%s: %s\n", label, show(cfg))
		return
	}
	on := action == "on" || action == "enable"
	if structural {
		fmt.Printf("%s -> %s (needs a restart to take effect)\n", label, boolWord(on, "on", "off"))
		commitCfgStructural(cfg, path, noRestart)
		return
	}
	fmt.Printf("%s -> %s (applied live)\n", label, boolWord(on, "on", "off"))
	commitCfg(cfg, path)
}

// tristate renders a *bool config field for status output: explicitly set
// either way, or unset with the default spelled out.
func tristate(p *bool, dflt bool) string {
	if p == nil {
		return "default (" + boolWord(dflt, "on", "off") + ")"
	}
	return boolWord(*p, "on", "off")
}

// cmdSettingsKeepalive gets/sets how often this node pings each connected
// peer — what keeps NAT mappings open and produces the RTT the peer list
// shows. Applied live (handleKeepalive: editResult(..., false)).
func cmdSettingsKeepalive(args []string) {
	settingsIntSet(args, "keepalive interval", " of seconds",
		func(c *config.Config) string {
			return fmt.Sprintf("%d (effective: %ds)", c.KeepaliveInterval, int(c.KeepaliveDuration().Seconds()))
		},
		func(c *config.Config, n int) error {
			if n < 0 || n > 86400 {
				return fmt.Errorf("keepalive interval must be 0..86400 seconds (0 = default)")
			}
			c.KeepaliveInterval = n
			return nil
		}, false)
}

// cmdSettingsPeerTimeout gets/sets how long a peer may go silent before its
// session is dropped. Applied live. Note the clamp the web page surfaces too:
// an explicit value below the keepalive interval is silently raised to it (see
// config.PeerTimeoutDuration), which is why status prints both numbers.
func cmdSettingsPeerTimeout(args []string) {
	settingsIntSet(args, "peer timeout", " of seconds",
		func(c *config.Config) string {
			return fmt.Sprintf("%d (effective: %ds)", c.PeerTimeout, int(c.PeerTimeoutDuration().Seconds()))
		},
		func(c *config.Config, n int) error {
			if n < 0 || n > 86400 {
				return fmt.Errorf("peer timeout must be 0..86400 seconds (0 = default)")
			}
			c.PeerTimeout = n
			return nil
		}, false)
}

// cmdSettingsHistoryLimit caps how many config snapshots are kept before the
// oldest are pruned. Applied live; bounds live in ConfigHistoryLimitSet, the
// same helper handleConfigHistoryLimit uses.
func cmdSettingsHistoryLimit(args []string) {
	settingsIntSet(args, "config history limit", "",
		func(c *config.Config) string {
			if c.ConfigHistoryLimit == 0 {
				return "default"
			}
			return strconv.Itoa(c.ConfigHistoryLimit)
		},
		(*config.Config).ConfigHistoryLimitSet, false)
}

// cmdSettingsWorkerThreads sets the outbound-TUN/inbound-UDP worker pool
// size; 0 means runtime.NumCPU()-1 (min 1). Restart-needed: the pools are
// sized once, when the engine and transport are constructed.
func cmdSettingsWorkerThreads(args []string) {
	settingsIntSet(args, "worker threads", "",
		func(c *config.Config) string {
			if c.WorkerThreads == 0 {
				return fmt.Sprintf("default (effective: %d)", c.WorkerThreadsValue())
			}
			return strconv.Itoa(c.WorkerThreads)
		},
		func(c *config.Config, n int) error {
			// 128 mirrors handleWorkerThreads' own workerThreadsMax: headroom
			// over any sane core count, there to catch a fat-fingered value.
			if n < 0 || n > 128 {
				return fmt.Errorf("worker_threads must be 0..128 (0 = one per core, less one)")
			}
			c.WorkerThreads = n
			return nil
		}, true)
}

// cmdSettingsTunQueues sets how many IFF_MULTI_QUEUE read queues to open on
// each overlay interface. Linux-only, a harmless no-op elsewhere.
// Restart-needed: the queue count is fixed when the TUN device is opened.
func cmdSettingsTunQueues(args []string) {
	settingsIntSet(args, "tun queues", "",
		func(c *config.Config) string {
			if c.TunQueues == 0 {
				return "default"
			}
			return strconv.Itoa(c.TunQueues)
		},
		func(c *config.Config, n int) error {
			if n < 0 || n > 64 { // mirrors handleTunQueues' tunQueuesMax
				return fmt.Errorf("tun_queues must be 0..64 (0 = default)")
			}
			c.TunQueues = n
			return nil
		}, true)
}

// cmdSettingsSocketBuffer sets the per-UDP-socket receive/send buffer target,
// in megabytes — the same unit the Settings card uses, and the reason to store
// the MB figure rather than bytes is that the config file then reads back as
// the number the operator typed (config.SocketBuffer accepts either; see
// SocketBufferMBThreshold). Restart-needed: set with setsockopt at bind time.
func cmdSettingsSocketBuffer(args []string) {
	maxMB := config.SocketBufferMaxBytes >> 20
	settingsIntSet(args, "socket buffer", " of megabytes",
		func(c *config.Config) string {
			if c.SocketBuffer == 0 {
				return fmt.Sprintf("default (effective: %d MB)", c.SocketBufferValue()>>20)
			}
			return fmt.Sprintf("%d MB (effective: %d MB)", c.SocketBuffer, c.SocketBufferValue()>>20)
		},
		func(c *config.Config, n int) error {
			if n < 0 || n > maxMB {
				return fmt.Errorf("socket buffer must be 0..%d MB (0 = default)", maxMB)
			}
			c.SocketBuffer = n
			return nil
		}, true)
}

// cmdSettingsUDPGSO toggles UDP segmentation/receive offload on the underlay
// socket — batching several packets per syscall. Experimental; Linux
// amd64/arm64 only, a no-op elsewhere. Restart-needed: initGSO runs once,
// when the transport is opened.
func cmdSettingsUDPGSO(args []string) {
	settingsBoolSet(args, "udp gso/gro",
		func(c *config.Config) string { return tristate(c.EnableUDPGSO, false) },
		func(c *config.Config, on bool) { c.EnableUDPGSO = &on }, true)
}

// cmdSettingsIPForwarding toggles whether the daemon turns on host IPv4/IPv6
// forwarding at startup — the on-ramp for redistributed routes and NAT. On by
// default; the previous value is restored on a clean shutdown.
// Restart-needed: there is no live "turn host forwarding on/off" hook, the
// sysctls are only ever flipped at startup.
func cmdSettingsIPForwarding(args []string) {
	settingsBoolSet(args, "ip forwarding",
		func(c *config.Config) string { return tristate(c.IPForwarding, true) },
		func(c *config.Config, on bool) { c.IPForwarding = &on }, true)
}

// cmdSettingsIPRedirects toggles whether the daemon turns OFF host acceptance
// and sending of ICMP redirects at startup. On by default (i.e. redirects are
// disabled by default) — note the double negative the config field name
// carries, which is why "on" here means "redirects are suppressed", matching
// the Settings row's own label. Restart-needed, same reason as forwarding.
func cmdSettingsIPRedirects(args []string) {
	settingsBoolSet(args, "disable icmp redirects",
		func(c *config.Config) string { return tristate(c.DisableRedirects, true) },
		func(c *config.Config, on bool) { c.DisableRedirects = &on }, true)
}

// cmdSettingsAcceptMgrUpgrades toggles whether a directly-connected Manager
// peer may push and apply a new gravinet binary to this node. Off by default,
// and deliberately local-only — no remote peer can turn this on, which is the
// whole point of the switch. Applied live: the remote-apply gate re-reads it
// from the config file on every push.
func cmdSettingsAcceptMgrUpgrades(args []string) {
	settingsBoolSet(args, "accept manager-pushed upgrades",
		func(c *config.Config) string { return boolWord(c.Upgrade.AcceptManagerUpgrades, "on", "off") },
		func(c *config.Config, on bool) { c.Upgrade.AcceptManagerUpgrades = on }, false)
}

// cmdSettingsLoginBan gets/sets the web admin's login lockout policy: how
// many failed attempts from one source trigger a lockout, and how long it
// lasts. Two numbers rather than two leaves, because they're one policy and
// WebAdminLoginBanSet — the same helper handleLoginBan uses, which owns the
// bounds — takes them together. Restart-needed: the throttle is built from
// these values once, when the web admin server starts.
func cmdSettingsLoginBan(args []string) {
	noRestart, args := hasFlag(args, "no-restart")
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		b := cfg.WebAdmin.LoginBan
		fmt.Printf("login lockout: %d attempt(s) (effective: %d), %ds (effective: %ds)\n",
			b.MaxFailures, b.EffectiveMaxFailures(), b.BanSeconds, b.EffectiveBanSeconds())
		return
	}
	if len(rest) != 2 {
		fatal("settings login-ban ATTEMPTS SECONDS   (0 for either restores its default)")
	}
	attempts, err1 := strconv.Atoi(rest[0])
	secs, err2 := strconv.Atoi(rest[1])
	if err1 != nil || err2 != nil {
		fatal("both attempts and seconds must be numbers")
	}
	if err := cfg.WebAdminLoginBanSet(attempts, secs); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("login lockout -> %d attempt(s), %ds (needs a restart to take effect)\n", attempts, secs)
	commitCfgStructural(cfg, path, noRestart)
}

// cmdSettingsListenAddrs picks which IP addresses the web admin binds.
//
//	gravinet settings listen-addrs                    show the current set
//	gravinet settings listen-addrs 127.0.0.1,10.0.0.5 replace it
//	gravinet settings listen-addrs default            back to loopback + mesh
//
// This one earns its CLI more than most settings do: it is the setting that
// can take the web admin away from you, and the console is then the only way
// back. Deliberately a whole-set replace rather than add/remove — the web
// admin's picker writes the whole set too, so both spell the same operation
// and neither can half-apply a change to which addresses answer.
func cmdSettingsListenAddrs(args []string) {
	cfg, path, rest := openCfg(args)
	port := cfg.WebAdminPort()
	if len(rest) == 0 || rest[0] == "status" {
		if len(cfg.ListenAddrsRaw()) == 0 {
			fmt.Printf("listen addresses: default (loopback + this node's mesh addresses), port %d\n", port)
			return
		}
		fmt.Printf("listen addresses (port %d):\n", port)
		for _, a := range cfg.ListenAddrsRaw() {
			fmt.Printf("  %s\n", a)
		}
		return
	}
	if rest[0] == "default" || rest[0] == "-" {
		cfg.WebAdmin.ListenAddrs = nil
		fmt.Println("listen addresses -> default (loopback + this node's mesh addresses)")
		commitCfg(cfg, path)
		return
	}
	var addrs []string
	for _, s := range strings.Split(rest[0], ",") {
		if s = strings.TrimSpace(s); s != "" {
			addrs = append(addrs, s)
		}
	}
	clean, err := validateListenAddrList(addrs)
	if err != nil {
		fatal("%v", err)
	}
	cfg.WebAdmin.ListenAddrs = clean
	// Keep Listen's host consistent with the pick list, preferring loopback as
	// the primary bind for the same reason the web admin does: it is the one
	// address that cannot stop existing underneath the daemon.
	if _, ps, e := net.SplitHostPort(cfg.WebAdmin.Listen); e == nil {
		lead := clean[0]
		for _, a := range clean {
			if ip, err := netip.ParseAddr(a); err == nil && ip.IsLoopback() {
				lead = a
				break
			}
		}
		cfg.WebAdmin.Listen = net.JoinHostPort(lead, ps)
	}
	fmt.Printf("listen addresses -> %s (port %d); restart to apply\n", strings.Join(clean, ", "), port)
	commitCfg(cfg, path)
}

// validateListenAddrList mirrors the web admin's validateListenAddrs: IP
// literals only (a name would silently move what the admin interface is
// exposed on if it later resolved differently), no link-locals (they need a
// zone to bind), and never an empty set.
func validateListenAddrList(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range in {
		ip, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address (names aren't accepted here — this binds a socket)", raw)
		}
		if ip.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("%s is link-local and needs a zone to bind", ip)
		}
		k := ip.Unmap().String()
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("give at least one address, or \"default\" for loopback + mesh")
	}
	return out, nil
}

// ddnsParamsForCheck assembles the same inputs a registration run uses, from
// the same places: the three network facts from the host's live resolver
// settings, and the key from config.
//
// Nothing here has to reconstruct what the daemon would do with interfaces any
// more. Through v1004 it did — the daemon passed the registrar the overlay
// devices its engine had up, and from a terminal there is no engine to ask, so
// this had to derive the same list from the config and the two could disagree.
// Every interface is published now, so both callers pass the same thing:
// nothing.
func ddnsParamsForCheck(cfg *config.Config) (ddns.Params, error) {
	info := service.HostResolver()
	host := strings.TrimSpace(info.Hostname)
	domain := strings.TrimSpace(info.SearchDomain)
	var servers []string
	for _, s := range info.DNSServers {
		if s = strings.TrimSpace(s); s != "" {
			servers = append(servers, s)
		}
	}

	var missing []string
	if host == "" {
		missing = append(missing, "a hostname")
	}
	if domain == "" {
		missing = append(missing, "a search domain")
	}
	if len(servers) == 0 {
		missing = append(missing, "at least one DNS server")
	}
	if len(missing) > 0 {
		return ddns.Params{}, fmt.Errorf("this node has no %s set; see `gravinet system resolver`", strings.Join(missing, " and no "))
	}

	key, err := ddns.ParseKey(cfg.DDNS.TSIGKey)
	if err != nil {
		return ddns.Params{}, err
	}
	return ddns.Params{
		Hostname: host,
		Domain:   domain,
		Servers:  servers,
		TTL:      uint32(cfg.DDNS.TTL),
		Key:      key,
		Reverse:  cfg.DDNS.ReverseEnabled(),
	}, nil
}

// cmdSettingsDDNS is "gravinet settings ddns" — dynamic DNS self-registration.
//
// One leaf for the whole block rather than four, which is the exception to how
// the rest of this group is arranged, and the reason is that three of the four
// fields are meaningless alone: a TTL on a node with no interval set changes
// nothing, and an interval with the wrong key set is the failure this is
// supposed to make visible. Printing them together is what lets a read at 3am
// answer "is this node registering, and if not why not" in one line.
//
// The key is written but never printed. It is a shared secret, and a terminal
// is a place things get pasted into tickets.
func cmdSettingsDDNS(args []string) {
	cfg, path, rest := openCfg(args)
	d := &cfg.DDNS

	if len(rest) == 0 {
		state := "off"
		if d.Active() {
			state = fmt.Sprintf("every %d minute(s)", d.IntervalMinutes)
		}
		fmt.Printf("dynamic dns registration: %s\n", state)
		fmt.Printf("  ttl:      %s\n", orDash(ttlLabel(d.TTL)))
		fmt.Printf("  reverse:  %s\n", onOff(d.ReverseEnabled()))
		fmt.Printf("  tsig key: %s\n", tsigLabel(d.TSIGKey))
		// The three inputs the run needs, read from the host rather than from
		// this file — the same values System > Resolver shows, and the reason
		// a node with an interval set can still be publishing nothing.
		info := service.HostResolver()
		fmt.Printf("  name:     %s\n", orDash(info.Hostname))
		fmt.Printf("  domain:   %s\n", orDash(info.SearchDomain))
		fmt.Printf("  servers:  %s\n", orDash(joinComma(info.DNSServers)))
		if d.Active() && (info.Hostname == "" || info.SearchDomain == "" || len(info.DNSServers) == 0) {
			fmt.Println("\nnote: registration is on, but this host is missing one of the three things it needs.")
			fmt.Println("      Set them under System > Resolver, or with `gravinet system resolver`.")
		}
		return
	}

	switch rest[0] {
	case "check":
		// A dry run. Everything a registration does except the updates: the
		// same SOA lookups, the same read-backs, and a verdict per name.
		//
		// This is an operational action on a settings leaf, which the register-
		// now button was removed from the web page for being in v996. The
		// difference is that this one writes nothing and answers a question the
		// settings themselves cannot: the config can be entirely correct and
		// the run still publish no PTR, for reasons that live on the network
		// rather than in this file.
		p, err := ddnsParamsForCheck(cfg)
		if err != nil {
			fatal("%v", err)
		}
		d, err := ddns.Diagnose(p)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Print(d.String())
		return
	case "run":
		// A registration, now, with the outcome on stdout.
		//
		// The daemon does this on a timer and reports to the log, which is the
		// right home for a recurring job's output and the wrong place to stand
		// while changing a zone. Anyone who has just fixed an update policy
		// wants to know whether it worked before the next quarter hour, and
		// "wait fifteen minutes, then read a log" is not an answer.
		//
		// This is the register-now button that came off the web page in v996,
		// put where it belongs. The objection then was that triggering a
		// registration is an operational action and does not go beside a
		// dark-mode switch; a terminal is exactly where operational actions do
		// go, and unlike a button this one prints what happened.
		p, err := ddnsParamsForCheck(cfg)
		if err != nil {
			fatal("%v", err)
		}
		res, err := ddns.Register(p, func(format string, args ...any) {
			fmt.Printf(format+"\n", args...)
		})
		if err != nil {
			fatal("%v", err)
		}
		for _, name := range res.Published {
			fmt.Printf("  ok      %s\n", name)
		}
		for _, e := range res.Errors {
			fmt.Printf("  FAILED  %s\n", e)
		}
		fmt.Printf("\n%d name(s) published, %d changed by this run, %d problem(s)\n",
			len(res.Published), res.Updated, len(res.Errors))
		if len(res.Errors) > 0 {
			// A non-zero exit so a script that runs this after a zone change
			// notices, rather than reading "FAILED" into a log nobody greps.
			os.Exit(1)
		}
		return
	case "interval":
		if len(rest) < 2 {
			fatal("usage: gravinet settings ddns interval <minutes|0>")
		}
		d.IntervalMinutes = mustAtoi(rest[1], "interval")
	case "ttl":
		if len(rest) < 2 {
			fatal("usage: gravinet settings ddns ttl <seconds|0>")
		}
		d.TTL = mustAtoi(rest[1], "ttl")
	case "reverse":
		if len(rest) < 2 {
			fatal("usage: gravinet settings ddns reverse <on|off>")
		}
		v := rest[1] == "on"
		d.Reverse = &v
	case "key":
		if len(rest) < 2 {
			fatal("usage: gravinet settings ddns key <name:base64secret[:algorithm]|->")
		}
		if rest[1] == "-" {
			d.TSIGKey = ""
		} else {
			// Parsed before it is stored, so a bad secret is refused here
			// rather than once an interval in a log nobody is reading.
			if _, err := ddns.ParseInlineKey(rest[1]); err != nil {
				fatal("%v", err)
			}
			d.TSIGKey = rest[1]
		}
	default:
		fatal("usage: gravinet settings ddns [check|run|interval <minutes>|ttl <seconds>|reverse <on|off>|key <name:base64secret[:algorithm]|->]")
	}

	if err := cfg.Validate(); err != nil {
		fatal("invalid config after change: %v", err)
	}
	if err := cfg.SaveTo(path); err != nil {
		fatal("save config: %v", err)
	}
	fmt.Println("saved")
	if reloadDaemon(cfg.ControlSocket) {
		fmt.Println("daemon reloaded")
	}
}

// ttlLabel renders a record lifetime. Zero is spelled out rather than printed
// bare: "ttl: 0" reads as a field nobody filled in, when it is in fact an
// instruction to every resolver on the network.
func ttlLabel(n int) string {
	if n == 0 {
		return "0 (resolvers are told not to cache)"
	}
	return fmt.Sprintf("%ds", n)
}

// tsigLabel says whether a key is set, and never what it is.
func tsigLabel(spec string) string {
	if strings.TrimSpace(spec) == "" {
		return "none (updates are sent unsigned)"
	}
	if k, err := ddns.ParseKey(spec); err == nil {
		return fmt.Sprintf("%s (%s)", k.Name, k.Algorithm)
	}
	return "set, but unreadable — check it"
}

func mustAtoi(s, what string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		fatal("%s: want a non-negative whole number, got %q", what, s)
	}
	return n
}
