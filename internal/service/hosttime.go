package service

// Host clock, timezone, and NTP — the backend for the web admin's System > Time
// page, and the third member of the System group after Power and Upgrade.
//
// Why a mesh VPN ships a clock page at all: gravinet's handshake carries a
// wall-clock timestamp and the engine refuses any HS_INIT whose timestamp is
// further than mesh's clockSkew tolerance (±2 minutes) from local time — that's
// what makes the replay window bounded and small. A node whose clock has drifted
// past that tolerance doesn't degrade, it simply stops forming sessions, and the
// only clue is a debug-level rejection log that names the skew and says "check
// NTP/system time on both nodes". This page is the other half of that sentence:
// the place you go to look, and to fix it, without leaving the admin UI.
//
// Structure mirrors power.go: a small typed read (HostTime) plus one setter per
// thing you can change (SetHostTimezone / SetHostNTP / SetHostClock), each
// returning (ok, hint) so the UI can explain a refusal instead of failing
// silently, and each dispatching on runtime.GOOS.
//
// The host is the source of truth here — nothing on this page is stored in
// gravinet's own config. That's a deliberate departure from parapet, which keeps
// a TimeSync block in its config and re-applies it on every commit: parapet is
// the sole configuration authority for the box it runs on, while gravinet is one
// daemon among many on a machine whose clock is just as likely to be managed by
// the distro, a cloud-init template, or a hypervisor's guest agent. Storing a
// second copy would let the two disagree, and the disagreement would silently
// win on the next config reload. So gravinet reads what the OS says and writes
// through to it; if someone changes the zone with timedatectl afterwards, this
// page shows that on the next refresh rather than fighting it.
//
// Per-platform tooling, all optional and all probed before use:
//
//	linux:   timedatectl for read + timezone + clock + the NTP master switch;
//	         servers land in /etc/systemd/timesyncd.conf, or chrony's conf when
//	         chrony is the active implementation.
//	darwin:  systemsetup (-gettimezone/-settimezone, -getusingnetworktime/
//	         -setusingnetworktime, -get/-setnetworktimeserver, -setdate/-settime).
//	         macOS takes exactly one time server.
//	freebsd: /etc/localtime (a copy, per tzsetup) + /var/db/zoneinfo, ntpd via
//	         service(8)/sysrc, servers in /etc/ntp.conf.
//	openbsd: /etc/localtime symlink, openntpd via rcctl, servers in /etc/ntpd.conf.
//	windows: tzutil for the zone (a Windows zone id, NOT an IANA name — see
//	         TimeInfo.TimezoneStyle), w32tm for NTP, Set-Date for the clock.
//
// Every external command is run through exec.Command with separate arguments —
// never a shell string — and every value that reaches one is validated first
// (validTimezone / validNTPServer), so a timezone or server name can't smuggle
// in a second command.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// TimeInfo is the live time/NTP state of the host, as read from the OS.
//
// The *Known flags exist because "off" and "couldn't tell" are different
// answers and conflating them would be a lie the UI then repeats: on macOS and
// the BSDs there's no cheap equivalent of NTPSynchronized, so SyncKnown is false
// there and the page says nothing rather than claiming the clock is unsynced.
type TimeInfo struct {
	Now           time.Time // this host's wall clock at the moment of the read
	Timezone      string    // zone name, or "" if it couldn't be determined
	TimezoneStyle string    // "iana" everywhere except windows, which is "windows"
	Abbrev        string    // local zone abbreviation, e.g. "MST"
	OffsetSeconds int       // seconds east of UTC for Now
	NTPEnabled    bool      // is time sync switched on
	NTPKnown      bool      // could NTPEnabled be determined at all
	Synchronized  bool      // has the sync daemon actually locked on
	SyncKnown     bool      // could Synchronized be determined at all
	Servers       []string  // configured time servers, in file order
	Manager       string    // which implementation is in play, for display
	CanTimezone   bool      // is a timezone change possible on this host
	CanNTP        bool      // is an NTP change possible on this host
	CanClock      bool      // is a manual clock set possible on this host
	Hint          string    // why one of the Can* is false, if any is
}

// HostTime reads the host's current clock, timezone, and NTP state. It never
// fails: anything that can't be determined is left zero with its *Known flag
// false, so a host with no time tooling at all still renders a page showing the
// clock (which always comes from this process, not a command).
func HostTime() TimeInfo {
	now := time.Now()
	abbrev, offset := now.Zone()
	info := TimeInfo{
		Now:           now,
		Abbrev:        abbrev,
		OffsetSeconds: offset,
		TimezoneStyle: "iana",
	}

	switch runtime.GOOS {
	case "linux":
		readLinuxTime(&info)
	case "darwin":
		readDarwinTime(&info)
	case "freebsd", "openbsd":
		readBSDTime(&info)
	case "windows":
		info.TimezoneStyle = "windows"
		readWindowsTime(&info)
	default:
		info.Hint = "gravinet can't manage time settings on " + runtime.GOOS
	}
	return info
}

// ── linux ───────────────────────────────────────────────────────────────────

// readLinuxTime fills info from timedatectl where present. `timedatectl show`
// is the machine-readable form (KEY=value lines) and is stable across systemd
// versions, unlike the human `timedatectl status` table — parsing the latter is
// what breaks when a distro translates its labels.
func readLinuxTime(info *TimeInfo) {
	haveTdc := haveCmd("timedatectl")
	if haveTdc {
		kv := keyVals(cmdOut("timedatectl", "show"))
		info.Timezone = kv["Timezone"]
		if v, ok := kv["NTP"]; ok {
			info.NTPEnabled, info.NTPKnown = v == "yes", true
		}
		if v, ok := kv["NTPSynchronized"]; ok {
			info.Synchronized, info.SyncKnown = v == "yes", true
		}
	}
	if info.Timezone == "" {
		info.Timezone = zoneFromLocaltime()
	}

	// Which implementation owns the servers? Ask systemd which unit is actually
	// active rather than guessing from what's installed — plenty of hosts have
	// both chrony and timesyncd on disk with only one running, and writing the
	// idle one's config is a silent no-op that looks like a successful save.
	switch {
	case unitActive("chronyd") || unitActive("chrony"):
		info.Manager = "chrony"
		info.Servers = directiveValues(chronyConf(), "server", "pool")
	case unitActive("systemd-timesyncd"):
		info.Manager = "systemd-timesyncd"
		info.Servers = timesyncdServers()
	case haveCmd("chronyd"):
		info.Manager = "chrony"
		info.Servers = directiveValues(chronyConf(), "server", "pool")
	case fileExists(timesyncdConfPath) || haveCmd("systemd-timesyncd"):
		info.Manager = "systemd-timesyncd"
		info.Servers = timesyncdServers()
	case haveCmd("ntpd"):
		info.Manager = "ntpd"
		info.Servers = directiveValues("/etc/ntp.conf", "server", "pool")
	}

	info.CanTimezone = haveTdc || dirExists("/usr/share/zoneinfo")
	info.CanClock = haveTdc || haveCmd("date")
	info.CanNTP = haveTdc && info.Manager != ""
	switch {
	case !info.CanNTP && !haveTdc:
		info.Hint = "NTP changes need timedatectl, which isn't on this host"
	case !info.CanNTP:
		info.Hint = "no time-sync implementation found (looked for chrony, systemd-timesyncd, and ntpd)"
	case !info.CanTimezone:
		info.Hint = "timezone changes need timedatectl or /usr/share/zoneinfo, neither of which is on this host"
	}
}

// timesyncdServers returns the servers systemd-timesyncd is using: the NTP=
// line from its config if one is set, otherwise whatever the running daemon
// reports, which is how a host on distro defaults (an empty NTP=) still shows
// the pool it's actually talking to instead of an empty list.
func timesyncdServers() []string {
	if v := directiveValues(timesyncdConfPath, "NTP"); len(v) > 0 {
		return v
	}
	if haveCmd("timedatectl") {
		kv := keyVals(cmdOut("timedatectl", "show-timesync"))
		if s := kv["ServerName"]; s != "" {
			return []string{s}
		}
		if s := kv["SystemNTPServers"]; s != "" {
			return strings.Fields(s)
		}
		if s := kv["FallbackNTPServers"]; s != "" {
			return strings.Fields(s)
		}
	}
	return nil
}

// chronyConf finds chrony's config, which lives in a different place depending
// on packaging: Debian/Ubuntu split it under /etc/chrony/, RHEL and the rest
// keep it at /etc/chrony.conf.
func chronyConf() string {
	for _, p := range []string{"/etc/chrony/chrony.conf", "/etc/chrony.conf"} {
		if fileExists(p) {
			return p
		}
	}
	return "/etc/chrony.conf"
}

// ── darwin ──────────────────────────────────────────────────────────────────

// readDarwinTime uses systemsetup, whose output is "Label: value" lines. Note
// macOS has no per-query sync-state answer comparable to NTPSynchronized, so
// SyncKnown stays false and the UI simply doesn't claim either way.
func readDarwinTime(info *TimeInfo) {
	have := haveCmd("systemsetup")
	if have {
		info.Timezone = afterColon(cmdOut("systemsetup", "-gettimezone"))
		if v := afterColon(cmdOut("systemsetup", "-getusingnetworktime")); v != "" {
			info.NTPEnabled, info.NTPKnown = strings.EqualFold(v, "on"), true
		}
		if s := afterColon(cmdOut("systemsetup", "-getnetworktimeserver")); s != "" {
			info.Servers = []string{s}
		}
	}
	if info.Timezone == "" {
		info.Timezone = zoneFromLocaltime()
	}
	info.Manager = "macOS network time"
	info.CanTimezone = have
	info.CanNTP = have
	info.CanClock = have
	if !have {
		info.Hint = "time changes need systemsetup, which isn't on this host"
	}
}

// ── freebsd / openbsd ───────────────────────────────────────────────────────

// readBSDTime handles both BSDs, which differ mainly in service tooling
// (service(8)/sysrc vs rcctl) and in which NTP daemon is stock (ntpd from base
// on FreeBSD, openntpd on OpenBSD).
func readBSDTime(info *TimeInfo) {
	info.Timezone = zoneFromLocaltime()
	if info.Timezone == "" && runtime.GOOS == "freebsd" {
		// FreeBSD's /etc/localtime is a copy rather than a symlink, so there's
		// nothing to resolve; tzsetup records the name it installed here.
		if b, err := os.ReadFile("/var/db/zoneinfo"); err == nil {
			info.Timezone = strings.TrimSpace(string(b))
		}
	}

	if runtime.GOOS == "openbsd" {
		info.Manager = "openntpd"
		info.Servers = directiveValues("/etc/ntpd.conf", "server", "servers")
		if haveCmd("rcctl") {
			// `rcctl check` exits 0 only when the daemon is actually running,
			// which is the honest reading of "is NTP on" on OpenBSD.
			info.NTPEnabled = runOK("rcctl", "check", "ntpd")
			info.NTPKnown = true
			info.CanNTP = true
		}
	} else {
		info.Manager = "ntpd"
		info.Servers = directiveValues("/etc/ntp.conf", "server", "pool")
		if haveCmd("service") {
			info.NTPEnabled = runOK("service", "ntpd", "status")
			info.NTPKnown = true
			info.CanNTP = true
		}
	}

	info.CanTimezone = dirExists("/usr/share/zoneinfo")
	info.CanClock = haveCmd("date")
	switch {
	case !info.CanNTP && runtime.GOOS == "openbsd":
		info.Hint = "NTP changes need rcctl, which isn't on this host"
	case !info.CanNTP:
		info.Hint = "NTP changes need service(8), which isn't on this host"
	case !info.CanTimezone:
		info.Hint = "timezone changes need /usr/share/zoneinfo, which isn't on this host"
	}
}

// ── windows ─────────────────────────────────────────────────────────────────

// readWindowsTime uses tzutil for the zone and w32tm for sync state. Windows
// names its zones its own way ("US Mountain Standard Time", not
// "America/Phoenix"), which is why TimeInfo.TimezoneStyle exists: the UI drops
// the IANA picker here and takes a Windows zone id instead, rather than offering
// a list of names tzutil would reject.
func readWindowsTime(info *TimeInfo) {
	if haveCmd("tzutil") {
		info.Timezone = strings.TrimSpace(cmdOut("tzutil", "/g"))
		info.CanTimezone = true
	} else {
		info.Hint = "timezone changes need tzutil.exe, which isn't on this host"
	}
	info.Manager = "Windows Time (w32time)"
	if haveCmd("w32tm") {
		info.CanNTP = true
		st := cmdOut("w32tm", "/query", "/status")
		// A source of "Local CMOS Clock" (or "Free-running System Clock") means
		// w32time is running but not syncing with anyone — that's "NTP off" as
		// far as this page is concerned, not merely "unsynchronized".
		src := afterColon(lineContaining(st, "Source:"))
		if src != "" {
			info.NTPKnown = true
			info.SyncKnown = true
			local := strings.Contains(strings.ToLower(src), "cmos") || strings.Contains(strings.ToLower(src), "free-running")
			info.NTPEnabled = !local
			info.Synchronized = !local && !strings.Contains(st, "unsynchronized")
		}
		cfg := cmdOut("w32tm", "/query", "/configuration")
		if peers := afterColon(lineContaining(cfg, "NtpServer:")); peers != "" {
			info.Servers = windowsPeerList(peers)
		}
	} else if info.Hint == "" {
		info.Hint = "NTP changes need w32tm.exe, which isn't on this host"
	}
	info.CanClock = haveCmd("powershell") || haveCmd("pwsh")
}

// windowsPeerList splits w32tm's manual peer list. Entries are space- or
// comma-separated and each may carry a trailing ",0x9" flag field, plus the
// query output appends a "(Local)" / "(Policy)" provenance marker.
func windowsPeerList(s string) []string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "("); i >= 0 {
		s = s[:i]
	}
	var out []string
	for _, f := range strings.Fields(s) {
		if host := strings.TrimSpace(strings.SplitN(f, ",", 2)[0]); host != "" {
			out = append(out, host)
		}
	}
	return out
}

// ── setters ─────────────────────────────────────────────────────────────────

// SetHostTimezone sets the host timezone. tz is an IANA zone name everywhere
// except Windows, which wants one of its own zone ids (see TimeInfo.
// TimezoneStyle). Returns (true, "") on success or (false, hint).
func SetHostTimezone(tz string) (bool, string) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return false, "timezone must not be empty"
	}
	if err := validTimezone(tz); err != nil {
		return false, err.Error()
	}

	switch runtime.GOOS {
	case "linux":
		if haveCmd("timedatectl") {
			if out, err := exec.Command("timedatectl", "set-timezone", tz).CombinedOutput(); err == nil {
				return true, ""
			} else if !dirExists("/usr/share/zoneinfo") {
				return false, cmdErr("set the timezone", out, err)
			}
		}
		// No systemd (or timedatectl refused on a host where the zoneinfo tree
		// is still usable): do what tzdata's own docs prescribe — point
		// /etc/localtime at the zone and record the name in /etc/timezone,
		// which is the Debian convention other tooling reads back.
		return symlinkZone(tz)
	case "darwin":
		if !haveCmd("systemsetup") {
			return false, "setting the timezone needs systemsetup, which isn't on this host"
		}
		if out, err := exec.Command("systemsetup", "-settimezone", tz).CombinedOutput(); err != nil {
			return false, cmdErr("set the timezone", out, err)
		}
		return true, ""
	case "freebsd":
		// FreeBSD keeps a *copy* at /etc/localtime (tzsetup(8) installs it that
		// way so /etc stays self-contained on a separate root), and records the
		// chosen name in /var/db/zoneinfo.
		src := filepath.Join("/usr/share/zoneinfo", tz)
		b, err := os.ReadFile(src)
		if err != nil {
			return false, "no such timezone on this host: " + tz
		}
		if err := writeFilePreserving("/etc/localtime", b, 0o644); err != nil {
			return false, "couldn't write /etc/localtime: " + err.Error()
		}
		if err := os.WriteFile("/var/db/zoneinfo", []byte(tz+"\n"), 0o644); err != nil {
			return false, "timezone set, but recording it in /var/db/zoneinfo failed: " + err.Error()
		}
		return true, ""
	case "openbsd":
		return symlinkZone(tz)
	case "windows":
		if !haveCmd("tzutil") {
			return false, "setting the timezone needs tzutil.exe, which isn't on this host"
		}
		if out, err := exec.Command("tzutil", "/s", tz).CombinedOutput(); err != nil {
			return false, cmdErr("set the timezone", out, err)
		}
		return true, ""
	default:
		return false, "gravinet can't set the timezone on " + runtime.GOOS
	}
}

// symlinkZone points /etc/localtime at a zone file and records the name in
// /etc/timezone. Shared by the Linux fallback path and OpenBSD, where the
// symlink *is* the mechanism.
func symlinkZone(tz string) (bool, string) {
	src := filepath.Join("/usr/share/zoneinfo", tz)
	if !fileExists(src) {
		return false, "no such timezone on this host: " + tz
	}
	tmp := "/etc/localtime.gravinet-tmp"
	os.Remove(tmp)
	if err := os.Symlink(src, tmp); err != nil {
		return false, "couldn't create the timezone link: " + err.Error()
	}
	// Rename over the old link rather than unlink-then-symlink, so there's no
	// window in which the host has no /etc/localtime at all — during which
	// every process that resolves local time would silently fall back to UTC.
	if err := os.Rename(tmp, "/etc/localtime"); err != nil {
		os.Remove(tmp)
		return false, "couldn't replace /etc/localtime: " + err.Error()
	}
	if runtime.GOOS == "linux" {
		os.WriteFile("/etc/timezone", []byte(tz+"\n"), 0o644)
	}
	return true, ""
}

// SetHostNTP turns time synchronisation on or off and, when turning it on,
// installs the given server list. An empty list with enabled=true means "sync,
// but leave the servers at whatever the host already had" — matching parapet's
// page, where clearing the list is how you switch NTP off in the first place, so
// an empty list never reaches here as a request to erase a working config.
func SetHostNTP(enabled bool, servers []string) (bool, string) {
	clean := make([]string, 0, len(servers))
	for _, s := range servers {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if err := validNTPServer(s); err != nil {
			return false, err.Error()
		}
		clean = append(clean, s)
	}

	switch runtime.GOOS {
	case "linux":
		return setLinuxNTP(enabled, clean)
	case "darwin":
		if !haveCmd("systemsetup") {
			return false, "NTP changes need systemsetup, which isn't on this host"
		}
		// macOS takes exactly one server. Send the first and say so rather than
		// silently dropping the rest.
		if enabled && len(clean) > 0 {
			if out, err := exec.Command("systemsetup", "-setnetworktimeserver", clean[0]).CombinedOutput(); err != nil {
				return false, cmdErr("set the time server", out, err)
			}
		}
		state := "off"
		if enabled {
			state = "on"
		}
		if out, err := exec.Command("systemsetup", "-setusingnetworktime", state).CombinedOutput(); err != nil {
			return false, cmdErr("switch network time "+state, out, err)
		}
		if enabled && len(clean) > 1 {
			return true, "macOS accepts a single time server; used " + clean[0] + " and ignored the rest"
		}
		return true, ""
	case "freebsd":
		if enabled {
			if len(clean) > 0 {
				if err := setDirectiveLines("/etc/ntp.conf", []string{"server", "pool"}, prefixEach("server ", clean)); err != nil {
					return false, "couldn't update /etc/ntp.conf: " + err.Error()
				}
			}
			runQuiet("sysrc", "ntpd_enable=YES")
			if out, err := exec.Command("service", "ntpd", "restart").CombinedOutput(); err != nil {
				return false, cmdErr("start ntpd", out, err)
			}
			return true, ""
		}
		runQuiet("sysrc", "ntpd_enable=NO")
		if out, err := exec.Command("service", "ntpd", "stop").CombinedOutput(); err != nil {
			return false, cmdErr("stop ntpd", out, err)
		}
		return true, ""
	case "openbsd":
		if !haveCmd("rcctl") {
			return false, "NTP changes need rcctl, which isn't on this host"
		}
		if enabled {
			if len(clean) > 0 {
				// openntpd's directive is `server` (or `servers` for a pool
				// name that resolves to several); write the singular form.
				if err := setDirectiveLines("/etc/ntpd.conf", []string{"server", "servers"}, prefixEach("server ", clean)); err != nil {
					return false, "couldn't update /etc/ntpd.conf: " + err.Error()
				}
			}
			runQuiet("rcctl", "enable", "ntpd")
			if out, err := exec.Command("rcctl", "restart", "ntpd").CombinedOutput(); err != nil {
				return false, cmdErr("start ntpd", out, err)
			}
			return true, ""
		}
		runQuiet("rcctl", "disable", "ntpd")
		if out, err := exec.Command("rcctl", "stop", "ntpd").CombinedOutput(); err != nil {
			return false, cmdErr("stop ntpd", out, err)
		}
		return true, ""
	case "windows":
		if !haveCmd("w32tm") {
			return false, "NTP changes need w32tm.exe, which isn't on this host"
		}
		if enabled {
			if len(clean) > 0 {
				// 0x9 = client mode with SpecialInterval, the flag Microsoft's
				// own docs use for a manual peer list.
				peers := strings.Join(prefixEach("", clean), ",0x9 ") + ",0x9"
				if out, err := exec.Command("w32tm", "/config", "/manualpeerlist:"+peers, "/syncfromflags:manual", "/update").CombinedOutput(); err != nil {
					return false, cmdErr("set the time servers", out, err)
				}
			}
			runQuiet("sc", "config", "w32time", "start=", "auto")
			runQuiet("net", "start", "w32time")
			if out, err := exec.Command("w32tm", "/resync").CombinedOutput(); err != nil {
				// A resync can fail simply because the servers aren't reachable
				// yet; the configuration itself did land, so report success with
				// the reason attached rather than rolling back.
				return true, "servers configured, but the first sync attempt failed: " + trimOneLine(string(out))
			}
			return true, ""
		}
		if out, err := exec.Command("net", "stop", "w32time").CombinedOutput(); err != nil {
			return false, cmdErr("stop the Windows Time service", out, err)
		}
		runQuiet("sc", "config", "w32time", "start=", "demand")
		return true, ""
	default:
		return false, "gravinet can't change NTP settings on " + runtime.GOOS
	}
}

// setLinuxNTP writes the server list into whichever implementation is active
// and then flips systemd's master switch. The two steps are separate on purpose:
// `timedatectl set-ntp` toggles the unit, but it has no opinion about server
// lists, so the file write has to happen first or enabling would start the
// daemon on its old servers.
func setLinuxNTP(enabled bool, servers []string) (bool, string) {
	if !haveCmd("timedatectl") {
		return false, "NTP changes need timedatectl, which isn't on this host"
	}
	info := HostTime()

	if enabled && len(servers) > 0 {
		switch info.Manager {
		case "chrony":
			if err := setDirectiveLines(chronyConf(), []string{"server", "pool"}, prefixEach("server ", servers)); err != nil {
				return false, "couldn't update " + chronyConf() + ": " + err.Error()
			}
		case "ntpd":
			if err := setDirectiveLines("/etc/ntp.conf", []string{"server", "pool"}, prefixEach("server ", servers)); err != nil {
				return false, "couldn't update /etc/ntp.conf: " + err.Error()
			}
		default:
			if err := setTimesyncdServers(servers); err != nil {
				return false, "couldn't update " + timesyncdConfPath + ": " + err.Error()
			}
		}
	}

	state := "false"
	if enabled {
		state = "true"
	}
	if out, err := exec.Command("timedatectl", "set-ntp", state).CombinedOutput(); err != nil {
		return false, cmdErr("switch NTP "+map[bool]string{true: "on", false: "off"}[enabled], out, err)
	}
	// set-ntp starts/stops the unit but doesn't reload a config it was already
	// running with, so a server-list change on an already-enabled host needs an
	// explicit restart to take effect.
	if enabled && len(servers) > 0 {
		switch info.Manager {
		case "chrony":
			runQuiet("systemctl", "try-restart", "chronyd")
			runQuiet("systemctl", "try-restart", "chrony")
		case "ntpd":
			runQuiet("systemctl", "try-restart", "ntpd")
		default:
			runQuiet("systemctl", "try-restart", "systemd-timesyncd")
		}
	}
	return true, ""
}

// setTimesyncdServers rewrites only the NTP= line of timesyncd.conf, preserving
// everything else in the file — FallbackNTP=, PollIntervalMaxSec=, comments a
// distro or an operator put there. parapet writes this file wholesale; doing the
// same here would quietly discard those, and the operator would have no way to
// tell that a save on the *server list* had also dropped their poll interval.
func setTimesyncdServers(servers []string) error {
	return setTimesyncdServersAt(timesyncdConfPath, servers)
}

// timesyncdConfPath is where systemd-timesyncd keeps its config. Split out as a
// variable purely so setTimesyncdServersAt's line-preserving behaviour — the
// whole point of not writing this file wholesale — is testable against a temp
// file instead of only ever against the real /etc.
var timesyncdConfPath = "/etc/systemd/timesyncd.conf"

func setTimesyncdServersAt(path string, servers []string) error {
	line := "NTP=" + strings.Join(servers, " ")
	body, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return writeFilePreserving(path, []byte("# Managed by [gravinet] (System > Time).\n[Time]\n"+line+"\n"), 0o644)
	}
	var out []string
	placed, sawTime := false, false
	for _, ln := range strings.Split(string(body), "\n") {
		t := strings.TrimSpace(ln)
		if strings.EqualFold(t, "[Time]") {
			sawTime = true
			out = append(out, ln, line)
			placed = true
			continue
		}
		// Drop any existing NTP= (but never FallbackNTP=, which is a different
		// key that happens to end in the same three letters).
		if isDirective(t, "NTP") {
			continue
		}
		out = append(out, ln)
	}
	if !sawTime {
		out = append(out, "[Time]", line)
	} else if !placed {
		out = append(out, line)
	}
	return writeFilePreserving(path, []byte(strings.Join(out, "\n")), 0o644)
}

// SetHostClock sets the wall clock from an ISO-8601 local datetime string
// ("2026-07-23T14:30" or "...:30:00"). It refuses while NTP is on, exactly as
// parapet's page does: a daemon that is actively steering the clock will undo a
// manual set within seconds, so accepting one would be theatre.
func SetHostClock(iso string) (bool, string) {
	iso = strings.TrimSpace(iso)
	if iso == "" {
		return false, "no date and time given"
	}
	t, err := parseLocalDateTime(iso)
	if err != nil {
		return false, err.Error()
	}
	if info := HostTime(); info.NTPKnown && info.NTPEnabled {
		return false, "NTP is on — clear the time servers to switch it off before setting the clock by hand, or the sync daemon will just put it back"
	}

	switch runtime.GOOS {
	case "linux":
		stamp := t.Format("2006-01-02 15:04:05")
		if haveCmd("timedatectl") {
			if out, err := exec.Command("timedatectl", "set-time", stamp).CombinedOutput(); err == nil {
				return true, ""
			} else if !haveCmd("date") {
				return false, cmdErr("set the clock", out, err)
			}
		}
		if out, err := exec.Command("date", "-s", stamp).CombinedOutput(); err != nil {
			return false, cmdErr("set the clock", out, err)
		}
		return true, ""
	case "darwin":
		if !haveCmd("systemsetup") {
			return false, "setting the clock needs systemsetup, which isn't on this host"
		}
		// systemsetup splits date and time, and wants MM:DD:YY / HH:MM:SS.
		if out, err := exec.Command("systemsetup", "-setdate", t.Format("01:02:06")).CombinedOutput(); err != nil {
			return false, cmdErr("set the date", out, err)
		}
		if out, err := exec.Command("systemsetup", "-settime", t.Format("15:04:05")).CombinedOutput(); err != nil {
			return false, cmdErr("set the time", out, err)
		}
		return true, ""
	case "freebsd":
		if out, err := exec.Command("date", "-f", "%Y-%m-%d %H:%M:%S", t.Format("2006-01-02 15:04:05")).CombinedOutput(); err != nil {
			return false, cmdErr("set the clock", out, err)
		}
		return true, ""
	case "openbsd":
		// OpenBSD's date(1) has no -f; its positional form is
		// [[[[[cc]yy]mm]dd]HH]MM[.SS].
		if out, err := exec.Command("date", t.Format("200601021504.05")).CombinedOutput(); err != nil {
			return false, cmdErr("set the clock", out, err)
		}
		return true, ""
	case "windows":
		shell := "powershell"
		if !haveCmd(shell) {
			shell = "pwsh"
		}
		if !haveCmd(shell) {
			return false, "setting the clock needs PowerShell, which isn't on this host"
		}
		arg := "Set-Date -Date '" + t.Format("2006-01-02 15:04:05") + "'"
		if out, err := exec.Command(shell, "-NoProfile", "-Command", arg).CombinedOutput(); err != nil {
			return false, cmdErr("set the clock", out, err)
		}
		return true, ""
	default:
		return false, "gravinet can't set the clock on " + runtime.GOOS
	}
}

// parseLocalDateTime accepts what an <input type="datetime-local"> produces,
// with or without seconds, and interprets it in the host's own zone — the same
// zone the page displayed, so "set it to 14:30" means 14:30 as shown.
func parseLocalDateTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("date and time must look like 2026-07-23T14:30 (got %q)", s)
}

// ── validation ──────────────────────────────────────────────────────────────

// validTimezone rejects anything that isn't plausibly a zone name before it
// reaches a command line or a path join. Spaces are allowed because Windows zone
// ids contain them ("US Mountain Standard Time"); path traversal and shell
// metacharacters are not, which is what keeps the /usr/share/zoneinfo join and
// the exec argument safe even though the value came from a browser.
func validTimezone(tz string) error {
	if len(tz) > 96 {
		return fmt.Errorf("timezone name is too long")
	}
	for _, r := range tz {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '_' || r == '-' || r == '+' || r == ' ' || r == '.':
		default:
			return fmt.Errorf("timezone %q contains characters that aren't allowed in a zone name", tz)
		}
	}
	if strings.Contains(tz, "..") {
		return fmt.Errorf("timezone %q looks like a path traversal", tz)
	}
	if strings.HasPrefix(tz, "/") {
		return fmt.Errorf("timezone must be a zone name, not an absolute path")
	}
	return nil
}

// validNTPServer accepts a hostname, IPv4 address, or bracketed/bare IPv6
// address, and nothing that could turn into a second argument or command.
func validNTPServer(s string) error {
	if len(s) > 253 {
		return fmt.Errorf("time server address is too long")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == ':' || r == '_' || r == '[' || r == ']' || r == '%':
		default:
			return fmt.Errorf("time server %q contains characters that aren't allowed in an address", s)
		}
	}
	return nil
}

// ── small helpers ───────────────────────────────────────────────────────────

func haveCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// cmdOut runs a command and returns its combined output, or "" if it couldn't
// run at all. Failures are deliberately indistinguishable from empty output:
// every caller is probing for information it can do without.
func cmdOut(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	return string(out)
}

// runOK reports whether a command exited zero, discarding its output — for the
// `rcctl check` / `service status` probes where the exit code *is* the answer.
func runOK(name string, args ...string) bool {
	return exec.Command(name, args...).Run() == nil
}

func runQuiet(name string, args ...string) {
	if !haveCmd(name) {
		return
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	cmd.Run()
}

// unitActive reports whether a systemd unit is currently active. Used instead of
// "is the binary installed" so we configure the implementation that's actually
// running (see readLinuxTime).
func unitActive(unit string) bool {
	if !haveCmd("systemctl") {
		return false
	}
	return runOK("systemctl", "is-active", "--quiet", unit)
}

// keyVals parses KEY=value output (timedatectl show, systemd's --property form).
func keyVals(s string) map[string]string {
	m := make(map[string]string)
	for _, ln := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// afterColon returns the part of a "Label: value" line after the first colon —
// systemsetup and w32tm both report that way. Lines with no colon yield "".
func afterColon(s string) string {
	_, v, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// lineContaining returns the first line of s containing sub, or "".
func lineContaining(s, sub string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, sub) {
			return ln
		}
	}
	return ""
}

// isDirective reports whether a trimmed config line sets the given directive —
// i.e. the keyword followed by whitespace or '=', so "NTP" doesn't match
// "FallbackNTP=" and "server" doesn't match "servertimeout".
func isDirective(line, keyword string) bool {
	if len(line) <= len(keyword) || !strings.EqualFold(line[:len(keyword)], keyword) {
		return false
	}
	switch line[len(keyword)] {
	case ' ', '\t', '=':
		return true
	}
	return false
}

// directiveValues collects the values of the given directives from a config
// file, in file order: "server a", "pool b", "NTP=c d" all contribute. Commented
// lines are skipped, so a distro's commented-out example pool doesn't show up in
// the UI as if it were configured.
func directiveValues(path string, keywords ...string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		for _, kw := range keywords {
			if !isDirective(t, kw) {
				continue
			}
			rest := strings.TrimSpace(t[len(kw):])
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "="))
			// A chrony/ntpd server line can carry options after the address
			// ("server x iburst"); the address is the first field. An NTP= line
			// is a whole space-separated list, and every field is an address.
			fields := strings.Fields(rest)
			if strings.EqualFold(kw, "NTP") {
				out = append(out, fields...)
			} else if len(fields) > 0 {
				out = append(out, fields[0])
			}
			break
		}
	}
	return out
}

// setDirectiveLines replaces every line setting one of `drop` with the given
// replacement lines, leaving the rest of the file — comments, options,
// unrelated directives — exactly as it was. The replacements go where the first
// dropped line was, so a hand-ordered file keeps its shape.
func setDirectiveLines(path string, drop []string, replacement []string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var out []string
	placed := false
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		matched := false
		if t != "" && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, ";") {
			for _, kw := range drop {
				if isDirective(t, kw) {
					matched = true
					break
				}
			}
		}
		if !matched {
			out = append(out, ln)
			continue
		}
		if !placed {
			out = append(out, replacement...)
			placed = true
		}
	}
	if !placed {
		out = append(out, replacement...)
	}
	return writeFilePreserving(path, []byte(strings.Join(out, "\n")), 0o644)
}

// writeFilePreserving writes via a temp file in the same directory and renames,
// so a reader (or the sync daemon itself) never sees a half-written config, and
// keeps the original's permissions when there was one.
func writeFilePreserving(path string, body []byte, mode os.FileMode) error {
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".gravinet-tmp"
	if err := os.WriteFile(tmp, body, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// zoneFromLocaltime recovers the IANA zone name from /etc/localtime when it's a
// symlink into the zoneinfo tree — the portable read that works on every unix
// here, and the only one available on OpenBSD.
func zoneFromLocaltime() string {
	dst, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	if i := strings.Index(dst, "zoneinfo/"); i >= 0 {
		return dst[i+len("zoneinfo/"):]
	}
	return ""
}

// prefixEach returns each item with prefix prepended, for building config lines
// ("server 0.pool.ntp.org") or peer-list entries.
func prefixEach(prefix string, items []string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, prefix+s)
	}
	return out
}

// cmdErr turns a failed invocation into a one-line hint, preferring the
// command's own output — the same shape (and for the same reason) as power.go's
// powerErr, which is where "Interactive authentication required" and friends
// come from.
func cmdErr(what string, out []byte, err error) string {
	msg := trimOneLine(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return "couldn't " + what + ": " + msg
}
