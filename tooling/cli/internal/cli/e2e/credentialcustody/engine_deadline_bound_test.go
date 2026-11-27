package credentialcustody

import (
	"testing"
	"time"
)

// A run's bound is its ceiling until the test's deadline comes closer than the
// ceiling plus the reserve; then it is what the deadline leaves, never less
// than the floor. Without a deadline it is the ceiling.
func TestARunsBoundComesFromTheTestsDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		deadline    time.Duration
		hasDeadline bool
		want        time.Duration
	}{
		"no deadline":          {0, false, engineDeadline},
		"a distant deadline":   {time.Hour, true, engineDeadline},
		"a closer deadline":    {2 * time.Minute, true, 2*time.Minute - runReserve},
		"a deadline too close": {runReserve, true, runFloor},
		"a deadline passed":    {-time.Minute, true, runFloor},
	} {
		if got := runBound(now.Add(tc.deadline), tc.hasDeadline, now, engineDeadline); got != tc.want {
			t.Errorf("%s: runBound = %s, want %s", name, got, tc.want)
		}
	}
}
