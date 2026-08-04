package webadmin

import (
	"net/netip"
	"reflect"
	"testing"

	"gravinet/internal/config"
)

// validateListenAddrs guards the one irreversible mistake this picker can
// make. Everything else it does is recoverable from the browser you are
// already using; saving an empty set is not, because it removes the interface
// you would fix it from.
func TestValidateListenAddrsRejectsEmpty(t *testing.T) {
	for _, in := range [][]string{nil, {}, {""}, {"  ", "\t"}} {
		if _, err := validateListenAddrs(in); err == nil {
			t.Errorf("validateListenAddrs(%q) = nil error; an empty set leaves the admin interface bound to nothing", in)
		}
	}
}

// Names are refused rather than resolved: this binds a socket, and a name that
// resolved differently later would silently move what the admin interface is
// exposed on.
func TestValidateListenAddrsRejectsNames(t *testing.T) {
	if _, err := validateListenAddrs([]string{"localhost"}); err == nil {
		t.Error("accepted a hostname; only IP literals may be bound")
	}
	if _, err := validateListenAddrs([]string{"fe80::1"}); err == nil {
		t.Error("accepted a link-local address, which needs a zone to bind")
	}
}

func TestValidateListenAddrsNormalizes(t *testing.T) {
	got, err := validateListenAddrs([]string{" 127.0.0.1 ", "127.0.0.1", "::ffff:10.0.0.1", "fd00:203::157"})
	if err != nil {
		t.Fatalf("validateListenAddrs: %v", err)
	}
	want := []string{"127.0.0.1", "10.0.0.1", "fd00:203::157"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q (trimmed, deduped, v4-mapped unwrapped)", got, want)
	}
}

// Loopback leads when it was picked: it is the one address that cannot stop
// existing underneath the daemon, so it is the safest primary bind.
func TestPrimaryOfPrefersLoopback(t *testing.T) {
	if got := primaryOf([]string{"192.168.5.108", "127.0.0.1", "fd00:203::157"}); got != "127.0.0.1" {
		t.Fatalf("primaryOf = %q, want 127.0.0.1", got)
	}
	if got := primaryOf([]string{"192.168.5.108", "fd00:203::157"}); got != "192.168.5.108" {
		t.Fatalf("primaryOf = %q, want the first pick when loopback was deselected", got)
	}
}

// An unconfigured node must keep doing exactly what it does today: loopback
// from Listen, plus the overlay addresses the cluster-management binder adds.
// That is what lets this ship without a migration.
func TestWebAdminListenSetDefaultIsTodaysBehaviour(t *testing.T) {
	c := &config.Config{}
	c.WebAdmin.Listen = "127.0.0.1:8443"
	mesh := mustAddrs(t, "192.168.203.157", "fd00:203::157")

	got := c.WebAdminListenSet(mesh)
	want := []string{"127.0.0.1:8443", "192.168.203.157:8443", "[fd00:203::157]:8443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Once a set is configured it is the whole answer — nothing is added back
// behind the operator, including the mesh addresses that make up the default.
func TestWebAdminListenSetConfiguredIsExhaustive(t *testing.T) {
	c := &config.Config{}
	c.WebAdmin.Listen = "127.0.0.1:8443"
	c.WebAdmin.ListenAddrs = []string{"192.168.5.108"}

	got := c.WebAdminListenSet(mustAddrs(t, "192.168.203.157", "fd00:203::157"))
	want := []string{"192.168.5.108:8443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q — a configured pick list must not gain addresses", got, want)
	}
}

func TestWebAdminListenSetPutsLoopbackFirst(t *testing.T) {
	c := &config.Config{}
	c.WebAdmin.Listen = "0.0.0.0:8443"
	c.WebAdmin.ListenAddrs = []string{"192.168.5.108", "127.0.0.1"}

	got := c.WebAdminListenSet(nil)
	if len(got) == 0 || got[0] != "127.0.0.1:8443" {
		t.Fatalf("got %q, want loopback bound first", got)
	}
}

func mustAddrs(t *testing.T, in ...string) []netip.Addr {
	t.Helper()
	var out []netip.Addr
	for _, s := range in {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("bad test address %q: %v", s, err)
		}
		out = append(out, a)
	}
	return out
}
