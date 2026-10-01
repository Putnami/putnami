package runnersource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	runner "go.putnami.dev/protocol/runner"
)

// Snapshot retains content identity separately from Git and index context.
// It is source evidence, not an executable request: the existing HEAD-bound
// tree identity, Git objects and expected-plan admission are still required.
type Snapshot struct {
	Manifest    runner.SourceManifest
	Digest      string
	Git         runner.GitContext
	IndexDigest string
}

// Capture reads working-tree bytes, including unstaged edits and non-ignored
// new files. It never reads staged blobs as execution source or copies .git.
// Two complete scans and Git/index checks reject observable capture races.
// Submodules and sparse/unmerged indexes are explicitly unsupported here.
//
// bound names the git-ignored paths the admission layer bound as required
// task inputs, as workspace-relative slash paths. Each is captured
// with its manifest entry flagged bound, takes part in both scans and in the
// race check exactly as every other entry, and must be a regular file or a
// symlink that Git reports ignored: a bound path Git does not ignore, or that
// is absent or special, is an error, never a silent demotion. A directory is
// never bound; paths only.
func (s *Store) Capture(ctx context.Context, repo string, bound []string) (Snapshot, error) {
	return s.capture(ctx, repo, bound, nil)
}

// betweenScans is only a deterministic race-test seam. It is never supplied by
// production callers and is not a process-global hook.
func (s *Store) capture(ctx context.Context, repo string, bound []string, betweenScans func()) (Snapshot, error) {
	top, err := sourceGit(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return Snapshot{}, err
	}
	realRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return Snapshot{}, err
	}
	realTop, err := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	if err != nil || realRepo != realTop {
		return Snapshot{}, fmt.Errorf("source capture requires the repository root")
	}
	root, err := os.OpenRoot(realRepo)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = root.Close() }()
	gitContext, status, err := sourceGitContext(ctx, realRepo)
	if err != nil {
		return Snapshot{}, err
	}
	boundPaths, err := boundPathSet(bound)
	if err != nil {
		return Snapshot{}, err
	}
	first, firstIndex, err := s.scan(ctx, realRepo, root, boundPaths)
	if err != nil {
		return Snapshot{}, err
	}
	if betweenScans != nil {
		betweenScans()
	}
	second, secondIndex, err := s.scan(ctx, realRepo, root, boundPaths)
	if err != nil {
		return Snapshot{}, err
	}
	endGitContext, endStatus, err := sourceGitContext(ctx, realRepo)
	if err != nil {
		return Snapshot{}, err
	}
	if gitContext != endGitContext || !bytes.Equal(status, endStatus) || !bytes.Equal(firstIndex, secondIndex) || !reflect.DeepEqual(first, second) {
		return Snapshot{}, fmt.Errorf("source changed during capture; retry after edits finish")
	}
	digest, err := runner.SourceDigest(first)
	if err != nil {
		return Snapshot{}, err
	}
	indexHash := sha256.Sum256(firstIndex)
	return Snapshot{
		Manifest: first, Digest: digest, Git: gitContext,
		IndexDigest: hex.EncodeToString(indexHash[:]),
	}, nil
}

func sourceGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	output, found, err := sourceGitOptional(ctx, repo, args...)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("source git %s: object not found", args[0])
	}
	return output, nil
}

// sourceGitOptional treats Git's documented quiet "not found" exit as data.
// Other exits remain failures, without rendering stderr that may contain a
// credential-bearing URL from local configuration.
func sourceGitOptional(ctx context.Context, repo string, args ...string) ([]byte, bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo, "-c", "core.fsmonitor=false"}, args...)...)
	var output boundedGitOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("source git %s: %w", args[0], err)
	}
	return output.buffer.Bytes(), true, nil
}

type boundedGitOutput struct{ buffer bytes.Buffer }

func (b *boundedGitOutput) Write(data []byte) (int, error) {
	if b.buffer.Len()+len(data) > runner.MaxManifestBytes {
		return 0, fmt.Errorf("source Git metadata exceeds protocol limit")
	}
	return b.buffer.Write(data)
}

func sourceGitContext(ctx context.Context, repo string) (runner.GitContext, []byte, error) {
	branch, attached, err := sourceGitOptional(ctx, repo, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return runner.GitContext{}, nil, err
	}
	head, hasHead, err := sourceGitOptional(ctx, repo, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return runner.GitContext{}, nil, err
	}
	if !hasHead && !attached {
		return runner.GitContext{}, nil, fmt.Errorf("source Git HEAD is neither a commit nor an initial branch")
	}
	status, err := sourceGit(ctx, repo, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return runner.GitContext{}, nil, err
	}
	name := strings.TrimSpace(string(branch))
	gitContext := runner.GitContext{Head: strings.TrimSpace(string(head)), Branch: name, Dirty: len(status) != 0}
	return gitContext, status, runner.ValidateGitContext(gitContext)
}

// boundPathSet validates the admitted bound paths once, before any scan.
func boundPathSet(bound []string) (map[string]bool, error) {
	set := make(map[string]bool, len(bound))
	for _, name := range bound {
		if err := runner.ValidateSourcePath(name); err != nil {
			return nil, fmt.Errorf("bound input %q: %w", name, err)
		}
		set[name] = true
	}
	return set, nil
}

// IgnoredPaths asks Git which of the given workspace-relative paths it
// ignores in the repository's worktree, through `git check-ignore`. A tracked
// path is never reported, whatever the ignore rules say, which is exactly the
// question the admission asks: "would a fresh clone lack this file?".
func IgnoredPaths(ctx context.Context, repo string, paths []string) (map[string]bool, error) {
	ignored := make(map[string]bool, len(paths))
	if len(paths) == 0 {
		return ignored, nil
	}
	var stdin bytes.Buffer
	for _, name := range paths {
		stdin.WriteString(name)
		stdin.WriteByte(0)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "-c", "core.fsmonitor=false", "check-ignore", "-z", "--stdin")
	cmd.Stdin = &stdin
	var output boundedGitOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		var exitError *exec.ExitError
		// Exit 1 is Git's documented "none of the paths are ignored".
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return ignored, nil
		}
		return nil, fmt.Errorf("source git check-ignore: %w", err)
	}
	for _, name := range bytes.Split(output.buffer.Bytes(), []byte{0}) {
		if len(name) != 0 {
			ignored[string(name)] = true
		}
	}
	return ignored, nil
}

func (s *Store) scan(ctx context.Context, repo string, root *os.Root, bound map[string]bool) (runner.SourceManifest, []byte, error) {
	manifest := runner.SourceManifest{Version: runner.SourceManifestVersion, Entries: []runner.SourceEntry{}}
	if err := rejectSpecialSource(ctx, repo, root); err != nil {
		return manifest, nil, err
	}
	// A bound path is admitted into the scan only while Git ignores it: the
	// admission decided on that fact, and a path that became tracked or lost
	// its ignore rule since is a different source, not a bound one.
	if err := verifyBoundIgnored(ctx, repo, bound); err != nil {
		return manifest, nil, err
	}
	index, err := sourceGit(ctx, repo, "ls-files", "--stage", "-z")
	if err != nil {
		return manifest, nil, err
	}
	// -v exposes skip-worktree and assume-unchanged entries. Those can hide
	// missing input or dirty bytes from the Git context, so fail explicitly.
	flags, err := sourceGit(ctx, repo, "ls-files", "-v", "-z")
	if err != nil {
		return manifest, nil, err
	}
	for _, row := range bytes.Split(flags, []byte{0}) {
		if len(row) > 0 && row[0] != 'H' {
			return manifest, nil, fmt.Errorf("source capture does not support sparse, assume-unchanged, or unmerged index entries")
		}
	}
	paths := map[string]bool{}
	// modes keeps each tracked path's index mode, which is where Windows keeps
	// the executable bit its filesystem does not have.
	modes := map[string]string{}
	for _, row := range bytes.Split(index, []byte{0}) {
		if len(row) == 0 {
			continue
		}
		header, name, ok := strings.Cut(string(row), "\t")
		parts := strings.Fields(header)
		if !ok || len(parts) != 3 || parts[2] != "0" {
			return manifest, nil, fmt.Errorf("source capture does not support an unmerged index")
		}
		if parts[0] == "160000" {
			return manifest, nil, fmt.Errorf("source capture does not yet support submodule %q", name)
		}
		paths[name] = true
		modes[name] = parts[0]
	}
	untracked, err := sourceGit(ctx, repo, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return manifest, nil, err
	}
	for _, name := range bytes.Split(untracked, []byte{0}) {
		if len(name) != 0 {
			paths[string(name)] = false
		}
	}
	for name := range bound {
		paths[name] = false
	}
	if len(paths) > runner.MaxSourceEntries {
		return manifest, nil, fmt.Errorf("source exceeds protocol entry limit")
	}
	ordered := make([]string, 0, len(paths))
	for name := range paths {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	var total int64
	for _, name := range ordered {
		if err := ctx.Err(); err != nil {
			return manifest, nil, err
		}
		entry, exists, err := s.captureEntry(root, name, paths[name], modes[name])
		if err != nil {
			return manifest, nil, fmt.Errorf("capture %q: %w", name, err)
		}
		// rejectSpecialSource skipped the ignored tree, so the bound path's
		// kind was checked by captureEntry alone: a regular file or a symlink,
		// nothing else, and only for the exact path that was bound.
		entry.Bound = bound[name]
		if exists {
			total += entry.Size
			if total > runner.MaxSourceTotalBytes {
				return manifest, nil, fmt.Errorf("source exceeds protocol total byte limit")
			}
			manifest.Entries = append(manifest.Entries, entry)
		}
	}
	return manifest, index, runner.ValidateSourceManifest(manifest)
}

// verifyBoundIgnored refuses a bound path Git does not report ignored.
func verifyBoundIgnored(ctx context.Context, repo string, bound map[string]bool) error {
	if len(bound) == 0 {
		return nil
	}
	names := make([]string, 0, len(bound))
	for name := range bound {
		names = append(names, name)
	}
	sort.Strings(names)
	ignored, err := IgnoredPaths(ctx, repo, names)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !ignored[name] {
			return fmt.Errorf("bound input %q is not ignored by Git; only an ignored path can be bound", name)
		}
	}
	return nil
}

// Git deliberately omits untracked FIFOs and sockets from ls-files. Inspect
// the non-ignored tree as well so omission cannot silently admit ambient source.
func rejectSpecialSource(ctx context.Context, repo string, root *os.Root) error {
	ignored, err := sourceGit(ctx, repo, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return err
	}
	excluded := map[string]bool{}
	for _, name := range bytes.Split(ignored, []byte{0}) {
		excluded[strings.TrimSuffix(string(name), "/")] = true
	}
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name() == ".git" || excluded[name] {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && !entry.Type().IsRegular() && entry.Type()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported source object %q: expected a regular file or symlink", name)
		}
		return nil
	})
}

func (s *Store) captureEntry(root *os.Root, name string, tracked bool, indexMode string) (runner.SourceEntry, bool, error) {
	entry := runner.SourceEntry{Path: name}
	if err := runner.ValidateSourcePath(name); err != nil {
		return entry, false, err
	}
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && tracked {
		return entry, false, nil // A tracked deletion is absent from the full tree.
	}
	if err != nil {
		return entry, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		entry.Kind = "symlink"
		// Windows stores a relative link target with backslashes. The
		// manifest spells targets in slash form, and its validation still
		// refuses a target that escapes the source.
		var target string
		target, err = root.Readlink(name)
		entry.Target = filepath.ToSlash(target)
		return entry, err == nil, err
	}
	if !info.Mode().IsRegular() {
		return entry, false, fmt.Errorf("expected a regular file or symlink")
	}
	entry.Kind, entry.Mode, entry.Size = "file", "0644", info.Size()
	if executableSource(info, indexMode) {
		entry.Mode = "0755"
	}
	file, err := root.OpenFile(name, sourceReadFlags, 0)
	if err != nil {
		return entry, false, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return entry, false, fmt.Errorf("source changed while opening")
	}
	entry.Digest, err = s.put(file, entry.Size)
	return entry, err == nil, err
}
