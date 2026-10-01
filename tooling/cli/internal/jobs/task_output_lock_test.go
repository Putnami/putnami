//go:build unix

package jobs

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
	"go.putnami.dev/tooling/cli/internal/store"
)

// outputLockJob is a plan node for project path projectPath running name
// (command~step), whose task declares outputs.
func outputLockJob(projectPath, name string, outputs map[string]extension.DeclaredOutput) *ScheduledJob {
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/" + projectPath, Name: projectPath, Path: projectPath},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: name, ExtensionName: "@test/ext"},
	}
	if outputs != nil {
		job.Extension.Tasks = map[string]extension.TaskDefinition{
			"task": {Declares: &extension.TaskDeclaration{Outputs: outputs}},
		}
		job.Step = &extension.PipelineStep{Task: "task"}
	}
	return job
}

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
	forward := m.keysFor(jobs, nil)
	backward := m.keysFor([]*ScheduledJob{jobs[2], jobs[1], jobs[0]}, nil)
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
	aliased := newTestOutputLocks(t, link, "s", "", nil).keysFor(jobs, nil)
	if !slices.Equal(forward, aliased) {
		t.Fatalf("a symlinked spelling of the workspace derived other ids:\n%v\n%v", forward, aliased)
	}
	other := newTestOutputLocks(t, t.TempDir(), "s", "", nil).keysFor(jobs, nil)
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
	keys := holder.keysFor(jobs, nil)
	if len(keys) != 3 {
		t.Fatalf("keys = %v, want three", keys)
	}
	var middle *ScheduledJob
	for _, job := range jobs {
		if taskOutputLockNames(job)[0] == keys[1].name {
			middle = job
		}
	}
	middleHold, err := holder.acquire(t.Context(), []*ScheduledJob{middle}, nil)
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
		all, err = waiter.acquire(t.Context(), []*ScheduledJob{jobs[2], jobs[1], jobs[0]}, nil)
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
	hold, err := holder.acquire(t.Context(), []*ScheduledJob{running}, nil)
	if err != nil {
		t.Fatalf("hold the command output: %v", err)
	}

	messages := make(chan string, 1)
	waiter := newTestOutputLocks(t, root, "waiter", "", func(message string) { messages <- message })
	keys := waiter.keysFor([]*ScheduledJob{generating}, nil)
	done := make(chan error, 1)
	var waited *outputLockHold
	go func() {
		var err error
		waited, err = waiter.acquire(t.Context(), []*ScheduledJob{generating}, nil)
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
	hold, err := holder.acquire(t.Context(), []*ScheduledJob{job}, nil)
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
		waiterHold, err := waiter.acquire(t.Context(), []*ScheduledJob{job}, nil)
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
			key := waiter.keysFor([]*ScheduledJob{job}, nil)[0]
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
				hold, err := waiter.acquire(t.Context(), []*ScheduledJob{job}, nil)
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
	key := session.keysFor([]*ScheduledJob{execTask}, nil)[0]
	lockPath := filepath.Join(session.dir, key.id+".lock")
	holdingTasks := func() []string {
		t.Helper()
		holder, known := readOutputLockHolder(lockPath)
		if !known {
			return nil
		}
		return holder.Tasks
	}

	first, err := session.acquire(t.Context(), []*ScheduledJob{execTask}, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := session.acquire(t.Context(), []*ScheduledJob{merge}, nil)
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
		_, err := waiter.acquire(ctx, []*ScheduledJob{execTask}, nil)
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

	keys := session.keysFor([]*ScheduledJob{plain}, always)
	project := keyNamed(t, keys, "project:proj")
	commandOutput := keyNamed(t, keys, "command-output:proj/test")
	if !project.preparation || commandOutput.preparation {
		t.Fatalf("keys = %+v, want only project:proj as a preparation key", keys)
	}
	if len(session.keysFor([]*ScheduledJob{plain}, nil)) != 1 {
		t.Fatal("a task whose preparation writes no project tree took more than its command-output key")
	}
	for _, key := range session.keysFor([]*ScheduledJob{generating, plain}, always) {
		if key.preparation {
			t.Fatalf("%s is a preparation key although a member declares it until capture", key.name)
		}
	}

	hold, err := session.acquire(t.Context(), []*ScheduledJob{plain}, always)
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
	hold, err := holder.acquire(t.Context(), []*ScheduledJob{job}, nil)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	messages := make(chan string, 1)
	waiter := newTestOutputLocks(t, root, "waiter", "", func(message string) { messages <- message })
	done := make(chan error, 1)
	go func() {
		_, err := waiter.acquire(ctx, []*ScheduledJob{job}, nil)
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
	key := holder.keysFor([]*ScheduledJob{job}, nil)[0]
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
	first, err := session.acquire(t.Context(), []*ScheduledJob{job}, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := session.acquire(t.Context(), []*ScheduledJob{job}, nil)
	if err != nil {
		t.Fatalf("second acquire in the same session: %v", err)
	}
	key := session.keysFor([]*ScheduledJob{job}, nil)[0]
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
	parentHold, err := parent.acquire(t.Context(), []*ScheduledJob{spawner}, nil)
	if err != nil {
		t.Fatalf("parent acquire: %v", err)
	}
	defer parentHold.release()
	inherited := parentHold.value()
	spawnerKey := parent.keysFor([]*ScheduledJob{spawner}, nil)[0]
	if inherited != spawnerKey.id {
		t.Fatalf("children inherit %q, want the spawner's key %q", inherited, spawnerKey.id)
	}

	messages := make(chan string, 1)
	child := newTestOutputLocks(t, root, "child", inherited, func(message string) { messages <- message })
	childHold, err := child.acquire(t.Context(), []*ScheduledJob{spawner}, nil)
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
	otherHold, err := other.acquire(t.Context(), []*ScheduledJob{unrelated}, nil)
	if err != nil {
		t.Fatalf("unrelated acquire: %v", err)
	}
	done := make(chan error, 1)
	var childValue string
	go func() {
		hold, err := child.acquire(t.Context(), []*ScheduledJob{unrelated}, nil)
		childValue = hold.value()
		hold.release()
		done <- err
	}()
	waitMessage(t, messages, "a descendant skipped a key no ancestor holds")
	otherHold.release()
	if err := <-done; err != nil {
		t.Fatalf("descendant acquire of an unrelated key: %v", err)
	}
	unrelatedKey := child.keysFor([]*ScheduledJob{unrelated}, nil)[0]
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
		got, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
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
