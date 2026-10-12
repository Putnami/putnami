package apicheck

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/typescript/extension/internal/apisurface"
)

// gitTimeout bounds one git call. Every call reads local objects only.
const gitTimeout = 2 * time.Minute

// commitRecordSeparator is ASCII RS: it opens each commit record of a log, and
// NUL separates the fields, because a commit body is arbitrary text.
const commitRecordSeparator = "\x1e"

// errNotARepository is git's answer outside a work tree.
var errNotARepository = errors.New("not a git repository")

// repoState is what the check needs to know about the repository before it
// reads history.
type repoState struct {
	hasHead bool
	shallow bool
}

// gitOutput runs git in dir and returns its stdout. A failure carries git's
// stderr.
func gitOutput(dir string, args ...string) (string, error) {
	stdout, stderr, err := gitCapture(dir, nil, nil, args...)
	if err != nil {
		return "", fmt.Errorf("git %s (in %s): %w: %s", args[0], dir, err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// gitCapture runs git in dir with env (nil inherits this process's) and stdin,
// and returns stdout and stderr.
func gitCapture(dir string, env []string, stdin io.Reader, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", stderr.String(), fmt.Errorf("timed out after %s", gitTimeout)
		}
		return "", stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

// readRepoState reports whether dir is in a work tree with a commit, and
// whether the clone is shallow. Outside a work tree it returns
// errNotARepository; a git that cannot run is an error.
func readRepoState(dir string) (repoState, error) {
	output, stderr, err := gitCapture(dir, nil, nil, "rev-parse", "--is-inside-work-tree", "--is-shallow-repository")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return repoState{}, fmt.Errorf("%w: %s", errNotARepository, strings.TrimSpace(stderr))
		}
		return repoState{}, fmt.Errorf("run git: %w", err)
	}
	lines := strings.Fields(output)
	if len(lines) != 2 || lines[0] != "true" {
		return repoState{}, fmt.Errorf("%w: %s is not inside a work tree", errNotARepository, dir)
	}
	state := repoState{shallow: lines[1] == "true"}
	_, _, headErr := gitCapture(dir, nil, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	state.hasHead = headErr == nil
	return state, nil
}

// hasTags reports whether the repository has at least one tag, of any line.
func hasTags(dir string) (bool, error) {
	output, err := gitOutput(dir, "for-each-ref", "--count=1", "--format=%(refname)", "refs/tags")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

// filesAtTag returns the source files under dir as the tag holds them, with
// slash paths relative to dir, from a listing of the tag's tree. It loads only
// the files apisurface.IsSource selects, so nothing under node_modules and no
// file of another kind is loaded. Symbolic links and submodules are not files
// of the package and are left out.
func filesAtTag(dir string, entries []treeEntry) ([]apisurface.File, error) {
	var paths, objects []string
	for _, entry := range entries {
		if entry.regular && apisurface.IsSource(entry.name) {
			paths = append(paths, entry.name)
			objects = append(objects, entry.object)
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	contents, err := readBlobs(dir, objects)
	if err != nil {
		return nil, err
	}
	files := make([]apisurface.File, len(paths))
	for i, name := range paths {
		files[i] = apisurface.File{Path: name, Data: contents[i]}
	}
	return files, nil
}

// filesInTree returns the source files of the working tree under dir, with
// slash paths relative to dir, uncommitted edits included. It reads the files
// git tracks and the untracked files git does not ignore: the files a commit
// of the tree would hold, so both sides of a comparison follow one rule. A
// tracked file that is deleted, and anything that is not a regular file, is
// left out.
func filesInTree(dir string) ([]apisurface.File, error) {
	output, err := gitOutput(dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ".")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []apisurface.File
	for name := range strings.SplitSeq(output, "\x00") {
		if name == "" || seen[name] || !apisurface.IsSource(name) {
			continue
		}
		seen[name] = true
		full := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the working tree of %s: %w", dir, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(full) //nolint:gosec // a file git lists under the project
		if err != nil {
			return nil, fmt.Errorf("read the working tree of %s: %w", dir, err)
		}
		files = append(files, apisurface.File{Path: name, Data: data})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// treeEntry is one entry of a tag's tree.
type treeEntry struct {
	// name is the entry's slash path, relative to the directory listed.
	name string
	// object is a regular file's blob.
	object string
	// size is a regular file's size in bytes, read only by a sized listing.
	size int
	// regular is false for a symbolic link, a directory or a submodule.
	regular bool
}

// treeListing selects what treeAtTag lists.
type treeListing struct {
	// recursive lists every entry under the path instead of the entry at it.
	recursive bool
	// sized reads each regular file's size. A wide listing leaves it off: a
	// partial clone fetches every blob it sizes.
	sized bool
}

// treeAtTag returns the entries of tag's tree that `git ls-tree` lists at
// pathspec, read literally and relative to dir.
func treeAtTag(dir, tag, pathspec string, listing treeListing) ([]treeEntry, error) {
	args := []string{"ls-tree", "-z"}
	if listing.recursive {
		args = append(args, "-r")
	}
	if listing.sized {
		args = append(args, "-l")
	}
	args = append(args, "refs/tags/"+tag, "--", pathspec)
	output, stderr, err := gitCapture(dir, append(os.Environ(), "GIT_LITERAL_PATHSPECS=1"), nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree (in %s): %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	// An entry is "<mode> <type> <object>", then " <size>" in a sized
	// listing, then a tab and the path. The size of a tree or a submodule is
	// "-".
	fieldCount := 3
	if listing.sized {
		fieldCount = 4
	}
	var entries []treeEntry
	for record := range strings.SplitSeq(output, "\x00") {
		meta, name, found := strings.Cut(record, "\t")
		if !found {
			continue
		}
		entry := treeEntry{name: name}
		fields := strings.Fields(meta)
		if len(fields) == fieldCount && fields[1] == "blob" && fields[0] != "120000" {
			entry.object, entry.regular = fields[2], true
			if listing.sized {
				if entry.size, err = strconv.Atoi(fields[3]); err != nil {
					return nil, fmt.Errorf("git ls-tree: unexpected size in %q", meta)
				}
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// readBlobs returns the contents of objects, in order, from one
// `git cat-file --batch` process.
func readBlobs(dir string, objects []string) ([][]byte, error) {
	stdout, stderr, err := gitCapture(dir, nil, strings.NewReader(strings.Join(objects, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("git cat-file (in %s): %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	reader := bufio.NewReader(strings.NewReader(stdout))
	contents := make([][]byte, 0, len(objects))
	for _, object := range objects {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file: read the header of %s: %w", object, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("git cat-file: unexpected header %q for %s", strings.TrimSpace(header), object)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 {
			return nil, fmt.Errorf("git cat-file: unexpected size in %q", strings.TrimSpace(header))
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, fmt.Errorf("git cat-file: read %s: %w", object, err)
		}
		contents = append(contents, data[:size])
	}
	return contents, nil
}

// commit is one commit of the range the check reads.
type commit struct {
	sha     string
	subject string
	body    string
}

// commitsSince returns the commits after tag up to HEAD that touch pathspec,
// read literally and relative to dir, newest first.
func commitsSince(dir, tag, pathspec string) ([]commit, error) {
	output, stderr, err := gitCapture(dir, append(os.Environ(), "GIT_LITERAL_PATHSPECS=1"), nil,
		"log", "--format="+commitRecordSeparator+"%H%x00%s%x00%b",
		"--no-show-signature", "refs/tags/"+tag+"..HEAD", "--", pathspec)
	if err != nil {
		return nil, fmt.Errorf("git log (in %s): %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	var commits []commit
	for record := range strings.SplitSeq(output, commitRecordSeparator) {
		fields := strings.SplitN(record, "\x00", 3)
		if len(fields) != 3 {
			continue
		}
		commits = append(commits, commit{
			sha:     strings.TrimSpace(fields[0]),
			subject: strings.TrimSpace(fields[1]),
			body:    strings.TrimRight(fields[2], "\n"),
		})
	}
	return commits, nil
}
