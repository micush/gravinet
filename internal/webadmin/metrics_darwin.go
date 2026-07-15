//go:build darwin

package webadmin

import (
	"bufio"
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// This file implements the metrics readers for macOS by shelling out to the
// same stable, documented command-line tools tcpdump/Activity Monitor-style
// scripts have used for decades (sysctl, vm_stat, netstat), rather than
// hand-rolling Mach host_statistics() calls or binary sysctl struct parsing
// by hand. Those are certainly possible without cgo, but their exact
// in-memory struct layouts are the kind of thing that's easy to get subtly
// wrong without a real Mac to verify against — a quiet mis-parse is worse
// here than the modest cost of spawning a short-lived process every 2s.
// If any of these ever fail to parse (e.g. a future macOS changes the text
// format), the reader just returns ok=false and the Metrics tab reports that
// series unavailable rather than showing wrong numbers.

// readCPUTotals reports macOS CPU utilization via `top -l 1 -n 0`, whose
// summary line ("CPU usage: 8.23% user, 15.38% sys, 76.38% idle") is the same
// interface classic macOS shell one-liners have used for this for decades.
// `kern.cp_time` — the sysctl FreeBSD exposes for exactly this, and what this
// reader used to shell out to — either doesn't exist on macOS the same way or
// isn't reliably parseable there (unlike vm_stat/netstat below, which are
// solidly macOS-native), which is why the CPU graph came up permanently empty:
// this reader always returned ok=false, silently, by design (see the file
// comment) — no error ever surfaced, it just never had a first data point.
//
// top reports an instantaneous idle share for the interval since its own last
// sample, not an ever-increasing tick count the way Linux's /proc/stat or
// FreeBSD's kern.cp_time do. To fit the shared collector's delta-of-two-
// monotonic-counters math unchanged, this keeps a running (total, idle) pair
// across calls, advancing both by a fixed step every call and splitting the
// idle share of that step by the percentage top just reported — so a delta
// between any two calls (which is all the collector ever computes) reproduces
// the right percentage for the interval between them.
var (
	darwinCPUMu    sync.Mutex
	darwinCPUTotal uint64
	darwinCPUIdle  uint64
)

func readCPUTotals() (total, idle uint64, ok bool) {
	idlePct, ok := topIdlePercent()
	if !ok {
		return 0, 0, false
	}
	const step = 1_000_000 // arbitrary; only the ratio idle/total matters
	darwinCPUMu.Lock()
	defer darwinCPUMu.Unlock()
	darwinCPUTotal += step
	darwinCPUIdle += uint64(idlePct / 100 * step)
	return darwinCPUTotal, darwinCPUIdle, true
}

// topIdlePercent runs a single top(1) sample (no process list needed, so -n 0
// keeps it cheap) and parses its idle share. The actual parsing lives in
// parseTopIdlePercent (metrics_darwin_parse.go), split out — despite being
// used only here — so it can be unit-tested without an actual Mac to run top
// on; this function is the thin, untestable-here shell-out on top of it.
func topIdlePercent() (float64, bool) {
	out, err := exec.Command("top", "-l", "1", "-n", "0").Output()
	if err != nil {
		return 0, false
	}
	return parseTopIdlePercent(out)
}

// readMemUsedPct combines `sysctl -n hw.memsize` (total physical bytes) with
// `vm_stat` (page-granularity breakdown) to estimate used memory the same way
// Activity Monitor roughly does: active + wired + compressed pages counted as
// "used", free/inactive/speculative counted as available.
func readMemUsedPct() (float64, bool) {
	memOut, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, false
	}
	totalBytes, err := strconv.ParseUint(strings.TrimSpace(string(memOut)), 10, 64)
	if err != nil || totalBytes == 0 {
		return 0, false
	}

	vmOut, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, false
	}
	pageSize := uint64(4096) // vm_stat's header states the real value; parsed below when present
	stats := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(vmOut))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			// "Mach Virtual Memory Statistics: (page size of 4096 bytes)"
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := line[i+len("page size of "):]
				if sp := strings.IndexByte(rest, ' '); sp > 0 {
					if n, err := strconv.ParseUint(rest[:sp], 10, 64); err == nil && n > 0 {
						pageSize = n
					}
				}
			}
			continue
		}
		idx := strings.LastIndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		valStr := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[idx+1:]), "."))
		n, err := strconv.ParseUint(valStr, 10, 64)
		if err != nil {
			continue
		}
		stats[key] = n
	}

	wired, haveWired := stats["Pages wired down"]
	active, haveActive := stats["Pages active"]
	compressed, haveCompressed := stats["Pages occupied by compressor"]
	if !haveWired || !haveActive {
		return 0, false
	}
	if !haveCompressed {
		compressed = 0 // absent on older macOS releases without the memory compressor
	}
	usedBytes := (wired + active + compressed) * pageSize
	if usedBytes > totalBytes {
		usedBytes = totalBytes
	}
	return float64(usedBytes) / float64(totalBytes) * 100, true
}

// readDiskUsedPct reports used space on / as a percentage, via statfs(2).
// Unlike the CPU/memory readers above, this doesn't shell out: Go's syscall
// package already ships a typed, verified Statfs_t layout for darwin (unlike
// the raw sysctl blobs CPU/memory would need), so calling it directly is both
// simpler and no less trustworthy than parsing `df` output would be. As with
// readMemUsedPct, "used" is total minus what's actually available to the
// caller (Bavail), matching what `df` shows rather than raw free blocks.
func readDiskUsedPct() (float64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0, false
	}
	if st.Blocks == 0 {
		return 0, false
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	avail := float64(st.Bavail) * float64(st.Bsize)
	return (total - avail) / total * 100, true
}

// readUptime reports system uptime via `sysctl -n kern.boottime`, whose
// struct-timeval boot time this parses in parseBoottimeSysctl
// (metrics_boottime_parse.go) — shared with FreeBSD, whose sysctl(8) prints
// the identical struct form for this OID.
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
// Column positions are read from the header rather than hardcoded, since
// netstat's exact column set has varied slightly across macOS releases (e.g.
// a trailing "Drop" column on some, not others).
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
