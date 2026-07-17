// Package service handles running gravinet as an operating-system service:
// generating unit/plist/SCM definitions, installing/uninstalling them, readiness
// notification (systemd sd_notify), and the Windows service-control dispatcher.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Options describes the service to install.
type Options struct {
	Name        string // service id, e.g. "gravinet"
	DisplayName string
	Description string
	ExecPath    string // absolute path to the binary
	ConfigPath  string // value for `-config`
	User        string // run-as user (linux/darwin); empty = root
}

// Defaults fills unset fields from the current binary and conventional paths.
func Defaults() Options {
	exe, _ := os.Executable()
	if exe != "" {
		exe, _ = filepath.Abs(exe)
	}
	cfg := "/etc/gravinet/config.json"
	switch runtime.GOOS {
	case "windows":
		cfg = `C:\ProgramData\gravinet\config.json`
	case "freebsd":
		// FreeBSD convention keeps third-party config under /usr/local/etc,
		// unlike Linux/macOS where /etc is used directly for it too.
		cfg = "/usr/local/etc/gravinet/config.json"
	}
	return Options{
		Name:        "gravinet",
		DisplayName: "gravinet",
		Description: "[gravinet] full-mesh encrypted overlay VPN daemon",
		ExecPath:    exe,
		ConfigPath:  cfg,
	}
}

func (o Options) withDefaults() Options {
	d := Defaults()
	if o.Name == "" {
		o.Name = d.Name
	}
	if o.DisplayName == "" {
		o.DisplayName = d.DisplayName
	}
	if o.Description == "" {
		o.Description = d.Description
	}
	if o.ExecPath == "" {
		o.ExecPath = d.ExecPath
	}
	if o.ConfigPath == "" {
		o.ConfigPath = d.ConfigPath
	}
	return o
}

// SystemdUnit renders a minimal Type=notify systemd unit. It deliberately
// carries NO capability/privilege hardening directives (NoNewPrivileges,
// CapabilityBoundingSet, AmbientCapabilities): the daemon runs as root, needs
// CAP_NET_ADMIN for TUN, and the web admin's PAM login must read the shadow
// database — and those directives break PAM. Keep this unit plain.
func SystemdUnit(o Options) string {
	o = o.withDefaults()
	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", o.Description)
	b.WriteString("After=network-online.target\nWants=network-online.target\n")
	// Disable the start-rate limiter so the unit never gives up restarting
	// under a rapid crash/restart loop. StartLimitIntervalSec is a [Unit]
	// directive (systemd ignores it under [Service]). Kept in sync with
	// install/gravinet.service.
	b.WriteString("StartLimitIntervalSec=0\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=notify\n")
	fmt.Fprintf(&b, "ExecStart=%s run -config %s\n", o.ExecPath, o.ConfigPath)
	if o.User != "" {
		fmt.Fprintf(&b, "User=%s\n", o.User)
	}
	// Restart on any exit (not just failures) so the daemon is always running
	// unless an operator explicitly stopped it; a `systemctl stop` is still a
	// clean stop and does not loop-restart.
	b.WriteString("Restart=always\nRestartSec=8\n")
	// Guarantee the stop phase terminates. This unit is Type=notify, so
	// `systemctl restart` waits for the old process to exit before starting
	// the replacement; without a bounded stop, a teardown step wedged on the
	// kernel or a subprocess hangs the restart indefinitely. The daemon's own
	// shutdown watchdog (shutdownGrace) force-exits just under this timeout,
	// so it exits cleanly (with a log line) first in the normal stuck case;
	// this is the outer backstop for when even that can't run — systemd
	// escalates SIGTERM -> SIGKILL after the timeout, and SendSIGKILL=yes
	// ensures that final kill is never disabled. Kept in sync with
	// install/gravinet.service.
	b.WriteString("TimeoutStopSec=8\nSendSIGKILL=yes\n\n")
	b.WriteString("[Install]\nWantedBy=multi-user.target\n")
	return b.String()
}

// LaunchdLabel returns the launchd service label gravinet installs under
// (e.g. "com.gravinet.daemon"), the single place this is derived so
// LaunchdPlist, InstallPath, and Restart/CanRestart can't drift apart.
func LaunchdLabel(o Options) string {
	return "com." + o.withDefaults().Name + ".daemon"
}

// LaunchdPlist renders a macOS launchd daemon plist.
func LaunchdPlist(o Options) string {
	o = o.withDefaults()
	label := LaunchdLabel(o)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key>\n  <string>%s</string>\n", label)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range []string{o.ExecPath, "run", "-config", o.ConfigPath} {
		fmt.Fprintf(&b, "    <string>%s</string>\n", a)
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key>\n  <string>/var/log/%s.err.log</string>\n", o.Name)
	fmt.Fprintf(&b, "  <key>StandardOutPath</key>\n  <string>/var/log/%s.out.log</string>\n", o.Name)
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// RcdScript renders a FreeBSD rc.d script. gravinet doesn't daemonize itself
// (same foreground-process model it uses under systemd/launchd), so this
// wraps it with base-system daemon(8) rather than assuming rc.subr can
// supervise it directly.
//
// Uses daemon(8)'s -P (supervisor pidfile) for rc.subr's own default
// pidfile-based logic (status/poll, and the base of our stop override below),
// not -p (child pidfile), with -r (auto-restart on unexpected death) —
// deliberately, not the more obvious choice. daemon(8)'s own manual page
// warns about exactly the bug the other way round produces: with -p as the
// *only* pidfile, `service gravinet stop` would signal gravinet directly —
// which daemon(8) (still watching its child under -r) then sees as an
// unexpected death and restarts, turning "stop" into "stop, then immediately
// start again". -P puts the supervisor's own pid in the file rc.subr reads
// instead, so a plain SIGTERM to it forwards to gravinet (a documented
// behavior of -r/-p/-P) and daemon(8) correctly treats the resulting exit as
// intentional rather than a crash to recover from.
//
// A *separate* -p child_pidfile is also passed — never wired into rc.subr's
// own pidfile variable, so none of the above changes — solely so the stop
// override below has a way to reach gravinet's own pid directly if graceful
// shutdown doesn't finish in time. See that override's comment for why it
// exists at all: rc.subr's default stop has no timeout on its wait, and a
// wedged gravinet shutdown would otherwise hang `service gravinet stop`
// (and install-freebsd.sh's upgrade-in-place step, which calls it) forever.
func RcdScript(o Options) string {
	o = o.withDefaults()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n#\n")
	fmt.Fprintf(&b, "# PROVIDE: %s\n", o.Name)
	b.WriteString("# REQUIRE: NETWORKING\n# KEYWORD: shutdown\n#\n")
	fmt.Fprintf(&b, "# Add this to /etc/rc.conf to enable at boot:\n#   %s_enable=\"YES\"\n#\n", o.Name)
	b.WriteString("# Or: sysrc " + o.Name + "_enable=YES\n\n")
	b.WriteString(". /etc/rc.subr\n\n")
	fmt.Fprintf(&b, "name=%q\n", o.Name)
	fmt.Fprintf(&b, "desc=%q\n", o.Description)
	fmt.Fprintf(&b, "rcvar=%s_enable\n\n", o.Name)
	fmt.Fprintf(&b, "load_rc_config %q\n\n", o.Name)
	fmt.Fprintf(&b, ": ${%s_enable:=\"NO\"}\n\n", o.Name)
	fmt.Fprintf(&b, "pidfile=\"/var/run/%s.pid\"\n", o.Name)
	fmt.Fprintf(&b, "child_pidfile=\"/var/run/%s.child.pid\"\n", o.Name)
	b.WriteString(`command="/usr/sbin/daemon"` + "\n")
	fmt.Fprintf(&b, "command_args=\"-c -f -r -p ${child_pidfile} -P ${pidfile} -t %s %s run -config %s\"\n\n", o.Name, o.ExecPath, o.ConfigPath)

	// Bounded stop: rc.subr's default stop_cmd (kill $sig_stop, then
	// wait_for_pids with NO timeout) waits forever if the process it signaled
	// never exits. The normal path here is identical to that default — same
	// pid, same signal, same wait_for_pids call at the end — so a healthy
	// gravinet stops exactly as before; the only difference is a 10-second
	// cap, past which this escalates to SIGKILL rather than hanging. It kills
	// the child pid first (guaranteeing gravinet itself dies, using the
	// separate -p pidfile above) and the supervisor pid second (so -r's
	// auto-restart can't win a race and spawn a new one first). This is a
	// safety net for gravinet wedging during its own shutdown, not a
	// substitute for fixing that if it happens — it trades a hang for a clear
	// "did not stop in time; killing it" message instead.
	fmt.Fprintf(&b, "%s_stop()\n{\n", o.Name)
	b.WriteString("\trc_pid=$(check_pidfile \"$pidfile\" \"$command\")\n")
	b.WriteString("\tif [ -z \"$rc_pid\" ]; then\n")
	fmt.Fprintf(&b, "\t\techo \"%s not running? (check $pidfile).\"\n", o.Name)
	b.WriteString("\t\treturn 1\n")
	b.WriteString("\tfi\n")
	fmt.Fprintf(&b, "\techo \"Stopping %s.\"\n", o.Name)
	b.WriteString("\tkill -${sig_stop:-TERM} \"$rc_pid\" 2>/dev/null\n")
	b.WriteString("\ttries=0\n")
	b.WriteString("\twhile kill -0 \"$rc_pid\" 2>/dev/null; do\n")
	b.WriteString("\t\ttries=$((tries + 1))\n")
	b.WriteString("\t\tif [ \"$tries\" -ge 20 ]; then\n")
	fmt.Fprintf(&b, "\t\t\techo \"%s did not stop within 10s; killing it.\"\n", o.Name)
	b.WriteString("\t\t\tchild_pid=$(cat \"$child_pidfile\" 2>/dev/null)\n")
	b.WriteString("\t\t\t[ -n \"$child_pid\" ] && kill -KILL \"$child_pid\" 2>/dev/null\n")
	b.WriteString("\t\t\tkill -KILL \"$rc_pid\" 2>/dev/null\n")
	b.WriteString("\t\t\tbreak\n")
	b.WriteString("\t\tfi\n")
	b.WriteString("\t\tsleep 0.5\n")
	b.WriteString("\tdone\n")
	b.WriteString("\twait_for_pids \"$rc_pid\"\n")
	b.WriteString("\trm -f \"$pidfile\" \"$child_pidfile\"\n")
	b.WriteString("}\n")
	fmt.Fprintf(&b, "stop_cmd=\"%s_stop\"\n\n", o.Name)

	b.WriteString(`run_rc_command "$1"` + "\n")
	return b.String()
}

// OpenBSDRcScript renders an OpenBSD rc.d(8) script driven by rcctl(8).
//
// It is deliberately far smaller than the FreeBSD RcdScript above, for the
// same reason tun_openbsd.go is smaller than tun_freebsd.go: OpenBSD's base
// rc.subr already does the work FreeBSD needs daemon(8) and a custom stop
// override for. gravinet runs in the foreground (identical to its
// systemd/launchd model), and OpenBSD has no daemon(8) supervisor — but
// rc.subr backgrounds a non-forking daemon itself when rc_bg=YES, and its
// default stop (SIGTERM, then reap) already reaches gravinet directly because
// there's no intermediate supervisor process to confuse a plain signal the
// way FreeBSD's -r/-p/-P arrangement can. So the elaborate bounded-stop and
// double-pidfile dance the FreeBSD script needs simply doesn't arise here;
// `rcctl stop` (or `rcctl -f stop` to force) is enough.
//
// daemon_flags carries the -config default; rcctl(8) lets an operator
// override it in rc.conf.local (`rcctl set gravinet flags ...`) without
// editing this file, the same convention OpenBSD packages use.
func OpenBSDRcScript(o Options) string {
	o = o.withDefaults()
	var b strings.Builder
	b.WriteString("#!/bin/ksh\n#\n")
	fmt.Fprintf(&b, "# %s\n#\n", o.Description)
	fmt.Fprintf(&b, "# Enable at boot:  rcctl enable %s\n", o.Name)
	fmt.Fprintf(&b, "# Start now:       rcctl start %s\n\n", o.Name)
	fmt.Fprintf(&b, "daemon=%q\n", o.ExecPath)
	fmt.Fprintf(&b, "daemon_flags=%q\n", fmt.Sprintf("run -config %s", o.ConfigPath))
	if o.User != "" {
		fmt.Fprintf(&b, "daemon_user=%q\n", o.User)
	}
	b.WriteString("\n. /etc/rc.d/rc.subr\n\n")
	// gravinet doesn't fork; rc.subr must background it. This is OpenBSD's
	// built-in equivalent of the FreeBSD daemon(8) wrapper.
	b.WriteString("rc_bg=YES\n\n")
	b.WriteString("rc_cmd $1\n")
	return b.String()
}

// WindowsInstallCommands renders the sc.exe commands to register the service.
func WindowsInstallCommands(o Options) string {
	o = o.withDefaults()
	bin := fmt.Sprintf(`\"%s\" run -config \"%s\"`, o.ExecPath, o.ConfigPath)
	return fmt.Sprintf(
		"sc.exe create %s binPath= \"%s\" start= auto DisplayName= \"%s\"\n"+
			"sc.exe description %s \"%s\"\n"+
			// Recovery: restart on the first, second, and subsequent failures,
			// with a short escalating backoff (5s/10s/30s); reset the failure
			// count after a day of stability. The failure-actions flag makes
			// these also fire on a non-zero-exit stop, which is how a restart
			// after a settings change comes back automatically (the service
			// reports a failure exit rather than deadlocking on a self-restart).
			// The daemon also re-applies these at startup, so an existing
			// install repairs itself without needing this rerun.
			"sc.exe failure %s reset= 86400 actions= restart/5000/restart/10000/restart/30000\n"+
			"sc.exe failureflag %s 1\n"+
			"sc.exe start %s\n",
		o.Name, bin, o.DisplayName, o.Name, o.Description, o.Name, o.Name, o.Name)
}

// Definition returns the service definition appropriate to the current OS.
func Definition(o Options) string {
	switch runtime.GOOS {
	case "linux":
		return SystemdUnit(o)
	case "darwin":
		return LaunchdPlist(o)
	case "windows":
		return WindowsInstallCommands(o)
	case "freebsd":
		return RcdScript(o)
	case "openbsd":
		return OpenBSDRcScript(o)
	default:
		return SystemdUnit(o)
	}
}

// InstallPath is where the definition belongs on the current OS.
func InstallPath(o Options) string {
	o = o.withDefaults()
	switch runtime.GOOS {
	case "linux":
		return filepath.Join("/etc/systemd/system", o.Name+".service")
	case "darwin":
		return filepath.Join("/Library/LaunchDaemons", LaunchdLabel(o)+".plist")
	case "freebsd":
		return filepath.Join("/usr/local/etc/rc.d", o.Name)
	case "openbsd":
		// OpenBSD keeps rc.d scripts in /etc/rc.d (both base and packages),
		// not the /usr/local/etc/rc.d path FreeBSD uses.
		return filepath.Join("/etc/rc.d", o.Name)
	default:
		return ""
	}
}

// Install writes the service definition (linux/darwin) and returns the path and
// the follow-up command the operator should run. On Windows it returns the
// sc.exe commands to run (writing to the SCM requires those commands).
func Install(o Options) (path, next string, err error) {
	o = o.withDefaults()
	switch runtime.GOOS {
	case "linux":
		p := InstallPath(o)
		if err := os.WriteFile(p, []byte(SystemdUnit(o)), 0o644); err != nil {
			return "", "", err
		}
		return p, fmt.Sprintf("systemctl daemon-reload && systemctl enable --now %s", o.Name), nil
	case "darwin":
		p := InstallPath(o)
		if err := os.WriteFile(p, []byte(LaunchdPlist(o)), 0o644); err != nil {
			return "", "", err
		}
		return p, fmt.Sprintf("launchctl load %s", p), nil
	case "windows":
		return "", WindowsInstallCommands(o), nil
	case "freebsd":
		p := InstallPath(o)
		// rc.d scripts must be directly executable, unlike the systemd unit
		// and launchd plist above, which are data files their respective
		// service managers read rather than run.
		if err := os.WriteFile(p, []byte(RcdScript(o)), 0o755); err != nil {
			return "", "", err
		}
		return p, fmt.Sprintf("sysrc %s_enable=YES && service %s start", o.Name, o.Name), nil
	case "openbsd":
		p := InstallPath(o)
		// Like FreeBSD's rc.d script, an OpenBSD rc.d script must be directly
		// executable — rc.subr runs it.
		if err := os.WriteFile(p, []byte(OpenBSDRcScript(o)), 0o755); err != nil {
			return "", "", err
		}
		return p, fmt.Sprintf("rcctl enable %s && rcctl start %s", o.Name, o.Name), nil
	default:
		return "", "", fmt.Errorf("service install not supported on %s", runtime.GOOS)
	}
}

// Uninstall removes the definition (linux/darwin) or returns the sc.exe delete
// command (windows).
func Uninstall(o Options) (next string, err error) {
	o = o.withDefaults()
	switch runtime.GOOS {
	case "linux":
		_ = os.Remove(InstallPath(o))
		return fmt.Sprintf("systemctl disable %s", o.Name), nil
	case "darwin":
		p := InstallPath(o)
		_ = os.Remove(p)
		return fmt.Sprintf("launchctl unload %s", p), nil
	case "windows":
		return fmt.Sprintf("sc.exe stop %s && sc.exe delete %s", o.Name, o.Name), nil
	case "freebsd":
		_ = os.Remove(InstallPath(o))
		return fmt.Sprintf("service %s stop; sysrc -x %s_enable", o.Name, o.Name), nil
	case "openbsd":
		_ = os.Remove(InstallPath(o))
		return fmt.Sprintf("rcctl stop %s; rcctl disable %s", o.Name, o.Name), nil
	default:
		return "", fmt.Errorf("service uninstall not supported on %s", runtime.GOOS)
	}
}

// CanRestart reports whether Restart is likely to work on this host, without
// actually restarting anything — checking for the relevant service-manager
// binary and that gravinet is actually registered as a service under it. Used
// to give an immediate, accurate error (e.g. from the web admin, before it
// tells the browser a restart is in progress) instead of optimistically
// claiming a restart is happening and only finding out afterward that it
// silently wasn't going to work.
func CanRestart() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return false, "restart the daemon to apply (no systemd here)"
		}
		if exec.Command("systemctl", "cat", "gravinet").Run() != nil {
			return false, "restart the daemon to apply (gravinet isn't a systemd service here)"
		}
		return true, ""
	case "darwin":
		if _, err := exec.LookPath("launchctl"); err != nil {
			return false, "restart the daemon to apply (launchctl not found)"
		}
		if exec.Command("launchctl", "print", "system/"+LaunchdLabel(Defaults())).Run() != nil {
			return false, "restart the daemon to apply (gravinet isn't a loaded launchd service here)"
		}
		return true, ""
	case "windows":
		if _, err := exec.LookPath("powershell"); err != nil {
			return false, "restart the service to apply (powershell not found)"
		}
		if exec.Command("powershell", "-NoProfile", "-Command", "Get-Service gravinet").Run() != nil {
			return false, "restart the service to apply (gravinet isn't a registered Windows service here)"
		}
		return true, ""
	case "freebsd":
		if _, err := os.Stat("/usr/local/etc/rc.d/gravinet"); err != nil {
			return false, "restart the daemon to apply (gravinet isn't an rc.d service here)"
		}
		return true, ""
	case "openbsd":
		if _, err := os.Stat("/etc/rc.d/gravinet"); err != nil {
			return false, "restart the daemon to apply (gravinet isn't an rc.d service here)"
		}
		return true, ""
	default:
		return false, "restart the daemon to apply"
	}
}

// detachedRestart launches name(args...) as an independent, backgrounded
// process (Start, not Run) and returns as soon as it has launched — it does
// NOT wait for that process to finish, or for the restart it performs to
// land. See Restart's doc comment for why that distinction is the entire
// point: every caller in this file that asks the platform service manager to
// restart gravinet is doing so from *inside the very process being
// restarted*, and Run()ing the restart command blocks this goroutine waiting
// for a stop-then-start cycle whose stop half is itself waiting for this
// process to exit — a self-wait neither half can resolve. Start()ing it
// instead, and giving the old process a couple of seconds' head start to
// finish exiting on its own, breaks that cycle: the restart runs on its own
// clock, independent of whether this goroutine (or this whole process) is
// still around to see it finish.
func detachedRestart(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

// Restart restarts the installed gravinet service via the platform service
// manager. Returns (true, "") on success, or (false, hint) describing how to
// restart by hand. Used by the CLI and the web admin so both behave
// identically — including on macOS (launchctl kickstart) and Windows
// (Restart-Service), not just Linux.
//
// Every platform branch below launches its restart command via
// detachedRestart rather than exec.Command(...).Run(). This isn't a style
// preference: systemctl/launchctl/service(8)/rcctl restart are all, like
// PowerShell's Restart-Service (see the windows case below for the fullest
// explanation), synchronous stop-then-start cycles — and the stop half waits
// for gravinet's own main process to exit. When Restart is called from
// gravinet's own restart-on-underlay-change or restart-on-suspend-resume
// path (cmd/gravinet's runBody, after selfRestart's in-place re-exec has
// already failed), that main process IS this one, already mid-shutdown in
// the very same goroutine that would go on to Run() the restart command —
// so a blocking call here waits on a stop phase that is itself waiting on
// this goroutine to finish waiting, forever, short of the service manager's
// own stop timeout (if any) eventually forcing the issue with SIGKILL. The
// process doesn't crash or exit in that window — systemctl/etc. and the OS
// still show it "running" — it just sits there with its listeners, TUN
// device, and mesh sessions already torn down by the shutdown that ran
// before this call, deaf to everything until something external kills it.
// Exactly the class of bug the FreeBSD rc.d script's bounded stop_cmd
// override (see RcdScript's doc comment) and the windows branch below were
// already written to avoid; the other platforms are brought in line with
// them here.
func Restart() (bool, string) {
	if ok, hint := CanRestart(); !ok {
		return false, hint
	}
	switch runtime.GOOS {
	case "linux":
		if err := detachedRestart("sh", "-c", "sleep 2; systemctl restart gravinet"); err != nil {
			return false, "couldn't restart automatically — run: sudo systemctl restart gravinet"
		}
		return true, ""
	case "darwin":
		label := LaunchdLabel(Defaults())
		if err := detachedRestart("sh", "-c", "sleep 2; launchctl kickstart -k system/"+label); err != nil {
			return false, "couldn't restart automatically — run: sudo launchctl kickstart -k system/" + label
		}
		return true, ""
	case "windows":
		// PowerShell's Restart-Service is the same kind of synchronous
		// stop-then-start cycle as the other platforms' commands (see this
		// function's doc comment) — Stop-Service polls the SCM until it
		// observes this process reporting SERVICE_STOPPED, so a blocking call
		// here would wait on this process's own death from inside itself.
		// detachedRestart avoids that the same way it does everywhere else.
		// One Windows-specific note: Start() only confirms the script
		// launched, not that Restart-Service inside it actually succeeded —
		// the web admin's own poll-for-a-new-boot-id loop (see
		// quietRestart/quietPollBack in ui.go) is what actually confirms the
		// restart landed, and is a more honest check than trusting this exit
		// code ever was, since it only fires after a real fresh process with
		// a fresh boot id answers /api/ping.
		if err := detachedRestart("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
			"Start-Sleep -Seconds 2; Restart-Service gravinet"); err != nil {
			return false, "couldn't restart automatically — run (elevated): Restart-Service gravinet"
		}
		return true, ""
	case "freebsd":
		if err := detachedRestart("sh", "-c", "sleep 2; service gravinet restart"); err != nil {
			return false, "couldn't restart automatically — run: service gravinet restart"
		}
		return true, ""
	case "openbsd":
		if err := detachedRestart("sh", "-c", "sleep 2; rcctl restart gravinet"); err != nil {
			return false, "couldn't restart automatically — run: rcctl restart gravinet"
		}
		return true, ""
	default:
		return false, "restart the daemon to apply"
	}
}
