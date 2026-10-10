package apicheck

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/go/extension/internal/apisurface"
	wsproto "go.putnami.dev/protocol/workspace"
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

// lastReachableTag returns the nearest tag reachable from HEAD that matches a
// complete line tag pattern, with the semantics of the version bump's own
// reading (tooling/cli/internal/git LastReachableTag): `git describe` finds the
// nearest tag matching the pattern's prefix glob, and a tag the pattern could
// never render reads as no tag. ok is false for a line with no reachable tag.
// A git failure is an error, even when it exits as the untagged line does.
func lastReachableTag(dir, pattern string) (tag string, ok bool, err error) {
	placeholder := wsproto.LineTagPlaceholder
	if strings.Count(pattern, placeholder) != 1 || strings.ContainsAny(pattern, " \t\n*?[") || strings.HasPrefix(pattern, "-") {
		return "", false, fmt.Errorf("%q is not a usable line tag pattern", pattern)
	}
	match := strings.Replace(pattern, placeholder, "*", 1)
	output, stderr, runErr := gitCapture(dir, nil, nil, "describe", "--tags", "--abbrev=0", "--match", match)
	if runErr != nil {
		describeErr := fmt.Errorf("git describe: %w: %s", runErr, strings.TrimSpace(stderr))
		return "", false, describeUntagged(dir, match, describeErr)
	}
	tag = strings.TrimSpace(output)
	prefix, suffix, _ := strings.Cut(pattern, placeholder)
	if len(tag) <= len(prefix)+len(suffix) || !strings.HasPrefix(tag, prefix) || !strings.HasSuffix(tag, suffix) {
		return "", false, nil
	}
	return tag, true, nil
}

// describeUntagged returns nil only when a failed `git describe` is the
// untagged line: `git tag --merged HEAD` answers cleanly with no matching tag.
// Any other answer, a stderr line included, is the failure it reports.
func describeUntagged(dir, match string, describeErr error) error {
	reachable, stderr, err := gitCapture(dir, withoutGitTrace(os.Environ()), nil,
		"tag", "--list", "--merged", "HEAD", "--", match)
	switch tags := strings.Fields(reachable); {
	case err != nil:
		return fmt.Errorf("read the last tag: %w; git tag --merged: %w: %s", describeErr, err, strings.TrimSpace(stderr))
	case strings.TrimSpace(stderr) != "":
		return fmt.Errorf("read the last tag: %w; git tag --merged: %s", describeErr, strings.TrimSpace(stderr))
	case len(tags) > 0:
		return fmt.Errorf("read the last tag: %w; HEAD reaches %s", describeErr, strings.Join(tags, ", "))
	}
	return nil
}

// hasTags reports whether the repository has at least one tag, of any line.
func hasTags(dir string) (bool, error) {
	output, err := gitOutput(dir, "for-each-ref", "--count=1", "--format=%(refname)", "refs/tags")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

// withoutGitTrace is env without git's GIT_TRACE* settings, which write to
// stderr.
func withoutGitTrace(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, "GIT_TRACE") {
			kept = append(kept, entry)
		}
	}
	return kept
}

// filesAtTag returns the files under dir the API surface reads, as the tag
// holds them, with paths relative to dir. It lists the tree first and loads
// only the files apisurface.Reads selects, so the Go files of internal,
// testdata and vendor directories and of nested modules are never loaded.
// Symbolic links and submodules are not files of the package and are left
// out.
func filesAtTag(dir, tag string) ([]apisurface.File, error) {
	entries, err := treeAtTag(dir, tag, ".", treeListing{recursive: true})
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	var listed []string
	for _, entry := range entries {
		if !entry.regular {
			continue
		}
		blobs[entry.name] = entry.object
		listed = append(listed, entry.name)
	}
	paths := apisurface.Reads(listed)
	if len(paths) == 0 {
		return nil, nil
	}
	objects := make([]string, len(paths))
	for i, name := range paths {
		objects[i] = blobs[name]
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

// sourceFile reports whether a project-relative path is a file the API
// surface reads: a Go file or a go.mod.
func sourceFile(name string) bool {
	return strings.HasSuffix(name, ".go") || path.Base(name) == "go.mod"
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

// commit is the message of one commit of the range the check reads. The check
// reads what a commit declares, never which commit it is.
type commit struct {
	subject string
	body    string
}

// commitsSince returns the commits after tag up to HEAD that touch dir,
// newest first.
func commitsSince(dir, tag string) ([]commit, error) {
	output, err := gitOutput(dir, "log", "--format="+commitRecordSeparator+"%s%x00%b",
		"--no-show-signature", "refs/tags/"+tag+"..HEAD", "--", ".")
	if err != nil {
		return nil, err
	}
	var commits []commit
	for record := range strings.SplitSeq(output, commitRecordSeparator) {
		fields := strings.SplitN(record, "\x00", 2)
		if len(fields) != 2 {
			continue
		}
		commits = append(commits, commit{
			subject: strings.TrimSpace(fields[0]),
			body:    strings.TrimRight(fields[1], "\n"),
		})
	}
	return commits, nil
}
