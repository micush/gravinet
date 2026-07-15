package webadmin

import "testing"

// TestParseTopIdlePercent exercises the parser against real-shaped `top -l 1`
// output (and a few edge cases), without needing an actual Mac to run top on
// — see metrics_darwin_parse.go for why this file has no darwin restriction.
func TestParseTopIdlePercent(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   float64
		wantOK bool
	}{
		{
			name: "typical top -l 1 output",
			output: "Processes: 412 total, 3 running, 409 sleeping, 2145 threads \n" +
				"2026/07/04 18:00:00\n" +
				"Load Avg: 2.10, 2.30, 2.45\n" +
				"CPU usage: 8.23% user, 15.38% sys, 76.38% idle\n" +
				"SharedLibs: 512M resident, 88M data, 30M linkedit.\n",
			want:   76.38,
			wantOK: true,
		},
		{
			name:   "idle-only line, no leading whitespace variance",
			output: "CPU usage: 0.0% user, 0.0% sys, 100.0% idle\n",
			want:   100.0,
			wantOK: true,
		},
		{
			name:   "fully busy",
			output: "CPU usage: 92.5% user, 7.5% sys, 0.0% idle\n",
			want:   0.0,
			wantOK: true,
		},
		{
			name:   "no CPU usage line at all",
			output: "Processes: 1 total\nLoad Avg: 0.1, 0.2, 0.3\n",
			want:   0,
			wantOK: false,
		},
		{
			name:   "empty output",
			output: "",
			want:   0,
			wantOK: false,
		},
		{
			name:   "CPU usage line present but malformed (no idle token)",
			output: "CPU usage: something unexpected\n",
			want:   0,
			wantOK: false,
		},
		{
			name:   "CPU usage line with an unparseable percentage",
			output: "CPU usage: NaN% user, 1% sys, oops% idle\n",
			want:   0,
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTopIdlePercent([]byte(tc.output))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("idle%% = %v, want %v", got, tc.want)
			}
		})
	}
}
