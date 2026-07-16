package webadmin

import (
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gravinet/internal/logx"
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
	be  Backend
	log *logx.Logger // may be nil in tests; always nil-checked before use

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

	// Last successfully-computed percentage for cpu/mem/disk, carried
	// forward (re-appended at the current tick's timestamp) when a reader
	// has a transient failure after previously succeeding — see sample()'s
	// comment on why a stalled graph is worse than a flat-but-current one.
	// Deliberately not done for per-interface rx/tx: a "carried forward"
	// rate is a much less honest stand-in for a byte counter than a
	// carried-forward percentage is for CPU/mem/disk, so a netstat/readNetDev
	// hiccup still just skips that tick for interface throughput.
	lastCPUPct, lastMemPct, lastDiskPct float64
	haveCPUPct, haveMemPct, haveDiskPct bool
	cpuFailing, memFailing, diskFailing bool
	uptimeFailing                       bool

	available bool        // whether /proc metrics could be read at all
	sampling  atomic.Bool // true while a sample() launched from run() is in flight
	stop      chan struct{}
}

func newMetricsCollector(be Backend, log *logx.Logger) *metricsCollector {
	return &metricsCollector{be: be, log: log, ifaces: map[string]*ifaceMetrics{}, stop: make(chan struct{})}
}

// noteReaderHealth logs (Warnf, then Infof on recovery) only on the
// true/false transition of a reader's ok result, not on every tick — so a
// reader that's simply unsupported on this platform (always ok=false) never
// logs at all, matching the existing "report unavailable, don't error"
// design, while one that starts failing after a run of successes — exactly
// what carrying a value forward would otherwise mask silently — shows up in
// the daemon's own log with nothing more than a restart and a tail needed to
// see it.
func (m *metricsCollector) noteReaderHealth(name string, ok bool, failing *bool) {
	if !ok && !*failing {
		*failing = true
		if m.log != nil {
			m.log.Warnf("webadmin: %s metrics reader failed after previously succeeding — "+
				"that graph will hold its last known value until it recovers", name)
		}
	} else if ok && *failing {
		*failing = false
		if m.log != nil {
			m.log.Infof("webadmin: %s metrics reader recovered", name)
		}
	}
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
			// sample() can block for ~1s on macOS (top -l 1's startup
			// delay). Running it inline here would stretch the effective
			// cadence to interval+readtime (~3s), collecting fewer points
			// than the window expects and leaving the newest one further
			// behind "now". Run it in its own goroutine so the ticker keeps
			// firing on schedule; the sampling atomic guard skips a tick
			// only in the pathological case where a prior sample is somehow
			// still running a full interval later (so slow samples drop a
			// tick rather than queueing up unboundedly).
			if m.sampling.CompareAndSwap(false, true) {
				go func() {
					defer m.sampling.Store(false)
					m.sample()
				}()
			}
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
	// Timestamp is captured AFTER the readers return (below), not here: on
	// macOS the CPU reader (top -l 1) blocks ~1s before it emits anything —
	// top collects an initial reference frame before its first sample — and
	// the other readers shell out too. Stamping `now` before that wait, as
	// this used to, meant every point landed with a timestamp from ~1s+
	// before it was actually collected. snapshot()'s server_now, by
	// contrast, is read fresh when the HTTP request arrives, i.e. current —
	// so the newest point sat a fixed ~1s+ behind the chart's right edge on
	// every series at once (they all share this timestamp). That's the gap
	// in the macOS screenshots: not a slow or flaky reader, just points
	// dated earlier than the moment they represent. Capturing the time when
	// the readers finish closes it. cutoff (retention trim) is likewise
	// computed from that same post-read `now`.
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

	now := time.Now().Unix()
	cutoff := now - int64(metricRetention/time.Second)

	m.mu.Lock()
	defer m.mu.Unlock()

	// CPU: utilization from the /proc/stat aggregate-jiffy deltas.
	if cpuOK {
		m.available = true
		if m.haveCPU && cpuTotal > m.lastCPUTotal {
			dt := cpuTotal - m.lastCPUTotal
			di := cpuIdle - m.lastCPUIdle
			busy := clampPct(float64(dt-di) / float64(dt) * 100)
			m.cpu = appendTrim(m.cpu, metricPoint{T: now, V: busy}, cutoff)
			m.lastCPUPct, m.haveCPUPct = busy, true
		}
		m.lastCPUTotal, m.lastCPUIdle, m.haveCPU = cpuTotal, cpuIdle, true
	} else if m.haveCPUPct {
		// The reader failed this tick after previously succeeding: keep the
		// line moving through "now" on its last known value rather than
		// stalling — see this function's doc comment for why a graph that
		// silently stops advancing is worse than one that's briefly flat.
		m.cpu = appendTrim(m.cpu, metricPoint{T: now, V: m.lastCPUPct}, cutoff)
	}
	m.noteReaderHealth("CPU", cpuOK, &m.cpuFailing)

	// Memory: used percentage from MemTotal/MemAvailable.
	if memOK {
		m.available = true
		v := clampPct(memPct)
		m.mem = appendTrim(m.mem, metricPoint{T: now, V: v}, cutoff)
		m.lastMemPct, m.haveMemPct = v, true
	} else if m.haveMemPct {
		m.mem = appendTrim(m.mem, metricPoint{T: now, V: m.lastMemPct}, cutoff)
	}
	m.noteReaderHealth("memory", memOK, &m.memFailing)

	// Disk: used percentage of the root filesystem (/ on Unix, C:\ on Windows).
	if diskOK {
		m.available = true
		v := clampPct(diskPct)
		m.disk = appendTrim(m.disk, metricPoint{T: now, V: v}, cutoff)
		m.lastDiskPct, m.haveDiskPct = v, true
	} else if m.haveDiskPct {
		m.disk = appendTrim(m.disk, metricPoint{T: now, V: m.lastDiskPct}, cutoff)
	}
	m.noteReaderHealth("disk", diskOK, &m.diskFailing)

	// System uptime: latest value only, no history (see the struct field doc).
	// Nothing meaningful to carry forward here — it just stops advancing
	// until the reader recovers, same as before.
	if uptimeOK {
		m.available = true
		m.uptimeSecs, m.haveUptime = uptimeSecs, true
	}
	m.noteReaderHealth("uptime", uptimeOK, &m.uptimeFailing)

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
	nowUnix := time.Now().Unix()
	cutoff := nowUnix - int64(minutes)*60
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
		"server_now":      nowUnix,
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
