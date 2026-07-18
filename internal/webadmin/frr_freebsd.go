//go:build freebsd

package webadmin

// FreeBSD's frr port (net/frr9, net/frr10, ...) is a different shape from
// Debian/RHEL's package: config lives under /usr/local/etc/frr, there's no
// single per-daemon on/off file — rc.conf's frr_daemons variable lists every
// daemon to run, in order, with mgmtd and zebra required first — and it's
// driven through service(8), not systemd. All of that is confirmed against
// the actual rc.d script FreeBSD ships (net/frrN/files/frr.in in the
// freebsd-ports tree), not guessed: the pidfile path, the vtysh.conf
// bootstrap, and the reload footgun documented on applyFRRService below all
// come directly from reading it.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// syncDaemons is syncDaemons' FreeBSD implementation: there's no per-daemon
// on/off file to rewrite the way Linux's /etc/frr/daemons works, so this sets
// rc.conf's frr_daemons to exactly the wanted set via sysrc(8), makes sure
// frr_enable="YES", and makes sure the integrated-config marker (frrVtyshConf)
// exists — see its doc comment for why that last part can't be left to the
// rc.d script's own bootstrap. Returns whether anything actually changed, the
// same contract as the Linux version.
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
	return changed, nil
}

// ensureFRRDaemonsEnabled is a no-op on FreeBSD. Its Linux counterpart exists
// because Debian/RHEL's package ships every optional daemon =no by default,
// so bgpd/bfdd need turning on once before syncDaemons' reload path has
// anything to matter. FreeBSD's syncDaemons above sets frr_daemons directly
// to the fully-computed wanted set on every single save — there's no
// intermediate "everything defaults off, flip these two on" step to
// compensate for, so there's nothing for this to do.
func ensureFRRDaemonsEnabled(log *logx.Logger) {}

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
