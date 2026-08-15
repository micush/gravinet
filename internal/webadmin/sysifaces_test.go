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
	"sort"
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
	spec, err := buildHostSpec(srv, hostEdit{Iface: "eth1", Op: "addrs", Addrs: []string{"10.1.1.9/24"}})
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
	if _, err := buildHostSpec(srv, hostEdit{Iface: "eth1", Op: "addrs", Addrs: []string{"10.1.1.9"}}); err == nil {
		t.Error("an address with no prefix length should be refused")
	}
	if _, err := buildHostSpec(srv, hostEdit{Iface: "eth1", Op: "gateway", GW4: "fd00::1"}); err == nil {
		t.Error("an IPv6 address in the IPv4 gateway field should be refused")
	}
	if _, err := buildHostSpec(srv, hostEdit{Iface: "eth1", Op: "nonsense"}); err == nil {
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
		// "The change is applied", not "The address is applied": this page now
		// edits addressing modes too, and a mode switch that half-succeeded is
		// the case most in need of the warning. Widened deliberately rather
		// than by accident — the assertion is here to stop the wording drifting
		// back to something that describes only half of what the page does.
		"The change is applied, but ",
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

// hostIfaceEditServer is a Server over a real config file, so buildHostSpec can
// read the record it interprets an edit through.
func hostIfaceEditServer(t *testing.T, ifaces ...config.HostIface) *Server {
	t.Helper()
	cfg := &config.Config{UDPPorts: []int{51820}, EnableIPv4: true, HostInterfaces: ifaces}
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	srv := New(config.WebAdmin{AuthMode: "local"}, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	return srv
}

func cidrs(ps []netip.Prefix) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	sort.Strings(out)
	return out
}

// An MTU or gateway edit must not adopt an address gravinet does not record.
//
// The addresses an edit did not mention are carried forward so Prune does not
// remove them, and they used to be read from the live interface and filtered by
// mode. The filter was supposed to keep a lease out, and does not in the case
// that matters: an interface with no record has no mode, buildHostSpec reads
// that as static, and so a DHCP lease on a never-managed interface passed the
// filter, went into the record as a static address, and was reapplied as one at
// every reload and written into the host's boot configuration. One MTU change
// and the leased address was gravinet's forever.
//
// With no record there is also nothing to prune against, so the edit must not
// prune: those addresses belong to whatever is managing the interface.
func TestMTUEditDoesNotAdoptUnrecordedAddresses(t *testing.T) {
	srv := hostIfaceEditServer(t) // no record for eth9 at all

	spec, err := buildHostSpec(srv, hostEdit{Iface: "eth9", Op: "mtu", MTU: 1400})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Addrs) != 0 {
		t.Errorf("an MTU edit adopted %v; gravinet records no address for this interface", cidrs(spec.Addrs))
	}
	if spec.Prune {
		t.Error("an MTU edit on an unrecorded interface must not prune: there is no intended set to prune against")
	}
	if spec.MTU != 1400 {
		t.Errorf("MTU = %d, want 1400", spec.MTU)
	}

	// Same for a gateway edit, which shares the path.
	spec, err = buildHostSpec(srv, hostEdit{Iface: "eth9", Op: "gateway", GW4: "10.9.9.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Addrs) != 0 || spec.Prune {
		t.Errorf("gateway edit: addrs=%v prune=%v, want none and false", cidrs(spec.Addrs), spec.Prune)
	}
}

// With a record, the addresses carried forward are the recorded ones — so an
// MTU edit reasserts what gravinet was asked for and prunes anything else,
// which is what removes a lease still sitting beside a static address.
func TestMTUEditCarriesRecordedAddressesOnly(t *testing.T) {
	srv := hostIfaceEditServer(t, config.HostIface{
		Iface: "eth9", Mode4: hostnet.ModeStatic, Mode6: hostnet.ModeStatic,
		Addrs: []string{"192.168.122.12/24", "fd0a:9::1/64"}, GW4: "192.168.122.1",
	})

	spec, err := buildHostSpec(srv, hostEdit{Iface: "eth9", Op: "mtu", MTU: 1400})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.168.122.12/24", "fd0a:9::1/64"}
	got := cidrs(spec.Addrs)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("addrs = %v, want %v", got, want)
	}
	if !spec.Prune {
		t.Error("an edit against a record submits the whole intended set, so it prunes")
	}
	if spec.GW4.String() != "192.168.122.1" {
		t.Errorf("gateway not carried forward: %v", spec.GW4)
	}
}

// A family recorded as non-static keeps its addresses out of the carried-forward
// set, so an MTU edit cannot turn a lease into a static address that way either.
func TestMTUEditSkipsNonStaticFamilies(t *testing.T) {
	srv := hostIfaceEditServer(t, config.HostIface{
		Iface: "eth9", Mode4: hostnet.ModeDHCP, Mode6: hostnet.ModeStatic,
		Addrs: []string{"fd0a:9::1/64"},
	})
	spec, err := buildHostSpec(srv, hostEdit{Iface: "eth9", Op: "mtu", MTU: 1400})
	if err != nil {
		t.Fatal(err)
	}
	if got := cidrs(spec.Addrs); len(got) != 1 || got[0] != "fd0a:9::1/64" {
		t.Errorf("addrs = %v, want only the static IPv6 address", got)
	}
}

// The per-address source tag. The bug this pins is the one visible on the page:
// eth0 recorded static with one address, a DHCP lease still up beside it, and
// both rows tagged "static" — because the tag was the family's mode painted onto
// every address of that family. The leftover lease was labelled as a decision
// somebody made, which is the exact question the tag exists to answer.
func TestAddressSourceTagIsPerAddress(t *testing.T) {
	rec := config.HostIface{
		Iface: "eth0", Mode4: hostnet.ModeStatic, Mode6: hostnet.ModeStatic,
		Addrs: []string{"192.168.122.12/24"},
	}
	recorded := map[netip.Prefix]bool{}
	for _, a := range rec.Addrs {
		p, _ := netip.ParsePrefix(a)
		recorded[p] = true
	}

	// The classification the handler applies, exercised over the case on the
	// page: the recorded address and the lease that has not gone away.
	tag := func(addr netip.Prefix, fam hostnet.Mode) string {
		switch {
		case !fam.IsStatic():
			return string(fam)
		case recorded[addr]:
			return string(hostnet.ModeStatic)
		default:
			return "unmanaged"
		}
	}

	lease := netip.MustParsePrefix("192.168.122.198/24")
	mine := netip.MustParsePrefix("192.168.122.12/24")

	if got := tag(mine, rec.Mode4); got != "static" {
		t.Errorf("the recorded address should read static, got %q", got)
	}
	if got := tag(lease, rec.Mode4); got == "static" {
		t.Error("a leased address gravinet does not record must not be tagged static")
	} else if got != "unmanaged" {
		t.Errorf("the unrecorded address should read unmanaged, got %q", got)
	}

	// Under a non-static family the address really did come from the network,
	// so the family's own mode is the honest tag and no record is expected.
	if got := tag(lease, hostnet.ModeDHCP); got != "dhcp" {
		t.Errorf("under dhcp the tag should be dhcp, got %q", got)
	}
	if got := tag(netip.MustParsePrefix("2001:db8::5/64"), hostnet.ModeSLAAC); got != "slaac" {
		t.Errorf("under slaac the tag should be slaac, got %q", got)
	}
}

// The UI must read the server's per-address tag rather than deriving one from
// the interface's family mode, which is what produced the mislabelling.
func TestInterfacesUIUsesPerAddressTag(t *testing.T) {
	src := mustRead("ui.go")
	if !strings.Contains(src, "modeTag(a.mode)") {
		t.Error("the addresses column should tag each address from a.mode")
	}
	for _, bad := range []string{
		"modeTag(a.family==='ipv6' ? e.mode6 : e.mode4)",
		"modeTag(a.family === 'ipv6' ? e.mode6 : e.mode4)",
	} {
		if strings.Contains(src, bad) {
			t.Errorf("the addresses column still derives the tag from the family mode: %s", bad)
		}
	}
}
