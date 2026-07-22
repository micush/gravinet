package webadmin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// handleUpgradeRemoteApply is the ONLY upgrade endpoint that a peer can reach,
// and it is closed by default. It exists so a Manager can push a gravinet
// source archive to nodes that have explicitly opted in (config
// Upgrade.AcceptManagerUpgrades) — the trust rides the mesh session that
// already authenticates every other thing a Manager does, plus the archive's
// own content digest.
//
// What crosses the wire is source, never a binary. That is not an
// implementation detail, it is the reason a single push can serve a mixed
// fleet: gravinet runs on Linux, the BSDs, macOS and Windows, publishes no
// prebuilt artifact for any of them, and a binary is only ever valid for the
// one platform that built it. An archive is valid everywhere, and each node
// compiles it into a binary that is native by construction. It also narrows
// what the opt-in actually grants: a Manager supplies source this node chooses
// to compile, not bytes this node is asked to execute.
//
// It deliberately does NOT call upgradeLocalOnly (the gate every other handler
// in upgrade.go uses to refuse all peers). Instead it enforces a different,
// stricter-in-its-own-way set of conditions, ALL of which must hold:
//
//  1. This node opted in. AcceptManagerUpgrades() is true. Off by default;
//     when off this endpoint is indistinguishable from not existing.
//  2. The caller is a Manager this node holds a LIVE DIRECT SESSION with.
//     Not a locally-logged-in session (that path uses the local upload), and
//     not a Manager known only through gossip — IsManagerNeighborAddr requires
//     a real handshake, closing the gossip-spoof gap that would otherwise turn
//     a mislabeled address into remote root. A locally logged-in operator is
//     also accepted, so the same endpoint can be exercised/tested on-box.
//  3. The bytes are what the Manager said they were. The archive is hashed as
//     it is spooled and compared against the digest declared earlier in the
//     same multipart body, so a truncated or substituted stream is refused
//     before anything is extracted, let alone compiled.
//  4. It still has to survive the build and the apply. The daemon's "apply" op
//     compiles the archive with this node's own toolchain, runs the same
//     selftest config gate, and arms the same confirm-or-rollback guard a
//     local upgrade does, so a Manager cannot pin this node onto a broken
//     binary: if the new one can't rejoin the mesh within ConfirmSeconds, this
//     node backs it out on its own authority.
//
// The Manager reaches this over the existing overlay management proxy (see
// cluster.go handleProxy), so it is subject to the same SSRF-guarded,
// overlay-sourced hop as every other managed API call.
func (s *Server) handleUpgradeRemoteApply(w http.ResponseWriter, r *http.Request) {
	if s.upgradeOff(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}

	// Gate 1: this node must have opted in.
	if s.upg.AcceptManagerUpgrades == nil || !s.upg.AcceptManagerUpgrades() {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "this node does not accept Manager-pushed upgrades \u2014 enable upgrade.accept_manager_upgrades in its settings first (it is off by default, and off means upgrades here are strictly local-only)",
		})
		return
	}

	// Gate 2: the caller must be either a locally logged-in session on this
	// exact node, or a Manager this node holds a live *direct* session with.
	// A gossip-only "manager" is rejected here even though authed() would let
	// it through for ordinary management — causing code to be built and run as
	// root is not ordinary management. See IsManagerNeighborAddr.
	_, localSession := s.validSession(r)
	if !localSession {
		ip := remoteIP(r)
		if !ip.IsValid() || !s.be.OverlayContains(ip) || !s.be.IsManagerNeighborAddr(ip) {
			reason := "the caller is not a directly-connected Manager peer"
			if ip.IsValid() {
				if !s.be.OverlayContains(ip) {
					reason = fmt.Sprintf("the connection from %s did not arrive over the overlay", ip)
				} else if !s.be.IsManagerNeighborAddr(ip) {
					reason = fmt.Sprintf("%s is a valid overlay address but is not a directly-connected Manager (a Manager known only through gossip is not sufficient to push an upgrade)", ip)
				}
			}
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "refusing a remote upgrade: " + reason,
			})
			return
		}
	}

	// Gate 3: spool the archive, hashing as it lands, and hold it against the
	// digest the Manager declared. The digest part is required and must arrive
	// first: a hash computed over bytes this node already accepted, then
	// compared against a claim that arrived afterwards in the same body, is
	// not a check a substituted stream would fail.
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expected a multipart upload: " + err.Error()})
		return
	}
	want := ""
	spooled := ""
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
		case "sha256":
			b, err := io.ReadAll(io.LimitReader(part, 128))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			want = strings.ToLower(strings.TrimSpace(string(b)))
		case "source":
			if want == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the sha256 digest must arrive before the source archive"})
				return
			}
			path, got, err := spoolUpload(s.upg.StateDir, part)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			spooled = path
			if got != want {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": fmt.Sprintf("the pushed archive hashes to %s but the push declared %s \u2014 refusing it", got, want),
				})
				return
			}
		}
	}
	if spooled == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the push carried no source archive"})
		return
	}

	// Gate 4: build and apply through the daemon's own op — same toolchain,
	// same selftest gate, same confirm-or-rollback guard as a local upgrade.
	// If the new binary can't rejoin the mesh, this node reverts itself; the
	// Manager gets no say in that.
	s.log.Warnf("upgrade: building Manager-pushed source (sha256 %s, accept_manager_upgrades is on)", want[:12])
	body, _ := json.Marshal(map[string]any{"src_path": spooled})
	s.op(w, "apply", body)
}
