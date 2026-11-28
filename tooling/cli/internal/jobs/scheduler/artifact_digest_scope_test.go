package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// A directory artifact's digest reads a file's executable bit alone, on an
// execution and on a hit: the bytes a producer writes under another umask
// keep both digests, and the two stay one.
func TestArtifactDigests_APermissionBitOtherThanExecuteMovesNoDigest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-hit-publishes-the-artifact-digest-its-entry-recorded")
	root := t.TempDir()
	s, job := artifactProducer(root)
	client := filepath.Join(root, "app", ".gen", "client.ts")
	writeArtifactFile(t, client, "export const a = 1\n")
	chmod := func(mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(client, mode); err != nil {
			t.Fatal(err)
		}
	}
	chmod(0o644)
	executed := publishedArtifact(t, s, job, nil)
	for _, mode := range []os.FileMode{0o600, 0o664, 0o640} {
		chmod(mode)
		if got := publishedArtifact(t, s, job, nil); got != executed {
			t.Fatalf("mode %o moved the execution's digest: %s -> %s", mode, executed, got)
		}
		if hit := publishedArtifact(t, s, job, captureArtifactEntry(t, s, job)); hit != executed {
			t.Fatalf("mode %o: the hit's digest %s is not the execution's %s", mode, hit, executed)
		}
	}
	chmod(0o755)
	if publishedArtifact(t, s, job, nil) == executed {
		t.Fatal("the executable bit kept the execution's digest")
	}
}

// The artifact a port names leaves the subpaths the port excludes out of its
// digest, executed and restored: an edit of the feature evidence alone keeps
// the digest the test steps fold, and an edit of another file moves it. The
// evidence is still captured with the output.
func TestArtifactDigests_AnExcludedSubpathMovesNoDigest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-hit-publishes-the-artifact-digest-its-entry-recorded")
	root := t.TempDir()
	s, job := artifactProducer(root)
	task := job.Extension.Tasks["generate"]
	task.Outputs["gen"] = extension.TaskOutputPort{ArtifactExcludes: []string{"feature-evidence"}}
	job.Extension.Tasks["generate"] = task
	gen := filepath.Join(root, "app", ".gen")
	evidence := filepath.Join(gen, "feature-evidence", "go-framework.json")
	writeArtifactFile(t, filepath.Join(gen, "openapi.json"), "{}")
	writeArtifactFile(t, evidence, `{"sourceBinding":"one"}`)

	executed := publishedArtifact(t, s, job, nil)
	entry := captureArtifactEntry(t, s, job)
	if hit := publishedArtifact(t, s, job, entry); hit != executed {
		t.Fatalf("the hit's digest %s is not the execution's %s", hit, executed)
	}
	captured := false
	for _, file := range entry.Manifest.Files {
		captured = captured || file.Path == "gen/feature-evidence/go-framework.json"
	}
	if !captured {
		t.Fatalf("the entry does not capture the excluded evidence: %+v", entry.Manifest.Files)
	}

	writeArtifactFile(t, evidence, `{"sourceBinding":"two"}`)
	if got := publishedArtifact(t, s, job, nil); got != executed {
		t.Fatalf("an evidence edit moved the execution's digest: %s -> %s", executed, got)
	}
	if hit := publishedArtifact(t, s, job, captureArtifactEntry(t, s, job)); hit != executed {
		t.Fatalf("an evidence edit moved the hit's digest: %s -> %s", executed, hit)
	}
	writeArtifactFile(t, filepath.Join(gen, "openapi.json"), `{"paths":{}}`)
	if publishedArtifact(t, s, job, nil) == executed {
		t.Fatal("an edit outside the excluded subpath kept the digest")
	}
}

// A skip the task reports itself publishes the digest of the outputs it left,
// as a success does, even when its result is not cacheable: its consumer keys
// on the bytes it reads, never on a fixed value. A task that is not
// deterministic stores no entry for the skip and publishes no key, so no later
// run replays what it wrote (Go's unresolved test identity).
func TestArtifactDigests_AnUncachedSkipPublishesItsDigestAndStoresNothing(t *testing.T) {
	declaration := &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
		"manifest": {Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: "out/identity.json", OptionalEmpty: true},
	}}
	job := declaredJob("test~test-identity", "identity", declaration)
	task := job.Extension.Tasks["identity"]
	task.Outputs = map[string]extension.TaskOutputPort{"manifest": {}}
	job.Extension.Tasks["identity"] = task
	job.ArtifactOutputs = []string{"manifest"}
	f := newCaptureFixture(t, job)
	content := `{"unresolved":"drawn"}`
	writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, "out", "identity.json"), content)

	var mu sync.Mutex
	hashes := map[string]string{}
	f.sched.finalizeExecutedJob(t.Context(), job, &JobResult{Status: "skipped"}, true, "identity-key", "", nil, &mu, hashes)

	sum := sha256.Sum256([]byte(content))
	want := "artifact-v1:file:sha256:" + hex.EncodeToString(sum[:])
	if got := hashes[cachekey.ArtifactDigestKey(job.Key(), "manifest")]; got != want {
		t.Fatalf("published digest = %q, want the digest of the manifest the skip left %q", got, want)
	}
	if _, published := hashes[job.Key()]; published {
		t.Fatal("an uncacheable skip published its key")
	}
	if entry, err := f.cache.LookupTaskEntry("identity-key"); err == nil && entry != nil {
		t.Fatalf("an uncacheable skip stored an entry: %+v", entry)
	}

	// A skip the scheduler synthesized carries its cause and publishes nothing.
	synthesized := map[string]string{}
	f.sched.finalizeExecutedJob(t.Context(), job, &JobResult{Status: "skipped", Error: &JobError{Message: "not needed"}},
		true, "identity-key", "", nil, &mu, synthesized)
	if len(synthesized) != 0 {
		t.Fatalf("a synthesized skip published %v", synthesized)
	}
}

// SourceBoundEntryKeys resolves a store key as the scheduler's own lookup
// does: the key itself for a task that is not source-bound, and for one that
// is, the key bound to its project's current binding, the empty one in a
// workspace Git does not manage.
func TestSourceBoundEntryKeysResolveAsTheLookupDoes(t *testing.T) {
	plain := declaredJob("test~generate", "generate", &extension.TaskDeclaration{})
	bound := declaredJob("test~describe", "describe", &extension.TaskDeclaration{})
	bound.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{SourceBound: true}
	f := newCaptureFixture(t, plain, bound)
	resolve := SourceBoundEntryKeys(f.ws)
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, job := range []*ScheduledJob{plain, bound} {
		got, ok := resolve(job, key)
		want, wantOK := f.sched.lookupEntryKey(job, key)
		if got != want || ok != wantOK {
			t.Errorf("%s: SourceBoundEntryKeys = %q %v, want the lookup's %q %v", job.Key(), got, ok, want, wantOK)
		}
	}
	if got, _ := resolve(plain, key); got != key {
		t.Errorf("a task that is not source-bound resolved %q, want its key", got)
	}
	if got, _ := resolve(bound, key); got != cachekey.SourceBoundEntryKey(key, "") {
		t.Errorf("a source-bound task of an unmanaged workspace resolved %q, want its key bound to the empty binding", got)
	}
}
