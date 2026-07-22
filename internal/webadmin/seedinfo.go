package webadmin

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// seedHost extracts the bare host from a seed address, undoing whatever the
// web UI's own stripScheme()/seedWithScheme() may have added: an optional
// "tcp://"/"udp://" prefix, then either "host:port" or a bare "host". Mirrors
// the JS stripScheme() in ui.go, so a seed address always parses the same way
// on both sides.
func seedHost(addr string) string {
	a := strings.TrimSpace(addr)
	low := strings.ToLower(a)
	if strings.HasPrefix(low, "tcp://") || strings.HasPrefix(low, "udp://") {
		a = a[6:]
	}
	if host, _, err := net.SplitHostPort(a); err == nil {
		return host
	}
	// No port present - use as-is, trimming any bracket wrapping a bare IPv6
	// literal (SplitHostPort only accepts brackets when a port follows them).
	return strings.Trim(a, "[]")
}

// seedInfoResult is the forward DNS / reverse DNS / WHOIS / geo-IP picture
// for one seed's host. Every section is independent and best-effort: one
// failing (e.g. no PTR record) doesn't stop the others from being reported.
type seedInfoResult struct {
	Host string `json:"host"`
	IsIP bool   `json:"isIP"`

	Forward    []string `json:"forward,omitempty"`
	ForwardErr string   `json:"forwardErr,omitempty"`

	ReverseTarget string   `json:"reverseTarget,omitempty"`
	Reverse       []string `json:"reverse,omitempty"`
	ReverseErr    string   `json:"reverseErr,omitempty"`

	WhoisTarget string `json:"whoisTarget,omitempty"`
	Whois       string `json:"whois,omitempty"`
	WhoisErr    string `json:"whoisErr,omitempty"`

	// GeoEnabled reports whether Geo-IP lookups are turned on at all (see
	// config.WebAdmin.GeoIPLookup), always populated regardless of outcome,
	// so the UI can tell "turned off" apart from "attempted and failed"
	// instead of showing a generic empty/error state for both.
	GeoEnabled bool         `json:"geoEnabled"`
	GeoTarget  string       `json:"geoTarget,omitempty"`
	Geo        *geoIPResult `json:"geo,omitempty"`
	GeoErr     string       `json:"geoErr,omitempty"`
}

// lookupSeedInfo resolves host (forward DNS, if it's a hostname), then runs
// reverse DNS, WHOIS, and — if geoEnabled — a Geo-IP lookup against whichever
// IP is now in play — the host itself if it was already an IP, or its first
// forward-resolved address otherwise. All three run concurrently since none
// depends on the others, only on the forward step (when there is one).
func lookupSeedInfo(ctx context.Context, host string, geoEnabled bool) seedInfoResult {
	res := seedInfoResult{Host: host, GeoEnabled: geoEnabled}

	target := host
	if ip := net.ParseIP(host); ip != nil {
		res.IsIP = true
	} else {
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			res.ForwardErr = err.Error()
			target = "" // nothing to reverse-lookup or whois
		} else {
			res.Forward = addrs
			target = ""
			for _, a := range addrs {
				if net.ParseIP(a) != nil {
					target = a
					break
				}
			}
		}
	}
	if target == "" {
		return res
	}

	var wg sync.WaitGroup
	res.ReverseTarget = target
	res.WhoisTarget = target
	wg.Add(2)
	go func() {
		defer wg.Done()
		names, err := net.DefaultResolver.LookupAddr(ctx, target)
		if err != nil {
			res.ReverseErr = err.Error()
			return
		}
		res.Reverse = names
	}()
	go func() {
		defer wg.Done()
		text, err := whoisIP(ctx, target)
		if err != nil {
			res.WhoisErr = err.Error()
			return
		}
		res.Whois = text
	}()
	if geoEnabled {
		res.GeoTarget = target
		wg.Add(1)
		go func() {
			defer wg.Done()
			geo, err := lookupGeoIP(ctx, target)
			if err != nil {
				res.GeoErr = err.Error()
				return
			}
			res.Geo = &geo
		}()
	}
	wg.Wait()
	return res
}

// whoisQuery sends one query to a whois server (RFC 3912: connect, write the
// query line, read until the server closes the connection — there's no
// framing beyond that). Partial output is still returned even if the read
// itself ends in an error, since a whois server closing early after sending
// a usable amount of text is common and the partial text is still useful.
func whoisQuery(ctx context.Context, server, query string) (string, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(server, "43"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(8 * time.Second))
	}
	if _, err := conn.Write([]byte(query + "\r\n")); err != nil {
		return "", err
	}
	b, err := io.ReadAll(conn)
	if len(b) > 0 {
		return string(b), nil
	}
	return "", err
}

// whoisReferral picks out a "refer:"/"whois:" line, the standard way a whois
// server points to the more specific server actually holding a record.
func whoisReferral(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		low := strings.ToLower(line)
		if strings.HasPrefix(low, "refer:") || strings.HasPrefix(low, "whois:") {
			if idx := strings.IndexByte(line, ':'); idx >= 0 {
				return strings.TrimSpace(line[idx+1:])
			}
		}
	}
	return ""
}

// whoisIP looks up IP registration info, starting at IANA and following one
// referral to the responsible regional registry (ARIN, RIPE, APNIC, LACNIC,
// or AFRINIC) — the standard way whois clients resolve "who actually holds
// this address" for any public IP worldwide, without needing to know in
// advance which of the five registries to ask.
func whoisIP(ctx context.Context, ip string) (string, error) {
	text, err := whoisQuery(ctx, "whois.iana.org", ip)
	if err != nil {
		return "", err
	}
	if refer := whoisReferral(text); refer != "" && !strings.EqualFold(refer, "whois.iana.org") {
		if t2, err2 := whoisQuery(ctx, refer, ip); err2 == nil && strings.TrimSpace(t2) != "" {
			return t2, nil
		}
	}
	return text, nil
}

// handleSeedInfo runs forward DNS, reverse DNS, WHOIS, and (if enabled)
// Geo-IP for a seed's host, for the info (\U0001F6C8) button next to a
// ticked seed in the web UI.
func (s *Server) handleSeedInfo(w http.ResponseWriter, r *http.Request) {
	var req struct{ Net, Addr string }
	if !decode(w, r, &req) {
		return
	}
	host := seedHost(req.Addr)
	if host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty seed address"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, lookupSeedInfo(ctx, host, s.cfg.GeoIPEnabled()))
}
