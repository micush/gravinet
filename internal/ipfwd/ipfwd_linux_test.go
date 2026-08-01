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

func TestDisableRedirectsFromOn(t *testing.T) {
	dir := t.TempDir()
	v4a := filepath.Join(dir, "v4accept")
	v4s := filepath.Join(dir, "v4send")
	v6a := filepath.Join(dir, "v6accept")
	os.WriteFile(v4a, []byte("1\n"), 0o644)
	os.WriteFile(v4s, []byte("1\n"), 0o644)
	os.WriteFile(v6a, []byte("1\n"), 0o644)
	procV4AcceptRedirects, procV4SendRedirects, procV6AcceptRedirects = v4a, v4s, v6a

	st := DisableRedirects(true, true)
	if st.V4Failed || st.V6Failed {
		t.Fatalf("unexpected failure: %+v", st)
	}
	for _, f := range []string{v4a, v4s, v6a} {
		if got, _ := os.ReadFile(f); string(got) != "0\n" {
			t.Fatalf("%s not disabled: %q", f, got)
		}
	}
	RestoreRedirects(st)
	for _, f := range []string{v4a, v4s, v6a} {
		if got, _ := os.ReadFile(f); string(got) != "1\n" {
			t.Fatalf("%s not restored to 1: %q", f, got)
		}
	}
}

func TestDisableRedirectsPreservesAlreadyOff(t *testing.T) {
	dir := t.TempDir()
	v4a := filepath.Join(dir, "v4accept")
	os.WriteFile(v4a, []byte("0\n"), 0o644)
	procV4AcceptRedirects = v4a
	procV4SendRedirects = filepath.Join(dir, "nonexistent-send")
	procV6AcceptRedirects = filepath.Join(dir, "nonexistent-v6") // simulate IPv6 disabled

	st := DisableRedirects(true, true)
	if !st.V6Missing() {
		t.Error("expected V6Missing when knob absent")
	}
	// Restoring must leave an already-off knob off, not flip it to 1.
	RestoreRedirects(st)
	if got, _ := os.ReadFile(v4a); string(got) != "0\n" {
		t.Fatalf("already-off accept_redirects must stay off after restore, got %q", got)
	}
}

func TestDisableRedirectsV6MissingNotFatal(t *testing.T) {
	dir := t.TempDir()
	procV4AcceptRedirects = filepath.Join(dir, "v4accept")
	procV4SendRedirects = filepath.Join(dir, "v4send")
	os.WriteFile(procV4AcceptRedirects, []byte("1\n"), 0o644)
	os.WriteFile(procV4SendRedirects, []byte("1\n"), 0o644)
	procV6AcceptRedirects = filepath.Join(dir, "absent")

	st := DisableRedirects(true, true)
	if st.V6Failed {
		t.Error("absent v6 redirect knob should be Missing, not Failed")
	}
	if !st.V6Missing() {
		t.Error("expected V6Missing")
	}
}
