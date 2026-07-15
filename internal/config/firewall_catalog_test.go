package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFirewallCatalogRoundTrip(t *testing.T) {
	fw := Firewall{
		Enabled: true,
		Rules: []FirewallRule{
			{Action: "deny", Dst: "webservers", Services: []string{"DNS"}, Log: true, Notes: "n"},
			{Action: "deny", Src: "trusted", SrcNegate: true, Dst: "webservers", DstNegate: false,
				Services: []string{"DNS"}, ServicesNegate: true, Notes: "negated"},
		},
		Objects: []FirewallObject{
			{Name: "webservers", Kind: "host", Addresses: []string{"10.0.0.10", "10.0.0.11"}},
			{Name: "pool", Kind: "range", Addresses: []string{"10.0.0.5-10.0.0.20"}},
			{Name: "sites", Kind: "group", Members: []string{"webservers"}},
			{Name: "db", Kind: "fqdn", Addresses: []string{"db.example.com"}},
		},
		Services: []FirewallService{
			{Name: "DNS", Ports: []FirewallServicePort{{Proto: "udp", PortMin: 53}, {Proto: "tcp", PortMin: 53}}},
		},
	}
	b, err := json.Marshal(fw)
	if err != nil {
		t.Fatal(err)
	}
	var got Firewall
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Objects) != 4 || len(got.Services) != 1 {
		t.Fatalf("catalog lost in round trip: %d objects, %d services", len(got.Objects), len(got.Services))
	}
	if !got.Rules[0].Log || len(got.Rules[0].Services) != 1 || got.Rules[0].Dst != "webservers" {
		t.Fatalf("rule catalog references lost in round trip: %+v", got.Rules[0])
	}
	neg := got.Rules[1]
	if !neg.SrcNegate || neg.DstNegate || !neg.ServicesNegate {
		t.Fatalf("negate flags lost or corrupted in round trip: %+v", neg)
	}
	// A rule's negate fields all default false and are omitempty — confirm
	// an ordinary (non-negated) rule doesn't grow "_negate" keys in its JSON
	// at all, so old configs and new configs read identically for the
	// common case.
	b0, err := json.Marshal(fw.Rules[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"src_negate", "dst_negate", "services_negate"} {
		if strings.Contains(string(b0), key) {
			t.Errorf("non-negated rule's JSON unexpectedly contains %q: %s", key, b0)
		}
	}
}

func TestValidateFirewallCatalog(t *testing.T) {
	ok := Firewall{
		Objects: []FirewallObject{
			{Name: "a", Kind: "host", Addresses: []string{"1.1.1.1"}},
			{Name: "g", Kind: "group", Members: []string{"a"}},
		},
		Services: []FirewallService{{Name: "s", Ports: []FirewallServicePort{{Proto: "tcp", PortMin: 22}}}},
	}
	if err := validateFirewallCatalog(ok); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}

	bad := []struct {
		name string
		fw   Firewall
		want string
	}{
		{"unknown kind", Firewall{Objects: []FirewallObject{{Name: "x", Kind: "weird", Addresses: []string{"1.1.1.1"}}}}, "unknown kind"},
		{"group missing member", Firewall{Objects: []FirewallObject{{Name: "g", Kind: "group", Members: []string{"nope"}}}}, "unknown member"},
		{"empty object name", Firewall{Objects: []FirewallObject{{Name: "", Kind: "host", Addresses: []string{"1.1.1.1"}}}}, "empty name"},
		{"service no ports", Firewall{Services: []FirewallService{{Name: "s"}}}, "no ports"},
	}
	for _, c := range bad {
		err := validateFirewallCatalog(c.fw)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got err %v, want containing %q", c.name, err, c.want)
		}
	}
}
