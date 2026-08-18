package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// blockingReload is a reload hook that reports when it was entered and then
// waits to be released, so a test can observe whether a response reached the
// client while the reload was still running.
type blockingReload struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingReload() *blockingReload {
	return &blockingReload{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingReload) fn() error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

func (b *blockingReload) releaseAll() { close(b.release) }

// addrOrderFixture builds a server with one running-looking network and a
// reload hook the test controls.
func addrOrderFixture(t *testing.T) (*httptest.Server, *http.Cookie, *blockingReload, string) {
	t.Helper()
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		UDPPorts: []int{65432}, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/16"}},
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
	br := newBlockingReload()
	srv.SetReload(br.fn)
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, sessionFor(t, ts), br, cfgPath
}

// postNetwork sends one /api/network edit, giving up after d. The timeout is
// the assertion mechanism here, not a flake guard: a response that only
// arrives after the reload finishes cannot arrive at all while the reload is
// blocked.
func postNetwork(t *testing.T, ts *httptest.Server, c *http.Cookie, body map[string]any, d time.Duration) (map[string]any, error) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+"/api/network", bytes.NewReader(b))
	req.AddCookie(c)
	client := &http.Client{Timeout: d}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// TestOverlayAddressRespondsBeforeApplying guards against reintroducing
// "changing a peer's overlay address returns 'context deadline exceeded' from
// the manager, for a change that in fact succeeded".
//
// Managing a peer means proxying to it over the mesh — resolveManagedTarget
// returns overlay addresses and nothing else — so the request and its reply
// both ride the network being edited. mutateConfig used to reload inline,
// before the handler wrote anything, and reloadFn applies an overlay-address
// change by rebuilding the network: TUN closed, address removed, sessions
// re-formed. The peer then tried to answer down a socket whose source address
// no longer existed. Nothing came back and no RST could, since the return path
// was the overlay itself, so the manager waited out proxyClient's 15s deadline
// and reported a timeout for an edit that had been saved and applied.
//
// The fix is ordering: commit, respond, flush, then apply. This test holds the
// reload open and requires the response to arrive anyway — which it cannot if
// the reload runs first.
func TestOverlayAddressRespondsBeforeApplying(t *testing.T) {
	ts, c, br, cfgPath := addrOrderFixture(t)
	defer br.releaseAll()

	res, err := postNetwork(t, ts, c, map[string]any{
		"op": "address", "net": "1234", "address4": "10.0.0.42/16",
	}, 3*time.Second)
	if err != nil {
		t.Fatalf("no response while the reload was still running: %v\n"+
			"the live apply is happening before the reply is written, which is exactly what made a peer edit time out on the manager", err)
	}
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("edit reported failure: %v", res)
	}

	// Committed before the response, not after it: a caller that reads the
	// config the moment it sees ok:true must find the new address there.
	stored, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Networks[0].Address4; got != "10.0.0.42/16" {
		t.Errorf("config holds address4 %q after an ok response, want 10.0.0.42/16 — the change must be committed before the reply, only its application deferred", got)
	}

	// Deferred, not dropped. Without this the "fix" would be to stop applying
	// the change at all, which is the v856 bug this must not walk back into.
	select {
	case <-br.entered:
	case <-time.After(3 * time.Second):
		t.Error("the reload never ran — the address was written and never applied, which is the defect v857 fixed")
	}
}

// TestOrdinaryEditStillReloadsBeforeResponding: the deferral is deliberately
// narrow. Every other op depends on its effect being true by the time the
// response says it succeeded, so those must still reload inline — a response
// that beats its own reload would be a silent behaviour change across the
// whole config surface, not a fix.
func TestOrdinaryEditStillReloadsBeforeResponding(t *testing.T) {
	ts, c, br, _ := addrOrderFixture(t)
	defer br.releaseAll()

	if _, err := postNetwork(t, ts, c, map[string]any{
		"op": "notes", "net": "1234", "notes": "hi",
	}, 1500*time.Millisecond); err == nil {
		t.Error("a notes edit answered while its reload was still blocked — the deferral has leaked past the one op that needs it")
	}
}

// TestNetworkAddressEditHasNoConfirm: the own-node overlay-address editor on
// Mesh > Networks used to raise a confirm, and the only thing that dialog said
// was that the node would restart to apply the change. v857 removed the
// restart; the dialog outlived it by two releases, asking for a click to
// acknowledge something that no longer happened.
//
// Rewriting it to describe the rebuild instead would have kept a modal whose
// reason for existing had gone. The operator double-clicked their own node's
// address cell, on their own node's page, and typed a new address; peers
// reconnecting is what that means. The consequence lives in the cell's own
// tooltip now, the same way the UDP/TCP port fields carry theirs.
//
// subnet and mtu keep their confirms, and this pins that too — theirs is not a
// restart warning but a mesh-wide one (every other node must be changed to
// match, and gravinet cannot detect a mismatch), and both do still restart the
// node. That is the line: a dialog earns its place by guarding something
// destructive or undiscoverable, not by narrating an ordinary edit.
func TestNetworkAddressEditHasNoConfirm(t *testing.T) {
	i := strings.Index(indexHTML, "} else if (field === 'address4' || field === 'address6'){")
	if i < 0 {
		t.Fatal("the address branch of the Networks cell editor was not found")
	}
	branch := indexHTML[i:]
	j := strings.Index(branch, "\n    } else {")
	if j < 0 {
		t.Fatal("could not bound the address branch")
	}
	branch = branch[:j]

	if strings.Contains(branch, "confirm(") {
		t.Error("the own-node address edit raises a confirm again — there is no restart left for it to warn about")
	}
	if strings.Contains(branch, "restartPending") {
		t.Error("the address branch still consults restartPending, which only mattered for suppressing the restart warning")
	}

	// The information the dialog carried has to survive somewhere the operator
	// can read it, or removing the dialog just deletes it.
	if !strings.Contains(indexHTML, "the network is rebuilt to take the new address") {
		t.Error("the address cell no longer explains what the edit does; the confirm was removed and its content went with it")
	}

	// mtu and subnet: still confirmed, for a reason that is not a restart.
	for _, want := range []string{
		"Set the overlay MTU for",
		"The same change must be made on every other node in this network",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("a confirm that should have been kept is gone (%q) — subnet and mtu warn about a mesh-wide mismatch gravinet cannot detect, not about a restart", want)
		}
	}
}
