//go:build linux || darwin

package machineproc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/sdk/extension/gitread"
)

// The worker's memory of the last run it served per worktree, and whether a
// request could be answered from it (CLI ADR 0116 §6a). The worker decides;
// the engine answers each of its runs of a request decided answerable from
// what the held run's engine run at the same position loaded and planned
// (RunMemory, engine.HeldRuns), and the scheduler each plan from the record
// the held run's plan left (scheduler.HeldRuns). Either one computes what it
// holds nothing for.

// memorySyncTimeout bounds the sync point a decision waits for.
const memorySyncTimeout = 2 * time.Second

// lastMemoryName records the worker's last decision; `putnami machine status`
// shows it.
const lastMemoryName = "last-memory.json"

// heldRun is what a worker keeps of the last run it served in a worktree.
type heldRun struct {
	request string
	seq     uint64
	witness map[string]string
	// plans are the records the run's plans left the scheduler to answer the
	// next request with, in the order the run executed them; nil when one of
	// them left none.
	plans []any
	// runs are what the run's engine runs computed before they executed, in
	// order; nil when one of them left nothing or plans is nil.
	runs []heldEngineRun
}

// heldEngineRun is what an engine run computed before it executed: its
// workspace, extensions, selection and plan (engine.HeldRuns), and the
// position of the plan it handed the scheduler, whose record answers it.
type heldEngineRun struct {
	record any
	plan   int
}

var memory = struct {
	sync.Mutex
	watcher *Watcher
	failed  bool
	watched map[string]bool
	held    map[string]*heldRun
	gitDirs map[string][]string
	// run is the memory of the run the worker serves now.
	run runMemory
}{watched: map[string]bool{}, held: map[string]*heldRun{}, gitDirs: map[string][]string{}}

// runMemory is what the scheduler reads and leaves during the run the worker
// serves: the records the held run's plans left, offered to the run's plans
// in order when the worker decided it answerable, and the records its own
// plans leave. It is armed only for a run that may be kept.
type runMemory struct {
	armed bool
	root  string
	// offered are the held run's records; nil unless the run is answerable.
	offered []any
	// taken counts the plans that asked for a record.
	taken int
	// plans are the records the run's plans left; partial says one plan left
	// none, or ran in another workspace, so the run holds none.
	plans   []any
	partial bool
	// answered and computed count the plans answered from a record and the
	// plans that ran; planAnswered says it of each plan, in order.
	answered, computed int
	planAnswered       []bool
	// check is the state the decision took, which an engine run takes again
	// before it skips loading and planning; nil unless the run is answerable.
	check *memoryCheck
	// offeredRuns are what the held run's engine runs computed; nil unless
	// the run is answerable.
	offeredRuns []heldEngineRun
	// runsTaken counts the engine runs that asked, and runPlan is the
	// position of the plan the current one hands the scheduler.
	runsTaken, runPlan int
	// runs are what the run's engine runs computed; runsPartial says one of
	// them left nothing, or its plan was not answered from a record.
	runs        []heldEngineRun
	runsPartial bool
	// skipped and planned count the engine runs answered from what the held
	// run computed and those that loaded and planned.
	skipped, planned int
}

// memoryCheck is the state a decision took before the run: the journal
// sequence after the sync point and the witnesses of what the journal does
// not see.
type memoryCheck struct {
	root    string
	request string
	seq     uint64
	witness map[string]string
	started time.Time
}

// RunMemory is the worker's memory as the engine and the scheduler read it
// (engine.HeldRuns, scheduler.HeldRuns): main installs it in a worker, never
// elsewhere.
type RunMemory struct{}

// Take returns the record the held run left for the next plan of the run in
// root, or nil: for a run not decided answerable, past the plans the held run
// executed, and once a plan of the run was computed, after which the held
// records no longer describe what the run left on disk.
func (RunMemory) Take(root string) any {
	memory.Lock()
	defer memory.Unlock()
	r := &memory.run
	if !r.armed || !sameRoot(r.root, root) {
		return nil
	}
	i := r.taken
	r.taken++
	if r.computed > 0 || i >= len(r.offered) {
		return nil
	}
	return r.offered[i]
}

// Keep records what a plan of the run left, and whether it was answered.
func (RunMemory) Keep(root string, record any, answered bool) {
	memory.Lock()
	defer memory.Unlock()
	r := &memory.run
	if !r.armed {
		return
	}
	if record == nil || !sameRoot(r.root, root) {
		r.partial = true
	}
	r.plans = append(r.plans, record)
	r.planAnswered = append(r.planAnswered, answered)
	if answered {
		r.answered++
	} else {
		r.computed++
	}
}

// TakeRun returns what the held run's engine run at the same position
// computed before it executed, for the next engine run of the run in root, or
// nil: for a run not decided answerable, past the engine runs the held run
// made, when the plan this one would hand the scheduler is not the one whose
// record the held run paired with it, once anything of the run was computed,
// and when anything the decision witnessed moved since it was taken. What ran
// in this process before the engine, a first-use bootstrap or a before-hook,
// can write the worktree or move Git, and the engine then loads and plans what
// is there (CLI ADR 0116 §6a).
func (RunMemory) TakeRun(root string) any {
	memory.Lock()
	r := &memory.run
	if !r.armed || !sameRoot(r.root, root) {
		memory.Unlock()
		return nil
	}
	i := r.runsTaken
	r.runsTaken++
	r.runPlan = len(r.plans)
	if r.check == nil || r.computed > 0 || r.planned > 0 || i >= len(r.offeredRuns) ||
		r.offeredRuns[i].plan != r.runPlan || r.taken != r.runPlan {
		memory.Unlock()
		return nil
	}
	offered, check, w := r.offeredRuns[i].record, r.check, memory.watcher
	memory.Unlock()
	if w == nil || w.Sync(check.root, memorySyncTimeout) != nil ||
		worktreeChanges(w.Since(check.root, check.seq), check.root) != "" ||
		movedWitness(check.witness, outsideWitness(check.root)) != "" {
		return nil
	}
	return offered
}

// KeepRun records what an engine run of the run computed before it executed,
// and whether it was answered from what TakeRun returned. It is held only when
// the engine run handed the scheduler one plan and the scheduler answered it
// from the held run's record: a value whose plan executed is not held, so what
// a later run reuses was never touched by an execution.
func (RunMemory) KeepRun(root string, record any, skipped bool) {
	memory.Lock()
	defer memory.Unlock()
	r := &memory.run
	if !r.armed {
		return
	}
	if skipped {
		r.skipped++
	} else {
		r.planned++
	}
	p := r.runPlan
	if record == nil || !sameRoot(r.root, root) || len(r.plans) != p+1 || r.plans[p] == nil || !r.planAnswered[p] {
		r.runsPartial = true
		return
	}
	r.runs = append(r.runs, heldEngineRun{record: record, plan: p})
}

// sameRoot says whether a and b name the same worktree, through any symbolic
// link: the engine resolves the workspace root its own way.
func sameRoot(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// beforeRun decides whether req could be answered from memory, and takes the
// state the run is held under when it qualifies. It returns nil when the
// request names no worktree or the platform has no watcher.
func beforeRun(req request) (*memoryCheck, MemoryDecision) {
	memory.Lock()
	memory.run = runMemory{}
	memory.Unlock()
	decision := MemoryDecision{Time: time.Now().UTC().Format(time.RFC3339)}
	root := worktreeRoot(req.Dir)
	decision.Root = root
	if root == "" {
		decision.Reason = "the command names no workspace"
		return nil, decision
	}
	w := memoryWatcher()
	if w == nil {
		decision.Reason = "this platform has no watcher"
		return nil, decision
	}
	memory.Lock()
	first := !memory.watched[root]
	memory.watched[root] = true
	memory.Unlock()
	if first {
		_ = w.Watch(root)
	}
	if err := w.Sync(root, memorySyncTimeout); err != nil {
		decision.Reason = "the watcher did not reach its sync point: " + err.Error()
		return nil, decision
	}
	check := &memoryCheck{
		root: root, request: requestDigest(req), started: time.Now(),
		seq: w.Since(root, ^uint64(0)).Seq, witness: outsideWitness(root),
	}
	memory.Lock()
	held := memory.held[root]
	memory.Unlock()
	switch {
	case first:
		decision.Reason = "the worktree was not watched before this request"
	case held == nil:
		decision.Reason = "no run of this worktree is held"
	case held.request != check.request:
		decision.Reason = "the request differs from the held one"
	default:
		if changed := worktreeChanges(w.Since(root, held.seq), root); changed != "" {
			decision.Reason = "the worktree changed: " + changed
		} else if moved := movedWitness(held.witness, check.witness); moved != "" {
			decision.Reason = "a witness outside the worktree moved: " + moved
		} else {
			decision.Answerable = true
		}
	}
	// The run's plans and engine runs leave their records for keep, and a run
	// decided answerable is offered the held run's: the decision and the offer
	// read the same held run.
	run := runMemory{armed: true, root: root}
	if decision.Answerable {
		run.offered, run.offeredRuns, run.check = held.plans, held.runs, check
	}
	memory.Lock()
	memory.run = run
	memory.Unlock()
	return check, decision
}

// afterRun keeps the run it followed when it ended with code 0, executed no
// task the answer could not hold, and wrote nothing in the worktree, and
// records the decision.
func afterRun(check *memoryCheck, decision MemoryDecision, code int) {
	memory.Lock()
	run := memory.run
	memory.run = runMemory{}
	memory.Unlock()
	decision.Answered = run.answered > 0 && run.computed == 0
	decision.PlanSkipped = run.skipped > 0 && run.planned == 0
	if check != nil {
		plans, runs := run.plans, run.runs
		if run.partial || len(plans) == 0 {
			plans = nil
		}
		// What an engine run computed answers only with the record of the plan
		// it handed the scheduler.
		if plans == nil || run.runsPartial || len(runs) == 0 {
			runs = nil
		}
		decision.Kept = keep(check, code, plans, runs)
	}
	if dir, err := Dir(os.Getenv, false); err == nil {
		if data, err := json.Marshal(decision); err == nil {
			path := filepath.Join(dir, lastMemoryName)
			if os.WriteFile(path+".tmp", data, 0o600) == nil {
				_ = os.Rename(path+".tmp", path)
			}
		}
	}
}

func keep(check *memoryCheck, code int, plans []any, runs []heldEngineRun) string {
	memory.Lock()
	delete(memory.held, check.root)
	w := memory.watcher
	memory.Unlock()
	if code != 0 {
		return "not kept: the run exited " + strconv.Itoa(code)
	}
	if reason := sessionDisqualifies(check.root, check.started); reason != "" {
		return "not kept: " + reason
	}
	if err := w.Sync(check.root, memorySyncTimeout); err != nil {
		return "not kept: the watcher did not reach its sync point"
	}
	if changed := worktreeChanges(w.Since(check.root, check.seq), check.root); changed != "" {
		return "not kept: the run changed the worktree: " + changed
	}
	memory.Lock()
	memory.held[check.root] = &heldRun{request: check.request, seq: check.seq, witness: check.witness, plans: plans, runs: runs}
	memory.Unlock()
	return "kept"
}

// memoryWatcher is the worker's watcher, started on first use; nil where the
// platform has none.
func memoryWatcher() *Watcher {
	memory.Lock()
	defer memory.Unlock()
	if memory.watcher == nil && !memory.failed {
		w, err := NewWatcher(defaultWatchBudget())
		if err != nil {
			memory.failed = true
			return nil
		}
		memory.watcher = w
	}
	return memory.watcher
}

// worktreeRoot is the nearest directory at or above dir that holds a
// workspace configuration, or "".
func worktreeRoot(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "putnami.workspace.json")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// requestDigest identifies what the request asks and the context it runs in.
func requestDigest(req request) string {
	h := sha256.New()
	env := append([]string(nil), req.Env...)
	sort.Strings(env)
	for _, part := range [][]string{req.Args, {req.Dir, req.Build, req.Executable, req.Fingerprint, strconv.FormatUint(uint64(req.Umask), 8), strconv.FormatBool(req.Terminal), strconv.FormatBool(req.StderrTerminal)}, env} {
		for _, s := range part {
			h.Write([]byte(s)) //nolint:errcheck // hash.Hash.Write never errors
			h.Write([]byte{0}) //nolint:errcheck // hash.Hash.Write never errors
		}
		h.Write([]byte{1}) //nolint:errcheck // hash.Hash.Write never errors
	}
	return hex.EncodeToString(h.Sum(nil))
}

// worktreeChanges names the first change the journal holds outside the
// worktree's .putnami directory, the run's own state, or "".
func worktreeChanges(j Journal, root string) string {
	state := filepath.Join(root, ".putnami")
	for _, b := range j.Broken {
		if !within(b, state) {
			return "broken " + b
		}
	}
	for _, c := range j.Changed {
		if !within(c, state) {
			return c
		}
	}
	return ""
}

// outsideWitness takes what a run reads that the worktree's journal does not
// see: the stats of Git's HEAD, configuration, packed refs and every
// directory of its refs, the digest of the index's entries (a run's status
// refreshes the index's cached stats without changing an entry), and the
// stats of the directories of the Putnami home that hold extension runtimes
// and toolchains.
func outsideWitness(root string) map[string]string {
	paths := []string{}
	for _, dir := range gitDirs(root) {
		for _, name := range []string{"HEAD", "config", "packed-refs", "shallow", "commondir"} {
			paths = append(paths, filepath.Join(dir, name))
		}
		_ = filepath.WalkDir(filepath.Join(dir, "refs"), func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				paths = append(paths, path)
			}
			return nil
		})
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"artifacts", "artifacts/sha256", "artifacts/cli-source", "toolchains", "tools"} {
			paths = append(paths, filepath.Join(home, ".putnami", name))
		}
	}
	witness := make(map[string]string, len(paths)+1)
	for _, path := range paths {
		sig, _ := SigOf(path) // an absent path is witnessed absent
		witness[path] = sigString(sig)
	}
	witness["git index entries"] = indexEntries(root)
	return witness
}

func sigString(sig FileSig) string {
	return strconv.FormatUint(sig.Dev, 16) + ":" + strconv.FormatUint(sig.Ino, 16) + ":" + strconv.FormatUint(sig.Mode, 8) + ":" +
		strconv.FormatUint(sig.Uid, 10) + ":" + strconv.FormatUint(sig.Gid, 10) + ":" + strconv.FormatInt(sig.Size, 10) + ":" +
		strconv.FormatInt(sig.ModNano, 10) + ":" + strconv.FormatInt(sig.CtimeNano, 10)
}

// indexEntries is the digest of the index's entries: mode, object, stage
// and path of each.
func indexEntries(root string) string {
	out, err := exec.Command(gitread.Executable(), "-C", root, "ls-files", "--stage", "-z").Output() //nolint:gosec // fixed arguments
	if err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:])
}

// movedWitness names the first path whose stat differs, or "".
func movedWitness(held, now map[string]string) string {
	for path, sig := range held {
		if now[path] != sig {
			return path
		}
	}
	for path := range now {
		if _, ok := held[path]; !ok {
			return path
		}
	}
	return ""
}

// gitDirs is the worktree's Git directory and its common directory, read once
// per worktree.
func gitDirs(root string) []string {
	memory.Lock()
	dirs, ok := memory.gitDirs[root]
	memory.Unlock()
	if ok {
		return dirs
	}
	out, err := exec.Command(gitread.Executable(), "-C", root, "rev-parse", "--absolute-git-dir", "--git-common-dir").Output() //nolint:gosec // fixed arguments
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		seen := map[string]bool{}
		for _, line := range lines {
			dir := line
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(root, dir)
			}
			dir = filepath.Clean(dir)
			if !seen[dir] {
				seen[dir] = true
				dirs = append(dirs, dir)
			}
		}
	}
	memory.Lock()
	memory.gitDirs[root] = dirs
	memory.Unlock()
	return dirs
}

// sessionDisqualifies reads the session the run recorded and names why it
// cannot be held: a task that did not end served from the cache, skipped, or
// executed in a step whose inputs the answer's witnesses cover.
func sessionDisqualifies(root string, started time.Time) string {
	sessions := filepath.Join(root, ".putnami", "sessions")
	entries, err := os.ReadDir(sessions)
	if err != nil {
		return "the run recorded no session"
	}
	newest, newestTime := "", time.Time{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || info.ModTime().Before(started) {
			continue
		}
		if info.ModTime().After(newestTime) {
			newest, newestTime = e.Name(), info.ModTime()
		}
	}
	if newest == "" {
		return "the run recorded no session"
	}
	data, err := os.ReadFile(filepath.Join(sessions, newest, "session.json")) //nolint:gosec // the run's own session
	if err != nil {
		return "the run's session cannot be read"
	}
	var session struct {
		Tasks []struct {
			Identity struct {
				Key string `json:"key"`
			} `json:"identity"`
			Status string `json:"status"`
			Reuse  string `json:"reuse"`
		} `json:"tasks"`
	}
	if json.Unmarshal(data, &session) != nil {
		return "the run's session cannot be read"
	}
	executed := 0
	first := ""
	for _, t := range session.Tasks {
		if t.Status == "skipped" || t.Reuse == "local-cache" {
			continue
		}
		executed++
		if first == "" {
			first = t.Identity.Key
		}
	}
	if executed > 0 {
		return strconv.Itoa(executed) + " tasks executed, first " + first
	}
	return ""
}

// ReadLastMemory is the worker's last decision, or nil.
func ReadLastMemory() *MemoryDecision {
	dir, err := Dir(os.Getenv, false)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, lastMemoryName))
	if err != nil {
		return nil
	}
	var d MemoryDecision
	if json.Unmarshal(data, &d) != nil {
		return nil
	}
	return &d
}
