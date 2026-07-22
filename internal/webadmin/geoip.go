package webadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// geoIPResult is the subset of ipapi.co's response the info panel's location
// section and map actually use — not the full record (currency, languages,
// calling code, etc. aren't relevant here).
type geoIPResult struct {
	City        string  `json:"city,omitempty"`
	Region      string  `json:"region,omitempty"`
	Country     string  `json:"country_name,omitempty"`
	CountryCode string  `json:"country_code,omitempty"`
	Org         string  `json:"org,omitempty"`
	Latitude    float64 `json:"latitude,omitempty"`
	Longitude   float64 `json:"longitude,omitempty"`

	// ipapi.co reports its own failures (rate limit, reserved/private IP,
	// etc.) as HTTP 200 with a JSON body shaped {"error":true,"reason":"..."}
	// rather than a non-2xx status, so these have to be checked explicitly
	// after a successful decode — a 200 alone doesn't mean success here.
	Error  bool   `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// geoIPHTTPClient is reused across lookups (idiomatic for a client that's
// otherwise stateless) with an explicit timeout as a backstop alongside
// whatever deadline the caller's context already carries.
var geoIPHTTPClient = &http.Client{Timeout: 10 * time.Second}

// geoIPBaseURL is ipapi.co's endpoint, overridable so tests can point it at
// a local httptest.Server instead of making a real network call.
var geoIPBaseURL = "https://ipapi.co"

// lookupGeoIP queries ipapi.co for ip's approximate geolocation. Gated
// behind config.WebAdmin.GeoIPLookup (on by default — see its doc comment),
// so this never runs when an operator has explicitly disabled it.
func lookupGeoIP(ctx context.Context, ip string) (geoIPResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geoIPBaseURL+"/"+ip+"/json/", nil)
	if err != nil {
		return geoIPResult{}, err
	}
	// ipapi.co's free tier asks callers to identify themselves with a
	// distinct User-Agent (their docs note a generic/absent one is more
	// likely to get caught by abuse heuristics); this also makes
	// [gravinet]'s traffic to them honestly attributable rather than
	// anonymous.
	req.Header.Set("User-Agent", "gravinet-webadmin")
	resp, err := geoIPHTTPClient.Do(req)
	if err != nil {
		return geoIPResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return geoIPResult{}, fmt.Errorf("geo-ip lookup returned %s", resp.Status)
	}
	var out geoIPResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return geoIPResult{}, err
	}
	if out.Error {
		reason := out.Reason
		if reason == "" {
			reason = "lookup failed"
		}
		return geoIPResult{}, fmt.Errorf("%s", reason)
	}
	return out, nil
}
