package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/hostnet"
	"gravinet/internal/logx"
)

// The inventory is read from the live host, so the assertions are about
// shape and ordering rather than specific addresses — loopback is the one
// interface every machine running this test is guaranteed to have.
func TestSystemInterfacesInventory(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{
		UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	srv := New(wcfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	req, _ := http.NewRequest("GET", ts.URL+"/api/system/interfaces", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Interfaces []sysIface `json:"interfaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Interfaces) == 0 {
		t.Fatal("no interfaces reported; every host has at least loopback")
	}

	var lo *sysIface
	for i := range out.Interfaces {
		if out.Interfaces[i].Name == "lo" || out.Interfaces[i].Name == "lo0" {
			lo = &out.Interfaces[i]
		}
	}
	if lo == nil {
		t.Fatalf("loopback not in the inventory: %+v", out.Interfaces)
	}
	if lo.MTU == 0 {
		t.Error("loopback reported with no MTU")
	}
	// Link-local addresses are not reported: the kernel assigns them, this
	// page cannot change them, and on a v6 host they bury the addresses an
	// operator came to read. Checked across every interface, since the host
	// running this almost certainly has at least one fe80::.
	for _, e := range out.Interfaces {
		for _, a := range e.Addrs {
			p, err := netip.ParsePrefix(a.CIDR)
			if err != nil {
				t.Errorf("%s: %q is not a valid prefix", e.Name, a.CIDR)
				continue
			}
			if p.Addr().IsLinkLocalUnicast() {
				t.Errorf("%s: link-local address %q should not be reported", e.Name, a.CIDR)
			}
			if a.Scope == "link-local" {
				t.Errorf("%s: %q still carries the link-local scope", e.Name, a.CIDR)
			}
		}
	}

	var sawLoopbackScope bool
	for _, a := range lo.Addrs {
		if a.Scope == "loopback" {
			sawLoopbackScope = true
		}
		if a.Family != "ipv4" && a.Family != "ipv6" {
			t.Errorf("address %q has family %q", a.CIDR, a.Family)
		}
		if !strings.Contains(a.CIDR, "/") {
			t.Errorf("address %q should carry its prefix length", a.CIDR)
		}
	}
	if !sawLoopbackScope {
		t.Errorf("loopback's own address should be scoped loopback: %+v", lo.Addrs)
	}

	// Interfaces are name-sorted so the list does not reshuffle between
	// loads, and addresses are global-first so the one an operator came
	// looking for is the one they read first.
	for i := 1; i < len(out.Interfaces); i++ {
		if out.Interfaces[i-1].Name > out.Interfaces[i].Name {
			t.Errorf("interfaces are not name-sorted: %q before %q", out.Interfaces[i-1].Name, out.Interfaces[i].Name)
		}
	}
	for _, e := range out.Interfaces {
		last := -1
		for _, a := range e.Addrs {
			if r := scopeRank(a.Scope); r < last {
				t.Errorf("%s: addresses not ordered global-first: %+v", e.Name, e.Addrs)
				break
			} else {
				last = r
			}
		}
	}
}

// Addresses and gateway are editable; the table itself still offers no
// add/remove, because an interface is not something this page creates.
func TestSystemInterfacesSectionEditing(t *testing.T) {
	sec := between(t, indexHTML, "function secInterfaces(", "function secRadvd(")
	if strings.Contains(sec, "_rowAdd") || strings.Contains(sec, "_rowRemove") {
		t.Error("the interfaces table offers add/remove; interfaces are not created here")
	}
	if !strings.Contains(sec, "enhanceTable(table)") {
		t.Error("async-loaded table is not enhanced, so it would render with no toolbar")
	}
	for _, want := range []string{"ifEditAddrs", "ifEditGateway", "/api/system/interface-edit"} {
		if !strings.Contains(sec, want) {
			t.Errorf("editing wiring missing %q", want)
		}
	}
	// A mesh device's address is editable, but its gateway is not: the
	// address is redirected to the network's overlay setting, while overlay
	// routing comes from what peers advertise and has nowhere to be written.
	// The v850 rule that excluded mesh rows from editing entirely was
	// replaced in v856 by writing the edit through instead.
	if strings.Contains(sec, "tr.dataset.mesh === '1') return") {
		t.Error("mesh rows are still skipped entirely; their address should be editable")
	}
	if !strings.Contains(sec, "if (gc && !mesh)") {
		t.Error("a mesh device's gateway should not be editable")
	}
	// The two things an operator needs to know before double-clicking: that
	// there is no confirmation step, and that the change is persisted rather
	// than lost at the next reboot.
	for _, want := range []string{"no confirmation", "survive a reboot"} {
		if !strings.Contains(sec, want) {
			t.Errorf("hint does not say %q", want)
		}
	}
}

// One edit names one thing — the addresses, or the gateway — but both the
// apply and the config record are whole-interface. The half the operator did
// not touch has to be filled in from what is there, or editing a gateway
// would wipe the addresses.
func TestBuildHostSpecFillsTheUntouchedHalf(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	cfg := &config.Config{UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := cfg.SetHostIface(config.HostIface{
		Iface: "eth1", Addrs: []string{"10.1.1.5/24"}, GW4: "10.1.1.1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	srv := New(config.WebAdmin{}, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)

	// An addresses-only edit keeps the recorded gateway.
	spec, err := buildHostSpec(srv, "eth1", "addrs", []string{"10.1.1.9/24"}, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if spec.GW4.String() != "10.1.1.1" {
		t.Errorf("an addresses-only edit dropped the gateway: %+v", spec)
	}
	if len(spec.Addrs) != 1 || spec.Addrs[0].String() != "10.1.1.9/24" {
		t.Errorf("addresses not carried: %+v", spec.Addrs)
	}

	// Bad input is refused rather than silently coerced.
	if _, err := buildHostSpec(srv, "eth1", "addrs", []string{"10.1.1.9"}, "", "", 0); err == nil {
		t.Error("an address with no prefix length should be refused")
	}
	if _, err := buildHostSpec(srv, "eth1", "gateway", nil, "fd00::1", "", 0); err == nil {
		t.Error("an IPv6 address in the IPv4 gateway field should be refused")
	}
	if _, err := buildHostSpec(srv, "eth1", "nonsense", nil, "", "", 0); err == nil {
		t.Error("an unknown op should be refused")
	}
}

// Editing is implemented on every platform gravinet supports, each through
// the mechanism that platform's overlay device already uses. This is a
// compile-time claim as much as a runtime one — the point is that no build
// falls back to a stub that refuses.
func TestAddressEditingIsImplementedHere(t *testing.T) {
	// A prefix that cannot exist on any interface, so the call reaches the
	// platform layer and fails there rather than being rejected earlier.
	_, _, err := hostnet.Apply(hostnet.Spec{
		Iface: "gravinet-no-such-iface-xyz",
		Addrs: []netip.Prefix{netip.MustParsePrefix("198.18.99.1/32")},
	})
	if err == nil {
		t.Fatal("adding to a nonexistent interface should fail")
	}
	// The failure must come from the platform mechanism, not from a stub
	// saying the feature does not exist here.
	if strings.Contains(err.Error(), "not implemented on this platform") {
		t.Errorf("%s has no address-editing implementation: %v", runtime.GOOS, err)
	}
}

// A successful edit says nothing at all. The operator asked for an address to
// change; the table redraws showing it changed. Which files gravinet wrote it
// to is gravinet's problem.
//
// The exceptions are the two things the operator cannot see: a change that
// did not happen, and a change that happened but will not come back after a
// reboot or a restore.
func TestInterfaceEditsAreSilentOnSuccess(t *testing.T) {
	sec := between(t, indexHTML, "function secInterfaces(", "function secRadvd(")

	// No success chatter, in a dialog or inline.
	for _, gone := range []string{"ifLastNote", "r.body.note"} {
		if strings.Contains(sec, gone) {
			t.Errorf("a successful edit still reports %q back to the operator", gone)
		}
	}
	// Every editor — addresses, gateway, MTU — still alerts on a change that
	// did not happen...
	const editors = 3
	if n := strings.Count(sec, "|| 'could not apply'"); n != editors {
		t.Errorf("expected all %d editors to alert on failure, found %d", editors, n)
	}
	// ...and on one that applied but will not persist.
	if n := strings.Count(sec, "alert(r.body.warning)"); n != editors {
		t.Errorf("expected all %d editors to surface a persistence warning, found %d", editors, n)
	}
}

// The handler must not manufacture a success message, and must still describe
// a partial failure in terms of what it costs the operator rather than which
// subsystem returned it.
func TestApplyAndRecordWarningWording(t *testing.T) {
	src := mustRead("sysifaces_edit.go")
	if strings.Contains(src, `"ok": true, "note"`) {
		t.Error("the handler still returns a success note")
	}
	for _, want := range []string{
		"a restored backup will not bring it back",
		"will not survive a reboot",
		"The address is applied, but ",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("warning wording missing %q", want)
		}
	}
}

// Addresses and gateway are edited the same way: one box per address family.
// A single box for both meant reading a mixed list to find the address being
// changed, and retyping the other family alongside it to avoid removing it.
func TestAddressEditorIsSplitByFamily(t *testing.T) {
	sec := between(t, indexHTML, "function secInterfaces(", "function secRadvd(")

	for _, want := range []string{"ife-a4", "ife-a6", "ife-gw4", "ife-gw6"} {
		if !strings.Contains(sec, want) {
			t.Errorf("missing per-family input %q", want)
		}
	}
	// The single combined box is gone.
	if strings.Contains(sec, "ife-addrs") {
		t.Error("the combined address box is still present")
	}
	// Both families are recombined into one request: the handler takes the
	// interface's whole intended set, and sending one family at a time would
	// make clearing the other look like an edit that did not mention it.
	if !strings.Contains(sec, `[cell.querySelector('.ife-a4').value, cell.querySelector('.ife-a6').value]`) {
		t.Error("the two boxes are not recombined into a single address list")
	}
	// And the prefill splits what is there now, or opening the editor would
	// show every address in the IPv4 box.
	for _, want := range []string{"cur.filter(a => !isV6(a))", "cur.filter(isV6)"} {
		if !strings.Contains(sec, want) {
			t.Errorf("the editor does not split existing addresses by family: missing %q", want)
		}
	}
}
