package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"gravinet/internal/logx"
)

func TestHandleLogsDownload(t *testing.T) {
	dir := t.TempDir()
	lp := filepath.Join(dir, "gravinet.log")
	content := "2026/06/18 00:00:00 [INFO] one\n2026/06/18 00:00:01 [ERROR] two\n"
	os.WriteFile(lp, []byte(content), 0o644)

	s := &Server{logPath: lp}
	rr := httptest.NewRecorder()
	s.handleLogs(rr, httptest.NewRequest(http.MethodGet, "/api/logs?download=1", nil))
	var out struct {
		Text    string `json:"text"`
		Enabled bool   `json:"enabled"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if !out.Enabled || out.Text != content {
		t.Fatalf("download should return the whole file; enabled=%v text=%q", out.Enabled, out.Text)
	}
}

func TestHandleLogsClear(t *testing.T) {
	dir := t.TempDir()
	lp := filepath.Join(dir, "gravinet.log")
	rf, err := logx.NewRotatingFile(lp, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	rf.Write([]byte("some log lines\nmore lines\n"))

	s := &Server{logPath: lp, logClear: rf.Truncate}
	rr := httptest.NewRecorder()
	s.handleLogsClear(rr, httptest.NewRequest(http.MethodPost, "/api/logs/clear", nil))
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if !out.OK {
		t.Fatalf("clear should succeed, got ok=%v error=%q", out.OK, out.Error)
	}
	if fi, _ := os.Stat(lp); fi.Size() != 0 {
		t.Fatalf("log file should be empty after clear, size=%d", fi.Size())
	}

	// Disabled logging: clear is a no-op reporting why.
	s2 := &Server{}
	rr2 := httptest.NewRecorder()
	s2.handleLogsClear(rr2, httptest.NewRequest(http.MethodPost, "/api/logs/clear", nil))
	var out2 struct {
		OK bool `json:"ok"`
	}
	json.Unmarshal(rr2.Body.Bytes(), &out2)
	if out2.OK {
		t.Error("clear should report not-ok when logging is disabled")
	}

	// GET is rejected.
	rr3 := httptest.NewRecorder()
	s.handleLogsClear(rr3, httptest.NewRequest(http.MethodGet, "/api/logs/clear", nil))
	if rr3.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET clear should be 405, got %d", rr3.Code)
	}
}
