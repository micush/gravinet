package webadmin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// pushConcurrency bounds how many peers are built at once. Every target
// compiles the archive locally, so a wide fan-out means N simultaneous Go
// builds across the fleet — on the small boxes gravinet often runs on, that is
// the difference between a rollout and a self-inflicted outage. Sequential
// would be safest and far too slow for a real fleet; this is the middle.
const pushConcurrency = 4

// handleUpgradePush distributes an uploaded source archive to one or more
// managed peers, from the node the operator is logged into. It is the
// counterpart to each peer's handleUpgradeRemoteApply: this side does the
// pushing, that side does the (opt-in, verified) accepting.
//
// One upload, one archive, every platform. Because what is distributed is
// source rather than a built binary, the operator does not select peers by
// architecture, cross-compile anything, or hold a matrix of artifacts — a
// mesh of Linux, FreeBSD, OpenBSD, macOS and Windows nodes all take the same
// bytes and each compiles its own native binary from them.
//
// Like the other fleet-driving actions (see handleProxy's blocklist), this is
// driven from the node you are actually looking at and is never itself proxied
// to a peer — "node A tells node B to push B's archive across B's mesh" is
// exactly the two-managers confusion that blocklist prevents, so this handler
// is local-session-only.
//
// Request: a multipart POST carrying a "nodes" field (a JSON array of peer
// names) and a "source" file (the archive). The archive is spooled once and
// hashed once; every peer then receives the same bytes and the same digest.
func (s *Server) handleUpgradePush(w http.ResponseWriter, r *http.Request) {
	// Local-only: this drives a fleet action, so it must originate at the node
	// the operator is logged into, never arrive over the proxy from a peer.
	if !s.upgradeLocalOnly(w, r) {
		return
	}
	if s.upgradeOff(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expected a multipart upload: " + err.Error()})
		return
	}
	var nodes []string
	spooled, sum := "", ""
	defer func() {
		if spooled != "" {
			os.Remove(spooled)
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		switch part.FormName() {
		case "nodes":
			b, err := io.ReadAll(io.LimitReader(part, 1<<20))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			if err := json.Unmarshal(b, &nodes); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nodes must be a JSON array of peer names: " + err.Error()})
				return
			}
		case "source":
			if len(nodes) == 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the nodes list must arrive before the source archive"})
				return
			}
			path, got, err := spoolUpload(s.upg.StateDir, part)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			spooled, sum = path, got
		}
	}
	if len(nodes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a non-empty nodes list is required"})
		return
	}
	if spooled == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the push carried no source archive"})
		return
	}

	type result struct {
		Node   string `json:"node"`
		OK     bool   `json:"ok"`
		Status int    `json:"status,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]result, len(nodes))

	var wg sync.WaitGroup
	sem := make(chan struct{}, pushConcurrency)
	for i, node := range nodes {
		wg.Add(1)
		go func(i int, node string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			target, err := s.resolveManagedTarget(node)
			if err != nil {
				msg := err.Error()
				if mte, ok := err.(*managedTargetError); ok {
					msg = mte.msg
				}
				results[i] = result{Node: node, OK: false, Error: msg}
				return
			}
			status, perr := s.pushSourceTo(target, spooled, sum)
			if perr != nil {
				results[i] = result{Node: node, OK: false, Status: status, Error: perr.Error()}
				return
			}
			results[i] = result{Node: node, OK: true, Status: status}
		}(i, node)
	}
	wg.Wait()

	pushed := 0
	for _, r := range results {
		if r.OK {
			pushed++
		}
	}
	s.log.Infof("upgrade: pushed source (sha256 %s) to %d of %d requested peer(s)", sum[:12], pushed, len(nodes))
	writeJSON(w, http.StatusOK, map[string]any{"sha256": sum, "pushed": pushed, "results": results})
}

// pushSourceTo streams one spooled source archive (digest first, then bytes) to
// a single peer's remote-apply endpoint over the overlay. Returns the peer's
// HTTP status and, on anything other than 200, an error carrying the peer's own
// message so the operator sees why that specific node refused (most often: it
// hasn't opted in, or it has no Go toolchain).
//
// The file is reopened per peer rather than buffered in memory: a source tree
// is only a few MiB, but N concurrent pushes each holding their own copy is a
// cost with no upside when the bytes are already on disk.
func (s *Server) pushSourceTo(target *clusterPeerTarget, srcPath, sum string) (int, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return 0, fmt.Errorf("reopening the spooled archive: %w", err)
	}
	defer f.Close()

	// Stream the multipart body through a pipe so the archive is never held in
	// memory in full — the reader side feeds the request as it's written.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var werr error
		defer func() { pw.CloseWithError(werr) }()
		// digest part first: the peer refuses to accept a byte of archive
		// before it holds the digest to check it against (see
		// handleUpgradeRemoteApply).
		part, err := mw.CreateFormField("sha256")
		if err != nil {
			werr = err
			return
		}
		if _, err := part.Write([]byte(sum)); err != nil {
			werr = err
			return
		}
		aw, err := mw.CreateFormFile("source", "gravinet-src.tgz")
		if err != nil {
			werr = err
			return
		}
		if _, err := io.Copy(aw, f); err != nil {
			werr = err
			return
		}
		werr = mw.Close()
	}()

	hostport := net.JoinHostPort(target.ip.String(), strconv.Itoa(target.port))
	url := "https://" + hostport + "/api/upgrade/remote-apply"
	req, err := http.NewRequest(http.MethodPost, url, pr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	// A source push plus the peer's full build, selftest and swap can take a
	// while — the build alone is bounded at ten minutes on the peer's side, and
	// on a small box it will use most of that. Give it room well beyond the
	// ordinary proxyClient timeout, but still bounded so one wedged peer can't
	// hold a rollout open forever.
	client := &http.Client{Timeout: 15 * time.Minute, Transport: proxyClient.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusOK {
		// Surface the peer's own error text (e.g. "does not accept
		// Manager-pushed upgrades", or a compiler error) rather than a bare
		// status.
		msg := string(bytes.TrimSpace(body))
		var je struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &je) == nil && je.Error != "" {
			msg = je.Error
		}
		return resp.StatusCode, fmt.Errorf("%s", msg)
	}
	return resp.StatusCode, nil
}
