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
