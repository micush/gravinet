package service

// Host power control — reboot or shut down the whole machine gravinet runs on,
// optionally on a delay, and cancel a pending scheduled action where the OS
// supports it. This is the backend for the web admin's System > Power page.
//
// It is deliberately separate from Restart() above: Restart bounces only the
// gravinet *service* via the platform service manager, whereas this reboots or
// powers off the entire *host* via the OS's own shutdown facility. The two
// share the same self-restart hazard (a command that waits on this very
// process) only for the immediate case, and handle it the same way — see
// detachedRestart's doc comment and the immediate branches below.
//
// Cross-platform via a runtime.GOOS switch, exactly like CanRestart/Restart:
//   - linux:   `systemctl reboot|poweroff` for an immediate action (falling back
//              to `shutdown`), `shutdown -r|-h +N` for a scheduled one, and
//              `shutdown -c` to cancel.
//   - darwin:  `shutdown -r|-h now|+N`. macOS shutdown has no cancel.
//   - windows: `shutdown /r|/s /t <seconds>`, and `shutdown /a` to cancel.
//   - freebsd/openbsd: `shutdown -r|-p now|+N`. BSD shutdown has no -c cancel.
//
// Delays are expressed uniformly as whole minutes from now (0 = immediately),
// normalized by the caller — the web admin resolves "now"/"in N min"/"at HH:MM"
// into a minute count before calling here, so this layer never has to reason
// about clock formats that differ per platform (Linux/BSD shutdown accept an
// absolute HH:MM; Windows only accepts a second count), and Windows just
// multiplies by 60.

import (
	"fmt"
	"os/exec"
	"runtime"
)

// HostPowerSupported reports whether an OS-level host power action looks usable
// on this machine — i.e. whether the tool each platform branch below relies on
// is present. Mirrors CanRestart's shape: (true, "") when it's available, or
// (false, hint) with a human-readable reason otherwise, so the web admin can
// grey the page out with an explanation rather than failing at click time.
func HostPowerSupported() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err == nil {
			return true, ""
		}
		if _, err := exec.LookPath("shutdown"); err == nil {
			return true, ""
		}
		return false, "host power control needs systemctl or shutdown, neither of which is on this host"
	case "darwin", "freebsd", "openbsd":
		if _, err := exec.LookPath("shutdown"); err == nil {
			return true, ""
		}
		return false, "host power control needs the shutdown command, which isn't on this host"
	case "windows":
		if _, err := exec.LookPath("shutdown"); err == nil {
			return true, ""
		}
		return false, "host power control needs shutdown.exe, which isn't on this host"
	default:
		return false, "host power control isn't supported on this operating system"
	}
}

// HostPower reboots ("restart") or powers off ("shutdown") the host. delayMin
// is whole minutes from now; 0 means immediately. Returns (true, "") once the
// action has been scheduled/launched, or (false, hint) on a validation or
// dispatch failure.
//
// The immediate case backgrounds the command with a one-second head start (via
// detachedRestart, i.e. Start not Run) so this HTTP handler's reply flushes to
// the browser before the machine goes down and so we don't block a goroutine on
// a command that tears this process out from under it. The scheduled case runs
// the command directly and waits for it: `shutdown +N` and `shutdown /t` both
// register the timer and return promptly, so we can surface a real error (e.g.
// "not permitted") instead of guessing.
func HostPower(action string, delayMin int) (bool, string) {
	if action != "restart" && action != "shutdown" {
		return false, "action must be 'restart' or 'shutdown'"
	}
	if delayMin < 0 {
		return false, "delay must not be negative"
	}
	if ok, hint := HostPowerSupported(); !ok {
		return false, hint
	}
	reboot := action == "restart"

	switch runtime.GOOS {
	case "linux":
		if delayMin == 0 {
			// Prefer systemctl for the immediate case (it's the native path on
			// a systemd host and matches how Restart drives the service); fall
			// back to shutdown if systemctl isn't present. Backgrounded so the
			// reply flushes first.
			verb := "poweroff"
			flag := "-h"
			if reboot {
				verb, flag = "reboot", "-r"
			}
			if _, err := exec.LookPath("systemctl"); err == nil {
				if err := detachedRestart("sh", "-c", "sleep 1; systemctl "+verb); err == nil {
					return true, ""
				}
			}
			if err := detachedRestart("sh", "-c", "sleep 1; shutdown "+flag+" now"); err != nil {
				return false, "couldn't " + action + " the host automatically"
			}
			return true, ""
		}
		flag := "-h"
		if reboot {
			flag = "-r"
		}
		if out, err := exec.Command("shutdown", flag, fmt.Sprintf("+%d", delayMin)).CombinedOutput(); err != nil {
			return false, powerErr(action, out, err)
		}
		return true, ""

	case "darwin", "freebsd", "openbsd":
		// BSD/macOS: power off is -p (not -h, which only halts) on the BSDs;
		// macOS accepts -h for power off. Use the platform's own spelling.
		off := "-p"
		if runtime.GOOS == "darwin" {
			off = "-h"
		}
		flag := off
		if reboot {
			flag = "-r"
		}
		if delayMin == 0 {
			if err := detachedRestart("sh", "-c", "sleep 1; shutdown "+flag+" now"); err != nil {
				return false, "couldn't " + action + " the host automatically"
			}
			return true, ""
		}
		// Scheduled: shutdown holds until the deadline, so background it rather
		// than block; we can't capture its eventual result, but launch failure
		// (e.g. missing binary) is still reported.
		if err := detachedRestart("sh", "-c", fmt.Sprintf("shutdown %s +%d", flag, delayMin)); err != nil {
			return false, "couldn't schedule the host " + action
		}
		return true, ""

	case "windows":
		flag := "/s" // shut down
		if reboot {
			flag = "/r"
		}
		secs := delayMin * 60
		if out, err := exec.Command("shutdown", flag, "/t", fmt.Sprintf("%d", secs)).CombinedOutput(); err != nil {
			return false, powerErr(action, out, err)
		}
		return true, ""

	default:
		return false, "host power control isn't supported on this operating system"
	}
}

// HostPowerCancel cancels a pending scheduled reboot/shutdown. Only Linux
// (`shutdown -c`) and Windows (`shutdown /a`) expose a first-class cancel;
// macOS and the BSDs don't, so there we return a clear (false, reason) rather
// than pretend to have cancelled something.
func HostPowerCancel() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("shutdown"); err != nil {
			return false, "cancelling needs the shutdown command, which isn't on this host"
		}
		if out, err := exec.Command("shutdown", "-c").CombinedOutput(); err != nil {
			return false, powerErr("cancel", out, err)
		}
		return true, ""
	case "windows":
		if _, err := exec.LookPath("shutdown"); err != nil {
			return false, "cancelling needs shutdown.exe, which isn't on this host"
		}
		if out, err := exec.Command("shutdown", "/a").CombinedOutput(); err != nil {
			return false, powerErr("cancel", out, err)
		}
		return true, ""
	default:
		return false, "cancelling a scheduled power action isn't supported on " + runtime.GOOS
	}
}

// powerErr turns a failed shutdown/systemctl invocation into a one-line hint,
// preferring the command's own stderr (trimmed) when it wrote any, since that's
// usually the most useful thing ("Failed to ...: Interactive authentication
// required", "Access is denied", etc.).
func powerErr(action string, out []byte, err error) string {
	msg := trimOneLine(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return "couldn't " + action + " the host: " + msg
}

// trimOneLine collapses command output to its first non-empty line, so a
// multi-line stderr doesn't get splashed into a JSON error field.
func trimOneLine(s string) string {
	for _, line := range splitLines(s) {
		if t := trimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// Small dependency-free helpers so this file doesn't pull in strings just for
// two calls (service.go already imports strings, but keeping power.go's helper
// surface local makes it obvious these are single-line-collapsing conveniences,
// not general utilities).
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}
