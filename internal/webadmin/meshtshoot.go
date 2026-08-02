package webadmin

// Mesh-wide troubleshooting bundle: one download containing every reachable
// managed peer's own tshoot bundle (this node included), flattened into a
// single .tgz of per-peer .txt files plus an errors.txt for anyone that
// couldn't be reached. See tshoot.go's own doc comment for what goes into
// each individual bundle; this is the fan-out sibling that collects all of
// them in one request instead of clicking through every peer by hand — the
// same problem meshcapture.go already solved for packet captures, applied
// here to tshoot.
//
// Unlike the mesh-wide packet capture, this needs no background job, shared
// deadline, or polling: building one node's bundle is near-instant (it
// reads already-in-memory state plus a bounded log tail, not a timed
// capture window), so the whole fan-out is one synchronous request that
// blocks until every peer has answered or timed out, then returns the
// finished archive directly — no /start, /status, /download three-step
// dance, just one handler.
//
// Always acts on the node the browser session is logged into, regardless of
// which peer is selected in the header — see LOCAL_API's doc comment (ui.go)
// on the mesh/* paths for why: this fans out from *this* node's own
// managed-peer list, and proxying the request to a selected peer would
// silently fan out from that peer's list instead, while still looking like
// it was driven from here.
//
// Each remote peer's bundle arrives as its own single-member .tgz (the exact
// shape packTshootTgz produces) rather than as raw text — reusing /api/tshoot
// unchanged instead of adding a second, text-only endpoint. extractSingleTxtMember
// unwraps it so the outer archive holds a flat list of plain .txt files, not
// a .tgz nested inside a .tgz.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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

// meshTshootPeerTimeout bounds how long the fan-out waits on any one peer.
// Generous relative to the mesh capture's own per-probe timeouts (an
// interface lookup there is near-instant), since building a tshoot bundle
// does real work on the far end — reads a bounded log tail, marshals every
// peer/route/ban/firewall-rule struct on every network — but still bounded,
// so one unreachable or wedged peer can't hang the whole download.
const meshTshootPeerTimeout = 15 * time.Second

// meshTshootPeerResult is one target's outcome: either Txt (the peer's
// bundle, already unwrapped to plain text) or Err, never both meaningfully
// set at once.
type meshTshootPeerResult struct {
	NodeID   string
	Hostname string
	Self     bool
	Txt      []byte
	Err      error
}

// handleMeshTshoot fans out to every reachable managed peer plus this node,
// pulls each one's tshoot bundle, and returns one flattened .tgz containing
// every peer's <hostname>.txt plus errors.txt for anyone that failed.
func (s *Server) handleMeshTshoot(w http.ResponseWriter, r *http.Request) {
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
		// Same reachability test meshCaptureJob.run and handleCluster's
		// Manageable both use: a gossip-only address with no live session
		// (and not a seed, which is always dial-attempted) is one this node
		// structurally can't reach.
		if !(ip.IsValid() && p.WebPort != 0 && (p.Connected || p.IsSeed)) {
			continue
		}
		targets = append(targets, target{nodeID: p.NodeID, hostname: p.Hostname})
	}

	results := make([]meshTshootPeerResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			results[i] = meshTshootPeerResult{NodeID: t.nodeID, Hostname: t.hostname, Self: t.self}
			results[i].Txt, results[i].Err = s.fetchTshootOne(t.nodeID, t.self)
		}(i, t)
	}
	wg.Wait()

	data, err := bundleMeshTshoot(results)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	name := fmt.Sprintf("gravinet-tshoot-mesh-%s.tgz", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// fetchTshootOne gets one target's tshoot bundle as plain text: built
// in-process (no HTTP round trip) for self via the exact same
// buildTshootText handleTshoot itself calls, or pulled over the overlay for
// a peer — the same dial captureOnePeer (meshcapture.go) already uses for
// the mesh-wide capture's remote leg, hitting the peer's own unmodified
// /api/tshoot rather than a second endpoint.
func (s *Server) fetchTshootOne(nodeID string, self bool) ([]byte, error) {
	if self {
		txt, _ := s.buildTshootText()
		return []byte(txt), nil
	}

	target, err := s.resolveManagedTarget(nodeID)
	if err != nil {
		return nil, err
	}
	base := "https://" + net.JoinHostPort(target.ip.String(), strconv.Itoa(target.port))

	ctx, cancel := context.WithTimeout(context.Background(), meshTshootPeerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tshoot", nil)
	if err != nil {
		return nil, err
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 16MiB: generous relative to a real bundle (a redacted config plus a
	// bounded ~4MiB log tail plus JSON-dumped peer/route/ban state), same
	// defensive-cap reasoning as peerCall's limit param elsewhere in this
	// package.
	tgz, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(tgz))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("peer returned %s: %s", resp.Status, msg)
	}
	return extractSingleTxtMember(tgz)
}

// extractSingleTxtMember gunzips and untars a tshoot .tgz (as produced by
// packTshootTgz — always exactly one member) and returns that member's raw
// bytes, so a peer's bundle can be flattened straight into the outer mesh
// archive as a plain .txt instead of nesting a .tgz inside a .tgz. Falls
// back to treating the body as already-plain-text if it isn't gzip at all
// (packTshootTgz's own fallback path on a from-source node too old to have
// this fix, or one that hit its own "archiving somehow failed" branch) —
// rather than failing the whole peer over a format difference that carries
// no real information loss either way.
func extractSingleTxtMember(body []byte) ([]byte, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body, nil // not gzip — assume it's the plain-text fallback shape
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	if _, err := tr.Next(); err != nil {
		return nil, fmt.Errorf("empty tshoot archive: %w", err)
	}
	return io.ReadAll(tr)
}

// meshTshootNameRe sanitizes a peer's hostname/node id into a safe tar
// member name — same character class meshcapture.go's own naming helper
// uses, applied here to .txt names instead of .pcap.
var meshTshootNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// bundleMeshTshoot tars+gzips every successful result's text as
// <hostname>.txt, plus an errors.txt listing anyone that failed. Mirrors
// meshCaptureJob.bundle's naming convention (sanitized hostname, "-local"
// suffix for self, numbered on collision) applied to a text bundle instead
// of a pcap. Returns an error instead of an (empty) archive if nothing
// succeeded at all — mirroring meshCaptureJob.run's "no reachable managed
// peers, and no local capture to fall back to" case, since a .tgz containing
// only errors.txt is a worse UI experience than a plain failure message.
func bundleMeshTshoot(results []meshTshootPeerResult) ([]byte, error) {
	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)
	now := time.Now()

	used := map[string]int{}
	nameFor := func(p meshTshootPeerResult) string {
		base := meshTshootNameRe.ReplaceAllString(p.Hostname, "_")
		base = strings.Trim(base, "_")
		if base == "" {
			base = meshTshootNameRe.ReplaceAllString(p.NodeID, "_")
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
			return fmt.Sprintf("%s-%d.txt", base, n)
		}
		return base + ".txt"
	}

	ok := 0
	var errLines []string
	for _, p := range results {
		if p.Err == nil && len(p.Txt) > 0 {
			ok++
			name := nameFor(p)
			hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(p.Txt)), ModTime: now}
			if tw.WriteHeader(hdr) == nil {
				_, _ = tw.Write(p.Txt)
			}
			continue
		}
		who := p.Hostname
		if who == "" {
			who = p.NodeID
		}
		msg := "empty bundle"
		if p.Err != nil {
			msg = p.Err.Error()
		}
		errLines = append(errLines, fmt.Sprintf("%s: %s", who, msg))
	}

	if ok == 0 {
		if len(errLines) == 1 {
			return nil, fmt.Errorf("%s", errLines[0])
		}
		return nil, fmt.Errorf("could not build a bundle for any peer")
	}

	if len(errLines) > 0 {
		txt := strings.Join(errLines, "\n") + "\n"
		hdr := &tar.Header{Name: "errors.txt", Mode: 0o644, Size: int64(len(txt)), ModTime: now}
		if tw.WriteHeader(hdr) == nil {
			_, _ = tw.Write([]byte(txt))
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
