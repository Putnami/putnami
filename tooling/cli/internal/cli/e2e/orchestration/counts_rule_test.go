package orchestration

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// ledgerCounts is one ledger (clitest.Counts.Ledger) read back: the processes
// each origin started by kind, and the git calls each made by subcommand.
type ledgerCounts struct {
	engine, engineGit, jobs, jobsGit map[string]int
}

func (l ledgerCounts) total() int {
	total := 0
	for _, counts := range []map[string]int{l.engine, l.jobs} {
		for _, n := range counts {
			total += n
		}
	}
	return total
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
			n, err := strconv.Atoi(entry[cut+1:])
			if cut < 0 || err != nil {
				return ledgerCounts{}, fmt.Errorf("ledger entry %q is not \"<name> <n>\"", entry)
			}
			maps[i][entry[:cut]] = n
		}
	}
	return ledgerCounts{engine: maps[0], engineGit: maps[1], jobs: maps[2], jobsGit: maps[3]}, nil
}

// checkCountsRule holds now against was, by the settled decision
// exact-orchestration-counts-are-tests-of-this-repository: the case's total
// only goes down, and every count only goes down, except the engine's git
// calls, which may rise, once per run, by no more than the jobs' git calls
// fall, in all and for each subcommand.
func checkCountsRule(was, now string) error {
	before, err := parseLedger(was)
	if err != nil {
		return err
	}
	after, err := parseLedger(now)
	if err != nil {
		return err
	}
	if after.total() > before.total() {
		return fmt.Errorf("the total rises from %d to %d", before.total(), after.total())
	}
	for _, side := range []struct {
		name          string
		before, after map[string]int
	}{
		{"engine", before.engine, after.engine},
		{"jobs", before.jobs, after.jobs},
		{"jobs git", before.jobsGit, after.jobsGit},
	} {
		for name, n := range side.after {
			if side.name == "engine" && name == "git" {
				continue
			}
			if n > side.before[name] {
				return fmt.Errorf("%s %s rises from %d to %d", side.name, name, side.before[name], n)
			}
		}
	}
	rise := after.engine["git"] - before.engine["git"]
	fall := before.jobs["git"] - after.jobs["git"]
	if rise > 0 && rise > fall {
		return fmt.Errorf("the engine's git calls rise by %d, more than the jobs' fall by %d", rise, fall)
	}
	// The same holds for each git subcommand: the engine takes over a read of
	// the jobs only by the calls of that read they no longer make. The cut
	// read counts as one group, the listing and the check that decides
	// whether a write under .putnami needs it again (protocols/job ADR 0003).
	group := func(calls map[string]int, name string) int {
		if cutRead[name] {
			total := 0
			for member := range cutRead {
				total += calls[member]
			}
			return total
		}
		return calls[name]
	}
	for name := range after.engineGit {
		rise := group(after.engineGit, name) - group(before.engineGit, name)
		fall := group(before.jobsGit, name) - group(after.jobsGit, name)
		if rise > 0 && rise > fall {
			return fmt.Errorf("the engine's git %s rises by %d, more than the jobs' fall by %d", name, rise, fall)
		}
	}
	return nil
}

// cutRead are the git subcommands of the cut read the engine takes over from
// the jobs: the listing, and the check whether .putnami is excluded.
var cutRead = map[string]bool{"ls-files": true, "check-ignore": true}

// TestTheExtensionCountsOnlyGoDown holds each case's recorded ledger against
// the one before it, so a number that moves the wrong way fails here even
// where the case itself cannot run. TestTheExtensionCounts holds the run to
// the recorded ledger exactly, so no count climbs back towards the one before.
func TestTheExtensionCountsOnlyGoDown(t *testing.T) {
	for _, tc := range extensionCases {
		if tc.was == "" {
			t.Errorf("the %s case records no previous ledger", tc.name)
			continue
		}
		if err := checkCountsRule(tc.was, tc.cold); err != nil {
			t.Errorf("the %s case: %v\n was %s\n now %s\n%s", tc.name, err, tc.was, tc.cold, countsHint)
		}
	}
}

// The rule itself: a total that rises, a count that rises, and an engine rise
// larger than the jobs' fall are refused; an engine rise the jobs' fall pays
// for is kept.
func TestTheCountsRule(t *testing.T) {
	was := "engine job 2 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs git 3 | jobs git ls-files 3"
	for _, tc := range []struct {
		now string
		ok  bool
	}{
		{was, true},
		{"engine job 2 · git 11 | engine git ls-files 5 · rev-parse 6 | jobs git 1 | jobs git ls-files 1", true},
		{"engine job 2 · git 13 | engine git ls-files 7 · rev-parse 6 | jobs git 1 | jobs git ls-files 1", false},
		{"engine job 3 · git 9 | engine git ls-files 3 · rev-parse 6 | jobs git 3 | jobs git ls-files 3", false},
		{"engine job 2 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs git 3 | jobs git ls-files 2 · rev-parse 1", false},
		{"engine job 2 · git 10 | engine git ls-files 4 · rev-parse 6 | jobs none | jobs git none", true},
		{"engine job 2 · git 10 | engine git ls-files 5 · rev-parse 5 | jobs git 3 | jobs git ls-files 3", false},
		{"engine job 2 · git 10 | engine git ls-files 5 · rev-parse 5 | jobs git 2 | jobs git ls-files 2", true},
		{"engine job 2 · git 11 | engine git check-ignore 1 · ls-files 4 · rev-parse 6 | jobs git 2 | jobs git ls-files 2", true},
		{"engine job 2 · git 11 | engine git check-ignore 1 · ls-files 4 · rev-parse 6 | jobs git 3 | jobs git ls-files 3", false},
		{"engine job 2 · git 11 | engine git ls-files 4 · rev-parse 6 · show 1 | jobs git 2 | jobs git ls-files 2", false},
	} {
		if err := checkCountsRule(was, tc.now); (err == nil) != tc.ok {
			t.Errorf("checkCountsRule(%q) = %v, want ok %v", tc.now, err, tc.ok)
		}
	}
}
