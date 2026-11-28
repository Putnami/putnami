package scheduler

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// artifactProducer is a generate job of project app whose gen output, the
// project's .gen directory, one consumer binds as an artifact. The directory
// cedes .gen/ceded to another task.
func artifactProducer(root string) (*Scheduler, *ScheduledJob) {
	task := extension.TaskDefinition{
		Kind: "command", Command: "generate",
		Outputs: map[string]extension.TaskOutputPort{"gen": {}},
		Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
			"gen": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen", Excludes: []string{".gen/ceded"}},
		}},
	}
	job := &ScheduledJob{
		Project:         &workspace.Project{ID: "/app", Name: "app", Path: "app"},
		Extension:       &extension.ExtensionDescription{Name: "@test/ts", Tasks: map[string]extension.TaskDefinition{"generate": task}},
		JobDef:          &extension.JobDefinition{Name: "test~generate", ExtensionName: "@test/ts", Cache: true},
		Step:            &extension.PipelineStep{ID: "generate", Task: "generate"},
		ArtifactOutputs: []string{"gen"},
	}
	return &Scheduler{ws: &workspace.Workspace{Root: root}}, job
}

func writeArtifactFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// captureArtifactEntry stores the job's declared outputs as an execution's
// capture would, through stageDeclaredOutput and the store's ingest.
func captureArtifactEntry(t *testing.T, s *Scheduler, job *ScheduledJob) *store.TaskEntry {
	t.Helper()
	plans, err := cachekey.ResolveDeclaredOutputs(s.outputRoots(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	specs := make([]store.DeclaredEntryOutput, 0, len(plans))
	for _, plan := range plans {
		if err := stageDeclaredOutput(plan, store.TaskStagingPath(staging, plan.Spec)); err != nil {
			t.Fatal(err)
		}
		specs = append(specs, plan.Spec)
	}
	entry, err := store.NewLocalStore(t.TempDir()).IngestTaskEntry(staging, store.TaskEntrySpec{
		Key:     "artifact-key",
		Result:  &store.EntryResult{Status: "success"},
		Outputs: specs,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return entry
}

func publishedArtifact(t *testing.T, s *Scheduler, job *ScheduledJob, entry *store.TaskEntry) string {
	t.Helper()
	hashes := map[string]string{}
	s.publishArtifactDigests(job, &JobResult{}, entry, &sync.Mutex{}, hashes)
	digest, ok := hashes[cachekey.ArtifactDigestKey(job.Key(), "gen")]
	if !ok || len(hashes) != 1 {
		t.Fatalf("published = %v, want the gen digest alone", hashes)
	}
	return digest
}

// An execution digests the files the task left, as the capture keeps them,
// and a hit digests what its entry recorded at capture: one set of bytes has
// one digest either way. A ceded subpath, a coverage shard and the build stamp
// are no part of it: the stamp the scheduler rewrites at every run moves
// neither digest. The hit reads the recorded content digests, not the files:
// an edit the entry does not hold leaves the hit's digest. An entry that lists
// no files is answered from the tree.
func TestArtifactDigests_AHitPublishesTheDigestItsEntryRecorded(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-hit-publishes-the-artifact-digest-its-entry-recorded")
	root := t.TempDir()
	s, job := artifactProducer(root)
	gen := filepath.Join(root, "app", ".gen")
	writeArtifactFile(t, filepath.Join(gen, "client.ts"), "export const a = 1\n")
	writeArtifactFile(t, filepath.Join(gen, "nested", "types.ts"), "export type A = string\n")
	writeArtifactFile(t, filepath.Join(gen, "version.json"), `{"revision":"run-1"}`)

	executed := publishedArtifact(t, s, job, nil)
	writeArtifactFile(t, filepath.Join(gen, "ceded", "other.json"), "{}")
	writeArtifactFile(t, filepath.Join(gen, "lcov.info.1.tmp"), "shard")
	if again := publishedArtifact(t, s, job, nil); again != executed {
		t.Fatalf("a ceded subpath or a coverage shard moved the digest: %s -> %s", executed, again)
	}

	entry := captureArtifactEntry(t, s, job)
	if hit := publishedArtifact(t, s, job, entry); hit != executed {
		t.Fatalf("the hit's digest %s is not the execution's %s for the same bytes", hit, executed)
	}

	// The scheduler rewrites the stamp at every run: no digest reads it.
	writeArtifactFile(t, filepath.Join(gen, "version.json"), `{"revision":"run-2"}`)
	if restamped := publishedArtifact(t, s, job, nil); restamped != executed {
		t.Fatalf("a rewritten stamp moved the execution's digest: %s -> %s", executed, restamped)
	}
	if hit := publishedArtifact(t, s, job, entry); hit != executed {
		t.Fatalf("a rewritten stamp moved the hit's digest: %s -> %s", executed, hit)
	}
	if err := os.Remove(filepath.Join(gen, "version.json")); err != nil {
		t.Fatal(err)
	}
	if unstamped := publishedArtifact(t, s, job, nil); unstamped != executed {
		t.Fatalf("a missing stamp moved the execution's digest: %s -> %s", executed, unstamped)
	}

	// Any other file is read from the record, never hashed again.
	writeArtifactFile(t, filepath.Join(gen, "client.ts"), "export const a = 2\n")
	if hit := publishedArtifact(t, s, job, entry); hit != executed {
		t.Fatalf("the hit re-hashed a recorded file: %s, want the recorded %s", hit, executed)
	}
	onDisk := publishedArtifact(t, s, job, nil)
	if onDisk == executed {
		t.Fatal("an execution's digest kept a content edit")
	}

	withoutManifest := *entry
	withoutManifest.Manifest = nil
	if got := publishedArtifact(t, s, job, &withoutManifest); got != onDisk {
		t.Fatalf("an entry without a manifest = %s, want the tree's digest %s", got, onDisk)
	}
}

// An output that holds nothing, absent or an empty directory, publishes the
// empty digest, and so does an entry that records it empty. A job no consumer
// binds publishes nothing.
func TestArtifactDigests_AnEmptyOutputPublishesTheEmptyDigest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-hit-publishes-the-artifact-digest-its-entry-recorded")
	root := t.TempDir()
	s, job := artifactProducer(root)
	task := job.Extension.Tasks["generate"]
	gen := task.Declares.Outputs["gen"]
	gen.OptionalEmpty = true
	task.Declares.Outputs["gen"] = gen
	job.Extension.Tasks["generate"] = task

	if got := publishedArtifact(t, s, job, nil); got != cachekey.ArtifactEmptyDigest {
		t.Fatalf("an absent output = %s, want %s", got, cachekey.ArtifactEmptyDigest)
	}
	if err := os.MkdirAll(filepath.Join(root, "app", ".gen", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := publishedArtifact(t, s, job, nil); got != cachekey.ArtifactEmptyDigest {
		t.Fatalf("an output without a file = %s, want %s", got, cachekey.ArtifactEmptyDigest)
	}
	if got := publishedArtifact(t, s, job, captureArtifactEntry(t, s, job)); got != cachekey.ArtifactEmptyDigest {
		t.Fatalf("an entry that records the output empty = %s, want %s", got, cachekey.ArtifactEmptyDigest)
	}

	job.ArtifactOutputs = nil
	hashes := map[string]string{}
	s.publishArtifactDigests(job, &JobResult{}, nil, &sync.Mutex{}, hashes)
	if len(hashes) != 0 {
		t.Fatalf("a job no consumer binds published %v", hashes)
	}
}

// A job whose artifact a consumer binds is never served status-only: its
// consumer reads the files.
func TestArtifactDigests_AStatusOnlyHitDoesNotServeAnArtifact(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-hit-publishes-the-artifact-digest-its-entry-recorded")
	s, job := artifactProducer(t.TempDir())
	recorded := &store.ResultOnlyTaskEntry{Result: &store.EntryResult{Status: "success"}}
	if got := s.serveStatusOnlyHit(job, "key", recorded, ReuseLocalCache, &sync.Mutex{}, map[string]string{}); got != nil {
		t.Fatalf("a status-only hit served a job whose artifact a consumer binds: %+v", got)
	}
}
