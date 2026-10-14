// Declared-output capture and restore.
//
// Each cacheable task DECLARES its output contract
// (protocols/extension/task_contract.go). This file turns that contract into a
// task-owned entry: capture is exactly the declared set, and restore is exactly
// the recorded set.
//
// # What executes where, per declaration root
//
// The original design was "run the task in an isolated staging root and
// ingest whatever lands there". That is not implementable for this workspace
// today, and the reason is worth recording because it is a property of the
// extensions, not of the store:
//
//   - COMMAND-OUTPUT — captured FROM the real .putnami/out/<project>/<command>
//     directory, NOT redirected into staging. Two facts forbid the redirect.
//     First, the steps of one command READ each other's trees there:
//     package-npm reads <command-output>/lib and <command-output>/types,
//     package-docker reads <command-output>/compile. Handing a task an empty
//     staging directory would break those reads. Second, extensions do not even
//     take the path from the job context — extension-sdk/pkgmeta and
//     typescript/extension's findBuildOutput RECOMPUTE
//     <workspaceRoot>/.putnami/out/<project>/<command> from the workspace root,
//     so a redirected outputPath would be ignored and the declared output would
//     be written to the real location anyway, turning every capture into
//     ErrIncompleteCapture.
//   - PROJECT — captured FROM its real location (<project>/.gen, the configured
//     client directory). Generators write in-tree by construction; this is the
//     case the issue already anticipated.
//   - WORKSPACE — same: captured from its real location under the workspace
//     root.
//
// So the staging root is the STORE's boundary, not the process's: after the
// task runs, each declared output is staged at store.TaskStagingPath (a tree of
// hardlinks, or copies when a link is impossible) and IngestTaskEntry validates
// the declaration against it. What that buys is a baseline-independent property:
// the entry holds exactly the declared paths,
// so it never depends on which sibling step ran first or on what an earlier
// session left in the shared directory. What it does NOT buy is policing writes
// OUTSIDE the declaration; that needs the process-level isolation this
// workspace cannot take yet, and it is not this file's promise.
//
// # An executing task starts from an empty owned directory
//
// A declared DIRECTORY output is captured as it stands after the run. So that
// a required one holds only what that run wrote, the scheduler empties what
// the task owns in it before the task executes (emptyOwnedOutputs, ADR 0064):
// every entry but the subpaths the declaration cedes or preserves
// (cachekey.OutputSkippedSubpaths) and the scheduler's own build stamp. That is the set
// a restore replaces, so an execution and a restore leave the same paths
// untouched. A file another task wrote inside the owned region is removed with
// the rest, exactly as a restore of the owner's entry removes it: a path that
// must outlive its neighbor's execution has to be ceded by the owner, as the
// TypeScript build-generate cedes the config-extract schema fallback.
//
// A directory output with a literal path is emptied when it is required or
// when it lives under the command-output root (cachekey.EmptiedOutputDeclarations,
// ADR 0065). The engine owns the command-output directory and the plan gives
// each output one owner, so an optionalEmpty output there holds only what its
// task wrote. A restore of an entry that records such an output empty empties
// it the same way (materializeDeclaredOutputs), so an execution and a restore
// that write nothing leave the same directory.
//
// An optionalEmpty output under the project or the workspace root is not
// emptied: it can hold a committed file no task writes, a run that writes
// nothing there is recorded empty, and a restore of that record leaves the
// path as it stands. A file is not emptied because the task replaces it whole;
// an invocation-scoped output because it lives in a private tree created for
// the run; a pathFrom output because its path is known only after the task
// ran. A file a generator stops emitting therefore stays in an output of those
// shapes until the generator removes it.
//
// # Which tasks take this path
//
// usesDeclaredCapture is a STATIC predicate — declaration + job shape only, no
// run results — because the lookup that happens before the task runs and the
// store that happens after it MUST agree. A cacheable task must take this path:
// an incomplete declaration is run without caching rather than being captured
// from a directory whose ownership cannot be proven. Task-owned entries live at
// a format-qualified store address (task_entry.go), so an undeclared task cannot
// accidentally consume one.
//
// No command is excluded. The `package` command used to be: its shared output
// directory hosted a channel index that every packager read-merge-wrote and
// publish read back, so a cache hit restored one packager's artifact and skipped
// the merge, and publish silently skipped a channel that had really been built.
// A later fix moved each packager's record inside the
// output that packager owns, which removed the write outside a declaration
// rather than working around it — so the ordinary rule covers `package` too.
//
// Source rewriters use the same task-owned status entry, plus a cache-private
// result marker. The scheduler captures the keyed project-source digest before
// execution and recomputes it afterward. That digest and the key read a keyed
// project config as raw bytes (cachekey.JobConfigScope), so a layout-only rewrite of one
// counts as a source edit. A clean run publishes an ordinary
// reusable verdict; a run whose source digest moved is marked and every restore
// rejects it, so another worktree executes the fixer instead of receiving green
// without the edits. Source bytes are never captured or replayed.
//
// # Remote cache
//
// Tasks on this path DO consult the remote cache: a declared miss
// asks the provider before claiming the lease, under the format-qualified key
// store.RemoteTaskEntryKey — see jobs/remotecache/remote_task.go for the delivery and
// store/task_remote.go for how a format-2 entry travels the unchanged wire. A
// remote hit is published into the local store as an ordinary task-owned entry
// and then materialized by restoreDeclaredCacheHit, so local, remote and
// coalesced hits share one materialize rather than three.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const sourcesResourceID = "sources"

// sourceInputDigest hashes exactly the file inputs that contribute this task's
// cache-key file content — the project-relative set, plus the workspace-relative
// set a task may also declare. Capturing this component before execution avoids
// mistaking a changed dependency hash, runtime identity, or other non-source key
// field for a source rewrite. It shares cachekey.KeyFilePatterns with the key itself, so
// the detector cannot observe a narrower set than the key trusts.
//
// A task with NO project-relative patterns is an error, not an empty digest: its
// key is blind to the sources it rewrites, so a constant "clean" digest would
// publish a permanently reusable green verdict and the fixer would never run
// again. The caller reads that error as an unproven result and marks the entry
// non-restorable — the same answer it gives any other indeterminate comparison.
func sourceInputDigest(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	cache *store.CacheManager,
) (string, error) {
	patterns, workspacePatterns := cachekey.KeyFilePatterns(ws, job, commandParams)
	if len(patterns) == 0 {
		return "", fmt.Errorf(
			"task %q rewrites its sources but declares no keyed file patterns", job.JobDef.Name)
	}
	scope := cachekey.JobConfigScope(ws, job)
	digest, err := cache.HashFiles(filepath.Join(ws.Root, job.Project.Path), patterns, scope)
	if err != nil {
		return "", err
	}
	if len(workspacePatterns) == 0 || ws.Root == "" {
		return digest, nil
	}
	// The same whole-document rule the key applies to workspace patterns.
	workspaceDigest, err := cache.HashFiles(ws.Root, workspacePatterns,
		store.ProjectConfigScope{Verbatim: scope.Verbatim})
	if err != nil {
		return "", err
	}
	return digest + ":" + workspaceDigest, nil
}

// declaresCommandOutput reports whether a task owns bytes under the shared
// per-command directory. It is used before a real execution to detach a stale
// cache symlink before the task can mutate it, and by sharedExecutionEligible,
// which must refuse every such task: a shared-node follower would capture a
// command-output directory its leader never wrote.
func declaresCommandOutput(job *ScheduledJob) bool {
	declaration := taskDeclarationOf(job)
	if declaration == nil {
		return false
	}
	for _, output := range declaration.Outputs {
		if output.EffectiveRoot() == extension.OutputRootCommandOutput {
			return true
		}
	}
	return false
}

// versionStampWithin returns the path of the scheduler's build stamp relative
// to a directory output at rel under root, and whether that output holds it.
// The stamp lives under the project root only. The containment folds case, as
// every ownership comparison does.
func versionStampWithin(root, rel string) (string, bool) {
	if root != extension.OutputRootProject {
		return "", false
	}
	if len(versionStampRelPath) <= len(rel)+1 || versionStampRelPath[len(rel)] != '/' ||
		!strings.EqualFold(versionStampRelPath[:len(rel)], rel) {
		return "", false
	}
	return versionStampRelPath[len(rel)+1:], true
}

// emptyOwnedOutputs empties what job owns in each of its declared directory
// outputs (cachekey.EmptiedOutputDeclarations), so the execution that follows writes
// into a directory holding nothing of an earlier run.
//
// What it leaves in place is what a restore of the same output leaves in place
// (cededSubpathsOf): the subpaths the declaration cedes to another task or
// preserves for the workspace. It also leaves the build stamp, which is the
// scheduler's file and not the task's: refreshVersionFile writes it once per
// project and session, and a second task of the session that owns the
// directory holding it must still find it there.
//
// The caller holds the task-output locks that cover every emptied path: the
// command-output key always, and the project or workspace key because an
// emptied output is durable (task_output_lock.go). A command-output directory
// must already be detached from a previous session's link
// (ensureCommandOutputWritable), so no rename here reaches a store blob.
//
// An error leaves an output that may still hold an earlier run's files. The
// caller fails the task instead of executing it: a capture of that directory
// would publish those files under this run's key.
func (s *Scheduler) emptyOwnedOutputs(job *ScheduledJob) error {
	for _, declared := range cachekey.EmptiedOutputDeclarations(job) {
		if err := s.emptyDeclaredOutput(job, declared); err != nil {
			return err
		}
	}
	return nil
}

// emptyDeclaredOutput empties what job owns in one output of
// cachekey.EmptiedOutputDeclarations, and keeps what emptyOwnedOutputs keeps. An
// execution calls it, and so does a restore that empties an output, so the
// two remove the same entries. The caller holds the locks and has detached the
// command-output directory, as emptyOwnedOutputs states.
func (s *Scheduler) emptyDeclaredOutput(job *ScheduledJob, declared cachekey.EmptiedOutputDeclaration) error {
	roots := s.outputRoots()
	if roots.Root == "" {
		// Without a workspace root an output path would resolve against the
		// working directory, which is nobody's declared output.
		return fmt.Errorf("no workspace root to resolve declared outputs against")
	}
	root := declared.Output.EffectiveRoot()
	base, ok := roots.OutputRoot(job, root)
	if !ok {
		return fmt.Errorf("declared output %q has unknown root %q", declared.ID, root)
	}
	keep := cachekey.OutputSkippedSubpaths(declared.Rel, declared.Output)
	if stamp, held := versionStampWithin(root, declared.Rel); held {
		keep = append(append([]string(nil), keep...), stamp)
	}
	dest := filepath.Join(base, filepath.FromSlash(declared.Rel))
	if err := emptyTaskOutput(dest, store.ResolveScratchRoot(roots.Root), keep...); err != nil {
		return fmt.Errorf("declared output %q: %w", declared.ID, err)
	}
	return nil
}

// emptiedByRestore returns, by id, the outputs a restore of entry empties:
// each output of cachekey.EmptiedOutputDeclarations that entry records empty. A
// required output is never recorded empty, so in practice this is an
// optionalEmpty output under the command-output root. A restore of such an
// entry leaves the directory the execution that produced the entry left: the
// kept subpaths and nothing else.
func emptiedByRestore(job *ScheduledJob, entry *store.TaskEntry) map[string]cachekey.EmptiedOutputDeclaration {
	emptied := map[string]cachekey.EmptiedOutputDeclaration{}
	for _, declared := range cachekey.EmptiedOutputDeclarations(job) {
		if recorded, ok := entry.Output(declared.ID); ok && !recorded.Present() {
			emptied[declared.ID] = declared
		}
	}
	return emptied
}

// emptyTaskOutput is store.EmptyTaskOutput. It is a variable only so a test can
// make the emptying fail.
var emptyTaskOutput = store.EmptyTaskOutput

func (s *Scheduler) outputRoots() cachekey.WorkspaceRoots {
	root := ""
	if s.ws != nil {
		root = s.ws.Root
	}
	return cachekey.WorkspaceRoots{Root: root}
}

// storeDeclaredCapture publishes a task-owned entry holding exactly the job's
// declared outputs, and reports whether it did.
//
// Every failure mode ends in NO ENTRY rather than a smaller one. A declaration
// that cannot be resolved, a required output the task did not produce
// (ErrIncompleteCapture), a staging or ingest error: all leave the key
// uncached, so the next run recomputes. That is the only safe direction — a
// partial entry is indistinguishable from a complete one at lookup time and
// would restore less than the declaration promises on every future hit, on
// every machine that pulls it.
//
// A cacheable SKIP (a deterministic in-job skip, cachekey.IsCacheableResult) is captured
// from an EMPTY staging root rather than from the workspace. A skip produced no
// files, and the paths its declaration names may still hold an earlier run's
// bytes — capturing those would publish, under this key, a tree this execution
// never made. The declaration then decides the outcome by itself: an all-optional
// task records every output empty and caches its status, while a task with a
// required output is simply not cached.
func (s *Scheduler) storeDeclaredCapture(job *ScheduledJob, result *JobResult, hash string) bool {
	produced := result.Status == "success"
	// A capture that finds nothing because the task legitimately produced nothing
	// is expected; only a SUCCESSFUL run disagreeing with its declaration is a
	// problem worth a warning.
	report := slog.Warn
	if !produced {
		report = slog.Debug
	}

	plans, err := cachekey.ResolveDeclaredOutputs(s.outputRoots(), job, result.Data)
	if err != nil {
		report("declared capture skipped: cannot resolve declared outputs",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return false
	}

	scratch := store.ResolveScratchRoot(s.ws.Root)
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		slog.Warn("declared capture skipped: cannot create scratch root",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return false
	}
	staging, err := os.MkdirTemp(scratch, "task-capture-")
	if err != nil {
		slog.Warn("declared capture skipped: cannot create staging root",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return false
	}
	defer os.RemoveAll(staging)

	specs := make([]store.DeclaredEntryOutput, 0, len(plans))
	for _, plan := range plans {
		if produced {
			if err := stageDeclaredOutput(plan, store.TaskStagingPath(staging, plan.Spec)); err != nil {
				slog.Warn("declared capture skipped: cannot stage declared output",
					"project", job.Project.Name, "job", job.JobDef.Name, "output", plan.Spec.ID, "error", err)
				return false
			}
		}
		specs = append(specs, plan.Spec)
	}

	meta := &store.EntryMetadata{
		Extension:  job.Extension.Name,
		Task:       job.JobDef.Name,
		Project:    job.Project.Name,
		DurationMs: result.Duration.Milliseconds(),
	}
	entry, err := s.cache.IngestTaskEntry(staging, store.TaskEntrySpec{
		Key:      hash,
		Result:   cachekey.EntryResultFromJobResult(result),
		Metadata: meta,
		Outputs:  specs,
	})
	if err != nil {
		// A required declared output a SUCCESSFUL run did not produce means the
		// declaration and the task disagree; say so loudly and cache nothing.
		report("declared capture failed; task not cached",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return false
	}
	if s.cfg.Debug {
		// The CLI installs no debug-level slog handler, so the evidence that a
		// task traveled the declared path has to be visible at info level.
		slog.Info("declared capture stored",
			"job", job.Key(), "entry", entry.Address, "outputs", len(entry.Outputs))
	}
	return true
}

// stageDeclaredOutput materializes one declared output inside the ingest's
// staging root at the path the store derives from the same declaration.
//
// Bytes are HARDLINE, not copied: the CAS ingest reads and copies each file
// into a blob of its own (store.ensureBlobLocked), so a link here never makes a
// workspace file and a CAS blob share an inode — a later in-place write to the
// workspace file would otherwise silently corrupt every entry sharing that blob.
// Anything that cannot be linked (a symlink inside a declared subtree, a
// cross-device layout) is copied by content.
//
// A missing source is NOT an error here. Whether the task was allowed to produce
// nothing is the declaration's business, and IngestTaskEntry is the one place
// that answers it.
//
// A CEDED subpath is skipped, and that half is load-bearing rather than an
// optimization. The declaration says those bytes belong to another task, so
// capturing them would publish, under this task's key, a subtree this task does
// not produce: a later hit would resurrect whatever an earlier build happened to
// leave there, for a project whose current sources produce nothing.
// Skipping it keeps the entry equal to the declaration.
//
// The RESTORE side honors the same carve-outs (materializeDeclaredOutputs):
// the ceding task's restore leaves whatever the destination holds at a ceded
// subpath exactly as it is, the way its execution would. It used to swap a
// staged tree over the whole destination, which deleted the ceded subtree and
// relied on the owning task being restored right after — but "right after" is
// not a window other commands respect: a `package` describe that executed and a
// `publish` step reading the contract path both run concurrently with a `test`
// generate served from cache, and the bundle was gone in between — a
// remote-cache recurrence of the same race. What the owning task's restore then does is
// unchanged — a PRESENT output swaps the current sources' bytes in, and an
// optionalEmpty absence leaves a project or workspace path alone and empties a
// command-output one (materializeDeclaredOutputs).
func stageDeclaredOutput(plan cachekey.DeclaredOutputPlan, dst string) error {
	src := plan.Src
	info, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if plan.Spec.Kind == extension.OutputKindFile {
		if info.IsDir() {
			return fmt.Errorf("declared a file but %s is a directory", src)
		}
		return stageDeclaredFile(src, dst, info)
	}

	if !info.IsDir() {
		return fmt.Errorf("declared a directory but %s is not one", src)
	}
	return filepath.Walk(src, func(walked string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, walked)
		if err != nil {
			return err
		}
		if isExcludedOutputPath(plan.Excludes, filepath.ToSlash(rel)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return stageDeclaredFile(walked, target, info)
	})
}

// isExcludedOutputPath reports whether an output-relative path is a ceded
// subpath or lives inside one.
//
// The comparison folds case for the same reason ownership does: two spellings
// that alias on the default macOS or Windows filesystem cannot be owned by
// different tasks, so they cannot be captured by different tasks either.
func isExcludedOutputPath(excludes []string, rel string) bool {
	for _, exclude := range excludes {
		if strings.EqualFold(rel, exclude) ||
			(len(rel) > len(exclude) && rel[len(exclude)] == '/' && strings.EqualFold(rel[:len(exclude)], exclude)) {
			return true
		}
	}
	return false
}

func stageDeclaredFile(src, dst string, info os.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		if err := os.Link(src, dst); err == nil {
			return nil
		}
	}
	return copyDeclaredFile(src, dst, info.Mode().Perm())
}

// copyDeclaredFile copies bytes into the staging root, following symlinks.
func copyDeclaredFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// restoreDeclaredCacheHit turns a task-owned entry into a job result, putting
// every recorded output back where the task would have written it. A restore
// failure returns nil so the caller executes the task instead: recomputing is
// always a correct answer, serving a half-restored tree is not.
//
// It is the one place a hit writes the workspace, so it holds the task's
// task-output writer keys for the whole restore: another session executing or
// restoring the same outputs finishes first. It takes no reader key: a
// restore writes the recorded outputs and reads no resource another task
// writes. A lock it cannot take (the session is canceled) is a restore failure
// like any other.
func (s *Scheduler) restoreDeclaredCacheHit(
	ctx context.Context,
	job *ScheduledJob,
	hash string,
	entry *store.TaskEntry,
	mu *sync.Mutex,
	hashes map[string]string,
) *JobResult {
	if entry == nil {
		return nil
	}

	result := cachekey.JobResultFromEntryResult(entry.Result)
	if cachekey.DeclaredSourceRewriter(job) && result.SourceMutated {
		// The entry is keyed by the source bytes before the fixer ran. Reusing its
		// successful status would skip the edits in this worktree.
		return nil
	}
	_, outputs, err := s.lockTaskOutputs(ctx, []*ScheduledJob{job}, nil, false)
	defer outputs.release()
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("declared restore skipped: cannot lock task outputs; executing task",
				"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		}
		return nil
	}
	s.invalidateCapabilitySourceBindingsForRestore(job, entry)
	// The drift reference is the destination as it stands BEFORE the swap: the
	// committed bytes this checkout holds, which the recorded tree is about to
	// replace (task_drift.go).
	drift := s.snapshotRestoredDrift(job, entry)
	// A restore rewrites every recorded output, so the memoized input digests
	// those paths can move are dropped once it ended, whether or not it
	// completed (cachekey/key_movement.go).
	restored := cachekey.RestoredOutputPaths(s.outputRoots(), job, entry)
	if err := s.materializeDeclaredOutputs(job, entry); err != nil {
		s.invalidateCapabilitySourceBindingsForRestore(job, entry)
		s.cache.InvalidateFileHashesUnder(restored)
		slog.Warn("declared restore failed; executing task",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return nil
	}
	s.invalidateCapabilitySourceBindingsForRestore(job, entry)
	// A restore that replaced the project's .gen subtree also replaced the build
	// stamp inside it with the PRODUCING run's bytes. Put this run's identity back
	// before the hit is published, so no same-run consumer and no later
	// package/publish/deploy reader can observe the older revision.
	s.restampRestoredVersionStamp(job, entry)
	s.cache.InvalidateFileHashesUnder(restored)
	// A provider may legitimately omit ActionResult.Events when it does not
	// advertise the optional action-events capability, and entries written before
	// that capability carry none. Recover the reserved artifacts whose exact
	// presence the verified task-owned descriptor proves before consumers and
	// renderers observe the hit.
	cachekey.RestoreReservedDeclaredArtifactEvents(job, entry, result)
	// Judged after the restore is complete and before the hit is published as
	// this task's result: the diagnostic is this run's, never the entry's.
	result = s.judgeRestoredDrift(job, entry, result, drift)

	mu.Lock()
	hashes[job.Key()] = hash
	mu.Unlock()

	if s.cfg.Debug {
		slog.Info("declared restore served a hit", "job", job.Key(), "entry", entry.Address)
	}

	result.MarkReuse(ReuseLocalCache)
	s.replayCacheEvents(job, result)
	return result
}

// materializeDeclaredOutputs restores every recorded output at the location its
// recorded root and path resolve to for THIS job.
//
// The recorded declaration is trusted without re-checking it against the
// manifest: cache key v5 folds the task-contract digest, so a declaration change
// moves the key and an entry can only ever be served to the contract it was
// written for.
//
// An output the entry records empty is left as it stands, unless the scheduler
// empties it before an execution (emptiedByRestore). Such an output is emptied
// here through emptyDeclaredOutput, with the keep set an execution uses, so
// the restore leaves the tree the producing execution left. The store never
// decides that: MaterializeTaskOutput leaves the destination of an empty
// record untouched, and only the engine knows the declaration. A failure to
// empty is a restore failure like any other, so the caller executes the task.
func (s *Scheduler) materializeDeclaredOutputs(job *ScheduledJob, entry *store.TaskEntry) error {
	roots := s.outputRoots()
	// A restore stages under the workspace scratch root that declared capture
	// stages under, so a restore adds nothing to a destination's parent but the
	// destination itself.
	stagingRoot := ""
	if s.ws != nil {
		stagingRoot = store.ResolveScratchRoot(s.ws.Root)
	}
	emptied := emptiedByRestore(job, entry)
	needsCommandOutput := false
	for _, output := range entry.Outputs {
		_, empties := emptied[output.ID]
		if output.Root == extension.OutputRootCommandOutput && (output.Present() || empties) {
			needsCommandOutput = true
			break
		}
	}
	// The detach comes before every write the restore makes, an emptying
	// included, so no rename reaches through a link into a store blob.
	if needsCommandOutput {
		if err := s.ensureCommandOutputWritable(job); err != nil {
			return err
		}
	}

	for _, output := range entry.Outputs {
		if declared, empties := emptied[output.ID]; empties {
			if err := s.emptyDeclaredOutput(job, declared); err != nil {
				return err
			}
			continue
		}
		base, ok := roots.OutputRoot(job, output.Root)
		if !ok {
			return fmt.Errorf("recorded output %q has unknown root %q", output.ID, output.Root)
		}
		dest := filepath.Join(base, filepath.FromSlash(output.Path))
		keep := cededSubpathsOf(job, output)
		if _, err := s.cache.MaterializeTaskOutput(entry, output.ID, dest, stagingRoot, keep...); err != nil {
			return err
		}
	}
	return nil
}

// cededSubpathsOf returns the subpaths a recorded directory output cedes or
// preserves (cachekey.OutputSkippedSubpaths), output-relative and in slash form, so its
// restore leaves them alone the way its capture skipped them. They come from the job's CURRENT declaration, not
// from the entry: the entry never records them (protocol ADR 0003), and it does
// not need to, because cache key v5 folds the task-contract digest — an entry
// is only ever served to the exact declaration it was captured under, so the
// carve-outs at restore are the carve-outs at capture.
//
// The resolution is the capture's (cachekey.OutputSkippedSubpaths over the same
// declaration), which is what keeps the two sides symmetric: a carve-out the
// capture honored is one the restore honors, and one the capture dropped as
// undecidable is dropped here too.
func cededSubpathsOf(job *ScheduledJob, output store.TaskEntryOutput) []string {
	if output.Kind != extension.OutputKindDirectory {
		return nil
	}
	declaration := taskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	declared, ok := declaration.Outputs[output.ID]
	if !ok {
		return nil
	}
	return cachekey.OutputSkippedSubpaths(output.Path, declared)
}

// restoreReplacesVersionStamp reports whether materializing this entry's
// recorded outputs overwrites the project's build stamp (.gen/version.json).
//
// MaterializeTaskOutput publishes a directory output by swapping a fully staged
// tree over its destination, so a PRESENT, project-rooted directory output that
// contains the stamp necessarily replaces it — with the bytes of whichever run
// produced the entry, not with the identity the scheduler seeded for this one.
// A project-rooted FILE output recorded AT the stamp path replaces exactly the
// same bytes, just one file at a time instead of a tree at a time, so it is the
// same question with the same answer.
//
// The question is asked of the ENTRY rather than of the manifest's `writes` list
// so the compensation stays tied to the mechanism that causes it: any task whose
// restore puts the stamp back is re-stamped, whether or not it also declares the
// `gen` resource. Keying on the resource — or on the directory shape alone —
// would let a declaration that names the stamp directly reintroduce the stale
// stamp silently, since nothing else in the restore path notices that the revision on
// disk is the producing run's.
func restoreReplacesVersionStamp(entry *store.TaskEntry) bool {
	if entry == nil {
		return false
	}
	for _, output := range entry.Outputs {
		if !output.Present() {
			continue
		}
		// A recorded root is always one of the three symbolic roots:
		// validateRecordedOutputs rejects an entry carrying anything else, so an
		// empty (defaulted) root never reaches a restore.
		if output.Root != extension.OutputRootProject {
			continue
		}
		clean := path.Clean(output.Path)
		switch output.Kind {
		case extension.OutputKindDirectory:
			if clean == "." || strings.HasPrefix(versionStampRelPath, clean+"/") {
				return true
			}
		case extension.OutputKindFile:
			if clean == versionStampRelPath {
				return true
			}
		}
	}
	return false
}

// ensureCommandOutputWritable makes the shared per-command output directory a
// real, writable directory before a task-owned restore or execution writes a
// named subpath into it. Materializing through a symlink left by an earlier
// session would mutate the store blob it points at; MaterializeDirSymlink
// atomically detaches it while preserving the sibling outputs it contains.
func (s *Scheduler) ensureCommandOutputWritable(job *ScheduledJob) error {
	state := s.stateForCommandOutput(job)
	state.mu.Lock()
	defer state.mu.Unlock()

	commandDir := filepath.Join(s.ws.Root, ".putnami", "out", state.project, state.command)
	if !state.materialized {
		if err := store.MaterializeDirSymlink(commandDir); err != nil {
			return err
		}
		if err := os.MkdirAll(commandDir, 0o755); err != nil {
			return err
		}
		state.materialized = true
	}
	return nil
}

func commandOutputKey(job *ScheduledJob) string {
	return job.Project.Path + "\x00" + jobCommandName(job)
}

func (s *Scheduler) stateForCommandOutput(job *ScheduledJob) *commandOutputState {
	key := commandOutputKey(job)
	s.commandOutputsMu.Lock()
	defer s.commandOutputsMu.Unlock()
	if s.commandOutputs == nil {
		s.commandOutputs = make(map[string]*commandOutputState)
	}
	state := s.commandOutputs[key]
	if state == nil {
		state = &commandOutputState{
			project: job.Project.Path,
			command: jobCommandName(job),
		}
		s.commandOutputs[key] = state
	}
	return state
}

// lookupDeclaredEntry is the complete read side for a declared-capture task:
// key, task-owned lookup, remote restore, then claim-or-wait for a genuine
// miss. The three ways an entry can arrive — already local, pulled from the provider,
// published by a sibling while we waited — all reach the workspace through
// restoreDeclaredCacheHit. A run that reads none of the task's files is served
// by a result-only entry instead, local or restored, and writes nothing to the
// workspace (serveStatusOnlyHit).
func (s *Scheduler) lookupDeclaredEntry(
	ctx context.Context,
	job *ScheduledJob,
	hashCopy map[string]string,
	mu *sync.Mutex,
	hashes map[string]string,
) (string, *JobResult, func()) {
	noop := func() {}

	keyStarted := time.Now()
	hash, err := cachekey.ComputeJobCacheHash(s.ws, job, s.commandParams, s.cfg.VersionInfo, s.cache, hashCopy)
	s.cacheStats.RecordLocalKeys(keyStarted, time.Now())
	if err != nil || hash == "" {
		if err != nil && !errors.Is(err, store.ErrGoEmbedInput) {
			if semanticErr := cachekey.ValidateUnkeyedGoEmbedInputs(s.ws, job, s.commandParams); semanticErr != nil {
				err = semanticErr
			}
		}
		if errors.Is(err, store.ErrGoEmbedInput) {
			s.renderer.JobStart(job)
			return "", &JobResult{Status: string(TaskStatusFailed), Error: &JobError{Message: err.Error()}}, noop
		}
		return "", nil, noop
	}

	restoreStarted := time.Now()
	entry, err := s.cache.LookupTaskEntry(hash)
	if err == nil && entry != nil {
		if result := s.restoreDeclaredCacheHit(ctx, job, hash, entry, mu, hashes); result != nil {
			s.cacheStats.RecordLocalRestoreVerify(restoreStarted, time.Now())
			s.cacheStats.RecordLocalHit()
			return hash, result, noop
		}
	}
	// A run that reads none of this task's files may be served the result an
	// earlier result-only restore recorded; a run that reads them never is, and
	// restores the full entry below or executes (remotecache/remote_result_only.go). The
	// result-only path applies only when no full local entry exists: a full
	// entry whose restore failed keeps the full fallback, so a partly restored
	// tree is rewritten by the provider's restore or by the execution.
	resultOnly := err == nil && entry == nil && s.remote != nil && !s.remote.TaskFilesNeeded(job)
	if resultOnly {
		if recorded := s.cache.LookupResultOnlyTaskEntry(hash); recorded != nil {
			if result := s.serveStatusOnlyHit(job, hash, recorded, ReuseLocalCache, mu, hashes); result != nil {
				s.cacheStats.RecordLocalRestoreVerify(restoreStarted, time.Now())
				s.cacheStats.RecordLocalHit()
				return hash, result, noop
			}
		}
	}
	s.cacheStats.RecordLocalRestoreVerify(restoreStarted, time.Now())
	s.cacheStats.RecordLocalMiss()

	if resultOnly {
		if recorded, reuse := s.remote.RestoreTaskResult(ctx, hash, job, s.cache); recorded != nil {
			if result := s.serveStatusOnlyHit(job, hash, recorded, reuse, mu, hashes); result != nil {
				return hash, result, noop
			}
		}
	} else if s.remote != nil {
		if remoteEntry, reuse := s.remote.RestoreTaskEntry(ctx, hash, job, s.cache); remoteEntry != nil {
			// The provider publishes into the local store, so what comes back here
			// is an ordinary task-owned entry: recency is already stamped on its
			// address by the materialize, and the restore below is the same one a
			// local hit takes. Only the reported provenance differs — and the remote
			// step reports it, because it is the step that knows whether the entry
			// was fetched or published by the lease owner while it waited.
			if result := s.restoreDeclaredCacheHit(ctx, job, hash, remoteEntry, mu, hashes); result != nil {
				result.MarkReuse(reuse)
				return hash, result, noop
			}
		}
	}

	// No positive entry exists anywhere for this key. Before paying for a lease
	// and an execution, check whether this exact key already FAILED here: a
	// recorded failure means nothing the key describes has changed since, so
	// running again can only reproduce it (task_failure.go). The negative
	// lookup is last on purpose — a positive entry, local or remote, always
	// wins — and it is local-only by construction: its store address is derived
	// from a different domain than any address the remote path can name.
	if replayed := s.replayFailedEntry(job, hash); replayed != nil {
		return hash, replayed, noop
	}

	result, release := s.coalesceMiss(ctx, job, hash, declaredCoalescer{scheduler: s, job: job, mu: mu, hashes: hashes})
	return hash, result, release
}

// declaredCoalescer routes the shared claim-or-wait loop at the task-owned
// address. Leasing on the derived address (rather than the raw key) keeps a
// task-owned artifact distinct from any other data that shares its cache key.
type declaredCoalescer struct {
	scheduler *Scheduler
	job       *ScheduledJob
	mu        *sync.Mutex
	hashes    map[string]string
}

func (c declaredCoalescer) claim(hash string, estimatedCost time.Duration) (bool, func()) {
	return c.scheduler.cache.TryClaimTaskEntry(hash, estimatedCost)
}

func (c declaredCoalescer) wait(ctx context.Context, hash string, timeout time.Duration) error {
	return c.scheduler.cache.WaitForTaskEntry(ctx, hash, timeout)
}

func (c declaredCoalescer) restore(ctx context.Context, hash string) *JobResult {
	entry, err := c.scheduler.cache.LookupTaskEntry(hash)
	if err != nil || entry == nil {
		return nil
	}
	return c.scheduler.restoreDeclaredCacheHit(ctx, c.job, hash, entry, c.mu, c.hashes)
}
