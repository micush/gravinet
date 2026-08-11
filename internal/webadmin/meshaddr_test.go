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

// Editing a mesh device's address on System > Interfaces writes the network's
// overlay address, rather than being refused or set on the host and then
// silently reverted by the next reload.
func TestMeshInterfaceEditWritesOverlayAddress(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []config.Network{{
			ID: "0000000000001234", Name: "lan", Enabled: true,
			Subnet4: "10.42.0.0/16", Subnet6: "fd00:42::/64",
		}},
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	srv := New(wcfg, &stubBackend{}, logx.Default()) // stub reports mesh0 on net 0x1234
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	post := func(body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/system/interface-edit", bytes.NewReader(b))
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
	load := func() config.Network {
		c2, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return c2.Networks[0]
	}

	// Both families land in the network's own fields.
	if code, out := post(map[string]any{
		"op": "addrs", "iface": "mesh0",
		"addrs": []string{"10.42.0.5/16", "fd00:42::5/64"},
	}); code != http.StatusOK {
		t.Fatalf("edit rejected: %d %v", code, out)
	}
	n := load()
	if n.Address4 != "10.42.0.5/16" || n.Address6 != "fd00:42::5/64" {
		t.Fatalf("overlay address not written: %+v", n)
	}
	// And nowhere else: a mesh device must not be recorded as a managed host
	// interface, or the reconciler would fight the mesh for it every reload.
	c2, _ := config.Load(cfgPath)
	if c2.HostIfaceFor("mesh0") != nil {
		t.Error("a mesh device must not be recorded as a managed host interface")
	}

	// An address outside the network's subnet cannot be routed by any peer,
	// so it is refused rather than accepted and then found not to work.
	if code, out := post(map[string]any{
		"op": "addrs", "iface": "mesh0", "addrs": []string{"192.0.2.5/24"},
	}); code == http.StatusOK {
		t.Error("an address outside the network's subnet should be refused")
	} else if msg, _ := out["error"].(string); !strings.Contains(msg, "10.42.0.0/16") {
		t.Errorf("the refusal should name the subnet it is outside, got %q", msg)
	}
	if n := load(); n.Address4 != "10.42.0.5/16" {
		t.Errorf("a refused edit must not have changed anything: %+v", n)
	}

	// One address per family: keeping the first of several would leave the
	// operator reading a list that is not what is running.
	if code, _ := post(map[string]any{
		"op": "addrs", "iface": "mesh0", "addrs": []string{"10.42.0.5/16", "10.42.0.6/16"},
	}); code == http.StatusOK {
		t.Error("two IPv4 overlay addresses should be refused")
	}

	// Clearing goes back to self-assignment, which is what an empty field has
	// always meant on Mesh > Networks.
	if code, _ := post(map[string]any{"op": "addrs", "iface": "mesh0", "addrs": []string{}}); code != http.StatusOK {
		t.Error("clearing the overlay address should be allowed")
	}
	if n := load(); n.Address4 != "" || n.Address6 != "" {
		t.Errorf("clearing should empty both fields: %+v", n)
	}

	// A mesh interface has no default gateway to set, and says so rather
	// than writing one nothing would apply.
	code, out := post(map[string]any{"op": "gateway", "iface": "mesh0", "gw4": "10.42.0.1"})
	if code == http.StatusOK {
		t.Error("setting a gateway on a mesh interface should be refused")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "peers") {
		t.Errorf("the refusal should explain where overlay routing comes from, got %q", msg)
	}
}

// A mesh device's MTU is the network's, for the same reason its address is:
// it is reapplied from the network's settings on every rebuild, so setting it
// on the host would be reverted.
func TestMeshInterfaceEditWritesNetworkMTU(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []config.Network{{
			ID: "0000000000001234", Name: "lan", Enabled: true, Subnet4: "10.42.0.0/16",
		}},
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	cred, _ := GenerateCredential("admin", "pw", 10000)
	srv := New(config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}},
		&stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	post := func(body map[string]any) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/system/interface-edit", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(map[string]any{"op": "mtu", "iface": "mesh0", "mtu": 1420}); code != http.StatusOK {
		t.Fatalf("mesh MTU edit rejected: %d", code)
	}
	c2, _ := config.Load(cfgPath)
	if c2.Networks[0].MTU != 1420 {
		t.Fatalf("the network's MTU was not written: %+v", c2.Networks[0])
	}
	// And not recorded as host state, or the host reconciler would fight the
	// mesh for the same interface on every reload.
	if c2.HostIfaceFor("mesh0") != nil {
		t.Error("a mesh device must not be recorded as a managed host interface")
	}

	// Out of range is refused before it reaches the kernel, which would
	// return an errno rather than an explanation.
	if code := post(map[string]any{"op": "mtu", "iface": "mesh0", "mtu": 68}); code == http.StatusOK {
		t.Error("an MTU below the IPv4 minimum should be refused")
	}
	if code := post(map[string]any{"op": "mtu", "iface": "mesh0", "mtu": 99999}); code == http.StatusOK {
		t.Error("an absurd MTU should be refused")
	}
	if c3, _ := config.Load(cfgPath); c3.Networks[0].MTU != 1420 {
		t.Error("a refused edit must not have changed anything")
	}
}
