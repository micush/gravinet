package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/control"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
	"gravinet/internal/service"
	"gravinet/internal/upgrade"
	"gravinet/internal/webadmin"
)

// ---------------------------------------------------------------------------
// selftest — the gate a candidate binary has to pass before it replaces anyone
// ---------------------------------------------------------------------------

// cmdSelfTest loads and validates a config and exits. It touches no interfaces,
// binds no sockets, and joins no mesh — it answers exactly one question, which
// is the one the upgrade preflight needs answered: *would this binary accept
// this node's config?*
//
// That question cannot be answered by any other means. The manifest cannot
// answer it (it describes a file, not a config), the version string cannot
// answer it, and the running binary certainly cannot — the whole point is that
// the new one may have tightened validation, renamed a field, or dropped a key
// the old one still writes. Without this, that regression is discovered by a
// daemon that has already replaced itself, on a node whose only management path
// is the mesh it is now failing to join. With it, it is discovered by a
// subprocess, before anything has been swapped, and reported as a sentence.
func cmdSelfTest(args []string) {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config file")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "selftest: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "selftest: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("gravinet %s: config %s loaded and valid (%d network(s))\n", version, *cfgPath, len(cfg.Networks))
}

// ---------------------------------------------------------------------------
// upgrade CLI
// ---------------------------------------------------------------------------

func usageUpgrade() {
	fmt.Fprint(os.Stderr, `gravinet upgrade — build a source archive and apply it to this node

  gravinet upgrade ARCHIVE   build a gravinet source archive (.tgz/.tar.gz/.zip)
                             with this node's Go toolchain and swap the result in
  gravinet upgrade status    this node's upgrade state
  gravinet upgrade rollback  back out the last applied upgrade

Flags for the archive form:
  -dry-run               build and preflight the result without swapping it in
  -allow-downgrade       permit replacing this binary with an older version
  -allow-pam-downgrade   permit replacing a PAM build with a non-PAM one
  -sock PATH             control socket path

Upgrades are always from source: gravinet publishes no prebuilt binary for any
platform, and building on the node that will run it is what lets one archive
serve a mesh of Linux, BSD, macOS and Windows nodes at once. A Go toolchain
must be present (the platform installers put one there).

Upgrades are local-only by default: nothing here can be triggered by a peer.
A node may opt in to accepting source pushed by a directly-connected Manager
with upgrade.accept_manager_upgrades — off unless you set it. The web admin's
System -> Upgrade page does the same thing with a file picker.
`)
}

// upgradeRoute decides what a "gravinet upgrade ..." invocation meant. It is
// split out from cmdUpgrade, which can only be exercised by dialing a control
// socket, so the one genuinely interesting decision here — verb or archive
// path? — can be tested on its own.
//
// It returns the verb to run and the arguments belonging to it. The empty verb
// means "no verb given, print usage".
func upgradeRoute(args []string) (verb string, rest []string) {
	if len(args) == 0 {
		return "", nil
	}
	switch m := expandVerb(args[0], v("status"), v("rollback"), v("help"), v("apply")); m {
	case "status", "rollback", "help":
		return m, args[1:]
	case "-h", "--help":
		return "help", args[1:]
	case "apply":
		// The old spelling, "upgrade apply -src ARCHIVE", still parses. It is
		// gone from every usage string and doc, but the install scripts and
		// anyone's runbook are not things this can see, and swallowing a
		// leading "apply" costs one switch arm.
		return "apply", args[1:]
	default:
		// Anything else is the archive itself: "gravinet upgrade ARCHIVE".
		// There is exactly one thing an upgrade takes, so requiring a verb and
		// a flag to say which of the one thing it is was pure ceremony.
		//
		// Note args, not args[1:] — nothing was consumed, because nothing was
		// a verb. A file whose name is a prefix of a verb resolves as the verb
		// ("status.tgz" is fine; a file literally named "st" is not), and
		// "./st" spells that path unambiguously. expandVerb already refuses to
		// treat anything with a leading dash as a verb, so a bare flag lands
		// here too and reaches the flag parser rather than being called
		// unknown.
		return "apply", args
	}
}

func cmdUpgrade(args []string) {
	verb, rest := upgradeRoute(args)
	switch verb {
	case "":
		usageUpgrade()
		os.Exit(2)
	case "status", "rollback":
		cmdUpgradeCtl(rest, verb, nil)
	case "help":
		usageUpgrade()
	default:
		cmdUpgradeApply(rest)
	}
}

// cmdUpgradeCtl runs a simple control-socket op and prints the reply.
func cmdUpgradeCtl(args []string, op string, body any) {
	fs := flag.NewFlagSet("upgrade "+op, flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	fs.Parse(args)
	printUpgradeReply(op, upgradeCall(*sock, op, body))
}

// upgradeArchiveArg resolves which archive an apply was asked for, given the
// positional arguments and whatever -src carried. Split out from
// cmdUpgradeApply for the same reason upgradeRoute is: it is the part with a
// rule in it, and the rest of that function is a control-socket call.
//
// A positional argument wins over -src. The two can only both appear if
// someone half-converted a script, and the new spelling is the one to honour;
// the deprecated flag losing quietly is better than a conflict error for a
// case where both values are almost certainly the same path anyway.
func upgradeArchiveArg(pos []string, srcFlag string) (string, error) {
	switch {
	case len(pos) > 1:
		return "", fmt.Errorf("one archive at a time; got %d (%s)", len(pos), strings.Join(pos, ", "))
	case len(pos) == 1:
		return pos[0], nil
	case srcFlag != "":
		return srcFlag, nil
	}
	return "", fmt.Errorf("usage: gravinet upgrade ARCHIVE.tgz [-dry-run]")
}

// cmdUpgradeApply hands the daemon a path to a source archive; the daemon
// extracts, builds, preflights and swaps. The build deliberately happens in the
// daemon rather than here, even though this CLI is root on the same box: that
// keeps one implementation of extract-build-preflight-apply behind the control
// socket, reached identically from the terminal and from the web admin, rather
// than two that can drift.
func cmdUpgradeApply(args []string) {
	pos, rest := splitPositionals(args, "sock", "src")
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	srcFlag := fs.String("src", "", "deprecated: pass the archive as an argument instead")
	dry := fs.Bool("dry-run", false, "build and preflight the result without swapping it in")
	down := fs.Bool("allow-downgrade", false, "permit replacing this binary with an older version")
	pamDown := fs.Bool("allow-pam-downgrade", false, "permit replacing a PAM build with a non-PAM one")
	fs.Parse(rest)

	src, err := upgradeArchiveArg(pos, *srcFlag)
	if err != nil {
		fatal("%v", err)
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		fatal("%v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		// The archive is now positional, so a typo'd *verb* lands here rather
		// than in an "unknown command" arm. Say what the alternatives were, or
		// "gravinet upgrade statsu" reports a missing file and nothing else.
		fatal("%v\n(gravinet upgrade ARCHIVE.tgz | status | rollback)", err)
	}
	fmt.Printf("building %s (this runs a full 'go build'; a minute or two is normal)...\n", abs)
	printUpgradeReply("apply", upgradeCall(*sock, "apply", map[string]any{
		"src_path": abs, "dry_run": *dry, "allow_downgrade": *down, "allow_pam_downgrade": *pamDown,
	}))
}

func upgradeCall(sock, op string, body any) control.Response {
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			fatal("%v", err)
		}
		raw = b
	}
	resp, err := control.Do(sock, control.Request{Cmd: "upgrade", UpOp: op, UpBody: raw})
	if err != nil {
		fatal("control socket: %v", err)
	}
	return resp
}

func printUpgradeReply(op string, resp control.Response) {
	if resp.Error != "" {
		fatal("%s", resp.Error)
	}
	if len(resp.UpBody) == 0 {
		fmt.Println("ok")
		return
	}
	var pretty any
	if err := json.Unmarshal(resp.UpBody, &pretty); err == nil {
		b, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(b))
		return
	}
	fmt.Println(string(resp.UpBody))
}

// ---------------------------------------------------------------------------
// daemon side
// ---------------------------------------------------------------------------

// upgradeSvc is the daemon's upgrade machinery: the guard, the mesh handle it
// asks about peer health, and the paths an upgrade needs.
type upgradeSvc struct {
	cfgPath  string
	stateDir string
	guard    *upgrade.Guard
	engine   *mesh.Engine
	target   string
	version  string
	pam      bool
	confirm  int
	webPort  int
}

// newUpgradeSvc builds the machinery, or returns nil if it failed to
// initialize (the state directory couldn't be created, or this binary's own
// path couldn't be resolved) — genuine setup failures. Nothing else is
// required: there is no key to configure and nothing to stage.
//
// The state directory is created 0700 and doubles as the spool for uploaded
// archives and the root for build temp dirs, so it must be a directory only
// root can write to: everything under it is either a record the node trusts to
// rescue itself or source it is about to compile and run.
func newUpgradeSvc(cfg *config.Config, cfgPath string, engine *mesh.Engine, webPort int) *upgradeSvc {
	dir := cfg.UpgradeStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logx.Errorf("upgrade: creating %s: %v (upgrades disabled on this node)", dir, err)
		return nil
	}
	target, err := upgrade.ResolveTarget("")
	if err != nil {
		logx.Errorf("upgrade: %v (upgrades disabled on this node)", err)
		return nil
	}
	return &upgradeSvc{
		cfgPath:  cfgPath,
		stateDir: dir,
		guard:    upgrade.NewGuard(dir, restartService, logx.Infof),
		engine:   engine,
		target:   target,
		version:  version,
		pam:      webadmin.PAMCompiledIn,
		confirm:  cfg.UpgradeConfirmSeconds(),
		webPort:  webPort,
	}
}

// restartService puts a swapped-in (or restored) binary into service through the
// same primitive the web admin's Restart button and the CLI use.
func restartService() error {
	if ok, hint := service.Restart(); !ok {
		return fmt.Errorf("%s", hint)
	}
	return nil
}

// peersConnected counts sessions across every network. This is the number the
// guard snapshots before a swap and holds the new binary to afterwards: a node
// that had peers and now has none has not started successfully, however healthy
// its process looks from the inside.
func (u *upgradeSvc) peersConnected() int {
	n := 0
	for _, id := range u.engine.NetworkIDs() {
		n += len(u.engine.ListPeers(id))
	}
	return n
}

// healthy is the guard's health check, passed as a callback to guard.Watch at
// boot (see main.go) so a freshly-swapped binary that never gets its peers
// back reverts itself without anyone needing to notice or intervene. "The
// mesh came back" is the only definition worth using here — every cheaper one
// (the process is running, the port is bound, the config parsed) is satisfied
// by precisely the binary this exists to catch.
func (u *upgradeSvc) healthy() (bool, string) {
	st := u.guard.Load()
	have := u.peersConnected()
	if st.PrePeers > 0 && have == 0 {
		return false, fmt.Sprintf("0 of %d peers reconnected after the upgrade", st.PrePeers)
	}
	return true, ""
}

func (u *upgradeSvc) webadminCtl() *webadmin.UpgradeCtl {
	return &webadmin.UpgradeCtl{
		Guard:          u.guard,
		StateDir:       u.stateDir,
		Target:         u.target,
		ConfigPath:     u.cfgPath,
		Version:        u.version,
		PAM:            u.pam,
		ConfirmSeconds: func() int { return u.confirm },
		Restart:        restartService,
		Peers:          u.peersConnected,
		Op:             u.controlOp,
		// Read the opt-in fresh from the config file each time rather than from
		// the value captured at startup: this endpoint is hit rarely (only when
		// a Manager actually pushes), so the cost is irrelevant, and it means
		// toggling accept_manager_upgrades in the web admin takes effect
		// immediately on the next push without waiting for a daemon restart. A
		// read error fails closed (false) — the safe default for a switch that
		// authorizes running binaries.
		AcceptManagerUpgrades: func() bool {
			cfg, err := config.Load(u.cfgPath)
			if err != nil {
				return false
			}
			return cfg.Upgrade.AcceptManagerUpgrades
		},
	}
}

// controlOp dispatches a `gravinet upgrade ...` command inside the daemon. The
// web admin reaches the same switch through UpgradeCtl.Op, so the terminal and
// the browser drive one implementation rather than two.
func (u *upgradeSvc) controlOp(op string, body []byte) ([]byte, error) {
	switch op {
	case "status":
		st := u.guard.Load()
		return json.Marshal(map[string]any{
			"version": u.version, "target": u.target, "state_dir": u.stateDir,
			"phase": st.Phase, "from": st.From, "to": st.To, "boots": st.Boots,
			"pre_peers": st.PrePeers, "last_error": st.LastError,
			"peers_now": u.peersConnected(),
			"rollback_available": func() bool {
				_, err := os.Stat(upgrade.BackupPath(u.target))
				return err == nil
			}(),
		})

	case "rollback":
		if err := u.guard.Rollback(); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"ok": true, "restarting": true})

	case "apply":
		var req struct {
			SrcPath           string `json:"src_path"`
			DryRun            bool   `json:"dry_run"`
			AllowDowngrade    bool   `json:"allow_downgrade"`
			AllowPAMDowngrade bool   `json:"allow_pam_downgrade"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		if req.SrcPath == "" {
			return nil, fmt.Errorf("no source archive given")
		}
		f, err := os.Open(req.SrcPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		// Refuse a second apply *before* spending ten minutes compiling for
		// one. The check is repeated after the build too (the window is wide
		// enough for another upgrade to start inside it), but failing here
		// turns a long wait followed by a rejection into an immediate answer.
		if st := u.guard.Load(); st.Phase == upgrade.PhasePending && !req.DryRun {
			return nil, fmt.Errorf("an upgrade (%s \u2192 %s) is already mid-trial on this node \u2014 wait for it to confirm or revert, or roll it back, before starting another", st.From, st.To)
		}

		// Read the candidate's baked-in version before spending a `go build`
		// on it (see upgrade.ExtractedVersion's own comment): if it matches
		// what is already running, applying it would rebuild and restart
		// into an identical binary for nothing. This extraction is cheap —
		// no compile — so it costs a fraction of a second regardless of how
		// long the eventual build would have taken.
		//
		// A miss (corrupt archive, an unreadable version line) comes back as
		// "", deliberately not an error, so it falls straight through to
		// Build below, which extracts the very same archive again and
		// reports the real problem through its own, already-tested error
		// path rather than this needing a second one.
		if srcVersion := upgrade.ExtractedVersion(f, u.stateDir); srcVersion != "" && srcVersion == u.version {
			logx.Infof("upgrade: uploaded archive is already-running version %s \u2014 skipping build and apply", u.version)
			if req.DryRun {
				return json.Marshal(map[string]any{
					"dry_run": true, "ok": true, "skipped": true, "already_on": u.version,
				})
			}
			return json.Marshal(map[string]any{"ok": true, "skipped": true, "already_on": u.version})
		}
		// ExtractedVersion consumed f reading the archive once already;
		// Build needs it again from the start.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewinding the uploaded archive: %w", err)
		}

		// The build gets its own generous timeout: it is a full `go build`,
		// potentially on a small box, and the preflight and swap that follow
		// are quick by comparison.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()

		bin, moduleRoot, probe, cleanup, err := upgrade.Build(ctx, f, u.stateDir)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		opts := upgrade.Options{
			Target: u.target, ConfigPath: u.cfgPath, RunningPAM: u.pam, RunningVersion: u.version,
			AllowDowngrade: req.AllowDowngrade, AllowPAMDowngrade: req.AllowPAMDowngrade,
		}
		if req.DryRun {
			if _, err := upgrade.Preflight(ctx, bin, opts); err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{
				"dry_run": true, "would_apply": probe.Version, "pam": probe.PAM, "ok": true,
			})
		}
		if st := u.guard.Load(); st.Phase == upgrade.PhasePending {
			return nil, fmt.Errorf("an upgrade (%s \u2192 %s) started while this one was building \u2014 wait for it to confirm or revert before starting another", st.From, st.To)
		}
		if err := u.guard.Arm(upgrade.State{
			Target: u.target, From: u.version, To: probe.Version,
			PrePeers: u.peersConnected(), ConfirmSeconds: u.confirm,
		}); err != nil {
			return nil, err
		}
		if _, err := upgrade.Apply(ctx, bin, opts); err != nil {
			_ = u.guard.Clear()
			return nil, err
		}
		// Refresh README/LICENSE/getting-started.md/API.md from the source
		// tree that just got compiled, so an in-place upgrade keeps the
		// installed docs current too — not just the binary. Must happen here,
		// before the deferred cleanup() above removes moduleRoot; a doc-sync
		// failure is logged but never fails the upgrade itself, which has
		// already succeeded by this point.
		if synced := upgrade.SyncInstalledDocs(moduleRoot, u.target); len(synced) > 0 {
			logx.Infof("upgrade: refreshed installed docs: %v", synced)
		}
		go func() {
			time.Sleep(500 * time.Millisecond)
			if err := restartService(); err != nil {
				logx.Errorf("upgrade: binary replaced but restart failed: %v", err)
			}
		}()
		return json.Marshal(map[string]any{"ok": true, "applied": probe.Version, "restarting": true})
	}
	return nil, fmt.Errorf("unknown upgrade op %q", op)
}
