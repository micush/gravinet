package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// The point of the field: syslog, time and resolver travel with the
// configuration, so restoring a backup onto replacement hardware brings them
// back instead of leaving the node with the wrong clock, the wrong resolvers
// and no log forwarding.
func TestHostSettingsRoundTrip(t *testing.T) {
	c := &Config{UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	c.HostSettings = &HostSettings{
		Syslog:   &HostSyslog{Targets: []HostSyslogTarget{{Host: "10.1.1.9", Port: 514, Proto: "udp"}}},
		Time:     &HostTime{Timezone: "Europe/London", NTPEnabled: true, NTPServers: []string{"10.1.1.1"}},
		Resolver: &HostResolver{Hostname: "grav3", DNSServers: []string{"10.1.1.1"}, SearchDomain: "lan.example"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("config with host settings should validate: %v", err)
	}

	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	h := back.HostSettings
	if h == nil {
		t.Fatal("host settings did not survive the round trip at all")
	}
	if h.Syslog == nil || len(h.Syslog.Targets) != 1 || h.Syslog.Targets[0].Host != "10.1.1.9" {
		t.Errorf("syslog did not survive: %+v", h.Syslog)
	}
	if h.Time == nil || h.Time.Timezone != "Europe/London" || !h.Time.NTPEnabled {
		t.Errorf("time did not survive: %+v", h.Time)
	}
	if h.Resolver == nil || h.Resolver.Hostname != "grav3" || h.Resolver.SearchDomain != "lan.example" {
		t.Errorf("resolver did not survive: %+v", h.Resolver)
	}

	// A config nobody has touched must serialize exactly as before, so
	// existing files round-trip unchanged.
	plain := &Config{UDPPorts: []int{51820}, EnableIPv4: true}
	pb, _ := json.Marshal(plain)
	if strings.Contains(string(pb), "host_settings") {
		t.Error("host_settings should be omitted when nothing is managed")
	}
}

// Each group is separately opt-in, and nil means "gravinet does not manage
// this". A reconciler that read an absent group as "set it to empty" would
// wipe a host's DNS on the first reload after an upgrade, which is why the
// pointer and not the slice carries that meaning.
func TestHostSettingsGroupsAreIndependentlyOptional(t *testing.T) {
	var h HostSettings
	if h.Syslog != nil || h.Time != nil || h.Resolver != nil {
		t.Fatal("the zero value must manage nothing")
	}
	if err := h.Validate(); err != nil {
		t.Errorf("an unmanaged host should validate: %v", err)
	}

	// An empty syslog target list is a real setting — "forward nowhere" —
	// and must be distinguishable from not managing syslog at all.
	managed := HostSettings{Syslog: &HostSyslog{Targets: nil}}
	b, _ := json.Marshal(managed)
	var back HostSettings
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Syslog == nil {
		t.Error("an empty target list must still round-trip as managed")
	}
}

func TestHostSettingsValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   HostSettings
		ok   bool
	}{
		{"ok", HostSettings{Syslog: &HostSyslog{Targets: []HostSyslogTarget{{Host: "h", Port: 514, Proto: "tcp"}}}}, true},
		{"no host", HostSettings{Syslog: &HostSyslog{Targets: []HostSyslogTarget{{Port: 514}}}}, false},
		{"bad port", HostSettings{Syslog: &HostSyslog{Targets: []HostSyslogTarget{{Host: "h", Port: 99999}}}}, false},
		{"bad proto", HostSettings{Syslog: &HostSyslog{Targets: []HostSyslogTarget{{Host: "h", Proto: "sctp"}}}}, false},
		{"blank proto is fine", HostSettings{Syslog: &HostSyslog{Targets: []HostSyslogTarget{{Host: "h"}}}}, true},
		{"bad dns", HostSettings{Resolver: &HostResolver{DNSServers: []string{"not-an-ip"}}}, false},
		{"good dns", HostSettings{Resolver: &HostResolver{DNSServers: []string{"10.1.1.1", "fd00::1"}}}, true},
	} {
		err := tc.in.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

// Console accounts travel by name and expiry only. The whole point of
// option 1: a configuration backup is downloaded through a browser and mailed
// around, so no credential of any kind may appear in it — a restored node
// comes back with its accounts present but locked.
func TestHostUsersCarryNoCredentials(t *testing.T) {
	h := HostSettings{Users: []HostUser{
		{Name: "alice"},
		{Name: "bob", ExpiresUnix: 1786408524},
	}}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	enc := string(b)

	// The fields that must be there.
	for _, want := range []string{`"name":"alice"`, `"name":"bob"`, `"expires_unix":1786408524`} {
		if !strings.Contains(enc, want) {
			t.Errorf("missing %q in %s", want, enc)
		}
	}
	// And the ones that must never be. Checked against the encoded form
	// rather than the struct, since the struct is the thing that could grow
	// a field later and this is what would catch it.
	for _, forbidden := range []string{"password", "hash", "secret", "shadow", "crypt"} {
		if strings.Contains(strings.ToLower(enc), forbidden) {
			t.Errorf("console account encoding contains %q: %s", forbidden, enc)
		}
	}

	// An account with no expiry omits the field rather than storing 0, so a
	// permanent account and one expiring at the epoch stay distinguishable.
	if strings.Contains(enc, `"name":"alice","expires_unix"`) {
		t.Errorf("an account with no expiry should omit the field: %s", enc)
	}

	var back HostSettings
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Users) != 2 || back.Users[1].ExpiresUnix != 1786408524 {
		t.Errorf("roster did not round-trip: %+v", back.Users)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("a valid roster should validate: %v", err)
	}
	if (HostSettings{Users: []HostUser{{Name: "  "}}}).Validate() == nil {
		t.Error("an account with no name should be refused")
	}
}
