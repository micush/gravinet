package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandleReadme(t *testing.T) {
	dir := t.TempDir()
	rp := filepath.Join(dir, "README.md")
	os.WriteFile(rp, []byte("# Title\n\nhello world\n"), 0o644)

	s := &Server{}
	s.SetReadmePath(rp)
	rr := httptest.NewRecorder()
	s.handleReadme(rr, httptest.NewRequest(http.MethodGet, "/api/readme", nil))
	var out struct {
		Text      string `json:"text"`
		Available bool   `json:"available"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if !out.Available || out.Text == "" {
		t.Fatalf("expected available readme, got available=%v text=%q", out.Available, out.Text)
	}

	// Unset path -> not available.
	s2 := &Server{}
	rr2 := httptest.NewRecorder()
	s2.handleReadme(rr2, httptest.NewRequest(http.MethodGet, "/api/readme", nil))
	var out2 struct {
		Available bool `json:"available"`
	}
	json.Unmarshal(rr2.Body.Bytes(), &out2)
	if out2.Available {
		t.Error("expected unavailable when no readme path set")
	}
}

// TestHandleGettingStarted covers the markdown-source endpoint the Info →
// Getting Started page renders via mdRender, the same way README does.
func TestHandleGettingStarted(t *testing.T) {
	dir := t.TempDir()
	gp := filepath.Join(dir, "getting-started.md")
	os.WriteFile(gp, []byte("# Getting started\n\nhello world\n"), 0o644)

	s := &Server{}
	s.SetGettingStartedPath(gp)
	rr := httptest.NewRecorder()
	s.handleGettingStarted(rr, httptest.NewRequest(http.MethodGet, "/api/getting-started", nil))
	var out struct {
		Text      string `json:"text"`
		Available bool   `json:"available"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if !out.Available || out.Text == "" {
		t.Fatalf("expected available getting-started guide, got available=%v text=%q", out.Available, out.Text)
	}

	// Unset path -> not available, same shape as Readme/License.
	s2 := &Server{}
	rr2 := httptest.NewRecorder()
	s2.handleGettingStarted(rr2, httptest.NewRequest(http.MethodGet, "/api/getting-started", nil))
	var out2 struct {
		Available bool `json:"available"`
	}
	json.Unmarshal(rr2.Body.Bytes(), &out2)
	if out2.Available {
		t.Error("expected unavailable when no getting-started path set")
	}
}

// TestHandleAPIDoc covers the API.md endpoint the Info → API page renders via
// mdRender, the same way README/getting-started.md do. Deliberately the same
// shape and same test shape as those two: this is one more instance of the
// same "read a doc file from disk fresh, no in-app copy" mechanism, not a
// new one.
func TestHandleAPIDoc(t *testing.T) {
	dir := t.TempDir()
	ap := filepath.Join(dir, "API.md")
	os.WriteFile(ap, []byte("# gravinet HTTP/JSON API\n\nhello world\n"), 0o644)

	s := &Server{}
	s.SetAPIDocPath(ap)
	rr := httptest.NewRecorder()
	s.handleAPIDoc(rr, httptest.NewRequest(http.MethodGet, "/api/api-doc", nil))
	var out struct {
		Text      string `json:"text"`
		Available bool   `json:"available"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if !out.Available || out.Text == "" {
		t.Fatalf("expected available API doc, got available=%v text=%q", out.Available, out.Text)
	}

	// Unset path -> not available, same shape as Readme/License/Getting Started.
	s2 := &Server{}
	rr2 := httptest.NewRecorder()
	s2.handleAPIDoc(rr2, httptest.NewRequest(http.MethodGet, "/api/api-doc", nil))
	var out2 struct {
		Available bool `json:"available"`
	}
	json.Unmarshal(rr2.Body.Bytes(), &out2)
	if out2.Available {
		t.Error("expected unavailable when no API doc path set")
	}
}

func TestHandleLicense(t *testing.T) {
	dir := t.TempDir()
	lp := filepath.Join(dir, "LICENSE")
	os.WriteFile(lp, []byte("GNU GENERAL PUBLIC LICENSE\nVersion 3\n"), 0o644)

	s := &Server{}
	s.SetLicensePath(lp)
	rr := httptest.NewRecorder()
	s.handleLicense(rr, httptest.NewRequest(http.MethodGet, "/api/license", nil))
	var out struct {
		Text      string `json:"text"`
		Available bool   `json:"available"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if !out.Available || out.Text == "" {
		t.Fatalf("expected available license, got available=%v text=%q", out.Available, out.Text)
	}

	// Unset path -> not available.
	s2 := &Server{}
	rr2 := httptest.NewRecorder()
	s2.handleLicense(rr2, httptest.NewRequest(http.MethodGet, "/api/license", nil))
	var out2 struct {
		Available bool `json:"available"`
	}
	json.Unmarshal(rr2.Body.Bytes(), &out2)
	if out2.Available {
		t.Error("expected unavailable when no license path set")
	}
}
