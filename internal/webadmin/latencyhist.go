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
import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"gravinet/internal/logx"
)

const (
	latencySampleInterval = 10 * time.Second
	latencyRetention      = 4 * time.Hour
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

	mu    sync.Mutex
	nets  map[string]map[string]*latencyPeerHistory // network name -> node id -> history
	stop  chan struct{}
}

func newLatencyCollector(be Backend, log *logx.Logger) *latencyCollector {
	return &latencyCollector{be: be, log: log, nets: map[string]map[string]*latencyPeerHistory{}, stop: make(chan struct{})}
}

func (l *latencyCollector) run() {
	l.sample() // populate immediately rather than waiting a full interval
	t := time.NewTicker(latencySampleInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.sample()
		case <-l.stop:
			return
		}
	}
}

func (l *latencyCollector) close() { close(l.stop) }

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
	if minutes > 240 {
		minutes = 240
	}
	writeJSON(w, http.StatusOK, s.latencyHist.snapshot(minutes))
}
