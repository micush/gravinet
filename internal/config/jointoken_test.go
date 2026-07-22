package config

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func memberConfig(t *testing.T) *Config {
	t.Helper()
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "00000000feedface", Name: "lan", Enabled: true, Subnet4: "10.42.0.0/16"}}}
	c.Networks[0].Keys[0] = KeySlot{Key: mustKey(t), Label: "key0", Enabled: true}
	c.Networks[0].Seeds = SeedList{{Address: "203.0.113.5:65432"}}
	return c
}

func mustKey(t *testing.T) string {
	t.Helper()
	// 32 zero bytes base64 — DecodeKey only checks length/encoding.
	return "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
}

func TestJoinTokenRoundTrip(t *testing.T) {
	src := memberConfig(t)
	tok, err := src.NetworkToken("lan", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !IsJoinToken(tok) {
		t.Fatalf("token not recognized: %q", tok)
	}

	// A blank node consumes it.
	dst := &Config{PrimaryPort: 65432, EnableIPv4: true}
	id, name, err := dst.NetworkJoinToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if id != "00000000feedface" || name != "lan" {
		t.Fatalf("joined id=%q name=%q", id, name)
	}
	n := dst.FindNetwork("00000000feedface")
	if n == nil || !n.Enabled {
		t.Fatal("network not added/enabled")
	}
	if n.Subnet4 != "10.42.0.0/16" {
		t.Fatalf("subnet not carried: %q", n.Subnet4)
	}
	if n.Keys[0].Key != src.Networks[0].Keys[0].Key || !n.Keys[0].Enabled {
		t.Fatal("key not imported")
	}
	if !containsSeedAddr(n.Seeds, "203.0.113.5:65432") {
		t.Fatalf("seed not carried: %v", n.Seeds)
	}
	// Re-applying the same token is idempotent (no duplicate key slots).
	if _, _, err := dst.NetworkJoinToken(tok); err != nil {
		t.Fatal(err)
	}
	used := 0
	for _, k := range dst.FindNetwork("00000000feedface").Keys {
		if k.Key != "" {
			used++
		}
	}
	if used != 1 {
		t.Fatalf("expected 1 key slot after re-join, got %d", used)
	}
}

func TestJoinTokenExtraSeedAndTTL(t *testing.T) {
	src := memberConfig(t)
	tok, err := src.NetworkToken("lan", []string{"198.51.100.9:65432"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dst := &Config{}
	if _, _, err := dst.NetworkJoinToken(tok); err != nil {
		t.Fatal(err)
	}
	if !containsSeedAddr(dst.Networks[0].Seeds, "198.51.100.9:65432") {
		t.Fatalf("explicit seed missing: %v", dst.Networks[0].Seeds)
	}
}

func TestJoinTokenExpired(t *testing.T) {
	// Mint a valid token, then re-encode it with a past expiry.
	src := memberConfig(t)
	jt := joinToken{V: 1, ID: "00000000feedface", Name: "lan", Subnet4: "10.42.0.0/16",
		Keys: []joinTokenKey{{Key: src.Networks[0].Keys[0].Key, Label: "key0"}},
		Exp:  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(jt)
	tok := joinTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	if _, _, err := (&Config{}).NetworkJoinToken(tok); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestJoinTokenMalformed(t *testing.T) {
	for _, bad := range []string{"", "hello", "grav1.@@@notbase64@@@", "grav2.AAAA"} {
		if _, _, err := (&Config{}).NetworkJoinToken(bad); err == nil {
			t.Fatalf("expected rejection for %q", bad)
		}
	}
}

// TestJoinTokenKeepsSeedsAndPeerCacheSeparate proves the fix for peer
// addresses silently turning into seeds on a joining node: a network's own
// configured Seeds still travel to the joiner's Seeds (adopted as if the
// joiner's own operator had typed them in), but entries that only ever lived
// in the issuer's PeerCache (recently-seen peers, never anyone's configured
// seed) land in the joiner's PeerCache instead — not mixed into its Seeds.
func TestJoinTokenKeepsSeedsAndPeerCacheSeparate(t *testing.T) {
	src := memberConfig(t)                                                          // Seeds = [203.0.113.5:65432]
	src.Networks[0].PeerCache = []string{"198.51.100.7:65432", "203.0.113.5:65432"} // one new, one dup of the real seed
	tok, err := src.NetworkToken("lan", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	dst := &Config{}
	if _, _, err := dst.NetworkJoinToken(tok); err != nil {
		t.Fatal(err)
	}
	n := dst.FindNetwork("00000000feedface")
	if n == nil {
		t.Fatal("network not joined")
	}
	if !containsSeedAddr(n.Seeds, "203.0.113.5:65432") {
		t.Fatalf("the issuer's genuine configured seed must land in the joiner's Seeds: %v", n.Seeds)
	}
	if containsSeedAddr(n.Seeds, "198.51.100.7:65432") {
		t.Fatalf("a peer-cache-only address must NOT be written into the joiner's Seeds: %v", n.Seeds)
	}
	if !containsStr(n.PeerCache, "198.51.100.7:65432") {
		t.Fatalf("the peer-cache-only address should still be a bootstrap candidate, just in PeerCache: %v", n.PeerCache)
	}
	// The address that was in both groups on the issuer shouldn't also show
	// up duplicated in the joiner's PeerCache — it's already covered by Seeds.
	if containsStr(n.PeerCache, "203.0.113.5:65432") {
		t.Fatalf("an address already carried as a genuine seed shouldn't be duplicated into PeerCache too: %v", n.PeerCache)
	}
}

func TestNetworkTokenNoKey(t *testing.T) {
	c := &Config{Networks: []Network{{ID: "00000000feedface", Name: "lan", Enabled: true}}}
	if _, err := c.NetworkToken("lan", nil, 0); err == nil {
		t.Fatal("expected error when network has no shareable key")
	}
}

func TestJoinTokenCarriesTCPPort(t *testing.T) {
	src := memberConfig(t)
	src.TCPFallbackPort = 443 // non-default fallback port
	tok, err := src.NetworkToken("lan", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The joiner adopts the port as a seed-dial hint (not as its own listen port).
	dst := &Config{PrimaryPort: 65432, EnableIPv4: true, TCPFallbackPort: 9999}
	if _, _, err := dst.NetworkJoinToken(tok); err != nil {
		t.Fatal(err)
	}
	n := dst.FindNetwork("00000000feedface")
	if n == nil {
		t.Fatal("network not joined")
	}
	if n.SeedTCPPort != 443 {
		t.Fatalf("SeedTCPPort = %d, want 443", n.SeedTCPPort)
	}
	// The joiner's own fallback port is untouched (heterogeneous ports allowed).
	if dst.TCPFallbackPort != 9999 {
		t.Fatalf("joiner's own TCP port changed to %d", dst.TCPFallbackPort)
	}
}

func TestJoinTokenOmitsTCPPortWhenDisabled(t *testing.T) {
	src := memberConfig(t)
	src.DisableTCPFallback = true
	tok, err := src.NetworkToken("lan", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	dst := &Config{PrimaryPort: 65432, EnableIPv4: true}
	if _, _, err := dst.NetworkJoinToken(tok); err != nil {
		t.Fatal(err)
	}
	if n := dst.FindNetwork("00000000feedface"); n == nil || n.SeedTCPPort != 0 {
		t.Fatalf("expected no seed hint when issuer disabled fallback, got %v", n)
	}
}
