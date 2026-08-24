package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// writeTempCfg saves cfg to a fresh temp dir and returns the path.
func writeTempCfg(t *testing.T, cfg *config.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	return path
}

// TestEnsureNodeIdentityFillsAndPersists covers the case the whole change
// exists for: a config on disk with node_id and hostname empty. Both must be
// populated in memory *and* survive to the file, because an id that is only
// in memory is regenerated next boot and peers key their tables on it.
func TestEnsureNodeIdentityFillsAndPersists(t *testing.T) {
	cfg := config.Default()
	path := writeTempCfg(t, cfg)

	ensureNodeIdentity(cfg, path)

	if cfg.NodeID == "" {
		t.Fatal("node_id still empty after ensureNodeIdentity")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.NodeID != cfg.NodeID {
		t.Errorf("node_id not persisted: file has %q, want %q", reloaded.NodeID, cfg.NodeID)
	}
	// Asserted as "whatever was generated, that is what landed on disk"
	// rather than against a literal: the detected name depends on the host
	// running the test, and ensureNodeIdentity legitimately leaves it empty
	// where the OS hostname is unreadable.
	if reloaded.Hostname != cfg.Hostname {
		t.Errorf("hostname not persisted: file has %q, want %q", reloaded.Hostname, cfg.Hostname)
	}
}

// TestEnsureNodeIdentityPreservesExisting guards the obvious way to get this
// wrong: rewriting an identity that was already set.
func TestEnsureNodeIdentityPreservesExisting(t *testing.T) {
	cfg := config.Default()
	cfg.NodeID = "0123456789abcdef"
	cfg.Hostname = "grav1"
	path := writeTempCfg(t, cfg)

	ensureNodeIdentity(cfg, path)

	if cfg.NodeID != "0123456789abcdef" {
		t.Errorf("node_id overwritten: got %q", cfg.NodeID)
	}
	if cfg.Hostname != "grav1" {
		t.Errorf("hostname overwritten: got %q", cfg.Hostname)
	}
}

// TestEnsureNodeIdentityKeepsAnFQDNHostname checks that an operator-set name
// is not put through shortHostname. Only auto-detected names are shortened.
func TestEnsureNodeIdentityKeepsAnFQDNHostname(t *testing.T) {
	cfg := config.Default()
	cfg.Hostname = "grav1.cush.local"
	path := writeTempCfg(t, cfg)

	ensureNodeIdentity(cfg, path)

	if cfg.Hostname != "grav1.cush.local" {
		t.Errorf("configured FQDN was rewritten to %q", cfg.Hostname)
	}
}

// TestEnsureNodeIdentityIDsAreDistinct is the grav1/grav2 case. Two nodes
// booting from identically empty configs must not end up with the same id:
// the engine treats a handshake carrying its own node id as a NAT hairpin and
// drops it, so a shared id means the two can never peer.
func TestEnsureNodeIdentityIDsAreDistinct(t *testing.T) {
	grav1, grav2 := config.Default(), config.Default()

	ensureNodeIdentity(grav1, writeTempCfg(t, grav1))
	ensureNodeIdentity(grav2, writeTempCfg(t, grav2))

	if grav1.NodeID == grav2.NodeID {
		t.Fatalf("both nodes generated the same node_id %q", grav1.NodeID)
	}
}

// withOSHostname points the osHostname seam at a fixed result for one test.
func withOSHostname(t *testing.T, name string, err error) {
	t.Helper()
	prev := osHostname
	osHostname = func() (string, error) { return name, err }
	t.Cleanup(func() { osHostname = prev })
}

// TestEnsureNodeIdentityShortensADetectedFQDN is the OpenBSD case: /etc/myname
// commonly holds a full domain name and os.Hostname echoes it back verbatim.
// The name is gossiped mesh-wide, so it has to be shortened on the way into
// the config, once, rather than left for every reader to trim.
//
// This is the test the suite was missing. Every host these tests run on
// already reports a short name, so shortHostname was a no-op on the real
// lookup and removing the call from ensureNodeIdentity broke nothing visible.
func TestEnsureNodeIdentityShortensADetectedFQDN(t *testing.T) {
	withOSHostname(t, "gn-openbsd.cush.local", nil)

	cfg := config.Default()
	path := writeTempCfg(t, cfg)

	ensureNodeIdentity(cfg, path)

	if cfg.Hostname != "gn-openbsd" {
		t.Errorf("detected FQDN not shortened: got %q, want %q", cfg.Hostname, "gn-openbsd")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Hostname != "gn-openbsd" {
		t.Errorf("short name not persisted: file has %q", reloaded.Hostname)
	}
}

// TestEnsureNodeIdentityUnreadableOSHostname and the case below it both assert
// the same contract from two directions: a hostname that cannot be determined
// leaves the field empty and does not block the id, because a node with no
// advertised name still peers fine and a node with no id does not peer at all.
func TestEnsureNodeIdentityUnreadableOSHostname(t *testing.T) {
	withOSHostname(t, "", errors.New("nope"))

	cfg := config.Default()
	path := writeTempCfg(t, cfg)

	ensureNodeIdentity(cfg, path)

	if cfg.Hostname != "" {
		t.Errorf("hostname invented from a failed lookup: %q", cfg.Hostname)
	}
	if cfg.NodeID == "" {
		t.Error("node_id not generated when the hostname lookup failed")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.NodeID != cfg.NodeID {
		t.Errorf("node_id not persisted: file has %q, want %q", reloaded.NodeID, cfg.NodeID)
	}
}

// TestEnsureNodeIdentityUnusableOSHostname covers a lookup that succeeds but
// yields nothing to use: an empty name, or the degenerate leading-dot form
// shortHostname reduces to "". Both must leave the field empty rather than
// writing a name no operator could look up.
func TestEnsureNodeIdentityUnusableOSHostname(t *testing.T) {
	for _, osName := range []string{"", ".", ".cush.local"} {
		t.Run("os_hostname_"+osName, func(t *testing.T) {
			withOSHostname(t, osName, nil)

			cfg := config.Default()
			ensureNodeIdentity(cfg, writeTempCfg(t, cfg))

			if cfg.Hostname != "" {
				t.Errorf("OS hostname %q yielded %q; want no hostname", osName, cfg.Hostname)
			}
			if cfg.NodeID == "" {
				t.Errorf("node_id not generated for OS hostname %q", osName)
			}
		})
	}
}

// captureLog redirects logx to a buffer for one test and returns it.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prevLevel := logx.Level(logx.LevelInfo)
	logx.SetLevel(logx.LevelInfo)
	logx.SetOutput(buf)
	t.Cleanup(func() {
		logx.SetOutput(os.Stderr)
		logx.SetLevel(prevLevel)
	})
	return buf
}

// TestEnsureNodeIdentityWritesNothingWhenNothingWasFilled pins the guard that
// the value assertions cannot see. With the id already set and the OS name
// unusable, the hostname branch produces "" either way — but taking the
// default branch on that empty string would add a bogus "hostname " to the
// filled list, log a line claiming a name was generated, and rewrite the file
// to say exactly what it already said. Observed by deleting the config first:
// a save that should not happen recreates it.
func TestEnsureNodeIdentityWritesNothingWhenNothingWasFilled(t *testing.T) {
	withOSHostname(t, ".", nil)
	log := captureLog(t)

	cfg := config.Default()
	cfg.NodeID = "0123456789abcdef"
	path := writeTempCfg(t, cfg)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	ensureNodeIdentity(cfg, path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("config rewritten when nothing was filled in (stat err: %v)", err)
	}
	if strings.Contains(log.String(), "generated") {
		t.Errorf("logged a generated value when nothing was generated: %s", log.String())
	}
}

// TestEnsureNodeIdentityDistinguishesHostnameFailures checks that the two
// no-hostname paths say different things. They reach the same outcome, so
// nothing about the config can tell them apart, and the log line is the only
// thing an operator has to work out whether the OS lookup failed or returned
// something unusable. Left unpinned, the read-failure branch can be deleted
// without any test noticing.
func TestEnsureNodeIdentityDistinguishesHostnameFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		osName string
		err    error
		want   string
	}{
		{"lookup failed", "", errors.New("sysctl: no such file"), "could not be read"},
		{"nothing usable", ".cush.local", nil, "no usable short name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withOSHostname(t, tc.osName, tc.err)
			log := captureLog(t)

			cfg := config.Default()
			ensureNodeIdentity(cfg, writeTempCfg(t, cfg))

			if got := log.String(); !strings.Contains(got, tc.want) {
				t.Errorf("warning does not identify the cause: want a mention of %q, got %s", tc.want, got)
			}
		})
	}
}

// TestEnsureNodeIdentityUnwritableConfig covers the best-effort contract: a
// config that cannot be written back must not stop the node coming up, and
// the generated values still apply to the running process.
func TestEnsureNodeIdentityUnwritableConfig(t *testing.T) {
	cfg := config.Default()
	// Parent directory deliberately never created, so the atomic save fails
	// at its CreateTemp step.
	path := filepath.Join(t.TempDir(), "no-such-dir", "config.json")

	ensureNodeIdentity(cfg, path)

	if cfg.NodeID == "" {
		t.Error("node_id not generated when the config could not be saved")
	}
}
