//go:build linux

package ipfwd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnableRestoreFromOff(t *testing.T) {
	dir := t.TempDir()
	v4 := filepath.Join(dir, "ip_forward")
	v6 := filepath.Join(dir, "forwarding")
	os.WriteFile(v4, []byte("0\n"), 0o644)
	os.WriteFile(v6, []byte("0\n"), 0o644)
	procV4, procV6 = v4, v6

	st := Enable(true, true)
	if st.V4Failed || st.V6Failed {
		t.Fatalf("unexpected failure: %+v", st)
	}
	if got, _ := os.ReadFile(v4); string(got) != "1\n" {
		t.Fatalf("v4 not enabled: %q", got)
	}
	if got, _ := os.ReadFile(v6); string(got) != "1\n" {
		t.Fatalf("v6 not enabled: %q", got)
	}
	Restore(st)
	if got, _ := os.ReadFile(v4); string(got) != "0\n" {
		t.Fatalf("v4 not restored to 0: %q", got)
	}
	if got, _ := os.ReadFile(v6); string(got) != "0\n" {
		t.Fatalf("v6 not restored to 0: %q", got)
	}
}

func TestEnablePreservesAlreadyOn(t *testing.T) {
	dir := t.TempDir()
	v4 := filepath.Join(dir, "ip_forward")
	os.WriteFile(v4, []byte("1\n"), 0o644)
	procV4 = v4
	procV6 = filepath.Join(dir, "nonexistent") // simulate IPv6 disabled

	st := Enable(true, true)
	if !st.V6Missing() {
		t.Error("expected V6Missing when knob absent")
	}
	// Restoring must leave an already-on knob on, not revert it to 0.
	Restore(st)
	if got, _ := os.ReadFile(v4); string(got) != "1\n" {
		t.Fatalf("already-on forwarding must stay on after restore, got %q", got)
	}
}

func TestV6MissingNotFatal(t *testing.T) {
	dir := t.TempDir()
	procV4 = filepath.Join(dir, "ip_forward")
	os.WriteFile(procV4, []byte("0\n"), 0o644)
	procV6 = filepath.Join(dir, "absent")
	st := Enable(true, true)
	if st.V6Failed {
		t.Error("absent v6 knob should be Missing, not Failed")
	}
	if !st.V6Missing() {
		t.Error("expected V6Missing")
	}
}
