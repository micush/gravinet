package upgrade

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Source is a peer that has advertised it holds an artifact: where to reach its
// web admin over the overlay, and how far away it is.
type Source struct {
	NodeID   string
	Hostname string
	Addr     netip.Addr // overlay address — never an underlay one; see Fetch
	Port     int        // its web-admin port
	RTT      time.Duration
	Seen     time.Time
}

// Doer is the HTTP surface Fetch needs, so a test can supply a fake fleet
// without standing up ten TLS servers on an overlay that does not exist.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// BlobPath is the endpoint a peer serves staged artifacts from.
const BlobPath = "/api/upgrade/blob"

// FetchTimeout bounds a single source attempt. Generous: this is a multi-MiB
// binary crossing an overlay that may be relayed through a third node on a
// domestic uplink.
const FetchTimeout = 10 * time.Minute

// OverlayClient is the HTTP client used to pull from peers. It mirrors
// webadmin's proxyClient: peer certs are self-signed and the channel is already
// the encrypted mesh, so certificate verification would be verifying the wrong
// thing. The trust boundary for *reaching* a peer is the mesh PSK; the trust
// boundary for *believing what it sends* is the artifact signature, which is
// checked on ingest regardless of which peer served the bytes. That separation
// is what makes it safe to pull a binary from any peer at all: a malicious peer
// can waste our bandwidth, but it cannot make us run its code.
func OverlayClient() *http.Client {
	return &http.Client{
		Timeout: FetchTimeout,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // overlay-internal, self-signed
			ForceAttemptHTTP2:   false,
			MaxIdleConnsPerHost: 2,
		},
	}
}

// OverlayGuard is the check Fetch applies to a source address before dialing it.
// A source list is built from peer *advertisements*, so a hostile peer can put
// any address it likes in one — including 127.0.0.1, the LAN, or a cloud
// metadata endpoint. Constraining the dial to genuine overlay addresses is the
// same SSRF guard webadmin's resolveManagedTarget applies for the same reason,
// and it is passed in rather than reimplemented so there is one definition of
// "is this really an overlay address" in the tree.
type OverlayGuard func(netip.Addr) bool

// Fetch downloads the artifact described by m from the first source that can
// serve it, verifying and storing it via st.Ingest.
//
// Sources are tried nearest-first (by the RTT the mesh already measures for
// every session — see peerSession.rttNanos), which on a ten-node mesh usually
// means a node in the same rack or region rather than the one across an ocean
// that happened to advertise first. Every source is tried before giving up: the
// most likely reason a fetch fails is not corruption but that the node holding
// the artifact went away mid-rollout, which is exactly the situation a fanout is
// supposed to survive.
//
// There is no ranged resume. It was considered and deliberately left out: a
// resumed download means trusting bytes on disk from a previous attempt against
// a *different* source, which turns one signature check into a per-range trust
// decision, and the artifact is tens of MiB across a link that is either fine
// (in which case a retry costs seconds) or so bad that the rollout has larger
// problems. Ingest hashes what it wrote, so a truncated attempt is discarded
// whole rather than patched.
func Fetch(ctx context.Context, st *Store, m Manifest, sources []Source, client Doer, guard OverlayGuard) error {
	if st.Have(m) {
		return nil // already here; nothing to do
	}
	if err := m.Verify(st.Trusted()); err != nil {
		// Refuse before spending a byte of bandwidth. A manifest we would not
		// accept the artifact for is not worth downloading the artifact for.
		return err
	}
	usable := make([]Source, 0, len(sources))
	for _, s := range sources {
		if !s.Addr.IsValid() || s.Port <= 0 || s.Port > 65535 {
			continue
		}
		if guard != nil && !guard(s.Addr) {
			continue // not an overlay address — see OverlayGuard
		}
		usable = append(usable, s)
	}
	if len(usable) == 0 {
		return errors.New("upgrade: no peer is advertising this artifact (nobody to fetch it from)")
	}
	sort.SliceStable(usable, func(i, j int) bool {
		// Unknown RTT (0) sorts last rather than first: a peer we have never
		// measured is not a peer we have measured at zero milliseconds.
		ri, rj := usable[i].RTT, usable[j].RTT
		if ri == 0 {
			ri = time.Hour
		}
		if rj == 0 {
			rj = time.Hour
		}
		return ri < rj
	})

	var errs []error
	for _, s := range usable {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fetchFrom(ctx, st, m, s, client); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sourceLabel(s), err))
			continue
		}
		return nil
	}
	return fmt.Errorf("upgrade: could not fetch %s from any of %d sources: %w", m.ID(), len(usable), errors.Join(errs...))
}

func sourceLabel(s Source) string {
	if s.Hostname != "" {
		return s.Hostname
	}
	if len(s.NodeID) > 8 {
		return s.NodeID[:8]
	}
	return s.NodeID
}

func fetchFrom(ctx context.Context, st *Store, m Manifest, s Source, client Doer) error {
	u := url.URL{
		Scheme:   "https",
		Host:     net.JoinHostPort(s.Addr.String(), strconv.Itoa(s.Port)),
		Path:     BlobPath,
		RawQuery: url.Values{"id": {m.ID()}}.Encode(),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned %s", resp.Status)
	}
	// Ingest re-hashes every byte and re-checks the signature, so nothing about
	// this response is trusted: not the length header, not the content type, not
	// the fact that the peer is a Manager. The bytes either hash to the digest a
	// key we trust signed, or they are discarded.
	return st.Ingest(m, resp.Body)
}
