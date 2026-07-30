package service

// Host remote-syslog forwarding — the backend for the web admin's System >
// Syslog page. Points this host's *local* syslog daemon at one or more
// remote collectors; deliberately additive, never a replacement — whatever
// this host already logs locally (its distro's default syslog.conf/
// rsyslog.conf rules) keeps landing wherever it already lands. gravinet
// only ever adds or removes its own clearly-marked forwarding rules
// alongside that, the same non-destructive "rewrite only our own delimited
// block, leave everything else exactly as it was" shape package hosts
// already uses for the OS hosts file.
//
// Like Resolver and Time (see hosttime.go's package comment for the full
// argument), the host's own config file is the source of truth here —
// nothing is stored in gravinet's own config.json. A syslog daemon is
// exactly the kind of thing a distro, a cloud-init template, or a fleet
// config tool is just as likely to already manage; gravinet reads what's
// there and writes through to it rather than keeping a second copy that
// could silently win on the next reload. That includes a target's on/off
// state: a disabled target is still written to the file (as a distinguishable
// comment line — see the "gravinet-disabled: " prefix used throughout this
// file), so unchecking a row in the web admin and coming back later still
// shows it, unchecked, rather than it quietly disappearing because nothing
// short of an active forwarding line survives a re-read.
//
// Supported:
//
//	linux:   rsyslog only — by far the most common syslog daemon (default
//	         on Debian/Ubuntu, RHEL/Fedora/Rocky/Alma, Amazon Linux, SUSE).
//	         Forwarding goes in its own drop-in under rsyslog.d — that file
//	         *is* the managed block, so no in-file markers are needed, the
//	         same "our own file, not shared" shape the NetworkManager/
//	         systemd-resolved drop-ins in hostresolver.go already use.
//	         syslog-ng and other Linux syslogd variants are deliberately
//	         not supported: syslog-ng's config depends on a distro-defined
//	         source name gravinet has no reliable way to discover, and
//	         guessing one risks a config that fails to load at all — the
//	         same "half-adapting would be more likely to be subtly wrong
//	         than honestly absent" call snmp.go's Windows support makes.
//	freebsd: classic BSD syslogd via /etc/syslog.conf's remote-forward
//	         syntax ("@host:port" for UDP, "@@host:port" for TCP) — a
//	         managed block appended to the existing file, mirroring
//	         hosts.Render.
//	openbsd: same as freebsd; OpenBSD's syslogd speaks the identical
//	         "@"/"@@" syntax.
//	darwin:  not supported. Apple's syslogd is legacy, superseded by the
//	         unified logging system (os_log/Console), which has no
//	         supported remote-forwarding mechanism gravinet can drive.
//	windows: not supported. No BSD-syslog-compatible daemon exists; the
//	         Windows Event Log is a fundamentally different model (typed
//	         channels and providers, not a wire protocol any of this
//	         speaks) — the same reasoning snmp.go gives for excluding it.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// SyslogTarget is one configured remote syslog forwarding destination.
// Disabled follows the same zero-value-is-enabled convention as gravinet's
// own firewall rules/exempts (config.FirewallRule.Disabled,
// config.FirewallExempt.Disabled): a target with Disabled left unset is
// active, so entries round-tripped from before this field existed (there
// weren't any, but future callers following the convention benefit too)
// stay in force.
type SyslogTarget struct {
	Remote   string // hostname or literal IP address; no port, no brackets
	Port     int    // 1-65535
	Protocol string // "udp" or "tcp"
	Disabled bool
}

// SyslogInfo is the live remote-syslog-forwarding state of the host.
type SyslogInfo struct {
	Targets   []SyslogTarget
	Manager   string // which syslog daemon this is applied through, for display
	CanSyslog bool   // is a forwarding change possible on this host
	Hint      string // why CanSyslog is false, if it is
}

// SyslogSupported reports whether this host can forward its syslog to a
// remote collector at all — the daemon this package knows how to drive is
// installed, on a platform this package supports one on. It's the single
// source of truth /api/config's capability flag, HostSyslog, and
// SetHostSyslog all share, so the menu the user sees and the endpoint that
// backs it can never disagree (same shape as SNMPSupported/LLDPSupported).
func SyslogSupported() (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if !haveCmd("rsyslogd") {
			return false, "syslog forwarding needs rsyslog, which isn't on this host"
		}
		return true, ""
	case "freebsd", "openbsd":
		if !haveCmd("syslogd") {
			return false, "syslog forwarding needs syslogd, which isn't on this host"
		}
		return true, ""
	default:
		return false, "syslog forwarding isn't supported on this operating system"
	}
}

// HostSyslog reads the host's current remote-syslog-forwarding state.
func HostSyslog() SyslogInfo {
	var info SyslogInfo
	info.CanSyslog, info.Hint = SyslogSupported()
	if !info.CanSyslog {
		return info
	}
	switch runtime.GOOS {
	case "linux":
		info.Manager = "rsyslog"
		if targets, ok := parseRsyslogDropInAt(rsyslogDropIn); ok {
			info.Targets = targets
		}
	case "freebsd", "openbsd":
		info.Manager = "syslogd"
		if targets, ok := parseBSDSyslogBlock(syslogConfPath); ok {
			info.Targets = targets
		}
	}
	return info
}

// SetHostSyslog replaces the full set of remote syslog-forwarding targets
// with targets, validating every entry before touching anything on disk —
// a single bad entry rejects the whole save rather than partially applying
// it, so the file on disk and what the admin thinks they just saved never
// diverge. An empty list removes gravinet's managed forwarding entirely
// (the drop-in is deleted / the managed block is cleared).
func SetHostSyslog(targets []SyslogTarget) (bool, string) {
	if ok, hint := SyslogSupported(); !ok {
		return false, hint
	}
	cleaned := make([]SyslogTarget, len(targets))
	for i, t := range targets {
		remote := strings.TrimSpace(t.Remote)
		if remote == "" {
			return false, fmt.Sprintf("target %d: remote is required", i+1)
		}
		if err := validSyslogHost(remote); err != nil {
			return false, fmt.Sprintf("target %d: %v", i+1, err)
		}
		if t.Port < 1 || t.Port > 65535 {
			return false, fmt.Sprintf("target %d: port must be 1-65535", i+1)
		}
		proto := strings.ToLower(strings.TrimSpace(t.Protocol))
		if proto != "udp" && proto != "tcp" {
			return false, fmt.Sprintf("target %d: protocol must be %q or %q", i+1, "udp", "tcp")
		}
		cleaned[i] = SyslogTarget{Remote: remote, Port: t.Port, Protocol: proto, Disabled: t.Disabled}
	}
	switch runtime.GOOS {
	case "linux":
		return setLinuxSyslog(cleaned)
	default: // freebsd, openbsd — the only other case SyslogSupported() lets through
		return setBSDSyslog(cleaned)
	}
}

// ── linux (rsyslog) ─────────────────────────────────────────────────────────

// rsyslogDropIn is entirely gravinet's own file — nothing else should ever
// write here — so unlike the BSD path below it needs no in-file markers:
// the file's mere presence/absence *is* whether any forwarding is
// configured at all (individual targets are still toggled via the
// "gravinet-disabled: " comment prefix below, same as the BSD side).
const rsyslogDropIn = "/etc/rsyslog.d/60-gravinet-syslog-forward.conf"

func setLinuxSyslog(targets []SyslogTarget) (bool, string) {
	if len(targets) == 0 {
		if err := os.Remove(rsyslogDropIn); err != nil && !os.IsNotExist(err) {
			return false, "couldn't remove " + rsyslogDropIn + ": " + err.Error()
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(rsyslogDropIn), 0o755); err != nil {
			return false, "couldn't create " + filepath.Dir(rsyslogDropIn) + ": " + err.Error()
		}
		if err := writeFilePreserving(rsyslogDropIn, []byte(renderRsyslogDropIn(targets)), 0o644); err != nil {
			return false, "couldn't write " + rsyslogDropIn + ": " + err.Error()
		}
	}
	reloadRsyslog()
	return true, ""
}

// rsyslogDisabledPrefix marks a target line as present but inactive: still
// a comment as far as rsyslog is concerned (so it never takes effect), but
// distinguishable from an ordinary explanatory comment so
// parseRsyslogDropInAt can recover it as a disabled target rather than
// silently dropping it.
const rsyslogDisabledPrefix = "# gravinet-disabled: "

// renderRsyslogDropIn renders rsyslogDropIn's content for a set of targets —
// one omfwd action() line per enabled target (the RainerScript syntax
// rsyslog's omfwd module reads for a forwarding rule), and one
// gravinet-disabled-prefixed comment line per disabled one.
func renderRsyslogDropIn(targets []SyslogTarget) string {
	var b strings.Builder
	b.WriteString("# Managed by gravinet (System > Syslog). Local logging is untouched;\n")
	b.WriteString("# this file only adds forwarding copies alongside it. Removing a target\n")
	b.WriteString("# in gravinet (or deleting this file) removes only that.\n")
	for _, t := range targets {
		line := fmt.Sprintf("*.* action(type=%q target=%q port=%q protocol=%q)",
			"omfwd", t.Remote, strconv.Itoa(t.Port), t.Protocol)
		if t.Disabled {
			line = rsyslogDisabledPrefix + line
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// parseRsyslogDropInAt reads back the targets gravinet itself last wrote to
// path. It only ever needs to understand output this same code produced —
// the file is entirely gravinet's own — so this reads the three quoted
// action() attributes back out of each line directly rather than parsing
// rsyslog's full action() grammar. ok reports whether the file could be
// read at all (mirrors the old single-target semantics: a missing file
// means "nothing configured", not an error).
func parseRsyslogDropInAt(path string) (targets []SyslogTarget, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		s := ln
		disabled := false
		if rest, cut := strings.CutPrefix(s, rsyslogDisabledPrefix); cut {
			disabled, s = true, rest
		}
		if !strings.Contains(s, "action(type=") {
			continue
		}
		host := attrValue(s, "target")
		portStr := attrValue(s, "port")
		proto := attrValue(s, "protocol")
		if host == "" || portStr == "" || proto == "" {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		targets = append(targets, SyslogTarget{Remote: host, Port: port, Protocol: proto, Disabled: disabled})
	}
	return targets, true
}

// attrValue extracts a double-quoted attribute value from an rsyslog
// action() line, e.g. attrValue(s, "target") on `target="1.2.3.4"` returns
// "1.2.3.4". Used only against gravinet's own previously written drop-in.
func attrValue(s, key string) string {
	marker := key + `="`
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// reloadRsyslog best-effort asks rsyslog to pick up a freshly written or
// removed drop-in. Mirrors setLinuxResolvedDNS's own reasoning in
// hostresolver.go: the file write is what actually matters — rsyslog reads
// it on its own next start regardless — so a reload failure here (rsyslog
// not currently running, no permission to signal it, whatever) isn't
// surfaced as a save failure; the config is already correctly on disk
// either way.
func reloadRsyslog() {
	if haveCmd("systemctl") {
		runQuiet("systemctl", "reload-or-restart", "rsyslog")
		return
	}
	runQuiet("service", "rsyslog", "restart")
}

// ── freebsd / openbsd (syslogd) ──────────────────────────────────────────────

const syslogConfPath = "/etc/syslog.conf"

func syslogBeginMarker() string {
	return "# BEGIN gravinet syslog-forward (managed by gravinet — do not edit within this block)"
}
func syslogEndMarker() string { return "# END gravinet syslog-forward" }

func setBSDSyslog(targets []SyslogTarget) (bool, string) {
	block := ""
	if len(targets) > 0 {
		block = renderBSDSyslogBlock(targets)
	}
	if err := setSyslogManagedBlock(syslogConfPath, block); err != nil {
		return false, "couldn't update " + syslogConfPath + ": " + err.Error()
	}
	reloadBSDSyslogd()
	return true, ""
}

// reloadBSDSyslogd best-effort restarts syslogd to pick up a freshly
// rewritten /etc/syslog.conf. See reloadRsyslog's doc comment for why a
// failure here doesn't fail the save — the config is already correctly on
// disk regardless of whether the running daemon picks it up immediately.
func reloadBSDSyslogd() {
	if runtime.GOOS == "openbsd" {
		runQuiet("rcctl", "restart", "syslogd")
		return
	}
	runQuiet("service", "syslogd", "restart")
}

// renderBSDSyslogBlock renders the gravinet-managed block for
// /etc/syslog.conf: one line per target using BSD syslogd's own
// remote-forward syntax — a selector (here "*.*", everything) followed by
// "@host:port" to forward over UDP or "@@host:port" over TCP —
// long-standing, documented behavior shared by both FreeBSD's and
// OpenBSD's syslogd. A disabled target's line is prefixed the same
// gravinet-disabled way the rsyslog side uses, so it's inert (a comment,
// as far as syslogd parses the file) but still recoverable on the next read.
func renderBSDSyslogBlock(targets []SyslogTarget) string {
	var b strings.Builder
	b.WriteString(syslogBeginMarker())
	b.WriteString("\n")
	for _, t := range targets {
		prefix := "@"
		if t.Protocol == "tcp" {
			prefix = "@@"
		}
		line := fmt.Sprintf("*.*\t%s%s:%d", prefix, t.Remote, t.Port)
		if t.Disabled {
			line = rsyslogDisabledPrefix + line
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(syslogEndMarker())
	b.WriteString("\n")
	return b.String()
}

// parseBSDSyslogBlock reads back the targets from the gravinet-managed
// block in path, or ok=false if there is no such block at all (mirrors the
// old single-target semantics: no block means "nothing configured", not
// an error — distinct from "a block exists but currently lists zero
// targets", which can't actually occur since setBSDSyslog clears the block
// entirely rather than writing an empty one).
func parseBSDSyslogBlock(path string) (targets []SyslogTarget, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	inBlock := false
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case t == syslogBeginMarker():
			inBlock, ok = true, true
			continue
		case t == syslogEndMarker():
			inBlock = false
			continue
		case !inBlock:
			continue
		}
		disabled := false
		if rest, cut := strings.CutPrefix(t, rsyslogDisabledPrefix); cut {
			disabled, t = true, rest
		}
		fields := strings.Fields(t)
		if len(fields) != 2 {
			continue
		}
		var proto, hostport string
		switch {
		case strings.HasPrefix(fields[1], "@@"):
			proto, hostport = "tcp", strings.TrimPrefix(fields[1], "@@")
		case strings.HasPrefix(fields[1], "@"):
			proto, hostport = "udp", strings.TrimPrefix(fields[1], "@")
		default:
			continue
		}
		host, portStr, err := net.SplitHostPort(hostport)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		targets = append(targets, SyslogTarget{Remote: host, Port: port, Protocol: proto, Disabled: disabled})
	}
	return targets, ok
}

// setSyslogManagedBlock rewrites only the gravinet-managed block in path
// (delimited by syslogBeginMarker/syslogEndMarker) to block, leaving every
// other line — including whatever this host already logs locally by
// default — untouched. block == "" removes the managed block entirely.
// Mirrors the same non-destructive block-replace package hosts already
// does for the OS hosts file (hosts.Render).
func setSyslogManagedBlock(path, block string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var kept []string
	inBlock := false
	for _, ln := range strings.Split(string(existing), "\n") {
		t := strings.TrimSpace(ln)
		if t == syslogBeginMarker() {
			inBlock = true
			continue
		}
		if t == syslogEndMarker() {
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}
		kept = append(kept, ln)
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	var b strings.Builder
	if len(kept) > 0 {
		b.WriteString(strings.Join(kept, "\n"))
		b.WriteString("\n")
	}
	if block != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(block)
	}
	return writeFilePreserving(path, []byte(b.String()), 0o644)
}

// ── validation ────────────────────────────────────────────────────────────

// validSyslogHost requires a bare hostname or literal IP address — no port,
// no brackets (Remote and Port are separate structured fields, so there's
// no "[ipv6]:port" combined form to unpick). Reuses validHostname's
// character-set restriction for a non-IP host, for the same
// injection-safety reason validTimezone/validNTPServer apply it elsewhere
// in this package: whatever passes here is written straight into a
// daemon's own config file, and a directive value is just as capable of
// smuggling a second directive via a stray newline or quote as an
// exec.Command argument is of smuggling a second command.
func validSyslogHost(host string) error {
	if net.ParseIP(host) != nil {
		return nil
	}
	if err := validHostname(host); err != nil {
		return fmt.Errorf("invalid syslog target host %q", host)
	}
	return nil
}
