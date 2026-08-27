package webadmin

import (
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/config"
)

// The bundle must carry Kea's own state, not just gravinet's stored intent.
//
// The CONFIG section has always included the DHCP block, which is what this
// node was *told* to serve. The file Kea actually parses is a separate
// artifact at keaConfPath, and nothing in the bundle mentioned it: a reader
// could see the subnets an operator entered and had no way to tell whether
// Kea had ever been given them. Same gap the FRR section closed for zebra.
func TestTshootCarriesKeaState(t *testing.T) {
	s := newTshootDHCPServer(t)
	txt, _ := s.buildTshootText()

	if !strings.Contains(txt, "========== DHCP / KEA ==========") {
		t.Fatal("the bundle has no DHCP/Kea section")
	}
	for _, want := range []string{
		keaConfPath,    // the file Kea parses, named
		"unit active:", // whether it is running, asked of the host
		"stored mode:",
		"kea-dhcp4 installed:",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("the DHCP/Kea section does not report %q", want)
		}
	}
}

// Ownership and Kea's own verdict on the file are reported when there is a
// file to report them about, and not otherwise — keaOwned answers true for an
// absent file (nothing there to protect), so printing it unconditionally
// would tell a reader gravinet wrote a file that does not exist.
//
// keaConfPath is a constant, so there is no test environment in which the
// real file is present; these two are pinned at the source instead of round
// tripped. Both are silent failures if lost: a bundle that omits them looks
// complete.
func TestTshootReportsKeaOwnershipAndParserVerdict(t *testing.T) {
	sec := tshootKeaSource(t)
	if !strings.Contains(sec, "written by gravinet") {
		t.Error("the bundle does not say whether kea-dhcp4.conf is gravinet's — an unmarked file is one gravinet will not touch, which changes how every other line reads")
	}
	if !strings.Contains(sec, "keaTestConf(keaConfPath)") {
		t.Error("the bundle does not ask Kea whether it would accept the file on disk")
	}
}

// The comparison is the part that earns the section.
//
// A node configured to serve, whose kea-dhcp4.conf is absent or is not what
// the stored configuration renders to, is serving something other than what
// its page shows — and that state is invisible in every other section of the
// bundle: CONFIG shows the intent, the runtime report shows a healthy running
// unit, and Kea's journal shows it happily serving the file it was given.
//
// A history restore is how a node gets into it: the restore writes the config
// and never re-renders Kea (applyDHCP is reached from the DHCP page alone),
// so the two are left apart with nothing anywhere to say so.
func TestTshootReportsKeaConfigDivergence(t *testing.T) {
	s := newTshootDHCPServer(t)
	txt, _ := s.buildTshootText()

	if !strings.Contains(txt, "stored config vs the file on disk") {
		t.Fatal("the bundle does not compare the stored configuration against the file Kea parses")
	}
	// No /etc/kea/kea-dhcp4.conf exists in a test environment, and this node
	// is configured to serve — which is the diverged case, and must be
	// reported as one rather than passed over.
	if !strings.Contains(txt, "DIVERGED") {
		t.Errorf("a serving node with no %s on disk was not reported as diverged:\n%s",
			keaConfPath, tshootSection(txt, "DHCP / KEA"))
	}
}

// Mode off, Kea absent, nothing running: there is nothing to say and the
// section must stay out of the bundle rather than pad it with a screenful of
// "absent". The bundle is read by people, and every section that is always
// empty trains them to skip a section that sometimes is not.
func TestTshootOmitsKeaSectionWhenThereIsNothingToSay(t *testing.T) {
	if keaInstalled() || keaActive() {
		t.Skip("Kea is present on this host, so the section is correctly emitted")
	}
	s, _, _ := newTestServer(t)
	cp := filepath.Join(t.TempDir(), "config.json")
	c := config.Default() // DHCP mode off
	if err := c.SaveTo(cp); err != nil {
		t.Fatal(err)
	}
	s.configPath = cp

	txt, _ := s.buildTshootText()
	if strings.Contains(txt, "========== DHCP / KEA ==========") {
		t.Error("the DHCP/Kea section is emitted on a node with no DHCP configured and no Kea installed")
	}
}

// The lease database is named and sized, never included. Its contents are
// every client on the LAN — MAC addresses, hostnames, who was on the network
// and when — which is not diagnostic data and does not belong in a file
// people mail to each other.
func TestTshootDoesNotIncludeLeaseContents(t *testing.T) {
	sec := tshootKeaSource(t)
	if strings.Contains(sec, "ReadFile(keaLeasePath") || strings.Contains(sec, "readKeaLeases") {
		t.Error("the bundle reads the lease database's contents; it must report only that the file exists and its size")
	}
	if !strings.Contains(sec, "Stat(keaLeasePath)") {
		t.Error("the bundle does not stat the lease database at all")
	}
}

// tshootKeaSource returns the DHCP/Kea section of tshoot.go.
func tshootKeaSource(t *testing.T) string {
	t.Helper()
	src := readSourceFile(t, "tshoot.go")
	i := strings.Index(src, `sec("DHCP / KEA")`)
	if i < 0 {
		t.Fatal("no DHCP/Kea section in tshoot.go")
	}
	sec := src[i:]
	if j := strings.Index(sec, `sec("CONFIG`); j > 0 {
		sec = sec[:j]
	}
	return sec
}

// newTshootDHCPServer is a node configured to serve one subnet, which is the
// state every assertion above is about.
func newTshootDHCPServer(t *testing.T) *Server {
	t.Helper()
	s, _, _ := newTestServer(t)
	cp := filepath.Join(t.TempDir(), "config.json")
	c := config.Default()
	c.DHCP.Mode = config.DHCPServer
	c.DHCP.Subnets = []config.DHCPSubnet{{
		Iface:     "eth1",
		Subnet:    "10.1.1.0/24",
		PoolStart: "10.1.1.100",
		PoolEnd:   "10.1.1.200",
		Router:    "10.1.1.1",
	}}
	if err := c.SaveTo(cp); err != nil {
		t.Fatal(err)
	}
	s.configPath = cp
	return s
}

// tshootSection returns one section of a bundle, for a readable failure.
func tshootSection(txt, name string) string {
	i := strings.Index(txt, "========== "+name+" ==========")
	if i < 0 {
		return "(section absent)"
	}
	rest := txt[i:]
	if j := strings.Index(rest[10:], "\n=========="); j > 0 {
		return rest[:j+10]
	}
	return rest
}
