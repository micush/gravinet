package upnp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIGDServer runs a real HTTP server standing in for a router's SOAP
// control endpoint, so Manager's lifecycle (mapping, renewal, teardown)
// is tested against something closer to the real HTTP round trip than a
// hand-rolled stub — the same reasoning as client_test.go's httptest use.
// It records every AddPortMapping/DeletePortMapping call (by port) so
// tests can assert on exactly what happened, and failPorts lets a test
// make specific ports get rejected while the rest succeed, to exercise
// Manager's per-mapping independence.
type fakeIGDServer struct {
	srv *httptest.Server

	mu        sync.Mutex
	adds      map[int]int // port -> successful AddPortMapping count
	failedAdd map[int]int // port -> rejected AddPortMapping count
	deletes   map[int]int // port -> DeletePortMapping count

	failPorts atomic.Value // map[int]bool — ports whose AddPortMapping is rejected; nil/empty means none
}

func newFakeIGDServer() *fakeIGDServer {
	f := &fakeIGDServer{adds: map[int]int{}, failedAdd: map[int]int{}, deletes: map[int]int{}}
	f.failPorts.Store(map[int]bool{})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.Header.Get("SOAPAction")
		body := readAll(r)
		port := extractPort(body)
		w.Header().Set("Content-Type", "text/xml")
		switch {
		case strings.Contains(action, "AddPortMapping"):
			if f.shouldFail(port) {
				f.mu.Lock()
				f.failedAdd[port]++
				f.mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>718</errorCode><errorDescription>ConflictInMappingEntry</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
				return
			}
			f.mu.Lock()
			f.adds[port]++
			f.mu.Unlock()
			fmt.Fprint(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:AddPortMappingResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"></u:AddPortMappingResponse></s:Body></s:Envelope>`)
		case strings.Contains(action, "DeletePortMapping"):
			f.mu.Lock()
			f.deletes[port]++
			f.mu.Unlock()
			fmt.Fprint(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:DeletePortMappingResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"></u:DeletePortMappingResponse></s:Body></s:Envelope>`)
		case strings.Contains(action, "GetExternalIPAddress"):
			fmt.Fprint(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"><NewExternalIPAddress>203.0.113.7</NewExternalIPAddress></u:GetExternalIPAddressResponse></s:Body></s:Envelope>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return f
}

func readAll(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// extractPort pulls the NewExternalPort value out of a SOAP request body —
// good enough for these tests' simple single-line-per-tag bodies.
func extractPort(body string) int {
	const tag = "<NewExternalPort>"
	i := strings.Index(body, tag)
	if i < 0 {
		return 0
	}
	rest := body[i+len(tag):]
	j := strings.Index(rest, "<")
	if j < 0 {
		return 0
	}
	p, _ := strconv.Atoi(rest[:j])
	return p
}

func (f *fakeIGDServer) shouldFail(port int) bool {
	fp, _ := f.failPorts.Load().(map[int]bool)
	return fp[port]
}

func (f *fakeIGDServer) setFailPorts(ports ...int) {
	m := map[int]bool{}
	for _, p := range ports {
		m[p] = true
	}
	f.failPorts.Store(m)
}

func (f *fakeIGDServer) addCount(port int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adds[port]
}

func (f *fakeIGDServer) failedAddCount(port int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failedAdd[port]
}

func (f *fakeIGDServer) deleteCount(port int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes[port]
}

func (f *fakeIGDServer) totalAdds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.adds {
		n += c
	}
	return n
}

func (f *fakeIGDServer) totalDeletes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.deletes {
		n += c
	}
	return n
}

func (f *fakeIGDServer) gateway() *Gateway {
	return &Gateway{ControlURL: f.srv.URL, ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:1", LocalIP: "192.168.1.50"}
}

func (f *fakeIGDServer) Close() { f.srv.Close() }

// waitFor polls cond until it's true or the timeout elapses, failing the
// test on timeout — used throughout instead of a fixed sleep so these
// tests aren't racy under load and don't needlessly wait longer than they
// have to.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// mappedSnapshot returns the set of mappings m currently believes are
// live, guarded by m's own mutex. Tests that need to know "has Manager
// fully processed this mapping's AddPortMapping result" must wait on this
// rather than on the fake server's own request counters: the server
// records a request as soon as it arrives, which is strictly *before* the
// client finishes reading the response and mapAll updates m.mapped — a
// narrow window in which Stop, called the moment a server-side counter
// ticks over, can race an in-flight response read, cancel it, and see a
// mapping as never-successful even though the server had already handled
// it. Waiting on this instead closes that window, since it's the exact
// same state (and mutex) Stop itself reads.
func (m *Manager) mappedSnapshot() map[PortMapping]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[PortMapping]bool, len(m.mapped))
	for k, v := range m.mapped {
		out[k] = v
	}
	return out
}

func TestNewManagerDropsInvalidAndDuplicateMappings(t *testing.T) {
	m := NewManager([]PortMapping{
		{Port: 65432, Protocol: "udp"},
		{Port: 0, Protocol: "TCP"},     // dropped: non-positive port
		{Port: -1, Protocol: "TCP"},    // dropped: non-positive port
		{Port: 65432, Protocol: "UDP"}, // dropped: duplicate of the first, once normalized
		{Port: 443, Protocol: "tcp"},
	}, "gravinet")
	if len(m.mappings) != 2 {
		t.Fatalf("got %d mappings, want 2: %+v", len(m.mappings), m.mappings)
	}
	want := map[PortMapping]bool{{65432, "UDP"}: true, {443, "TCP"}: true}
	for _, pm := range m.mappings {
		if !want[pm] {
			t.Errorf("unexpected mapping %+v", pm)
		}
	}
}

func TestManagerMapsMultiplePortsOnStartAndRemovesOnStop(t *testing.T) {
	fake := newFakeIGDServer()
	defer fake.Close()

	m := NewManager([]PortMapping{
		{Port: 65432, Protocol: "udp"},
		{Port: 65432, Protocol: "tcp"},
		{Port: 443, Protocol: "tcp"},
	}, "gravinet")
	m.discover = func(ctx context.Context) (*Gateway, error) { return fake.gateway(), nil }
	m.discoveryRetry = 5 * time.Millisecond

	m.Start()
	waitFor(t, time.Second, func() bool {
		snap := m.mappedSnapshot()
		return snap[PortMapping{65432, "UDP"}] && snap[PortMapping{65432, "TCP"}] && snap[PortMapping{443, "TCP"}]
	})
	// Port 65432 was requested for both UDP and TCP — both should have
	// gone through as independent mappings (same port, different
	// protocol is not a duplicate).
	if fake.totalAdds() < 3 {
		t.Fatalf("expected at least 3 successful AddPortMapping calls (one per mapping), got %d", fake.totalAdds())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Stop(ctx)

	if fake.deleteCount(65432) < 2 { // one for UDP, one for TCP
		t.Errorf("DeletePortMapping calls for port 65432 = %d, want at least 2", fake.deleteCount(65432))
	}
	if fake.deleteCount(443) != 1 {
		t.Errorf("DeletePortMapping calls for port 443 = %d, want 1", fake.deleteCount(443))
	}
}

// If the router rejects one mapping but accepts the rest, the accepted
// ones must still go live (and get renewed/torn down normally) — one bad
// mapping shouldn't take the others down with it.
func TestManagerPartialFailureStillMapsTheRest(t *testing.T) {
	fake := newFakeIGDServer()
	defer fake.Close()
	fake.setFailPorts(443) // this one is rejected every time; 65432 and 8443 are not

	m := NewManager([]PortMapping{
		{Port: 65432, Protocol: "UDP"},
		{Port: 443, Protocol: "TCP"},
		{Port: 8443, Protocol: "TCP"},
	}, "gravinet")
	m.discover = func(ctx context.Context) (*Gateway, error) { return fake.gateway(), nil }
	m.discoveryRetry = 5 * time.Millisecond

	m.Start()
	waitFor(t, time.Second, func() bool {
		snap := m.mappedSnapshot()
		return snap[PortMapping{65432, "UDP"}] && snap[PortMapping{8443, "TCP"}] && fake.failedAddCount(443) >= 1
	})
	if fake.addCount(443) != 0 {
		t.Errorf("port 443 should never have succeeded, got %d successful adds", fake.addCount(443))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Stop(ctx)

	// Only the two that actually mapped should be deleted — never the one
	// that was always rejected (nothing to remove for it).
	if fake.deleteCount(65432) != 1 || fake.deleteCount(8443) != 1 {
		t.Errorf("expected exactly 1 delete each for 65432 and 8443, got %d and %d", fake.deleteCount(65432), fake.deleteCount(8443))
	}
	if fake.deleteCount(443) != 0 {
		t.Errorf("port 443 was never mapped, should never be deleted, got %d delete calls", fake.deleteCount(443))
	}
}

func TestManagerRenewsBeforeLeaseExpires(t *testing.T) {
	fake := newFakeIGDServer()
	defer fake.Close()

	m := NewManager([]PortMapping{{Port: 65432, Protocol: "UDP"}}, "gravinet")
	m.discover = func(ctx context.Context) (*Gateway, error) { return fake.gateway(), nil }
	m.renewInterval = 10 * time.Millisecond
	m.discoveryRetry = 5 * time.Millisecond

	m.Start()
	// The initial AddPortMapping, plus at least one renewal tick.
	waitFor(t, time.Second, func() bool { return fake.addCount(65432) >= 3 })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Stop(ctx)
}

// If discovery keeps failing, Manager must retry rather than give up —
// UPnP being off (or the router being mid-reboot) is an expected, common
// outcome, not a fatal one.
func TestManagerRetriesFailedDiscovery(t *testing.T) {
	var attempts atomic.Int32
	m := NewManager([]PortMapping{{Port: 65432, Protocol: "udp"}}, "gravinet")
	m.discover = func(ctx context.Context) (*Gateway, error) {
		attempts.Add(1)
		return nil, fmt.Errorf("no gateway found (simulated)")
	}
	m.discoveryRetry = 5 * time.Millisecond

	m.Start()
	waitFor(t, time.Second, func() bool { return attempts.Load() >= 3 })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Stop(ctx)
}

// If every mapping is rejected (not just some), Manager must retry the
// whole cycle rather than wedge or crash — and Stop on a Manager that
// never got as far as any successful mapping must be a safe no-op.
func TestManagerRetriesWhenEveryMappingFails(t *testing.T) {
	fake := newFakeIGDServer()
	defer fake.Close()
	fake.setFailPorts(65432)

	m := NewManager([]PortMapping{{Port: 65432, Protocol: "udp"}}, "gravinet")
	m.discover = func(ctx context.Context) (*Gateway, error) { return fake.gateway(), nil }
	m.discoveryRetry = 5 * time.Millisecond

	m.Start()
	waitFor(t, time.Second, func() bool { return fake.failedAddCount(65432) >= 3 })
	if fake.addCount(65432) != 0 {
		t.Errorf("a rejected AddPortMapping must never count as a success, got %d successes", fake.addCount(65432))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Stop(ctx)

	if fake.totalDeletes() != 0 {
		t.Errorf("DeletePortMapping should never be called when nothing ever mapped, got %d calls", fake.totalDeletes())
	}
}

func TestManagerStopBeforeStartIsSafeNoOp(t *testing.T) {
	m := NewManager([]PortMapping{{Port: 65432, Protocol: "udp"}}, "gravinet")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Stop(ctx) // must not panic or hang
}

// An empty (or entirely-invalid) mapping set is a legitimate, if useless,
// configuration — e.g. UPnP turned on but every listen port disabled —
// and must behave as an inert no-op rather than erroring or hanging.
func TestManagerWithNoMappingsIsInert(t *testing.T) {
	m := NewManager(nil, "gravinet")
	called := false
	m.discover = func(ctx context.Context) (*Gateway, error) {
		called = true
		return nil, fmt.Errorf("should never be called")
	}

	m.Start()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Stop(ctx) // must return promptly, not hang waiting on a loop that never does anything

	if called {
		t.Error("discover should never be called when there are no mappings to establish")
	}
}

func TestManagerStopIsIdempotent(t *testing.T) {
	fake := newFakeIGDServer()
	defer fake.Close()

	m := NewManager([]PortMapping{{Port: 65432, Protocol: "udp"}}, "gravinet")
	m.discover = func(ctx context.Context) (*Gateway, error) { return fake.gateway(), nil }
	m.discoveryRetry = 5 * time.Millisecond

	m.Start()
	waitFor(t, time.Second, func() bool { return m.mappedSnapshot()[PortMapping{65432, "UDP"}] })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Stop(ctx)
	m.Stop(ctx) // second call must not panic or double-fail
}
