package git

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.putnami.dev/sdk/extension/linetag"
)

// ReleaseBaselineReader reads the release baselines of the projects of one
// work tree, asking git once for what does not depend on the project
// directory: the repository's state, its HEAD, its tags and each line's last
// release (tooling/cli ADR 0110). The lines read HEAD, the tag names, their
// peeled objects and the tags HEAD reaches once between them (linetag.Tags).
// A directory it cannot place in the work tree of its root, below a .git of
// its own, through a symbolic link, or outside the root, is read as
// ReadReleaseBaseline reads it.
//
// The reads describe the repository when they were first made. A run holds
// one reader, as it holds one read of the tree facts (protocols/job ADR 0002).
type ReleaseBaselineReader struct {
	root string

	stateOnce sync.Once
	resolved  string // the root with its links followed, "" when unusable
	inside    string
	shallow   string
	prefix    string // the root's --show-prefix, without its trailing slash

	commitOnce sync.Once
	committed  bool
	commitErr  error

	tagsOnce sync.Once
	hasTags  bool
	tagsErr  error

	releasesMu sync.Mutex
	releases   map[string]*onceRelease

	tagsReadOnce sync.Once
	tags         *linetag.Tags
}

type onceRelease struct {
	once    sync.Once
	release linetag.Release
	ok      bool
	err     error
}

// NewReleaseBaselineReader returns a reader for the work tree at root.
func NewReleaseBaselineReader(root string) *ReleaseBaselineReader {
	return &ReleaseBaselineReader{root: root, releases: map[string]*onceRelease{}}
}

// Read reads the release baseline of the project in dir, whose version line
// renders its tags with tagPattern, as ReadReleaseBaseline does.
func (r *ReleaseBaselineReader) Read(dir, tagPattern string) (ReleaseBaseline, error) {
	if rel, ok := r.place(dir); ok {
		return readReleaseBaseline(dir, tagPattern, rootReleaseReads{reader: r, rel: rel})
	}
	return ReadReleaseBaseline(dir, tagPattern)
}

// place returns dir relative to the root, in slash form, when the root's
// repository holds it and git would name it by that path.
func (r *ReleaseBaselineReader) place(dir string) (string, bool) {
	if r == nil || !filepath.IsAbs(dir) || !filepath.IsAbs(r.root) {
		return "", false
	}
	r.stateOnce.Do(r.readState)
	if r.resolved == "" {
		return "", false
	}
	dir = filepath.Clean(dir)
	rel, err := filepath.Rel(filepath.Clean(r.root), dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	// git names a directory by its physical path: one a link leads to is
	// not the path below the root.
	if resolved, err := filepath.EvalSymlinks(dir); err != nil || resolved != filepath.Join(r.resolved, rel) {
		return "", false
	}
	// A .git at or below dir, above the root, starts another repository: a
	// nested one or a submodule.
	for current := dir; current != filepath.Clean(r.root); current = filepath.Dir(current) {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil || !os.IsNotExist(err) {
			return "", false
		}
	}
	if rel == "." {
		return "", true
	}
	return filepath.ToSlash(rel), true
}

// readState reads the root's repository state once. A root git does not
// answer for leaves the reader unusable, so each directory asks git itself.
func (r *ReleaseBaselineReader) readState() {
	resolved, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return
	}
	output, _, err := dirReleaseReads{}.state(r.root)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 3 || lines[0] != "true" {
		return
	}
	r.inside, r.shallow, r.prefix = lines[0], lines[1], strings.TrimSuffix(lines[2], "/")
	r.resolved = resolved
}

// rootReleaseReads answers a directory of the root's work tree from the
// reader's reads.
type rootReleaseReads struct {
	reader *ReleaseBaselineReader
	rel    string // the directory relative to the root, "" at the root
}

func (s rootReleaseReads) state(string) (string, string, error) {
	prefix := s.reader.prefix
	if s.rel != "" {
		if prefix != "" {
			prefix += "/"
		}
		prefix += s.rel
	}
	if prefix != "" {
		prefix += "/"
	}
	return s.reader.inside + "\n" + s.reader.shallow + "\n" + prefix + "\n", "", nil
}

func (s rootReleaseReads) hasCommit(string) (bool, error) {
	r := s.reader
	r.commitOnce.Do(func() { r.committed, r.commitErr = dirReleaseReads{}.hasCommit(r.root) })
	return r.committed, r.commitErr
}

func (s rootReleaseReads) release(_, tagPattern string) (linetag.Release, bool, error) {
	r := s.reader
	r.releasesMu.Lock()
	entry, ok := r.releases[tagPattern]
	if !ok {
		entry = &onceRelease{}
		r.releases[tagPattern] = entry
	}
	r.releasesMu.Unlock()
	r.tagsReadOnce.Do(func() { r.tags = linetag.Open(r.root) })
	entry.once.Do(func() { entry.release, entry.ok, entry.err = r.tags.LastRelease(tagPattern) })
	return entry.release, entry.ok, entry.err
}

func (s rootReleaseReads) hasTags(string) (bool, error) {
	r := s.reader
	r.tagsOnce.Do(func() { r.hasTags, r.tagsErr = dirReleaseReads{}.hasTags(r.root) })
	return r.hasTags, r.tagsErr
}
