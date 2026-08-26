package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/config"
)

// newTestServerWithDHCP builds a server whose stored config has the given
// DHCP mode, so the handler's three empty-table cases can be told apart.
func newTestServerWithDHCP(t *testing.T, mode string) *Server {
	t.Helper()
	s, _, _ := newTestServer(t)
	cp := filepath.Join(t.TempDir(), "config.json")
	c := config.Default()
	c.DHCP.Mode = config.DHCPMode(mode)
	if mode == string(config.DHCPRelay) {
		c.DHCP.Relay.Links = []config.DHCPRelayLink{{
			Iface: "eth1", Servers: []string{"192.0.2.10"},
		}}
	}
	if err := c.SaveTo(cp); err != nil {
		t.Fatal(err)
	}
	s.configPath = cp
	return s
}

// readSourceFile returns a source file from this package, for the tests that
// assert on what the code does not call.
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// containsCall ignores matches inside comments, so a doc comment naming a
// function it deliberately does not call does not trip the check.
func containsCall(src, call string) bool {
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(line, call) {
			return true
		}
	}
	return false
}

// The handler's job beyond the reader is to distinguish the three ways a lease
// table can legitimately be empty. Rendering them identically is the failure
// this guards: an operator on a relay seeing "no leases" would reasonably
// conclude their DHCP was broken.

func leasesFor(t *testing.T, s *Server) dhcpLeasesJSON {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handleDHCPLeases(rr, httptest.NewRequest(http.MethodGet, "/api/dhcp-leases", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var out dhcpLeasesJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rr.Body.String())
	}
	return out
}

// A relay holds no leases of its own, and that is a complete answer rather
// than a missing one. It must say so, and must not read a stale local file
// left over from a previous stint as a server.
func TestLeasesOnARelaySaysWhereTheLeasesAre(t *testing.T) {
	s := newTestServerWithDHCP(t, "relay")
	out := leasesFor(t, s)
	if out.Mode != "relay" {
		t.Fatalf("mode %q, want relay", out.Mode)
	}
	if len(out.Leases) != 0 {
		t.Errorf("a relay reported leases of its own: %+v", out.Leases)
	}
	if out.Hint == "" {
		t.Error("no hint on a relay; an empty table alone reads as broken DHCP")
	}
}

// DHCP off with nothing running is the third empty case and gets its own
// explanation rather than the relay's.
func TestLeasesWithDHCPOffExplainsItself(t *testing.T) {
	s := newTestServerWithDHCP(t, "")
	out := leasesFor(t, s)
	if len(out.Leases) != 0 {
		t.Errorf("DHCP is off but leases were reported: %+v", out.Leases)
	}
	if out.Hint == "" {
		t.Error("no hint when DHCP is off")
	}
}

// The endpoint is read-only. This is the property that keeps a monitoring page
// from installing a package or starting a service as a side effect of being
// opened — see installKea's doc comment for why that matters.
func TestLeasesEndpointNeverWrites(t *testing.T) {
	src := readSourceFile(t, "dhcp_leases.go")
	for _, forbidden := range []string{
		"installKea(", "keaStopAndDisable(", "keaStartAndEnable(",
		"renderKea(", "os.WriteFile(", "os.Rename(", "exec.Command(",
	} {
		if containsCall(src, forbidden) {
			t.Errorf("the lease endpoint calls %s; Monitor pages must not mutate anything", forbidden)
		}
	}
}
