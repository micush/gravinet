//go:build linux

package hostnet

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func spec(iface string, addrs []string, gw4, gw6 string) Spec {
	s := Spec{Iface: iface}
	for _, a := range addrs {
		s.Addrs = append(s.Addrs, netip.MustParsePrefix(a))
	}
	if gw4 != "" {
		s.GW4 = netip.MustParseAddr(gw4)
	}
	if gw6 != "" {
		s.GW6 = netip.MustParseAddr(gw6)
	}
	return s
}

// The file-writing backends are the ones whose output can be checked without
// the platform that consumes it, so they are checked closely — a malformed
// unit or YAML is not noticed until a reboot, which is the worst time.
func TestNetworkdUnit(t *testing.T) {
	dir := t.TempDir()
	got := renderNetworkd(spec("eth1", []string{"10.1.1.5/24", "fd00::5/64"}, "10.1.1.1", "fd00::1"))
	for _, want := range []string{
		Marker, "[Match]", "Name=eth1", "[Network]",
		"Address=10.1.1.5/24", "Address=fd00::5/64",
		"Gateway=10.1.1.1", "Gateway=fd00::1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("networkd unit missing %q:\n%s", want, got)
		}
	}
	// And it refuses to replace a file it did not write.
	p := filepath.Join(dir, "x.network")
	if err := os.WriteFile(p, []byte("# hand written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeOwned(p, "anything", 0o644); err == nil {
		t.Error("a file without the marker must not be overwritten")
	}
	// One it did write is replaceable.
	if err := os.WriteFile(p, []byte(Marker+"\nold\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeOwned(p, Marker+"\nnew\n", 0o644); err != nil {
		t.Errorf("a gravinet-written file should be replaceable: %v", err)
	}
}

func TestNetplanYAML(t *testing.T) {
	got := renderNetplan(spec("ens3", []string{"192.0.2.10/24"}, "192.0.2.1", ""))
	for _, want := range []string{
		Marker, "network:", "version: 2", "ethernets:", "    ens3:",
		"dhcp4: false", "addresses:", "- 192.0.2.10/24",
		"routes:", "- to: default", "via: 192.0.2.1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("netplan YAML missing %q:\n%s", want, got)
		}
	}
	// gateway4 was deprecated and is warned about by current netplan; using
	// it would work today and break later.
	if strings.Contains(got, "gateway4") || strings.Contains(got, "gateway6") {
		t.Errorf("netplan YAML uses the deprecated gateway4/6 keys:\n%s", got)
	}
}

func TestIfupdownStanza(t *testing.T) {
	got := renderIfupdown(spec("eth0", []string{"10.0.0.2/24", "10.0.0.3/24", "fd00::2/64"}, "10.0.0.1", "fd00::1"))
	for _, want := range []string{
		Marker, "auto eth0", "iface eth0 inet static", "address 10.0.0.2/24",
		"gateway 10.0.0.1", "eth0:1", "address 10.0.0.3/24",
		"iface eth0 inet6 static", "address fd00::2/64", "gateway fd00::1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ifupdown stanza missing %q:\n%s", want, got)
		}
	}
	// Exactly one gateway per family: repeating it in an alias stanza makes
	// ifup fail on the second one.
	if n := strings.Count(got, "gateway 10.0.0.1"); n != 1 {
		t.Errorf("IPv4 gateway written %d times, want 1:\n%s", n, got)
	}
}

// An interface name is written into files and command lines, so a name that
// could break out of either is refused before any backend sees it.
func TestRefusesUnsafeIfaceNames(t *testing.T) {
	for _, bad := range []string{"", "eth0 up", "eth0;reboot", "../../etc/passwd", "eth0\nauto x"} {
		if _, err := Persist(Spec{Iface: bad}); err == nil {
			t.Errorf("interface name %q should be refused", bad)
		}
	}
}

// MTU has to reach each backend in that backend's own spelling, and the
// networkd case has a trap: MTUBytes belongs in [Link], and networkd ignores
// it under [Network] without complaining.
func TestMTUReachesEachBackend(t *testing.T) {
	s := spec("eth1", []string{"10.1.1.5/24"}, "10.1.1.1", "")
	s.MTU = 1400

	nd := renderNetworkd(s)
	if !strings.Contains(nd, "[Link]\nMTUBytes=1400") {
		t.Errorf("networkd MTU must be under [Link]:\n%s", nd)
	}
	if strings.Index(nd, "MTUBytes") > strings.Index(nd, "[Network]") {
		t.Errorf("MTUBytes appears after [Network], so networkd would ignore it:\n%s", nd)
	}

	if np := renderNetplan(s); !strings.Contains(np, "mtu: 1400") {
		t.Errorf("netplan MTU missing:\n%s", np)
	}
	if iu := renderIfupdown(s); !strings.Contains(iu, "mtu 1400") {
		t.Errorf("ifupdown MTU missing:\n%s", iu)
	}

	// Unset means unmanaged, and must not be written as a zero.
	s.MTU = 0
	for name, out := range map[string]string{
		"networkd": renderNetworkd(s), "netplan": renderNetplan(s), "ifupdown": renderIfupdown(s),
	} {
		if strings.Contains(out, "MTU") || strings.Contains(out, "mtu") {
			t.Errorf("%s writes an MTU when none is managed:\n%s", name, out)
		}
	}
}

// Prune is the difference between an editor and a reconciler, and getting it
// wrong is not a subtle failure: the reconciler runs at every startup and
// reload, so pruning there strips any address gravinet's record does not
// mention — one added by DHCP, by another tool, or by an edit gravinet never
// saw. It also costs FRR the connected routes derived from those addresses,
// which is how it was noticed.
func TestApplyOnlyPrunesWhenAsked(t *testing.T) {
	const ifname = "lo"
	extra := netip.MustParsePrefix("198.18.222.9/32")
	if err := addAddr(ifname, extra); err != nil {
		t.Skipf("cannot add a test address here: %v", err)
	}
	defer delAddr(ifname, extra)

	has := func() bool {
		got, err := GlobalAddrs(ifname)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range got {
			if p == extra {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatal("fixture address did not land")
	}

	// A reconciler pass naming none of it must leave it alone.
	if _, removed, err := Apply(Spec{Iface: ifname}); err != nil || removed != 0 {
		t.Fatalf("reconcile removed %d address(es) (err %v); it must not prune", removed, err)
	}
	if !has() {
		t.Fatal("the reconciler removed an address it did not know about")
	}

	// The editor, submitting the whole intended set, does remove it.
	if _, removed, err := Apply(Spec{Iface: ifname, Prune: true}); err != nil {
		t.Fatal(err)
	} else if removed != 1 {
		t.Fatalf("editor removed %d address(es), want 1", removed)
	}
	if has() {
		t.Fatal("an edit submitting an empty set should have removed it")
	}
}
