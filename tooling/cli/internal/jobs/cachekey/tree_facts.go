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
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
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
	// releases reads the baselines (releaseReader).
	releaseOnce sync.Once
	releases    *putnamigit.ReleaseBaselineReader
	extractMu   sync.Mutex
	extractions map[string]*onceExtraction
	// extractRoot is the directory of the run the release baselines are
	// extracted under, created on the first extraction and removed by Close.
	extractRoot string
	// lineDirectories lists, for a version line, the directories of the run's
	// projects of that line that declare releaseBaseline. SetPlanned fills it.
	lineDirectories map[string][]string

	// cutMu guards the candidate cut: the current read, the writes of the run
	// since it, and the read each job's key took.
	cutMu   sync.Mutex
	cut     *cutRead
	pending []pendingWrite
	cutSeq  int
	jobCuts map[string]*cutRead
	// outputs are the directories a write may leave the cut as it is under:
	// the workspace's .putnami and the declared outputs of the plan
	// (SetPlanned), absolute and clean. excludedOnce asks Git once whether
	// its rules exclude each as a whole (putnamigit.IgnoresDirectories);
	// excluded holds those it does, relative to the root, case folded.
	outputs      []string
	excludedOnce sync.Once
	excluded     map[string]bool
	// rulesMoved records that a write of the run named a .gitignore, a path
	// under .git, or a directory above an output: Git's answer about the
	// outputs then no longer holds for any but .putnami, as tooling/cli
	// ADR 0106 settles for it.
	rulesMoved bool
	// ancestors maps every strict ancestor of an output but .putnami, as
	// written and with links followed, to those outputs, built again when
	// the outputs ancestorsOf names change; and gitignores holds the
	// .gitignore of each, read when Git was asked (aboveAnOutput).
	rulesMu     sync.Mutex
	ancestorsOf []string
	ancestors   map[string][]string
	gitignores  map[string]string

	historyMu sync.Mutex
	histories map[string]*onceHistory
}

// cutRead is one read of the workspace's candidate cut, and what is derived
// from it once: its encoding, its digest and the listing file of the run.
type cutRead struct {
	seq   int
	files []protocoljob.CandidateFile
	err   error
	// stamp is the stamp of the index file taken before the read, and
	// stamped whether one could be taken (putnamigit.ReadIndexStamp).
	stamp   putnamigit.IndexStamp
	stamped bool
	// witness holds the paths whose stats witness the listing, when the
	// reader read it in-process (putnamigit.CandidateFilesWitnessed).
	witness    []string
	sourceOnce sync.Once
	source     store.CandidateCut

	encodeOnce    sync.Once
	encoded       []byte
	encodedDigest string
	encodeErr     error
	listingOnce   sync.Once
	listing       *protocoljob.CandidateListing
	listingErr    error
	// listedOnce derives once where the read lists a candidate
	// (listedDirectories): every directory on the path of a candidate, and
	// where each directory link on such a path leads.
	listedOnce  sync.Once
	listedDirs  map[string]bool
	linkTargets []string
}

// pendingWrite is a write of the run since the current read of the cut: the
// slash paths, relative to the workspace root and with case folded, it names
// and lands at once its symbolic links are followed (reachedPath), and whether
// the directories of TreeFacts.outputs it lands at or under, relative to the
// root and case folded (outputsOf).
type pendingWrite struct {
	paths   []string
	outputs []string
	// rules says whether the write names a .gitignore, a path under .git or
	// a directory above an output (aboveAnOutput).
	rules bool
}

type onceHistory struct {
	once    sync.Once
	history *protocoljob.ChangeHistory
	err     error
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
	candidateFiles     = putnamigit.CandidateFilesWitnessed
	ignoresDirectories = putnamigit.IgnoresDirectories
	readChangeHistory  = putnamigit.ReadChangeHistory
)

// RunTreeFacts returns the run's tree facts for the workspace at root: one
// object per cache manager, so every key and every job context of the run
// share its reads. A nil manager returns a new object.
func RunTreeFacts(cache *store.CacheManager, root string) *TreeFacts {
	facts, _ := cache.Value("tree-facts\x00"+root, func() any {
		facts := NewTreeFacts(root)
		// A key's `git:**` digest of the workspace lists the cut from the same
		// read (protocols/job ADR 0003).
		cache.SetCandidateSource(facts.CandidateSource())
		// A write under an excluded .putnami keeps the digests of Git
		// candidates (tooling/cli ADR 0106).
		cache.SetCandidateExclusion(facts.ExcludesFromCandidates)
		return facts
	}).(*TreeFacts)
	return facts
}

// NewTreeFacts returns an empty reader of the tree facts of the workspace at
// root.
func NewTreeFacts(root string) *TreeFacts {
	return &TreeFacts{root: root, baselines: map[string]*onceBaseline{}, extractions: map[string]*onceExtraction{},
		histories: map[string]*onceHistory{}, jobCuts: map[string]*cutRead{}}
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
//
// When the run has read the candidate cut already, the tracked paths are the
// cut's tracked entries, and the stamp is the one taken before that read: the
// cut lists every path of the index with its mode (`ls-files --stage`), so the
// index is not read a second time.
func (t *TreeFacts) TrackedRead(read func(dir string) ([]string, error)) ([]string, putnamigit.IndexStamp, bool, error) {
	t.tracked.once.Do(func() {
		t.cutMu.Lock()
		cut := t.cut
		t.cutMu.Unlock()
		if cut != nil && cut.err == nil && cut.stamped {
			t.trackedStamp, t.trackedStamped = cut.stamp, true
			t.tracked.paths = trackedOfCut(cut.files)
			return
		}
		t.trackedStamp, t.trackedStamped = putnamigit.ReadIndexStamp(t.root)
		t.tracked.paths, t.tracked.err = read(t.root)
	})
	return t.tracked.paths, t.trackedStamp, t.trackedStamped, t.tracked.err
}

// trackedOfCut is the TrackedPaths listing a candidate cut holds: its paths
// the index tracks, an unmerged one included, sorted and unique as the cut is.
func trackedOfCut(files []protocoljob.CandidateFile) []string {
	paths := []string{}
	for _, file := range files {
		if file.Mode != protocoljob.CandidateModeUntracked {
			paths = append(paths, file.Path)
		}
	}
	return paths
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
		if DeclaresRuntimeInput(job, extproto.RuntimeInputCandidateFiles) ||
			DeclaresRuntimeInput(job, extproto.RuntimeInputWorkspaceCandidateFiles) {
			_ = t.currentCut()
		}
	}
}

// currentCut returns the current read of the workspace's candidate cut
// (putnamigit.CandidateFiles), reading it again when a write of the run since
// the last read may have changed it (InvalidateCut). Outside every repository
// its error is a *gitread.UnmanagedError (putnamigit.OutsideRepository).
func (t *TreeFacts) currentCut() *cutRead {
	return t.currentCutFor(workspaceDirectories)
}

// workspaceDirectories names the whole workspace for currentCutFor.
var workspaceDirectories = []string{"."}

// currentCutFor returns a read of the cut whose listing at or below
// directories holds every write of the run: the current read, read again
// when a write since it reached one of directories (stale).
func (t *TreeFacts) currentCutFor(directories []string) *cutRead {
	t.cutMu.Lock()
	defer t.cutMu.Unlock()
	if t.cut == nil || t.stale(directories) {
		t.readCut()
	}
	return t.cut
}

// stale reports whether a write of the run since the current read may have
// changed the cut at or below one of directories: it lies at, below or above
// one of them. A write under an output directory Git excludes as a whole,
// below which the current read lists nothing, changes nothing (excludedWrite);
// Git is asked only when such writes alone reach the directories (tooling/cli
// ADR 0109). The caller holds cutMu.
func (t *TreeFacts) stale(directories []string) bool {
	excludable := false
	for _, write := range t.pending {
		if !write.reaches(directories) {
			continue
		}
		if !t.excludable(write) {
			return true
		}
		excludable = true
	}
	if !excludable {
		return false
	}
	for _, write := range t.pending {
		if write.reaches(directories) && !t.excludedWrite(write) {
			return true
		}
	}
	// The current read stays as it is until the next read, which clears the
	// writes: the excluded ones change nothing.
	kept := t.pending[:0]
	for _, write := range t.pending {
		if !t.excludable(write) || !t.excludedWrite(write) {
			kept = append(kept, write)
		}
	}
	t.pending = kept
	return false
}

// reaches reports whether the write lies at, below or above one of
// directories, slash paths relative to the workspace root ("." for the root),
// with case folded, as a case-insensitive file system resolves a path.
func (w pendingWrite) reaches(directories []string) bool {
	for _, written := range w.paths {
		for _, directory := range directories {
			directory = strings.ToLower(directory)
			if directory == "." || written == "." || written == directory ||
				strings.HasPrefix(written, directory+"/") || strings.HasPrefix(directory, written+"/") {
				return true
			}
		}
	}
	return false
}

// readCut reads the cut again and makes it the current read, which holds
// every write of the run so far. The caller holds cutMu.
func (t *TreeFacts) readCut() {
	t.cutSeq++
	read := &cutRead{seq: t.cutSeq}
	read.stamp, read.stamped = putnamigit.ReadIndexStamp(t.root)
	read.files, read.witness, read.err = candidateFiles(t.root)
	t.cut, t.pending = read, nil
}

// keyCut is the read a key of the job takes for directories: the current one,
// read again when a write since it reached them, recorded so the job's
// context carries the read its key holds (contextCut).
func (t *TreeFacts) keyCut(jobKey string, directories []string) *cutRead {
	read := t.currentCutFor(directories)
	t.cutMu.Lock()
	t.jobCuts[jobKey] = read
	t.cutMu.Unlock()
	return read
}

// contextCut is the read the job's context carries: the one its key took
// last, or the current read for a job whose key took none, such as an
// uncached one.
func (t *TreeFacts) contextCut(jobKey string) *cutRead {
	t.cutMu.Lock()
	read, ok := t.jobCuts[jobKey]
	t.cutMu.Unlock()
	if ok {
		return read
	}
	return t.currentCut()
}

// InvalidateCut records that a task of the run wrote paths, absolute paths
// under the workspace: a file it created or removed may change what the cut
// lists at, below or above the path, so the next key or context whose
// directories the write reaches reads the cut again, and one whose
// directories it does not reach keeps the current read, whose listing there
// the write did not change. A .gitignore reaches the whole directory that
// holds it, whose listing its rules change, and a write under a .git reaches
// the directory that holds it, the repository whose index and exclude rules it
// holds: the workspace's own reaches every directory. A path under
// the workspace's .putnami directory, where the store and every command output
// live, leaves the cut as it is when Git's rules exclude that directory as a
// whole and the cut lists nothing below it; Git is asked once per run,
// only when such writes alone reach a key's directories, and those writes are
// dropped once the current read is known to exclude .putnami. A task that may have
// rewritten anything passes the workspace root.
func (t *TreeFacts) InvalidateCut(paths []string) {
	if t == nil {
		return
	}
	// The paths are resolved before the lock is taken: a key waits on it.
	resolvedRoot := resolvedPath(t.root)
	writes := make([]pendingWrite, 0, len(paths))
	for _, path := range paths {
		write := pendingWrite{outputs: t.outputsOf(path), rules: t.aboveAnOutput(path)}
		for _, candidate := range [][2]string{{t.root, path}, {resolvedRoot, resolvedPath(path)}} {
			rel, err := filepath.Rel(candidate[0], candidate[1])
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			reached := reachedPath(filepath.ToSlash(rel))
			write.rules = write.rules || reached != strings.ToLower(filepath.ToSlash(rel))
			write.paths = append(write.paths, reached)
		}
		if len(write.paths) > 0 {
			writes = append(writes, write)
		}
	}
	t.cutMu.Lock()
	defer t.cutMu.Unlock()
	for _, write := range writes {
		t.rulesMoved = t.rulesMoved || write.rules
	}
	if t.cut == nil {
		return
	}
	t.pending = append(t.pending, writes...)
}

// aboveAnOutput reports whether a write to path, as written or with its links
// followed, lies strictly above an output directory other than .putnami and
// changed a .gitignore between it and that output: Git excludes an output by
// the rules of its ancestors alone. Each such .gitignore was read when Git was
// asked (excludedOutputs); one that differs now, appeared or disappeared,
// moved the rules. Before Git was asked, nothing it answered can be stale.
func (t *TreeFacts) aboveAnOutput(path string) bool {
	outputs := t.outputDirectories()
	t.rulesMu.Lock()
	if len(t.ancestorsOf) != len(outputs) || (len(outputs) > 0 && &t.ancestorsOf[0] != &outputs[0]) {
		t.ancestorsOf = outputs
		t.ancestors = map[string][]string{}
		putnami := filepath.Join(t.root, ".putnami")
		for _, output := range outputs {
			if output == putnami {
				continue
			}
			for _, form := range []string{output, resolvedPath(output)} {
				for dir := filepath.Dir(form); ; dir = filepath.Dir(dir) {
					t.ancestors[dir] = append(t.ancestors[dir], form)
					if filepath.Dir(dir) == dir {
						break
					}
				}
			}
		}
	}
	recorded, ancestors := t.gitignores, t.ancestors
	t.rulesMu.Unlock()
	if recorded == nil {
		return false
	}
	path = filepath.Clean(path)
	gitDir := filepath.Join(t.root, ".git")
	for _, written := range []string{path, resolvedPath(path)} {
		// A write at or above the repository's .git may have rewritten its
		// exclude rules (info/exclude, the excludes file its config names).
		if atOrUnder(gitDir, written) {
			return true
		}
		for _, output := range ancestors[written] {
			for dir := filepath.Dir(output); ; dir = filepath.Dir(dir) {
				file := filepath.Join(dir, ".gitignore")
				before, known := recorded[file]
				if !known || before != readRules(file) {
					return true
				}
				if dir == written || filepath.Dir(dir) == dir {
					break
				}
			}
		}
	}
	return false
}

// readRules is the content of a .gitignore, or "\x00absent" when none can be
// read there.
func readRules(file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return "\x00absent"
	}
	return string(data)
}

// reachedPath is the directory a write of rel, a slash path relative to the
// workspace root, can change the cut at, below or above, with case folded: rel
// itself, the directory of a .gitignore, whose rules apply to that whole
// directory, and the directory holding a .git, the repository whose index and
// exclude rules it holds ("." for the workspace's own).
func reachedPath(rel string) string {
	rel = strings.ToLower(rel)
	segments := strings.Split(rel, "/")
	for i, segment := range segments {
		if segment == ".git" {
			return parentOf(segments[:i])
		}
	}
	if segments[len(segments)-1] == ".gitignore" {
		return parentOf(segments[:len(segments)-1])
	}
	return rel
}

// parentOf joins segments, "." when there are none.
func parentOf(segments []string) string {
	if len(segments) == 0 {
		return "."
	}
	return strings.Join(segments, "/")
}

// atTopLevel reports whether the workspace root is the top of its work tree,
// where its own .git lies. A `git:**` digest covers the repository (tooling/cli
// ADR 0041) and the cut covers the workspace: the two agree only there.
func (t *TreeFacts) atTopLevel() bool {
	if t.root == "" || os.Getenv("GIT_DIR") != "" || os.Getenv("GIT_WORK_TREE") != "" {
		return false
	}
	_, err := os.Lstat(filepath.Join(t.root, ".git"))
	return err == nil
}

// excludable reports whether excludedWrite may answer for a write without
// asking Git more than the run did before: Git's answer is known, or the write
// lands under .putnami, the directory tooling/cli ADR 0106 asks about, and the
// one question asks about every output directory at once. The caller holds
// cutMu.
func (t *TreeFacts) excludable(write pendingWrite) bool {
	if slices.Contains(write.outputs, ".putnami") {
		return true
	}
	return len(write.outputs) > 0 && t.excluded != nil && !t.rulesMoved
}

// excludedWrite reports whether a write leaves the cut as it is: it lands at
// or under one of its output directories that Git's rules exclude as a whole
// and below which the current read lists nothing. The caller holds cutMu.
func (t *TreeFacts) excludedWrite(write pendingWrite) bool {
	read := t.cut
	if read == nil || read.err != nil {
		// A cut that could not be read says nothing of what lies below.
		return false
	}
	excluded := t.excludedOutputs()
	for _, output := range write.outputs {
		if output != ".putnami" && t.rulesMoved {
			continue
		}
		if excluded[output] && !read.lists(t.root, output) {
			return true
		}
	}
	return false
}

// excludedOutputs asks Git once, in one call, which output directories its
// rules exclude as a whole. A run whose plan declares no output asks about
// .putnami alone, as tooling/cli ADR 0106 did.
func (t *TreeFacts) excludedOutputs() map[string]bool {
	t.excludedOnce.Do(func() {
		putnami := filepath.Join(t.root, ".putnami")
		var rels []string
		for _, output := range t.outputDirectories() {
			// Git refuses a path beyond a link or in a nested repository,
			// and with it the whole question: such an output is not asked.
			if output != putnami && !t.askable(output) {
				continue
			}
			if rel, err := filepath.Rel(t.root, output); err == nil {
				rels = append(rels, filepath.ToSlash(rel))
			}
		}
		// The rules are read before Git is asked: one that changes in
		// between differs from what is read here, and moves the answer.
		gitignores := map[string]string{}
		for _, output := range t.outputDirectories() {
			for _, form := range []string{output, resolvedPath(output)} {
				for dir := filepath.Dir(form); ; dir = filepath.Dir(dir) {
					file := filepath.Join(dir, ".gitignore")
					if _, seen := gitignores[file]; seen {
						break
					}
					gitignores[file] = readRules(file)
					if filepath.Dir(dir) == dir {
						break
					}
				}
			}
		}
		t.rulesMu.Lock()
		t.gitignores = gitignores
		t.rulesMu.Unlock()
		ignored, err := ignoresDirectories(t.root, rels)
		if err != nil && len(rels) > 1 {
			// The one question tooling/cli ADR 0106 asks still holds.
			ignored, err = ignoresDirectories(t.root, []string{".putnami"})
		}
		t.excluded = map[string]bool{}
		if err != nil {
			return
		}
		for rel := range ignored {
			t.excluded[strings.ToLower(rel)] = true
		}
	})
	return t.excluded
}

// askable reports whether Git can answer for an output directory: no link
// leads it elsewhere, and no .git at or above it below the root starts a
// nested repository or a submodule.
func (t *TreeFacts) askable(output string) bool {
	if resolvedPath(output) != filepath.Join(resolvedPath(t.root), strings.TrimPrefix(output, t.root)) {
		return false
	}
	for current := output; current != t.root && atOrUnder(current, t.root); current = filepath.Dir(current) {
		if _, err := os.Lstat(filepath.Join(current, ".git")); !os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// lists reports whether the read lists a candidate at or below output, a
// slash path relative to root with case folded: a candidate's path names it,
// as a case-insensitive file system resolves it, or a directory on a
// candidate's path is a symbolic link that leads there, so the digest of the
// candidate reads bytes a write there changes. Each directory of the cut is
// stated once per read.
func (r *cutRead) lists(root, output string) bool {
	r.listedOnce.Do(func() {
		r.listedDirs = map[string]bool{}
		visited := map[string]bool{}
		for _, file := range r.files {
			r.listedDirs[strings.ToLower(file.Path)] = true
			for dir := path.Dir(file.Path); dir != "." && !visited[dir]; dir = path.Dir(dir) {
				visited[dir] = true
				r.listedDirs[strings.ToLower(dir)] = true
				full := filepath.Join(root, filepath.FromSlash(dir))
				if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
					r.linkTargets = append(r.linkTargets, resolvedPath(full))
				}
			}
		}
	})
	if r.listedDirs[output] {
		return true
	}
	resolved := resolvedPath(filepath.Join(root, filepath.FromSlash(output)))
	for _, target := range r.linkTargets {
		if atOrUnder(target, resolved) || atOrUnder(resolved, target) {
			return true
		}
	}
	return false
}

// ExcludesFromCandidates reports whether every one of paths, absolute paths a
// task of the run wrote, lies at or under the workspace's .putnami directory
// while a write there leaves the cut as it is (InvalidateCut): Git's rules
// exclude the directory as a whole and the cut lists nothing below it. Such a
// write adds no Git candidate, removes none and changes the bytes of none, so
// it moves no digest of Git candidates (store.CandidateExclusion, tooling/cli
// ADR 0106). The cache asks it only for a write that reaches such a digest; a
// run that has not read the cut yet reads it then, and Git is asked about
// .putnami once per run, as for InvalidateCut. A read a write of the run made
// stale is read again first, so the answer holds the candidates after that
// write. A path a symbolic link leads out of .putnami, .putnami itself being
// one, is not excluded: the write lands where the link leads (inPutnami).
//
// An answer that reads the cut, the first of the run or the first after a
// write made the read stale, does so while the cache holds its memo's lock: a
// key computed meanwhile waits for it.
func (t *TreeFacts) ExcludesFromCandidates(paths []string) bool {
	if t == nil || t.root == "" || len(paths) == 0 {
		return false
	}
	writes := make([]pendingWrite, 0, len(paths))
	for _, path := range paths {
		writes = append(writes, pendingWrite{outputs: t.outputsOf(path)})
	}
	t.cutMu.Lock()
	defer t.cutMu.Unlock()
	for _, write := range writes {
		if !t.excludable(write) {
			return false
		}
	}
	if t.cut == nil || t.stale(workspaceDirectories) {
		t.readCut()
	}
	for _, write := range writes {
		if !t.excludedWrite(write) {
			return false
		}
	}
	return true
}

// outputsOf returns the output directories a write to path lands at or
// under, relative to the root and case folded: path names one, and with every
// symbolic link on it followed, path still lies under that directory of the
// resolved workspace root. A link on the directory itself, or below it, that
// leads elsewhere makes the write land elsewhere, maybe among the candidates.
func (t *TreeFacts) outputsOf(path string) []string {
	var outputs []string
	resolvedRoot := ""
	for _, output := range t.outputDirectories() {
		if !atOrUnder(path, output) {
			continue
		}
		if resolvedRoot == "" {
			resolvedRoot = resolvedPath(t.root)
		}
		rel, err := filepath.Rel(t.root, output)
		if err != nil || !atOrUnder(resolvedPath(path), filepath.Join(resolvedRoot, rel)) {
			continue
		}
		outputs = append(outputs, strings.ToLower(filepath.ToSlash(rel)))
	}
	return outputs
}

// outputDirectories is outputs, the workspace's .putnami when SetPlanned
// recorded none.
func (t *TreeFacts) outputDirectories() []string {
	t.extractMu.Lock()
	defer t.extractMu.Unlock()
	if len(t.outputs) == 0 {
		t.outputs = []string{filepath.Join(t.root, ".putnami")}
	}
	return t.outputs
}

// atOrUnder reports whether path is directory or lies below it.
func atOrUnder(path, directory string) bool {
	return path == directory || strings.HasPrefix(path, directory+string(filepath.Separator))
}

// resolvedPath is path with every symbolic link on its longest existing
// prefix followed: where a write to path lands, also when path itself does not
// exist yet or any longer.
func resolvedPath(path string) string {
	for prefix := path; ; prefix = filepath.Dir(prefix) {
		if real, err := filepath.EvalSymlinks(prefix); err == nil {
			rest, err := filepath.Rel(prefix, path)
			if err != nil {
				return path
			}
			return filepath.Join(real, rest)
		}
		if filepath.Dir(prefix) == prefix {
			return path
		}
	}
}

// CandidateSource answers the keys of the run the workspace's candidate cut
// from the current read (store.CandidateSource), so a `git:**` digest of the
// workspace starts no Git and lists what the facts of the run list. It
// answers only for the workspace root when it is the top of its work tree, and
// nothing when the read failed or the workspace is outside every repository:
// the key then asks Git.
func (t *TreeFacts) CandidateSource() store.CandidateSource {
	resolved, err := filepath.EvalSymlinks(t.root)
	if err != nil {
		resolved = t.root
	}
	return func(dir string) (store.CandidateCut, bool) {
		if !t.atTopLevel() || (filepath.Clean(dir) != filepath.Clean(t.root) && filepath.Clean(dir) != resolved) {
			return store.CandidateCut{}, false
		}
		read := t.currentCut()
		if read.err != nil {
			return store.CandidateCut{}, false
		}
		read.sourceOnce.Do(func() {
			cut := store.CandidateCut{Paths: make([]string, 0, len(read.files)), Modes: map[string]string{}, Unmerged: map[string]bool{}, Witness: read.witness}
			for _, file := range read.files {
				cut.Paths = append(cut.Paths, file.Path)
				switch file.Mode {
				case protocoljob.CandidateModeUntracked:
				case protocoljob.CandidateModeUnmerged:
					cut.Unmerged[file.Path] = true
				default:
					cut.Modes[file.Path] = file.Mode
				}
			}
			read.source = cut
		})
		return read.source, true
	}
}

// under is the candidateFiles fact of directories in the read: the cut's
// paths at or below one of them, in the cut's order. found is false outside
// every repository, where there is no cut.
func (r *cutRead) under(directories []string) (files []protocoljob.CandidateFile, found bool, err error) {
	if putnamigit.OutsideRepository(r.err) {
		return nil, false, nil
	}
	if r.err != nil {
		return nil, false, r.err
	}
	files = []protocoljob.CandidateFile{}
	for _, file := range r.files {
		for _, directory := range directories {
			if directory == "." || file.Path == directory || strings.HasPrefix(file.Path, directory+"/") {
				files = append(files, file)
				break
			}
		}
	}
	return files, true, nil
}

// encodedFiles is the workspace's cut of the read as a listing file holds it, and
// the digest of those bytes, encoded once. found is false outside every
// repository.
func (r *cutRead) encodedFiles() (data []byte, digest string, found bool, err error) {
	files, found, err := r.under([]string{"."})
	if err != nil || !found {
		return nil, "", found, err
	}
	r.encodeOnce.Do(func() { r.encoded, r.encodedDigest, r.encodeErr = protocoljob.EncodeCandidateFiles(files) })
	return r.encoded, r.encodedDigest, true, r.encodeErr
}

// CandidatesUnder is the candidateFiles fact of directories the context of
// the job keyed jobKey carries (contextCut).
func (t *TreeFacts) CandidatesUnder(jobKey string, directories []string) ([]protocoljob.CandidateFile, bool, error) {
	return t.contextCut(jobKey).under(directories)
}

// WorkspaceCandidates is the workspaceCandidateFiles fact the context of the
// job keyed jobKey carries: the file of the run that holds the workspace's cut
// of its read, written once per read, and its digest. It is nil outside every
// repository.
func (t *TreeFacts) WorkspaceCandidates(jobKey string) (*protocoljob.CandidateListing, error) {
	read := t.contextCut(jobKey)
	data, digest, found, err := read.encodedFiles()
	if err != nil || !found {
		return nil, err
	}
	read.listingOnce.Do(func() {
		dir, err := t.runDirectory()
		if err != nil {
			read.listingErr = err
			return
		}
		file := filepath.Join(dir, fmt.Sprintf("workspace-candidates-%d.json", read.seq))
		if err := os.WriteFile(file, data, 0o644); err != nil { //nolint:gosec // a file of the run that jobs read
			read.listingErr = fmt.Errorf("write the workspace's candidate cut: %w", err)
			return
		}
		read.listing = &protocoljob.CandidateListing{File: file, Digest: digest}
	})
	return read.listing, read.listingErr
}

// ChangeHistory is the changeHistory fact of baseline, the run's impacted
// baseline, read once per baseline (putnamigit.ReadChangeHistory).
func (t *TreeFacts) ChangeHistory(baseline string) (*protocoljob.ChangeHistory, error) {
	t.historyMu.Lock()
	entry, ok := t.histories[baseline]
	if !ok {
		entry = &onceHistory{}
		t.histories[baseline] = entry
	}
	t.historyMu.Unlock()
	entry.once.Do(func() {
		entry.history, entry.err = readChangeHistory(t.root, baseline, wsproto.WorkspaceConfigFilename)
	})
	return entry.history, entry.err
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
	entry.once.Do(func() { entry.baseline, entry.err = t.releaseReader().Read(dir, pattern) })
	return entry.baseline, entry.err
}

// releaseReader is the run's one reader of release baselines, which asks git
// once for the repository's state, HEAD and tags (tooling/cli ADR 0110).
func (t *TreeFacts) releaseReader() *putnamigit.ReleaseBaselineReader {
	t.releaseOnce.Do(func() { t.releases = putnamigit.NewReleaseBaselineReader(t.root) })
	return t.releases
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
	outputs := []string{filepath.Join(t.root, ".putnami")}
	seen := map[string]bool{outputs[0]: true}
	for _, job := range planned {
		for _, written := range DeclaredOutputPaths(WorkspaceRoots{Root: t.root}, job, &jobmodel.JobResult{}) {
			written = filepath.Clean(written)
			if seen[written] || written == t.root || !atOrUnder(written, t.root) {
				continue
			}
			seen[written] = true
			outputs = append(outputs, written)
		}
	}
	t.extractMu.Lock()
	t.lineDirectories = lines
	t.outputs = outputs
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
		root, err := t.runDirectory()
		if err != nil {
			entry.err = err
			return
		}
		sum := sha256.Sum256([]byte(key))
		target := filepath.Join(root, hex.EncodeToString(sum[:8]))
		if err := os.MkdirAll(target, 0o755); err != nil {
			entry.err = err
			return
		}
		entry.dir, entry.err = target, extract(target)
	})
	return entry.dir, entry.err
}

// runDirectory is the directory of the run the tree facts write files under
// (release baseline extractions, the workspace's cut), created once and
// removed by Close.
func (t *TreeFacts) runDirectory() (string, error) {
	t.extractMu.Lock()
	defer t.extractMu.Unlock()
	if t.extractRoot == "" {
		scratch := store.ResolveScratchRoot(t.root)
		err := os.MkdirAll(scratch, 0o755)
		if err == nil {
			t.extractRoot, err = os.MkdirTemp(scratch, "tree-facts-")
		}
		if err != nil {
			t.extractRoot = ""
			return "", fmt.Errorf("create the tree facts directory of the run: %w", err)
		}
	}
	return t.extractRoot, nil
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
	// The listing files were in the removed directory: a later context reads
	// and writes the cut again.
	t.cutMu.Lock()
	t.cut, t.pending, t.jobCuts = nil, nil, map[string]*cutRead{}
	t.cutMu.Unlock()
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

// treeFactInputs are the runtime inputs that name a keyed tree fact, beside
// releaseBaseline, which releaseBaselineIdentity keys. changeHistory and
// releaseBaselineFiles add nothing to a key.
var treeFactInputs = []string{
	extproto.RuntimeInputCandidateFiles,
	extproto.RuntimeInputIgnoredDirectories,
	extproto.RuntimeInputRepository,
	extproto.RuntimeInputTrackedFiles,
	extproto.RuntimeInputWorkspaceCandidateFiles,
}

// CandidateDirectories are the directories the candidateFiles fact of job
// covers, relative to the workspace root in slash form, sorted and unique:
// its own directory (TreeDirectory), and each member of its project's
// dependency closure when a closure input holds a `git:` pattern
// (protocols/job ADR 0003).
func CandidateDirectories(ws *workspace.Workspace, job *jobmodel.ScheduledJob) []string {
	directories := []string{TreeDirectory(job)}
	if ws != nil && job != nil && job.Project != nil && slices.ContainsFunc(closureKeyPatterns(job), func(pattern string) bool {
		_, ok := wsproto.GitFilePattern(pattern)
		return ok
	}) {
		for _, member := range ProjectDependencyClosure(ws, job.Project) {
			dir := path.Clean(filepath.ToSlash(member.Path))
			if dir == "" || dir == "/" {
				dir = "."
			}
			directories = append(directories, dir)
		}
	}
	slices.Sort(directories)
	directories = slices.Compact(directories)
	if slices.Contains(directories, ".") {
		return []string{"."}
	}
	return directories
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
// facts is the run's read, or nil for the one RunTreeFacts gives cache.
func treeFactIdentity(ws *workspace.Workspace, job *jobmodel.ScheduledJob, cache *store.CacheManager, facts *TreeFacts) ([]string, error) {
	var identity []string
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
		case extproto.RuntimeInputCandidateFiles:
			directories := CandidateDirectories(ws, job)
			files, found, err := facts.keyCut(job.Key(), directories).under(directories)
			if err != nil {
				return nil, fmt.Errorf("read the candidate cut: %w", err)
			}
			if !found {
				identity = append(identity, name+"="+protocoljob.RepositoryUnmanaged)
				continue
			}
			identity = append(identity, name+"="+candidatesDigest(directories, files))
		case extproto.RuntimeInputWorkspaceCandidateFiles:
			_, digest, found, err := facts.keyCut(job.Key(), workspaceDirectories).encodedFiles()
			if err != nil {
				return nil, fmt.Errorf("read the candidate cut: %w", err)
			}
			if !found {
				digest = protocoljob.RepositoryUnmanaged
			}
			identity = append(identity, name+"="+digest)
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

// candidatesDigest is the key value of a candidate cut: the directories it
// covers, then each path and its mode, NUL-separated.
func candidatesDigest(directories []string, files []protocoljob.CandidateFile) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(directories, "\x00")))
	for _, file := range files {
		h.Write([]byte{1})
		h.Write([]byte(file.Path))
		h.Write([]byte{0})
		h.Write([]byte(file.Mode))
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
