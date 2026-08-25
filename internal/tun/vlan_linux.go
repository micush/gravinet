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
	// IFLA_LINKINFO and IFLA_LINK are in the same enum as syscall.IFLA_MTU
	// and are stable for the same reason.
	iflaLinkInfo = 18
	iflaLink     = 5
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
