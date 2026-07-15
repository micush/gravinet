package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func baseNet() Network {
	return Network{ID: "n1", Subnet4: "10.0.0.0/24", MTU: 9216}
}

func validate(t *testing.T, n Network) Network {
	t.Helper()
	c := &Config{EnableIPv4: true, PrimaryPort: 51820, Networks: []Network{n}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return c.Networks[0]
}

func TestRouteRejectDefaults(t *testing.T) {
	// Fresh creation includes the default-route reject explicitly, for both
	// address families — an unguarded ::/0 hits the exact same underlay loop
	// 0.0.0.0/0 does; nothing about the hazard is v4-specific.
	want := []RejectRoute{{CIDR: "0.0.0.0/0"}, {CIDR: "::/0"}}
	if got := NewNetworkDefaults().RouteRej; !reflect.DeepEqual(got, want) {
		t.Fatalf("NewNetworkDefaults RouteRej = %v, want %v", got, want)
	}
	// Legacy/unset (nil) -> default injected.
	n := baseNet()
	n.RouteRej = nil
	if got := validate(t, n).RouteRej; !reflect.DeepEqual(got, want) {
		t.Fatalf("unset RouteRej validated to %v, want %v", got, want)
	}
	// Explicit empty (operator cleared it) -> respected, not re-injected.
	n = baseNet()
	n.RouteRej = []RejectRoute{}
	if got := validate(t, n).RouteRej; len(got) != 0 {
		t.Fatalf("explicit empty RouteRej validated to %v, want []", got)
	}
	// Custom non-empty list -> left alone (no silent default-route merge).
	n = baseNet()
	n.RouteRej = []RejectRoute{{CIDR: "172.16.0.0/12"}}
	if got := validate(t, n).RouteRej; !reflect.DeepEqual(got, []RejectRoute{{CIDR: "172.16.0.0/12"}}) {
		t.Fatalf("custom RouteRej validated to %v, want [172.16.0.0/12]", got)
	}
}

func TestRouteRejectRemovalSticks(t *testing.T) {
	// Start from the materialized default, remove both entries, re-validate:
	// must stay gone (opting a node fully into a full-tunnel default in
	// either family, e.g. an IPv6-only exit node, must actually stick).
	c := &Config{EnableIPv4: true, PrimaryPort: 51820, Networks: []Network{baseNet()}}
	c.Networks[0].RouteRej = nil
	if err := c.Validate(); err != nil { // injects [0.0.0.0/0, ::/0]
		t.Fatal(err)
	}
	if err := c.RouteDelete("n1", "0.0.0.0/0"); err != nil {
		t.Fatal(err)
	}
	if err := c.RouteDelete("n1", "::/0"); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil { // must NOT re-inject
		t.Fatal(err)
	}
	if got := c.Networks[0].RouteRej; len(got) != 0 {
		t.Fatalf("after removal RouteRej = %v, want empty (removal must stick)", got)
	}
}

func TestRejectRouteJSONBackCompat(t *testing.T) {
	// Legacy bare-string form reads as a non-inclusive entry.
	var legacy []RejectRoute
	if err := json.Unmarshal([]byte(`["0.0.0.0/0","10.0.0.0/8"]`), &legacy); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy, []RejectRoute{{CIDR: "0.0.0.0/0"}, {CIDR: "10.0.0.0/8"}}) {
		t.Fatalf("legacy parse = %+v", legacy)
	}
	// Object form carries inclusive.
	var obj []RejectRoute
	if err := json.Unmarshal([]byte(`[{"cidr":"10.129.16.0/21","inclusive":true}]`), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj) != 1 || !obj[0].Inclusive || obj[0].CIDR != "10.129.16.0/21" {
		t.Fatalf("object parse = %+v", obj)
	}
	// Marshal: non-inclusive -> bare string (preserves the historical form);
	// inclusive -> object.
	out, _ := json.Marshal([]RejectRoute{{CIDR: "0.0.0.0/0"}, {CIDR: "10.129.16.0/21", Inclusive: true}})
	want := `["0.0.0.0/0",{"cidr":"10.129.16.0/21","inclusive":true}]`
	if string(out) != want {
		t.Fatalf("marshal = %s, want %s", out, want)
	}
}

func TestRouteRejectOpSetsInclusive(t *testing.T) {
	c := &Config{EnableIPv4: true, PrimaryPort: 51820, Networks: []Network{baseNet()}}
	c.Networks[0].RouteRej = []RejectRoute{}
	if err := c.RouteReject("n1", "10.129.16.0/21", false); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].RouteRej; len(got) != 1 || got[0].Inclusive {
		t.Fatalf("after exact reject: %+v", got)
	}
	// Re-rejecting the same CIDR with inclusive=true updates in place (no dup).
	if err := c.RouteReject("n1", "10.129.16.0/21", true); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].RouteRej; len(got) != 1 || !got[0].Inclusive {
		t.Fatalf("after inclusive update: %+v", got)
	}
}
