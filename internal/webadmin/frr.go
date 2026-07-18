package webadmin

// FRR (Free Range Routing) management: gravinet owns the BGP/BFD configuration
// and the FRR daemon lifecycle, and lets FRR run the actual sessions. This is a
// port of parapet's frr.rs, narrowed to the BGP + BFD surface: gravinet does
// not supervise FRR as a child process (FRR is a multi-daemon suite, better
// driven through its config) — instead it renders an integrated frr.conf,
// makes sure the daemons BGP needs are enabled, and reloads FRR. The same
// "generate a config and apply it" shape gravinet already uses elsewhere.
//
// Rendering (this file) is OS-agnostic and pure (unit-tested): FRR's config
// language and vtysh's JSON output don't vary by platform. Applying it does
// vary — Linux's package puts config under /etc/frr, gates daemons through a
// single /etc/frr/daemons file, and is driven via systemctl; FreeBSD's puts
// config under /usr/local/etc/frr, gates daemons through rc.conf's
// frr_daemons variable, and is driven via service(8). frr_default.go
// (everything except FreeBSD — Linux is the only platform any of this has
// ever really run on, but Windows/macOS/OpenBSD harmlessly share its
// definitions since vtysh is never found there anyway) and frr_freebsd.go
// hold that split; this file calls into it through frrDir/frrConf (each a
// const, but a different value per file — the build tag picks which one
// actually compiles in) and a handful of identically-named functions
// (syncDaemons, applyFRRService, ensureFRRDaemonsEnabled, managedDaemonSet)
// each side implements. Applying writes the files and reloads FRR; if FRR
// isn't installed the apply is a logged no-op, so a routing edit can never
// take down the node — the config still persists to gravinet's own
// config.json, it just isn't pushed to a daemon that isn't there.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// frrManagedDaemons is the set of FRR daemons this module owns the on/off state
// of. Any daemon here that a config doesn't need is forced off so a
// previously-enabled protocol is actually turned off, not left running;
// daemons outside this set are left exactly as the operator/package left
// them. Ported verbatim from parapet's managed list. This is the OS-agnostic
// logical set — FreeBSD additionally always needs mgmtd and zebra listed
// first regardless of this list (see freebsdNeededDaemons in
// frr_freebsd.go); Linux's package already defaults those two to on and
// deliberately isn't asked to manage them here, for parity with how it
// always has.
var frrManagedDaemons = []string{
	"bgpd", "ospfd", "ospf6d", "ripd", "ripngd", "isisd", "pimd",
	"ldpd", "nhrpd", "eigrpd", "babeld", "sharpd", "pbrd", "bfdd",
	"fabricd", "vrrpd", "pathd", "staticd",
}

// frrInstalled reports whether FRR is present on this host (its config
// directory exists — frrDir is set per-OS in frr_default.go/frr_freebsd.go).
// Apply is a no-op when it isn't.
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

// isIPv6Peer reports whether a neighbor's peer address is an IPv6 literal
// (vs. IPv4 or something unparsable, e.g. an interface name for an
// unnumbered peer). FRR — like Cisco, which it mirrors here — activates
// every configured neighbor under `address-family ipv4 unicast` by default,
// regardless of the peer's own address family: an IPv6-addressed neighbor
// gets swept into that same implicit activation unless it's explicitly
// deactivated there. Left alone, that's a peer with an IPv6 transport
// session negotiating the IPv4 unicast AFI/SAFI, which is not what an
// operator adding a v6 neighbor wants.
func isIPv6Peer(peer string) bool {
	ip := net.ParseIP(peer)
	return ip != nil && ip.To4() == nil
}

// isIPv6Network reports whether an advertised-network prefix (e.g.
// "10.0.0.0/24" or "fd00::/64" — a bare address, sans "/len", also works)
// is IPv6. Determines which address-family block a `network` statement
// belongs under: FRR rejects a prefix whose family doesn't match the AF
// it's declared in.
func isIPv6Network(prefix string) bool {
	host := prefix
	if i := strings.IndexByte(prefix, '/'); i >= 0 {
		host = prefix[:i]
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
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
	// Global session-level knobs gravinet always applies, regardless of what's
	// configured above: log-neighbor-changes (visibility into session
	// state transitions in the log), no ebgp-requires-policy (modern FRR
	// defaults to rejecting eBGP routes with no inbound/outbound route-map
	// attached; gravinet doesn't manage route-maps, so a neighbor configured
	// here would otherwise exchange no routes at all), deterministic-med
	// (consistent best-path selection across peers from the same AS,
	// independent of the order routes arrived in), bestpath as-path
	// multipath-relax (allows ECMP across paths with equal-length but
	// different AS-paths — otherwise multipath is limited to
	// literally-identical AS-paths), and a 10s conditional-advertisement
	// timer (how often conditionally-advertised routes are re-evaluated).
	// Since this function fully regenerates frr.conf from scratch on every
	// apply rather than patching an existing file, "add if not present"
	// means unconditionally emitting them here.
	out.WriteString(" bgp log-neighbor-changes\n")
	out.WriteString(" no bgp ebgp-requires-policy\n")
	out.WriteString(" bgp deterministic-med\n")
	out.WriteString(" bgp bestpath as-path multipath-relax\n")
	out.WriteString(" bgp conditional-advertisement timer 10\n")
	if safeToken(b.RouterID) {
		fmt.Fprintf(&out, " bgp router-id %s\n", b.RouterID)
	}
	// Session timers. FRR's `timers bgp <keepalive> <hold>` takes both; emit
	// when either is set (0 means unset → FRR default).
	if b.KeepaliveTime > 0 || b.HoldTime > 0 {
		fmt.Fprintf(&out, " timers bgp %d %d\n", b.KeepaliveTime, b.HoldTime)
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
		// Per-neighbor BFD — there's no global toggle (see BGPConfig's doc
		// comment); each neighbor's own setting is authoritative.
		if n.BFD {
			fmt.Fprintf(&out, " neighbor %s bfd\n", n.Peer)
		}
		if n.Shutdown {
			fmt.Fprintf(&out, " neighbor %s shutdown\n", n.Peer)
		}
	}
	out.WriteString(" address-family ipv4 unicast\n")
	for _, pfx := range b.Networks {
		// A v6 prefix belongs in the ipv6 unicast block below — FRR rejects
		// a `network` statement here whose prefix doesn't match the AF.
		if safeToken(pfx) && !isIPv6Network(pfx) {
			fmt.Fprintf(&out, "  network %s\n", pfx)
		}
	}
	if b.RedistributeConnected {
		out.WriteString("  redistribute connected\n")
	}
	if b.RedistributeStatic {
		out.WriteString("  redistribute static\n")
	}
	var v6Neighbors, v4Neighbors []config.BGPNeighbor
	for _, n := range b.Neighbors {
		if !safeToken(n.Peer) || n.RemoteAS == 0 {
			continue
		}
		if isIPv6Peer(n.Peer) {
			v6Neighbors = append(v6Neighbors, n)
		} else {
			v4Neighbors = append(v4Neighbors, n)
		}
	}
	for _, n := range v4Neighbors {
		fmt.Fprintf(&out, "  neighbor %s activate\n", n.Peer)
	}
	for _, n := range v6Neighbors {
		// FRR defaults every neighbor active in ipv4 unicast, v6 peers
		// included; explicitly deactivate a v6 peer here so it isn't left
		// running an IPv4 unicast exchange over its v6 session (see
		// isIPv6Peer's doc comment). It's activated in ipv6 unicast instead,
		// below.
		fmt.Fprintf(&out, "  no neighbor %s activate\n", n.Peer)
	}
	out.WriteString(" exit-address-family\n")

	// A v6 neighbor with nothing activated anywhere would come up but
	// exchange no routes at all, so only bother with this block — and only
	// emit it — when there's an actual v6 peer or v6 prefix to carry.
	var v6Networks []string
	for _, pfx := range b.Networks {
		if safeToken(pfx) && isIPv6Network(pfx) {
			v6Networks = append(v6Networks, pfx)
		}
	}
	if len(v6Neighbors) > 0 || len(v6Networks) > 0 {
		// A real FRR running-config uses an indented `!` between sub-blocks
		// that are still inside the same stanza; only a column-0 `!` ends
		// the stanza (see parseRunningConfigBGP's stanza-end check, and
		// TestImportBGPFromFRRFile's sample text for what FRR itself
		// emits). An unindented separator here would end `router bgp`
		// before this block is ever reached on import.
		out.WriteString(" !\n")
		out.WriteString(" address-family ipv6 unicast\n")
		for _, pfx := range v6Networks {
			fmt.Fprintf(&out, "  network %s\n", pfx)
		}
		// Mirrors the ipv4 unicast block: gravinet's redistribute toggles
		// aren't family-specific, so "on" means both — FRR still requires
		// the directive under each address-family that should carry it.
		if b.RedistributeConnected {
			out.WriteString("  redistribute connected\n")
		}
		if b.RedistributeStatic {
			out.WriteString("  redistribute static\n")
		}
		for _, n := range v6Neighbors {
			fmt.Fprintf(&out, "  neighbor %s activate\n", n.Peer)
		}
		out.WriteString(" exit-address-family\n")
	}
	out.WriteString("!\n")
	return out.String()
}

// neededDaemons is which FRR daemons this config requires. staticd is always
// on (FRR's general-purpose daemon); bgpd whenever BGP is enabled; bfdd
// whenever BFD is on for any neighbor. Ported from parapet's needed_daemons
// (BGP/BFD subset), minus its global BFD toggle — gravinet doesn't have one.
func neededDaemons(b config.BGPConfig) []string {
	d := []string{"staticd"}
	if b.Enabled && b.ASN != 0 {
		d = append(d, "bgpd")
		bfd := false
		for _, n := range b.Neighbors {
			if n.BFD {
				bfd = true
				break
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

// bgpConfigRemovesSomething reports whether next drops a neighbor or
// advertised network that prev had, or tears down the whole `router bgp`
// stanza (BGP disabled, or the ASN itself changed — FRR treats a different
// ASN as a distinct instance, not an edit of the old one). renderFRR simply
// omits what's gone rather than emitting a `no neighbor ...`/`no network ...`
// negation, so nothing in the newly-written frr.conf actually tells FRR to
// drop it — applyBGP uses this to force a real restart in that case rather
// than trust a reload to notice.
//
// This is the fix for deleting a neighbor in the editor not actually
// disappearing from FRR: a plain `vtysh -b` only ever integrates the lines a
// config *has* into the running daemons, it cannot retract a line that's no
// longer there, so the stale neighbor would keep running. `systemctl reload
// frr` can do the equivalent of a full resync, but only when the host has
// frr-reload.py (the frr-pythontools package) installed and working — not
// something gravinet installs or can assume, and in practice a fair number
// of hosts don't have it, or have a reload that silently misbehaves. A
// restart re-reads frr.conf from scratch, so it's the one path that removes
// stale config on every host regardless of what optional tooling is present.
func bgpConfigRemovesSomething(prev, next config.BGPConfig) bool {
	if prev.Enabled && prev.ASN != 0 && (!next.Enabled || next.ASN != prev.ASN) {
		return true
	}
	haveNeighbor := make(map[string]bool, len(next.Neighbors))
	for _, n := range next.Neighbors {
		haveNeighbor[n.Peer] = true
	}
	for _, n := range prev.Neighbors {
		if !haveNeighbor[n.Peer] {
			return true
		}
	}
	haveNetwork := make(map[string]bool, len(next.Networks))
	for _, n := range next.Networks {
		haveNetwork[n] = true
	}
	for _, n := range prev.Networks {
		if !haveNetwork[n] {
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
// syncDaemons, which reconciles the FRR daemon set to a given config, and
// ensureFRRDaemonsEnabled, which makes sure BGP/BFD can come up the moment
// FRR is detected, are both OS-specific (Linux manages a single
// /etc/frr/daemons file; FreeBSD manages rc.conf's frr_daemons variable) —
// see frr_default.go and frr_freebsd.go.

// enableDaemonsContent turns each named daemon on in the body of a
// Linux-style `<name>=yes`/`<name>=no` daemons file: any `<name>=no` line
// becomes `<name>=yes`. It only ever enables — never disables — and leaves
// every other line untouched (including managed daemons not named here, and
// unrelated settings/comments). Pure string transform returning the new body
// and whether anything actually changed, so the logic is unit-testable
// without touching disk. FreeBSD has no equivalent file to transform (see
// frr_freebsd.go's ensureFRRDaemonsEnabled for why it doesn't need one); this
// stays here rather than in frr_default.go only because it's pure and
// harmless to keep alongside its test on every platform.
//
// This is deliberately separate from syncDaemonsContent, which reconciles the
// whole managed set to a specific BGP config and will turn unused daemons off.
// enableDaemonsContent backs the "make sure FRR can run BGP/BFD the moment it's
// detected" step, which must neither depend on nor disturb the stored config.
func enableDaemonsContent(existing string, names []string) (string, bool) {
	changed := false
	lines := strings.Split(existing, "\n")
	for i, line := range lines {
		for _, d := range names {
			if strings.HasPrefix(line, d+"=no") {
				if line != d+"=yes" {
					changed = true
				}
				lines[i] = d + "=yes"
				break
			}
		}
	}
	return strings.Join(lines, "\n"), changed
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
// save). The actual service call is dispatched to a background goroutine —
// an frr restart can take many seconds, and the HTTP handler shouldn't block on
// it — after the config has been written synchronously (so the save is durable
// and the API can return success immediately). Ported from parapet's
// apply_routing.
//
// prev is the BGP config this one is replacing (the zero value if there
// wasn't one, e.g. the first-ever save) — used solely to detect whether this
// save removes a neighbor, network, or the whole BGP stanza, which forces a
// real restart instead of a reload (see bgpConfigRemovesSomething).
//
// syncDaemons, applyFRRService, and managedDaemonSet are each implemented
// once per OS (frr_default.go / frr_freebsd.go); everything below this line
// is the shared reconciliation shape both sides plug into.
func applyBGP(b, prev config.BGPConfig, log *logx.Logger) (string, error) {
	if !frrInstalled() {
		return "FRR is not installed; BGP config saved but not applied", nil
	}
	if err := writeAtomicFile(frrConf, renderFRR(b)); err != nil {
		return "", err
	}
	wanted := managedDaemonSet(b)
	daemonSetChanged, err := syncDaemons(b)
	if err != nil {
		return "", err
	}
	// A daemon can be "wanted" without actually being alive — e.g. an earlier
	// restart failed partway, or it crash-looped on bad config. In that case
	// syncDaemons sees no change and would wrongly take the reload path
	// forever, which can't start a stopped daemon. Treat "wanted but not
	// alive" the same as "changed" so it gets a real restart.
	needsStart := false
	for _, d := range wanted {
		if !daemonAlive(d) {
			needsStart = true
			break
		}
	}
	// A removed neighbor/network (or BGP being turned off, or the ASN
	// changing) needs the same real restart: neither a reload (not
	// guaranteed to actually diff-and-retract — see applyFRRService's two
	// implementations for why) nor the vtysh -b fallback below can retract
	// config that's no longer in the file, only add to what's running. See
	// bgpConfigRemovesSomething.
	removed := bgpConfigRemovesSomething(prev, b)
	daemonsChanged := daemonSetChanged || needsStart || removed
	daemonCount := len(wanted)

	// FreeBSD's reload path can silently no-op and still report success (see
	// applyFRRService in frr_freebsd.go) — not a "sometimes fails" case a
	// retry helps with, but a "may lie about succeeding" one no fallback can
	// catch after the fact, so it's never used at all: every apply there is a
	// restart, exactly as if daemonsChanged were always true.
	restartOnly := runtime.GOOS == "freebsd"

	go func() {
		action := "reload"
		if daemonsChanged || restartOnly {
			action = "restart"
		}
		ok := applyFRRService(action)
		// A restart failure when a daemon needs to transition (e.g. bgpd just
		// got enabled) has no vtysh safety net (vtysh -b can only push config
		// into an already-running daemon, not spawn one), but such failures are
		// often transient (FRR startup races, a queued service job); retry once.
		if (daemonsChanged || restartOnly) && !ok {
			time.Sleep(3 * time.Second)
			ok = applyFRRService("restart")
		}
		switch {
		case ok:
			log.Infof("bgp: routing applied: %d daemon(s), frr %sed", daemonCount, action)
		case !daemonsChanged && vtyshApplyBoot():
			// vtysh -b only pushes config into already-running daemons — valid
			// only when no daemon needed to change state (a pure config reload)
			// and nothing was removed (daemonsChanged already covers both).
			log.Infof("bgp: routing applied via vtysh -b")
		default:
			svc := "systemctl"
			if runtime.GOOS == "freebsd" {
				svc = "service(8)"
			}
			tried := " and vtysh"
			if daemonsChanged || restartOnly {
				tried = " (twice)"
			}
			log.Warnf("bgp: config written but could not %s FRR (tried %s%s)", action, svc, tried)
		}
	}()

	return fmt.Sprintf("%d daemon(s) configured, reloading FRR in background", daemonCount), nil
}

// daemonAlive reports whether an FRR daemon is actually running: read its pid
// from /var/run/frr/<daemon>.pid — the same path on both Linux and FreeBSD,
// confirmed against FreeBSD's own frr rc.d script (net/frrN's files/frr.in) —
// and check that pid is a live process by sending it signal 0, which delivers
// nothing but fails with ESRCH if the pid is gone. This is more reliable than
// what a daemon-enable file/variable merely says should be running. Ported
// from parapet's daemon_alive, generalized past Linux's /proc: signal 0 is
// the same portable liveness probe on every Unix, whereas the previous
// /proc/<pid>-directory check only ever worked on Linux, silently returning
// false on FreeBSD (and macOS) for lack of a mounted procfs — which forced
// every non-Linux config change through a full restart even for a one-line
// edit, since applyBGP treats "can't confirm alive" as "needs a restart, the
// safe direction". On Windows (BGP is never reachable there — bgpSupported()
// gates on vtysh, which Windows never has) Signal is simply unsupported and
// this returns false, the same safe fallback as before.
func daemonAlive(name string) bool {
	raw, err := os.ReadFile("/var/run/frr/" + name + ".pid")
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
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

// runVtysh runs `vtysh -c <cmd>` and returns its stdout, with a hard wall-clock
// bound that holds even if vtysh — or a grandchild that inherited its stdout
// pipe — ignores the context kill. os/exec's Output() waits for the stdout
// copier to finish, which a lingering grandchild can stall indefinitely past
// the context deadline; that exact stall is what could wedge the BGP page. So
// the command runs on its own goroutine and the caller stops waiting at the
// deadline no matter what. A truly wedged vtysh leaks at most one goroutine and
// one (already SIGKILL-targeted) process; it can never block the HTTP handler.
// ok is false when vtysh is absent, errored, or exceeded the bound.
func runVtysh(cmd string) (out []byte, ok bool) {
	bin, present := vtyshPath()
	if !present {
		return nil, false
	}
	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1) // buffered so the goroutine never blocks on send
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), bgpQueryTimeout)
		defer cancel()
		o, err := exec.CommandContext(ctx, bin, "-c", cmd).Output()
		ch <- result{o, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, false
		}
		return r.out, true
	case <-time.After(bgpQueryTimeout + 2*time.Second):
		// vtysh (or a child) is wedged; abandon it rather than block the caller.
		return nil, false
	}
}

// importBGPFromFRR reads the BGP configuration FRR is *actually running* and
// reshapes it into a config.BGPConfig, so gravinet can reflect a pre-existing
// setup (one configured outside gravinet — a hand-edited frr.conf, another
// tool, or a prior install) instead of showing an empty editor while live peers
// are established.
//
// The primary source is the BGP summary JSON — the exact query the live-peers
// view uses, which is proven to work wherever peers actually appear. It yields
// the local AS, the router id, and the configured neighbor list (peer address +
// remote AS, including neighbors that are configured but down). That's what the
// editor most needs, and building from it means the editor matches the live
// table on any host where the peers panel works. Parsing `show running-config`
// was the old primary source, but it can fail or be restricted on some hosts
// (and produced exactly the "peers live, editor empty" mismatch) — so it's now
// only a best-effort enrichment for the fields the summary doesn't carry
// (advertised networks, redistribute, per-neighbor description/BFD). If it
// fails, the core config from JSON still stands.
//
// Returns ok=false only when vtysh is absent/wedged or no BGP is running.
// Passwords are deliberately not imported (FRR may hold them encrypted, and
// re-emitting them verbatim on a later save would corrupt the session);
// hasPasswords reports whether any neighbor had one so the UI can warn.
//
// Read-only: importing never writes frr.conf or config.json. gravinet takes
// ownership only when the operator explicitly saves (adopts) the config.
// frrConfigPaths are the on-disk FRR configs to read the BGP stanza from, in
// priority order: the integrated config first, then a per-daemon bgpd.conf.
// Built from frrDir (set per-OS) rather than a hardcoded Linux path, so this
// looks in the right place on FreeBSD too. A var (not a const list) so tests
// can point it at a fixture.
var frrConfigPaths = []string{frrConf, frrDir + "/bgpd.conf"}

// importBGPFromFRR reads the BGP configuration FRR is set up with and reshapes
// it into a config.BGPConfig, so gravinet reflects a pre-existing setup rather
// than showing an empty editor.
//
// The authoritative source is FRR's own config file (/etc/frr/frr.conf) — read
// directly off disk. This is deliberate and is the fix for this having failed
// repeatedly: earlier versions read the config through vtysh
// (`show ip bgp summary json` / `show running-config`), which only works when
// bgpd is up and answering — so a host whose peer session wasn't established,
// or whose daemon wasn't fully responsive, imported nothing even though the
// config was sitting in the file the whole time. The file doesn't depend on the
// daemon at all, and its format is exactly what parseRunningConfigBGP handles.
//
// vtysh is now only a fallback (config held solely in a running daemon, or a
// nonstandard file path) and a source for the live router-id/AS when the file
// omits them. Returns ok=false only when no source yields a BGP stanza.
// Passwords are not imported (they'd round-trip badly and needn't be surfaced);
// hasPasswords reports whether any neighbor had one so the UI can warn.
//
// Read-only: importing never writes anything. gravinet takes ownership only
// when the operator explicitly saves (adopts) the config.
//
// reason is a short human diagnostic — which source the config came from, or,
// when ok is false, why every source came up empty (file missing, unreadable,
// no stanza; vtysh absent or not answering). It's logged and surfaced to the UI
// so an empty editor isn't indistinguishable from a broken import, which is what
// made this hard to diagnose across earlier attempts. log may be nil.
func importBGPFromFRR(log *logx.Logger) (cfg config.BGPConfig, hasPasswords bool, ok bool, reason string) {
	var tried []string // per-source notes, joined into reason only if nothing works
	// 1) The config file — authoritative and daemon-independent.
	for _, path := range frrConfigPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			// A missing file is unremarkable; a present-but-unreadable one
			// (permissions — the classic "daemon can't read /etc/frr") is the
			// kind of thing the operator needs told, so record that case.
			if !os.IsNotExist(err) {
				tried = append(tried, fmt.Sprintf("%s: %v", path, err))
			}
			continue
		}
		if fc, pw, fok := parseRunningConfigBGP(string(raw)); fok {
			cfg, hasPasswords, ok, reason = fc, pw, true, "read from "+path
			break
		}
		tried = append(tried, path+": no 'router bgp' stanza")
	}
	// 2) Fallback: the live running-config via vtysh (config held only in the
	//    daemon — e.g. configured live and never `write memory`'d — or a config
	//    path we don't know about).
	if !ok {
		if rc, ran := runVtysh("show running-config"); ran {
			if rcCfg, pw, rok := parseRunningConfigBGP(string(rc)); rok {
				cfg, hasPasswords, ok, reason = rcCfg, pw, true, "read from vtysh running-config"
			} else {
				tried = append(tried, "vtysh running-config: no 'router bgp' stanza")
			}
		} else {
			tried = append(tried, "vtysh: not installed or not answering (is bgpd running?)")
		}
	}
	// 3) Enrich from the live summary: fill router-id / local AS when the config
	//    didn't carry them explicitly, and — if we still have nothing — use the
	//    summary as a last-resort source (a running speaker with no readable
	//    config file).
	// "show bgp summary json", not "show ip bgp summary json" — the "ip"
	// keyword restricts FRR to IPv4 unicast only, which would silently drop an
	// IPv6-only speaker's AS/router-id from this enrichment (see the identical
	// fix in handleBGP).
	if sum, ran := runVtysh("show bgp summary json"); ran {
		if scfg, _, sok := summaryToBGPConfig(sum); sok {
			if !ok {
				cfg, ok, reason = scfg, true, "read from live BGP summary"
			} else {
				if cfg.RouterID == "" {
					cfg.RouterID = scfg.RouterID
				}
				if cfg.ASN == 0 {
					cfg.ASN = scfg.ASN
				}
			}
		}
	}
	if !ok {
		if len(tried) > 0 {
			reason = "no existing BGP config found — " + strings.Join(tried, "; ")
		} else {
			reason = "no existing BGP config found on this host"
		}
	}
	if log != nil {
		if ok {
			log.Infof("bgp import: %s (asn=%d, %d neighbor(s))", reason, cfg.ASN, len(cfg.Neighbors))
		} else {
			log.Infof("bgp import: nothing imported — %s", reason)
		}
	}
	return cfg, hasPasswords, ok, reason
}

// summaryToBGPConfig builds a config.BGPConfig from `show bgp summary json`
// output: local AS, router id, and the neighbor list (peer + remote AS),
// deduped by address across families. Pure and unit-tested. ok is false when
// there's no BGP speaker (no AS and no peers). The returned index maps peer
// address → position in cfg.Neighbors, for later enrichment.
func summaryToBGPConfig(sum []byte) (cfg config.BGPConfig, at map[string]int, ok bool) {
	peers, routerID, localAS := parseBGPSummary(sum)
	if localAS == 0 && len(peers) == 0 {
		return config.BGPConfig{}, nil, false
	}
	cfg.Enabled = true
	cfg.ASN = uint32(localAS) // BGP ASNs are 32-bit; fits uint32
	cfg.RouterID = routerID
	at = map[string]int{}
	for _, p := range peers {
		if p.Peer == "" || p.RemoteAS == 0 {
			continue
		}
		if _, dup := at[p.Peer]; dup {
			continue // a dual-stack peer appears once per address family
		}
		at[p.Peer] = len(cfg.Neighbors)
		cfg.Neighbors = append(cfg.Neighbors, config.BGPNeighbor{Peer: p.Peer, RemoteAS: uint32(p.RemoteAS)})
	}
	return cfg, at, true
}

// parseRunningConfigBGP extracts the BGP config from FRR `show running-config`
// text. Pure function (no I/O) so it's fully unit-testable. It reads the single
// `router bgp <asn>` stanza — router-id, per-neighbor remote-as/description/
// bfd/shutdown, and the address-family's networks and redistribute
// directives — mirroring exactly the surface renderFRR emits, so what's
// imported round-trips through the same fields. Neighbor order is preserved
// (first appearance). hasPasswords is set if any neighbor carried a
// `password` line, though the secret itself is not captured.
func parseRunningConfigBGP(text string) (cfg config.BGPConfig, hasPasswords bool, ok bool) {
	// Normalize line endings first: a config that ever passed through a CRLF
	// editor would otherwise leave a trailing \r on the ASN token (`65001\r`),
	// which fails to parse and silently yields "no stanza" — an import that
	// looks broken for a reason that's invisible in the UI.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
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
		case strings.HasPrefix(line, "timers bgp "):
			f := strings.Fields(strings.TrimPrefix(line, "timers bgp "))
			if len(f) >= 2 {
				if ka, err := strconv.ParseUint(f[0], 10, 32); err == nil {
					cfg.KeepaliveTime = uint32(ka)
				}
				if hold, err := strconv.ParseUint(f[1], 10, 32); err == nil {
					cfg.HoldTime = uint32(hold)
				}
			}
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
			case "shutdown":
				getN(peer).Shutdown = true
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
