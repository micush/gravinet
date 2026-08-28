package webadmin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mesh-wide packet capture is the fan-out sibling of the single-node capture
// in capture.go: instead of one interface on one node, it starts a capture on
// the mesh interface of every reachable managed peer (plus this node) at
// once, holds it open for a fixed window, then bundles each peer's .pcap into
// one .tgz. It exists specifically for the "does node A actually see what
// node B sent" class of problem that a single-node capture can't answer on
// its own — v767's changelog entry called out exactly this gap: chasing an
// IPv6 asymmetry required a paired capture on both ends of one ping, done by
// hand by visiting each node's Capture tab separately and hoping the timing
// lined up.
//
// Design choices, and why:
//
//   - Duration is a manager-side deadline (job.startedAt + duration), not a
//     fixed sleep per peer. Each peer's goroutine sleeps *until that
//     deadline*, not for a fixed span starting whenever its own start call
//     happened to land — so a peer with a slow/laggy overlay hop still stops
//     at roughly the same wall-clock instant as a fast one, keeping the
//     windows comparable across peers instead of independently drifting.
//
//   - This node's own overlay interface name is read straight from
//     s.be.Interfaces() (authoritative — the mesh engine's own record of
//     which kernel device backs which network), and a peer's name is asked
//     of the peer itself via /api/capture/mesh-iface, for the same reason:
//     the device name isn't guessable from a convention. It's "mesh0"-ish on
//     Linux, "utunN" on macOS, a driver-assigned adapter name on Windows —
//     and can vary node to node even on the same OS depending on what else
//     is using tun devices there.
//
//   - Only the *first* network's interface is captured per node. Multi-network
//     nodes exist (Config.Networks), but capture.go's captureState is a
//     single-active-capture-per-node design already (see its own doc
//     comment) — this fan-out reuses that same single slot on every node it
//     touches rather than introducing per-network concurrency this codebase
//     doesn't have anywhere else yet. Good enough for the overwhelmingly
//     common single-network deployment; a node with several networks gets
//     whichever one s.be.Interfaces() lists first (sorted by NetworkID, so
//     at least it's stable run to run).
//
//   - A job reuses the same captureState (capture.go) on every node it
//     touches, itself included — so starting a mesh-wide capture also ends
//     whatever single-node capture an operator happened to have running or
//     displayed on any touched node's own Capture tab (including this one).
//     That's the same "one active capture per node" rule the single-capture
//     UI already lives with; this just makes it apply on N nodes in one
//     click instead of one.
//
//   - One peer's failure (unreachable, capture unsupported on its platform,
//     etc.) doesn't fail the job — it's recorded per-peer and everyone else's
//     capture still runs and still ends up in the .tgz. A summary of any
//     failures rides along as errors.txt inside the archive, since the
//     browser only sees the bundle once, not a running log.
//
//   - Like captureState, only one mesh-wide job exists at a time per Server;
//     starting a new one simply replaces the manager's pointer to the
//     previous (now-unreachable-from-the-UI) job. The previous job's
//     in-flight peer goroutines still run to completion in the background
//     (bounded by their own duration) rather than being cancelled, since
//     they're touching *other nodes'* capture state and an abrupt local
//     cancel wouldn't stop those anyway.

// meshCaptureDurations are the only capture windows the UI offers. Kept short
// and enumerated (not a free-form number) because every peer this touches
// pauses its single capture slot for the duration — a long or accidental
// capture on a whole fleet is a bigger footgun than the single-node version,
// which at least only affects the one node the operator is looking at.
var meshCaptureDurations = map[int]bool{5: true, 10: true, 30: true, 60: true}

// meshCapturePeerResult is one peer's outcome within a meshCaptureJob.
type meshCapturePeerResult struct {
	NodeID   string `json:"node_id,omitempty"`
	Hostname string `json:"hostname"`
	Self     bool   `json:"self,omitempty"`
	Iface    string `json:"iface,omitempty"`
	Status   string `json:"status"` // "running", "done", "error"
	Error    string `json:"error,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`

	pcap []byte // unexported: raw capture, held only until bundle() tars it and never sent to the browser directly
}

// meshCaptureJob is one fan-out run. See the package-level doc comment above
// for the design; mu guards every field below it since peer goroutines and
// the status/download HTTP handlers all touch this concurrently.
type meshCaptureJob struct {
	id        int64
	startedAt time.Time
	duration  time.Duration

	mu     sync.Mutex
	peers  []*meshCapturePeerResult
	done   bool
	failed string // job-level failure — no reachable peers, or every peer errored
	tgz    []byte
}

// meshCaptureManager owns the single active (or most recently finished) job.
type meshCaptureManager struct {
	mu  sync.Mutex
	seq int64
	job *meshCaptureJob
}

func newMeshCaptureManager() *meshCaptureManager { return &meshCaptureManager{} }

func (m *meshCaptureManager) start(s *Server, duration time.Duration) *meshCaptureJob {
	m.mu.Lock()
	m.seq++
	job := &meshCaptureJob{id: m.seq, startedAt: time.Now(), duration: duration}
	m.job = job
	m.mu.Unlock()
	go job.run(s)
	return job
}

func (m *meshCaptureManager) current() *meshCaptureJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.job
}

// run discovers targets, fans out one goroutine per peer (this node included),
// waits for all of them, and bundles the results. It never returns an error
// itself — every failure mode (no peers, every peer erroring, one peer
// erroring) is recorded on the job for the status/download handlers to report.
func (j *meshCaptureJob) run(s *Server) {
	deadline := j.startedAt.Add(j.duration)

	type target struct {
		nodeID, hostname string
		self             bool
	}
	targets := []target{{hostname: s.be.Hostname(), self: true}}
	for _, p := range s.be.ManagedPeers(managedPeerTTL) {
		ip := p.Overlay4
		if !ip.IsValid() {
			ip = p.Overlay6
		}
		// Same reachability test handleCluster uses for clusterPeer.Manageable:
		// a gossip-only address with no live session (and not a seed, which is
		// always dial-attempted) is one this node structurally can't reach.
		if !(ip.IsValid() && p.WebPort != 0 && (p.Connected || p.IsSeed)) {
			continue
		}
		targets = append(targets, target{nodeID: p.NodeID, hostname: p.Hostname})
	}

	j.mu.Lock()
	for _, t := range targets {
		j.peers = append(j.peers, &meshCapturePeerResult{
			NodeID: t.nodeID, Hostname: t.hostname, Self: t.self, Status: "running",
		})
	}
	peers := j.peers
	j.mu.Unlock()

	if len(targets) == 0 {
		j.mu.Lock()
		j.failed = "no reachable managed peers, and no local capture to fall back to"
		j.done = true
		j.mu.Unlock()
		return
	}

	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(res *meshCapturePeerResult, t target) {
			defer wg.Done()
			setIface := func(ifc string) {
				j.mu.Lock()
				res.Iface = ifc
				j.mu.Unlock()
			}
			data, iface, err := captureOnePeer(s, t.nodeID, t.self, deadline, setIface)
			j.mu.Lock()
			res.Iface = iface
			if err != nil {
				res.Status = "error"
				res.Error = err.Error()
			} else {
				res.Status = "done"
				res.Bytes = len(data)
				res.pcap = data
			}
			j.mu.Unlock()
		}(peers[i], t)
	}
	wg.Wait()

	j.bundle()
}

// captureOnePeer runs one leg: discover the node's real overlay interface,
// start capturing on it, sleep until the shared deadline, stop, and pull back
// the resulting pcap bytes. self is handled with direct in-process calls
// (its own captureState, deliberately not the Capture tab's — see below); a
// remote peer is handled with the same overlay dial handleProxy uses, just
// made directly from this goroutine instead of round-tripping through the
// browser — which does still go through that peer's own /api/capture/start,
// and so does still disturb its Capture tab. Only this node is spared.
//
// setIface is called the moment the interface name is known — which is
// within the first round trip, well before the (multi-second, up to a
// minute) sleep to the shared deadline — so the status poll can show it
// immediately instead of only once this function returns at the very end.
// The final named return value below is still authoritative (setIface is
// purely a progress notification, not the source of truth the caller keeps);
// they're the same value once discovery succeeds, and setIface is simply
// never called if discovery itself fails.
func captureOnePeer(s *Server, nodeID string, self bool, deadline time.Time, setIface func(string)) (data []byte, iface string, err error) {
	if self {
		ifaces := s.be.Interfaces()
		if len(ifaces) == 0 || ifaces[0].Iface == "" {
			return nil, "", fmt.Errorf("no overlay interface configured on this node")
		}
		iface = ifaces[0].Iface
		setIface(iface)
		ifi, err := net.InterfaceByName(iface)
		if err != nil {
			return nil, iface, fmt.Errorf("interface %s: %v", iface, err)
		}
		// A private captureState, not s.capture.
		//
		// s.capture is the one the Capture tab is bound to, and this used it
		// for convenience — the same in-process path handleCaptureStart uses.
		// The convenience was not free. begin() resets the buffer, points
		// iface at the overlay device and sets running, so a mesh-wide
		// capture reached into the operator's own capture tab and took it
		// over: whatever they had running was killed, whatever they had
		// captured was discarded, and the tab was left sitting on a mesh0
		// capture nobody started there. What the fan-out needs from that
		// state is a buffer and a pcap writer, neither of which has any
		// reason to be the shared one.
		//
		// So there can now be two captures open on this host at once, where
		// begin() previously guaranteed one: the operator's, and this. Two
		// handles and two buffers, each independently bounded by capMaxBytes,
		// which is the price of not stealing the first one.
		cs := newCaptureState()
		ep, _ := cs.begin(ifi.Name, linktypeForIface(ifi))
		h, lt, err := startCapture(ifi.Name, capSnaplen, func(t time.Time, d []byte) {
			cs.addEpoch(ep, t, d)
		})
		if err != nil {
			cs.failStart(ep)
			return nil, iface, err
		}
		cs.setLinktype(ep, reconcileLinktype(ifi, lt))
		cs.setHandle(ep, h)

		sleepUntil(deadline)
		cs.stop()

		var buf bytes.Buffer
		cs.writePcap(&buf)
		return buf.Bytes(), iface, nil
	}

	target, err := s.resolveManagedTarget(nodeID)
	if err != nil {
		return nil, "", err
	}
	base := "https://" + net.JoinHostPort(target.ip.String(), strconv.Itoa(target.port))

	ifBody, err := peerCall(http.MethodGet, base+"/api/capture/mesh-iface", nil, 8<<20)
	if err != nil {
		return nil, "", fmt.Errorf("discovering interface: %w", err)
	}
	var ifResp struct {
		Ifaces []struct{ Network, Iface string } `json:"ifaces"`
	}
	if err := json.Unmarshal(ifBody, &ifResp); err != nil || len(ifResp.Ifaces) == 0 {
		return nil, "", fmt.Errorf("peer has no overlay interface configured")
	}
	iface = ifResp.Ifaces[0].Iface
	setIface(iface)

	startReq, _ := json.Marshal(map[string]string{"iface": iface})
	startBody, err := peerCall(http.MethodPost, base+"/api/capture/start", bytes.NewReader(startReq), 8<<20)
	if err != nil {
		return nil, iface, fmt.Errorf("starting: %w", err)
	}
	var startResp struct {
		Error string `json:"error"`
	}
	json.Unmarshal(startBody, &startResp)
	if startResp.Error != "" {
		return nil, iface, fmt.Errorf("starting: %s", startResp.Error)
	}

	sleepUntil(deadline)

	if _, err := peerCall(http.MethodPost, base+"/api/capture/stop", nil, 8<<20); err != nil {
		return nil, iface, fmt.Errorf("stopping: %w", err)
	}

	// capMaxBytes is the rolling buffer cap the pcap is built from; matches
	// handleProxy's proxyBodyLimit exception for this exact endpoint.
	pcap, err := peerCall(http.MethodGet, base+"/api/capture/pcap", nil, capMaxBytes+(1<<20))
	if err != nil {
		return nil, iface, fmt.Errorf("downloading: %w", err)
	}
	return pcap, iface, nil
}

func sleepUntil(t time.Time) {
	if d := time.Until(t); d > 0 {
		time.Sleep(d)
	}
}

// peerCall makes one request to a peer's admin API using the same client and
// trust model as handleProxy (overlay-internal, self-signed TLS skipped, the
// caller is authorized by its overlay source address on the peer's side —
// see webadmin.authed's Manager-mode bypass). Response body is capped at
// limit bytes, same defensive reasoning as proxyBodyLimit.
func peerCall(method, url string, body io.Reader, limit int64) ([]byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

var meshCaptureNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// bundle tars+gzips every peer's successful pcap into the job's .tgz, and, if
// any peer failed, adds an errors.txt summarizing which and why. Sets
// j.failed instead of j.tgz if nothing succeeded at all.
func (j *meshCaptureJob) bundle() {
	j.mu.Lock()
	peers := make([]*meshCapturePeerResult, len(j.peers))
	copy(peers, j.peers)
	j.mu.Unlock()

	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)
	now := time.Now()

	used := map[string]int{}
	nameFor := func(p *meshCapturePeerResult) string {
		base := meshCaptureNameRe.ReplaceAllString(p.Hostname, "_")
		base = strings.Trim(base, "_")
		if base == "" {
			base = meshCaptureNameRe.ReplaceAllString(p.NodeID, "_")
			base = strings.Trim(base, "_")
		}
		if base == "" {
			base = "peer"
		}
		if p.Self {
			base += "-local"
		}
		used[base]++
		if n := used[base]; n > 1 {
			return fmt.Sprintf("%s-%d.pcap", base, n)
		}
		return base + ".pcap"
	}

	ok := 0
	var errLines []string
	for _, p := range peers {
		if p.Status == "done" {
			ok++
			name := nameFor(p)
			hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(p.pcap)), ModTime: now}
			if tw.WriteHeader(hdr) == nil {
				tw.Write(p.pcap)
			}
		} else {
			who := p.Hostname
			if who == "" {
				who = p.NodeID
			}
			errLines = append(errLines, fmt.Sprintf("%s: %s", who, p.Error))
		}
	}
	if len(errLines) > 0 {
		txt := strings.Join(errLines, "\n") + "\n"
		hdr := &tar.Header{Name: "errors.txt", Mode: 0o644, Size: int64(len(txt)), ModTime: now}
		if tw.WriteHeader(hdr) == nil {
			tw.Write([]byte(txt))
		}
	}
	tw.Close()
	gzw.Close()

	j.mu.Lock()
	defer j.mu.Unlock()
	if ok == 0 {
		if len(errLines) == 1 {
			j.failed = errLines[0]
		} else {
			j.failed = "capture failed on every peer — see details below"
		}
	} else {
		j.tgz = out.Bytes()
	}
	j.done = true
}

// ---- HTTP handlers -----------------------------------------------------

// handleCaptureMeshIface reports this node's own overlay interface name(s),
// straight from the mesh engine rather than guessed from a naming
// convention — see meshCaptureJob's doc comment for why that matters. A
// manager fanning out a mesh-wide capture calls this on each peer first, so
// it never has to ask the operator what a given node happens to call its
// mesh device.
func (s *Server) handleCaptureMeshIface(w http.ResponseWriter, r *http.Request) {
	type ifc struct {
		Network string `json:"network"`
		Iface   string `json:"iface"`
	}
	var out []ifc
	for _, i := range s.be.Interfaces() {
		if i.Iface == "" {
			continue
		}
		out = append(out, ifc{Network: i.Name, Iface: i.Iface})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ifaces": out})
}

func (s *Server) handleCaptureMeshStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DurationSeconds int `json:"duration_seconds"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !meshCaptureDurations[req.DurationSeconds] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "duration must be 5, 10, 30, or 60 seconds"})
		return
	}
	job := s.meshCapture.start(s, time.Duration(req.DurationSeconds)*time.Second)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": job.id})
}

func (s *Server) handleCaptureMeshStatus(w http.ResponseWriter, r *http.Request) {
	job := s.meshCapture.current()
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	job.mu.Lock()
	peers := make([]meshCapturePeerResult, len(job.peers))
	for i, p := range job.peers {
		peers[i] = *p
	}
	done, failed, ready := job.done, job.failed, job.done && job.tgz != nil
	job.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"active":           true,
		"id":               job.id,
		"duration_seconds": int(job.duration / time.Second),
		"elapsed_seconds":  time.Since(job.startedAt).Seconds(),
		"done":             done,
		"error":            failed,
		"ready":            ready,
		"peers":            peers,
	})
}

func (s *Server) handleCaptureMeshDownload(w http.ResponseWriter, r *http.Request) {
	job := s.meshCapture.current()
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no mesh capture has been run"})
		return
	}
	job.mu.Lock()
	done, failed, tgz := job.done, job.failed, job.tgz
	job.mu.Unlock()
	if !done {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "capture still running"})
		return
	}
	if tgz == nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": failed})
		return
	}
	name := fmt.Sprintf("gravinet-mesh-capture-%s.tgz", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Write(tgz)
}
