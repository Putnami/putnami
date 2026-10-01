// Declared-output drift: protocol/extension ADR 0004.
//
// A declared output carrying `drift` is bytes the repository COMMITS — a
// generated client, a schema sidecar — and the question it asks is whether the
// committed bytes still equal what the current inputs generate. Only the
// writer can answer that, and only at the moment it writes: anything that runs
// later in the session reads a tree the generator, or a cache restore of its
// entry, has already rewritten. So the engine takes the reference IMMEDIATELY
// BEFORE the task writes, on both paths that write:
//
//   - EXECUTED: prepareTask snapshots the output before the subprocess starts (and
//     before the preBuild hook, which is part of the run from the worktree's
//     point of view); closeTask compares after finalizeExecutedJob.
//   - RESTORED: restoreDeclaredCacheHit snapshots the recorded destination
//     before materializeDeclaredOutputs swaps the staged tree in, and compares
//     after it.
//
// Both compare the same two things — the digest of every file at the output
// path before, and after — so a cold run and a warm run reach one verdict. That
// is what lets a fresh CI checkout of stale committed bytes fail from the
// restore path (the entry was published by the run that regenerated them).
//
// # The reference for a pathFrom output
//
// A literal path names its own snapshot. A pathFrom output's path is unknown
// until the task reports it, so its reference is a digest of the whole root the
// output resolves under, taken before the run, and the resolved path is looked
// up in it afterwards. Validation admits the policy on a pathFrom output under
// the PROJECT root only (protocols/extension), so the root walked here is
// always one project tree. Dot-directories and the package's project-walk
// exclusions are skipped on BOTH sides of the comparison, so the two walks see
// the same set.
//
// # What never happens here
//
// The reference and the verdict are facts about the worktree the task ran in,
// not about its inputs: neither is a cache-key input (TestDriftReferenceIs
// NeverACacheKeyInput), the diagnostic is appended AFTER the entry is published
// so it never travels in an entry, and a drift failure is never recorded as a
// replayable failure (recordableFailure). The task's SUCCESSFUL entry is still
// published under `fail`: the next run hits it and judges the — now
// regenerated — worktree again.
package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// driftReportedPaths bounds how many differing paths one diagnostic lists. The
// counts are always complete; the list is what a terminal reader can use.
const driftReportedPaths = 20

// driftTree is the digest of every regular file under one root, keyed by the
// slash-form path relative to that root. A root that is itself a regular file
// digests under the key "". A symlink digests as its target string, so a
// relinked output is a change without following the link out of the tree.
type driftTree map[string]string

// driftReference is the pre-write snapshot for one task, keyed by declared
// output id. Each entry records the root it was walked from, so a pathFrom
// output resolved afterwards can be located inside it.
type driftReference struct {
	entries map[string]driftReferenceEntry
}

type driftReferenceEntry struct {
	output extension.DeclaredOutput
	// base is the absolute directory the snapshot was walked from: the output
	// path itself for a literal output, the project root for a pathFrom one.
	base string
	// baseIsRoot reports that base is the output's ROOT (pathFrom): the resolved
	// output path is then a subpath of base, looked up by prefix.
	baseIsRoot bool
	tree       driftTree
	err        error
}

// policedOutputs returns the job's declared outputs carrying a drift policy, in
// output-id order.
func policedOutputs(job *ScheduledJob) []string {
	declaration := taskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	var ids []string
	for id, output := range declaration.Outputs {
		if output.Drift != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// snapshotExecutedDrift takes the reference for a task about to execute, or
// returns nil when the task polices nothing. The root is resolved exactly as
// declared capture resolves it (workspaceRoots.outputRoot), so the snapshot
// and the capture agree about where the output lives.
func (s *Scheduler) snapshotExecutedDrift(job *ScheduledJob) *driftReference {
	ids := policedOutputs(job)
	if len(ids) == 0 {
		return nil
	}
	declaration := taskDeclarationOf(job)
	roots := s.outputRoots()
	reference := &driftReference{entries: make(map[string]driftReferenceEntry, len(ids))}
	for _, id := range ids {
		output := declaration.Outputs[id]
		base, ok := roots.outputRoot(job, output.EffectiveRoot())
		if !ok {
			reference.entries[id] = driftReferenceEntry{output: output, err: fmt.Errorf("unknown root %q", output.EffectiveRoot())}
			continue
		}
		entry := driftReferenceEntry{output: output, base: base}
		if output.PathFrom != "" {
			// The path is unknown until the task reports it: digest the whole root.
			entry.baseIsRoot = true
			entry.tree, entry.err = walkDriftTree(base, nil)
		} else {
			rel, err := extension.NormalizeOutputPath(output.Path)
			if err != nil {
				entry.err = err
			} else {
				entry.base = filepath.Join(base, filepath.FromSlash(rel))
				entry.tree, entry.err = walkDriftTree(entry.base, outputSkippedSubpaths(rel, output))
			}
		}
		reference.entries[id] = entry
	}
	return reference
}

// judgeExecutedDrift compares the outputs an executed task produced with the
// reference openTask took, and applies the declared policy to the result. It
// runs AFTER finalizeExecutedJob published the entry and recorded the outcome,
// so nothing it adds reaches the cache.
func (s *Scheduler) judgeExecutedDrift(job *ScheduledJob, result *JobResult, reference *driftReference) *JobResult {
	if reference == nil || result == nil || result.Status != string(TaskStatusSuccess) {
		return result
	}
	plans, err := resolveDeclaredOutputs(s.outputRoots(), job, result.Data)
	if err != nil {
		slog.Warn("drift not judged: cannot resolve declared outputs",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return result
	}
	appendedFrom := len(result.Events)
	for _, plan := range plans {
		entry, ok := reference.entries[plan.spec.ID]
		if !ok {
			continue
		}
		before, ok := entry.referenceFor(plan.spec.Path, plan.excludes)
		if !ok {
			slog.Warn("drift not judged: no reference for the resolved output path",
				"project", job.Project.Name, "job", job.JobDef.Name, "output", plan.spec.ID, "path", plan.spec.Path, "error", entry.err)
			continue
		}
		after, err := walkDriftTree(plan.src, plan.excludes)
		if err != nil {
			slog.Warn("drift not judged: cannot read the produced output",
				"project", job.Project.Name, "job", job.JobDef.Name, "output", plan.spec.ID, "error", err)
			continue
		}
		result = s.applyDriftVerdict(job, result, entry.output, plan.spec.ID, plan.spec.Root, plan.spec.Path, compareDriftTrees(before, after))
	}
	// A WARN verdict leaves the task successful, and the renderers print a
	// successful task's diagnostics only as they stream; these were appended
	// after the stream closed, so hand them over now. A failed task's are
	// rendered with its failure details, and a restored task's are replayed
	// with its entry's events, so neither is repeated here.
	if result.Status == string(TaskStatusSuccess) && s.renderer != nil {
		for _, event := range result.Events[appendedFrom:] {
			s.renderer.JobEvent(job, event)
		}
	}
	return result
}

// snapshotRestoredDrift takes the reference for a cache hit about to be
// materialized: the destination of every recorded output whose current
// declaration polices drift, before the swap. The recorded path is known, so
// no root-wide walk is needed on this path.
func (s *Scheduler) snapshotRestoredDrift(job *ScheduledJob, entry *store.TaskEntry) *driftReference {
	ids := policedOutputs(job)
	if len(ids) == 0 || entry == nil {
		return nil
	}
	declaration := taskDeclarationOf(job)
	roots := s.outputRoots()
	reference := &driftReference{entries: make(map[string]driftReferenceEntry, len(ids))}
	for _, output := range entry.Outputs {
		declared, ok := declaration.Outputs[output.ID]
		if !ok || declared.Drift == "" {
			continue
		}
		base, ok := roots.outputRoot(job, output.Root)
		if !ok {
			continue
		}
		item := driftReferenceEntry{output: declared, base: filepath.Join(base, filepath.FromSlash(output.Path))}
		if declared.PathFrom != "" && underSkippedSegment(output.Path) {
			// The executed path cannot reference a pathFrom output that resolves
			// under a directory the root walk skips (referenceFor); declining it
			// here too keeps the two paths at one verdict for one declaration.
			item.err = fmt.Errorf("output path %q lies under a directory the reference walk skips", output.Path)
			slog.Warn("drift not judged: policed output resolves under a skipped directory",
				"project", job.Project.Name, "job", job.JobDef.Name, "output", output.ID, "path", output.Path)
		} else {
			item.tree, item.err = walkDriftTree(item.base, cededSubpathsOf(job, output))
		}
		reference.entries[output.ID] = item
	}
	return reference
}

// judgeRestoredDrift compares the materialized outputs with the reference
// snapshotRestoredDrift took.
//
// An output the entry recorded EMPTY is judged too, against an EMPTY tree: the
// producing run wrote nothing at that path, so committed bytes found there are
// stale whatever the restore did with them. The restore itself leaves the
// destination alone (MaterializeTaskOutput's binding invariant for a recorded
// empty output), so on this path the remedy is to delete, not to commit — and
// the message says so. Without this arm a warm hit passed a stale committed
// client that the same generator, executed cold, removes and fails on.
func (s *Scheduler) judgeRestoredDrift(job *ScheduledJob, entry *store.TaskEntry, result *JobResult, reference *driftReference) *JobResult {
	if reference == nil || result == nil || entry == nil {
		return result
	}
	for _, output := range entry.Outputs {
		item, ok := reference.entries[output.ID]
		if !ok || item.err != nil {
			continue
		}
		after := driftTree{}
		if output.Present() {
			walked, err := walkDriftTree(item.base, cededSubpathsOf(job, output))
			if err != nil {
				slog.Warn("drift not judged: cannot read the restored output",
					"project", job.Project.Name, "job", job.JobDef.Name, "output", output.ID, "error", err)
				continue
			}
			after = walked
		}
		result = s.applyDriftVerdict(job, result, item.output, output.ID, output.Root, output.Path, compareDriftTrees(item.tree, after))
	}
	return result
}

// referenceFor returns the pre-write tree of the output at rel (root-relative,
// slash form). For a literal output the snapshot IS that tree; for a pathFrom
// output it is the subtree of the root snapshot at rel, which exists only when
// no segment of rel is one the walk skips. The root walk could not know the
// output's skipped subpaths (outputSkippedSubpaths) before the path was
// reported, so they are dropped from the subtree here, exactly as the
// after-walk skips them: a preserved project document is judged on neither
// side.
func (e driftReferenceEntry) referenceFor(rel string, skipped []string) (driftTree, bool) {
	if e.err != nil {
		return nil, false
	}
	if !e.baseIsRoot {
		return e.tree, true
	}
	if underSkippedSegment(rel) {
		return nil, false
	}
	subtree := driftTree{}
	prefix := rel + "/"
	for walked, digest := range e.tree {
		switch {
		case walked == rel:
			// The resolved output is a single file at the root level.
			subtree[""] = digest
		case strings.HasPrefix(walked, prefix):
			if inner := walked[len(prefix):]; !isExcludedOutputPath(skipped, inner) {
				subtree[inner] = digest
			}
		}
	}
	return subtree, true
}

// underSkippedSegment reports whether a root-relative path has a segment the
// reference walk refuses to descend into, which is the one shape of pathFrom
// output the mechanism cannot judge: the root snapshot never saw it. Both write
// paths decline such an output the same way (a Warn log, no verdict), so a
// warm and a cold run still agree.
func underSkippedSegment(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if skipDriftDir(segment) {
			return true
		}
	}
	return false
}

// skipDriftDir is the ONE walk rule both sides of a comparison apply to a
// directory strictly below the walk root: repository and tool state
// (dot-directories) and the content-probe exclusions every project walk in
// this package refuses to descend into (projectWalkExcludedDirs: dependency
// installs, vendored trees, build output) are never committed generated
// output, and a generated client that contains one must not report it.
func skipDriftDir(name string) bool {
	return strings.HasPrefix(name, ".") || projectWalkExcludedDirs[name]
}

// walkDriftTree digests every regular file under root. A missing root is an
// empty tree, not an error: an output that did not exist before the run, or
// that a run removed, compares as such. excludes are output-relative ceded
// subpaths in slash form, skipped like the capture skips them.
func walkDriftTree(root string, excludes []string) (driftTree, error) {
	tree := driftTree{}
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return tree, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		digest, err := digestDriftEntry(root, info)
		if err != nil {
			return nil, err
		}
		tree[""] = digest
		return tree, nil
	}
	err = filepath.WalkDir(root, func(walked string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if walked == root {
			return nil
		}
		rel, err := filepath.Rel(root, walked)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if skipDriftDir(entry.Name()) || isExcludedOutputPath(excludes, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if isExcludedOutputPath(excludes, rel) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		digest, err := digestDriftEntry(walked, info)
		if err != nil {
			return err
		}
		if digest != "" {
			tree[rel] = digest
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tree, nil
}

// digestDriftEntry digests one non-directory entry: a regular file by content,
// a symlink by target. Anything else (a socket, a device) has no bytes to
// commit and digests to "" so the walk drops it.
func digestDriftEntry(path string, info fs.FileInfo) (string, error) {
	switch {
	case info.Mode().IsRegular():
		file, err := os.Open(path) //nolint:gosec // a declared output the task owns
		if err != nil {
			return "", err
		}
		defer file.Close()
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			return "", err
		}
		return hex.EncodeToString(hash.Sum(nil)), nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return "symlink:" + target, nil
	default:
		return "", nil
	}
}

// driftDiff is the difference between two trees, each list sorted.
type driftDiff struct {
	added, removed, changed []string
}

func (d driftDiff) empty() bool {
	return len(d.added) == 0 && len(d.removed) == 0 && len(d.changed) == 0
}

func compareDriftTrees(before, after driftTree) driftDiff {
	var diff driftDiff
	for rel, digest := range after {
		previous, existed := before[rel]
		switch {
		case !existed:
			diff.added = append(diff.added, rel)
		case previous != digest:
			diff.changed = append(diff.changed, rel)
		}
	}
	for rel := range before {
		if _, exists := after[rel]; !exists {
			diff.removed = append(diff.removed, rel)
		}
	}
	sort.Strings(diff.added)
	sort.Strings(diff.removed)
	sort.Strings(diff.changed)
	return diff
}

// applyDriftVerdict turns one output's diff into the declared consequence: no
// diff, nothing; `warn`, one warning diagnostic; `fail`, one error diagnostic
// and a failed result whose error carries the same code.
func (s *Scheduler) applyDriftVerdict(
	job *ScheduledJob,
	result *JobResult,
	output extension.DeclaredOutput,
	id, root, rel string,
	diff driftDiff,
) *JobResult {
	if diff.empty() {
		return result
	}
	location := driftOutputLocation(job, root, rel)
	message := driftMessage(id, location, diff)
	severity := runtimeproto.SeverityWarning
	if output.Drift == extension.OutputDriftFail {
		severity = runtimeproto.SeverityError
	}
	result.Events = append(result.Events, driftDiagnosticEvent(severity, message, location, id, diff))
	if result.Canonical != nil {
		result.Canonical.Diagnostics = append(result.Canonical.Diagnostics, TaskDiagnostic{
			Severity: string(severity), Code: extension.OutputDriftDiagnosticCode, Message: message, File: location,
		})
	}
	if output.Drift != extension.OutputDriftFail {
		return result
	}
	result.Status = string(TaskStatusFailed)
	result.SourceMutated = false
	result.Error = &JobError{Code: extension.OutputDriftDiagnosticCode, Message: message}
	if result.Canonical != nil {
		result.Canonical.Status = TaskStatusFailed
		result.Canonical.Error = result.Error
	}
	if s.cfg.Debug {
		slog.Info("declared output drift failed the task", "job", job.Key(), "output", id, "path", location)
	}
	return result
}

// driftOutputLocation is the workspace-relative path of the output in slash
// form — the `file` a diagnostic renders and a reader can open.
func driftOutputLocation(job *ScheduledJob, root, rel string) string {
	switch root {
	case extension.OutputRootProject:
		return path.Join(filepath.ToSlash(projectRelPath(job)), rel)
	default:
		return rel
	}
}

// driftMessage is the one human line: what differs, by how much, and the
// remedy. The path list is bounded; the counts are not.
func driftMessage(id, location string, diff driftDiff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "generated output %q at %s differs from the bytes present before the task ran: %d added, %d removed, %d changed",
		id, location, len(diff.added), len(diff.removed), len(diff.changed))
	listed := 0
	for _, group := range []struct {
		label string
		paths []string
	}{{"added", diff.added}, {"removed", diff.removed}, {"changed", diff.changed}} {
		for _, rel := range group.paths {
			if listed == driftReportedPaths {
				break
			}
			if rel == "" {
				rel = "."
			}
			fmt.Fprintf(&b, "; %s %s", group.label, rel)
			listed++
		}
	}
	if total := len(diff.added) + len(diff.removed) + len(diff.changed); total > listed {
		fmt.Fprintf(&b, "; and %d more", total-listed)
	}
	if len(diff.added) == 0 && len(diff.changed) == 0 {
		// Nothing was written and everything present before is gone from the
		// task's view: the generator produces nothing here now. On the executed
		// path the files are already removed; on a restore of an empty entry
		// they are still on disk, so the remedy names both.
		b.WriteString(" — the task produces nothing at this path now; delete the stale files if they remain, and commit")
		return b.String()
	}
	b.WriteString(" — the worktree now holds the regenerated files; commit them")
	return b.String()
}

// driftDiagnosticEvent projects the verdict onto the runtime event stream in
// the same shape a subprocess diagnostic has, so every consumer — the terminal,
// --output=json, the session record — reads it through the ordinary path. The
// full lists travel in Data for machine readers; the message is bounded.
func driftDiagnosticEvent(severity runtimeproto.DiagnosticSeverity, message, location, id string, diff driftDiff) RawJobEvent {
	data := map[string]any{
		"severity": string(severity),
		"message":  message,
		"code":     extension.OutputDriftDiagnosticCode,
		"file":     location,
		"output":   id,
		"added":    driftPathList(diff.added),
		"removed":  driftPathList(diff.removed),
		"changed":  driftPathList(diff.changed),
	}
	return RawJobEvent{
		Version: runtimeproto.MaxKnownProtocolVersion,
		Type:    EventTypeDiagnostic,
		Message: message,
		Data:    data,
	}
}

// driftPathList is the JSON-friendly form of a path list: a decoded event
// stream holds []any, and an empty list is an explicit empty array rather than
// an absent key, so the three lists are always all present.
func driftPathList(paths []string) []any {
	list := make([]any, 0, len(paths))
	for _, rel := range paths {
		list = append(list, rel)
	}
	return list
}
