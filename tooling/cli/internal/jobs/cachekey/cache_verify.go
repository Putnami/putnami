package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// VerificationOutput is one task declaration resolved to a concrete root and
// slash-form relative path. It is a read-only projection for cache verify; the
// declaration and cache-entry vocabulary remain unchanged.
type VerificationOutput struct {
	Root string
	Path string
}

// VerificationContract is the declared evidence cache verify needs without
// duplicating the scheduler's cache-key and path-resolution rules.
type VerificationContract struct {
	Ambient        bool
	Deterministic  bool
	MutatesSources bool
	ProjectInputs  []string
	Outputs        []VerificationOutput
}

// VerificationContractOf projects the exact cache/key/output declarations the
// scheduler used for job. Dynamic pathFrom outputs are resolved from result.
func VerificationContractOf(
	ws *workspace.Workspace,
	job *jobmodel.ScheduledJob,
	result *jobmodel.JobResult,
) (VerificationContract, error) {
	contract := VerificationContract{}
	if ws == nil || job == nil || job.JobDef == nil {
		return contract, fmt.Errorf("task is incomplete")
	}
	policy := job.JobDef.TaskCachePolicy
	contract.Deterministic = policy != nil && policy.Deterministic
	contract.ProjectInputs, _ = KeyFilePatterns(ws, job, nil)
	contract.MutatesSources = taskMutatesSources(job)
	contract.Ambient = cacheKeyUsesAmbientInputs(ws, job)
	if jobmodel.TaskDeclarationOf(job) == nil {
		return contract, nil
	}
	plans, err := ResolveDeclaredOutputs(WorkspaceRoots{Root: ws.Root}, job, nil)
	if result != nil {
		plans, err = ResolveDeclaredOutputs(WorkspaceRoots{Root: ws.Root}, job, result.Data)
	}
	if err != nil {
		return contract, err
	}
	contract.Outputs = make([]VerificationOutput, 0, len(plans))
	for _, plan := range plans {
		contract.Outputs = append(contract.Outputs, VerificationOutput{
			Root: plan.Spec.EffectiveRoot(), Path: plan.Spec.Path,
		})
	}
	return contract, nil
}

type VerificationTreeSnapshot map[string]string

func SnapshotJobWorkspaceTree(ws *workspace.Workspace, job *jobmodel.ScheduledJob) (VerificationTreeSnapshot, error) {
	if ws == nil || job == nil || job.Project == nil {
		return nil, fmt.Errorf("cache verify: task has no project root")
	}
	return snapshotVerificationTree(ws.Root)
}

func snapshotVerificationTree(root string) (VerificationTreeSnapshot, error) {
	snapshot := make(VerificationTreeSnapshot)
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if ProjectWalkExcludedDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, _ = fmt.Fprintf(hash, "%s\x00", info.Mode().String())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			_, _ = hash.Write([]byte(target))
		} else if info.Mode().IsRegular() {
			data, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			_, _ = hash.Write(data)
		} else {
			return nil
		}
		snapshot[filepath.ToSlash(rel)] = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	return snapshot, err
}

func DiffVerificationTreeSnapshots(before, after VerificationTreeSnapshot) []string {
	changed := make(map[string]bool)
	for path, digest := range before {
		if after[path] != digest {
			changed[path] = true
		}
	}
	for path, digest := range after {
		if before[path] != digest {
			changed[path] = true
		}
	}
	paths := make([]string, 0, len(changed))
	for path := range changed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
