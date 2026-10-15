package git

import (
	"bytes"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
)

// CandidatePaths lists the current cut: tracked paths plus non-ignored new
// files, relative to root. Callers inspect the worktree, not the index's bytes;
// an unstaged deletion can therefore still be listed. Git is required: an
// unavailable candidate set must never be mistaken for an empty successful scan.
//
// With prefixes, it lists only the candidates at or below one of them: each is
// a slash path relative to root, matched literally, as a file or a directory.
func CandidatePaths(root string, prefixes ...string) ([]string, error) {
	args := append([]string{"ls-files", "-z", "--cached", "--others", "--exclude-standard"}, literalPathspecs(prefixes)...)
	cmd := exec.Command("git", args...)
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

// TrackedUnder lists the paths the index of the repository holding dir tracks
// at or below rel, a slash path relative to dir. The paths are relative to
// dir, in slash form, sorted and unique. It reads the index alone, so a
// tracked file deleted from the worktree is listed, and rel need not exist.
// When Git does not manage dir, the error is an *UnmanagedError.
func TrackedUnder(dir, rel string) ([]string, error) {
	stdout, stderr, err := runCapture(dir, "ls-files", "-z", "--cached", "--", ":(literal)"+rel)
	if err != nil {
		if unmanaged := Unmanaged(dir); unmanaged != nil {
			return nil, unmanaged
		}
		return nil, fmt.Errorf("git ls-files (in %s): %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	paths := splitNUL(stdout)
	sort.Strings(paths)
	return slices.Compact(paths), nil
}

// literalPathspecs turns slash paths into the pathspec arguments that match
// each one literally, at or below it, or into none when there is no path.
func literalPathspecs(prefixes []string) []string {
	if len(prefixes) == 0 {
		return nil
	}
	args := []string{"--"}
	for _, prefix := range prefixes {
		args = append(args, ":(literal)"+prefix)
	}
	return args
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
// is refused (ADR 0063). With prefixes, it reads only the entries at or below
// one of them, as CandidatePaths lists them.
func ReadIndexModes(root string, prefixes ...string) (IndexModes, error) {
	output, err := run(root, append([]string{"ls-files", "--stage", "-z"}, literalPathspecs(prefixes)...)...)
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
