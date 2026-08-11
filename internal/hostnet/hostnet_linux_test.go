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

// NetworkManager is the one backend that does not render a file, so it was the
// one with no test — and the argv it builds is where the only wrong-index bug
// in this package lived. Checked as key/value pairs rather than by substring:
// the failure being guarded against was a value landing on a key's position,
// which a `strings.Contains` over the joined argv would not have caught.
func TestNMArgsPairing(t *testing.T) {
	nmVal := func(t *testing.T, args []string, key string) string {
		t.Helper()
		// Properties start after `connection modify <name>`, so the name
		// itself cannot be mistaken for a key.
		for i := 3; i < len(args)-1; i += 2 {
			if args[i] == key {
				return args[i+1]
			}
		}
		t.Fatalf("no %s in %v", key, args)
		return ""
	}

	// Both families addressed: both manual, both address lists set.
	args := nmModifyArgs("Wired connection 1",
		spec("eth0", []string{"10.1.1.5/24", "10.1.1.6/24", "fd00::5/64"}, "10.1.1.1", "fd00::1"))
	if len(args)%2 != 1 {
		t.Fatalf("argv is not `connection modify <name>` plus key/value pairs: %v", args)
	}
	for k, want := range map[string]string{
		"ipv4.method":    "manual",
		"ipv6.method":    "manual",
		"ipv4.addresses": "10.1.1.5/24,10.1.1.6/24",
		"ipv6.addresses": "fd00::5/64",
		"ipv4.gateway":   "10.1.1.1",
		"ipv6.gateway":   "fd00::1",
	} {
		if got := nmVal(t, args, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// One family emptied disables that family and leaves the other alone.
	// This is the case that used to emit `modify <name> disabled manual ...`,
	// where the literal "disabled" replaced the property name and nmcli
	// rejected the whole command — so the address applied live and silently
	// did not survive a reboot.
	args = nmModifyArgs("Wired connection 1", spec("eth0", []string{"fd00::5/64"}, "", "fd00::1"))
	if got := nmVal(t, args, "ipv4.method"); got != "disabled" {
		t.Errorf("no IPv4 addresses: ipv4.method = %q, want disabled", got)
	}
	if got := nmVal(t, args, "ipv6.method"); got != "manual" {
		t.Errorf("emptying IPv4 must not touch IPv6: ipv6.method = %q, want manual", got)
	}
	if got := nmVal(t, args, "ipv4.addresses"); got != "" {
		t.Errorf("ipv4.addresses = %q, want empty", got)
	}
	for _, a := range args {
		if a == "ipv4.gateway" {
			t.Error("no IPv4 gateway was given, so none should be written")
		}
	}

	// And the mirror case, so a fix that hardcoded one family would fail.
	args = nmModifyArgs("Wired connection 1", spec("eth0", []string{"10.1.1.5/24"}, "10.1.1.1", ""))
	if got := nmVal(t, args, "ipv6.method"); got != "disabled" {
		t.Errorf("no IPv6 addresses: ipv6.method = %q, want disabled", got)
	}
	if got := nmVal(t, args, "ipv4.method"); got != "manual" {
		t.Errorf("emptying IPv6 must not touch IPv4: ipv4.method = %q, want manual", got)
	}

	// MTU is only written when managed, the same as every other backend.
	if got := nmVal(t, nmModifyArgs("c", Spec{Iface: "eth0", MTU: 1400}), "802-3-ethernet.mtu"); got != "1400" {
		t.Errorf("mtu = %q, want 1400", got)
	}
	for _, a := range nmModifyArgs("c", Spec{Iface: "eth0"}) {
		if a == "802-3-ethernet.mtu" {
			t.Error("an unmanaged MTU must not be written as a zero")
		}
	}
}

// modeSpec builds a spec with modes, since the shared helper predates them.
func modeSpec(iface string, m4, m6 Mode, addrs []string, gw4, gw6 string) Spec {
	s := spec(iface, addrs, gw4, gw6)
	s.Mode4, s.Mode6 = m4, m6
	return s
}

// Each backend spells the modes differently, and each spelling is a claim about
// a tool none of these tests can run. What they can check is that the four
// renderers agree about which mode was asked for — a backend that quietly wrote
// static where the operator picked DHCP is the failure this package exists to
// prevent, and it looks identical to success until the next boot.
func TestBackendsSpellTheModes(t *testing.T) {
	// Plain DHCP on both families: no static address survives, no gateway is
	// written, and every backend says so in its own vocabulary.
	dhcp := modeSpec("eth0", ModeDHCP, ModeDHCP6, []string{"10.1.1.5/24", "fd00::5/64"}, "10.1.1.1", "fd00::1")

	np := renderNetplan(dhcp)
	for _, want := range []string{"dhcp4: true", "dhcp6: true", "accept-ra: true"} {
		if !strings.Contains(np, want) {
			t.Errorf("netplan missing %q:\n%s", want, np)
		}
	}
	for _, bad := range []string{"addresses:", "routes:", "10.1.1.5", "fd00::5"} {
		if strings.Contains(np, bad) {
			t.Errorf("netplan wrote %q for a fully non-static interface:\n%s", bad, np)
		}
	}

	nd := renderNetworkd(dhcp)
	if !strings.Contains(nd, "DHCP=yes") {
		t.Errorf("networkd should fold both families into DHCP=yes:\n%s", nd)
	}
	if strings.Contains(nd, "Address=") || strings.Contains(nd, "Gateway=") {
		t.Errorf("networkd wrote an address or gateway under DHCP:\n%s", nd)
	}

	iu := renderIfupdown(dhcp)
	if !strings.Contains(iu, "inet dhcp") {
		t.Errorf("ifupdown missing an inet dhcp stanza:\n%s", iu)
	}
	if !strings.Contains(iu, "inet6 dhcp") || !strings.Contains(iu, "accept_ra 1") {
		t.Errorf("ifupdown missing inet6 dhcp with accept_ra:\n%s", iu)
	}
	if strings.Contains(iu, "static") {
		t.Errorf("ifupdown wrote a static stanza under DHCP:\n%s", iu)
	}

	// networkd folds both families into one key, so all four combinations have
	// to be right rather than just the two ends.
	for _, c := range []struct{ m4, m6, want Mode }{
		{ModeDHCP, ModeDHCP6, "yes"},
		{ModeDHCP, ModeStatic, "ipv4"},
		{ModeStatic, ModeDHCP6, "ipv6"},
		{ModeStatic, ModeStatic, "no"},
		{ModeDHCP, ModeSLAAC, "ipv4"}, // SLAAC is not DHCP
	} {
		got := renderNetworkd(modeSpec("eth0", c.m4, c.m6, nil, "", ""))
		if !strings.Contains(got, "DHCP="+string(c.want)+"\n") {
			t.Errorf("networkd %s/%s: want DHCP=%s:\n%s", c.m4, c.m6, c.want, got)
		}
	}
}

// The configuration this feature was asked for, through every Linux backend: a
// static IPv4 address with SLAAC IPv6 on one interface. The failure being
// guarded against is a backend that reads one family's mode and applies it to
// both — which on this pairing means either losing the static address or
// leaving IPv6 with nothing.
func TestStaticV4WithSLAACV6(t *testing.T) {
	s := modeSpec("eth0", ModeStatic, ModeSLAAC, []string{"10.1.1.5/24"}, "10.1.1.1", "")

	np := renderNetplan(s)
	for _, want := range []string{
		"dhcp4: false", // static v4
		"dhcp6: false", // SLAAC is not DHCPv6
		"accept-ra: true",
		"- 10.1.1.5/24",
		"via: 10.1.1.1",
	} {
		if !strings.Contains(np, want) {
			t.Errorf("netplan missing %q:\n%s", want, np)
		}
	}

	nd := renderNetworkd(s)
	for _, want := range []string{"DHCP=no", "IPv6AcceptRA=true", "Address=10.1.1.5/24", "Gateway=10.1.1.1"} {
		if !strings.Contains(nd, want) {
			t.Errorf("networkd missing %q:\n%s", want, nd)
		}
	}

	iu := renderIfupdown(s)
	for _, want := range []string{"inet static", "address 10.1.1.5/24", "gateway 10.1.1.1", "inet6 auto"} {
		if !strings.Contains(iu, want) {
			t.Errorf("ifupdown missing %q:\n%s", want, iu)
		}
	}
	if strings.Contains(iu, "inet6 static") {
		t.Errorf("ifupdown wrote a static v6 stanza under SLAAC:\n%s", iu)
	}

	// A static family keeps accept-ra off, so switching v6 to static actually
	// stops the kernel putting an autoconfigured address back.
	off := renderNetplan(modeSpec("eth0", ModeStatic, ModeStatic, []string{"fd00::5/64"}, "", ""))
	if !strings.Contains(off, "accept-ra: false") {
		t.Errorf("static IPv6 must turn accept-ra off:\n%s", off)
	}
}

// NM has its own vocabulary and it does not line up with gravinet's: "auto" is
// DHCP for IPv4 but SLAAC for IPv6, and NM's "dhcp" exists only for IPv6. A
// mapping that assumed the words matched would put an interface on SLAAC when
// the operator asked for DHCPv6, which yields addresses — from the wrong place.
func TestNMArgsModes(t *testing.T) {
	val := func(t *testing.T, args []string, key string) string {
		t.Helper()
		for i := 3; i < len(args)-1; i += 2 {
			if args[i] == key {
				return args[i+1]
			}
		}
		t.Fatalf("no %s in %v", key, args)
		return ""
	}
	has := func(args []string, key string) bool {
		for _, a := range args {
			if a == key {
				return true
			}
		}
		return false
	}

	for _, c := range []struct{ m4, m6, want4, want6 string }{
		{string(ModeDHCP), string(ModeSLAAC), "auto", "auto"},
		{string(ModeDHCP), string(ModeDHCP6), "auto", "dhcp"},
		{string(ModeStatic), string(ModeSLAAC), "manual", "auto"},
		{string(ModeStatic), string(ModeDHCP6), "manual", "dhcp"},
	} {
		args := nmModifyArgs("c", modeSpec("eth0", Mode(c.m4), Mode(c.m6),
			[]string{"10.1.1.5/24", "fd00::5/64"}, "10.1.1.1", "fd00::1"))
		if got := val(t, args, "ipv4.method"); got != c.want4 {
			t.Errorf("%s/%s: ipv4.method = %q, want %q", c.m4, c.m6, got, c.want4)
		}
		if got := val(t, args, "ipv6.method"); got != c.want6 {
			t.Errorf("%s/%s: ipv6.method = %q, want %q", c.m4, c.m6, got, c.want6)
		}
	}

	// A stale static address left in an NM profile under method auto is applied
	// alongside the lease at the next boot, so a non-static family's addresses
	// and gateway are cleared rather than carried.
	args := nmModifyArgs("c", modeSpec("eth0", ModeDHCP, ModeSLAAC,
		[]string{"10.1.1.5/24", "fd00::5/64"}, "10.1.1.1", "fd00::1"))
	if got := val(t, args, "ipv4.addresses"); got != "" {
		t.Errorf("ipv4.addresses = %q under dhcp, want empty", got)
	}
	if got := val(t, args, "ipv6.addresses"); got != "" {
		t.Errorf("ipv6.addresses = %q under slaac, want empty", got)
	}
	if has(args, "ipv4.gateway") || has(args, "ipv6.gateway") {
		t.Errorf("a gateway must not be written under a non-static mode: %v", args)
	}

	// The mixed case again: v4 manual with its address and gateway intact, v6 on
	// SLAAC with neither.
	args = nmModifyArgs("c", modeSpec("eth0", ModeStatic, ModeSLAAC,
		[]string{"10.1.1.5/24"}, "10.1.1.1", ""))
	if got := val(t, args, "ipv4.addresses"); got != "10.1.1.5/24" {
		t.Errorf("ipv4.addresses = %q, want the static address", got)
	}
	if got := val(t, args, "ipv4.gateway"); got != "10.1.1.1" {
		t.Errorf("ipv4.gateway = %q, want 10.1.1.1", got)
	}
	if got := val(t, args, "ipv6.method"); got != "auto" {
		t.Errorf("ipv6.method = %q, want auto", got)
	}

	// "disabled" now means only what it always meant — a static family with no
	// addresses — and is no longer standing in for "get one from the network".
	args = nmModifyArgs("c", modeSpec("eth0", ModeStatic, ModeStatic, []string{"10.1.1.5/24"}, "", ""))
	if got := val(t, args, "ipv6.method"); got != "disabled" {
		t.Errorf("static IPv6 with no addresses: ipv6.method = %q, want disabled", got)
	}
	if got := val(t, args, "ipv4.method"); got != "manual" {
		t.Errorf("ipv4.method = %q, want manual", got)
	}
}
