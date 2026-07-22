//go:build !linux && !darwin && !freebsd && !openbsd && !windows

package netfilter

import "fmt"

// Manager is a no-op stub on platforms with no kernel NAT backend here
// (everything except linux, darwin, freebsd, openbsd, and windows, each of
// which has its own real implementation — see netfilter_linux.go,
// netfilter_pf.go + netfilter_{darwin,freebsd,openbsd}.go, and
// netfilter_windows.go).
type Manager struct{}

// New reports that kernel NAT is unsupported on this platform.
func New() (*Manager, error) {
	return nil, fmt.Errorf("kernel NAT is not supported on this platform")
}

func (m *Manager) Backend() string          { return "unsupported" }
func (m *Manager) Apply(rules []Rule) error { return nil }
func (m *Manager) Clear() error             { return nil }
