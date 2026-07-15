package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
	fmt.Fprint(os.Stderr, `gravinet upgrade — apply a new binary to this node

Build host (only if you want signed provenance; not required):
  gravinet upgrade genkey                       generate a release keypair
  gravinet upgrade sign -bin PATH -key B64      sign a built binary -> PATH.json

On this node:
  gravinet upgrade stage -bin PATH [-manifest P]  stage an artifact locally
  gravinet upgrade list                           artifacts staged here
  gravinet upgrade status                         this node's upgrade state
  gravinet upgrade apply -id ID                   apply a staged artifact
  gravinet upgrade rollback                       back out the last applied upgrade

Upgrades are local-only: nothing here can be triggered by a peer, Manager or
otherwise, from anywhere on the mesh, under any configuration. With no
upgrade.trusted_keys configured (the default), 'stage' accepts an unsigned
artifact; configure trusted_keys if you want the manifest signature checked.
The web admin's Info -> Upgrade page does the same thing with a file picker.
`)
}

func cmdUpgrade(args []string) {
	if len(args) == 0 {
		usageUpgrade()
		os.Exit(2)
	}
	switch args[0] {
	case "genkey":
		cmdUpgradeGenKey(args[1:])
	case "sign":
		cmdUpgradeSign(args[1:])
	case "stage":
		cmdUpgradeStage(args[1:])
	case "list":
		cmdUpgradeCtl(args[1:], "list", nil)
	case "status":
		cmdUpgradeCtl(args[1:], "status", nil)
	case "apply":
		cmdUpgradeApply(args[1:])
	case "rollback":
		cmdUpgradeCtl(args[1:], "rollback", nil)
	case "help", "-h", "--help":
		usageUpgrade()
	default:
		fmt.Fprintf(os.Stderr, "unknown upgrade command %q\n\n", args[0])
		usageUpgrade()
		os.Exit(2)
	}
}

func cmdUpgradeGenKey(args []string) {
	fs := flag.NewFlagSet("upgrade genkey", flag.ExitOnError)
	fs.Parse(args)
	pub, priv, err := upgrade.GenerateKey()
	if err != nil {
		fatal("genkey: %v", err)
	}
	fmt.Printf(`Release keypair generated.

  PUBLIC  (goes in every node's config, under upgrade.trusted_keys):
    %s

  PRIVATE (keep this off every mesh node — a build host or a laptop, ideally
  offline; anyone holding it can run code on every node that trusts it):
    %s

Add the public half on each node:
  upgrade: { "trusted_keys": ["%s"] }
`, pub, priv, pub)
}

func cmdUpgradeSign(args []string) {
	fs := flag.NewFlagSet("upgrade sign", flag.ExitOnError)
	bin := fs.String("bin", "", "path to the built gravinet binary")
	key := fs.String("key", "", "base64 private key from 'upgrade genkey' (or set GRAVINET_RELEASE_KEY)")
	ver := fs.String("version", "", "version string the binary reports (probed from the binary if empty)")
	goos := fs.String("os", "", "target GOOS (probed if empty)")
	arch := fs.String("arch", "", "target GOARCH (probed if empty)")
	notes := fs.String("notes", "", "release note recorded in the manifest")
	out := fs.String("out", "", "manifest path (default: <bin>.json)")
	fs.Parse(args)

	if *bin == "" {
		fatal("usage: gravinet upgrade sign -bin PATH -key BASE64 [-notes ...]")
	}
	k := *key
	if k == "" {
		k = os.Getenv("GRAVINET_RELEASE_KEY")
	}
	if k == "" {
		fatal("no signing key: pass -key or set GRAVINET_RELEASE_KEY")
	}
	priv, err := upgrade.ParsePrivateKey(k)
	if err != nil {
		fatal("%v", err)
	}

	v, o, a := *ver, *goos, *arch
	pam := false
	// Ask the binary what it is, rather than trusting flags. A manifest that
	// claims amd64 over an arm64 binary is exactly the mistake that takes a
	// fleet down, and here — on the build host, before anything is distributed —
	// is the cheapest possible place to catch it. This only works when signing a
	// binary for the host's own platform; a cross-built artifact needs the flags.
	if p, perr := upgrade.ProbeBinary(context.Background(), *bin); perr == nil {
		if v == "" {
			v = p.Version
		}
		if o == "" {
			o = p.OS
		}
		if a == "" {
			a = p.Arch
		}
		pam = p.PAM
		if (o != p.OS || a != p.Arch) && (*goos == "" || *arch == "") {
			fatal("the binary reports %s/%s; pass -os/-arch explicitly if that is wrong", p.OS, p.Arch)
		}
	} else if v == "" || o == "" || a == "" {
		fatal("could not probe %s (%v) — for a cross-built artifact pass -version, -os and -arch", *bin, perr)
	}

	m, err := upgrade.NewManifest(*bin, v, o, a, pam, *notes)
	if err != nil {
		fatal("%v", err)
	}
	if err := m.Sign(priv); err != nil {
		fatal("%v", err)
	}
	b, err := m.Bytes()
	if err != nil {
		fatal("%v", err)
	}
	path := *out
	if path == "" {
		path = *bin + ".json"
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("signed %s (%s, %s/%s, pam=%v, %d bytes)\n  manifest: %s\n  signer:   %s\n",
		m.ID(), m.Version, m.OS, m.Arch, m.PAM, m.Size, path, m.Signer)
}

// cmdUpgradeStage ingests an artifact into this node's store directly, without
// going through the daemon: the store is a directory and Ingest is atomic and
// self-verifying, so a root CLI writing into it races safely with a running
// daemon reading from it. This CLI path always takes a manifest (signed or
// not, per Store.Verify's trust policy — see internal/upgrade/store.go); the
// web admin's Upgrade page is the one with the no-manifest, auto-probed
// upload for a raw binary or a source archive.
func cmdUpgradeStage(args []string) {
	fs := flag.NewFlagSet("upgrade stage", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config file")
	bin := fs.String("bin", "", "path to the artifact")
	manPath := fs.String("manifest", "", "path to its signed manifest (default: <bin>.json)")
	fs.Parse(args)
	if *bin == "" {
		fatal("usage: gravinet upgrade stage -bin PATH [-manifest PATH]")
	}
	if *manPath == "" {
		*manPath = *bin + ".json"
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("config: %v", err)
	}
	mb, err := os.ReadFile(*manPath)
	if err != nil {
		fatal("manifest: %v", err)
	}
	m, err := upgrade.ParseManifest(mb)
	if err != nil {
		fatal("%v", err)
	}
	st, err := upgrade.NewStore(cfg.UpgradeStoreDir(), cfg.Upgrade.TrustedKeys)
	if err != nil {
		fatal("%v", err)
	}
	f, err := os.Open(*bin)
	if err != nil {
		fatal("%v", err)
	}
	defer f.Close()
	if err := st.Ingest(m, f); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("staged %s in %s\n", m.ID(), st.Dir())
}

// cmdUpgradeCtl runs a simple control-socket op and prints the reply.
func cmdUpgradeCtl(args []string, op string, body any) {
	fs := flag.NewFlagSet("upgrade "+op, flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	fs.Parse(args)
	printUpgradeReply(op, upgradeCall(*sock, op, body))
}

func cmdUpgradeApply(args []string) {
	fs := flag.NewFlagSet("upgrade apply", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	id := fs.String("id", "", "artifact id (see 'gravinet upgrade list')")
	dry := fs.Bool("dry-run", false, "preflight only: verify, probe and config-test the binary without swapping it")
	down := fs.Bool("allow-downgrade", false, "permit replacing this binary with an older version")
	pamDown := fs.Bool("allow-pam-downgrade", false, "permit replacing a PAM build with a non-PAM one")
	fs.Parse(args)
	if *id == "" {
		fatal("usage: gravinet upgrade apply -id VERSION-OS-ARCH [-dry-run]")
	}
	printUpgradeReply("apply", upgradeCall(*sock, "apply", map[string]any{
		"id": *id, "dry_run": *dry, "allow_downgrade": *down, "allow_pam_downgrade": *pamDown,
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

// upgradeSvc is the daemon's upgrade machinery: the store, the guard, the fleet
// handle, and the one in-flight rollout a manager may be running.
type upgradeSvc struct {
	cfgPath string
	store   *upgrade.Store
	guard   *upgrade.Guard
	engine  *mesh.Engine
	target  string
	version string
	pam     bool
	confirm int
	keep    int
	webPort int
}

// newUpgradeSvc builds the machinery, or returns nil if it failed to
// initialize (store directory couldn't be created, or this binary's own path
// couldn't be resolved) — a genuine setup failure, not a missing key: upgrades
// no longer need trusted_keys to be usable, only to be signature-checked.
func newUpgradeSvc(cfg *config.Config, cfgPath string, engine *mesh.Engine, webPort int) *upgradeSvc {
	st, err := upgrade.NewStore(cfg.UpgradeStoreDir(), cfg.Upgrade.TrustedKeys)
	if err != nil {
		logx.Errorf("upgrade: %v (upgrades disabled on this node)", err)
		return nil
	}
	target, err := upgrade.ResolveTarget("")
	if err != nil {
		logx.Errorf("upgrade: %v (upgrades disabled on this node)", err)
		return nil
	}
	return &upgradeSvc{
		cfgPath: cfgPath,
		store:   st,
		guard:   upgrade.NewGuard(st.Dir(), restartService, logx.Infof),
		engine:  engine,
		target:  target,
		version: version,
		pam:     webadmin.PAMCompiledIn,
		confirm: cfg.UpgradeConfirmSeconds(),
		keep:    cfg.UpgradeKeepArtifacts(),
		webPort: webPort,
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
		Store:          u.store,
		Guard:          u.guard,
		Target:         u.target,
		ConfigPath:     u.cfgPath,
		Version:        u.version,
		PAM:            u.pam,
		ConfirmSeconds: func() int { return u.confirm },
		KeepArtifacts:  func() int { return u.keep },
		Restart:        restartService,
		Peers:          u.peersConnected,
		Op:             u.controlOp,
	}
}

// controlOp dispatches a CLI `gravinet upgrade ...` command inside the daemon.
func (u *upgradeSvc) controlOp(op string, body []byte) ([]byte, error) {
	switch op {
	case "list":
		out := []map[string]any{}
		for _, m := range u.store.List() {
			// m.Signer is empty for an unsigned (local-only mode) artifact —
			// slicing it like a signed one's key would panic.
			signer := m.Signer
			if len(signer) > 16 {
				signer = signer[:16]
			}
			out = append(out, map[string]any{
				"id": m.ID(), "version": m.Version, "os": m.OS, "arch": m.Arch,
				"size": m.Size, "pam": m.PAM, "notes": m.Notes, "signer": signer,
			})
		}
		return json.Marshal(out)

	case "status":
		st := u.guard.Load()
		return json.Marshal(map[string]any{
			"version": u.version, "target": u.target, "store": u.store.Dir(),
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
			ID                string `json:"id"`
			DryRun            bool   `json:"dry_run"`
			AllowDowngrade    bool   `json:"allow_downgrade"`
			AllowPAMDowngrade bool   `json:"allow_pam_downgrade"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		m, ok := u.store.Lookup(req.ID)
		if !ok {
			return nil, fmt.Errorf("no artifact %q staged on this node (see 'gravinet upgrade list')", req.ID)
		}
		opts := upgrade.Options{
			Target: u.target, ConfigPath: u.cfgPath, RunningPAM: u.pam, RunningVersion: u.version,
			AllowDowngrade: req.AllowDowngrade, AllowPAMDowngrade: req.AllowPAMDowngrade,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if req.DryRun {
			bin, err := u.store.BinPath(m)
			if err != nil {
				return nil, err
			}
			p, err := upgrade.Preflight(ctx, m, bin, opts)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"dry_run": true, "would_apply": p.Version, "ok": true})
		}
		if st := u.guard.Load(); st.Phase == upgrade.PhasePending {
			return nil, fmt.Errorf("an upgrade (%s \u2192 %s) is already mid-trial on this node \u2014 wait for it to confirm or revert, or roll it back, before starting another", st.From, st.To)
		}
		if err := u.guard.Arm(upgrade.State{
			Target: u.target, ArtifactID: m.ID(), From: u.version, To: m.Version,
			PrePeers: u.peersConnected(), ConfirmSeconds: u.confirm,
		}); err != nil {
			return nil, err
		}
		if _, err := upgrade.Apply(ctx, u.store, m, opts); err != nil {
			_ = u.guard.Clear()
			return nil, err
		}
		u.store.GC(u.keep)
		go func() {
			time.Sleep(500 * time.Millisecond)
			if err := restartService(); err != nil {
				logx.Errorf("upgrade: binary replaced but restart failed: %v", err)
			}
		}()
		return json.Marshal(map[string]any{"ok": true, "applied": m.Version, "restarting": true})
	}
	return nil, fmt.Errorf("unknown upgrade op %q", op)
}
