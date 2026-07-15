//go:build openbsd

package netfilter

import (
	"fmt"
	"os/exec"
)

// openbsdAnchor is a plain gravinet-owned anchor name (OpenBSD, like
// FreeBSD, has no default wildcard anchor — see netfilter_pf_bsdhook.go for
// why we hook it into /etc/pf.conf ourselves instead).
const openbsdAnchor = "gravinet_nat"

// New wires up the gravinet anchor in /etc/pf.conf if it isn't already
// there, ensures pf itself is enabled, and returns a Manager whose Clear
// restores pf to whatever enabled/disabled state it found (it only disables
// pf on Clear if this call was the one that enabled it).
func New() (*Manager, error) {
	if _, err := exec.LookPath("pfctl"); err != nil {
		return nil, fmt.Errorf("pfctl not found on PATH")
	}
	if err := ensureAnchorHook(openbsdAnchor); err != nil {
		return nil, fmt.Errorf("wiring gravinet pf anchor: %w", err)
	}
	weEnabledIt := enablePF()
	release := func() {
		if weEnabledIt {
			_ = exec.Command("pfctl", "-d").Run()
		}
	}
	return &Manager{anchor: openbsdAnchor, release: release}, nil
}
