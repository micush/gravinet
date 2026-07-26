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
	if n := got / jumboDatagram; n < 1500 {
		t.Fatalf("default socket buffer %d holds only %d jumbo datagrams; want >=1500", got, n)
	}
}

func TestSocketBufferClamps(t *testing.T) {
	const (
		min = 256 << 10
		max = 256 << 20
	)
	cases := []struct {
		in, want int
	}{
		{0, 16 << 20},      // unset -> default
		{1, min},           // absurdly small -> floor
		{min - 1, min},     // just under the floor
		{min, min},         // exactly the floor
		{8 << 20, 8 << 20}, // an ordinary explicit value passes through
		{max, max},         // exactly the ceiling
		{max + 1, max},     // over the ceiling
		{1 << 40, max},     // absurdly large
		{-5, min},          // negative is not treated as "unset"
	}
	for _, c := range cases {
		cfg := Config{SocketBuffer: c.in}
		if got := cfg.SocketBufferValue(); got != c.want {
			t.Fatalf("SocketBuffer=%d resolved to %d, want %d", c.in, got, c.want)
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
