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

	"gravinet/internal/config"
	"gravinet/internal/service"
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
	// Same reasoning as handleUpgradePush's: the request body is already read
	// by the time an op runs, and building a source archive takes minutes,
	// which is well past the server's 30s ReadTimeout. That deadline is on the
	// connection rather than on either direction of it, so leaving it in place
	// lets the connection be torn down under a build that is proceeding
	// perfectly well — the browser then reports a network error for an upgrade
	// that may still be running on the node.
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		s.log.Debugf("upgrade: could not clear read deadline for %s: %v", name, err)
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

// handleUpgradeOSUpdates reads or replaces this node's scheduled host OS
// package update configuration, and can trigger an immediate run — the
// backend for System > Upgrade's "OS updates" section. Local-only (see
// upgradeLocalOnly), the same as every other endpoint on this page: unlike
// Power/Time/Users/SNMP/LLDP, which all deliberately follow whichever
// node is currently selected, scheduling unattended OS patching for a
// *remote* peer without that peer's own operator directly involved is
// exactly the kind of thing this page's existing "no peer can trigger this"
// philosophy already exists to prevent for gravinet's own upgrade
// mechanism — applied here to a second thing that can meaningfully change
// what's running on a host.
//
// GET returns the current config plus live state: whether this platform
// supports it at all (OSUpdatesSupported), when the next scheduled run
// would be (NextOSUpdateRun), whether one is running right now
// (OSUpdateRunning), and the last completed run's outcome (the on-disk
// breadcrumb, LoadOSUpdateState — survives a gravinet restart and a
// browser refresh).
//
// POST takes either a config save or an immediate-run request:
//
//	{op:"save", enabled, cadence, weekday, day_of_month, hour, minute}
//	{op:"run_now"}
//
// A "run_now" starts a real update pass, which can take minutes — it's
// deliberately asynchronous: the handler kicks it off in a goroutine and
// returns immediately with running:true, rather than blocking the request
// (and risking a browser/proxy timeout) for however long the host's
// package manager takes. The page finds out it's done the same way it
// finds out a scheduled run happened: polling GET and watching
// last_run/running.
func (s *Server) handleUpgradeOSUpdates(w http.ResponseWriter, r *http.Request) {
	if !s.upgradeLocalOnly(w, r) {
		return
	}
	statePath := ""
	if s.upg != nil {
		statePath = service.OSUpdateStatePath(s.upg.StateDir)
	}

	if r.Method == http.MethodGet {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, osUpdatesJSON(cfg.OSUpdates, statePath))
		return
	}

	var req struct {
		Op         string `json:"op"`
		Enabled    bool   `json:"enabled"`
		Cadence    string `json:"cadence"`
		Weekday    int    `json:"weekday"`
		DayOfMonth int    `json:"day_of_month"`
		Hour       int    `json:"hour"`
		Minute     int    `json:"minute"`
	}
	if !decode(w, r, &req) {
		return
	}

	switch req.Op {
	case "run_now":
		if statePath == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "upgrade machinery failed to initialize on this node — check the daemon log"})
			return
		}
		if service.OSUpdateRunning() {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "an update is already running"})
			return
		}
		s.log.Infof("webadmin: starting an OS update pass now (requested from admin UI)")
		go func() {
			if ok, output := service.RunOSUpdateNow(statePath, "manual"); !ok {
				s.log.Warnf("webadmin: manual OS update run failed: %s", output)
			}
		}()
		cfg, _ := config.Load(s.configPath)
		resp := osUpdatesJSON(cfg.OSUpdates, statePath)
		resp["ok"] = true
		writeJSON(w, http.StatusOK, resp)
		return
	case "save":
		if req.Enabled {
			switch req.Cadence {
			case "daily":
			case "weekly":
				if req.Weekday < 0 || req.Weekday > 6 {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "weekday must be 0 (Sunday) through 6 (Saturday)"})
					return
				}
			case "monthly":
				if req.DayOfMonth < 1 || req.DayOfMonth > 28 {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "day_of_month must be between 1 and 28 (capped so every month actually has that day)"})
					return
				}
			default:
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cadence must be 'daily', 'weekly', or 'monthly'"})
				return
			}
			if req.Hour < 0 || req.Hour > 23 || req.Minute < 0 || req.Minute > 59 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "hour must be 0-23 and minute 0-59"})
				return
			}
		}
		osu := config.OSUpdateConfig{
			Enabled: req.Enabled, Cadence: req.Cadence, Weekday: req.Weekday,
			DayOfMonth: req.DayOfMonth, Hour: req.Hour, Minute: req.Minute,
		}
		s.log.Infof("webadmin: saving OS update schedule (enabled=%v cadence=%q) (requested from admin UI)", req.Enabled, req.Cadence)
		if err := s.mutateConfig(r, func(cfg *config.Config) error {
			cfg.OSUpdates = osu
			return nil
		}); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		resp := osUpdatesJSON(osu, statePath)
		resp["ok"] = true
		writeJSON(w, http.StatusOK, resp)
		return
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "op must be 'save' or 'run_now'"})
		return
	}
}

// osUpdatesJSON flattens an OSUpdateConfig plus live scheduler/run state
// for the wire.
func osUpdatesJSON(cfg config.OSUpdateConfig, statePath string) map[string]any {
	supported, hint := service.OSUpdatesSupported()
	resp := map[string]any{
		"enabled": cfg.Enabled, "cadence": cfg.Cadence, "weekday": cfg.Weekday,
		"day_of_month": cfg.DayOfMonth, "hour": cfg.Hour, "minute": cfg.Minute,
		"supported": supported, "hint": hint,
		"running": service.OSUpdateRunning(),
	}
	if next, ok := service.NextOSUpdateRun(cfg, time.Now()); ok {
		resp["next_run"] = next.Format(time.RFC3339)
	}
	if statePath != "" {
		st := service.LoadOSUpdateState(statePath)
		if !st.LastRun.IsZero() {
			resp["last_run"] = st.LastRun.Format(time.RFC3339)
			resp["last_ok"] = st.LastOK
			resp["last_output"] = st.LastOutput
			resp["last_triggered_by"] = st.LastTriggeredBy
		}
	}
	return resp
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
