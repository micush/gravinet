package webadmin

import (
	"fmt"
	"net/http"
	"slices"

	"gravinet/internal/config"
)

// logLevels are the accepted values, in increasing verbosity.
var logLevels = []string{"error", "warn", "info", "debug"}

// handleLogLevel reports and sets the daemon's log level.
//
// This exists because the level was previously reachable only by editing
// config.json and restarting — and a restart is the one thing you cannot afford
// while investigating most faults worth raising the level for. Restarting resets
// every session, backoff timer, and learned endpoint, so any bug that only
// reproduces on a live network event (a roam, a peer flapping, a fallback that
// won't establish) is destroyed by the act of preparing to observe it. Worse,
// nearly every *rejection* path in the mesh logs at Debug — a replayed
// handshake, a clock-skew mismatch, a handshake that claims our own node id, a
// failed TLS dial. At the default Info level a node that is receiving handshakes
// and refusing all of them is indistinguishable, in the log, from a node
// receiving nothing at all. Those are opposite bugs, and telling them apart
// began with "edit this JSON file and restart".
//
// mutateConfig's reload applies the level live (see reloadFn), so this takes
// effect on the next request with no restart and no loss of mesh state.
func (s *Server) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{
			"level":  s.be.LogLevel(),
			"levels": logLevels,
		})
		return
	}
	var req struct {
		Level string `json:"level"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !slices.Contains(logLevels, req.Level) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("unknown log level %q; want one of %v", req.Level, logLevels),
		})
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.LogLevel = req.Level
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "level": req.Level, "restart": false})
}
