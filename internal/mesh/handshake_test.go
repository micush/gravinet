package mesh

import (
	"bytes"
	"net/netip"
	"testing"
)

func TestHSPayloadRoundTrip(t *testing.T) {
	in := hsPayload{
		Index:     0xCAFEBABE,
		TimeNano:  1_700_000_000_123456789,
		Ephemeral: bytes.Repeat([]byte{0xAB}, ephemeralLen),
		Overlay4:  netip.MustParseAddr("10.42.1.7"),
		Overlay6:  netip.MustParseAddr("fd00:42::7"),
		NodeID:    "node-abc",
		Hostname:  "laptop.local",
	}
	out, err := decodeHSPayload(encodeHSPayload(in))
	if err != nil {
		t.Fatal(err)
	}
	if out.Index != in.Index || out.TimeNano != in.TimeNano ||
		!bytes.Equal(out.Ephemeral, in.Ephemeral) ||
		out.Overlay4 != in.Overlay4 || out.Overlay6 != in.Overlay6 ||
		out.NodeID != in.NodeID || out.Hostname != in.Hostname {
		t.Fatalf("payload mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestHSPayloadV4Only(t *testing.T) {
	in := hsPayload{
		Index:     1,
		Ephemeral: make([]byte, ephemeralLen),
		Overlay4:  netip.MustParseAddr("192.168.0.5"),
		NodeID:    "n",
		Hostname:  "h",
	}
	out, err := decodeHSPayload(encodeHSPayload(in))
	if err != nil {
		t.Fatal(err)
	}
	if out.Overlay4 != in.Overlay4 || out.Overlay6.IsValid() {
		t.Fatalf("v4-only payload wrong: %+v", out)
	}
}

func TestHSPayloadTruncated(t *testing.T) {
	full := encodeHSPayload(hsPayload{Ephemeral: make([]byte, ephemeralLen), NodeID: "x", Hostname: "y"})
	if _, err := decodeHSPayload(full[:5]); err == nil {
		t.Fatal("truncated payload should error")
	}
}

func TestHSPayloadCarriesName(t *testing.T) {
	in := hsPayload{
		Index: 7, TimeNano: 123, Ephemeral: make([]byte, ephemeralLen),
		NodeID: "n1", Hostname: "h1",
		Subnet4: netip.MustParsePrefix("10.9.0.0/16"),
		Name:    "corpnet",
	}
	out, err := decodeHSPayload(encodeHSPayload(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Name != "corpnet" {
		t.Fatalf("name not carried: got %q", out.Name)
	}
	if out.Subnet4 != in.Subnet4 {
		t.Fatalf("subnet mangled: %v", out.Subnet4)
	}
}

func TestAbsorbIdentity(t *testing.T) {
	ns := &netState{} // empty: no name, no subnet
	adv := hsPayload{
		Subnet4: netip.MustParsePrefix("10.9.0.0/16"),
		Name:    "corpnet",
	}
	if !ns.absorbIdentity(adv) {
		t.Fatalf("expected to learn name+subnet")
	}
	if ns.name != "corpnet" || ns.subnet4 != adv.Subnet4 {
		t.Fatalf("not learned: name=%q subnet4=%v", ns.name, ns.subnet4)
	}
	// A second advert with different values must NOT override what we have.
	other := hsPayload{Subnet4: netip.MustParsePrefix("10.5.0.0/16"), Name: "other"}
	if ns.absorbIdentity(other) {
		t.Fatalf("should not change once set")
	}
	if ns.name != "corpnet" || ns.subnet4 != adv.Subnet4 {
		t.Fatalf("values were overridden: name=%q subnet4=%v", ns.name, ns.subnet4)
	}
}
