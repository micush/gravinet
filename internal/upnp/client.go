// Package upnp implements a minimal UPnP Internet Gateway Device (IGD)
// client: SSDP discovery to find the LAN router's port-mapping control
// endpoint, plus the SOAP actions gravinet needs (AddPortMapping,
// DeletePortMapping, GetExternalIPAddress) to ask it to forward a port from
// its WAN side to this host. This is the standard "auto-configure my
// router" NAT-traversal convenience many P2P/VPN tools offer, so a node
// behind a home/office router with no manual port forward configured can
// still be reached directly by peers.
//
// No third-party dependencies: SSDP is a handful of lines of raw UDP, and
// the SOAP calls are hand-built XML over net/http. See manager.go for the
// piece that actually owns a mapping's lifecycle (discover once, renew
// before the lease expires, remove on shutdown); Gateway here is the
// stateless client that lifecycle is built on.
package upnp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const ssdpAddr = "239.255.255.250:1900"

// searchTargets is tried in order — WANIPConnection covers the
// overwhelming majority of home/office routers; WANPPPConnection is the
// older DSL/PPPoE variant some routers still expose instead. Both are
// queried on every Discover call (a router only answers the ones it
// actually implements), and findWANService below prefers WANIPConnection
// if a device oddly advertises both.
var searchTargets = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// discoverTimeout bounds how long Discover waits for a reply before giving
// up — SSDP is UDP/best-effort, so "nothing answered" has to be decided by
// a timeout rather than any positive signal. Capped by ctx's own deadline
// too, if it has one and it's sooner.
const discoverTimeout = 3 * time.Second

// Gateway is a discovered UPnP Internet Gateway Device's port-mapping
// control endpoint: where to send AddPortMapping/DeletePortMapping/
// GetExternalIPAddress requests, and the LAN-facing address this host
// talks to it from (NewInternalClient in every mapping request).
type Gateway struct {
	ControlURL  string
	ServiceType string
	LocalIP     string
}

// Discover finds a UPnP Internet Gateway Device on the local network via
// SSDP (multicast M-SEARCH to 239.255.255.250:1900) and resolves its
// port-mapping control URL from the device description document the
// LOCATION header points to. Returns an error if nothing usable answers
// before discoverTimeout (or ctx's own deadline, if sooner) — most
// commonly because UPnP is simply off on the router, which is a normal,
// expected outcome, not something callers should treat as exceptional.
func Discover(ctx context.Context) (*Gateway, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, fmt.Errorf("upnp: opening discovery socket: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(discoverTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	conn.SetDeadline(deadline)

	dst, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, fmt.Errorf("upnp: resolving ssdp address: %w", err)
	}

	var lastErr error
	sent := false
	for _, st := range searchTargets {
		req := "M-SEARCH * HTTP/1.1\r\n" +
			"HOST: " + ssdpAddr + "\r\n" +
			"MAN: \"ssdp:discover\"\r\n" +
			"MX: 2\r\n" +
			"ST: " + st + "\r\n\r\n"
		if _, werr := conn.WriteToUDP([]byte(req), dst); werr != nil {
			lastErr = werr
			continue
		}
		sent = true
	}
	if !sent {
		return nil, fmt.Errorf("upnp: sending M-SEARCH: %w", lastErr)
	}

	buf := make([]byte, 2048)
	for {
		n, _, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("upnp: no gateway responded (%v): %w", lastErr, rerr)
			}
			return nil, fmt.Errorf("upnp: no gateway responded: %w", rerr)
		}
		loc, perr := parseSSDPLocation(buf[:n])
		if perr != nil {
			continue // not a usable reply — keep listening until the deadline
		}
		gw, gerr := resolveGateway(ctx, loc)
		if gerr != nil {
			lastErr = gerr
			continue // this device answered but isn't a usable IGD — keep listening in case another one is
		}
		return gw, nil
	}
}

// parseSSDPLocation extracts the LOCATION header value from a raw SSDP
// M-SEARCH response (an HTTP-response-shaped block of text received over
// UDP). Header-name matching is case-insensitive, since SSDP
// implementations vary in casing ("LOCATION" vs "Location").
func parseSSDPLocation(resp []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(resp))
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(line[:i]), "LOCATION") {
			loc := strings.TrimSpace(line[i+1:])
			if loc == "" {
				return "", fmt.Errorf("upnp: empty LOCATION header")
			}
			return loc, nil
		}
	}
	return "", fmt.Errorf("upnp: no LOCATION header in SSDP response")
}

// resolveGateway fetches the UPnP device description document at loc,
// extracts the WANIPConnection/WANPPPConnection control URL from it, and
// pairs it with the LAN address this host would use to reach the gateway.
func resolveGateway(ctx context.Context, loc string) (*Gateway, error) {
	u, err := url.Parse(loc)
	if err != nil {
		return nil, fmt.Errorf("upnp: bad LOCATION %q: %w", loc, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upnp: fetching device description: %w", err)
	}
	defer resp.Body.Close()
	// A device description is a few KB; a well-behaved IGD never sends
	// anywhere close to this cap. Guards against an ill-behaved or
	// malicious responder on the LAN segment tying up memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("upnp: reading device description: %w", err)
	}
	controlPath, serviceType, err := parseDeviceDescription(body)
	if err != nil {
		return nil, err
	}
	localIP, err := localIPFor(u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("upnp: determining local address for gateway %s: %w", u.Hostname(), err)
	}
	return &Gateway{
		ControlURL:  resolveControlURL(u, controlPath),
		ServiceType: serviceType,
		LocalIP:     localIP,
	}, nil
}

// localIPFor returns the local address this host would use to reach host.
// Dialing UDP performs no handshake or packet exchange — it just consults
// the OS routing table for which local interface a packet to host would
// go out on. Mirrors internal/mesh/pmtu.go's localSourceIP, same idiom.
func localIPFor(host string) (string, error) {
	c, err := net.Dial("udp4", net.JoinHostPort(host, "1900"))
	if err != nil {
		return "", err
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP.IsUnspecified() {
		return "", fmt.Errorf("could not determine local address for %s", host)
	}
	return ua.IP.String(), nil
}

// upnpDevice/upnpService mirror just the fields gravinet needs from a UPnP
// device description document (UPnP Device Architecture 1.0 §2.3):
// recursively nested devices (deviceList) and each device's own
// serviceList, since the WAN connection service is typically two or three
// levels down (root device → WANDevice → WANConnectionDevice →
// WANIPConnection/WANPPPConnection service), not on the root device
// itself.
type upnpDevice struct {
	DeviceList struct {
		Device []upnpDevice `xml:"device"`
	} `xml:"deviceList"`
	ServiceList struct {
		Service []upnpService `xml:"service"`
	} `xml:"serviceList"`
}

type upnpService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

type upnpRoot struct {
	Device upnpDevice `xml:"device"`
}

// parseDeviceDescription walks a UPnP device description document looking
// for a WANIPConnection or WANPPPConnection service and returns its
// controlURL (still relative to the description document's own URL — the
// caller resolves it via resolveControlURL) and exact service type string,
// which every subsequent SOAP call's namespace/SOAPAction must match
// verbatim.
func parseDeviceDescription(body []byte) (controlURL, serviceType string, err error) {
	var root upnpRoot
	if uerr := xml.Unmarshal(body, &root); uerr != nil {
		return "", "", fmt.Errorf("upnp: parsing device description: %w", uerr)
	}
	if cu, st, ok := findWANService(root.Device); ok {
		return cu, st, nil
	}
	return "", "", fmt.Errorf("upnp: no WANIPConnection/WANPPPConnection service in device description")
}

// findWANService recursively searches d and its nested devices for a WAN
// connection service, preferring WANIPConnection over WANPPPConnection if
// a device advertises both (see searchTargets' ordering rationale).
func findWANService(d upnpDevice) (controlURL, serviceType string, ok bool) {
	var pppCU, pppST string
	havePPP := false
	for _, svc := range d.ServiceList.Service {
		switch svc.ServiceType {
		case "urn:schemas-upnp-org:service:WANIPConnection:1":
			return svc.ControlURL, svc.ServiceType, true
		case "urn:schemas-upnp-org:service:WANPPPConnection:1":
			pppCU, pppST, havePPP = svc.ControlURL, svc.ServiceType, true
		}
	}
	for _, child := range d.DeviceList.Device {
		if cu, st, cok := findWANService(child); cok {
			return cu, st, true
		}
	}
	if havePPP {
		return pppCU, pppST, true
	}
	return "", "", false
}

// resolveControlURL resolves a device description's controlURL — commonly
// a bare path like "/upnp/control/WANIPConn1" — against the URL the
// description document was itself fetched from.
func resolveControlURL(descURL *url.URL, controlPath string) string {
	ref, err := url.Parse(controlPath)
	if err != nil {
		return controlPath // best-effort — a malformed value fails at request time with a clearer error from the failed HTTP call itself
	}
	return descURL.ResolveReference(ref).String()
}

// soapArg is one input argument of a UPnP SOAP action call, in the exact
// order the action's spec defines (order matters to some IGD
// implementations, unlike ordinary XML attribute/element rules).
type soapArg struct {
	Name, Value string
}

// AddPortMapping asks the gateway to forward externalPort (WAN side) to
// internalPort on this host (protocol is "UDP" or "TCP", matching the
// UPnP spec's NewProtocol enumeration exactly). AddPortMapping is defined
// to replace any existing mapping for the same external port/protocol
// rather than conflict with it, which is also how a lease renewal works —
// call it again with the same arguments before the lease runs out.
// leaseSeconds is how long the router should keep the mapping; 0 means
// "forever" per the UPnP spec, but plenty of consumer routers mishandle
// that value (silently drop it, cap it, or reject it outright), so
// callers should prefer a finite duration and renew before it expires
// (see Manager).
func (g *Gateway) AddPortMapping(ctx context.Context, externalPort, internalPort int, protocol, description string, leaseSeconds int) error {
	args := []soapArg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(externalPort)},
		{"NewProtocol", protocol},
		{"NewInternalPort", strconv.Itoa(internalPort)},
		{"NewInternalClient", g.LocalIP},
		{"NewEnabled", "1"},
		{"NewPortMappingDescription", description},
		{"NewLeaseDuration", strconv.Itoa(leaseSeconds)},
	}
	_, err := g.soapCall(ctx, "AddPortMapping", args)
	return err
}

// DeletePortMapping removes a previously added mapping.
func (g *Gateway) DeletePortMapping(ctx context.Context, externalPort int, protocol string) error {
	args := []soapArg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(externalPort)},
		{"NewProtocol", protocol},
	}
	_, err := g.soapCall(ctx, "DeletePortMapping", args)
	return err
}

// GetExternalIPAddress returns the router's current WAN-side IP — mainly
// useful for logging what address peers should actually be reaching,
// which is the router's IP, not necessarily anything this host itself
// knows about.
func (g *Gateway) GetExternalIPAddress(ctx context.Context) (string, error) {
	vals, err := g.soapCall(ctx, "GetExternalIPAddress", nil)
	if err != nil {
		return "", err
	}
	return vals["NewExternalIPAddress"], nil
}

// soapCall builds and sends a UPnP SOAP request for action against g's
// control URL/service type, and parses the response into a name→value map
// of whatever output arguments it carried (empty for actions like
// AddPortMapping/DeletePortMapping that return none).
func (g *Gateway) soapCall(ctx context.Context, action string, args []soapArg) (map[string]string, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?>`)
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&body, `<u:%s xmlns:u="%s">`, action, g.ServiceType)
	for _, a := range args {
		fmt.Fprintf(&body, `<%s>%s</%s>`, a.Name, xmlEscape(a.Value), a.Name)
	}
	fmt.Fprintf(&body, `</u:%s>`, action)
	body.WriteString(`</s:Body></s:Envelope>`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.ControlURL, strings.NewReader(body.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", fmt.Sprintf(`"%s#%s"`, g.ServiceType, action))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upnp: %s request: %w", action, err)
	}
	defer resp.Body.Close()
	// SOAP 1.1 returns a Fault body on HTTP 500 for a rejected request —
	// that's the normal, spec-defined way an IGD reports "no" — so the
	// status code is deliberately not treated as fatal on its own; the
	// body is always parsed for a Fault first, since that's the
	// actionable detail.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("upnp: reading %s response: %w", action, err)
	}
	return parseSOAPResponse(action, respBody)
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s)) //nolint:errcheck // strings.Builder never errors
	return b.String()
}

// soapFault mirrors a UPnP SOAP Fault body far enough to extract the two
// things worth surfacing: the plain faultstring every SOAP 1.1 fault
// carries, and — when the fault came from a UPnP action specifically
// (rather than a transport-level SOAP problem) — the numeric UPnPError
// code and description, e.g. "718 ConflictInMappingEntry".
type soapFault struct {
	FaultString string `xml:"Body>Fault>faultstring"`
	ErrorCode   string `xml:"Body>Fault>detail>UPnPError>errorCode"`
	ErrorDesc   string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
}

// parseSOAPResponse checks a UPnP SOAP response for a Fault — surfaced as
// an error, including the UPnP error code when present — and otherwise
// returns its output arguments as a flat name→value map. Every action
// gravinet calls returns at most a handful of scalar values, so a generic
// "grab every leaf element under the response body" pass (parseLeafElements)
// is simpler than a struct per action.
func parseSOAPResponse(action string, body []byte) (map[string]string, error) {
	var fault soapFault
	if err := xml.Unmarshal(body, &fault); err == nil && fault.FaultString != "" {
		if fault.ErrorCode != "" {
			return nil, fmt.Errorf("upnp: %s rejected: %s %s", action, fault.ErrorCode, fault.ErrorDesc)
		}
		return nil, fmt.Errorf("upnp: %s rejected: %s", action, fault.FaultString)
	}
	var generic struct {
		Body struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(body, &generic); err != nil {
		return nil, fmt.Errorf("upnp: parsing %s response: %w", action, err)
	}
	return parseLeafElements(generic.Body.Inner), nil
}

// parseLeafElements does a flat decode of a UPnP SOAP action response
// body — <ActionNameResponse><NewFoo>bar</NewFoo>...</ActionNameResponse>
// — into name→value, ignoring the wrapping response element itself and
// any namespace prefixes (implementations vary in which prefix they use).
func parseLeafElements(inner []byte) map[string]string {
	out := map[string]string{}
	dec := xml.NewDecoder(bytes.NewReader(inner))
	var curName string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			curName = t.Name.Local
		case xml.CharData:
			if curName != "" {
				if v := strings.TrimSpace(string(t)); v != "" {
					out[curName] = v
				}
			}
		case xml.EndElement:
			curName = ""
		}
	}
	return out
}
