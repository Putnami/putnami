// Package gitrepo reads a repository through the git CLI. It only runs
// read-only plumbing and log commands against HEAD: it never checks out
// history, never writes to the repository, and never runs the repository's
// own commands.
package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// maxBlobBytes bounds how much of one file the collector keeps in memory.
// Manifests, CI configurations and instruction files are far smaller.
const maxBlobBytes = 1 << 20

// Repo is a repository checkout read at HEAD.
type Repo struct {
	root string
}

// Open resolves the top level of the repository that contains dir. Collection
// requires Git's no-lazy-fetch option so a missing promisor object cannot
// trigger network traffic or writes to the repository.
func Open(ctx context.Context, dir string) (*Repo, error) {
	probe := &Repo{root: dir}
	if _, err := probe.Output(ctx, "version"); err != nil {
		return nil, fmt.Errorf("git must support --no-lazy-fetch for local-only collection: %w", err)
	}
	out, err := probe.Output(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", dir, err)
	}
	return &Repo{root: strings.TrimSpace(string(out))}, nil
}

// Root is the absolute path of the repository's top level. It never leaves
// the machine.
func (r *Repo) Root() string { return r.root }

func (r *Repo) command(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{
		// Every Git read must refuse lazy fetching of promised objects.
		"--no-lazy-fetch",
		"-C", r.root,
		"-c", "core.quotepath=off",
		"-c", "log.showSignature=false",
		"-c", "color.ui=false",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(),
		"LC_ALL=C",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_NO_LAZY_FETCH=1",
	)
	return cmd
}

// Output runs one git command and returns its standard output.
func (r *Repo) Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := r.command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Stream runs one git command and hands each output line to visit, so that a
// long history never sits in memory at once.
func (r *Repo) Stream(ctx context.Context, visit func(line string) error, args ...string) error {
	cmd := r.command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var visitErr error
	for scanner.Scan() {
		if visitErr = visit(scanner.Text()); visitErr != nil {
			break
		}
	}
	scanErr := scanner.Err()
	if visitErr != nil || scanErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case visitErr != nil:
		return visitErr
	case scanErr != nil:
		return fmt.Errorf("git %s: %w", args[0], scanErr)
	case waitErr != nil:
		return fmt.Errorf("git %s: %w: %s", args[0], waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Head returns the full id and the committer time, in Unix seconds, of HEAD.
func (r *Repo) Head(ctx context.Context) (string, int64, error) {
	out, err := r.Output(ctx, "log", "-1", "--format=%H %ct", "HEAD")
	if err != nil {
		return "", 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return "", 0, fmt.Errorf("HEAD has no commit")
	}
	at, err := strconv.ParseInt(fields[1], 10, 64)
	return fields[0], at, err
}

// RootCommits returns every commit without parent reachable from HEAD, with
// its committer time.
func (r *Repo) RootCommits(ctx context.Context) (map[string]int64, error) {
	out, err := r.Output(ctx, "log", "--max-parents=0", "--format=%H %ct", "HEAD")
	if err != nil {
		return nil, err
	}
	roots := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		at, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, err
		}
		roots[fields[0]] = at
	}
	return roots, nil
}

// CountCommits counts the non-merge commits reachable from HEAD.
func (r *Repo) CountCommits(ctx context.Context) (int, error) {
	out, err := r.Output(ctx, "rev-list", "--count", "--no-merges", "HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// File is one tracked file at HEAD.
type File struct {
	Path string
	Size int64
	// Lines is the number of lines of a text file; zero for a binary or an
	// empty file.
	Lines int
	// Text is false for a file git considers binary.
	Text bool
}

// Tree lists every tracked file at HEAD, sorted by path, with its size and,
// for text files, its line count.
func (r *Repo) Tree(ctx context.Context) ([]File, error) {
	out, err := r.Output(ctx, "ls-tree", "-r", "-l", "-z", "--full-tree", "HEAD")
	if err != nil {
		return nil, err
	}
	var files []File
	index := map[string]int{}
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		size, parseErr := strconv.ParseInt(fields[3], 10, 64)
		if parseErr != nil || size < 0 {
			return nil, fmt.Errorf("HEAD blob %q is unavailable (size %q)", path, fields[3])
		}
		index[path] = len(files)
		files = append(files, File{Path: path, Size: size})
	}
	counts, err := r.Output(ctx, "grep", "-I", "-c", "-z", "--no-color", "-e", "", "HEAD")
	if err != nil && !isNoMatch(err) {
		return nil, err
	}
	for _, line := range strings.Split(string(counts), "\n") {
		path, count, ok := strings.Cut(strings.TrimPrefix(line, "HEAD:"), "\x00")
		if !ok {
			continue
		}
		if i, found := index[path]; found {
			files[i].Lines, _ = strconv.Atoi(count)
			files[i].Text = true
		}
	}
	for i := range files {
		if files[i].Size == 0 {
			files[i].Text = true
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// isNoMatch reports git grep's exit status 1, which means "nothing matched".
func isNoMatch(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// Match is one line git grep found at HEAD.
type Match struct {
	Path string
	Line int
}

// Grep finds the lines at HEAD that match an extended regular expression,
// in text files only, restricted to pathspecs when any are given.
func (r *Repo) Grep(ctx context.Context, pattern string, pathspecs ...string) ([]Match, error) {
	args := []string{"grep", "-I", "-n", "-z", "--no-color", "-E", "-e", pattern, "HEAD"}
	if len(pathspecs) > 0 {
		args = append(append(args, "--"), pathspecs...)
	}
	var matches []Match
	err := r.Stream(ctx, func(line string) error {
		parts := strings.SplitN(strings.TrimPrefix(line, "HEAD:"), "\x00", 3)
		if len(parts) < 3 {
			return nil
		}
		number, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil
		}
		matches = append(matches, Match{Path: parts[0], Line: number})
		return nil
	}, args...)
	if err != nil && !isNoMatch(err) {
		return nil, err
	}
	return matches, nil
}

// ReadFiles returns the content at HEAD of each path, truncated to 1 MiB. A
// path that is not a file at HEAD is absent from the result; an unavailable
// blob that HEAD names is an error.
func (r *Repo) ReadFiles(ctx context.Context, paths []string) (map[string][]byte, error) {
	contents := map[string][]byte{}
	var request bytes.Buffer
	var asked []string
	for _, path := range paths {
		if strings.ContainsAny(path, "\n\r") {
			continue
		}
		request.WriteString("HEAD:" + path + "\n")
		asked = append(asked, path)
	}
	if len(asked) == 0 {
		return contents, nil
	}
	cmd := r.command(ctx, "cat-file", "--batch")
	cmd.Stdin = &request
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(stdout)
	var readErr error
	for _, path := range asked {
		header, err := reader.ReadString('\n')
		if err != nil {
			readErr = err
			break
		}
		if strings.HasSuffix(strings.TrimSpace(header), " missing") {
			present, err := r.blobAtHead(ctx, path)
			if err != nil {
				readErr = err
				break
			}
			if present {
				readErr = fmt.Errorf("HEAD blob %q is unavailable", path)
				break
			}
			continue
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" && fields[1] != "tree" {
			readErr = fmt.Errorf("unexpected cat-file header for %q", path)
			break
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			readErr = err
			break
		}
		kept := min(size, maxBlobBytes)
		content := make([]byte, kept)
		if _, err := io.ReadFull(reader, content); err != nil {
			readErr = err
			break
		}
		if _, err := io.CopyN(io.Discard, reader, size-kept+1); err != nil {
			readErr = err
			break
		}
		if fields[1] == "blob" {
			contents[path] = content
		}
	}
	_, _ = io.Copy(io.Discard, reader)
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case readErr != nil:
		return nil, fmt.Errorf("git cat-file: %w", readErr)
	case waitErr != nil:
		return nil, fmt.Errorf("git cat-file: %w", waitErr)
	}
	return contents, nil
}

// blobAtHead reports whether an exact path names a blob in HEAD without
// opening the blob object.
func (r *Repo) blobAtHead(ctx context.Context, path string) (bool, error) {
	out, err := r.Output(ctx, "ls-tree", "-z", "--full-tree", "HEAD", "--", ":(literal)"+path)
	if err != nil {
		return false, err
	}
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		if !ok || name != path {
			continue
		}
		fields := strings.Fields(meta)
		return len(fields) == 3 && fields[1] == "blob", nil
	}
	return false, nil
}

// FileDates is what the full history says about a set of paths.
type FileDates struct {
	// Changed is the committer time of the newest commit that changed the
	// path.
	Changed map[string]int64
	// Added is the committer time of the oldest commit that added the path.
	// A path that only a merge added, such as a subtree or an imported
	// history, gets the oldest commit that changed it.
	Added map[string]int64
}

// Dates reads, in one pass over the whole history, when each path last
// changed and when it was first added. Merges are skipped: they only repeat
// changes their parents already made.
func (r *Repo) Dates(ctx context.Context, paths []string) (FileDates, error) {
	dates := FileDates{Changed: map[string]int64{}, Added: map[string]int64{}}
	if len(paths) == 0 {
		return dates, nil
	}
	args := []string{"log", "--no-merges", "--no-renames", "--format=\x1e%ct", "--name-status", "HEAD", "--"}
	for _, path := range paths {
		args = append(args, ":(literal)"+path)
	}
	var at int64
	oldest := map[string]int64{}
	err := r.Stream(ctx, func(line string) error {
		if rest, ok := strings.CutPrefix(line, "\x1e"); ok {
			parsed, err := strconv.ParseInt(rest, 10, 64)
			at = parsed
			return err
		}
		status, path, ok := strings.Cut(line, "\t")
		if !ok {
			return nil
		}
		path = Unquote(path)
		if _, seen := dates.Changed[path]; !seen {
			dates.Changed[path] = at
		}
		oldest[path] = at
		if status == "A" {
			dates.Added[path] = at
		}
		return nil
	}, args...)
	for path, at := range oldest {
		if _, added := dates.Added[path]; !added {
			dates.Added[path] = at
		}
	}
	return dates, err
}

// Unquote reverses git's C-style quoting of a path that holds unusual bytes.
func Unquote(path string) string {
	if len(path) < 2 || path[0] != '"' || path[len(path)-1] != '"' {
		return path
	}
	if unquoted, err := strconv.Unquote(path); err == nil {
		return unquoted
	}
	return path
}
