package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestNetworkDNSSyncBackfillOnMissingKey checks that a network JSON with no
// "dns_sync" key at all (any config predating conditional DNS forwarding, or
// one that otherwise never had the object) loads with DNSSync defaulted on —
// not silently disabled at encoding/json's zero value, which is
// indistinguishable from a deliberate choice and, once re-saved, becomes one.
func TestNetworkDNSSyncBackfillOnMissingKey(t *testing.T) {
	raw := `{"id":"1","name":"n"}`
	var n Network
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := NewNetworkDefaults().DNSSync
	if n.DNSSync != want {
		t.Fatalf("DNSSync = %+v, want default %+v", n.DNSSync, want)
	}
}

// TestNetworkDNSSyncExplicitValueRespected checks the backfill in
// Network.UnmarshalJSON never overrides a "dns_sync" object that's actually
// present — including one that happens to be all zeros, which is also a
// valid, deliberate configuration (disabled, unlimited gossip, default TTL)
// and must be left alone, not "corrected" back to the default.
func TestNetworkDNSSyncExplicitValueRespected(t *testing.T) {
	allZero := `{"id":"1","name":"n","dns_sync":{"enabled":false,"gossip_pps":0,"ttl_seconds":0}}`
	var n Network
	if err := json.Unmarshal([]byte(allZero), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.DNSSync != (DNSSync{}) {
		t.Fatalf("explicit all-zero dns_sync must be respected verbatim, got %+v", n.DNSSync)
	}

	explicit := `{"id":"1","name":"n","dns_sync":{"enabled":true,"gossip_pps":9,"ttl_seconds":60}}`
	var n2 Network
	if err := json.Unmarshal([]byte(explicit), &n2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := DNSSync{Enabled: true, GossipPPS: 9, TTLSeconds: 60}
	if n2.DNSSync != want {
		t.Fatalf("DNSSync = %+v, want %+v", n2.DNSSync, want)
	}
}

// TestDNSSyncSearchDomainsOnByDefaultForExistingConfig checks that
// DisableSearchDomains — added after dns_sync itself, so every config
// written before it exists has a "dns_sync" object with no
// "disable_search_domains" key at all — decodes to false (search-suffix
// promotion for learned forwards stays on) without any config edit, rather
// than silently landing disabled at encoding/json's zero value the way a
// positively-named "search_domains" flag would have. This is the field's
// whole reason for being named as a negative: the zero value must mean "on."
func TestDNSSyncSearchDomainsOnByDefaultForExistingConfig(t *testing.T) {
	preExisting := `{"id":"1","name":"n","dns_sync":{"enabled":true,"gossip_pps":5,"ttl_seconds":300}}`
	var n Network
	if err := json.Unmarshal([]byte(preExisting), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.DNSSync.DisableSearchDomains {
		t.Fatal("a dns_sync object predating this field must default to search domains ON (DisableSearchDomains=false)")
	}

	// And a brand-new network (no dns_sync key at all) gets the same default
	// via the backfill path.
	fresh := `{"id":"2","name":"n2"}`
	var n2 Network
	if err := json.Unmarshal([]byte(fresh), &n2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n2.DNSSync.DisableSearchDomains {
		t.Fatal("a fresh network's DNSSync default must have search domains ON")
	}

	// An operator that explicitly wants to opt out must have that respected.
	optedOut := `{"id":"3","name":"n3","dns_sync":{"enabled":true,"disable_search_domains":true}}`
	var n3 Network
	if err := json.Unmarshal([]byte(optedOut), &n3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !n3.DNSSync.DisableSearchDomains {
		t.Fatal("an explicit disable_search_domains:true must be respected")
	}
}

func TestLogFilePath(t *testing.T) {
	// Default: gravinet.log next to the config file.
	c := &Config{}
	if got := c.LogFilePath("/etc/gravinet/config.json"); got != "/etc/gravinet/gravinet.log" {
		t.Errorf("default log path = %q", got)
	}
	// Explicit override.
	c.LogFile = "/var/log/gn.log"
	if got := c.LogFilePath("/etc/gravinet/config.json"); got != "/var/log/gn.log" {
		t.Errorf("override log path = %q", got)
	}
	// Disabled sentinels.
	for _, off := range []string{"-", "none", "off"} {
		c.LogFile = off
		if got := c.LogFilePath("/etc/gravinet/config.json"); got != "" {
			t.Errorf("%q should disable file logging, got %q", off, got)
		}
	}
}

func TestReadmePath(t *testing.T) {
	dir := t.TempDir()
	// Explicit override always wins.
	c := &Config{ReadmeFile: "/opt/x/README.md"}
	if got := c.ReadmePath("/etc/gravinet/config.json", "/usr/local/bin"); got != "/opt/x/README.md" {
		t.Errorf("override = %q", got)
	}
	// exe-relative install location (…/bin -> …/share/doc/gravinet/README.md).
	c = &Config{}
	bindir := filepath.Join(dir, "bin")
	docdir := filepath.Join(dir, "share", "doc", "gravinet")
	os.MkdirAll(docdir, 0o755)
	os.WriteFile(filepath.Join(docdir, "README.md"), []byte("hi"), 0o644)
	if got := c.ReadmePath("/nonexistent/config.json", bindir); got != filepath.Join(docdir, "README.md") {
		t.Errorf("exe-relative resolution = %q", got)
	}
	// Windows layout: README sits beside the .exe (no share/doc tree). Use a
	// separate subtree so the earlier share/doc copy isn't reachable via "..".
	winbin := filepath.Join(dir, "win", "app")
	os.MkdirAll(winbin, 0o755)
	os.WriteFile(filepath.Join(winbin, "README.md"), []byte("hi"), 0o644)
	if got := c.ReadmePath("/nonexistent/config.json", winbin); got != filepath.Join(winbin, "README.md") {
		t.Errorf("beside-the-binary resolution = %q", got)
	}

	// Falls back to next-to-config when no exe-relative copy exists.
	cfgdir := filepath.Join(dir, "etc")
	os.MkdirAll(cfgdir, 0o755)
	os.WriteFile(filepath.Join(cfgdir, "README.md"), []byte("hi"), 0o644)
	if got := c.ReadmePath(filepath.Join(cfgdir, "config.json"), "/no/such/bin"); got != filepath.Join(cfgdir, "README.md") {
		t.Errorf("config-dir resolution = %q", got)
	}
	// Nothing found -> empty.
	if got := c.ReadmePath("/no/such/config.json", "/no/such/bin"); got != "" {
		t.Errorf("expected empty when missing, got %q", got)
	}
}

func TestLicensePath(t *testing.T) {
	dir := t.TempDir()
	// Explicit override wins.
	c := &Config{LicenseFile: "/opt/x/LICENSE"}
	if got := c.LicensePath("/etc/gravinet/config.json", "/usr/local/bin"); got != "/opt/x/LICENSE" {
		t.Errorf("override = %q", got)
	}
	// Unix install: …/bin -> …/share/doc/gravinet/LICENSE.
	c = &Config{}
	bindir := filepath.Join(dir, "bin")
	docdir := filepath.Join(dir, "share", "doc", "gravinet")
	os.MkdirAll(docdir, 0o755)
	os.WriteFile(filepath.Join(docdir, "LICENSE"), []byte("GPL"), 0o644)
	if got := c.LicensePath("/nonexistent/config.json", bindir); got != filepath.Join(docdir, "LICENSE") {
		t.Errorf("exe-relative resolution = %q", got)
	}
	// Windows: LICENSE beside the .exe (isolated subtree).
	winbin := filepath.Join(dir, "win", "app")
	os.MkdirAll(winbin, 0o755)
	os.WriteFile(filepath.Join(winbin, "LICENSE"), []byte("GPL"), 0o644)
	if got := c.LicensePath("/nonexistent/config.json", winbin); got != filepath.Join(winbin, "LICENSE") {
		t.Errorf("beside-the-binary resolution = %q", got)
	}
	// Nothing found -> empty.
	if got := c.LicensePath("/no/such/config.json", "/no/such/bin"); got != "" {
		t.Errorf("expected empty when missing, got %q", got)
	}
}

func TestUnderlayMTUValue(t *testing.T) {
	cases := map[int]int{0: 1280, 1280: 1280, 1500: 1500, 9216: 9216, 100: 590, 99999: 9216}
	for in, want := range cases {
		c := &Config{UnderlayMTU: in}
		if got := c.UnderlayMTUValue(); got != want {
			t.Errorf("UnderlayMTU %d -> %d, want %d", in, got, want)
		}
	}
}

func TestUnderlayMTUMaxValue(t *testing.T) {
	tru, fls := true, false
	cases := []struct {
		mtu, max int
		disc     *bool
		want     int
	}{
		{0, 0, nil, 9000}, // defaults: floor 1280, ceil 9000
		{1280, 9000, nil, 9000},
		{1280, 99999, nil, 9216}, // clamped to datagram ceiling
		{1500, 1200, nil, 1500},  // ceil below floor -> floor
		{1280, 9000, &fls, 1280}, // discovery off -> collapses to floor
		{1280, 4000, &tru, 4000},
	}
	for _, c := range cases {
		cfg := &Config{UnderlayMTU: c.mtu, UnderlayMTUMax: c.max, PMTUDiscovery: c.disc}
		if got := cfg.UnderlayMTUMaxValue(); got != c.want {
			t.Errorf("mtu=%d max=%d disc=%v -> %d, want %d", c.mtu, c.max, c.disc, got, c.want)
		}
	}
}

func TestPMTUDiscoveryEnabled(t *testing.T) {
	tru, fls := true, false
	if !(&Config{}).PMTUDiscoveryEnabled() {
		t.Error("default should be enabled")
	}
	if !(&Config{PMTUDiscovery: &tru}).PMTUDiscoveryEnabled() {
		t.Error("explicit true should be enabled")
	}
	if (&Config{PMTUDiscovery: &fls}).PMTUDiscoveryEnabled() {
		t.Error("explicit false should be disabled")
	}
}

// TestSaveToAtomicAndPerms verifies SaveTo writes 0600, leaves no temp file
// behind, and round-trips. It also confirms no stale ".tmp" of the old fixed
// name is created.
func TestSaveToAtomicAndPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config perm = %o, want 0600", perm)
	}
	// No leftover temp files (neither the old fixed name nor a CreateTemp one).
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != "config.json" {
			t.Fatalf("unexpected leftover file after SaveTo: %q", e.Name())
		}
	}
	// Round-trip: it should re-load.
	if _, err := Load(path); err != nil {
		t.Fatalf("reload after SaveTo: %v", err)
	}
}

// TestSaveToConcurrent hammers SaveTo from multiple goroutines; with a fixed
// temp name this races and can leave a corrupt file or stray temp, with a
// unique temp name it must always end clean and loadable.
func TestSaveToConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := Default()
			if err := c.SaveTo(path); err != nil {
				t.Errorf("concurrent SaveTo: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, err := Load(path); err != nil {
		t.Fatalf("config unloadable after concurrent saves: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != "config.json" {
			t.Fatalf("leftover temp after concurrent saves: %q", e.Name())
		}
	}
}
