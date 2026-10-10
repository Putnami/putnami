package git

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
)

// CandidatePaths lists the current cut: tracked paths plus non-ignored new
// files, relative to root. Callers inspect the worktree, not the index's bytes;
// an unstaged deletion can therefore still be listed. Git is required: an
// unavailable candidate set must never be mistaken for an empty successful scan.
func CandidatePaths(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("git ls-files did not terminate its NUL-delimited output")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			return nil, fmt.Errorf("git ls-files returned an empty candidate path")
		}
		paths = append(paths, string(part))
	}
	sort.Strings(paths)
	unique := paths[:0]
	for _, path := range paths {
		if len(unique) == 0 || unique[len(unique)-1] != path {
			unique = append(unique, path)
		}
	}
	return unique, nil
}

// IndexModes is what the index records for each tracked path of a repository:
// the mode of each merged path, and the paths that are unmerged, which have no
// single mode.
type IndexModes struct {
	Modes    map[string]string
	Unmerged map[string]bool
}

// ReadIndexModes reads the index of the repository at root once, as a source
// binding reads it (ReadSourceBindingSnapshot). A `git:` cache key needs it for
// two answers a binding gives: on a host that does not store the executable
// bit, a tracked file takes its bit from the index mode, and an unmerged path
// is refused (ADR 0061).
func ReadIndexModes(root string) (IndexModes, error) {
	output, err := run(root, "ls-files", "--stage", "-z")
	if err != nil {
		return IndexModes{}, fmt.Errorf("read the index modes: %w", err)
	}
	index := IndexModes{Modes: map[string]string{}, Unmerged: map[string]bool{}}
	for _, entry := range splitNUL(output) {
		mode, _, stage, path, ok := parseStageEntry(entry)
		if !ok {
			return IndexModes{}, fmt.Errorf("read the index modes: malformed ls-files stage entry")
		}
		if stage != "0" {
			index.Unmerged[path] = true
			continue
		}
		index.Modes[path] = mode
	}
	return index, nil
}
