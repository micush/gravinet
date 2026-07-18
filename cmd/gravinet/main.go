// Command gravinet is a single-binary, full-mesh encrypted overlay VPN.
//
// Usage:
//
//	gravinet run     -config /etc/gravinet/config.json   (Windows default: %ProgramData%\gravinet\config.json; FreeBSD default: /usr/local/etc/gravinet/config.json)
//	gravinet genkey  [-n 1]
//	gravinet version
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/control"
	"gravinet/internal/crypto"
	"gravinet/internal/hosts"
	"gravinet/internal/ipfwd"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
	"gravinet/internal/netfilter"
	"gravinet/internal/resolver"
	"gravinet/internal/service"
	"gravinet/internal/transport"
	"gravinet/internal/tun"
	"gravinet/internal/upgrade"
	"gravinet/internal/upnp"
	"gravinet/internal/webadmin"
)

// Build metadata, overridable via -ldflags.
var (
	version = "512"
	commit  = "none"
)

// shutdownGrace bounds how long graceful teardown may take before the daemon
// force-exits itself (see the shutdown watchdog in runBody). A clean shutdown
// is far quicker than this; the budget only exists so a teardown step wedged
// on the kernel or a subprocess can't hang the process — and, because the
// systemd unit is Type=notify, hang a `systemctl restart` waiting on this
// process to exit. Kept below the systemd unit's TimeoutStopSec (8s) so the
// daemon's own force-exit wins the race in the normal stuck case, producing a
// clean "forcing exit" log line rather than an abrupt SIGKILL; the unit's
// TimeoutStopSec + SendSIGKILL is the independent outer backstop for when even
// os.Exit can't run. If you raise the unit's TimeoutStopSec, raise this too
// (staying below it) to preserve that ordering.
const shutdownGrace = 5 * time.Second

// armShutdownWatchdog starts a timer that invokes onTimeout if the returned
// disarm func hasn't been called within grace. It's how the shutdown path
// guarantees the process exits even when a teardown step wedges: onTimeout is
// os.Exit in production (see runBody), so a stuck engine.Stop()/device
// close/nftables clear can't hang the process — and therefore can't hang a
// Type=notify `systemctl restart` waiting on this process to exit. disarm is
// idempotent and safe to defer; calling it after a clean teardown cancels the
// timer so onTimeout never runs. Extracted from runBody so the arm/disarm
// race is unit-testable without actually exiting the process.
func armShutdownWatchdog(grace time.Duration, onTimeout func()) (disarm func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return // disarmed: graceful teardown completed in time
		case <-time.After(grace):
			onTimeout()
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "genkey":
		cmdGenKey(os.Args[2:])
	case "genpass":
		cmdGenPass(os.Args[2:])
	case "ban":
		cmdBan(os.Args[2:])
	case "unban":
		cmdUnban(os.Args[2:])
	case "list":
		cmdConfigList(os.Args[2:])
	case "status":
		cmdList(os.Args[2:])
	case "network", "net":
		cmdNetwork(os.Args[2:])
	case "key":
		cmdKey(os.Args[2:])
	case "route":
		cmdRoute(os.Args[2:])
	case "seed":
		cmdSeed(os.Args[2:])
	case "nat":
		cmdNAT(os.Args[2:])
	case "host", "hosts":
		cmdHost(os.Args[2:])
	case "qos":
		cmdQoS(os.Args[2:])
	case "bandwidth", "bw":
		cmdBandwidth(os.Args[2:])
	case "fw":
		cmdFW(os.Args[2:])
	case "managed":
		cmdManaged(os.Args[2:])
	case "manager":
		cmdManager(os.Args[2:])
	case "service":
		cmdService(os.Args[2:])
	case "upgrade":
		cmdUpgrade(os.Args[2:])
	case "selftest":
		// Not advertised in the usage banner: this exists for the upgrade
		// preflight to run against a *candidate* binary before it replaces
		// anything (see internal/upgrade/apply.go's SelfTest). It is harmless to
		// run by hand, and occasionally useful for exactly the same reason.
		cmdSelfTest(os.Args[2:])
	case "version", "-v", "--version":
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
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `[gravinet] — full-mesh encrypted overlay VPN

commands:
  run        run the daemon
  genkey     generate one or more base64 AES-256 keys
  genpass    generate a web-admin user credential (PBKDF2) for the config
  network    manage networks:    add|delete|enable|disable|rename|subnet|join|list
  key        manage join keys:   list|show|generate|set|enable|disable|delete|distribute
  route      manage routes:      add|delete|advertise|reject|list
  seed       manage seed addrs:  list|add|remove ADDR [-net NAME]
  nat        manage NAT:         add|delete|enable|disable|state|list
  host       advertise hosts:    list|add NAME IP|remove NAME [-net NAME]
  qos        manage QoS:         add|delete|mark|unmark|enable|disable|list
  bandwidth  manage throttles:   up|down|both RATE [interface IF]|list
  fw         manage firewall rules (live, via the daemon)
  ban        ban a node:         gravinet ban <node> [-net NAME|id] [-notes N]
  unban      unban a node:       gravinet unban <node> [-force]
  managed    get/set managed mode (be managed): gravinet managed [on|off]
  manager    get/set manager mode (manage others): gravinet manager [on|off]
  list       print the whole config
  status     live peers/bans/routes (via the daemon)
  service    install/uninstall/print the OS service definition
  version    print version

config commands edit the config file and ask a running daemon to reload;
run "gravinet <command> -h" for command flags
`)
}

func cmdService(args []string) {
	// Accept the action as a leading positional, then parse flags after it.
	action := "print"
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		action = rest[0]
		rest = rest[1:]
	}
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config path for the service to use")
	user := fs.String("user", "", "run-as user (linux/macos)")
	osFlag := fs.String("os", runtime.GOOS, "target OS for 'print': linux|darwin|windows|freebsd|openbsd")
	execPath := fs.String("exec", "", "override the binary path in the definition")
	_ = fs.Parse(rest)

	opts := service.Defaults()
	if *cfgPath != "" {
		opts.ConfigPath = *cfgPath
	}
	if *execPath != "" {
		opts.ExecPath = *execPath
	}
	opts.User = *user

	switch action {
	case "print":
		switch *osFlag {
		case "linux":
			fmt.Print(service.SystemdUnit(opts))
		case "darwin":
			fmt.Print(service.LaunchdPlist(opts))
		case "windows":
			fmt.Print(service.WindowsInstallCommands(opts))
		case "freebsd":
			fmt.Print(service.RcdScript(opts))
		case "openbsd":
			fmt.Print(service.OpenBSDRcScript(opts))
		default:
			fmt.Print(service.Definition(opts))
		}
	case "install":
		path, next, err := service.Install(opts)
		if err != nil {
			fatal("service install: %v", err)
		}
		if path != "" {
			fmt.Printf("wrote %s\n", path)
		}
		fmt.Printf("next: %s\n", next)
	case "uninstall":
		next, err := service.Uninstall(opts)
		if err != nil {
			fatal("service uninstall: %v", err)
		}
		fmt.Printf("next: %s\n", next)
	default:
		fatal("service: unknown action %q (print|install|uninstall)", action)
	}
}

func cmdGenPass(args []string) {
	fs := flag.NewFlagSet("genpass", flag.ExitOnError)
	user := fs.String("user", "admin", "admin username")
	iter := fs.Int("iter", 100000, "PBKDF2 iterations")
	pass := fs.String("pass", "", "password (omit to read from stdin)")
	_ = fs.Parse(args)

	pw := *pass
	if pw == "" {
		fmt.Fprint(os.Stderr, "password: ")
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		pw = strings.TrimRight(line, "\r\n")
	}
	if pw == "" {
		fatal("genpass: empty password")
	}
	cred, err := webadmin.GenerateCredential(*user, pw, *iter)
	if err != nil {
		fatal("genpass: %v", err)
	}
	b, _ := json.MarshalIndent(cred, "", "  ")
	fmt.Println("# add this object to web_admin.users in your config:")
	fmt.Println(string(b))
}

func cmdGenKey(args []string) {
	fs := flag.NewFlagSet("genkey", flag.ExitOnError)
	n := fs.Int("n", 1, "number of keys to generate")
	_ = fs.Parse(args)
	for i := 0; i < *n; i++ {
		k, err := crypto.GenerateKey()
		if err != nil {
			fatal("genkey: %v", err)
		}
		fmt.Println(k)
	}
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config file")
	initCfg := fs.Bool("init", false, "write a default config to -config if missing, then exit")
	_ = fs.Parse(args)

	if *initCfg {
		writeInitialConfig(*cfgPath)
		return
	}

	// Wrapped in a closure so it can run *after* the SCM handshake below
	// (service.RunService / StartServiceCtrlDispatcherW) rather than before
	// it. This block does config load, TUN/adapter creation, DNS lookups for
	// seeds, and engine/transport/TLS setup — on a freshly booted Windows
	// host, with the network stack, DNS resolver, and Wintun driver binding
	// all still settling and competing with everything else starting at
	// once, this can take a lot longer than it does on an idle system (e.g.
	// running install-windows.ps1's own Start-Service right after install).
	// The SCM gives a new service process a limited window (~30s by default)
	// to call StartServiceCtrlDispatcherW and report a status; blow through
	// that window and the SCM kills the process outright — tearing down any
	// TUN adapters it had already created — before it even reaches the code
	// that would've told the SCM "I'm up". Doing the SCM handshake first and
	// this setup after means the ack goes out immediately regardless of how
	// long setup takes, so a slow boot delays coming up instead of getting
	// killed mid-startup.
	runBody := func(stop <-chan struct{}, ready func()) {
		// Let the service start on a fresh host: if the config is missing, write a
		// default (new node id, no networks yet) and carry on rather than failing.
		if _, err := os.Stat(*cfgPath); os.IsNotExist(err) {
			logx.Infof("config %s not found; writing defaults", *cfgPath)
			if err := writeDefaultConfig(*cfgPath); err != nil {
				fatal("create default config: %v", err)
			}
		}

		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fatal("config: %v", err)
		}

		logx.SetLevel(logx.ParseLevel(cfg.LogLevel))

		// Mirror all log output to a file (in addition to the console) so it can be
		// reviewed later and surfaced in the web admin's Logs view. Best-effort: if
		// the file can't be opened we keep logging to the console and carry on.
		logPath := cfg.LogFilePath(*cfgPath)
		var logClear func() error
		var logResize func(int64) // set the file's size cap live (nil if file logging is off)
		if logPath != "" {
			// FIFO mode (single rolling file, oldest lines dropped when full) is
			// what the web admin's Log Size setting drives; a config that predates
			// that setting and configured LogMaxMB/LogKeep still gets the classic
			// numbered-backup rotation. LogFIFO() decides which.
			var lf *logx.RotatingFile
			var lerr error
			if cfg.LogFIFO() {
				lf, lerr = logx.NewFIFOFile(logPath, cfg.LogMaxBytes())
			} else {
				lf, lerr = logx.NewRotatingFile(logPath, cfg.LogMaxBytes(), cfg.LogBackups())
			}
			if lerr != nil {
				logx.Warnf("could not open log file %s: %v (logging to console only)", logPath, lerr)
				logPath = ""
			} else {
				logx.SetOutput(io.MultiWriter(logx.BestEffort(os.Stderr), lf))
				logClear = lf.Truncate
				logResize = lf.SetMaxBytes
				if cfg.LogFIFO() {
					logx.Infof("logging to %s (FIFO, cap %s)", logPath, config.FormatSize(cfg.LogMaxBytes()))
				} else {
					logx.Infof("logging to %s (rotate at %d MiB, keep %d)", logPath, cfg.LogMaxBytes()>>20, cfg.LogBackups())
				}
			}
		}
		store := config.NewStore(cfg)

		workers := cfg.WorkerThreads
		if workers <= 0 {
			workers = runtime.NumCPU() - 1
			if workers < 1 {
				workers = 1
			}
		}

		logx.Infof("gravinet %s starting", version)
		logx.Infof("config: %s", store.Get().Path())

		// Upgrade guard, before anything that can fail. If this process is a
		// binary that was just swapped in, this is where it gets counted — and
		// where, if it has now failed to prove itself several boots running, the
		// previous binary is restored and this one is taken back out of service.
		// It has to run here, ahead of interfaces, sockets and the engine,
		// precisely because those are the things a bad binary dies on: a guard
		// that only runs on a *successful* startup path cannot rescue a node from
		// a binary that never gets that far.
		if cfg.UpgradeEnabled() {
			if st, err := upgrade.NewStore(cfg.UpgradeStoreDir(), cfg.Upgrade.TrustedKeys); err == nil {
				g := upgrade.NewGuard(st.Dir(), restartService, logx.Infof)
				if act, gs := g.OnBoot(); act == upgrade.BootReverted {
					logx.Errorf("upgrade: reverted %s -> %s (%s); restarting into the restored binary",
						gs.To, gs.From, gs.LastError)
					// The restart is already in flight (the guard asked the service
					// manager for it). Stop here rather than continuing to bring up a
					// mesh with a binary we have just decided is broken.
					return
				}
			} else {
				logx.Warnf("upgrade: %v", err)
			}
		}

		// Search domains (Naming > DNS) have no interface-scoped mechanism on
		// macOS/FreeBSD/OpenBSD (see internal/resolver's package doc for why) —
		// warn once at startup rather than only ever surfacing this as a
		// recurring per-sync-cycle debug line nobody's watching for. Search
		// domains are derived from each network's advertised (enabled) domains,
		// so this checks the same source spec.SearchDomains is built from below.
		if runtime.GOOS == "darwin" || runtime.GOOS == "freebsd" || runtime.GOOS == "openbsd" {
			var withSearch []string
			for _, n := range cfg.Networks {
				hasEnabled := false
				for _, d := range n.DNSAdvertise {
					if !d.Disabled {
						hasEnabled = true
						break
					}
				}
				if hasEnabled {
					withSearch = append(withSearch, n.Name)
				}
			}
			if len(withSearch) > 0 {
				logx.Warnf("search domains (Naming > DNS > Advertise) are configured on %s but not supported on %s — "+
					"no interface-scoped mechanism exists here; conditional DNS forwarding (routing domains) is unaffected",
					strings.Join(withSearch, ", "), runtime.GOOS)
			}
		}

		// A network that advertises DNS forwards but has DNSSync disabled will
		// never apply any of them to this node's own OS resolver — not the
		// ones it advertises, and not anything learned from a peer either,
		// since syncDNS returns immediately when spec.DNSSync is false.
		// That's a valid choice (an advertise-only gateway that doesn't want
		// local resolution changes), but it's also exactly what an all-zero,
		// never-configured DNSSync block looks like, so it's worth a startup
		// line rather than leaving an operator to work it out from an empty
		// `resolvectl` link.
		for _, n := range cfg.Networks {
			if n.DNSSync.Enabled {
				continue
			}
			hasAdvertise := false
			for _, d := range n.DNSAdvertise {
				if !d.Disabled {
					hasAdvertise = true
					break
				}
			}
			if hasAdvertise {
				logx.Warnf("network %s advertises DNS forward(s) but dns_sync.enabled is false — "+
					"this node will not register any conditional-forward or search domain with its own OS resolver "+
					"(other peers still receive and can apply the advertisement normally)", n.Name)
			}
		}
		logx.Infof("worker threads: %d (cores=%d)", workers, runtime.NumCPU())

		hostname := cfg.Hostname
		if hostname == "" {
			osName, _ := os.Hostname()
			hostname = shortHostname(osName)
		}

		// Clear any gravinet-managed hosts entries left over from a previous run
		// before we resolve seeds. Those map peer hostnames to overlay (tunnel) IPs;
		// if a seed is named after such a peer it would otherwise resolve to an
		// unreachable tunnel address and the node could never bootstrap. They're
		// repopulated as peers reconnect.
		clearStaleHostsBlocks(cfg)
		clearStaleDNSForwards(cfg)

		specs, devices := buildNetSpecs(cfg)
		if len(specs) == 0 {
			logx.Warnf("no overlay networks are up")
		}

		engine := mesh.NewEngine(mesh.Options{
			NodeID:            cfg.NodeID,
			Hostname:          hostname,
			Nets:              specs,
			Log:               logx.Default(),
			Managed:           cfg.Managed,
			Manager:           cfg.Manager,
			WebPort:           webPortOf(cfg),
			RouteAdvInterval:  cfg.RouteAdvDuration(),
			KeepaliveInterval: cfg.KeepaliveDuration(),
			PeerTimeout:       cfg.PeerTimeoutDuration(),
			UnderlayMTU:       cfg.UnderlayMTUValue(),
			UnderlayMTUMax:    cfg.UnderlayMTUMaxValue(),
			TCPFallbackPort:   engineFallbackPort(cfg),
			PrimaryPort:       cfg.PrimaryPort,
			ExtraTCPPorts:     toUint16Ports(cfg.ExtraTCPListenPorts),
			ExtraUDPPorts:     toUint16Ports(cfg.ExtraListenPorts),
			TunWorkers:        workers,
			FirewallObjects:   toMeshFirewallObjects(cfg.FirewallObjects),
			FirewallServices:  toMeshFirewallServices(cfg.FirewallServices),
		})
		// restartRequested is closed (at most once) when the engine detects a
		// sleep/resume cycle it can't fully recover from in-process — see
		// mesh.Engine.SetSuspendResumeHook's doc. Distinct from the OS-signal
		// stop channel below: a signal means "an operator or service manager
		// wants this process to actually stop," which must never trigger a
		// restart, while this means "keep running, just cleanly from scratch."
		restartRequested := make(chan struct{})
		var requestRestartOnce sync.Once
		engine.SetSuspendResumeHook(func() { requestRestartOnce.Do(func() { close(restartRequested) }) })
		// The same clean-restart path, triggered by a Wi-Fi/cellular roam
		// (checkUnderlayChange) rather than a sleep/resume. Blunt on purpose: a
		// roam can leave peers pinned to endpoints on the network we just left and
		// routes pointing at the old gateway, which a from-scratch restart rebuilds
		// where in-process patch-up doesn't. Grace-gated and one-shot inside the
		// engine so a flapping link can't spin the service. Opt out with
		// restart_on_underlay_change=false.
		if cfg.RestartOnUnderlayChangeEnabled() {
			engine.SetUnderlayChangeHook(func() {
				logx.Infof("underlay network changed (roam); restarting to re-establish connectivity to all peers")
				requestRestartOnce.Do(func() { close(restartRequested) })
			})
		}
		if mx := cfg.UnderlayMTUMaxValue(); mx > cfg.UnderlayMTUValue() {
			logx.Infof("underlay floor %d bytes; probing each path for up to %d (larger overlay packets are fragmented to fit)", cfg.UnderlayMTUValue(), mx)
		} else {
			logx.Infof("underlay datagram cap %d bytes; larger overlay packets are fragmented for the underlay", cfg.UnderlayMTUValue())
		}

		// portChangeGrace is how long the old underlay socket keeps serving inbound
		// after a live port change, giving peers time to migrate to the new port.
		const portChangeGrace = 2 * time.Minute
		pktHandler := func(payload []byte, from netip.AddrPort, fam transport.Family) {
			engine.OnPacket(payload, from, fam)
		}
		// tr is the UDP underlay. PrimaryPort == 0 means the operator turned UDP
		// off entirely (the "-" sentinel in the web admin's UDP port field —
		// Config.Validate refuses this unless the TCP/TLS fallback below is
		// enabled), in which case tr stays nil, mirroring how tlsTr just below
		// stays nil when the fallback is off or fails to bind. Dual.Send already
		// treats a nil UDP the same way it treats a nil TLS.
		var tr *transport.Transport
		if cfg.PrimaryPort > 0 {
			var err error
			tr, err = transport.Open(transport.Options{
				PrimaryPort:   cfg.PrimaryPort,
				FallbackPorts: config.FallbackUDPPorts,
				ExtraPorts:    cfg.ExtraListenPorts,
				EnableV4:      cfg.EnableIPv4,
				EnableV6:      cfg.EnableIPv6,
				Workers:       workers,
				Log:           logx.Default(),
				Handler:       pktHandler,
			})
			if err != nil {
				for _, d := range devices {
					d.Close()
				}
				fatal("transport: %v", err)
			}
		} else {
			logx.Infof("udp underlay disabled (primary_port=0); relying on the tcp/tls fallback")
		}

		// TCP/TLS fallback: every node also listens on the fallback port (default
		// 443), TLS-wrapped so it reads as HTTPS, so peers on networks that block UDP
		// can still reach the mesh. Inbound TLS frames go through the same handler
		// (same handshake auth and rate limiting as UDP). Every configured port here
		// (the primary fallback port and each of extra_tcp_listen_ports) is bound
		// best-effort by OpenTLS: one already taken by something else is logged and
		// skipped there, not fatal — this only reaches the UDP-only fallback below if
		// *none* of them could bind.
		var tlsTr *transport.TLSTransport
		if cfg.TCPFallbackEnabled() {
			fp := cfg.TCPFallbackPortValue()
			if tt, terr := transport.OpenTLS(transport.TLSOptions{
				Port:       fp,
				ExtraPorts: cfg.ExtraTCPListenPorts,
				Handler:    pktHandler,
				Log:        logx.Default(),
			}); terr != nil {
				logx.Warnf("tcp/%d fallback unavailable (%v) — continuing UDP-only", fp, terr)
			} else {
				tlsTr = tt
				if !tlsTr.HasPrimary() {
					logx.Warnf("tcp/%d fallback port already in use — skipped; still listening on configured extra tcp port(s)", fp)
				}
			}
		}
		engine.Attach(transport.Dual{UDP: tr, TLS: tlsTr})

		// upnpMgr, when non-nil, owns the background UPnP IGD mapping for
		// every port this node actually listens on — see internal/upnp and
		// config.Config.EnableUPnP's doc comment for the full picture. Only
		// ever started once, here at startup — not re-evaluated by reloadFn
		// on a later config change or a live port change (see webadmin's
		// handleUPnPSetting) — and best-effort torn down in shutdown()
		// below.
		//
		// The two primary ports are mapped using the port that actually
		// bound (tr.Port()/tlsTr.Port()), not just what was configured: the
		// UDP primary in particular can silently fall back to a different
		// port than cfg.PrimaryPort (see transport.Open's doc comment and
		// config.FallbackUDPPorts) — mapping the configured-but-unbound
		// port would forward WAN traffic to a port nothing is listening on.
		// Extra listen ports don't have an equivalent "what actually bound"
		// accessor, so those are mapped straight from config the same
		// best-effort way their own local bind already is (see
		// Config.ExtraListenPorts/ExtraTCPListenPorts' doc comments): one
		// that never actually bound locally just wastes a WAN mapping
		// nothing answers, not a hazard.
		var upnpMgr *upnp.Manager
		if cfg.EnableUPnP {
			var mappings []upnp.PortMapping
			if tr != nil {
				mappings = append(mappings, upnp.PortMapping{Port: tr.Port(), Protocol: "UDP"})
			}
			if tlsTr != nil && tlsTr.HasPrimary() {
				mappings = append(mappings, upnp.PortMapping{Port: tlsTr.Port(), Protocol: "TCP"})
			}
			for _, p := range cfg.ExtraListenPorts {
				mappings = append(mappings, upnp.PortMapping{Port: p, Protocol: "UDP"})
			}
			for _, p := range cfg.ExtraTCPListenPorts {
				mappings = append(mappings, upnp.PortMapping{Port: p, Protocol: "TCP"})
			}
			// NewManager drops non-positive/duplicate entries and no-ops if
			// the resulting set is empty (e.g. UDP off and no TCP fallback
			// bound — Config.Validate wouldn't allow that combination, but
			// this stays inert either way rather than assuming it can't
			// happen), so it's always safe to construct unconditionally.
			upnpMgr = upnp.NewManager(mappings, "gravinet")
			upnpMgr.Start()
		}

		// curTr tracks the live underlay transport. The primary UDP port can be
		// changed at runtime (Settings): we open a fresh socket on the new port,
		// swap it in for outbound, and grace-close the old one — see reloadFn.
		// curTLS tracks the live TCP/TLS fallback transport, which the TCP port
		// setting rebinds the same way. reattach rebuilds the Dual sender from
		// whichever pair is current and hands it to the engine. reqExtraUDP/
		// reqExtraTCP track the extra listen ports actually applied, so reloadFn
		// can detect a change to *those* independently of the primary/fallback
		// port — extra_listen_ports/extra_tcp_listen_ports changing alone used to
		// go unnoticed until the next unrelated port change or a full restart.
		var trMu sync.Mutex
		reqPort := cfg.PrimaryPort
		curTr := tr
		curTLS := tlsTr
		reqTCPPort := 0
		if tlsTr != nil {
			reqTCPPort = tlsTr.Port()
		}
		reqExtraUDP := cfg.ExtraListenPorts
		reqExtraTCP := cfg.ExtraTCPListenPorts
		getTr := func() *transport.Transport {
			trMu.Lock()
			defer trMu.Unlock()
			return curTr
		}
		getTLS := func() *transport.TLSTransport {
			trMu.Lock()
			defer trMu.Unlock()
			return curTLS
		}
		reattach := func() {
			trMu.Lock()
			d := transport.Dual{UDP: curTr, TLS: curTLS}
			trMu.Unlock()
			engine.Attach(d)
		}

		// Tracked for stale peer-cache pruning (see the persist hook below):
		// startedAt anchors how long a cached candidate has had to prove itself
		// reachable, and everConnected accumulates, per network, every endpoint
		// string actually seen live at any point since this process started —
		// not just the current instant — so a peer that's briefly down when a
		// persist happens to fire isn't punished for bad timing.
		startedAt := time.Now()
		everConnected := map[uint64]map[string]bool{}

		// Persist hook: write live/learned state back to config so it survives a
		// restart — firewall edits (web UI / control socket), the name and subnet a
		// node learns when it joins by id, and the overlay address it self-assigns
		// via DAD (so a node keeps the same mesh IP across restarts). Installed
		// before Start so the first (lone-node) assignment is captured.
		//
		// Locked via config.Lock rather than a hook-local mutex: this hook and the
		// web admin's own editor (mutateConfig) are two independent writers of the
		// same file, on entirely separate triggers (this one fires from
		// notifyChange, on the engine's own schedule). A hook-local mutex only
		// serializes this hook against itself — it does nothing to stop this hook
		// from loading its own copy, a web admin edit saving in between, and this
		// hook then saving its (now stale) copy over it, silently reverting the
		// edit. config.Lock is the same process-wide, per-path lock mutateConfig
		// uses, so the two can never interleave that way.
		cfgLock := config.Lock(*cfgPath)
		engine.SetPersistHook(func(networkID uint64) {
			cfgLock.Lock()
			defer cfgLock.Unlock()
			cur, err := config.Load(*cfgPath)
			if err != nil {
				logx.Warnf("persist: load %s: %v", *cfgPath, err)
				return
			}
			// Persist the node-global address-object and service catalogs the
			// rules of every network resolve their src/dst/services references
			// against, so a named object/service survives a restart. Node-global
			// (see config.Config.FirewallObjects' doc comment), so this happens
			// once per persist call, unconditionally — not scoped to whichever
			// networkID triggered this call, unlike the per-network work below.
			if objs, oerr := engine.FirewallObjectsList(); oerr == nil {
				co := make([]config.FirewallObject, 0, len(objs))
				for _, o := range objs {
					co = append(co, config.FirewallObject{
						Name: o.Name, Kind: o.Kind, Addresses: o.Addresses, Members: o.Members, Notes: o.Notes,
					})
				}
				cur.FirewallObjects = co
			}
			if svcs, serr := engine.FirewallServicesList(); serr == nil {
				cs := make([]config.FirewallService, 0, len(svcs))
				for _, s := range svcs {
					cp := make([]config.FirewallServicePort, 0, len(s.Ports))
					for _, p := range s.Ports {
						cp = append(cp, config.FirewallServicePort{Proto: p.Proto, PortMin: p.PortMin, PortMax: p.PortMax})
					}
					cs = append(cs, config.FirewallService{Name: s.Name, Ports: cp, Notes: s.Notes})
				}
				cur.FirewallServices = cs
			}
			// Persist the engine's current rulebase. The engine holds the rules
			// whether or not the firewall is enabled (enabled only gates enforcement),
			// so this is safe to write in either state and won't wipe rules while the
			// firewall is off.
			rules, fwErr := engine.FirewallRules(networkID)
			idHex := fmt.Sprintf("%016x", networkID)
			for i := range cur.Networks {
				nid, _ := strconv.ParseUint(cur.Networks[i].ID, 16, 64)
				if nid != networkID && cur.Networks[i].ID != idHex {
					continue
				}
				if fwErr == nil {
					out := make([]config.FirewallRule, 0, len(rules))
					for _, r := range rules {
						out = append(out, config.FirewallRule{
							Disabled: r.Disabled, Action: r.Action, Direction: r.Direction, Proto: r.Proto,
							Src: r.Src, Dst: r.Dst, SrcNegate: r.SrcNegate, DstNegate: r.DstNegate,
							SrcPortMin: r.SrcPortMin, SrcPortMax: r.SrcPortMax,
							DstPortMin: r.DstPortMin, DstPortMax: r.DstPortMax,
							Services: r.Services, ServicesNegate: r.ServicesNegate, Log: r.Log,
							Notes: r.Notes,
						})
					}
					cur.Networks[i].Firewall.Rules = out
				}
				// Name and subnet a node learned from the network when joining by id.
				if name, s4, s6, ok := engine.NetworkIdentity(networkID); ok {
					if cur.Networks[i].Name == "" && name != "" {
						cur.Networks[i].Name = name
					}
					if cur.Networks[i].Subnet4 == "" && s4.IsValid() {
						cur.Networks[i].Subnet4 = s4.String()
					}
					if cur.Networks[i].Subnet6 == "" && s6.IsValid() {
						cur.Networks[i].Subnet6 = s6.String()
					}
				}
				// Pin the self-assigned overlay address so the node keeps the same
				// mesh IP across restarts (only when not already pinned in config).
				if a4, a6, ok := engine.NetworkSelfAddrs(networkID); ok {
					if cur.Networks[i].Address4 == "" && a4.IsValid() {
						if b := prefixBits(cur.Networks[i].Subnet4); b >= 0 {
							cur.Networks[i].Address4 = netip.PrefixFrom(a4, b).String()
						}
					}
					if cur.Networks[i].Address6 == "" && a6.IsValid() {
						if b := prefixBits(cur.Networks[i].Subnet6); b >= 0 {
							cur.Networks[i].Address6 = netip.PrefixFrom(a6, b).String()
						}
					}
				}
				// Fold mesh-distributed keys into config, and apply any retractions
				// — see foldPropagatedKeys/applyKeyRetractions below for the actual
				// (independently testable) logic.
				if updated, unplaced, changed := foldPropagatedKeys(cur.Networks[i].Keys, engine.PropagatedKeys(networkID)); changed || unplaced > 0 {
					cur.Networks[i].Keys = updated
					if unplaced > 0 {
						logx.Warnf("persist: no free key slot on net %016x for a distributed key; not persisted (retire an old key first)", networkID)
					}
				}
				if updated, refused, changed := applyKeyRetractions(cur.Networks[i].Keys, engine.RetractedKeys(networkID)); changed || len(refused) > 0 {
					cur.Networks[i].Keys = updated
					for _, id := range refused {
						logx.Warnf("persist: net %016x: could not retract key %x (it's the network's only enabled key)", networkID, id)
					}
				}

				// Bootstrap peer cache: union currently-connected peer endpoints (fresh
				// first) with what's already cached, deduped and capped, so a restart
				// has many seed candidates and isn't reliant on the one configured seed.
				//
				// Entries that were never actually reachable get dropped here rather
				// than carried forward forever: an address that's wrong, decommissioned,
				// or otherwise not a real gravinet peer would otherwise sit in the
				// config permanently, since nothing else in the bootstrap path ever
				// removes it (a config-provided-style entry stays outside the
				// node-identity-based stale-seed pruning the runtime does for
				// gossip-learned addresses — see install() in internal/mesh — precisely
				// because it's *supposed* to be a durable, operator-independent
				// fallback; that durability is exactly what makes an actually-dead
				// entry able to persist unnoticed). A real but temporarily-down peer is
				// protected two ways: everConnected remembers every endpoint ever seen
				// live this run (not just this instant), and peerCacheStaleGrace gives
				// a freshly-added candidate real time to prove itself before judging it,
				// so routine downtime (a reboot, a brief outage) isn't mistaken for permanent staleness.
				if ec := everConnected[networkID]; ec == nil {
					everConnected[networkID] = map[string]bool{}
				}
				fresh := make([]string, 0, 4)
				for _, ep := range engine.PeerEndpoints(networkID) {
					s := ep.String()
					fresh = append(fresh, s)
					everConnected[networkID][s] = true
				}
				merged, pruned := mergePeerCache(fresh, cur.Networks[i].PeerCache, everConnected[networkID],
					time.Since(startedAt), peerCacheStaleGrace, peerCacheMax)
				if pruned > 0 {
					logx.Infof("persist: dropped %d peer_cache entr%s on net %016x that never connected within %s of this node's uptime",
						pruned, map[bool]string{true: "y", false: "ies"}[pruned == 1], networkID, peerCacheStaleGrace)
				}
				cur.Networks[i].PeerCache = merged
				if err := cur.SaveTo(*cfgPath); err != nil {
					logx.Warnf("persist: save %s: %v", *cfgPath, err)
				}
				return
			}
		})

		engine.Start()
		logx.Infof("mesh engine running with %d network(s)", len(specs))

		// Enable host IP forwarding so this node can route between the overlay and
		// its other interfaces — the on-ramp that makes redistributed routes and NAT
		// actually carry traffic. Default on; opt out with "ip_forwarding": false.
		// The prior values are restored on clean shutdown.
		var fwdState ipfwd.State
		if cfg.ForwardingEnabled() {
			fwdState = ipfwd.Enable(true, true)
			switch {
			case fwdState.V4Missing():
				logx.Infof("IPv4 forwarding: knob absent on host; skipped")
			case fwdState.V4Failed:
				logx.Warnf("IPv4 forwarding: could not enable (need root/CAP_NET_ADMIN?) — routing between interfaces may not work")
			default:
				logx.Infof("IPv4 forwarding enabled")
			}
			switch {
			case fwdState.V6Missing():
				logx.Infof("IPv6 forwarding: IPv6 disabled on host; skipped")
			case fwdState.V6Failed:
				logx.Warnf("IPv6 forwarding: could not enable")
			default:
				logx.Infof("IPv6 forwarding enabled")
			}
		}

		// Kernel NAT: masquerade/SNAT/DNAT for forwarded gateway traffic must be done
		// by the host's conntrack-backed netfilter — the userspace overlay NAT can't
		// reverse-translate replies the kernel delivers to our own interface. Program
		// a gravinet-owned ruleset (nft, or iptables fallback); re-applied on reload
		// and cleared on shutdown. Lazily created so hosts without any NAT rules (or
		// without nft/iptables) pay nothing.
		var nfMgr *netfilter.Manager
		// lastNATRules is the ruleset most recently *applied* (not merely
		// requested) via nfMgr.Apply, so both the startup apply and the
		// async worker below can skip re-applying when nothing NAT-related
		// actually changed. reloadFn triggers a NAT apply at the end of
		// *every* reload — i.e. on every web-admin edit, not just NAT ones
		// (renaming a network, toggling a firewall exemption, editing a DNS
		// entry, ...) — because reloadFn has no cheaper way to know in
		// advance whether this particular edit touched NAT. Rule is
		// comparable, so slices.Equal is a deep comparison, not a
		// reference/length check.
		//
		// natMu guards nfMgr + lastNATRules against the concurrent access
		// natApply's async worker (below) introduces: the worker's own
		// goroutine, applyKernelNAT racing ahead to check "did this change"
		// while the worker is still applying a previous one, and shutdown's
		// nfMgr.Clear() read of nfMgr. It's held only around the "is this a
		// no-op" check and the nfMgr read/write themselves — never across
		// the actual nfMgr.Apply/.Clear() call, which is the slow part.
		var natMu sync.Mutex
		var lastNATRules []netfilter.Rule
		// natApplyNow does the actual create-if-needed + skip-if-unchanged +
		// Apply, synchronously. Used directly for the one-time startup apply
		// below (nothing is being served yet, so nothing is waiting on it —
		// same as before this change) and by natApply's worker goroutine for
		// every reload-triggered apply.
		natApplyNow := func(rules []netfilter.Rule) {
			natMu.Lock()
			if nfMgr != nil && slices.Equal(rules, lastNATRules) {
				natMu.Unlock()
				return
			}
			if nfMgr == nil {
				if len(rules) == 0 {
					natMu.Unlock()
					return
				}
				m, err := netfilter.New()
				if err != nil {
					natMu.Unlock()
					logx.Warnf("kernel NAT: %v — masquerade/port-forward rules will not take effect", err)
					return
				}
				nfMgr = m
			}
			mgr := nfMgr
			natMu.Unlock()
			if err := mgr.Apply(rules); err != nil {
				logx.Warnf("kernel NAT: could not apply rules via %s (need root/CAP_NET_ADMIN?): %v", mgr.Backend(), err)
				return // leave lastNATRules as-is so a retry is attempted next reload
			}
			natMu.Lock()
			lastNATRules = rules
			natMu.Unlock()
			if len(rules) > 0 {
				logx.Infof("kernel NAT: applied %d rule(s) via %s", len(rules), mgr.Backend())
			}
		}
		natApplyNow(kernelNATRules(cfg)) // startup: synchronous — nothing is being served yet, same as always

		// natApplyCh hands reload-triggered rulesets to a single background
		// worker instead of applying them inline in applyKernelNAT. On
		// Linux/macOS/BSD, nfMgr.Apply is a near-instant nft/iptables/pf(4)
		// call (see netfilter_linux.go, _darwin.go, _*bsd.go) — cheap enough
		// that applying it inline was never a problem there. On Windows it
		// shells out to PowerShell to reprogram WinNAT, which — per Apply's
		// own callers elsewhere (see toggleTagState's doc comment in
		// webadmin/ui.go) — can take on the order of 30s; since
		// applyKernelNAT runs from reloadFn, which mutateConfig calls
		// synchronously, which the HTTP handler for every edit endpoint
		// waits on before responding (s.editResult), applying inline meant
		// every web-admin save on a Windows node — not just NAT edits —
		// paid that 30s. Handing it to a worker instead is the same shape
		// already used for network teardown just below (RemoveNetwork run
		// in its own goroutine, for the identical reason): the edit that
		// triggered a real NAT change now gets the same instant response as
		// any other edit, and toggleTagState's optimistic tag-flip already
		// covers the fact that the change hasn't landed on the host quite
		// yet.
		//
		// Capacity 1, and a send that finds it full drains the pending
		// (now-stale) value first — so the worker always converges on the
		// most recently *desired* ruleset rather than working through a
		// backlog a rapid run of edits already made obsolete; applying an
		// intermediate state nothing wants anymore would waste exactly the
		// 30s this exists to avoid. Sends only ever come from applyKernelNAT,
		// itself only ever called from reloadFn, which only ever runs one at
		// a time under mutateConfig's cfgMu — so sends are already strictly
		// ordered and only the worker's own consumption can lag behind.
		// natApplyNow's own unchanged-check (shared with the startup call
		// above) makes the worker idempotent even if a stale duplicate ever
		// makes it through, so no ordering guarantee is load-bearing here.
		natApplyCh := make(chan []netfilter.Rule, 1)
		go func() {
			for rules := range natApplyCh {
				natApplyNow(rules)
			}
		}()
		// removing tracks network IDs currently being torn down by the
		// background goroutine reloadFn's removal loop launches (see there),
		// keyed by ID, each closed when that removal finishes. Declared here
		// rather than inside reloadFn so it persists across calls: the race
		// this closes is between a removal launched by one reloadFn call and
		// an add/reload for the same ID from a *later* call (e.g. a rapid
		// disable then re-enable of the same network) — a single call's own
		// local state wouldn't span that gap. removingMu guards the map
		// itself; each value is only ever written once (at launch) and read
		// (closed) once, so no separate lock is needed per entry.
		var removingMu sync.Mutex
		removing := map[uint64]chan struct{}{}
		applyKernelNAT := func(c *config.Config) {
			rules := kernelNATRules(c)
			select {
			case natApplyCh <- rules:
			default:
				// Worker is still busy with a previous apply — replace
				// whatever's pending rather than blocking here (that would
				// just reintroduce the very stall this exists to avoid) or
				// queueing behind it (see natApplyCh's comment on why the
				// latest desired state is what matters).
				select {
				case <-natApplyCh:
				default:
				}
				natApplyCh <- rules
			}
		}

		// reloadFn re-reads the config file and applies what can change live —
		// firewall rules, NAT, QoS, bandwidth throttles, and authentication keys
		// (including turning any of them on or off). The remaining structural changes
		// (networks, addressing, ports) take effect on restart. Shared by the control
		// socket and the web admin so
		// both apply config edits the same way.
		reloadFn := func() error {
			newCfg, err := config.Load(*cfgPath)
			if err != nil {
				return err
			}
			overlays := overlaysOf(newCfg)
			engine.SetManaged(newCfg.Managed)
			engine.SetManager(newCfg.Manager)
			engine.SetRouteAdvInterval(newCfg.RouteAdvDuration())
			engine.SetKeepaliveInterval(newCfg.KeepaliveDuration())
			engine.SetPeerTimeout(newCfg.PeerTimeoutDuration())
			// If a previous reload's async removal of any network is still in
			// flight, wait for it here before this call forms any opinion
			// about what's running — otherwise a rapid disable-then-re-enable
			// of the same network could see it as already gone (removed from
			// engine.NetworkIDs() early in RemoveNetwork, well before the
			// slower interface teardown that follows actually finishes) and
			// call AddNetwork for a fresh one while the old one's TUN device
			// is still being closed, racing two devices for the same
			// interface name/adapter. This is the only place that matters:
			// every later read of engine.NetworkIDs() in this function
			// happens after it.
			for _, n := range newCfg.Networks {
				id, err := strconv.ParseUint(n.ID, 16, 64)
				if err != nil {
					continue
				}
				removingMu.Lock()
				ch := removing[id]
				removingMu.Unlock()
				if ch != nil {
					<-ch
				}
			}
			running := map[uint64]bool{}
			for _, id := range engine.NetworkIDs() {
				running[id] = true
			}
			desired := map[uint64]bool{}
			for i, n := range newCfg.Networks {
				if !n.Enabled {
					continue
				}
				id, err := strconv.ParseUint(n.ID, 16, 64)
				if err != nil {
					continue
				}
				desired[id] = true
				if running[id] {
					// Already up: apply the hot-reloadable runtime settings + keys.
					var spec mesh.NetSpec
					spec.ID = id
					fillRuntimeSpec(&spec, n, newCfg.EffectiveFirewallExempt(), newCfg.NATStateTimeout, newCfg.FirewallServices)
					// fillRuntimeSpec doesn't resolve seeds (only buildOneNetSpec does
					// at startup); resolve them here so a seed added at runtime is
					// dialed live via ReloadRuntime's seed merge.
					boot := append(append([]string{}, n.Seeds.Addrs()...), n.PeerCache...)
					spec.Seeds = resolveSeeds(boot, newCfg.PrimaryPort, overlays)
					spec.TCPSeeds = resolveTCPSeeds(boot, newCfg.TCPFallbackPortValue(), overlays)
					if ks, kerr := crypto.NewKeySet(keyStrings(n)); kerr == nil {
						spec.Keys = ks
						spec.KeyLabels = keyLabelMap(n)
						spec.KeyExpires = keyExpiryMap(n)
					} else {
						logx.Warnf("reload: net %016x: keeping current keys (%v)", id, kerr)
					}
					if e := engine.ReloadRuntime(id, spec); e != nil {
						logx.Warnf("reload: net %016x: %v", id, e)
					}
					continue
				}
				// New or just-enabled network: build it and bring it up live.
				spec, dev, berr := buildOneNetSpec(n, newCfg, overlays, i)
				if berr != nil {
					logx.Warnf("reload: net %s: %v", n.ID, berr)
					continue
				}
				if e := engine.AddNetwork(spec); e != nil {
					logx.Warnf("reload: net %016x: add: %v", id, e)
					dev.Close()
				}
			}
			// Networks no longer present or now disabled: tear them down live. The
			// engine closes their TUN devices on removal.
			//
			// Run each teardown in its own goroutine rather than waiting for it
			// here: by this point the config is already saved and validated, so
			// the caller (mutateConfig, and beyond it the HTTP request the web
			// admin's toggle is waiting on) has everything it needs to report
			// success and let the UI move on. RemoveNetwork itself can take a
			// real, user-visible amount of time — waiting out an in-flight
			// tunLoop read, then removing every route + the address on that
			// interface, each a separate subprocess spawn on Windows — none of
			// which the person clicking "disable" should have to sit through.
			// engine.RemoveNetwork/NetworkIDs are already safe to call
			// concurrently with the rest of reload (and with each other): the
			// engine's own locking, not caller serialization, is what protects
			// the shared network map.
			//
			// Logged verbosely (attempt, duration, and an explicit post-check
			// against the live engine state) specifically to chase down a report
			// that a disabled network kept passing traffic on Windows despite
			// every layer of this looking correct on read-through: config save,
			// this reload, RemoveNetwork's session cleanup, and the handshake
			// handler's rejection of unknown networks. If that happens again,
			// these lines pin down exactly which of those layers is lying.
			for _, id := range engine.NetworkIDs() {
				if !desired[id] {
					done := make(chan struct{})
					removingMu.Lock()
					removing[id] = done
					removingMu.Unlock()
					go func(id uint64) {
						defer func() {
							removingMu.Lock()
							delete(removing, id)
							removingMu.Unlock()
							close(done)
						}()
						logx.Infof("reload: net %016x: disabled/removed from config, tearing down live", id)
						start := time.Now()
						err := engine.RemoveNetwork(id)
						elapsed := time.Since(start)
						if err != nil {
							logx.Warnf("reload: net %016x: remove failed after %s: %v", id, elapsed, err)
							return
						}
						stillUp := false
						for _, rid := range engine.NetworkIDs() {
							if rid == id {
								stillUp = true
							}
						}
						if stillUp {
							logx.Warnf("reload: net %016x: RemoveNetwork returned no error after %s, but the network is still in engine.NetworkIDs() — this should be impossible", id, elapsed)
						} else {
							logx.Infof("reload: net %016x: torn down in %s, confirmed gone from live engine state", id, elapsed)
						}
					}(id)
				}
			}
			// Underlay primary-port change: rebind live. Open a fresh socket on the
			// new port and swap it in for outbound; keep the old transport's receive
			// workers running for a grace window so peers still dialing the old port
			// keep getting served (and migrate when they see our replies from the new
			// port), then close it. Connected peers are poked immediately so they roam
			// to the new endpoint without waiting for the next keepalive.
			//
			// newCfg.PrimaryPort == 0 means UDP is being turned off entirely (the "-"
			// sentinel in the web admin's UDP port field) — handled the same way the
			// TCP/TLS fallback's on/off toggle is just below. Config.Validate refuses
			// to let both be off at once, so there's always at least one live
			// transport; Dual.Send already treats a nil UDP the same as a nil TLS.
			trMu.Lock()
			prevPort := reqPort
			prevExtraUDP := reqExtraUDP
			trMu.Unlock()
			// The extra-ports-only branch only matters while UDP stays enabled
			// (newCfg.PrimaryPort != 0) — if it's off before and after, there's no
			// listener to attach extra ports to either way, so a change to the list
			// is moot (same reasoning as the TCP fallback's extras check below).
			if newCfg.PrimaryPort != prevPort || (newCfg.PrimaryPort != 0 && !slices.Equal(newCfg.ExtraListenPorts, prevExtraUDP)) {
				if newCfg.PrimaryPort == 0 {
					trMu.Lock()
					prev := curTr
					curTr = nil
					reqPort = 0
					reqExtraUDP = nil
					trMu.Unlock()
					engine.SetExtraUDPPorts(nil)
					engine.SetPrimaryPort(0) // stop advertising host candidates nobody can dial over UDP
					reattach()               // Dual.UDP is now nil; sends fall through to TLS (or error, for callers with no live TLS conn yet — see Dual.Send)
					logx.Infof("udp underlay disabled")
					if prev != nil {
						go func(old *transport.Transport) {
							time.Sleep(portChangeGrace)
							old.Close()
						}(prev)
					}
				} else if nt, terr := transport.Open(transport.Options{
					PrimaryPort:   newCfg.PrimaryPort,
					FallbackPorts: config.FallbackUDPPorts,
					ExtraPorts:    newCfg.ExtraListenPorts,
					EnableV4:      newCfg.EnableIPv4,
					EnableV6:      newCfg.EnableIPv6,
					Workers:       workers,
					Log:           logx.Default(),
					Handler:       pktHandler,
				}); terr != nil {
					logx.Errorf("underlay port change %d -> %d failed: %v — keeping port %d", prevPort, newCfg.PrimaryPort, terr, prevPort)
				} else {
					trMu.Lock()
					prev := curTr
					curTr = nt
					reqPort = newCfg.PrimaryPort
					reqExtraUDP = newCfg.ExtraListenPorts
					trMu.Unlock()
					reattach()                                                      // outbound now originates from the new port (keeps current TLS)
					engine.PokePeers()                                              // immediate keepalive so peers roam to it
					engine.SetExtraUDPPorts(toUint16Ports(newCfg.ExtraListenPorts)) // advertise the new list live
					engine.SetPrimaryPort(newCfg.PrimaryPort)                       // host candidates carry the port; re-advertise them on the new one
					logx.Infof("underlay port changed %d -> %d (old port still serving inbound for %s, then closing)", prevPort, newCfg.PrimaryPort, portChangeGrace)
					if prev != nil {
						go func(old *transport.Transport) {
							time.Sleep(portChangeGrace)
							old.Close()
						}(prev)
					}
				}
			}
			// TCP/TLS fallback port change: rebind the fallback listener the same way,
			// and update the port the engine dials peers on so the homogeneous-mesh
			// assumption (everyone on the same fallback port) stays consistent.
			trMu.Lock()
			prevTCP := reqTCPPort
			prevExtraTCP := reqExtraTCP
			trMu.Unlock()
			wantTCP := 0
			if newCfg.TCPFallbackEnabled() {
				wantTCP = newCfg.TCPFallbackPortValue()
			}
			// The extra-ports-only branch only matters while fallback stays enabled
			// (wantTCP != 0) — if it's off before and after, there's no listener to
			// attach extra ports to either way, so a change to the list is moot.
			if wantTCP != prevTCP || (wantTCP != 0 && !slices.Equal(newCfg.ExtraTCPListenPorts, prevExtraTCP)) {
				if wantTCP == 0 {
					trMu.Lock()
					prev := curTLS
					curTLS = nil
					reqTCPPort = 0
					reqExtraTCP = nil
					trMu.Unlock()
					engine.SetFallbackPort(0)
					engine.SetExtraTCPPorts(nil)
					reattach()
					logx.Infof("tcp fallback disabled")
					if prev != nil {
						go func(old *transport.TLSTransport) { time.Sleep(portChangeGrace); old.Close() }(prev)
					}
				} else if ntls, terr := transport.OpenTLS(transport.TLSOptions{Port: wantTCP, ExtraPorts: newCfg.ExtraTCPListenPorts, Handler: pktHandler, Log: logx.Default()}); terr != nil {
					logx.Errorf("tcp fallback port change %d -> %d failed: %v — keeping %d", prevTCP, wantTCP, terr, prevTCP)
				} else {
					trMu.Lock()
					prev := curTLS
					curTLS = ntls
					reqTCPPort = wantTCP
					reqExtraTCP = newCfg.ExtraTCPListenPorts
					trMu.Unlock()
					engine.SetFallbackPort(wantTCP)
					engine.SetExtraTCPPorts(toUint16Ports(newCfg.ExtraTCPListenPorts))
					reattach()
					if !ntls.HasPrimary() {
						logx.Warnf("tcp/%d fallback port already in use — skipped; still listening on configured extra tcp port(s)", wantTCP)
					}
					logx.Infof("tcp fallback port changed %d -> %d (old port serving inbound for %s, then closing)", prevTCP, wantTCP, portChangeGrace)
					if prev != nil {
						go func(old *transport.TLSTransport) { time.Sleep(portChangeGrace); old.Close() }(prev)
					}
				}
			}
			// Log level, live. This was previously read once at startup and never
			// again, which made it useless precisely when it's needed: raising the
			// level to debug to investigate a fault meant restarting the daemon,
			// and a restart resets every session, backoff and learned endpoint —
			// destroying the very state being investigated. Anything that only
			// reproduces on a live network event (a roam, a peer flapping) had to
			// be re-triggered from a cold start with fingers crossed. It's a
			// process-global setting with no dependent state, so applying it here
			// is safe and immediate.
			// Log level, live. Compared against the *logger's current level*, not
			// against cfg — cfg is the startup snapshot and is never reassigned,
			// so comparing to it meant the level could only ever be changed away
			// from its boot value, once. Boot at info, set debug: applied
			// (debug != info). Then set it back to info: the test is
			// info != info, false, and SetLevel never runs — the file saves
			// "info" correctly while the running logger stays on debug, and
			// /api/config (which reports the live level, as it should) snaps the
			// UI straight back to debug. Every other live setting here compares
			// against a tracking variable it actually updates (prevPort, prevTCP,
			// prevExtraTCP); this one reached for the stale config.
			//
			// The logger is the single source of truth for what's in effect, so
			// ask it. Set first, then log: a change that raises verbosity prints
			// under the new level, and one that lowers it to warn/error is
			// deliberately quiet.
			if want := logx.ParseLevel(newCfg.LogLevel); want != logx.CurrentLevel() {
				prev := logx.LevelName()
				logx.SetLevel(want)
				logx.Infof("log level: %s -> %s", prev, logx.LevelName())
			}
			// Log size cap, live. Like the level above, this was previously read
			// only at startup, so changing it in the web admin did nothing until a
			// restart. The rotating file exposes SetMaxBytes; a shrink is applied
			// to the on-disk file immediately (FIFO trims to fit now). logResize is
			// nil when file logging is disabled, so the guard is required.
			if logResize != nil {
				logResize(newCfg.LogMaxBytes())
			}
			applyKernelNAT(newCfg) // re-program host NAT for any rule changes
			logx.Infof("config reloaded from %s (networks add/remove, firewall/NAT/QoS/bandwidth/keys, and the underlay port applied live)", *cfgPath)
			return nil
		}

		// Local control IPC for the ban/unban/list CLI. Resolved through the same
		// helper the CLI uses (cmd/gravinet/cli_sock.go), so the end that binds and
		// the end that dials cannot disagree about the path.
		sock, note := config.NormalizeControlSocket(cfg.ControlSocket)
		if note != "" {
			logx.Warnf("control socket: %s", note)
		}
		// Upgrade machinery: nil only on genuine init failure now (see
		// newUpgradeSvc) — it no longer needs a trusted release key just to
		// exist. Built here because both the control socket (the CLI's `gravinet
		// upgrade ...`) and the web admin (this node's own Upgrade page) hang off
		// the same store, guard and fleet handle — two front doors, one set of
		// state.
		upg := newUpgradeSvc(cfg, *cfgPath, engine, int(webPortOf(cfg)))

		ctlSrv, err := control.Serve(sock, engine, logx.Default())
		if err != nil {
			// This used to be a lone Warnf, which is how the original bug stayed
			// invisible for so long: the daemon started fine, the mesh came up, and
			// the only symptom surfaced much later and somewhere else — the CLI
			// saying "no such file or directory" about a socket nothing had ever
			// created. Say plainly, at error level, that the CLI is now dead and why.
			logx.Errorf("control socket %s could not be created: %v", sock, err)
			logx.Errorf("every control command (status, ban, managed, network, fw, key, ...) will fail until this is fixed; the mesh itself is unaffected")
			logx.Errorf("check that the directory exists and is writable, or set control_socket in %s to a path under one that is", *cfgPath)
		} else {
			logx.Infof("control socket: %s", sock)
			ctlSrv.SetReload(reloadFn)
			if upg != nil {
				ctlSrv.SetUpgrade(upg.controlOp)
			}
			// Let -net accept a network name (not just a hex id) for ban/unban/fw,
			// matching the config commands. Reads the current config each time so it
			// reflects edits.
			ctlSrv.SetNameResolver(func(ref string) (uint64, bool) {
				cur, err := config.Load(*cfgPath)
				if err != nil {
					return 0, false
				}
				return cur.NetworkID(ref)
			})

			// Persist hook for firewall/identity/address is installed before Start (above).

		}

		// Web admin (hot config) — optional.
		var webSrv *webadmin.Server
		if cfg.WebAdmin.Enabled {
			webSrv = webadmin.New(cfg.WebAdmin, engine, logx.Default())
			webSrv.SetConfigPath(*cfgPath)
			webSrv.SetVersion(version, commit)
			webSrv.SetLogPath(logPath)
			webSrv.SetLogClear(logClear)
			exeDir := ""
			if exe, eerr := os.Executable(); eerr == nil {
				exeDir = filepath.Dir(exe)
			}
			webSrv.SetReadmePath(cfg.ReadmePath(*cfgPath, exeDir))
			webSrv.SetLicensePath(cfg.LicensePath(*cfgPath, exeDir))
			webSrv.SetGettingStartedPath(cfg.GettingStartedPath(*cfgPath, exeDir))
			webSrv.SetReload(reloadFn)
			if upg != nil {
				webSrv.SetUpgrade(upg.webadminCtl())
			}
			if err := webSrv.Start(); err != nil {
				logx.Warnf("web admin failed to start: %v", err)
				webSrv = nil
			}
		}

		// If this process is a freshly-swapped binary, it is now on the clock. The
		// engine is up and dialing peers; the guard gives it its confirm window to
		// get them back, and if it does not, it puts the previous binary back and
		// restarts — without needing to be told, because the node that most needs
		// rescuing is precisely the one that can no longer be reached to tell.
		if upg != nil {
			if st := upg.guard.Load(); st.Phase == upgrade.PhasePending {
				logx.Warnf("upgrade: running %s on trial (was %s) — it has %ds to get %d peer(s) back or it will be reverted",
					st.To, st.From, st.ConfirmSeconds, st.PrePeers)
				stopWatch := upg.guard.Watch(upg.healthy)
				defer stopWatch()
			}
		}

		// Make the web admin reachable for cluster management over each network's
		// overlay address. The primary listener is typically bound to loopback (safe
		// default), which a remote peer's management proxy can't reach; binding the
		// overlay address closes that gap without exposing the underlay. Skipped when
		// the primary bind is a wildcard (it already covers the overlay). Overlay
		// addresses are assigned dynamically, so this runs on a ticker.
		overlayBindStop := make(chan struct{})
		if webSrv != nil && !wildcardHost(cfg.WebAdmin.Listen) {
			if port := webPortOf(cfg); port != 0 {
				go func() {
					ensure := func() {
						for _, id := range engine.NetworkIDs() {
							a4, a6, ok := engine.NetworkSelfAddrs(id)
							if !ok {
								continue
							}
							for _, a := range []netip.Addr{a4, a6} {
								if !a.IsValid() {
									continue
								}
								addr := net.JoinHostPort(a.String(), strconv.Itoa(int(port)))
								if err := webSrv.EnsureListener(addr); err != nil {
									logx.Debugf("webadmin overlay listener %s: %v", addr, err)
								}
							}
						}
					}
					t := time.NewTicker(8 * time.Second)
					defer t.Stop()
					ensure()
					for {
						select {
						case <-overlayBindStop:
							return
						case <-t.C:
							ensure()
						}
					}
				}()
			}
		}
		defer close(overlayBindStop)

		// Key-expiry sweep: a key that crosses its expiry changes the set of valid
		// keys, but nothing rebuilds that set on its own. This watches for an expiry
		// boundary and triggers the same reload an admin edit would, which rebuilds
		// the key set live and drops sessions riding a now-expired key.
		tickStop := make(chan struct{})
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			last := ""
			if c, err := config.Load(*cfgPath); err == nil {
				last = expirySig(c)
			}
			for {
				select {
				case <-tickStop:
					return
				case <-t.C:
					c, err := config.Load(*cfgPath)
					if err != nil {
						continue
					}
					if sig := expirySig(c); sig != last {
						last = sig
						logx.Infof("key expiry: a key crossed its expiry; applying the change live")
						if e := reloadFn(); e != nil {
							logx.Warnf("key expiry reload: %v", e)
						}
					}
				}
			}
		}()

		// Seed-note inheritance: the first time a connected peer's live
		// endpoint matches a configured seed's, copy that seed's note onto
		// the peer's own permanent, node-id-keyed note — see
		// inheritSeedNotes' doc comment (seednotes.go) for why this runs on
		// a ticker rather than the connection event itself, and why it's
		// tied to the peer's id rather than its address. Shares tickStop:
		// closing it in shutdown already stops every ticker in this block,
		// not just the key-expiry one above.
		go func() {
			t := time.NewTicker(20 * time.Second)
			defer t.Stop()
			inheritSeedNotes(*cfgPath, engine, reloadFn)
			for {
				select {
				case <-tickStop:
					return
				case <-t.C:
					inheritSeedNotes(*cfgPath, engine, reloadFn)
				}
			}
		}()

		// The daemon body: run until stopped, then tear down in the safe order.
		//
		// Every step below can, in principle, block indefinitely on the
		// kernel or a subprocess: engine.Stop() waits on the TUN read loops,
		// the device/transport closes touch the OS, the nftables Clear() and
		// the DNS/hosts cleanup shell out. If any of them wedges, this process
		// never exits — and because the systemd unit is Type=notify,
		// `systemctl restart`/`stop` waits for THIS process to exit before it
		// starts the replacement, so one stuck teardown step hangs the entire
		// restart (the reported symptom). The watchdog armed here bounds that:
		// once shutdown begins, a background goroutine force-exits the process
		// if graceful teardown hasn't completed within shutdownGrace,
		// guaranteeing the process exits — and therefore the restart
		// proceeds — no matter which step is stuck. A clean shutdown that
		// finishes first disarms it (close(shutdownDone) before returning), so
		// the hard exit only ever fires on a genuine hang. The unit's
		// TimeoutStopSec is an independent outer backstop for the pathological
		// case where even this can't run.
		shutdown := func() {
			disarm := armShutdownWatchdog(shutdownGrace, func() {
				logx.Warnf("graceful shutdown exceeded %s — forcing exit so the service can restart", shutdownGrace)
				os.Exit(1) // non-zero: this was not a clean stop
			})
			defer disarm()
			close(tickStop)
			if webSrv != nil {
				webSrv.Close()
			}
			if ctlSrv != nil {
				ctlSrv.Close()
			}
			// engine.Stop blocks on the TUN read loops, so close interfaces first.
			for _, d := range devices {
				d.Close()
			}
			engine.Stop()
			if t := getTr(); t != nil {
				t.Close()
			}
			if t := getTLS(); t != nil {
				t.Close()
			}
			if cfg.ForwardingEnabled() {
				ipfwd.Restore(fwdState)
			}
			natMu.Lock()
			mgr := nfMgr
			natMu.Unlock()
			if mgr != nil {
				mgr.Clear() // remove the gravinet NAT ruleset
			}
			if upnpMgr != nil {
				// Bounded well under shutdownGrace: this is one of several
				// steps sharing that budget, and a router that's gone
				// unreachable must not be able to hang process exit — the
				// mapping just lingers until its own lease expires in that
				// case (see internal/upnp's lease/renewal doc comments).
				upnpCtx, upnpCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
				upnpMgr.Stop(upnpCtx)
				upnpCancel()
			}
			var rx, tx uint64
			if t := getTr(); t != nil {
				rx, tx = t.Stats()
			}
			logx.Infof("transport stats: rx=%d tx=%d datagrams", rx, tx)
			// Clear gravinet-managed hosts entries so stale overlay IPs don't linger
			// after the service stops (mirrors the startup clear in clearStaleHostsBlocks).
			clearStaleHostsBlocks(cfg)
			clearStaleDNSForwards(cfg)
			logx.Infof("stopped")
		}
		if ready != nil {
			ready()
		}
		select {
		case <-stop:
			logx.Infof("shutting down")
			shutdown()
		case <-restartRequested:
			logx.Infof("shutting down for a clean restart to fully rebuild the underlay socket, TUN device, and OS routes")
			shutdown()
			// On Windows under the SCM, restart by reporting a failure exit and
			// letting the configured recovery action bring us back — NOT by
			// calling Restart-Service on ourselves, which deadlocks against our
			// own SCM stop and leaves the service stopped (the reported bug).
			// RestartViaServiceManagerExit arms the non-zero exit; returning
			// here lets RunService report it so the SCM restarts us.
			if service.RestartViaServiceManagerExit() {
				logx.Infof("exiting for Windows service-manager recovery restart")
				return
			}
			if err := selfRestart(); err != nil {
				// selfRestart is an in-place re-exec (Unix). Where it isn't
				// available — notably an interactive-less Windows service, whose
				// re-exec story needs SCM cooperation selfRestart doesn't have —
				// hand off to the platform service manager if one is managing
				// gravinet: Restart-Service / systemctl restart / rcctl restart,
				// the same mechanism the CLI and web admin use. Only if that too
				// is unavailable do we leave it to the operator.
				if ok, _ := service.CanRestart(); ok {
					if done, hint := service.Restart(); done {
						logx.Infof("restart requested via the service manager")
					} else {
						logx.Warnf("automatic restart failed: %v — %s", err, hint)
					}
				} else {
					logx.Warnf("automatic restart failed: %v — restart gravinet manually to fully recover", err)
				}
			}
			// selfRestart replaces this process on success and never returns.
		}
	}

	// On Windows, run under the Service Control Manager if launched by it.
	// This call (and the SCM's SERVICE_RUNNING ack inside it) happens BEFORE
	// runBody's setup work executes — see the comment on runBody above.
	svcRun := func(stop <-chan struct{}) { runBody(stop, nil) }
	if handled, err := service.RunService("gravinet", svcRun); err != nil {
		logx.Warnf("service dispatcher: %v", err)
	} else if handled {
		return
	}

	// Interactive / systemd / launchd: stop on SIGINT or SIGTERM.
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		got := <-sig
		logx.Infof("received %s", got)
		close(stop)
	}()

	runBody(stop, func() {
		if err := service.NotifyReady(); err != nil {
			logx.Warnf("sd_notify: %v", err)
		}
	})
}

// cmdBan issues a distributed ban via the control socket.
func cmdBan(args []string) {
	node, rest := splitPositional(args)
	fs := flag.NewFlagSet("ban", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	netID := fs.String("net", "", "network name or hex id; optional if only one")
	notes := fs.String("notes", "", "ban notes")
	fs.Parse(rest)
	if node == "" {
		fatal("usage: gravinet ban <node-id> [-net NAME|id] [-notes text] [-sock path]")
	}
	resp, err := control.Do(*sock, control.Request{Cmd: "ban", Net: *netID, Node: node, Notes: *notes})
	ctlResult(resp, err)
}

// cmdUnban removes a ban this node originated.
func cmdUnban(args []string) {
	node, rest := splitPositional(args)
	fs := flag.NewFlagSet("unban", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	netID := fs.String("net", "", "network name or hex id; optional if only one")
	force := fs.Bool("force", false, "clear bans on this node from ALL origins (break-glass for a departed originator)")
	fs.Parse(rest)
	if node == "" {
		fatal("usage: gravinet unban <node-id> [-force] [-net NAME|id] [-sock path]")
	}
	resp, err := control.Do(*sock, control.Request{Cmd: "unban", Net: *netID, Node: node, Force: *force})
	ctlResult(resp, err)
}

// splitPositional pulls a single leading non-flag argument out of args so flags
// may appear before or after the node id.
func splitPositional(args []string) (positional string, rest []string) {
	for i, a := range args {
		if len(a) > 0 && a[0] != '-' {
			rest = append(append([]string{}, args[:i]...), args[i+1:]...)
			return a, rest
		}
	}
	return "", args
}

// cmdList shows peers, bans, and redistributed routes.
func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	netID := fs.String("net", "", "network name or hex id; optional if only one")
	fs.Parse(args)
	resp, err := control.Do(*sock, control.Request{Cmd: "list", Net: *netID})
	if err != nil {
		fatal("control: %v%s", err, controlDialHint())
	}
	if !resp.OK {
		fatal("%s", resp.Error)
	}
	fmt.Printf("Peers (%d):\n", len(resp.Peers))
	for _, p := range resp.Peers {
		reach := "direct"
		if p.Relayed {
			reach = "relayed"
		}
		fmt.Printf("  %-18s %-20s v4=%s v6=%s  public=%s (%s)\n", p.NodeID, p.Hostname, p.Overlay4, p.Overlay6, p.Endpoint, reach)
	}
	if resp.NATClass != "" {
		line := "This node: NAT=" + resp.NATClass
		if resp.Public != "" {
			line += "  public=" + resp.Public
		}
		fmt.Println(line)
	}
	fmt.Printf("Bans (%d):\n", len(resp.Bans))
	for _, b := range resp.Bans {
		mine := ""
		if b.Mine {
			mine = " (originated here)"
		}
		fmt.Printf("  %-18s by %-18s %q%s\n", b.Target, b.Origin, b.Notes, mine)
	}
	fmt.Printf("Routes (%d):\n", len(resp.Routes))
	for _, r := range resp.Routes {
		fmt.Printf("  %-20s via %s (metric %d)\n", r.CIDR, r.Via, r.Metric)
	}
}

func ctlResult(resp control.Response, err error) {
	if err != nil {
		fatal("control: %v%s", err, controlDialHint())
	}
	if !resp.OK {
		fatal("%s", resp.Error)
	}
	fmt.Println("ok")
}

// cmdFW drives the firewall rulebase: list, add, del, move, copy, cut, paste.
func cmdFW(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet fw <list|add|del|move|copy|cut|paste|exempt> [...]")
	}
	op, rest := args[0], args[1:]
	switch op {
	case "exempt":
		cmdFWExempt(rest)
		return
	case "list":
		fs := flag.NewFlagSet("fw list", flag.ExitOnError)
		sock := fs.String("sock", defaultControlSocket(), "control socket path")
		netID := fs.String("net", "", "network name or hex id")
		fs.Parse(rest)
		resp, err := control.Do(*sock, control.Request{Cmd: "fw", Net: *netID, FWOp: "list"})
		if err != nil {
			fatal("control: %v", err)
		}
		if !resp.OK {
			fatal("%s", resp.Error)
		}
		fmt.Printf("Firewall rules (%d) — default policy: allow, stateful\n", len(resp.FW))
		for i, r := range resp.FW {
			svcNote := ""
			if r.ServicesNegate {
				svcNote = " !svc"
			}
			fmt.Printf("  [%d] id=%d %-5s %-4s proto=%-4s src=%-18s dst=%-18s sport=%d-%d dport=%d-%d%s %s\n",
				i, r.ID, r.Action, r.Direction, r.Proto,
				negPrefix(r.SrcNegate)+orAny(r.Src), negPrefix(r.DstNegate)+orAny(r.Dst),
				r.SrcPortMin, r.SrcPortMax, r.DstPortMin, r.DstPortMax, svcNote, r.Notes)
		}
	case "add":
		fs := flag.NewFlagSet("fw add", flag.ExitOnError)
		sock := fs.String("sock", defaultControlSocket(), "control socket path")
		netID := fs.String("net", "", "network name or hex id")
		at := fs.Int("at", -1, "insert position (-1 = end)")
		action := fs.String("action", "allow", "allow|deny")
		dir := fs.String("dir", "both", "in|out|both")
		proto := fs.String("proto", "any", "tcp|udp|icmp|any")
		src := fs.String("src", "", "source CIDR/host (empty = any)")
		dst := fs.String("dst", "", "dest CIDR/host (empty = any)")
		srcNegate := fs.Bool("src-negate", false, "match anything EXCEPT -src")
		dstNegate := fs.Bool("dst-negate", false, "match anything EXCEPT -dst")
		svcNegate := fs.Bool("services-negate", false, "match any service EXCEPT -proto/-sport/-dport")
		sport := fs.String("sport", "", "source port or range a-b")
		dport := fs.String("dport", "", "dest port or range a-b")
		notes := fs.String("notes", "", "notes")
		fs.Parse(rest)
		spMin, spMax := parsePortRange(*sport)
		dpMin, dpMax := parsePortRange(*dport)
		rule := mesh.FirewallRule{
			Action: *action, Direction: *dir, Proto: *proto, Src: *src, Dst: *dst,
			SrcNegate: *srcNegate, DstNegate: *dstNegate, ServicesNegate: *svcNegate,
			SrcPortMin: spMin, SrcPortMax: spMax, DstPortMin: dpMin, DstPortMax: dpMax,
			Notes: *notes,
		}
		resp, err := control.Do(*sock, control.Request{Cmd: "fw", Net: *netID, FWOp: "add", FWAt: *at, FWRule: rule})
		if err != nil {
			fatal("control: %v", err)
		}
		if !resp.OK {
			fatal("%s", resp.Error)
		}
		if len(resp.FW) == 1 {
			fmt.Printf("added rule id=%d\n", resp.FW[0].ID)
		} else {
			fmt.Println("ok")
		}
	case "del", "copy", "cut":
		ids, rest2 := splitIDs(rest)
		fs := flag.NewFlagSet("fw "+op, flag.ExitOnError)
		sock := fs.String("sock", defaultControlSocket(), "control socket path")
		netID := fs.String("net", "", "network name or hex id")
		fs.Parse(rest2)
		if len(ids) == 0 {
			fatal("usage: gravinet fw %s <id[,id,...]> [-net NAME|id]", op)
		}
		ctlResult(control.Do(*sock, control.Request{Cmd: "fw", Net: *netID, FWOp: op, FWIDs: ids}))
	case "move":
		id, rest2 := splitPositional(rest)
		fs := flag.NewFlagSet("fw move", flag.ExitOnError)
		sock := fs.String("sock", defaultControlSocket(), "control socket path")
		netID := fs.String("net", "", "network name or hex id")
		to := fs.Int("to", 0, "target index")
		fs.Parse(rest2)
		rid := parseUint(id)
		ctlResult(control.Do(*sock, control.Request{Cmd: "fw", Net: *netID, FWOp: "move", FWIDs: []uint64{rid}, FWTo: *to}))
	case "paste":
		fs := flag.NewFlagSet("fw paste", flag.ExitOnError)
		sock := fs.String("sock", defaultControlSocket(), "control socket path")
		netID := fs.String("net", "", "network name or hex id")
		at := fs.Int("at", -1, "insert position (-1 = end)")
		fs.Parse(rest)
		resp, err := control.Do(*sock, control.Request{Cmd: "fw", Net: *netID, FWOp: "paste", FWAt: *at})
		if err != nil {
			fatal("control: %v", err)
		}
		if !resp.OK {
			fatal("%s", resp.Error)
		}
		fmt.Printf("pasted %d rule(s)\n", resp.Count)
	default:
		fatal("unknown fw subcommand %q", op)
	}
}

func orAny(s string) string {
	if s == "" {
		return "any"
	}
	return s
}

// negPrefix prepends the CLI's negation marker ("!") when a src/dst field is
// negated, matching the same convention pfctl/iptables-adjacent tools use —
// "!10.0.0.0/24" reads naturally as "not 10.0.0.0/24".
func negPrefix(negate bool) string {
	if negate {
		return "!"
	}
	return ""
}

// shortHostname strips any domain suffix from an OS-reported hostname.
// os.Hostname() conventionally returns a short name on Linux, macOS,
// Windows, and FreeBSD — but OpenBSD's /etc/myname is very commonly set to
// a full FQDN (e.g. "gn-openbsd.cush.local"), and os.Hostname() there just
// echoes it back verbatim. gravinet's hostname is gossiped mesh-wide and
// used for peer display and bare-hostname resolution, so a lone FQDN breaks
// both: the peers table shows it inconsistently next to every other node's
// short name, and "ping gn-openbsd" wouldn't resolve the way "ping
// gn-cush1" does. Only called for the auto-detected case — an explicit
// config.Hostname is taken verbatim, on the assumption that whoever set it
// did so deliberately.
func shortHostname(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i]
	}
	return s
}

// splitIDs pulls a leading comma-separated id list out of args.
func splitIDs(args []string) ([]uint64, []string) {
	id, rest := splitPositional(args)
	if id == "" {
		return nil, rest
	}
	var ids []uint64
	for _, p := range strings.Split(id, ",") {
		ids = append(ids, parseUint(p))
	}
	return ids, rest
}

func parseUint(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		fatal("bad id %q", s)
	}
	return v
}

func parsePortRange(s string) (int, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		lo, _ := strconv.Atoi(strings.TrimSpace(s[:i]))
		hi, _ := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		return lo, hi
	}
	p, _ := strconv.Atoi(s)
	return p, p
}

// tunOpenRetryBudget bounds how long newTunRetrying keeps trying. Comfortably
// longer than a kernel netdev teardown, short enough that a genuinely
// misconfigured interface still fails fast enough to be obvious in the log.
const tunOpenRetryBudget = 15 * time.Second

// newTunRetrying creates the overlay interface, retrying on failure with
// backoff.
//
// The retry is not cosmetic — it closes a real hole that loses the interface
// entirely after a sleep/wake on Linux. The sequence:
//
//  1. The host sleeps and wakes. maintLoop detects the clock jump and fires the
//     suspend/resume hook, which asks for a clean process restart (the underlay
//     socket, TUN device and OS routes can't all be reliably rebuilt in place —
//     see mesh.Engine.SetSuspendResumeHook).
//  2. shutdown() runs, closing the TUN fd. A Linux TUN is non-persistent, so
//     mesh0 is destroyed with it.
//  3. selfRestart() immediately execs a new image of this process.
//  4. The new process calls tun.New("mesh0") — but the kernel's teardown of the
//     *old* mesh0 is asynchronous (unregister_netdevice is deferred, and can be
//     held up by references the stack hasn't dropped yet). TUNSETIFF can still
//     see the name in use and fail.
//
// A single attempt then loses a race against its own predecessor. And the
// consequence was total: buildNetSpecs logs the error and `continue`s, dropping
// the network — so there is no NetSpec, no netState, no maintLoop and therefore
// no dataplane supervisor either. The supervisor in mesh/dataplane.go guards a
// *running* network's interface; nothing guarded failing to create it. The
// daemon came up reporting success with zero networks, mesh0 simply absent, and
// nothing ever retried it. Exactly the reported symptom: the interface is gone
// after a wake and stays gone until someone restarts the daemon by hand.
//
// Retrying costs nothing when the interface opens first time, which is the
// overwhelmingly common case.
func newTunRetrying(name string, mtu int) (*tun.Device, error) {
	deadline := time.Now().Add(tunOpenRetryBudget)
	delay := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		d, err := tun.New(name, mtu)
		if err == nil {
			if attempt > 1 {
				logx.Infof("overlay interface %s created on attempt %d (the previous one was still being torn down by the kernel)", name, attempt)
			}
			return d, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("after %d attempt(s) over %s: %w", attempt, tunOpenRetryBudget, err)
		}
		logx.Warnf("overlay interface %s: %v — retrying in %s (a previous instance's interface may still be going away)", name, err, delay)
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// buildNetSpecs creates the overlay interfaces and assembles per-network specs.
// Networks that fail to come up are logged and skipped.
// Networks that fail to come up are logged and skipped.
func buildNetSpecs(cfg *config.Config) ([]mesh.NetSpec, []*tun.Device) {
	var specs []mesh.NetSpec
	var devices []*tun.Device
	// All overlay subnets the node participates in. A seed must never resolve
	// into one of these — a bootstrap peer has to be an underlay address, not a
	// mesh/tunnel IP (see resolveSeeds).
	overlays := overlaysOf(cfg)
	for i, n := range cfg.Networks {
		if !n.Enabled {
			continue
		}
		spec, dev, err := buildOneNetSpec(n, cfg, overlays, i)
		if err != nil {
			// Dropping the network here is not a partial degradation — it means
			// no NetSpec, so no netState, no maintLoop, and no data-plane
			// supervisor (mesh/dataplane.go) either. The interface will not be
			// retried or rebuilt by anything; it is simply absent until someone
			// restarts the daemon. Say so plainly rather than emitting one error
			// line and carrying on as if the node were healthy.
			logx.Errorf("network %s: %v", n.ID, err)
			logx.Errorf("network %s is DISABLED for this run: its overlay interface could not be created, so nothing will supervise or retry it. The node will run with this network entirely absent until gravinet is restarted.", n.ID)
			continue
		}
		specs = append(specs, spec)
		devices = append(devices, dev)
		logx.Infof("network %s (%s): %s up, mtu %d, self4=%s self6=%s seeds=%d routes=%d hosts_sync=%v",
			n.ID, n.Name, dev.Name(), dev.MTU(), spec.Self4, spec.Self6, len(spec.Seeds), len(spec.Routes), spec.HostsSync)
	}
	return specs, devices
}

// buildOneNetSpec builds the full spec (and creates the TUN device) for a single
// network. Shared by startup and the live-reload "add network" path. overlays is
// the set of all overlay subnets, used to keep seeds on underlay addresses.
func buildOneNetSpec(n config.Network, cfg *config.Config, overlays []netip.Prefix, idx int) (mesh.NetSpec, *tun.Device, error) {
	netID, err := strconv.ParseUint(n.ID, 16, 64)
	if err != nil {
		return mesh.NetSpec{}, nil, fmt.Errorf("id must be 64-bit hex: %w", err)
	}
	keys, err := crypto.NewKeySet(keyStrings(n))
	if err != nil {
		return mesh.NetSpec{}, nil, fmt.Errorf("bad keys: %w", err)
	}
	name := n.TUNName
	if name == "" {
		name = fmt.Sprintf("mesh%d", idx)
	}
	dev, err := newTunRetrying(name, n.MTU)
	if err != nil {
		return mesh.NetSpec{}, nil, fmt.Errorf("tun: %w", err)
	}

	spec := mesh.NetSpec{
		ID:         netID,
		Name:       n.Name,
		Keys:       keys,
		KeyLabels:  keyLabelMap(n),
		KeyExpires: keyExpiryMap(n),
		Dev:        dev,
		AllowRelay: n.AllowRelay,
		Ban:        cfg.AuthBan,
	}
	// Let the engine rebuild this interface if the OS tears it down at runtime
	// (driver reset, `ip link del`, VM/Wi-Fi churn). Same name + MTU as the
	// original. Explicit nil-on-error so a failed create never hands back a
	// non-nil Device interface wrapping a nil *tun.Device.
	spec.NewDevice = func() (mesh.Device, error) {
		// Retrying here too: a rebuild is triggered precisely when the OS has
		// just destroyed the interface, so the kernel may still be unwinding the
		// old netdev when we ask for the name back (see newTunRetrying).
		d, derr := newTunRetrying(name, n.MTU)
		if derr != nil {
			return nil, derr
		}
		return d, nil
	}
	spec.SeedTCPPort = n.SeedTCPPort
	if n.Subnet4 != "" {
		spec.Subnet4 = netip.MustParsePrefix(n.Subnet4)
	}
	if n.Subnet6 != "" {
		spec.Subnet6 = netip.MustParsePrefix(n.Subnet6)
	}
	if n.Address4 != "" {
		if p, err := netip.ParsePrefix(n.Address4); err == nil {
			if err := dev.AddIPv4(p.Addr(), p.Bits()); err != nil {
				logx.Errorf("network %s: assign v4: %v", n.ID, err)
			} else {
				spec.Self4 = p.Addr()
				// The connected route for the whole subnet is supposed to be a
				// side effect of AddIPv4's netmask (see tun_darwin.go), but
				// that isn't reliable in practice on real macOS — a live
				// report showed the route missing here specifically (this is
				// the path every node takes on every ordinary restart once
				// its address is pinned to config; DAD's assignAddr, which
				// has the same explicit AddRoute fix, never runs again once
				// that's true). Install it explicitly; a no-op if it's
				// already there.
				if spec.Subnet4.IsValid() {
					if err := dev.AddRoute(spec.Subnet4, 0); err != nil {
						logx.Debugf("network %s: base route %s: %v", n.ID, spec.Subnet4, err)
					}
				}
			}
		}
	}
	if n.Address6 != "" {
		if p, err := netip.ParsePrefix(n.Address6); err == nil {
			if err := dev.AddIPv6(p.Addr(), p.Bits()); err != nil {
				logx.Errorf("network %s: assign v6: %v", n.ID, err)
			} else {
				spec.Self6 = p.Addr()
				if spec.Subnet6.IsValid() {
					if err := dev.AddRoute(spec.Subnet6, 0); err != nil {
						logx.Debugf("network %s: base route %s: %v", n.ID, spec.Subnet6, err)
					}
				}
			}
		}
	}
	boot := append(append([]string{}, n.Seeds.Addrs()...), n.PeerCache...)
	spec.Seeds = resolveSeeds(boot, cfg.PrimaryPort, overlays)
	spec.TCPSeeds = resolveTCPSeeds(boot, cfg.TCPFallbackPortValue(), overlays)

	spec.HostsSync = n.HostsSync.Enabled
	spec.HostsPath = n.HostsSync.Path
	if n.HostsSync.TTLSeconds > 0 {
		spec.HostsTTL = time.Duration(n.HostsSync.TTLSeconds) * time.Second
	}

	spec.DNSSync = n.DNSSync.Enabled
	spec.SearchLearned = !n.DNSSync.DisableSearchDomains
	if n.DNSSync.TTLSeconds > 0 {
		spec.DNSTTL = time.Duration(n.DNSSync.TTLSeconds) * time.Second
	}

	spec.BroadcastPPS = n.StormControl.BroadcastPPS
	spec.MulticastPPS = n.StormControl.MulticastPPS
	spec.StormBurst = n.StormControl.Burst

	fillRuntimeSpec(&spec, n, cfg.EffectiveFirewallExempt(), cfg.NATStateTimeout, cfg.FirewallServices)
	return spec, dev, nil
}

// overlaysOf returns every overlay subnet across all configured networks, so the
// seed resolver can reject bootstrap addresses that fall inside the mesh.
func overlaysOf(cfg *config.Config) []netip.Prefix {
	var overlays []netip.Prefix
	for _, n := range cfg.Networks {
		if p, err := netip.ParsePrefix(n.Subnet4); err == nil {
			overlays = append(overlays, p.Masked())
		}
		if p, err := netip.ParsePrefix(n.Subnet6); err == nil {
			overlays = append(overlays, p.Masked())
		}
	}
	return overlays
}

// fillRuntimeSpec populates the hot-reloadable fields of a NetSpec (throttle,
// QoS, firewall, NAT) from a network's config. Shared by initial setup and by
// the reload path so both produce identical specs.
// toMeshFirewallObjects / toMeshFirewallServices convert the node-global
// firewall catalog from its config shape to its mesh shape — used once at
// startup to seed mesh.Options.FirewallObjects/FirewallServices (the engine
// initializes every network's live catalog from there; see
// mesh.Options.FirewallObjects' doc comment). The reverse conversion (mesh
// shape back to config shape, for persistence) lives inline in the persist
// hook above, matching how the rules conversion there has always worked.
func toMeshFirewallObjects(objs []config.FirewallObject) []mesh.FirewallObject {
	out := make([]mesh.FirewallObject, 0, len(objs))
	for _, o := range objs {
		out = append(out, mesh.FirewallObject{
			Name: o.Name, Kind: o.Kind, Addresses: o.Addresses, Members: o.Members, Notes: o.Notes,
		})
	}
	return out
}
func toMeshFirewallServices(svcs []config.FirewallService) []mesh.FirewallService {
	out := make([]mesh.FirewallService, 0, len(svcs))
	for _, s := range svcs {
		ms := mesh.FirewallService{Name: s.Name, Notes: s.Notes}
		for _, p := range s.Ports {
			ms.Ports = append(ms.Ports, mesh.FirewallServicePort{Proto: p.Proto, PortMin: p.PortMin, PortMax: p.PortMax})
		}
		out = append(out, ms)
	}
	return out
}

func fillRuntimeSpec(spec *mesh.NetSpec, n config.Network, exempts []config.FirewallExempt, natStateTimeout int, fwServices []config.FirewallService) {
	// Redistributed routes (hot-reloadable; applied live on reload). Disabled
	// route entries are skipped.
	for _, rt := range n.Routes {
		if !rt.Enabled {
			continue
		}
		if p, err := netip.ParsePrefix(rt.CIDR); err == nil {
			spec.Routes = append(spec.Routes, p)
			if rt.Metric != 0 {
				if spec.RouteMetric == nil {
					spec.RouteMetric = map[netip.Prefix]int{}
				}
				spec.RouteMetric[p] = rt.Metric
			}
		} else {
			logx.Errorf("network %s: bad route %q: %v", n.ID, rt.CIDR, err)
		}
	}
	for _, rj := range n.RouteRej {
		if rj.Disabled {
			continue // disabled reject entries stay in config but aren't applied
		}
		if p, err := netip.ParsePrefix(rj.CIDR); err == nil {
			spec.RouteReject = append(spec.RouteReject, mesh.RejectRule{Prefix: p.Masked(), Inclusive: rj.Inclusive})
		} else {
			logx.Errorf("network %s: bad route-reject %q: %v", n.ID, rj.CIDR, err)
		}
	}

	// Locally-disabled peers (local-only blocklist; applied live on reload).
	spec.DisabledPeers = append([]string(nil), n.DisabledPeers...)
	if len(n.PeerNotes) > 0 {
		spec.PeerNotes = make(map[string]string, len(n.PeerNotes))
		for k, v := range n.PeerNotes {
			spec.PeerNotes[k] = v
		}
	}

	// Bandwidth throttling (off by default).
	if n.Throttle.Enabled {
		spec.ThrottleUp = n.Throttle.UpBytesPerSec
		spec.ThrottleDown = n.Throttle.DownBytesPerSec
		spec.ThrottleBurst = n.Throttle.BurstBytes
		spec.ThrottleQueue = n.Throttle.QueueBytes
	}

	// QoS classifier (off by default; also needs an up-throttle to bite).
	if n.QoS.Enabled {
		classes := n.QoS.Classes
		if classes < 1 {
			classes = 5
		}
		spec.QoS = mesh.NewClassifier(classes, n.QoS.DefaultClass, qosClassRules(n.QoS.Rules, fwServices, n.Name), n.QoS.ClassDSCP)
	}

	// Firewall: the enabled flag governs *enforcement* (allow() short-circuits to
	// allow-all when off), but the rulebase is loaded regardless of that flag.
	// Loading rules only when enabled meant disabling the firewall cleared the
	// engine's rulebase, so the UI (which reads the live engine rules) showed no
	// rules while disabled, and a persist firing while disabled could even wipe
	// them from config. Keeping the rules loaded keeps them visible and safe;
	// they simply aren't applied until the firewall is switched back on.
	spec.FirewallEnabled = n.Firewall.Enabled
	for _, fr := range n.Firewall.Rules {
		spec.FirewallRules = append(spec.FirewallRules, mesh.FirewallRule{
			Disabled:       fr.Disabled,
			Action:         fr.Action,
			Direction:      fr.Direction,
			Proto:          fr.Proto,
			Src:            fr.Src,
			Dst:            fr.Dst,
			SrcNegate:      fr.SrcNegate,
			DstNegate:      fr.DstNegate,
			SrcPortMin:     fr.SrcPortMin,
			SrcPortMax:     fr.SrcPortMax,
			DstPortMin:     fr.DstPortMin,
			DstPortMax:     fr.DstPortMax,
			Services:       fr.Services,
			ServicesNegate: fr.ServicesNegate,
			Log:            fr.Log,
			Notes:          fr.Notes,
		})
	}
	// The reusable address-object/service catalogs rules resolve against are
	// node-global now (config.Config.FirewallObjects/FirewallServices,
	// mesh.Options.FirewallObjects/FirewallServices) — not part of this
	// per-network spec. The engine already has the current catalog live
	// (seeded at startup, updated via SetFirewallObjects/SetFirewallServices)
	// and reapplies it to every network on each reload; nothing to fill in
	// here.
	// Always-allowed exemptions (control/management/routing). This list is
	// node-global — the same allowlist applies to every network — and is passed
	// in by the caller. Populated regardless of the enabled flag so the engine can
	// report them and apply them the moment the filter is switched on.
	for _, ex := range exempts {
		if ex.Disabled {
			continue // disabled entries stay in the allowlist but aren't applied
		}
		proto, _ := config.ParseExemptProto(ex.Proto) // validated on load
		spec.FirewallExempts = append(spec.FirewallExempts, mesh.FirewallExempt{
			Name:  ex.Name,
			Proto: proto,
			Port:  uint16(ex.Port),
			Mgmt:  ex.Mgmt,
		})
	}

	// NAT (off by default).
	spec.NATEnabled = n.NAT.Enabled
	spec.NATStateTimeout = time.Duration(natStateTimeout) * time.Second
	spec.AdvHosts = spec.AdvHosts[:0]
	for _, h := range n.HostsAdvertise {
		if h.Disabled {
			continue // disabled records stay in config but are not advertised
		}
		ip, err := netip.ParseAddr(h.IP)
		if err != nil {
			continue
		}
		spec.AdvHosts = append(spec.AdvHosts, mesh.HostRecordSpec{Name: h.Name, IP: ip})
	}
	spec.HostReject = spec.HostReject[:0]
	for _, h := range n.HostsReject {
		if h.Disabled {
			continue // disabled reject entries stay in config but don't filter
		}
		spec.HostReject = append(spec.HostReject, h.Name)
	}
	spec.AdvDNS = spec.AdvDNS[:0]
	for _, d := range n.DNSAdvertise {
		if d.Disabled {
			continue // disabled forwards stay in config but are not advertised
		}
		servers := make([]netip.Addr, 0, len(d.Servers))
		for _, s := range d.Servers {
			if a, err := netip.ParseAddr(s); err == nil {
				servers = append(servers, a)
			}
		}
		if len(servers) == 0 {
			continue
		}
		spec.AdvDNS = append(spec.AdvDNS, mesh.DNSForwardSpec{Domain: d.Domain, Servers: servers})
	}
	spec.DNSReject = spec.DNSReject[:0]
	for _, d := range n.DNSReject {
		if d.Disabled {
			continue // disabled reject entries stay in config but don't filter
		}
		spec.DNSReject = append(spec.DNSReject, d.Domain)
	}
	// Search domains are no longer a separately managed list: each advertised
	// (enabled) domain doubles as a plain search suffix for this node's own
	// resolver, so the domain string here is the search domain string too.
	// Gated on the same "at least one valid server" check spec.AdvDNS above
	// applies, and for the same reason: a domain with no valid server never
	// becomes a routing entry, so it must not become a search suffix either —
	// otherwise an unqualified query completed against it has nothing behind
	// it to answer, which is strictly worse than not completing it at all.
	spec.SearchDomains = spec.SearchDomains[:0]
	for _, d := range n.DNSAdvertise {
		if d.Disabled {
			continue
		}
		hasValidServer := false
		for _, s := range d.Servers {
			if _, err := netip.ParseAddr(s); err == nil {
				hasValidServer = true
				break
			}
		}
		if !hasValidServer {
			continue
		}
		spec.SearchDomains = append(spec.SearchDomains, d.Domain)
	}
	spec.NAT = spec.NAT[:0]
	if n.NAT.Enabled {
		for _, nr := range n.NAT.Rules {
			if !nr.Enabled {
				continue
			}
			spec.NAT = append(spec.NAT, mesh.NATRuleSpec{
				Source:    nr.Source,
				Dest:      nr.Dest,
				Translate: nr.Translate,
				Interface: nr.Interface,
			})
		}
	}
}

// kernelNATRules derives the NAT rules that must be enforced by the host kernel
// (netfilter), as opposed to the userspace overlay path. Traffic crossing a
// physical interface needs kernel-level, conntrack-backed NAT to reverse-
// translate replies correctly; the userspace overlay path handles everything
// else (see internal/mesh/nat.go). A masquerade/SNAT rule is scoped to a
// specific OutIface, so installing one for a rule whose traffic never
// actually egresses that interface (pure overlay-to-overlay routing, say) is
// harmless — it simply never matches anything — so every enabled rule gets a
// kernel-side attempt here regardless of what its traffic turns out to be.
func kernelNATRules(cfg *config.Config) []netfilter.Rule {
	var out []netfilter.Rule
	for i := range cfg.Networks {
		n := &cfg.Networks[i]
		if !n.Enabled || !n.NAT.Enabled {
			continue
		}
		for _, r := range n.NAT.Rules {
			if !r.Enabled {
				continue
			}
			src := parsePfx(r.Source)
			dst := parsePfx(r.Dest)
			t := strings.TrimSpace(r.Translate)
			// port-forward:<addr> is DNAT — see config.NATRule's doc comment.
			// Case-insensitive prefix match, same as config.buildNATRule's.
			if len(t) >= len(natPortForwardPrefix) && strings.EqualFold(t[:len(natPortForwardPrefix)], natPortForwardPrefix) {
				addr := strings.TrimSpace(t[len(natPortForwardPrefix):])
				to, err := netip.ParseAddr(addr)
				if err != nil || !to.IsValid() {
					continue // DNAT needs a literal IPv4/IPv6 target
				}
				out = append(out, netfilter.Rule{Kind: netfilter.DNAT, Dest: dst, InIface: r.Interface, To: to, V6: to.Is6()})
				continue
			}
			if t == "" || strings.EqualFold(t, "masquerade") {
				// Family comes from the source prefix; default IPv4 when unspecified.
				out = append(out, netfilter.Rule{Kind: netfilter.Masquerade, Source: src, OutIface: r.Interface, V6: src.IsValid() && src.Addr().Is6()})
			} else if to, err := netip.ParseAddr(t); err == nil && to.IsValid() {
				out = append(out, netfilter.Rule{Kind: netfilter.SNAT, Source: src, OutIface: r.Interface, To: to, V6: to.Is6()})
			}
		}
	}
	return out
}

// natPortForwardPrefix marks a NAT rule's Translate value as DNAT — see
// config.NATRule's doc comment. Kept as its own copy here (matching
// internal/config's own copy) rather than exported from either package
// solely for this one shared keyword.
const natPortForwardPrefix = "port-forward:"

// parsePfx parses a CIDR (or a bare IPv4/IPv6 host as /32 or /128);
// empty/invalid = zero prefix, which the netfilter generators treat as "any".
func parsePfx(s string) netip.Prefix {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Prefix{}
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return p
	}
	if a, err := netip.ParseAddr(s); err == nil {
		bits := 32
		if a.Is6() {
			bits = 128
		}
		return netip.PrefixFrom(a, bits)
	}
	return netip.Prefix{}
}

// resolveSeeds turns "host:port" strings into underlay endpoints.
// peerCacheMax bounds the number of bootstrap endpoints cached per network.
const peerCacheMax = 32

// peerCacheStaleGrace is how long a peer_cache candidate that's never once
// connected gets to keep its place before the persist hook drops it. Long
// enough that routine downtime (a reboot, a brief outage) isn't mistaken for
// permanent staleness; short enough that a wrong or decommissioned address
// doesn't linger in the config indefinitely.
const peerCacheStaleGrace = 1 * time.Hour

// mergePeerCache builds the next peer_cache to persist: fresh (currently
// connected endpoints) always wins a slot first, then as many of the
// previously-cached entries as still deserve one, deduped and capped at max.
//
// A cached entry keeps its slot if it's already fresh, if everConnected
// records it as having been reachable at some point since this process
// started (not just at this instant — a peer that's briefly down when this
// runs shouldn't be punished for bad timing), or if uptime hasn't yet reached
// staleGrace (a freshly-added candidate deserves a fair chance before being
// judged). Otherwise it's dropped and counted in the returned prune count.
//
// This exists because nothing else in the bootstrap path ever removes a
// peer_cache entry: it's deliberately durable, operator-independent bootstrap
// state (unlike gossip-learned seeds, which the runtime prunes by node
// identity once their peer connects via a different address — see install()
// in internal/mesh) — and that durability is exactly what lets a wrong or
// decommissioned address linger in the config forever if nothing ever
// re-validates it.
// foldPropagatedKeys applies mesh-distributed keys onto a network's key
// slots: a key not yet present goes into pk.SlotHint if that slot is free (so
// it lands in the same slot as wherever it was distributed from, when
// possible), else the first free slot; a key already present has its label
// and expiry reconciled if the mesh's copy has since changed (a relabel, or
// an expiry change made after the fact, on a distributed key — both travel
// the same way). Enabled on an already-present slot is left alone either way,
// so retiring a key by disabling its slot is never undone here. unplaced
// counts distributed keys that had no free slot to land in, for the caller to
// log; changed reports whether anything in keys actually differs from before.
func foldPropagatedKeys(keys [8]config.KeySlot, propagated []mesh.PropagatedKeyInfo) (updated [8]config.KeySlot, unplaced int, changed bool) {
	updated = keys
	for _, pk := range propagated {
		haveSlot, free := -1, -1
		for si := range updated {
			if updated[si].Key == pk.KeyB64 {
				haveSlot = si
				break
			}
			if free < 0 && updated[si].Key == "" {
				free = si
			}
		}
		if haveSlot >= 0 {
			if updated[haveSlot].Label != pk.Label {
				updated[haveSlot].Label = pk.Label
				changed = true
			}
			if updated[haveSlot].Expires != pk.Expires {
				updated[haveSlot].Expires = pk.Expires
				changed = true
			}
			continue
		}
		target := free
		if pk.SlotHint >= 0 && pk.SlotHint < len(updated) && updated[pk.SlotHint].Key == "" {
			target = pk.SlotHint
		}
		if target < 0 {
			unplaced++
			continue
		}
		updated[target] = config.KeySlot{Key: pk.KeyB64, Label: pk.Label, Enabled: true, Expires: pk.Expires, Distributed: true}
		changed = true
	}
	return updated, unplaced, changed
}

// applyKeyRetractions clears any slot in keys matching a retracted key ID,
// unless it's the network's only enabled key — config.IsLastEnabledKey's
// safety rule (the same one config.KeyDelete enforces), reused here rather
// than reimplemented, so a retraction can never brick this node's own ability
// to authenticate. refused holds the IDs a retraction couldn't be applied to
// for that reason, for the caller to log; a refused retraction stays pending
// on the engine side (see RetractedKeys/forgetAppliedRetractions) and is
// retried on a later persist cycle once the operator resolves it.
func applyKeyRetractions(keys [8]config.KeySlot, retracted []crypto.KeyID) (updated [8]config.KeySlot, refused []crypto.KeyID, changed bool) {
	updated = keys
	for _, id := range retracted {
		slot := -1
		for si, k := range updated {
			if k.Key == "" {
				continue
			}
			raw, err := crypto.DecodeKey(k.Key)
			if err != nil || crypto.DeriveKeyID(raw) != id {
				continue
			}
			slot = si
			break
		}
		if slot < 0 {
			continue // don't have this key
		}
		if config.IsLastEnabledKey(&config.Network{Keys: updated}, slot) {
			refused = append(refused, id)
			continue
		}
		updated[slot] = config.KeySlot{}
		changed = true
	}
	return updated, refused, changed
}

func mergePeerCache(fresh, cached []string, everConnected map[string]bool, uptime, staleGrace time.Duration, max int) (merged []string, pruned int) {
	merged = make([]string, 0, max)
	seen := map[string]bool{}
	addEp := func(s string) {
		if s != "" && !seen[s] && len(merged) < max {
			seen[s] = true
			merged = append(merged, s)
		}
	}
	for _, s := range fresh {
		addEp(s)
	}
	for _, s := range cached {
		if seen[s] || everConnected[s] || uptime < staleGrace {
			addEp(s)
			continue
		}
		pruned++
	}
	return merged, pruned
}

// clearStaleHostsBlocks wipes each network's gravinet-managed hosts block at
// startup. The block maps peer hostnames to overlay IPs; left over from a prior
// run (before any peer reconnects) it can hijack seed resolution — a seed named
// after a peer would resolve to that peer's tunnel IP, which is unreachable until
// the tunnel is up, so the node never bootstraps. Cleared here, repopulated live.
func clearStaleHostsBlocks(cfg *config.Config) {
	for _, n := range cfg.Networks {
		if !n.HostsSync.Enabled {
			continue
		}
		path := n.HostsSync.Path
		if path == "" {
			path = hosts.DefaultPath()
		}
		id, err := strconv.ParseUint(n.ID, 16, 64)
		if err != nil {
			continue
		}
		if err := hosts.Sync(path, fmt.Sprintf("%016x", id), nil); err != nil {
			logx.Debugf("startup: clearing stale hosts block (%s): %v", path, err)
		}
	}
}

// clearStaleDNSForwards reverts each network's gravinet-registered conditional-
// forwarding domains at startup/shutdown, the DNS analog of
// clearStaleHostsBlocks. Left registered from a prior run with no daemon behind
// them, a routing domain fails every query under it (SERVFAIL, not a fallback
// to normal DNS — see internal/resolver's design notes), which is worse than
// the stale-hosts case: it's a live OS-wide resolution regression, not just a
// self-referential seed-bootstrap hazard. Cleared here, repopulated live by
// syncDNS once the mesh (and, on Linux, its tun interface) is back up.
func clearStaleDNSForwards(cfg *config.Config) {
	for idx, n := range cfg.Networks {
		if !n.DNSSync.Enabled {
			continue
		}
		id, err := strconv.ParseUint(n.ID, 16, 64)
		if err != nil {
			continue
		}
		tag := fmt.Sprintf("%016x", id)
		iface := n.TUNName
		if iface == "" {
			iface = fmt.Sprintf("mesh%d", idx) // must match buildNetSpecs' auto-naming
		}
		if err := resolver.Clear(tag, iface); err != nil {
			logx.Debugf("startup: clearing stale dns forwards (tag %s, iface %s): %v", tag, iface, err)
		}
	}
}

func resolveSeeds(seeds []string, primaryPort int, overlays []netip.Prefix) []netip.AddrPort {
	if primaryPort <= 0 {
		primaryPort = config.DefaultUDPPort
	}
	// A seed given without a port is tried on the primary port and every
	// fallback port, since the remote may have bound any of them (the same walk
	// the local daemon does). An explicit "host:port" is used verbatim; a
	// comma-separated "host:port,port,..." tries each of the listed ports
	// against that host instead of the built-in fallback set — for a seed
	// known to answer on a handful of specific, non-default ports (e.g. ones
	// likely to make it through a restrictive firewall) rather than gravinet's
	// own.
	standard := append([]int{primaryPort}, config.FallbackUDPPorts...)
	seen := map[netip.AddrPort]bool{}
	var out []netip.AddrPort
	add := func(ap netip.AddrPort) {
		if !ap.IsValid() || seen[ap] {
			return
		}
		// Inception guard: a bootstrap seed must resolve to an underlay address.
		// If it lands inside one of our overlay subnets it's almost certainly a
		// stale mesh IP (e.g. a hosts-sync entry from a previous run) that is only
		// reachable through the very tunnel we're trying to bring up — using it
		// would deadlock the node offline. Drop it.
		for _, sub := range overlays {
			if sub.IsValid() && sub.Contains(ap.Addr()) {
				logx.Warnf("seed resolves to overlay address %s (inside %s) — ignoring it; "+
					"a bootstrap peer must be an underlay address, not a mesh/tunnel IP "+
					"(stale /etc/hosts entry?)", ap.Addr(), sub)
				return
			}
		}
		seen[ap] = true
		out = append(out, ap)
	}
	for _, s := range seeds {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// tcp:// seeds are dialed over the TLS fallback, not UDP — handled by
		// resolveTCPSeeds. Strip the (default) udp:// scheme for the rest.
		tr, hp := config.SeedParts(s)
		if tr != "udp" {
			continue
		}
		s = hp
		if host, ports, err := net.SplitHostPort(s); err == nil {
			// Explicit port(s) — usually one, but "host:port,port,..." tries
			// each against the same host (e.g. a restrictive firewall known
			// to pass only a handful of specific ports), same shape as the
			// no-port expansion below just with an operator-chosen list
			// instead of the built-in one. lastErr keeps the single-port
			// case's error message identical to before (there's exactly one
			// candidate, so it's also the only error); for a multi-port
			// entry it's just a representative failure, logged only if every
			// candidate failed.
			any := false
			var lastErr error
			for _, p := range strings.Split(ports, ",") {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				ua, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, p))
				if err != nil {
					lastErr = err
					continue
				}
				ap := ua.AddrPort()
				add(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
				any = true
			}
			if !any {
				logx.Errorf("seed %q: %v", s, lastErr)
			}
			continue
		}
		// No port: expand across the standard port set.
		any := false
		for _, p := range standard {
			cand := net.JoinHostPort(s, strconv.Itoa(p))
			if ua, err := net.ResolveUDPAddr("udp", cand); err == nil {
				ap := ua.AddrPort()
				add(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
				any = true
			}
		}
		if !any {
			logx.Errorf("seed %q: could not resolve", s)
		}
	}
	return out
}

// resolveTCPSeeds turns "tcp://host[:port[,port,...]]" strings into underlay
// endpoints to be dialed over the TCP/TLS fallback directly. A seed without an
// explicit port uses the default fallback port; a comma-separated port list
// (same shape as the UDP side's own expansion) tries each port against the
// same host. Non-tcp seeds are ignored (handled by resolveSeeds).
func resolveTCPSeeds(seeds []string, tcpPort int, overlays []netip.Prefix) []netip.AddrPort {
	if tcpPort <= 0 {
		tcpPort = config.DefaultTCPFallbackPort
	}
	seen := map[netip.AddrPort]bool{}
	var out []netip.AddrPort
	// add resolves and stages a single "host:port" candidate — dedup and the
	// overlay-address guard live here so every caller (single explicit port,
	// each port in a comma list, or the no-port default) gets the same
	// treatment without repeating it.
	add := func(hostport, orig string) {
		ua, err := net.ResolveTCPAddr("tcp", hostport)
		if err != nil {
			logx.Errorf("tcp seed %q: %v", orig, err)
			return
		}
		ap := ua.AddrPort()
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if !ap.IsValid() || seen[ap] {
			return
		}
		for _, sub := range overlays {
			if sub.IsValid() && sub.Contains(ap.Addr()) {
				logx.Warnf("tcp seed resolves to overlay address %s (inside %s) — ignoring it", ap.Addr(), sub)
				return
			}
		}
		seen[ap] = true
		out = append(out, ap)
	}
	for _, s := range seeds {
		tr, hp := config.SeedParts(strings.TrimSpace(s))
		if tr != "tcp" || hp == "" {
			continue
		}
		host, ports, err := net.SplitHostPort(hp)
		if err != nil {
			add(net.JoinHostPort(hp, strconv.Itoa(tcpPort)), s) // no port → default fallback port
			continue
		}
		for _, p := range strings.Split(ports, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			add(net.JoinHostPort(host, p), s)
		}
	}
	return out
}

// prefixBits returns the prefix length of a CIDR string, or -1 if unparseable.
// Used to pin a self-assigned host address with its network's mask.
func prefixBits(cidr string) int {
	if cidr == "" {
		return -1
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return -1
	}
	return p.Bits()
}

// webPortOf extracts the web-admin port from the configured listen address, so
// managed peers can be reached over the overlay. 0 if unset/unparseable.
// engineFallbackPort is the TCP/TLS fallback port the engine assumes peers
// listen on (so it can dial them when UDP fails). Zero when the fallback is
// disabled, which turns off outbound fallback dialing.
func engineFallbackPort(cfg *config.Config) int {
	if !cfg.TCPFallbackEnabled() {
		return 0
	}
	return cfg.TCPFallbackPortValue()
}

// toUint16Ports converts config's []int port lists (validated 1-65535 by
// Config.Validate) to the []uint16 mesh.Options/Engine setters expect —
// ports fit uint16 by definition, this is just the storage type the wire
// format (appendPortList) already uses.
func toUint16Ports(ports []int) []uint16 {
	if len(ports) == 0 {
		return nil
	}
	out := make([]uint16, len(ports))
	for i, p := range ports {
		out[i] = uint16(p)
	}
	return out
}

func webPortOf(cfg *config.Config) uint16 {
	if cfg.WebAdmin.Listen == "" {
		return 0
	}
	_, ps, err := net.SplitHostPort(cfg.WebAdmin.Listen)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(ps)
	if err != nil || p < 1 || p > 65535 {
		return 0
	}
	return uint16(p)
}

// wildcardHost reports whether a listen address binds all interfaces (so it
// already covers the overlay address and needs no extra overlay listener).
func wildcardHost(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	return host == "" || host == "0.0.0.0" || host == "::"
}

func keyStrings(n config.Network) []string {
	out := make([]string, 0, len(n.Keys))
	now := time.Now()
	for _, k := range n.Keys {
		if k.Enabled && k.Key != "" && !k.Expired(now) {
			out = append(out, k.Key)
		}
	}
	return out
}

// keyLabelMap builds a derived-KeyID -> label lookup for a network's currently
// enabled, unexpired keys — the exact same filter as keyStrings, so the labels
// line up with whatever the engine can actually authenticate with. It's purely
// for the admin UI (showing which key label is authenticating a given peer's
// session); the engine itself never trusts or compares on labels. Malformed key
// material (shouldn't happen — Validate rejects it at save time) is skipped
// rather than failing the reload.
func keyLabelMap(n config.Network) map[crypto.KeyID]string {
	now := time.Now()
	out := make(map[crypto.KeyID]string, len(n.Keys))
	for _, k := range n.Keys {
		if !k.Enabled || k.Key == "" || k.Expired(now) {
			continue
		}
		raw, err := crypto.DecodeKey(k.Key)
		if err != nil {
			continue
		}
		out[crypto.DeriveKeyID(raw)] = k.Label
	}
	return out
}

// keyExpiryMap maps each enabled, non-expired key (by derived ID) to its
// config expiry (RFC3339, "" = never), the expiry counterpart to
// keyLabelMap. Used by the propagated-key reconciliation (see
// mesh.NetSpec.KeyExpires) to tell whether config has fully caught up with a
// mesh-learned key's current expiry — not consulted for expiry enforcement
// itself, which reads config directly.
func keyExpiryMap(n config.Network) map[crypto.KeyID]string {
	now := time.Now()
	out := make(map[crypto.KeyID]string, len(n.Keys))
	for _, k := range n.Keys {
		if !k.Enabled || k.Key == "" || k.Expired(now) {
			continue
		}
		raw, err := crypto.DecodeKey(k.Key)
		if err != nil {
			continue
		}
		out[crypto.DeriveKeyID(raw)] = k.Expires
	}
	return out
}

// expirySig is a fingerprint of every key that has an expiry, plus whether it's
// currently expired. It changes exactly when a key crosses its expiry boundary,
// so the daemon can detect that moment without reloading on every tick.
func expirySig(cfg *config.Config) string {
	var b strings.Builder
	now := time.Now()
	for _, n := range cfg.Networks {
		for i, k := range n.Keys {
			if k.Expires != "" {
				fmt.Fprintf(&b, "%s/%d=%t;", n.ID, i, k.Expired(now))
			}
		}
	}
	return b.String()
}

// writeInitialConfig scaffolds a base config with no networks — the user creates
// networks explicitly with `gravinet network add`.
func writeInitialConfig(path string) {
	if _, err := os.Stat(path); err == nil {
		fatal("refusing to overwrite existing config at %s", path)
	}
	if err := writeDefaultConfig(path); err != nil {
		fatal("write config: %v", err)
	}
	fmt.Printf("wrote config to %s\n", path)
	fmt.Printf("no networks yet — create one with:  gravinet network add NAME -config %s\n", path)
}

// writeDefaultConfig writes a fresh default config (new node id) to path,
// creating the parent directory. Used both by `run --init` and by an automatic
// first-start bootstrap so the service can come up before anything is configured.
func writeDefaultConfig(path string) error {
	c := config.Default()
	c.NodeID = randomNodeID()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return c.SaveTo(path)
}

func randomNodeID() string { return randHex(8) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		fatal("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "gravinet: "+format+"\n", a...)
	os.Exit(1)
}

// protoNumber maps a config protocol name to its IP protocol number (0 =
// any). Accepts the same tokens FirewallServicePort.Proto documents
// (tcp|udp|icmp|<number>|any) so a named service's legs and a QoS rule's own
// literal proto resolve identically.
func protoNumber(name string) uint8 {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "any":
		return 0
	case "tcp":
		return 6
	case "udp":
		return 17
	case "icmp":
		return 1
	default:
		if n, err := strconv.Atoi(strings.TrimSpace(name)); err == nil && n > 0 && n < 256 {
			return uint8(n)
		}
		return 0
	}
}

// qosLeg is one resolved protocol/port leg of a named service, as used by
// qosServiceCatalog/qosClassRules below — the QoS analogue of mesh's fwLeg.
type qosLeg struct {
	proto            uint8
	portMin, portMax uint16
}

// qosServiceCatalog resolves a node's named service catalog (the same
// Config.FirewallServices firewall rules resolve their own Services field
// against) into a lowercased-name -> legs lookup for qosClassRules. Mirrors
// mesh's unexported svcLegs (internal/mesh/firewall.go) since QoS rules and
// firewall rules both take the raw port_min/port_max==0 "any" convention
// from the same FirewallService/FirewallServicePort shape.
func qosServiceCatalog(svcs []config.FirewallService) map[string][]qosLeg {
	cat := make(map[string][]qosLeg, len(svcs))
	for _, s := range svcs {
		key := strings.ToLower(strings.TrimSpace(s.Name))
		if key == "" {
			continue
		}
		var legs []qosLeg
		for _, p := range s.Ports {
			lo := uint16(p.PortMin)
			hi := uint16(p.PortMax)
			if hi == 0 {
				hi = lo // a single port, or "any" when both are 0
			}
			legs = append(legs, qosLeg{proto: protoNumber(p.Proto), portMin: lo, portMax: hi})
		}
		cat[key] = legs
	}
	return cat
}

// qosClassRules maps a network's configured QoS rules into the engine's
// ClassRule form, skipping disabled rules so a paused rule stays in config but
// is never classified. Split out from fillRuntimeSpec so the disabled-skip is
// directly testable (the compiled classifier is opaque).
//
// A rule's literal Protocol/PortMin/PortMax and its named Services (resolved
// against fwServices) are unioned exactly like FirewallRule unions its own
// inline proto/port with its named services (see resolveLegs in
// internal/mesh/firewall.go): each leg becomes its own ClassRule sharing the
// rule's Class/DSCP, so traffic matching any of them lands in that class. A
// rule naming an unknown service logs a warning and contributes nothing for
// that name — the rest of the rule, if any, still applies — rather than
// silently falling back to "match everything", which would turn a typo or a
// since-deleted service into an unintended catch-all. A rule with neither a
// literal leg nor any Services matches everything, unchanged from before
// Services existed.
func qosClassRules(in []config.QoSRule, fwServices []config.FirewallService, netName string) []mesh.ClassRule {
	cat := qosServiceCatalog(fwServices)
	rules := make([]mesh.ClassRule, 0, len(in))
	for _, r := range in {
		if r.Disabled {
			continue
		}
		dscp := -1
		if r.DSCP != nil {
			dscp = *r.DSCP
		}
		added := 0
		hasInline := r.Protocol != "" && !strings.EqualFold(r.Protocol, "any")
		hasPorts := r.PortMin != 0 || r.PortMax != 0
		if hasInline || hasPorts {
			rules = append(rules, mesh.ClassRule{
				Proto: protoNumber(r.Protocol), PortMin: uint16(r.PortMin), PortMax: uint16(r.PortMax),
				DSCP: dscp, Class: r.Class,
			})
			added++
		}
		for _, name := range r.Services {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			legs, ok := cat[key]
			if !ok {
				logx.Errorf("network %s: qos rule references unknown service %q, skipping", netName, name)
				continue
			}
			for _, lg := range legs {
				rules = append(rules, mesh.ClassRule{
					Proto: lg.proto, PortMin: lg.portMin, PortMax: lg.portMax, DSCP: dscp, Class: r.Class,
				})
				added++
			}
		}
		if added == 0 && len(r.Services) == 0 {
			// No literal leg and no services named at all: the original
			// "match anything" catch-all, unchanged from before Services
			// existed.
			rules = append(rules, mesh.ClassRule{DSCP: dscp, Class: r.Class})
		}
	}
	return rules
}
