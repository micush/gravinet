package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/control"
	"gravinet/internal/mesh"
	"gravinet/internal/service"
)

// The commands in this file manage declarative settings by editing the config
// file (load → mutate → validate → save). After a successful save they ask a
// running daemon to reload (best effort); structural changes apply on restart.

// defaultConfigPath is where subcommands look for the config file when
// -config isn't given. It's platform-specific (see defaultpath_windows.go /
// defaultpath_other.go) since Windows has no /etc.
var defaultConfigPath = platformDefaultConfigPath()

// extractOpt pulls "-name VAL" / "--name VAL" / "-name=VAL" out of args.
func extractOpt(args []string, name string) (string, []string) {
	out := args[:0:0]
	val := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-"+name || a == "--"+name {
			if i+1 < len(args) {
				val = args[i+1]
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "-"+name+"=") {
			val = strings.SplitN(a, "=", 2)[1]
			continue
		}
		if strings.HasPrefix(a, "--"+name+"=") {
			val = strings.SplitN(a, "=", 2)[1]
			continue
		}
		out = append(out, a)
	}
	return val, out
}

// kw returns the token following keyword kw (e.g. "key" -> next arg).
func kw(args []string, keyword string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == keyword {
			return args[i+1]
		}
	}
	return ""
}

func openCfg(args []string) (*config.Config, string, []string) {
	path, rest := extractOpt(args, "config")
	if path == "" {
		path = defaultConfigPath
	}
	cfg, err := config.Load(path)
	if err != nil {
		fatal("load config %s: %v", path, err)
	}
	return cfg, path, rest
}

func commitCfg(cfg *config.Config, path string) {
	if err := cfg.Validate(); err != nil {
		fatal("invalid config after change: %v", err)
	}
	if err := cfg.SaveTo(path); err != nil {
		fatal("save config: %v", err)
	}
	fmt.Printf("saved %s\n", path)
	if reloadDaemon(cfg.ControlSocket) {
		fmt.Println("daemon reloaded (live changes applied; structural changes apply on restart)")
	} else {
		fmt.Println("daemon not reachable — changes apply when the service starts")
	}
}

// reloadDaemon asks a running daemon to re-read its config. Returns false if no
// daemon is listening (so the caller can say "applies on restart").
func reloadDaemon(sock string) bool {
	// Normalize rather than only defaulting an empty value: a config still holding
	// the stale "/run/gravinet.sock" is exactly the case where the daemon is bound
	// somewhere else (the platform default), and dialing the stale path verbatim
	// would report "daemon not reachable" about a daemon that is running fine.
	endpoint, _ := config.NormalizeControlSocket(sock)
	resp, err := control.Do(endpoint, control.Request{Cmd: "reload"})
	return err == nil && resp.Error == ""
}

// commitCfgStructural saves a structural change (one that needs the daemon to
// rebuild interfaces/sessions, e.g. adding/joining/enabling a network) and, by
// default, restarts the service so it takes effect immediately.
func commitCfgStructural(cfg *config.Config, path string, noRestart bool) {
	if err := cfg.Validate(); err != nil {
		fatal("invalid config after change: %v", err)
	}
	if err := cfg.SaveTo(path); err != nil {
		fatal("save config: %v", err)
	}
	fmt.Printf("saved %s\n", path)
	if noRestart {
		fmt.Println("not restarting (--no-restart); restart the service to apply")
		return
	}
	if ok, hint := service.Restart(); ok {
		fmt.Println("restarted the gravinet service — change is live")
	} else {
		fmt.Println(hint)
	}
}

// hasFlag reports whether -name/--name is present and returns args without it.
func hasFlag(args []string, name string) (bool, []string) {
	out := args[:0:0]
	found := false
	for _, a := range args {
		if a == "-"+name || a == "--"+name {
			found = true
			continue
		}
		out = append(out, a)
	}
	return found, out
}

// pickNetwork resolves a network by name, or the sole network if name is empty.
func pickNetwork(cfg *config.Config, name string) *config.Network {
	if name != "" {
		for i := range cfg.Networks {
			if cfg.Networks[i].Name == name || cfg.Networks[i].ID == name {
				return &cfg.Networks[i]
			}
		}
		fatal("no network named %q", name)
	}
	switch len(cfg.Networks) {
	case 0:
		fatal("no networks configured; add one with 'gravinet network add NAME'")
	case 1:
		return &cfg.Networks[0]
	default:
		fatal("multiple networks; specify one with -net NAME")
	}
	return nil
}

// ---- network -----------------------------------------------------------------

func cmdNetwork(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet network <add|delete|enable|disable|rename|notes|subnet|join|join-token|token|list> ...")
	}
	sub := args[0]
	cfg, path, rest := openCfg(args[1:])
	noRestart, rest := hasFlag(rest, "no-restart")

	sub = expandVerb(sub, v("list"), v("add"), v("delete", "del", "remove"), v("enable", "disable"), v("rename"), v("notes"), v("subnet", "set-subnet"), v("join"), v("join-token"), v("token", "invite"))
	switch sub {
	case "list":
		if len(cfg.Networks) == 0 {
			fmt.Println("(no networks)")
			return
		}
		for _, n := range cfg.Networks {
			state := "disabled"
			if n.Enabled {
				state = "enabled"
			}
			keys := 0
			for _, k := range n.Keys {
				if k.Key != "" {
					keys++
				}
			}
			fmt.Printf("%-16s %-9s id=%s subnet4=%s keys=%d seeds=%d\n",
				n.Name, state, n.ID, n.Subnet4, keys, len(n.Seeds))
			if n.Notes != "" {
				fmt.Printf("  notes: %s\n", n.Notes)
			}
		}
		return

	case "add":
		if len(rest) == 0 {
			fatal("usage: gravinet network add NAME [subnet CIDR] [subnet6 CIDR]")
		}
		name := rest[0]
		v4 := optOrKw(rest, "subnet")
		v6 := optOrKw(rest, "subnet6")
		n, err := cfg.NetworkAdd(name, v4, v6)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Printf("added network %q (id %s, subnet4=%s subnet6=%s, generated key)\n",
			name, n.ID, orNone(n.Subnet4), orNone(n.Subnet6))

	case "delete", "del", "remove":
		if len(rest) == 0 {
			fatal("usage: gravinet network delete NAME")
		}
		if err := cfg.NetworkDelete(rest[0]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("deleted network %q\n", rest[0])

	case "enable", "disable":
		if len(rest) == 0 {
			fatal("usage: gravinet network %s NAME", sub)
		}
		if err := cfg.NetworkSetEnabled(rest[0], sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd network %q\n", sub, rest[0])

	case "rename":
		if len(rest) < 2 {
			fatal("usage: gravinet network rename OLD NEW")
		}
		if err := cfg.NetworkRename(rest[0], rest[1]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("renamed network %q to %q\n", rest[0], rest[1])
		commitCfg(cfg, path) // local label only — no restart needed
		return

	case "notes":
		if len(rest) < 1 {
			fatal("usage: gravinet network notes NAME [TEXT...]  (empty TEXT clears the note)")
		}
		name := rest[0]
		notes := strings.Join(rest[1:], " ")
		if err := cfg.NetworkSetNotes(name, notes); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("set notes on network %q\n", name)
		commitCfg(cfg, path) // local metadata only — no restart needed
		return

	case "subnet", "set-subnet":
		if len(rest) == 0 {
			fatal("usage: gravinet network subnet NAME [subnet CIDR] [subnet6 CIDR]  (use 'none' to clear a family)")
		}
		name := rest[0]
		v4 := optOrKw(rest, "subnet")
		v6 := optOrKw(rest, "subnet6")
		if v4 == "" && v6 == "" {
			fatal("provide subnet CIDR and/or subnet6 CIDR (use 'none' to clear one family)")
		}
		if err := cfg.NetworkSetSubnets(name, v4, v6); err != nil {
			fatal("%v", err)
		}
		n := cfg.FindNetwork(name)
		fmt.Printf("network %q subnets now subnet4=%s subnet6=%s\n  (restart required, and apply the same change on every node in this network)\n",
			name, orNone(n.Subnet4), orNone(n.Subnet6))
		// Unlike every other case in this switch, re-addressing a live
		// interface is genuinely something the hot-reload path (below,
		// commitCfg) does not do — see internal/webadmin/edit.go's
		// handleNetwork, which sets restart:=true for exactly this op and
		// no other network op. So this is the one case that still needs the
		// full commitCfgStructural/service-restart path.
		commitCfgStructural(cfg, path, noRestart)
		return

	case "join":
		// gravinet network join ID key KEY peer PEER [subnet CIDR] [subnet6 CIDR]
		// or: gravinet network join grav1.<token>
		if len(rest) == 0 {
			fatal("usage: gravinet network join ID key KEY peer PEER   (or: gravinet network join <token>)")
		}
		if config.IsJoinToken(rest[0]) {
			id, name, err := cfg.NetworkJoinToken(rest[0])
			if err != nil {
				fatal("%v", err)
			}
			fmt.Printf("joined network %s%s from token\n  name and subnet are learned from the network once a peer connects\n",
				id, ifStr(name != "", " ("+name+")", ""))
			break
		}
		id := rest[0]
		key := kw(rest, "key")
		peer := strings.TrimSpace(kw(rest, "peer"))
		if err := cfg.NetworkJoin(id, key, peer, optOrKw(rest, "subnet"), optOrKw(rest, "subnet6")); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("joining network %s (key set%s)\n  name and subnet will be learned from the network once a peer connects\n",
			id, ifStr(peer != "", ", seed "+peer, ""))

	case "join-token":
		if len(rest) == 0 {
			fatal("usage: gravinet network join-token <token>   (token from 'network token' on a member node)")
		}
		id, name, err := cfg.NetworkJoinToken(rest[0])
		if err != nil {
			fatal("%v", err)
		}
		fmt.Printf("joined network %s%s from token\n  name and subnet are learned from the network once a peer connects\n",
			id, ifStr(name != "", " ("+name+")", ""))

	case "token", "invite":
		// gravinet network token NAME [addr HOST:PORT] [expires DUR]
		if len(rest) == 0 {
			fatal("usage: gravinet network token NAME [addr HOST:PORT] [expires DUR]\n  share the printed token with a new node, which joins via 'network join <token>'")
		}
		name := rest[0]
		var extra []string
		if a := strings.TrimSpace(kw(rest, "addr")); a != "" {
			extra = append(extra, a)
		}
		var ttl time.Duration
		if e := strings.TrimSpace(kw(rest, "expires")); e != "" {
			d, derr := time.ParseDuration(e)
			if derr != nil || d <= 0 {
				fatal("invalid expires duration %q (use e.g. 24h, 72h)", e)
			}
			ttl = d
		}
		seedCount := cfg.TokenSeedCount(name, extra)
		tok, err := cfg.NetworkToken(name, extra, ttl)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(tok)
		fmt.Fprintln(os.Stderr, "\nThis token contains the network key — anyone with it can join. Share it over a secure channel.")
		if ttl > 0 {
			fmt.Fprintf(os.Stderr, "It expires in %s.\n", ttl)
		} else {
			fmt.Fprintln(os.Stderr, "It does not expire; pass 'expires 24h' to time-box it.")
		}
		if seedCount == 0 {
			fmt.Fprintln(os.Stderr, "No bootstrap seed is embedded — add one with 'addr HOST:PORT' (this node's reachable underlay endpoint), or the joiner must add a seed manually.")
		}
		return

	default:
		fatal("unknown: gravinet network %s", sub)
	}
	// add/delete/enable/disable/join/join-token all reach here. None of them
	// need a service restart — the engine's reload path already brings a new
	// or newly-enabled network up live (building its TUN device and calling
	// AddNetwork) and tears a removed/disabled one down live (RemoveNetwork,
	// same as it already does for the web admin — see handleNetwork's
	// restart:=false for these exact ops, and reloadFn in cmd/gravinet/main.go
	// for what "live" actually does). A full restart would just needlessly
	// drop every *other* network's sessions along with it.
	commitCfg(cfg, path)
}

// ---- route -------------------------------------------------------------------

// cmdSeed manages a network's bootstrap seed addresses (host, host:port, or
// host:port,port,... to try more than one port against the same host).
// Config-file style like route/nat: edit and live-reload. Unlike connected
// peers, seeds persist whether or not anyone is currently connected.
func cmdSeed(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet seed <list|add|remove|notes> [ADDR] [-net NAME] [-notes N]")
	}
	sub := args[0]
	netName, rest := extractOpt(args[1:], "net")
	notes, rest := extractOpt(rest, "notes")
	cfg, path, rest := openCfg(rest)
	n := pickNetwork(cfg, netName)

	sub = expandVerb(sub, v("list"), v("add"), v("remove", "delete", "del"), v("notes"))
	switch sub {
	case "list":
		fmt.Printf("network %s seeds:\n", n.Name)
		if len(n.Seeds) == 0 {
			fmt.Println("  (none)")
		}
		for _, s := range n.Seeds {
			if s.Notes != "" {
				fmt.Printf("  %-30s %s\n", s.Address, s.Notes)
			} else {
				fmt.Printf("  %s\n", s.Address)
			}
		}
		return

	case "add":
		if len(rest) == 0 {
			fatal("usage: gravinet seed add ADDR [-notes N]")
		}
		if err := cfg.SeedAdd(netName, rest[0]); err != nil {
			fatal("%v", err)
		}
		if notes != "" {
			if err := cfg.SeedSetNotes(netName, rest[0], notes); err != nil {
				fatal("%v", err)
			}
		}
		fmt.Printf("added seed %s to %s\n", rest[0], n.Name)

	case "remove", "delete", "del":
		if len(rest) == 0 {
			fatal("usage: gravinet seed remove ADDR")
		}
		if err := cfg.SeedRemove(netName, rest[0]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("removed seed %s from %s\n", rest[0], n.Name)

	case "notes":
		if len(rest) == 0 {
			fatal("usage: gravinet seed notes ADDR -notes N")
		}
		if err := cfg.SeedSetNotes(netName, rest[0], notes); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("set notes on seed %s on %s\n", rest[0], n.Name)

	default:
		fatal("unknown: gravinet seed %s", sub)
	}
	commitCfg(cfg, path)
}

func cmdRoute(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet route <add|delete|advertise|reject|enable|disable|reject-enable|reject-disable|list> CIDR [-net NAME]")
	}
	sub := args[0]
	netName, rest := extractOpt(args[1:], "net")
	cfg, path, rest := openCfg(rest)
	n := pickNetwork(cfg, netName)

	sub = expandVerb(sub, v("list"), v("add", "advertise", "redistribute"), v("delete", "del", "remove"), v("reject"), v("enable", "disable"), v("reject-enable", "reject-disable"))
	switch sub {
	case "list":
		fmt.Printf("network %s routes:\n", n.Name)
		if len(n.Routes) == 0 && len(n.RouteRej) == 0 {
			fmt.Println("  (none)")
		}
		for _, r := range n.Routes {
			fmt.Printf("  advertise %-20s metric=%d %s\n", r.CIDR, r.Metric, onOff(r.Enabled))
		}
		for _, c := range n.RouteRej {
			scope := "exact"
			if c.Inclusive {
				scope = "inclusive"
			}
			fmt.Printf("  reject    %-20s %-9s %s\n", c.CIDR, scope, onOff(!c.Disabled))
		}
		return

	case "add", "advertise", "redistribute": // redistribute kept as a legacy alias
		metricStr, rest2 := extractOpt(rest, "metric")
		if len(rest2) == 0 {
			fatal("usage: gravinet route %s CIDR [-metric N]", sub)
		}
		cidr := rest2[0]
		metric := 0
		if metricStr != "" {
			m, err := strconv.Atoi(metricStr)
			if err != nil {
				fatal("bad metric %q", metricStr)
			}
			metric = m
		}
		if err := cfg.RouteAdd(netName, cidr, metric); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("advertising route %s on %s (metric %d)\n", cidr, n.Name, metric)

	case "delete", "del", "remove":
		if len(rest) == 0 {
			fatal("usage: gravinet route delete CIDR")
		}
		cidr := rest[0]
		if err := cfg.RouteDelete(netName, cidr); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("deleted route %s from %s\n", cidr, n.Name)

	case "reject":
		inclusive, rest2 := hasFlag(rest, "inclusive")
		if len(rest2) == 0 {
			fatal("usage: gravinet route reject CIDR [-inclusive]")
		}
		cidr := rest2[0]
		if err := cfg.RouteReject(netName, cidr, inclusive); err != nil {
			fatal("%v", err)
		}
		scope := "exact match only"
		if inclusive {
			scope = "inclusive (also blocks networks contained within it)"
		}
		fmt.Printf("rejecting advertised route %s on %s — %s\n", cidr, n.Name, scope)

	case "enable", "disable":
		if len(rest) == 0 {
			fatal("usage: gravinet route %s CIDR", sub)
		}
		cidr := rest[0]
		if err := cfg.RouteSetEnabled(netName, cidr, sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd advertised route %s on %s\n", sub, cidr, n.Name)

	case "reject-enable", "reject-disable":
		if len(rest) == 0 {
			fatal("usage: gravinet route %s CIDR", sub)
		}
		cidr := rest[0]
		if err := cfg.RouteRejectSetEnabled(netName, cidr, sub == "reject-enable"); err != nil {
			fatal("%v", err)
		}
		verb := "enabled"
		if sub == "reject-disable" {
			verb = "disabled"
		}
		fmt.Printf("%s reject entry %s on %s\n", verb, cidr, n.Name)

	default:
		fatal("unknown: gravinet route %s", sub)
	}
	commitCfg(cfg, path)
}

// cmdFWExempt manages the node-global firewall exemption allowlist — the
// always-allowed control/management/routing protocols the rulebase can't
// override. The list is global: it applies to every network. Config-file style
// (like route/nat): edit and live-reload.
func cmdFWExempt(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet fw exempt <list|add|del|enable|disable|reset> [...]")
	}
	sub := args[0]
	cfg, path, rest := openCfg(args[1:])

	sub = expandVerb(sub, v("list"), v("add"), v("del", "delete", "remove"), v("reset"), v("enable", "disable"))
	switch sub {
	case "list":
		exempts, isDefault := cfg.FirewallExemptList()
		src := "custom"
		if isDefault {
			src = "built-in defaults"
		}
		mgmtPort := cfg.WebAdminPort()
		fmt.Printf("firewall exemptions (%s) — global, always allowed on every network:\n", src)
		if len(exempts) == 0 {
			fmt.Println("  (none — every protocol is subject to the rulebase)")
		}
		for i, e := range exempts {
			port := "any"
			switch {
			case e.Mgmt:
				port = strconv.Itoa(mgmtPort) // the actual management port
			case e.Port != 0:
				port = strconv.Itoa(e.Port)
			}
			proto := e.Proto
			if proto == "" {
				proto = "any"
			}
			fmt.Printf("  [%d] %-18s proto=%-6s port=%-6s %s\n", i, e.Name, proto, port, onOff(!e.Disabled))
		}
		return

	case "add":
		fs := flag.NewFlagSet("fw exempt add", flag.ExitOnError)
		name := fs.String("name", "", "label for the exemption")
		proto := fs.String("proto", "any", "tcp|udp|icmp|ospf|<number>|any")
		port := fs.Int("port", 0, "port; matches source OR destination (0 = any/port-less)")
		mgmt := fs.Bool("mgmt", false, "track this node's web-admin port (overrides -port)")
		fs.Parse(rest)
		if *name == "" {
			fatal("exemption needs a -name")
		}
		e := config.FirewallExempt{Name: *name, Proto: *proto, Port: *port, Mgmt: *mgmt}
		if err := cfg.FirewallExemptAdd(e); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("added exemption %q\n", *name)

	case "del", "delete", "remove":
		if len(rest) == 0 {
			fatal("usage: gravinet fw exempt del IDX [IDX...]  (indices from 'fw exempt list')")
		}
		var idxs []int
		for _, a := range rest {
			for _, tok := range strings.Split(a, ",") {
				tok = strings.TrimSpace(tok)
				if tok == "" {
					continue
				}
				v, err := strconv.Atoi(tok)
				if err != nil {
					fatal("bad index %q", tok)
				}
				idxs = append(idxs, v)
			}
		}
		if err := cfg.FirewallExemptDelete(idxs); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("removed %d exemption(s)\n", len(idxs))

	case "reset":
		cfg.FirewallExemptReset()
		fmt.Println("reverted firewall exemptions to the built-in defaults")

	case "enable", "disable":
		if len(rest) == 0 {
			fatal("usage: gravinet fw exempt %s IDX  (indices from 'fw exempt list')", sub)
		}
		idx, err := strconv.Atoi(strings.TrimSpace(rest[0]))
		if err != nil {
			fatal("bad index %q", rest[0])
		}
		if err := cfg.FirewallExemptSetEnabled(idx, sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd exemption [%d]\n", sub, idx)

	default:
		fatal("unknown: gravinet fw exempt %s", sub)
	}
	commitCfg(cfg, path)
}

// ---- nat ---------------------------------------------------------------------

func cmdHost(args []string) {
	cfg, path, rest := openCfg(args)
	netName, rest := extractOpt(rest, "net")
	if len(rest) == 0 {
		fatal("usage: gravinet host <list|add NAME IP|remove NAME|enable NAME|disable NAME|reject NAME|reject-remove NAME|reject-enable NAME|reject-disable NAME> [-net NAME]")
	}
	sub, rest := rest[0], rest[1:]
	n := pickNetwork(cfg, netName)
	sub = expandVerb(sub, v("list"), v("add"), v("remove", "delete", "del"), v("enable", "disable"), v("reject"), v("reject-remove"), v("reject-enable", "reject-disable"))
	switch sub {
	case "list":
		fmt.Printf("network %s advertised hosts:\n", n.Name)
		if len(n.HostsAdvertise) == 0 {
			fmt.Println("  (none)")
		}
		for _, h := range n.HostsAdvertise {
			fmt.Printf("  %-30s %-15s %s\n", h.Name, h.IP, onOff(!h.Disabled))
		}
		fmt.Printf("network %s rejected hosts (refused from peers):\n", n.Name)
		if len(n.HostsReject) == 0 {
			fmt.Println("  (none)")
		}
		for _, h := range n.HostsReject {
			fmt.Printf("  %-30s %s\n", h.Name, onOff(!h.Disabled))
		}
		return
	case "add":
		if len(rest) < 2 {
			fatal("usage: gravinet host add NAME IP")
		}
		if err := cfg.HostAdd(netName, rest[0], rest[1]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("advertising %s -> %s on %s\n", rest[0], rest[1], n.Name)
	case "remove", "delete", "del":
		if len(rest) < 1 {
			fatal("usage: gravinet host remove NAME")
		}
		if err := cfg.HostDelete(netName, rest[0]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("stopped advertising %s on %s\n", rest[0], n.Name)
	case "enable", "disable":
		if len(rest) < 1 {
			fatal("usage: gravinet host %s NAME", sub)
		}
		if err := cfg.HostSetEnabled(netName, rest[0], sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd advertising %s on %s\n", sub, rest[0], n.Name)
	case "reject":
		if len(rest) < 1 {
			fatal("usage: gravinet host reject NAME")
		}
		if err := cfg.HostRejectAdd(netName, rest[0]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("rejecting host %s on %s\n", rest[0], n.Name)
	case "reject-remove":
		if len(rest) < 1 {
			fatal("usage: gravinet host reject-remove NAME")
		}
		if err := cfg.HostRejectDelete(netName, rest[0]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("stopped rejecting host %s on %s\n", rest[0], n.Name)
	case "reject-enable", "reject-disable":
		if len(rest) < 1 {
			fatal("usage: gravinet host %s NAME", sub)
		}
		if err := cfg.HostRejectSetEnabled(netName, rest[0], sub == "reject-enable"); err != nil {
			fatal("%v", err)
		}
		verb := "enabled"
		if sub == "reject-disable" {
			verb = "disabled"
		}
		fmt.Printf("%s host reject %s on %s\n", verb, rest[0], n.Name)
	default:
		fatal("unknown: gravinet host %s", sub)
	}
	commitCfg(cfg, path)
}

func cmdNAT(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet nat <add IFACE|delete IFACE|enable-rule INDEX|disable-rule INDEX|enable|disable|list> [-net NAME]")
	}
	sub := args[0]
	netName, rest := extractOpt(args[1:], "net")
	cfg, path, rest := openCfg(rest)
	n := pickNetwork(cfg, netName)

	sub = expandVerb(sub, v("list"), v("enable", "disable"), v("enable-rule", "disable-rule"), v("state"), v("add"), v("delete", "del", "remove"))
	switch sub {
	case "list":
		fmt.Printf("network %s NAT (%s)", n.Name, onOff(n.NAT.Enabled))
		st := cfg.NATStateTimeout
		if st <= 0 {
			fmt.Printf("  state-timeout=120s (global default)\n")
		} else {
			fmt.Printf("  state-timeout=%ds (global)\n", st)
		}
		if len(n.NAT.Rules) == 0 {
			fmt.Println("  (no rules)")
		}
		for i, r := range n.NAT.Rules {
			src := r.Source
			if src == "" {
				src = "any"
			}
			dst := r.Dest
			if dst == "" {
				dst = "any"
			}
			tgt := r.Translate
			if r.Interface != "" {
				tgt = r.Translate + " (" + r.Interface + ")"
			}
			fmt.Printf("  [%d] src=%-18s dst=%-18s -> %-22s %s\n",
				i, src, dst, tgt, onOff(r.Enabled))
		}
		return
	case "enable", "disable":
		if err := cfg.NATSetEnabled(netName, sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd NAT on %s\n", sub, n.Name)
	case "enable-rule", "disable-rule":
		if len(rest) == 0 {
			fatal("usage: gravinet nat %s INDEX  (see `gravinet nat list`)", sub)
		}
		idx, err := strconv.Atoi(rest[0])
		if err != nil {
			fatal("rule index must be a number")
		}
		if err := cfg.NATRuleSetEnabled(netName, idx, sub == "enable-rule"); err != nil {
			fatal("%v", err)
		}
		verb := "enabled"
		if sub == "disable-rule" {
			verb = "disabled"
		}
		fmt.Printf("%s NAT rule [%d] on %s\n", verb, idx, n.Name)
	case "state":
		if len(rest) == 0 {
			fatal("usage: gravinet nat state SECONDS  (0 = default 120s)")
		}
		secs, err := strconv.Atoi(rest[0])
		if err != nil {
			fatal("state timeout must be a number of seconds")
		}
		if err := cfg.NATStateTimeoutSet(secs); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("set global NAT state timeout to %ds\n", secs)
	case "add":
		// Two forms: a bare interface (masquerade shorthand) or keyword args
		// source/dest/translate/iface for a full rule. translate itself
		// carries whether the rule masquerades/statically SNATs (a literal
		// address, or "masquerade") or port-forwards/DNATs
		// ("port-forward:<ipv4>") — there's no separate direction keyword.
		src := kw(rest, "source")
		dst := kw(rest, "dest")
		translate := kw(rest, "translate")
		iface := kw(rest, "iface")
		if src == "" && dst == "" && translate == "" && iface == "" {
			if len(rest) == 0 {
				fatal("usage: gravinet nat add IFACE  |  nat add [source CIDR] [dest CIDR] (translate ADDR|masquerade|port-forward:ADDR | iface IFACE)")
			}
			iface = rest[0] // bare-interface masquerade shorthand
		}
		if err := cfg.NATRuleAdd(netName, src, dst, translate, iface); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("added NAT rule on %s\n", n.Name)
	case "delete", "del", "remove":
		if len(rest) == 0 {
			fatal("usage: gravinet nat delete INDEX  (see `gravinet nat list`)  |  nat delete IFACE")
		}
		if idx, err := strconv.Atoi(rest[0]); err == nil {
			if e := cfg.NATRuleDeleteAt(netName, idx); e != nil {
				fatal("%v", e)
			}
			fmt.Printf("deleted NAT rule [%d] on %s\n", idx, n.Name)
		} else {
			if e := cfg.NATDelete(netName, rest[0]); e != nil {
				fatal("%v", e)
			}
			fmt.Printf("deleted NAT rule for %s on %s\n", rest[0], n.Name)
		}
	default:
		fatal("unknown: gravinet nat %s", sub)
	}
	commitCfg(cfg, path)
}

// ---- qos ---------------------------------------------------------------------

func cmdQoS(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet qos <add MATCH priority LEVEL|delete MATCH|enable-rule MATCH|disable-rule MATCH|mark CLASS DSCP|unmark CLASS|enable|disable|list> [-net NAME]\n" +
			"  MATCH is either 'PROTO PORT' or 'service NAME[,NAME2,...]' — the latter\n" +
			"  names entries from the firewall's service catalog ('gravinet firewall service ...').")
	}
	sub := args[0]
	netName, rest := extractOpt(args[1:], "net")
	cfg, path, rest := openCfg(rest)
	n := pickNetwork(cfg, netName)

	if n.QoS.Classes == 0 {
		n.QoS.Classes = 3
	}

	sub = expandVerb(sub, v("list"), v("enable", "disable"), v("enable-rule", "disable-rule"), v("add"), v("delete", "del", "remove"), v("mark"), v("unmark"))
	switch sub {
	case "list":
		fmt.Printf("network %s QoS (%s, %d classes, default class %d):\n",
			n.Name, onOff(n.QoS.Enabled), n.QoS.Classes, n.QoS.DefaultClass)
		for cl := 0; cl < n.QoS.Classes; cl++ {
			dscp := mesh.DefaultClassDSCP(cl, n.QoS.Classes, n.QoS.DefaultClass)
			override := ""
			if cl < len(n.QoS.ClassDSCP) && n.QoS.ClassDSCP[cl] >= 0 {
				dscp = n.QoS.ClassDSCP[cl]
				override = " (override)"
			}
			fmt.Printf("  class %d (%-7s) marks traffic %s%s\n", cl, className(cl, n.QoS.Classes), config.DSCPName(dscp), override)
		}
		if len(n.QoS.Rules) == 0 {
			fmt.Println("  (no rules)")
		}
		for _, r := range n.QoS.Rules {
			fmt.Printf("  %-28s -> class %d (%s) %s\n", qosRuleMatchLabel(r), r.Class, className(r.Class, n.QoS.Classes), onOff(!r.Disabled))
		}
		return
	case "enable", "disable":
		if err := cfg.QoSSetEnabled(netName, sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd QoS on %s\n", sub, n.Name)
	case "enable-rule", "disable-rule":
		if len(rest) < 1 {
			fatal("usage: gravinet qos %s MATCH", sub)
		}
		proto, port, services, _ := parseQoSMatch(sub, rest)
		if err := cfg.QoSRuleSetEnabled(netName, proto, port, services, sub == "enable-rule"); err != nil {
			fatal("%v", err)
		}
		verb := "enabled"
		if sub == "disable-rule" {
			verb = "disabled"
		}
		fmt.Printf("%s QoS rule %s on %s\n", verb, qosRuleMatchLabel(config.QoSRule{Protocol: proto, PortMin: port, PortMax: port, Services: services}), n.Name)
	case "add":
		// gravinet qos add tcp 3389 priority highest
		// gravinet qos add service ssh,rdp priority highest
		if len(rest) < 1 {
			fatal("usage: gravinet qos add MATCH priority LEVEL")
		}
		proto, port, services, remainder := parseQoSMatch(sub, rest)
		class := priorityToClass(kw(remainder, "priority"), n.QoS.Classes)
		if err := cfg.QoSAdd(netName, proto, port, services, class); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("added QoS %s -> class %d (%s) on %s\n", qosRuleMatchLabel(config.QoSRule{Protocol: proto, PortMin: port, PortMax: port, Services: services}), class, className(class, n.QoS.Classes), n.Name)
	case "delete", "del", "remove":
		if len(rest) < 1 {
			fatal("usage: gravinet qos delete MATCH")
		}
		proto, port, services, _ := parseQoSMatch(sub, rest)
		if err := cfg.QoSDelete(netName, proto, port, services); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("deleted QoS rule %s on %s\n", qosRuleMatchLabel(config.QoSRule{Protocol: proto, PortMin: port, PortMax: port, Services: services}), n.Name)
	case "mark":
		// gravinet qos mark 0 46   (mark class 0's traffic EF/DSCP 46)
		if len(rest) < 2 {
			fatal("usage: gravinet qos mark CLASS DSCP")
		}
		class, err := strconv.Atoi(rest[0])
		if err != nil {
			fatal("invalid class %q", rest[0])
		}
		dscp, err := strconv.Atoi(rest[1])
		if err != nil {
			fatal("invalid dscp %q", rest[1])
		}
		if err := cfg.QoSSetClassDSCP(netName, class, dscp); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("class %d (%s) on %s now marks traffic %s\n", class, className(class, n.QoS.Classes), n.Name, config.DSCPName(dscp))
	case "unmark":
		if len(rest) < 1 {
			fatal("usage: gravinet qos unmark CLASS")
		}
		class, err := strconv.Atoi(rest[0])
		if err != nil {
			fatal("invalid class %q", rest[0])
		}
		if err := cfg.QoSClearClassDSCP(netName, class); err != nil {
			fatal("%v", err)
		}
		def := mesh.DefaultClassDSCP(class, n.QoS.Classes, n.QoS.DefaultClass)
		fmt.Printf("class %d (%s) on %s reverted to default mark %s\n", class, className(class, n.QoS.Classes), n.Name, config.DSCPName(def))
	default:
		fatal("unknown: gravinet qos %s", sub)
	}
	commitCfg(cfg, path)
}

// parseQoSMatch parses a qos subcommand's MATCH argument(s), which are either
// "PROTO PORT" (a literal leg, unchanged from before named services existed)
// or "service NAME[,NAME2,...]" (one or more entries from the firewall
// service catalog — see FirewallService/QoSRule.Services). Returns the
// resolved proto/port/services plus whatever args followed the match (e.g.
// "priority LEVEL" for add).
func parseQoSMatch(sub string, rest []string) (proto string, port int, services []string, remainder []string) {
	if strings.EqualFold(rest[0], "service") {
		if len(rest) < 2 {
			fatal("usage: gravinet qos %s service NAME[,NAME2,...]", sub)
		}
		for _, s := range strings.Split(rest[1], ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				services = append(services, s)
			}
		}
		if len(services) == 0 {
			fatal("usage: gravinet qos %s service NAME[,NAME2,...]", sub)
		}
		return "", 0, services, rest[2:]
	}
	if len(rest) < 2 {
		fatal("usage: gravinet qos %s PROTO PORT", sub)
	}
	return strings.ToLower(rest[0]), mustPort(rest[1]), nil, rest[2:]
}

// qosRuleMatchLabel renders a rule's proto/port/services match for CLI
// output, e.g. "tcp port 3389", "services ssh,rdp", or "any" for a catch-all.
func qosRuleMatchLabel(r config.QoSRule) string {
	var parts []string
	if r.Protocol != "" || r.PortMin != 0 || r.PortMax != 0 {
		port := fmt.Sprintf("%d", r.PortMin)
		if r.PortMax != r.PortMin {
			port = fmt.Sprintf("%d-%d", r.PortMin, r.PortMax)
		}
		proto := r.Protocol
		if proto == "" {
			proto = "any"
		}
		parts = append(parts, fmt.Sprintf("%s port %s", proto, port))
	}
	if len(r.Services) > 0 {
		parts = append(parts, "services "+strings.Join(r.Services, ","))
	}
	if len(parts) == 0 {
		return "any"
	}
	return strings.Join(parts, " + ")
}

// ---- bandwidth ---------------------------------------------------------------

func cmdBandwidth(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet bandwidth <up|down|both RATE [interface IFACE]|enable|disable|list> [-net NAME]")
	}
	sub := args[0]
	netName, rest := extractOpt(args[1:], "net")
	cfg, path, rest := openCfg(rest)

	if sub == "list" {
		for _, n := range cfg.Networks {
			t := n.Throttle
			fmt.Printf("%-16s %-9s up=%s down=%s (tun=%s)\n",
				n.Name, onOff(t.Enabled), rateStr(t.UpBytesPerSec), rateStr(t.DownBytesPerSec), n.TUNName)
		}
		return
	}

	if sub == "enable" || sub == "disable" {
		n := pickNetwork(cfg, netName)
		if err := cfg.ThrottleSetEnabled(n.Name, sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd bandwidth limit on %s\n", sub, n.Name)
		commitCfg(cfg, path)
		return
	}

	if len(rest) == 0 {
		fatal("usage: gravinet bandwidth %s RATE [interface IFACE]", sub)
	}
	bps := mustRate(rest[0])
	iface := kw(rest, "interface")
	var n *config.Network
	if iface != "" {
		for i := range cfg.Networks {
			if cfg.Networks[i].TUNName == iface {
				n = &cfg.Networks[i]
				break
			}
		}
		if n == nil {
			n = pickNetwork(cfg, netName)
			n.TUNName = iface // record the requested interface name
		}
	} else {
		n = pickNetwork(cfg, netName)
	}

	if err := cfg.ThrottleSet(n.Name, sub, bps); err != nil {
		fatal("%v", err)
	}
	msg := fmt.Sprintf("set %s bandwidth on %s to %s", sub, n.Name, rateStr(bps))
	if !n.Throttle.Enabled {
		msg += " (limiting is off — run 'gravinet bandwidth enable' to apply it)"
	}
	fmt.Println(msg)
	commitCfg(cfg, path)
}

// ---- list (whole config) -----------------------------------------------------

func cmdConfigList(args []string) {
	cfg, path, _ := openCfg(args)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fatal("render config: %v", err)
	}
	fmt.Printf("# %s\n%s\n", path, data)
}

// ---- helpers -----------------------------------------------------------------

// optOrKw reads a value given either as a flag (-name V/--name=V) or as a bare
// keyword (name V).
func optOrKw(args []string, name string) string {
	if v, _ := extractOpt(args, name); v != "" {
		return v
	}
	return kw(args, name)
}

func mustV4CIDR(s string) {
	ip, _, err := net.ParseCIDR(s)
	if err != nil || ip.To4() == nil {
		fatal("subnet %q must be an IPv4 CIDR (e.g. 10.50.0.0/16); use subnet6 for IPv6", s)
	}
}

func mustV6CIDR(s string) {
	ip, _, err := net.ParseCIDR(s)
	if err != nil || ip.To4() != nil {
		fatal("subnet6 %q must be an IPv6 CIDR (e.g. fd00:80::/64)", s)
	}
}

// chooseSubnets resolves the v4/v6 overlay subnets for a new network. With no
// subnet/subnet6 given it auto-assigns a non-overlapping dual-stack pair. If
// either is given explicitly it uses exactly that — so you can pin your own
// range, or make a single-family (v4-only or v6-only) network.
func chooseSubnets(cfg *config.Config, rest []string) (string, string) {
	v4 := optOrKw(rest, "subnet")
	v6 := optOrKw(rest, "subnet6")
	if v4 == "" && v6 == "" {
		return nextFreeSubnets(cfg)
	}
	if v4 != "" {
		mustV4CIDR(v4)
	}
	if v6 != "" {
		mustV6CIDR(v6)
	}
	return v4, v6
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func nextFreeSubnets(cfg *config.Config) (string, string) { return cfg.NextFreeSubnets() }

func findNet(cfg *config.Config, name string) *config.Network { return cfg.FindNetwork(name) }

func deleteNet(cfg *config.Config, name string) bool { return cfg.NetworkDelete(name) == nil }

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func removeStr(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func ifStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

func mustCIDR(s string) {
	if _, _, err := net.ParseCIDR(s); err != nil {
		fatal("invalid CIDR %q: %v", s, err)
	}
}

func mustPort(s string) int {
	p, err := strconv.Atoi(s)
	if err != nil || p < 0 || p > 65535 {
		fatal("invalid port %q", s)
	}
	return p
}

// mustRate parses a rate like "150mbps" into bytes/s (fatal on error).
func mustRate(s string) int {
	b, err := config.ParseRate(s)
	if err != nil {
		fatal("%v", err)
	}
	return b
}

func rateStr(bytesPerSec int) string { return config.RateString(bytesPerSec) }

// priorityToClass maps a priority name to a class index (0 = highest).
func priorityToClass(level string, classes int) int {
	c, err := config.PriorityToClass(level, classes)
	if err != nil {
		fatal("%v", err)
	}
	return c
}

func className(class, classes int) string { return config.ClassName(class, classes) }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// cmdManaged toggles "managed" cluster mode (being managed). Off by
// default. Applied via commitCfg (live reload), not commitCfgStructural:
// engine.SetManaged is explicitly designed to take effect on a running
// daemon immediately (it's applied inside the same reloadFn that handles
// firewall/NAT/QoS/key changes, none of which restart either) — this command
// doesn't touch interfaces or sessions the way network add/enable/join do, so
// it doesn't belong in the restart-by-default bucket those use. No
// --no-restart flag here either: there's nothing to opt out of restarting.
func cmdManaged(args []string) {
	cfg, path, rest := openCfg(args)
	action := "status"
	if len(rest) > 0 && (rest[0] == "on" || rest[0] == "off" || rest[0] == "enable" || rest[0] == "disable" || rest[0] == "status") {
		action = rest[0]
	}
	switch action {
	case "on", "enable":
		cfg.Managed = true
		fmt.Println("managed mode ON — this node now advertises itself for remote management and accepts management over the overlay from mesh peers that are themselves in manager mode (see 'gravinet manager')")
		commitCfg(cfg, path)
	case "off", "disable":
		cfg.Managed = false
		fmt.Println("managed mode OFF")
		commitCfg(cfg, path)
	default:
		st := "off"
		if cfg.Managed {
			st = "on"
		}
		fmt.Printf("managed mode: %s\n", st)
	}
}

// cmdManager toggles "manager" cluster mode (managing others) — the other
// half of the managed/manager split (see config.Config's doc comments).
// Mirrors cmdManaged exactly: same live-reload path via engine.SetManager,
// same reasoning for skipping commitCfgStructural and --no-restart.
func cmdManager(args []string) {
	cfg, path, rest := openCfg(args)
	action := "status"
	if len(rest) > 0 && (rest[0] == "on" || rest[0] == "off" || rest[0] == "enable" || rest[0] == "disable" || rest[0] == "status") {
		action = rest[0]
	}
	switch action {
	case "on", "enable":
		cfg.Manager = true
		fmt.Println("manager mode ON — this node can now browse and manage other mesh peers that are in managed mode, from its header dropdown / proxy")
		commitCfg(cfg, path)
	case "off", "disable":
		cfg.Manager = false
		fmt.Println("manager mode OFF")
		commitCfg(cfg, path)
	default:
		st := "off"
		if cfg.Manager {
			st = "on"
		}
		fmt.Printf("manager mode: %s\n", st)
	}
}
