//go:build windows

// Kernel NAT on Windows via WinNAT, the built-in NAT engine behind the
// NetNat PowerShell module (also what Hyper-V's and Docker Desktop's NAT
// switches use under the hood). There is no netlink/ioctl-style API for it
// from Go, so — like the nft/iptables/pfctl backends, all of which are also
// just wrapped CLI tools — this shells out, here to powershell.exe.
package netfilter

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Manager applies the gravinet NAT ruleset via WinNAT.
type Manager struct{}

// New checks that PowerShell and the NetNat module are reachable. WinNAT
// itself is a Windows component (no separate install/enable step the way
// nft/iptables/pfctl need a binary on PATH), so there's nothing else to set
// up here.
func New() (*Manager, error) {
	psPath, err := powershellPath()
	if err != nil {
		return nil, err
	}
	if err := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command",
		"Get-Command New-NetNat -ErrorAction Stop | Out-Null").Run(); err != nil {
		return nil, fmt.Errorf("NetNat PowerShell module not available: %w", err)
	}
	return &Manager{}, nil
}

// Backend reports the NAT backend in use.
func (m *Manager) Backend() string { return "winnat" }

// Apply installs exactly the given rules, replacing any gravinet-owned
// NetNat objects from a previous call. Rules WinNAT cannot express (see
// winNATScript) are reported back as a combined error rather than silently
// skipped, same as the Linux backend's ip6tables-missing case.
func (m *Manager) Apply(rules []Rule) error {
	script, unsupported := winNATScript(rules)
	if err := m.runPS(script); err != nil {
		return err
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("WinNAT cannot express %d rule(s) (fixed-address SNAT, address-only DNAT, and IPv6 have no WinNAT equivalent — see winNATScript)", len(unsupported))
	}
	return nil
}

// Clear removes every NetNat object (and static mapping) gravinet owns.
func (m *Manager) Clear() error {
	script, _ := winNATScript(nil) // nil rules: script is just the teardown of prior objects
	return m.runPS(script)
}

func (m *Manager) runPS(script string) error {
	psPath, err := powershellPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", "-")
	cmd.Stdin = strings.NewReader(script)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("powershell: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// powershellPath prefers pwsh (PowerShell 7+) and falls back to Windows
// PowerShell, which ships on every supported Windows release.
func powershellPath() (string, error) {
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("neither pwsh.exe nor powershell.exe found on PATH")
}
