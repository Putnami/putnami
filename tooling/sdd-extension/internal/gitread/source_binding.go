package gitread

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocaps "go.putnami.dev/protocol/capabilities"
	"go.putnami.dev/sdk/extension/sourcebinding"
)

// ProjectSourceBinding computes the source-v1 binding for one exact project
// root in the current worktree. Git supplies the tracked and non-ignored
// untracked input set; working bytes win over index bytes and deleted files are
// absent. The capabilities protocol owns exclusions, canonical ordering, and
// hashing.
//
// Copied from tooling/cli/internal/git/source_binding.go. The measured variant
// core also exports is omitted: no SDD engine reads the subprocess count. What
// one path contributes is not copied: both read it through the SDK's
// sourcebinding package, which also decides where a regular file's executable
// bit comes from on a host that does not store one.
func ProjectSourceBinding(repoRoot, projectRoot string) (string, error) {
	return projectSourceBinding(repoRoot, projectRoot, sourcebinding.HostStatsExecBit)
}

// projectSourceBinding is ProjectSourceBinding for a host that stores the
// executable bit (statsExecBit) or does not.
func projectSourceBinding(repoRoot, projectRoot string, statsExecBit bool) (string, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	projectRoot, err = filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	projectRel, err := filepath.Rel(repoRoot, projectRoot)
	if err != nil || projectRel == ".." || strings.HasPrefix(projectRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("project root %q is outside repository root", projectRoot)
	}
	if _, err := os.Stat(projectRoot); err != nil {
		return "", fmt.Errorf("inspect project root: %w", err)
	}

	pathspec := ":(literal)" + filepath.ToSlash(projectRel)
	tracked, err := run(repoRoot, "ls-files", "--stage", "-z", "--", pathspec)
	if err != nil {
		return "", fmt.Errorf("enumerate tracked project source: %w", err)
	}
	untracked, err := run(repoRoot, "ls-files", "--others", "--exclude-standard", "-z", "--", pathspec)
	if err != nil {
		return "", fmt.Errorf("enumerate untracked project source: %w", err)
	}

	records := make([]protocaps.SourceBindingFile, 0)
	seen := make(map[string]bool)
	for _, entry := range splitNUL(tracked) {
		mode, objectID, stage, repoPath, ok := parseStageEntry(entry)
		if !ok {
			return "", fmt.Errorf("enumerate tracked project source: malformed ls-files stage entry")
		}
		if stage != "0" {
			return "", fmt.Errorf("enumerate tracked project source: unmerged index stage %s for %q", stage, repoPath)
		}
		record, exists, recordErr := sourcebinding.Record(run, repoRoot, projectRoot, repoPath, mode, objectID, statsExecBit)
		if recordErr != nil {
			return "", recordErr
		}
		if exists {
			records = append(records, record)
			seen[record.Path] = true
		}
	}
	for _, repoPath := range splitNUL(untracked) {
		record, exists, recordErr := sourcebinding.Record(run, repoRoot, projectRoot, repoPath, "", "", statsExecBit)
		if recordErr != nil {
			return "", recordErr
		}
		if exists && !seen[record.Path] {
			records = append(records, record)
			seen[record.Path] = true
		}
	}
	binding, err := protocaps.ComputeSourceBinding(records)
	if err != nil {
		return "", fmt.Errorf("compute project source binding: %w", err)
	}
	return binding, nil
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
