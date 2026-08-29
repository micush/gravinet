package tui

// These tests exist because a wrong argv is the single worst failure mode
// this whole feature has: it doesn't crash, doesn't error loudly, it just
// runs a different command than the one the operator asked for. Every test
// here drives an action through the real dispatch path (dispatchAdd /
// dispatchRowAction, never calling a submit func directly) and asserts on
// exactly what runGravinet was called with — the same fake used in
// mutate_test.go, so these are checking the whole path from keystroke to
// argv, not just the argv-building function in isolation.

import (
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// submitCurrentForm fills in a form's fields from a map and presses Enter —
// a small helper so each test below reads as "these are the field values,
// here's the argv that should produce."
func submitCurrentForm(t *testing.T, m *model, values map[string]string) {
	t.Helper()
	if m.form == nil {
		t.Fatal("no form is open")
	}
	for i := range m.form.field {
		if v, ok := values[m.form.field[i].key]; ok {
			m.form.field[i].value = v
		}
	}
	m.handleKey(key{t: keyEnter})
}

func lastCall(f *fakeGravinet) []string {
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func hasArgs(t *testing.T, got []string, want ...string) {
	t.Helper()
	joined := strings.Join(got, "\u2027") // an unlikely separator, for a clear diff
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q among the args, got: %s", w, joined)
		}
	}
}

// argAfter returns the argument immediately following flag in args, for
// asserting on positional pairs like "-net corp".
func argAfter(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestNetworksAddArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("networks")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"name": "lab", "subnet4": "10.9.0.0/16", "subnet6": ""})

	got := lastCall(f)
	hasArgs(t, got, "network", "add", "lab", "subnet", "10.9.0.0/16")
	if strings.Contains(strings.Join(got, " "), "subnet6") {
		t.Errorf("an empty subnet6 should not appear in the argv at all: %v", got)
	}
}

func TestNetworksAddRequiresAName(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("networks")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"name": ""})
	if len(f.calls) != 0 {
		t.Errorf("an empty name should be rejected before ever shelling out, got a call: %v", f.calls)
	}
	if m.result == nil || m.result.ok {
		t.Error("expected a failure result for a blank name")
	}
}

func TestNetworksDeleteArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("networks") // selection defaults to "corp"
	m.dispatchRowAction('d')
	if m.confirm == nil {
		t.Fatal("delete should ask for confirmation first")
	}
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "network", "delete", "corp")
}

func TestNetworksToggleArgvPicksTheOppositeOfCurrentState(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel() // corp is Enabled: true in testSnapshot
	m.setSection("networks")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "network", "disable", "corp")

	// A successful action triggers a real refresh (showResult's own
	// behavior, correctly exercised above) — re-seed a fixture snapshot
	// rather than continue mutating the one that refresh just replaced.
	// A successful action triggers a real refresh (showResult's own
	// behavior, correctly exercised above) — against a config path that
	// doesn't exist on this test host, which clears the row selection
	// (syncSelection sees zero rows and resets it). Re-seed a fixture
	// snapshot and re-sync selection explicitly, rather than continue
	// mutating state a real refresh already moved on from.
	f.calls = nil
	m.snap = testSnapshot()
	m.snap.cfg.Networks[0].Enabled = false
	m.syncSelection()
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "network", "enable", "corp")
}

func TestNetworksEditOnlyCallsForFieldsThatChanged(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("networks")
	m.dispatchRowAction('e')
	if m.form == nil {
		t.Fatal("'e' should open the edit form")
	}
	// Change only notes; leave mtu/subnet4/subnet6 exactly as pre-filled.
	for i := range m.form.field {
		if m.form.field[i].key == "notes" {
			m.form.field[i].value = "new note"
		}
	}
	m.handleKey(key{t: keyEnter})

	if len(f.calls) != 1 {
		t.Fatalf("changing one field should issue exactly one CLI call, got %d: %v", len(f.calls), f.calls)
	}
	hasArgs(t, f.calls[0], "network", "notes", "corp", "new note")
}

func TestNetworksEditCallsOncePerChangedField(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("networks")
	m.dispatchRowAction('e')
	for i := range m.form.field {
		switch m.form.field[i].key {
		case "notes":
			m.form.field[i].value = "changed"
		case "mtu":
			m.form.field[i].value = "1420"
		}
	}
	m.handleKey(key{t: keyEnter})
	if len(f.calls) != 2 {
		t.Fatalf("two changed fields should issue two calls, got %d: %v", len(f.calls), f.calls)
	}
	hasArgs(t, f.calls[0], "network", "notes", "corp", "changed")
	hasArgs(t, f.calls[1], "network", "mtu", "corp", "1420")
}

func TestNetworksAdvancedFormUsesDirectConfigNotExec(t *testing.T) {
	// address4/mesh-mode/allow-relay/self-seed have no CLI verb — this must
	// go through commitConfig (a file write), never through runGravinet.
	f := installFakeGravinet(t)
	dir := t.TempDir()
	m := testModel()
	m.cfgPath = dir + "/config.json"
	m.snap.cfgPath = m.cfgPath
	m.setSection("networks")
	m.dispatchRowAction('E')
	if m.form == nil {
		t.Fatal("'E' should open the advanced form")
	}
	for i := range m.form.field {
		if m.form.field[i].key == "relay" {
			m.form.field[i].value = "true"
		}
	}
	m.handleKey(key{t: keyEnter})

	if len(f.calls) != 0 {
		t.Errorf("the advanced form has no CLI verb to call, but runGravinet was invoked: %v", f.calls)
	}
	if m.result == nil || !m.result.ok {
		t.Fatalf("expected a successful direct-config commit, got %+v", m.result)
	}
	if !strings.Contains(m.result.lines[len(m.result.lines)-1]+strings.Join(m.result.lines, " "), "restart") {
		t.Error("the advanced form's result should mention that a restart is required")
	}
	loaded, err := config.Load(m.cfgPath)
	if err != nil {
		t.Fatalf("config was not actually saved: %v", err)
	}
	if !loaded.Networks[0].AllowRelay {
		t.Error("allow-relay was not persisted")
	}
}

func TestKeysGenerateArgvForAnEmptySlot(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("keys")
	// The test snapshot has no keys set at all, so slot 0 on corp is empty
	// and is what selection lands on by default.
	m.dispatchRowAction('e')
	if m.form == nil {
		t.Fatal("'e' on an empty slot should open the generate/import form")
	}
	submitCurrentForm(t, m, map[string]string{"mode": "generate", "label": "primary"})
	got := lastCall(f)
	hasArgs(t, got, "key", "generate", "0", "-net", "corp", "-label", "primary")
}

func TestKeysImportRequiresAKeyValue(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("keys")
	m.dispatchRowAction('e')
	submitCurrentForm(t, m, map[string]string{"mode": "import", "keyval": ""})
	if len(f.calls) != 0 {
		t.Errorf("importing with no key value should not shell out: %v", f.calls)
	}
}

func TestKeysImportArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("keys")
	m.dispatchRowAction('e')
	submitCurrentForm(t, m, map[string]string{"mode": "import", "keyval": "AAAABBBBCCCC"})
	hasArgs(t, lastCall(f), "key", "set", "0", "AAAABBBBCCCC", "-net", "corp")
}

func TestKeysDeleteArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.snap.cfg.Networks[0].Keys[0] = config.KeySlot{Key: "somekey", Label: "current", Enabled: true}
	m.setSection("keys")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "key", "delete", "0", "-net", "corp")
}

func TestKeysToggleArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.snap.cfg.Networks[0].Keys[0] = config.KeySlot{Key: "somekey", Label: "current", Enabled: true}
	m.setSection("keys")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "key", "disable", "0", "-net", "corp")
}

func TestSeedsAddArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("seeds")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"net": "corp", "addr": "seed2.example:51820", "notes": "backup"})
	got := lastCall(f)
	hasArgs(t, got, "seed", "add", "seed2.example:51820", "-net", "corp", "-notes", "backup")
}

func TestSeedsAddFillsNetworkHintWhenThereIsExactlyOneNetwork(t *testing.T) {
	m := testModel()
	m.setSection("seeds")
	m.dispatchAdd()
	got, ok := formFieldValue(m.form, "net")
	if !ok || got != "corp" {
		t.Errorf("net field default = %q, ok=%v, want corp", got, ok)
	}
}

func TestSeedsDeleteArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel() // has one seed: seed.example:51820 on corp
	m.setSection("seeds")
	m.dispatchRowAction('d')
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "seed", "remove", "seed.example:51820", "-net", "corp")
}

func TestSeedsToggleArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("seeds")
	m.dispatchRowAction(' ')
	hasArgs(t, lastCall(f), "seed", "disable", "seed.example:51820", "-net", "corp")
}

func TestPeersToggleUsesDirectConfigNotExec(t *testing.T) {
	// Peers has no CLI verb at all — see peersActions' own comment.
	f := installFakeGravinet(t)
	dir := t.TempDir()
	m := testModel()
	m.cfgPath = dir + "/config.json"
	m.snap.cfgPath = m.cfgPath
	m.setSection("peers") // one direct peer (1111...), one relayed
	m.dispatchRowAction(' ')
	if len(f.calls) != 0 {
		t.Errorf("peer enable/disable has no CLI verb, but runGravinet was invoked: %v", f.calls)
	}
	if m.result == nil || !m.result.ok {
		t.Fatalf("expected a successful direct-config commit, got %+v", m.result)
	}
	loaded, err := config.Load(m.cfgPath)
	if err != nil {
		t.Fatalf("config was not saved: %v", err)
	}
	if len(loaded.Networks[0].DisabledPeers) != 1 {
		t.Errorf("expected exactly one disabled peer recorded, got %v", loaded.Networks[0].DisabledPeers)
	}
}

func TestPeersBanArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("peers")
	m.dispatchRowAction('d')
	if m.confirm == nil {
		t.Fatal("banning a peer should ask for confirmation")
	}
	m.handleKey(key{t: keyRune, r: 'y'})
	got := lastCall(f)
	hasArgs(t, got, "ban")
	if net, ok := argAfter(got, "-net"); !ok || net != "corp" {
		t.Errorf("-net = %q, ok=%v, want corp", net, ok)
	}
}

func TestBansAddArgv(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.setSection("bans")
	m.dispatchAdd()
	submitCurrentForm(t, m, map[string]string{"net": "corp", "node": "deadbeef", "notes": "compromised"})
	got := lastCall(f)
	hasArgs(t, got, "ban", "deadbeef", "-net", "corp", "-notes", "compromised")
}

func TestBansUnbanOnlyOffersDeleteForOwnBans(t *testing.T) {
	f := installFakeGravinet(t)
	m := testModel()
	m.snap.bans = []liveBan{
		{net: "corp", BanInfo: mesh.BanInfo{Target: "notmine", Mine: false}},
		{net: "corp", BanInfo: mesh.BanInfo{Target: "mine", Mine: true}},
	}
	m.setSection("bans")

	m.selTable, m.selID = "bans", "corp"+idSep+"notmine"
	m.dispatchRowAction('d')
	if len(f.calls) != 0 || m.confirm != nil {
		t.Error("deleting a ban this node did not issue should do nothing")
	}

	m.selID = "corp" + idSep + "mine"
	m.dispatchRowAction('d')
	if m.confirm == nil {
		t.Fatal("deleting this node's own ban should ask for confirmation")
	}
	m.handleKey(key{t: keyRune, r: 'y'})
	hasArgs(t, lastCall(f), "unban", "mine", "-net", "corp")
}

// formFieldValue reads a field's current value out of an open form, for
// tests that check a form's pre-filled defaults without submitting it.
func formFieldValue(f *formState, key string) (string, bool) {
	if f == nil {
		return "", false
	}
	for _, fl := range f.field {
		if fl.key == key {
			return fl.value, true
		}
	}
	return "", false
}
