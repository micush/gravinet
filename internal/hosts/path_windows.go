//go:build windows

package hosts

import (
	"os"
	"path/filepath"
)

// DefaultPath is the Windows hosts file, resolved via %SystemRoot%.
func DefaultPath() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "drivers", "etc", "hosts")
}
