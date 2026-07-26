package config

import "testing"

// The default has to be large enough that it doesn't recreate the overflow it
// exists to prevent. At jumbo sizes a socket holds buffer/~8900 datagrams, and
// the old 4 MiB was ~470 — roughly 8 ms at 4 Gbps, which measurably overflowed
// on a live link (UdpRcvbufErrors). Assert the datagram capacity rather than
// the byte count, since that is the property that actually matters.
func TestSocketBufferDefaultHoldsEnoughJumboDatagrams(t *testing.T) {
	var c Config
	got := c.SocketBufferValue()
	const jumboDatagram = 8900
	if got != SocketBufferDefaultBytes {
		t.Fatalf("default socket buffer = %d, want %d", got, SocketBufferDefaultBytes)
	}
	if n := got / jumboDatagram; n < 1500 {
		t.Fatalf("default socket buffer %d holds only %d jumbo datagrams; want >=1500", got, n)
	}
}

func TestSocketBufferClamps(t *testing.T) {
	cases := []struct {
		in, want int
		why      string
	}{
		{0, SocketBufferDefaultBytes, "unset uses the default"},
		{1, 1 << 20, "1 is one megabyte, not one byte"},
		{16, 16 << 20, "the default, written explicitly"},
		{32, 32 << 20, "what an operator types into the Settings card"},
		{256, SocketBufferMaxBytes, "the largest meaningful megabyte value"},
		{512, SocketBufferMaxBytes, "megabytes over the ceiling clamp down"},
		{SocketBufferMBThreshold, SocketBufferMaxBytes, "the threshold itself is megabytes"},
		{SocketBufferMBThreshold + 1, SocketBufferMinBytes, "just past it is bytes, and below the floor"},
		{SocketBufferMinBytes, SocketBufferMinBytes, "exactly the byte floor"},
		{8 << 20, 8 << 20, "an explicit byte value passes through"},
		{SocketBufferMaxBytes, SocketBufferMaxBytes, "exactly the byte ceiling"},
		{SocketBufferMaxBytes + 1, SocketBufferMaxBytes, "over the byte ceiling"},
		{1 << 40, SocketBufferMaxBytes, "absurdly large"},
		{-5, SocketBufferMinBytes, "negative is not treated as unset"},
	}
	for _, c := range cases {
		cfg := Config{SocketBuffer: c.in}
		if got := cfg.SocketBufferValue(); got != c.want {
			t.Fatalf("socket_buffer=%d resolved to %d, want %d (%s)", c.in, got, c.want, c.why)
		}
	}
}

// Both units have to mean the same thing, or the config file and the Settings
// card would disagree about the same setting — the whole reason the dual
// interpretation exists. The ranges cannot overlap: the largest meaningful MB
// value (256) is far below the smallest meaningful byte value (262144).
func TestSocketBufferMegabytesAndBytesAgree(t *testing.T) {
	for _, mb := range []int{1, 4, 16, 32, 64, 128, 256} {
		cMB := Config{SocketBuffer: mb}
		cBytes := Config{SocketBuffer: mb << 20}
		asMB, asBytes := cMB.SocketBufferValue(), cBytes.SocketBufferValue()
		if asMB != asBytes {
			t.Fatalf("%d MB resolved to %d but %d bytes resolved to %d", mb, asMB, mb<<20, asBytes)
		}
	}
	if SocketBufferMBThreshold >= SocketBufferMinBytes {
		t.Fatalf("the MB threshold (%d) overlaps the byte floor (%d) — the units are no longer distinguishable",
			SocketBufferMBThreshold, SocketBufferMinBytes)
	}
}

// SocketBufferMB is what the Settings card displays, so it has to round-trip:
// whatever it shows, typed back in, must resolve to the same buffer.
func TestSocketBufferMBRoundTrips(t *testing.T) {
	for _, in := range []int{0, 1, 16, 32, 256, 512, 8 << 20, SocketBufferMaxBytes} {
		cfg := Config{SocketBuffer: in}
		shown := cfg.SocketBufferMB()
		back := Config{SocketBuffer: shown}
		if back.SocketBufferValue() != cfg.SocketBufferValue() {
			t.Fatalf("socket_buffer=%d displays as %d MB, which resolves to %d instead of %d",
				in, shown, back.SocketBufferValue(), cfg.SocketBufferValue())
		}
	}
}

// A negative value must not fall through to the default — that would make a
// typo silently look like it worked while quietly resolving to something the
// operator did not ask for.
func TestSocketBufferNegativeIsFlooredNotDefaulted(t *testing.T) {
	cfg := Config{SocketBuffer: -1}
	var zero Config
	if got, def := cfg.SocketBufferValue(), zero.SocketBufferValue(); got == def {
		t.Fatalf("negative socket_buffer resolved to the default %d instead of the floor", got)
	}
}
