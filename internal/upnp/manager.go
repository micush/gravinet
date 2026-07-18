package upnp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gravinet/internal/logx"
)

// defaultLeaseSeconds is how long each mapping is requested for;
// defaultRenewInterval (well under half the lease) is how often it's
// refreshed — see AddPortMapping's doc comment on why a finite lease beats
// requesting "forever". defaultDiscoveryRetry is the backoff between
// discovery attempts while no gateway has answered yet — a router
// rebooting, or UPnP simply being off on it, are both transient/expected
// rather than exceptional, so this retries quietly rather than giving up.
const (
	defaultLeaseSeconds   = 3600
	defaultRenewInterval  = 25 * time.Minute
	defaultDiscoveryRetry = 2 * time.Minute
)

// PortMapping is one port a Manager keeps forwarded through the gateway.
// Protocol is "UDP" or "TCP" (case-insensitive as given to NewManager —
// see there).
type PortMapping struct {
	Port     int
	Protocol string
}

func (pm PortMapping) String() string { return fmt.Sprintf("%s/%d", pm.Protocol, pm.Port) }

// Manager owns the background lifecycle of a *set* of UPnP port mappings
// sharing one discovered gateway: discover once, add every mapping in the
// set, keep renewing each before its lease expires, and remove whichever
// ones actually succeeded again on Stop. One shared discovery — rather
// than one Manager (and one SSDP round trip) per port — is both more
// polite to the router and means a single Manager instance is the thing
// cmd/gravinet/main.go actually drives; Gateway (client.go) is the
// low-level, stateless client it's built on.
//
// Mappings are independent of one another: if the router rejects one
// (say, a conflicting entry already exists) the rest still proceed, and a
// rejected one is retried on every later cycle the same as if it were the
// only mapping configured. Only a cycle where *none* of them succeed is
// treated as a failure worth backing off and rediscovering over.
type Manager struct {
	mappings    []PortMapping
	description string

	// Overridable seams for testing — same pattern as e.g. frr_test.go's
	// statFile seam elsewhere in this codebase. Defaulted by NewManager;
	// production code never touches these after construction.
	discover       func(ctx context.Context) (*Gateway, error)
	leaseSeconds   int
	renewInterval  time.Duration
	discoveryRetry time.Duration

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	gw     *Gateway             // set once at least one mapping succeeds; read by Stop for cleanup
	mapped map[PortMapping]bool // which mappings are currently believed live on gw
}

// NewManager creates a Manager for the given set of port mappings, sharing
// description across all of them (each mapping's own entry in the
// router's port-forwarding table is labeled "<description> (<protocol>
// <port>)", so they're distinguishable there). Nothing happens until
// Start is called. Each mapping's Protocol is normalized to uppercase
// ("udp" → "UDP") since that's what the UPnP spec's NewProtocol argument
// requires verbatim. Mappings with a non-positive port are dropped (a
// disabled port, e.g. UDP turned off entirely, has nothing to map), and
// duplicates (same port+protocol given twice) are collapsed to one.
func NewManager(mappings []PortMapping, description string) *Manager {
	var clean []PortMapping
	seen := map[PortMapping]bool{}
	for _, pm := range mappings {
		if pm.Port <= 0 {
			continue
		}
		pm.Protocol = strings.ToUpper(pm.Protocol)
		if seen[pm] {
			continue
		}
		seen[pm] = true
		clean = append(clean, pm)
	}
	return &Manager{
		mappings:       clean,
		description:    description,
		discover:       Discover,
		leaseSeconds:   defaultLeaseSeconds,
		renewInterval:  defaultRenewInterval,
		discoveryRetry: defaultDiscoveryRetry,
		mapped:         map[PortMapping]bool{},
	}
}

// Start begins the background discover-map-renew loop and returns
// immediately — discovery and mapping happen asynchronously, since a
// router that's slow, absent, or has UPnP turned off must never hold up
// the rest of gravinet's startup. Every failure is logged and retried;
// Start itself cannot fail. A Manager with no mappings left after
// NewManager's cleanup (see there) starts a no-op loop that exits
// immediately — callers are still free to call Stop on it. Calling Start
// more than once on the same Manager is not supported (mirrors the rest
// of this codebase's lifecycle types — a fresh Manager per logical set of
// mappings is the intended usage).
func (m *Manager) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go m.run(ctx)
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.done)
	if len(m.mappings) == 0 {
		return
	}
	for {
		gw, err := m.discover(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logx.Warnf("upnp: no gateway found (%v) — retrying in %s", err, m.discoveryRetry)
			if !m.sleep(ctx, m.discoveryRetry) {
				return
			}
			continue
		}
		if ctx.Err() != nil {
			return // Stop raced discover() succeeding right as it was cancelled
		}
		if m.mapAll(ctx, gw) == 0 {
			if ctx.Err() != nil {
				return
			}
			logx.Warnf("upnp: no port mappings succeeded — retrying in %s", m.discoveryRetry)
			if !m.sleep(ctx, m.discoveryRetry) {
				return
			}
			continue
		}
		m.mu.Lock()
		m.gw = gw
		m.mu.Unlock()
		if ip, ierr := gw.GetExternalIPAddress(ctx); ierr == nil && ip != "" {
			logx.Infof("upnp: gateway reports external IP %s", ip)
		}

		m.renewLoop(ctx, gw)
		if ctx.Err() != nil {
			return
		}
		// renewLoop only returns without ctx being done when a renewal
		// cycle failed *every* mapping, hard enough to warrant
		// rediscovering from scratch (e.g. the gateway itself became
		// unreachable) — clear the stale state and loop back to Discover.
		m.mu.Lock()
		m.gw = nil
		m.mapped = map[PortMapping]bool{}
		m.mu.Unlock()
	}
}

// mapAll attempts every configured mapping against gw, logging each one
// individually (a success only the first time it's newly mapped, so a
// renewal that keeps succeeding doesn't spam the log every cycle; a
// failure every time, since a router that keeps rejecting one is worth
// keeping visible). Returns how many succeeded this round.
func (m *Manager) mapAll(ctx context.Context, gw *Gateway) int {
	ok := 0
	for _, pm := range m.mappings {
		desc := fmt.Sprintf("%s (%s)", m.description, pm)
		err := gw.AddPortMapping(ctx, pm.Port, pm.Port, pm.Protocol, desc, m.leaseSeconds)
		m.mu.Lock()
		if err != nil {
			delete(m.mapped, pm)
			m.mu.Unlock()
			logx.Warnf("upnp: mapping %s failed (%v)", pm, err)
			continue
		}
		wasNew := !m.mapped[pm]
		m.mapped[pm] = true
		m.mu.Unlock()
		if wasNew {
			logx.Infof("upnp: mapped %s -> this host via gateway", pm)
		}
		ok++
	}
	return ok
}

func (m *Manager) renewLoop(ctx context.Context, gw *Gateway) {
	t := time.NewTicker(m.renewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if m.mapAll(ctx, gw) == 0 {
				if ctx.Err() != nil {
					return
				}
				logx.Warnf("upnp: renewing every mapping failed — rediscovering")
				return
			}
		}
	}
}

// sleep waits for d, or returns false early if ctx ends first.
func (m *Manager) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// Stop ends the background loop and best-effort removes whichever
// mappings were actually live, bounded by ctx — shutdown budgets are
// tight (see shutdownGrace in main.go), so a router that's gone
// unreachable must not hang process exit; every DeletePortMapping call
// here shares ctx's own deadline, so a slow/unreachable router simply
// truncates how many of them get attempted rather than blowing past the
// budget. Safe to call on a Manager that was never Started, or on which
// nothing ever actually mapped (nothing to remove in either case).
func (m *Manager) Stop(ctx context.Context) {
	if m.cancel == nil {
		return // never started
	}
	m.cancel()
	select {
	case <-m.done:
	case <-ctx.Done():
	}
	m.mu.Lock()
	gw := m.gw
	var live []PortMapping
	for pm, ok := range m.mapped {
		if ok {
			live = append(live, pm)
		}
	}
	m.mu.Unlock()
	if gw == nil {
		return
	}
	for _, pm := range live {
		if err := gw.DeletePortMapping(ctx, pm.Port, pm.Protocol); err != nil {
			logx.Warnf("upnp: removing %s mapping on shutdown: %v", pm, err)
		}
	}
}
