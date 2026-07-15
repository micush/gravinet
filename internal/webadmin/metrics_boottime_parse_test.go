package webadmin

import "testing"

func TestParseBoottimeSysctl(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
		ok   bool
	}{
		{"macos-style", "{ sec = 1697000000, usec = 123456 } Wed Oct 11 09:06:40 2023\n", 1697000000, true},
		{"no-trailing-date", "{ sec = 1000, usec = 0 }", 1000, true},
		{"extra-whitespace", "{ sec =    42, usec = 0 }", 42, true},
		{"empty", "", 0, false},
		{"garbage", "not a boottime\n", 0, false},
		{"bare-integer-not-matched", "1697000000\n", 0, false}, // no "sec =" — this parser only, not the fallback
	}
	for _, c := range cases {
		got, ok := parseBoottimeSysctl([]byte(c.in))
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestParseBareEpochSysctl(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
		ok   bool
	}{
		{"plain", "1697000000\n", 1697000000, true},
		{"whitespace-padded", "  42  ", 42, true},
		{"struct-form-not-matched", "{ sec = 1000, usec = 0 }", 0, false}, // not this parser's job
		{"empty", "", 0, false},
	}
	for _, c := range cases {
		got, ok := parseBareEpochSysctl([]byte(c.in))
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
