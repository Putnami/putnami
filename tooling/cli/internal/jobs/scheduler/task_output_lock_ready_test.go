//go:build unix

package scheduler

import (
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// genServe is a serve-workload step that reads proj's gen resource and the
// sources, the way serve-app does, and declares no output.
func genServe(name string) *ScheduledJob {
	return readerJob("proj", name, nil, extension.ResourceRef{ID: "gen"}, extension.ResourceRef{ID: extension.ResourceIDSources})
}

// TestTaskOutputLockReadyKeys pins which keys a serve-workload step releases
// once its process claimed readiness: its reader keys, and only when every
// need of the key ends by then. A finite reader keeps the key until its
// capture, and so does a serve step that also writes under it.
func TestTaskOutputLockReadyKeys(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	serving := genServe("serve~serve")
	finite := genReader("run~run")
	writing := readerJob("proj", "serve~serve-writing", map[string]extension.DeclaredOutput{
		"state": dirDeclaration(extension.OutputRootProject, ".gen/serve", false),
	}, extension.ResourceRef{ID: "gen"})
	always := func(*ScheduledJob) bool { return true }
	session := newTestOutputLocks(t, root, "session", "", nil)
	session.serveSteps = map[string]bool{serving.Key(): true, writing.Key(): true}

	project := keyNamed(t, session.keysFor([]*ScheduledJob{serving}, nil, true), "project:proj")
	if !project.ready || project.exclusive || project.preparation || project.serve {
		t.Fatalf("serve step reader key = %+v, want a shared ready key", project)
	}
	if key := keyNamed(t, session.keysFor([]*ScheduledJob{finite}, nil, true), "project:proj"); key.ready {
		t.Fatalf("finite reader key = %+v, want it held until the capture", key)
	}
	if key := keyNamed(t, session.keysFor([]*ScheduledJob{serving, finite}, nil, true), "project:proj"); key.ready {
		t.Fatalf("key = %+v, want it held until the capture a finite reader of the acquisition needs", key)
	}
	if key := keyNamed(t, session.keysFor([]*ScheduledJob{writing}, nil, true), "project:proj"); key.ready || !key.exclusive {
		t.Fatalf("key = %+v, want the writer key of a serve step held exclusively until the capture", key)
	}
	if key := keyNamed(t, session.keysFor([]*ScheduledJob{serving}, always, true), "project:proj"); !key.ready || !key.exclusive {
		t.Fatalf("key = %+v, want a preparation beside the reader held exclusively until readiness", key)
	}
	if key := keyNamed(t, session.keysFor([]*ScheduledJob{serving}, always, false), "project:proj"); !key.preparation || key.ready {
		t.Fatalf("restore key = %+v, want a preparation key: a restore reads nothing", key)
	}
}

// TestTaskOutputLockServeStepReleasesItsReaderKeyOnceReady proves that a
// serve-workload step holds its reader key through its process start, that
// another session's writer of the resource waits meanwhile, that the readiness
// claim releases the key so that writer runs beside the serve, and that the
// serve's subprocess never receives the key as held.
func TestTaskOutputLockServeStepReleasesItsReaderKeyOnceReady(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-serve-step-releases-its-reader-locks-once-ready",
		"a-serve-step-keeps-its-reader-key-until-its-ready-claim")
	spectest.Proves(t, "cli/job-planning-execution", "a-serve-step-releases-its-reader-locks-once-ready",
		"a-writer-runs-beside-a-ready-serve-step")
	spectest.Proves(t, "cli/job-planning-execution", "a-serve-step-releases-its-reader-locks-once-ready",
		"the-serve-process-never-sees-its-reader-key-held")
	root := t.TempDir()
	serving := genServe("serve~serve")
	session := newTestOutputLocks(t, root, "20261119-160000-000001", "", nil)
	session.serveSteps = map[string]bool{serving.Key(): true}
	session.serveReadyGrace = time.Hour
	project := keyNamed(t, session.keysFor([]*ScheduledJob{serving}, nil, true), "project:proj")

	hold, err := session.acquire(t.Context(), []*ScheduledJob{serving}, nil, true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if preparing := hold.value(); !strings.Contains(preparing, project.id) {
		t.Fatalf("preparation value = %q, want the reader key the preparation holds", preparing)
	}
	ctx := hold.endPreparation(t.Context())
	if got := heldOutputLocksValue(ctx); strings.Contains(got, project.id) {
		t.Fatalf("subprocess value = %q: the serve process sees its reader key held", got)
	}
	started := withServeStep(ctx)
	releaseServeOutputLocks(started)
	if probeFree(t, session, project) || !probeShared(t, session, project) {
		t.Fatal("the reader key is not held shared once the process started")
	}

	writing := newTestOutputLocks(t, root, "20261119-160000-000002", "", func(string) {})
	waiting := acquireAsync(t.Context(), writing, genWriter("build~generate"))
	requirePending(t, waiting, "another session's writer")
	releaseServeReadyLocks(ctx)
	requirePending(t, waiting, "another session's writer, after a claim outside a serve step")
	releaseServeReadyLocks(started)
	writer := awaitAcquired(t, waiting, "another session's writer once the serve was ready")
	writer.release()

	releaseServeReadyLocks(started)
	hold.release()
	if !probeFree(t, session, project) {
		t.Fatal("the reader key is still held after release")
	}
	(*outputLockHold)(nil).serveReady()
}

// TestTaskOutputLockServeStepReleasesItsReaderKeyAfterTheGrace proves that a
// serve whose process never claims readiness keeps its reader key only for
// serveReadyGrace after its process started.
func TestTaskOutputLockServeStepReleasesItsReaderKeyAfterTheGrace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-serve-step-releases-its-reader-locks-once-ready",
		"a-serve-step-without-a-ready-claim-releases-its-reader-key-after-the-grace")
	root := t.TempDir()
	serving := genServe("serve~serve")
	session := newTestOutputLocks(t, root, "20261119-160000-000003", "", nil)
	session.serveSteps = map[string]bool{serving.Key(): true}
	session.serveReadyGrace = 20 * time.Millisecond
	project := keyNamed(t, session.keysFor([]*ScheduledJob{serving}, nil, true), "project:proj")

	hold, err := session.acquire(t.Context(), []*ScheduledJob{serving}, nil, true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer hold.release()
	ctx := hold.endPreparation(t.Context())
	if probeFree(t, session, project) {
		t.Fatal("the reader key was released before the process started")
	}
	writing := newTestOutputLocks(t, root, "20261119-160000-000004", "", func(string) {})
	waiting := acquireAsync(t.Context(), writing, genWriter("build~generate"))
	releaseServeOutputLocks(withServeStep(ctx))
	awaitAcquired(t, waiting, "another session's writer once the grace passed").release()
}

// TestTaskOutputLockServeStepPreparationKeyEndsAtReadiness proves that a serve
// step whose preparation writes its project tree holds that project key
// exclusively until its readiness claim: past the preparation, since its
// reader need goes on, and no longer, since nothing else needs it.
func TestTaskOutputLockServeStepPreparationKeyEndsAtReadiness(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-serve-step-releases-its-reader-locks-once-ready",
		"a-serve-step-whose-preparation-writes-keeps-its-project-key-until-ready")
	root := t.TempDir()
	serving := genServe("serve~serve")
	always := func(*ScheduledJob) bool { return true }
	session := newTestOutputLocks(t, root, "20261119-160000-000005", "", nil)
	session.serveSteps = map[string]bool{serving.Key(): true}
	session.serveReadyGrace = time.Hour
	project := keyNamed(t, session.keysFor([]*ScheduledJob{serving}, always, true), "project:proj")

	hold, err := session.acquire(t.Context(), []*ScheduledJob{serving}, always, true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer hold.release()
	started := withServeStep(hold.endPreparation(t.Context()))
	releaseServeOutputLocks(started)
	if probeShared(t, session, project) {
		t.Fatal("the project key is not held exclusively after the preparation")
	}
	releaseServeReadyLocks(started)
	if !probeFree(t, session, project) {
		t.Fatal("the project key is still held after the readiness claim")
	}
}
