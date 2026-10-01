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
