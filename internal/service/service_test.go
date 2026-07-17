package service

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testOpts() Options {
	return Options{
		Name:        "gravinet",
		DisplayName: "gravinet",
		Description: "test daemon",
		ExecPath:    "/usr/local/bin/gravinet",
		ConfigPath:  "/etc/gravinet/config.json",
	}
}

func TestSystemdUnit(t *testing.T) {
	o := testOpts()
	o.User = "meshd"
	u := SystemdUnit(o)
	for _, want := range []string{
		"Type=notify",
		"ExecStart=/usr/local/bin/gravinet run -config /etc/gravinet/config.json",
		"User=meshd",
		"WantedBy=multi-user.target",
		// Restart-guarantee directives: without a bounded stop, a wedged
		// teardown hangs `systemctl restart` on this Type=notify unit
		// indefinitely; without an always-restart + disabled start limiter,
		// the unit can give up. These must stay present. See SystemdUnit.
		"Restart=always",
		"TimeoutStopSec=",
		"SendSIGKILL=yes",
		"StartLimitIntervalSec=0",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("systemd unit missing %q\n%s", want, u)
		}
	}
	// These hardening directives break PAM web-admin login. They must never come
	// back.
	for _, banned := range []string{
		"NoNewPrivileges",
		"CapabilityBoundingSet",
		"AmbientCapabilities",
		"ProtectSystem",
		"ProtectHome",
		"PrivateUsers",
		"PrivateTmp",
		"RestrictSUIDSGID",
	} {
		if strings.Contains(u, banned) {
			t.Errorf("systemd unit must NOT contain %q (it breaks PAM auth)\n%s", banned, u)
		}
	}
	// StartLimitIntervalSec is a [Unit] directive; systemd ignores it (with a
	// warning) under [Service]. Assert it appears before the [Service] header.
	unitIdx := strings.Index(u, "[Unit]")
	svcIdx := strings.Index(u, "[Service]")
	sliIdx := strings.Index(u, "StartLimitIntervalSec")
	if sliIdx < 0 || unitIdx < 0 || svcIdx < 0 || !(sliIdx > unitIdx && sliIdx < svcIdx) {
		t.Errorf("StartLimitIntervalSec must be in the [Unit] section (before [Service]); got:\n%s", u)
	}
}

func TestLaunchdPlist(t *testing.T) {
	p := LaunchdPlist(testOpts())
	for _, want := range []string{
		"<key>Label</key>",
		"com.gravinet.daemon",
		"<string>run</string>",
		"<string>-config</string>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q\n%s", want, p)
		}
	}
}

func TestLaunchdLabelConsistentAcrossUses(t *testing.T) {
	o := testOpts()
	label := LaunchdLabel(o)
	if label != "com.gravinet.daemon" {
		t.Fatalf("LaunchdLabel = %q, want %q", label, "com.gravinet.daemon")
	}
	// LaunchdPlist must derive its Label from the exact same helper, not a
	// separately-hardcoded copy that could drift from it. (InstallPath does
	// too, but it branches on runtime.GOOS internally, so asserting its darwin
	// behavior here would fail this file on non-darwin test runs — that half
	// is covered by darwin-only test files instead.)
	if p := LaunchdPlist(o); !strings.Contains(p, "<string>"+label+"</string>") {
		t.Fatalf("LaunchdPlist doesn't embed LaunchdLabel's value (%q):\n%s", label, p)
	}
}

func TestRcdScript(t *testing.T) {
	r := RcdScript(testOpts())
	for _, want := range []string{
		"# PROVIDE: gravinet",
		"# REQUIRE: NETWORKING",
		"# KEYWORD: shutdown",
		". /etc/rc.subr",
		`name="gravinet"`,
		"rcvar=gravinet_enable",
		`command="/usr/sbin/daemon"`,
		"/usr/local/bin/gravinet run -config /etc/gravinet/config.json",
		`run_rc_command "$1"`,
	} {
		if !strings.Contains(r, want) {
			t.Errorf("rc.d script missing %q\n%s", want, r)
		}
	}
	// rc.subr's own pidfile-based logic (status/poll, and check_pidfile in
	// the stop override below) must still resolve to the daemon(8) supervisor
	// via -P, not gravinet's own pid via -p: using -p there would reintroduce
	// the bug daemon(8)'s own manual page warns about — `service gravinet
	// stop` would signal gravinet directly, which daemon(8) (still watching
	// under -r) then sees as an unexpected death and restarts, turning "stop"
	// into "stop, then start again". See RcdScript's comment. A *separate*
	// -p child_pidfile is expected now (see below) but must never be the one
	// wired to rc.subr's own $pidfile variable.
	if !strings.Contains(r, "-P ${pidfile}") {
		t.Errorf("rc.d script must point rc.subr's pidfile logic at daemon(8)'s -P (supervisor pidfile)\n%s", r)
	}
	if strings.Contains(r, "-p ${pidfile}") {
		t.Errorf("rc.d script must not wire daemon(8)'s -p (child pidfile) to rc.subr's own $pidfile var alongside -r\n%s", r)
	}
	// The dedicated child pidfile: only used by the bounded stop override's
	// SIGKILL escalation, so it can reach gravinet's own pid directly instead
	// of just the supervisor's.
	if !strings.Contains(r, "-p ${child_pidfile}") {
		t.Errorf("rc.d script must pass a separate -p child_pidfile for the stop override's SIGKILL escalation to target\n%s", r)
	}
	// Bounded stop: rc.subr's default stop_cmd (wait_for_pids) has no
	// timeout and hangs forever if gravinet never exits — exactly what made
	// install-freebsd.sh's upgrade-in-place step (which runs `service
	// gravinet onestop`) hang. There must be an override that bounds the
	// wait and escalates to SIGKILL rather than blocking indefinitely.
	if !strings.Contains(r, `stop_cmd="gravinet_stop"`) {
		t.Errorf("rc.d script must override stop_cmd with a bounded version, not rely on rc.subr's untimed default wait_for_pids\n%s", r)
	}
	if !strings.Contains(r, "kill -KILL") {
		t.Errorf("rc.d script's stop override must escalate to SIGKILL if graceful shutdown doesn't finish in time\n%s", r)
	}
}

func TestOpenBSDRcScript(t *testing.T) {
	o := testOpts()
	o.User = "_gravinet"
	r := OpenBSDRcScript(o)
	for _, want := range []string{
		"#!/bin/ksh",
		". /etc/rc.d/rc.subr",
		`daemon="/usr/local/bin/gravinet"`,
		`daemon_flags="run -config /etc/gravinet/config.json"`,
		`daemon_user="_gravinet"`,
		"rc_bg=YES", // gravinet doesn't fork; rc.subr must background it
		"rc_cmd $1",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("openbsd rc.d script missing %q\n%s", want, r)
		}
	}
	// OpenBSD's rc.subr dispatches via rc_cmd, not FreeBSD's run_rc_command,
	// and there is no daemon(8) supervisor to wrap — pulling the FreeBSD
	// machinery in here would be wrong, so guard against it drifting in.
	for _, banned := range []string{
		"run_rc_command",
		"/usr/sbin/daemon",
		"child_pidfile",
	} {
		if strings.Contains(r, banned) {
			t.Errorf("openbsd rc.d script must NOT contain FreeBSD-ism %q\n%s", banned, r)
		}
	}
}

func TestWindowsInstallCommands(t *testing.T) {
	c := WindowsInstallCommands(testOpts())
	for _, want := range []string{
		"sc.exe create gravinet", "start= auto", "sc.exe description gravinet",
		// Recovery: three restart actions (first/second/subsequent) and the
		// failure-actions flag so a non-zero-exit stop also triggers recovery.
		"sc.exe failure gravinet", "actions= restart/5000/restart/10000/restart/30000",
		"reset= 86400", "sc.exe failureflag gravinet 1",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("windows commands missing %q\n%s", want, c)
		}
	}
	// The service must be created (and recovery configured) before it's started.
	if strings.Index(c, "sc.exe failure gravinet") > strings.Index(c, "sc.exe start gravinet") {
		t.Error("recovery must be configured before the service is started")
	}
}

// Restart's platform branches (systemctl restart, launchctl kickstart,
// service(8) restart, rcctl restart, Restart-Service) are all synchronous
// stop-then-start cycles whose stop half waits for gravinet's own main
// process to exit. Called from gravinet's own restart-on-underlay-change or
// restart-on-suspend-resume path, that process is this one — so if Restart
// ever went back to exec.Command(...).Run() for any of them, the daemon
// would deadlock waiting on a restart that is itself waiting on the daemon
// to exit (see Restart's doc comment). detachedRestart is what every branch
// funnels through to avoid that; this proves it actually launches detached
// (Start, not Run) rather than waiting on its child.
func TestDetachedRestartDoesNotBlock(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh on this system")
	}
	start := time.Now()
	if err := detachedRestart("sh", "-c", "sleep 5"); err != nil {
		t.Fatalf("detachedRestart: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Fatalf("detachedRestart blocked for %v waiting on a 5s child — it must Start(), not Run(), "+
			"or every platform branch of Restart() can deadlock a process restarting itself", elapsed)
	}
}

// A command that can't even be launched (bad binary, not just a nonzero
// exit) must still be reported — detachedRestart trades waiting for the
// result for waiting on the launch, not for silence on total failure.
func TestDetachedRestartReportsLaunchFailure(t *testing.T) {
	if err := detachedRestart("gravinet-definitely-not-a-real-binary-xyz"); err == nil {
		t.Fatal("detachedRestart should return an error when the command can't be launched at all")
	}
}

func TestDefaultsFromBinary(t *testing.T) {
	o := Defaults()
	if o.Name != "gravinet" || o.ExecPath == "" {
		t.Fatalf("defaults not populated: %+v", o)
	}
}

func TestNotifyReadyNoSocket(t *testing.T) {
	os.Unsetenv("NOTIFY_SOCKET")
	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady with no socket should be a no-op, got %v", err)
	}
}

func TestNotifyReadySendsReady(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sd_notify is linux-only")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "notify.sock")
	addr := &net.UnixAddr{Name: sockPath, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	os.Setenv("NOTIFY_SOCKET", sockPath)
	defer os.Unsetenv("NOTIFY_SOCKET")

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, _ := conn.ReadFromUnix(buf)
		done <- string(buf[:n])
	}()

	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}
	select {
	case msg := <-done:
		if msg != "READY=1" {
			t.Fatalf("expected READY=1, got %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive readiness notification")
	}
}
