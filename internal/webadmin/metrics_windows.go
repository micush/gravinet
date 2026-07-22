//go:build windows

package webadmin

import (
	"net"
	"syscall"
	"unsafe"
)

var (
	kernel32Metrics          = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes       = kernel32Metrics.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32Metrics.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceEx   = kernel32Metrics.NewProc("GetDiskFreeSpaceExW")
	procGetTickCount64       = kernel32Metrics.NewProc("GetTickCount64")
	iphlpapiMetrics          = syscall.NewLazyDLL("iphlpapi.dll")
	procGetIfEntry           = iphlpapiMetrics.NewProc("GetIfEntry")
)

// filetime64 mirrors the Win32 FILETIME struct: two little-endian DWORDs that
// together form a 64-bit tick count.
type filetime64 struct{ low, high uint32 }

func (f filetime64) ticks() uint64 { return uint64(f.high)<<32 | uint64(f.low) }

// readCPUTotals uses GetSystemTimes, which reports idle/kernel/user time as
// FILETIMEs since boot. Per the Win32 docs, kernel time already includes idle
// time, so total = kernel+user and idle = idle — the same total/idle shape the
// collector already expects from the Linux /proc/stat reader.
func readCPUTotals() (total, idle uint64, ok bool) {
	var idleFT, kernelFT, userFT filetime64
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFT)),
		uintptr(unsafe.Pointer(&kernelFT)),
		uintptr(unsafe.Pointer(&userFT)),
	)
	if r == 0 {
		return 0, 0, false
	}
	idle = idleFT.ticks()
	total = kernelFT.ticks() + userFT.ticks()
	return total, idle, true
}

// memoryStatusEx mirrors Win32's MEMORYSTATUSEX.
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

// readMemUsedPct uses GlobalMemoryStatusEx for physical memory used percent.
func readMemUsedPct() (float64, bool) {
	var m memoryStatusEx
	m.dwLength = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 || m.ullTotalPhys == 0 {
		return 0, false
	}
	used := float64(m.ullTotalPhys-m.ullAvailPhys) / float64(m.ullTotalPhys) * 100
	return used, true
}

// readDiskUsedPct uses GetDiskFreeSpaceExW for used space on the system
// drive (C:\), the Windows analogue of statfs(2)/df on Unix. It reports
// TotalNumberOfFreeBytes rather than the caller's free-bytes-available (the
// two only differ under per-user disk quotas), matching what Explorer's
// drive properties and most disk-usage tools show.
func readDiskUsedPct() (float64, bool) {
	path, err := syscall.UTF16PtrFromString(`C:\`)
	if err != nil {
		return 0, false
	}
	var freeAvail, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 || total == 0 {
		return 0, false
	}
	return float64(total-totalFree) / float64(total) * 100, true
}

// readUptime uses GetTickCount64, which directly returns milliseconds
// elapsed since the system started — a documented Win32 API returning the
// value itself, unlike the BSD/Darwin readers, which have to shell out to
// sysctl and parse text. Rolls over after ~584 million years, i.e. never.
func readUptime() (uint64, bool) {
	r, _, _ := procGetTickCount64.Call()
	if r == 0 {
		return 0, false
	}
	return uint64(r) / 1000, true
}

// mibIfRow mirrors the classic (and still supported) Win32 MIB_IFROW struct
// from iprtrmib.h, used with GetIfEntry. It predates the newer MIB_IF_ROW2 /
// GetIfEntry2 API and has a smaller, long-stable layout, at the cost of
// 32-bit (rather than 64-bit) octet counters — they can wrap on a very fast
// link, but the collector's rate() helper already treats a counter going
// backwards as "skip this sample" rather than misreporting a huge rate.
const (
	maxInterfaceNameLen = 256
	maxLenPhysAddr      = 8
	maxLenIfDescr       = 256
)

type mibIfRow struct {
	wszName           [maxInterfaceNameLen]uint16
	dwIndex           uint32
	dwType            uint32
	dwMtu             uint32
	dwSpeed           uint32
	dwPhysAddrLen     uint32
	bPhysAddr         [maxLenPhysAddr]byte
	dwAdminStatus     uint32
	dwOperStatus      uint32
	dwLastChange      uint32
	dwInOctets        uint32
	dwInUcastPkts     uint32
	dwInNUcastPkts    uint32
	dwInDiscards      uint32
	dwInErrors        uint32
	dwInUnknownProtos uint32
	dwOutOctets       uint32
	dwOutUcastPkts    uint32
	dwOutNUcastPkts   uint32
	dwOutDiscards     uint32
	dwOutErrors       uint32
	dwOutQLen         uint32
	dwDescrLen        uint32
	bDescr            [maxLenIfDescr]byte
}

// readNetDev reports byte counters keyed by the same interface name Go's net
// package uses, so the collector's lookup by ii.Iface (from net.Interfaces())
// matches without extra translation.
func readNetDev() map[string]devCounters {
	out := map[string]devCounters{}
	ifis, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifis {
		var row mibIfRow
		row.dwIndex = uint32(ifi.Index)
		r, _, _ := procGetIfEntry.Call(uintptr(unsafe.Pointer(&row)))
		if r != 0 { // non-zero return means failure (NO_ERROR == 0)
			continue
		}
		out[ifi.Name] = devCounters{rx: uint64(row.dwInOctets), tx: uint64(row.dwOutOctets)}
	}
	return out
}
