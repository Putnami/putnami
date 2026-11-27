package git

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/gitread"
)

// CandidatePaths lists the current cut: tracked paths plus non-ignored new
// files, relative to root. Callers inspect the worktree, not the index's bytes;
// an unstaged deletion can therefore still be listed. Git is required: an
// unavailable candidate set must never be mistaken for an empty successful scan.
//
// With prefixes, it lists only the candidates at or below one of them: each is
// a slash path relative to root, matched literally, as a file or a directory.
//
// The listing is read in-process (inprocess_*.go) when the repository's state is
// one the reader supports, and from Git otherwise (tooling/cli ADR 0112).
func CandidatePaths(root string, prefixes ...string) ([]string, error) {
	if paths, err := candidatePathsInProcess(root, prefixes); !inProcessUnsupported(err) {
		return paths, err
	}
	return CandidatePathsFromGit(root, prefixes...)
}

// candidatePathsInProcess is CandidatePaths read in-process: the index's
// paths and the untracked ones, a nested repository as its directory with a
// trailing slash, as ls-files lists it.
func candidatePathsInProcess(root string, prefixes []string) ([]string, error) {
	repo, err := openLocal(root)
	if err != nil {
		return nil, err
	}
	cut, err := repo.readCut(prefixes...)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(cut.Index.Entries)+len(cut.Untracked))
	for _, entry := range cut.Index.Entries {
		paths = append(paths, entry.Path)
	}
	for _, untracked := range cut.Untracked {
		paths = append(paths, untracked.Listed())
	}
	if len(paths) == 0 {
		return nil, nil
	}
	sort.Strings(paths)
	return slices.Compact(paths), nil
}

// CandidatePathsFromGit is CandidatePaths read from Git alone, with the
// environment of every Git read of the cut (gitread.RepositoryEnvironment):
// the in-process reader reads the configuration Git reads there, so a bound
// listing is the same whichever of the two answers (tooling/cli ADR 0112).
func CandidatePathsFromGit(root string, prefixes ...string) ([]string, error) {
	args := append([]string{"ls-files", "-z", "--cached", "--others", "--exclude-standard"}, literalPathspecs(prefixes)...)
	stdout, stderr, err := gitread.RunCapture(root, args...)
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(stderr))
	}
	data := []byte(stdout)
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

// TrackedPaths lists every path the index of the repository holding dir
// tracks at or below dir, relative to dir, in slash form, sorted and unique. It
// reads the index alone, so a tracked file deleted from the worktree is
// listed, and an untracked file is not. TrackedPathsUnder reads the paths
// below one path from the list.
// When Git does not manage dir, the error is an *UnmanagedError.
func TrackedPaths(dir string) ([]string, error) {
	stdout, stderr, err := gitread.RunCapture(dir, "ls-files", "-z", "--cached")
	if err != nil {
		if unmanaged := gitread.Unmanaged(dir); unmanaged != nil {
			return nil, unmanaged
		}
		return nil, fmt.Errorf("git ls-files (in %s): %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	paths := gitread.SplitNUL(stdout)
	sort.Strings(paths)
	return slices.Compact(paths), nil
}

// TrackedPathsUnder returns the paths of tracked, a TrackedPaths list, at or
// below rel, a slash path relative to the same directory; rel need not exist.
// It folds case, as the emptying does: on a case-insensitive filesystem the
// index can spell a path another way than rel, and that path is listed too.
func TrackedPathsUnder(tracked []string, rel string) []string {
	var under []string
	for _, path := range tracked {
		if len(path) == len(rel) && strings.EqualFold(path, rel) ||
			len(path) > len(rel) && path[len(rel)] == '/' && strings.EqualFold(path[:len(rel)], rel) {
			under = append(under, path)
		}
	}
	return under
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
// one of them, as CandidatePaths lists them. It reads the index in-process
// when it can, as CandidatePaths does.
func ReadIndexModes(root string, prefixes ...string) (IndexModes, error) {
	if index, err := indexModesInProcess(root, prefixes); !inProcessUnsupported(err) {
		return index, err
	}
	return ReadIndexModesFromGit(root, prefixes...)
}

// indexModesInProcess is ReadIndexModes read in-process.
func indexModesInProcess(root string, prefixes []string) (IndexModes, error) {
	repo, err := openLocal(root)
	if err != nil {
		return IndexModes{}, err
	}
	entries, err := repo.index()
	if err != nil {
		return IndexModes{}, err
	}
	index := IndexModes{Modes: map[string]string{}, Unmerged: map[string]bool{}}
	for _, entry := range entries.Entries {
		if len(prefixes) > 0 && !underPrefixes(entry.Path, prefixes) {
			continue
		}
		if entry.Stage != 0 {
			index.Unmerged[entry.Path] = true
			continue
		}
		index.Modes[entry.Path] = entry.ModeString()
	}
	return index, nil
}

// ReadIndexModesFromGit is ReadIndexModes read from Git alone.
func ReadIndexModesFromGit(root string, prefixes ...string) (IndexModes, error) {
	output, err := gitread.Run(root, append([]string{"ls-files", "--stage", "-z"}, literalPathspecs(prefixes)...)...)
	if err != nil {
		return IndexModes{}, fmt.Errorf("read the index modes: %w", err)
	}
	index := IndexModes{Modes: map[string]string{}, Unmerged: map[string]bool{}}
	for _, entry := range gitread.SplitNUL(output) {
		mode, _, stage, path, ok := gitread.ParseStageEntry(entry)
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

// WorkTreeRoot is what `git rev-parse --show-toplevel` answers in dir: the
// root of the work tree that holds it. It is read in-process when the
// repository's layout is one the reader supports (tooling/cli ADR 0112), and
// from Git otherwise.
func WorkTreeRoot(dir string) (string, error) {
	if root, err := workTreeRootInProcess(dir); !inProcessUnsupported(err) {
		return root, err
	}
	return WorkTreeRootFromGit(dir)
}

// WorkTreeRootFromGit is WorkTreeRoot read from Git alone, with the
// environment of the in-process reader (gitread.RepositoryEnvironment).
func WorkTreeRootFromGit(dir string) (string, error) {
	out, stderr, err := gitread.RunCapture(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}
	return strings.TrimSuffix(out, "\n"), nil
}
