package webadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withGeoIPServer points geoIPBaseURL at a local httptest.Server for the
// duration of one test and restores the real ipapi.co URL afterward, so
// tests never depend on (or accidentally hit) the live third-party service.
func withGeoIPServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := geoIPBaseURL
	geoIPBaseURL = srv.URL
	t.Cleanup(func() { geoIPBaseURL = old })
}

func TestLookupGeoIPSuccess(t *testing.T) {
	withGeoIPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/203.0.113.5/") {
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("expected a non-empty User-Agent header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"city":"Sydney","region":"New South Wales","country_name":"Australia",
			"country_code":"AU","org":"Example ISP","latitude":-33.8591,"longitude":151.2002}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := lookupGeoIP(ctx, "203.0.113.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.City != "Sydney" || got.Country != "Australia" || got.Org != "Example ISP" {
		t.Errorf("got %+v, missing expected fields", got)
	}
	if got.Latitude != -33.8591 || got.Longitude != 151.2002 {
		t.Errorf("got lat/lon %v/%v, want -33.8591/151.2002", got.Latitude, got.Longitude)
	}
}

// TestLookupGeoIPErrorInBody covers ipapi.co's actual failure shape: HTTP
// 200 with {"error":true,"reason":"..."} in the body, rather than a non-2xx
// status — this is the specific quirk lookupGeoIP has to check for
// explicitly, since a bare status-code check would treat this as success.
func TestLookupGeoIPErrorInBody(t *testing.T) {
	withGeoIPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":true,"reason":"Reserved IP Address"}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := lookupGeoIP(ctx, "10.0.0.1")
	if err == nil {
		t.Fatal("expected an error for a reserved/private IP")
	}
	if !strings.Contains(err.Error(), "Reserved IP Address") {
		t.Errorf("error = %q, want it to include the reason from the response body", err.Error())
	}
}

func TestLookupGeoIPNon200(t *testing.T) {
	withGeoIPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := lookupGeoIP(ctx, "203.0.113.5")
	if err == nil {
		t.Fatal("expected an error for a non-200 status")
	}
}

// TestLookupSeedInfoGeoDisabled confirms geoEnabled=false never even attempts
// a Geo-IP call — the test server errors the request if hit at all, so a
// stray call (not just a wrong result) fails the test. Uses a short context
// timeout: lookupSeedInfo's WHOIS/reverse-DNS goroutines run concurrently
// with the Geo-IP one and this test doesn't care whether they succeed, only
// that Geo-IP behaves — no need to wait out their own timeouts too.
func TestLookupSeedInfoGeoDisabled(t *testing.T) {
	withGeoIPServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("lookupGeoIP should not be called when geoEnabled is false")
		w.WriteHeader(http.StatusInternalServerError)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := lookupSeedInfo(ctx, "203.0.113.5", false)
	if res.GeoEnabled {
		t.Error("GeoEnabled should be false")
	}
	if res.GeoTarget != "" || res.Geo != nil || res.GeoErr != "" {
		t.Errorf("expected no geo fields populated, got target=%q geo=%v err=%q", res.GeoTarget, res.Geo, res.GeoErr)
	}
}

// TestLookupSeedInfoGeoEnabled confirms geoEnabled=true runs the Geo-IP
// lookup alongside reverse DNS/WHOIS and folds a successful result into
// seedInfoResult correctly. Same short-timeout reasoning as the test above.
func TestLookupSeedInfoGeoEnabled(t *testing.T) {
	withGeoIPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"city":"Testville","country_name":"Testland","latitude":1.5,"longitude":2.5}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := lookupSeedInfo(ctx, "203.0.113.5", true)
	if !res.GeoEnabled {
		t.Error("GeoEnabled should be true")
	}
	if res.GeoTarget != "203.0.113.5" {
		t.Errorf("GeoTarget = %q, want the resolved IP", res.GeoTarget)
	}
	if res.GeoErr != "" {
		t.Errorf("unexpected GeoErr: %q", res.GeoErr)
	}
	if res.Geo == nil || res.Geo.City != "Testville" {
		t.Errorf("Geo = %+v, want a populated result with City=Testville", res.Geo)
	}
}
