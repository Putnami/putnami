package git

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.putnami.dev/sdk/extension/linetag"
	"go.putnami.dev/tooling/cli/internal/fileindex"
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

	releasesMu sync.Mutex
	releases   map[string]*onceRelease

	tagsReadOnce sync.Once
	tags         *linetag.Tags

	// record answers and keeps the baselines of the root's work tree under
	// their witness (tooling/cli ADR 0113 §4); nil records nothing.
	record      *fileindex.Record
	witnessOnce sync.Once
	witness     baselineWitness
	witnessed   bool
}

type onceRelease struct {
	once    sync.Once
	release linetag.Release
	ok      bool
	err     error
}

// NewReleaseBaselineReader returns a reader for the work tree at root that
// answers from, and records into, the process's current input record
// (fileindex.CurrentRecord), none outside a run.
func NewReleaseBaselineReader(root string) *ReleaseBaselineReader {
	return NewRecordedReleaseBaselineReader(root, fileindex.CurrentRecord())
}

// ReleaseBaselineTable is the table of the input record that holds the
// release baselines, keyed by the project directory, the tag pattern and the
// witness of the commits they name (tooling/cli ADR 0113 §4).
const ReleaseBaselineTable = "release-baseline"

// baselineKey is the key of dir's baseline under pattern and witness.
func baselineKey(dir, pattern string, witness baselineWitness) string {
	return dir + "\x00" + pattern + "\x00" + hex.EncodeToString(witness[:])
}

// NewRecordedReleaseBaselineReader returns a reader for the work tree at root
// that answers a baseline from record when its witness holds, and records the
// baselines it reads (tooling/cli ADR 0113 §4). A nil record records nothing.
func NewRecordedReleaseBaselineReader(root string, record *fileindex.Record) *ReleaseBaselineReader {
	return &ReleaseBaselineReader{root: root, releases: map[string]*onceRelease{}, record: record}
}

// Read reads the release baseline of the project in dir, whose version line
// renders its tags with tagPattern, as ReadReleaseBaseline does.
//
// A directory of the root's work tree is answered from the reader's record
// when the witness of the commits its baseline names holds, with no Git call
// of its own. Otherwise it is read, and a tagged or untagged baseline is
// recorded, kept when the record saves only if the witness, taken again then,
// still holds.
func (r *ReleaseBaselineReader) Read(dir, tagPattern string) (ReleaseBaseline, error) {
	rel, ok := r.place(dir)
	if !ok {
		return ReadReleaseBaseline(dir, tagPattern)
	}
	if r.record == nil {
		return readReleaseBaseline(dir, tagPattern, rootReleaseReads{reader: r, rel: rel})
	}
	r.witnessOnce.Do(func() { r.witness, r.witnessed = r.releaseWitness() })
	if r.witnessed {
		if value, _, ok := r.record.Answer(ReleaseBaselineTable, baselineKey(dir, tagPattern, r.witness), r.record.Hold); ok {
			if baseline, ok := decodeRecordedBaseline(value, tagPattern); ok {
				return baseline, nil
			}
		}
	}
	baseline, err := readReleaseBaseline(dir, tagPattern, rootReleaseReads{reader: r, rel: rel})
	if err != nil || !r.witnessed {
		return baseline, err
	}
	if baseline.State != ReleaseBaselineTagged && baseline.State != ReleaseBaselineUntagged {
		return baseline, nil
	}
	// The record takes the whole witness again before it writes: a baseline
	// read while a part of it moved is not kept.
	witness := r.witness
	r.record.PutRechecked(ReleaseBaselineTable, baselineKey(dir, tagPattern, witness), encodeRecordedBaseline(baseline),
		hex.EncodeToString(witness[:]), func() bool {
			again, ok := r.recheckWitness()
			return ok && again == witness
		})
	return baseline, nil
}

// recordedBaseline is a baseline as the record keeps it: its value, and the
// commit and prefix that locate the files its tag holds.
type recordedBaseline struct {
	Baseline ReleaseBaseline `json:"baseline"`
	Commit   string          `json:"commit,omitempty"`
	Prefix   string          `json:"prefix,omitempty"`
}

func encodeRecordedBaseline(baseline ReleaseBaseline) []byte {
	data, err := json.Marshal(recordedBaseline{Baseline: baseline, Commit: baseline.Commit, Prefix: baseline.Prefix})
	if err != nil {
		panic(fmt.Sprintf("encode release baseline: %v", err))
	}
	return data
}

// decodeRecordedBaseline decodes a recorded baseline; false for a value that
// is not one of pattern's tagged or untagged baselines.
func decodeRecordedBaseline(value []byte, pattern string) (ReleaseBaseline, bool) {
	var recorded recordedBaseline
	if err := json.Unmarshal(value, &recorded); err != nil {
		return ReleaseBaseline{}, false
	}
	baseline := recorded.Baseline
	baseline.Commit, baseline.Prefix = recorded.Commit, recorded.Prefix
	switch {
	case baseline.Pattern != pattern:
		return ReleaseBaseline{}, false
	case baseline.State == ReleaseBaselineTagged && baseline.Tag != "" && baseline.Commit != "":
	case baseline.State == ReleaseBaselineUntagged && baseline.Tag == "":
	default:
		return ReleaseBaseline{}, false
	}
	return baseline, true
}

func (r *ReleaseBaselineReader) openTags() { r.tags = linetag.Open(r.root) }

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
	r.tagsReadOnce.Do(r.openTags)
	entry.once.Do(func() { entry.release, entry.ok, entry.err = r.tags.LastRelease(tagPattern) })
	return entry.release, entry.ok, entry.err
}

// head is HEAD's commit as the reader's one read of the tags holds it, from
// which each line's last release was reached.
func (s rootReleaseReads) head(string) (string, error) {
	r := s.reader
	r.tagsReadOnce.Do(r.openTags)
	return r.tags.Head()
}

// hasTags answers from the reader's one read of the tag names.
func (s rootReleaseReads) hasTags(string) (bool, error) {
	r := s.reader
	r.tagsReadOnce.Do(r.openTags)
	return r.tags.HasTags()
}
