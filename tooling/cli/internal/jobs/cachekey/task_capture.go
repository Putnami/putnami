package cachekey

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

func CleanProjectRel(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return clean, true
}

// taskMutatesSources reports whether the job's v3 declaration states that it
// rewrites the source tree it reads.
func taskMutatesSources(job *jobmodel.ScheduledJob) bool {
	declaration := jobmodel.TaskDeclarationOf(job)
	return declaration != nil && declaration.MutatesSources
}

// EmptiedOutputDeclaration is one declared output the scheduler empties before
// its task executes, with its path in canonical form.
type EmptiedOutputDeclaration struct {
	ID     string
	Rel    string
	Output extension.DeclaredOutput
}

// EmptiedOutputDeclarations returns, in id order, the declared outputs of job
// that the scheduler empties before the job executes: every durable directory
// output whose path the declaration states, when it is required, lives under
// the command-output root, or declares emptied.
//
// A required output is recorded present in every entry, so a restore prunes
// exactly what the emptying removes. An optionalEmpty output under the
// command-output root is emptied too. The engine owns that directory, nothing
// there is committed, and the plan gives each output one owner
// (validatePlanContract), so the emptying removes only what this task wrote
// on an earlier run. An entry can record it empty, and a restore of that
// record empties it the same way (materializeDeclaredOutputs).
//
// An optionalEmpty output under the project or the workspace root is emptied
// when it declares emptied (ADR 0067): its task states that only it writes the
// path, so the output holds what the task wrote and the subpaths it cedes or
// preserves. Without the flag it is left out. It can hold a file no task
// writes, such as a committed configuration the task reads when it writes
// nothing there, and a restore of an entry that records it empty leaves it as
// it stands (store.TaskOutputEmpty).
//
// A pathFrom output is left out because its path is known only after the run,
// and an invocation-scoped one because it lives in a tree created for the run.
// An output whose path does not normalize is left out too: manifest validation
// reports it, and no capture is taken from a path nobody can name. A literal
// path never names its root (extension.NormalizeOutputPath), so the project
// root, the workspace root and a command-output directory are never emptied.
//
// It reads the plan node's declaration alone, because the cache key names the
// same set (EmptiedOutputIDs): what an execution empties, what a restore
// empties, and the address an entry is stored at are decided by one predicate.
func EmptiedOutputDeclarations(job *jobmodel.ScheduledJob) []EmptiedOutputDeclaration {
	declaration := jobmodel.TaskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	ids := make([]string, 0, len(declaration.Outputs))
	for id := range declaration.Outputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var emptied []EmptiedOutputDeclaration
	for _, id := range ids {
		output := declaration.Outputs[id]
		if output.Kind != extension.OutputKindDirectory || output.PathFrom != "" || output.IsInvocationScoped() {
			continue
		}
		switch output.EffectiveRoot() {
		case extension.OutputRootCommandOutput:
		case extension.OutputRootProject, extension.OutputRootWorkspace:
			if output.OptionalEmpty && !output.Emptied {
				continue
			}
		default:
			continue
		}
		rel, err := extension.NormalizeOutputPath(output.Path)
		if err != nil {
			continue
		}
		emptied = append(emptied, EmptiedOutputDeclaration{ID: id, Rel: rel, Output: output})
	}
	return emptied
}

// EmptiedOutputIDs returns the ids of the outputs the scheduler empties before
// job executes, which is the value the cache key carries for them
// (store.CacheKey.EmptiedOutputs). It is a variable only so a test can key a
// job the way an engine that emptied nothing did.
var EmptiedOutputIDs = func(job *jobmodel.ScheduledJob) []string {
	emptied := EmptiedOutputDeclarations(job)
	if len(emptied) == 0 {
		return nil
	}
	ids := make([]string, 0, len(emptied))
	for _, output := range emptied {
		ids = append(ids, output.ID)
	}
	return ids
}

// DeclaredOutputPlan is one declared output resolved for the run being
// captured: what the entry must record, and where its bytes actually live.
type DeclaredOutputPlan struct {
	Spec store.DeclaredEntryOutput
	// Src is the absolute REAL location of the output — the path the task wrote
	// and the path a restore puts it back at. It is never recorded in the entry:
	// the store is machine-global, so entries carry root-relative paths only.
	Src string
	// Excludes are the subpaths the declaration cedes, relative to the output's
	// own path (Src), in slash form. Staging skips them, so the entry never
	// holds bytes this task does not own.
	//
	// They are deliberately NOT recorded in the entry. The excluded bytes are
	// never staged, so a restore has nothing to put back for them, and cache
	// key v5 folds the task-contract digest: a declaration that gains or loses
	// a carve-out relocates the key rather than reusing an entry captured under
	// the other shape.
	Excludes []string
}

// ResolveDeclaredOutputs turns the job's declaration into the concrete set the
// ingest must satisfy, in canonical id order.
//
// data is the executed task's result data, which supplies the value of any
// pathFrom port. An OPTIONAL output whose port reports nothing is dropped from
// the set entirely — the task legitimately produced no such artifact, and an
// output whose path is unknown cannot be recorded. A REQUIRED one is an error:
// publishing the rest would produce an entry that restores less than the
// declaration promises.
func ResolveDeclaredOutputs(
	ws WorkspaceRoots,
	job *jobmodel.ScheduledJob,
	data map[string]any,
) ([]DeclaredOutputPlan, error) {
	declaration := jobmodel.TaskDeclarationOf(job)
	if declaration == nil {
		return nil, fmt.Errorf("task has no v3 declaration")
	}

	ids := make([]string, 0, len(declaration.Outputs))
	for id := range declaration.Outputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	plans := make([]DeclaredOutputPlan, 0, len(ids))
	for _, id := range ids {
		output := declaration.Outputs[id]
		root := output.EffectiveRoot()
		base, ok := ws.OutputRoot(job, root)
		if !ok {
			return nil, fmt.Errorf("declared output %q has unknown root %q", id, root)
		}

		rel, ok, err := declaredOutputPath(output, data)
		if err != nil {
			return nil, fmt.Errorf("declared output %q: %w", id, err)
		}
		if !ok {
			// An optional output whose port reported no path: nothing to record.
			continue
		}
		plans = append(plans, DeclaredOutputPlan{
			Spec: store.DeclaredEntryOutput{
				ID:       id,
				Kind:     output.Kind,
				Root:     root,
				Path:     rel,
				Optional: output.OptionalEmpty,
			},
			Src: filepath.Join(base, filepath.FromSlash(rel)),
			// A carve-out on a pathFrom output is undecidable from the manifest
			// and validation rejects it. Ignoring it here rather than resolving
			// it against the runtime-reported path keeps capture in exact
			// agreement with the ownership check, which drops it too
			// (DeclaredOutput.Ref): one of them honoring a carve-out the other
			// ignores is how a subtree ends up owned by one task and captured
			// by neither.
			Excludes: OutputSkippedSubpaths(rel, output),
		})
	}
	return plans, nil
}

// declaredExcludesOf returns the carve-outs a declaration states, and nothing
// for an output whose path the task reports at runtime.
func declaredExcludesOf(output extension.DeclaredOutput) []string {
	if output.PathFrom != "" {
		return nil
	}
	return output.Excludes
}

// OutputSkippedSubpaths returns every output-relative subpath the engine
// leaves alone for one declared output at rel: the subpaths it CEDES to another
// task (protocol ADR 0003) and the ones it PRESERVES for the workspace (protocol
// ADR 0005). Capture skips them, a restore keeps whatever the destination
// holds at them, and a drift comparison judges neither side of them. Every one
// of those three paths resolves the set here, so they cannot disagree.
func OutputSkippedSubpaths(rel string, output extension.DeclaredOutput) []string {
	skipped := outputRelativeExcludes(rel, declaredExcludesOf(output))
	preserved := extension.DecidablePreserves(output)
	if len(preserved) == 0 {
		return skipped
	}
	merged := make([]string, 0, len(skipped)+len(preserved))
	merged = append(merged, skipped...)
	merged = append(merged, preserved...)
	sort.Strings(merged)
	return slices.Compact(merged)
}

// outputRelativeExcludes rebases a declaration's ceded subpaths onto the output
// they sit inside, which is the form the staging walk compares against.
//
// Only DECIDABLE carve-outs survive (extension.DecidableExcludes): an entry
// that does not normalize, or does not sit strictly inside the output's path,
// is dropped rather than guessed at. Dropping it keeps the bytes in the capture,
// which is the safe direction for a defect manifest validation already reports —
// the alternative would silently stop capturing a subtree because a carve-out
// was misspelled.
func outputRelativeExcludes(rel string, excludes []string) []string {
	decidable := extension.DecidableExcludes(rel, excludes)
	if len(decidable) == 0 {
		return nil
	}
	relative := make([]string, 0, len(decidable))
	for _, exclude := range decidable {
		// DecidableExcludes guarantees each entry is strictly inside rel, so the
		// remainder after the separator is non-empty. The prefix is dropped by
		// LENGTH rather than by string match because the containment test that
		// admitted it is case-insensitive, exactly like every other ownership
		// comparison.
		relative = append(relative, exclude[len(rel)+1:])
	}
	return relative
}

// declaredOutputPath resolves one declaration to its root-relative path.
// Returns ok=false only for an optional pathFrom output whose port reported
// nothing usable.
func declaredOutputPath(output extension.DeclaredOutput, data map[string]any) (string, bool, error) {
	if output.PathFrom == "" {
		normalized, err := extension.NormalizeOutputPath(output.Path)
		if err != nil {
			return "", false, fmt.Errorf("invalid path %q: %w", output.Path, err)
		}
		return normalized, true, nil
	}

	raw, ok := declaredPortPathValue(data, output.PathFrom)
	if !ok {
		if output.OptionalEmpty {
			return "", false, nil
		}
		return "", false, fmt.Errorf("output port %q reported no path", output.PathFrom)
	}
	normalized, err := extension.NormalizeOutputPath(raw)
	if err != nil {
		return "", false, fmt.Errorf("output port %q reported invalid path %q: %w", output.PathFrom, raw, err)
	}
	return normalized, true, nil
}

// declaredPortPathValue reads a task output port's reported path from the
// executed result data. Both wire shapes the extensions use are accepted: a
// single string (typescript's clientOutput) and a list (go's clientOutputs), of
// which the first usable entry wins, keeping the recorded path deterministic
// when a task reports more than one client directory.
func declaredPortPathValue(data map[string]any, port string) (string, bool) {
	if data == nil || port == "" {
		return "", false
	}
	switch raw := data[port].(type) {
	case string:
		return cleanProjectRelSlash(raw)
	case []string:
		for _, value := range raw {
			if rel, ok := cleanProjectRelSlash(value); ok {
				return rel, true
			}
		}
	case []any:
		for _, value := range raw {
			text, ok := value.(string)
			if !ok {
				continue
			}
			if rel, ok := cleanProjectRelSlash(text); ok {
				return rel, true
			}
		}
	}
	return "", false
}

// cleanProjectRelSlash is CleanProjectRel in slash form, which is what a
// declared output path is.
func cleanProjectRelSlash(rel string) (string, bool) {
	clean, ok := CleanProjectRel(rel)
	if !ok {
		return "", false
	}
	return filepath.ToSlash(clean), true
}

// WorkspaceRoots resolves a symbolic output root to the absolute directory the
// job writes it under. It mirrors declaredOutputBase (plan_contract.go) with
// filesystem paths instead of the slash form the ownership comparison uses.
type WorkspaceRoots struct{ Root string }

func (w WorkspaceRoots) OutputRoot(job *jobmodel.ScheduledJob, root string) (string, bool) {
	switch root {
	case extension.OutputRootWorkspace:
		return w.Root, true
	case extension.OutputRootProject:
		return filepath.Join(w.Root, ProjectRelPath(job)), true
	case extension.OutputRootCommandOutput:
		return filepath.Join(w.Root, ".putnami", "out", ProjectRelPath(job), jobmodel.JobCommandName(job)), true
	default:
		return "", false
	}
}
