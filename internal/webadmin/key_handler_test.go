package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/crypto"
)

// TestKeyDistribute exercises the /api/key "distribute" op: it should push an
// already-enabled key straight to the engine's FloodKey (no config write, no
// restart), refuse a disabled or empty slot before ever calling the engine,
// and surface a backend error rather than swallowing it.
func TestKeyDistribute(t *testing.T) {
	enabledKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	disabledKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		Networks: []config.Network{{
			ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Keys: [8]config.KeySlot{
				{Key: enabledKey, Label: "current", Enabled: true},
				{Key: disabledKey, Label: "retired", Enabled: false},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}

	srv, be, ts := newTestServer(t)
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	c := sessionFor(t, ts)

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/key", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	// Distributing the enabled key succeeds, calls through to the engine's
	// FloodKey exactly once, and reports the (stubbed) connected-peer count.
	out := post(map[string]any{"op": "distribute", "net": "1234", "slot": 0})
	if out["error"] != nil {
		t.Fatalf("distribute errored: %v", out["error"])
	}
	if be.floodKeyCalls != 1 {
		t.Fatalf("FloodKey calls = %d, want 1", be.floodKeyCalls)
	}
	if _, ok := out["peers"]; !ok {
		t.Fatal("expected a peers count in the response")
	}
	// The Distributed flag is persisted, so the checkbox stays ticked across
	// a page reload — read it back from disk rather than trusting the response.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Networks[0].Keys[0].Distributed {
		t.Fatal("slot 0 should be persisted as Distributed after a successful distribute")
	}

	// Distributing a disabled key is refused, and never reaches the engine.
	out = post(map[string]any{"op": "distribute", "net": "1234", "slot": 1})
	if out["error"] == nil {
		t.Fatal("expected an error distributing a disabled key")
	}
	if be.floodKeyCalls != 1 {
		t.Fatalf("FloodKey calls = %d after refused distribute, want still 1", be.floodKeyCalls)
	}

	// Distributing an empty slot is refused.
	out = post(map[string]any{"op": "distribute", "net": "1234", "slot": 2})
	if out["error"] == nil {
		t.Fatal("expected an error distributing an empty slot")
	}
	if be.floodKeyCalls != 1 {
		t.Fatalf("FloodKey calls = %d after empty-slot distribute, want still 1", be.floodKeyCalls)
	}

	// A backend failure surfaces as an error and doesn't crash the handler.
	be.floodKeyErr = errBoom
	out = post(map[string]any{"op": "distribute", "net": "1234", "slot": 0})
	if out["error"] == nil {
		t.Fatal("expected the backend's FloodKey error to surface")
	}
}

type keyTestError string

func (e keyTestError) Error() string { return string(e) }

const errBoom = keyTestError("boom")

// TestKeyUndistribute exercises the /api/key "undistribute" op: it should call
// the engine's RetractKey and clear the slot's persisted Distributed flag, and
// never touch the key material itself (the slot still has its key afterward).
// TestKeyUndistribute proves unticking "Distributed" only detaches this node
// from managing the key together with peers — it must NOT retract the key
// from anyone. Retraction is delete's job (see TestKeyDeleteRetractsDistributed).
func TestKeyUndistribute(t *testing.T) {
	keyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		Networks: []config.Network{{
			ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Keys: [8]config.KeySlot{
				{Key: keyB64, Label: "shared", Enabled: true, Distributed: true},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}

	srv, be, ts := newTestServer(t)
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	c := sessionFor(t, ts)

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/key", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	out := post(map[string]any{"op": "undistribute", "net": "1234", "slot": 0})
	if out["error"] != nil {
		t.Fatalf("undistribute errored: %v", out["error"])
	}
	if be.retractKeyCalls != 0 {
		t.Fatalf("RetractKey calls = %d, want 0 — unticking Distributed must not retract the key from peers", be.retractKeyCalls)
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	slot := reloaded.Networks[0].Keys[0]
	if slot.Distributed {
		t.Fatal("Distributed should be cleared after undistribute")
	}
	if slot.Key != keyB64 {
		t.Fatal("undistribute must not remove this node's own copy of the key")
	}
}

// TestKeyDeleteRetractsDistributed proves deleting a key that's currently
// Distributed retracts it from every peer holding a copy — the behavior
// TestKeyUndistribute above proves untick deliberately does NOT do. Deleting
// an ordinary (non-distributed) key must not call RetractKey at all.
func TestKeyDeleteRetractsDistributed(t *testing.T) {
	distKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	plainKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	thirdKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		Networks: []config.Network{{
			ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Keys: [8]config.KeySlot{
				{Key: distKeyB64, Label: "shared", Enabled: true, Distributed: true},
				{Key: plainKeyB64, Label: "local-only", Enabled: true},
				{Key: thirdKeyB64, Label: "spare", Enabled: true},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}

	srv, be, ts := newTestServer(t)
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	c := sessionFor(t, ts)

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/key", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	// Deleting the non-distributed slot must not retract anything.
	out := post(map[string]any{"op": "delete", "net": "1234", "slot": 1})
	if out["error"] != nil {
		t.Fatalf("delete errored: %v", out["error"])
	}
	if be.retractKeyCalls != 0 {
		t.Fatalf("RetractKey calls after deleting a non-distributed slot = %d, want 0", be.retractKeyCalls)
	}

	// Deleting the distributed slot must retract it from peers.
	out = post(map[string]any{"op": "delete", "net": "1234", "slot": 0})
	if out["error"] != nil {
		t.Fatalf("delete errored: %v", out["error"])
	}
	if be.retractKeyCalls != 1 {
		t.Fatalf("RetractKey calls after deleting a distributed slot = %d, want 1", be.retractKeyCalls)
	}
	if be.lastRetractKeyB64 != distKeyB64 {
		t.Fatalf("retracted key = %q, want the deleted slot's own key %q", be.lastRetractKeyB64, distKeyB64)
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Networks[0].Keys[0].Key != "" {
		t.Fatal("slot 0 should actually be deleted from this node's own config too")
	}
}

// TestKeyLabelPropagatesToDistributedPeers proves relabeling a slot that's
// currently Distributed pushes the new label out via FloodKey, while
// relabeling an ordinary (non-distributed) slot does not call it at all.
func TestKeyLabelPropagatesToDistributedPeers(t *testing.T) {
	distKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	plainKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		Networks: []config.Network{{
			ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Keys: [8]config.KeySlot{
				{Key: distKeyB64, Label: "old-name", Enabled: true, Distributed: true},
				{Key: plainKeyB64, Label: "local-only", Enabled: true},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}

	srv, be, ts := newTestServer(t)
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	c := sessionFor(t, ts)

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/key", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	// Relabeling the distributed slot propagates via FloodKey.
	out := post(map[string]any{"op": "label", "net": "1234", "slot": 0, "label": "new-name"})
	if out["error"] != nil {
		t.Fatalf("label errored: %v", out["error"])
	}
	if be.floodKeyCalls != 1 {
		t.Fatalf("FloodKey calls after relabeling a distributed slot = %d, want 1", be.floodKeyCalls)
	}

	// Relabeling the plain (non-distributed) slot does not.
	out = post(map[string]any{"op": "label", "net": "1234", "slot": 1, "label": "renamed-local"})
	if out["error"] != nil {
		t.Fatalf("label errored: %v", out["error"])
	}
	if be.floodKeyCalls != 1 {
		t.Fatalf("FloodKey calls after relabeling a non-distributed slot = %d, want still 1", be.floodKeyCalls)
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Networks[0].Keys[0].Label != "new-name" {
		t.Fatalf("slot 0 label = %q, want %q", reloaded.Networks[0].Keys[0].Label, "new-name")
	}
	if reloaded.Networks[0].Keys[1].Label != "renamed-local" {
		t.Fatalf("slot 1 label = %q, want %q", reloaded.Networks[0].Keys[1].Label, "renamed-local")
	}
}

// TestKeyExpiryPropagatesToDistributedPeers is the same regression class as
// TestKeyLabelPropagatesToDistributedPeers, for the expiry field: a peer that
// already has a copy of a distributed key only learns about a new expiry (or
// a cleared one) if it's re-flooded — the expiry lives in the same
// encodeKeyAdd payload FloodKey sends, so without re-flooding, every other
// node keeps trusting the key on whatever expiry it had when it was first
// distributed, silently outliving what the operator just set on the origin
// node. Also confirms the flood carries the key's *current* label, not an
// empty one — the label and expiry ops share the same post-switch flood
// call, and the expiry request never carries a label of its own.
func TestKeyExpiryPropagatesToDistributedPeers(t *testing.T) {
	distKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	plainKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		Networks: []config.Network{{
			ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Keys: [8]config.KeySlot{
				{Key: distKeyB64, Label: "shared-key", Enabled: true, Distributed: true},
				{Key: plainKeyB64, Label: "local-only", Enabled: true},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}

	srv, be, ts := newTestServer(t)
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	c := sessionFor(t, ts)

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/key", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	// Setting an expiry on the distributed slot propagates via FloodKey, and
	// carries the slot's existing label along (not blank).
	future := "2027-01-01T00:00:00Z"
	out := post(map[string]any{"op": "expiry", "net": "1234", "slot": 0, "expires": future})
	if out["error"] != nil {
		t.Fatalf("expiry errored: %v", out["error"])
	}
	if be.floodKeyCalls != 1 {
		t.Fatalf("FloodKey calls after setting expiry on a distributed slot = %d, want 1", be.floodKeyCalls)
	}
	if be.lastFloodLabel != "shared-key" {
		t.Fatalf("flooded label = %q, want the slot's existing label %q (not blank)", be.lastFloodLabel, "shared-key")
	}
	wantNano, _ := time.Parse(time.RFC3339, future)
	if be.lastFloodExpNs != wantNano.UnixNano() {
		t.Fatalf("flooded expiry = %d, want %d (%s)", be.lastFloodExpNs, wantNano.UnixNano(), future)
	}

	// Setting an expiry on the plain (non-distributed) slot does not flood.
	out = post(map[string]any{"op": "expiry", "net": "1234", "slot": 1, "expires": future})
	if out["error"] != nil {
		t.Fatalf("expiry errored: %v", out["error"])
	}
	if be.floodKeyCalls != 1 {
		t.Fatalf("FloodKey calls after setting expiry on a non-distributed slot = %d, want still 1", be.floodKeyCalls)
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Networks[0].Keys[0].Expires != future {
		t.Fatalf("slot 0 expiry = %q, want %q", reloaded.Networks[0].Keys[0].Expires, future)
	}
}
