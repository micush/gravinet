package webadmin

// FRR (Free Range Routing) management: gravinet owns the BGP/BFD configuration
// and the FRR daemon lifecycle, and lets FRR run the actual sessions. This is a
// port of parapet's frr.rs, narrowed to the BGP + BFD surface: gravinet does
// not supervise FRR as a child process (FRR is a multi-daemon suite, better
// driven through its config) — instead it renders an integrated frr.conf,
// makes sure the daemons BGP needs are enabled in /etc/frr/daemons, and
// reloads FRR. The same "generate a config and apply it" shape gravinet already
// uses elsewhere.
//
// Rendering is a pure function (unit-tested). Applying writes the files and
// reloads FRR; if FRR isn't installed the apply is a logged no-op, so a routing
// edit can never take down the node — the config still persists to gravinet's
// own config.json, it just isn't pushed to a daemon that isn't there.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

const (
	frrDir     = "/etc/frr"
	frrConf    = "/etc/frr/frr.conf"
	frrDaemons = "/etc/frr/daemons"
)

// frrManagedDaemons is the set of FRR daemons this module owns the on/off state
// of in /etc/frr/daemons. Any daemon here that a config doesn't need is forced
// =no so a previously-enabled protocol is actually turned off, not left
// running; daemons outside this set are left exactly as the operator/package
// left them. Ported verbatim from parapet's managed list.
var frrManagedDaemons = []string{
	"bgpd", "ospfd", "ospf6d", "ripd", "ripngd", "isisd", "pimd",
	"ldpd", "nhrpd", "eigrpd", "babeld", "sharpd", "pbrd", "bfdd",
	"fabricd", "vrrpd", "pathd", "staticd",
}

// frrInstalled reports whether FRR is present on this host (its config
// directory exists). Apply is a no-op when it isn't.
func frrInstalled() bool {
	fi, err := statFile(frrDir)
	return err == nil && fi.IsDir()
}

// safeToken reports whether a token is safe to splice into an frr.conf line:
// non-empty, bounded, and built only from characters that can't break out of
// the intended line (no whitespace, no shell/comment metacharacters). This is
// the injection guard every user-supplied value passes through before it's
// emitted. Ported from parapet's safe_token.
func safeToken(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	for _, b := range []byte(t) {
		ok := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
			b == '.' || b == ':' || b == '/' || b == '-' || b == '_'
		if !ok {
			return false
		}
	}
	return true
}

// filterInline strips characters that could break out of an frr.conf line
// (newlines, carriage returns) and any other control/whitespace, then caps the
// length — used for free-text fields (description, password) that aren't
// safeToken-constrained but still must stay on one line. Mirrors parapet's
// per-field char filters.
func filterInline(s string, max int, dropAllWhitespace bool) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' {
			continue
		}
		if dropAllWhitespace && (r == ' ' || r == '\t') {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}

// renderFRR renders the integrated frr.conf for this node's BGP config. Pure
// function — no I/O — so it's exhaustively unit-testable, exactly as parapet's
// render() is. Only the BGP block is emitted (plus FRR's boilerplate header);
// gravinet's port covers BGP and its attached BFD, not OSPF/policy routing.
func renderFRR(b config.BGPConfig) string {
	var out strings.Builder
	out.WriteString("! gravinet-managed FRR configuration. Do not edit by hand.\n")
	out.WriteString("frr defaults traditional\n!\n")

	if !b.Enabled || b.ASN == 0 {
		return out.String()
	}

	fmt.Fprintf(&out, "router bgp %d\n", b.ASN)
	if safeToken(b.RouterID) {
		fmt.Fprintf(&out, " bgp router-id %s\n", b.RouterID)
	}
	for _, n := range b.Neighbors {
		if !safeToken(n.Peer) || n.RemoteAS == 0 {
			continue
		}
		fmt.Fprintf(&out, " neighbor %s remote-as %d\n", n.Peer, n.RemoteAS)
		if d := filterInline(strings.TrimSpace(n.Description), 60, false); d != "" {
			fmt.Fprintf(&out, " neighbor %s description %s\n", n.Peer, d)
		}
		if n.Password != "" {
			// Only emit a password with no whitespace — safe inline in the conf.
			if pw := filterInline(n.Password, 80, true); pw != "" {
				fmt.Fprintf(&out, " neighbor %s password %s\n", n.Peer, pw)
			}
		}
		// Per-neighbor BFD, also implied by the global BGP BFD toggle.
		if n.BFD || b.BFD {
			fmt.Fprintf(&out, " neighbor %s bfd\n", n.Peer)
		}
	}
	out.WriteString(" address-family ipv4 unicast\n")
	for _, net := range b.Networks {
		if safeToken(net) {
			fmt.Fprintf(&out, "  network %s\n", net)
		}
	}
	if b.RedistributeConnected {
		out.WriteString("  redistribute connected\n")
	}
	if b.RedistributeStatic {
		out.WriteString("  redistribute static\n")
	}
	for _, n := range b.Neighbors {
		if safeToken(n.Peer) && n.RemoteAS != 0 {
			fmt.Fprintf(&out, "  neighbor %s activate\n", n.Peer)
		}
	}
	out.WriteString(" exit-address-family\n!\n")
	return out.String()
}

// neededDaemons is which FRR daemons this config requires. staticd is always
// on (FRR's general-purpose daemon); bgpd whenever BGP is enabled; bfdd
// whenever BFD is on globally or for any neighbor. Ported from parapet's
// needed_daemons (BGP/BFD subset).
func neededDaemons(b config.BGPConfig) []string {
	d := []string{"staticd"}
	if b.Enabled && b.ASN != 0 {
		d = append(d, "bgpd")
		bfd := b.BFD
		if !bfd {
			for _, n := range b.Neighbors {
				if n.BFD {
					bfd = true
					break
				}
			}
		}
		if bfd {
			d = append(d, "bfdd")
		}
	}
	return d
}

func daemonWanted(want []string, name string) bool {
	for _, w := range want {
		if w == name {
			return true
		}
	}
	return false
}

// syncDaemonsContent rewrites the body of /etc/frr/daemons so every daemon in
// frrManagedDaemons is =yes when wanted and =no otherwise, leaving all other
// lines untouched. Pure string transform (returns the new body and whether it
// changed) so the reconciliation logic is unit-testable without touching disk.
// Ported from parapet's sync_daemons.
func syncDaemonsContent(existing string, want []string) (string, bool) {
	changed := false
	lines := strings.Split(existing, "\n")
	for i, line := range lines {
		for _, d := range frrManagedDaemons {
			on := d + "=yes"
			off := d + "=no"
			if strings.HasPrefix(line, on) || strings.HasPrefix(line, off) {
				desired := off
				if daemonWanted(want, d) {
					desired = on
				}
				if line != desired {
					changed = true
				}
				lines[i] = desired
				break
			}
		}
	}
	return strings.Join(lines, "\n"), changed
}

// syncDaemons applies syncDaemonsContent to the real /etc/frr/daemons file.
// Returns whether the file changed. An empty/missing file means FRR isn't
// installed the way we expect and is a hard error (the caller has already
// checked frrInstalled, so this is a belt-and-suspenders guard).
func syncDaemons(b config.BGPConfig) (bool, error) {
	raw, err := os.ReadFile(frrDaemons)
	if err != nil || len(raw) == 0 {
		return false, fmt.Errorf("%s not found (is FRR installed?)", frrDaemons)
	}
	body, changed := syncDaemonsContent(string(raw), neededDaemons(b))
	if changed {
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if err := writeAtomicFile(frrDaemons, body); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// writeAtomicFile writes body to path via a temp file + rename, so a reader
// never sees a half-written config. Ported from parapet's write_atomic.
func writeAtomicFile(path, body string) error {
	tmp := path + ".gravinet.tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// applyBGP reconciles FRR with the desired BGP config: render frr.conf, sync
// the daemon set, and restart or reload FRR so the change takes effect. It
// returns a human note describing what it did. When FRR isn't installed it's a
// no-op with an explanatory note (never an error that would block the config
// save). The actual systemctl call is dispatched to a background goroutine —
// an frr restart can take many seconds, and the HTTP handler shouldn't block on
// it — after the config has been written synchronously (so the save is durable
// and the API can return success immediately). Ported from parapet's
// apply_routing.
func applyBGP(b config.BGPConfig, log *logx.Logger) (string, error) {
	if !frrInstalled() {
		return "FRR is not installed; BGP config saved but not applied", nil
	}
	if err := writeAtomicFile(frrConf, renderFRR(b)); err != nil {
		return "", err
	}
	wanted := neededDaemons(b)
	daemonsFileChanged, err := syncDaemons(b)
	if err != nil {
		return "", err
	}
	// A daemon can be "wanted" (=yes in /etc/frr/daemons) without actually
	// being alive — e.g. an earlier restart failed partway, or it crash-looped
	// on bad config. In that case syncDaemons sees no file change and would
	// wrongly take the reload path forever, which can't start a stopped daemon.
	// Treat "wanted but not alive" the same as "file changed" so it gets a real
	// restart.
	needsStart := false
	for _, d := range wanted {
		if !daemonAlive(d) {
			needsStart = true
			break
		}
	}
	daemonsChanged := daemonsFileChanged || needsStart
	daemonCount := len(wanted)

	go func() {
		action := "reload"
		if daemonsChanged {
			action = "restart"
		}
		ok := runSystemctl(action)
		// A restart failure when a daemon needs to transition (e.g. bgpd just
		// got enabled) has no vtysh safety net (vtysh -b can only push config
		// into an already-running daemon, not spawn one), but such failures are
		// often transient (FRR startup races, a queued systemd job); retry once.
		if daemonsChanged && !ok {
			time.Sleep(3 * time.Second)
			ok = runSystemctl("restart")
		}
		switch {
		case ok:
			log.Infof("bgp: routing applied: %d daemon(s), frr %sed", daemonCount, action)
		case !daemonsChanged && vtyshApplyBoot():
			// vtysh -b only pushes config into already-running daemons — valid
			// only when no daemon needed to change state (a pure config reload).
			log.Infof("bgp: routing applied via vtysh -b")
		default:
			tried := " and vtysh"
			if daemonsChanged {
				tried = " (twice)"
			}
			log.Warnf("bgp: config written but could not %s FRR (tried systemctl%s)", action, tried)
		}
	}()

	return fmt.Sprintf("%d daemon(s) configured, reloading FRR in background", daemonCount), nil
}

// daemonAlive reports whether an FRR daemon is actually running, via its pid
// file at /var/run/frr/<daemon>.pid plus a live /proc/<pid>. This is more
// reliable than what /etc/frr/daemons merely says should be running. Ported
// from parapet's daemon_alive. On systems without /proc (non-Linux) the /proc
// check simply fails, so this returns false and applyBGP falls back to a full
// restart rather than a reload — the safe direction.
func daemonAlive(name string) bool {
	raw, err := os.ReadFile("/var/run/frr/" + name + ".pid")
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return false
	}
	fi, err := statFile("/proc/" + strconv.Itoa(pid))
	return err == nil && fi.IsDir()
}

// runSystemctl runs `systemctl <action> frr`, wrapped in `timeout` so a stuck
// systemd job can't hang the background goroutine forever. Falls back to a
// direct systemctl call if `timeout` itself is missing. Ported from parapet's
// run_systemctl.
func runSystemctl(action string) bool {
	if err := exec.Command("timeout", "45", "systemctl", action, "frr").Run(); err == nil {
		return true
	} else if _, lookErr := exec.LookPath("timeout"); lookErr == nil {
		// timeout ran but the job failed/timed out.
		return false
	}
	// timeout binary missing — direct call.
	return exec.Command("systemctl", action, "frr").Run() == nil
}

// vtyshApplyBoot runs `vtysh -b`, pushing the on-disk frr.conf into the running
// daemons' VTYs. Only valid when every needed daemon is already up (it can't
// spawn one). Ported from parapet's vtysh_apply.
func vtyshApplyBoot() bool {
	bin, ok := vtyshPath()
	if !ok {
		bin = "vtysh"
	}
	return exec.Command(bin, "-b").Run() == nil
}

// importBGPFromFRR reads the BGP configuration FRR is *actually running* and
// reshapes it into a config.BGPConfig, so gravinet can reflect a pre-existing
// setup (one configured outside gravinet — a hand-edited frr.conf, another
// tool, or a prior install) instead of showing an empty editor while live peers
// are established. It asks vtysh for the running-config and parses the
// `router bgp` stanza. Returns ok=false when vtysh is absent, the query fails,
// or there's no BGP stanza. Passwords are deliberately not imported (FRR may
// hold them encrypted, and re-emitting them verbatim on a later save would
// corrupt the session) — hasPasswords reports whether any neighbor had one so
// the UI can warn.
//
// This is read-only: importing never writes frr.conf or config.json. gravinet
// only takes ownership when the operator explicitly saves (adopts) the config.
func importBGPFromFRR() (cfg config.BGPConfig, hasPasswords bool, ok bool) {
	bin, present := vtyshPath()
	if !present {
		return config.BGPConfig{}, false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), bgpQueryTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-c", "show running-config").Output()
	if err != nil {
		return config.BGPConfig{}, false, false
	}
	return parseRunningConfigBGP(string(out))
}

// parseRunningConfigBGP extracts the BGP config from FRR `show running-config`
// text. Pure function (no I/O) so it's fully unit-testable. It reads the single
// `router bgp <asn>` stanza — router-id, per-neighbor remote-as/description/bfd,
// and the address-family's networks and redistribute directives — mirroring
// exactly the surface renderFRR emits, so what's imported round-trips through
// the same fields. Neighbor order is preserved (first appearance). hasPasswords
// is set if any neighbor carried a `password` line, though the secret itself is
// not captured.
func parseRunningConfigBGP(text string) (cfg config.BGPConfig, hasPasswords bool, ok bool) {
	lines := strings.Split(text, "\n")
	inStanza := false
	// Preserve neighbor first-seen order while allowing lookup by peer.
	idx := map[string]int{}
	getN := func(peer string) *config.BGPNeighbor {
		if i, seen := idx[peer]; seen {
			return &cfg.Neighbors[i]
		}
		cfg.Neighbors = append(cfg.Neighbors, config.BGPNeighbor{Peer: peer})
		idx[peer] = len(cfg.Neighbors) - 1
		return &cfg.Neighbors[len(cfg.Neighbors)-1]
	}

	for _, raw := range lines {
		if !inStanza {
			// Stanza opens at a column-0 `router bgp <asn>` line. FRR may append
			// `vrf <name>`; only the default VRF is imported (fields after the
			// ASN are ignored).
			f := strings.Fields(raw)
			if len(f) >= 3 && f[0] == "router" && f[1] == "bgp" {
				if asn, err := strconv.ParseUint(f[2], 10, 32); err == nil {
					cfg.Enabled = true
					cfg.ASN = uint32(asn)
					ok = true
					inStanza = true
				}
			}
			continue
		}
		// Inside the stanza: a non-indented, non-empty line ends it (the next
		// top-level stanza, or a column-0 `exit`).
		if raw != "" && !strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "\t") {
			break
		}
		line := strings.TrimSpace(raw)
		if line == "" || line == "!" || line == "exit" || line == "exit-address-family" {
			continue
		}
		if strings.HasPrefix(line, "no ") { // negations — skip
			continue
		}
		switch {
		case strings.HasPrefix(line, "bgp router-id "):
			cfg.RouterID = strings.TrimSpace(strings.TrimPrefix(line, "bgp router-id "))
		case strings.HasPrefix(line, "neighbor "):
			rest := strings.TrimSpace(strings.TrimPrefix(line, "neighbor "))
			f := strings.Fields(rest)
			if len(f) < 2 {
				continue
			}
			peer, verb := f[0], f[1]
			switch verb {
			case "remote-as":
				if len(f) >= 3 {
					if as, err := strconv.ParseUint(f[2], 10, 32); err == nil {
						getN(peer).RemoteAS = uint32(as)
					}
				}
			case "description":
				getN(peer).Description = strings.TrimSpace(strings.TrimPrefix(rest, f[0]+" description "))
			case "password":
				// Presence noted; secret not captured (see importBGPFromFRR).
				_ = getN(peer)
				hasPasswords = true
			case "bfd":
				getN(peer).BFD = true
			case "activate":
				// address-family activation — neighbor already captured.
				_ = getN(peer)
			}
		case strings.HasPrefix(line, "network "):
			cfg.Networks = append(cfg.Networks, strings.TrimSpace(strings.TrimPrefix(line, "network ")))
		case line == "redistribute connected":
			cfg.RedistributeConnected = true
		case line == "redistribute static":
			cfg.RedistributeStatic = true
		}
	}
	return cfg, hasPasswords, ok
}
