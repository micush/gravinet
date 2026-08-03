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
	"strconv"
	"strings"

	"gravinet/internal/config"
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
		fmt.Println("UPnP ON — on startup this node asks the LAN router to forward every port it listens on (UDP, TCP fallback, extras) from the WAN side automatically; best-effort, a silent no-op if the router doesn't offer UPnP")
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
// the primary (outbound + advertised), the rest extra listen-only ports —
// or "-" to turn UDP off entirely. Applied live (the daemon rebinds on
// reload); mirrors handlePort exactly, including refusing to turn UDP off
// while the TCP fallback is also off.
func cmdSettingsUDPPort(args []string) {
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		if cfg.PrimaryPort == 0 {
			fmt.Println("udp: off (\"-\")")
		} else {
			fmt.Printf("udp port(s): %s\n", fmtPortList(cfg.PrimaryPort, cfg.ExtraListenPorts))
		}
		return
	}
	if rest[0] == "-" {
		if !cfg.TCPFallbackEnabled() {
			fatal("can't turn off the UDP port while the TCP fallback is also off — at least one must stay on")
		}
		cfg.PrimaryPort = 0
		cfg.ExtraListenPorts = nil
		fmt.Println("udp -> off (TCP/TLS fallback only)")
		commitCfg(cfg, path)
		return
	}
	ports, err := parsePortList(rest[0])
	if err != nil {
		fatal("%v (comma-separated, e.g. 65432,443 — or \"-\" to turn UDP off)", err)
	}
	cfg.PrimaryPort = ports[0]
	cfg.ExtraListenPorts = ports[1:]
	fmt.Printf("udp port(s) -> %s (first is primary; applied live, the daemon rebinds)\n", fmtPortList(ports[0], ports[1:]))
	commitCfg(cfg, path)
}

// cmdSettingsTCPPort is cmdSettingsUDPPort's TCP/TLS-fallback counterpart —
// first port is the fallback listener, the rest extras, "-" disables the
// fallback (values are kept, not cleared, so re-enabling remembers them).
// Mirrors handleTCPPort, including the can't-disable-both refusal.
func cmdSettingsTCPPort(args []string) {
	cfg, path, rest := openCfg(args)
	if len(rest) == 0 || rest[0] == "status" {
		if !cfg.TCPFallbackEnabled() {
			fmt.Println("tcp fallback: off (\"-\")")
			return
		}
		p := cfg.TCPFallbackPort
		if p == 0 {
			p = config.DefaultTCPFallbackPort
		}
		fmt.Printf("tcp port(s): %s\n", fmtPortList(p, cfg.ExtraTCPListenPorts))
		return
	}
	if rest[0] == "-" {
		if cfg.PrimaryPort == 0 {
			fatal("can't turn off the TCP fallback while the UDP port is also off — at least one must stay on")
		}
		cfg.DisableTCPFallback = true
		fmt.Println("tcp fallback -> off (ports remembered for later re-enable)")
		commitCfg(cfg, path)
		return
	}
	ports, err := parsePortList(rest[0])
	if err != nil {
		fatal("%v (comma-separated, e.g. 65432,443 — or \"-\" to turn the TCP fallback off)", err)
	}
	cfg.DisableTCPFallback = false
	cfg.TCPFallbackPort = ports[0]
	cfg.ExtraTCPListenPorts = ports[1:]
	fmt.Printf("tcp port(s) -> %s (first is the fallback listener; applied live)\n", fmtPortList(ports[0], ports[1:]))
	commitCfg(cfg, path)
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
