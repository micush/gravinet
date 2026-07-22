package webadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"gravinet/internal/upgrade"
)

// UpgradeCtl is the daemon's upgrade machinery, handed to the web admin so the
// handlers below can drive it. Nil means the feature failed to initialize on
// this node (its state directory couldn't be created, or the running binary's
// own path couldn't be resolved) — genuine setup failures. There is no
// configuration required to use upgrades at all; see config.UpgradeEnabled.
type UpgradeCtl struct {
	Guard      *upgrade.Guard
	StateDir   string // where the guard's state file lives, and where uploads are spooled
	Target     string // installed binary path this node would replace
	ConfigPath string
	Version    string
	PAM        bool

	ConfirmSeconds func() int

	// Restart puts a freshly-swapped binary into service.
	Restart func() error
	// Peers reports how many peers are currently connected, for the pre-swap
	// snapshot the guard uses to decide what "healthy" means afterwards.
	Peers func() int

	// Op runs one of the daemon's upgrade operations (status, apply, rollback)
	// and returns its JSON reply. It is the same entry point the CLI reaches
	// over the control socket, deliberately: the web admin is a second front
	// door onto one implementation, not a second implementation.
	Op func(op string, body []byte) ([]byte, error)

	// AcceptManagerUpgrades reports this node's opt-in to source archives
	// pushed by a directly-authenticated Manager peer (config
	// Upgrade.AcceptManagerUpgrades). Nil or false-returning means the
	// remote-apply endpoint stays fully closed, exactly as if the feature
	// did not exist. Only handleUpgradeRemoteApply consults it.
	AcceptManagerUpgrades func() bool
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

// handleUpgradeHome is what the tab loads first: this node's own state. There
// is nothing staged to report alongside it — a build and its apply are one
// request now. Reported even when the machinery failed to initialize, with
// enabled=false and the reason, because a tab that renders "not found" leaves
// the operator guessing at something they could otherwise act on in about
// thirty seconds.
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
		"enabled":            true,
		"version":            s.upg.Version,
		"target":             s.upg.Target,
		"state_dir":          s.upg.StateDir,
		"pam":                s.upg.PAM,
		"phase":              st.Phase,
		"from":               st.From,
		"to":                 st.To,
		"boots":              st.Boots,
		"pre_peers":          st.PrePeers,
		"peers_now":          s.upg.Peers(),
		"last_error":         st.LastError,
		"confirm_seconds":    s.upg.ConfirmSeconds(),
		"rollback_available": backupErr == nil,
	})
}

// spoolUpload streams an upload to a temp file under the state directory,
// hashing as it goes, and returns the path and hex digest. Spooling rather
// than streaming straight into a build is what lets one upload serve several
// consumers: the push handler sends the same bytes to N peers, and every
// consumer needs a digest computed over exactly what was written, not over
// what a sender claimed.
//
// The caller owns the returned path and must remove it.
func spoolUpload(dir string, r io.Reader) (path, sum string, err error) {
	f, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", "", err
	}
	path = f.Name()
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, upgrade.MaxSourceUploadSize+1))
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(path)
		return "", "", copyErr
	}
	if closeErr != nil {
		os.Remove(path)
		return "", "", closeErr
	}
	if n == 0 {
		os.Remove(path)
		return "", "", errors.New("the upload was empty")
	}
	if n > upgrade.MaxSourceUploadSize {
		os.Remove(path)
		return "", "", fmt.Errorf("upload exceeds the %d-byte size ceiling", int64(upgrade.MaxSourceUploadSize))
	}
	return path, hex.EncodeToString(h.Sum(nil)), nil
}

// handleUpgradeSource is the whole local upgrade surface: upload a gravinet
// source archive (.tgz/.tar.gz or .zip, detected by content rather than by
// filename), and this node builds it with its own Go toolchain, preflights the
// result against its own config, and swaps it in behind the confirm-or-
// rollback guard.
//
// There is no binary upload alongside this, and no staging step before it.
// gravinet publishes no prebuilt binary for any platform — every fresh
// checkout is source and nothing else — so a binary upload had no supply to
// draw on, and an artifact shelf had nothing to hold between a build and an
// apply that now happen in one request. What replaced both is the thing the
// platform installers have always done: compile it here, on the machine that
// will run it.
func (s *Server) handleUpgradeSource(w http.ResponseWriter, r *http.Request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, upgrade.MaxSourceUploadSize)
	path, sum, err := spoolUpload(s.upg.StateDir, r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	defer os.Remove(path)
	s.log.Infof("upgrade: building uploaded source (sha256 %s) from the web admin", sum[:12])
	body, _ := json.Marshal(map[string]any{"src_path": path})
	s.op(w, "apply", body)
}
