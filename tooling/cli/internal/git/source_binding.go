package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocaps "go.putnami.dev/protocol/capabilities"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/sourcebinding"
	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// hostStatsExecBit reports whether the host filesystem stores the executable
// bit. Windows does not: every file stats without one there, so a tracked file
// takes its executable bit from the mode git reports for it instead.
const hostStatsExecBit = sourcebinding.HostStatsExecBit

// SourceBindingMetrics reports the physical child-process cost of computing a
// source binding. Enumeration reads the index and the work tree in-process,
// with at most one git config call, or makes two git ls-files calls per
// snapshot where the in-process reader steps aside; gitlinks add the rev-parse
// calls needed to identify their visible worktree.
type SourceBindingMetrics struct {
	SpawnedProcesses int
}

// ProjectSourceBindingMeasured computes the source-v1 binding for one exact
// project root in the current worktree, plus its exact git subprocess count.
// Git supplies the tracked and non-ignored untracked input set; working bytes
// win over index bytes and deleted files are absent. The capabilities protocol
// owns exclusions, canonical ordering, and hashing.
//
// The subprocess count is observational only: it neither changes the source-v1
// bytes nor introduces process-global instrumentation that could mix concurrent
// callers' measurements.
func ProjectSourceBindingMeasured(repoRoot, projectRoot string) (string, SourceBindingMetrics, error) {
	if _, _, err := sourceBindingRoots(repoRoot, projectRoot); err != nil {
		return "", SourceBindingMetrics{}, err
	}
	snapshot, metrics, err := ReadSourceBindingSnapshot(repoRoot)
	if err != nil {
		return "", metrics, err
	}
	binding, bindingMetrics, err := snapshot.ProjectSourceBindingMeasured(projectRoot)
	metrics.SpawnedProcesses += bindingMetrics.SpawnedProcesses
	return binding, metrics, err
}

// SourceBindingSnapshot holds only Git's tracked and unignored-untracked file
// enumeration, never file bytes or computed bindings. It is immutable and can
// serve multiple exact project roots concurrently. Callers must replace it after
// a source-visible write: new paths, index entries and ignore rules can change
// which files belong to a binding even when an existing file's bytes did not.
type SourceBindingSnapshot struct {
	repoRoot  string
	tracked   []sourceBindingIndexEntry
	untracked []string
}

type sourceBindingIndexEntry struct {
	mode, objectID, stage, path string
}

// ReadSourceBindingSnapshot enumerates the repository once, using the same Git
// tracked/untracked semantics as an individual project lookup. It reads the
// index and the untracked files in-process when the repository's state is one
// the reader supports (tooling/cli ADR 0112), and asks Git otherwise. Stage validation
// remains project-local: a conflict in an unrelated project cannot poison every
// binding merely because the enumeration is shared.
func ReadSourceBindingSnapshot(repoRoot string) (*SourceBindingSnapshot, SourceBindingMetrics, error) {
	metrics := SourceBindingMetrics{}
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, metrics, fmt.Errorf("resolve repository root: %w", err)
	}
	snapshot, spawned, err := sourceBindingSnapshotInProcess(repoRoot)
	metrics.SpawnedProcesses += spawned
	if !inProcessUnsupported(err) {
		return snapshot, metrics, err
	}
	fromGit, gitMetrics, err := sourceBindingSnapshotFromGit(repoRoot)
	metrics.SpawnedProcesses += gitMetrics.SpawnedProcesses
	return fromGit, metrics, err
}

// sourceBindingSnapshotFromGit is the snapshot Git lists, in two ls-files
// calls.
func sourceBindingSnapshotFromGit(repoRoot string) (*SourceBindingSnapshot, SourceBindingMetrics, error) {
	metrics := SourceBindingMetrics{}
	metrics.SpawnedProcesses++
	tracked, err := gitread.Run(repoRoot, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, metrics, fmt.Errorf("enumerate tracked project source: %w", err)
	}
	metrics.SpawnedProcesses++
	untracked, err := gitread.Run(repoRoot, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, metrics, fmt.Errorf("enumerate untracked project source: %w", err)
	}
	snapshot := &SourceBindingSnapshot{repoRoot: repoRoot, untracked: gitread.SplitNUL(untracked)}
	for _, entry := range gitread.SplitNUL(tracked) {
		mode, objectID, stage, repoPath, ok := gitread.ParseStageEntry(entry)
		if !ok {
			return nil, metrics, fmt.Errorf("enumerate tracked project source: malformed ls-files stage entry")
		}
		snapshot.tracked = append(snapshot.tracked, sourceBindingIndexEntry{mode, objectID, stage, repoPath})
	}
	return snapshot, metrics, nil
}

// sourceBindingSnapshotInProcess is the snapshot read in-process: the
// entries ls-files --stage lists, and the untracked paths ls-files --others
// lists, a nested repository with a trailing slash, and the git processes
// the read started: one when it read Git's configuration.
func sourceBindingSnapshotInProcess(repoRoot string) (*SourceBindingSnapshot, int, error) {
	repo, err := openLocal(repoRoot)
	if repo == nil {
		return nil, spawnedBy(err), err
	}
	cut, err := repo.readCut()
	if err != nil {
		return nil, repo.Spawned, err
	}
	snapshot := &SourceBindingSnapshot{repoRoot: repoRoot, untracked: make([]string, 0, len(cut.Untracked))}
	for _, entry := range cut.Index.Entries {
		snapshot.tracked = append(snapshot.tracked, sourceBindingIndexEntry{entry.ModeString(), entry.Object, entry.StageString(), entry.Path})
	}
	for _, untracked := range cut.Untracked {
		snapshot.untracked = append(snapshot.untracked, untracked.Listed())
	}
	return snapshot, repo.Spawned, nil
}

// ProjectSourceBindingMeasured derives one project binding from the shared file
// enumeration, reading current working bytes, modes and gitlink HEADs. Its
// metrics count only this call's gitlink processes, not the snapshot's two calls.
func (snapshot *SourceBindingSnapshot) ProjectSourceBindingMeasured(projectRoot string) (string, SourceBindingMetrics, error) {
	metrics := SourceBindingMetrics{}
	runMeasured := func(root string, args ...string) (string, error) {
		metrics.SpawnedProcesses++
		return gitread.Run(root, args...)
	}
	projectRoot, projectRel, err := sourceBindingRoots(snapshot.repoRoot, projectRoot)
	if err != nil {
		return "", metrics, err
	}
	contains := func(repoPath string) bool {
		return projectRel == "." || repoPath == projectRel || strings.HasPrefix(repoPath, projectRel+"/")
	}

	records := make([]protocaps.SourceBindingFile, 0)
	seen := make(map[string]bool)
	for _, entry := range snapshot.tracked {
		if !contains(entry.path) {
			continue
		}
		if entry.stage != "0" {
			return "", metrics, fmt.Errorf("enumerate tracked project source: unmerged index stage %s for %q", entry.stage, entry.path)
		}
		record, exists, recordErr := sourcebinding.RecordThrough(indexedSourceDigest, runMeasured, snapshot.repoRoot, projectRoot, entry.path, entry.mode, entry.objectID, hostStatsExecBit)
		if recordErr != nil {
			return "", metrics, recordErr
		}
		if exists {
			records = append(records, record)
			seen[record.Path] = true
		}
	}
	for _, repoPath := range snapshot.untracked {
		if !contains(repoPath) {
			continue
		}
		record, exists, recordErr := sourcebinding.RecordThrough(indexedSourceDigest, runMeasured, snapshot.repoRoot, projectRoot, repoPath, "", "", hostStatsExecBit)
		if recordErr != nil {
			return "", metrics, recordErr
		}
		if exists && !seen[record.Path] {
			records = append(records, record)
			seen[record.Path] = true
		}
	}
	binding, err := protocaps.ComputeSourceBinding(records)
	if err != nil {
		return "", metrics, fmt.Errorf("compute project source binding: %w", err)
	}
	return binding, metrics, nil
}

func sourceBindingRoots(repoRoot, projectRoot string) (string, string, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve repository root: %w", err)
	}
	projectRoot, err = filepath.Abs(projectRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve project root: %w", err)
	}
	projectRel, err := filepath.Rel(repoRoot, projectRoot)
	if err != nil || projectRel == ".." || strings.HasPrefix(projectRel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("project root %q is outside repository root", projectRoot)
	}
	if _, err := os.Stat(projectRoot); err != nil {
		return "", "", fmt.Errorf("inspect project root: %w", err)
	}
	return projectRoot, filepath.ToSlash(projectRel), nil
}

// SourceBindingExcludes reports whether source-v1 leaves the slash-separated,
// source-root-relative path out of a project's binding. It is the protocol's
// rule, so a caller that may not import the protocol package reads it here.
func SourceBindingExcludes(path string) bool {
	return protocaps.SourceBindingExcludes(path)
}

// indexedSourceDigest answers a source file's raw SHA-256 through the
// machine-local file index (internal/fileindex), under protocols/workspace ADR
// 0009: a file whose stat is the one recorded with its digest is not read
// again. The record is the one a read would produce.
func indexedSourceDigest(absPath string, read func() ([]byte, error)) ([]byte, error) {
	return fileindex.Digest(absPath, fileindex.RawContent, read)
}
