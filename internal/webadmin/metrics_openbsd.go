//go:build openbsd

package webadmin

import (
	"os/exec"
	"syscall"
	"time"
)

// This file implements the metrics readers for OpenBSD the same way
// metrics_freebsd.go and metrics_darwin.go do: shell out to stable,
// documented tools (sysctl, top, netstat) rather than hand-roll binary
// sysctl struct parsing. That matters even more here than on FreeBSD:
// OpenBSD's memory statistics (CTL_VM/VM_UVMEXP) come back as a single
// struct uvmexp with no individually-named sysctl leaves the way FreeBSD's
// vm.stats.vm.v_*_count are — the only way to read it without linking
// against the kernel's own struct layout (as e.g. gopsutil/oshi do, see
// their OpenBSD ports) is to go through a tool that already did that
// parsing for us, i.e. top(1). If a reader ever fails to parse, it returns
// ok=false and the Metrics tab reports that series unavailable rather than
// showing something misleading.
//
// The actual parsing (both here and for cp_time/netstat) lives in
// metrics_openbsd_parse.go so it can be unit-tested without an OpenBSD box.

// readCPUTotals uses `sysctl -n kern.cp_time`; see parseOpenBSDCPUTime for
// why the field count isn't assumed to be a fixed 5, and
// splitOpenBSDSysctlList for why the output isn't split on whitespace alone.
func readCPUTotals() (total, idle uint64, ok bool) {
	out, err := exec.Command("sysctl", "-n", "kern.cp_time").Output()
	if err != nil {
		return 0, 0, false
	}
	return parseOpenBSDCPUTime(splitOpenBSDSysctlList(string(out)))
}

// readMemUsedPct runs `top` in one-shot batch mode and parses its "Memory:"
// line; see parseOpenBSDMemLine for the field mapping. -b is passed
// explicitly even though piping stdout already makes top default to batch
// mode on its own (a non-terminal output stream is a "dumb terminal" as far
// as top(1) is concerned) — -d1 bounds it to exactly one display regardless
// of any $TOP environment default the invoking user has set, so this can't
// hang waiting for a second refresh.
func readMemUsedPct() (float64, bool) {
	out, err := exec.Command("top", "-b", "-d1").Output()
	if err != nil {
		return 0, false
	}
	return parseOpenBSDMemLine(out)
}

// readDiskUsedPct reports used space on / as a percentage, via statfs(2).
// This is the one metric here that doesn't need to shell out: unlike
// OpenBSD's memory statistics (see the file comment above), Go's syscall
// package ships a typed, verified Statfs_t layout for openbsd, so calling it
// directly is both simpler and no less trustworthy than parsing `df` output
// would be. F_bavail is signed (it can go negative once a user is over their
// reserved-blocks quota); that's clamped to 0 so a used-percentage over 100%
// is never reported.
func readDiskUsedPct() (float64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0, false
	}
	if st.F_blocks == 0 {
		return 0, false
	}
	avail := st.F_bavail
	if avail < 0 {
		avail = 0
	}
	total := float64(st.F_blocks) * float64(st.F_bsize)
	availBytes := float64(avail) * float64(st.F_bsize)
	return (total - availBytes) / total * 100, true
}

// readUptime reports system uptime via `sysctl -n kern.boottime`. macOS and
// FreeBSD's sysctl(8) print this OID as "{ sec = N, usec = N } <date>"
// (parseBoottimeSysctl, metrics_boottime_parse.go), but unlike cp_time and
// the other readers in this file — which were written and tested against
// real OpenBSD output (see metrics_openbsd_parse.go) — it isn't verified
// here whether OpenBSD's sysctl(8) uses that same struct-print convention
// for kern.boottime or instead prints a bare epoch integer. This tries the
// struct form first, then a bare integer, and reports unavailable if
// neither matches, rather than guess and risk silently reporting a wrong
// uptime — the same caution this file's other readers already apply to
// anything they can't parse with confidence.
func readUptime() (uint64, bool) {
	out, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return 0, false
	}
	boot, ok := parseBoottimeSysctl(out)
	if !ok {
		boot, ok = parseBareEpochSysctl(out)
	}
	if !ok {
		return 0, false
	}
	now := time.Now().Unix()
	if now < boot {
		return 0, false
	}
	return uint64(now - boot), true
}

// readNetDev shells out to `netstat -ibn`; see parseOpenBSDNetstat for the
// column mapping and the two OpenBSD-specific fixes (down-interface "*"
// suffix, and point-to-point interfaces that may have no "<Link>" row).
func readNetDev() map[string]devCounters {
	raw, err := exec.Command("netstat", "-ibn").Output()
	if err != nil {
		return map[string]devCounters{}
	}
	return parseOpenBSDNetstat(raw)
}
