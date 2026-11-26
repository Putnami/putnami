package orchestration

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// countsHint is what a count above its ceiling says: how to change it.
const countsHint = "a count may fall below its ceiling with no edit; a pull request that raises a count raises its ceiling here and says why next to it, in one short comment; tooling/cli/doc/12-profiling-and-telemetry.md says what each count is and how it is lowered"

// ledgerCounts is one ledger (clitest.Counts.Ledger) read back: the processes
// each origin started by kind, and the git calls each made by subcommand.
type ledgerCounts struct {
	engine, engineGit, jobs, jobsGit map[string]int
}

// parseLedger reads a ledger written as "engine <kinds> | engine git <calls> |
// jobs <kinds> | jobs git <calls>", each list "<name> <n>" joined by " · ", or
// "none".
func parseLedger(ledger string) (ledgerCounts, error) {
	parts := strings.Split(ledger, " | ")
	prefixes := []string{"engine ", "engine git ", "jobs ", "jobs git "}
	if len(parts) != len(prefixes) {
		return ledgerCounts{}, fmt.Errorf("ledger %q has %d parts, want %d", ledger, len(parts), len(prefixes))
	}
	maps := make([]map[string]int, len(parts))
	for i, part := range parts {
		list, ok := strings.CutPrefix(part, prefixes[i])
		if !ok {
			return ledgerCounts{}, fmt.Errorf("ledger part %q does not start with %q", part, prefixes[i])
		}
		maps[i] = map[string]int{}
		if list == "none" {
			continue
		}
		for _, entry := range strings.Split(list, " · ") {
			cut := strings.LastIndex(entry, " ")
			if cut < 0 {
				return ledgerCounts{}, fmt.Errorf("ledger entry %q is not \"<name> <n>\"", entry)
			}
			n, err := strconv.Atoi(entry[cut+1:])
			if err != nil {
				return ledgerCounts{}, fmt.Errorf("ledger entry %q is not \"<name> <n>\"", entry)
			}
			if _, twice := maps[i][entry[:cut]]; twice {
				return ledgerCounts{}, fmt.Errorf("ledger part %q names %q twice", part, entry[:cut])
			}
			maps[i][entry[:cut]] = n
		}
	}
	return ledgerCounts{engine: maps[0], engineGit: maps[1], jobs: maps[2], jobsGit: maps[3]}, nil
}

// checkCeilings holds the ledger a run got under its ceiling ledger, by the
// settled decision exact-orchestration-counts-are-tests-of-this-repository:
// every count of got is at most the same count of ceiling, and a count the
// ceiling does not name has a ceiling of 0. A count below its ceiling passes.
func checkCeilings(got, ceiling string) error {
	run, err := parseLedger(got)
	if err != nil {
		return err
	}
	limit, err := parseLedger(ceiling)
	if err != nil {
		return fmt.Errorf("the ceiling: %w", err)
	}
	var above []string
	for _, side := range []struct {
		name       string
		got, limit map[string]int
	}{
		{"engine processes", run.engine, limit.engine},
		{"engine git calls", run.engineGit, limit.engineGit},
		{"jobs processes", run.jobs, limit.jobs},
		{"jobs git calls", run.jobsGit, limit.jobsGit},
	} {
		for name, n := range side.got {
			if n > side.limit[name] {
				above = append(above, fmt.Sprintf("%s %s %d, ceiling %d", side.name, name, n, side.limit[name]))
			}
		}
	}
	if len(above) > 0 {
		slices.Sort(above)
		return fmt.Errorf("above its ceiling: %s", strings.Join(above, "; "))
	}
	return nil
}

// holdCeiling fails t when got rises above ceiling, and logs a ledger below
// it, whose ceiling a later pull request may lower.
func holdCeiling(t *testing.T, name, got, ceiling string) bool {
	t.Helper()
	if err := checkCeilings(got, ceiling); err != nil {
		t.Errorf("%s: %v\n got     %s\n ceiling %s\n%s", name, err, got, ceiling, countsHint)
		return false
	}
	if got != ceiling {
		t.Logf("%s is below its ceiling:\n got     %s\n ceiling %s", name, got, ceiling)
	}
	return true
}

// The ceilings themselves: a ledger at or below its ceiling passes, one count
// above it fails, and so does a count the ceiling does not name.
func TestTheCountCeilings(t *testing.T) {
	ceiling := "engine job 2 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs git 3 | jobs git ls-files 3"
	for _, tc := range []struct {
		got string
		ok  bool
	}{
		{ceiling, true},
		// Lower counts, and a count that falls to none.
		{"engine job 1 · git 9 | engine git ls-files 3 · rev-parse 6 | jobs git 3 | jobs git ls-files 3", true},
		{"engine job 2 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs none | jobs git none", true},
		// A count above its ceiling, on each side.
		{"engine job 3 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs git 3 | jobs git ls-files 3", false},
		{"engine job 2 · git 10 | engine git ls-files 5 · rev-parse 5 | jobs git 3 | jobs git ls-files 3", false},
		{"engine job 2 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs git 4 | jobs git ls-files 3", false},
		{"engine job 2 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs git 3 | jobs git ls-files 4", false},
		// A kind or a subcommand the ceiling does not name.
		{"engine job 2 · control 1 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs git 3 | jobs git ls-files 3", false},
		{"engine job 2 · git 10 | engine git ls-files 3 · rev-parse 6 · show 1 | jobs git 3 | jobs git ls-files 3", false},
		// A ledger that does not parse, or names a count twice.
		{"engine job 2", false},
		{"engine job 2 · job 1 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs git 3 | jobs git ls-files 3", false},
	} {
		if err := checkCeilings(tc.got, ceiling); (err == nil) != tc.ok {
			t.Errorf("checkCeilings(%q) = %v, want ok %v", tc.got, err, tc.ok)
		}
	}
	// A ceiling that does not parse fails whatever the run got.
	for _, bad := range []string{"engine job 2", "engine job 2 · git 10 · git 1 | engine git none | jobs none | jobs git none"} {
		if err := checkCeilings(ceiling, bad); err == nil {
			t.Errorf("checkCeilings against the ceiling %q = nil, want an error", bad)
		}
	}
}
