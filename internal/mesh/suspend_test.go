package mesh

import (
	"net/netip"
	"testing"
	"time"
)

func TestSuspendDetection(t *testing.T) {
	if suspended(5*time.Second, 5*time.Second) {
		t.Error("steady 5s tick flagged as suspend")
	}
	if suspended(6*time.Second, 5*time.Second) {
		t.Error("1s of jitter flagged as suspend")
	}
	if suspended(2*time.Second, 5*time.Second) {
		t.Error("backward wall step (negative skew) flagged as suspend")
	}
	if !suspended(time.Hour, 5*time.Second) {
		t.Error("hour-long monotonic freeze not detected")
	}
	if !suspended(90*time.Second, 5*time.Second) {
		t.Error("90s freeze not detected")
	}
}

// TestNotifySuspendResumeFiresOnce proves the suspend/resume hook — the one
// that triggers a full process restart in cmd/gravinet — fires at most once
// per engine, even if notifySuspendResume is called repeatedly (as it would
// be from a mesh with several networks, each independently detecting the
// same underlying sleep/wake on its own maintenance loop). A restart is only
// meaningful to request once; anything after the first call would just be
// racing the process tearing itself down.
func TestNotifySuspendResumeFiresOnce(t *testing.T) {
	eng := NewEngine(Options{NodeID: "self"})
	var calls int
	eng.SetSuspendResumeHook(func() { calls++ })
	eng.notifySuspendResume()
	eng.notifySuspendResume()
	eng.notifySuspendResume()
	if calls != 1 {
		t.Fatalf("hook fired %d times, want exactly 1", calls)
	}
}

// TestNotifySuspendResumeNilHookIsNoop confirms an engine with no hook
// installed (SetSuspendResumeHook never called — e.g. any caller that hasn't
// opted in) tolerates notifySuspendResume being called without panicking.
func TestNotifySuspendResumeNilHookIsNoop(t *testing.T) {
	eng := NewEngine(Options{NodeID: "self"})
	eng.notifySuspendResume() // must not panic
}

// After a resume, onResume must age every session so the same maintenance tick's
// pruneDead tears it down, prompting a fresh handshake.
func TestOnResumeForcesReconnect(t *testing.T) {
	const netID = uint64(0xA10)
	eng := NewEngine(Options{
		NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450,
		Nets: []NetSpec{{ID: netID, Name: "n", Dev: newFakeDev("d"),
			Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	eng.Attach(nopSender{})
	ns := eng.network(netID)

	ps := &peerSession{nodeID: "peer", net: ns, localIdx: 7,
		endpoint: netip.MustParseAddrPort("203.0.113.1:65432")}
	ps.initPMTU(eng.pmtuFloor, eng.pmtuCeil)
	ps.setLastRx(time.Now()) // freshly alive
	eng.mu.Lock()
	eng.sessions[7] = ps
	eng.mu.Unlock()
	ns.mu.Lock()
	ns.byNode["peer"] = ps
	ns.mu.Unlock()

	now := time.Now()
	eng.onResume(ns, now)
	if now.Sub(ps.lastRxTime()) <= eng.peerTimeoutDuration() {
		t.Fatalf("onResume did not age lastRx past peerTimeout: gap=%v", now.Sub(ps.lastRxTime()))
	}
	// The same tick's pruneDead should now reap it.
	eng.pruneDead(ns, now)
	eng.mu.RLock()
	_, stillSession := eng.sessions[7]
	eng.mu.RUnlock()
	ns.mu.RLock()
	_, stillNode := ns.byNode["peer"]
	ns.mu.RUnlock()
	if stillSession || stillNode {
		t.Fatalf("session not reaped after resume (session=%v node=%v)", stillSession, stillNode)
	}
}

// TestNotifyUnderlayChangeFiresOnce mirrors the suspend/resume one-shot: the
// underlay-change hook (which also triggers a full process restart) must fire
// at most once per engine no matter how many times a roam is observed. The
// engine is aged past the startup grace so the guard doesn't mask the check.
func TestNotifyUnderlayChangeFiresOnce(t *testing.T) {
	eng := NewEngine(Options{NodeID: "self"})
	eng.startedAt = time.Now().Add(-2 * underlayRestartGrace) // past the grace window
	var calls int
	eng.SetUnderlayChangeHook(func() { calls++ })
	eng.notifyUnderlayChange()
	eng.notifyUnderlayChange()
	eng.notifyUnderlayChange()
	if calls != 1 {
		t.Fatalf("hook fired %d times, want exactly 1", calls)
	}
}

// TestNotifyUnderlayChangeNilHookIsNoop confirms an engine with no underlay
// hook installed tolerates notifyUnderlayChange without panicking.
func TestNotifyUnderlayChangeNilHookIsNoop(t *testing.T) {
	eng := NewEngine(Options{NodeID: "self"})
	eng.startedAt = time.Now().Add(-2 * underlayRestartGrace)
	eng.notifyUnderlayChange() // must not panic
}

// TestNotifyUnderlayChangeGraceMutesThenFires proves the startup-grace guard
// that keeps a link flapping right after boot from spinning the service: a
// change observed inside the grace window is suppressed WITHOUT consuming the
// one-shot, and a later change past the window still fires exactly once.
func TestNotifyUnderlayChangeGraceMutesThenFires(t *testing.T) {
	eng := NewEngine(Options{NodeID: "self"})
	var calls int
	eng.SetUnderlayChangeHook(func() { calls++ })

	// Fresh engine: startedAt is ~now, so we're inside the grace window.
	eng.notifyUnderlayChange()
	if calls != 0 {
		t.Fatalf("hook fired %d times inside the startup grace, want 0", calls)
	}

	// Age past the grace window; the next change must fire.
	eng.startedAt = time.Now().Add(-2 * underlayRestartGrace)
	eng.notifyUnderlayChange()
	if calls != 1 {
		t.Fatalf("hook fired %d times after the grace window, want 1", calls)
	}
}

// TestReconnectAllPeersRearmsEndpointsForRedial covers the terminal-partition
// bug: after a roam, reconnectAllPeers ages every session and pruneDead reaps
// them outright — node, routes, AND endpoint. A peer only ever learned via
// gossip (not a configured seed) then has nothing left to dial, so recovery
// hinges entirely on a configured seed being reachable on the new underlay.
// When it isn't (a lossy roam), the session table empties and stays empty:
// every later roam ages an already-empty table and does nothing, so the mesh
// never recovers until a restart re-reads the seeds. reconnectAllPeers must
// therefore re-arm each former peer's endpoint as a redial target before
// aging it, turning every former peer — not just seeds — into a standing dial
// target the maintenance loop keeps retrying.
func TestReconnectAllPeersRearmsEndpointsForRedial(t *testing.T) {
	const netID = uint64(0xA12)
	eng := NewEngine(Options{
		NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450,
		Nets: []NetSpec{{ID: netID, Name: "n", Dev: newFakeDev("d"),
			Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	eng.Attach(nopSender{})
	ns := eng.network(netID)

	// A gossip-learned (non-seed) peer with a known underlay endpoint.
	ep := netip.MustParseAddrPort("203.0.113.1:65432")
	ps := &peerSession{nodeID: "peer", net: ns, localIdx: 7, endpoint: ep}
	ps.initPMTU(eng.pmtuFloor, eng.pmtuCeil)
	ps.setLastRx(time.Now())
	eng.mu.Lock()
	eng.sessions[7] = ps
	eng.mu.Unlock()
	ns.mu.Lock()
	ns.byNode["peer"] = ps
	ns.mu.Unlock()

	// Confirm it isn't already a seed (so the assertion below is meaningful).
	ns.mu.RLock()
	seedBefore := false
	for _, s := range ns.seeds {
		if s == ep {
			seedBefore = true
		}
	}
	ns.mu.RUnlock()
	if seedBefore {
		t.Fatal("test setup: peer endpoint should not be a seed before the roam")
	}

	// Roam recovery, then reap the aged session as the maintenance tick would.
	eng.reconnectAllPeers(ns)
	eng.pruneDead(ns, time.Now())

	// The reaped peer's endpoint must now be a standing redial target, or it's
	// gone forever and recovery can't proceed without a reachable seed.
	ns.mu.RLock()
	seedAfter := false
	for _, s := range ns.seeds {
		if s == ep {
			seedAfter = true
		}
	}
	ns.mu.RUnlock()
	if !seedAfter {
		t.Fatal("reconnectAllPeers did not re-arm the reaped peer's endpoint for redial — " +
			"after a lossy roam the session table would empty and never recover without a restart")
	}
}
