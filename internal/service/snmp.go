package service

// SNMP agent management — the backend for System > SNMP. Writes net-snmp's
// snmpd.conf and reconciles the snmpd OS service (enable+restart when the
// config is runnable, disable+stop otherwise) to match. Ported from
// parapet's own SnmpManager (src/snmpd.rs), with one deliberate
// architecture change: parapet spawns and supervises snmpd as a direct
// child of its own process; gravinet manages it as an ordinary OS service
// instead (systemctl/service+sysrc/rcctl/brew services), the same way it
// already treats FRR rather than running that as a child either. A child of
// gravinet's own process would die every time gravinet itself restarts — a
// config change, an upgrade, a crash-and-recover — which happens far more
// often than an operator wants their SNMP monitoring to blink; a real OS
// service persists across that. See config.SNMPConfig's doc comment too.
//
// Supported: linux, freebsd, openbsd, darwin (via Homebrew's net-snmp,
// best-effort — there's no one standard install layout for it the way
// there is on the BSDs and Linux distros, so the config path search below
// checks the common Homebrew prefixes rather than assuming one). Windows
// is deliberately NOT supported here: Windows' own SNMP is a built-in OS
// feature with a completely different configuration mechanism (registry
// keys under the SNMP service's Parameters key, enabled via DISM, not a
// text config file net-snmp reads) — different enough that half-adapting
// this same code to it would be more likely to be subtly wrong than
// honestly absent. SNMPSupported says so plainly rather than pretending.
//
// darwin has its own real caveat, worth stating plainly rather than
// discovering via a mysterious failure: gravinet's own process commonly
// runs as root there (a launchd daemon needing tun-device/raw-socket
// access), and Homebrew refuses to operate as root — the identical
// constraint install-macos.sh already documents for why it fetches Go
// straight from go.dev instead of via brew. snmpServiceStart/Stop's
// `brew services ...` calls below can hit that same refusal when gravinet
// itself is the one invoking them; the resulting error is Homebrew's own
// "Do not run this as root" text, surfaced verbatim through the usual
// cmdErr wrapping rather than a mysterious silent failure, but there is no
// workaround implemented here (running `sudo -u <user> brew ...` would
// need to know *which* user has net-snmp installed, which this package has
// no way to determine). On a host where this bites, the operator's own
// non-root `brew services start net-snmp` — set up once, outside
// gravinet — is the practical answer; ApplySNMP still writes a correct
// snmpd.conf either way, so that half of this always works even when the
// service-management half can't.
//
// snmpd.conf is written to the *standard* net-snmp path, not a
// gravinet-specific one, for the same reason parapet's own comment gives:
// on Debian/Ubuntu the packaged snmpd ships an AppArmor profile confining
// the binary to a fixed set of paths, and the standard path is one of them.
// Because gravinet uses the OS's own packaged snmpd service (rather than
// bypassing it the way parapet's child-process model does), that packaged
// service's own AppArmor/SELinux policy already expects to read from here —
// nothing needs to be loosened or dropped to complain mode the way
// parapet's installer does for its bypass model.

import (
	"os"
	"os/exec"
	"runtime"
	"strings"

	"gravinet/internal/config"
)

// SNMPConfPath returns where snmpd.conf should be written on this platform.
func SNMPConfPath() string {
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd":
		return "/etc/snmp/snmpd.conf"
	case "darwin":
		// No single standard layout the way the BSDs/Linux distros have one;
		// check the common Homebrew prefixes for an existing snmp directory
		// (i.e. net-snmp is actually installed there) before falling back to
		// a guess. Apple Silicon's default prefix is /opt/homebrew; Intel
		// Homebrew's is /usr/local.
		for _, p := range []string{"/opt/homebrew/etc/snmp", "/usr/local/etc/snmp"} {
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				return p + "/snmpd.conf"
			}
		}
		return "/usr/local/etc/snmp/snmpd.conf"
	default:
		return ""
	}
}

// snmpdBinary searches the usual install locations for snmpd, the same
// "check a short list of real paths, then fall back to PATH" shape
// hosttime.go and sysusers.go's own binary lookups use.
func snmpdBinary() string {
	candidates := []string{
		"/usr/sbin/snmpd", "/sbin/snmpd", "/usr/bin/snmpd",
		"/usr/local/sbin/snmpd", "/usr/local/bin/snmpd",
		"/opt/homebrew/sbin/snmpd",
	}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("snmpd"); err == nil {
		return p
	}
	return ""
}

// SNMPSupported reports whether snmpd looks usable on this host at all —
// the binary is installed and the platform is one this package manages a
// service on. (true, "") or (false, hint).
func SNMPSupported() (bool, string) {
	if runtime.GOOS == "windows" {
		return false, "gravinet doesn't manage Windows' own SNMP Service — it has a completely different (registry-based) configuration mechanism than net-snmp's config file"
	}
	if SNMPConfPath() == "" {
		return false, "SNMP isn't supported on this operating system"
	}
	if snmpdBinary() == "" {
		return false, "snmpd isn't installed on this host (is the snmp/net-snmp package installed?)"
	}
	return true, ""
}

// ApplySNMP reconciles the on-disk config and OS service state with cfg:
// runnable (see config.SNMPConfig.IsRunnable) means (re)write snmpd.conf
// then enable+restart the service; not runnable means stop+disable it,
// leaving the config file in place (harmless while the service isn't
// running — the next enable rewrites it anyway). A failure here is about
// the *service*, never about whether the config itself was accepted; the
// config is gravinet's own source of truth regardless of whether the OS
// service cooperated, the same "config is truth, the daemon reconciles to
// it" split every other Apply-style function in this package follows.
func ApplySNMP(cfg config.SNMPConfig) (bool, string) {
	if ok, hint := SNMPSupported(); !ok {
		return false, hint
	}
	if !cfg.IsRunnable() {
		ok, hint := snmpServiceStop()
		return ok, hint
	}
	confPath := SNMPConfPath()
	if err := writeSNMPConf(confPath, cfg); err != nil {
		return false, "could not write " + confPath + ": " + err.Error()
	}
	return snmpServiceStart()
}

// writeSNMPConf renders and writes snmpd.conf. The community string is a
// secret (SNMPv2c has no per-request auth beyond it), so the file is
// written 0600 — root/the daemon's own user only, matching parapet's exact
// reasoning and permission choice.
func writeSNMPConf(path string, cfg config.SNMPConfig) error {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(renderSNMPConf(cfg)), 0o600)
}

func dirOf(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "."
	}
	return path[:i]
}

// renderSNMPConf renders snmpd.conf's text. Every value is sanitized before
// being written, independent of any validation the HTTP handler already
// did — this function has to be safe to call with an arbitrary
// config.SNMPConfig regardless of what validated it, since "some caller
// upstream already checked" is exactly the assumption that goes stale
// first in a codebase that changes.
func renderSNMPConf(cfg config.SNMPConfig) string {
	var b strings.Builder
	b.WriteString("# Generated by gravinet — do not edit by hand.\n")
	b.WriteString("# Read-only SNMPv2c agent.\n\n")

	// rocommunity grants read-only access from any source; gravinet doesn't
	// manage a host firewall rule to scope who can reach it (see
	// config.SNMPConfig.Interfaces's doc comment) — restrict reachability
	// with the host's own firewall if that matters in your environment.
	b.WriteString("rocommunity " + cleanSNMPCommunity(cfg.Community) + "\n")
	if cfg.Location != "" {
		b.WriteString("sysLocation " + snmpConfValue(cfg.Location) + "\n")
	}
	if cfg.Contact != "" {
		b.WriteString("sysContact " + snmpConfValue(cfg.Contact) + "\n")
	}
	return b.String()
}

// cleanSNMPCommunity strips whitespace, quotes, backslashes, and control
// characters so the community string can never inject a second snmpd.conf
// directive onto the rocommunity line. Mirrors parapet's clean_community.
func cleanSNMPCommunity(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r <= 0x1f || r == 0x7f || r == '"' || r == '\\' || r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// snmpConfValue quotes a directive value (sysLocation/sysContact commonly
// contain spaces), stripping control characters and embedded quotes so a
// value can never break out of its own directive. Mirrors parapet's
// conf_value.
func snmpConfValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r <= 0x1f || r == 0x7f || r == '"' || r == '\\' {
			continue
		}
		b.WriteRune(r)
	}
	return "\"" + b.String() + "\""
}

// validSNMPListen checks a listen address token (e.g. "udp:161",
// "0.0.0.0:161", "udp:10.0.0.1:161") — the small character set those forms
// use, so a listen address can never smuggle in an extra argv token or
// config directive. Mirrors parapet's valid_listen. Not currently rendered
// into snmpd.conf (snmpd's own listen address directive is passed on its
// command line in parapet's child-process model; gravinet's OS-service
// model instead relies on the packaged service's own default of "every
// address" and doesn't override it) — validated and kept for the field's
// wire/display round-trip and so a later listen-address feature has a
// tested, ready-made validator rather than an unvalidated string.
func validSNMPListen(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == ':' || r == '-') {
			return false
		}
	}
	return true
}

// snmpServiceStart enables (survives reboot) and (re)starts the snmpd
// service via this platform's own service manager.
func snmpServiceStart() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		unit := linuxSNMPUnit()
		if out, err := exec.Command("systemctl", "enable", "--now", unit).CombinedOutput(); err != nil {
			if out2, err2 := exec.Command("systemctl", "restart", unit).CombinedOutput(); err2 != nil {
				return false, cmdErr("systemctl enable/restart "+unit, out, err) + "; " + cmdErr("systemctl restart", out2, err2)
			}
			exec.Command("systemctl", "enable", unit).Run() // best-effort; restart already succeeded
		}
		return true, ""
	case "freebsd":
		exec.Command("sysrc", "snmpd_enable=YES").Run()
		if out, err := exec.Command("service", "snmpd", "restart").CombinedOutput(); err != nil {
			return false, cmdErr("service snmpd restart", out, err)
		}
		return true, ""
	case "openbsd":
		exec.Command("rcctl", "enable", "snmpd").Run()
		if out, err := exec.Command("rcctl", "restart", "snmpd").CombinedOutput(); err != nil {
			return false, cmdErr("rcctl restart snmpd", out, err)
		}
		return true, ""
	case "darwin":
		if out, err := exec.Command("brew", "services", "restart", "net-snmp").CombinedOutput(); err != nil {
			return false, cmdErr("brew services restart net-snmp", out, err)
		}
		return true, ""
	default:
		return false, "SNMP isn't supported on this operating system"
	}
}

// snmpServiceStop stops and disables the snmpd service.
func snmpServiceStop() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		unit := linuxSNMPUnit()
		if out, err := exec.Command("systemctl", "disable", "--now", unit).CombinedOutput(); err != nil {
			return false, cmdErr("systemctl disable --now "+unit, out, err)
		}
		return true, ""
	case "freebsd":
		exec.Command("sysrc", "snmpd_enable=NO").Run()
		if out, err := exec.Command("service", "snmpd", "stop").CombinedOutput(); err != nil && !strings.Contains(strings.ToLower(string(out)), "not running") {
			return false, cmdErr("service snmpd stop", out, err)
		}
		return true, ""
	case "openbsd":
		exec.Command("rcctl", "disable", "snmpd").Run()
		if out, err := exec.Command("rcctl", "stop", "snmpd").CombinedOutput(); err != nil && !strings.Contains(strings.ToLower(string(out)), "not running") {
			return false, cmdErr("rcctl stop snmpd", out, err)
		}
		return true, ""
	case "darwin":
		if out, err := exec.Command("brew", "services", "stop", "net-snmp").CombinedOutput(); err != nil {
			return false, cmdErr("brew services stop net-snmp", out, err)
		}
		return true, ""
	default:
		return false, "SNMP isn't supported on this operating system"
	}
}

// linuxSNMPUnit picks between the two unit names the snmpd package uses
// across distros — "snmpd.service" everywhere this project has actually
// seen it (Debian/Ubuntu's snmpd package, Fedora/RHEL/openSUSE/Arch's
// net-snmp package all install a unit named snmpd), with "snmp.service" as
// a fallback for the rare distro that names it the other way — mirroring
// parapet's own installer, which checks for exactly these two names in
// exactly this order for the identical reason.
func linuxSNMPUnit() string {
	if unitFileExists("snmpd.service") {
		return "snmpd.service"
	}
	if unitFileExists("snmp.service") {
		return "snmp.service"
	}
	return "snmpd.service" // neither found (not installed yet, or a mid-install race); this is still the name systemctl error messages will report against
}

func unitFileExists(unit string) bool {
	out, err := exec.Command("systemctl", "list-unit-files", unit).CombinedOutput()
	return err == nil && strings.Contains(string(out), unit)
}

// SNMPServiceRunning reports whether the snmpd service is currently active,
// for the System > SNMP page to show real status rather than just echoing
// back the config it was told to apply (which could be stale if the
// service failed to start, or was stopped by hand since).
func SNMPServiceRunning() bool {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("systemctl", "is-active", "--quiet", linuxSNMPUnit()).Run() == nil
	case "freebsd":
		return exec.Command("service", "snmpd", "status").Run() == nil
	case "openbsd":
		return exec.Command("rcctl", "check", "snmpd").Run() == nil
	case "darwin":
		out, err := exec.Command("brew", "services", "list").CombinedOutput()
		if err != nil {
			return false
		}
		for _, ln := range strings.Split(string(out), "\n") {
			f := strings.Fields(ln)
			if len(f) >= 2 && f[0] == "net-snmp" && f[1] == "started" {
				return true
			}
		}
		return false
	default:
		return false
	}
}
