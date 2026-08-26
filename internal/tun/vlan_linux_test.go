//go:build linux

package tun

import (
	"encoding/binary"
	"syscall"
	"testing"
)

// linkMsg builds one RTM_GETLINK message body the way the kernel lays it out:
// a 16-byte ifinfomsg followed by attributes. Built with the same rtattr
// helper AddVLAN uses, so a test that passes pins the encoder and the decoder
// against each other — the nest read back here is the nest written there.
func linkMsg(index int, name string, attrs ...[]byte) []byte {
	b := make([]byte, 16)
	binary.NativeEndian.PutUint32(b[4:8], uint32(index))
	b = append(b, rtattr(syscall.IFLA_IFNAME, []byte(name+"\x00"))...)
	for _, a := range attrs {
		b = append(b, a...)
	}
	return b
}

// vlanInfoAttr builds the IFLA_LINKINFO nest the vlan driver emits.
// protocol is written big-endian because the kernel writes it with
// nla_put_be16; passing 0 omits it, which is what a kernel too old to report
// the ethertype does.
func vlanInfoAttr(id int, protocol uint16) []byte {
	idBuf := make([]byte, 2)
	binary.NativeEndian.PutUint16(idBuf, uint16(id))
	data := rtattr(iflaVLANID, idBuf)
	if protocol != 0 {
		pBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(pBuf, protocol)
		data = append(data, rtattr(iflaVLANProtocol, pBuf)...)
	}
	info := append(rtattr(iflaInfoKind, []byte("vlan\x00")), rtattr(iflaInfoData, data)...)
	return rtattr(iflaLinkInfo, info)
}

func parentAttr(index int) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, uint32(index))
	return rtattr(iflaLink, b)
}

func kindAttr(kind string) []byte {
	return rtattr(iflaLinkInfo, rtattr(iflaInfoKind, []byte(kind+"\x00")))
}

// The case the page got wrong for as long as it existed: an ordinary tagged
// interface, present and carrying the tag its definition asks for, has to come
// back as a VLAN with its parent and id. The sysfs read this replaced could
// not answer it on any host, so every healthy row was reported as a name
// collision.
func TestParseVLANLinksReadsTagAndParent(t *testing.T) {
	msgs := [][]byte{
		linkMsg(3, "eth1"),
		linkMsg(7, "eth1.22", parentAttr(3), vlanInfoAttr(22, EthPrt8021Q)),
	}
	got := parseVLANLinks(msgs)
	v, ok := got["eth1.22"]
	if !ok {
		t.Fatalf("a tagged interface was not recognised as one: %+v", got)
	}
	if v.ID != 22 {
		t.Errorf("tag = %d, want 22", v.ID)
	}
	if v.Parent != "eth1" {
		t.Errorf("parent = %q, want eth1", v.Parent)
	}
	if v.Protocol != EthPrt8021Q {
		t.Errorf("protocol = %#x, want %#x", v.Protocol, EthPrt8021Q)
	}
}

// Nothing that is not a VLAN may appear in the result. A bridge or a bond
// wrongly matched here would be offered as something to stack a tag on and
// would make a definition naming it look satisfied.
func TestParseVLANLinksIgnoresOtherKinds(t *testing.T) {
	msgs := [][]byte{
		linkMsg(1, "lo"),
		linkMsg(2, "br0", kindAttr("bridge")),
		linkMsg(3, "bond0", kindAttr("bond")),
		linkMsg(4, "eth0"),
	}
	if got := parseVLANLinks(msgs); len(got) != 0 {
		t.Errorf("non-VLAN devices came back as VLANs: %+v", got)
	}
}

// The parent's message is allowed to arrive after the VLAN's. Resolving
// IFLA_LINK inline as each message is read would leave the parent empty here,
// which the page renders as a device whose parent it cannot confirm.
func TestParseVLANLinksResolvesAParentDumpedLater(t *testing.T) {
	msgs := [][]byte{
		linkMsg(7, "eth1.22", parentAttr(3), vlanInfoAttr(22, EthPrt8021Q)),
		linkMsg(3, "eth1"),
	}
	if got := parseVLANLinks(msgs)["eth1.22"].Parent; got != "eth1" {
		t.Errorf("parent = %q, want eth1 — resolution depends on dump order", got)
	}
}

// A VLAN whose lower link is in another namespace keeps an ifindex that
// resolves to nothing here. That is "cannot tell", not "wrong parent", and the
// device still has to come back as a VLAN carrying its tag.
func TestParseVLANLinksToleratesAnUnresolvableParent(t *testing.T) {
	msgs := [][]byte{linkMsg(7, "vlan22", parentAttr(99), vlanInfoAttr(22, EthPrt8021Q))}
	v, ok := parseVLANLinks(msgs)["vlan22"]
	if !ok {
		t.Fatal("a VLAN whose parent is elsewhere was not reported as a VLAN")
	}
	if v.Parent != "" {
		t.Errorf("parent = %q, want empty", v.Parent)
	}
	if v.ID != 22 {
		t.Errorf("tag = %d, want 22", v.ID)
	}
}

// IFLA_VLAN_PROTOCOL is nla_put_be16 while IFLA_VLAN_ID next to it is
// nla_put_u16. Reading the ethertype in host order on a little-endian host
// turns 0x8100 into 0x0081 and makes every ordinary VLAN look exotic.
func TestParseVLANLinksReadsTheEthertypeBigEndian(t *testing.T) {
	msgs := [][]byte{linkMsg(7, "eth1.22", vlanInfoAttr(22, EthPrt8021AD))}
	if got := parseVLANLinks(msgs)["eth1.22"].Protocol; got != EthPrt8021AD {
		t.Errorf("protocol = %#x, want %#x", got, EthPrt8021AD)
	}
	// And the tag beside it is host order, so it must survive unswapped.
	if got := parseVLANLinks(msgs)["eth1.22"].ID; got != 22 {
		t.Errorf("tag = %d, want 22", got)
	}
}

// A kernel that reports no ethertype is reporting the default. Defaulting to
// zero instead would make every VLAN on such a host mismatch on protocol.
func TestParseVLANLinksDefaultsAMissingEthertype(t *testing.T) {
	msgs := [][]byte{linkMsg(7, "eth1.22", vlanInfoAttr(22, 0))}
	if got := parseVLANLinks(msgs)["eth1.22"].Protocol; got != EthPrt8021Q {
		t.Errorf("protocol = %#x, want the 802.1Q default %#x", got, EthPrt8021Q)
	}
}

// Interface names are NUL-terminated and padded in netlink. Keeping the
// terminator makes every lookup against the map miss.
func TestParseVLANLinksTrimsNames(t *testing.T) {
	got := parseVLANLinks([][]byte{
		linkMsg(3, "eth1"),
		linkMsg(7, "eth1.22", parentAttr(3), vlanInfoAttr(22, EthPrt8021Q)),
	})
	for name := range got {
		if name != "eth1.22" {
			t.Errorf("device name = %q, want eth1.22", name)
		}
	}
	if got["eth1.22"].Parent != "eth1" {
		t.Errorf("parent = %q, want eth1", got["eth1.22"].Parent)
	}
}

// Whatever the kernel hands back, a malformed message must not take the daemon
// down: this runs on every load of the interfaces page.
func TestParseVLANLinksSurvivesTruncatedMessages(t *testing.T) {
	full := linkMsg(7, "eth1.22", parentAttr(3), vlanInfoAttr(22, EthPrt8021Q))
	for n := 0; n < len(full); n++ {
		parseVLANLinks([][]byte{full[:n]})
	}
}

// The dump itself has to work on a real host. It cannot be asserted that any
// VLAN is found — a test host generally has none, and this container's kernel
// has no 8021q at all — but the call must reach the kernel and come back
// without error, which is what distinguishes a working netlink path from the
// sysfs read that silently returned nothing everywhere.
func TestVLANLinksDumpsWithoutError(t *testing.T) {
	got, err := VLANLinks()
	if err != nil {
		t.Fatalf("dumping links: %v", err)
	}
	for name, v := range got {
		if v.ID < 1 || v.ID > 4094 {
			t.Errorf("%s came back with tag %d, outside 1-4094", name, v.ID)
		}
	}
}
