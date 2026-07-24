package service

// Link-layer discovery agent (lldpd) management — the backend for System >
// L2 Disco. Ported from parapet's LldpManager (src/lldpd.rs) and its
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

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"

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
	if lldpdBinary() == "" {
		return false, "lldpd isn't installed on this host (is the lldpd package installed?)"
	}
	return true, ""
}

func lldpdBinary() string {
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
// space-joined systemd drop-in line. Exported so handleSystemL2Disco can
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
		flags := strings.Join(args[1:], " ")
		if out, err := exec.Command("rcctl", "set", "lldpd", "flags", flags).CombinedOutput(); err != nil {
			return false, cmdErr("rcctl set lldpd flags", out, err)
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

// lldpJournalHint runs `journalctl -u lldpd.service` and returns a short,
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
func lldpJournalHint() string {
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

func lldpServiceStart() (bool, string) {
	removeStaleLLDPSockets()
	switch runtime.GOOS {
	case "linux":
		if out, err := exec.Command("systemctl", "enable", "--now", "lldpd.service").CombinedOutput(); err != nil {
			if out2, err2 := exec.Command("systemctl", "restart", "lldpd.service").CombinedOutput(); err2 != nil {
				return false, cmdErr("systemctl enable/restart lldpd.service", out, err) + "; " + cmdErr("systemctl restart", out2, err2) + lldpJournalHint()
			}
			exec.Command("systemctl", "enable", "lldpd.service").Run() // best-effort; restart already succeeded
		}
		return true, ""
	case "freebsd":
		exec.Command("sysrc", "lldpd_enable=YES").Run()
		if out, err := exec.Command("service", "lldpd", "restart").CombinedOutput(); err != nil {
			return false, cmdErr("service lldpd restart", out, err)
		}
		return true, ""
	case "openbsd":
		exec.Command("rcctl", "enable", "lldpd").Run()
		if out, err := exec.Command("rcctl", "restart", "lldpd").CombinedOutput(); err != nil {
			return false, cmdErr("rcctl restart lldpd", out, err)
		}
		return true, ""
	case "darwin":
		if out, err := exec.Command("brew", "services", "restart", "lldpd").CombinedOutput(); err != nil {
			return false, cmdErr("brew services restart lldpd", out, err)
		}
		return true, ""
	default:
		return false, "L2 discovery isn't supported on this operating system"
	}
}

func lldpServiceStop() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if out, err := exec.Command("systemctl", "disable", "--now", "lldpd.service").CombinedOutput(); err != nil {
			return false, cmdErr("systemctl disable --now lldpd.service", out, err)
		}
		return true, ""
	case "freebsd":
		exec.Command("sysrc", "lldpd_enable=NO").Run()
		if out, err := exec.Command("service", "lldpd", "stop").CombinedOutput(); err != nil && !strings.Contains(strings.ToLower(string(out)), "not running") {
			return false, cmdErr("service lldpd stop", out, err)
		}
		return true, ""
	case "openbsd":
		exec.Command("rcctl", "disable", "lldpd").Run()
		if out, err := exec.Command("rcctl", "stop", "lldpd").CombinedOutput(); err != nil && !strings.Contains(strings.ToLower(string(out)), "not running") {
			return false, cmdErr("rcctl stop lldpd", out, err)
		}
		return true, ""
	case "darwin":
		if out, err := exec.Command("brew", "services", "stop", "lldpd").CombinedOutput(); err != nil {
			return false, cmdErr("brew services stop lldpd", out, err)
		}
		return true, ""
	default:
		return false, "L2 discovery isn't supported on this operating system"
	}
}

// LLDPServiceRunning reports whether the lldpd service is currently active.
func LLDPServiceRunning() bool {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("systemctl", "is-active", "--quiet", "lldpd.service").Run() == nil
	case "freebsd":
		return exec.Command("service", "lldpd", "status").Run() == nil
	case "openbsd":
		return exec.Command("rcctl", "check", "lldpd").Run() == nil
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

// LLDPNeighbor is one discovered link-layer neighbor, as reported by
// `lldpcli show neighbors`. Mirrors parapet's discovery_row shape exactly
// (local_iface/system_name/port/mgmt_ip).
type LLDPNeighbor struct {
	LocalIface string
	SystemName string
	Port       string
	MgmtIP     string
}

// LLDPNeighbors queries the running lldpd (via lldpcli, which talks to
// whatever instance is running over its control socket regardless of who
// launched it) for its current neighbor table. Returns (neighbors,
// available, hint) — available is false with a reason when lldpcli isn't
// installed, lldpd isn't running, or its JSON couldn't be parsed; distinct
// from (available: true, empty neighbors), which just means no neighbors
// have been heard from yet.
func LLDPNeighbors() ([]LLDPNeighbor, bool, string) {
	cli := lldpcliBinary()
	if cli == "" {
		return nil, false, "lldpd is not installed"
	}
	out, err := exec.Command(cli, "-f", "json", "show", "neighbors").CombinedOutput()
	if err != nil {
		return nil, false, "lldpd is not running (enable link-layer discovery in System > L2 Disco)"
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
