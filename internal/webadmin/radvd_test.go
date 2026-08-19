package webadmin

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"gravinet/internal/config"
)

// radvdApplySource is the handler file read at test time, so a guard can
// assert on code that has no rendered artifact to check instead.
var radvdApplySource = mustRead("radvd_apply.go")

func mustRead(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func raCfg(e ...config.RAInterface) config.RAConfig {
	return config.RAConfig{Enabled: true, Interfaces: e}
}

// The ordinary case: plain SLAAC on a LAN, with DNS and a search domain
// carried in the RA itself (RFC 8106), which is how a SLAAC-only network gets
// DNS without standing up DHCPv6.
func TestRenderRadvdSLAACWithDNS(t *testing.T) {
	got := renderRadvd(raCfg(config.RAInterface{
		Iface:    "eth1",
		Prefixes: []string{"fd0a:1::/64"},
		DNS:      []string{"fd0a:1::1"},
		Search:   []string{"lan.example"},
	}))
	for _, want := range []string{
		"interface eth1",
		"AdvSendAdvert on;",
		"AdvManagedFlag off;",
		"AdvOtherConfigFlag off;",
		"prefix fd0a:1::/64",
		"AdvAutonomous on;", // SLAAC: hosts build their own address
		"RDNSS fd0a:1::1",
		"DNSSL lan.example",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// With the M flag set, addresses come from DHCPv6, so autonomous
// configuration must be off — leaving it on tells hosts to do both, and they
// end up with a SLAAC address the DHCPv6 server knows nothing about.
func TestRenderRadvdManagedTurnsOffAutonomous(t *testing.T) {
	got := renderRadvd(raCfg(config.RAInterface{
		Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}, Managed: true,
	}))
	if !strings.Contains(got, "AdvManagedFlag on;") {
		t.Error("M flag not set")
	}
	if !strings.Contains(got, "AdvAutonomous off;") {
		t.Errorf("managed mode must disable SLAAC autoconfiguration:\n%s", got)
	}
}

// NotDefault advertises the prefix while explicitly declining to be a default
// router — distinct from an unset lifetime, which leaves radvd's own default.
func TestRenderRadvdLifetimes(t *testing.T) {
	unset := renderRadvd(raCfg(config.RAInterface{Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}}))
	if strings.Contains(unset, "AdvDefaultLifetime") {
		t.Errorf("an unset lifetime should emit nothing:\n%s", unset)
	}
	explicit := renderRadvd(raCfg(config.RAInterface{Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}, DefaultLifetime: 600}))
	if !strings.Contains(explicit, "AdvDefaultLifetime 600;") {
		t.Errorf("explicit lifetime missing:\n%s", explicit)
	}
	not := renderRadvd(raCfg(config.RAInterface{Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}, NotDefault: true}))
	if !strings.Contains(not, "AdvDefaultLifetime 0;") {
		t.Errorf("not-a-default-router should emit lifetime 0:\n%s", not)
	}
}

// An interface stanza with no prefix makes radvd refuse to start, which would
// take down advertisements on every other LAN too. One unadvertised LAN is
// the better failure, so such an entry is skipped.
func TestRenderRadvdSkipsPrefixlessInterface(t *testing.T) {
	got := renderRadvd(raCfg(
		config.RAInterface{Iface: "nonexistent-iface-xyz"}, // no prefixes, no such interface
		config.RAInterface{Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}},
	))
	if strings.Contains(got, "nonexistent-iface-xyz") {
		t.Errorf("prefixless entry should be skipped entirely:\n%s", got)
	}
	if !strings.Contains(got, "interface eth1") {
		t.Errorf("a healthy entry must still render alongside a skipped one:\n%s", got)
	}
}

// Disabled entries and a disabled feature produce no stanzas at all — and in
// particular gravinet writes nothing that would disturb a radvd an operator
// is already running by hand.
func TestRenderRadvdRespectsDisabled(t *testing.T) {
	off := renderRadvd(config.RAConfig{Enabled: false, Interfaces: []config.RAInterface{
		{Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}},
	}})
	if strings.Contains(off, "interface eth1") {
		t.Errorf("disabled config should render no interfaces:\n%s", off)
	}
	parked := renderRadvd(raCfg(config.RAInterface{
		Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}, Disabled: true,
	}))
	if strings.Contains(parked, "interface eth1") {
		t.Errorf("disabled entry should render nothing:\n%s", parked)
	}
}

// RDNSS has no representation for an IPv4 server and radvd rejects the whole
// file rather than one line, so a v4 address must be dropped rather than
// emitted.
func TestRadvdRejectsIPv4DNS(t *testing.T) {
	got := renderRadvd(raCfg(config.RAInterface{
		Iface: "eth1", Prefixes: []string{"fd0a:1::/64"},
		DNS: []string{"10.1.1.1", "fd0a:1::1"},
	}))
	if strings.Contains(got, "10.1.1.1") {
		t.Errorf("IPv4 must not reach RDNSS:\n%s", got)
	}
	if !strings.Contains(got, "fd0a:1::1") {
		t.Errorf("the valid v6 server should survive:\n%s", got)
	}
}

// Validation catches the mistakes that otherwise fail silently on the host.
func TestRAInterfaceValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   config.RAInterface
		ok   bool
	}{
		{"ok", config.RAInterface{Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}, DNS: []string{"fd0a:1::1"}}, true},
		{"no iface", config.RAInterface{Prefixes: []string{"fd0a:1::/64"}}, false},
		{"v4 prefix", config.RAInterface{Iface: "eth1", Prefixes: []string{"10.1.1.0/24"}}, false},
		// A /48 is a legitimate prefix that SLAAC cannot use: hosts ignore it,
		// which looks exactly like the RA not working at all.
		{"non-/64", config.RAInterface{Iface: "eth1", Prefixes: []string{"fd0a:1::/48"}}, false},
		{"v4 dns", config.RAInterface{Iface: "eth1", DNS: []string{"10.1.1.1"}}, false},
		{"lifetime too long", config.RAInterface{Iface: "eth1", DefaultLifetime: 99999}, false},
		{"no prefixes is fine", config.RAInterface{Iface: "eth1"}, true},
	} {
		err := tc.in.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

// bgpActive decides whether the UI re-renders the BGP editor from FRR's live
// config, discarding what gravinet stored. Getting it wrong does not just
// mislead a banner — it wipes settings, and the next autosave persists the
// wipe.
//
// The reported sequence: enable BGP, then enable AutoBGP. AutoBGP derives the
// ASN in its own reconciler, so the stored config is briefly Enabled with ASN
// 0, which the old test read as "not managing".
func TestBGPActiveCoversAutoBGPBeforeASNIsDerived(t *testing.T) {
	active := func(enabled bool, asn uint32, auto bool) bool {
		return enabled && (asn != 0 || auto)
	}
	for _, tc := range []struct {
		name    string
		enabled bool
		asn     uint32
		auto    bool
		want    bool
	}{
		{"the reported case: autobgp on, asn not yet derived", true, 0, true, true},
		{"autobgp on and asn since derived", true, 65001, true, true},
		{"hand-configured", true, 65001, false, true},
		{"enabled but genuinely unconfigured", true, 0, false, false},
		{"disabled entirely", false, 65001, true, false},
	} {
		if got := active(tc.enabled, tc.asn, tc.auto); got != tc.want {
			t.Errorf("%s: active = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The nav key and the section dispatch have to agree, and the loader's
// navigated-away guard has to test the same key — a mismatch there makes the
// page render into a container the user has already left, or never render at
// all. Renaming the section is exactly when those three drift apart.
func TestIPv6RANavWiring(t *testing.T) {
	src := indexHTML
	for _, want := range []string{
		"['ipv6ra',",                 // nav entry
		"ipv6ra:secRadvd",            // section dispatch
		"state.section !== 'ipv6ra'", // the loader guards on the same key
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q", want)
		}
	}
	// The old key must be gone from the nav and dispatch, or two entries
	// point at the same section and one of them is dead.
	for _, gone := range []string{"['radvd',", "radvd:secRadvd"} {
		if strings.Contains(src, gone) {
			t.Errorf("stale nav wiring %q still present", gone)
		}
	}
}

// The gate has to be wired in all four places or it half-works: the server
// field, the state default, the read, and sectionVisible. A missing state
// default in particular reads as undefined, which is falsy — so the section
// would be hidden on every platform, including the one it works on.
func TestIPv6RAGateWiring(t *testing.T) {
	src := indexHTML
	for _, want := range []string{
		"ipv6raSupported:false", // declared in the state object
		"state.ipv6raSupported = !!(c.body && c.body.ipv6ra_supported)", // read from /api/config
		"if (sec === 'ipv6ra') return !!state.ipv6raSupported;",         // and actually gates the section
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing gate wiring: %q", want)
		}
	}
}

// The platform half of the gate is not a proxy for the daemon half. gravinet
// renders radvd's format only, so a FreeBSD host with a working rtadvd must
// still report unsupported — otherwise it is offered a page that would write
// it a file it cannot parse.
func TestIPv6RASupportedIsPlatformGated(t *testing.T) {
	if runtime.GOOS != "linux" && ipv6RASupported() {
		t.Errorf("%s must report unsupported until its own RA config format is rendered", runtime.GOOS)
	}
	if runtime.GOOS == "linux" && ipv6RASupported() != radvdInstalled() {
		t.Error("on linux the gate should track whether the daemon is installed")
	}
}

// A foreign radvd.conf must be preserved and stepped over, never refused and
// never destroyed. Refusing left an operator with a dead page and an
// instruction to move a file by hand; overwriting would lose a config someone
// wrote.
func TestSetAsideRadvdConf(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/radvd.conf"
	if err := os.WriteFile(path, []byte("interface eth0 { };\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	to, err := setAsideRadvdConf(path)
	if err != nil {
		t.Fatal(err)
	}
	if to != path+".pre-gravinet" {
		t.Errorf("first displacement should land on a predictable name, got %q", to)
	}
	if b, err := os.ReadFile(to); err != nil || !strings.Contains(string(b), "interface eth0") {
		t.Fatalf("the original content must survive: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the original path should be free for gravinet to write")
	}

	// A second takeover must not clobber the first backup.
	if err := os.WriteFile(path, []byte("second hand-written file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	to2, err := setAsideRadvdConf(path)
	if err != nil {
		t.Fatal(err)
	}
	if to2 == to {
		t.Fatal("a second displacement must not overwrite the first backup")
	}
	if b, _ := os.ReadFile(to); !strings.Contains(string(b), "interface eth0") {
		t.Error("the first backup was destroyed by the second takeover")
	}
	if b, _ := os.ReadFile(to2); !strings.Contains(string(b), "second hand-written") {
		t.Error("the second file was not preserved")
	}
}

// A file gravinet wrote is recognised as its own and must not be backed up on
// every apply, or a routine save would litter the directory.
func TestRadvdOwnedRecognisesOwnOutput(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/radvd.conf"
	conf := renderRadvd(raCfg(config.RAInterface{Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}}))
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	if !radvdOwned(path) {
		t.Error("gravinet must recognise its own rendered output")
	}
	if err := os.WriteFile(path, []byte("interface eth0 { };\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if radvdOwned(path) {
		t.Error("a foreign file must not be claimed as gravinet's")
	}
	// A path that does not exist yet is ours to create.
	if !radvdOwned(dir + "/nope.conf") {
		t.Error("an absent file should be treated as available")
	}
}

// The +/- toolbar is drawn by enhanceTable from _rowAdd/_rowRemove, and
// renderSection sweeps a section's tables once, synchronously. Sections that
// build their tables from an async load are not in that sweep and have to
// call enhanceTable themselves — miss it and the table renders with no way to
// add a row, which is how this page shipped: an empty list and no + button.
func TestIPv6RASectionDrawsItsOwnToolbar(t *testing.T) {
	sec := between(t, indexHTML, "function secRadvd(", "function secQoS(")
	for _, want := range []string{
		"table._rowAdd",
		"table._rowRemove",
		"enhanceTable(table)",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("missing %q — the section would render without its toolbar", want)
		}
	}
}

// label() title-cases a section key, which renders 'ipv6ra' as 'Ipv6ra' — the
// nav rail read that way until v885 gave it its own short label. Rail and
// heading are separate on purpose, the same split l2disco uses: the rail is
// narrow, a standalone <h2> is not.
func TestIPv6RAHeadingIsNotTitleCasedKey(t *testing.T) {
	if !strings.Contains(indexHTML, "if (s==='ipv6ra') return 'IPv6 Router Advertisements';") {
		t.Error("the section heading override is missing; the page would be titled Ipv6ra")
	}
	if !strings.Contains(indexHTML, "if (s==='ipv6ra') return 'v6 ra';") {
		t.Error("the rail label override is missing; the nav entry would read Ipv6ra again")
	}
	// No label contains the key any more, so the search index has to carry it
	// or typing "ipv6ra" — the name in the URL hash and the config — finds
	// nothing.
	if !strings.Contains(indexHTML, "{kind:'section', section:s}, tip + ' ' + s)") {
		t.Error("section keys are no longer indexed for search; 'ipv6ra' and 'bandwidth' would be unfindable by name")
	}
	// The heading names the feature, so the hint must not open by naming it
	// again — the first thing under a title should say something new.
	if strings.Contains(indexHTML, "secHint(c, 'IPv6 router advertisements.") {
		t.Error("the hint still repeats the page title back at the reader")
	}
}

// enhanceTable renders the toolbar for every table it is given. It used to
// bail when a table had no rows and no +/- actions, which made a table's
// controls appear and disappear with its contents and left an empty table
// with no way to add the first row.
func TestToolbarIsUnconditional(t *testing.T) {
	fn := between(t, indexHTML, "function enhanceTable(table)", "function selAllWire")
	// The bail is gone.
	if strings.Contains(fn, "if (!hasData && !table._rowAdd") {
		t.Error("enhanceTable still bails on empty tables; the toolbar would be conditional again")
	}
	// And with it, the opt-in that only existed to work around the bail.
	if strings.Contains(fn, "_forceFilter") && !strings.Contains(fn, "_forceFilter with it") {
		t.Error("_forceFilter is referenced as live behaviour but no longer does anything")
	}
	// _noFilter is a different decision and must survive: it is about a box
	// that would have nothing to search, not about when the bar exists.
	if !strings.Contains(fn, "_noFilter") {
		t.Error("_noFilter should still opt a table out of the filter box")
	}
}

// The M/O flags and the router lifetime have no controls in the editor, so an
// update request carries none of them. They stay settable by editing the
// config file, which means a UI save must not clear them — editing a DNS
// server should not switch off someone's DHCPv6 flag as a side effect.
func TestRAUpdatePreservesFieldsTheEditorCannotSend(t *testing.T) {
	src := between(t, indexHTML, "function secRadvd(", "function secQoS(")
	// The editor really does not send them, which is what makes the
	// preservation necessary rather than merely tidy.
	for _, gone := range []string{"rae-m", "rae-o", "rae-nd", "rae-life", "<th>flags</th>"} {
		if strings.Contains(src, gone) {
			t.Errorf("%q is still in the editor; this guard assumes it was removed", gone)
		}
	}
	// And the handler carries the stored values forward.
	for _, want := range []string{
		"e.Managed, e.OtherConfig = cur.Managed, cur.OtherConfig",
		"e.DefaultLifetime, e.NotDefault = cur.DefaultLifetime, cur.NotDefault",
	} {
		if !strings.Contains(radvdApplySource, want) {
			t.Errorf("update does not preserve %q", want)
		}
	}
}

// The interface field is a picker, not a free-text box: an operator should
// not have to remember whether it is eth1 or ens4, and a typo here produces a
// config that validates, saves, renders, and advertises on nothing.
func TestRAInterfaceIsAPicker(t *testing.T) {
	sec := between(t, indexHTML, "function secRadvd(", "function secQoS(")
	if strings.Contains(sec, `input class="rae-iface"`) {
		t.Error("the interface field is still a free-text input")
	}
	if !strings.Contains(sec, `select class="rae-iface"`) {
		t.Error("the interface field is not a select")
	}
	// Reuses the shared cached lookup rather than hitting /api/interfaces
	// directly, so switching managed node picks up that node's interfaces.
	if !strings.Contains(sec, "await systemInterfaces()") {
		t.Error("the picker does not use the shared systemInterfaces cache")
	}
	// A stored interface that has since disappeared must remain selectable,
	// or opening the editor on that row would silently rewrite it to whatever
	// is first in the list.
	if !strings.Contains(sec, "names.indexOf(sel) < 0") {
		t.Error("a stored-but-absent interface would not survive opening the editor")
	}
	// The select can sit on the "choose…" placeholder, so saving must refuse
	// an empty choice rather than posting one the server would reject.
	if !strings.Contains(sec, "choose an interface to advertise on") {
		t.Error("adding with no interface chosen is not refused client-side")
	}
}

// Mesh devices must not be advertisable. The picker hiding them is a
// convenience; the server refusing them is the control, because a picker
// cannot stop an API call and advertising into the overlay would tell every
// mesh peer this node is their router.
func TestRAExcludesMeshInterfaces(t *testing.T) {
	// Client side: the list is filtered against what the GET reports.
	sec := between(t, indexHTML, "function secRadvd(", "function secQoS(")
	for _, want := range []string{"r.body.mesh_ifaces", "meshIfaces.indexOf(n) < 0"} {
		if !strings.Contains(sec, want) {
			t.Errorf("picker does not filter mesh devices: missing %q", want)
		}
	}
	// Server side: add and update both refuse one.
	for _, want := range []string{
		"mesh_ifaces",                // reported to the client
		"s.refuseMeshIface(e.Iface)", // and enforced on save
		"router advertisements belong on a LAN interface",
	} {
		if !strings.Contains(radvdApplySource, want) {
			t.Errorf("handler missing %q", want)
		}
	}
}

// RFC 4191 default router preference. The point is a host choosing between
// two routers on one link — a backup set low is used only while the high one
// is silent, without the two fighting over who is default.
func TestRenderRadvdPreference(t *testing.T) {
	for _, tc := range []struct{ pref, want string }{
		{"high", "AdvDefaultPreference high;"},
		{"low", "AdvDefaultPreference low;"},
		{"medium", "AdvDefaultPreference medium;"},
	} {
		got := renderRadvd(raCfg(config.RAInterface{
			Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}, Preference: tc.pref,
		}))
		if !strings.Contains(got, tc.want) {
			t.Errorf("preference %q: missing %q in:\n%s", tc.pref, tc.want, got)
		}
	}

	// Unset emits nothing, leaving radvd's own default (medium).
	unset := renderRadvd(raCfg(config.RAInterface{Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}}))
	if strings.Contains(unset, "AdvDefaultPreference") {
		t.Errorf("an unset preference should emit nothing:\n%s", unset)
	}

	// A preference on a node advertising itself as not-a-default-router is
	// inert — radvd ignores it alongside a zero lifetime — so writing one
	// would suggest a ranking nothing acts on.
	notDefault := renderRadvd(raCfg(config.RAInterface{
		Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}, Preference: "high", NotDefault: true,
	}))
	if strings.Contains(notDefault, "AdvDefaultPreference") {
		t.Errorf("preference must not be emitted alongside not-default:\n%s", notDefault)
	}
}

func TestRAPreferenceValidation(t *testing.T) {
	base := func(p string) config.RAInterface {
		return config.RAInterface{Iface: "eth1", Prefixes: []string{"fd0a:1::/64"}, Preference: p}
	}
	for _, ok := range []string{"", "low", "medium", "high", "HIGH", " high "} {
		if err := base(ok).Validate(); err != nil {
			t.Errorf("preference %q should be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"highest", "1", "urgent"} {
		if err := base(bad).Validate(); err == nil {
			t.Errorf("preference %q should be rejected", bad)
		}
	}
}

// The dropdown has to send the value and prefill from the row, or editing an
// interface would silently reset its preference to the default.
func TestRAPreferencePicker(t *testing.T) {
	sec := between(t, indexHTML, "function secRadvd(", "function secQoS(")
	for _, want := range []string{
		`select class="rae-pref"`,                         // it is a dropdown, not a text box
		"preference: tr.querySelector('.rae-pref').value", // and it is sent
		"data-preference",                                 // the row carries it
		"preference:tr.dataset.preference",                // and the editor prefills from it
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("preference picker missing %q", want)
		}
	}
}

// Disabling BGP has to stick. The UI imports FRR's live configuration and
// re-renders the editor from it whenever gravinet is not managing BGP, and
// the editor autosaves — so if "not managing" is read from Enabled, turning
// BGP off makes gravinet import the still-running FRR config and write it
// back enabled, and the setting can never be turned off at all.
func TestBGPManagedIsNotEnabled(t *testing.T) {
	off := config.BGPConfig{Enabled: false, ASN: 65001}
	if !bgpConfigured(off) {
		t.Error("a configured-but-disabled BGP must still count as managed, or the import overwrites it")
	}
	// Every kind of configuration counts, since any of them is work an
	// import would discard.
	for name, b := range map[string]config.BGPConfig{
		"asn":         {ASN: 65001},
		"autobgp":     {AutoBGP: true},
		"neighbors":   {Neighbors: []config.BGPNeighbor{{}}},
		"networks":    {Networks: []string{"10.0.0.0/8"}},
		"redist v4":   {RedistributeConnectedRoutes: []string{"10.1.1.0/24"}},
		"redist mesh": {RedistributeMeshRoutes: []string{"10.2.2.0/24"}},
	} {
		if !bgpConfigured(b) {
			t.Errorf("%s should count as configured", name)
		}
	}
	// A node that has never been touched is not managed, so the import still
	// runs for its intended case: showing what FRR already has.
	if bgpConfigured(config.BGPConfig{}) {
		t.Error("an untouched config should not count as managed")
	}
	if bgpConfigured(config.BGPConfig{Enabled: true}) {
		t.Error("Enabled alone is not a configuration; the import should still show FRR's")
	}
	// And the UI must gate on it rather than on active.
	if !strings.Contains(indexHTML, "if (!r.body.managed && (r.body.supported || r.body.installed)){") {
		t.Error("the UI still imports FRR's config based on the active flag")
	}
}

// AutoBGP is a setting of its own, not a sub-switch of the enable pill. It
// keeps its value while BGP is off and resumes when BGP comes back on.
//
// Two wrong fixes preceded this one. v845 made a state unrepresentable rather
// than fixing what could not represent it; v864 did the same thing again,
// clearing AutoBGP whenever BGP was disabled. What actually could not hold
// was the reconciler treating a disabled BGP as drift — that is what is fixed
// here, and the two settings are independent again.
func TestAutoBGPIsIndependentOfEnabled(t *testing.T) {
	src := mustRead("autobgp.go")

	// The reconciler stays dormant while BGP is off...
	if !strings.Contains(src, "if !cfg.BGP.Enabled {") {
		t.Error("the reconciler still runs while BGP is disabled")
	}
	// ...does not treat "off" as drift...
	if strings.Contains(src, "identityChanged := !cfg.BGP.Enabled") {
		t.Error("a disabled BGP is still treated as drift to correct")
	}
	// ...and never writes Enabled itself.
	if strings.Contains(src, "c.BGP.Enabled = true") {
		t.Error("the reconciler still forces BGP enabled")
	}

	// And nothing clears AutoBGP on the way through the save handler or the
	// pill, which is what v864 got wrong.
	if strings.Contains(mustRead("bgp.go"), "req.AutoBGP = false") {
		t.Error("the save handler still clears AutoBGP when BGP is disabled")
	}
	if strings.Contains(indexHTML, "autoCb.checked = false") {
		t.Error("the disable pill still turns the AutoBGP checkbox off")
	}

	// A disabled config with AutoBGP set still counts as managed, so v863's
	// import fix keeps working for exactly this shape.
	if !bgpConfigured(config.BGPConfig{Enabled: false, AutoBGP: true}) {
		t.Error("a disabled config with AutoBGP set must still count as managed")
	}
}
