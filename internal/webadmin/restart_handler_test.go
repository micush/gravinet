package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gravinet/internal/service"
)

// TestHandleRestartRefusesWhenUnavailable proves /api/restart consults
// service.CanRestart before ever telling the browser a restart is underway:
// on a host where gravinet isn't actually registered as a service (true in
// this test environment, and previously true for the *entire* handler on
// every OS but Linux), it must return a clear error rather than optimistically
// claiming restarting:true and leaving the browser to time out finding out
// otherwise.
func TestHandleRestartRefusesWhenUnavailable(t *testing.T) {
	ok, hint := service.CanRestart()
	if ok {
		t.Skip("this host can actually restart gravinet as a service; the negative case isn't exercisable here")
	}

	srv := testServer()
	ts := httptest.NewServer(http.HandlerFunc(srv.handleRestart))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	got, _ := out["error"].(string)
	if got != hint {
		t.Fatalf("error = %q, want the exact hint from CanRestart: %q", got, hint)
	}
	if _, restarting := out["restarting"]; restarting {
		t.Fatal("response must not claim restarting:true when CanRestart says it can't")
	}
}
