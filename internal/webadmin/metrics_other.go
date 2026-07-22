//go:build !linux && !darwin && !windows && !freebsd && !openbsd

package webadmin

// No dedicated metrics backend on this platform yet (linux, darwin, windows,
// freebsd, and openbsd are implemented) — the Metrics tab just reports
// itself unavailable rather than showing anything.
func readCPUTotals() (total, idle uint64, ok bool) { return 0, 0, false }
func readMemUsedPct() (float64, bool)              { return 0, false }
func readDiskUsedPct() (float64, bool)             { return 0, false }
func readUptime() (uint64, bool)                   { return 0, false }
func readNetDev() map[string]devCounters           { return map[string]devCounters{} }
