package mesh

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"testing"

	"gravinet/internal/crypto"
)

type emsgSender struct{}

func (emsgSender) Send(netip.AddrPort, []byte) error { return syscall.EMSGSIZE }

func TestIsMsgSize(t *testing.T) {
	if !isMsgSize(syscall.EMSGSIZE) {
		t.Error("bare EMSGSIZE not recognized")
	}
	if !isMsgSize(fmt.Errorf("send: %w", syscall.EMSGSIZE)) {
		t.Error("wrapped EMSGSIZE not recognized")
	}
	if isMsgSize(errors.New("connection refused")) {
		t.Error("non-EMSGSIZE error matched")
	}
}

// When a data send fails with EMSGSIZE (DF set, path MTU shrank below our
// estimate), the engine must drop the peer's discovered MTU back to the floor
// and re-discover, so the next packets fragment small enough to get through.
func TestSendResetsPMTUOnEMSGSIZE(t *testing.T) {
	const netID = uint64(0x9001)
	eng := NewEngine(Options{
		NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450,
		Nets: []NetSpec{{ID: netID, Name: "n", Dev: newFakeDev("d"),
			Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	eng.Attach(emsgSender{})
	ns := eng.network(netID)
	if ns == nil {
		t.Fatal("no network")
	}

	// A peer session with a converged-high PMTU.
	shared := bytes.Repeat([]byte{0x11}, 32)
	psk := bytes.Repeat([]byte{0x22}, 32)
	sess, err := crypto.NewSession(crypto.DeriveSessionKeys(shared, psk, []byte("t"), true))
	if err != nil {
		t.Fatal(err)
	}
	ps := &peerSession{nodeID: "p", net: ns, endpoint: netip.MustParseAddrPort("203.0.113.9:65432"), sess: sess, remoteIdx: 1}
	ps.initPMTU(eng.pmtuFloor, eng.pmtuCeil)
	ps.pmtuMu.Lock()
	ps.pmtu.eff, ps.pmtu.phase = 1450, phaseSettled
	ps.pmtuMu.Unlock()
	ps.setEff(1450)
	if ps.effMTU.Load() != 1450 {
		t.Fatalf("setup: effMTU=%d want 1450", ps.effMTU.Load())
	}

	// A large packet takes the fragmented path; the first fragment's send
	// returns EMSGSIZE, which must trigger a reset to the floor.
	eng.sendData(ps, make([]byte, 3000))

	if got := ps.effMTU.Load(); got != int32(eng.pmtuFloor) {
		t.Fatalf("after EMSGSIZE effMTU=%d, want floor %d", got, eng.pmtuFloor)
	}
}
