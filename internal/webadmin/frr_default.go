//go:build !freebsd

package webadmin

// The default FRR integration: Debian/RHEL-style packaging, where config
// lives at /etc/frr and a single /etc/frr/daemons file gates which daemons
// run, driven through systemctl. This is what every platform except FreeBSD
// gets (see frr_freebsd.go for that one) — in practice that's really just
// Linux, since Windows/macOS/OpenBSD never have vtysh at any of the paths
// bgpVtyshPaths checks, so bgpSupported() stays false and none of this is
// ever reached there; it's harmless to share the same definitions with them
// rather than carve out yet another platform file for code that would only
// ever no-op identically anyway.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

const (
	frrDir     = "/etc/frr"
	frrConf    = "/etc/frr/frr.conf"
	frrDaemons = "/etc/frr/daemons"
)

// managedDaemonSet is the daemon list applyBGP checks liveness against and
// reports the count of. On this platform that's exactly neededDaemons(b) —
// FreeBSD's frr_daemons variable additionally needs mgmtd and zebra listed
// explicitly (see freebsdNeededDaemons); Linux's /etc/frr/daemons ships those
// two already =yes and gravinet was never asked to manage them, so there's
// nothing to add here.
func managedDaemonSet(b config.BGPConfig) []string {
	return neededDaemons(b)
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

// frrBGPBFDDaemons are the daemons that must be running for gravinet's BGP and
// BFD features to work when FRR is present: the BGP speaker and the BFD session
// daemon. These are exactly the two values ensureFRRDaemonsEnabled makes sure
// read `=yes` in /etc/frr/daemons whenever FRR is detected.
var frrBGPBFDDaemons = []string{"bgpd", "bfdd"}

// ensureFRRDaemonsEnabled makes sure bgpd and bfdd are enabled in
// /etc/frr/daemons whenever FRR is detected on this host, restarting FRR if it
// had to change anything. Run once in the background at daemon startup (see
// Server.Start): a stock FRR install ships with every optional daemon set to
// =no, so BGP/BFD would never come up until something turned them on — this is
// that something.
//
// It only ever flips =no to =yes for those two daemons; it never disables
// anything and never touches other lines, so it can't fight the config-driven
// reconciliation in applyBGP (which owns turning daemons back off when a saved
// BGP config doesn't need them). No-op when FRR isn't installed, and idempotent:
// on every later boot the values already read =yes, nothing changes, and FRR is
// not restarted.
func ensureFRRDaemonsEnabled(log *logx.Logger) {
	if !frrInstalled() {
		return // no FRR here — nothing to enable.
	}
	raw, err := os.ReadFile(frrDaemons)
	if err != nil || len(raw) == 0 {
		// FRR's config directory exists but the daemons file doesn't (or is
		// empty): not the layout we manage. Leave it alone rather than guess.
		return
	}
	body, changed := enableDaemonsContent(string(raw), frrBGPBFDDaemons)
	if !changed {
		return // bgpd and bfdd already enabled — nothing to do.
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if err := writeAtomicFile(frrDaemons, body); err != nil {
		log.Warnf("bgp: could not enable bgpd/bfdd in %s: %v", frrDaemons, err)
		return
	}
	if applyFRRService("restart") {
		log.Infof("bgp: enabled bgpd and bfdd in %s and restarted FRR", frrDaemons)
	} else {
		log.Warnf("bgp: enabled bgpd and bfdd in %s but could not restart FRR", frrDaemons)
	}
}

// applyFRRService runs `systemctl <action> frr`, wrapped in `timeout` so a
// stuck systemd job can't hang the background goroutine forever. Falls back
// to a direct systemctl call if `timeout` itself is missing. Ported from
// parapet's run_systemctl. See frr_freebsd.go for the service(8) equivalent.
func applyFRRService(action string) bool {
	if err := exec.Command("timeout", "45", "systemctl", action, "frr").Run(); err == nil {
		return true
	} else if _, lookErr := exec.LookPath("timeout"); lookErr == nil {
		// timeout ran but the job failed/timed out.
		return false
	}
	// timeout binary missing — direct call.
	return exec.Command("systemctl", action, "frr").Run() == nil
}
