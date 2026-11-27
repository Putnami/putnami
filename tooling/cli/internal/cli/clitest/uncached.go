package clitest

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// readSessions holds each session RequireUncachedRun has read, by workspace
// root and session ID.
var readSessions sync.Map

// RequireUncachedRun fails t unless the run of verb that just ended in the
// workspace at root used no Putnami cache entry: no task carries a cache key,
// none was reused, and the local cache leg counted no hit and no miss. It
// reads the session record rather than the cache summary line, which the
// summary prints only once the local leg measured a millisecond of work: a
// run that computes its source bindings faster prints no line at all. The
// latest session must be one this helper has not read yet and must have run
// verb alone, so a session an earlier run left is never taken for this one.
func RequireUncachedRun(t *testing.T, root, verb, out string) {
	t.Helper()
	id := LatestSessionID(t, root)
	if _, seen := readSessions.LoadOrStore(filepath.Join(root, id), true); seen {
		t.Fatalf("the latest session %s is one an earlier run left: this run recorded none:\n%s", id, out)
	}
	session, _ := ReadPortableSession(t, root, id)
	if !slices.Equal(session.Commands, []string{verb}) {
		t.Fatalf("the latest session %s ran %v, want %s:\n%s", id, session.Commands, verb, out)
	}
	if len(session.Tasks) == 0 {
		t.Errorf("the session recorded no task:\n%s", out)
	}
	for _, task := range session.Tasks {
		// A task whose cache is off has no key: nothing is looked up or
		// stored under one.
		if task.InputDigest != "" || task.Reuse != protocolcli.TaskReuseNone {
			t.Errorf("%s has key %q and reuse %q, want no key and no reuse:\n%s",
				task.Identity.Key, task.InputDigest, task.Reuse, out)
		}
	}
	if cache := session.Run.Cache; cache != nil && cache.Local != nil && (cache.Local.Hits != 0 || cache.Local.Misses != 0) {
		t.Errorf("the run looked up Putnami cache entries for delegated tasks: %d hit, %d miss:\n%s",
			cache.Local.Hits, cache.Local.Misses, out)
	}
}
