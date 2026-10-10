package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// HostPlatform is the GOOS/GOARCH of the machine running the CLI, in the same
// `os/arch` spelling extensions use for platform requests.
func HostPlatform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// taskRuntimeIdentity resolves the ambient runtime facts a task DECLARED as
// cache-key inputs into `name=value` pairs.
//
// Only declared names are resolved, and only names the CLI knows how to answer:
// an unrecognized runtime input contributes nothing rather than an empty value,
// because a placeholder would silently claim the key covers something it does
// not. `extensionVersion` resolves to nothing on purpose — every key already
// names the extension's implementation, by its version or by the digest that
// replaces it, and answering the version here would move, at every build
// stamp, the keys that digest keeps stable. `releaseBaseline` reads git and can
// fail, so releaseBaselineIdentity resolves it beside this function.
//
// The result feeds CacheKey.RuntimeIdentity, which is hashed only when
// non-empty, so a task that declares no runtime input keeps its exact key.
func taskRuntimeIdentity(job *ScheduledJob) []string {
	if job == nil || job.JobDef == nil ||
		job.JobDef.TaskCachePolicy == nil || job.JobDef.TaskCachePolicy.Key == nil {
		return nil
	}
	var identity []string
	for _, name := range job.JobDef.TaskCachePolicy.Key.Runtime {
		if name == extproto.RuntimeInputHostPlatform {
			identity = append(identity, name+"="+HostPlatform())
		}
	}
	return identity
}

// CacheBypass is the run's cache-REUSE policy: which planned tasks are refused
// the cache this time. It answers one question — "may this job be looked up?" —
// and it is the only thing --no-cache and --no-cache-projects contribute to
// execution.
//
// It is execution policy, not identity. Nothing here reaches a cache key, a run
// marker, or the task params a job's identity is computed from: a bypassed job
// keeps the exact key it would have had, it is simply never looked up, never
// served, and never published under it. That is the rule --max-parallel,
// --retry-failed and the resource budgets already follow.
//
// The zero value bypasses nothing. A project-scoped bypass is only correct once
// it has been RESOLVED OVER THE PLAN, which is why NewCacheBypass is the only
// way to build one — see its comment for the dependent closure and why an
// unresolved project set would be a cache-poisoning hole.
type CacheBypass struct {
	// All is --no-cache: every task of the run refuses the cache, so no key is
	// even computed and the remote cache is never consulted.
	All bool
	// jobs is the resolved closure of --no-cache-projects, by job key.
	jobs map[string]bool
}

// NewCacheBypass resolves the run's cache-reuse policy over the plan.
//
// projects is the id set --no-cache-projects named. The resolved bypass covers
// their jobs AND every planned job that transitively depends on one, and the
// closure is the correctness half of the flag, not a nicety: a bypassed job
// publishes no key, so a dependent left cacheable would compute its own key
// WITHOUT that upstream hash and could then be served an entry produced against
// different upstream content. Extending the bypass downstream keeps the rule
// the cache already relies on — a task is served only when every input its key
// summarizes is accounted for.
//
// The closure walks cache-key edges (cacheKeyDependencies, i.e. DependsOn plus
// an invocation producer's own dependencies), not write-serialization edges:
// SerializeAfter orders execution and never contributes to a key, so a job that
// merely waits for a bypassed one keeps its cache.
//
// all short-circuits everything: --no-cache already refuses every job, so the
// closure has nothing to add.
func NewCacheBypass(planned []*ScheduledJob, all bool, projects map[string]bool) CacheBypass {
	if all {
		return CacheBypass{All: true}
	}
	if len(projects) == 0 {
		return CacheBypass{}
	}
	// Dependents adjacency over cache-key edges, plus the seed set.
	dependents := make(map[string][]string, len(planned))
	excluded := make(map[string]bool)
	queue := make([]string, 0, len(planned))
	for _, job := range planned {
		if job == nil {
			continue
		}
		for _, dep := range cacheKeyDependencies(job) {
			dependents[dep] = append(dependents[dep], job.Key())
		}
		if job.Project != nil && projects[job.Project.ID] && !excluded[job.Key()] {
			excluded[job.Key()] = true
			queue = append(queue, job.Key())
		}
	}
	for len(queue) > 0 {
		key := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, dependent := range dependents[key] {
			if excluded[dependent] {
				continue
			}
			excluded[dependent] = true
			queue = append(queue, dependent)
		}
	}
	return CacheBypass{jobs: excluded}
}

// Excludes reports whether this job is refused the cache for this run.
func (b CacheBypass) Excludes(job *ScheduledJob) bool {
	if b.All {
		return true
	}
	if len(b.jobs) == 0 || job == nil {
		return false
	}
	return b.jobs[job.Key()]
}

// isCacheEnabled checks whether caching is enabled for a job in this
// invocation.
func isCacheEnabled(job *ScheduledJob, bypass CacheBypass) bool {
	return !bypass.Excludes(job) && CanUseCache(job)
}

const genResourceID = "gen"

// writesProjectGen identifies the task that owns the generated tree. It gates
// the legacy files-less-entry rejection (remote.go): an entry claiming to be a
// generation result but carrying no payload cannot restore .gen and must not be
// served as a hit. It is NOT a cache-key input — see computeJobCacheHash.
func writesProjectGen(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil {
		return false
	}
	for _, ref := range job.JobDef.Writes {
		if ref.EffectiveScope() == extension.ResourceScopeProject && ref.ID == genResourceID {
			return true
		}
	}
	return false
}

// jobWritesSourceTree detects the resource that serializes source rewriters.
func jobWritesSourceTree(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil {
		return false
	}
	for _, ref := range job.JobDef.Writes {
		if ref.ID == extension.ResourceIDSources {
			return true
		}
	}
	return false
}

// declaredSourceRewriter identifies tasks whose successful result needs the
// clean-only cache rule. Check both signals so an invalid contract still fails
// safe before manifest validation reports the missing half of the pair.
func declaredSourceRewriter(job *ScheduledJob) bool {
	return taskDeclarationOf(job) != nil && (taskMutatesSources(job) || jobWritesSourceTree(job))
}

func rewritesSourceTree(job *ScheduledJob) bool {
	return taskMutatesSources(job) || jobWritesSourceTree(job)
}

// cacheTaskName is the common task identity used by cache-key and batch-key
// construction. Task-contract digests distinguish ordinary output shapes.
// Source rewriters add a narrow format suffix because their result envelope has
// extra restore semantics; changing the detector must not leave an older,
// over-conservative mutation marker permanently shadowing a later clean result.
func cacheTaskName(job *ScheduledJob) string {
	if job == nil || job.JobDef == nil {
		return ""
	}
	if declaredSourceRewriter(job) {
		return job.JobDef.Name + "@source-clean-v2"
	}
	return job.JobDef.Name
}

// paramHasVersionVar reports whether the params map carries a non-empty
// `version-var` (or its camelCase mirror) — the signal extensions use to
// declare that their build embeds the version string into the output.
func paramHasVersionVar(params map[string]any) bool {
	for _, key := range []string{"version-var", "versionVar"} {
		if v, ok := params[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return true
			}
		}
	}
	return false
}

// taskIsVersionAware reports whether the job's task declares that its output
// embeds the full publish version (cache.versionAware) — e.g. an npm package,
// a Go module, a version-stamped archive, or a docker image (version tag +
// manifest + stamp layer). Such tasks must vary their cache key with the
// per-commit version suffix so repackaging at a new release id is a cache miss
// even when the upstream build content is byte-identical. This is the
// manifest-declared sibling of the param-driven `version-var` signal, which
// only covers Go binaries that inject the version via ldflags.
func taskIsVersionAware(job *ScheduledJob) bool {
	return job.JobDef.TaskCachePolicy != nil && job.JobDef.TaskCachePolicy.VersionAware
}

// keyEmbedsVersion reports whether a job's cache key carries its line's full
// version. versionVarSet tells whether the job's resolved cache-key parameters
// set version-var. Only such a job reads a version in its context when the
// cache can serve it (versionsSeenBy).
func keyEmbedsVersion(job *ScheduledJob, versionVarSet bool) bool {
	return taskIsVersionAware(job) || (taskReadsVersionVar(job) && versionVarSet)
}

// taskReadsVersionVar reports whether the job's task consumes a `version-var`
// param input — the signal that it injects the publish version into its output
// via -X ldflags (the Go build/cross-compile tasks). It reads the task's
// derived cache-key params (DeriveTaskCacheKey lifts every `{from: "params"}`
// input name into Key.Params), so it needs no manifest change and only matches
// tasks that genuinely stamp a version — never test/lint/etc.
func taskReadsVersionVar(job *ScheduledJob) bool {
	if job.JobDef == nil || job.JobDef.TaskCachePolicy == nil || job.JobDef.TaskCachePolicy.Key == nil {
		return false
	}
	for _, p := range job.JobDef.TaskCachePolicy.Key.Params {
		if p == "version-var" || p == "versionVar" {
			return true
		}
	}
	return false
}

// taskCacheParams projects the full resolved task context onto parameters valid
// for the task's owning command. The task contract is the precise source:
// DeriveTaskCacheKey lifts every `inputs.<name>.from = "params"` port into
// Key.Params. Command flags are a conservative compatibility boundary for
// existing or third-party manifests whose task inputs are incomplete: they may
// over-key sibling steps of one command, but cannot leak a parent-only flag such
// as `publish --channel` into a dependency owned by `build`.
func taskCacheParams(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
) map[string]any {
	return taskCacheParamsWith(ws, job, commandParams, false)
}

// taskCacheParamsWith is the shared projection, with the selection variant
// used by a release-set selection fingerprint. That identity answers "is this
// member's packaging recipe the same one the head recorded", so it must not see
// the invocation's own flags — `--channel`, `--dry-run`, the version stamp —
// nor the release-set plan bound into the job, which is DERIVED from the
// fingerprints and would make the computation circular.
func taskCacheParamsWith(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	selection bool,
) map[string]any {
	if job == nil || job.JobDef == nil {
		return nil
	}
	paramNames := taskCacheParamNames(job)
	if len(paramNames) == 0 {
		return nil
	}

	var configDefaults map[string]any
	if ws != nil && ws.Config != nil && job.Extension != nil {
		configDefaults = ws.Config.GetCommandDefaults(jobCommandName(job), job.Extension.Name)
	}
	if selection {
		job, commandParams = jobWithoutReleaseSetPlan(job), nil
	}
	resolved := resolvedJobParams(job, commandParams, configDefaults)
	keyed := make(map[string]any, len(paramNames))
	for name := range paramNames {
		value, ok := resolved[name]
		if !ok && strings.Contains(name, "-") {
			value, ok = resolved[kebabToCamel(name)]
		}
		if ok {
			keyed[name] = value
		}
	}
	if len(keyed) == 0 {
		return nil
	}
	return keyed
}

// jobWithoutReleaseSetPlan returns a shallow copy whose bound parameters no
// longer carry the release-set plan. The copy is deliberate: the planner's job
// must keep the plan, only this key computation must not see it.
func jobWithoutReleaseSetPlan(job *ScheduledJob) *ScheduledJob {
	if job.JobDef.BoundParams == nil {
		return job
	}
	if _, bound := job.JobDef.BoundParams[releaseset.ContextParamName]; !bound {
		return job
	}
	definition := *job.JobDef
	definition.BoundParams = maps.Clone(job.JobDef.BoundParams)
	delete(definition.BoundParams, releaseset.ContextParamName)
	scoped := *job
	scoped.JobDef = &definition
	return &scoped
}

// taskCacheParamNames returns the canonical names that are safe to project for
// one task. Contract-declared names are exact. Flags from the owning command
// provide a fail-safe while task contracts are made complete; flags from a
// parent command are deliberately absent because dependency jobs retain their
// own command identity in the plan.
func taskCacheParamNames(job *ScheduledJob) map[string]struct{} {
	names := make(map[string]struct{})
	if policy := job.JobDef.TaskCachePolicy; policy != nil && policy.Key != nil {
		for _, name := range policy.Key.Params {
			names[name] = struct{}{}
		}
	}
	// A literal step binding is a behavioral task input even when the port is
	// declared `from: task` (the literal replaces that producer) or comes from a
	// legacy task with no input contract. Hash it unconditionally so the cache
	// always keys on the same value BuildJobContext delivers.
	for name := range job.JobDef.BoundParams {
		names[name] = struct{}{}
	}
	for name := range job.JobDef.Flags {
		names[name] = struct{}{}
	}
	if job.Extension != nil {
		if command := job.Extension.Jobs[jobCommandName(job)]; command != nil {
			for name := range command.Flags {
				names[name] = struct{}{}
			}
		}
	}
	return names
}

// projectFilePatterns returns "filePatterns" declared in the project's option
// layers for this job — options.{cmd}, options.{ext}, and options.{ext}:{cmd}.
// The extension layer is matched both by the extension's canonical name
// (e.g. "@putnami/typescript") and by any reference string the project's
// own `extensions` array uses for it (e.g. "/typescript/extension") — projects
// naturally key their options by the same ref they declared, and a
// canonical-name-only match silently drops those declarations. Patterns are
// project-relative globs, like a job definition's own filePatterns.
func projectFilePatterns(ws *workspace.Workspace, job *ScheduledJob) []string {
	proj := job.Project
	if proj == nil || proj.Config == nil || proj.Config.Options == nil {
		return nil
	}
	var patterns []string
	for _, layer := range projectOptionLayers(ws, job) {
		if opts, ok := proj.Config.Options[layer]; ok {
			patterns = append(patterns, paramStrings(opts, "filePatterns")...)
		}
	}
	return patterns
}

// projectOptionLayers returns the option-layer keys that apply to this job:
// options.{cmd}, options.{ext}, options.{ext}:{cmd}, plus any path-based
// extension reference the project's own `extensions` array uses for this
// extension (e.g. "/typescript/extension") and its :{cmd} variant. A project
// jobConfigScope names the option layers of THIS job's extension inside a
// project config: the extension's canonical name and the workspace path
// reference a project may declare it by ("/go/extension"). The store keeps
// those layers and every key it cannot attribute to another extension, and
// drops the rest — a deploy option addressed to a different extension is not an
// input of this task, and hashing it re-runs the task for bytes it never reads.
//
// The path reference is derived from the extension's own RelPath rather than
// from the project's `extensions` array, so the scope is the same for every
// project whose config this job hashes — its own and, through a closure input,
// its dependencies'.
//
// A job with no extension yields an EMPTY scope, which keeps the whole config:
// the wider key is the safe answer when there is no task identity to scope by.
//
// A job that rewrites its sources gets a Verbatim scope: it reads every
// config it keys as text, so the key and the post-run mutation detector hash
// the raw bytes. Both read this function, so they cannot disagree.
func jobConfigScope(ws *workspace.Workspace, job *ScheduledJob) store.ProjectConfigScope {
	if rewritesSourceTree(job) {
		return store.ProjectConfigScope{Verbatim: true}
	}
	if job == nil || job.Extension == nil {
		return store.ProjectConfigScope{}
	}
	var layers []string
	if job.Extension.Name != "" {
		layers = append(layers, job.Extension.Name)
	}
	if ref := strings.Trim(filepath.ToSlash(job.Extension.RelPath), "/"); ref != "" {
		layers = append(layers, "/"+ref)
	}
	scope := store.ProjectConfigScope{ExtensionLayers: layers}
	if ws != nil {
		ownership := extension.ResolveOptionOwnership(ws.Root, workspaceExtensionReferences(ws))
		for _, namespace := range ownership.ForeignNamespaces(job.Extension.Name) {
			// A command this extension provides is merged into its resolved
			// parameters under that name, so the block is its input whether or
			// not another manifest named it. The ownership resolver applies the
			// same rule to every extension it could read; this repeats it for
			// THIS extension, which the job carries resolved even when the
			// workspace references it by registry name.
			if _, provided := job.Extension.Commands[namespace]; provided {
				continue
			}
			scope.ForeignNamespaces = append(scope.ForeignNamespaces, namespace)
		}
		sort.Strings(scope.ForeignNamespaces)
	}
	return scope
}

// workspaceExtensionReferences is every extension reference the workspace
// resolves, sorted and deduplicated.
//
// It reads the workspace document's own list AND each project's RESOLVED one,
// which is where a scope's contribution lands, for the reason the extension
// consumer index reads both: an extension a workspace or a scope declares once
// is not in any project's own file, and a reference this list misses is a
// manifest nobody reads — its namespaces then stay in every other extension's
// key, which costs invalidation rather than correctness but costs it on every
// task of the workspace.
//
// It is derived from the PROJECT LIST, never from the run's selection: two runs
// over one tree must answer the same thing, or a task would key differently
// depending on which projects were asked for.
func workspaceExtensionReferences(ws *workspace.Workspace) []string {
	seen := make(map[string]bool)
	var references []string
	add := func(ref string) {
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		references = append(references, ref)
	}
	if ws.Config != nil {
		for _, ref := range ws.Config.Extensions.Names() {
			add(ref)
		}
	}
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		for _, ref := range project.Extensions {
			add(ref)
		}
		if project.Config == nil {
			continue
		}
		for _, ref := range project.Config.Extensions {
			add(ref)
		}
	}
	sort.Strings(references)
	return references
}

// projectOptionLayers returns the option-layer keys that apply to this job:
// options.{cmd}, options.{ext}, options.{ext}:{cmd}, plus any path-based
// extension reference the project's own `extensions` array uses for this
// extension (e.g. "/typescript/extension") and its :{cmd} variant. A project
// keys its options by the same ref it declared, so matching only the canonical
// extension name silently drops those declarations. projectFilePatterns and
// projectEnvInputs read the SAME layers, so file and env cache-key inputs are
// declared uniformly; keeping the layer list in one place stops the two readers
// from drifting. Callers guard proj/Config/Options non-nil before calling.
func projectOptionLayers(ws *workspace.Workspace, job *ScheduledJob) []string {
	cmdName := jobCommandName(job)
	layers := []string{cmdName, job.Extension.Name, job.Extension.Name + ":" + cmdName}
	for _, ref := range job.Project.Config.Extensions {
		if ref == job.Extension.Name || !strings.HasPrefix(ref, "/") {
			continue
		}
		if filepath.Join(ws.Root, ref) == job.Extension.Path {
			layers = append(layers, ref, ref+":"+cmdName)
		}
	}
	return layers
}

// projectEnvInputs returns the environment-variable NAMES a project declares as
// cache-key inputs in its option layers for this job, under the `envInputs`
// key — the env-var sibling of filePatterns, read from the same layers. An
// env-gated task (e.g. a suite that runs against a live target only when
// PUTNAMI_E2E_TARGET is set and skips-with-reason otherwise) declares the
// selecting vars here so a run WITH the env set keys differently from a run
// without it; without the declaration nothing folds those vars into the key and
// the env-set run serves the env-less run's cached skip and never executes.
//
// Two declaration forms are accepted:
//   - a literal name (no glob metacharacter) is returned as-is even when unset,
//     so a declared-but-unset var still folds "NAME=" into the key and a later
//     env-set run keys differently; and
//   - a glob (e.g. "PUTNAMI_E2E_*") is expanded against the CURRENT process
//     environment, contributing every currently-set var NAME it matches.
//
// The result is sorted and deduplicated: hashEnvVars re-sorts the names but does
// not deduplicate, so declaring the same var twice — once literally and once via
// a glob that also matches it — must collapse here, or the duplicate would
// destabilize the key relative to declaring it once.
func projectEnvInputs(ws *workspace.Workspace, job *ScheduledJob) []string {
	proj := job.Project
	if proj == nil || proj.Config == nil || proj.Config.Options == nil {
		return nil
	}
	var decls []string
	for _, layer := range projectOptionLayers(ws, job) {
		if opts, ok := proj.Config.Options[layer]; ok {
			decls = append(decls, paramStrings(opts, "envInputs")...)
		}
	}
	if len(decls) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(decls))
	var names []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, decl := range decls {
		if isEnvGlob(decl) {
			for _, name := range matchEnvNames(decl) {
				add(name)
			}
			continue
		}
		add(decl)
	}
	sort.Strings(names)
	return names
}

// isEnvGlob reports whether an envInputs declaration is a glob pattern rather
// than a literal environment-variable name — i.e. it carries a path.Match
// metacharacter. Env-var names are flat (no path separators), so path.Match's
// single-segment semantics apply to names directly.
func isEnvGlob(decl string) bool {
	return strings.ContainsAny(decl, "*?[")
}

// matchEnvNames expands a glob against the CURRENT process environment,
// returning the names of every currently-set variable whose name matches. A
// malformed pattern matches nothing (path.Match's error is treated as no-match),
// mirroring how the file-glob machinery ignores match errors.
func matchEnvNames(pattern string) []string {
	var names []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if matched, err := path.Match(pattern, name); err == nil && matched {
			names = append(names, name)
		}
	}
	return names
}

// generateAssetFiles returns the absolute paths of any cross-project "generate"
// assets declared in the job's project config, so they contribute to the cache
// key. The deeply-typed config map walk is isolated here, using early returns,
// to keep computeJobCacheHash flat and readable.
func generateAssetFiles(ws *workspace.Workspace, job *ScheduledJob) []string {
	if job.Project.Config == nil {
		return nil
	}
	genOpts, ok := job.Project.Config.Options["generate"]
	if !ok {
		return nil
	}
	assetsRaw, ok := genOpts["assets"]
	if !ok {
		return nil
	}
	assets, ok := assetsRaw.([]any)
	if !ok {
		return nil
	}

	var files []string
	for _, entry := range assets {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		from, ok := m["from"].(string)
		if !ok || from == "" {
			continue
		}
		if strings.HasPrefix(from, "/") {
			files = append(files, filepath.Join(ws.Root, from[1:]))
		} else {
			files = append(files, filepath.Join(ws.Root, job.Project.Path, from))
		}
	}
	return files
}

// keyFilePatterns returns the project-relative and workspace-relative pattern
// sets that select this job's file-content cache-key inputs.
//
// The cache key and the source-rewriter mutation detector must read the SAME
// declaration: a pattern source folded into one and not the other would let a
// fixer rewrite a keyed file the detector never looks at, and publish that
// pre-fix key as a reusable clean verdict. Both callers therefore go through
// this function rather than assembling the list themselves.
//
// Sources, in order: the task's declared cache key files (falling back to the
// job's own filePatterns), the command's raw `filePatterns` flag, and the
// project's option layers — commandParams carries only CLI flags, so a
// project-level declaration (e.g. options."/typescript/extension".filePatterns)
// must be read from those layers directly or an edit to it silently hits a
// stale entry.
func keyFilePatterns(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
) (files, workspaceFiles []string) {
	if job.JobDef.TaskCachePolicy != nil && job.JobDef.TaskCachePolicy.Key != nil {
		// Copied, not aliased: the appends below must never reach into the
		// manifest-owned backing array through spare capacity.
		files = append(files, job.JobDef.TaskCachePolicy.Key.Files...)
		workspaceFiles = append(workspaceFiles, job.JobDef.TaskCachePolicy.Key.WorkspaceFiles...)
	}
	// A declared `closure` input covers the SEED project too — the closure is the
	// project plus what it depends on — so its patterns are project-relative key
	// patterns as well. Stating that here is not redundancy for its own sake: the
	// file-hash helper treats an EMPTY pattern set as "hash the whole project
	// tree", so a task whose only file input is a closure port would otherwise key
	// on every source file it never reads.
	files = append(files, closureKeyPatterns(job)...)
	if len(files) == 0 {
		files = append(files, job.JobDef.FilePatterns...)
	}
	files = append(files, paramStrings(commandParams, "filePatterns")...)
	files = append(files, projectFilePatterns(ws, job)...)
	return files, workspaceFiles
}

// Go embed selectors are extension capabilities, not user-supplied globs.
// A project option or CLI filePatterns flag has no negotiated manifest stamp;
// accepting one there would let an older CLI silently drop that input.
func validateDeclaredGoEmbedSelectors(ws *workspace.Workspace, job *ScheduledJob, params map[string]any) error {
	for _, patterns := range [][]string{paramStrings(params, "filePatterns"), projectFilePatterns(ws, job)} {
		for _, pattern := range patterns {
			if strings.HasPrefix(pattern, "go-embed:") {
				return fmt.Errorf("%w: selector %q requires an extension task input, not filePatterns", store.ErrGoEmbedInput, pattern)
			}
		}
	}
	return nil
}

// validateUnkeyedGoEmbedInputs keeps required selector inputs fail-closed for
// tasks without a cache identity. Keyed tasks validate these same declarations
// through ComputeHashUsing; this path runs only when key computation is skipped.
func validateUnkeyedGoEmbedInputs(ws *workspace.Workspace, job *ScheduledJob, params map[string]any) error {
	if err := validateDeclaredGoEmbedSelectors(ws, job, params); err != nil {
		return err
	}
	files, workspaceFiles := keyFilePatterns(ws, job, params)
	hasSelector := func(patterns []string) bool {
		for _, pattern := range patterns {
			if strings.HasPrefix(pattern, "go-embed:") {
				return true
			}
		}
		return false
	}
	cache := store.NewCacheManager(nil)
	validatePatterns := func(root string, patterns []string, scope store.ProjectConfigScope) error {
		if !hasSelector(patterns) {
			return nil
		}
		_, err := cache.HashFiles(root, patterns, scope)
		if errors.Is(err, store.ErrGoEmbedInput) {
			return err
		}
		return nil
	}
	if err := validatePatterns(filepath.Join(ws.Root, job.Project.Path), files, jobConfigScope(ws, job)); err != nil {
		return fmt.Errorf("validate unkeyed project inputs: %w", err)
	}
	if err := validatePatterns(ws.Root, workspaceFiles, store.ProjectConfigScope{Verbatim: true}); err != nil {
		return fmt.Errorf("validate unkeyed workspace inputs: %w", err)
	}
	if hasSelector(closureKeyPatterns(job)) {
		for _, member := range projectDependencyClosure(ws, job.Project) {
			if err := validatePatterns(filepath.Join(ws.Root, member.Path), closureKeyPatterns(job), jobConfigScope(ws, job)); err != nil {
				return fmt.Errorf("closure inputs for %s: %w", member.ID, err)
			}
		}
	}
	if job.InvocationProducer != nil {
		if err := validateUnkeyedGoEmbedInputs(ws, job.InvocationProducer, params); err != nil {
			return fmt.Errorf("invocation producer for %s: %w", job.Key(), err)
		}
	}
	return nil
}

// hostOSClass maps the OS class store.BuildCacheKey records for this host to
// the one computeJobCacheHashWith keys with. It is the identity; tests replace
// it to key a job the way another host would.
var hostOSClass = func(osClass string) string { return osClass }

// workspaceSourceState is the source state computeJobCacheHashWith keys with,
// and the one the scheduler's version stamp marks: store.SourceStateUnmanaged
// where Git does not manage the workspace root, and empty inside a repository.
// Both read the run's cache manager, so they share one answer. Tests replace it
// to key a job the way a root in the other state would.
var workspaceSourceState = func(cache *store.CacheManager, workspaceRoot string) string {
	return cache.SourceState(workspaceRoot)
}

// computeJobCacheHash derives a job's cache key from its inputs. The upstream
// hashes mixed into the key are the dependencies' cache keys (read from
// hashes), not their output bytes — so the key is fully input-derived and
// computable before the job runs, given its dependencies' keys are known.
func computeJobCacheHash(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	versions RunVersions,
	cache *store.CacheManager,
	hashes map[string]string,
) (string, error) {
	return computeJobCacheHashWith(ws, job, commandParams, versions, cache, hashes, false)
}

// computeJobCacheHashWith is the one derivation behind two identities of the
// same task (D13).
//
// With selection false it is the EXECUTION key: every declared input, the
// project version and the embedded version included, so a build across commits
// never serves bytes that embed the previous commit's version.
//
// With selection true it is the SELECTION key: the same computation with both
// version arguments emptied, the invocation's own parameters withheld, and no
// OS class. A
// release-set member records sha256 of it, and a later publication republishes
// the member exactly when that value moved — so a packager upgrade or an
// upstream change republishes, while re-running the same tree at a new version
// stamp does not.
func computeJobCacheHashWith(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	versions RunVersions,
	cache *store.CacheManager,
	hashes map[string]string,
	selection bool,
) (string, error) {
	if err := validateDeclaredGoEmbedSelectors(ws, job, commandParams); err != nil {
		return "", err
	}
	cacheParams := taskCacheParamsWith(ws, job, commandParams, selection)

	// Build cache key policy from task cache config
	var policy store.CacheKeyPolicy
	if job.JobDef.TaskCachePolicy != nil && job.JobDef.TaskCachePolicy.Key != nil {
		policy.Env = job.JobDef.TaskCachePolicy.Key.Env
	}
	policy.Files, policy.WorkspaceFiles = keyFilePatterns(ws, job, commandParams)

	// Merge project-declared envInputs into the env-var key inputs, mirroring
	// filePatterns. An env-gated task — one whose skip path is selected by env
	// vars the key otherwise ignores — declares those vars so a run with the env
	// set keys differently from an env-less run, closing the "0 succeeded, N
	// cached" hole where the env-set run silently serves the env-less run's
	// cached skip. projectEnvInputs already sorts+dedupes; policy.Env flows into
	// the key via hashEnvVars.
	if extra := projectEnvInputs(ws, job); len(extra) > 0 {
		policy.Env = append(policy.Env, extra...)
	}

	// A project config is addressed to several extensions at once, so the key
	// reads only the layers this job's extension owns (see ProjectConfigScope).
	policy.ConfigScope = jobConfigScope(ws, job)

	// Resolve cross-project generate assets as extra files for cache key
	policy.ExtraFiles = append(policy.ExtraFiles, generateAssetFiles(ws, job)...)

	// Ambient runtime facts the task declared (host platform). The toolchain
	// field differs between platforms for a task that uses a runtime toolchain,
	// and the implementation digest for an installed extension that ships
	// platform executables; a task with neither shares its key between a
	// developer and CI on another platform. Neither is a declaration, so a task
	// whose output depends on the machine declares the host platform here.
	policy.RuntimeIdentity = taskRuntimeIdentity(job)
	baseline, err := releaseBaselineIdentity(ws, job, cache)
	if err != nil {
		return "", fmt.Errorf("cache key for %s: %w", job.Key(), err)
	}
	if baseline != "" {
		policy.RuntimeIdentity = append(policy.RuntimeIdentity, baseline)
	}

	// Collect upstream hashes for dependencies
	var upstreamHashes []string
	for _, depKey := range job.DependsOn {
		if h, ok := hashes[depKey]; ok {
			upstreamHashes = append(upstreamHashes, h)
		}
	}

	// A consumer of an invocation-scoped resource folds in the PRODUCING
	// ACTION's digest.
	//
	// It cannot come through the loop above. A producer is uncacheable by
	// contract — a task with an invocation-scoped output must set cache:false —
	// so it never publishes a key, and without this the consumer's identity
	// would be blind to the setup it ran against: two different provisioning
	// actions would serve each other's cached verdicts. The digest is derived
	// from the producer's own declared inputs, so the secret's CONTENT is never
	// an input to any cache key; that is the whole point of using the action
	// rather than the artifact.
	//
	// It also REPLACES a deleted mechanism: the test-infra planner used to
	// hash the synthesized DATABASE_TEST_BINDINGS itself and exported that
	// digest into a per-project environment variable so hashEnvVars would pick
	// it up. That made a consumer's identity depend on the credential's bytes —
	// a re-provisioned server on a new ephemeral port produced a different key
	// for identical inputs — and it needed a process-global side effect
	// sequenced before every key computation to work at all.
	producerDigest, err := invocationProducerDigest(ws, job, commandParams, versions, cache, hashes, selection)
	if err != nil {
		return "", fmt.Errorf("invocation producer for %s: %w", job.Key(), err)
	}
	if producerDigest != "" {
		upstreamHashes = append(upstreamHashes, producerDigest)
	}

	// A task that declares a `closure` input reads its DEPENDENCIES' committed
	// files, so those bytes are part of its identity.
	//
	// It cannot travel through policy.Files, which is resolved against this
	// project's root alone, nor through the dependency hashes above: a
	// dependency contributes an upstream hash only when a job of that project is
	// in the plan and cacheable, and a closure input is about COMMITTED files,
	// not about another task's output. Without the fold, `test-env-up` — whose
	// runtime walks the closure's infra/requirements.json — kept one key while
	// the environment it provisions changed, and the consumer that folds its
	// action digest served a stored verdict against a different world.
	digest, err := closureInputsDigest(ws, job, cache)
	if err != nil {
		return "", err
	}
	if digest != "" {
		upstreamHashes = append(upstreamHashes, digest)
	}

	// A task that keys on the Git candidate cut judges the workspace, and it
	// reads the membership from its context: which projects there are, their
	// names, paths, configs, extensions and edges. User config or an ignored
	// scope manifest can change that membership without changing one candidate
	// file, so the cut alone would replay a verdict about another set of
	// projects.
	if keysOnCandidateCut(job) {
		digest, err := workspaceMembershipDigest(ws)
		if err != nil {
			return "", err
		}
		upstreamHashes = append(upstreamHashes, digest)
	}

	projRoot := filepath.Join(ws.Root, job.Project.Path)

	// When the job embeds a version string into its output, the cache key must
	// vary with the commit suffix — otherwise successive builds across commits
	// hit the same cache and serve stale bytes that embed the previous commit's
	// version. Two signals declare this: a task-level `cache.versionAware` policy
	// (version-stamped package artifacts: npm packages, Go modules, archives), or
	// a version-stamping build task whose resolved, declared parameter inputs
	// carry version-var. Resolving that task projection covers CLI flags,
	// workspace defaults and project options under every parent command without
	// leaking the signal into tasks that do not read it.
	//
	// Writing the project `gen` resource is deliberately NOT one of them.
	// It used to be, on the grounds that the scheduler seeds .gen/version.json
	// inside the tree a gen task captures — but generation OUTPUT is derived from
	// declared content, and the one revision-bearing file in it is that seeded
	// stamp, which restoreDeclaredCacheHit re-materializes at the CURRENT identity
	// after every hit (task_capture.go). Keying generation on the commit instead
	// made every gen task miss on every SHA and, because downstream keys mix
	// upstream TASK KEYS, dragged the whole compile/test/describe closure cold with
	// it — 54 generate, 18 cross-compile, 18 test and 18 lint misses on an
	// otherwise-unchanged tree, while project-scoped tasks that do not depend on
	// generation hit.
	//
	// Removing the signal needs NO store.cacheKeyVersion bump: EmbeddedVersion
	// stays a positional field of the key, so a generation key simply moves to a
	// new address. Every key that does move is new (no pre-change entry lives
	// there), and every key that does NOT move belongs to a task whose semantics
	// are unchanged, so no entry can be reinterpreted. Superseded generation
	// entries age out through ordinary GC instead of costing every user and every
	// CI namespace the whole-cache miss a bump would.
	var embeddedVersion string
	if len(versions) > 0 && keyEmbedsVersion(job, paramHasVersionVar(cacheParams)) {
		if projVersion := VersionInfoForProject(versions, job.Project); projVersion != nil {
			embeddedVersion = projVersion.Full
		}
	}

	extensionImplementationDigest, err := extensionImplementationDigest(ws, job, cache)
	if err != nil {
		return "", fmt.Errorf("hash extension implementation: %w", err)
	}

	// The line's base version is not a key input (ADR 0060): it comes from the
	// tags and commit messages a checkout holds, which differ between lanes
	// that build one tree. A task whose output embeds it declares it above.
	if selection {
		// A selection fingerprint that moved with the version would republish
		// every member on every publication, which is exactly what it exists
		// to avoid.
		embeddedVersion = ""
	}

	cacheKey := store.BuildCacheKey(
		job.Extension.Name,
		job.Extension.Version,
		extensionImplementationDigest,
		cacheToolchainVersion(job),
		cacheTaskName(job),
		jobContractDigest(job),
		job.Project.Name,
		ws.MetadataDigestFor(job.Project),
		embeddedVersion,
		selectedProjectIDs(job.SelectedProjects),
		cacheParams,
		projRoot,
		ws.Root,
		policy,
		upstreamHashes,
	)
	cacheKey.OSClass = hostOSClass(cacheKey.OSClass)
	if selection {
		// The OS class keeps Windows cache entries apart from POSIX ones
		// (D-W5). A selection key is compared with the one a publication
		// recorded on whatever host ran it, so it carries no OS class.
		cacheKey.OSClass = ""
	} else {
		// The source state keeps an output built with no source binding apart
		// from one built inside a repository. A selection key belongs to a
		// publication, which a root without a repository never reaches.
		cacheKey.SourceState = workspaceSourceState(cache, ws.Root)
	}

	hash, err := cacheKey.ComputeHashUsing(cache)
	if err != nil {
		return "", err
	}

	return hash, nil
}

// keysOnCandidateCut reports whether the job's task declares a key input on
// the Git candidate cut (a `git:` pattern, ADR 0041), from the project, the
// workspace or the dependency closure.
func keysOnCandidateCut(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil ||
		job.JobDef.TaskCachePolicy == nil || job.JobDef.TaskCachePolicy.Key == nil {
		return false
	}
	key := job.JobDef.TaskCachePolicy.Key
	isGitPattern := func(pattern string) bool {
		_, ok := wsproto.GitFilePattern(pattern)
		return ok
	}
	return slices.ContainsFunc(key.Files, isGitPattern) ||
		slices.ContainsFunc(key.WorkspaceFiles, isGitPattern) ||
		slices.ContainsFunc(key.ClosureFiles, isGitPattern)
}

// workspaceMembershipDigest hashes the membership a job context carries
// (workspaceProjectsContext), less what differs between two checkouts of one
// tree: the line's base version and the absolute path.
func workspaceMembershipDigest(ws *workspace.Workspace) (string, error) {
	members := workspaceProjectsContext(ws, nil)
	for i := range members {
		members[i].Version = ""
		members[i].FullPath = ""
	}
	encoded, err := json.Marshal(members)
	if err != nil {
		return "", fmt.Errorf("encode the workspace membership: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "workspaceMembership:" + hex.EncodeToString(sum[:]), nil
}

// closureKeyPatterns returns the project-relative globs this job's task
// declared as `closure` inputs — the ones hashed across the whole dependency
// closure rather than against this project alone.
func closureKeyPatterns(job *ScheduledJob) []string {
	if job == nil || job.JobDef == nil ||
		job.JobDef.TaskCachePolicy == nil || job.JobDef.TaskCachePolicy.Key == nil {
		return nil
	}
	return job.JobDef.TaskCachePolicy.Key.ClosureFiles
}

// closureInputsDigest hashes the job's declared closure patterns in every member
// of the project's dependency closure, or returns "" when the task declares no
// closure input.
//
// Each member contributes its workspace-relative PATH and its content digest, so
// the value is independent of where the workspace is checked out, as ExtraFiles,
// the other cross-project key mechanism, is for a path under the workspace root.
// That matters here because this digest reaches a CONSUMER's key through the
// producing action, and a key that varied with the checkout directory would make
// every remote entry unshareable between a developer and CI.
//
// A member with no matching file contributes its empty digest rather than being
// skipped, so ADDING a manifest to a dependency moves the key exactly as
// editing one does. The per-member digests come from the CacheManager's memo, so
// one closure walk costs one stat per (member, pattern set) per CLI invocation.
func closureInputsDigest(ws *workspace.Workspace, job *ScheduledJob, cache *store.CacheManager) (string, error) {
	patterns := closureKeyPatterns(job)
	if len(patterns) == 0 || ws == nil || cache == nil || job == nil || job.Project == nil {
		return "", nil
	}
	members := projectDependencyClosure(ws, job.Project)
	if len(members) == 0 {
		return "", nil
	}
	// The closure is already in canonical project-id order, but sort the parts
	// anyway: the digest must not depend on the graph walk's ordering.
	parts := make([]string, 0, len(members))
	for _, member := range members {
		digest, err := cache.HashFiles(filepath.Join(ws.Root, member.Path), patterns, jobConfigScope(ws, job))
		if err != nil {
			return "", fmt.Errorf("closure inputs for %s: %w", member.ID, err)
		}
		parts = append(parts, member.Path+"\x00"+digest)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x01")))
	return "closureFiles:" + hex.EncodeToString(sum[:]), nil
}

// invocationProducerDigest returns the action digest of the producer whose
// invocation-scoped resource this job consumes, or "" when the job consumes
// none.
//
// The recursion terminates because a consumer is transitively DOWNSTREAM of its
// producer in an acyclic plan, so following producer links strictly ascends the
// graph.
func invocationProducerDigest(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	versions RunVersions,
	cache *store.CacheManager,
	hashes map[string]string,
	selection bool,
) (string, error) {
	if job == nil || job.InvocationProducer == nil {
		return "", nil
	}
	digest, err := computeJobCacheHashWith(ws, job.InvocationProducer, commandParams, versions, cache, hashes, selection)
	if err != nil {
		return "", err
	}
	return digest, nil
}

// cacheKeyDependencies names every plan key whose cache hash contributes to
// this job's key, directly or through the producing action of an
// invocation-scoped resource it consumes.
//
// The execution path copies only the entries it needs out of the shared hash
// map, so it has to ask for the producer's dependencies too — otherwise a
// consumer's key would be computed from a smaller upstream set at execution
// time than PrecomputeKeys used, and the two would name different addresses for
// the same work.
func cacheKeyDependencies(job *ScheduledJob) []string {
	if job == nil || job.InvocationProducer == nil {
		return job.DependsOn
	}
	keys := append([]string(nil), job.DependsOn...)
	for producer := job.InvocationProducer; producer != nil; producer = producer.InvocationProducer {
		keys = append(keys, producer.DependsOn...)
	}
	return keys
}

// jobContractDigest is the task-contract digest stamped on the job's manifest
// task at discovery (extension.Resolve / pipeline expansion). Empty for jobs
// with no manifest task; the key treats that as a stable value.
func jobContractDigest(job *ScheduledJob) string {
	if job == nil || job.JobDef == nil {
		return ""
	}
	return job.JobDef.ContractDigest
}

func selectedProjectIDs(projects []*workspace.Project) []string {
	if len(projects) == 0 {
		return nil
	}
	ids := make([]string, 0, len(projects))
	seen := make(map[string]bool, len(projects))
	for _, project := range projects {
		if project == nil {
			continue
		}
		id := project.ID
		if id == "" {
			id = project.Name
		}
		if id == "" {
			id = project.Path
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	return ids
}

// resultEmittedMeta reports whether the job subprocess emitted a "meta" event.
// The extension SDK emits meta only once the job binary starts its own logic,
// so its presence rules out a wrapper bail-out (e.g. an unavailable Go
// toolchain, or an empty project) that emits a bare SKIP before the job runs.
func resultEmittedMeta(result *JobResult) bool {
	if result.EmittedMeta {
		return true
	}
	for i := range result.Events {
		if result.Events[i].Type == EventTypeMeta {
			return true
		}
	}
	return false
}

// taskIsDeterministic reports whether the job's task declares that its result is
// fully determined by its cache key (cache.deterministic). Tasks whose outcome
// depends on host or runtime state outside the key leave this false. Tasks
// whose skip path depends on env vars must declare those vars as env inputs so
// the key captures them.
func taskIsDeterministic(job *ScheduledJob) bool {
	return job.JobDef.TaskCachePolicy != nil && job.JobDef.TaskCachePolicy.Deterministic
}

// isCacheableResult reports whether a freshly executed job result should be
// written to the cache. Successful results always are.
//
// A "skipped" result is cached only when BOTH hold:
//   - the task is declared deterministic, so the skip is a function of the
//     cache-key inputs (e.g. "this library has no main package to describe")
//     and not of runtime state the key does not capture; and
//   - the job actually ran before skipping (a meta event was emitted), ruling
//     out wrapper bail-outs that skip before the job starts (e.g. an
//     unavailable Go toolchain), which depend on host state.
//
// Both guards are required: the first excludes in-job skips that depend on
// state outside the cache key; the second excludes env-dependent pre-job skips.
// Caching either would let one environment poison a later run that should do
// real work.
func isCacheableResult(job *ScheduledJob, result *JobResult) bool {
	switch result.Status {
	case "success":
		return true
	case "skipped":
		return taskIsDeterministic(job) && resultEmittedMeta(result)
	default:
		return false
	}
}
