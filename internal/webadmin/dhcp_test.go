package webadmin

import (
	"strings"
	"testing"

	"gravinet/internal/config"
)

// fakeRelay stands in for the real one so the apply path can be exercised
// without binding port 67, which a test cannot do and should not.
//
// It mirrors liveRelay's own first move — a config with nothing to relay is a
// no-op, not a start — so "started" here means the same thing it does on a
// real host. A fake that counted every call instead would report a relay
// running for a half-written row that the real one would have ignored.
type fakeRelay struct {
	started, stopped int
	links            []config.DHCPRelayLink
}

func (f *fakeRelay) Apply(c config.DHCPConfig) error {
	f.stopped++ // liveRelay.Apply stops before it starts
	f.links = nil
	if !c.RelayActive() {
		return nil
	}
	f.started++
	f.links = c.EnabledLinks()
	return nil
}
func (f *fakeRelay) Stop() { f.stopped++; f.links = nil }

// Listening mirrors liveRelay's: the links a running relay actually bound.
// The fake binds nothing, so it reports what it was asked to run — enough for
// the runtime report to be exercised without a socket.
func (f *fakeRelay) Listening() []string {
	var out []string
	for _, l := range f.links {
		out = append(out, l.Iface)
	}
	return out
}

// Switching the relay off has to stop it, not merely stop starting it. The
// apply is the only thing between a stored mode and a socket still bound to
// port 67 on a link the operator has just told the node to leave alone.
func TestApplyDHCPStopsTheRelayWhenItIsOff(t *testing.T) {
	f := &fakeRelay{}
	withFakeRelay(t, f)
	if _, err := applyDHCP(config.DHCPConfig{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f.stopped == 0 {
		t.Error("switching DHCP off left the relay running")
	}
	if f.started != 0 {
		t.Error("the off mode started a relay")
	}

	// And relay mode starts it.
	f = &fakeRelay{}
	withFakeRelay(t, f)
	c := config.DHCPConfig{Mode: config.DHCPRelay, Relay: config.DHCPRelayConfig{
		Links: []config.DHCPRelayLink{{Iface: "eth1", Servers: []string{"10.0.0.5"}}},
	}}
	if _, err := applyDHCP(c); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f.started != 1 {
		t.Errorf("relay mode started the relay %d times, want 1", f.started)
	}
}

// The apply reports the preflight rather than swallowing it, so the same class
// of silent failure v942 fixed for radvd is reported here too.
func TestApplyDHCPReportsThePreflight(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	if !strings.Contains(src, "dhcpProblemNote(c)") {
		t.Error("the apply no longer reports the DHCP preflight")
	}
	if !strings.Contains(src, `"problems":`) {
		t.Error("the GET no longer reports per-interface problems")
	}
}

// Nothing in this package drives a DHCP server any more. Checked as text
// because the failure it guards against is a reintroduction, and a
// reintroduction compiles.
//
// Identifiers rather than the word: StartDHCPRelay's own warning has to say
// "Kea" out loud, because naming the daemon still running is the whole of what
// that sentence is for. What must not come back is anything that renders its
// config, installs it, or drives its unit.
func TestNoDHCPServerIsDrivenFromHere(t *testing.T) {
	for _, gone := range []string{
		"renderKea(", "keaService(", "keaInstalled(", "installKea(", "keaActive(",
		"keaOwned(", "keaConfPath", "keaLeasePath", "keaStopAndDisable(",
		"config.DHCPSubnet", "EnabledSubnets(", "subnet4", "dhcp-socket-type",
	} {
		for _, f := range []string{"dhcp_apply.go", "dhcp_runtime.go", "dhcp_preflight.go", "tshoot.go"} {
			if strings.Contains(mustRead(f), gone) {
				t.Errorf("%s still reaches for %s; gravinet stopped serving DHCP in v988", f, gone)
			}
		}
	}
	// And the endpoints that only the server half had are gone from the mux,
	// so a stale page cannot still reach a handler that no longer means
	// anything.
	mux := mustRead("webadmin.go")
	for _, gone := range []string{"/api/dhcp-leases", "/api/dhcp/relay-iface"} {
		if strings.Contains(mux, gone) {
			t.Errorf("%s is still routed", gone)
		}
	}
}

// The preflight is about the links actually in service. A parked link, or a
// whole relay configuration sitting unused while the mode is off, is not
// supposed to be doing anything — reporting that it is not would hand the
// operator their own request back as a fault.
func TestDHCPProblemsOnlyCheckLinksInService(t *testing.T) {
	c := config.DHCPConfig{
		Mode:  config.DHCPRelay,
		Relay: config.DHCPRelayConfig{Links: []config.DHCPRelayLink{{Iface: "definitely-not-a-nic", Servers: []string{"10.0.0.5"}}}},
	}
	probs := dhcpProblems(c)
	if len(probs) != 1 {
		t.Fatalf("want the relay interface reported once, got %v", probs)
	}
	if !strings.Contains(probs["definitely-not-a-nic"], "relayed") {
		t.Errorf("the reason does not say what is not happening: %q", probs["definitely-not-a-nic"])
	}

	// The note is ordered and carries noteworthy's separator.
	n := dhcpProblemNote(c)
	if n == "" || !strings.HasSuffix(n, "; ") {
		t.Errorf("note = %q, want a non-empty note ending in the separator", n)
	}
	if got := noteworthy(n); strings.HasSuffix(got, "; ") {
		t.Errorf("noteworthy did not trim the trailing separator: %q", got)
	}

	c.Mode = config.DHCPOff
	if len(dhcpProblems(c)) != 0 {
		t.Error("a node doing nothing reported problems with not doing it")
	}
	c.Mode = config.DHCPRelay
	c.Relay.Links[0].Disabled = true
	if len(dhcpProblems(c)) != 0 {
		t.Error("a parked link was reported as failing to relay")
	}
}

// The relay is a table of links from v949, so every row op the page posts has
// to exist in the handler. The two are joined only by the op string, which is
// exactly the kind of seam that goes quietly wrong: a renamed op leaves a
// button that posts something the handler answers with "unknown op".
func TestDHCPRelayRowOpsAreHandled(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	for _, op := range []string{"relay-add", "relay-update", "relay-delete", "relay-enable", "relay-disable"} {
		if !strings.Contains(src, `"`+op+`"`) {
			t.Errorf("the page posts %q but the handler does not implement it", op)
		}
		if !strings.Contains(indexHTML, op) {
			t.Errorf("the handler implements %q but no control posts it", op)
		}
	}
	// The pre-v949 whole-form op is gone on both sides. Leaving it in the
	// handler would mean a stale client could still overwrite every link at
	// once, which is the shape this change exists to remove.
	if strings.Contains(src, `case "relay":`) {
		t.Error("the handler still accepts the pre-v949 whole-form relay op")
	}
	// The subnet ops went with the server in v988. A stale page still posting
	// one must be answered with "unknown op" rather than quietly editing
	// something.
	for _, op := range []string{`case "add"`, `case "update"`, `case "delete"`, `case "enable"`} {
		if strings.Contains(src, op) {
			t.Errorf("the handler still implements the server-side op %s", op)
		}
	}
}

// Asking for the retired server mode is refused, and refused before it is
// stored. Config.Validate would fold it to off on the way past, so a handler
// that validated only on the way out would answer somebody selecting a role
// that no longer exists with a silent success.
func TestDHCPHandlerRefusesTheRetiredServerMode(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	i := strings.Index(src, `case "mode":`)
	j := strings.Index(src, "config.ValidDHCPMode(m)")
	k := strings.Index(src, "d.Mode = m")
	if i < 0 || j < 0 || k < 0 || !(i < j && j < k) {
		t.Error("the mode op no longer validates before it stores, so a retired mode would be silently accepted")
	}
}

// A link with no server is stored rather than refused: the row exists so the
// operator can fill the rest in, and rejecting it would lose the interface
// they just chose. It must not start a relay in that state, though.
func TestDHCPRelayHalfWrittenLinkIsStoredButNotRun(t *testing.T) {
	c := config.DHCPConfig{Mode: config.DHCPRelay, Relay: config.DHCPRelayConfig{
		Links: []config.DHCPRelayLink{{Iface: "eth1"}},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("a half-written relay row was refused on save: %v", err)
	}
	if c.RelayActive() {
		t.Error("a link with nowhere to forward to started a relay")
	}
	f := &fakeRelay{}
	withFakeRelay(t, f)
	if _, err := applyDHCP(c); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f.started != 0 {
		t.Error("the apply started a relay that had nothing to forward to")
	}
}

// Parking one link must not take the others down with it, and the link that
// reaches the relay must be the one that was left enabled. This is the state
// column doing its job at the layer below the page.
func TestDHCPRelayParkedLinkIsExcluded(t *testing.T) {
	c := config.DHCPConfig{Mode: config.DHCPRelay, Relay: config.DHCPRelayConfig{
		Links: []config.DHCPRelayLink{
			{Iface: "eth1", Servers: []string{"10.0.0.5"}, Disabled: true},
			{Iface: "eth2", Servers: []string{"10.0.0.6"}, MaxHops: 8},
		},
	}}
	f := &fakeRelay{}
	withFakeRelay(t, f)
	if _, err := applyDHCP(c); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f.started != 1 {
		t.Fatalf("relay started %d times, want 1", f.started)
	}
	if len(f.links) != 1 || f.links[0].Iface != "eth2" {
		t.Fatalf("want only the enabled link relayed, got %v", f.links)
	}
	// Each link carries its own hop limit now, rather than sharing one.
	if f.links[0].MaxHops != 8 {
		t.Errorf("the link's own hop limit was lost: %d", f.links[0].MaxHops)
	}
}

// The one thing v988 says out loud. A node that was serving keeps its Kea
// running — gravinet does not reach out to stop a daemon during an upgrade —
// so the log line is the only evidence left that a server nothing in the
// console admits to is still handing out leases.
func TestRetiredServerModeIsAnnouncedAtStartup(t *testing.T) {
	src := mustRead("dhcp_apply.go")
	i := strings.Index(src, "func StartDHCPRelay(")
	if i < 0 {
		t.Fatal("StartDHCPRelay is gone")
	}
	body := src[i:]
	if !strings.Contains(body, "RetiredServerMode()") {
		t.Error("startup no longer checks for a config that was serving, so the upgrade is silent")
	}
	for _, want := range []string{"still running", "kea-dhcp4.conf"} {
		if !strings.Contains(body, want) {
			t.Errorf("the startup warning does not mention %q, so it does not say what is still serving", want)
		}
	}
}
