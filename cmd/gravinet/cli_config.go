package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
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
//
// Also carries a one-line audit summary of the CLI invocation that triggered
// this reload, in Request.Notes, so it lands in the daemon's own log —
// closing the gap left by config history (internal/config/history.go),
// which only ever sees changes made through the web admin (its one call
// site is webadmin.go's mutateConfig): a CLI-driven edit like this one
// otherwise leaves no record anywhere once it's saved. This intentionally
// doesn't try to give CLI edits full snapshot/diff/restore parity with web
// admin's history — just a log line — so it's the CLI process sending a
// string over the control socket it already talks to for reload, not a
// second process independently writing the log file. That distinction
// matters: RotatingFile tracks its size in memory per instance, and the
// daemon already holds its own long-lived instance open on that exact file
// (see cmd/gravinet's logx.SetOutput wiring) — a second, independent writer
// from a short-lived CLI process could race that instance's FIFO trimming
// and corrupt the file. Routing through the control socket keeps the
// daemon's own logx.Logger (see control.Server.log) the only writer, ever.
func reloadDaemon(sock string) bool {
	// Normalize rather than only defaulting an empty value: a config still holding
	// the stale "/run/gravinet.sock" is exactly the case where the daemon is bound
	// somewhere else (the platform default), and dialing the stale path verbatim
	// would report "daemon not reachable" about a daemon that is running fine.
	endpoint, _ := config.NormalizeControlSocket(sock)
	summary := strings.Join(redactSensitiveCLIArgs(os.Args[1:]), " ")
	resp, err := control.Do(endpoint, control.Request{Cmd: "reload", Notes: summary})
	return err == nil && resp.Error == ""
}

// reTokenLike/reKeyLike identify the two shapes of secret this CLI ever
// takes as a plain argument: a join token (config.joinTokenPrefix, "grav1.")
// and a raw base64 AES-256 network key ("key set <KEY>", "network join ID
// key KEY peer PEER"). reKeyLike matches base64's fixed output length for
// exactly 32 random bytes (crypto.GenerateKey's own size) — 44 characters
// including the trailing "=" pad — rather than any base64-looking string,
// so an ordinary argument that merely contains a slash or plus sign isn't
// swept up by accident.
var (
	reTokenLike = regexp.MustCompile(`^grav1\.`)
	reKeyLike   = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)
)

// redactSensitiveCLIArgs returns args with anything matching reTokenLike/
// reKeyLike replaced by a placeholder, so reloadDaemon's audit summary can
// otherwise include the rest of the invocation verbatim. Deliberately
// pattern-based rather than aware of which specific command/keyword
// position carries the secret (e.g. "key set's 3rd arg" specifically) — a
// future command that takes a secret positional stays covered by this
// without needing a matching update here.
func redactSensitiveCLIArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if reTokenLike.MatchString(a) || reKeyLike.MatchString(a) {
			out[i] = "<redacted>"
			continue
		}
		out[i] = a
	}
	return out
}

// commitCfgStructural saves a structural change (one that needs the daemon to
// rebuild interfaces/sessions, e.g. adding/joining/enabling a network) and, by
// default, restarts the service so it takes effect immediately.
//
// Unlike commitCfg, this never reaches reloadDaemon — a full service restart
// doesn't go through the control socket's "reload" command at all (the old
// process is torn down and a new one started fresh), so a command that lands
// here (mtu, subnet, address, key rotation) doesn't get the CLI audit line
// reloadDaemon adds to the daemon's log — see its doc comment. The new
// daemon's own startup sequence logs plenty about the resulting state, just
// not "why" or "via which CLI command." Known gap, not fixed here: closing
// it would mean passing a summary through the restart itself (an env var or
// a marker file the new process reads at startup), which is a genuinely
// different mechanism from reloadDaemon's, not a small extension of it.
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
		fatal("usage: gravinet network <add|delete|enable|disable|rename|notes|subnet|mtu|join|join-token|token|list> ...")
	}
	sub := args[0]
	cfg, path, rest := openCfg(args[1:])
	noRestart, rest := hasFlag(rest, "no-restart")

	sub = expandVerb(sub, v("list"), v("add"), v("delete", "del", "remove"), v("enable", "disable"), v("rename"), v("notes"), v("subnet", "set-subnet"), v("mtu"), v("join"), v("join-token"), v("token", "invite"))
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

	case "mtu":
		if len(rest) < 2 {
			fatal("usage: gravinet network mtu NAME BYTES   (e.g. gravinet network mtu corp 8915)")
		}
		mtu, err := strconv.Atoi(rest[1])
		if err != nil {
			fatal("mtu must be a number, got %q", rest[1])
		}
		advice, err := cfg.NetworkSetMTU(rest[0], mtu)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Printf("network %q mtu now %d\n  (restart required, and apply the same change on every node in this network)\n", rest[0], mtu)
		if advice != "" {
			fmt.Printf("  note: %s\n", advice)
		}
		// Same reasoning as "subnet" above: changing the MTU resizes the live
		// overlay interface, which the hot-reload path does not do.
		commitCfgStructural(cfg, path, noRestart)
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

// quickstartUsage is printed on any usage error from cmdQuickstart — shared
// so the two failure points in parseQuickstartArgs stay word-for-word
// identical rather than drifting apart.
const quickstartUsage = "usage: gravinet quickstart NAME [subnet CIDR] [subnet6 CIDR] [addr HOST:PORT] [expires DUR] [-config PATH] [-no-service]\n   or: gravinet quickstart join TOKEN [-config PATH] [-no-service]"

// parseQuickstartArgs validates cmdQuickstart's invocation shape and reports
// which of the two forms it is, given rest (the positional args left after
// -config/-no-service have already been stripped by the caller). Pulled out
// as its own pure function — like chooseSubnets/nextFreeSubnets above it —
// specifically so the shape (empty args, "join" with no token) is unit-
// testable without going through cmdQuickstart's fatal()/os.Exit path, and so
// cmdQuickstart itself can never index rest[0]/rest[1] before this has
// confirmed they exist: an earlier version indexed unconditionally and
// panicked on a flags-only invocation (see TestParseQuickstartArgs).
func parseQuickstartArgs(rest []string) (joining bool, netNameOrToken string, err error) {
	if len(rest) == 0 {
		return false, "", fmt.Errorf("%s", quickstartUsage)
	}
	if rest[0] == "join" {
		if len(rest) < 2 {
			return false, "", fmt.Errorf("usage: gravinet quickstart join TOKEN [-config PATH] [-no-service]")
		}
		return true, rest[1], nil
	}
	return false, rest[0], nil
}

// cmdQuickstart is a macro, not a new capability: it chains exactly the steps
// the README already tells a new operator to run by hand — create (or load) a
// config, add or join a network, mint a join token for the next node, and
// install the OS service — into one command, in the right order, using the
// exact same underlying calls (writeDefaultConfig, Config.NetworkAdd/
// NetworkJoinToken/NetworkToken, commitCfg, service.Install) those individual
// commands already use. It has no logic of its own beyond sequencing and
// friendlier output; anything it does can still be done step by step with the
// commands it wraps, and nothing here needs elevated trust beyond what those
// already have.
//
// Two forms:
//
//	gravinet quickstart NAME [subnet CIDR] [subnet6 CIDR] [addr HOST:PORT] [expires DUR] [-config PATH] [-no-service]
//	gravinet quickstart join TOKEN [-config PATH] [-no-service]
//
// The first stands up a brand-new mesh: NAME becomes the first (seed) node's
// network, and the token printed at the end is exactly what "network token"
// would print — hand it to the next machine's "gravinet quickstart join
// <token>". "join" is a reserved first word here the same way it is under
// "network"; a network genuinely named "join" needs "network add join"
// directly instead of this shorthand.
//
// Unlike every other config-editing command in this file, this one is the
// single place allowed to create the config file itself rather than fatal()
// on a missing one (openCfg's whole contract) — that's the actual gap this
// command exists to close: today a fresh install needs "run -init" (or a
// first service start) before any "network add"/"join" can succeed at all.
func cmdQuickstart(args []string) {
	path, rest := extractOpt(args, "config")
	if path == "" {
		path = defaultConfigPath
	}
	noService, rest := hasFlag(rest, "no-service")

	// Validate the invocation shape fully before touching the filesystem at
	// all — writeDefaultConfig below is a real side effect (a file left on
	// disk), and running it ahead of a usage error would mean a mistyped
	// invocation (e.g. only flags, no NAME/TOKEN) leaves a stray empty
	// config behind instead of just failing cleanly.
	joining, netNameOrToken, err := parseQuickstartArgs(rest)
	if err != nil {
		fatal("%v", err)
	}

	// The one deviation from every other command here: create the config if
	// it's not there yet, instead of erroring — see the doc comment above.
	// writeDefaultConfig itself refuses nothing (it's also what an
	// automatic first-start bootstrap uses), so this is safe to call
	// whether or not a service has ever run against this path before.
	freshConfig := false
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			fatal("stat %s: %v", path, err)
		}
		if err := writeDefaultConfig(path); err != nil {
			fatal("write config: %v", err)
		}
		freshConfig = true
	}
	cfg, err := config.Load(path)
	if err != nil {
		fatal("load config %s: %v", path, err)
	}
	if freshConfig {
		fmt.Printf("wrote a new config to %s\n", path)
	} else {
		fmt.Printf("using existing config at %s\n", path)
	}

	var netName string
	if joining {
		id, name, err := cfg.NetworkJoinToken(netNameOrToken)
		if err != nil {
			fatal("%v", err)
		}
		netName = name
		fmt.Printf("joined network %s%s from token\n  name and subnet are learned from the network once a peer connects\n",
			id, ifStr(name != "", " ("+name+")", ""))
	} else {
		netName = netNameOrToken
		v4 := optOrKw(rest, "subnet")
		v6 := optOrKw(rest, "subnet6")
		n, err := cfg.NetworkAdd(netName, v4, v6)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Printf("added network %q (id %s, subnet4=%s subnet6=%s, generated key)\n",
			netName, n.ID, orNone(n.Subnet4), orNone(n.Subnet6))

		// Mint the token for the *next* node right away — the whole point of
		// quickstart is that this is the very next thing an operator needs,
		// so don't make them run "network token" as a separate step.
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
		seedCount := cfg.TokenSeedCount(netName, extra)
		tok, err := cfg.NetworkToken(netName, extra, ttl)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println("\njoin token for the next node:")
		fmt.Println(tok)
		fmt.Println("(contains the network key — share it over a secure channel; join with 'gravinet quickstart join <token>')")
		if seedCount == 0 {
			fmt.Println("no bootstrap seed embedded — pass 'addr HOST:PORT' (this node's reachable underlay endpoint) next time, or the joiner adds a seed manually")
		}
	}

	fmt.Println()
	commitCfg(cfg, path)

	if noService {
		fmt.Println("\nskipping service install (-no-service); run 'gravinet run -config " + path + "' directly, or 'gravinet service install' when ready")
		return
	}
	opts := service.Defaults()
	opts.ConfigPath = path
	svcPath, next, err := service.Install(opts)
	if err != nil {
		fatal("service install: %v", err)
	}
	fmt.Println()
	if svcPath != "" {
		fmt.Printf("wrote %s\n", svcPath)
	}
	fmt.Printf("next: %s\n", next)
	if !joining {
		fmt.Printf("\nquickstart complete for network %q — run the command above to bring gravinet up, then hand the join token to the next node.\n", netName)
	} else {
		fmt.Printf("\nquickstart complete — run the command above to bring gravinet up and join network %q.\n", netName)
	}
}

// ---- route -------------------------------------------------------------------

// cmdSeed manages a network's bootstrap seed addresses (host, host:port, or
// host:port,port,... to try more than one port against the same host).
// Config-file style like route/nat: edit and live-reload. Unlike connected
// peers, seeds persist whether or not anyone is currently connected.
func cmdSeed(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet seed <list|add|remove|enable|disable|notes> [ADDR] [-net NAME] [-notes N]")
	}
	sub := args[0]
	netName, rest := extractOpt(args[1:], "net")
	notes, rest := extractOpt(rest, "notes")
	cfg, path, rest := openCfg(rest)
	n := pickNetwork(cfg, netName)

	sub = expandVerb(sub, v("list"), v("add"), v("remove", "delete", "del"), v("enable", "disable"), v("notes"))
	switch sub {
	case "list":
		fmt.Printf("network %s seeds:\n", n.Name)
		if len(n.Seeds) == 0 {
			fmt.Println("  (none)")
		}
		for _, s := range n.Seeds {
			if s.Notes != "" {
				fmt.Printf("  %-30s %-8s %s\n", s.Address, onOff(!s.Disabled), s.Notes)
			} else {
				fmt.Printf("  %-30s %s\n", s.Address, onOff(!s.Disabled))
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

	case "enable", "disable":
		if len(rest) == 0 {
			fatal("usage: gravinet seed %s ADDR", sub)
		}
		if err := cfg.SeedSetEnabled(netName, rest[0], sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd seed %s on %s\n", sub, rest[0], n.Name)
		if sub == "disable" {
			fmt.Println("note: this stops the address being dialed and drops any session standing on it, but it does not keep the node behind it away — on a full mesh another peer usually gossips it back within seconds. To keep a node disconnected, disable the peer itself under Mesh > Peers in the web admin.")
		}

	case "notes":
		if len(rest) == 0 {
			fatal("usage: gravinet seed notes ADDR [TEXT...]  (empty TEXT clears the note)")
		}
		// Trailing words are the note. This was "-notes TEXT", a mandatory
		// flag sitting right next to "gravinet network notes NAME TEXT...",
		// which has taken its text positionally all along — same operation,
		// two spellings, for no reason anyone could have named. -notes is
		// still accepted and still wins when both are given.
		text := notes
		if len(rest) > 1 {
			text = strings.Join(rest[1:], " ")
		}
		if err := cfg.SeedSetNotes(netName, rest[0], text); err != nil {
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

	case "prefer":
		if len(rest) < 2 {
			fatal("usage: gravinet route prefer CIDR NODEID [NODEID...]   (most preferred first)")
		}
		cidr := rest[0]
		if err := cfg.RoutePrefer(netName, cidr, rest[1:]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("preferring %s for %s on %s, in that order — falls back automatically when one is unreachable\n",
			strings.Join(rest[1:], ", "), cidr, n.Name)

	case "prefer-clear", "prefer-remove":
		if len(rest) == 0 {
			fatal("usage: gravinet route prefer-clear CIDR")
		}
		if err := cfg.RoutePreferRemove(netName, rest[0]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("cleared origin preference for %s on %s — lowest advertised metric wins again\n", rest[0], n.Name)

	case "prefer-enable", "prefer-disable":
		if len(rest) == 0 {
			fatal("usage: gravinet route %s CIDR", sub)
		}
		if err := cfg.RoutePreferSetEnabled(netName, rest[0], sub == "prefer-enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd origin preference for %s on %s\n", strings.TrimPrefix(sub, "prefer-"), rest[0], n.Name)

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
		// The name is positional: it was mandatory anyway ("exemption needs a
		// -name"), and "fw exempt del IDX" three arms below has always taken
		// its subject as an argument. -proto/-port/-mgmt stay flags — those
		// have real defaults and are genuinely optional.
		pos, rest := splitPositionals(rest, "name", "proto", "port")
		fs := flag.NewFlagSet("fw exempt add", flag.ExitOnError)
		nameFlag := fs.String("name", "", "deprecated: pass the name as an argument instead")
		proto := fs.String("proto", "any", "tcp|udp|icmp|ospf|<number>|any")
		port := fs.Int("port", 0, "port; matches source OR destination (0 = any/port-less)")
		mgmt := fs.Bool("mgmt", false, "track this node's web-admin port (overrides -port)")
		fs.Parse(rest)
		name := *nameFlag
		if len(pos) > 0 {
			name = strings.Join(pos, " ")
		}
		if name == "" {
			fatal("usage: gravinet fw exempt add NAME [-proto P] [-port N] [-mgmt]")
		}
		e := config.FirewallExempt{Name: name, Proto: *proto, Port: *port, Mgmt: *mgmt}
		if err := cfg.FirewallExemptAdd(e); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("added exemption %q\n", name)

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
		fatal("usage: gravinet nat <add IFACE|delete IFACE|enable-rule INDEX|disable-rule INDEX|enable|disable|list> [scope NAME]")
	}
	sub := args[0]
	// -net is gone: NAT is node-global from v953. A rule's optional "scope"
	// keyword names a mesh network whose overlay traffic it also applies to,
	// which is a property of the rule rather than a selector for which
	// network's table to edit.
	cfg, path, rest := openCfg(args[1:])

	sub = expandVerb(sub, v("list"), v("enable", "disable"), v("enable-rule", "disable-rule"), v("state"), v("add"), v("delete", "del", "remove"))
	switch sub {
	case "list":
		fmt.Printf("NAT (%s)", onOff(cfg.NAT.Enabled))
		st := cfg.NATStateTimeout
		if st <= 0 {
			fmt.Printf("  state-timeout=120s (global default)\n")
		} else {
			fmt.Printf("  state-timeout=%ds (global)\n", st)
		}
		if len(cfg.NAT.Rules) == 0 {
			fmt.Println("  (no rules)")
		}
		for i, r := range cfg.NAT.Rules {
			src := r.Source
			if src == "" {
				src = "any"
			}
			dst := r.Dest
			if dst == "" {
				dst = "any"
			}
			if r.DestPort != "" {
				dst += ":" + r.DestPort + "/" + r.Proto
			}
			tgt := r.Translate
			if r.Interface != "" {
				tgt = r.Translate + " (" + r.Interface + ")"
			}
			scope := r.Scope
			if scope == "" {
				scope = "host"
			}
			fmt.Printf("  [%d] src=%-18s dst=%-24s -> %-22s scope=%-10s %s\n",
				i, src, dst, tgt, scope, onOff(r.Enabled))
		}
		return
	case "enable", "disable":
		if err := cfg.NATSetEnabled(sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd NAT\n", sub)
	case "enable-rule", "disable-rule":
		if len(rest) == 0 {
			fatal("usage: gravinet nat %s INDEX  (see `gravinet nat list`)", sub)
		}
		idx, err := strconv.Atoi(rest[0])
		if err != nil {
			fatal("rule index must be a number")
		}
		if err := cfg.NATRuleSetEnabled(idx, sub == "enable-rule"); err != nil {
			fatal("%v", err)
		}
		verb := "enabled"
		if sub == "disable-rule" {
			verb = "disabled"
		}
		fmt.Printf("%s NAT rule [%d]\n", verb, idx)
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
		// ("port-forward:<ipv4>[:<port>]") — there's no separate direction
		// keyword. dest-port/proto scope a port-forward rule to a specific
		// port or range (PAT) instead of every port on dest — see
		// config.NATRule.DestPort's doc comment.
		src := kw(rest, "source")
		dst := kw(rest, "dest")
		destPort := kw(rest, "dest-port")
		proto := kw(rest, "proto")
		translate := kw(rest, "translate")
		iface := kw(rest, "iface")
		scope := kw(rest, "scope")
		if src == "" && dst == "" && destPort == "" && proto == "" && translate == "" && iface == "" {
			if len(rest) == 0 {
				fatal("usage: gravinet nat add IFACE  |  nat add [source CIDR] [dest CIDR] [dest-port N|N-M] [proto tcp|udp] (translate ADDR|masquerade|port-forward:ADDR[:PORT] | iface IFACE)")
			}
			iface = rest[0] // bare-interface masquerade shorthand
		}
		if err := cfg.NATRuleAdd(src, dst, destPort, proto, translate, iface, scope); err != nil {
			fatal("%v", err)
		}
		fmt.Println("added NAT rule")
	case "delete", "del", "remove":
		if len(rest) == 0 {
			fatal("usage: gravinet nat delete INDEX  (see `gravinet nat list`)  |  nat delete IFACE")
		}
		if idx, err := strconv.Atoi(rest[0]); err == nil {
			if e := cfg.NATRuleDeleteAt(idx); e != nil {
				fatal("%v", e)
			}
			fmt.Printf("deleted NAT rule [%d]\n", idx)
		} else {
			if e := cfg.NATDelete(rest[0]); e != nil {
				fatal("%v", e)
			}
			fmt.Printf("deleted NAT rule for %s\n", rest[0])
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
	// -net is gone: QoS is node-global from v954. A rule's optional "scope"
	// keyword names the mesh network it classifies on, blank for every one.
	cfg, path, rest := openCfg(args[1:])

	if cfg.QoS.Classes == 0 {
		cfg.QoS.Classes = 3
	}

	sub = expandVerb(sub, v("list"), v("enable", "disable"), v("enable-rule", "disable-rule"), v("add"), v("delete", "del", "remove"), v("mark"), v("unmark"))
	switch sub {
	case "list":
		fmt.Printf("QoS (%s, %d classes, default class %d):\n",
			onOff(cfg.QoS.Enabled), cfg.QoS.Classes, cfg.QoS.DefaultClass)
		for cl := 0; cl < cfg.QoS.Classes; cl++ {
			dscp := mesh.DefaultClassDSCP(cl, cfg.QoS.Classes, cfg.QoS.DefaultClass)
			override := ""
			if cl < len(cfg.QoS.ClassDSCP) && cfg.QoS.ClassDSCP[cl] >= 0 {
				dscp = cfg.QoS.ClassDSCP[cl]
				override = " (override)"
			}
			fmt.Printf("  class %d (%-7s) marks traffic %s%s\n", cl, className(cl, cfg.QoS.Classes), config.DSCPName(dscp), override)
		}
		if len(cfg.QoS.Rules) == 0 {
			fmt.Println("  (no rules)")
		}
		for _, r := range cfg.QoS.Rules {
			scope := r.Scope
			if scope == "" {
				scope = "any"
			}
			fmt.Printf("  %-28s -> class %d (%s) scope=%-10s %s\n",
				qosRuleMatchLabel(r), r.Class, className(r.Class, cfg.QoS.Classes), scope, onOff(!r.Disabled))
		}
		return
	case "enable", "disable":
		if err := cfg.QoSSetEnabled(sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd QoS \n", sub)
	case "enable-rule", "disable-rule":
		if len(rest) < 1 {
			fatal("usage: gravinet qos %s MATCH", sub)
		}
		proto, port, services, _ := parseQoSMatch(sub, rest)
		if err := cfg.QoSRuleSetEnabled(proto, port, services, kw(rest, "scope"), sub == "enable-rule"); err != nil {
			fatal("%v", err)
		}
		verb := "enabled"
		if sub == "disable-rule" {
			verb = "disabled"
		}
		fmt.Printf("%s QoS rule %s\n", verb, qosRuleMatchLabel(config.QoSRule{Protocol: proto, PortMin: port, PortMax: port, Services: services}))
	case "add":
		// gravinet qos add tcp 3389 priority highest
		// gravinet qos add service ssh,rdp priority highest
		if len(rest) < 1 {
			fatal("usage: gravinet qos add MATCH priority LEVEL")
		}
		proto, port, services, remainder := parseQoSMatch(sub, rest)
		class := priorityToClass(kw(remainder, "priority"), cfg.QoS.Classes)
		if err := cfg.QoSAdd(proto, port, services, class, kw(rest, "scope")); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("added QoS %s -> class %d (%s)\n", qosRuleMatchLabel(config.QoSRule{Protocol: proto, PortMin: port, PortMax: port, Services: services}), class, className(class, cfg.QoS.Classes))
	case "delete", "del", "remove":
		if len(rest) < 1 {
			fatal("usage: gravinet qos delete MATCH")
		}
		proto, port, services, _ := parseQoSMatch(sub, rest)
		if err := cfg.QoSDelete(proto, port, services, kw(rest, "scope")); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("deleted QoS rule %s\n", qosRuleMatchLabel(config.QoSRule{Protocol: proto, PortMin: port, PortMax: port, Services: services}))
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
		if err := cfg.QoSSetClassDSCP(class, dscp); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("class %d (%s) now marks traffic %s\n", class, className(class, cfg.QoS.Classes), config.DSCPName(dscp))
	case "unmark":
		if len(rest) < 1 {
			fatal("usage: gravinet qos unmark CLASS")
		}
		class, err := strconv.Atoi(rest[0])
		if err != nil {
			fatal("invalid class %q", rest[0])
		}
		if err := cfg.QoSClearClassDSCP(class); err != nil {
			fatal("%v", err)
		}
		def := mesh.DefaultClassDSCP(class, cfg.QoS.Classes, cfg.QoS.DefaultClass)
		fmt.Printf("class %d (%s) reverted to default mark %s\n", class, className(class, cfg.QoS.Classes), config.DSCPName(def))
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

// ---- shaping -----------------------------------------------------------------

// cmdBandwidth implements `gravinet bandwidth` (alias `bw`, group form
// `traffic shaping`). Keyed by interface since v960: -iface names the
// interface whose entry is being read or written, which is the thing the
// shaper is actually attached to.
func cmdBandwidth(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet bandwidth <list|add|del|enable|disable|up RATE|down RATE|both RATE> [-iface NAME]")
	}
	sub := args[0]
	iface, rest := extractOpt(args[1:], "iface")
	cfg, path, rest := openCfg(rest)

	if sub == "list" {
		if len(cfg.Shaping) == 0 {
			fmt.Println("no interface is shaped")
		}
		mesh := map[string]string{}
		for i, n := range cfg.Networks {
			mesh[cfg.IfaceForNetworkAt(i)] = n.Name
		}
		for _, sh := range cfg.Shaping {
			carries := "not a gravinet interface — nothing enforces this"
			if name, ok := mesh[sh.Iface]; ok {
				carries = "carries " + name
			}
			fmt.Printf("%-16s %-9s up=%s down=%s (%s)\n", sh.Iface, onOff(sh.Enabled),
				rateStr(sh.UpBytesPerSec), rateStr(sh.DownBytesPerSec), carries)
		}
		// Named once, at the end, rather than left to be inferred from the
		// per-row note above: gravinet shapes in its own data path, so an
		// entry on an interface it does not carry a network on is inert.
		if un := cfg.ShapingUnenforced(); len(un) > 0 {
			noun, poss := "this interface", "its"
			if len(un) > 1 {
				noun, poss = "these interfaces", "their"
			}
			fmt.Printf("  %s: gravinet moves no packets on %s, so %s rate is configured but not applied\n",
				strings.Join(un, ", "), noun, poss)
		}
		return
	}

	if iface == "" {
		fatal("usage: gravinet bandwidth %s -iface NAME", sub)
	}

	switch sub {
	case "add":
		if err := cfg.ShapingAdd(iface); err != nil {
			fatal("%v", err)
		}
		msg := fmt.Sprintf("added a shaping entry for %s, off and unlimited", iface)
		if !cfgHasMeshIface(cfg, iface) {
			msg += "\n  note: this node carries no mesh network on " + iface + ", so nothing will enforce a rate set here"
		}
		fmt.Println(msg)
		commitCfg(cfg, path)
		return
	case "del":
		if err := cfg.ShapingDelete(iface); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("removed the shaping entry for %s; it is now unshaped\n", iface)
		commitCfg(cfg, path)
		return
	case "enable", "disable":
		if err := cfg.ShapingSetEnabled(iface, sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd shaping on %s\n", sub, iface)
		commitCfg(cfg, path)
		return
	}

	if len(rest) == 0 {
		fatal("usage: gravinet bandwidth %s RATE -iface NAME", sub)
	}
	bps := mustRate(rest[0])
	if err := cfg.ShapingSet(iface, sub, bps); err != nil {
		fatal("%v", err)
	}
	msg := fmt.Sprintf("set %s bandwidth on %s to %s", sub, iface, rateStr(bps))
	if sh := cfg.ShapingFor(iface); sh != nil && !sh.Enabled {
		msg += " (shaping is off — run 'gravinet bandwidth enable -iface " + iface + "' to apply it)"
	}
	if !cfgHasMeshIface(cfg, iface) {
		msg += "\n  note: this node carries no mesh network on " + iface + ", so nothing enforces this rate"
	}
	fmt.Println(msg)
	commitCfg(cfg, path)
}

// cfgHasMeshIface reports whether one of this node's networks runs on iface.
func cfgHasMeshIface(cfg *config.Config, iface string) bool {
	for _, name := range cfg.MeshIfaces() {
		if name == iface {
			return true
		}
	}
	return false
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
