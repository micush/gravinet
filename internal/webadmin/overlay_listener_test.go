package webadmin

import (
	"crypto/tls"
	"net"
	"net/http"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// freePort returns an available loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

// TestEnsureListenerReachable proves the overlay-management mechanism: a web
// admin bound to one address (loopback, as in the safe default) can be made
// reachable on an additional address via EnsureListener — which is how a node
// becomes reachable for cluster management over its overlay address. Without
// this, a loopback-bound peer returns connection-refused to the proxy and the
// UI shows "no networks".
func TestEnsureListenerReachable(t *testing.T) {
	p1, p2 := freePort(t), freePort(t)
	primary := "127.0.0.1:" + itoa(p1)
	extra := "127.0.0.1:" + itoa(p2)

	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{Enabled: true, Listen: primary, AuthMode: "local", Users: []config.AdminUser{cred}}
	srv := New(wcfg, &stubBackend{}, logx.Default())
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	// Bind a second address, as the daemon does for each overlay address.
	if err := srv.EnsureListener(extra); err != nil {
		t.Fatalf("EnsureListener: %v", err)
	}
	// Idempotent: a second call is a no-op, not an error.
	if err := srv.EnsureListener(extra); err != nil {
		t.Fatalf("EnsureListener (repeat) should be a no-op: %v", err)
	}

	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	ping := func(hostport string) bool {
		// allow the goroutine a moment to begin serving
		for i := 0; i < 20; i++ {
			resp, err := client.Get("https://" + hostport + "/api/ping")
			if err == nil {
				resp.Body.Close()
				return resp.StatusCode == http.StatusOK
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}
	if !ping(primary) {
		t.Error("primary listener not reachable")
	}
	if !ping(extra) {
		t.Error("overlay (extra) listener not reachable — remote management would still fail")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
