package mesh

import (
	"net/netip"
	"sync"
	"testing"

	"gravinet/internal/crypto"
	"gravinet/internal/protocol"
	"gravinet/internal/transport"
)

// FuzzInnerDecoders feeds arbitrary bytes to every inner-control / packet parser
// that processes attacker-influenced data. None may panic.
func FuzzInnerDecoders(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add(make([]byte, 64))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodeBanAdd(b)
		_, _, _ = decodeBanDel(b)
		_, _, _, _ = decodeRouteAdd(b)
		_, _, _, _ = decodeRelay(b)
		_, _ = decodePeerList(b)
		_, _ = decodeAddr(b)
		_, _, _, _, _ = parseL4(b)
		_, _, _ = parseAddrs(b)
		_, _, _, _, _, _, _ = ipv4Fields(b)
		_, _ = decodeHSPayload(b)
	})
}

var (
	fuzzEngine *Engine
	fuzzOnce   sync.Once
)

// FuzzOnPacket drives arbitrary datagrams through the real network entry point.
// Hostile input must be rejected without panicking or forming peers.
func FuzzOnPacket(f *testing.F) {
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})

	from := netip.MustParseAddrPort("198.51.100.7:51820")

	// Seeds: a data header, an HS-init header, and short junk.
	d := make([]byte, protocol.DataHeaderLen+16)
	protocol.EncodeData(d, protocol.DataHeader{RecvSession: 1, Counter: 1})
	f.Add(d)
	h := make([]byte, protocol.HSInitHeaderLen+48)
	protocol.EncodeHSInit(h, protocol.HSInitHeader{Network: 0xBEEF})
	f.Add(h)
	f.Add([]byte{1, byte(protocol.TypeData)})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		fuzzOnce.Do(func() {
			dev := newFakeDev("F")
			fuzzEngine = NewEngine(Options{
				NodeID: "F", Hostname: "F",
				Nets: []NetSpec{{ID: 0xBEEF, Name: "n", Keys: ks, Dev: dev, Self4: netip.MustParseAddr("10.9.0.1")}},
			})
			if tr, err := openTestTransport(fuzzEngine); err == nil {
				fuzzEngine.Attach(tr)
			}
		})
		fuzzEngine.OnPacket(b, from, transport.V4)
		if fuzzEngine.PeerCount(0xBEEF) != 0 {
			t.Fatalf("garbage input must not form a peer")
		}
	})
}
