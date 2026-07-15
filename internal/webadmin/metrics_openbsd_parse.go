package webadmin

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// This file deliberately has no _openbsd.go suffix and no openbsd build tag,
// unlike the rest of the OpenBSD metrics readers: the parsing itself has no
// OS dependency, and keeping it free of the platform restriction is what
// lets it be unit-tested here without an actual OpenBSD box to run sysctl or
// top on (mirroring metrics_darwin_parse.go's parseTopIdlePercent). Only
// metrics_openbsd.go's readCPUTotals/readMemUsedPct/readNetDev (which do the
// actual shell-outs) call these.

// splitOpenBSDSysctlList splits a sysctl(8) array-valued value into its
// individual numeric tokens. OpenBSD's sysctl joins array elements with
// commas — confirmed against a real `sysctl kern.cp_time` sample:
// "kern.cp_time=2391,0,1987,60,117,976656" — unlike FreeBSD's `sysctl -n
// kern.cp_time`, which is space-separated. Splitting on whitespace alone
// (e.g. with strings.Fields) leaves OpenBSD's output as a single
// comma-joined token that fails to parse as any of the individual tick
// counts — this was silently breaking the CPU reader outright, since
// parseOpenBSDCPUTime's "at least 5 fields" check saw a slice of length 1
// and returned ok=false every time. Both commas and whitespace are treated
// as separators so this tolerates either format.
func splitOpenBSDSysctlList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

// parseOpenBSDCPUTime parses the raw output of `sysctl -n kern.cp_time` into
// (total, idle) tick counts. OpenBSD's CPUSTATES layout isn't fixed the way
// FreeBSD/Linux's classic 5-tuple (user, nice, sys, intr, idle) is: OpenBSD
// 6.4 (May 2018) added a 6th state, CP_SPIN (time spent spinning on a kernel
// lock), giving user=0, nice=1, sys=2, spin=3, intr=4, idle=5, CPUSTATES=6
// (see sys/sys/sched.h) — before that release it was the classic 5-tuple
// with idle last. Rather than hardcode which of the two layouts a given
// kernel uses, this sums *every* field for total and takes the *last* field
// as idle: idle is the final element in both the 5- and 6-state layouts, so
// this is correct either way without needing to know the OpenBSD release
// underneath it.
func parseOpenBSDCPUTime(fields []string) (total, idle uint64, ok bool) {
	if len(fields) < 5 {
		return 0, 0, false
	}
	vals := make([]uint64, len(fields))
	for i, f := range fields {
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		vals[i] = n
		total += n
	}
	idle = vals[len(vals)-1]
	return total, idle, true
}

// openbsdMemRe pulls the three numbers off top(1)'s "Memory:" summary line,
// e.g. "Memory: Real: 244M/733M act/tot Free: 234M Cache: 193M Swap: ...":
// group 1 is "tot" (the second Real: number, "act" itself is unused here),
// group 2 is Free, group 3 is Cache. Each token may carry a K/M/G/T suffix
// or none at all (top's format_k scales for readability) — parseTopSize
// below resolves that.
var openbsdMemRe = regexp.MustCompile(`Real:\s*[0-9]+[A-Za-z]*/([0-9]+[A-Za-z]*)\s+act/tot\s+Free:\s*([0-9]+[A-Za-z]*)\s+Cache:\s*([0-9]+[A-Za-z]*)`)

// parseTopSize parses a top(1)-style size token — a bare integer (top's base
// display unit, KB) or one with a K/M/G/T suffix — into a KB-equivalent
// value. Only the trailing letter matters, so this tolerates whatever case
// top happens to print it in.
func parseTopSize(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		s = s[:len(s)-1]
	case 'M', 'm':
		mult = 1024
		s = s[:len(s)-1]
	case 'G', 'g':
		mult = 1024 * 1024
		s = s[:len(s)-1]
	case 'T', 't':
		mult = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n * mult, true
}

// parseOpenBSDMemLine parses OpenBSD top(1)'s one-line memory summary into a
// used-memory percentage. top computes that line from exactly the fields
// we'd otherwise have to pull out of struct uvmexp by hand (see OpenBSD's
// usr.bin/top/machine.c, get_system_info):
//
//	act (unused here) = uvmexp.active
//	tot               = uvmexp.npages - uvmexp.free  (everything not free)
//	free              = uvmexp.free
//	cache             = bcstats.numbufpages           (buffer cache pages)
//
// So total physical memory is tot+free, and — mirroring metrics_freebsd.go's
// choice to exclude reclaimable file cache from "used" — used is tot minus
// cache (i.e. active+inactive+wired+paging: everything that isn't free and
// isn't just a cache of something already on disk).
func parseOpenBSDMemLine(output []byte) (float64, bool) {
	m := openbsdMemRe.FindSubmatch(output)
	if m == nil {
		return 0, false
	}
	tot, ok1 := parseTopSize(string(m[1]))
	free, ok2 := parseTopSize(string(m[2]))
	cache, ok3 := parseTopSize(string(m[3]))
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	total := tot + free
	if total == 0 {
		return 0, false
	}
	used := tot
	if cache < used {
		used -= cache
	} else {
		used = 0
	}
	return float64(used) / float64(total) * 100, true
}

// parseOpenBSDNetstat parses `netstat -ibn` output into per-interface
// cumulative byte counters. Column positions are read from the header
// rather than hardcoded, the same as metrics_freebsd.go and
// metrics_darwin.go.
//
// Two things needed fixing here that don't apply to FreeBSD/Darwin:
//
//  1. Down interfaces. OpenBSD's netstat appends a literal "*" to the
//     interface name in the Name column for any interface that's down (see
//     netstat(1): "An asterisk after an interface name indicates that the
//     interface is down"). Left in place, "tun0*" never matches the plain
//     "tun0" key the caller looks up by, so the interface silently never
//     appears in the result. Stripped here unconditionally — it's harmless
//     to strip from an already-up interface's name, since there's nothing
//     to strip.
//
//  2. Point-to-point interfaces and the "<Link>" row. The previous version
//     of this parser only accepted a row whose Network column starts with
//     "<Link" — right for a NIC, where the link-layer row carries the
//     authoritative total and every per-address-family row is a same-or-
//     lesser subset of it (see the FreeBSD forum example this mirrors:
//     <Link#1>'s Ipkts is 232753649 vs. the address-specific row's
//     232729261 — the Link row counts non-IP traffic the address row
//     doesn't). But gravinet's own interfaces are tun(4) devices — pure L3,
//     point-to-point, no link layer at all — and it was never actually
//     confirmed whether OpenBSD's netstat synthesizes a "<Link>" placeholder
//     row for a device with no link-layer identity to report, the way it
//     does for the similarly address-less loopback (whose Link row uses the
//     interface's own name, e.g. "lo0", in place of a MAC). If it doesn't,
//     the strict "<Link" requirement meant a tun interface's only row was
//     always being discarded, and the interface never appeared in the
//     result at all. Now a Link row is still preferred when one is seen
//     (first-write-wins is reversed here: once a Link row is recorded for a
//     name, later non-Link rows for that same name are ignored, since a tun
//     device carries only one address family anyway so there's no
//     meaningful undercounting risk either way), but a name with no Link
//     row at all still gets recorded from whatever row it does have.
func parseOpenBSDNetstat(raw []byte) map[string]devCounters {
	out := map[string]devCounters{}
	fromLink := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	var nameIdx, netIdx, ibytesIdx, obytesIdx int = -1, -1, -1, -1
	first := true
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if first {
			first = false
			for i, h := range fields {
				switch h {
				case "Name":
					nameIdx = i
				case "Network":
					netIdx = i
				case "Ibytes":
					ibytesIdx = i
				case "Obytes":
					obytesIdx = i
				}
			}
			continue
		}
		if nameIdx < 0 || netIdx < 0 || ibytesIdx < 0 || obytesIdx < 0 {
			continue // header never parsed correctly; bail out gracefully
		}
		need := nameIdx
		for _, i := range []int{netIdx, ibytesIdx, obytesIdx} {
			if i > need {
				need = i
			}
		}
		if len(fields) <= need {
			continue
		}
		name := strings.TrimSuffix(fields[nameIdx], "*")
		isLink := strings.HasPrefix(fields[netIdx], "<Link")
		if fromLink[name] && !isLink {
			continue // a Link row already won for this interface; skip the subset row
		}
		rx, errRx := strconv.ParseUint(fields[ibytesIdx], 10, 64)
		tx, errTx := strconv.ParseUint(fields[obytesIdx], 10, 64)
		if errRx != nil || errTx != nil {
			continue
		}
		out[name] = devCounters{rx: rx, tx: tx}
		if isLink {
			fromLink[name] = true
		}
	}
	return out
}
