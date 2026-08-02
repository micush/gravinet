package webadmin

// Passive latency history for the Monitor -> Latency page's "click a trend
// for a bigger, longer chart" view.
//
// Deliberately does NOT poll by forking ping(1) the way the page's own live,
// on-demand check (handleLocalLatency) does. That's fine run only while a
// human has the page open — a couple of ICMP probes per peer, run when
// asked — but turning that into a permanent background loop (the same shape
// metricsCollector already uses for CPU/mem/disk/interface throughput) would
// mean a subprocess fork and a real network round trip per peer, on a timer,
// forever, whether or not anyone is watching. At a hundred peers that's a
// real, continuous cost, not a free background sample the way reading a
// local counter is.
//
// Instead this samples mesh.PeerInfo.RTTMs — round-trip time the mesh engine
// already measures continuously via its own ctrlPing/ctrlPong keepalive, as
// a byproduct of traffic that happens anyway regardless of this collector's
// existence (see RTTMs's own doc comment in internal/mesh/ban.go). Recording
// an already-computed in-memory value on a timer costs nothing that scales
// with peer count. The tradeoff, stated plainly rather than glossed over: this
// is a different signal than the live page's ICMP-over-overlay ping, not the
// same measurement sampled more often — the mesh's own control-plane round
// trip over a peer's actual current session (direct or relayed), rather than
// ICMP through the TUN device. In practice the two should track closely,
// since both reflect the real path a peer's traffic takes, but they are not
// interchangeable.
//
// Checkpointed to disk (latency-history.json, next to the config file — same
// convention as webadmin-cert.pem/webadmin-session.key, see
// latencyHistoryPath/selfSignedPaths) so a restart doesn't reset the
// expanded chart back to nothing: loaded once at construction, saved
// synchronously in close() on a clean shutdown, and re-checkpointed every
// latencyCheckpointInterval in between so an unclean stop (a crash, a
// kill -9, a supervisor restart that never runs the shutdown path) loses at
// most one checkpoint's worth of samples, not the whole session. A missing
// or unreadable file is never fatal — this is a convenience cache of an
// already-reconstructible signal (the mesh keeps measuring RTT regardless),
// not a source of truth anything else depends on, so any failure to load or
// save it just logs and falls back to the collector's original
// always-starts-empty behavior.
import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"gravinet/internal/logx"
)

const (
	latencySampleInterval = 10 * time.Second
	// latencyRetention bounds how far back the Latency page's expanded chart
	// can go. 10s sampling over 24h is ~8,640 points per peer (~140KB) —
	// light enough per peer that even a few hundred peers stays a modest,
	// bounded in-memory cost, not something that needed its own eviction
	// policy beyond appendTrim's existing per-sample trim.
	latencyRetention = 24 * time.Hour
	// latencyCheckpointInterval bounds how much history a crash (as opposed
	// to a clean shutdown, which close() saves synchronously) can cost: at
	// most one checkpoint's worth, not the full session since the process
	// started. 5 minutes at a ~140KB-per-peer ceiling is a small, regular
	// write — the same tradeoff maybePersistPeers (internal/mesh) already
	// makes for a different piece of restart-sensitive state.
	latencyCheckpointInterval = 5 * time.Minute
)

// latencyPeerHistory is one peer's RTT history within one network.
type latencyPeerHistory struct {
	Hostname string        `json:"hostname"`
	Overlay  string        `json:"overlay"`
	Hist     []metricPoint `json:"hist"`
}

// latencyCollector samples every connected peer's RTTMs on a fixed interval
// and keeps a rolling latencyRetention-long history per (network, peer) —
// same shape as metricsCollector, see this file's own doc comment for why
// the two collectors read from very differently-costed sources.
type latencyCollector struct {
	be  Backend
	log *logx.Logger // may be nil in tests; always nil-checked before use
	// path is where history is checkpointed and restored from — empty
	// disables persistence entirely (e.g. an embedding or test context with
	// no configPath to anchor it to; see latencyHistoryPath), in which case
	// this collector behaves exactly as it did before persistence existed.
	path string

	mu   sync.Mutex
	nets map[string]map[string]*latencyPeerHistory // network name -> node id -> history
	stop chan struct{}
}

// latencyHistoryPath returns where the latency history checkpoint lives —
// next to the config file, the same convention selfSignedPaths and
// sessionKeyPath (webadmin.go) already use for their own restart-durable
// files. An empty configPath (some embedding/test contexts genuinely have
// none) means persistence is simply unavailable, not an error.
func latencyHistoryPath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), "latency-history.json")
}

func newLatencyCollector(be Backend, log *logx.Logger, path string) *latencyCollector {
	l := &latencyCollector{be: be, log: log, path: path, nets: map[string]map[string]*latencyPeerHistory{}, stop: make(chan struct{})}
	if path != "" {
		l.load()
	}
	return l
}

func (l *latencyCollector) run() {
	l.sample() // populate immediately rather than waiting a full interval
	t := time.NewTicker(latencySampleInterval)
	defer t.Stop()
	// A nil channel is never selectable, so leaving ckpt nil when
	// persistence is off (l.path == "") disables the checkpoint case below
	// without a second branch in the loop.
	var ckpt <-chan time.Time
	if l.path != "" {
		ct := time.NewTicker(latencyCheckpointInterval)
		defer ct.Stop()
		ckpt = ct.C
	}
	for {
		select {
		case <-t.C:
			l.sample()
		case <-ckpt:
			if err := l.save(); err != nil && l.log != nil {
				l.log.Warnf("webadmin: could not checkpoint latency history to %s: %v", l.path, err)
			}
		case <-l.stop:
			return
		}
	}
}

// close stops the sampling loop and, if persistence is enabled, saves the
// final state synchronously before returning — the clean-shutdown half of
// this file's own doc comment on checkpointing. Does not wait for run()'s
// goroutine to actually exit (matches bgpRedis/autoBGP's close() in this
// package, neither of which blocks on their own goroutine either); save()
// takes l.mu itself, so it's safe regardless of whether a last sample() is
// still in flight.
func (l *latencyCollector) close() {
	close(l.stop)
	if l.path == "" {
		return
	}
	if err := l.save(); err != nil && l.log != nil {
		l.log.Warnf("webadmin: could not save latency history to %s: %v", l.path, err)
	}
}

// sample reads every connected peer's current RTTMs across every network —
// ListPeers/Interfaces are both already-maintained in-memory state, not I/O
// — and appends one point per peer that has completed at least one keepalive
// round trip. A peer that's down simply doesn't appear in ListPeers at all,
// so its history just stops gaining new points for as long as it's gone —
// the gap itself is the "miss" signal, rather than a placeholder value.
func (l *latencyCollector) sample() {
	now := time.Now().Unix()
	cutoff := now - int64(latencyRetention/time.Second)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ifc := range l.be.Interfaces() {
		peers, ok := l.nets[ifc.Name]
		if !ok {
			peers = map[string]*latencyPeerHistory{}
			l.nets[ifc.Name] = peers
		}
		for _, p := range l.be.ListPeers(ifc.NetworkID) {
			if p.RTTMs <= 0 {
				continue // no keepalive round trip completed yet
			}
			addr := p.Overlay4
			if addr == "" {
				addr = p.Overlay6
			}
			ph, ok := peers[p.NodeID]
			if !ok {
				ph = &latencyPeerHistory{}
				peers[p.NodeID] = ph
			}
			ph.Hostname = p.Hostname
			ph.Overlay = addr
			ph.Hist = appendTrim(ph.Hist, metricPoint{T: now, V: p.RTTMs}, cutoff)
		}
	}
}

// save writes the collector's current history to l.path as JSON, via the
// same atomic temp-file-then-rename write every other piece of durable
// state in this package already uses (writeAtomicFile, frr.go) — a partial
// write here would corrupt the one copy a restart depends on, at exactly
// the moment (shutdown, or mid-checkpoint) when there's no later tick to
// self-heal it. l.path == "" is the caller's responsibility to check first
// (both call sites already do); called unconditionally here would just be
// a confusing error from writeAtomicFile("", ...) instead.
func (l *latencyCollector) save() error {
	l.mu.Lock()
	body, err := json.Marshal(l.nets)
	l.mu.Unlock()
	if err != nil {
		return fmt.Errorf("marshal latency history: %w", err)
	}
	return writeAtomicFile(l.path, string(body))
}

// load restores previously-checkpointed history from l.path, called once
// from newLatencyCollector before run() starts (so no locking is needed
// here — nothing else can be touching l.nets yet). Points already past
// latencyRetention are dropped immediately, the same trim sample() applies
// on every write, so a daemon that was down for longer than the retention
// window doesn't resurrect data old enough that normal operation would
// already have aged it out; a peer or network left with zero points after
// that trim is dropped entirely rather than kept around as an empty,
// never-cleaned-up entry. Missing file is not logged at all (first run, or
// an upgrade from a version before this existed — an expected, silent
// case, not a problem); a present-but-corrupt or unreadable one is logged
// and treated the same way: start empty, exactly as this collector always
// did before persistence existed.
func (l *latencyCollector) load() {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if !os.IsNotExist(err) && l.log != nil {
			l.log.Warnf("webadmin: could not read latency history from %s: %v (starting empty)", l.path, err)
		}
		return
	}
	var nets map[string]map[string]*latencyPeerHistory
	if err := json.Unmarshal(data, &nets); err != nil {
		if l.log != nil {
			l.log.Warnf("webadmin: could not parse latency history at %s: %v (starting empty)", l.path, err)
		}
		return
	}
	cutoff := time.Now().Unix() - int64(latencyRetention/time.Second)
	for netName, peers := range nets {
		for nodeID, ph := range peers {
			ph.Hist = sinceCutoff(ph.Hist, cutoff)
			if len(ph.Hist) == 0 {
				delete(peers, nodeID)
			}
		}
		if len(peers) == 0 {
			delete(nets, netName)
		}
	}
	l.nets = nets
}

// snapshot returns every network's per-peer history within the last minutes
// minutes — same windowing convention as metricsCollector.snapshot.
func (l *latencyCollector) snapshot(minutes int) map[string]any {
	cutoff := time.Now().Unix() - int64(minutes)*60
	l.mu.Lock()
	defer l.mu.Unlock()
	nets := make(map[string]any, len(l.nets))
	for netName, peers := range l.nets {
		out := make(map[string]any, len(peers))
		for nodeID, ph := range peers {
			out[nodeID] = map[string]any{
				"hostname": ph.Hostname,
				"overlay":  ph.Overlay,
				"hist":     sinceCutoff(ph.Hist, cutoff),
			}
		}
		nets[netName] = out
	}
	return map[string]any{"sample_interval": int(latencySampleInterval / time.Second), "networks": nets}
}

// handleLatencyHistory serves the collector's rolling history for the
// Latency page's click-to-expand chart. Separate from handleLocalLatency
// (the live, on-demand ICMP check that page also polls) — see this file's
// own doc comment for why they deliberately read from different sources.
func (s *Server) handleLatencyHistory(w http.ResponseWriter, r *http.Request) {
	if s.latencyHist == nil {
		writeJSON(w, http.StatusOK, map[string]any{"networks": map[string]any{}})
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
	writeJSON(w, http.StatusOK, s.latencyHist.snapshot(minutes))
}
