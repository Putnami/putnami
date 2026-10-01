package cli

import "testing"

// TestExitCodeValues pins the exit-code taxonomy. These values are a public
// contract branched on by scripts, CI, and agents; a change here is a breaking
// change and must be deliberate.
func TestExitCodeValues(t *testing.T) {
	cases := map[string]struct {
		got, want int
	}{
		"Success": {ExitSuccess, 0},
		"Failure": {ExitFailure, 1},
		"Usage":   {ExitUsage, 2},
		"Auth":    {ExitAuth, 3},
		"API":     {ExitAPI, 4},
		"Signal":  {ExitSignal, 130},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("Exit%s = %d, want %d", name, c.got, c.want)
		}
	}
}
