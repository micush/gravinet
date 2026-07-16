package webadmin

import (
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Sampling cadence and retention for the Info -> Metrics graphs. 2s sampling
// over 60 minutes is ~1800 points per series — light to collect and to plot.
const (
	metricSampleInterval = 2 * time.Second
	metricRetention      = 60 * time.Minute
)

// metricPoint is one timestamped sample (unix seconds, value).
type metricPoint struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// ifaceMetrics is the per-interface throughput history (bytes/sec).
type ifaceMetrics struct {
	Network string        `json:"network"`
	Iface   string        `json:"iface"`
	Rx      []metricPoint `json:"rx"`
	Tx      []metricPoint `json:"tx"`

	// sampler state (not serialized)
	lastRx, lastTx uint64
	lastT          int64
	have           bool
}

// metricsCollector samples host CPU, memory, disk, and per-overlay-interface
// throughput on a fixed cadence and keeps a rolling 60-minute history, so the
// Metrics tab is fully populated the moment it is opened (no need to keep the
// page open to accumulate a graph). The actual CPU/memory/disk/interface-
// counter readers (readCPUTotals, readMemUsedPct, readDiskUsedPct,
// readNetDev) are implemented per platform (metrics_linux.go,
// metrics_darwin.go, metrics_windows.go); if a platform's readers can't get a
// value, they return ok=false and the collector just reports that series as
// unavailable rather than erroring.
type metricsCollector struct {
	be Backend

	mu     sync.Mutex
	cpu    []metricPoint
	mem    []metricPoint
	disk   []metricPoint
	ifaces map[string]*ifaceMetrics // keyed by interface name

	// uptimeSecs is the latest seconds-since-boot reading, not a rolling
	// history like cpu/mem/disk above: it's a single monotonically-
	// increasing counter with no shape worth graphing (a chart of it would
	// just be a straight diagonal line), so only the current value is kept.
	uptimeSecs uint64
	haveUptime bool

	// CPU delta state
	lastCPUTotal, lastCPUIdle uint64
	haveCPU                   bool

	available bool // whether /proc metrics could be read at all
	stop      chan struct{}
}

func newMetricsCollector(be Backend) *metricsCollector {
	return &metricsCollector{be: be, ifaces: map[string]*ifaceMetrics{}, stop: make(chan struct{})}
}

func (m *metricsCollector) run() {
	m.sample() // prime CPU/iface deltas (produces no rate points yet)
	t := time.NewTicker(metricSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.sample()
		}
	}
}

func (m *metricsCollector) close() { close(m.stop) }

// Swappable via package vars (rather than calling the platform readers
// directly), purely for testability: TestSampleRunsReadersConcurrently
// (metrics_test.go) substitutes slow fakes for a couple of these to prove
// sample()'s wall time is bounded by the slowest reader, not their sum —
// the same shape as the real-world macOS cost (top, vm_stat, sysctl x2,
// netstat all shelling out per sample) that pushed every graph's newest
// point behind actual wall-clock time. Never swapped outside tests.
var (
	readCPUTotalsFn   = readCPUTotals
	readMemUsedPctFn  = readMemUsedPct
	readDiskUsedPctFn = readDiskUsedPct
	readUptimeFn      = readUptime
	readNetDevFn      = readNetDev
)

func (m *metricsCollector) sample() {
	now := time.Now().Unix()
	cutoff := now - int64(metricRetention/time.Second)

	// The readers below range from a fast syscall/proc-file read (Linux,
	// FreeBSD, ...) to several short-lived subprocess spawns apiece (macOS:
	// top, vm_stat, sysctl x2, netstat — see metrics_darwin.go; top alone
	// has its own internal ~1s sampling delay by design, see its doc
	// comment there). Run one at a time, their costs add up — on a Mac
	// under any load that can comfortably exceed metricSampleInterval. And
	// since every series is only ever populated from this one function, a
	// slow run doesn't just delay one graph: it pushes CPU, memory, disk,
	// and every interface's newest point behind actual wall-clock time by
	// the same margin, all at once — which at the 1-minute zoom is visible
	// as every line, not just one, stopping short of the chart's right
	// edge (the frontend draws that edge from the browser's own clock, not
	// from whatever the newest point happens to be — see ui.go's
	// renderMetricGraphs). Running the readers concurrently bounds the
	// wall time to the slowest single one instead of their sum, and just
	// as importantly keeps that wall time off m.mu below, so a concurrent
	// /api/metrics request isn't stuck waiting on it either.
	var (
		wg                sync.WaitGroup
		cpuTotal, cpuIdle uint64
		cpuOK             bool
		memPct            float64
		memOK             bool
		diskPct           float64
		diskOK            bool
		uptimeSecs        uint64
		uptimeOK          bool
		dev               map[string]devCounters
	)
	wg.Add(5)
	go func() { defer wg.Done(); cpuTotal, cpuIdle, cpuOK = readCPUTotalsFn() }()
	go func() { defer wg.Done(); memPct, memOK = readMemUsedPctFn() }()
	go func() { defer wg.Done(); diskPct, diskOK = readDiskUsedPctFn() }()
	go func() { defer wg.Done(); uptimeSecs, uptimeOK = readUptimeFn() }()
	go func() { defer wg.Done(); dev = readNetDevFn() }()
	wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	// CPU: utilization from the /proc/stat aggregate-jiffy deltas.
	if cpuOK {
		m.available = true
		if m.haveCPU && cpuTotal > m.lastCPUTotal {
			dt := cpuTotal - m.lastCPUTotal
			di := cpuIdle - m.lastCPUIdle
			busy := float64(dt-di) / float64(dt) * 100
			m.cpu = appendTrim(m.cpu, metricPoint{T: now, V: clampPct(busy)}, cutoff)
		}
		m.lastCPUTotal, m.lastCPUIdle, m.haveCPU = cpuTotal, cpuIdle, true
	}

	// Memory: used percentage from MemTotal/MemAvailable.
	if memOK {
		m.available = true
		m.mem = appendTrim(m.mem, metricPoint{T: now, V: clampPct(memPct)}, cutoff)
	}

	// Disk: used percentage of the root filesystem (/ on Unix, C:\ on Windows).
	if diskOK {
		m.available = true
		m.disk = appendTrim(m.disk, metricPoint{T: now, V: clampPct(diskPct)}, cutoff)
	}

	// System uptime: latest value only, no history (see the struct field doc).
	if uptimeOK {
		m.available = true
		m.uptimeSecs, m.haveUptime = uptimeSecs, true
	}

	// Per-interface throughput (bytes/sec) for the live overlay interfaces.
	live := map[string]bool{}
	for _, ii := range m.be.Interfaces() {
		if ii.Iface == "" {
			continue
		}
		live[ii.Iface] = true
		st := m.ifaces[ii.Iface]
		if st == nil {
			st = &ifaceMetrics{Iface: ii.Iface}
			m.ifaces[ii.Iface] = st
		}
		st.Network = ii.Name
		c, ok := dev[ii.Iface]
		if !ok {
			continue
		}
		if st.have && now > st.lastT {
			secs := float64(now - st.lastT)
			st.Rx = appendTrim(st.Rx, metricPoint{T: now, V: rate(c.rx, st.lastRx, secs)}, cutoff)
			st.Tx = appendTrim(st.Tx, metricPoint{T: now, V: rate(c.tx, st.lastTx, secs)}, cutoff)
		}
		st.lastRx, st.lastTx, st.lastT, st.have = c.rx, c.tx, now, true
	}
	// Drop interfaces that are no longer present (network removed).
	for name := range m.ifaces {
		if !live[name] {
			delete(m.ifaces, name)
		}
	}
}

// snapshot returns the series within the last `minutes`, deep-copied for JSON.
func (m *metricsCollector) snapshot(minutes int) map[string]any {
	cutoff := time.Now().Unix() - int64(minutes)*60
	m.mu.Lock()
	defer m.mu.Unlock()
	ifs := make([]ifaceMetrics, 0, len(m.ifaces))
	for _, st := range m.ifaces {
		ifs = append(ifs, ifaceMetrics{
			Network: st.Network, Iface: st.Iface,
			Rx: sinceCutoff(st.Rx, cutoff), Tx: sinceCutoff(st.Tx, cutoff),
		})
	}
	// Stable order by interface name.
	for i := 1; i < len(ifs); i++ {
		for j := i; j > 0 && ifs[j].Iface < ifs[j-1].Iface; j-- {
			ifs[j], ifs[j-1] = ifs[j-1], ifs[j]
		}
	}
	out := map[string]any{
		"available":       m.available,
		"sample_interval": int(metricSampleInterval / time.Second),
		"cpu":             sinceCutoff(m.cpu, cutoff),
		"mem":             sinceCutoff(m.mem, cutoff),
		"disk":            sinceCutoff(m.disk, cutoff),
		"disk_path":       diskPathLabel(),
		"ifaces":          ifs,
	}
	// Omitted entirely (rather than sent as 0) when unavailable, so the
	// frontend can tell "just booted" apart from "this platform's reader
	// couldn't get a value" and hide the card instead of showing 0s.
	if m.haveUptime {
		out["uptime_seconds"] = m.uptimeSecs
	}
	return out
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	minutes := 60
	if v, err := strconv.Atoi(r.URL.Query().Get("minutes")); err == nil {
		minutes = v
	}
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 60 {
		minutes = 60
	}
	writeJSON(w, http.StatusOK, s.metrics.snapshot(minutes))
}

// ---- helpers ----------------------------------------------------------------

func appendTrim(s []metricPoint, p metricPoint, cutoff int64) []metricPoint {
	s = append(s, p)
	// Trim from the front once the oldest points fall outside retention.
	i := 0
	for i < len(s) && s[i].T < cutoff {
		i++
	}
	if i > 0 {
		s = append(s[:0:0], s[i:]...)
	}
	return s
}

func sinceCutoff(s []metricPoint, cutoff int64) []metricPoint {
	i := 0
	for i < len(s) && s[i].T < cutoff {
		i++
	}
	out := make([]metricPoint, len(s)-i)
	copy(out, s[i:])
	return out
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// diskPathLabel names the filesystem readDiskUsedPct measures, for display
// next to the Disk graph's title ("Disk (/)" or "Disk (C:)").
func diskPathLabel() string {
	if runtime.GOOS == "windows" {
		return "C:"
	}
	return "/"
}

// rate converts a monotonically-increasing counter delta into a per-second rate,
// guarding against counter resets/wraps (returns 0 if the counter went backwards).
func rate(cur, prev uint64, secs float64) float64 {
	if secs <= 0 || cur < prev {
		return 0
	}
	return float64(cur-prev) / secs
}

// devCounters is a per-interface cumulative byte counter snapshot, produced by
// each platform's readNetDev().
type devCounters struct{ rx, tx uint64 }

// Per-platform readers, same signatures on every OS:
//
//	readCPUTotals() (total, idle uint64, ok bool)
//	readMemUsedPct() (float64, bool)
//	readDiskUsedPct() (float64, bool)
//	readNetDev() map[string]devCounters
//
// See metrics_linux.go, metrics_darwin.go, metrics_windows.go.
