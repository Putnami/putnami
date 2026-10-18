package cachekey

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// The key never reads the policy's subject: TaskIsSourceBound only reports
// the declaration.
func TestTaskIsSourceBoundReadsTheDeclaration(t *testing.T) {
	if TaskIsSourceBound(nil) || TaskIsSourceBound(&jobmodel.ScheduledJob{}) {
		t.Fatal("a job without a definition is source-bound")
	}
	job := &jobmodel.ScheduledJob{JobDef: &extension.JobDefinition{}}
	if TaskIsSourceBound(job) {
		t.Fatal("a job without a cache policy is source-bound")
	}
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{SourceBound: true}
	if !TaskIsSourceBound(job) {
		t.Fatal("a job whose policy declares sourceBound is not source-bound")
	}
}

// An execution stores its entry at the cache key, unless the task is
// source-bound: then at the cache key bound to the binding it was stamped
// with, the empty binding of an unmanaged workspace included. A stamp that
// named no usable binding gives no key, so nothing is stored.
func TestExecutedEntryKeyBindsTheStampedBinding(t *testing.T) {
	key := strings.Repeat("c", 64)
	plain := &jobmodel.ScheduledJob{JobDef: &extension.JobDefinition{TaskCachePolicy: &extension.TaskCachePolicy{Deterministic: true}}}
	bound := &jobmodel.ScheduledJob{JobDef: &extension.JobDefinition{TaskCachePolicy: &extension.TaskCachePolicy{Deterministic: true, SourceBound: true}}}
	stamped := &jobmodel.JobResult{Status: "success", SourceBinding: "source-v1:sha256:a", SourceBindingStamped: true}

	for _, tc := range []struct {
		name   string
		job    *jobmodel.ScheduledJob
		key    string
		result *jobmodel.JobResult
		want   string
	}{
		{"a task that is not source-bound", plain, key, stamped, key},
		{"a stamped binding", bound, key, stamped, SourceBoundEntryKey(key, "source-v1:sha256:a")},
		{"the empty binding of an unmanaged workspace", bound, key, &jobmodel.JobResult{SourceBindingStamped: true}, SourceBoundEntryKey(key, "")},
		{"a stamp without a usable binding", bound, key, &jobmodel.JobResult{Status: "success"}, ""},
		{"no result", bound, key, nil, ""},
		{"no cache key", bound, "", stamped, ""},
	} {
		if got := ExecutedEntryKey(tc.job, tc.key, tc.result); got != tc.want {
			t.Errorf("%s: ExecutedEntryKey = %q, want %q", tc.name, got, tc.want)
		}
	}
	if ExecutedEntryKey(bound, key, &jobmodel.JobResult{SourceBindingStamped: true}) == key {
		t.Fatal("the empty binding stores a source-bound entry at the plain cache key")
	}
}

// The binding is never persisted in the stored result: the store key carries
// it. A stored entry's data holds no cache-private binding record.
func TestAStoredResultCarriesNoSourceBinding(t *testing.T) {
	entry := EntryResultFromJobResult(&jobmodel.JobResult{
		Status: "success", SourceBinding: "source-v1:sha256:a", SourceBindingStamped: true,
	})
	if len(entry.Data) != 0 {
		t.Fatalf("stored data = %v, want none", entry.Data)
	}
	if decoded := JobResultFromEntryResult(entry); decoded.SourceBindingStamped || decoded.SourceBinding != "" {
		t.Fatalf("a decoded entry carries a binding: (%q, %t)", decoded.SourceBinding, decoded.SourceBindingStamped)
	}
}

// A source-bound store key is a pure function of the cache key and the
// binding. Every binding, the empty one included, names its own key, and none
// of them is the cache key or its plain entry address, which is also its plain
// remote key. The derived key and its remote key are valid provider keys, so
// the wire is unchanged.
func TestSourceBoundEntryKeyBindsTheBinding(t *testing.T) {
	key := strings.Repeat("a", 64)
	bound := SourceBoundEntryKey(key, "source-v1:sha256:one")
	if again := SourceBoundEntryKey(key, "source-v1:sha256:one"); again != bound {
		t.Fatalf("the derivation is not deterministic: %s then %s", bound, again)
	}
	seen := map[string]string{bound: "the binding"}
	for name, other := range map[string]string{
		"another binding":   SourceBoundEntryKey(key, "source-v1:sha256:two"),
		"the empty binding": SourceBoundEntryKey(key, ""),
		"another cache key": SourceBoundEntryKey(strings.Repeat("b", 64), "source-v1:sha256:one"),
		"the cache key":     key,
		"the plain address": store.TaskEntryAddress(key),
	} {
		if previous, clash := seen[other]; clash {
			t.Fatalf("%s equals %s: %s", name, previous, other)
		}
		seen[other] = name
	}
	if !cache.ValidKey(bound) || !cache.ValidKey(store.RemoteTaskEntryKey(bound)) {
		t.Fatalf("derived key %q or its remote key is not a valid provider key", bound)
	}
}

// Two sessions at the same cache key with different bindings never meet in
// the store. A lease at one binding does not block a claim at the other, the
// entry one publishes is not found at the other's key or at the cache key, and
// a waiter at the other binding is never told it was published.
func TestSourceBoundEntriesAtTwoBindingsNeverMeet(t *testing.T) {
	s := store.NewLocalStore(t.TempDir())
	key := strings.Repeat("c", 64)
	first := SourceBoundEntryKey(key, "source-v1:sha256:first")
	second := SourceBoundEntryKey(key, "source-v1:sha256:second")

	winner, release := s.TryClaimTaskEntry(first)
	if !winner {
		t.Fatal("the first claimant lost")
	}
	other, otherRelease := s.TryClaimTaskEntry(second)
	if !other {
		t.Fatal("a lease at one binding blocked a claim at another binding")
	}
	otherRelease()
	release()

	staging := t.TempDir()
	output := store.DeclaredEntryOutput{
		ID: "evidence", Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: "evidence.json",
	}
	staged := store.TaskStagingPath(staging, output)
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestTaskEntry(staging, store.TaskEntrySpec{
		Key: first, Result: &store.EntryResult{Status: "success"}, Outputs: []store.DeclaredEntryOutput{output},
	}); err != nil {
		t.Fatalf("ingest at the first binding: %v", err)
	}
	if entry, err := s.LookupTaskEntry(first); err != nil || entry == nil || entry.Key != first {
		t.Fatalf("the entry is not found at its own binding: %+v, %v", entry, err)
	}
	for name, lookup := range map[string]string{"the other binding": second, "the cache key": key} {
		if entry, err := s.LookupTaskEntry(lookup); err != nil || entry != nil {
			t.Fatalf("%s found the entry published at another binding: %+v, %v", name, entry, err)
		}
	}
	if err := s.WaitForTaskEntry(second, 50*time.Millisecond); !errors.Is(err, store.ErrLeaseExpired) {
		t.Fatalf("a waiter at the other binding = %v, want ErrLeaseExpired and no publication", err)
	}
}
