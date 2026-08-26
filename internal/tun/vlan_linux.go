//go:build linux

package tun

// 802.1Q tagged interface creation over rtnetlink — the link counterpart to
// addr_linux.go's address handling, and written the same way: raw netlink, no
// `ip` binary, no cgo.
//
// RTM_NEWLINK carrying a nested IFLA_LINKINFO is the whole of it. The nesting
// is the only part that is not obvious from the address code: IFLA_INFO_KIND
// names the driver ("vlan") and IFLA_INFO_DATA is a second nest holding that
// driver's own attributes, of which only IFLA_VLAN_ID is required. Both nests
// are ordinary rtattrs whose payload is more rtattrs, so rtattr composes with
// itself and there is nothing new to serialize.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
)

// rtnetlink attribute types for links. Not in the syscall package, which
// stopped gaining constants long before these mattered; the values are from
// linux/if_link.h and are ABI, so they do not move.
const (
	iflaInfoKind = 1
	iflaInfoData = 2
	iflaVLANID   = 1
	// IFLA_VLAN_PROTOCOL carries the tag's ethertype. Unlike IFLA_VLAN_ID,
	// which the kernel writes with nla_put_u16 in host order, this one is
	// nla_put_be16 — it is an ethertype and travels the way one does on the
	// wire. Reading it with the wrong endianness turns 0x8100 into 0x0081 and
	// makes every ordinary VLAN look like something exotic.
	iflaVLANProtocol = 5
	// IFLA_LINKINFO and IFLA_LINK are in the same enum as syscall.IFLA_MTU
	// and are stable for the same reason.
	iflaLinkInfo = 18
	iflaLink     = 5
)

// The two tag ethertypes the kernel's vlan driver accepts. AddVLAN sends no
// IFLA_VLAN_PROTOCOL, so what it creates is always the first of these — the
// kernel defaults to it. The second exists here to be recognised, not created:
// a device carrying it is a real VLAN that is nonetheless not the one a
// gravinet definition describes, and saying so is more use than reporting the
// row as healthy.
const (
	EthPrt8021Q  = 0x8100
	EthPrt8021AD = 0x88a8
)

// AddVLAN creates a tagged interface named name on parent, carrying VLAN id.
//
// Returns an error the caller can show an operator rather than a bare errno
// where the cause is knowable: EEXIST here almost always means the device is
// already there, which for a reconciler run at every startup is the ordinary
// case rather than a fault — see VLANExists for how that is distinguished.
func AddVLAN(parent, name string, id int) error {
	if id < 1 || id > 4094 {
		return fmt.Errorf("vlan id %d out of range", id)
	}
	pidx, err := net.InterfaceByName(parent)
	if err != nil {
		return fmt.Errorf("parent interface %s: %w", parent, err)
	}

	// struct ifinfomsg: family, pad, type, index, flags, change. Index is
	// left zero — the kernel allocates one for a device being created.
	ifi := make([]byte, 16)

	idBuf := make([]byte, 2)
	binary.NativeEndian.PutUint16(idBuf, uint16(id))
	// IFLA_LINKINFO { IFLA_INFO_KIND "vlan", IFLA_INFO_DATA { IFLA_VLAN_ID } }
	data := rtattr(iflaVLANID, idBuf)
	// The kind string is NUL-terminated: the kernel compares it against the
	// registered rtnl_link_ops name with strcmp.
	info := append(rtattr(iflaInfoKind, []byte("vlan\x00")), rtattr(iflaInfoData, data)...)

	linkIdx := make([]byte, 4)
	binary.NativeEndian.PutUint32(linkIdx, uint32(pidx.Index))

	body := ifi
	body = append(body, rtattr(iflaLink, linkIdx)...)
	body = append(body, rtattr(syscall.IFLA_IFNAME, []byte(name+"\x00"))...)
	body = append(body, rtattr(iflaLinkInfo, info)...)

	// EXCL rather than REPLACE: replacing a link is not what an operator who
	// asked for a new one means, and a name already taken by something else
	// is a collision worth reporting rather than clobbering.
	return netlinkExec(syscall.RTM_NEWLINK,
		syscall.NLM_F_REQUEST|syscall.NLM_F_CREATE|syscall.NLM_F_EXCL|syscall.NLM_F_ACK, body)
}

// DelLink removes an interface by name.
//
// This deletes a device, so the caller is responsible for having established
// that the device is gravinet's to delete. Nothing here checks: by the time a
// name reaches this function it is an index and a syscall.
func DelLink(name string) error {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %s: %w", name, err)
	}
	body := make([]byte, 16)
	binary.NativeEndian.PutUint32(body[4:8], uint32(ifi.Index))
	return netlinkExec(syscall.RTM_DELLINK, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK, body)
}

// VLANLink is what the kernel says one tagged interface is.
type VLANLink struct {
	// Name is the device name.
	Name string
	// Parent is the device the tag rides on, or empty when the lower link is
	// in another network namespace — a VLAN can be moved across a namespace
	// boundary away from its parent, and the ifindex it keeps then resolves
	// to nothing here. Empty means "not knowable from this namespace", which
	// is different from a mismatch and must not be reported as one.
	Parent string
	// ID is the 802.1Q tag, 1-4094.
	ID int
	// Protocol is the tag ethertype in host order: EthPrt8021Q or
	// EthPrt8021AD.
	Protocol uint16
}

// VLANLinks reports every tagged interface in this network namespace, keyed by
// device name.
//
// One RTM_GETLINK dump for the whole set rather than a lookup per device. The
// caller is a page listing definitions against what the host has, so it wants
// them all, and asking once means every row is read from a single consistent
// snapshot instead of from a host that may change between questions.
//
// This is the interface the kernel actually offers. An earlier version of this
// package read /sys/class/net/<dev>/8021q/vlan_id, which does not exist and
// never has: the 8021q driver registers no sysfs attributes at all — its only
// sysfs trace is DEVTYPE=vlan in the device's uevent, which names the driver
// but not the tag. The read therefore failed on every host, and the caller,
// unable to distinguish "not a VLAN" from "could not tell", reported every
// working tagged interface as a name collision. procfs would answer (the
// driver does publish /proc/net/vlan/<dev>), but it is a text file the kernel
// documents as human-readable, it is mode 0600, and it is absent without
// CONFIG_PROC_FS. Netlink is where this information is defined to live, and
// the nest below is the exact one AddVLAN builds, read back.
func VLANLinks() (map[string]VLANLink, error) {
	// An empty ifinfomsg: no filter, so the kernel dumps every link.
	msgs, err := netlinkDump(syscall.RTM_GETLINK, make([]byte, 16))
	if err != nil {
		return nil, fmt.Errorf("listing links: %w", err)
	}
	return parseVLANLinks(msgs), nil
}

// parseVLANLinks picks the tagged interfaces out of a set of RTM_GETLINK
// message bodies. Separate from the dump so it can be driven with the bytes a
// kernel would send, on a host whose kernel cannot make a VLAN to ask.
func parseVLANLinks(msgs [][]byte) map[string]VLANLink {
	// Parent resolution is deferred to a second pass. IFLA_LINK gives the
	// parent's ifindex, and the message naming that index may come after the
	// message referring to it — resolving inline would drop the parent of any
	// VLAN the kernel happened to dump first.
	names := make(map[int]string, len(msgs))
	type found struct {
		link      VLANLink
		parentIdx int
	}
	var vlans []found

	for _, m := range msgs {
		// struct ifinfomsg: family, pad, type, index, flags, change.
		if len(m) < 16 {
			continue
		}
		idx := int(int32(binary.NativeEndian.Uint32(m[4:8])))
		var (
			name      string
			parentIdx int
			isVLAN    bool
			id        int
			haveID    bool
			proto     uint16 = EthPrt8021Q
		)
		forEachAttr(m[16:], func(typ uint16, data []byte) {
			switch typ {
			case syscall.IFLA_IFNAME:
				name = nlString(data)
			case iflaLink:
				if len(data) >= 4 {
					parentIdx = int(int32(binary.NativeEndian.Uint32(data)))
				}
			case iflaLinkInfo:
				forEachAttr(data, func(t uint16, d []byte) {
					switch t {
					case iflaInfoKind:
						isVLAN = nlString(d) == "vlan"
					case iflaInfoData:
						// Parsed whatever the kind turns out to be: the two
						// attributes are siblings and the kernel is not
						// obliged to order them, so keying the parse on
						// having already seen the kind would depend on
						// something that is not guaranteed. The kind decides
						// whether the result is kept, below.
						forEachAttr(d, func(vt uint16, vd []byte) {
							switch vt {
							case iflaVLANID:
								if len(vd) >= 2 {
									id = int(binary.NativeEndian.Uint16(vd))
									haveID = true
								}
							case iflaVLANProtocol:
								if len(vd) >= 2 {
									proto = binary.BigEndian.Uint16(vd)
								}
							}
						})
					}
				})
			}
		})
		if name == "" {
			continue
		}
		names[idx] = name
		if isVLAN && haveID {
			vlans = append(vlans, found{VLANLink{Name: name, ID: id, Protocol: proto}, parentIdx})
		}
	}

	out := make(map[string]VLANLink, len(vlans))
	for _, f := range vlans {
		l := f.link
		l.Parent = names[f.parentIdx]
		out[l.Name] = l
	}
	return out
}

// nlString reads a netlink string attribute, which is NUL-terminated and
// padded — taking the payload whole would keep the terminator and every
// comparison against it would fail.
func nlString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// SetLinkUp brings an interface up. A freshly created VLAN device is down,
// and an interface that is down carries no traffic and accepts no address
// from a DHCP client, so creating one without this produces a device that
// looks right in the interface list and does nothing.
func SetLinkUp(name string) error {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %s: %w", name, err)
	}
	body := make([]byte, 16)
	binary.NativeEndian.PutUint32(body[4:8], uint32(ifi.Index))
	// flags and change both carry IFF_UP: change is the mask saying which
	// bits of flags to act on, and a zero mask would make this a no-op.
	binary.NativeEndian.PutUint32(body[8:12], syscall.IFF_UP)
	binary.NativeEndian.PutUint32(body[12:16], syscall.IFF_UP)
	return netlinkExec(syscall.RTM_SETLINK, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK, body)
}
