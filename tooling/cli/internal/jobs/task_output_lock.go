package jobs

// Task-output locks: two sessions of one workspace never write the same task
// output at the same time, and a task never reads a resource while another
// session's task writes it as a declared output.
//
// The in-session planner orders the writers of one resource and its readers
// (serializeWriteResources), and the cache lease coalesces two requesters of
// one cache key. Neither sees a second session in the same workspace that runs
// the same task, or another task of the same project, under different flags:
// its cache key differs, and its plan is its own. A task-output lock is the
// cross-session exclusion, held by the scheduler around every write a task
// makes to its outputs and every read it makes of a resource another task
// writes.
//
// # Keys
//
// A task takes, from its plan node and its declaration alone, its writer
// keys:
//
//   - command-output:<project>/<command>, always. It is the job context's
//     OutputPath: runJob detaches and creates it, extensions write undeclared
//     files into it (the Go test job's coverage profile), and every
//     command-output declared output lives inside it. The key is the whole
//     directory because detaching a cached symlink rewrites the whole
//     directory, not one declared path.
//   - project:<project>, when the task declares a durable project-rooted
//     output. One key per project rather than per path: a pathFrom output's
//     path is known only after the task ran, a literal .gen output contains
//     subpaths other tasks declare, and two lock files exclude each other only
//     when their keys are equal. The project key is the one key that is
//     knowable before the run and shared by every overlapping writer.
//   - workspace:, when the task declares a durable workspace-rooted output,
//     for the same reason.
//
// An executing task also takes a reader key for every resource its
// definition reads and does not write (taskReadLockNames): project:<project>
// for a project-scoped resource such as gen, workspace: for a
// workspace-scoped one. It is derived from the reader's declaration alone, so
// it is knowable before the run and the same in every session. A reader key
// excludes only the writers that hold that key: the tasks that declare a
// durable output rooted in that project or in the workspace, such as the
// generate task that declares .gen. A writer of the resource that declares no
// such output holds no key and is not excluded: the tasks that write the
// workspace-scoped workspace-clients declare no output, so a reader of
// workspace-clients is not protected from them. The sources resource takes no
// key: the tasks that write it mutate the source tree, which no output key
// covers. A reader key that no writer holds delays nobody.
//
// An executing task also takes project:<project> for its preparation alone
// when that preparation writes the project tree
// (preparationWritesProjectTree): the extension declares a preBuild hook,
// which writes the project's .gen, or this session has not yet refreshed the
// project's version stamp, .gen/version.json. The flag refreshVersionFile
// reads turns true only inside a preparation, so the task that writes the
// stamp is always one that took the key. The key is released when the
// preparation ends, before the subprocess starts, unless the task's
// declaration needs it until capture, as a writer key or a reader key: a
// long-running task, such as a serve, that reads nothing another task writes
// never keeps another session out of its project.
//
// Invocation-scoped outputs live in the invocation's private tree and take no
// key. A key id is a digest of the canonical workspace root and the key name,
// so ids from another workspace never match.
//
// # Modes
//
// A writer key and a preparation key are exclusive; a reader key is shared.
// Within one acquisition a key needed in both modes is exclusive: a task that
// reads its project's gen and declares a project-rooted output, or whose
// preparation writes the project tree, holds the project key exclusively
// until its capture. Nothing else decides a mode: a reader needs its reader
// key shared whatever else its session plans, so the readers of one resource
// in several sessions run together.
//
// # What a hold covers
//
// Inside one session a key is reference-counted: every task of the session
// that needs it references one flock, held in the mode of the need that
// locked it. A shared need joins the session's hold in either mode, so a
// reader that joins a writer's exclusive hold keeps it exclusive until the
// session's last holder released it. An exclusive need joins an exclusive
// hold. An exclusive need never converts the session's shared hold in place:
// converting a flock is not atomic, and two sessions converting one at once
// would each wait for the other. It waits until the session released its last
// shared reference, then locks the file exclusively; a need that arrives
// meanwhile waits behind it, so readers cannot starve it. That drain is the
// only wait the locks add to the session's own schedule, and the only way a
// key changes mode within a session. The planner orders a resource's writer
// before the readers that derive from it, so the drain is met by a writer the
// planner lets overlap the session's readers, such as a build's infra step,
// which follows the build's readers but runs beside the session's test step,
// and by a preparation that writes the project tree, which only the run
// decides.
//
// Across sessions a shared flock coexists with other shared ones and an
// exclusive flock with none: the readers of one resource in several sessions
// run together, unless a reader joined its own session's writer hold, and its
// writer waits for every one of them. A hold covers a
// cache restore into the task's outputs (restoreDeclaredCacheHit) with the
// writer keys alone: the restore writes the recorded outputs and reads no
// resource. For an execution it covers everything from output preparation
// through capture: the pre-run detach, the version-stamp refresh, the
// preBuild hook, the subprocess and its retries, the capture into the cache
// and the drift judgement (executeJobGroup). The reader keys span the same
// stretch, since the subprocess reads the resource and the capture stores
// what it read. A finalizer takes no lock: it reclaims invocation-scoped
// resources on the cleanup path, and a cleanup never waits for another
// session.
//
// prepareRun's version-stamp seed is not excluded: it takes no lock. It runs
// before any cache key exists and rewrites a project's stamp only when the
// identity recorded there (every field except the build time) differs from
// this session's: after the revision, the dirty state or a source binding
// changed. Once one session wrote the current identity, a session over the
// same worktree state writes nothing. Another session that reads or captures
// that project's .gen while a seed writes can see the new stamp, or a partial
// one, because the write is not atomic. A restore re-stamps what it restores
// (restampRestoredVersionStamp), so a captured stamp is never served as is.
// Locking the seed would make each session wait, before its first cache key,
// for every execution another session runs in any planned project.
//
// # Deadlock freedom
//
// A task takes every key it needs in one call, in the global lock order
// (outputLockOrder): its command-output keys, then project keys, then the
// workspace key, by ascending id within each class, whatever each key's mode;
// a batch takes the union of its members' keys the same way. The class order
// keeps a waiter from holding a wider key: a session that waits for another
// session's command output holds no project key meanwhile, so a third
// session's preparation in that project never waits for an execution it does
// not share. While a task holds a key it waits only for keys later in that
// order, or for work that finishes without it: its subprocess, and a preBuild
// hook another task of the session runs under its own keys and a bounded
// timeout. Every other wait of the lifecycle happens before the acquisition
// and holds no key: the cache lease, a shared-execution leader, a restore. A
// task may hold its cache lease while it waits for a key, and no key holder
// ever waits for a lease. Releasing the preparation keys early only shortens
// a hold. A shared hold excludes fewer holders than an exclusive one. The
// drain, the one in-session wait, keeps the argument: an exclusive need that
// waits for its own session's readers waits, like any waiter of that key, for
// holders that obey the same rules, and the needs queued behind it wait for
// the same key. Under those rules the holder of the
// highest contended key always progresses, so the waits form no cycle. A
// shared flock is granted whenever no exclusive one is held, so a writer
// whose key other sessions keep reading waits until none does.
//
// # Nested sessions
//
// A task can start a nested putnami session in its own workspace: the
// clientgen workspace sync builds the providers it reads, and a task may run
// any command. A nested task that waited for a key its spawning task holds
// would wait for its own parent. The scheduler therefore exports as
// heldOutputLocksEnv the ids of the keys the task holds at that moment,
// together with the ids its session inherited, and a session treats an
// inherited id as already held, in either mode. The preBuild hook receives the
// ids held during the preparation, the subprocess the ids held from the end of
// the preparation on. A nested task that writes under a key its ancestor
// holds only for reading therefore does not wait: another session's writer
// still waits for the ancestor's shared hold, but another session's reader may
// read while the nested task writes. Waiting instead would mean waiting for
// its own ancestor.
//
// A nested session still waits for a key a task of another session, or a
// sibling of its spawning task, holds. That wait is the one exception to the
// argument above, which counts the subprocess as work that finishes without a
// key. When the unrelated session in turn waits for a key the spawning task
// holds, or when the spawning task reads under a shared hold and a sibling
// that holds a key the nested session needs waits for that hold to drain,
// neither progresses until one is canceled or the spawning task's timeout
// kills its subprocess; that task then fails with a timeout. A wait line
// printed before names the other session's holder; a sibling's wait for its
// own session prints none.
//
// # Waiting
//
// A busy key is polled with LOCK_NB every outputLockPollInterval, so a
// canceled session stops waiting promptly and no goroutine stays parked in
// flock(2). No wall-clock bound turns a wait into a failure. The first busy
// poll prints one line. Only an exclusive holder records itself in the lock
// file, since it is then the only holder: its session, pid and tasks, and
// whether any of those tasks writes under the key. It rewrites the record
// whenever the set of its tasks holding the key changes and empties it before
// the last one unlocks. Shared holders never write the file, because several
// sessions hold it at once. An exclusive request that a shared request would
// pass, and that still fails right after, is blocked by readers: its line
// says another putnami process is reading the subject, and names no one.
// Otherwise the line names the recorded holder and says it is writing the
// subject, or reading it when none of its tasks writes; a record whose pid is
// not alive names no one, so the line never names a holder that is gone. A
// wait for the session's own readers prints nothing.
//
// Lock files live in .putnami/locks/task-outputs. No input walk visits
// .putnami and no declared output root contains that directory. The files are
// never deleted: a waiter that locked an unlinked file and a newcomer that
// created its replacement would each hold a lock on a different inode.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/store"
)

// heldOutputLocksEnv carries the comma-separated ids of the task-output locks
// held for a subprocess: the ids its own task holds and the ids that task's
// session inherited. It is execution-only: runJob sets it on cmd.Env last,
// runPreBuildHook passes it to the hook last, and no cache key, run marker or
// job context reads it.
const heldOutputLocksEnv = "PUTNAMI_HELD_OUTPUT_LOCKS"

// outputLockPollInterval is how often a busy key is polled. It bounds how long
// a canceled wait takes to return and how late a released key is noticed.
const outputLockPollInterval = 50 * time.Millisecond

// outputLockHolderReads is how many busy polls may find no live holder record
// (the holder is writing it, or it names a process that is gone) before the
// wait line is printed without a holder.
const outputLockHolderReads = 3

// outputLockKey is one lock a task needs.
type outputLockKey struct {
	// id is the lock file stem and the token heldOutputLocksEnv carries.
	id string
	// name is the canonical key, e.g. command-output:tooling/cli/test.
	name string
	// subject is the workspace-relative location the wait line names.
	subject string
	// preparation is true when no task of the acquisition needs the key after
	// its preparation: the hold releases it before the subprocess starts.
	preparation bool
	// exclusive is true when a task of the acquisition writes under the key:
	// a writer key, or a preparation that writes the project tree. A key the
	// acquisition only reads under is shared.
	exclusive bool
}

// outputLockHolder is the record a session holding a key exclusively keeps in
// its lock file, and a waiter reads to name it.
type outputLockHolder struct {
	Session string `json:"session,omitempty"`
	PID     int    `json:"pid"`
	// Task is the first of Tasks.
	Task string `json:"task,omitempty"`
	// Tasks are the session's tasks holding the lock, in acquisition order.
	Tasks []string `json:"tasks,omitempty"`
	Lock  string   `json:"lock,omitempty"`
	// Reading is true when none of Tasks writes under the lock: the task
	// that locked it exclusively released, and readers of the session that
	// joined its hold still hold it. A record without it names writers.
	Reading bool `json:"reading,omitempty"`
}

// taskOutputLocks is one session's view of the workspace's task-output locks.
// A nil manager takes no lock.
type taskOutputLocks struct {
	root string
	dir  string
	// holder is this session's identity; the task and lock fields are filled
	// per lock.
	holder outputLockHolder
	// inherited holds the ids an ancestor task holds for this session.
	inherited map[string]bool
	// notify receives the one wait line of a contended acquisition.
	notify func(message string)
	// onOwnSessionWait observes a task that waits for its own session's shared
	// hold to drain, or queues behind such a task. Tests install it; production
	// leaves it nil.
	onOwnSessionWait func(task string)

	mu   sync.Mutex
	held map[string]*outputLockEntry
}

// outputLockEntry is one key this session holds or is acquiring.
type outputLockEntry struct {
	// ready is closed once the acquisition settled, either way.
	ready chan struct{}
	// key is the key the entry locks.
	key outputLockKey
	// exclusive is the mode the entry locks its file in.
	exclusive bool
	// lock is nil until acquired.
	lock *flock.Lock
	// tasks has one element per reference the session holds on the acquired
	// lock, in acquisition order: its length is the reference count.
	tasks []string
	// writes tells, for each element of tasks, whether that reference writes
	// under the key.
	writes []bool
	// successor is the exclusive acquisition that waits for this shared entry
	// to drain. While it is set no need joins this entry: each waits for the
	// successor instead.
	successor *outputLockEntry
	// drained is closed when the last reference is released.
	drained chan struct{}
}

// newOutputLockEntry returns an entry that is not acquired yet, in key's mode.
func newOutputLockEntry(key outputLockKey) *outputLockEntry {
	return &outputLockEntry{
		ready:     make(chan struct{}),
		key:       key,
		exclusive: key.exclusive,
		drained:   make(chan struct{}),
	}
}

// outputLockHold is the references one acquisition holds. A nil hold holds
// nothing.
type outputLockHold struct {
	m    *taskOutputLocks
	task string
	// keys are every key the acquisition covers, inherited ones included, in
	// the global lock order.
	keys []outputLockKey

	mu sync.Mutex
	// taken are the keys this hold references, in acquisition order.
	taken []outputLockKey
	// prepared is true once endPreparation released the preparation keys.
	prepared bool
}

// newTaskOutputLocks returns the manager for one session of the workspace at
// root. inherited is the heldOutputLocksEnv value the session started with.
func newTaskOutputLocks(root string, holder outputLockHolder, inherited string, notify func(string)) *taskOutputLocks {
	if root == "" {
		return nil
	}
	canonical := canonicalOutputLockRoot(root)
	m := &taskOutputLocks{
		root:      canonical,
		dir:       filepath.Join(root, ".putnami", "locks", "task-outputs"),
		holder:    holder,
		inherited: make(map[string]bool),
		notify:    notify,
		held:      make(map[string]*outputLockEntry),
	}
	for _, id := range strings.Split(inherited, ",") {
		if id = strings.TrimSpace(id); id != "" {
			m.inherited[id] = true
		}
	}
	return m
}

// canonicalOutputLockRoot resolves the workspace root the key ids are derived
// from, so two spellings of one directory derive the same ids: a directory link,
// a junction on Windows included, is followed.
func canonicalOutputLockRoot(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if resolved, err := dirlink.Resolve(root); err == nil {
		root = resolved
	}
	return filepath.Clean(root)
}

// taskOutputLockNames returns the canonical names of the writer keys one task
// holds until its capture. See the file comment for why each key has the
// granularity it has.
func taskOutputLockNames(job *ScheduledJob) []string {
	if job == nil || job.Project == nil || job.JobDef == nil {
		return nil
	}
	project := projectRelPath(job)
	names := []string{"command-output:" + path.Join(project, jobCommandName(job))}
	declaration := taskDeclarationOf(job)
	if declaration == nil {
		return names
	}
	projectRooted, workspaceRooted := false, false
	for _, output := range declaration.Outputs {
		if output.IsInvocationScoped() {
			continue
		}
		switch output.EffectiveRoot() {
		case extension.OutputRootProject:
			projectRooted = true
		case extension.OutputRootWorkspace:
			workspaceRooted = true
		}
	}
	if projectRooted {
		names = append(names, projectOutputLockName(job))
	}
	if workspaceRooted {
		names = append(names, "workspace:")
	}
	return names
}

// taskReadLockNames returns the canonical key names one executing task holds,
// shared, until its capture: for each resource its definition reads and does
// not write, the key that resource's writers hold until theirs. A
// project-scoped resource maps to the task's project key, a workspace-scoped
// one to the workspace key. The sources resource maps to none: the tasks that
// write it mutate the source tree, which no output key covers.
func taskReadLockNames(job *ScheduledJob) []string {
	if job == nil || job.Project == nil || job.JobDef == nil {
		return nil
	}
	written := make(map[string]bool, len(job.JobDef.Writes))
	for _, ref := range job.JobDef.Writes {
		written[resourceConflictKey(job.Project.ID, ref)] = true
	}
	projectRead, workspaceRead := false, false
	for _, ref := range job.JobDef.Reads {
		if ref.ID == extension.ResourceIDSources || written[resourceConflictKey(job.Project.ID, ref)] {
			continue
		}
		if ref.EffectiveScope() == extension.ResourceScopeWorkspace {
			workspaceRead = true
		} else {
			projectRead = true
		}
	}
	var names []string
	if projectRead {
		names = append(names, projectOutputLockName(job))
	}
	if workspaceRead {
		names = append(names, "workspace:")
	}
	return names
}

// projectOutputLockName is the key of job's project tree.
func projectOutputLockName(job *ScheduledJob) string {
	return "project:" + projectRelPath(job)
}

// keyID is the lock file stem of the key name in this workspace.
func (m *taskOutputLocks) keyID(name string) string {
	sum := sha256.Sum256([]byte(m.root + "\x00" + name))
	return hex.EncodeToString(sum[:16])
}

// outputLockSubject renders a key name as the location the wait line names.
func outputLockSubject(name string) string {
	kind, rest, _ := strings.Cut(name, ":")
	switch kind {
	case "command-output":
		return path.Join(".putnami", "out", rest)
	case "project":
		return "the project-rooted outputs of " + rest
	default:
		return "the workspace-rooted declared outputs"
	}
}

// keysFor returns the unique keys the jobs need, in the global lock order
// every session acquires in (outputLockOrder). preparing reports the jobs
// whose preparation writes their project tree; nil reports none. reading adds
// the reader keys of the resources the jobs read: an execution reads them, a
// restore does not. A key is a preparation key when every job that needs it
// needs it for its preparation alone, and exclusive when any job writes under
// it.
func (m *taskOutputLocks) keysFor(jobs []*ScheduledJob, preparing func(*ScheduledJob) bool, reading bool) []outputLockKey {
	index := make(map[string]int)
	var keys []outputLockKey
	add := func(name string, exclusive, preparation bool) {
		id := m.keyID(name)
		if i, seen := index[id]; seen {
			keys[i].preparation = keys[i].preparation && preparation
			keys[i].exclusive = keys[i].exclusive || exclusive
			return
		}
		index[id] = len(keys)
		keys = append(keys, outputLockKey{
			id:          id,
			name:        name,
			subject:     outputLockSubject(name),
			preparation: preparation,
			exclusive:   exclusive,
		})
	}
	for _, job := range jobs {
		names := taskOutputLockNames(job)
		for _, name := range names {
			add(name, true, false)
		}
		if len(names) > 0 && preparing != nil && preparing(job) {
			add(projectOutputLockName(job), true, true)
		}
		if reading {
			for _, name := range taskReadLockNames(job) {
				add(name, false, false)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool { return outputLockOrder(keys[i], keys[j]) < 0 })
	return keys
}

// outputLockOrder is the one global acquisition order: command-output keys,
// then project keys, then the workspace key, and ascending id within a class.
// The id is a digest of the root and the name, so the order is total and the
// same in every session of the workspace.
func outputLockOrder(a, b outputLockKey) int {
	if c := outputLockClass(a.name) - outputLockClass(b.name); c != 0 {
		return c
	}
	return strings.Compare(a.id, b.id)
}

// outputLockClass ranks a key name's kind from the narrowest to the widest.
func outputLockClass(name string) int {
	kind, _, _ := strings.Cut(name, ":")
	switch kind {
	case "command-output":
		return 0
	case "project":
		return 1
	default:
		return 2
	}
}

// acquire takes every key the jobs need, in the global lock order, and waits for
// each key another session holds in a conflicting mode. preparing and reading
// are as for keysFor. A nil manager, and an error, return a nil hold.
func (m *taskOutputLocks) acquire(
	ctx context.Context,
	jobs []*ScheduledJob,
	preparing func(*ScheduledJob) bool,
	reading bool,
) (*outputLockHold, error) {
	if m == nil || len(jobs) == 0 {
		return nil, nil
	}
	hold := &outputLockHold{m: m, task: jobs[0].Key(), keys: m.keysFor(jobs, preparing, reading)}
	for _, key := range hold.keys {
		if m.inherited[key.id] {
			continue
		}
		if err := m.acquireOne(ctx, key, hold.task); err != nil {
			hold.release()
			return nil, err
		}
		hold.taken = append(hold.taken, key)
	}
	return hold, nil
}

// value is the heldOutputLocksEnv value for what the hold covers now: the
// inherited ids and the ids of its keys, without the preparation keys once
// the preparation ended.
func (h *outputLockHold) value() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.m.inherited)+len(h.keys))
	for id := range h.m.inherited {
		ids = append(ids, id)
	}
	for _, key := range h.keys {
		if h.m.inherited[key.id] || (h.prepared && key.preparation) {
			continue
		}
		ids = append(ids, key.id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// endPreparation releases the preparation keys and returns ctx carrying the
// value the subprocesses receive from then on. A second call changes nothing.
func (h *outputLockHold) endPreparation(ctx context.Context) context.Context {
	if h == nil {
		return ctx
	}
	h.mu.Lock()
	if !h.prepared {
		h.prepared = true
		kept := make([]outputLockKey, 0, len(h.taken))
		for _, key := range h.taken {
			if key.preparation {
				h.m.releaseOne(key.id, h.task)
				continue
			}
			kept = append(kept, key)
		}
		h.taken = kept
	}
	h.mu.Unlock()
	return context.WithValue(ctx, heldOutputLocksKey{}, h.value())
}

// release drops every reference the hold still has. A second call changes
// nothing.
func (h *outputLockHold) release() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.taken) - 1; i >= 0; i-- {
		h.m.releaseOne(h.taken[i].id, h.task)
	}
	h.taken = nil
}

// acquireOne takes one reference on key for task, in key's mode. The first
// task of the session to need the key locks its file; the others wait for that
// acquisition and join it, except an exclusive need of a shared hold: it waits
// until the session released its last reference on the shared hold, then
// locks the file exclusively, and every need that arrives meanwhile waits for
// that acquisition.
func (m *taskOutputLocks) acquireOne(ctx context.Context, key outputLockKey, task string) error {
	for {
		m.mu.Lock()
		entry := m.held[key.id]
		if entry == nil {
			entry = newOutputLockEntry(key)
			m.held[key.id] = entry
			m.mu.Unlock()
			return m.settle(ctx, entry, task)
		}
		if entry.lock != nil && entry.successor == nil {
			if entry.exclusive || !key.exclusive {
				entry.tasks = append(entry.tasks, task)
				entry.writes = append(entry.writes, key.exclusive)
				m.recordHolder(entry)
				m.mu.Unlock()
				return nil
			}
			next := newOutputLockEntry(key)
			entry.successor = next
			m.mu.Unlock()
			m.ownSessionWait(task)
			return m.awaitDrain(ctx, entry, next, task)
		}
		ready := entry.ready
		if entry.successor != nil {
			ready = entry.successor.ready
		}
		queued := entry.successor != nil
		m.mu.Unlock()
		if queued {
			m.ownSessionWait(task)
		}
		select {
		case <-ready:
			// Acquired: the next pass joins it. Failed: the entry is gone, or
			// the shared entry it waited behind is joinable again, and the next
			// pass decides anew.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// settle locks the file of entry, the session's entry for its key, for task,
// and publishes the outcome to the needs that wait for it.
func (m *taskOutputLocks) settle(ctx context.Context, entry *outputLockEntry, task string) error {
	lock, err := m.lockFile(ctx, entry.key, entry.exclusive, task)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if m.held[entry.key.id] == entry {
			delete(m.held, entry.key.id)
		}
	} else {
		entry.lock = lock
		entry.tasks = []string{task}
		entry.writes = []bool{entry.key.exclusive}
		m.recordHolder(entry)
	}
	close(entry.ready)
	return err
}

// awaitDrain waits until the session's shared entry has no reference left,
// which installs next, its exclusive successor, as the session's entry, and
// then locks next for task. A canceled wait withdraws next, so the needs
// queued behind it decide anew.
func (m *taskOutputLocks) awaitDrain(ctx context.Context, entry, next *outputLockEntry, task string) error {
	select {
	case <-entry.drained:
		return m.settle(ctx, next, task)
	case <-ctx.Done():
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.successor == next {
		entry.successor = nil
	}
	if m.held[next.key.id] == next {
		delete(m.held, next.key.id)
	}
	close(next.ready)
	return ctx.Err()
}

// ownSessionWait reports a task that waits for its own session's readers.
func (m *taskOutputLocks) ownSessionWait(task string) {
	if m.onOwnSessionWait != nil {
		m.onOwnSessionWait(task)
	}
}

// releaseOne drops task's reference. With references left it rewrites the
// holder record; with the last one it empties the record, unlocks the file,
// and hands the key to the exclusive successor waiting for the drain, if any.
// All of it happens under the mutex, so a task of this session that needs the
// key next never finds the file still locked by this session, and a waiter
// never reads a record naming a task that released.
func (m *taskOutputLocks) releaseOne(id, task string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.held[id]
	if entry == nil || entry.lock == nil || len(entry.tasks) == 0 {
		return
	}
	i := slices.Index(entry.tasks, task)
	if i < 0 {
		i = len(entry.tasks) - 1
	}
	entry.tasks = slices.Delete(entry.tasks, i, i+1)
	entry.writes = slices.Delete(entry.writes, i, i+1)
	if len(entry.tasks) > 0 {
		m.recordHolder(entry)
		return
	}
	if entry.successor != nil {
		m.held[id] = entry.successor
	} else {
		delete(m.held, id)
	}
	if entry.exclusive {
		_ = entry.lock.File().Truncate(0)
	}
	_ = entry.lock.Release()
	close(entry.drained)
}

// lockFile locks key's file, exclusively or shared, polling while another
// session holds it in a conflicting mode, until it is acquired or ctx ends.
func (m *taskOutputLocks) lockFile(ctx context.Context, key outputLockKey, exclusive bool, task string) (*flock.Lock, error) {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return nil, fmt.Errorf("prepare task-output locks: %w", err)
	}
	lockPath := filepath.Join(m.dir, key.id+".lock")
	announced := false
	reads := 0
	ticker := time.NewTicker(outputLockPollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := flock.Acquire(lockPath, exclusive, true)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, flock.ErrBusy) {
			return nil, fmt.Errorf("lock %s: %w", key.subject, err)
		}
		if !announced {
			if exclusive && heldForReading(lockPath) {
				// Readers held the key a moment ago. When the last of them has
				// released since, the key is free: take it without a line.
				if lock, err := flock.Acquire(lockPath, true, true); err == nil {
					return lock, nil
				}
				m.announce(outputLockReadWaitMessage(task, key))
				announced = true
			} else {
				holder, known := readOutputLockHolder(lockPath)
				reads++
				if known || reads >= outputLockHolderReads {
					m.announce(outputLockWaitMessage(task, key, holder, known))
					announced = true
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// heldForReading reports whether the lock file at lockPath can be locked
// shared right now. After an exclusive request found it busy, that means
// readers hold it. The probe releases its lock at once.
func heldForReading(lockPath string) bool {
	lock, err := flock.Acquire(lockPath, false, true)
	if err != nil {
		return false
	}
	_ = lock.Release()
	return true
}

// recordHolder writes this session's identity and the entry's holding tasks
// into the lock file of an exclusive entry; a shared entry writes nothing,
// because other sessions may hold the file with it. The caller holds the
// mutex. The record is written over the previous one before the file is
// truncated to its length, so a reader never finds the file empty while the
// lock is held; a reader decodes the first JSON value and ignores a longer
// previous record's tail.
func (m *taskOutputLocks) recordHolder(entry *outputLockEntry) {
	if !entry.exclusive {
		return
	}
	holder := m.holder
	holder.Task = entry.tasks[0]
	holder.Tasks = slices.Clone(entry.tasks)
	holder.Lock = entry.key.name
	holder.Reading = !slices.Contains(entry.writes, true)
	data, err := json.Marshal(holder)
	if err != nil {
		return
	}
	data = append(data, '\n')
	file := entry.lock.File()
	if _, err := file.WriteAt(data, 0); err != nil {
		return
	}
	_ = file.Truncate(int64(len(data)))
}

func (m *taskOutputLocks) announce(message string) {
	if m.notify != nil {
		m.notify(message)
		return
	}
	printOutputLockWait(message)
}

// printOutputLockWait writes a wait line to stderr, which keeps it out of
// machine output on stdout.
func printOutputLockWait(message string) {
	iox.Fprintf(os.Stderr, "putnami: %s\n", message)
}

// readOutputLockHolder reads the holder record of a lock file. known is false
// when the file holds no complete record, or when the process it names is not
// alive: a holder killed before it emptied its record left it behind.
func readOutputLockHolder(lockPath string) (outputLockHolder, bool) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return outputLockHolder{}, false
	}
	var holder outputLockHolder
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&holder); err != nil || !store.ProcessAlive(holder.PID) {
		return outputLockHolder{}, false
	}
	return holder, true
}

// outputLockWaitMessage is the one line a contended acquisition prints.
func outputLockWaitMessage(task string, key outputLockKey, holder outputLockHolder, known bool) string {
	who := "another putnami process"
	if known {
		if holder.Session != "" {
			who = fmt.Sprintf("session %s (pid %d", holder.Session, holder.PID)
		} else {
			who = fmt.Sprintf("another putnami process (pid %d", holder.PID)
		}
		tasks := holder.Tasks
		if len(tasks) == 0 && holder.Task != "" {
			tasks = []string{holder.Task}
		}
		switch len(tasks) {
		case 0:
		case 1:
			who += ", task " + tasks[0]
		default:
			who += fmt.Sprintf(", task %s and %d more", tasks[0], len(tasks)-1)
		}
		who += ")"
	}
	doing := "writing"
	if known && holder.Reading {
		doing = "reading"
	}
	return fmt.Sprintf("%s waits for %s, which is %s %s", task, who, doing, key.subject)
}

// outputLockReadWaitMessage is the one line an exclusive acquisition prints
// while readers hold the key. Readers record no holder, so it names none.
func outputLockReadWaitMessage(task string, key outputLockKey) string {
	return fmt.Sprintf("%s waits for another putnami process, which is reading %s", task, key.subject)
}

// heldOutputLocksKey carries a task's heldOutputLocksEnv value to runJob and
// to the preBuild hook.
type heldOutputLocksKey struct{}

// withHeldOutputLocks returns ctx carrying the heldOutputLocksEnv value runJob
// sets on the subprocess. An empty value leaves ctx unchanged.
func withHeldOutputLocks(ctx context.Context, value string) context.Context {
	if value == "" {
		return ctx
	}
	return context.WithValue(ctx, heldOutputLocksKey{}, value)
}

// heldOutputLocksValue is the heldOutputLocksEnv value ctx carries, or "".
func heldOutputLocksValue(ctx context.Context) string {
	value, _ := ctx.Value(heldOutputLocksKey{}).(string)
	return value
}

// applyHeldOutputLocks sets heldOutputLocksEnv from ctx, last, so neither the
// inherited environment nor a manifest's Env can state another value. Without
// a value in ctx the environment is returned unchanged.
func applyHeldOutputLocks(ctx context.Context, env []string) []string {
	value := heldOutputLocksValue(ctx)
	if value == "" {
		return env
	}
	return setEnv(env, heldOutputLocksEnv, value)
}

// heldOutputLocksHookEnv is the environment a preBuild hook receives after
// its manifest's Env: heldOutputLocksEnv from ctx, or nothing, in which case
// the hook inherits the session's own value like the subprocess does.
func heldOutputLocksHookEnv(ctx context.Context) []string {
	value := heldOutputLocksValue(ctx)
	if value == "" {
		return nil
	}
	return []string{heldOutputLocksEnv + "=" + value}
}

// announceOutputLockWait delivers a contended task-output lock's wait line.
func (s *Scheduler) announceOutputLockWait(message string) {
	if s.onOutputLockWait != nil {
		s.onOutputLockWait(message)
		return
	}
	printOutputLockWait(message)
}

// preBuildHookKey names the extension+project pair whose preBuild hook a
// session runs at most once (runPreBuildHook's slot key).
func preBuildHookKey(job *ScheduledJob) string {
	return job.Extension.Name + ":" + job.Project.Name
}

// preparationWritesProjectTree reports whether preparing job writes its
// project tree: this session has not yet started the extension's preBuild hook
// for the project (runPreBuildHook runs it with the project as its working
// directory and .gen as its output root), or refreshVersionFile has not yet
// stamped the project in this session. A hook slot and versionFiles[project]
// are only created inside a preparation that already holds the project key,
// so a task that reads them absent takes the key, and a task that reads them
// present writes nothing: it waits for the session's hook run or skips the
// stamp.
func (s *Scheduler) preparationWritesProjectTree(job *ScheduledJob) bool {
	if job == nil || job.Project == nil {
		return false
	}
	if job.Extension != nil && job.Extension.Hooks != nil && job.Extension.Hooks.PreBuild != nil {
		s.hooksMu.Lock()
		_, started := s.hookSlots[preBuildHookKey(job)]
		s.hooksMu.Unlock()
		if !started {
			return true
		}
	}
	s.versionFilesMu.Lock()
	defer s.versionFilesMu.Unlock()
	return !s.versionFiles[job.Project.Path]
}

// lockTaskOutputs acquires the task-output locks the jobs need; preparing and
// reading are as for keysFor. It returns ctx carrying the value the preBuild
// hook and the subprocesses inherit, and the hold, nil on error.
func (s *Scheduler) lockTaskOutputs(
	ctx context.Context,
	jobs []*ScheduledJob,
	preparing func(*ScheduledJob) bool,
	reading bool,
) (context.Context, *outputLockHold, error) {
	hold, err := s.outputLocks.acquire(ctx, jobs, preparing, reading)
	if err != nil {
		return ctx, nil, err
	}
	return withHeldOutputLocks(ctx, hold.value()), hold, nil
}
