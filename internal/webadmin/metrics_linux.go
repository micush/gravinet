//go:build linux

package webadmin

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// readCPUTotals reads aggregate CPU jiffies from /proc/stat: returns total and
// idle (idle+iowait). Utilization is derived from deltas between samples.
func readCPUTotals() (total, idle uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // user nice system idle iowait irq softirq steal ...
		var sum uint64
		for i, fv := range fields {
			n, _ := strconv.ParseUint(fv, 10, 64)
			sum += n
			if i == 3 || i == 4 { // idle + iowait
				idle += n
			}
		}
		return sum, idle, true
	}
	return 0, 0, false
}

// readMemUsedPct reads /proc/meminfo and returns used memory as a percentage,
// preferring MemAvailable (kernel's accurate reclaimable estimate).
func readMemUsedPct() (float64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var total, avail uint64
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, _ = strconv.ParseUint(fields[1], 10, 64)
			haveTotal = true
		case "MemAvailable:":
			avail, _ = strconv.ParseUint(fields[1], 10, 64)
			haveAvail = true
		}
	}
	if !haveTotal || !haveAvail || total == 0 {
		return 0, false
	}
	return float64(total-avail) / float64(total) * 100, true
}

// readDiskUsedPct reports used space on / as a percentage, via statfs(2).
// Like readMemUsedPct, it uses the "available to the caller" figure
// (Bavail, which excludes blocks reserved for root) rather than Bfree, so
// the percentage lines up with what `df` reports rather than what's
// technically unallocated.
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

// readUptime reads /proc/uptime, whose first field is seconds elapsed since
// boot (a float; the fractional part is dropped since the collector only
// needs whole-second granularity).
func readUptime() (uint64, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) < 1 {
		return 0, false
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs < 0 {
		return 0, false
	}
	return uint64(secs), true
}

// readNetDev parses /proc/net/dev into per-interface byte counters.
func readNetDev() map[string]devCounters {
	out := map[string]devCounters{}
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue // header lines have no colon
		}
		name := strings.TrimSpace(line[:idx])
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 16 {
			continue
		}
		// fields: rx_bytes ... (col 0), tx_bytes is col 8 of the post-colon stats.
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		out[name] = devCounters{rx: rx, tx: tx}
	}
	return out
}
