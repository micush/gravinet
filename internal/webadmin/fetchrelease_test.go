package webadmin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gravinet/internal/mesh"
)

// fakeGitHub serves the API and the archive host; latest == "" means no published release (so the tag list is used).
func fakeGitHub(t *testing.T, latest string, tags []string, archives map[string][]byte) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+ghRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if latest == "" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"tag_name":"` + latest + `"}`))
	})
	mux.HandleFunc("/repos/"+ghRepo+"/tags", func(w http.ResponseWriter, r *http.Request) {
		var parts []string
		for _, n := range tags {
			parts = append(parts, `{"name":"`+n+`"}`)
		}
		w.Write([]byte("[" + strings.Join(parts, ",") + "]"))
	})
	mux.HandleFunc("/"+ghRepo+"/archive/refs/tags/", func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"+ghRepo+"/archive/refs/tags/"), ".tar.gz")
		b, ok := archives[tag]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	srv := httptest.NewServer(mux)
	oa, oh := ghAPI, ghHost
	ghAPI, ghHost = srv.URL, srv.URL
	t.Cleanup(func() { ghAPI, ghHost = oa, oh; srv.Close() })
}

// githubSource is a minimal gravinet source tree the way GitHub packs a tag: one gravinet-<tag>/ top directory.
func githubSource(t *testing.T, module, ver string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: "gravinet-v" + ver + "/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	add("go.mod", "module "+module+"\n\ngo 1.22\n")
	add("cmd/gravinet/main.go", "package main\n\nvar (\n\tversion = \""+ver+"\"\n)\n")
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestNumericTagLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{{"v9", "v10", true}, {"v10", "v9", false}, {"v1019", "v1019", false}, {"1.2", "1.10", true}} {
		if numericTagLess(c.a, c.b) != c.less {
			t.Errorf("%s < %s should be %v", c.a, c.b, c.less)
		}
	}
}

func TestCandidateTagsReleaseFirstThenHighestDown(t *testing.T) {
	fakeGitHub(t, "v1020", []string{"v9", "v1020", "junk", "v1019", "v10"}, nil)
	got, err := candidateTags(context.Background())
	if err != nil || strings.Join(got, " ") != "v1020 v1019 v10 v9" {
		t.Errorf("with a release: %v %v", got, err)
	}
	// a stale published release must not hide newer tags
	fakeGitHub(t, "v1019", []string{"v1019", "v1021", "v1020"}, nil)
	got, err = candidateTags(context.Background())
	if err != nil || strings.Join(got, " ") != "v1021 v1020 v1019" {
		t.Errorf("stale release: %v %v", got, err)
	}
	fakeGitHub(t, "", []string{"v9", "junk", "v10", "v2"}, nil)
	got, err = candidateTags(context.Background())
	if err != nil || strings.Join(got, " ") != "v10 v9 v2" {
		t.Errorf("tags only: %v %v", got, err)
	}
	fakeGitHub(t, "", []string{"junk"}, nil)
	if _, err = candidateTags(context.Background()); err == nil {
		t.Error("no usable tag must be an error")
	}
}

func TestFetchReleaseSpoolsAndSkipsForeignTags(t *testing.T) {
	dir := t.TempDir()
	fakeGitHub(t, "", []string{"v304", "v1020"}, map[string][]byte{
		"v304":  []byte("not an archive at all"),
		"v1020": githubSource(t, "gravinet", "1020"),
	})
	path, sum, tag, err := fetchRelease(context.Background(), dir)
	if err != nil || tag != "v1020" || sum == "" {
		t.Fatalf("fetch: %q %q %v", path, tag, err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, githubSource(t, "gravinet", "1020")) || filepath.Dir(path) != dir {
		t.Error("the spooled bytes are not the downloaded archive, or not under the state directory")
	}
	os.Remove(path)
	if left, _ := filepath.Glob(filepath.Join(dir, ".upload-*")); len(left) != 0 {
		t.Errorf("a skipped tag left its spool file behind: %v", left)
	}

	fakeGitHub(t, "", []string{"v304"}, map[string][]byte{"v304": []byte("junk")})
	if _, _, _, err := fetchRelease(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "none of the tags") {
		t.Errorf("only foreign tags: %v", err)
	}
	fakeGitHub(t, "v5", nil, nil) // release names a tag with no archive
	if _, _, _, err := fetchRelease(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "download") {
		t.Errorf("missing archive: %v", err)
	}
}

// onlineServer is a local-login web admin whose apply op records what it was asked to build.
func onlineServer(t *testing.T, be *stubBackend) (ts *httptest.Server, cookie *http.Cookie, srcPath func() string) {
	t.Helper()
	var mu sync.Mutex
	var got string
	srv := secServer(be)
	srv.upg = &UpgradeCtl{
		StateDir: t.TempDir(),
		Op: func(op string, body []byte) ([]byte, error) {
			var m struct {
				SrcPath string `json:"src_path"`
			}
			json.Unmarshal(body, &m)
			if op == "apply" {
				mu.Lock()
				got = m.SrcPath
				if b, err := os.ReadFile(m.SrcPath); err == nil {
					got = string(b)
				}
				mu.Unlock()
			}
			return []byte(`{"ok":true,"applied":"1020"}`), nil
		},
	}
	ts = httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, sessionFor(t, ts), func() string { mu.Lock(); defer mu.Unlock(); return got }
}

func TestSourceOnlineFetchesThenApplies(t *testing.T) {
	ts, cookie, built := onlineServer(t, &stubBackend{})
	arc := githubSource(t, "gravinet", "1020")
	fakeGitHub(t, "v1020", nil, map[string][]byte{"v1020": arc})
	req, _ := http.NewRequest("POST", ts.URL+"/api/upgrade/source?online=1", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || built() != string(arc) {
		t.Fatalf("online source: %d %s; apply saw %d bytes", resp.StatusCode, b, len(built()))
	}
	// GitHub unreachable or tag without archive: 422, nothing applied
	fakeGitHub(t, "v1021", nil, nil)
	req, _ = http.NewRequest("POST", ts.URL+"/api/upgrade/source?online=1", nil)
	req.AddCookie(cookie)
	resp, _ = http.DefaultClient.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 422 || !strings.Contains(string(b), "download") {
		t.Errorf("failure: %d %s", resp.StatusCode, b)
	}
}

func TestPushOnlineFetchesOnceAndPushes(t *testing.T) {
	rel := make(chan struct{})
	close(rel)
	var gotBytes int
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		gotBytes = int(n)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "1020", "restarting": true})
	}))
	defer peer.Close()
	be := &stubBackend{overlayAddr: netip.MustParseAddr("127.0.0.1")}
	be.managedPeers = []mesh.ManagedPeer{{NodeID: "p1", Overlay4: netip.MustParseAddr("127.0.0.1"), WebPort: portOf(t, peer.URL), LastSeen: time.Now()}}
	ts, cookie, _ := onlineServer(t, be)
	fakeGitHub(t, "v1020", nil, map[string][]byte{"v1020": githubSource(t, "gravinet", "1020")})

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	nf, _ := mw.CreateFormField("nodes")
	nf.Write([]byte(`["p1"]`))
	of, _ := mw.CreateFormField("online")
	of.Write([]byte("1"))
	mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/upgrade/push", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"node":"p1"`) || !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("push online: %d %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"info":"Downloaded v1020 from GitHub (gravinet 1020)."`) {
		t.Errorf("the stream does not say which release was downloaded: %s", b)
	}
	if gotBytes < 100 {
		t.Errorf("the peer received %d bytes; the fetched archive was not pushed", gotBytes)
	}
}
