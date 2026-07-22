package upgrade

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeBinary writes an executable stand-in for a gravinet build: a script whose
// `version` output is the real, parseable line main.go prints. Everything the
// preflight does to a candidate — exec it, read its version, hand it a config —
// works on this, which is the point: the checks are about what a binary *does*
// when run, so a test binary that runs is a faithful test binary.
func fakeBinary(t *testing.T, dir, name, version string, pam bool, selftestOK bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shebang scripts are not executable on Windows; the swap logic is exercised on Unix")
	}
	pamS := "no"
	if pam {
		pamS = "yes"
	}
	path := filepath.Join(dir, name)
	fail := ""
	if !selftestOK {
		fail = `if [ "$1" = "selftest" ]; then echo "config: unknown field \"managed\"" >&2; exit 1; fi`
	}
	script := fmt.Sprintf("#!/bin/sh\n%s\necho \"gravinet %s (testbuild) %s/%s pam=%s\"\nexit 0\n",
		fail, version, runtime.GOOS, runtime.GOARCH, pamS)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// signedArtifact builds a fake binary, a manifest for it, and signs it, returning
// everything a store needs to accept it.
func signedArtifact(t *testing.T, version string, pam, selftestOK bool) (Manifest, string, ed25519.PrivateKey, string) {
	t.Helper()
	dir := t.TempDir()
	bin := fakeBinary(t, dir, "gravinet-artifact", version, pam, selftestOK)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManifest(bin, version, runtime.GOOS, runtime.GOARCH, pam, "test build")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Sign(priv); err != nil {
		t.Fatal(err)
	}
	return m, bin, priv, hex.EncodeToString(pub)
}

func mustIngest(t *testing.T, st *Store, m Manifest, bin string) {
	t.Helper()
	f, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := st.Ingest(m, f); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

func TestManifestSignAndVerify(t *testing.T) {
	m, _, _, pub := signedArtifact(t, "399", true, true)

	if err := m.Verify([]string{pub}); err != nil {
		t.Fatalf("a manifest we just signed should verify against its own key: %v", err)
	}
	// An empty trust set must fail closed. A node that has not been given any
	// release keys must not accept the first binary a peer offers it.
	if err := m.Verify(nil); !errors.Is(err, ErrNoTrustKeys) {
		t.Fatalf("empty trust set: got %v, want ErrNoTrustKeys", err)
	}
	// A different key, correctly signed by someone else, is not our key.
	_, otherPub, _ := func() (ed25519.PrivateKey, string, error) {
		pk, sk, _ := ed25519.GenerateKey(nil)
		return sk, hex.EncodeToString(pk), nil
	}()
	if err := m.Verify([]string{otherPub}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("untrusted signer: got %v, want ErrUntrusted", err)
	}
}

// The signature has to cover every field that changes what gets executed.
// Swapping the digest is the attack that matters: keep a valid signature, point
// it at different bytes.
func TestManifestTamperedFieldsFailVerification(t *testing.T) {
	base, _, _, pub := signedArtifact(t, "399", true, true)

	tamper := map[string]func(*Manifest){
		"digest":  func(m *Manifest) { m.SHA256 = strings.Repeat("a", 64) },
		"version": func(m *Manifest) { m.Version = "400" },
		"os":      func(m *Manifest) { m.OS = "windows" },
		"arch":    func(m *Manifest) { m.Arch = "arm64" },
		"size":    func(m *Manifest) { m.Size = m.Size + 1 },
		"pam":     func(m *Manifest) { m.PAM = !m.PAM },
		"notes":   func(m *Manifest) { m.Notes = "totally fine, ship it" },
	}
	for name, f := range tamper {
		t.Run(name, func(t *testing.T) {
			m := base
			f(&m)
			if err := m.Verify([]string{pub}); err == nil {
				t.Fatalf("tampering with %s left the signature verifying", name)
			}
		})
	}
}

// The version/os/arch strings become a filename and a URL query parameter, so
// they are the natural place to try a traversal.
func TestManifestRejectsPathTraversal(t *testing.T) {
	for _, bad := range []string{"../../etc/gravinet/config", "399/../../x", "a b", "399\n"} {
		m := Manifest{Version: bad, OS: "linux", Arch: "amd64", Size: 10, SHA256: strings.Repeat("0", 64)}
		if err := m.Validate(); err == nil {
			t.Fatalf("version %q was accepted", bad)
		}
	}
	m := Manifest{Version: "399", OS: "linux", Arch: "amd64", Size: 10, SHA256: strings.Repeat("0", 64), Notes: "a\nsigner=deadbeef"}
	if err := m.Validate(); err == nil {
		t.Fatal("a note containing a newline could forge a line in the signed byte string")
	}
}

func TestStoreIngestRejectsWrongBytes(t *testing.T) {
	m, _, _, pub := signedArtifact(t, "399", true, true)
	st, err := NewStore(t.TempDir(), []string{pub})
	if err != nil {
		t.Fatal(err)
	}
	// Correct manifest, different binary: the digest is the only thing standing
	// between a peer and arbitrary code execution on this node.
	other := fakeBinary(t, t.TempDir(), "impostor", "399", true, true)
	f, err := os.Open(other)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Make it a different length as well as different content, so we're not
	// relying on the two scripts happening to differ in size.
	if err := st.Ingest(m, io.MultiReader(f, strings.NewReader("# padding\n"))); err == nil {
		t.Fatal("bytes that do not match the manifest digest were ingested")
	}
	if st.Have(m) {
		t.Fatal("a rejected artifact was left in the store")
	}
	// And nothing half-written was left behind for a later apply to find.
	ents, _ := os.ReadDir(st.Dir())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".ingest-") {
			t.Fatalf("a temp file survived a failed ingest: %s", e.Name())
		}
	}
}

func TestStoreIngestRejectsUntrustedSigner(t *testing.T) {
	m, bin, _, _ := signedArtifact(t, "399", true, true)
	st, err := NewStore(t.TempDir(), []string{strings.Repeat("ab", 32)}) // some other key
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := st.Ingest(m, f); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("got %v, want ErrUntrusted", err)
	}
}

// A key removed from the config must stop being usable immediately, not linger
// because the artifact verified back when it was written.
func TestStoreListDropsArtifactsWhoseKeyIsNoLongerTrusted(t *testing.T) {
	m, bin, _, pub := signedArtifact(t, "399", true, true)
	dir := t.TempDir()
	st, _ := NewStore(dir, []string{pub})
	mustIngest(t, st, m, bin)
	if len(st.List()) != 1 {
		t.Fatal("artifact should be listed while its key is trusted")
	}
	revoked, _ := NewStore(dir, []string{strings.Repeat("cd", 32)})
	if len(revoked.List()) != 0 {
		t.Fatal("an artifact signed by a no-longer-trusted key is still being offered")
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"398", "399", true},
		{"399", "398", false},
		{"99", "100", true},  // the reason this is not a string compare
		{"100", "99", false}, //
		{"399", "399", false},
		{"1.2.0", "1.3.0", true}, // non-numeric falls back to lexical, but stays total
	}
	for _, c := range cases {
		if got := VersionLess(c.a, c.b); got != c.want {
			t.Errorf("VersionLess(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPreflightCatchesTheThingsThatBrickNodes(t *testing.T) {
	ctx := context.Background()
	m, _, _, pub := signedArtifact(t, "399", true, true)
	st, _ := NewStore(t.TempDir(), []string{pub})

	t.Run("wrong arch", func(t *testing.T) {
		bad := m
		bad.Arch = "s390x"
		if _, err := Preflight(ctx, bad, "/bin/true", Options{}); err == nil ||
			!strings.Contains(err.Error(), "this node is") {
			t.Fatalf("an artifact for another architecture was accepted: %v", err)
		}
	})

	t.Run("binary does not run", func(t *testing.T) {
		junk := filepath.Join(t.TempDir(), "truncated")
		os.WriteFile(junk, []byte("\x7fELF not really"), 0o755)
		if _, err := Preflight(ctx, m, junk, Options{}); err == nil {
			t.Fatal("a binary that cannot execute passed preflight — this is the truncated-download case")
		}
	})

	t.Run("binary reports a different version than the manifest", func(t *testing.T) {
		lying := fakeBinary(t, t.TempDir(), "lying", "397", true, true)
		if _, err := Preflight(ctx, m, lying, Options{}); err == nil ||
			!strings.Contains(err.Error(), "reports") {
			t.Fatalf("a binary whose self-reported version contradicts the manifest was accepted: %v", err)
		}
	})

	t.Run("silent PAM downgrade", func(t *testing.T) {
		mNoPAM, binNoPAM, _, pub2 := signedArtifact(t, "399", false, true)
		st2, _ := NewStore(t.TempDir(), []string{pub2})
		mustIngest(t, st2, mNoPAM, binNoPAM)
		_, err := Preflight(ctx, mNoPAM, binNoPAM, Options{RunningPAM: true})
		if err == nil || !strings.Contains(err.Error(), "PAM") {
			t.Fatalf("replacing a PAM build with a non-PAM one was allowed silently: %v", err)
		}
		if _, err := Preflight(ctx, mNoPAM, binNoPAM, Options{RunningPAM: true, AllowPAMDowngrade: true}); err != nil {
			t.Fatalf("the explicit override should permit it: %v", err)
		}
	})

	t.Run("new binary rejects this node's config", func(t *testing.T) {
		mBad, binBad, _, pubBad := signedArtifact(t, "399", true, false /* selftest fails */)
		stBad, _ := NewStore(t.TempDir(), []string{pubBad})
		mustIngest(t, stBad, mBad, binBad)
		cfg := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(cfg, []byte("{}"), 0o600)
		_, err := Preflight(ctx, mBad, binBad, Options{ConfigPath: cfg})
		if err == nil || !strings.Contains(err.Error(), "rejected this node's config") {
			t.Fatalf("a binary that refuses this node's config passed preflight: %v", err)
		}
	})

	t.Run("downgrade", func(t *testing.T) {
		old, oldBin, _, oldPub := signedArtifact(t, "397", true, true)
		st3, _ := NewStore(t.TempDir(), []string{oldPub})
		mustIngest(t, st3, old, oldBin)
		if _, err := Preflight(ctx, old, oldBin, Options{RunningVersion: "398"}); err == nil {
			t.Fatal("a downgrade was accepted without the explicit flag")
		}
		if _, err := Preflight(ctx, old, oldBin, Options{RunningVersion: "398", AllowDowngrade: true}); err != nil {
			t.Fatalf("an explicitly allowed downgrade was refused: %v", err)
		}
	})
	_ = st
}

func TestApplySwapsAndRevertRestores(t *testing.T) {
	m, bin, _, pub := signedArtifact(t, "399", false, true)
	st, _ := NewStore(t.TempDir(), []string{pub})
	mustIngest(t, st, m, bin)

	installDir := t.TempDir()
	target := fakeBinary(t, installDir, "gravinet", "398", false, true)
	before, _ := os.ReadFile(target)

	p, err := Apply(context.Background(), st, m, Options{Target: target, RunningVersion: "398"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if p.Version != "399" {
		t.Fatalf("probe reported %q", p.Version)
	}
	// The installed path now runs the new binary...
	if got := probeVersion(t, target); got != "399" {
		t.Fatalf("target reports %q after apply, want 399", got)
	}
	// ...the old one is right next to it, byte for byte...
	prev, err := os.ReadFile(BackupPath(target))
	if err != nil {
		t.Fatalf("no backup was kept: %v", err)
	}
	if string(prev) != string(before) {
		t.Fatal("the backup is not the binary that was replaced")
	}
	// ...and no staging file was left lying around next to it.
	if _, err := os.Stat(target + ".new"); err == nil {
		t.Fatal("a .new file survived a successful apply")
	}

	if err := Revert(target); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got := probeVersion(t, target); got != "398" {
		t.Fatalf("target reports %q after revert, want 398", got)
	}
	if _, err := os.Stat(target + ".failed"); err != nil {
		t.Fatal("the backed-out binary should be kept for forensics")
	}
}

// A failed preflight must not touch the installed binary at all.
func TestApplyLeavesTargetAloneWhenPreflightFails(t *testing.T) {
	m, bin, _, pub := signedArtifact(t, "399", true, false /* rejects config */)
	st, _ := NewStore(t.TempDir(), []string{pub})
	mustIngest(t, st, m, bin)

	installDir := t.TempDir()
	target := fakeBinary(t, installDir, "gravinet", "398", true, true)
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte("{}"), 0o600)

	if _, err := Apply(context.Background(), st, m, Options{Target: target, ConfigPath: cfg}); err == nil {
		t.Fatal("apply succeeded despite a failing preflight")
	}
	if got := probeVersion(t, target); got != "398" {
		t.Fatalf("the installed binary changed to %q despite a failed preflight", got)
	}
	if _, err := os.Stat(BackupPath(target)); err == nil {
		t.Fatal("a backup was taken even though nothing was replaced")
	}
	if _, err := os.Stat(target + ".new"); err == nil {
		t.Fatal("the staged candidate was left next to the target after a failed preflight")
	}
}

func probeVersion(t *testing.T, path string) string {
	t.Helper()
	p, err := ProbeBinary(context.Background(), path)
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	return p.Version
}

// --- guard ---

// The crash-loop case: the new binary starts, but never reaches health, over and
// over. Nobody is coming to help — the manager cannot reach a node that is not
// on the mesh — so the node has to back itself out.
func TestGuardRevertsAfterBootLoop(t *testing.T) {
	m, bin, _, pub := signedArtifact(t, "399", false, true)
	st, _ := NewStore(t.TempDir(), []string{pub})
	mustIngest(t, st, m, bin)

	installDir := t.TempDir()
	target := fakeBinary(t, installDir, "gravinet", "398", false, true)

	restarts := 0
	g := NewGuard(st.Dir(), func() error { restarts++; return nil }, t.Logf)
	if err := g.Arm(State{Target: target, From: "398", To: "399", ArtifactID: m.ID(), PrePeers: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), st, m, Options{Target: target, RunningVersion: "398"}); err != nil {
		t.Fatal(err)
	}

	// Each call is one start of the new binary that never confirms.
	for i := 1; i <= MaxBoots; i++ {
		act, s := g.OnBoot()
		if act != BootNormal {
			t.Fatalf("boot %d: reverted early (allowance is %d)", i, MaxBoots)
		}
		if s.Boots != i {
			t.Fatalf("boot %d: counted %d", i, s.Boots)
		}
		if got := probeVersion(t, target); got != "399" {
			t.Fatalf("boot %d: binary is %q, should still be the new one", i, got)
		}
	}
	act, s := g.OnBoot()
	if act != BootReverted {
		t.Fatalf("the boot after the allowance was spent should revert, got %v", act)
	}
	if s.Phase != PhaseReverted {
		t.Fatalf("phase is %q, want reverted", s.Phase)
	}
	if got := probeVersion(t, target); got != "398" {
		t.Fatalf("after revert the installed binary is %q, want the old 398", got)
	}
	if restarts != 1 {
		t.Fatalf("the restored binary was not put back into service (restarts=%d)", restarts)
	}
	// The restored (old) binary now boots. It must not count itself against the
	// failed upgrade and try to revert the revert.
	if act, _ := g.OnBoot(); act != BootNormal {
		t.Fatal("the rescued binary tried to rescue itself")
	}
}

// The other failure: the new binary runs fine but cannot rejoin the mesh. The
// process is healthy; the node is not.
func TestGuardWatchRevertsWhenHealthNeverArrives(t *testing.T) {
	m, bin, _, pub := signedArtifact(t, "399", false, true)
	st, _ := NewStore(t.TempDir(), []string{pub})
	mustIngest(t, st, m, bin)
	target := fakeBinary(t, t.TempDir(), "gravinet", "398", false, true)

	restarted := make(chan struct{}, 1)
	g := NewGuard(st.Dir(), func() error { restarted <- struct{}{}; return nil }, t.Logf)
	// A one-second window, so the test does not take ninety.
	if err := g.Arm(State{Target: target, From: "398", To: "399", PrePeers: 4, ConfirmSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), st, m, Options{Target: target}); err != nil {
		t.Fatal(err)
	}
	g.OnBoot()
	g.Watch(func() (bool, string) { return false, "0 of 4 peers reconnected" })

	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the watchdog never fired; a node that came up without the mesh stayed on the new binary")
	}
	s := g.Load()
	if s.Phase != PhaseReverted {
		t.Fatalf("phase %q, want reverted", s.Phase)
	}
	if !strings.Contains(s.LastError, "peers reconnected") {
		t.Fatalf("the recorded reason (%q) should be the health check's own, not a generic one", s.LastError)
	}
	if got := probeVersion(t, target); got != "398" {
		t.Fatalf("installed binary is %q after the watchdog reverted, want 398", got)
	}
}

func TestGuardWatchCommitsWhenHealthy(t *testing.T) {
	m, bin, _, pub := signedArtifact(t, "399", false, true)
	st, _ := NewStore(t.TempDir(), []string{pub})
	mustIngest(t, st, m, bin)
	target := fakeBinary(t, t.TempDir(), "gravinet", "398", false, true)

	g := NewGuard(st.Dir(), func() error { t.Error("a healthy node must not be restarted"); return nil }, t.Logf)
	g.Arm(State{Target: target, From: "398", To: "399", PrePeers: 4, ConfirmSeconds: 1})
	Apply(context.Background(), st, m, Options{Target: target})
	g.OnBoot()
	g.Watch(func() (bool, string) { return true, "" })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if g.Load().Phase == PhaseCommitted {
			// The backup is deliberately kept even after a commit: `upgrade
			// rollback` is for regressions that health checks do not see.
			if _, err := os.Stat(BackupPath(target)); err != nil {
				t.Fatal("the backup was deleted on commit, leaving no way back")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("never committed; phase is %q", g.Load().Phase)
}
