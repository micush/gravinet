package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// writeMemberConfig writes a config file with one keyed network and returns its path.
func writeMemberConfig(t *testing.T) string {
	t.Helper()
	c := &config.Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []config.Network{{ID: "00000000feedface", Name: "lan", Enabled: true, Subnet4: "10.42.0.0/16",
			Seeds: config.SeedList{{Address: "203.0.113.5:65432"}}}}}
	c.Networks[0].Keys[0] = config.KeySlot{Key: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", Label: "key0", Enabled: true}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := c.SaveTo(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func serverFor(path string) *Server {
	s := New(config.WebAdmin{AuthMode: "local"}, &stubBackend{}, logx.Default())
	s.SetConfigPath(path)
	s.SetReload(func() error { return nil })
	return s
}

func TestNetworkTokenGenerateAndJoinOverHTTP(t *testing.T) {
	// Node A mints a token over HTTP.
	srcSrv := serverFor(writeMemberConfig(t))
	ts := httptest.NewServer(http.HandlerFunc(srcSrv.handleNetworkToken))
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"net":"lan","expires":"24h"}`))
	if err != nil {
		t.Fatal(err)
	}
	var gen map[string]any
	json.NewDecoder(resp.Body).Decode(&gen)
	resp.Body.Close()
	tok, _ := gen["token"].(string)
	if !config.IsJoinToken(tok) {
		t.Fatalf("no token returned: %v", gen)
	}

	// Node B joins via the token over HTTP.
	dstPath := filepath.Join(t.TempDir(), "config.json")
	blank := &config.Config{PrimaryPort: 65432, EnableIPv4: true}
	if err := blank.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := blank.SaveTo(dstPath); err != nil {
		t.Fatal(err)
	}
	dstSrv := serverFor(dstPath)
	tj := httptest.NewServer(http.HandlerFunc(dstSrv.handleNetwork))
	defer tj.Close()
	body, _ := json.Marshal(map[string]string{"op": "join-token", "token": tok})
	r2, err := http.Post(tj.URL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("join-token returned %d", r2.StatusCode)
	}

	// The joined config now carries the same network.
	got, err := config.Load(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	n := got.FindNetwork("00000000feedface")
	if n == nil || !n.Enabled || n.Subnet4 != "10.42.0.0/16" {
		t.Fatalf("network not joined correctly: %+v", n)
	}
	if n.Keys[0].Key != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		t.Fatal("key not imported via token")
	}
	if !contains(n.Seeds.Addrs(), "203.0.113.5:65432") {
		t.Fatalf("seed not imported: %v", n.Seeds)
	}
}

func TestNetworkTokenBadNetwork(t *testing.T) {
	srv := serverFor(writeMemberConfig(t))
	ts := httptest.NewServer(http.HandlerFunc(srv.handleNetworkToken))
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"net":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] == nil {
		t.Fatalf("expected error for unknown network, got %v", out)
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
