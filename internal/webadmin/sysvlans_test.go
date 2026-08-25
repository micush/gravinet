package webadmin

import (
	"strings"
	"testing"

	"gravinet/internal/config"
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
	})
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
	rows := vlanRows([]config.HostVLAN{{Parent: "nosuchif0", ID: 10}})
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
