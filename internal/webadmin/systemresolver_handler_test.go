package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSystemResolverGet checks the read shape secResolver draws from. Reuses
// timeTestServer (systemtime_handler_test.go) — /api/system/resolver needs no
// network config either, same reasoning as Time: it reads host state, not
// gravinet's config (see hostresolver.go's package doc).
func TestSystemResolverGet(t *testing.T) {
	ts, c := timeTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/system/resolver", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/system/resolver = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"hostname", "servers", "search_domain", "manager", "can_hostname", "can_dns", "hint"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the page reads it directly", k)
		}
	}
	if _, ok := out["servers"].([]any); !ok {
		t.Errorf("servers = %#v, want a JSON array even when empty", out["servers"])
	}
	if h, _ := out["hostname"].(string); h == "" {
		t.Error("hostname should reflect the real os.Hostname() of the test host, never empty")
	}
}

// TestSystemResolverRejectsBadRequests covers the refusal paths. Nothing here
// reaches an OS command — every case is caught by validation before dispatch —
// so running the suite can't change the test machine's hostname or DNS config.
func TestSystemResolverRejectsBadRequests(t *testing.T) {
	ts, c := timeTestServer(t)
	post := func(body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/system/resolver", bytes.NewReader(b))
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

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"unknown op", map[string]any{"op": "nonsense"}, "op must be"},
		{"missing op", map[string]any{}, "op must be"},
		{"empty hostname", map[string]any{"op": "hostname", "hostname": ""}, "empty"},
		{"hostname injection", map[string]any{"op": "hostname", "hostname": "host; reboot"}, "invalid hostname"},
		{"hostname whitespace", map[string]any{"op": "hostname", "hostname": "bad host"}, "invalid hostname"},
		{"dns server not an IP", map[string]any{"op": "dns", "servers": []string{"dns.google"}}, "invalid DNS server"},
		{"dns server injection", map[string]any{"op": "dns", "servers": []string{"1.1.1.1", "evil; reboot"}}, "invalid DNS server"},
		{"search domain injection", map[string]any{"op": "dns", "servers": []string{"1.1.1.1"}, "search_domain": "bad domain; reboot"}, "invalid search domain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := post(tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (reply: %v)", code, out)
			}
			msg, _ := out["error"].(string)
			if msg == "" {
				t.Fatal("a rejection must carry an error message the page can show")
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

// TestSystemResolverIsProxyable guards the placement decision: like Power,
// Time, and Users, this endpoint follows the selected node, so it must NOT be
// pinned in the client's LOCAL_API list. Getting this wrong would silently
// apply a hostname/DNS change to the wrong host.
func TestSystemResolverIsProxyable(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if strings.Contains(local, "/api/system/resolver") {
		t.Error("/api/system/resolver is pinned in LOCAL_API; it should follow the selected node like /api/system/time")
	}
}

// TestSystemResolverNavPlacement pins Resolver into System, directly above
// Time — matching parapet's own item order (resolver, time, dhcp, snmp,
// users, power), with Upgrade ahead of it (gravinet-only) and Power still
// last (the group's most destructive item; new items land above it).
func TestSystemResolverNavPlacement(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'system'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'upgrade'", "'resolver'", "'time'", "'users'", "'power'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the system nav group is missing %s:\n%s", want, block)
		}
	}
	idx := func(item string) int { return strings.Index(block, "'"+item+"'") }
	if !(idx("upgrade") < idx("resolver") && idx("resolver") < idx("time") && idx("time") < idx("users") && idx("users") < idx("power")) {
		t.Errorf("System nav order must be upgrade, resolver, time, users, power; got positions %v",
			map[string]int{"upgrade": idx("upgrade"), "resolver": idx("resolver"), "time": idx("time"), "users": idx("users"), "power": idx("power")})
	}

	infoBlock := indexHTML[strings.Index(indexHTML, "{ name:'info'"):]
	infoBlock = infoBlock[:strings.Index(infoBlock, "]},")]
	if strings.Contains(infoBlock, "'resolver'") {
		t.Error("resolver leaked into the Info group")
	}

	if !strings.Contains(indexHTML, "resolver:secResolver") {
		t.Error("no resolver:secResolver entry in the section dispatch table")
	}
	if !strings.Contains(indexHTML, "function secResolver(") {
		t.Error("secResolver is not defined")
	}
}
