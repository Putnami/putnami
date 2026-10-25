//go:build unix

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/cli/model/workspace"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
)

// newTestOutputLocks returns a manager for one simulated session. Two managers
// over one root exclude each other exactly like two processes: flock(2) keys on
// the open file description, and each acquisition opens its own.
func newTestOutputLocks(t *testing.T, root, session, inherited string, notify func(string)) *taskOutputLocks {
	t.Helper()
	m := newTaskOutputLocks(root, outputLockHolder{Session: session, PID: os.Getpid()}, inherited, notify)
	if m == nil {
		t.Fatal("newTaskOutputLocks returned nil on a flock host")
	}
	return m
}

// probeFree reports whether key's lock file can be locked right now, without
// waiting, and releases it again at once.
func probeFree(t *testing.T, m *taskOutputLocks, key outputLockKey) bool {
	t.Helper()
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := flock.Acquire(filepath.Join(m.dir, key.id+".lock"), true, true)
	if errors.Is(err, flock.ErrBusy) {
		return false
	}
	if err != nil {
		t.Fatalf("probe %s: %v", key.name, err)
	}
	_ = lock.Release()
	return true
}

func keyNamed(t *testing.T, keys []outputLockKey, name string) outputLockKey {
	t.Helper()
	for _, key := range keys {
		if key.name == name {
			return key
		}
	}
	t.Fatalf("no key %q in %v", name, keys)
	return outputLockKey{}
}

// waitMessage waits for a wait line that MUST arrive. The bound only detects a
// hang (answerWaitBudget); the synchronization is the channel.
func waitMessage(t *testing.T, messages <-chan string, what string) string {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(answerWaitBudget):
		t.Fatalf("no wait line: %s", what)
		return ""
	}
}

func TestTaskOutputLockNames(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		job  *ScheduledJob
		want []string
	}{
		{name: "nil job", job: nil, want: nil},
		{
			name: "a task without a declaration locks its command output",
			job:  outputLockJob("tooling/cli", "test~test", nil),
			want: []string{"command-output:tooling/cli/test"},
		},
		{
			name: "the root project locks the root command output",
			job:  outputLockJob("", "lint~check", nil),
			want: []string{"command-output:lint"},
		},
		{
			name: "command-output declarations stay inside the command-output key",
			job: outputLockJob("proj", "test~exec", map[string]extension.DeclaredOutput{
				"coverage": fileDeclaration(extension.OutputRootCommandOutput, "coverage.out", true),
			}),
			want: []string{"command-output:proj/test"},
		},
		{
			name: "project-rooted literal and pathFrom outputs share one project key",
			job: outputLockJob("proj", "build~generate", map[string]extension.DeclaredOutput{
				"gen":    dirDeclaration(extension.OutputRootProject, ".gen", false),
				"client": {Kind: extension.OutputKindDirectory, PathFrom: "clientOutput", OptionalEmpty: true},
				"schema": {Kind: extension.OutputKindFile, Path: "schema/config.json", OptionalEmpty: true},
			}),
			want: []string{"command-output:proj/build", "project:proj"},
		},
		{
			name: "a workspace-rooted output adds the workspace key",
			job: outputLockJob("proj", "build~infra", map[string]extension.DeclaredOutput{
				"manifest": fileDeclaration(extension.OutputRootWorkspace, "infra.json", true),
			}),
			want: []string{"command-output:proj/build", "workspace:"},
		},
		{
			name: "invocation-scoped outputs take no key",
			job: outputLockJob("proj", "test~env-up", map[string]extension.DeclaredOutput{
				"lease": {Kind: extension.OutputKindFile, Scope: extensionproto.OutputScopeInvocation, Path: "database/lease.json"},
			}),
			want: []string{"command-output:proj/test"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskOutputLockNames(tc.job); !slices.Equal(got, tc.want) {
				t.Fatalf("taskOutputLockNames = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTaskOutputLockKeysAreSortedDedupedAndWorkspaceBound pins the key space:
// one global order whatever order the jobs arrive in, one key per shared path,
// the same ids for two spellings of one workspace, other ids for another.
func TestTaskOutputLockKeysAreSortedDedupedAndWorkspaceBound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := newTestOutputLocks(t, root, "s", "", nil)
	jobs := []*ScheduledJob{
		outputLockJob("a", "test~exec", nil),
		outputLockJob("b", "test~exec", nil),
		outputLockJob("a", "test~merge", map[string]extension.DeclaredOutput{
			"merged": fileDeclaration(extension.OutputRootProject, ".gen/conf/merged.yaml", true),
		}),
	}
	forward := m.keysFor(jobs, nil, false)
	backward := m.keysFor([]*ScheduledJob{jobs[2], jobs[1], jobs[0]}, nil, false)
	if len(forward) != 3 {
		t.Fatalf("keys = %v, want command-output:a/test, command-output:b/test and project:a once each", forward)
	}
	if !slices.Equal(forward, backward) {
		t.Fatalf("key order depends on job order:\n%v\n%v", forward, backward)
	}
	if !slices.IsSortedFunc(forward, outputLockOrder) {
		t.Fatalf("keys are not in the global lock order: %v", forward)
	}
	if last := forward[len(forward)-1]; last.name != "project:a" {
		t.Fatalf("keys = %v, want the project key after every command-output key", forward)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	aliased := newTestOutputLocks(t, link, "s", "", nil).keysFor(jobs, nil, false)
	if !slices.Equal(forward, aliased) {
		t.Fatalf("a symlinked spelling of the workspace derived other ids:\n%v\n%v", forward, aliased)
	}
	other := newTestOutputLocks(t, t.TempDir(), "s", "", nil).keysFor(jobs, nil, false)
	for i := range other {
		if other[i].id == keyNamed(t, forward, other[i].name).id {
			t.Fatalf("another workspace derived the same id for %s", other[i].name)
		}
	}
}

// TestTaskOutputLockAcquiresInOneGlobalOrder observes the acquisition order on
// the lock files themselves. While the acquisition waits on the middle key,
// every key before it in id order is already held and every key after it is
// still free.
func TestTaskOutputLockAcquiresInOneGlobalOrder(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-output-exclusion",
		"locks-are-taken-in-one-global-order")
	root := t.TempDir()
	jobs := []*ScheduledJob{
		outputLockJob("p1", "test~exec", nil),
		outputLockJob("p2", "test~exec", nil),
		outputLockJob("p3", "test~exec", nil),
	}
	holder := newTestOutputLocks(t, root, "holder", "", nil)
	keys := holder.keysFor(jobs, nil, false)
	if len(keys) != 3 {
		t.Fatalf("keys = %v, want three", keys)
	}
	var middle *ScheduledJob
	for _, job := range jobs {
		if taskOutputLockNames(job)[0] == keys[1].name {
			middle = job
		}
	}
	middleHold, err := holder.acquire(t.Context(), []*ScheduledJob{middle}, nil, false)
	if err != nil {
		t.Fatalf("hold middle key: %v", err)
	}

	messages := make(chan string, 1)
	waiter := newTestOutputLocks(t, root, "waiter", "", func(message string) { messages <- message })
	done := make(chan error, 1)
	var all *outputLockHold
	go func() {
		var err error
		// Reverse declaration order: the manager must sort.
		all, err = waiter.acquire(t.Context(), []*ScheduledJob{jobs[2], jobs[1], jobs[0]}, nil, false)
		done <- err
	}()

	waitMessage(t, messages, "the waiter never reported the busy middle key")
	if probeFree(t, holder, keys[0]) {
		t.Error("the first key in id order was not held while the waiter waited on the second")
	}
	if !probeFree(t, holder, keys[2]) {
		t.Error("the last key in id order was taken before the second was acquired")
	}
	middleHold.release()
	if err := <-done; err != nil {
		t.Fatalf("waiter acquire: %v", err)
	}
	for _, key := range keys {
		if probeFree(t, holder, key) {
			t.Errorf("%s is free after the waiter acquired every key", key.name)
		}
	}
	all.release()
	for _, key := range keys {
		if !probeFree(t, holder, key) {
			t.Errorf("%s is still held after release", key.name)
		}
	}
}

// TestTaskOutputLockWaiterHoldsNoWiderKey pins the class order: a session that
// waits for another session's command output holds no project key meanwhile,
// so a third session's preparation in that project does not wait behind an
// execution it does not share.
func TestTaskOutputLockWaiterHoldsNoWiderKey(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-output-exclusion",
		"locks-are-taken-in-one-global-order")
	root := t.TempDir()
	running := outputLockJob("proj", "test~exec", nil)
	generating := outputLockJob("proj", "test~exec", map[string]extension.DeclaredOutput{
		"gen": fileDeclaration(extension.OutputRootProject, ".gen/conf/merged.yaml", true),
	})
	holder := newTestOutputLocks(t, root, "holder", "", nil)
	hold, err := holder.acquire(t.Context(), []*ScheduledJob{running}, nil, false)
	if err != nil {
		t.Fatalf("hold the command output: %v", err)
	}

	messages := make(chan string, 1)
	waiter := newTestOutputLocks(t, root, "waiter", "", func(message string) { messages <- message })
	keys := waiter.keysFor([]*ScheduledJob{generating}, nil, false)
	done := make(chan error, 1)
	var waited *outputLockHold
	go func() {
		var err error
		waited, err = waiter.acquire(t.Context(), []*ScheduledJob{generating}, nil, false)
		done <- err
	}()

	waitMessage(t, messages, "the waiter never reported the busy command output")
	if !probeFree(t, holder, keyNamed(t, keys, "project:proj")) {
		t.Error("the waiter holds the project key while it waits for another session's command output")
	}
	hold.release()
	if err := <-done; err != nil {
		t.Fatalf("waiter acquire: %v", err)
	}
	waited.release()
}

// TestTaskOutputLockWaitLineNamesTheHolder pins the contention message: the
// holder's session, pid and task, the location it writes, and exactly one line
// for the whole wait.
func TestTaskOutputLockWaitLineNamesTheHolder(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-output-exclusion",
		"the-wait-names-the-holder-and-ends-on-cancellation")
	root := t.TempDir()
	job := outputLockJob("tooling/cli", "test~test", nil)
	holder := newTestOutputLocks(t, root, "20260925-192012-876307", "", nil)
	hold, err := holder.acquire(t.Context(), []*ScheduledJob{job}, nil, false)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	var mu sync.Mutex
	var lines []string
	first := make(chan string, 1)
	waiter := newTestOutputLocks(t, root, "20260925-192013-000001", "", func(message string) {
		mu.Lock()
		lines = append(lines, message)
		mu.Unlock()
		select {
		case first <- message:
		default:
		}
	})
	done := make(chan error, 1)
	go func() {
		waiterHold, err := waiter.acquire(t.Context(), []*ScheduledJob{job}, nil, false)
		waiterHold.release()
		done <- err
	}()

	message := waitMessage(t, first, "the waiter never reported the holder")
	for _, want := range []string{
		"/tooling/cli:test~test waits for session 20260925-192012-876307",
		fmt.Sprintf("pid %d", os.Getpid()),
		"task /tooling/cli:test~test",
		".putnami/out/tooling/cli/test",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("wait line %q does not contain %q", message, want)
		}
	}
	hold.release()
	if err := <-done; err != nil {
		t.Fatalf("waiter acquire: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 {
		t.Fatalf("one wait printed %d lines, want 1: %q", len(lines), lines)
	}
}

// TestTaskOutputLockWaitLineWithoutALiveHolderRecord covers a lock file whose
// holder wrote no record, and one whose record names a process that is gone:
// the wait still prints its one line, and it names no one.
func TestTaskOutputLockWaitLineWithoutALiveHolderRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		record string
	}{
		{name: "no record"},
		{
			name:   "a record naming a process that is gone",
			record: `{"session":"20260925-000000-000000","pid":99999999,"task":"/proj:test~exec","tasks":["/proj:test~exec"]}` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			job := outputLockJob("proj", "test~exec", nil)
			messages := make(chan string, 1)
			waiter := newTestOutputLocks(t, root, "waiter", "", func(message string) { messages <- message })
			key := waiter.keysFor([]*ScheduledJob{job}, nil, false)[0]
			if err := os.MkdirAll(waiter.dir, 0o755); err != nil {
				t.Fatal(err)
			}
			bare, err := flock.Acquire(filepath.Join(waiter.dir, key.id+".lock"), true, true)
			if err != nil {
				t.Fatalf("hold bare lock: %v", err)
			}
			if _, err := bare.File().WriteString(tc.record); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				hold, err := waiter.acquire(t.Context(), []*ScheduledJob{job}, nil, false)
				hold.release()
				done <- err
			}()
			message := waitMessage(t, messages, "no line without a live holder record")
			if !strings.Contains(message, "waits for another putnami process, which is writing .putnami/out/proj/test") {
				t.Errorf("wait line = %q", message)
			}
			_ = bare.Release()
			if err := <-done; err != nil {
				t.Fatalf("waiter acquire: %v", err)
			}
		})
	}
}

// TestTaskOutputLockHolderRecordFollowsTheHoldingTasks pins the holder record
// through the life of one shared key: it lists every task of the session that
// holds the key, the wait line counts them, a release drops the released task
// from it, and the last release empties it before the unlock.
func TestTaskOutputLockHolderRecordFollowsTheHoldingTasks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	execTask := outputLockJob("proj", "test~exec", nil)
	merge := outputLockJob("proj", "test~merge", nil)
	session := newTestOutputLocks(t, root, "20260925-192012-876307", "", nil)
	key := session.keysFor([]*ScheduledJob{execTask}, nil, false)[0]
	lockPath := filepath.Join(session.dir, key.id+".lock")
	holdingTasks := func() []string {
		t.Helper()
		holder, known := readOutputLockHolder(lockPath)
		if !known {
			return nil
		}
		return holder.Tasks
	}

	first, err := session.acquire(t.Context(), []*ScheduledJob{execTask}, nil, false)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := session.acquire(t.Context(), []*ScheduledJob{merge}, nil, false)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if got, want := holdingTasks(), []string{execTask.Key(), merge.Key()}; !slices.Equal(got, want) {
		t.Fatalf("record tasks = %v, want %v", got, want)
	}

	messages := make(chan string, 1)
	waiter := newTestOutputLocks(t, root, "20260925-192013-000001", "", func(message string) { messages <- message })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := waiter.acquire(ctx, []*ScheduledJob{execTask}, nil, false)
		done <- err
	}()
	message := waitMessage(t, messages, "the waiter never reported the holders")
	if want := "session 20260925-192012-876307 (pid " + fmt.Sprint(os.Getpid()) + ", task " + execTask.Key() + " and 1 more)"; !strings.Contains(message, want) {
		t.Errorf("wait line %q does not contain %q", message, want)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter returned %v", err)
	}

	first.release()
	if got, want := holdingTasks(), []string{merge.Key()}; !slices.Equal(got, want) {
		t.Fatalf("record tasks after the first release = %v, want %v", got, want)
	}
	second.release()
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("the lock file keeps %d bytes after the last release, want an empty record", info.Size())
	}
	if !probeFree(t, session, key) {
		t.Fatal("the lock is still held after the last release")
	}
}

// TestTaskOutputLockPreparationKeys pins the preparation key: the project key
// a task takes only because its preparation writes the project tree is
// released by endPreparation, which leaves every other key held and removes
// the released id from the value the subprocesses inherit. A declaration that
// needs the project key until capture keeps it.
func TestTaskOutputLockPreparationKeys(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plain := outputLockJob("proj", "test~exec", nil)
	generating := outputLockJob("proj", "build~generate", map[string]extension.DeclaredOutput{
		"gen": dirDeclaration(extension.OutputRootProject, ".gen", false),
	})
	always := func(*ScheduledJob) bool { return true }
	session := newTestOutputLocks(t, root, "session", "", nil)

	keys := session.keysFor([]*ScheduledJob{plain}, always, false)
	project := keyNamed(t, keys, "project:proj")
	commandOutput := keyNamed(t, keys, "command-output:proj/test")
	if !project.preparation || commandOutput.preparation {
		t.Fatalf("keys = %+v, want only project:proj as a preparation key", keys)
	}
	if len(session.keysFor([]*ScheduledJob{plain}, nil, false)) != 1 {
		t.Fatal("a task whose preparation writes no project tree took more than its command-output key")
	}
	for _, key := range session.keysFor([]*ScheduledJob{generating, plain}, always, false) {
		if key.preparation {
			t.Fatalf("%s is a preparation key although a member declares it until capture", key.name)
		}
	}

	hold, err := session.acquire(t.Context(), []*ScheduledJob{plain}, always, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if probeFree(t, session, project) || probeFree(t, session, commandOutput) {
		t.Fatal("a key was free during the preparation")
	}
	full := hold.value()
	ctx := hold.endPreparation(t.Context())
	if !probeFree(t, session, project) {
		t.Fatal("the preparation key is still held after the preparation ended")
	}
	if probeFree(t, session, commandOutput) {
		t.Fatal("the command-output key was released with the preparation key")
	}
	if got := heldOutputLocksValue(ctx); got != commandOutput.id || !strings.Contains(full, project.id) {
		t.Fatalf("subprocess value = %q (preparation value %q), want only %s after the preparation", got, full, commandOutput.name)
	}
	hold.endPreparation(t.Context())
	hold.release()
	if !probeFree(t, session, commandOutput) {
		t.Fatal("the command-output key is still held after release")
	}
}

// TestTaskOutputLockServeKeys pins which key a serve-workload step releases
// once its process started: its command-output key, and only when every task
// of the acquisition that needs it is a serve-workload step. Its writer key of
// a durable project-rooted output and its reader key stay held until the
// capture, the subprocess never receives the released key's id, and another
// session takes that key while the step still holds the others.
func TestTaskOutputLockServeKeys(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-serve-step-releases-its-command-output-once-it-started",
		"a-serve-step-keeps-every-other-key-until-capture")
	root := t.TempDir()
	clients := extension.ResourceRef{ID: "workspace-clients", Scope: extension.ResourceScopeWorkspace}
	serving := readerJob("proj", "serve~serve", map[string]extension.DeclaredOutput{
		"state": dirDeclaration(extension.OutputRootProject, ".gen/serve", false),
	}, clients)
	finite := outputLockJob("proj", "serve~generate", nil)
	always := func(*ScheduledJob) bool { return true }
	session := newTestOutputLocks(t, root, "session", "", nil)
	session.serveSteps = map[string]bool{serving.Key(): true}

	keys := session.keysFor([]*ScheduledJob{serving}, always, true)
	commandOutput := keyNamed(t, keys, "command-output:proj/serve")
	project := keyNamed(t, keys, "project:proj")
	workspace := keyNamed(t, keys, "workspace:")
	if !commandOutput.serve || project.serve || workspace.serve {
		t.Fatalf("keys = %+v, want only the command-output key released once the process started", keys)
	}
	for _, key := range session.keysFor([]*ScheduledJob{serving, finite}, always, true) {
		if key.serve {
			t.Fatalf("%s is a serve key although a finite step of the acquisition needs it until capture", key.name)
		}
	}
	for _, key := range session.keysFor([]*ScheduledJob{finite}, always, true) {
		if key.serve {
			t.Fatalf("%s is a serve key of a step that is not a serve-workload step", key.name)
		}
	}

	hold, err := session.acquire(t.Context(), []*ScheduledJob{serving}, always, true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if preparing := hold.value(); !strings.Contains(preparing, commandOutput.id) {
		t.Fatalf("preparation value = %q, want the command-output key the preparation holds", preparing)
	}
	ctx := hold.endPreparation(t.Context())
	if got, want := heldOutputLocksValue(ctx), strings.Join(slices.Sorted(slices.Values([]string{project.id, workspace.id})), ","); got != want {
		t.Fatalf("subprocess value = %q, want %q: the subprocess never holds the command-output key", got, want)
	}
	releaseServeOutputLocks(ctx)
	if probeFree(t, session, commandOutput) {
		t.Fatal("the command-output key was released outside a serve-workload step's process start")
	}

	other := newTestOutputLocks(t, root, "other", "", noWaitLine(t))
	started := withServeStep(ctx)
	releaseServeOutputLocks(started)
	if !probeFree(t, session, commandOutput) {
		t.Fatal("the command-output key is still held after the serve-workload step's process started")
	}
	if probeFree(t, session, project) || probeShared(t, session, project) {
		t.Fatal("the writer key of a durable project-rooted output was released before the capture")
	}
	if probeFree(t, session, workspace) || !probeShared(t, session, workspace) {
		t.Fatal("the reader key is not held shared until the capture")
	}
	finiteHold := awaitAcquired(t, acquireAsync(t.Context(), other, finite), "another session's finite step")
	finiteHold.release()

	releaseServeOutputLocks(started)
	hold.serveStarted()
	if probeFree(t, session, project) {
		t.Fatal("a second release dropped a key held until the capture")
	}
	hold.release()
	for _, key := range keys {
		if !probeFree(t, session, key) {
			t.Fatalf("%s is still held after release", key.name)
		}
	}
	(*outputLockHold)(nil).serveStarted()
	releaseServeOutputLocks(withServeStep(t.Context()))
}

// TestPreparationWritesProjectTree pins the two preparation writes that take
// the project key: a preBuild hook until this session started it, whatever
// this session already stamped, and the version stamp until this session
// refreshed it.
func TestPreparationWritesProjectTree(t *testing.T) {
	t.Parallel()
	s := &Scheduler{versionFiles: make(map[string]bool), hookSlots: make(map[string]*hookSlot)}
	plain := outputLockJob("proj", "test~exec", nil)
	hooked := outputLockJob("proj", "build~compile", nil)
	hooked.Extension.Hooks = &extension.ManifestHooks{PreBuild: &extension.HookDefinition{Kind: "command", Command: "true"}}

	if !s.preparationWritesProjectTree(plain) {
		t.Fatal("the first execution of a project does not lock the project for its version stamp")
	}
	s.versionFiles["proj"] = true
	if s.preparationWritesProjectTree(plain) {
		t.Fatal("a task locks its project although the session already stamped it")
	}
	if !s.preparationWritesProjectTree(hooked) {
		t.Fatal("a task whose extension declares a preBuild hook does not lock its project")
	}
	s.hookSlots[preBuildHookKey(hooked)] = &hookSlot{done: make(chan struct{})}
	if s.preparationWritesProjectTree(hooked) {
		t.Fatal("a task locks its project although the session already started its preBuild hook")
	}
	if s.preparationWritesProjectTree(nil) {
		t.Fatal("a nil job locks a project")
	}
}

// TestTaskOutputLockWaitHonorsCancellation pins that a canceled wait returns,
// and leaves nothing behind that could take the lock later.
func TestTaskOutputLockWaitHonorsCancellation(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-output-exclusion",
		"the-wait-names-the-holder-and-ends-on-cancellation")
	root := t.TempDir()
	job := outputLockJob("proj", "test~exec", nil)
	holder := newTestOutputLocks(t, root, "holder", "", nil)
	hold, err := holder.acquire(t.Context(), []*ScheduledJob{job}, nil, false)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	messages := make(chan string, 1)
	waiter := newTestOutputLocks(t, root, "waiter", "", func(message string) { messages <- message })
	done := make(chan error, 1)
	go func() {
		_, err := waiter.acquire(ctx, []*ScheduledJob{job}, nil, false)
		done <- err
	}()
	waitMessage(t, messages, "the waiter never started waiting")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled wait returned %v, want context.Canceled", err)
		}
	case <-time.After(answerWaitBudget):
		t.Fatal("a canceled wait did not return")
	}

	hold.release()
	key := holder.keysFor([]*ScheduledJob{job}, nil, false)[0]
	if !probeFree(t, holder, key) {
		t.Fatal("the canceled waiter took the lock after it returned")
	}
	waiter.mu.Lock()
	defer waiter.mu.Unlock()
	if len(waiter.held) != 0 {
		t.Fatalf("the canceled waiter kept entries: %v", waiter.held)
	}
}

// TestTaskOutputLockIsSharedWithinASession pins that tasks of one session never
// wait for each other, and that the flock outlives the last holder only.
func TestTaskOutputLockIsSharedWithinASession(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	job := outputLockJob("proj", "test~exec", nil)
	session := newTestOutputLocks(t, root, "session", "", func(message string) {
		t.Errorf("a task waited for its own session: %s", message)
	})
	first, err := session.acquire(t.Context(), []*ScheduledJob{job}, nil, false)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := session.acquire(t.Context(), []*ScheduledJob{job}, nil, false)
	if err != nil {
		t.Fatalf("second acquire in the same session: %v", err)
	}
	key := session.keysFor([]*ScheduledJob{job}, nil, false)[0]
	first.release()
	first.release() // a release is idempotent
	if probeFree(t, session, key) {
		t.Fatal("the lock was released while another task of the session still held it")
	}
	second.release()
	if !probeFree(t, session, key) {
		t.Fatal("the lock is still held after the last task released it")
	}
}

// TestTaskOutputLockReentersAncestorLocks pins the nested-session rule: a key
// the spawning task holds is held for its descendants, and only that key.
func TestTaskOutputLockReentersAncestorLocks(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-output-exclusion",
		"a-nested-session-reenters-only-its-ancestors-locks")
	root := t.TempDir()
	spawner := outputLockJob("proj", "clientgen-sync~sync", nil)
	unrelated := outputLockJob("other", "build~compile", nil)

	parent := newTestOutputLocks(t, root, "parent", "", nil)
	parentHold, err := parent.acquire(t.Context(), []*ScheduledJob{spawner}, nil, false)
	if err != nil {
		t.Fatalf("parent acquire: %v", err)
	}
	defer parentHold.release()
	inherited := parentHold.value()
	spawnerKey := parent.keysFor([]*ScheduledJob{spawner}, nil, false)[0]
	if inherited != spawnerKey.id {
		t.Fatalf("children inherit %q, want the spawner's key %q", inherited, spawnerKey.id)
	}

	messages := make(chan string, 1)
	child := newTestOutputLocks(t, root, "child", inherited, func(message string) { messages <- message })
	childHold, err := child.acquire(t.Context(), []*ScheduledJob{spawner}, nil, false)
	if err != nil {
		t.Fatalf("a descendant could not re-enter its ancestor's key: %v", err)
	}
	if value := childHold.value(); value != inherited {
		t.Fatalf("the descendant passes on %q, want %q", value, inherited)
	}
	select {
	case message := <-messages:
		t.Fatalf("a descendant waited for its own ancestor: %s", message)
	default:
	}
	childHold.release()
	if probeFree(t, parent, spawnerKey) {
		t.Fatal("a descendant's release dropped the ancestor's lock")
	}

	other := newTestOutputLocks(t, root, "unrelated", "", nil)
	otherHold, err := other.acquire(t.Context(), []*ScheduledJob{unrelated}, nil, false)
	if err != nil {
		t.Fatalf("unrelated acquire: %v", err)
	}
	done := make(chan error, 1)
	var childValue string
	go func() {
		hold, err := child.acquire(t.Context(), []*ScheduledJob{unrelated}, nil, false)
		childValue = hold.value()
		hold.release()
		done <- err
	}()
	waitMessage(t, messages, "a descendant skipped a key no ancestor holds")
	otherHold.release()
	if err := <-done; err != nil {
		t.Fatalf("descendant acquire of an unrelated key: %v", err)
	}
	unrelatedKey := child.keysFor([]*ScheduledJob{unrelated}, nil, false)[0]
	want := []string{spawnerKey.id, unrelatedKey.id}
	slices.Sort(want)
	if childValue != strings.Join(want, ",") {
		t.Fatalf("grandchildren inherit %q, want %q", childValue, strings.Join(want, ","))
	}
}

// TestHeldOutputLocksReachTheSubprocessLast pins the delivery of the held ids:
// set on cmd.Env over any inherited or manifest value, and absent otherwise.
func TestHeldOutputLocksReachTheSubprocessLast(t *testing.T) {
	t.Parallel()
	env := []string{"PATH=/bin", heldOutputLocksEnv + "=from-an-ancestor", heldOutputLocksEnv + "=from-a-manifest"}
	if got := applyHeldOutputLocks(t.Context(), env); !slices.Equal(got, env) {
		t.Fatalf("without held locks the environment changed: %v", got)
	}
	got := applyHeldOutputLocks(withHeldOutputLocks(t.Context(), "aa,bb"), env)
	if value := envLastValue(got, heldOutputLocksEnv); value != "aa,bb" {
		t.Fatalf("%s = %q, want the scheduler's value", heldOutputLocksEnv, value)
	}
	count := 0
	for _, entry := range got {
		if strings.HasPrefix(entry, heldOutputLocksEnv+"=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d %s entries reach the subprocess, want 1", count, heldOutputLocksEnv)
	}

	ws := makeExecutorTestWorkspace(t)
	seen := filepath.Join(ws.Root, "seen")
	script := filepath.Join(ws.Root, "print-held.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf '%s' \"$"+heldOutputLocksEnv+"\" > "+shellQuote(seen)+"\n")
	job := outputLockJob("proj", "build~compile", nil)
	job.Extension.Path = t.TempDir()
	job.JobDef.Command = script
	job.JobDef.TimeoutMs = unboundedJobTimeoutMs
	job.JobDef.Env = map[string]string{heldOutputLocksEnv: "from-a-manifest"}
	result, err := RunJob(withHeldOutputLocks(t.Context(), "aa,bb"), ws, job, nil, nil, nil, nil)
	if err != nil || result.Status != "success" {
		t.Fatalf("RunJob = %+v, %v", result, err)
	}
	if got := readFileAt(t, seen); got != "aa,bb" {
		t.Fatalf("the subprocess saw %s=%q, want the scheduler's value", heldOutputLocksEnv, got)
	}
}

// TestHeldOutputLocksEnvIsNeverACacheKeyInput pins the variable out of the key
// by name, the way TestParentSessionEnvIsNeverACacheKeyInput pins its own.
func TestHeldOutputLocksEnvIsNeverACacheKeyInput(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	hash := func() string {
		t.Helper()
		job := declareCacheTestTask(&ScheduledJob{
			Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
			Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
			JobDef: &extension.JobDefinition{
				Name: "build", CommandName: "build", ExtensionName: "@test/ext", Cache: true,
			},
		}, "")
		got, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return got
	}
	t.Setenv(heldOutputLocksEnv, "")
	unset := hash()
	t.Setenv(heldOutputLocksEnv, "0123456789abcdef0123456789abcdef")
	if got := hash(); got != unset {
		t.Fatalf("%s moved the task cache key", heldOutputLocksEnv)
	}
}

// readerJob is outputLockJob whose definition reads reads.
func readerJob(projectPath, name string, outputs map[string]extension.DeclaredOutput, reads ...extension.ResourceRef) *ScheduledJob {
	job := outputLockJob(projectPath, name, outputs)
	job.JobDef.Reads = reads
	return job
}

// genWriter is a plan node that writes proj's .gen as a declared
// project-rooted output, the way build~generate does.
func genWriter(name string) *ScheduledJob {
	return outputLockJob("proj", name, map[string]extension.DeclaredOutput{
		"gen": dirDeclaration(extension.OutputRootProject, ".gen", false),
	})
}

// genReader is a plan node that reads proj's gen resource and the sources,
// the way build~transpile does.
func genReader(name string) *ScheduledJob {
	return readerJob("proj", name, nil, extension.ResourceRef{ID: "gen"}, extension.ResourceRef{ID: extension.ResourceIDSources})
}

// probeShared reports whether key's lock file can be locked shared right now,
// without waiting, and releases it again at once.
func probeShared(t *testing.T, m *taskOutputLocks, key outputLockKey) bool {
	t.Helper()
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := flock.Acquire(filepath.Join(m.dir, key.id+".lock"), false, true)
	if errors.Is(err, flock.ErrBusy) {
		return false
	}
	if err != nil {
		t.Fatalf("probe %s: %v", key.name, err)
	}
	_ = lock.Release()
	return true
}

// noWaitLine fails the test on any wait line.
func noWaitLine(t *testing.T) func(string) {
	return func(message string) { t.Errorf("unexpected wait line: %s", message) }
}

// acquireAsync acquires the jobs' execution keys on its own goroutine.
func acquireAsync(ctx context.Context, m *taskOutputLocks, jobs ...*ScheduledJob) <-chan acquired {
	done := make(chan acquired, 1)
	go func() {
		hold, err := m.acquire(ctx, jobs, nil, true)
		done <- acquired{hold: hold, err: err}
	}()
	return done
}

type acquired struct {
	hold *outputLockHold
	err  error
}

// awaitAcquired waits for an acquisition that MUST settle. The bound only
// detects a hang (answerWaitBudget); the synchronization is the channel.
func awaitAcquired(t *testing.T, done <-chan acquired, what string) *outputLockHold {
	t.Helper()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("%s: %v", what, got.err)
		}
		return got.hold
	case <-time.After(answerWaitBudget):
		t.Fatalf("%s never settled", what)
		return nil
	}
}

// requirePending fails when an acquisition that cannot have settled did.
func requirePending(t *testing.T, done <-chan acquired, what string) {
	t.Helper()
	select {
	case got := <-done:
		t.Fatalf("%s settled (%v) while the key it needs was held", what, got.err)
	default:
	}
}

// heldEntry returns a copy of the mode and holding tasks of the session's
// entry for key, and whether it has one.
func heldEntry(m *taskOutputLocks, key outputLockKey) (exclusive bool, tasks []string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.held[key.id]
	if entry == nil {
		return false, nil, false
	}
	return entry.exclusive, slices.Clone(entry.tasks), true
}

func TestTaskReadLockNames(t *testing.T) {
	t.Parallel()
	gen := extension.ResourceRef{ID: "gen"}
	sources := extension.ResourceRef{ID: extension.ResourceIDSources}
	clients := extension.ResourceRef{ID: "workspace-clients", Scope: extension.ResourceScopeWorkspace}
	written := readerJob("proj", "build~describe", nil, gen, sources)
	written.JobDef.Writes = []extension.ResourceRef{{ID: "clients"}}
	rewritten := readerJob("proj", "build~generate", nil, gen, sources)
	rewritten.JobDef.Writes = []extension.ResourceRef{gen}

	for _, tc := range []struct {
		name string
		job  *ScheduledJob
		want []string
	}{
		{name: "nil job", job: nil, want: nil},
		{name: "a task that reads nothing takes no reader key", job: outputLockJob("proj", "test~exec", nil), want: nil},
		{name: "a project-scoped read takes the project key", job: readerJob("proj", "build~transpile", nil, gen), want: []string{"project:proj"}},
		{name: "a workspace-scoped read takes the workspace key", job: readerJob("proj", "test~run", nil, clients), want: []string{"workspace:"}},
		{name: "the sources resource takes no key", job: readerJob("proj", "lint~check", nil, sources), want: nil},
		{name: "every scope read once", job: readerJob("proj", "test~run", nil, gen, sources, clients, gen), want: []string{"project:proj", "workspace:"}},
		{name: "a read of another resource than the one written keeps its key", job: written, want: []string{"project:proj"}},
		{name: "a resource the task also writes is not read", job: rewritten, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskReadLockNames(tc.job); !slices.Equal(got, tc.want) {
				t.Fatalf("taskReadLockNames = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTaskOutputLockReaderKeyModes pins the mode of every key one acquisition
// takes: a reader key is shared, an execution takes it and a restore does not,
// and a key the acquisition also needs exclusively, as a writer key or for a
// preparation, is exclusive and held until capture.
func TestTaskOutputLockReaderKeyModes(t *testing.T) {
	t.Parallel()
	session := newTestOutputLocks(t, t.TempDir(), "session", "", nil)
	reader := genReader("build~transpile")
	always := func(*ScheduledJob) bool { return true }

	keys := session.keysFor([]*ScheduledJob{reader}, nil, true)
	if len(keys) != 2 {
		t.Fatalf("keys = %+v, want the command output and the project", keys)
	}
	if commandOutput := keyNamed(t, keys, "command-output:proj/build"); !commandOutput.exclusive {
		t.Fatal("the command-output key is not exclusive")
	}
	project := keyNamed(t, keys, "project:proj")
	if project.exclusive || project.preparation {
		t.Fatalf("reader key = %+v, want shared and held until capture", project)
	}
	if restore := session.keysFor([]*ScheduledJob{reader}, nil, false); len(restore) != 1 || restore[0].name != "command-output:proj/build" {
		t.Fatalf("restore keys = %+v, want the command output alone", restore)
	}

	clients := readerJob("proj", "test~run", nil, extension.ResourceRef{ID: "workspace-clients", Scope: extension.ResourceScopeWorkspace})
	if workspaceKey := keyNamed(t, session.keysFor([]*ScheduledJob{clients}, nil, true), "workspace:"); workspaceKey.exclusive {
		t.Fatal("a workspace-scoped read took the workspace key exclusively")
	}

	declaring := readerJob("proj", "build~compile", map[string]extension.DeclaredOutput{
		"dist": dirDeclaration(extension.OutputRootProject, "dist", false),
	}, extension.ResourceRef{ID: "gen"})
	if key := keyNamed(t, session.keysFor([]*ScheduledJob{declaring}, nil, true), "project:proj"); !key.exclusive || key.preparation {
		t.Fatalf("project key of a reader with a project-rooted output = %+v, want exclusive until capture", key)
	}
	if key := keyNamed(t, session.keysFor([]*ScheduledJob{reader}, always, true), "project:proj"); !key.exclusive || key.preparation {
		t.Fatalf("project key of a reader whose preparation writes the project = %+v, want exclusive until capture", key)
	}
	if key := keyNamed(t, session.keysFor([]*ScheduledJob{reader}, always, false), "project:proj"); !key.exclusive || !key.preparation {
		t.Fatalf("project key of a restore's preparation = %+v, want an exclusive preparation key", key)
	}
	if key := keyNamed(t, session.keysFor([]*ScheduledJob{reader, genWriter("build~generate")}, nil, true), "project:proj"); !key.exclusive {
		t.Fatal("a batch of a reader and a writer of one project took its key shared")
	}
}

// TestTaskOutputLockRecordSaysReadingOnceOnlyReadersHoldIt pins the holder
// record of an exclusive hold: it says the session writes while a writer
// holds it, and reads once only readers that joined the writer's hold are
// left, so another session's wait line names the reader without saying it
// writes. The reader keeps the hold exclusive until it releases.
func TestTaskOutputLockRecordSaysReadingOnceOnlyReadersHoldIt(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-read-exclusion",
		"a-reader-and-another-sessions-writer-exclude-each-other")
	root := t.TempDir()
	writer := genWriter("build~generate")
	reader := genReader("build~transpile")
	session := newTestOutputLocks(t, root, "20260925-192012-876307", "", noWaitLine(t))
	session.onOwnSessionWait = func(task string) { t.Errorf("%s waited for its own session", task) }
	project := keyNamed(t, session.keysFor([]*ScheduledJob{reader}, nil, true), "project:proj")
	if project.exclusive {
		t.Fatal("a reader took its reader key exclusively")
	}
	lockPath := filepath.Join(session.dir, project.id+".lock")

	writeHold, err := session.acquire(t.Context(), []*ScheduledJob{writer}, nil, true)
	if err != nil {
		t.Fatalf("writer acquire: %v", err)
	}
	readHold, err := session.acquire(t.Context(), []*ScheduledJob{reader}, nil, true)
	if err != nil {
		t.Fatalf("reader acquire: %v", err)
	}
	if holder, known := readOutputLockHolder(lockPath); !known || holder.Reading || !slices.Equal(holder.Tasks, []string{writer.Key(), reader.Key()}) {
		t.Fatalf("record while the writer holds = %+v (known %v), want both tasks, writing", holder, known)
	}
	writeHold.release()
	if holder, known := readOutputLockHolder(lockPath); !known || !holder.Reading || !slices.Equal(holder.Tasks, []string{reader.Key()}) {
		t.Fatalf("record once only the reader holds = %+v (known %v), want the reader, reading", holder, known)
	}
	if exclusive, _, ok := heldEntry(session, project); !ok || !exclusive {
		t.Fatal("the writer's release changed the mode of the hold its reader still holds")
	}

	messages := make(chan string, 1)
	other := newTestOutputLocks(t, root, "20260925-192013-000001", "", func(message string) { messages <- message })
	otherReader := genReader("test~run")
	waiting := acquireAsync(t.Context(), other, otherReader)
	message := waitMessage(t, messages, "another session's reader never reported the holder")
	want := fmt.Sprintf("%s waits for session 20260925-192012-876307 (pid %d, task %s), which is reading the project-rooted outputs of proj",
		otherReader.Key(), os.Getpid(), reader.Key())
	if message != want {
		t.Errorf("wait line = %q, want %q", message, want)
	}
	requirePending(t, waiting, "another session's reader")
	readHold.release()
	awaitAcquired(t, waiting, "another session's reader").release()
}

// TestTaskOutputLockReadersShareAcrossSessions pins the shared mode across
// sessions: two sessions read one resource at once, neither waits, neither
// writes a holder record, and a writer is kept out until the last one
// released.
func TestTaskOutputLockReadersShareAcrossSessions(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-read-exclusion",
		"readers-in-several-sessions-share-their-lock")
	root := t.TempDir()
	first := newTestOutputLocks(t, root, "first", "", noWaitLine(t))
	second := newTestOutputLocks(t, root, "second", "", noWaitLine(t))
	transpile := genReader("build~transpile")
	run := genReader("test~run")
	project := keyNamed(t, first.keysFor([]*ScheduledJob{transpile}, nil, true), "project:proj")
	lockPath := filepath.Join(first.dir, project.id+".lock")
	recordSize := func() int64 {
		t.Helper()
		info, err := os.Stat(lockPath)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}

	firstHold, err := first.acquire(t.Context(), []*ScheduledJob{transpile}, nil, true)
	if err != nil {
		t.Fatalf("first reader: %v", err)
	}
	secondHold, err := second.acquire(t.Context(), []*ScheduledJob{run}, nil, true)
	if err != nil {
		t.Fatalf("second reader: %v", err)
	}
	if probeFree(t, first, project) {
		t.Fatal("a writer could lock the resource two sessions read")
	}
	if !probeShared(t, first, project) {
		t.Fatal("a third reader could not join the two")
	}
	if size := recordSize(); size != 0 {
		t.Fatalf("readers wrote a %d-byte holder record", size)
	}
	firstHold.release()
	if probeFree(t, first, project) {
		t.Fatal("a writer could lock the resource while the second session still reads it")
	}
	if size := recordSize(); size != 0 {
		t.Fatalf("a reader's release left a %d-byte holder record", size)
	}
	secondHold.release()
	if !probeFree(t, first, project) {
		t.Fatal("the resource is still locked after both readers released")
	}
}

// TestTaskOutputLockReaderAndWriterExcludeEachOther pins both directions
// across sessions: a reader waits for another session's writer and names it,
// and a writer waits for another session's reader with a line that names no
// one, not even a holder record a gone writer left in the lock file.
func TestTaskOutputLockReaderAndWriterExcludeEachOther(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-read-exclusion",
		"a-reader-and-another-sessions-writer-exclude-each-other")
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-read-exclusion",
		"a-wait-behind-readers-names-no-holder")
	root := t.TempDir()
	writer := genWriter("build~generate")
	reader := genReader("test~run")

	t.Run("a reader waits for the writer", func(t *testing.T) {
		writing := newTestOutputLocks(t, root, "20260925-192012-876307", "", nil)
		hold, err := writing.acquire(t.Context(), []*ScheduledJob{writer}, nil, true)
		if err != nil {
			t.Fatalf("writer acquire: %v", err)
		}
		messages := make(chan string, 1)
		reading := newTestOutputLocks(t, root, "20260925-192013-000001", "", func(message string) { messages <- message })
		waiting := acquireAsync(t.Context(), reading, reader)
		message := waitMessage(t, messages, "the reader never reported the writer")
		want := fmt.Sprintf("%s waits for session 20260925-192012-876307 (pid %d, task %s), which is writing the project-rooted outputs of proj",
			reader.Key(), os.Getpid(), writer.Key())
		if message != want {
			t.Errorf("wait line = %q, want %q", message, want)
		}
		requirePending(t, waiting, "the reader")
		hold.release()
		awaitAcquired(t, waiting, "the reader").release()
	})

	t.Run("a writer waits for the reader", func(t *testing.T) {
		reading := newTestOutputLocks(t, root, "20260925-192014-000002", "", nil)
		project := keyNamed(t, reading.keysFor([]*ScheduledJob{reader}, nil, true), "project:proj")
		if err := os.MkdirAll(reading.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gone := `{"session":"20260925-000000-000000","pid":99999999,"task":"/proj:build~generate","tasks":["/proj:build~generate"]}` + "\n"
		if err := os.WriteFile(filepath.Join(reading.dir, project.id+".lock"), []byte(gone), 0o644); err != nil {
			t.Fatal(err)
		}
		hold, err := reading.acquire(t.Context(), []*ScheduledJob{reader}, nil, true)
		if err != nil {
			t.Fatalf("reader acquire: %v", err)
		}
		messages := make(chan string, 1)
		writing := newTestOutputLocks(t, root, "20260925-192015-000003", "", func(message string) { messages <- message })
		waiting := acquireAsync(t.Context(), writing, writer)
		message := waitMessage(t, messages, "the writer never reported the reader")
		if want := writer.Key() + " waits for another putnami process, which is reading the project-rooted outputs of proj"; message != want {
			t.Errorf("wait line = %q, want %q", message, want)
		}
		requirePending(t, waiting, "the writer")
		hold.release()
		awaitAcquired(t, waiting, "the writer").release()
	})
}

// TestTaskOutputLockExclusiveNeedWaitsForItsSessionsReaders pins the
// in-session rule: an exclusive need never converts the session's shared hold.
// It waits until the session's readers released, a reader that arrives
// meanwhile queues behind it, and the queued reader then joins the exclusive
// hold. No wait line is printed for the session's own readers.
func TestTaskOutputLockExclusiveNeedWaitsForItsSessionsReaders(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	reader := genReader("test~run")
	writer := genWriter("build~generate")
	queued := genReader("lint~docs")
	waits := make(chan string, 8)
	session := newTestOutputLocks(t, root, "session", "", noWaitLine(t))
	session.onOwnSessionWait = func(task string) { waits <- task }
	project := keyNamed(t, session.keysFor([]*ScheduledJob{reader}, nil, true), "project:proj")

	readHold, err := session.acquire(t.Context(), []*ScheduledJob{reader}, nil, true)
	if err != nil {
		t.Fatalf("reader acquire: %v", err)
	}
	writing := acquireAsync(t.Context(), session, writer)
	if task := waitMessage(t, waits, "the writer never waited for its session's reader"); task != writer.Key() {
		t.Fatalf("waiting task = %s, want the writer", task)
	}
	joining := acquireAsync(t.Context(), session, queued)
	if task := waitMessage(t, waits, "the reader never queued behind the writer"); task != queued.Key() {
		t.Fatalf("queued task = %s, want the second reader", task)
	}
	requirePending(t, writing, "the writer")
	requirePending(t, joining, "the queued reader")
	if !probeShared(t, session, project) || probeFree(t, session, project) {
		t.Fatal("the session converted its shared hold while its reader still held it")
	}

	readHold.release()
	writeHold := awaitAcquired(t, writing, "the writer")
	joinHold := awaitAcquired(t, joining, "the queued reader")
	exclusive, tasks, ok := heldEntry(session, project)
	if !ok || !exclusive || !slices.Equal(tasks, []string{writer.Key(), queued.Key()}) {
		t.Fatalf("session entry = exclusive %v, tasks %v, want the writer's exclusive hold joined by the queued reader", exclusive, tasks)
	}
	if probeShared(t, session, project) {
		t.Fatal("another session could read while the writer holds the key")
	}
	holder, known := readOutputLockHolder(filepath.Join(session.dir, project.id+".lock"))
	if !known || !slices.Equal(holder.Tasks, []string{writer.Key(), queued.Key()}) {
		t.Fatalf("holder record = %+v (known %v), want the writer and the queued reader", holder, known)
	}
	writeHold.release()
	joinHold.release()
	if !probeFree(t, session, project) {
		t.Fatal("the key is still held after every release")
	}
	if _, _, ok := heldEntry(session, project); ok {
		t.Fatal("the session kept an entry after every release")
	}
}

// TestTaskOutputLockCanceledExclusiveNeedReopensTheSharedHold pins the
// cancellation of an exclusive need that waits for its session's readers: it
// returns, and the reader queued behind it joins the shared hold at once.
func TestTaskOutputLockCanceledExclusiveNeedReopensTheSharedHold(t *testing.T) {
	t.Parallel()
	reader := genReader("test~run")
	writer := genWriter("build~generate")
	queued := genReader("lint~docs")
	waits := make(chan string, 8)
	session := newTestOutputLocks(t, t.TempDir(), "session", "", noWaitLine(t))
	session.onOwnSessionWait = func(task string) { waits <- task }
	project := keyNamed(t, session.keysFor([]*ScheduledJob{reader}, nil, true), "project:proj")

	readHold, err := session.acquire(t.Context(), []*ScheduledJob{reader}, nil, true)
	if err != nil {
		t.Fatalf("reader acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writing := acquireAsync(ctx, session, writer)
	waitMessage(t, waits, "the writer never waited for its session's reader")
	joining := acquireAsync(t.Context(), session, queued)
	waitMessage(t, waits, "the reader never queued behind the writer")

	cancel()
	select {
	case got := <-writing:
		if !errors.Is(got.err, context.Canceled) || got.hold != nil {
			t.Fatalf("canceled writer returned %+v", got)
		}
	case <-time.After(answerWaitBudget):
		t.Fatal("a canceled writer did not return")
	}
	joinHold := awaitAcquired(t, joining, "the queued reader")
	exclusive, tasks, ok := heldEntry(session, project)
	if !ok || exclusive || !slices.Equal(tasks, []string{reader.Key(), queued.Key()}) {
		t.Fatalf("session entry = exclusive %v, tasks %v, want the shared hold of both readers", exclusive, tasks)
	}
	readHold.release()
	joinHold.release()
	if !probeFree(t, session, project) {
		t.Fatal("the key is still held after every release")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.held) != 0 {
		t.Fatalf("the session kept entries: %v", session.held)
	}
}

// TestTaskOutputLockInheritedReaderKeyCountsAsHeld pins the nested-session
// rule for a key the ancestor holds shared: the descendant's writer does not
// wait for its own ancestor, and another session's writer still waits for the
// ancestor's shared hold.
func TestTaskOutputLockInheritedReaderKeyCountsAsHeld(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "concurrent-session-output-exclusion",
		"a-nested-session-reenters-only-its-ancestors-locks")
	root := t.TempDir()
	reader := genReader("clientgen-sync~sync")
	parent := newTestOutputLocks(t, root, "parent", "", nil)
	parentHold, err := parent.acquire(t.Context(), []*ScheduledJob{reader}, nil, true)
	if err != nil {
		t.Fatalf("parent acquire: %v", err)
	}
	defer parentHold.release()
	project := keyNamed(t, parent.keysFor([]*ScheduledJob{reader}, nil, true), "project:proj")
	if !strings.Contains(parentHold.value(), project.id) {
		t.Fatalf("children inherit %q, which lacks the reader key %s", parentHold.value(), project.id)
	}

	child := newTestOutputLocks(t, root, "child", parentHold.value(), noWaitLine(t))
	childHold, err := child.acquire(t.Context(), []*ScheduledJob{genWriter("build~generate")}, nil, true)
	if err != nil {
		t.Fatalf("a descendant could not write under its ancestor's reader key: %v", err)
	}
	defer childHold.release()

	messages := make(chan string, 1)
	other := newTestOutputLocks(t, root, "unrelated", "", func(message string) { messages <- message })
	ctx, cancel := context.WithCancel(t.Context())
	waiting := acquireAsync(ctx, other, genWriter("test~generate"))
	message := waitMessage(t, messages, "another session's writer did not wait for the ancestor's reader key")
	if !strings.Contains(message, "which is reading the project-rooted outputs of proj") {
		t.Errorf("wait line = %q", message)
	}
	cancel()
	if got := <-waiting; !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled writer returned %v", got.err)
	}
}
