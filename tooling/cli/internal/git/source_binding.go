package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocaps "go.putnami.dev/protocol/capabilities"
	"go.putnami.dev/sdk/extension/sourcebinding"
)

// SourceBindingMetrics reports the physical child-process cost of computing a
// source binding. Enumeration requires two git ls-files calls per snapshot;
// gitlinks add the rev-parse calls needed to identify their visible worktree.
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
// tracked/untracked semantics as an individual project lookup. Stage validation
// remains project-local: a conflict in an unrelated project cannot poison every
// binding merely because the enumeration is shared.
func ReadSourceBindingSnapshot(repoRoot string) (*SourceBindingSnapshot, SourceBindingMetrics, error) {
	metrics := SourceBindingMetrics{}
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, metrics, fmt.Errorf("resolve repository root: %w", err)
	}
	metrics.SpawnedProcesses++
	tracked, err := run(repoRoot, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, metrics, fmt.Errorf("enumerate tracked project source: %w", err)
	}
	metrics.SpawnedProcesses++
	untracked, err := run(repoRoot, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, metrics, fmt.Errorf("enumerate untracked project source: %w", err)
	}
	snapshot := &SourceBindingSnapshot{repoRoot: repoRoot, untracked: splitNUL(untracked)}
	for _, entry := range splitNUL(tracked) {
		mode, objectID, stage, repoPath, ok := parseStageEntry(entry)
		if !ok {
			return nil, metrics, fmt.Errorf("enumerate tracked project source: malformed ls-files stage entry")
		}
		snapshot.tracked = append(snapshot.tracked, sourceBindingIndexEntry{mode, objectID, stage, repoPath})
	}
	return snapshot, metrics, nil
}

// ProjectSourceBindingMeasured derives one project binding from the shared file
// enumeration, reading current working bytes, modes and gitlink HEADs. Its
// metrics count only this call's gitlink processes, not the snapshot's two calls.
func (snapshot *SourceBindingSnapshot) ProjectSourceBindingMeasured(projectRoot string) (string, SourceBindingMetrics, error) {
	metrics := SourceBindingMetrics{}
	runMeasured := func(root string, args ...string) (string, error) {
		metrics.SpawnedProcesses++
		return run(root, args...)
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
		record, exists, recordErr := sourcebinding.Record(runMeasured, snapshot.repoRoot, projectRoot, entry.path, entry.mode, entry.objectID, hostStatsExecBit)
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
		record, exists, recordErr := sourcebinding.Record(runMeasured, snapshot.repoRoot, projectRoot, repoPath, "", "", hostStatsExecBit)
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

func parseStageEntry(entry string) (mode, objectID, stage, repoPath string, ok bool) {
	metadata, repoPath, found := strings.Cut(entry, "\t")
	if !found {
		return "", "", "", "", false
	}
	fields := strings.Fields(metadata)
	if len(fields) != 3 {
		return "", "", "", "", false
	}
	return fields[0], fields[1], fields[2], repoPath, true
}
