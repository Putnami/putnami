// The artifact digests a producer publishes for its consumers.
//
// A consumer whose input port declares `fold: "artifact"` keys on the digest
// of the producer output its step binds (cachekey/artifact_inputs.go). The
// producer publishes that digest when its result is published, before any
// dependent starts: after the capture of an execution (finalizeExecutedJob),
// from the files the task left, and after the restore of a hit
// (restoreDeclaredCacheHit), from the paths, modes and content digests the
// entry's manifest recorded at capture, so a hit reads no file. Both read the
// files the capture keeps (stageDeclaredOutput and store.IngestTaskEntry): the
// subpaths the declaration cedes or preserves and the coverage shards stay out,
// and a symbolic link counts as the file it names. The build stamp stays out of
// both (cachekey.IsArtifactVersionStamp), and so do the subpaths the output
// port excludes (cachekey.PortArtifactDigest). One set of bytes therefore has
// one digest, executed or restored.
package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
)

// publishArtifactDigests publishes, at cachekey.ArtifactDigestKey, the digest
// of each output port of job a planned consumer binds as an artifact
// (ScheduledJob.ArtifactOutputs). result is the job's result, whose data reports a
// pathFrom output's path. entry is the task-owned entry a hit restored, or nil
// for an execution. A digest that cannot be read is not published: its
// consumers fold the job's key instead.
func (s *Scheduler) publishArtifactDigests(job *ScheduledJob, result *JobResult, entry *store.TaskEntry, mu *sync.Mutex, hashes map[string]string) {
	if job == nil || result == nil || len(job.ArtifactOutputs) == 0 || job.Step == nil || job.Extension == nil {
		return
	}
	task, ok := job.Extension.Tasks[job.Step.Task]
	if !ok {
		return
	}
	plans, err := cachekey.ResolveDeclaredOutputs(s.outputRoots(), job, result.Data)
	if err != nil {
		slog.Warn("artifact digest not published: cannot resolve declared outputs; its consumers fold the task's key",
			"job", job.Key(), "error", err)
		return
	}
	byID := make(map[string]cachekey.DeclaredOutputPlan, len(plans))
	for _, plan := range plans {
		byID[plan.Spec.ID] = plan
	}
	for _, port := range job.ArtifactOutputs {
		id, _, err := extension.ArtifactOutputFor(task, port)
		if err != nil {
			continue
		}
		digest, err := artifactDigest(task, port, byID, id, entry)
		if err != nil {
			slog.Warn("artifact digest not published; its consumers fold the task's key",
				"job", job.Key(), "output", port, "error", err)
			continue
		}
		mu.Lock()
		hashes[cachekey.ArtifactDigestKey(job.Key(), port)] = digest
		mu.Unlock()
	}
}

// artifactDigest is the digest of the declared output id the task's output
// port names: from entry's manifest when the entry answers
// (cachekey.ArtifactFilesFromEntry), from the tree otherwise, without the
// subpaths the port excludes. An output without a plan is a pathFrom output
// whose port reported no path, which holds nothing.
func artifactDigest(task extension.TaskDefinition, port string, plans map[string]cachekey.DeclaredOutputPlan, id string, entry *store.TaskEntry) (string, error) {
	plan, planned := plans[id]
	if !planned {
		return cachekey.ArtifactEmptyDigest, nil
	}
	if entry != nil {
		if files, ok := cachekey.ArtifactFilesFromEntry(entry, id); ok {
			return cachekey.PortArtifactDigest(task, port, plan.Spec.Kind, files), nil
		}
	}
	files, err := artifactFilesOnDisk(plan)
	if err != nil {
		return "", err
	}
	return cachekey.PortArtifactDigest(task, port, plan.Spec.Kind, files), nil
}

// artifactFilesOnDisk lists the files of a declared output as a capture of
// the tree would record them: none when the path is absent, the file of a
// file output, and every file of a directory output but the subpaths the
// declaration skips and the coverage shards.
func artifactFilesOnDisk(plan cachekey.DeclaredOutputPlan) ([]cachekey.ArtifactFile, error) {
	info, err := os.Lstat(plan.Src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if plan.Spec.Kind == extension.OutputKindFile {
		if info.IsDir() {
			return nil, fmt.Errorf("declared a file but %s is a directory", plan.Src)
		}
		if cachekey.IsArtifactVersionStamp(plan.Spec.Root, plan.Spec.Kind, plan.Spec.Path, "") {
			return nil, nil
		}
		file, err := artifactFile(plan.Src, info)
		if err != nil {
			return nil, err
		}
		return []cachekey.ArtifactFile{file}, nil
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("declared a directory but %s is not one", plan.Src)
	}
	var files []cachekey.ArtifactFile
	err = filepath.Walk(plan.Src, func(walked string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(plan.Src, walked)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if isExcludedOutputPath(plan.Excludes, slash) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() || store.UncacheableArtifact(rel) ||
			cachekey.IsArtifactVersionStamp(plan.Spec.Root, plan.Spec.Kind, plan.Spec.Path, slash) {
			return nil
		}
		file, err := artifactFile(walked, info)
		if err != nil {
			return err
		}
		file.Path = slash
		files = append(files, file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// artifactFile digests one file as the CAS records it: its content digest,
// and the permission bits of the file a symbolic link names.
func artifactFile(path string, info os.FileInfo) (cachekey.ArtifactFile, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		var err error
		if info, err = os.Stat(path); err != nil {
			return cachekey.ArtifactFile{}, err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return cachekey.ArtifactFile{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return cachekey.ArtifactFile{}, err
	}
	return cachekey.ArtifactFile{
		Mode:   uint32(info.Mode().Perm()),
		Digest: cache.DigestAlgorithm + ":" + hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// inJobSkip reports whether result is a skip the task itself reported. A skip
// the scheduler synthesizes on a task's behalf always carries its cause as the
// result's error (CapabilityAncestorSatisfied).
func inJobSkip(result *JobResult) bool {
	return result != nil && result.Status == "skipped" && result.Error == nil
}
