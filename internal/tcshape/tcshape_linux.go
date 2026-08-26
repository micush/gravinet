//go:build linux

package tcshape

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Manager programs kernel shaping via tc(8), remembering exactly which
// interfaces it has touched so teardown never reaches one it did not.
type Manager struct {
	bin     string
	applied map[string]bool
}

// New resolves tc. An error here means kernel shaping is unavailable, which
// the caller surfaces rather than swallowing: an entry that cannot be
// programmed is configuration doing nothing, and this page's whole history is
// about not letting that happen silently.
func New() (*Manager, error) {
	p, err := exec.LookPath("tc")
	if err != nil {
		return nil, fmt.Errorf("tc not found on PATH (install iproute2)")
	}
	return &Manager{bin: p, applied: map[string]bool{}}, nil
}

// Backend names the tool in use, mirroring netfilter.Manager.Backend.
func (m *Manager) Backend() string { return "tc" }

// Apply programs exactly the given interfaces and unprograms any it
// programmed before that are no longer listed. Idempotent.
//
// One interface failing does not abandon the rest: they are independent
// devices and a rate that cannot be set on one says nothing about another.
// The first error is returned once every interface has been attempted.
func (m *Manager) Apply(ifaces []Iface) error {
	want := make(map[string]bool, len(ifaces))
	var firstErr error

	for _, i := range ifaces {
		if i.Name == "" {
			continue
		}
		want[i.Name] = true
		if !i.Shaped() {
			// Listed but unlimited in both directions: nothing to install,
			// and anything we installed before has to come off.
			if m.applied[i.Name] {
				m.runAll(ClearPlan(i.Name))
				delete(m.applied, i.Name)
			}
			continue
		}
		if err := m.runAll(Plan(i)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		m.applied[i.Name] = true
	}

	for _, name := range m.trackedNot(want) {
		m.runAll(ClearPlan(name))
		delete(m.applied, name)
	}
	return firstErr
}

// Clear removes shaping from every interface this manager programmed.
func (m *Manager) Clear() error {
	for _, name := range m.trackedNot(nil) {
		m.runAll(ClearPlan(name))
		delete(m.applied, name)
	}
	return nil
}

// trackedNot lists the interfaces we have programmed that are absent from
// keep, in a stable order so teardown is reproducible in logs.
func (m *Manager) trackedNot(keep map[string]bool) []string {
	var out []string
	for name := range m.applied {
		if !keep[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// runAll executes a plan, stopping at the first non-tolerant failure.
func (m *Manager) runAll(cmds []Cmd) error {
	for _, c := range cmds {
		cmd := exec.Command(m.bin, c.Args...)
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			if c.Tolerant {
				continue
			}
			return fmt.Errorf("%s: %w: %s", c, err, strings.TrimSpace(errb.String()))
		}
	}
	return nil
}
