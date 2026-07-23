package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// usersTestServer stands up an authenticated admin server for the System >
// Users endpoint. Mirrors timeTestServer's shape.
func usersTestServer(t *testing.T) (*httptest.Server, *http.Cookie) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443", AuthMode: "local"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
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
	t.Cleanup(ts.Close)
	return ts, sessionFor(t, ts)
}

// TestSystemUsersGet checks the read shape the page draws from. This hits
// the real, unmocked service.ListSystemUsers — there's no config knob left
// to fake the group away, group membership is OS state — so it checks field
// presence/types rather than asserting a specific group_known/users value:
// whether the "gravinet" group happens to exist on the machine running the
// test suite is not this test's business, and asserting a specific answer
// would make the suite pass or fail depending on unrelated host state.
func TestSystemUsersGet(t *testing.T) {
	ts, c := usersTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/system/users", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/system/users = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"users", "can_manage", "can_expiry", "manage_hint", "expiry_hint", "group", "group_known", "group_hint", "auth_mode"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the page reads it directly", k)
		}
	}
	if _, ok := out["users"].([]any); !ok {
		t.Errorf("users = %#v, want a JSON array even when empty", out["users"])
	}
	if g, _ := out["group"].(string); g != "gravinet" {
		t.Errorf(`group = %v, want "gravinet"`, out["group"])
	}
	if _, ok := out["group_known"].(bool); !ok {
		t.Errorf("group_known = %#v, want a bool", out["group_known"])
	}
	if out["auth_mode"] != "local" {
		t.Errorf("auth_mode = %v, want %q (this test server's own config)", out["auth_mode"], "local")
	}
}

// TestSystemUsersRejectsBadRequests covers refusal paths that must be caught
// by validation before ever reaching an OS command — useradd/dscl/pw/net user
// are real, privileged mutations, and this suite must never actually invoke
// one against the machine running the tests.
func TestSystemUsersRejectsBadRequests(t *testing.T) {
	ts, c := usersTestServer(t)
	post := func(body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/system/users", strings.NewReader(string(b)))
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
		{"unknown op", map[string]any{"op": "wipe", "username": "bob"}, "op must be"},
		{"invalid username", map[string]any{"op": "add", "username": "Not Valid!", "password": "x"}, "invalid username"},
		{"root refused", map[string]any{"op": "add", "username": "root", "password": "x"}, "refusing"},
		{"empty password on add", map[string]any{"op": "add", "username": "bob", "password": ""}, "password required"},
		{"password op with bad name", map[string]any{"op": "password", "username": "../etc/passwd", "password": "x"}, "invalid username"},
		{"expiry with bad name", map[string]any{"op": "expiry", "username": "$(id)", "expires_unix": 0}, "invalid username"},
		{"delete with bad name", map[string]any{"op": "delete", "username": "bob smith"}, "invalid username"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := post(tc.body)
			if status == 200 {
				t.Fatalf("expected a rejection, got 200: %#v", out)
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

// TestSystemUsersRefusesSelfDelete pins the "don't let the signed-in session
// delete its own account" guard — an easy way to lock everyone out if it
// silently regressed. The account name doesn't need to actually exist as an
// OS account: the self-delete check runs before DeleteSystemUser is ever
// called, so this can't reach a real userdel either.
func TestSystemUsersRefusesSelfDelete(t *testing.T) {
	ts, c := usersTestServer(t)
	b, _ := json.Marshal(map[string]any{"op": "delete", "username": "admin"})
	req, _ := http.NewRequest("POST", ts.URL+"/api/system/users", strings.NewReader(string(b)))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("deleting the account the session is signed in as must be refused")
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "signed in") {
		t.Errorf("error = %q, want it to explain the self-delete refusal", msg)
	}
}

// TestSystemUsersIsProxyable guards the placement decision: like Power and
// Time, this endpoint follows the selected node, so it must NOT be in the
// client's LOCAL_API pin-to-this-node list. Getting this wrong would silently
// manage the wrong host's accounts.
func TestSystemUsersIsProxyable(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if strings.Contains(local, "/api/system/users") {
		t.Error("/api/system/users is pinned in LOCAL_API; it should follow the selected node like /api/system/power and /api/system/time")
	}
}

// TestSystemUsersNavPlacement pins the page into System, between Time and
// Power (Power stays last as the most destructive item in the group).
func TestSystemUsersNavPlacement(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'system'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'upgrade'", "'time'", "'users'", "'power'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the system nav group is missing %s:\n%s", want, block)
		}
	}
	if strings.Index(block, "'time'") > strings.Index(block, "'users'") {
		t.Error("Users must come after Time in the System group")
	}
	if strings.Index(block, "'users'") > strings.Index(block, "'power'") {
		t.Error("Power must stay last in the System group; new items go above it")
	}
	infoBlock := indexHTML[strings.Index(indexHTML, "{ name:'info'"):]
	infoBlock = infoBlock[:strings.Index(infoBlock, "]},")]
	if strings.Contains(infoBlock, "'users'") {
		t.Error("users leaked into the Info group")
	}
	if !strings.Contains(indexHTML, "users:secUsers") {
		t.Error("no users:secUsers entry in the section dispatch table")
	}
	if !strings.Contains(indexHTML, "function secUsers(") {
		t.Error("secUsers is not defined")
	}
}
