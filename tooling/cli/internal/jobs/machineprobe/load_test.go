package machineprobe

import "testing"

func TestParseLoadAverageReadsTheFirstField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want float64
		ok   bool
	}{
		{"3.80 1.19 0.42 2/114 5593\n", 3.80, true},
		{"0.00 0.00 0.00 1/1 1", 0, true},
		{"", 0, false},
		{"-1 0 0", 0, false},
		{"busy 0 0", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseLoadAverage(tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseLoadAverage(%q) = %v, %v; want %v, %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}
