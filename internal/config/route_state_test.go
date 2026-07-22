package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRouteSetEnabled(t *testing.T) {
	c := &Config{EnableIPv4: true, PrimaryPort: 51820, Networks: []Network{baseNet()}}
	if err := c.RouteAdd("n1", "192.168.9.0/24", 5); err != nil {
		t.Fatal(err)
	}
	// New advertised routes are enabled by default.
	if !c.Networks[0].Routes[0].Enabled {
		t.Fatal("new route should be enabled")
	}
	// Disable it — the route stays in config, only the flag flips.
	if err := c.RouteSetEnabled("n1", "192.168.9.0/24", false); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].Routes) != 1 || c.Networks[0].Routes[0].Enabled {
		t.Fatalf("route should be present and disabled: %+v", c.Networks[0].Routes)
	}
	// Metric is preserved across the toggle.
	if c.Networks[0].Routes[0].Metric != 5 {
		t.Fatalf("metric should be preserved: %+v", c.Networks[0].Routes[0])
	}
	// Re-enable.
	if err := c.RouteSetEnabled("n1", "192.168.9.0/24", true); err != nil {
		t.Fatal(err)
	}
	if !c.Networks[0].Routes[0].Enabled {
		t.Fatal("route should be enabled again")
	}
	// Unknown route errors.
	if err := c.RouteSetEnabled("n1", "172.16.0.0/12", true); err == nil {
		t.Error("toggling a missing route should error")
	}
}

func TestRouteRejectSetEnabled(t *testing.T) {
	c := &Config{EnableIPv4: true, PrimaryPort: 51820, Networks: []Network{baseNet()}}
	c.Networks[0].RouteRej = []RejectRoute{}
	if err := c.RouteReject("n1", "10.0.0.0/8", false); err != nil {
		t.Fatal(err)
	}
	// New reject entries are active by default (zero value of Disabled).
	if c.Networks[0].RouteRej[0].Disabled {
		t.Fatal("new reject entry should be enabled")
	}
	// Disable it — kept in config, flag flipped.
	if err := c.RouteRejectSetEnabled("n1", "10.0.0.0/8", false); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].RouteRej) != 1 || !c.Networks[0].RouteRej[0].Disabled {
		t.Fatalf("reject entry should be present and disabled: %+v", c.Networks[0].RouteRej)
	}
	// Re-enable.
	if err := c.RouteRejectSetEnabled("n1", "10.0.0.0/8", true); err != nil {
		t.Fatal(err)
	}
	if c.Networks[0].RouteRej[0].Disabled {
		t.Fatal("reject entry should be enabled again")
	}
	// Unknown entry errors.
	if err := c.RouteRejectSetEnabled("n1", "192.0.2.0/24", false); err == nil {
		t.Error("toggling a missing reject entry should error")
	}
}

// TestRejectRouteDisabledJSON pins the serialisation around the new Disabled
// field: an enabled non-inclusive entry stays a bare string (legacy form), but
// any disabled entry forces the object form carrying disabled:true, and reads
// back round-trip.
func TestRejectRouteDisabledJSON(t *testing.T) {
	in := []RejectRoute{
		{CIDR: "0.0.0.0/0"},                                      // enabled, non-inclusive -> bare string
		{CIDR: "10.0.0.0/8", Disabled: true},                     // disabled -> object
		{CIDR: "172.16.0.0/12", Inclusive: true, Disabled: true}, // both -> object
	}
	out, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `["0.0.0.0/0",{"cidr":"10.0.0.0/8","disabled":true},{"cidr":"172.16.0.0/12","inclusive":true,"disabled":true}]`
	if string(out) != want {
		t.Fatalf("marshal = %s\nwant     = %s", out, want)
	}
	var back []RejectRoute
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", back, in)
	}
	// A legacy bare string still reads as enabled (Disabled false).
	var legacy []RejectRoute
	if err := json.Unmarshal([]byte(`["0.0.0.0/0"]`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy[0].Disabled {
		t.Fatal("legacy bare-string reject must read as enabled")
	}
}
