package git

import (
	"fmt"
	"strconv"
	"strings"
)

// LineCount is the number of lines one path adds and deletes. Binary is set
// when git counts no lines for it; Added and Deleted are then zero. Untracked
// is set for a path git does not track: git has no count for it, so Added and
// Deleted are zero and the caller counts the file itself.
type LineCount struct {
	Added     int
	Deleted   int
	Binary    bool
	Untracked bool
}

// DiffLineCounts returns the lines each tracked path adds and deletes between
// base and the working tree, keyed by workspace-relative slash path. It
// counts committed, staged and unstaged changes together, as DiffWorkingTree
// lists them, and disables rename detection for the same reason. An untracked
// file is in the answer with only Untracked set: git has no numstat for it. A
// tracked path absent from the answer adds and deletes nothing against base.
func DiffLineCounts(repoRoot, base string) (map[string]LineCount, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil, fmt.Errorf("git diff line counts: empty base")
	}
	output, err := run(repoRoot, "diff", "--no-ext-diff", "--no-renames", "--numstat", "-z", base)
	if err != nil {
		return nil, fmt.Errorf("git diff line counts against %q: %w", base, err)
	}
	counts := parseNumstat(output)
	untracked, err := run(repoRoot, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("git list untracked files: %w", err)
	}
	for _, path := range splitNUL(untracked) {
		counts[path] = LineCount{Untracked: true}
	}
	return counts, nil
}

// parseNumstat reads `git diff --numstat -z --no-renames` output: one
// "<added>\t<deleted>\t<path>" record per NUL, with "-" for both counts of a
// binary path.
func parseNumstat(output string) map[string]LineCount {
	counts := make(map[string]LineCount)
	for _, record := range splitNUL(output) {
		fields := strings.SplitN(record, "\t", 3)
		if len(fields) != 3 || fields[2] == "" {
			continue
		}
		if fields[0] == "-" && fields[1] == "-" {
			counts[fields[2]] = LineCount{Binary: true}
			continue
		}
		added, addedErr := strconv.Atoi(fields[0])
		deleted, deletedErr := strconv.Atoi(fields[1])
		if addedErr != nil || deletedErr != nil {
			continue
		}
		counts[fields[2]] = LineCount{Added: added, Deleted: deleted}
	}
	return counts
}
