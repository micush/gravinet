package webadmin

// Config history: list/get/diff/restore/snapshot-now over the automatic and
// manual config snapshots kept by internal/config/history.go. Ported from
// parapet's own backups_get/backups_diff/backups_restore handlers
// (src/server.rs) — see that package's own doc comment for what adapted and
// why.

import (
	"encoding/json"
	"net/http"

	"gravinet/internal/config"
)

// handleHistoryList returns every snapshot's metadata, newest first.
func (s *Server) handleHistoryList(w http.ResponseWriter, r *http.Request) {
	items, err := config.List(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if items == nil {
		items = []config.SnapshotMeta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": items})
}

// handleHistoryGet returns one snapshot's full config, by id (or "current"
// for the live config).
func (s *Server) handleHistoryGet(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing 'id'"})
		return
	}
	current, err := config.Load(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	stamp, cfg, err := config.Get(s.configPath, id, current)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such snapshot"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "stamp": stamp, "config": cfg})
}

// handleHistoryDiff compares two snapshots (each side may be a snapshot id
// or "current"): a per-section semantic summary, plus both pretty-printed
// configs so the console can also render a line-level diff client-side.
func (s *Server) handleHistoryDiff(w http.ResponseWriter, r *http.Request) {
	aID := r.URL.Query().Get("a")
	bID := r.URL.Query().Get("b")
	if aID == "" || bID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "both 'a' and 'b' are required"})
		return
	}
	current, err := config.Load(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	aStamp, aCfg, aErr := config.Get(s.configPath, aID, current)
	bStamp, bCfg, bErr := config.Get(s.configPath, bID, current)
	if aErr != nil || bErr != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "one or both snapshots not found"})
		return
	}
	sections := config.FullSummary(aCfg, bCfg)
	sec := make([]map[string]any, len(sections))
	for i, d := range sections {
		sec[i] = map[string]any{"section": d.Section, "detail": d.Detail}
	}
	aJSON, _ := json.MarshalIndent(aCfg, "", "  ")
	bJSON, _ := json.MarshalIndent(bCfg, "", "  ")
	writeJSON(w, http.StatusOK, map[string]any{
		"a":        map[string]any{"id": aID, "stamp": aStamp},
		"b":        map[string]any{"id": bID, "stamp": bStamp},
		"sections": sec,
		"a_json":   string(aJSON),
		"b_json":   string(bJSON),
	})
}

// handleHistoryRestore rolls the live configuration back to a snapshot by
// id: loads it and runs it through the same validate/save/apply pipeline as
// any other change (restoreConfig), so the restore itself is committed,
// validated, and snapshotted — a bad restore can be rolled back the same
// way a bad edit can.
func (s *Server) handleHistoryRestore(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID string }
	if !decode(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing 'id'"})
		return
	}
	current, err := config.Load(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	_, candidate, err := config.Get(s.configPath, req.ID, current)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such snapshot"})
		return
	}
	if err := s.restoreConfig(r, candidate); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": true})
}

// handleHistorySnapshot takes a manual snapshot of the current
// configuration on demand, independent of any edit.
func (s *Server) handleHistorySnapshot(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	user, _ := s.validSession(r)
	id, err := config.SnapshotNow(s.configPath, cfg, user, cfg.EffectiveConfigHistoryLimit())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleConfigHistoryLimit sets how many config history snapshots are kept.
// Applied live — see ConfigHistoryLimitSet's doc comment for why this one,
// unlike TLS cert/login lockout, needs no restart.
func (s *Server) handleConfigHistoryLimit(w http.ResponseWriter, r *http.Request) {
	var req struct{ Limit int }
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(r, func(cfg *config.Config) error {
		return cfg.ConfigHistoryLimitSet(req.Limit)
	})
	s.editResult(w, err, false) // applied live; no restart
}
