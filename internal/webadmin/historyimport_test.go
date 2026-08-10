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
		"(state.peers||[]).find(x => x.node_id === state.target)", // or the managed one
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
}

// The node-id guard is the one part of import that lives only in the handler,
// so it needs an HTTP-level test rather than coverage of ImportSnapshot
// beneath it.
func TestHistoryImportRefusesForeignNodeID(t *testing.T) {
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

	post := func(upload *config.Config) (int, map[string]any) {
		raw, _ := json.Marshal(upload)
		b, _ := json.Marshal(map[string]any{"config": json.RawMessage(raw), "note": "x.json"})
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

	// Another node's config: refused, and the error names both ids so the
	// operator can tell which file they grabbed.
	foreign := importCfg(t)
	foreign.NodeID = "bbbbbbbbbbbbbbbb"
	code, out := post(foreign)
	if code == http.StatusOK {
		t.Fatal("a config from another node must not be filed")
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "bbbbbbbbbbbbbbbb") || !strings.Contains(msg, "aaaaaaaaaaaaaaaa") {
		t.Errorf("the error should name both node ids, got %q", msg)
	}

	// No node id at all is refused too: restoring it would mint a fresh
	// identity, losing this node just as completely as adopting another's.
	blank := importCfg(t)
	blank.NodeID = ""
	if code, _ := post(blank); code == http.StatusOK {
		t.Error("a config with no node id must not be filed")
	}

	// This node's own config still files.
	own := importCfg(t)
	own.NodeID = "aaaaaaaaaaaaaaaa"
	if code, out := post(own); code != http.StatusOK {
		t.Fatalf("this node's own config should file, got %d %v", code, out)
	}
	list, err := config.List(cfgPath)
	if err != nil || len(list) != 1 {
		t.Fatalf("exactly the one accepted upload should be in the list: %v (%v)", list, err)
	}
}
