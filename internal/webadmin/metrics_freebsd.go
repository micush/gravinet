//go:build freebsd

package webadmin

import (
	"bufio"
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// This file implements the metrics readers for FreeBSD the same way
// metrics_darwin.go does: shell out to stable, documented tools (sysctl,
// netstat) rather than hand-roll binary sysctl struct parsing, since a
// mis-parsed struct layout is a worse failure mode (silently wrong numbers)
// than the modest cost of a short-lived process every 2s. If a reader ever
// fails to parse, it returns ok=false and the Metrics tab reports that
// series unavailable rather than showing something misleading.

// readCPUTotals uses `sysctl -n kern.cp_time`, which reports cumulative
// scheduler ticks since boot as five integers: user, nice, sys, intr, idle
// (the classic CP_* ordering from <sys/dkstat.h>). FreeBSD and macOS share
// this exact convention — see metrics_darwin.go, whose implementation this
// mirrors precisely — so it plugs into the same total/idle delta math the
// collector already does.
func readCPUTotals() (total, idle uint64, ok bool) {
	out, err := exec.Command("sysctl", "-n", "kern.cp_time").Output()
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 5 {
		return 0, 0, false
	}
	vals := make([]uint64, 5)
	for i := 0; i < 5; i++ {
		n, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		vals[i] = n
		total += n
	}
	idle = vals[4] // CP_IDLE
	return total, idle, true
}

// readMemUsedPct combines hw.physmem (total physical bytes) with hw.pagesize
// and two of the vm.stats.vm.v_*_count page counters FreeBSD's own vm.stat(8)/
// top(1) are built on. "Used" here is wired + active pages — the same
// wired-plus-active-counts-as-used, everything-else-counts-as-available
// convention documented all over FreeBSD sysadmin tooling (e.g. the
// check_mem.pl BSD port, and how top(1) itself buckets "Wired"/"Active" vs.
// "Inact"/"Free") and the same shape as metrics_darwin.go's calculation
// (wired + active + compressed there; FreeBSD has no memory-compressor
// concept, so it's just wired + active here). "Cache" is deliberately left
// out: FreeBSD removed the cache page queue in recent releases, so
// v_cache_count reads 0 there anyway, and where it's still nonzero it's
// "almost available for allocation" territory rather than truly in-use.
func readMemUsedPct() (float64, bool) {
	out, err := exec.Command("sysctl", "-n",
		"hw.physmem", "hw.pagesize", "vm.stats.vm.v_wire_count", "vm.stats.vm.v_active_count").Output()
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 4 {
		return 0, false
	}
	vals := make([]uint64, 4)
	for i := 0; i < 4; i++ {
		n, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return 0, false
		}
		vals[i] = n
	}
	total, pageSize, wire, active := vals[0], vals[1], vals[2], vals[3]
	if total == 0 || pageSize == 0 {
		return 0, false
	}
	used := (wire + active) * pageSize
	if used > total {
		used = total
	}
	return float64(used) / float64(total) * 100, true
}

// readDiskUsedPct reports used space on / as a percentage, via statfs(2).
// Unlike the CPU/memory readers above, this doesn't shell out: Go's syscall
// package already ships a typed, verified Statfs_t layout for freebsd
// (unlike the raw sysctl values CPU/memory pull individually by name), so
// calling it directly is both simpler and no less trustworthy than parsing
// `df` output would be. Bavail is signed on FreeBSD (it can go negative once
// a user is over their reserved-blocks quota); that's clamped to 0 so a
// used-percentage over 100% is never reported.
func readDiskUsedPct() (float64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0, false
	}
	if st.Blocks == 0 {
		return 0, false
	}
	avail := st.Bavail
	if avail < 0 {
		avail = 0
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	availBytes := float64(avail) * float64(st.Bsize)
	return (total - availBytes) / total * 100, true
}

// readUptime reports system uptime via `sysctl -n kern.boottime`, whose
// struct-timeval boot time this parses in parseBoottimeSysctl
// (metrics_boottime_parse.go) — the same struct form macOS's sysctl(8)
// prints for this OID (see metrics_darwin.go, whose implementation this
// mirrors precisely).
func readUptime() (uint64, bool) {
	out, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return 0, false
	}
	boot, ok := parseBoottimeSysctl(out)
	if !ok {
		return 0, false
	}
	now := time.Now().Unix()
	if now < boot {
		return 0, false
	}
	return uint64(now - boot), true
}

// readNetDev parses `netstat -ibn`, picking the link-layer ("<Link#N>") row
// for each interface, which carries the authoritative cumulative byte counts
// (the same interface also has one row per configured address family, whose
// counters are duplicates of the link row rather than additional traffic).
// Column positions are read from the header rather than hardcoded: FreeBSD's
// column set is Name/Mtu/Network/Address/Ipkts/Ierrs/Idrop/Ibytes/Opkts/
// Oerrs/Obytes/Coll, one column ("Idrop") more than a typical macOS
// install's — this is the exact variation metrics_darwin.go's header-driven
// parsing (which this mirrors precisely) was already written to tolerate.
func readNetDev() map[string]devCounters {
	out := map[string]devCounters{}
	raw, err := exec.Command("netstat", "-ibn").Output()
	if err != nil {
		return out
	}
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
		if !strings.HasPrefix(fields[netIdx], "<Link") {
			continue // skip the per-address-family duplicate rows
		}
		rx, errRx := strconv.ParseUint(fields[ibytesIdx], 10, 64)
		tx, errTx := strconv.ParseUint(fields[obytesIdx], 10, 64)
		if errRx != nil || errTx != nil {
			continue
		}
		out[fields[nameIdx]] = devCounters{rx: rx, tx: tx}
	}
	return out
}
