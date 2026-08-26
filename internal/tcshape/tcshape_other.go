//go:build !linux

package tcshape

import "fmt"

// Manager is a stub on platforms with no kernel shaping backend here.
//
// tc is Linux-specific. The BSDs would need altq/dummynet and Windows would
// need QoS policies; none is implemented, so rather than pretend, New fails
// and the caller reports that entries on non-mesh interfaces will not be
// enforced on this host. Mesh interfaces are unaffected — they are shaped in
// userspace on every platform.
type Manager struct{}

// New reports that kernel shaping is unsupported on this platform.
func New() (*Manager, error) {
	return nil, fmt.Errorf("kernel traffic shaping is not supported on this platform")
}

func (m *Manager) Backend() string            { return "unsupported" }
func (m *Manager) Apply(ifaces []Iface) error { return nil }
func (m *Manager) Clear() error               { return nil }
