//go:build linux && (amd64 || arm64)

package tun

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// TestConcurrentCoalesceAndDirectWriteRace is the more precise reproduction:
// one goroutine playing the flusher (CoalesceWrite/FlushCoalesced, the
// normal path) while others play deliverInner's ring-full fallback (plain
// Write) — the exact mixed call pattern production hits, not just plain
// Write against itself.
func TestConcurrentCoalesceAndDirectWriteRace(t *testing.T) {
	d, err := New("gsoractest1", 1500)
	if err != nil {
		t.Skipf("cannot open /dev/net/tun in this environment: %v", err)
	}
	defer d.Close()

	tcpPkt := func(seed byte) []byte {
		pkt := make([]byte, gsoHdrLen+64)
		pkt[0] = 0x45
		binary.BigEndian.PutUint16(pkt[ipTotalLenOff:], uint16(len(pkt)))
		pkt[ipProtoOff] = 6
		pkt[ipSrcOff], pkt[ipSrcOff+1], pkt[ipSrcOff+2], pkt[ipSrcOff+3] = 10, 0, 0, 1
		pkt[ipDstOff], pkt[ipDstOff+1], pkt[ipDstOff+2], pkt[ipDstOff+3] = 10, 0, 0, 2
		tcp := pkt[ipv4HeaderLen:]
		binary.BigEndian.PutUint16(tcp[tcpSrcPortOff:], 1000+uint16(seed))
		binary.BigEndian.PutUint16(tcp[tcpDstPortOff:], 22)
		tcp[12] = 5 << 4
		tcp[tcpFlagsOff] = tcpFlagACK
		for i := range tcp[20:] {
			tcp[20+i] = seed
		}
		binary.BigEndian.PutUint16(pkt[ipChecksumOff:], ipv4Checksum(pkt[:ipv4HeaderLen]))
		binary.BigEndian.PutUint16(tcp[tcpChecksumOff:], tcpChecksum(pkt[:ipv4HeaderLen], tcp))
		return pkt
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// The "flusher": repeatedly offers packets to the coalescer and flushes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := byte(0)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := d.CoalesceWrite(tcpPkt(i)); err != nil {
				return
			}
			if err := d.FlushCoalesced(); err != nil {
				return
			}
			i++
		}
	}()

	// Several "fallback" writers: plain Write, exactly deliverInner's
	// ring-full path.
	const fallbackWriters = 8
	wg.Add(fallbackWriters)
	for g := 0; g < fallbackWriters; g++ {
		go func(id byte) {
			defer wg.Done()
			pkt := make([]byte, 64)
			for i := range pkt {
				pkt[i] = id
			}
			for i := 0; i < 500; i++ {
				if _, err := d.Write(pkt); err != nil {
					return
				}
			}
		}(byte(g))
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// package's write-side ring falls back to: multiple goroutines calling
// Write concurrently on the same Device, exactly what deliverInner's
// ring-full fallback does alongside the flusher goroutine's own writes (see
// internal/mesh/engine.go's ps.net.dev().Write(ip) fallback and
// tungso.go's drainDirect/CoalesceWrite). Requires a real /dev/net/tun
// (root + CAP_NET_ADMIN); skips if unavailable.
func TestConcurrentWriteRace(t *testing.T) {
	d, err := New("gsoractest0", 1500)
	if err != nil {
		t.Skipf("cannot open /dev/net/tun in this environment: %v", err)
	}
	defer d.Close()

	const goroutines = 8
	const writesEach = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			pkt := make([]byte, 64)
			for i := range pkt {
				pkt[i] = byte(id) // each goroutine writes a distinctive, uniform payload
			}
			for i := 0; i < writesEach; i++ {
				if _, err := d.Write(pkt); err != nil {
					return // device closed concurrently elsewhere is fine; not what this checks
				}
			}
		}(g)
	}
	wg.Wait()
}
