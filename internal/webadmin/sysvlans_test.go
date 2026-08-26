package webadmin

import (
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/hostnet"
)

func vlan(parent string, id int, name string) config.HostVLAN {
	return config.HostVLAN{Parent: parent, ID: id, Name: name}
}

// The default name is the convention an operator reading `ip link` expects,
// and an explicit one overrides it.
func TestVLANName(t *testing.T) {
	if got := vlan("eth0", 100, "").VLANName(); got != "eth0.100" {
		t.Errorf("default name = %q, want eth0.100", got)
	}
	if got := vlan("eth0", 100, "storage").VLANName(); got != "storage" {
		t.Errorf("explicit name = %q, want storage", got)
	}
}

// The boundaries are the standard's, not an arbitrary range: 0 and 4095 are
// reserved and neither is a tag a switch will carry.
func TestVLANIDRange(t *testing.T) {
	for _, id := range []int{-1, 0, 4095, 9999} {
		if err := vlan("eth0", id, "").Validate(); err == nil {
			t.Errorf("vlan id %d was accepted", id)
		}
	}
	for _, id := range []int{1, 100, 4094} {
		if err := vlan("eth0", id, "").Validate(); err != nil {
			t.Errorf("vlan id %d was rejected: %v", id, err)
		}
	}
}

// IFNAMSIZ is 15 usable characters. Refused on save rather than left to the
// kernel, so the operator finds out while the field is still in front of them
// — and the message says what to do about it.
func TestVLANNameLength(t *testing.T) {
	long := vlan("enp0s31f6extra", 1000, "")
	err := long.Validate()
	if err == nil {
		t.Fatalf("a %d-character name was accepted", len(long.VLANName()))
	}
	if !strings.Contains(err.Error(), "15") {
		t.Errorf("the message does not say what the limit is: %v", err)
	}
	// An explicit short name is the way out, and it has to work.
	if err := vlan("enp0s31f6extra", 1000, "mgmt").Validate(); err != nil {
		t.Errorf("an explicit short name was still rejected: %v", err)
	}
}

func TestVLANNameRejectsSeparators(t *testing.T) {
	for _, n := range []string{"eth 0", "eth/0", "eth0:1"} {
		if err := vlan("eth0", 10, n).Validate(); err == nil {
			t.Errorf("name %q was accepted", n)
		}
	}
}

// Two definitions producing one device, or one tag twice on one parent: the
// kernel would take one and drop the other, so both are refused here where
// there is somewhere to say why.
func TestValidateHostVLANsCollisions(t *testing.T) {
	dupName := &config.Config{HostVLANs: []config.HostVLAN{
		vlan("eth0", 10, "shared"), vlan("eth1", 20, "shared"),
	}}
	if err := dupName.ValidateHostVLANs(); err == nil {
		t.Error("two tagged interfaces with one name were accepted")
	}
	dupTag := &config.Config{HostVLANs: []config.HostVLAN{
		vlan("eth0", 10, "a"), vlan("eth0", 10, "b"),
	}}
	if err := dupTag.ValidateHostVLANs(); err == nil {
		t.Error("the same tag twice on one parent was accepted")
	}
	// Same tag on a different parent is an ordinary trunk configuration.
	ok := &config.Config{HostVLANs: []config.HostVLAN{
		vlan("eth0", 10, ""), vlan("eth1", 10, ""),
	}}
	if err := ok.ValidateHostVLANs(); err != nil {
		t.Errorf("vlan 10 on two different parents was rejected: %v", err)
	}
}

// QinQ is refused rather than half-supported: nothing here models an outer
// tag, and the reconciler has no ordering that guarantees the inner device's
// parent exists first.
func TestValidateHostVLANsRefusesStacking(t *testing.T) {
	c := &config.Config{HostVLANs: []config.HostVLAN{
		vlan("eth0", 10, ""),         // eth0.10
		vlan("eth0.10", 20, "inner"), // stacked on it
	}}
	err := c.ValidateHostVLANs()
	if err == nil {
		t.Fatal("a VLAN stacked on another VLAN was accepted")
	}
	if !strings.Contains(err.Error(), "stacked") {
		t.Errorf("the message does not explain the refusal: %v", err)
	}
}

// A parked row is not supposed to be on the host, so its absence is not
// reported as a fault — that would hand the operator their own request back.
func TestVLANRowsDisabledReportsNoProblem(t *testing.T) {
	rows := vlanRows([]config.HostVLAN{
		{Parent: "nosuchif0", ID: 10, Disabled: true},
	}, hostView{})
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Problem != "" {
		t.Errorf("a disabled row reported %q", rows[0].Problem)
	}
	if rows[0].Present {
		t.Error("a device that does not exist was reported present")
	}
}

// A definition whose parent is gone says so, and names the parent rather than
// only the device: "eth9.10 is not present" sends someone looking for the
// wrong thing.
func TestVLANRowsMissingParent(t *testing.T) {
	rows := vlanRows([]config.HostVLAN{{Parent: "nosuchif0", ID: 10}}, hostView{})
	if rows[0].Problem == "" {
		t.Fatal("a VLAN whose parent is missing reported no problem")
	}
	if !strings.Contains(rows[0].Problem, "nosuchif0") {
		t.Errorf("the message does not name the missing parent: %q", rows[0].Problem)
	}
}

// Deleting a tagged interface takes its addressing record with it. Left
// behind, the record would be reapplied the moment the same VLAN was
// recreated, putting back an address the operator had deleted.
func TestDropHostIface(t *testing.T) {
	in := []config.HostIface{
		{Iface: "eth0"}, {Iface: "eth0.100"}, {Iface: "eth1"},
	}
	out := dropHostIface(in, "eth0.100")
	if len(out) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(out), out)
	}
	for _, h := range out {
		if h.Iface == "eth0.100" {
			t.Error("the deleted interface's addressing record survived")
		}
	}
	// Case-insensitive, since interface names reach this from a request.
	if got := dropHostIface(in, "ETH0.100"); len(got) != 2 {
		t.Errorf("a differently-cased name did not match: %+v", got)
	}
}

// A host view standing in for a machine with eth1 up and a tagged interface
// on it. Built by hand so the cases below describe a host rather than needing
// one — none of them could be written while net.InterfaceByName and a sysfs
// read inside the loop were the only things that could answer.
func hostWith(vlans map[string]hostnet.VLANDevice, links map[string]bool) hostView {
	if links == nil {
		links = map[string]bool{}
	}
	if vlans == nil {
		vlans = map[string]hostnet.VLANDevice{}
	}
	return hostView{links: links, vlans: vlans}
}

// The reported bug, and the reason this file can now describe it: a tagged
// interface that is present, up, on the right parent and carrying the right
// tag is working, and the page must say nothing about it.
//
// It said "vlan22 exists but is not a tagged interface — something else on
// this host owns that name" about every such row on every host, because the
// lookup behind that branch read /sys/class/net/<dev>/8021q/vlan_id, a path
// the kernel has never created. The read failed everywhere, "not a VLAN" was
// indistinguishable from "could not tell", and the collision branch fired on
// every healthy definition.
func TestVLANRowsHealthyTaggedInterfaceReportsNoProblem(t *testing.T) {
	rows := vlanRows(
		[]config.HostVLAN{{Parent: "eth1", ID: 22, Name: "vlan22"}},
		hostWith(
			map[string]hostnet.VLANDevice{"vlan22": {Parent: "eth1", ID: 22, Protocol: 0x8100}},
			map[string]bool{"eth1": true, "vlan22": true},
		),
	)
	if rows[0].Problem != "" {
		t.Errorf("a working tagged interface reported %q", rows[0].Problem)
	}
	if !rows[0].Present || !rows[0].Up {
		t.Errorf("a present, up device came back present=%v up=%v", rows[0].Present, rows[0].Up)
	}
}

// The same row with a derived name, which is what a new definition produces.
func TestVLANRowsHealthyDerivedNameReportsNoProblem(t *testing.T) {
	rows := vlanRows(
		[]config.HostVLAN{{Parent: "eth1", ID: 22}},
		hostWith(
			map[string]hostnet.VLANDevice{"eth1.22": {Parent: "eth1", ID: 22, Protocol: 0x8100}},
			map[string]bool{"eth1": true, "eth1.22": true},
		),
	)
	if rows[0].Name != "eth1.22" {
		t.Errorf("derived name = %q, want eth1.22", rows[0].Name)
	}
	if rows[0].Problem != "" {
		t.Errorf("a working tagged interface reported %q", rows[0].Problem)
	}
}

// The collision branch still has to fire when it is actually true, or the fix
// would have traded a false alarm for a blind spot. A name owned by something
// that is not a tagged interface is the case it exists for.
func TestVLANRowsRealNameCollisionIsStillReported(t *testing.T) {
	rows := vlanRows(
		[]config.HostVLAN{{Parent: "eth1", ID: 22, Name: "vlan22"}},
		hostWith(nil, map[string]bool{"eth1": true, "vlan22": true}),
	)
	if !strings.Contains(rows[0].Problem, "not a tagged interface") {
		t.Errorf("a genuine name collision reported %q", rows[0].Problem)
	}
}

// A device carrying a different tag than the row claims, and one on a
// different parent, are both mismatches worth naming.
func TestVLANRowsReportsTagAndParentMismatch(t *testing.T) {
	rows := vlanRows(
		[]config.HostVLAN{{Parent: "eth1", ID: 22, Name: "vlan22"}},
		hostWith(
			map[string]hostnet.VLANDevice{"vlan22": {Parent: "eth1", ID: 99, Protocol: 0x8100}},
			map[string]bool{"eth1": true, "vlan22": true},
		),
	)
	if !strings.Contains(rows[0].Problem, "99") {
		t.Errorf("a tag mismatch reported %q", rows[0].Problem)
	}
	rows = vlanRows(
		[]config.HostVLAN{{Parent: "eth1", ID: 22, Name: "vlan22"}},
		hostWith(
			map[string]hostnet.VLANDevice{"vlan22": {Parent: "eth2", ID: 22, Protocol: 0x8100}},
			map[string]bool{"eth1": true, "eth2": true, "vlan22": true},
		),
	)
	if !strings.Contains(rows[0].Problem, "eth2") {
		t.Errorf("a parent mismatch reported %q", rows[0].Problem)
	}
}

// A VLAN whose lower link is in another namespace reports no parent. That is
// "cannot tell", not "wrong parent", and reporting it as a mismatch would name
// a device the operator would then go looking for.
func TestVLANRowsUnknownParentIsNotAMismatch(t *testing.T) {
	rows := vlanRows(
		[]config.HostVLAN{{Parent: "eth1", ID: 22, Name: "vlan22"}},
		hostWith(
			map[string]hostnet.VLANDevice{"vlan22": {Parent: "", ID: 22, Protocol: 0x8100}},
			map[string]bool{"vlan22": true},
		),
	)
	if rows[0].Problem != "" {
		t.Errorf("a VLAN whose parent is elsewhere reported %q", rows[0].Problem)
	}
}

// A device that is down is still reported as such — the branch above the
// tagged-interface checks must not have been swallowed by them.
func TestVLANRowsDownDeviceStillReported(t *testing.T) {
	rows := vlanRows(
		[]config.HostVLAN{{Parent: "eth1", ID: 22}},
		hostWith(
			map[string]hostnet.VLANDevice{"eth1.22": {Parent: "eth1", ID: 22, Protocol: 0x8100}},
			map[string]bool{"eth1": true, "eth1.22": false},
		),
	)
	if !strings.Contains(rows[0].Problem, "down") {
		t.Errorf("a device that is down reported %q", rows[0].Problem)
	}
}

// An add whose device name is already exactly the VLAN being asked for is not
// a clash. gravinet's devices outlive the process that made them, so a
// restored backup or a delete-then-re-add meets its own handiwork here, and
// refusing would leave a device on the host with no row describing it.
func TestRefuseVLANNameInUseAdoptsTheSameDevice(t *testing.T) {
	err := refuseVLANNameInUse(
		config.HostVLAN{Parent: "eth1", ID: 22},
		hostWith(
			map[string]hostnet.VLANDevice{"eth1.22": {Parent: "eth1", ID: 22, Protocol: 0x8100}},
			map[string]bool{"eth1": true, "eth1.22": true},
		),
	)
	if err != nil {
		t.Errorf("re-adding an identical definition was refused: %v", err)
	}
}

// A name owned by something else is still refused, and the message says what
// is in the way rather than only that something is.
func TestRefuseVLANNameInUseRejectsAForeignDevice(t *testing.T) {
	err := refuseVLANNameInUse(
		config.HostVLAN{Parent: "eth1", ID: 22},
		hostWith(nil, map[string]bool{"eth1": true, "eth1.22": true}),
	)
	if err == nil {
		t.Fatal("a name owned by something else was accepted")
	}
	if !strings.Contains(err.Error(), "not a tagged interface") {
		t.Errorf("the message does not say what is in the way: %v", err)
	}
	// And a device of the right name carrying the wrong tag.
	err = refuseVLANNameInUse(
		config.HostVLAN{Parent: "eth1", ID: 22},
		hostWith(
			map[string]hostnet.VLANDevice{"eth1.22": {Parent: "eth1", ID: 99, Protocol: 0x8100}},
			map[string]bool{"eth1": true, "eth1.22": true},
		),
	)
	if err == nil || !strings.Contains(err.Error(), "99") {
		t.Errorf("a device carrying the wrong tag was accepted or unexplained: %v", err)
	}
}

// A free name is free.
func TestRefuseVLANNameInUseAllowsAFreeName(t *testing.T) {
	if err := refuseVLANNameInUse(
		config.HostVLAN{Parent: "eth1", ID: 22},
		hostWith(nil, map[string]bool{"eth1": true}),
	); err != nil {
		t.Errorf("a free name was refused: %v", err)
	}
}

// The name a new definition gets is derived from the parent and the tag, with
// nothing supplied by the request. A configuration written before that was
// true keeps its own name: the device is referenced by name from the
// addressing records, the DHCP subnets and the firewall, and re-deriving it
// would rename a live interface out from under all of them.
func TestVLANNameIsDerivedButAnExistingOneIsKept(t *testing.T) {
	if got := (config.HostVLAN{Parent: "eth1", ID: 22}).VLANName(); got != "eth1.22" {
		t.Errorf("derived name = %q, want eth1.22", got)
	}
	if got := (config.HostVLAN{Parent: "eth1", ID: 22, Name: "vlan22"}).VLANName(); got != "vlan22" {
		t.Errorf("a name already in the configuration was re-derived to %q", got)
	}
}

// A parent too long to carry a derived name is refused, and because there is
// no name box on the page any more the message has to say where the override
// lives instead.
func TestDerivedNameTooLongPointsAtTheConfigFile(t *testing.T) {
	err := (config.HostVLAN{Parent: "enp0s20f0u3u1", ID: 4094}).Validate()
	if err == nil {
		t.Fatal("an 18-character derived name was accepted")
	}
	if !strings.Contains(err.Error(), "configuration file") {
		t.Errorf("the message does not say where the override is: %v", err)
	}
	if !strings.Contains(err.Error(), "enp0s20f0u3u1") {
		t.Errorf("the message does not name the parent that does not fit: %v", err)
	}
}
