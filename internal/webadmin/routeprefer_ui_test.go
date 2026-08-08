package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// The Preferred peers card posts these four ops. A drag that saves an order the
// backend does not accept would fail silently from the user's side except for
// an alert, so the wiring is worth asserting directly.
func TestHandleRoutePreferOps(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		UDPPorts: []int{65432}, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{
			ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
			Routes: []config.Route{{CIDR: "0.0.0.0/0", Enabled: true}},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	ts, c := preferTestServer(t, cfgPath)
	defer ts.Close()

	post := func(body map[string]any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/route", bytes.NewReader(b))
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
	reload := func() *config.Network {
		got, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return &got.Networks[0]
	}

	// The drag handler's payload shape: whole ordered list, every time.
	if ok, _ := post(map[string]any{"op": "prefer", "net": "lan", "cidr": "0.0.0.0/0",
		"origins": []string{"nodeC", "nodeB"}})["ok"].(bool); !ok {
		t.Fatal("prefer rejected")
	}
	n := reload()
	if len(n.RoutePrefer) != 1 || strings.Join(n.RoutePrefer[0].Origins, ",") != "nodeC,nodeB" {
		t.Fatalf("saved order = %+v, want [nodeC nodeB]", n.RoutePrefer)
	}

	// Reordering replaces rather than appends — a drag sends the full list.
	if ok, _ := post(map[string]any{"op": "prefer", "net": "lan", "cidr": "0.0.0.0/0",
		"origins": []string{"nodeB", "nodeC"}})["ok"].(bool); !ok {
		t.Fatal("reorder rejected")
	}
	if got := strings.Join(reload().RoutePrefer[0].Origins, ","); got != "nodeB,nodeC" {
		t.Fatalf("after reorder = %q, want nodeB,nodeC", got)
	}

	if ok, _ := post(map[string]any{"op": "prefer-disable", "net": "lan", "cidr": "0.0.0.0/0"})["ok"].(bool); !ok {
		t.Fatal("prefer-disable rejected")
	}
	n = reload()
	if !n.RoutePrefer[0].Disabled {
		t.Fatal("prefer-disable did not disable the entry")
	}
	if len(n.RoutePrefer[0].Origins) != 2 {
		t.Fatal("disabling lost the order; the card re-enables in place and expects it intact")
	}

	if ok, _ := post(map[string]any{"op": "prefer-enable", "net": "lan", "cidr": "0.0.0.0/0"})["ok"].(bool); !ok {
		t.Fatal("prefer-enable rejected")
	}
	if reload().RoutePrefer[0].Disabled {
		t.Fatal("prefer-enable did not re-enable")
	}

	if ok, _ := post(map[string]any{"op": "prefer-remove", "net": "lan", "cidr": "0.0.0.0/0"})["ok"].(bool); !ok {
		t.Fatal("prefer-remove rejected")
	}
	if len(reload().RoutePrefer) != 0 {
		t.Fatal("prefer-remove left the entry behind")
	}
}

// A duplicate origin is rejected by the ops layer; the card can produce one only
// through a bug, and the failure should be an error rather than a silently
// reordered list.
func TestHandleRoutePreferRejectsDuplicate(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		UDPPorts: []int{65432}, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	ts, c := preferTestServer(t, cfgPath)
	defer ts.Close()
	b, _ := json.Marshal(map[string]any{"op": "prefer", "net": "lan", "cidr": "0.0.0.0/0",
		"origins": []string{"nodeA", "nodeA"}})
	req, _ := http.NewRequest("POST", ts.URL+"/api/route", bytes.NewReader(b))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if ok, _ := out["ok"].(bool); ok {
		t.Fatal("duplicate origin accepted")
	}
}

// The card is built with the $() helper, which parses its argument as innerHTML
// on a plain <div> — so a bare <li> would be dropped by the parser and the very
// next appendChild would throw, aborting the whole Routes render. Every list row
// must therefore be constructed inside its own <ul>, never as a bare <li>.
func TestPreferredPeersBuildsNoBareListItems(t *testing.T) {
	src := indexHTML
	start := strings.Index(src, "function buildPreferredPeers(")
	if start < 0 {
		t.Fatal("buildPreferredPeers not found in the embedded UI")
	}
	end := strings.Index(src[start:], "\nfunction wirePreferDrag(")
	if end < 0 {
		t.Fatal("could not bound buildPreferredPeers")
	}
	body := src[start : start+end]
	for _, bad := range []string{"$('<li", "$(\"<li", "$('<tr", "$('<td"} {
		if strings.Contains(body, bad) {
			t.Errorf("buildPreferredPeers constructs %s directly; the $() helper drops it and the render aborts", bad)
		}
	}
	if !strings.Contains(body, `<ul class="pref-list">`) {
		t.Error("the list container is not a <ul>; .pref-item rows need a real list parent")
	}
	if !strings.Contains(body, "listWrap.innerHTML") {
		t.Error("rows are not parsed inside their own <ul>; see ui_dom_helper_test.go")
	}
}

func preferTestServer(t *testing.T, cfgPath string) (*httptest.Server, *http.Cookie) {
	t.Helper()
	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	srv := New(wcfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	return ts, sessionFor(t, ts)
}

// prefSource returns buildPreferredPeers + buildPreferBlock from the embedded
// UI, bounded so a match in unrelated code cannot make these assertions pass.
func prefSource(t *testing.T) string {
	t.Helper()
	start := strings.Index(indexHTML, "function buildPreferredPeers(")
	if start < 0 {
		t.Fatal("buildPreferredPeers not found in the embedded UI")
	}
	end := strings.Index(indexHTML[start:], "\nfunction wirePreferDrag(")
	if end < 0 {
		t.Fatal("could not bound the Preferred peers source")
	}
	return indexHTML[start : start+end]
}

// A mesh can carry thousands of routes, so the card cannot render a block per
// contested prefix. Two earlier shapes failed in opposite directions: rendering
// everything (unusable, and tens of thousands of DOM nodes on a page that
// re-renders every status poll), then rendering only what was already
// configured (bounded, but every unconfigured route unreachable — you cannot
// filter for a route you have no way of knowing exists).
//
// The search-to-add picker is what resolves both: every contested prefix is
// findable by typing, and only picked routes get a draggable list.
func TestPreferredPeersUsesRoutePicker(t *testing.T) {
	src := prefSource(t)
	if !strings.Contains(src, "buildRouteChipPicker(pickable, configured,") {
		t.Error("the card does not use the shared route picker; routes are either all rendered or undiscoverable")
	}
	// Blocks are built for picked routes only.
	if !strings.Contains(src, "const chosen = picker ? picker.get() : configured") {
		t.Error("the rendered blocks are not driven by the picker selection")
	}
	// A configured route must arrive already selected, or the card would open
	// showing none of the decisions currently in force.
	if !strings.Contains(src, "const configured = [...saved.keys()]") {
		t.Error("configured prefixes are not preselected in the picker")
	}
	// Unpicking must clear the stored preference, or a setting stays in force
	// with nothing on screen representing it.
	if !strings.Contains(src, "op:'prefer-remove'") {
		t.Error("deselecting a route does not clear its preference")
	}
}

// A configured route whose advertisers have dropped below two must still be
// reachable — otherwise the setting representing it becomes uneditable at
// exactly the moment an operator would want to look at it.
func TestPreferredPeersKeepsConfiguredRoutesPickable(t *testing.T) {
	src := prefSource(t)
	if !strings.Contains(src, "for (const c of configured) if (!pickable.includes(c)) pickable.push(c)") {
		t.Error("configured prefixes are dropped from the pickable set when uncontested")
	}
}

// Preparing the data must stay linear in the number of learned routes. An inner
// peers.find() per row makes the render quadratic once a mesh has both many
// peers and many routes, which is exactly the case this card is for.
func TestPreferredPeersAvoidsQuadraticLookups(t *testing.T) {
	src := prefSource(t)
	if !strings.Contains(src, "const labels = new Map()") {
		t.Error("peer labels are not precomputed into a map")
	}
	// No linear scan may run inside a row loop. The only .find() allowed here
	// is the one-per-card lookup of this network's status entry.
	for _, bad := range []string{"advertisers.find(", "(status.peers||[]).find("} {
		if strings.Contains(src, bad) {
			t.Errorf("%s runs per row; build a map once instead", bad)
		}
	}
	if !strings.Contains(src, "const metricOf = new Map(") {
		t.Error("per-row metric lookup is not precomputed")
	}
}

// Single-advertiser prefixes are the overwhelming majority and can never be
// ordered. Including them would bury the rows that matter.
func TestPreferredPeersSkipsUncontestedPrefixes(t *testing.T) {
	src := prefSource(t)
	if !strings.Contains(src, "if (l.length > 1) contested.push(cidr)") {
		t.Error("prefixes with one advertiser are not excluded from the contested set")
	}
}

// Every contested prefix must be in the picker's pool, so all of them are
// reachable by typing. This is the property the two earlier layouts lacked:
// one rendered everything and did not scale, the other scaled but left
// unconfigured routes with no path to them at all.
func TestPreferredPeersOffersEveryContestedRoute(t *testing.T) {
	src := prefSource(t)
	if !strings.Contains(src, "const pickable = contested.slice()") {
		t.Error("the picker pool is not seeded from the full contested set")
	}
	if !strings.Contains(src, "if (l.length > 1) contested.push(cidr)") {
		t.Error("the contested set is not built from every multi-advertiser prefix")
	}
}

// The enabled/disabled badge must be a pill, matching every other card header.
// .tag-toggle alone only supplies the cursor and hover affordance — .pill is
// what draws the rounded badge — so dropping it renders the state as bare
// coloured text that reads nothing like the rest of the UI.
func TestPreferredPeersStateBadgeIsAPill(t *testing.T) {
	src := prefSource(t)
	if !strings.Contains(src, `class="pill tag-toggle `) {
		t.Error("the preference state badge is not a pill; it will render as bare text unlike every other card")
	}
	if strings.Contains(src, `class="tag-toggle `) {
		t.Error("a bare tag-toggle remains; the block head takes the pill form used by card headers")
	}
}

// Rows read by hostname. The node id is what the API is keyed on and what gets
// saved, but repeating a 16-char hex string on every row crowds out the name it
// sits beside. It stays reachable on hover so a row can still be matched
// against config or a log line.
func TestPreferredPeersLabelsByHostname(t *testing.T) {
	src := prefSource(t)
	if strings.Contains(src, `h + ' \u00b7 ' + id`) {
		t.Error("the node id is still concatenated into the row label")
	}
	if !strings.Contains(src, "labels.set(id, h || id)") {
		t.Error("labels are not hostname-with-id-fallback")
	}
	if !strings.Contains(src, "const rowTitle =") {
		t.Error("the node id is not preserved anywhere; dropping it from the row makes it undiscoverable")
	}
}
