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

// buildRouteChipPicker returns a handle ({wrap,get,set,setAvailable}), not a
// DOM node. Passing the handle straight to appendChild throws a TypeError, and
// in the listen-address card that throw was caught by the promise's own
// .catch(), so a perfectly good response rendered "could not load addresses" —
// indistinguishable from a dead endpoint.
//
// Checked across every call site rather than just this one: the picker is
// shared by the BGP redistribute rows, Mesh Routes, L2 Disco and the upgrade
// push selector, and the mistake is equally available to all of them.
func TestChipPickerCallSitesAppendWrap(t *testing.T) {
	const call = "buildRouteChipPicker("
	src := indexHTML
	found := 0
	for i := 0; ; {
		j := strings.Index(src[i:], call)
		if j < 0 {
			break
		}
		at := i + j
		i = at + len(call)
		// Skip the definition itself.
		if strings.HasSuffix(src[:at], "function ") {
			continue
		}
		found++
		// appendChild(buildRouteChipPicker(...)) hands the handle to the DOM.
		if strings.HasSuffix(src[:at], "appendChild(") {
			t.Errorf("call site at offset %d appends buildRouteChipPicker's handle directly; "+
				"append its .wrap instead, or the picker never renders", at)
		}
	}
	if found == 0 {
		t.Fatal("no buildRouteChipPicker call sites found; this test is no longer checking anything")
	}
}

// The listen-address card in particular must reach .wrap, and its failure path
// must not swallow the reason — the render throw above was invisible precisely
// because the catch discarded it.
func TestListenPickerRendersAndReportsFailures(t *testing.T) {
	src := indexHTML
	i := strings.Index(src, "/api/webadmin/listen-options")
	if i < 0 {
		t.Fatal("the listen-options call is gone from the UI")
	}
	window := src[i:min(i+2600, len(src))]
	if !strings.Contains(window, ".wrap") {
		t.Error("the listen-address card never appends the picker's .wrap; the card renders a label with no picker under it")
	}
	if !strings.Contains(window, "catch(e") && !strings.Contains(window, "catch(err") {
		t.Error("the listen-options catch discards its error; a render bug in this block is then indistinguishable from a failed fetch")
	}
}

// The picker belongs under the label and description, not in a column beside
// them. The stacked layout is a stylesheet rule keyed on :has(.route-picker),
// which cannot match before the picker is in the DOM — so this row, whose
// picker is built only once its fetch resolves, opts in explicitly. Without
// that it renders side-by-side until the response lands, and permanently
// side-by-side when the response fails, putting the error text off to the
// right of the label.
func TestListenRowStacksLayout(t *testing.T) {
	src := indexHTML
	i := strings.Index(src, `id="listen-addrs-row"`)
	if i < 0 {
		t.Fatal("the listen-addrs row is gone from the UI")
	}
	// The row's own opening tag.
	open := src[strings.LastIndex(src[:i], "<div"):]
	open = open[:strings.Index(open, ">")+1]

	if !strings.Contains(open, "settings-row stacked") {
		t.Errorf("listen-addrs row is not stacked: %s\n"+
			"without it the picker sits beside the label, and a failed load puts the error there too", open)
	}
	// An inline align-items on the row beats the stacked rule's own stretch,
	// which is how this row lost the layout the first time.
	if strings.Contains(open, "align-items") {
		t.Errorf("listen-addrs row sets align-items inline: %s\n"+
			"that overrides the stacked rule's stretch", open)
	}
	// flex:1/min-width on the picker's container is right-hand-column sizing
	// and means nothing once the row is a column.
	if j := strings.Index(src[i:], "api('/api/webadmin/listen-options')"); j > 0 {
		if strings.Contains(src[i:i+j], "flex:1") {
			t.Error("the picker's container still carries flex:1 column sizing")
		}
	}
}

// The stacked rule must actually cover the class the row asks for, alongside
// the :has() form the synchronously-built pickers rely on.
func TestStackedSettingsRowRuleExists(t *testing.T) {
	src := indexHTML
	i := strings.Index(src, ".settings-row.stacked")
	if i < 0 {
		t.Fatal("no .settings-row.stacked rule; the listen-addrs row asks for a class nothing styles")
	}
	rule := src[i:min(i+220, len(src))]
	rule = rule[:strings.Index(rule, "}")+1]
	for _, decl := range []string{"flex-direction:column", "align-items:stretch"} {
		if !strings.Contains(rule, decl) {
			t.Errorf("the stacked rule has no %s: %s", decl, rule)
		}
	}
	// The original :has() form must survive — the BGP, Mesh Routes and L2
	// Disco pickers get their layout from it and never set the class.
	if !strings.Contains(src, ":has(.route-picker)") {
		t.Error(":has(.route-picker) is gone; the other pickers lose their stacked layout")
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
