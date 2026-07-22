package webadmin

import (
	"io"
	"net/http"
	"os"
	"time"

	"gravinet/internal/upgrade"
)

// UpgradeCtl is the daemon's upgrade machinery, handed to the web admin so the
// handlers below can drive it. Nil means the feature failed to initialize on
// this node (its store directory couldn't be created, or the running
// binary's own path couldn't be resolved) — genuine setup failures, not "no
// trusted release keys configured": upgrades are on by default now and don't
// need a key at all, see config.UpgradeEnabled.
type UpgradeCtl struct {
	Store      *upgrade.Store
	Guard      *upgrade.Guard
	Target     string // installed binary path this node would replace
	ConfigPath string
	Version    string
	PAM        bool

	ConfirmSeconds func() int
	KeepArtifacts  func() int

	// Restart puts a freshly-swapped binary into service.
	Restart func() error
	// Peers reports how many peers are currently connected, for the pre-swap
	// snapshot the guard uses to decide what "healthy" means afterwards.
	Peers func() int

	// Op runs one of the daemon's upgrade operations (list, status, apply,
	// rollback) and returns its JSON reply. It is the same entry point the CLI
	// reaches over the control socket, deliberately: the web admin is a second
	// front door onto one implementation, not a second implementation.
	Op func(op string, body []byte) ([]byte, error)
}

// SetUpgrade installs the upgrade machinery. Called by the daemon at startup.
func (s *Server) SetUpgrade(u *UpgradeCtl) { s.upg = u }

func (s *Server) upgradeOff(w http.ResponseWriter) bool {
	if s.upg == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "upgrade machinery failed to initialize on this node — check the daemon log",
		})
		return true
	}
	return false
}

// upgradeLocalOnly enforces that this endpoint is reachable only by a session
// logged into this exact node — never by a peer, Manager or otherwise.
//
// It exists as its own explicit gate, checked first by every handler in this
// file, rather than relying on authed()'s general bypass. That bypass —
// accept a request whose source resolves to a Manager peer over the overlay —
// is the *correct* default for the rest of the admin API (firewall, routes,
// NAT, ...), which is exactly why upgrades cannot just inherit it: this
// feature has no remote trigger at all, from anywhere, under any
// configuration, and authed() has no way to know this one family of
// endpoints opted out of the bypass it grants everything else.
func (s *Server) upgradeLocalOnly(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.validSession(r); ok {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "upgrades are local-only: this node does not accept upgrade administration from any peer, Manager or otherwise \u2014 log in directly on this node",
	})
	return false
}

// handleUpgradeRollback backs out an upgrade that already committed. The
// automatic guard only covers failures it can see (crash loops, a node that
// never rejoins the mesh); this covers the ones it cannot — a regression that a
// health check has no opinion about, discovered by a human an hour later.
func (s *Server) handleUpgradeRollback(w http.ResponseWriter, r *http.Request) {
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
	st := s.upg.Guard.Load()
	if st.Target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "this node has no record of an applied upgrade to roll back"})
		return
	}
	s.log.Warnf("upgrade: rolling back %s -> %s at operator request", st.To, st.From)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rolling_back_to": st.From, "restarting": true})
	go func() {
		time.Sleep(700 * time.Millisecond)
		if err := s.upg.Guard.Rollback(); err != nil {
			s.log.Errorf("upgrade: rollback failed: %v", err)
		}
	}()
}

// ---------------------------------------------------------------------------
// The operator-facing surface (the Upgrade tab)
// ---------------------------------------------------------------------------

// op runs a daemon upgrade operation and relays its JSON reply verbatim. The
// handlers below are thin on purpose: every decision they could make has already
// been made, and tested, in internal/upgrade.
func (s *Server) op(w http.ResponseWriter, name string, body []byte) {
	if s.upg.Op == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "this node has no upgrade control surface"})
		return
	}
	out, err := s.upg.Op(name, body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

// handleUpgradeHome is what the tab loads first: this node's own state, plus
// everything it has staged. Reported even when upgrades are switched off, with
// enabled=false and the reason — a tab that renders "not found" leaves the
// operator guessing, and the actual answer ("this node trusts no release keys")
// is one they can act on in about thirty seconds.
func (s *Server) handleUpgradeHome(w http.ResponseWriter, r *http.Request) {
	if !s.upgradeLocalOnly(w, r) {
		return
	}
	if s.upg == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"reason":  "upgrade machinery failed to initialize on this node — check the daemon log",
			"version": s.version,
		})
		return
	}
	st := s.upg.Guard.Load()
	_, backupErr := os.Stat(upgrade.BackupPath(s.upg.Target))
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":         true,
		"version":         s.upg.Version,
		"target":          s.upg.Target,
		"store":           s.upg.Store.Dir(),
		"pam":             s.upg.PAM,
		"phase":           st.Phase,
		"from":            st.From,
		"to":              st.To,
		"boots":           st.Boots,
		"pre_peers":       st.PrePeers,
		"peers_now":       s.upg.Peers(),
		"last_error":      st.LastError,
		"confirm_seconds": s.upg.ConfirmSeconds(),
		// signing_required tells the browser whether it may offer the plain
		// binary-only upload (no manifest, no key) or must collect a signed
		// manifest first — mirrors Store.Verify's own policy exactly, since it's
		// read from the same trust set.
		"signing_required":   len(s.upg.Store.Trusted()) > 0,
		"rollback_available": backupErr == nil,
	})
}

// handleUpgradeLocalApply applies an already-staged artifact to this node:
// the request is just its id, since Upload already put the bytes in the
// store. Runs through the daemon's "apply" op (see controlOp in
// cmd/gravinet/upgrade.go), the same entry point the CLI reaches over the
// control socket — one implementation, two front doors.
func (s *Server) handleUpgradeLocalApply(w http.ResponseWriter, r *http.Request) {
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.op(w, "apply", body)
}

// handleUpgradeStage takes a build and its signed manifest as a multipart
// upload and ingests them. This is how the first copy of a new build reaches
// a node from the operator's browser, when they already have a built binary
// (typically because trusted_keys is configured, so it needs to be signed) —
// see handleUpgradeStageSource for the no-manifest, source-only path.
//
// The manifest is required and must arrive first (the form is ordered): the
// artifact's bytes can only be verified against a manifest already held, and
// Ingest refuses to write a single byte anywhere it could later be executed
// from until it has one. Store.Verify decides whether that manifest actually
// needs a valid signature (trusted_keys configured) or just needs to be
// structurally sound (none configured) — this handler doesn't special-case
// that itself, so it can't drift out of step with what Ingest decides a
// moment later.
func (s *Server) handleUpgradeStage(w http.ResponseWriter, r *http.Request) {
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
	var man upgrade.Manifest
	haveMan := false
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
		case "manifest":
			b, err := io.ReadAll(io.LimitReader(part, 64<<10))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			m, err := upgrade.ParseManifest(b)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			// Reject before accepting a single byte of the artifact — a manifest
			// this node would not accept the binary for is not a manifest worth
			// spending an upload on. Store.Verify applies this node's actual
			// trust policy (signature-checked, or structural-only if this node
			// trusts no release keys), so this can never drift out of step with
			// what Ingest itself will decide a moment later.
			if err := s.upg.Store.Verify(m); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
				return
			}
			man, haveMan = m, true
		case "artifact":
			if !haveMan {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a manifest must be uploaded before the artifact \u2014 use /api/upgrade/stage-source instead if you don't have one (uploads a source .tgz/.tar.gz or .zip, no manifest needed)"})
				return
			}
			if err := s.upg.Store.Ingest(man, part); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			s.log.Infof("upgrade: staged %s from the web admin", man.ID())
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "staged": man.ID()})
			return
		}
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the upload carried no artifact"})
}

// handleUpgradeStageSource takes an uploaded gravinet source tree as a single
// request body — a gzip-compressed tar (.tgz/.tar.gz) or a zip (.zip),
// whichever extractSourceArchive determines it to be from its content — builds
// it (the same `go build` the platform installers run), and ingests the
// result exactly like handleUpgradeStage's unsigned binary path does. This
// exists for the case the raw-binary path doesn't cover: you don't have a
// built gravinet binary at all, only source — which, for this project, is
// *every* fresh checkout, since there is no separate prebuilt release
// artifact anywhere. The installers already do this build step
// automatically; this is the same step, offered from the browser.
//
// zip, alongside the tgz the installers themselves produce, matters here
// specifically because it's what GitHub's own "Download ZIP" button hands
// you — the most likely way to get this project's source onto a box that
// doesn't already have a git client on it — and before this it simply
// wasn't accepted as an upgrade source at all.
//
// Only available in local-only-unsigned mode (no trusted_keys), and for the
// same reason handleUpgradeStage's unsigned path is: there is no key to check
// a signature against here even in principle, since nothing about the signed
// -manifest scheme covers source code, only a built artifact's digest. If you
// want signed provenance, sign a binary you've already built and use the
// ordinary binary+manifest upload instead — building from arbitrary uploaded
// source under a node that trusts release keys would be a hole in that
// promise, not an extension of it.
func (s *Server) handleUpgradeStageSource(w http.ResponseWriter, r *http.Request) {
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
	if len(s.upg.Store.Trusted()) > 0 {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "this node trusts release keys (upgrade.trusted_keys) \u2014 building from uploaded source has no signature to check and is only offered in local-only-unsigned mode; build and sign a binary yourself and use the ordinary upload instead",
		})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSourceUploadSize)
	m, err := StageFromSource(s.upg.Store, r.Body, "built from uploaded source via web admin, unsigned (local-only mode)")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.log.Infof("upgrade: built and staged %s from uploaded source via the web admin", m.ID())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "staged": m.ID(), "unsigned": true, "built_from_source": true})
}
