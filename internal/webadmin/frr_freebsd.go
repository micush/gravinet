//go:build freebsd

package webadmin

// FreeBSD's frr port (net/frr9, net/frr10, ...) is a different shape from
// Debian/RHEL's package: config lives under /usr/local/etc/frr, there's no
// single per-daemon on/off file — rc.conf's frr_daemons variable lists every
// daemon to run, in order, with mgmtd and zebra required first — and it's
// driven through service(8), not systemd. All of that is confirmed against
// the actual rc.d script FreeBSD ships (net/frrN/files/frr.in in the
// freebsd-ports tree), not guessed: the pidfile path, the vtysh.conf
// bootstrap, the /var/lib/frr bootstrap, the reload footgun (applyFRRService),
// and why gravinet has to actively enable FRR at startup rather than waiting
// for a config save (ensureFRRDaemonsEnabled) all come directly from reading
// it — several of these only became apparent once "FRR pkg-installs fine but
// never actually runs" surfaced as a real report, not just a theoretical gap.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

const (
	frrDir  = "/usr/local/etc/frr"
	frrConf = "/usr/local/etc/frr/frr.conf"
	// frrVtyshConf holds the "service integrated-vtysh-config" marker every
	// FRR daemon needs to read the single frrConf instead of hunting for its
	// own per-daemon config file (bgpd.conf, zebra.conf, ...). The rc.d
	// script writes this itself, but only the first time `service frr start`
	// (not restart) runs with frr_vtysh_boot=YES — and applyBGP always uses
	// restart here (see applyFRRService), so a box that's never had a plain
	// `start` would otherwise never get it. syncDaemons writes it directly
	// instead of relying on that.
	frrVtyshConf = "/usr/local/etc/frr/vtysh.conf"
)

// freebsdBaseDaemons are the two daemons FreeBSD's frr rc.d script requires
// first in frr_daemons, in this order, for anything else to start: mgmtd
// (the management-plane daemon every other FRR daemon now depends on) and
// zebra (the core RIB manager). Linux's package ships these two already
// =yes in /etc/frr/daemons by default, and gravinet's managed set
// (frrManagedDaemons) deliberately excludes them so it never touches that
// default; FreeBSD's frr_daemons variable has no implicit default of its
// own once gravinet starts managing it, so these can't be left implicit.
var freebsdBaseDaemons = []string{"mgmtd", "zebra"}

// freebsdNeededDaemons is neededDaemons(b) — the OS-agnostic logical set —
// prefixed with freebsdBaseDaemons, in the order FreeBSD's rc.d script
// requires.
func freebsdNeededDaemons(b config.BGPConfig) []string {
	return append(append([]string{}, freebsdBaseDaemons...), neededDaemons(b)...)
}

// managedDaemonSet is the daemon list applyBGP checks liveness against and
// reports the count of — here, everything frr_daemons will actually be set
// to, mgmtd and zebra included, since on FreeBSD those two are just as much
// part of what this save changes as bgpd or staticd are.
func managedDaemonSet(b config.BGPConfig) []string {
	return freebsdNeededDaemons(b)
}

// sysrcGet reads a single rc.conf variable via `sysrc -n`, which prints just
// the value (or nothing, with a non-zero exit, if unset).
func sysrcGet(name string) (string, bool) {
	out, err := exec.Command("sysrc", "-n", name).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// sysrcSet writes a single rc.conf variable via `sysrc <name>=<value>`. value
// is always one of this file's own fixed strings (a space-joined daemon list
// built from the constant name lists above, or the literal "YES") — never
// operator input — so there's no quoting/injection concern passing it as one
// exec.Command argument.
func sysrcSet(name, value string) error {
	return exec.Command("sysrc", name+"="+value).Run()
}

// frrVarLibBootstrap creates /var/lib/frr — FRR's runtime state directory,
// which zebra and the other daemons need to write into to fully start — and
// gives it to the frr user/group. This mirrors exactly what FreeBSD's own
// frr rc.d script does, unconditionally, on every `service frr start` (see
// files/frr.in in the freebsd-ports tree: `mkdir -p /var/lib/frr; chown -R
// frr:frr /var/lib/frr`, right before it starts anything).
//
// gravinet needs to do this itself because it never actually calls `service
// frr start` — applyFRRService always uses `restart` (see its own doc
// comment for why), and the rc.d script's `restart` case skips this
// bootstrap block entirely; it only runs inside `start`. Without this, a
// freshly pkg-installed FRR that's never had a bare `service frr start` run
// against it by a human would have its daemons trying to come up with
// nowhere to write their runtime state — which is exactly the shape of "FRR
// installs fine but never actually runs" this function exists to prevent.
//
// Idempotent (MkdirAll on an existing dir is a no-op) and best-effort by
// design at the call site: a failure here is logged but never blocks the
// rest of an apply, since some builds/configurations may not need this
// directory at all.
func frrVarLibBootstrap() error {
	const dir = "/var/lib/frr"
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	u, err := user.Lookup("frr")
	if err != nil {
		return fmt.Errorf("lookup frr user: %w", err)
	}
	uid, errU := strconv.Atoi(u.Uid)
	gid, errG := strconv.Atoi(u.Gid)
	if errU != nil || errG != nil {
		return fmt.Errorf("parse frr uid/gid %q/%q: %v / %v", u.Uid, u.Gid, errU, errG)
	}
	return filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	})
}

// syncDaemons is syncDaemons' FreeBSD implementation: there's no per-daemon
// on/off file to rewrite the way Linux's /etc/frr/daemons works, so this sets
// rc.conf's frr_daemons to exactly the wanted set via sysrc(8), makes sure
// frr_enable="YES", makes sure the integrated-config marker (frrVtyshConf)
// exists — see its doc comment for why that last part can't be left to the
// rc.d script's own bootstrap — and makes sure /var/lib/frr is in place (see
// frrVarLibBootstrap, same reasoning). Returns whether anything actually
// changed, the same contract as the Linux version.
func syncDaemons(b config.BGPConfig) (bool, error) {
	desired := strings.Join(freebsdNeededDaemons(b), " ")
	changed := false

	if cur, ok := sysrcGet("frr_daemons"); !ok || cur != desired {
		if err := sysrcSet("frr_daemons", desired); err != nil {
			return changed, fmt.Errorf("sysrc frr_daemons: %w", err)
		}
		changed = true
	}
	if en, ok := sysrcGet("frr_enable"); !ok || !strings.EqualFold(en, "YES") {
		if err := sysrcSet("frr_enable", "YES"); err != nil {
			return changed, fmt.Errorf("sysrc frr_enable: %w", err)
		}
		changed = true
	}
	if _, err := os.Stat(frrVtyshConf); errors.Is(err, os.ErrNotExist) {
		if err := writeAtomicFile(frrVtyshConf, "service integrated-vtysh-config\n"); err != nil {
			return changed, fmt.Errorf("write %s: %w", frrVtyshConf, err)
		}
		changed = true
	}
	// Best-effort: a chown failure here (e.g. the frr user somehow not
	// existing) shouldn't block saving/pushing an otherwise-valid BGP
	// config — the daemons might still come up fine without it on some
	// builds, and this same bootstrap already runs, with its failure
	// actually surfaced, from ensureFRRDaemonsEnabled at startup.
	_ = frrVarLibBootstrap()
	return changed, nil
}

// ensureFRRDaemonsEnabled makes sure FRR is actually running as soon as it's
// detected on this host — mirroring the *effect* of the Linux version (force
// bgpd/bfdd on, restart if that changed anything), even though the
// mechanism here is completely different (no daemons file to flip lines in).
//
// This turned out to matter more than an earlier version of this file
// assumed: without it, a freshly pkg-installed FRR just sits there —
// frr_enable defaults to "NO" in the rc.d script, and nothing sets it to
// "YES" until an operator actually saves a BGP config through gravinet's
// Traffic > BGP page. That's "FRR installs fine but never actually runs"
// exactly as reported, and it's a real gap from parity with Linux, where
// this same function forces FRR up the moment it's detected, independent of
// whether BGP has been configured yet. Run once in the background at
// gravinet startup (see Server.Start), same as Linux.
//
// Sets frr_daemons to a minimal baseline — mgmtd, zebra, staticd, bgpd,
// bfdd — rather than leaving frr_daemons unset (which would fall back to
// the port's own default of essentially every protocol daemon: babeld,
// eigrpd, isisd, ospfd, ospf6d, ripd, ripngd, ...). This matches what Linux
// ends up running: bgpd+bfdd forced on, plus whatever staticd/zebra already
// default to there — not every protocol FRR happens to ship.
//
// Idempotent: if frr_enable/frr_daemons already match this baseline,
// nothing changes and FRR isn't restarted — same as Linux only restarting
// when enableDaemonsContent actually had something to flip.
func ensureFRRDaemonsEnabled(log *logx.Logger) {
	if !frrInstalled() {
		return // no FRR here — nothing to enable.
	}
	baseline := append(append([]string{}, freebsdNeededDaemons(config.BGPConfig{})...), "bgpd", "bfdd")
	desired := strings.Join(baseline, " ")

	changed := false
	if cur, ok := sysrcGet("frr_daemons"); !ok || cur != desired {
		if err := sysrcSet("frr_daemons", desired); err != nil {
			log.Warnf("bgp: could not set frr_daemons: %v", err)
			return
		}
		changed = true
	}
	if en, ok := sysrcGet("frr_enable"); !ok || !strings.EqualFold(en, "YES") {
		if err := sysrcSet("frr_enable", "YES"); err != nil {
			log.Warnf("bgp: could not set frr_enable: %v", err)
			return
		}
		changed = true
	}
	if !changed {
		return // already enabled with this daemon set — nothing to do.
	}
	if err := frrVarLibBootstrap(); err != nil {
		log.Warnf("bgp: could not prepare /var/lib/frr (FRR may fail to start without it): %v", err)
		// Keep going anyway — some builds/configurations may not need it.
	}
	if applyFRRService("restart") {
		log.Infof("bgp: enabled FRR (frr_daemons=%q) and started it", desired)
	} else {
		log.Warnf("bgp: enabled FRR (frr_daemons=%q) but could not start it", desired)
	}
}

// applyFRRService runs `service frr <action>`, FreeBSD's rc.d equivalent of
// frr_default.go's systemctl-based implementation. Deliberately promotes
// "reload" to "restart": the frr rc.d script's own reload case
// checks for %%PREFIX%%/sbin/frr-reload.py (the frrN-pythontools package,
// not something gravinet's installer pulls in) and, if it's missing, prints
// a message and *exits 0* — success — having done nothing. A caller that
// trusts that exit code would believe a config change was applied when it
// silently wasn't, which is worse than just always paying for a restart. See
// the identical Linux-side reasoning (frr-reload.py / frr-pythontools)
// documented on bgpConfigRemovesSomething in frr.go.
func applyFRRService(action string) bool {
	if action == "reload" {
		action = "restart"
	}
	if err := exec.Command("timeout", "45", "service", "frr", action).Run(); err == nil {
		return true
	} else if _, lookErr := exec.LookPath("timeout"); lookErr == nil {
		return false
	}
	return exec.Command("service", "frr", action).Run() == nil
}
