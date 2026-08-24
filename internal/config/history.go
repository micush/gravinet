package config

// Date-stamped configuration snapshots.
//
// Ported from parapet's src/backups.rs. Every committed config change writes
// a copy of the resulting configuration into a "history/" directory next to
// the live config file. Each snapshot is a small JSON envelope:
//
//	{
//	  "id":      "1719245412123",           // unix milliseconds, the stable key
//	  "ts":      1719245412,                // unix seconds
//	  "stamp":   "2026-06-24 15:30:12 UTC",  // human date stamp
//	  "user":    "alice",                   // who made the change ("" if unknown)
//	  "summary": "networks, settings",      // which sections changed
//	  "config":  { ... full config ... }
//	}
//
// Retention is FIFO: once the count exceeds the configured limit
// (Config.EffectiveConfigHistoryLimit), the oldest snapshots are deleted.
// Snapshots are deliberately stored *outside* the config object itself, so
// writing one is never itself a config change and can't recurse.
//
// gravinet's own edits are much more granular than parapet's (parapet
// replaces a whole section per PUT; gravinet has dozens of small handlers —
// SeedAdd, SeedRemove, SeedSetNotes are three separate commits for what's
// conceptually "editing one seed"), so a real editing session here produces
// more commits, and therefore more snapshots, than the same session would in
// parapet. That's a direct, faithful port of parapet's "snapshot every
// commit" behavior rather than a debounced approximation of it — the
// tradeoff is a noisier history for the same FIFO limit, not a different
// mechanism.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CurrentID is the id used to refer to the live (current) configuration in
// Get/diff, never a real snapshot file.
const CurrentID = "current"

// historyDir is the directory that holds snapshot files, derived from the
// config path.
func historyDir(configPath string) string {
	dir := filepath.Dir(configPath)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "history")
}

// Snapshot is one stored config snapshot, full envelope (used by Get).
type Snapshot struct {
	ID      string  `json:"id"`
	TS      int64   `json:"ts"`
	Stamp   string  `json:"stamp"`
	User    string  `json:"user"`
	Summary string  `json:"summary"`
	Config  *Config `json:"config"`
}

// SnapshotMeta is a snapshot's header fields only, no config body — what the
// list view needs, without ever parsing every snapshot's full config to
// build a table.
type SnapshotMeta struct {
	ID      string `json:"id"`
	TS      int64  `json:"ts"`
	Stamp   string `json:"stamp"`
	User    string `json:"user"`
	Summary string `json:"summary"`
}

// SnapshotNow takes a snapshot of the current configuration on demand (the
// "save the configuration now" button), independent of a commit/diff.
// Unlike OnCommit, this always writes — there is no "nothing changed" guard
// — because the operator explicitly asked to capture the present state.
// Returns the new snapshot id.
func SnapshotNow(configPath string, cfg *Config, user string, limit int) (string, error) {
	d := historyDir(configPath)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	id, err := writeOne(d, cfg, user, "manual snapshot")
	if err != nil {
		return "", err
	}
	prune(d, limit)
	return id, nil
}

// OnCommit reconciles snapshots after a committed change from before to
// after. Does nothing if the two configs are equivalent (by ChangedSections).
// The first time any change is tracked (no snapshots yet), the pre-change
// state is captured first as a baseline, so the history is complete and
// "restore the previous config" works from the very first edit. Then the new
// state is captured and the directory is pruned to limit newest snapshots.
// Best-effort throughout: never blocks or fails a commit on a snapshot
// problem, only logs would be appropriate at the call site.
func OnCommit(configPath string, before, after *Config, user string, limit int) {
	changed := ChangedSections(before, after)
	if len(changed) == 0 {
		return // nothing meaningful changed; don't clutter the history
	}
	d := historyDir(configPath)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return
	}
	ids, _ := listIDs(d)
	if len(ids) == 0 {
		_, _ = writeOne(d, before, "", "baseline (before first tracked change)")
	}
	summary := ""
	for i, c := range changed {
		if i > 0 {
			summary += ", "
		}
		summary += c
	}
	_, _ = writeOne(d, after, user, summary)
	prune(d, limit)
}

// ImportSnapshot files an externally-supplied configuration into the history
// as a snapshot, so a config downloaded from this or another node can be
// restored through the same reviewed path as any other entry: inspect it,
// diff it against what is running, then restore.
//
// It is deliberately only a snapshot. Importing does not apply anything —
// the running config is untouched until the operator restores the entry —
// which keeps "upload a file" from being a way to reconfigure a node in one
// unreviewed step, and makes the diff view the thing standing between a file
// and a live node.
//
// The caller validates before calling; this stores what it is given.
func ImportSnapshot(configPath string, cfg *Config, user, note string, limit int) (string, error) {
	d := historyDir(configPath)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	summary := "uploaded"
	if note = strings.TrimSpace(note); note != "" {
		summary += " \u00b7 " + note
	}
	id, err := writeOne(d, cfg, user, summary)
	if err != nil {
		return "", err
	}
	prune(d, limit)
	return id, nil
}

// nowMillis is unix milliseconds, the snapshot id source.
func nowMillis() int64 { return time.Now().UnixMilli() }

// writeOne writes a single snapshot file. Ids are unix-millis; if that id is
// already taken (two commits in the same millisecond), step forward until
// free so each snapshot keeps a unique, monotonically increasing key. Writes
// the envelope to a temp file first, then renames into place, so a reader
// never observes a partially-written snapshot; also writes the tiny .meta
// sidecar (see SnapshotMeta) so the list view stays cheap.
func writeOne(d string, cfg *Config, user, summary string) (string, error) {
	id := nowMillis()
	for {
		if _, err := os.Stat(snapshotFile(d, id, ".json")); os.IsNotExist(err) {
			break
		}
		id++
	}
	secs := id / 1000
	snap := Snapshot{
		ID:      strconv.FormatInt(id, 10),
		TS:      secs,
		Stamp:   fmtUTC(secs),
		User:    user,
		Summary: summary,
		Config:  cfg,
	}
	if err := writeJSONAtomic(snapshotFile(d, id, ".json"), snap); err != nil {
		return "", err
	}
	meta := SnapshotMeta{ID: snap.ID, TS: snap.TS, Stamp: snap.Stamp, User: snap.User, Summary: snap.Summary}
	_ = writeJSONAtomic(snapshotFile(d, id, ".meta"), meta)
	return snap.ID, nil
}

// writeJSONAtomic marshals v as pretty JSON and writes it to path via a
// temp-file-then-rename, so a crash or a concurrent reader never sees a
// half-written file.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// fmtUTC formats unix seconds as "YYYY-MM-DD HH:MM:SS UTC". Parapet hand-
// rolled this (Howard Hinnant's civil-from-days algorithm) since Rust's std
// has no format-string time formatter without an external crate; Go's time
// package already does this directly.
func fmtUTC(secs int64) string {
	return time.Unix(secs, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

// listIDs returns every snapshot id present in the directory, numerically
// ascending (oldest first) — ids are the file stems of *.json files that
// parse as integers.
func listIDs(d string) ([]int64, error) {
	entries, err := os.ReadDir(d)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []int64
	for _, e := range entries {
		name := e.Name()
		stem, ok := strings.CutSuffix(name, ".json")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(stem, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, n)
	}
	sortInt64s(ids)
	return ids, nil
}

func sortInt64s(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// prune deletes oldest snapshots until at most limit remain.
func prune(d string, limit int) {
	if limit < 1 {
		limit = 1
	}
	ids, _ := listIDs(d)
	if len(ids) <= limit {
		return
	}
	for _, id := range ids[:len(ids)-limit] {
		_ = os.Remove(snapshotFile(d, id, ".json"))
		_ = os.Remove(snapshotFile(d, id, ".meta"))
	}
}

// snapshotFile builds the path of one file in the history directory from a
// numeric snapshot id. Every per-id path in this package is built here, and
// the id is an int64 by the time it arrives, so the value reaching
// filepath.Join is digits and nothing else by construction.
func snapshotFile(d string, id int64, ext string) string {
	return filepath.Join(d, fmt.Sprintf("%d%s", id, ext))
}

// snapshotFileFor is the same, for an id that arrived as a string — from a
// query parameter, a request body, or a CLI argument.
//
// The id does not stay a string. It is parsed to an int64 and the path is
// rebuilt from the number, which is the same construction the writers have
// always used: writeOne and prune build their paths from an int64 with %d,
// and only the read side ever took the caller's text and concatenated it.
// Now neither does, so what reaches filepath.Join cannot contain a
// separator or a dot whatever the caller sent, and that is provable by
// reading these six lines rather than by tracing a validator through the
// call graph.
//
// The round trip through FormatInt is what makes the accepted set exactly
// the canonical ids: it turns away leading zeros, a leading plus, and
// anything that does not fit an int64. All of those used to be accepted by
// validID and then fail to name a file, so the outcome an operator sees —
// "no such snapshot" — is unchanged; only the reason is.
//
// This replaces validID, whose problem was not its strictness (digits-only
// is tighter than the check CodeQL's guidance asks for) but that it guarded
// one of the two functions here that build a path from an id, and nothing
// obliged the other to call it.
func snapshotFileFor(d, id, ext string) (string, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != id {
		return "", fmt.Errorf("invalid snapshot id %q", id)
	}
	return snapshotFile(d, n, ext), nil
}

// Count is how many config snapshots currently exist.
func Count(configPath string) int {
	ids, _ := listIDs(historyDir(configPath))
	return len(ids)
}

// List returns every snapshot's metadata, newest first — the payload the
// console's history table renders. Reads only the .meta sidecars, never a
// snapshot's (potentially large) config body.
func List(configPath string) ([]SnapshotMeta, error) {
	d := historyDir(configPath)
	ids, err := listIDs(d)
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotMeta, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- { // newest first
		id := strconv.FormatInt(ids[i], 10)
		if m, ok := readMeta(d, id); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// readMeta reads a snapshot's metadata for the list view: the .meta sidecar
// if present, falling back to a full parse of the envelope (and then writing
// the sidecar, so subsequent loads are cheap) if it's missing for any reason
// — a snapshot is never silently dropped from the list just because its
// sidecar didn't get written.
// readMeta reads a snapshot's metadata for the list view: the .meta sidecar
// if present, falling back to a full parse of the envelope (and then writing
// the sidecar, so subsequent loads are cheap) if it's missing for any reason
// — a snapshot is never silently dropped from the list just because its
// sidecar didn't get written.
//
// The sidecar path is built the same way the envelope path is. It was not:
// readEnvelope below validated its id and this function did not, so
// readMeta(d, "../../secret") read a .meta outside the history directory and
// returned what it found. Only List calls this, and List passes ids it
// generated itself, so nothing reached it — but "safe because of who calls
// it" is the property that stops holding when someone adds a caller, and the
// sidecar write two lines down would have made that a write primitive rather
// than a read one.
func readMeta(d, id string) (SnapshotMeta, bool) {
	var m SnapshotMeta
	path, err := snapshotFileFor(d, id, ".meta")
	if err != nil {
		return m, false
	}
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, &m) == nil {
			return m, true
		}
	}
	env, ok := readEnvelope(d, id)
	if !ok {
		return m, false
	}
	m = SnapshotMeta{ID: env.ID, TS: env.TS, Stamp: env.Stamp, User: env.User, Summary: env.Summary}
	_ = writeJSONAtomic(path, m)
	return m, true
}

// rawEnvelope mirrors Snapshot but keeps the config body as raw JSON, so it
// can be unmarshaled starting from Default() — exactly like Load() does —
// rather than a bare zero-valued Config. Necessary because some fields have
// omitempty with a non-zero default (e.g. web_admin.listen defaults to
// "127.0.0.1:8443"); a value saved *at* that default is omitted from the
// JSON, and unmarshaling straight into a zero-valued struct would silently
// turn it into "" instead of the real default. Snapshot itself (with a
// concrete *Config) is still what's used for *writing* — marshaling a fully
// resolved Config is exactly what SaveTo already does, no special handling
// needed on that side.
type rawEnvelope struct {
	ID      string          `json:"id"`
	TS      int64           `json:"ts"`
	Stamp   string          `json:"stamp"`
	User    string          `json:"user"`
	Summary string          `json:"summary"`
	Config  json.RawMessage `json:"config"`
}

// readEnvelope loads and parses one snapshot envelope by id, resolving its
// config the same Default()-then-Unmarshal way Load() resolves the live
// config file.
func readEnvelope(d, id string) (Snapshot, bool) {
	var s Snapshot
	path, err := snapshotFileFor(d, id, ".json")
	if err != nil {
		return s, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s, false
	}
	var raw rawEnvelope
	if json.Unmarshal(data, &raw) != nil {
		return s, false
	}
	cfg := Default()
	if len(raw.Config) > 0 {
		if json.Unmarshal(raw.Config, cfg) != nil {
			return s, false
		}
	}
	s = Snapshot{ID: raw.ID, TS: raw.TS, Stamp: raw.Stamp, User: raw.User, Summary: raw.Summary, Config: cfg}
	return s, true
}

// Get returns the full config for a snapshot id, or for the live config when
// id is CurrentID.
func Get(configPath, id string, current *Config) (stamp string, cfg *Config, err error) {
	if id == CurrentID {
		clone, cerr := current.Clone()
		if cerr != nil {
			return "", nil, cerr
		}
		return "current (live)", clone, nil
	}
	env, ok := readEnvelope(historyDir(configPath), id)
	if !ok || env.Config == nil {
		return "", nil, fmt.Errorf("no such snapshot")
	}
	return env.Stamp, env.Config, nil
}
