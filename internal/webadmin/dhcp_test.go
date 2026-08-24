package webadmin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/config"
)

func dhcpSubnet() config.DHCPSubnet {
	return config.DHCPSubnet{
		Iface: "eth1", Subnet: "10.1.1.0/24",
		PoolStart: "10.1.1.100", PoolEnd: "10.1.1.200", Router: "10.1.1.1",
		DNS: []string{"10.1.1.1", "9.9.9.9"}, Search: []string{"lan.example"},
		LeaseSeconds: 7200,
	}
}

func renderKeaMap(t *testing.T, c config.DHCPConfig) map[string]any {
	t.Helper()
	b, err := renderKea(c)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v\n%s", err, b)
	}
	return m
}

// The ordinary case, and the properties that make it a working scope rather
// than merely valid JSON.
func TestRenderKeaSubnet(t *testing.T) {
	m := renderKeaMap(t, config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{dhcpSubnet()}})
	d, _ := m["Dhcp4"].(map[string]any)
	if d == nil {
		t.Fatal("no Dhcp4 object")
	}
	subs, _ := d["subnet4"].([]any)
	if len(subs) != 1 {
		t.Fatalf("want 1 subnet, got %d", len(subs))
	}
	s, _ := subs[0].(map[string]any)
	if s["subnet"] != "10.1.1.0/24" {
		t.Errorf("subnet = %v", s["subnet"])
	}
	if s["interface"] != "eth1" {
		t.Errorf("interface = %v", s["interface"])
	}
	// Kea reserves subnet id 0.
	if id, _ := s["id"].(float64); id < 1 {
		t.Errorf("subnet id = %v, must be 1 or greater", s["id"])
	}
	pools, _ := s["pools"].([]any)
	if len(pools) != 1 {
		t.Fatalf("want 1 pool, got %d", len(pools))
	}
	if p, _ := pools[0].(map[string]any); p["pool"] != "10.1.1.100 - 10.1.1.200" {
		t.Errorf("pool = %v", p["pool"])
	}
	// Kea listens only on the interfaces it is named, which is what keeps a
	// DHCP server off every other link on the host.
	ic, _ := d["interfaces-config"].(map[string]any)
	ifs, _ := ic["interfaces"].([]any)
	if len(ifs) != 1 || ifs[0] != "eth1" {
		t.Errorf("interfaces-config = %v, must name exactly the served links", ifs)
	}
	opts := map[string]string{}
	for _, o := range s["option-data"].([]any) {
		om := o.(map[string]any)
		opts[om["name"].(string)] = om["data"].(string)
	}
	if opts["routers"] != "10.1.1.1" {
		t.Errorf("routers = %q", opts["routers"])
	}
	if opts["domain-name-servers"] != "10.1.1.1, 9.9.9.9" {
		t.Errorf("domain-name-servers = %q", opts["domain-name-servers"])
	}
	if opts["domain-search"] != "lan.example" {
		t.Errorf("domain-search = %q", opts["domain-search"])
	}
}

// Rendering through encoding/json is the reason this integration has no
// safeToken guard, unlike the text renderers next door. The property that buys
// is that a hostile string cannot break out of the field it is written in — it
// comes back out of the parser as the same string.
func TestRenderKeaEscapesRatherThanBreakingOut(t *testing.T) {
	s := dhcpSubnet()
	nasty := `x", "malicious": {"a":1}, "z":"`
	s.Search = []string{nasty}
	m := renderKeaMap(t, config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{s}})
	d := m["Dhcp4"].(map[string]any)
	if _, injected := d["malicious"]; injected {
		t.Fatal("an operator string escaped its field and became config structure")
	}
	sub := d["subnet4"].([]any)[0].(map[string]any)
	found := false
	for _, o := range sub["option-data"].([]any) {
		om := o.(map[string]any)
		if om["name"] == "domain-search" && om["data"] == nasty {
			found = true
		}
	}
	if !found {
		t.Error("the search domain did not round-trip through the marshaller intact")
	}
}

// A node in relay mode renders no scopes, and nothing else has to remember to
// check the mode first — EnabledSubnets is where the exclusion lives.
func TestRenderKeaServesNothingOutsideServerMode(t *testing.T) {
	for _, mode := range []config.DHCPMode{config.DHCPOff, config.DHCPRelay} {
		m := renderKeaMap(t, config.DHCPConfig{Mode: mode, Subnets: []config.DHCPSubnet{dhcpSubnet()}})
		d := m["Dhcp4"].(map[string]any)
		if subs, _ := d["subnet4"].([]any); len(subs) != 0 {
			t.Errorf("mode %q rendered %d subnet(s)", mode, len(subs))
		}
	}
}

// A hand-maintained config is set aside, never clobbered, and a file gravinet
// wrote is recognised as its own.
func TestKeaOwnership(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kea-dhcp4.conf")

	if !keaOwned(p) {
		t.Error("an absent file should be ours to write")
	}
	if err := os.WriteFile(p, []byte(`{"Dhcp4":{"subnet4":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if keaOwned(p) {
		t.Error("a hand-written config was claimed as gravinet's")
	}
	if err := os.WriteFile(p, []byte("not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if keaOwned(p) {
		t.Error("an unparseable file was claimed as gravinet's — this must fail safe")
	}

	out, err := renderKea(config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{dhcpSubnet()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if !keaOwned(p) {
		t.Error("gravinet does not recognise its own output")
	}

	// Setting aside preserves what was there, and a second takeover does not
	// overwrite the first backup.
	to, err := setAsideKeaConf(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(to); err != nil {
		t.Errorf("the displaced config is not at %s: %v", to, err)
	}
	if err := os.WriteFile(p, []byte("second hand-written file"), 0o644); err != nil {
		t.Fatal(err)
	}
	to2, err := setAsideKeaConf(p)
	if err != nil {
		t.Fatal(err)
	}
	if to2 == to {
		t.Error("a second takeover overwrote the first backup")
	}
}

// fakeRelay stands in for the real one so the apply path can be exercised
// without binding port 67, which a test cannot do and should not.
type fakeRelay struct{ started, stopped int }

func (f *fakeRelay) Apply(c config.DHCPConfig) error {
	f.started++
	return nil
}
func (f *fakeRelay) Stop() { f.stopped++ }

// The mutual exclusion at the point it actually has to hold: an apply. Leaving
// the previous role running is how a node ends up both serving and relaying,
// which is the failure the single Mode field exists to prevent — but the model
// only makes it unrepresentable in the config, not on the host. This is the
// half that tears the other role down.
func TestApplyDHCPStopsTheRoleThatIsNotSelected(t *testing.T) {
	prev := dhcpRelay
	t.Cleanup(func() { dhcpRelay = prev })

	// Switching to server must stop the relay.
	f := &fakeRelay{}
	dhcpRelay = f
	// No subnets, so nothing is written and no service is driven — the point
	// here is the teardown, not the render.
	if _, err := applyDHCP(config.DHCPConfig{Mode: config.DHCPServer}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f.stopped == 0 {
		t.Error("switching to server mode left the relay running")
	}
	if f.started != 0 {
		t.Error("server mode started the relay")
	}

	// Switching to off must stop it too.
	f = &fakeRelay{}
	dhcpRelay = f
	if _, err := applyDHCP(config.DHCPConfig{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f.stopped == 0 {
		t.Error("switching DHCP off left the relay running")
	}

	// Relay mode starts it, and does not stop it on the way through.
	f = &fakeRelay{}
	dhcpRelay = f
	c := config.DHCPConfig{Mode: config.DHCPRelay, Relay: config.DHCPRelayConfig{
		Interfaces: []string{"eth1"}, Servers: []string{"10.0.0.5"},
	}}
	if _, err := applyDHCP(c); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f.started != 1 {
		t.Errorf("relay mode started the relay %d times, want 1", f.started)
	}
	if f.stopped != 0 {
		t.Error("relay mode stopped the relay it had just been asked to run")
	}
}

// The apply path drives the Kea unit whenever the node is not serving. Checked
// against the source because the alternative is stopping a real service on the
// machine running the tests.
func TestApplyDHCPStopsKeaWhenNotServing(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	i := strings.Index(src, "if c.Mode != config.DHCPServer {")
	j := strings.Index(src, `keaService("stop")`)
	if i < 0 || j < 0 || j < i {
		t.Error("the apply no longer stops Kea when the node is not in server mode")
	}
	// And the preflight is folded into the note, so the same class of silent
	// failure v942 fixed for radvd is reported here too.
	if !strings.Contains(src, "dhcpProblemNote(c)") {
		t.Error("the apply no longer reports the DHCP preflight")
	}
	if !strings.Contains(src, `"problems":`) {
		t.Error("the GET no longer reports per-interface problems")
	}
}

// The preflight itself: an interface whose address is outside the subnet it is
// set to serve gets a Kea that starts, runs, and never answers.
func TestDHCPProblemsOnlyCheckTheActiveMode(t *testing.T) {
	c := config.DHCPConfig{
		Mode:    config.DHCPRelay,
		Subnets: []config.DHCPSubnet{{Iface: "definitely-not-a-nic", Subnet: "10.1.1.0/24", PoolStart: "10.1.1.10", PoolEnd: "10.1.1.20"}},
		Relay:   config.DHCPRelayConfig{Interfaces: []string{"definitely-not-a-nic"}, Servers: []string{"10.0.0.5"}},
	}
	probs := dhcpProblems(c)
	if len(probs) != 1 {
		t.Fatalf("want the relay interface reported once, got %v", probs)
	}
	if !strings.Contains(probs["definitely-not-a-nic"], "relayed") {
		t.Errorf("relay mode reported a server problem: %q", probs["definitely-not-a-nic"])
	}

	c.Mode = config.DHCPServer
	probs = dhcpProblems(c)
	if !strings.Contains(probs["definitely-not-a-nic"], "served") {
		t.Errorf("server mode reported a relay problem: %q", probs["definitely-not-a-nic"])
	}

	c.Mode = config.DHCPOff
	if len(dhcpProblems(c)) != 0 {
		t.Error("a node doing nothing reported problems with not doing it")
	}

	// The note is ordered and carries noteworthy's separator.
	c.Mode = config.DHCPServer
	n := dhcpProblemNote(c)
	if n == "" || !strings.HasSuffix(n, "; ") {
		t.Errorf("note = %q, want a non-empty note ending in the separator", n)
	}
	if got := noteworthy(n); strings.HasSuffix(got, "; ") {
		t.Errorf("noteworthy did not trim the trailing separator: %q", got)
	}
}
