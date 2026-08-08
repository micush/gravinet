package webadmin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// Sampling cadence and retention for the Info -> Metrics graphs. 2s sampling
// over 24h is ~43,200 points per series — per host-level series (cpu/mem/
// disk) plus two per live interface (rx/tx), not per-peer the way the
// Latency page's history is, so even a mesh with several dozen networks
// stays a modest, bounded in-memory cost.
const (
	metricSampleInterval = 2 * time.Second
	metricRetention      = 24 * time.Hour
	// metricCheckpointInterval bounds how much history an unclean stop can
	// cost — the same tradeoff latencyCheckpointInterval already makes, and
	// the same 5 minutes, for the same reason: a clean shutdown saves
	// synchronously in close(), so this only covers a crash or a kill -9.
	//
	// The write is larger than latency's. 2s sampling over 24h is ~43,200
	// points per series, and there are three host series plus two per live
	// interface, so a single-network host checkpoints a few megabytes. That is
	// a bigger periodic write than latency-history.json but the same shape of
	// cost, and the alternative — a shorter retention on disk than in memory —
	// would mean the graph silently shortens across a restart, which is the
	// thing this exists to stop.
	metricCheckpointInterval = 5 * time.Minute
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
	// path is where history is checkpointed and restored from — empty
	// disables persistence entirely (an embedding or test context with no
	// configPath to anchor it to; see metricsHistoryPath), in which case this
	// collector behaves exactly as it did before persistence existed.
	path string

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

func newMetricsCollector(be Backend, log *logx.Logger, path string) *metricsCollector {
	m := &metricsCollector{be: be, log: log, path: path, ifaces: map[string]*ifaceMetrics{}, stop: make(chan struct{})}
	if path != "" {
		m.load()
	}
	return m
}

// metricsHistoryPath returns where the metrics checkpoint lives — next to the
// config file, the same convention latencyHistoryPath and selfSignedPaths
// already use for their own restart-durable files. An empty configPath (some
// embedding/test contexts genuinely have none) means persistence is simply
// unavailable, not an error.
func metricsHistoryPath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), "metrics-history.json")
}

// metricsSnapshot is the on-disk shape: the four rolling series and nothing
// else. The collector's sampler state (CPU totals, per-interface byte counters
// and their timestamps) is deliberately excluded — those are deltas against a
// reading taken moments ago by *this* process, and carrying them across a
// restart would compute a rate spanning the downtime, i.e. one enormous
// fabricated spike at the exact moment the graph resumes. Leaving them zero
// makes the first tick after a restart prime the deltas and emit no rate point,
// which is what a fresh start already does.
//
// uptimeSecs is excluded for a simpler reason: it is a current value, not a
// history, and a stale one restored from disk would be wrong the instant it
// loaded.
type metricsSnapshot struct {
	CPU    []metricPoint            `json:"cpu"`
	Mem    []metricPoint            `json:"mem"`
	Disk   []metricPoint            `json:"disk"`
	Ifaces map[string]*ifaceMetrics `json:"ifaces"`
}

// save writes the collector's current history to m.path as JSON, via the same
// atomic temp-file-then-rename write every other piece of durable state in this
// package uses (writeAtomicFile, frr.go) — a partial write would corrupt the
// one copy a restart depends on, at exactly the moment (shutdown, or
// mid-checkpoint) when there is no later tick to self-heal it. m.path == "" is
// the caller's responsibility to check first; both call sites do.
func (m *metricsCollector) save() error {
	m.mu.Lock()
	snap := metricsSnapshot{CPU: m.cpu, Mem: m.mem, Disk: m.disk, Ifaces: m.ifaces}
	body, err := json.Marshal(snap)
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("marshal metrics history: %w", err)
	}
	return writeAtomicFile(m.path, string(body))
}

// load restores previously-checkpointed history, called once from
// newMetricsCollector before run() starts, so no locking is needed — nothing
// else can be touching these fields yet. Points already past metricRetention
// are dropped immediately, the same trim sample() applies on every write, so a
// daemon that was down longer than the retention window does not resurrect data
// normal operation would already have aged out; an interface left with no
// points after that trim is dropped rather than kept as an empty entry that
// nothing ever cleans up.
//
// A missing file is not logged (first run, or an upgrade from before this
// existed — expected and silent); a corrupt or unreadable one is logged and
// treated identically: start empty, exactly as this collector did before
// persistence existed. This is a convenience cache of a signal the host
// regenerates on its own, not a source of truth anything depends on.
func (m *metricsCollector) load() {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if !os.IsNotExist(err) && m.log != nil {
			m.log.Warnf("webadmin: could not read metrics history from %s: %v (starting empty)", m.path, err)
		}
		return
	}
	var snap metricsSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		if m.log != nil {
			m.log.Warnf("webadmin: could not parse metrics history at %s: %v (starting empty)", m.path, err)
		}
		return
	}
	cutoff := time.Now().Unix() - int64(metricRetention/time.Second)
	m.cpu = trimPoints(snap.CPU, cutoff)
	m.mem = trimPoints(snap.Mem, cutoff)
	m.disk = trimPoints(snap.Disk, cutoff)
	for name, ifm := range snap.Ifaces {
		if ifm == nil {
			continue
		}
		ifm.Rx = trimPoints(ifm.Rx, cutoff)
		ifm.Tx = trimPoints(ifm.Tx, cutoff)
		if len(ifm.Rx) == 0 && len(ifm.Tx) == 0 {
			continue
		}
		// have/lastRx/lastTx/lastT stay at their zero values: see
		// metricsSnapshot on why the deltas must not survive a restart.
		m.ifaces[name] = ifm
	}
}

// trimPoints drops points older than cutoff. appendTrim does the same thing on
// the append path; this is the load-time equivalent with nothing to append.
func trimPoints(s []metricPoint, cutoff int64) []metricPoint {
	i := 0
	for i < len(s) && s[i].T < cutoff {
		i++
	}
	if i == 0 {
		return s
	}
	return append(s[:0:0], s[i:]...)
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
	// A nil channel is never selectable, so leaving ckpt nil when persistence
	// is off (m.path == "") disables the checkpoint case below without needing
	// a second branch in the loop — same shape as latencyCollector.run.
	var ckpt <-chan time.Time
	if m.path != "" {
		ct := time.NewTicker(metricCheckpointInterval)
		defer ct.Stop()
		ckpt = ct.C
	}
	for {
		select {
		case <-m.stop:
			return
		case <-ckpt:
			if err := m.save(); err != nil && m.log != nil {
				m.log.Warnf("webadmin: could not checkpoint metrics history to %s: %v", m.path, err)
			}
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

// close stops the sampling loop and, if persistence is enabled, saves the final
// state synchronously before returning — the clean-shutdown half of the
// checkpointing described on metricCheckpointInterval. Does not wait for run()'s
// goroutine to exit (matching latencyCollector.close and bgpRedis/autoBGP);
// save() takes m.mu itself, so it is safe whether or not a last sample() is
// still in flight.
func (m *metricsCollector) close() {
	close(m.stop)
	if m.path == "" {
		return
	}
	if err := m.save(); err != nil && m.log != nil {
		m.log.Warnf("webadmin: could not save metrics history to %s: %v", m.path, err)
	}
}

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
	if minutes > 1440 {
		minutes = 1440
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

// IfaceThroughput is one overlay interface's instantaneous send/receive
// rate, part of HostSnapshot.
type IfaceThroughput struct {
	Network       string
	Iface         string
	RxBytesPerSec float64
	TxBytesPerSec float64
}

// HostSnapshot is a single, instantaneous read of this host's CPU/memory/
// disk/uptime and per-overlay-interface throughput — see TakeHostSnapshot.
type HostSnapshot struct {
	CPUPercent    float64
	CPUOK         bool
	MemPercent    float64
	MemOK         bool
	DiskPercent   float64
	DiskOK        bool
	UptimeSeconds uint64
	UptimeOK      bool
	Ifaces        []IfaceThroughput
}

// TakeHostSnapshot reads CPU/memory/disk/uptime and per-interface
// throughput once, right now — the same readers metricsCollector.sample()
// uses for its rolling history (Info → Metrics' graphs), just a single
// instantaneous read instead of an accumulated series. Exported for
// "gravinet monitor metrics" (cmd/gravinet), which — running as a separate,
// short-lived process — has no access to the daemon's own history buffer
// and settles for "right now" instead of a graph.
//
// CPU and interface throughput are both rates, not levels — a single
// cumulative jiffy or byte counter doesn't mean anything on its own, it
// needs a delta — so this samples both twice, one second apart, the same
// one-second window sample() itself uses between ticks. Memory, disk, and
// uptime are already instantaneous values and only need one read. Blocks
// for about a second; that's expected; ifaces is the live network ->
// kernel-interface mapping to report throughput for (see mesh.IfaceInfo /
// the control socket's "ifaces" command) since this function has no engine
// of its own to ask.
func TakeHostSnapshot(ifaces []mesh.IfaceInfo) HostSnapshot {
	var s HostSnapshot
	totalA, idleA, cpuOKA := readCPUTotalsFn()
	devA := readNetDevFn()
	time.Sleep(time.Second)
	totalB, idleB, cpuOKB := readCPUTotalsFn()
	devB := readNetDevFn()

	if cpuOKA && cpuOKB && totalB > totalA {
		dt := totalB - totalA
		di := idleB - idleA
		s.CPUPercent = clampPct(float64(dt-di) / float64(dt) * 100)
		s.CPUOK = true
	}
	s.MemPercent, s.MemOK = readMemUsedPctFn()
	s.DiskPercent, s.DiskOK = readDiskUsedPctFn()
	s.UptimeSeconds, s.UptimeOK = readUptimeFn()

	for _, ii := range ifaces {
		if ii.Iface == "" {
			continue
		}
		a, okA := devA[ii.Iface]
		b, okB := devB[ii.Iface]
		if !okA || !okB {
			continue
		}
		s.Ifaces = append(s.Ifaces, IfaceThroughput{
			Network:       ii.Name,
			Iface:         ii.Iface,
			RxBytesPerSec: rate(b.rx, a.rx, 1),
			TxBytesPerSec: rate(b.tx, a.tx, 1),
		})
	}
	return s
}
