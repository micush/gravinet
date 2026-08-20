package service

// Link-layer discovery agent (lldpd) management — the backend for System >
// LLDP. Ported from parapet's LldpManager (src/lldpd.rs) and its
// status::discovery_json() neighbor-status reader, with the same
// architecture change SNMP already made: parapet spawns and supervises
// lldpd as a direct child of its own process (elaborate code exists there
// just to handle lldpd's privsep worker process, process groups, and
// crash-hint diagnostics that come with owning that lifecycle); gravinet
// instead manages it as an ordinary OS service, the same way it already
// treats FRR and snmpd rather than running either as a child. A child of
// gravinet's own process would die every time gravinet itself restarts,
// which happens far more often than an operator wants link-layer discovery
// to blink; a real OS service persists across that, and gravinet doesn't
// need to reimplement lldpd's own process-group/privsep-worker cleanup
// dance to get there.
//
// The one piece that's identical either way: reading live neighbor data.
// lldpcli talks to whatever lldpd instance is running over its control
// socket regardless of who launched it, so LLDPNeighbors below works the
// same whether gravinet, systemd, or an operator by hand started the agent.
//
// Configuration is delivered as extra flags to the lldpd binary — the exact
// argv parapet's own lldpArgs equivalent builds (`-d`, optionally `-c` for
// CDP, optionally `-I <ifaces>`) — rather than lldpd's own config-file
// grammar (lldpcli directives in /etc/lldpd.conf / /etc/lldpd.d/*.conf).
// That grammar is real and would also work, but this package sticks to the
// argv shape already precisely documented in parapet's own comments rather
// than a config-file syntax that can't be verified against a live lldpd
// here; getting an unfamiliar directive grammar subtly wrong would silently
// fail to apply instead of erroring. On Linux those flags are delivered via
// a systemd drop-in (`ExecStart=` cleared then reset, a standard,
// well-documented override mechanism) rather than assuming any particular
// distro's own /etc/default or /etc/sysconfig convention for extra
// arguments; on the BSDs, via each platform's own `_flags` rc variable,
// appended to (not replacing) whatever base invocation the packaged rc.d
// script already uses — so `-d` is deliberately NOT included there, unlike
// the Linux drop-in (which fully replaces ExecStart and so needs the
// complete, self-sufficient invocation).
//
// Supported: linux, freebsd, openbsd, darwin (Homebrew, with the identical
// root-vs-Homebrew caveat SNMP's package comment already documents — see
// there for the full reasoning; it applies here unchanged). Windows is
// unsupported: LLDP has no equivalent built-in Windows service the way SNMP
// at least has *something* registry-based to point at, so there is nothing
// to even honestly describe as "different" — it's just absent.
//
// OpenBSD caveat: since 7.8, OpenBSD ships its own unrelated, from-scratch
// lldpd(8) in base (LLDP-only, queried via lldp(8) not lldpcli, no rc.d(8)
// script by design). This package only ever drives the ports net/lldpd
// package (the same one Linux/FreeBSD/macOS use) — see lldpdBinary's own
// doc comment for how the two same-named binaries are told apart, and
// lldpProcs' for why the base one is also kept out of stray-process
// detection even when gravinet's own LLDP is switched off.

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/config"
)

// LLDPSupported reports whether lldpd looks usable on this host at all —
// installed, on a platform this package manages a service on.
func LLDPSupported() (bool, string) {
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd", "darwin":
	default:
		return false, "L2 discovery isn't supported on this operating system"
	}
	if lldpdBinary() != "" {
		return true, ""
	}
	return false, lldpNotInstalledHint()
}

// lldpNotInstalledHint is LLDPSupported's message once lldpdBinary has come
// back empty. Split out on its own so the OpenBSD branch's os.Stat check
// isn't buried inline in LLDPSupported: on OpenBSD specifically, "nothing
// gravinet can drive" and "genuinely nothing installed" need different
// messages — a host with only the incompatible built-in lldpd(8) (see
// lldpdBinary's own doc comment) is not in the same state as a host with
// no lldpd at all, and the generic "is the lldpd package installed?"
// phrasing would be actively misleading there — pkg_add would report
// lldpd already present.
func lldpNotInstalledHint() string {
	if runtime.GOOS == "openbsd" && firstExisting("/usr/sbin/lldpd") != "" {
		return "OpenBSD's built-in lldpd(8) (since 7.8) isn't compatible with gravinet's LLDP \u2014 no CDP support, no lldpcli, and OpenBSD ships it without an rc.d(8) script by design. Install the net/lldpd package instead (pkg_add lldpd)."
	}
	return "lldpd isn't installed on this host (is the lldpd package installed?)"
}

// lldpdBinary locates the lldpd binary gravinet can actually configure and
// drive as a service.
//
// OpenBSD needs its own path list, deliberately excluding /usr/sbin (and
// /sbin, /usr/bin — the other base-system candidates every other platform
// checks): starting with 7.8, OpenBSD ships its own from-scratch lldpd(8)
// in base, at /usr/sbin/lldpd — same name, unrelated project, and
// incompatible with everything this package assumes about "the" lldpd.
// It's LLDP-only (IEEE 802.1AB; no CDP support at all, ever — see
// DiscoveryConfig's own doc comment on why CDP matters here), its query
// tool is lldp(8), not lldpcli, and — confirmed on OpenBSD's own misc@
// mailing list as an intentional decision, not an oversight — it ships
// with no /etc/rc.d(8) script: the base system can't cleanly claim the rc
// script name the ports net/lldpd package (the one this file actually
// knows how to drive) already owns. Checking /usr/sbin/lldpd here would
// make LLDPSupported report "yes" for a binary this package can neither
// configure (no -c/-I flags to give it) nor service-manage (rcctl set
// lldpd flags .../rcctl enable lldpd/rcctl start lldpd all fail outright:
// "rcctl: service lldpd does not exist") nor query (no lldpcli) — exactly
// the confusing crash a real bug report hit before this exclusion existed.
// The ports package's own /usr/local install is unaffected and still
// found normally.
func lldpdBinary() string {
	if runtime.GOOS == "openbsd" {
		return firstExisting("/usr/local/sbin/lldpd", "/usr/local/bin/lldpd")
	}
	return firstExisting(
		"/usr/sbin/lldpd", "/sbin/lldpd", "/usr/bin/lldpd",
		"/usr/local/sbin/lldpd", "/usr/local/bin/lldpd", "/opt/homebrew/sbin/lldpd",
	)
}

func lldpcliBinary() string {
	return firstExisting(
		"/usr/sbin/lldpcli", "/usr/bin/lldpcli", "/sbin/lldpcli",
		"/usr/local/sbin/lldpcli", "/usr/local/bin/lldpcli", "/opt/homebrew/sbin/lldpcli",
	)
}

// firstExisting returns the first candidate path that exists as a regular
// (non-directory) file, or "" if none do — the same "check a short list of
// real paths" shape sysusers.go/snmp.go's own binary lookups use.
func firstExisting(candidates ...string) string {
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// ApplyLLDP reconciles the on-disk service config and OS service state with
// cfg: runnable (see config.DiscoveryConfig.IsRunnable) means write the
// interface/CDP flags then enable+restart the service; not runnable means
// stop+disable it. Mirrors ApplySNMP's shape and the same "config is truth,
// a reconciliation failure is a note not a rejection" split.
func ApplyLLDP(cfg config.DiscoveryConfig) (bool, string) {
	if ok, hint := LLDPSupported(); !ok {
		return false, hint
	}
	if !cfg.IsRunnable() {
		return lldpServiceStop()
	}
	if ok, hint := writeLLDPFlags(cfg); !ok {
		return false, hint
	}
	return lldpServiceStart()
}

// lldpArgs builds the same argv parapet's own start_lldpd does: -d
// (foreground — needed for the Linux drop-in, which fully replaces
// ExecStart; harmless to compute even where it isn't used, since the BSD
// flag-writers below simply don't include it), -c if any interface has CDP
// on, and -I <comma list> naming every active, validated, non-loopback
// interface (omitted — meaning "all interfaces" — when every known
// interface happens to be active, mirroring parapet's own omission rule,
// though in practice gravinet's sparse config model means this only
// happens if literally every entry in Interfaces is active).
func lldpArgs(cfg config.DiscoveryConfig) []string {
	args := []string{"-d"}
	if cfg.AnyCDP() {
		args = append(args, "-c")
	}
	if ifaces := activeLLDPIfaces(cfg); len(ifaces) > 0 {
		args = append(args, "-I", strings.Join(ifaces, ","))
	}
	return args
}

// openBSDLLDPServiceName returns the rc.d(8) service name gravinet should
// actually pass to rcctl on this OpenBSD host. This can't be hardcoded as
// "lldpd": OpenBSD is mid-migration, release by release, over which
// program that name refers to. 7.8 added a base lldpd(8) with no rc.d
// script of its own — deliberately, so it wouldn't collide with the ports
// net/lldpd package's existing "lldpd" script (see lldpdBinary's own doc
// comment). 7.9 then renamed the ports package's script to "elldpd" —
// specifically, per OpenBSD's own 7.8->7.9 upgrade guide, "to free up the
// rc script name for future use in base." A later release giving base's
// own lldpd(8) an actual "lldpd" script (at which point that name would
// mean something gravinet still can't drive — see lldpdBinary again) is
// the obvious next step in that migration, though not confirmed yet.
//
// Rather than encode that release-by-release schedule here and have it go
// stale the next time OpenBSD moves the name again, this checks the
// filesystem directly for whichever rc.d script actually exists —
// "lldpd" (pre-7.9) or "elldpd" (7.9 and, presumably, later) — which is
// the same thing rcctl itself would need to find to succeed. Falls back
// to "lldpd" when neither is found (lldpd not installed at all, or some
// future rename this hasn't caught up with yet), so an error at least
// names the service an operator would recognize rather than an empty
// string.
func openBSDLLDPServiceName() string {
	return openBSDLLDPServiceNameFrom(fileExists("/etc/rc.d/lldpd"), fileExists("/etc/rc.d/elldpd"))
}

// openBSDLLDPServiceNameFrom is openBSDLLDPServiceName's actual decision,
// taking the two rc.d(8) script checks as plain values rather than
// deriving them internally — the same "take runnable/wantArgv as
// arguments instead of a config" shape strayLLDPProcs above already uses,
// and for the identical reason: it's what makes the decision itself pure
// and directly testable without needing a real /etc/rc.d/lldpd or
// /etc/rc.d/elldpd to exist on whatever machine runs the test suite.
func openBSDLLDPServiceNameFrom(lldpdScriptExists, elldpdScriptExists bool) string {
	if lldpdScriptExists {
		return "lldpd"
	}
	if elldpdScriptExists {
		return "elldpd"
	}
	return "lldpd"
}

func activeLLDPIfaces(cfg config.DiscoveryConfig) []string {
	var out []string
	for _, i := range cfg.Interfaces {
		if i.Name != "lo" && (i.LLDP || i.CDP) && ValidLLDPIface(i.Name) {
			out = append(out, i.Name)
		}
	}
	return out
}

// ValidLLDPIface mirrors parapet's valid_iface exactly: 1–15 ASCII
// alphanumeric characters, '.', '-', '_', or '@' — so an interface name can
// never smuggle in an extra argv token or, on Linux, break out of the
// space-joined systemd drop-in line. Exported so handleSystemLLDP can
// reject an invalid name at the HTTP layer with a clear error, rather than
// only defending against it here by silently dropping it from the active
// list — both matter: silently dropping it is what keeps a bad name from
// ever reaching an argv even if some future caller forgets to validate;
// rejecting it up front is what tells the operator their save didn't do
// what they typed instead of quietly doing less than they asked.
func ValidLLDPIface(name string) bool {
	if name == "" || len(name) > 15 {
		return false
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '-' || r == '_' || r == '@'
		if !ok {
			return false
		}
	}
	return true
}

// writeLLDPFlags delivers cfg's argv to wherever this platform's packaged
// lldpd service reads its extra arguments from.
func writeLLDPFlags(cfg config.DiscoveryConfig) (bool, string) {
	args := lldpArgs(cfg)
	switch runtime.GOOS {
	case "linux":
		return writeLinuxLLDPDropIn(args)
	case "freebsd":
		// -d excluded: FreeBSD's lldpd rc.d script's own base invocation
		// already handles foreground/daemonization; lldpd_flags is appended
		// to that, not a full replacement, unlike the Linux drop-in below.
		flags := strings.Join(args[1:], " ")
		if out, err := exec.Command("sysrc", "lldpd_flags="+flags).CombinedOutput(); err != nil {
			return false, cmdErr("sysrc lldpd_flags", out, err)
		}
		return true, ""
	case "openbsd":
		svc := openBSDLLDPServiceName()
		// rcctl only pre-declares a flags variable for base rc.d scripts.
		// lldpd/elldpd ships via the ports pkg_scripts mechanism instead,
		// where `rcctl set <svc> flags ...` is rejected outright —
		// "rcctl: <svc> is not enabled" — until the service has been
		// enabled at least once; on a host where LLDP is being turned
		// on for the first time, that hasn't happened yet, since enabling
		// only otherwise happens later, in lldpServiceStart, which this
		// flags write always runs before (see ApplyLLDP). Best-effort and
		// idempotent, mirroring the `rcctl enable` calls already made
		// elsewhere in this file.
		exec.Command("rcctl", "enable", svc).Run()
		flags := strings.Join(args[1:], " ")
		if out, err := exec.Command("rcctl", "set", svc, "flags", flags).CombinedOutput(); err != nil {
			return false, cmdErr("rcctl set "+svc+" flags", out, err)
		}
		return true, ""
	case "darwin":
		// No config file or flags var to write here — see this package's
		// doc comment on the root-vs-Homebrew caveat; brew services restart
		// (in lldpServiceStart) is the whole of what darwin gets.
		return true, ""
	default:
		return false, "L2 discovery isn't supported on this operating system"
	}
}

// writeLinuxLLDPDropIn writes a systemd drop-in that clears the packaged
// unit's own ExecStart and replaces it with lldpd plus args — a standard,
// well-documented override mechanism, rather than guessing at whichever
// per-distro /etc/default or /etc/sysconfig convention (if any) the
// packaged unit happens to source extra arguments from. Also pins
// Type=simple explicitly: some distros' lldpd.service ships Type=notify
// (lldpd supports systemd's sd_notify readiness protocol), and -d alone
// doesn't guarantee that handshake happens the way that Type= expects —
// systemd can end up waiting on a notification that never arrives, or
// otherwise disagreeing with the base unit's assumptions about how the
// process behaves. Type=simple matches exactly what -d actually does (stay
// in the foreground; systemd just watches the PID, no handshake needed),
// removing that ambiguity outright rather than depending on whichever
// Type= the packaged unit happened to ship with.
func writeLinuxLLDPDropIn(args []string) (bool, string) {
	bin := lldpdBinary()
	if bin == "" {
		return false, "lldpd isn't installed"
	}
	dir := "/etc/systemd/system/lldpd.service.d"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, "could not create " + dir + ": " + err.Error()
	}
	line := "ExecStart=" + bin
	for _, a := range args {
		line += " " + a
	}
	content := "# Generated by gravinet — do not edit by hand.\n[Service]\nType=simple\nExecStart=\n" + line + "\n"
	path := dir + "/gravinet.conf"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, "could not write " + path + ": " + err.Error()
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return false, cmdErr("systemctl daemon-reload", out, err)
	}
	return true, ""
}

// LLDPJournalHint runs `journalctl -u lldpd.service` and returns a short,
// actionable hint built from the service's most recent log line —
// mirroring parapet's own last_log_line()+crash_hint() pattern
// (src/lldpd.rs) exactly, just reading from the systemd journal instead of
// a file gravinet captured itself, since a service-managed lldpd's stderr
// goes there by default. Without this, a failed start only ever reports
// systemd's own generic "control process exited with error code" — true,
// but useless on its own; the actual reason (an SELinux/AppArmor denial,
// a config problem, a port already in use, ...) only ever lived in the
// journal. Best-effort: any failure to even read the journal just means no
// extra detail is available, never an error of its own.
//
// Exported (not just used internally by lldpServiceStart/RestartLLDPIfRunning
// below) so systemLLDPJSON can also call it directly when the service
// reports active but lldpcli can't reach it — the same live-active-but-
// unreachable state a failed start's error string was already describing,
// just observed a different way, so it deserves the same specific answer
// instead of the generic "check the log yourself" reconcileLLDPNeighborsHint
// used to hand back on its own.
func LLDPJournalHint() string {
	out, err := exec.Command("journalctl", "-u", "lldpd.service", "-n", "20", "--no-pager", "-q").CombinedOutput()
	if err != nil {
		return ""
	}
	tail := lastNonEmptyLine(string(out))
	if tail == "" {
		return ""
	}
	return " — journal: " + tail + lldpCrashHint(tail)
}

// lldpCrashHint looks at a captured log line and returns a short, targeted
// explanation when it recognizes the pattern, or "" when it doesn't. Pure
// (no logging/IO) so the mapping is directly testable without a real
// journal — mirrors parapet's own crash_hint for the identical reason its
// doc comment gives.
func lldpCrashHint(logLine string) string {
	lower := strings.ToLower(logLine)
	switch {
	case strings.Contains(lower, "avc") || strings.Contains(lower, "selinux") || strings.Contains(lower, "permission denied"):
		return " (SELinux may be denying lldpd — check `ausearch -m avc -ts recent` or the SELinux Alert Browser, and consider `audit2allow` for a local policy module if it's a false positive)"
	case strings.Contains(lower, "apparmor"):
		return " (AppArmor may be blocking lldpd — check `journalctl -k | grep -i apparmor` and the lldpd profile under /etc/apparmor.d/)"
	case strings.Contains(lower, "another instance is running") || strings.Contains(lower, "giving up") || strings.Contains(lower, "address already in use"):
		return " (another lldpd instance, or a leftover control socket from one, may already be present — check `pgrep -a lldpd` and `ls /var/run/lldpd.socket /run/lldpd.socket` 2>/dev/null)"
	default:
		return ""
	}
}

// lastNonEmptyLine returns the last non-blank line of s, truncated to a
// reasonable length for an error message.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			r := []rune(t)
			if len(r) > 200 {
				return string(r[:200])
			}
			return t
		}
	}
	return ""
}

// lldpStaleSocketPaths lists every path lldpd's control socket is known to
// live at. /run/lldpd.socket and /var/run/lldpd.socket are what parapet's
// own cleanup already targets (src/lldpd.rs); /run/lldpd/lldpd.socket — a
// subdirectory variant, the shape systemd's RuntimeDirectory=lldpd
// convention produces — was confirmed directly against a real system via
// an SELinux AVC denial naming that exact path (source process lldpd,
// access "connectto", target /run/lldpd/lldpd.socket).
// /var/run/lldpd/lldpd.socket is included defensively for BSD parity
// (unconfirmed, but removing a path that was never there costs nothing).
var lldpStaleSocketPaths = []string{
	"/run/lldpd/lldpd.socket",
	"/run/lldpd.socket",
	"/var/run/lldpd.socket",
	"/var/run/lldpd/lldpd.socket",
}

// removeStaleLLDPSockets best-effort unlinks any leftover lldpd control
// socket immediately before (re)starting the service. Mirrors parapet's
// own stop()-time cleanup and the exact race it documents at length: SIGKILL
// (or any unclean stop — a crash, systemd giving up and force-killing a
// hung stop, ...) gives lldpd zero chance to unlink its own socket file, so
// it can outlive the process that created it. A freshly starting lldpd
// finds that leftover and tries to connect to it first — its own "is
// another instance already running" self-check — which either makes it
// refuse to start outright ("another instance is running... giving up") or
// gets the *attempt* itself denied by SELinux, exactly as directly observed
// against this exact path on a real system (see lldpStaleSocketPaths).
// Removing every candidate ourselves, right before every start, closes
// that race regardless of which failure mode it would otherwise produce.
// Silent and best-effort: gravinet's own process isn't running in lldpd's
// SELinux domain, so it isn't subject to the same policy an lldpd-owned
// unlink would be; a path that doesn't exist (the common case — no prior
// instance crashed) is simply skipped, not an error.
func removeStaleLLDPSockets() {
	removeSocketsAt(lldpStaleSocketPaths)
}

// removeSocketsAt is removeStaleLLDPSockets' actual work, taking the
// candidate list as a parameter so it's directly testable against a
// temp-dir fixture rather than the real, root-owned /run paths.
func removeSocketsAt(paths []string) {
	for _, p := range paths {
		os.Remove(p)
	}
}

// lldpServiceStopQuietly stops the service without disabling it and without
// reporting anything — the first step of a start, so the service manager's
// own instance is gone before reapAllLLDPD signals whatever's left. Stopping
// through the service manager first is what makes that reap safe: killing a
// process the manager still considers its own is what triggers
// Restart=on-failure, which is precisely the five-restarts-in-one-second
// burst a real host's journal showed before it gave up entirely. After a
// clean stop, anything still answering to the name lldpd is genuinely
// untracked, and terminating it can't race anyone.
func lldpServiceStopQuietly() {
	switch runtime.GOOS {
	case "linux":
		exec.Command("systemctl", "stop", "lldpd.service").Run()
	case "freebsd":
		exec.Command("service", "lldpd", "stop").Run()
	case "openbsd":
		exec.Command("rcctl", "stop", openBSDLLDPServiceName()).Run()
	case "darwin":
		exec.Command("brew", "services", "stop", "lldpd").Run()
	}
}

func lldpServiceStart() (bool, string) {
	// Reconcile process reality before starting: stop the tracked instance,
	// terminate every remaining (by then untracked) lldpd, then clear any
	// control socket left behind. Each step exists because of a failure
	// observed on a real host — see lldpServiceStopQuietly, reapLLDPProcs,
	// and removeStaleLLDPSockets respectively. Doing this unconditionally on
	// every start, rather than only when something looks wrong, is the whole
	// point: a stale instance is invisible to every status check gravinet
	// has, so "looks wrong" is not a state it can reliably detect first.
	lldpServiceStopQuietly()
	reapAllLLDPD()
	removeStaleLLDPSockets()

	switch runtime.GOOS {
	case "linux":
		exec.Command("systemctl", "enable", "lldpd.service").Run() // best-effort: starting matters, surviving reboot is a bonus
		if out, err := exec.Command("systemctl", "start", "lldpd.service").CombinedOutput(); err != nil {
			return false, cmdErr("systemctl start lldpd.service", out, err) + LLDPJournalHint()
		}
		return true, ""
	case "freebsd":
		exec.Command("sysrc", "lldpd_enable=YES").Run()
		if out, err := exec.Command("service", "lldpd", "start").CombinedOutput(); err != nil {
			return false, cmdErr("service lldpd start", out, err)
		}
		return true, ""
	case "openbsd":
		svc := openBSDLLDPServiceName()
		exec.Command("rcctl", "enable", svc).Run()
		if out, err := exec.Command("rcctl", "start", svc).CombinedOutput(); err != nil {
			return false, cmdErr("rcctl start "+svc, out, err)
		}
		return true, ""
	case "darwin":
		if out, err := exec.Command("brew", "services", "start", "lldpd").CombinedOutput(); err != nil {
			return false, cmdErr("brew services start lldpd", out, err)
		}
		return true, ""
	default:
		return false, "L2 discovery isn't supported on this operating system"
	}
}

// lldpServiceStop disables and stops the service, then makes sure that
// actually means what it says: any lldpd still running afterward is
// terminated, and any control socket left behind is removed.
//
// The reap is the whole point, and it's what a real host demonstrated the
// need for: with discovery switched off, the page showed "not running"
// (truthfully — the service manager's own instance was gone) directly above
// a warning that one lldpd was still running anyway, left over from an
// earlier configuration the manager never tracked. It kept advertising stale
// settings and holding the control socket indefinitely. Off has to mean off.
//
// Runs even when the service-manager stop reports a failure: a stop that
// partly worked leaving processes behind is exactly the case that most needs
// cleaning up, and the manager's own error is still returned regardless.
func lldpServiceStop() (bool, string) {
	ok, hint := true, ""
	switch runtime.GOOS {
	case "linux":
		if out, err := exec.Command("systemctl", "disable", "--now", "lldpd.service").CombinedOutput(); err != nil {
			ok, hint = false, cmdErr("systemctl disable --now lldpd.service", out, err)
		}
	case "freebsd":
		exec.Command("sysrc", "lldpd_enable=NO").Run()
		if out, err := exec.Command("service", "lldpd", "stop").CombinedOutput(); err != nil && !strings.Contains(strings.ToLower(string(out)), "not running") {
			ok, hint = false, cmdErr("service lldpd stop", out, err)
		}
	case "openbsd":
		svc := openBSDLLDPServiceName()
		exec.Command("rcctl", "disable", svc).Run()
		if out, err := exec.Command("rcctl", "stop", svc).CombinedOutput(); err != nil && !strings.Contains(strings.ToLower(string(out)), "not running") {
			ok, hint = false, cmdErr("rcctl stop "+svc, out, err)
		}
	case "darwin":
		if out, err := exec.Command("brew", "services", "stop", "lldpd").CombinedOutput(); err != nil {
			ok, hint = false, cmdErr("brew services stop lldpd", out, err)
		}
	default:
		return false, "L2 discovery isn't supported on this operating system"
	}

	reapAllLLDPD()
	removeStaleLLDPSockets()
	return ok, hint
}

// LLDPServiceRunning reports whether the lldpd service is currently active.
func LLDPServiceRunning() bool {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("systemctl", "is-active", "--quiet", "lldpd.service").Run() == nil
	case "freebsd":
		return exec.Command("service", "lldpd", "status").Run() == nil
	case "openbsd":
		return exec.Command("rcctl", "check", openBSDLLDPServiceName()).Run() == nil
	case "darwin":
		out, err := exec.Command("brew", "services", "list").CombinedOutput()
		if err != nil {
			return false
		}
		for _, ln := range strings.Split(string(out), "\n") {
			f := strings.Fields(ln)
			if len(f) >= 2 && f[0] == "lldpd" && f[1] == "started" {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// RestartLLDPIfRunning restarts lldpd, but only if it's currently running —
// never starts it fresh, since "should this be running at all" is System >
// LLDP's own config's job to decide, not this function's. For picking
// up an OS-level change lldpd only reads at its own startup: its LLDP
// SysName TLV defaults to this host's hostname, read once when it starts,
// so a rename via System > Resolver leaves it advertising the old name
// until it's restarted — nothing else triggers that on its own.
//
// Removes stale control sockets first, same as lldpServiceStart — a restart
// stops the old instance and starts a new one just like a fresh start does,
// so it's exposed to the identical unclean-stop race removeStaleLLDPSockets'
// own doc comment describes. lldpServiceStart already guarded against this;
// this restart path was missing the same guard, an inconsistency worth
// closing regardless of which path actually produced any one observed
// failure.
func RestartLLDPIfRunning() (bool, string) {
	if !LLDPServiceRunning() {
		return true, ""
	}
	removeStaleLLDPSockets()
	switch runtime.GOOS {
	case "linux":
		if out, err := exec.Command("systemctl", "restart", "lldpd.service").CombinedOutput(); err != nil {
			return false, cmdErr("systemctl restart lldpd.service", out, err) + LLDPJournalHint()
		}
	case "freebsd":
		if out, err := exec.Command("service", "lldpd", "restart").CombinedOutput(); err != nil {
			return false, cmdErr("service lldpd restart", out, err)
		}
	case "openbsd":
		svc := openBSDLLDPServiceName()
		if out, err := exec.Command("rcctl", "restart", svc).CombinedOutput(); err != nil {
			return false, cmdErr("rcctl restart "+svc, out, err)
		}
	case "darwin":
		if out, err := exec.Command("brew", "services", "restart", "lldpd").CombinedOutput(); err != nil {
			return false, cmdErr("brew services restart lldpd", out, err)
		}
	}
	return true, ""
}

// lldpProc is one running lldpd process: its pid, and the full command line
// it was started with (binary plus argv).
type lldpProc struct {
	PID  int
	Argv string
}

// parseLLDPProcs parses `pgrep -a -x lldpd` output — one "pid command line"
// line per process, e.g. "24452 /usr/sbin/lldpd -d -c -I eth0". Pure — no
// IO — so it's directly testable against captured pgrep output, the same
// reason lldpCrashHint above is pure and tested against captured journal
// lines rather than a real journal.
//
// pid 1 and unparseable pids are dropped rather than guessed at: everything
// downstream of this signals what it returns, and there is no scenario where
// this function returning init is a thing worth acting on.
func parseLLDPProcs(pgrepOutput string) []lldpProc {
	var out []lldpProc
	for _, line := range strings.Split(strings.TrimRight(pgrepOutput, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue // a bare pid with no command somehow — skip rather than guess
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 1 {
			continue
		}
		out = append(out, lldpProc{PID: pid, Argv: strings.Join(fields[1:], " ")})
	}
	return out
}

// lldpProcs lists every running lldpd process. -x (exact name match) rather
// than pgrep's default substring behavior, so this can never match some
// unrelated process that merely has "lldpd" somewhere in its name — this
// list gets signalled, so it errs toward missing a process over including
// one that shouldn't be there.
//
// Best-effort: pgrep exiting non-zero (status 1 — nothing matched, the
// common case — or pgrep not being installed at all) reads as "no processes
// found", never an error, for the same reason removeStaleLLDPSockets is
// silent: this is cleanup, and being unable to check is not itself a
// condition worth failing a save over.
//
// On OpenBSD, excludeOpenBSDBaseLLDPD drops any process actually running
// from /usr/sbin/lldpd — the OS's own unrelated base-system daemon (see
// lldpdBinary's own doc comment) — before it ever reaches stray detection.
// This matters even when gravinet's own LLDP is off: strayLLDPProcs
// treats "not runnable" as "every lldpd-named process on the host is a
// stray," which is the right call for a leftover gravinet-managed
// instance but was never meant to reach a completely different daemon an
// operator is running on purpose for their own, unrelated use of
// OpenBSD's built-in LLDP receiver. Exact-name pgrep matching made that
// assumption safe right up until two same-named, unrelated daemons could
// exist on one host at once.
func lldpProcs() []lldpProc {
	out, err := exec.Command("pgrep", "-a", "-x", "lldpd").CombinedOutput()
	if err != nil {
		return nil
	}
	procs := parseLLDPProcs(string(out))
	if runtime.GOOS == "openbsd" {
		procs = excludeOpenBSDBaseLLDPD(procs)
	}
	return procs
}

// excludeOpenBSDBaseLLDPD drops any process whose argv[0] is exactly
// OpenBSD's own base-system lldpd(8) (/usr/sbin/lldpd) from a process
// list otherwise headed for stray-detection/termination — see lldpProcs'
// own doc comment for why this can't just be left to the ordinary argv
// comparison strayLLDPProcs already does. Matches "/usr/sbin/lldpd" alone
// or followed by a space (its own flags); a hypothetical
// "/usr/sbin/lldpd-something-else" binary, which does not exist today,
// would not match. Pure — no process execution — so it's directly
// testable against captured pgrep output, same as parseLLDPProcs above.
func excludeOpenBSDBaseLLDPD(procs []lldpProc) []lldpProc {
	var out []lldpProc
	for _, p := range procs {
		if p.Argv == "/usr/sbin/lldpd" || strings.HasPrefix(p.Argv, "/usr/sbin/lldpd ") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// dedupLLDPArgvs collapses procs to their distinct command lines, for
// reporting. lldpd always runs as two processes sharing one identical
// command line — a privsep monitor (root) and its worker child, exactly the
// 1928776/1928950 pair confirmed against a real host — so one real running
// instance would otherwise be reported as two.
func dedupLLDPArgvs(procs []lldpProc) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range procs {
		if seen[p.Argv] {
			continue
		}
		seen[p.Argv] = true
		out = append(out, p.Argv)
	}
	return out
}

// expectedLLDPArgv is the exact command line this node's config would run
// lldpd with — what writeLinuxLLDPDropIn puts in ExecStart. "" when lldpd
// isn't installed (nothing to compare against).
func expectedLLDPArgv(cfg config.DiscoveryConfig) string {
	bin := lldpdBinary()
	if bin == "" {
		return ""
	}
	return bin + " " + strings.Join(lldpArgs(cfg), " ")
}

// strayLLDPProcs picks out the processes that shouldn't be running: an
// lldpd left over from a previous configuration, or from a stop that didn't
// actually terminate it. Confirmed for real against a host that had both a
// current service-managed instance AND an older orphaned pair still running
// with stale flags from a previous configuration, invisible to both
// LLDPServiceRunning (which only asks the service manager about the ONE
// instance it tracks by name, not by argv) and LLDPNeighbors (which just
// talks to whichever instance currently holds the control socket, and has
// nothing to say about any others quietly still running).
//
// Two rules, deliberately different in strictness:
//
//   - not runnable: nothing should be running at all, so every process is a
//     stray. Platform-independent and certain.
//   - runnable: only meaningful on linux, where gravinet writes the whole
//     ExecStart (see writeLinuxLLDPDropIn) and so knows the exact argv the
//     legitimate instance has. On the BSDs and darwin the flags gravinet
//     writes are *appended* to whatever base invocation the packaged rc.d
//     script already uses, so the real argv contains tokens gravinet never
//     chose — exact-matching there would flag the correctly running instance
//     as a stray and terminate it. Reporting nothing is the right answer
//     over that. Same for wantArgv being "" (lldpd not installed, so there
//     is nothing to compare against): never guess when the comparison isn't
//     available.
//
// Takes runnable/wantArgv rather than deriving them from a config so it's
// pure and directly testable, including the runnable branch, on a machine
// with no lldpd installed at all — which is otherwise exactly the case that
// makes wantArgv empty and silently skips that branch in a test.
func strayLLDPProcs(runnable bool, wantArgv string, procs []lldpProc) []lldpProc {
	if len(procs) == 0 {
		return nil
	}
	if !runnable {
		return procs
	}
	if runtime.GOOS != "linux" || wantArgv == "" {
		return nil
	}
	var strays []lldpProc
	for _, p := range procs {
		if p.Argv != wantArgv {
			strays = append(strays, p)
		}
	}
	return strays
}

// lldpStraysNow is strayLLDPProcs against this host's live process list and
// the given config — the one place the two are brought together.
func lldpStraysNow(cfg config.DiscoveryConfig) []lldpProc {
	return strayLLDPProcs(cfg.IsRunnable(), expectedLLDPArgv(cfg), lldpProcs())
}

// LLDPStrays reports the distinct command lines of every stray lldpd
// process currently running (see strayLLDPProcs). Read-only — for the
// status the page shows. ReapStrayLLDPD is what actually does something
// about them.
func LLDPStrays(cfg config.DiscoveryConfig) []string {
	return dedupLLDPArgvs(lldpStraysNow(cfg))
}

// signalLLDPProcs sends one signal to every listed pid in a single kill(1)
// call. Shells out rather than using os.Process.Signal for the same reason
// everything else in this file shells out — the platform service managers,
// pgrep, journalctl — and to keep signal names out of Go's per-GOOS
// syscall constants on a file that also compiles for Windows.
func signalLLDPProcs(sig string, procs []lldpProc) {
	if len(procs) == 0 {
		return
	}
	args := []string{sig}
	for _, p := range procs {
		args = append(args, strconv.Itoa(p.PID))
	}
	exec.Command("kill", args...).Run() // best-effort; a pid that already exited is not an error worth reporting
}

// lldpProcAlive reports whether a pid still exists, via kill -0 (signal 0 —
// the standard "check, don't actually signal" probe).
func lldpProcAlive(pid int) bool {
	return exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil
}

// lldpReapGrace bounds how long reapLLDPProcs waits for a SIGTERM'd lldpd to
// exit on its own before escalating to SIGKILL. A clean exit is what's
// wanted: lldpd unlinks its own control socket on the way out, which is the
// whole failure this cleanup exists to prevent recreating (see
// removeStaleLLDPSockets). 3s is far longer than lldpd actually needs and
// still short enough that a save doesn't visibly hang on it.
const lldpReapGrace = 3 * time.Second

// reapLLDPProcs terminates the given processes: SIGTERM first, then SIGKILL
// for anything still alive after lldpReapGrace. Returns the distinct command
// lines it acted on.
//
// SIGTERM first specifically so lldpd gets the chance to unlink its own
// control socket — SIGKILL doesn't, which is exactly how a leftover socket
// outlives the process that made it and then blocks the next start with
// "another instance is running... giving up" (the failure a real host's
// journal showed, five times in one second, until systemd gave up entirely).
// removeStaleLLDPSockets still runs after this as the backstop for whatever
// didn't get to clean up after itself.
func reapLLDPProcs(procs []lldpProc) []string {
	if len(procs) == 0 {
		return nil
	}
	acted := dedupLLDPArgvs(procs)
	signalLLDPProcs("-TERM", procs)

	deadline := time.Now().Add(lldpReapGrace)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if !anyLLDPProcAlive(procs) {
			return acted
		}
	}

	var survivors []lldpProc
	for _, p := range procs {
		if lldpProcAlive(p.PID) {
			survivors = append(survivors, p)
		}
	}
	signalLLDPProcs("-KILL", survivors)
	time.Sleep(100 * time.Millisecond) // let the kernel actually reap them before anything re-checks
	return acted
}

func anyLLDPProcAlive(procs []lldpProc) bool {
	for _, p := range procs {
		if lldpProcAlive(p.PID) {
			return true
		}
	}
	return false
}

// reapAllLLDPD terminates every running lldpd process unconditionally.
// Called only from the two places where the service manager's own instance
// has just been stopped (lldpServiceStart/lldpServiceStop below), so
// "everything still running" and "everything untracked" are the same set at
// that moment — no argv comparison needed or wanted, unlike the selective
// ReapStrayLLDPD below.
func reapAllLLDPD() []string {
	return reapLLDPProcs(lldpProcs())
}

// ReapStrayLLDPD terminates only the lldpd processes that don't belong (see
// strayLLDPProcs) and leaves a correctly-configured instance alone.
// Returns what it terminated and what — if anything — is still there
// afterward, so a caller can log the difference; a non-empty remaining list
// means the processes genuinely couldn't be killed (not gravinet's to
// signal, a different service manager restarting them, ...) rather than
// simply not having been tried.
//
// Exported for the once-at-startup cleanup in main.go: unlike
// lldpServiceStart/Stop, that path must NOT disturb an instance that
// matches config, since gravinet restarts far more often than an operator
// wants link-layer discovery to blink — the same reasoning this package's
// doc comment gives for lldpd being an OS service rather than a child
// process in the first place.
func ReapStrayLLDPD(cfg config.DiscoveryConfig) (killed, remaining []string) {
	strays := lldpStraysNow(cfg)
	if len(strays) == 0 {
		return nil, nil
	}
	killed = reapLLDPProcs(strays)
	removeStaleLLDPSockets()
	return killed, dedupLLDPArgvs(lldpStraysNow(cfg))
}

// LLDPNeighbor is one discovered link-layer neighbor, as reported by
// `lldpcli show neighbors`. Mirrors parapet's discovery_row shape, plus
// Protocol (parapet has no equivalent — it never distinguished LLDP from
// CDP in its own neighbor table, since it only ever spoke LLDP itself).
type LLDPNeighbor struct {
	LocalIface string
	SystemName string
	Port       string
	MgmtIP     string
	// Protocol is lldpd's own "via" field verbatim — "LLDP", "CDPv1", or
	// "CDPv2" in practice, since AnyCDP/lldpArgs only ever turn on those two
	// protocols (lldpd itself also understands EDP/FDP/SONMP, but nothing in
	// gravinet's own config surface ever enables them, so those values are
	// never actually produced here even though lldpd's binary supports
	// them). Empty if a fixture or lldpd version omits "via" — Monitor › L2
	// Peers falls back to showing "\u2013" rather than guessing.
	Protocol string
}

// LLDPNeighbors queries the running lldpd (via lldpcli, which talks to
// whatever instance is running over its control socket regardless of who
// launched it) for its current neighbor table. Returns (neighbors,
// available, hint) — available is false with a reason when lldpcli isn't
// installed or its connect attempt failed; distinct from (available: true,
// empty neighbors), which just means no neighbors have been heard from yet.
//
// The connect-failure hint is deliberately neutral ("could not connect"),
// not "lldpd is not running": this function has no way to actually know
// *why* the connect failed — the service could be genuinely down, still
// starting up, or up-but-unreachable (a permissions/SELinux denial on the
// socket, a non-standard control-socket location, ...) — and asserting
// "not running" as if it were a known fact directly contradicted a
// service-status tag showing "running" right next to it, reported as a
// real, confusing bug. systemLLDPJSON (which also knows the service's
// own active/inactive state from a separate, independent check) is what
// composes the final, contradiction-aware hint the page actually shows.
func LLDPNeighbors() ([]LLDPNeighbor, bool, string) {
	cli := lldpcliBinary()
	if cli == "" {
		return nil, false, "lldpd is not installed"
	}
	out, err := exec.Command(cli, "-f", "json", "show", "neighbors").CombinedOutput()
	if err != nil {
		return nil, false, "could not connect to lldpd's control interface"
	}
	rows, err := parseLLDPNeighborsJSON(out)
	if err != nil {
		return nil, false, "could not parse lldpd output"
	}
	return rows, true, ""
}

// parseLLDPNeighborsJSON parses `lldpcli -f json show neighbors`' output.
// Pure and side-effect-free — no process execution — so it's directly
// testable against fixture JSON, independent of whether lldpd/lldpcli is
// even installed on whatever machine runs those tests.
//
// lldpd JSON shape: { "lldp": { "interface": { "<if>": {...} } } }.
// Different lldpd versions wrap "interface" as an object or an array of
// single-key objects; handle both, exactly as parapet's own comment
// documents needing to.
func parseLLDPNeighborsJSON(data []byte) ([]LLDPNeighbor, error) {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	var rows []LLDPNeighbor
	lldp, _ := v["lldp"].(map[string]any)
	switch ifaceNode := lldp["interface"].(type) {
	case map[string]any:
		for ifname, body := range ifaceNode {
			rows = append(rows, lldpNeighborRow(ifname, body))
		}
	case []any:
		for _, entry := range ifaceNode {
			if m, ok := entry.(map[string]any); ok {
				for ifname, body := range m {
					rows = append(rows, lldpNeighborRow(ifname, body))
				}
			}
		}
	}
	return rows, nil
}

func lldpNeighborRow(ifname string, body any) LLDPNeighbor {
	bm, _ := body.(map[string]any)
	chassis := bm["chassis"]
	port := bm["port"]

	row := LLDPNeighbor{LocalIface: ifname, SystemName: lldpFirstName(chassis)}
	if via, ok := bm["via"].(string); ok {
		row.Protocol = via
	}

	if pm, ok := port.(map[string]any); ok {
		if descr, ok := pm["descr"].(string); ok && descr != "" {
			row.Port = descr
		} else if idm, ok := pm["id"].(map[string]any); ok {
			if val, ok := idm["value"].(string); ok {
				row.Port = val
			}
		}
		if row.Port == "" {
			row.Port = lldpFirstName(port)
		}
	}

	if cm, ok := chassis.(map[string]any); ok {
		if first, ok := lldpFirstMapValue(cm).(map[string]any); ok {
			if ip, ok := first["mgmt-ip"].(string); ok {
				row.MgmtIP = ip
			}
		}
	}

	return row
}

// lldpFirstName pulls a human name out of a chassis/port JSON node, which
// lldpd represents either as {"<name>": {...}} (the common case — the sole
// key IS the name) or with an explicit id/name/descr field. Recognized
// field names are checked first regardless of map order (Go's map
// iteration order is unspecified, unlike the ordered map parapet's own
// first_name walks); this only diverges from parapet's exact "first key"
// behavior if a chassis/port object has more than one top-level key with
// none of id/name/descr among them, a shape lldpd isn't known to produce.
func lldpFirstName(node any) string {
	m, ok := node.(map[string]any)
	if !ok {
		if s, ok := node.(string); ok {
			return s
		}
		return ""
	}
	for _, k := range []string{"id", "name", "descr"} {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	for k := range m {
		return k
	}
	return ""
}

func lldpFirstMapValue(m map[string]any) any {
	for _, v := range m {
		return v
	}
	return nil
}
