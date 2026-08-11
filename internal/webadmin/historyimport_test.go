package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

func importCfg(t *testing.T) *config.Config {
	t.Helper()
	c := &config.Config{
		UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	return c
}

// An uploaded config becomes an ordinary history entry, restorable through
// the same reviewed path as any other — which is the point: it is what makes
// "restore the version I saved last week" possible.
func TestImportSnapshotAppearsInHistory(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	cfg := importCfg(t)
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	id, err := config.ImportSnapshot(path, cfg, "admin", "gravinet-config-grav3-2026-08-10.json", 10)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("no snapshot id returned")
	}

	list, err := config.List(path)
	if err != nil {
		t.Fatal(err)
	}
	var found *config.SnapshotMeta
	for i := range list {
		if list[i].ID == id {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("the imported snapshot is not in the list: %+v", list)
	}
	// An uploaded entry is otherwise indistinguishable from one this node
	// took, so the summary has to say where it came from.
	if !strings.HasPrefix(found.Summary, "uploaded") {
		t.Errorf("summary %q should mark this as uploaded", found.Summary)
	}
	if !strings.Contains(found.Summary, "grav3") {
		t.Errorf("summary %q should carry the note naming the file", found.Summary)
	}
	if found.User != "admin" {
		t.Errorf("user = %q, want admin", found.User)
	}
}

// Importing files a snapshot; it must not touch the running configuration.
// That separation is what keeps "upload a file" from being a way to
// reconfigure a node in one unreviewed step.
func TestImportSnapshotDoesNotApply(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	running := importCfg(t)
	if err := running.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	other := importCfg(t)
	other.Networks[0].Name = "somewhere-else"
	other.Networks[0].Subnet4 = "10.9.9.0/24"
	if _, err := config.ImportSnapshot(path, other, "admin", "", 10); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("importing a config changed the running configuration; it must only file a snapshot")
	}
}

// The upload path is a Download counterpart, so the filename it produces has
// to name the node — several nodes' snapshots land in one downloads folder
// and are otherwise distinguishable only by timestamp.
func TestDownloadFilenameCarriesHostname(t *testing.T) {
	src := indexHTML
	for _, want := range []string{
		"function chNodeName()",
		"state.selfHostname", // this node
		"(state.cluster||[]).find(x => x.node_id === state.target)", // or the managed one
		"'gravinet-config-'+(host ? host+'-' : '')+chStampToken(stamp)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("download filename wiring missing %q", want)
		}
	}
	// A hostname goes into a filename, so it must be sanitised.
	if !strings.Contains(src, "function chFileToken(") {
		t.Error("hostname is not sanitised before going into a filename")
	}
	// state.peers is the mesh peer list and is not loaded on this page;
	// reading it here is what made remote downloads fall back to a node-id
	// prefix instead of the hostname.
	if strings.Contains(between(t, src, "function chNodeName()", "function chFileToken("), "state.peers") {
		t.Error("chNodeName reads state.peers; it must use state.cluster, the header picker's list")
	}
}

// A configuration from another node is allowed: restoring one is how a node
// moves to replacement hardware. It is recorded rather than refused, so the
// list distinguishes a snapshot of this node from one carried over.
//
// v845 refused this and v865 reversed it. The refusal was not wrong about the
// consequences — restoring a foreign config does take on that node's identity
// — it was wrong that avoiding them was gravinet's call to make.
func TestHistoryImportAcceptsForeignNodeID(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := importCfg(t)
	cfg.NodeID = "aaaaaaaaaaaaaaaa"
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}

	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	srv := New(wcfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	post := func(upload *config.Config, note string) (int, map[string]any) {
		raw, _ := json.Marshal(upload)
		b, _ := json.Marshal(map[string]any{"config": json.RawMessage(raw), "note": note})
		req, _ := http.NewRequest("POST", ts.URL+"/api/history/import", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	summaries := func() []string {
		list, err := config.List(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range list {
			out = append(out, m.Summary)
		}
		return out
	}

	// Another node's config files, and says where it came from.
	foreign := importCfg(t)
	foreign.NodeID = "bbbbbbbbbbbbbbbb"
	if code, out := post(foreign, "grav1.json"); code != http.StatusOK {
		t.Fatalf("a config from another node should be accepted: %d %v", code, out)
	}
	got := summaries()
	if len(got) != 1 {
		t.Fatalf("want one entry, got %v", got)
	}
	if !strings.Contains(got[0], "bbbbbbbbbbbbbbbb") {
		t.Errorf("the entry should name the node it came from: %q", got[0])
	}
	if !strings.Contains(got[0], "grav1.json") {
		t.Errorf("the entry should keep the operator's note: %q", got[0])
	}

	// This node's own config is not labelled with a provenance it does not
	// have — every entry saying "from node ..." would make the label useless.
	own := importCfg(t)
	own.NodeID = "aaaaaaaaaaaaaaaa"
	if code, _ := post(own, "mine.json"); code != http.StatusOK {
		t.Fatal("this node's own config should still file")
	}
	for _, sm := range summaries() {
		if strings.Contains(sm, "mine.json") && strings.Contains(sm, "from node") {
			t.Errorf("an own-node upload should carry no provenance label: %q", sm)
		}
	}

	// A config with no node id at all is accepted too, and labelled, since
	// restoring it would mint a fresh identity.
	blank := importCfg(t)
	blank.NodeID = ""
	if code, _ := post(blank, "blank.json"); code != http.StatusOK {
		t.Fatal("a config with no node id should be accepted")
	}
	var sawBlank bool
	for _, sm := range summaries() {
		if strings.Contains(sm, "blank.json") && strings.Contains(sm, "no node id") {
			sawBlank = true
		}
	}
	if !sawBlank {
		t.Error("an upload with no node id should say so in its entry")
	}

	// Still not a way to load nonsense: validation is unchanged.
	bad := importCfg(t)
	bad.Networks[0].Subnet4 = "not-a-subnet"
	if code, _ := post(bad, "bad.json"); code == http.StatusOK {
		t.Error("an invalid configuration should still be refused")
	}
}

// The toolbar's enable/disable logic is keyed on button labels, so renaming a
// button silently changes which selection it requires. Both halves have to
// agree, and nothing else checks that they do.
func TestConfigHistoryToolbar(t *testing.T) {
	src := indexHTML
	bar := between(t, src, "// Split into two groups", "card.appendChild(t)")
	upd := between(t, src, "function chUpdateButtons(table)", "function chSingleSelectedId")

	// Every label in the bar must be one chUpdateButtons knows about, or it
	// falls to the default and demands exactly one ticked row.
	for _, label := range []string{"Snapshot", "Diff", "Download", "Upload", "Restore"} {
		if !strings.Contains(bar, "label:'"+label+"'") {
			t.Errorf("toolbar is missing the %q button", label)
		}
	}
	// Snapshot and Upload act on no selection; gating them on one would make
	// Upload unreachable until an unrelated row was ticked.
	if !strings.Contains(upd, "case 'Snapshot': case 'Upload': b.disabled = false") {
		t.Error("Snapshot/Upload are not always enabled")
	}
	if !strings.Contains(upd, "case 'Diff': b.disabled = n !== 2") {
		t.Error("Diff is not gated on exactly two ticked rows")
	}
	// Download and Upload sit in the right-hand group, split from the ones
	// that act on ticked rows.
	if !strings.Contains(bar, "right:true") {
		t.Error("the toolbar has no right-hand group")
	}
	// Only Restore is danger-coloured; the rest share the default blue.
	if strings.Count(bar, "cls:'danger'") != 1 {
		t.Error("exactly one button (Restore) should be danger-coloured")
	}
	if strings.Contains(bar, "cls:'ghost'") {
		t.Error("no config-history button should be ghost-styled any more")
	}
}

// A restore reinstates a whole configuration, so it almost always contains
// something structural. Leaving a "restart" banner behind makes the operator
// finish a job they already asked for — and until they do, the node is
// running a configuration that is neither the old one nor the one they
// restored.
//
// edit()'s third argument is what turns that into a quiet restart, and it is
// the same mechanism the network editor has used since it was written.
func TestRestoreRestartsAutomatically(t *testing.T) {
	src := indexHTML
	if !strings.Contains(src, "edit('/api/history/restore', { id: id }, true)") {
		t.Error("restore does not ask edit() to restart automatically; a restart banner would be left behind")
	}
	// The mechanism it relies on: edit() must still act on that argument.
	if !strings.Contains(src, "if (autoRestart) { quietRestart(); return true; }") {
		t.Error("edit() no longer honours its autoRestart argument")
	}
	// And restore must be the only path to it, so the modal's own "Restore
	// this" button cannot bypass the restart by calling the API directly.
	if n := strings.Count(src, "'/api/history/restore'"); n != 1 {
		t.Errorf("restore is posted from %d places; they would need the same treatment", n)
	}
}
