package main

// The "gravinet system" group — the CLI half of internal/webadmin/ui.go's
// NAV_GROUPS "system" entry, which had no CLI presence at all before this
// file: nine pages in the rail (upgrade, resolver, time, snmp, l2disco,
// syslog, users, config-history, power) reachable only from a browser. That
// was the single largest CLI/GUI divergence, and it's the reason the group
// list in usage() didn't match the rail it claimed to mirror.
//
// Every leaf here is host-level state, not daemon-internal state, so — like
// monitor's metrics/route-table/bgp-peers leaves — these call the same
// exported readers and writers the web handlers call (internal/service's
// HostResolver/HostTime/ApplySNMP/ApplyLLDP/HostSyslog/ListSystemUsers/
// HostPower, internal/config's history List/Get/FullSummary/SnapshotNow)
// rather than reimplementing anything or routing through the control
// socket. There is one implementation of each operation, reached two ways.
//
// Restart semantics are copied from each setting's own web handler rather
// than decided here, the same discipline cli_settings.go documents: SNMP and
// L2 Disco reconcile their agent immediately after the config write
// (ApplySNMP/ApplyLLDP, exactly as handleSystemSNMP/handleSystemL2Disco do),
// so they're commitCfg (live), not commitCfgStructural.

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/service"
)

// systemAction pops a leading verb off args, defaulting to "show". Mirrors
// settingsAction's shape in cli_settings.go; the verb set differs per leaf,
// so it's passed in rather than hardcoded.
func systemAction(args []string, verbs []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		a := expandVerb(args[0], verbs)
		for _, verb := range verbs {
			if a == verb {
				return a, args[1:]
			}
		}
	}
	return "show", args
}

// reportHostOp prints the result of one of internal/service's (ok, note)
// host operations. A note on success is advisory (e.g. "changed, but a
// manual restart is needed"); on failure it's the reason, and the reason is
// the whole point of the exit code.
func reportHostOp(what string, ok bool, note string) {
	if !ok {
		if note == "" {
			note = "failed"
		}
		fatal("%s: %s", what, note)
	}
	fmt.Printf("%s: ok\n", what)
	if note != "" {
		fmt.Printf("note: %s\n", note)
	}
}

// ---------------------------------------------------------------- resolver

// cmdSystemResolver is "gravinet system resolver" — this host's hostname and
// default DNS servers (System > Resolver). Note the asymmetry the web page
// has too: setting the hostname needs a gravinet restart for mesh peers to
// see the new name (the advertised name is read once at startup), so this
// says so rather than silently leaving peers on the old one. Unlike the web
// handler it does NOT restart on its own — a CLI caller is already at a
// shell and can restart deliberately; a background restart triggered by what
// reads like a getter-adjacent command is the kind of surprise the web UI can
// afford (it asked, in a browser, with a spinner) and a script cannot.
func cmdSystemResolver(args []string) {
	action, rest := systemAction(args, v("show", "hostname", "dns"))
	switch action {
	case "hostname":
		fs := flag.NewFlagSet("system resolver hostname", flag.ExitOnError)
		fs.Parse(rest)
		if fs.NArg() != 1 {
			fatal("system resolver hostname NAME")
		}
		ok, note := service.SetHostname(fs.Arg(0))
		reportHostOp("hostname", ok, note)
		if ok {
			fmt.Println("restart gravinet for mesh peers to see the new name (the advertised hostname is read once at startup)")
		}
	case "dns":
		fs := flag.NewFlagSet("system resolver dns", flag.ExitOnError)
		search := fs.String("search", "", "search domain")
		fs.Parse(rest)
		if fs.NArg() == 0 {
			fatal("system resolver dns SERVER [SERVER...] [-search DOMAIN]")
		}
		ok, note := service.SetHostDNS(fs.Args(), *search)
		reportHostOp("host DNS", ok, note)
	default:
		info := service.HostResolver()
		fmt.Printf("hostname:      %s\n", info.Hostname)
		if len(info.DNSServers) == 0 {
			fmt.Println("dns servers:   (none)")
		} else {
			fmt.Printf("dns servers:   %s\n", strings.Join(info.DNSServers, ", "))
		}
		if info.SearchDomain != "" {
			fmt.Printf("search domain: %s\n", info.SearchDomain)
		}
		if info.Manager != "" {
			fmt.Printf("managed by:    %s\n", info.Manager)
		}
		if info.Hint != "" {
			fmt.Printf("note:          %s\n", info.Hint)
		}
	}
}

// -------------------------------------------------------------------- time

// cmdSystemTime is "gravinet system time" — the host clock, timezone, and NTP
// synchronization (System > Time).
func cmdSystemTime(args []string) {
	action, rest := systemAction(args, v("show", "timezone", "ntp", "clock"))
	switch action {
	case "timezone":
		fs := flag.NewFlagSet("system time timezone", flag.ExitOnError)
		fs.Parse(rest)
		if fs.NArg() != 1 {
			fatal("system time timezone ZONE  (e.g. America/Phoenix)")
		}
		ok, note := service.SetHostTimezone(fs.Arg(0))
		reportHostOp("timezone", ok, note)
	case "ntp":
		fs := flag.NewFlagSet("system time ntp", flag.ExitOnError)
		fs.Parse(rest)
		sub, servers := systemAction(fs.Args(), v("on", "off", "enable", "disable"))
		switch sub {
		case "on", "enable":
			ok, note := service.SetHostNTP(true, servers)
			reportHostOp("NTP", ok, note)
		case "off", "disable":
			ok, note := service.SetHostNTP(false, nil)
			reportHostOp("NTP", ok, note)
		default:
			fatal("system time ntp on [SERVER...] | off")
		}
	case "clock":
		fs := flag.NewFlagSet("system time clock", flag.ExitOnError)
		fs.Parse(rest)
		if fs.NArg() != 1 {
			fatal("system time clock RFC3339  (e.g. 2026-08-02T15:04:05)")
		}
		ok, note := service.SetHostClock(fs.Arg(0))
		reportHostOp("clock", ok, note)
	default:
		info := service.HostTime()
		fmt.Printf("now:       %s\n", info.Now.Format(time.RFC3339))
		fmt.Printf("timezone:  %s\n", info.Timezone)
		fmt.Printf("ntp:       %s\n", boolWord(info.NTPEnabled, "enabled", "disabled"))
		if info.SyncKnown {
			fmt.Printf("synced:    %s\n", boolWord(info.Synchronized, "yes", "no"))
		} else {
			fmt.Println("synced:    unknown")
		}
		if len(info.Servers) > 0 {
			fmt.Printf("servers:   %s\n", strings.Join(info.Servers, ", "))
		}
		if info.Hint != "" {
			fmt.Printf("note:      %s\n", info.Hint)
		}
	}
}

// -------------------------------------------------------------------- snmp

// cmdSystemSNMP is "gravinet system snmp" — the read-only SNMPv2c agent
// (System > SNMP). Config-backed, then reconciled with the host's snmpd the
// same way handleSystemSNMP does: ApplySNMP after the save, so "enabled" in
// config and a running agent don't drift apart across a single command.
func cmdSystemSNMP(args []string) {
	if ok, hint := service.SNMPSupported(); !ok {
		fmt.Printf("note: %s\n", hint)
	}
	action, rest := systemAction(args, v("show", "on", "off", "enable", "disable", "community", "listen", "location", "contact"))
	cfg, path, rest := openCfg(rest)
	switch action {
	case "on", "enable":
		if len(activeCommunities(cfg.SNMP)) == 0 {
			fatal("SNMP needs at least one enabled community string first: gravinet system snmp community add NAME")
		}
		cfg.SNMP.Enabled = true
	case "off", "disable":
		cfg.SNMP.Enabled = false
	case "community":
		sub, cargs := systemAction(rest, v("add", "del", "remove"))
		if len(cargs) != 1 {
			fatal("system snmp community add|del NAME")
		}
		switch sub {
		case "add":
			for _, c := range cfg.SNMP.Communities {
				if c.Community == cargs[0] {
					fatal("community %q already present", cargs[0])
				}
			}
			cfg.SNMP.Communities = append(cfg.SNMP.Communities, config.SNMPCommunity{Community: cargs[0]})
		case "del", "remove":
			kept := cfg.SNMP.Communities[:0]
			found := false
			for _, c := range cfg.SNMP.Communities {
				if c.Community == cargs[0] {
					found = true
					continue
				}
				kept = append(kept, c)
			}
			if !found {
				fatal("no such community %q", cargs[0])
			}
			cfg.SNMP.Communities = kept
		default:
			fatal("system snmp community add|del NAME")
		}
		// Mirror handleSystemSNMP's own guard: enabled with nothing to
		// authenticate with is a config that can't start an agent.
		if cfg.SNMP.Enabled && len(activeCommunities(cfg.SNMP)) == 0 {
			fatal("that would leave SNMP enabled with no community string; turn it off first")
		}
	case "listen":
		if len(rest) != 1 {
			fatal("system snmp listen ADDR:PORT")
		}
		cfg.SNMP.ListenAddr = rest[0]
	case "location":
		cfg.SNMP.Location = strings.Join(rest, " ")
	case "contact":
		cfg.SNMP.Contact = strings.Join(rest, " ")
	default:
		fmt.Printf("snmp:      %s\n", boolWord(cfg.SNMP.Enabled, "on", "off"))
		fmt.Printf("running:   %s\n", boolWord(service.SNMPServiceRunning(), "yes", "no"))
		if cfg.SNMP.ListenAddr != "" {
			fmt.Printf("listen:    %s\n", cfg.SNMP.ListenAddr)
		}
		for _, c := range cfg.SNMP.Communities {
			state := ""
			if c.Disabled {
				state = " (disabled)"
			}
			fmt.Printf("community: %s%s\n", c.Community, state)
		}
		if cfg.SNMP.Location != "" {
			fmt.Printf("location:  %s\n", cfg.SNMP.Location)
		}
		if cfg.SNMP.Contact != "" {
			fmt.Printf("contact:   %s\n", cfg.SNMP.Contact)
		}
		return
	}
	commitCfg(cfg, path)
	ok, hint := service.ApplySNMP(cfg.SNMP)
	if !ok {
		fmt.Printf("saved, but the snmpd service could not be reconciled: %s\n", hint)
	}
}

// activeCommunities is the enabled subset — the count handleSystemSNMP
// validates against, kept as its own helper so the two guards above read
// the same way.
func activeCommunities(c config.SNMPConfig) []config.SNMPCommunity {
	var out []config.SNMPCommunity
	for _, x := range c.Communities {
		if !x.Disabled {
			out = append(out, x)
		}
	}
	return out
}

// ---------------------------------------------------------------- l2disco

// cmdSystemL2Disco is "gravinet system l2disco" — link-layer discovery
// (LLDP/CDP) configuration (System > L2 Disco). The live neighbor table it
// sits beside in the GUI is "gravinet monitor l2-peers", the same split the
// rail has between Traffic > BGP and Monitor > BGP Peers.
func cmdSystemL2Disco(args []string) {
	if ok, hint := service.LLDPSupported(); !ok {
		fmt.Printf("note: %s\n", hint)
	}
	action, rest := systemAction(args, v("show", "on", "off", "enable", "disable", "iface"))
	cfg, path, rest := openCfg(rest)
	switch action {
	case "on", "enable":
		cfg.Discovery.Disabled = false
	case "off", "disable":
		cfg.Discovery.Disabled = true
	case "iface":
		sub, iargs := systemAction(rest, v("add", "del", "remove"))
		if len(iargs) != 1 {
			fatal("system l2disco iface add|del NAME")
		}
		name := iargs[0]
		switch sub {
		case "add":
			if !service.ValidLLDPIface(name) {
				fatal("%q is not a usable interface name for LLDP/CDP", name)
			}
			for _, i := range cfg.Discovery.Interfaces {
				if i.Name == name {
					fatal("interface %q already configured", name)
				}
			}
			cfg.Discovery.Interfaces = append(cfg.Discovery.Interfaces,
				config.DiscoveryIface{Name: name, LLDP: true, CDP: true})
		case "del", "remove":
			kept := cfg.Discovery.Interfaces[:0]
			found := false
			for _, i := range cfg.Discovery.Interfaces {
				if i.Name == name {
					found = true
					continue
				}
				kept = append(kept, i)
			}
			if !found {
				fatal("no such configured interface %q", name)
			}
			cfg.Discovery.Interfaces = kept
		default:
			fatal("system l2disco iface add|del NAME")
		}
	default:
		fmt.Printf("l2disco:   %s\n", boolWord(!cfg.Discovery.Disabled, "on", "off"))
		fmt.Printf("running:   %s\n", boolWord(service.LLDPServiceRunning(), "yes", "no"))
		if len(cfg.Discovery.Interfaces) == 0 {
			fmt.Println("interfaces: (none)")
		}
		for _, i := range cfg.Discovery.Interfaces {
			fmt.Printf("interface: %-12s lldp=%v cdp=%v\n", i.Name, i.LLDP, i.CDP)
		}
		return
	}
	commitCfg(cfg, path)
	ok, hint := service.ApplyLLDP(cfg.Discovery)
	if !ok {
		fmt.Printf("saved, but the lldpd service could not be reconciled: %s\n", hint)
	}
}

// ------------------------------------------------------------------ syslog

// cmdSystemSyslog is "gravinet system syslog" — forward this host's syslog to
// a remote collector (System > Syslog). Host state, not gravinet config: the
// targets live in the host syslog daemon's own configuration, which is why
// this reads and writes through service.HostSyslog/SetHostSyslog rather than
// openCfg like SNMP and L2 Disco do.
func cmdSystemSyslog(args []string) {
	if ok, hint := service.SyslogSupported(); !ok {
		fmt.Printf("note: %s\n", hint)
	}
	action, rest := systemAction(args, v("show", "add", "del", "remove", "clear"))
	info := service.HostSyslog()
	switch action {
	case "add":
		fs := flag.NewFlagSet("system syslog add", flag.ExitOnError)
		proto := fs.String("proto", "udp", "transport: udp|tcp")
		port := fs.Int("port", 514, "collector port")
		fs.Parse(rest)
		if fs.NArg() != 1 {
			fatal("system syslog add HOST [-proto udp|tcp] [-port N]")
		}
		targets := append(info.Targets, service.SyslogTarget{
			Remote: fs.Arg(0), Port: *port, Protocol: *proto,
		})
		ok, note := service.SetHostSyslog(targets)
		reportHostOp("syslog", ok, note)
	case "del", "remove":
		if len(rest) != 1 {
			fatal("system syslog del HOST")
		}
		var targets []service.SyslogTarget
		found := false
		for _, t := range info.Targets {
			if t.Remote == rest[0] {
				found = true
				continue
			}
			targets = append(targets, t)
		}
		if !found {
			fatal("no syslog target for host %q", rest[0])
		}
		ok, note := service.SetHostSyslog(targets)
		reportHostOp("syslog", ok, note)
	case "clear":
		ok, note := service.SetHostSyslog(nil)
		reportHostOp("syslog", ok, note)
	default:
		if len(info.Targets) == 0 {
			fmt.Println("no remote syslog targets configured")
		}
		for _, t := range info.Targets {
			state := ""
			if t.Disabled {
				state = " (disabled)"
			}
			fmt.Printf("target: %s:%d/%s%s\n", t.Remote, t.Port, t.Protocol, state)
		}
		if info.Hint != "" {
			fmt.Printf("note: %s\n", info.Hint)
		}
	}
}

// ------------------------------------------------------------------- users

// cmdSystemUsers is "gravinet system users" — the local OS accounts permitted
// to sign in to the console (System > Users). These are real OS accounts in a
// gravinet group, not config entries, which is why this goes through
// service.ListSystemUsers/AddSystemUser/... and not openCfg.
func cmdSystemUsers(args []string) {
	action, rest := systemAction(args, v("show", "list", "add", "del", "remove", "passwd", "expiry"))
	switch action {
	case "add":
		fs := flag.NewFlagSet("system users add", flag.ExitOnError)
		pass := fs.String("pass", "", "password (omit to be prompted)")
		expires := fs.String("expires", "", "expiry date YYYY-MM-DD (omit for none)")
		fs.Parse(rest)
		if fs.NArg() != 1 {
			fatal("system users add NAME [-pass PW] [-expires YYYY-MM-DD]")
		}
		ok, note := service.AddSystemUser(fs.Arg(0), readPassword(*pass), parseExpiry(*expires))
		reportHostOp("add user", ok, note)
	case "passwd":
		fs := flag.NewFlagSet("system users passwd", flag.ExitOnError)
		pass := fs.String("pass", "", "password (omit to be prompted)")
		fs.Parse(rest)
		if fs.NArg() != 1 {
			fatal("system users passwd NAME [-pass PW]")
		}
		ok, note := service.SetSystemUserPassword(fs.Arg(0), readPassword(*pass))
		reportHostOp("set password", ok, note)
	case "expiry":
		fs := flag.NewFlagSet("system users expiry", flag.ExitOnError)
		fs.Parse(rest)
		if fs.NArg() < 1 {
			fatal("system users expiry NAME [YYYY-MM-DD]  (omit the date to clear)")
		}
		date := ""
		if fs.NArg() > 1 {
			date = fs.Arg(1)
		}
		ok, note := service.SetSystemUserExpiry(fs.Arg(0), parseExpiry(date))
		reportHostOp("set expiry", ok, note)
	case "del", "remove":
		if len(rest) != 1 {
			fatal("system users del NAME")
		}
		ok, note := service.DeleteSystemUser(rest[0])
		reportHostOp("delete user", ok, note)
	default:
		info := service.ListSystemUsers()
		if len(info.Users) == 0 {
			fmt.Println("no console users")
		}
		for _, u := range info.Users {
			exp := "never"
			switch {
			case !u.ExpiryKnown:
				exp = "unknown"
			case !u.Expires.IsZero():
				exp = u.Expires.Format("2006-01-02")
			}
			flag := ""
			if u.Expired {
				flag = " (expired — locked out by the OS)"
			}
			fmt.Printf("%-20s expires=%s%s\n", u.Name, exp, flag)
		}
		if info.ManageHint != "" {
			fmt.Printf("note: %s\n", info.ManageHint)
		}
	}
}

// readPassword returns pass, or prompts for it on stderr when empty — the
// same courtesy cmdGenPass already extends, so a password never has to sit
// in shell history just because the flag exists.
func readPassword(pass string) string {
	if pass != "" {
		return pass
	}
	fmt.Fprint(os.Stderr, "password: ")
	var line string
	fmt.Scanln(&line)
	if line == "" {
		fatal("empty password")
	}
	return line
}

// parseExpiry turns YYYY-MM-DD into a time, or the zero time for "no
// expiry" — the shape service.AddSystemUser/SetSystemUserExpiry expect.
func parseExpiry(date string) time.Time {
	if strings.TrimSpace(date) == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		fatal("expiry date must be YYYY-MM-DD: %v", err)
	}
	return t
}

// ---------------------------------------------------------- config history

// cmdSystemConfigHistory is "gravinet system config-history" — the automatic
// and manual config snapshots the GUI's Config History page lists, diffs, and
// restores. Restore goes through the same commitCfg path every other config
// command uses (save + daemon reload), so restoring an old config behaves
// exactly like editing your way back to it by hand.
func cmdSystemConfigHistory(args []string) {
	action, rest := systemAction(args, v("show", "list", "diff", "restore", "snapshot"))
	cfg, path, rest := openCfg(rest)
	switch action {
	case "diff":
		if len(rest) != 1 {
			fatal("system config-history diff ID   (ids from \"gravinet system config-history list\")")
		}
		stamp, old, err := config.Get(path, rest[0], cfg)
		if err != nil {
			fatal("snapshot %s: %v", rest[0], err)
		}
		fmt.Printf("snapshot %s (%s) vs the config on disk now:\n", rest[0], stamp)
		secs := config.FullSummary(old, cfg)
		if len(secs) == 0 {
			fmt.Println("  (no differences)")
			return
		}
		for _, sd := range secs {
			fmt.Printf("  %-22s %s\n", sd.Section, sd.Detail)
		}
	case "restore":
		if len(rest) != 1 {
			fatal("system config-history restore ID")
		}
		stamp, old, err := config.Get(path, rest[0], cfg)
		if err != nil {
			fatal("snapshot %s: %v", rest[0], err)
		}
		fmt.Printf("restoring snapshot %s (%s)\n", rest[0], stamp)
		commitCfg(old, path)
	case "snapshot":
		id, err := config.SnapshotNow(path, cfg, "cli", cfg.ConfigHistoryLimit)
		if err != nil {
			fatal("snapshot: %v", err)
		}
		fmt.Printf("wrote snapshot %s\n", id)
	default:
		list, err := config.List(path)
		if err != nil {
			fatal("config history: %v", err)
		}
		if len(list) == 0 {
			fmt.Println("no config snapshots")
			return
		}
		for _, m := range list {
			fmt.Printf("%-24s %-20s %s\n", m.ID, m.Stamp, m.User)
		}
	}
}

// ------------------------------------------------------------------- power

// cmdSystemPower is "gravinet system power" — restart or shut down the whole
// host (System > Power). Deliberately requires the action spelled out in full
// (no prefix expansion, no defaulting to anything): every other leaf in this
// group defaults to a read when given no verb, and defaulting to a read is
// exactly what you want everywhere except here, where the read-shaped
// mistake would take the machine down. Note this is the host, not the
// gravinet service — that's "gravinet service" / a restart under settings.
func cmdSystemPower(args []string) {
	if ok, hint := service.HostPowerSupported(); !ok {
		fatal("power: %s", hint)
	}
	fs := flag.NewFlagSet("system power", flag.ExitOnError)
	delay := fs.Int("delay", 0, "minutes to wait before acting")
	rest := args
	action := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		action, rest = rest[0], rest[1:]
	}
	fs.Parse(rest)
	switch action {
	case "reboot", "restart", "shutdown", "poweroff":
		if action == "restart" {
			action = "reboot"
		}
		if action == "poweroff" {
			action = "shutdown"
		}
		ok, note := service.HostPower(action, *delay)
		reportHostOp("host "+action, ok, note)
	case "cancel":
		ok, note := service.HostPowerCancel()
		reportHostOp("cancel", ok, note)
	default:
		fatal("system power reboot|shutdown|cancel [-delay MINUTES]  (this takes the HOST down, not just the gravinet service)")
	}
}

// boolWord renders a bool as one of two words — used all over this file's
// status output, where "on"/"off" and "yes"/"no" both come up.
func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
