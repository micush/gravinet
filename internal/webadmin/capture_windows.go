//go:build windows

package webadmin

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Windows blocks arbitrary raw-frame capture at the OS level — there is no
// syscall equivalent of Linux's AF_PACKET or macOS's /dev/bpf reachable from
// user mode. Real packet capture needs a dedicated driver; Npcap (what
// Wireshark installs, and what Wireshark's project maintains) is the de facto
// standard one, and — like classic WinPcap before it — it exposes a stable,
// long-unchanged C ABI via wpcap.dll ("WinPcap API-compatible mode", which is
// Npcap's default install option). This backend loads that DLL dynamically at
// runtime: if Npcap isn't installed, captureSupported is simply false and the
// UI shows the same "not supported" message as a platform with no backend.
//
// NOTE: matching a Go net.Interface (e.g. "Ethernet", "Wi-Fi", "mesh1") to an
// Npcap device is done primarily by GUID, not by name. Npcap/WinPcap name
// their devices "\Device\NPF_{GUID}", using the same GUID Windows assigns the
// adapter — obtained here via GetAdaptersAddresses' AdapterName field, keyed
// off the interface's index. That GUID match is exact and unambiguous.
//
// It matters because Npcap's device *description* is driver-level, not
// per-adapter: every gravinet TUN adapter shares one description ("gravinet
// Tunnel" / "gravinet Tunnel #N", one per Wintun pool instance) regardless of
// what the adapter is named or renamed to (e.g. a mesh's configured name,
// "mesh1"). With only one mesh configured that ambiguity is invisible; with
// two or more it isn't — description-only matching can pick the wrong
// adapter, or (as the friendly name won't appear in the description at all)
// find no match. So GUID lookup is tried first, and description matching
// (case-insensitive, substring either direction) is kept only as a fallback
// for cases the GUID lookup can't resolve — mainly physical NICs, where the
// friendly name and description are usually close enough to match anyway. If
// neither finds a confident match, the error lists what Npcap actually sees
// so you can tell what's going on.

var (
	wpcap               = syscall.NewLazyDLL("wpcap.dll")
	procPcapFindAllDevs = wpcap.NewProc("pcap_findalldevs")
	procPcapFreeAllDevs = wpcap.NewProc("pcap_freealldevs")
	procPcapOpenLive    = wpcap.NewProc("pcap_open_live")
	procPcapNextEx      = wpcap.NewProc("pcap_next_ex")
	procPcapClose       = wpcap.NewProc("pcap_close")
	procPcapDatalink    = wpcap.NewProc("pcap_datalink")
	procPcapGetErr      = wpcap.NewProc("pcap_geterr")

	iphlpapiCapture          = syscall.NewLazyDLL("iphlpapi.dll")
	procGetAdaptersAddresses = iphlpapiCapture.NewProc("GetAdaptersAddresses")
)

// captureSupported is a var (not a const) on Windows, since whether Npcap is
// actually installed can only be determined at runtime.
var captureSupported = wpcap.Load() == nil

const pcapErrbufSize = 256

// pcapIf mirrors struct pcap_if (pcap_if_t) — unchanged since WinPcap 3.x.
type pcapIf struct {
	next        uintptr
	name        *byte
	description *byte
	addresses   uintptr
	flags       uint32
	_           uint32 // padding to 8-byte alignment on 64-bit
}

// pcapPkthdr mirrors struct pcap_pkthdr. Windows' `struct timeval` (from
// winsock2.h) always uses 32-bit fields regardless of process bitness, unlike
// Unix's 64-bit-on-LP64 timeval — so this is 16 bytes total, not 24.
type pcapPkthdr struct {
	tvSec  int32
	tvUsec int32
	caplen uint32
	len    uint32
}

func cString(s string) []byte { return append([]byte(s), 0) }

// ipAdapterAddresses mirrors only the leading fields of Win32's
// IP_ADAPTER_ADDRESSES (iptypes.h) that are needed here: the union of
// Length+IfIndex (8 bytes, giving ULONGLONG alignment to the struct as a
// whole, unchanged since the struct's introduction), the Next link, and the
// AdapterName pointer. Every field after that has grown across Windows
// versions (Vista, Win7, Win8 each appended more), but since this is only
// ever read through a pointer into the OS-owned buffer GetAdaptersAddresses
// fills in — never allocated or laid out by Go — leaving the later fields
// out is harmless: nothing here reads past AdapterName, and the true
// per-record size (needed to step between adapters) comes from the record's
// own Next pointer, not from sizeof(ipAdapterAddresses).
type ipAdapterAddresses struct {
	length      uint32
	ifIndex     uint32
	next        uintptr
	adapterName *byte
}

const (
	gaaFlagSkipUnicast   = 0x0001
	gaaFlagSkipAnycast   = 0x0002
	gaaFlagSkipMulticast = 0x0004
	gaaFlagSkipDNSServer = 0x0008
	gaaFlagSkipFriendly  = 0x0020
	errBufferOverflow    = 111 // ERROR_BUFFER_OVERFLOW
	afUnspec             = 0
)

// adapterGUIDForIndex resolves a Go net.Interface.Index to the adapter GUID
// string Windows assigned it (e.g. "{4D36E972-E325-11CE-BFC1-08002BE10318}"),
// via GetAdaptersAddresses. This is the same GUID Npcap/WinPcap embed in
// their device name ("\Device\NPF_{GUID}"), which is what makes it useful
// for finding the Npcap device that corresponds to a specific adapter
// unambiguously — unlike the human-readable name/description, which for
// gravinet's TUN adapters is not unique when more than one mesh is
// configured (see the file-level comment above).
func adapterGUIDForIndex(ifIndex int) (string, bool) {
	flags := uintptr(gaaFlagSkipUnicast | gaaFlagSkipAnycast | gaaFlagSkipMulticast |
		gaaFlagSkipDNSServer | gaaFlagSkipFriendly)
	size := uint32(15000) // MSDN's suggested starting size
	var buf []byte
	for attempt := 0; attempt < 5; attempt++ {
		buf = make([]byte, size)
		r, _, _ := procGetAdaptersAddresses.Call(
			afUnspec,
			flags,
			0,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&size)),
		)
		if r == 0 { // ERROR_SUCCESS
			break
		}
		if r != errBufferOverflow || attempt == 4 {
			return "", false
		}
		// size was updated in place with the required byte count; loop and retry.
	}
	if len(buf) == 0 {
		return "", false
	}
	for p := uintptr(unsafe.Pointer(&buf[0])); p != 0; {
		aa := (*ipAdapterAddresses)(unsafe.Pointer(p))
		if aa.ifIndex == uint32(ifIndex) {
			if aa.adapterName == nil {
				return "", false
			}
			return goString(aa.adapterName), true
		}
		p = aa.next
	}
	return "", false
}

func goString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for {
		b := *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + uintptr(n)))
		if b == 0 {
			break
		}
		n++
	}
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + uintptr(i)))
	}
	return string(b)
}

// findDevice locates the Npcap device that corresponds to ifaceName. It first
// tries an exact match on adapter GUID (see adapterGUIDForIndex), and falls
// back to a best-effort, case-insensitive match against Npcap's device
// descriptions only if that doesn't resolve one.
func findDevice(ifaceName string) (string, error) {
	var head uintptr
	errbuf := make([]byte, pcapErrbufSize)
	r, _, _ := procPcapFindAllDevs.Call(uintptr(unsafe.Pointer(&head)), uintptr(unsafe.Pointer(&errbuf[0])))
	if int32(r) != 0 {
		return "", fmt.Errorf("pcap_findalldevs: %s", strings.TrimRight(string(errbuf), "\x00"))
	}
	defer procPcapFreeAllDevs.Call(head)

	type candidate struct{ name, desc string }
	var all []candidate
	for p := head; p != 0; {
		dev := (*pcapIf)(unsafe.Pointer(p))
		all = append(all, candidate{goString(dev.name), goString(dev.description)})
		p = dev.next
	}
	if len(all) == 0 {
		return "", fmt.Errorf("Npcap reports no capturable devices (is it installed with admin rights?)")
	}

	if ifi, err := net.InterfaceByName(ifaceName); err == nil {
		if guid, ok := adapterGUIDForIndex(ifi.Index); ok {
			want := strings.ToLower(`\Device\NPF_` + guid)
			for _, c := range all {
				if strings.ToLower(c.name) == want {
					return c.name, nil
				}
			}
		}
	}

	want := strings.ToLower(ifaceName)
	for _, c := range all {
		dl := strings.ToLower(c.desc)
		if dl == want || strings.Contains(dl, want) || strings.Contains(want, dl) {
			return c.name, nil
		}
	}

	var seen []string
	for _, c := range all {
		seen = append(seen, fmt.Sprintf("%q", c.desc))
	}
	return "", fmt.Errorf("no Npcap device matched %q; Npcap sees: %s", ifaceName, strings.Join(seen, ", "))
}

type windowsCapture struct {
	handle  uintptr
	stopped atomic.Bool
}

func (h *windowsCapture) stop() {
	h.stopped.Store(true)
	// pcap_next_ex is called with a short timeout, so the loop notices
	// stopped on its own; pcap_close from here as well in case it's idle.
	procPcapClose.Call(h.handle)
}

func startCapture(ifaceName string, snaplen int, onPacket func(time.Time, []byte)) (capHandle, int, error) {
	if !captureSupported {
		return nil, -1, fmt.Errorf("packet capture requires Npcap (https://npcap.com) to be installed")
	}
	device, err := findDevice(ifaceName)
	if err != nil {
		return nil, -1, err
	}

	errbuf := make([]byte, pcapErrbufSize)
	deviceC := cString(device)
	const toMs = 200 // short timeout so the read loop can notice stop() promptly
	r, _, _ := procPcapOpenLive.Call(
		uintptr(unsafe.Pointer(&deviceC[0])),
		uintptr(snaplen),
		1, // promiscuous
		toMs,
		uintptr(unsafe.Pointer(&errbuf[0])),
	)
	if r == 0 {
		return nil, -1, fmt.Errorf("pcap_open_live %q: %s", device, strings.TrimRight(string(errbuf), "\x00"))
	}
	handle := r

	dlt, _, _ := procPcapDatalink.Call(handle)
	linktype := linktypeRaw
	switch int32(dlt) {
	case 1: // DLT_EN10MB
		linktype = linktypeEthernet
	case 0: // DLT_NULL
		linktype = linktypeNull
	default:
		linktype = int(int32(dlt))
	}

	h := &windowsCapture{handle: handle}
	go h.loop(onPacket)
	return h, linktype, nil
}

func (h *windowsCapture) loop(onPacket func(time.Time, []byte)) {
	for {
		if h.stopped.Load() {
			return
		}
		var hdr *pcapPkthdr
		var data *byte
		r, _, _ := procPcapNextEx.Call(h.handle, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data)))
		switch int32(r) {
		case 1: // got a packet
			if hdr == nil || data == nil || hdr.caplen == 0 {
				continue
			}
			pkt := make([]byte, hdr.caplen)
			for i := uint32(0); i < hdr.caplen; i++ {
				pkt[i] = *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(data)) + uintptr(i)))
			}
			onPacket(time.Unix(int64(hdr.tvSec), int64(hdr.tvUsec)*1000), pkt)
		case 0: // timeout, no packet — loop back and check stopped
			continue
		default: // error or EOF
			return
		}
	}
}

// procPcapGetErr is resolved but not currently called — pcap_open_live and
// pcap_findalldevs already fill an errbuf directly, which covers today's
// error paths. Kept resolved for straightforward future use (e.g. surfacing
// pcap_next_ex failures in more detail).
