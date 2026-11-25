package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extproto "go.putnami.dev/protocol/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TreeFacts is a run's one read of the tree facts a task can declare
// (go.putnami.dev/protocol/job, doc/adr/0002): the paths the Git index tracks,
// the directories Git ignores entirely, the repository state, and each
// project's release baseline. The key computation and the job context read
// the same object (RunTreeFacts), so a job receives exactly the value its key
// holds. Each fact is read once for the workspace, before the first task of
// the run starts (Prime), and answered for every task from that read.
type TreeFacts struct {
	root string

	tracked        onceList
	trackedStamp   putnamigit.IndexStamp
	trackedStamped bool
	ignored        onceList

	repositoryOnce  sync.Once
	repositoryState string
	repositoryRoot  string
	repositoryErr   error

	baselinesMu sync.Mutex
	baselines   map[string]*onceBaseline
	extractMu   sync.Mutex
	extractions map[string]*onceExtraction
	// extractRoot is the directory of the run the release baselines are
	// extracted under, created on the first extraction and removed by Close.
	extractRoot string
	// lineDirectories lists, for a version line, the directories of the run's
	// projects of that line that declare releaseBaseline. SetPlanned fills it.
	lineDirectories map[string][]string
}

type onceList struct {
	once  sync.Once
	paths []string
	err   error
}

type onceBaseline struct {
	once     sync.Once
	baseline putnamigit.ReleaseBaseline
	err      error
}

type onceExtraction struct {
	once sync.Once
	dir  string
	err  error
}

// The Git reads of the tree facts. They are variables only so a test can count
// them.
var (
	trackedPaths       = putnamigit.TrackedPaths
	ignoredDirectories = putnamigit.IgnoredDirectories
	writeFilesAtCommit = putnamigit.WriteFilesAtCommit
)

// RunTreeFacts returns the run's tree facts for the workspace at root: one
// object per cache manager, so every key and every job context of the run
// share its reads. A nil manager returns a new object.
func RunTreeFacts(cache *store.CacheManager, root string) *TreeFacts {
	facts, _ := cache.Value("tree-facts\x00"+root, func() any { return NewTreeFacts(root) }).(*TreeFacts)
	return facts
}

// NewTreeFacts returns an empty reader of the tree facts of the workspace at
// root.
func NewTreeFacts(root string) *TreeFacts {
	return &TreeFacts{root: root, baselines: map[string]*onceBaseline{}, extractions: map[string]*onceExtraction{}}
}

// Tracked returns every path the Git index tracks under the workspace root,
// relative to it in slash form, sorted and unique (putnamigit.TrackedPaths),
// read once. Outside a work tree the error is a *gitread.UnmanagedError.
func (t *TreeFacts) Tracked() ([]string, error) {
	return t.TrackedWith(trackedPaths)
}

// TrackedWith is Tracked, reading the index with read when the run has not
// read it yet: the scheduler counts its reads through it.
func (t *TreeFacts) TrackedWith(read func(dir string) ([]string, error)) ([]string, error) {
	paths, _, _, err := t.TrackedRead(read)
	return paths, err
}

// TrackedRead is TrackedWith, with the stamp of the index file taken before
// the read (putnamigit.ReadIndexStamp), and whether one could be taken. The
// stamp precedes the read, so a stamp that still matches later says the read
// is current.
func (t *TreeFacts) TrackedRead(read func(dir string) ([]string, error)) ([]string, putnamigit.IndexStamp, bool, error) {
	t.tracked.once.Do(func() {
		t.trackedStamp, t.trackedStamped = putnamigit.ReadIndexStamp(t.root)
		t.tracked.paths, t.tracked.err = read(t.root)
	})
	return t.tracked.paths, t.trackedStamp, t.trackedStamped, t.tracked.err
}

// Ignored returns the directories Git ignores entirely under the workspace
// root, relative to it in slash form, sorted, read once
// (putnamigit.IgnoredDirectories). Outside every repository it is empty; a
// Git failure is an error, never an empty answer.
func (t *TreeFacts) Ignored() ([]string, error) {
	t.ignored.once.Do(func() { t.ignored.paths, t.ignored.err = ignoredDirectories(t.root) })
	return t.ignored.paths, t.ignored.err
}

// Prime reads, before the run's first task starts, each work-tree fact a
// planned task declares, so no task's fact or key depends on what a task of
// the run wrote before it was asked. A failure is kept as that fact's answer.
func (t *TreeFacts) Prime(planned []*jobmodel.ScheduledJob) {
	for _, job := range planned {
		if DeclaresRuntimeInput(job, extproto.RuntimeInputTrackedFiles) {
			_, _ = t.Tracked()
		}
		if DeclaresRuntimeInput(job, extproto.RuntimeInputIgnoredDirectories) {
			_, _ = t.Ignored()
		}
		if DeclaresRuntimeInput(job, extproto.RuntimeInputRepository) || DeclaresRuntimeInput(job, extproto.RuntimeInputReleaseBaseline) {
			_, _, _ = t.Repository()
		}
	}
}

// Repository returns the repository state and the root of its work tree,
// read once (putnamigit.RepositoryState).
func (t *TreeFacts) Repository() (string, string, error) {
	t.repositoryOnce.Do(func() {
		t.repositoryState, t.repositoryRoot, t.repositoryErr = putnamigit.RepositoryState(t.root)
	})
	return t.repositoryState, t.repositoryRoot, t.repositoryErr
}

// ReleaseBaseline returns the release baseline of the project directory dir
// under the line tag pattern, read once per directory and pattern
// (putnamigit.ReadReleaseBaseline).
func (t *TreeFacts) ReleaseBaseline(dir, pattern string) (putnamigit.ReleaseBaseline, error) {
	key := dir + "\x00" + pattern
	t.baselinesMu.Lock()
	entry, ok := t.baselines[key]
	if !ok {
		entry = &onceBaseline{}
		t.baselines[key] = entry
	}
	t.baselinesMu.Unlock()
	entry.once.Do(func() { entry.baseline, entry.err = putnamigit.ReadReleaseBaseline(dir, pattern) })
	return entry.baseline, entry.err
}

// SetPlanned records, for each version line, the directories of the planned
// projects that declare releaseBaseline and releaseBaselineFiles, which one
// extraction of the line's last tag holds.
func (t *TreeFacts) SetPlanned(ws *workspace.Workspace, planned []*jobmodel.ScheduledJob) {
	lines := map[string][]string{}
	for _, job := range planned {
		if job == nil || job.Project == nil || !DeclaresRuntimeInput(job, extproto.RuntimeInputReleaseBaseline) ||
			!DeclaresRuntimeInput(job, extproto.RuntimeInputReleaseBaselineFiles) {
			continue
		}
		pattern := LineTagPattern(ws, job.Project.Line)
		dir := filepath.Join(ws.Root, job.Project.Path)
		if !slices.Contains(lines[pattern], dir) {
			lines[pattern] = append(lines[pattern], dir)
		}
	}
	t.extractMu.Lock()
	t.lineDirectories = lines
	t.extractMu.Unlock()
}

// BaselineDirectory returns the directory that holds the files baseline.Tag
// holds at the project directory dir, "" when the directory is absent at the
// tag. It extracts the line's last tag once per tag commit and line, for every
// directory of the line SetPlanned recorded whose baseline names the same
// commit. A directory the plan did not record is extracted once on its own.
// Each directory is placed by its baseline's prefix, the path Git names it by
// in the work tree, so a project reached through a link is found. A directory
// the extraction does not hold is an error, never a directory to name.
func (t *TreeFacts) BaselineDirectory(dir, pattern string, baseline putnamigit.ReleaseBaseline) (string, error) {
	if baseline.State != putnamigit.ReleaseBaselineTagged || baseline.Object == "" {
		return "", nil
	}
	_, repoRoot, err := t.Repository()
	if err != nil {
		return "", err
	}
	if repoRoot == "" {
		return "", fmt.Errorf("the release baseline of %s names a tag outside a work tree", dir)
	}
	t.extractMu.Lock()
	directories := t.lineDirectories[pattern]
	t.extractMu.Unlock()
	key := baseline.Commit + "\x00" + pattern
	prefixes := []string{baseline.Prefix}
	if slices.Contains(directories, dir) {
		for _, other := range directories {
			if other == dir {
				continue
			}
			sibling, err := t.ReleaseBaseline(other, pattern)
			if err != nil {
				return "", err
			}
			if sibling.State == putnamigit.ReleaseBaselineTagged && sibling.Commit == baseline.Commit && sibling.Object != "" {
				prefixes = append(prefixes, sibling.Prefix)
			}
		}
	} else {
		key += "\x00" + dir
	}
	extraction, err := t.extraction(key, func(target string) error {
		return t.extract(repoRoot, baseline.Commit, prefixes, target)
	})
	if err != nil {
		return "", err
	}
	extracted := filepath.Join(extraction, filepath.FromSlash(baseline.Prefix))
	if info, err := os.Stat(extracted); err != nil || !info.IsDir() {
		return "", fmt.Errorf("the release baseline of %s: %s holds no directory at %q", dir, baseline.Tag, baseline.Prefix)
	}
	return extracted, nil
}

// extraction returns the directory of the run extract filled for key, filling
// it once.
func (t *TreeFacts) extraction(key string, extract func(target string) error) (string, error) {
	t.extractMu.Lock()
	entry, ok := t.extractions[key]
	if !ok {
		entry = &onceExtraction{}
		t.extractions[key] = entry
	}
	t.extractMu.Unlock()
	entry.once.Do(func() {
		t.extractMu.Lock()
		if t.extractRoot == "" {
			scratch := store.ResolveScratchRoot(t.root)
			err := os.MkdirAll(scratch, 0o755)
			if err == nil {
				t.extractRoot, err = os.MkdirTemp(scratch, "release-baselines-")
			}
			if err != nil {
				t.extractMu.Unlock()
				entry.err = fmt.Errorf("create the release baseline directory: %w", err)
				return
			}
		}
		sum := sha256.Sum256([]byte(key))
		target := filepath.Join(t.extractRoot, hex.EncodeToString(sum[:8]))
		t.extractMu.Unlock()
		if err := os.MkdirAll(target, 0o755); err != nil {
			entry.err = err
			return
		}
		entry.dir, entry.err = target, extract(target)
	})
	return entry.dir, entry.err
}

// extract writes into target the regular files commit holds at prefixes
// (slash paths relative to the work tree's root, "" for the root), as raw
// blobs (putnamigit.WriteFilesAtCommit).
func (t *TreeFacts) extract(repoRoot, commit string, prefixes []string, target string) error {
	var paths []string
	for _, prefix := range prefixes {
		if prefix == "" {
			paths = []string{"."}
			break
		}
		if !slices.Contains(paths, prefix) {
			paths = append(paths, prefix)
		}
	}
	if err := writeFilesAtCommit(repoRoot, commit, paths, target); err != nil {
		return fmt.Errorf("extract the release baseline at %s: %w", commit, err)
	}
	return nil
}

// Close removes the run's release baseline extractions. A later job context
// extracts again.
func (t *TreeFacts) Close() {
	if t == nil {
		return
	}
	t.extractMu.Lock()
	root := t.extractRoot
	t.extractRoot = ""
	t.extractions = map[string]*onceExtraction{}
	t.extractMu.Unlock()
	if root != "" {
		_ = os.RemoveAll(root)
	}
}

// DeclaresRuntimeInput reports whether job's task declares the runtime input
// name.
func DeclaresRuntimeInput(job *jobmodel.ScheduledJob, name string) bool {
	return job != nil && job.JobDef != nil && job.JobDef.TaskCachePolicy != nil &&
		job.JobDef.TaskCachePolicy.Key != nil && slices.Contains(job.JobDef.TaskCachePolicy.Key.Runtime, name)
}

// treeFactInputs are the runtime inputs that name a tree fact, beside
// releaseBaseline, which releaseBaselineIdentity keys.
var treeFactInputs = []string{
	extproto.RuntimeInputIgnoredDirectories,
	extproto.RuntimeInputRepository,
	extproto.RuntimeInputTrackedFiles,
}

// TreeDirectory is the directory the tree facts of job cover, relative to the
// workspace root in slash form: its project's directory, or "." for a
// workspace task or a project at the root.
func TreeDirectory(job *jobmodel.ScheduledJob) string {
	// A workspace task is the one the plan hands the selected projects
	// (jobmodel.ScheduledJob.TypedIdentity).
	if job == nil || job.Project == nil || len(job.SelectedProjects) > 0 {
		return "."
	}
	dir := path.Clean(filepath.ToSlash(job.Project.Path))
	if dir == "" || dir == "/" {
		return "."
	}
	return dir
}

// under returns the sorted paths at or below directory ("." holds all).
func under(paths []string, directory string) []string {
	if directory == "." {
		return paths
	}
	prefix := directory + "/"
	start := sort.SearchStrings(paths, directory)
	var selected []string
	for _, p := range paths[start:] {
		if p != directory && !strings.HasPrefix(p, prefix) {
			if p > prefix {
				break
			}
			continue
		}
		selected = append(selected, p)
	}
	if selected == nil {
		selected = []string{}
	}
	return selected
}

// TrackedUnder is the trackedFiles fact of directory: the index's paths at or
// below it, or the empty answer outside a work tree.
func (t *TreeFacts) TrackedUnder(directory string) ([]string, error) {
	paths, err := t.Tracked()
	if putnamigit.OutsideRepository(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return under(paths, directory), nil
}

// treeFactIdentity resolves the tree-fact runtime inputs job declares into
// their `name=value` cache-key pairs, sorted by name. A listing is keyed by
// the digest of its paths, the repository by its state. A Git failure is the
// key's error, so the task runs uncached (ADR 0062 point 2).
func treeFactIdentity(ws *workspace.Workspace, job *jobmodel.ScheduledJob, cache *store.CacheManager) ([]string, error) {
	var identity []string
	var facts *TreeFacts
	directory := TreeDirectory(job)
	for _, name := range treeFactInputs {
		if !DeclaresRuntimeInput(job, name) {
			continue
		}
		if facts == nil {
			facts = RunTreeFacts(cache, ws.Root)
		}
		switch name {
		case extproto.RuntimeInputRepository:
			state, _, err := facts.Repository()
			if err != nil {
				return nil, err
			}
			identity = append(identity, name+"="+state)
		case extproto.RuntimeInputTrackedFiles:
			paths, err := facts.TrackedUnder(directory)
			if err != nil {
				return nil, fmt.Errorf("read the tracked files: %w", err)
			}
			identity = append(identity, name+"="+listingDigest(directory, paths))
		case extproto.RuntimeInputIgnoredDirectories:
			paths, err := facts.IgnoredUnder(directory)
			if err != nil {
				return nil, err
			}
			identity = append(identity, name+"="+listingDigest(directory, paths))
		}
	}
	return identity, nil
}

// listingDigest is the key value of a listing: the directory it covers and
// its paths, NUL-separated.
func listingDigest(directory string, paths []string) string {
	h := sha256.New()
	h.Write([]byte(directory))
	for _, p := range paths {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IgnoredUnder is the ignoredDirectories fact of directory: the directories
// Git ignores entirely at or below it.
func (t *TreeFacts) IgnoredUnder(directory string) ([]string, error) {
	paths, err := t.Ignored()
	if err != nil {
		return nil, err
	}
	return under(paths, directory), nil
}
