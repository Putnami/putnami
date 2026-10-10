// Package infraagg aggregates one workload's deployability requirements into
// the artifact a deployer reads: <workload>/.gen/requirements.json.
//
// An earlier migration moved this out of the CLI. Core used to gate the
// emission itself: it watched every build~describe / build~generate completion,
// walked the workload's dependency closure, merged the committed per-project
// manifests, decided what a workload's DEFAULT runtime looked like — and, to do
// that last part, matched project tags and extension names to recognize a
// TypeScript application so it could turn HTTP/2 off for it. That is language
// policy living in the orchestrator, and it is the class of knowledge the extension model
// exists to move.
//
// The split this package draws is the same one that was drawn for machine
// caches:
//
//   - The ORCHESTRATOR owns the graph. Which projects a workload depends on,
//     what a project is classified as, and which build steps must finish before
//     the requirements are current are all core's answers; they arrive here as
//     the job context's project.dependencyClosure and project.type, and as the
//     pipeline edges that schedule the aggregating task.
//
//   - The EXTENSION owns the language's runtime compatibility. Only the
//     TypeScript extension knows Bun does not serve h2c; only a language
//     extension can say what its workloads must run under. It states that as one
//     function (Options.RuntimeCompatibility) instead of core inferring it from
//     tags.
//
//   - This package owns everything neither of them decides: merge order,
//     authored-over-default runtime precedence, the stale-defaults sidecar
//     removal, overrides, validation, and the atomic write. That policy is
//     identical for every language, and three copies of it would drift.
//
// Nothing in Aggregate is fatal. A malformed contribution, a merge conflict, a
// validation finding and a failed write all come back as diagnostics: a build
// that produced binaries must not be failed by a deployability manifest, and a
// finding nobody sees is worse than one attributed to its project. A failed
// write or removal also sets Result.Err, because the files it leaves on disk
// need not describe the inputs, and a caller that caches the outcome must not
// keep them.
//
// Deployment derives the same manifest from the same committed files and
// writes it as the workload's deployment declaration, the canonical bytes a
// release set carries as a member of kind deployment. It never touches the
// defaults sidecar, so it can run beside Aggregate. It writes a declaration
// only for an aggregate without an error finding, and it removes an earlier
// one otherwise, so the file on disk never describes other inputs.
package infraagg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/infra"
	job "go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/robustio"
)

// AggregatedSchemaURL is written into the emitted .gen/requirements.json so
// editors validate the aggregated artifact against the published schema.
const AggregatedSchemaURL = "https://putnami.dev/schemas/putnami-infra.json"

// RuntimeManifestFilename is the workload-only runtime block, sourced from
// <workload>/infra/runtime.json. It is kept separate from the per-project
// requirements manifest because the per-project schema rejects a top-level
// runtime block (it is a workload-root concern).
const RuntimeManifestFilename = "runtime.json"

// AggregatedManifestFile is the project-relative, slash-separated path of a
// workload's aggregated manifest: <project>/.gen/requirements.json.
const AggregatedManifestFile = infra.AggregatedManifestDir + "/" + infra.AggregatedManifestFilename

// RuntimeDefaultsFile is the project-relative, slash-separated path of a
// workload's runtime defaults sidecar: <project>/.gen/infra/runtime.json.
// Aggregate writes it when the workload authors no infra/runtime.json and
// removes it when the workload does. Nothing reads it back: it shows operators
// the values the workload runs under.
const RuntimeDefaultsFile = infra.AggregatedManifestDir + "/" + infra.PerProjectManifestDir + "/" + RuntimeManifestFilename

// projectTypeApplication is the classification of a deployable workload.
// Libraries are consumed, never deployed, so they never receive an aggregated
// manifest. An unclassified project is an application, which is the same
// default the orchestrator applies.
const projectTypeApplication = "application"

// Outcome is what one aggregation did to the workload's manifest.
type Outcome string

const (
	// OutcomeEmitted means the aggregated manifest was written.
	OutcomeEmitted Outcome = "emitted"
	// OutcomeCleared means the workload declares nothing, so any manifest left
	// by a prior build was removed rather than left to mislead a deployer.
	OutcomeCleared Outcome = "cleared"
	// OutcomeSkipped means the project is not a deployable workload.
	OutcomeSkipped Outcome = "skipped"
)

// Options carries the one decision this package leaves to the caller.
type Options struct {
	// RuntimeCompatibility adjusts a workload's runtime block with constraints
	// the LANGUAGE imposes, whatever the source of the block: it is applied to a
	// developer-authored infra/runtime.json and to the synthesized defaults
	// alike, because a constraint that only held for one of the two would make
	// the authored file a way to request an unsupported runtime.
	//
	// Nil means the language imposes none.
	RuntimeCompatibility func(*infra.Runtime)
}

// Result reports what happened, in the shape a task result can carry.
type Result struct {
	// Outcome is emitted, cleared, or skipped.
	Outcome Outcome
	// ManifestPath is the absolute path of the aggregated manifest — the file
	// written, or the one removed. Empty for a skipped project.
	ManifestPath string
	// ManifestFile is ManifestPath relative to the project, slash-separated.
	// It is the same in every checkout of the workspace. Empty for a skipped
	// project.
	ManifestFile string
	// Contributions is the number of per-project manifests that reached the
	// merge.
	Contributions int
	// Diagnostics are the run's findings, already attributed to the project
	// they came from. Never fatal.
	Diagnostics []diag.Diagnostic
	// Err is a write or removal that failed. It can leave an earlier manifest
	// or runtime defaults sidecar in place, so the files on disk need not
	// describe the inputs. Aggregate also reports it in Diagnostics.
	Err error
}

// Data projects the result into a task's structured result data. It names the
// manifest by its project-relative path, so a cache entry that replays the
// data names no directory of the checkout that stored it.
func (r Result) Data() map[string]any {
	return map[string]any{
		"outcome":       string(r.Outcome),
		"manifest":      r.ManifestFile,
		"contributions": r.Contributions,
	}
}

// IsWorkload reports whether a project's resolved type is a deployable
// workload. An empty type defaults to "application", matching the
// orchestrator's own default, so a project that classifies nothing is deployed.
func IsWorkload(projectType string) bool {
	return projectType == "" || projectType == projectTypeApplication
}

// Aggregate emits the workload's aggregated infra manifest from the job
// context.
//
// It walks the context's dependency closure, collects each project's committed
// generator-owned infra/requirements.json, merges them, applies the workload's
// overrides, resolves the workload-only runtime block, and writes the result to
// <workload>/.gen/requirements.json.
//
// Merge order is explicit: committed generated requirements from the workload's
// own project and its dependency graph first, then human runtime intent from
// infra/runtime.json, then the final ephemeral .gen/requirements.json artifact.
func Aggregate(ctx *pctx.Context, opts Options) Result {
	if ctx == nil {
		return Result{Outcome: OutcomeSkipped}
	}
	workloadRoot := projectRoot(ctx.WorkspaceRoot, ctx.Project.Path, ctx.Project.FullPath)
	manifestPath := AggregatedManifestPath(workloadRoot)

	if !IsWorkload(ctx.Project.Type) {
		return Result{Outcome: OutcomeSkipped}
	}

	contributions, diags := collectContributions(ctx)

	// Source the workload-only runtime block up front: a workload may declare
	// only ingress/scaling (no databases/secrets/etc.) and must still emit.
	runtime, runtimeDiags, runtimeErr := sourceRuntime(workloadRoot, opts)
	diags = append(diags, runtimeDiags...)

	if len(contributions) == 0 && runtime == nil {
		// Nothing to declare. Clear any manifest left by a prior build so a
		// deployer can't consume requirements that have since been removed.
		removeErr := removeAggregatedManifest(manifestPath)
		if removeErr != nil {
			diags = append(diags, diag.Errorf(infra.ErrorCodeParseError, "", "%v", removeErr))
		}
		return Result{
			Outcome:      OutcomeCleared,
			ManifestPath: manifestPath,
			ManifestFile: AggregatedManifestFile,
			Diagnostics:  diags,
			Err:          errors.Join(runtimeErr, removeErr),
		}
	}

	merged, assembleDiags := assemble(workloadID(ctx), workloadRoot, contributions, runtime)
	diags = append(diags, assembleDiags...)
	merged.Schema = AggregatedSchemaURL

	var writeErr error
	if err := writeAggregatedManifest(manifestPath, merged); err != nil {
		writeErr = fmt.Errorf("write aggregated infra manifest %s: %w", manifestPath, err)
		diags = append(diags, diag.Errorf(infra.ErrorCodeParseError, "",
			"write aggregated infra manifest for %s: %v", ctx.Project.Name, err))
	}
	return Result{
		Outcome:       OutcomeEmitted,
		ManifestPath:  manifestPath,
		ManifestFile:  AggregatedManifestFile,
		Contributions: len(contributions),
		Diagnostics:   diags,
		Err:           errors.Join(runtimeErr, writeErr),
	}
}

// assemble is the pure half of an aggregation: it merges the contributions,
// applies the workload's overrides, attaches the runtime block, and validates
// the result. It reads the workload's committed overrides file and writes
// nothing, so Aggregate and Deployment derive the same manifest from the same
// committed files. The returned manifest carries no $schema.
func assemble(workload, workloadRoot string, contributions []infra.ProjectContribution, runtime *infra.Runtime) (infra.AggregatedManifest, []diag.Diagnostic) {
	merged, diags := infra.Merge(workload, contributions)

	// Apply workload-level suppression rules so an owner can drop a
	// requirement a library or framework generator declared.
	merged, overrideDiags := applyWorkloadOverrides(workloadRoot, merged)
	diags = append(diags, overrideDiags...)

	if runtime != nil {
		merged = infra.WithRuntime(merged, runtime)
	}

	// Validate the assembled artifact as a safety net (e.g. negative scaling
	// from the runtime file). The caller decides what a finding costs.
	diags = append(diags, infra.ValidateAggregatedManifest(&merged)...)
	return merged, diags
}

// AggregatedManifestPath is the workload's aggregated manifest location.
func AggregatedManifestPath(workloadRoot string) string {
	return filepath.Join(workloadRoot, filepath.FromSlash(AggregatedManifestFile))
}

// RemoveAggregatedManifest deletes a workload's aggregated manifest if one
// exists. Used when a workload no longer declares any requirements, so a stale
// artifact from a prior build cannot mislead deployers. Best-effort: a missing
// file is not an error.
func RemoveAggregatedManifest(workloadRoot string) []diag.Diagnostic {
	if err := removeAggregatedManifest(AggregatedManifestPath(workloadRoot)); err != nil {
		return []diag.Diagnostic{diag.Errorf(infra.ErrorCodeParseError, "", "%v", err)}
	}
	return nil
}

// removeAggregatedManifest deletes the aggregated manifest at path. A missing
// file is not an error.
func removeAggregatedManifest(path string) error {
	if err := robustio.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale aggregated infra manifest %s: %w", path, err)
	}
	return nil
}

// collectContributions reads the committed generator-owned per-project
// requirements manifest for every project in the context's dependency closure.
// A missing file is not an error; a present-but-invalid file yields diagnostics
// attributed to the project and contributes nothing. Ephemeral .gen/infra
// fragments are generator scratch only and are not deployability markers.
//
// A context that carries no closure (an older orchestrator, a direct
// invocation) still aggregates the project's OWN requirements rather than
// nothing: a workload's own declaration is never in doubt, and silently
// emitting an empty manifest would be worse than emitting a narrow one.
func collectContributions(ctx *pctx.Context) ([]infra.ProjectContribution, []diag.Diagnostic) {
	closure := ctx.Project.DependencyClosure
	if len(closure) == 0 {
		closure = []pctx.ProjectRef{{
			ID:       "/" + workloadID(ctx),
			Name:     ctx.Project.Name,
			Path:     ctx.Project.Path,
			FullPath: ctx.Project.FullPath,
		}}
	}

	var contributions []infra.ProjectContribution
	var diags []diag.Diagnostic
	for _, member := range closure {
		root := projectRoot(ctx.WorkspaceRoot, member.Path, member.FullPath)
		if root == "" {
			continue
		}
		c, d := loadContribution(infra.ProjectRequirementsPath(root), projectRefID(member))
		if c != nil {
			contributions = append(contributions, *c)
		}
		diags = append(diags, d...)
	}
	return contributions, diags
}

// workloadID is the name a workload's aggregated manifest and deployment
// declaration give it in their workload member: the project id without its
// leading slash, from distribution.MemberProjectID. A release-set member names
// the same project with the same function, so a deployer that matches the
// declaration to its release-set member compares equal values even when the
// project's name differs from its path.
//
// The id comes from the task's typed identity when the task is project-scoped,
// then from the dependency closure entry at the project's path. A context that
// carries neither (an older orchestrator, a direct invocation) falls back to
// the project path, which equals the id unless a grouping folder sits in it.
// A project at the workspace root has an empty id; it keeps its name, because
// the manifest requires a workload and no release-set member can name it.
func workloadID(ctx *pctx.Context) string {
	id := ""
	if identity := ctx.Identity; identity != nil && identity.Scope == job.TaskScopeProject && identity.Project.ID != "" {
		id = distribution.MemberProjectID(identity.Project.ID)
	} else if member, ok := closureSeed(ctx); ok {
		id = projectRefID(member)
	} else {
		id = distribution.MemberProjectID(filepath.ToSlash(ctx.Project.Path))
	}
	if id == "" {
		return ctx.Project.Name
	}
	return id
}

// closureSeed returns the dependency closure entry of the project itself. The
// closure is in project-id order, so the seed is not always its first entry.
func closureSeed(ctx *pctx.Context) (pctx.ProjectRef, bool) {
	for _, member := range ctx.Project.DependencyClosure {
		if member.ID != "" && filepath.ToSlash(member.Path) == filepath.ToSlash(ctx.Project.Path) {
			return member, true
		}
	}
	return pctx.ProjectRef{}, false
}

// projectRefID is the name a contribution's sources give one closure member:
// the same project-id form as workloadID, or the member's path when the
// orchestrator assigned it no id. A member at the workspace root keeps its
// name, because a source must name a project.
func projectRefID(member pctx.ProjectRef) string {
	id := distribution.MemberProjectID(member.ID)
	if member.ID == "" {
		id = distribution.MemberProjectID(filepath.ToSlash(member.Path))
	}
	if id == "" {
		return member.Name
	}
	return id
}

// loadContribution loads a single per-project manifest from path under the
// generated-requirements contributor identity. An absent file returns
// (nil, nil). A parse or validation failure returns (nil, diagnostics)
// attributed to project.
func loadContribution(path, project string) (*infra.ProjectContribution, []diag.Diagnostic) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	m, diags := infra.LoadGeneratedPerProjectManifest(path)
	if diag.HasErrors(diags) {
		return nil, attributeDiags(project, diags)
	}
	return &infra.ProjectContribution{
		Project:     project,
		Contributor: infra.GeneratedRequirementsContributor,
		Manifest:    *m,
	}, attributeDiags(project, diags)
}

// sourceRuntime resolves the workload-only runtime block for the aggregated
// manifest (see resolveRuntime) and keeps the defaults sidecar in step with it:
//
//  1. <workload>/infra/runtime.json (developer-authored) wins when present; any
//     stale <workload>/.gen/infra/runtime.json is removed so it does not
//     compete with the authored file. A cache hit of the calling task runs
//     none of this and removes nothing.
//  2. Otherwise the synthesized defaults are written to
//     <workload>/.gen/infra/runtime.json, so deployers and operators can see the
//     values the workload will run under. The defaults are re-emitted on every
//     build to track changes in infra.DefaultRuntime() and in the language's
//     compatibility constraints.
//
// A failed sidecar removal or write costs the runtime block, yields that one
// diagnostic, and returns the failure as the error.
func sourceRuntime(workloadRoot string, opts Options) (*infra.Runtime, []diag.Diagnostic, error) {
	runtime, authored, diags := resolveRuntime(workloadRoot, opts)
	defaultsPath := runtimeDefaultsPath(workloadRoot)

	if authored {
		if err := robustio.Remove(defaultsPath); err != nil && !os.IsNotExist(err) {
			err = fmt.Errorf("remove stale runtime defaults %s: %w", defaultsPath, err)
			return nil, []diag.Diagnostic{diag.Errorf(infra.ErrorCodeParseError, "", "%v", err)}, err
		}
		return runtime, diags, nil
	}

	if err := writeRuntimeDefaults(defaultsPath, runtime); err != nil {
		err = fmt.Errorf("write runtime defaults %s: %w", defaultsPath, err)
		return nil, []diag.Diagnostic{diag.Errorf(infra.ErrorCodeParseError, "", "%v", err)}, err
	}
	return runtime, diags, nil
}

// resolveRuntime returns the workload-only runtime block and whether the
// developer authored it, without writing anything:
//
//  1. <workload>/infra/runtime.json (developer-authored) wins when present,
//     then the language's runtime compatibility constraints are applied.
//  2. Otherwise infra.DefaultRuntime(), with the same constraints applied.
//
// A missing developer file is not an error; a malformed one yields diagnostics
// and no block. Scaling-range validation happens later via
// ValidateAggregatedManifest on the assembled artifact.
func resolveRuntime(workloadRoot string, opts Options) (*infra.Runtime, bool, []diag.Diagnostic) {
	developerPath := filepath.Join(workloadRoot, infra.PerProjectManifestDir, RuntimeManifestFilename)
	if _, err := os.Stat(developerPath); err == nil {
		rt, diags := readRuntimeFile(developerPath)
		applyRuntimeCompatibility(rt, opts)
		return rt, true, diags
	}

	defaults := infra.DefaultRuntime()
	applyRuntimeCompatibility(defaults, opts)
	return defaults, false, nil
}

// runtimeDefaultsPath is the workload's framework-generated runtime defaults
// sidecar.
func runtimeDefaultsPath(workloadRoot string) string {
	return filepath.Join(workloadRoot, filepath.FromSlash(RuntimeDefaultsFile))
}

func applyRuntimeCompatibility(rt *infra.Runtime, opts Options) {
	if rt == nil || opts.RuntimeCompatibility == nil {
		return
	}
	opts.RuntimeCompatibility(rt)
}

// readRuntimeFile strict-parses a runtime.json into a Runtime block.
func readRuntimeFile(path string) (*infra.Runtime, []diag.Diagnostic) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(infra.ErrorCodeParseError, "",
			"read workload runtime %s: %v", path, err)}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rt infra.Runtime
	if err := dec.Decode(&rt); err != nil {
		// Route through the protocol's own mapping so a retired cost-policy
		// field (scaling.min and friends) is named as such. The workload
		// loses its whole runtime block when this file cannot be read, so
		// the diagnostic is the only thing standing between the author and
		// a deploy that silently drops their domain and security settings.
		diags := infra.DecodeDiagnostics(err)
		for i := range diags {
			diags[i].Message = fmt.Sprintf("workload runtime %s: %s", path, diags[i].Message)
		}
		return nil, diags
	}
	return &rt, nil
}

// writeRuntimeDefaults emits the workload's framework-generated runtime
// defaults sidecar atomically (temp + rename). The file is overwritten on
// every build so it tracks any change in infra.DefaultRuntime().
func writeRuntimeDefaults(path string, rt *infra.Runtime) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rt, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// applyWorkloadOverrides reads the workload's optional overrides file
// (<workload>/infra/overrides.json) and applies its suppression rules to the
// merged manifest, so an owner can drop requirements that a library or
// framework generator declared. A missing file is a no-op; a malformed one is
// surfaced as a diagnostic and leaves the manifest unchanged.
func applyWorkloadOverrides(workloadRoot string, m infra.AggregatedManifest) (infra.AggregatedManifest, []diag.Diagnostic) {
	path := filepath.Join(workloadRoot, infra.PerProjectManifestDir, infra.OverridesFilename)
	if _, err := os.Stat(path); err != nil {
		return m, nil
	}
	ov, diags := infra.LoadOverrides(path)
	if diag.HasErrors(diags) {
		return m, diags
	}
	out, applyDiags := infra.ApplyOverrides(m, *ov)
	return out, append(diags, applyDiags...)
}

// writeAggregatedManifest writes m to <workload>/.gen/requirements.json
// atomically (write to a temp file then rename) so deployers never observe a
// partially written file. Output is indented and newline-terminated to match
// other .gen/ artifacts.
func writeAggregatedManifest(path string, m infra.AggregatedManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// writeFileAtomic writes data to path through a temp file and a rename, so a
// concurrent reader observes either the previous file or the complete new one.
// The rename goes through robustio because on Windows it fails while a reader
// holds path open.
func writeFileAtomic(path string, data []byte) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	if err := robustio.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// projectRoot resolves a project reference to an absolute directory. The
// context's absolute path is authoritative when present; the workspace-relative
// one is the fallback, so a reference that carries only a path still resolves.
func projectRoot(workspaceRoot, path, fullPath string) string {
	if fullPath != "" {
		return fullPath
	}
	if path == "" {
		return workspaceRoot
	}
	return filepath.Join(workspaceRoot, path)
}

// attributeDiags prefixes each diagnostic message with the contributing
// project so a workload's build can trace a finding back to its origin.
func attributeDiags(project string, diags []diag.Diagnostic) []diag.Diagnostic {
	for i := range diags {
		diags[i].Message = fmt.Sprintf("project %s: %s", project, diags[i].Message)
	}
	return diags
}
