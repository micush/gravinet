package config

import "testing"

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"200M":    200 << 20,
		"1M":      1 << 20,
		"99K":     99 << 10,
		"1G":      1 << 30,
		"2T":      2 << 40,
		"200 MB":  200 << 20,
		"200MiB":  200 << 20,
		"512k":    512 << 10,
		"1048576": 1 << 20, // bare byte count
		"1.5M":    1<<20 + 512<<10,
	}
	for in, want := range ok {
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("ParseSize(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "   ", "abc", "1X", "-5M", "0", "0M", "M", "1.2.3M"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should have errored", bad)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{
		200 << 20: "200M",
		1 << 30:   "1G",
		99 << 10:  "99K",
		1 << 40:   "1T",
		1536:      "1536", // not a clean binary multiple -> raw bytes
		0:         "0",
	}
	for in, want := range cases {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestLogMaxBytesPrecedence(t *testing.T) {
	// Empty config -> 200 MiB default.
	if got := (&Config{}).LogMaxBytes(); got != DefaultLogMaxBytes {
		t.Errorf("empty cfg LogMaxBytes() = %d, want default %d", got, DefaultLogMaxBytes)
	}
	// LogMaxSize wins over the legacy LogMaxMB.
	c := &Config{LogMaxSize: "1G", LogMaxMB: 50}
	if got := c.LogMaxBytes(); got != 1<<30 {
		t.Errorf("LogMaxSize precedence: got %d, want %d", got, 1<<30)
	}
	if !c.LogFIFO() {
		t.Error("LogFIFO() should be true when LogMaxSize is set")
	}
	if c.LogMaxSizeString() != "1G" {
		t.Errorf("LogMaxSizeString() = %q, want 1G", c.LogMaxSizeString())
	}
	// Legacy-only config still honored, and not in FIFO mode.
	l := &Config{LogMaxMB: 50}
	if got := l.LogMaxBytes(); got != 50<<20 {
		t.Errorf("legacy LogMaxMB: got %d, want %d", got, 50<<20)
	}
	if l.LogFIFO() {
		t.Error("LogFIFO() should be false for a legacy LogMaxMB-only config")
	}
	// Floor: a tiny cap is raised to the minimum.
	if got := (&Config{LogMaxSize: "1K"}).LogMaxBytes(); got != minLogMaxBytes {
		t.Errorf("floor not applied: got %d, want %d", got, minLogMaxBytes)
	}
}

func TestValidateRejectsBadLogSize(t *testing.T) {
	if err := (&Config{LogMaxSize: "200M"}).Validate(); err != nil {
		// A well-formed size must not be the thing that fails validation.
		// (Other required fields may still fail; guard specifically.)
		if got := err.Error(); len(got) >= 13 && got[:13] == "log_max_size:" {
			t.Errorf("valid log_max_size rejected: %v", err)
		}
	}
	err := (&Config{LogMaxSize: "banana"}).Validate()
	if err == nil {
		t.Fatal("Validate() should reject an unparseable log_max_size")
	}
}
