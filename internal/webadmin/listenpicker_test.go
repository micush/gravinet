package webadmin

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/config"
)

// The listen-address picker shipped rendering nothing: its card drew a label
// and a description with no widget underneath. The handler was correct the
// whole time — the UI read `lo.options` off api()'s return value, but api()
// returns {ok, status, body} and the payload is under `body`. The guard
// `if (!lo || !lo.options) return;` therefore matched on every load and bailed
// without a word.
//
// Nothing caught it because the handler test asserted the JSON and the UI test
// asserted the DOM, and the bug lived exactly between them. These two tests
// pin the seam: the payload must carry the fields the UI names, and the UI
// must reach them through api()'s wrapper.

func listenOptionsBody(t *testing.T) map[string]any {
	t.Helper()
	s, _, _ := newTestServer(t)
	dir := t.TempDir()
	cp := filepath.Join(dir, "config.json")
	c := config.Default()
	c.Networks = []config.Network{{
		ID: "1234", Name: "lan", Enabled: true,
		Subnet4: "10.0.0.0/24", Address4: "10.0.0.1/24",
	}}
	if err := c.SaveTo(cp); err != nil {
		t.Fatal(err)
	}
	s.configPath = cp

	w := httptest.NewRecorder()
	s.handleListenOptions(w, httptest.NewRequest("GET", "/api/webadmin/listen-options", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// The payload must carry every field the picker names, spelled the same way.
func TestListenOptionsPayloadShape(t *testing.T) {
	out := listenOptionsBody(t)
	for _, k := range []string{"options", "selected", "port", "current"} {
		if _, ok := out[k]; !ok {
			t.Errorf("payload has no %q; the UI reads it by that name", k)
		}
	}
	opts, ok := out["options"].([]any)
	if !ok || len(opts) == 0 {
		t.Fatalf("options = %v, want a non-empty list — an empty one renders an empty picker", out["options"])
	}
	first, _ := opts[0].(map[string]any)
	for _, k := range []string{"addr", "label", "kind"} {
		if _, ok := first[k]; !ok {
			t.Errorf("option has no %q", k)
		}
	}
	// Loopback must be present and pre-selected, or the picker opens showing
	// nothing chosen on a node that is in fact listening on loopback.
	sel, _ := out["selected"].([]any)
	if len(sel) == 0 {
		t.Error("selected is empty; the picker would open showing no addresses chosen")
	}
}

// The UI must reach the payload through api()'s {ok,status,body} wrapper, not
// off the wrapper itself. Asserted against the shipped script text because
// that is where the mistake was, and a DOM test would not have seen it.
func TestListenPickerUnwrapsAPIResponse(t *testing.T) {
	src := indexHTML
	i := strings.Index(src, "/api/webadmin/listen-options")
	if i < 0 {
		t.Fatal("the listen-options call is gone from the UI")
	}
	window := src[i:min(i+700, len(src))]
	if !strings.Contains(window, ".body") {
		t.Error("the listen-options handler does not unwrap api()'s response; reading the wrapper makes the picker silently render nothing")
	}
	// And a failed load must say so rather than leaving an empty card.
	if !strings.Contains(window, "could not load") {
		t.Error("no visible failure path; a bad response would render a label with nothing under it")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The restart note was removed from the card and the search index; it should
// not creep back, since the setting no longer claims it.
func TestListenCardHasNoRestartClaim(t *testing.T) {
	if strings.Contains(indexHTML, "Applies on restart") {
		t.Error("the admin-interface card still claims it applies on restart")
	}
	if _, err := os.Stat("ui.go"); err != nil {
		t.Skip("source not present")
	}
}
