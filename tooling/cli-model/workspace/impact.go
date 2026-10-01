package workspace

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

// ChangeImpactOptions widens the shared change→project mapping for callers that
// see file changes --impacted's git diff cannot express.
type ChangeImpactOptions struct {
	// WorkspaceInputPatterns are the glob patterns declared by task inputs scoped
	// `from: "workspace"` — lockfiles, root compiler/linter config. Those files sit
	// OUTSIDE every project root, so the path-prefix mapping attributes them to no
	// project at all, yet a change to one invalidates every project that may run
	// the task. When a changed file matches one of these patterns, EVERY project is
	// treated as directly affected.
	//
	// It exists because the watch loop's deleted classifier
	// (internal/watch/classifier.go, removed by an earlier cleanup) covered this case
	// and --impacted does not: a `bun.lock` edit re-ran every project under
	// `--watch` and selected nothing under `--impacted`. The gap now lives in this
	// one shared function as an explicit, opt-in option rather than in a second
	// algorithm. `--impacted` deliberately passes it EMPTY. Its root-file widening
	// comes from each provider's precise project-level watchedFiles answer instead;
	// this coarse whole-workspace option remains for callers that explicitly
	// declare task input patterns, such as watch.
	WorkspaceInputPatterns []string

	// Tasks resolves a change onto the TASKS that read it. Nil keeps the
	// project-level mapping: a changed file claims its project whole and a
	// dependency edge reaches a dependent whole.
	Tasks TaskImpactIndex
}

// TaskImpactIndex answers the task-level questions the change→project pass asks
// of the extension manifests the model never loads.
//
// A task scope is a plan name: `<command>~<step>` for a pipeline step, the
// command alone for a command job with no pipeline. That is the spelling
// ScheduledJob.CommandName() and StepID() derive, the one the plan table
// prints, and the one the trace, `--verbose`, `impacted` and `why_impacted`
// report. Every answer is sorted and deduplicated, so one trace reads the same
// on every run.
//
// The index errs WIDE by construction, and the asymmetry ADR 0043 names is why:
// a task wrongly left out of an answer is a gate that never ran, while one
// wrongly left in costs a job that had nothing to do — and the cache serves it.
type TaskImpactIndex interface {
	// TasksReadingPath returns the tasks of a project whose declared inputs
	// select the workspace-relative path — the same declarations the cache
	// keys on — closed under the tasks that depend on them inside the project.
	// Empty means no action of the project reads the file, and the file claims
	// nothing there.
	TasksReadingPath(projectID, path string) []string

	// TasksReachedFrom returns the tasks a project runs because the listed
	// tasks of a project it reads moved: the ones whose step declares a `^`
	// reference to one of them, closed under the tasks that depend on those. A
	// nil list stands for every task of the source project.
	//
	// It names no project because the `^` relation is declared by a manifest,
	// not by a project: which pairs of projects it crosses — a dependency edge
	// or a contract edge — is the graph's business, and the graph is the
	// model's.
	TasksReachedFrom(tasks []string) []string

	// TasksOfExtension returns every task an in-workspace extension project's
	// manifest declares, or nil when the project declares no extension
	// manifest. It is the extension-consumer scope of ADR 0042 said in the
	// vocabulary above.
	TasksOfExtension(extensionProjectID string) []string

	// ReadsAsExtensionRuntime reports whether a path is part of an
	// in-workspace extension's runtime — built or embedded into the binary its
	// consumers execute — rather than an input of one of its own tasks. Such a
	// path keeps the extension project whole, as its manifest does.
	ReadsAsExtensionRuntime(projectID, path string) bool
}

// ProjectsForChangedFiles maps a set of changed workspace-relative paths onto
// every project that must rebuild: for each path the nearest project whose
// directory holds it, plus every project that claims it as an asset, as a
// declared file input, or as a scope config it inherits from, optionally
// widened by workspace-scoped task inputs, then propagated through the graph's
// impact edges to their transitive dependents.
//
// It is the ONE change→project calculation in the CLI. `--impacted` reaches it
// through ImpactedProjectsWithBaseline (git diff → here) and the watch loop
// reaches it directly (file watcher → here), so a fix to the mapping cannot land
// on one surface and miss the other. Results are ordered by ws.Projects, so the
// map iteration inside stays invisible to callers.
func ProjectsForChangedFiles(ws *Workspace, changedFiles []string, opts ChangeImpactOptions) []*Project {
	projects, _ := ChangeImpact(ws, changedFiles, opts)
	return projects
}

// ChangeImpact is ProjectsForChangedFiles plus the evidence ImpactedSelection
// reports: the changed workspace-root paths that reached no project. Both come
// out of ONE pass over the diff, so the explanation cannot describe a different
// mapping than the one that produced the selection.
func ChangeImpact(ws *Workspace, changedFiles []string, opts ChangeImpactOptions) ([]*Project, []string) {
	r := TraceChangeImpact(ws, changedFiles, opts)
	return r.Projects, r.UnownedRootFiles
}

// ImpactEdgeKind names the relation that put a project into the impacted set.
type ImpactEdgeKind string

const (
	// Seed kinds: a changed file's direct claim on a project (classifyChangedPaths).

	// ImpactSeedPathOwner is the project or asset path that contains the file;
	// Via is that path.
	ImpactSeedPathOwner ImpactEdgeKind = "path-owner"
	// ImpactSeedRootWatchedFile is a provider's per-project watchedFiles answer
	// claiming a workspace-root entry; Via is the claimed entry.
	ImpactSeedRootWatchedFile ImpactEdgeKind = "root-watched-file"
	// ImpactSeedDeclaredInput is an `options.<layer>.filePatterns` set that
	// selected the file; Via is that set, joined by one space.
	ImpactSeedDeclaredInput ImpactEdgeKind = "declared-input"
	// ImpactSeedWorkspaceInput is a ChangeImpactOptions.WorkspaceInputPatterns
	// entry that matched; Via is that pattern.
	ImpactSeedWorkspaceInput ImpactEdgeKind = "workspace-input"
	// ImpactSeedScopeConfig is a scope's putnami.json the project inherits its
	// tags, extensions or name pattern from (Project.Scope.ConfigPaths); Via is
	// the scope directory.
	ImpactSeedScopeConfig ImpactEdgeKind = "scope-config"

	// Edge kinds: how propagateImpact reached a project from one already in the set.

	// ImpactEdgeDependency is a declared or provider-derived dependency
	// (ws.Graph.ImpactDependentsOf). An activated scope's implicit include
	// edge is not one: it orders the schedule and carries no impact,
	// so it never names a reason here.
	ImpactEdgeDependency ImpactEdgeKind = "dependency"
	// ImpactEdgeExtensionConsumer is an impact-only extension-consumer edge from
	// extensionConsumerIndex. It leaves an extension whose binary the change
	// rebuilt and reaches a consumer for that extension's tasks only: the
	// consumer is task-scoped (ImpactTrace.Scopes), and nothing propagates out
	// of it.
	ImpactEdgeExtensionConsumer ImpactEdgeKind = "extension-consumer"
	// ImpactEdgeContract is the derived edge from a provider to a generated
	// client of its contract (DependencyGraph.ContractClientsOf). The contract
	// is the client's whole provider-side input, so a change reaches the
	// client over this edge only when it moved the provider's COMMITTED
	// contract (Project.ContractPath is one of the changed files); an
	// implementation-only change of the provider leaves every client alone.
	// A provider change that would move the contract without committing it
	// fails the provider's own drift check, and a regenerated client's files
	// seed the client directly. When the edge fires, it reaches the client's
	// tasks the way a dependency edge does: the client's `^` step references
	// resolved against the provider. ImpactReach, which asks about a change to
	// the provider as a whole, fires it unconditionally.
	ImpactEdgeContract ImpactEdgeKind = "contract"
)

// ImpactSeed is one changed file's claim on a project, before propagation.
type ImpactSeed struct {
	// File is the changed path, workspace-relative, as classifyChangedPaths
	// cleaned it, in slash form on every host.
	File string
	// Kind is one of the five seed kinds.
	Kind ImpactEdgeKind
	// Via is the path, watched entry, pattern set, or scope directory that made
	// the claim. A path or directory is in slash form on every host; a pattern
	// set keeps the form it was authored in.
	Via string
}

// ImpactEdge is the edge the walk crossed when it reached a project in the
// state the project ended in: the edge that made it FULL when any did — a
// project that runs everything names the reason it does — and the first edge
// that reached it at all otherwise. A project's scope can still grow after
// that edge through another one; the scope is the union, and Scopes is where
// it is read.
type ImpactEdge struct {
	// From is a project ID already in the set.
	From string
	// Kind is one of the three edge kinds.
	Kind ImpactEdgeKind
	// Via is, on a contract edge a change fired, the provider's committed
	// contract that changed: workspace-relative, in slash form. It is empty
	// on every other kind, and on a contract edge ImpactReach crossed, which
	// names no changed file.
	Via string
	// ContractSHA256 is, beside Via, the sha256 of that contract as the
	// workspace loaded it (Project.ContractSHA256): the digest a client
	// regenerated from it records.
	ContractSHA256 string
}

// ImpactTrace explains one change-to-project mapping. Every selected project
// has an entry in exactly one of Seeds and Edges: a project with seeds has no
// edge.
type ImpactTrace struct {
	// Seeds maps a project ID to the claims that seeded it, in changedFiles
	// order.
	Seeds map[string][]ImpactSeed
	// Edges maps a propagated project ID to the edge that reached it (ImpactEdge).
	Edges map[string]ImpactEdge
	// Scopes maps a task-scoped project ID to the sorted tasks of it that the
	// change reaches (TaskImpactIndex names the spelling). A project absent
	// from Scopes is fully impacted: every task of every command runs. A
	// present one runs the listed tasks and what those depend on, and nothing
	// else.
	//
	// Without a TaskImpactIndex the entries are the extension project IDs of
	// the ADR 0042 extension-consumer scope instead — the same relation at
	// project resolution. The two spellings never collide: a project ID starts
	// with "/" and a task scope never does.
	Scopes map[string][]string
}

// ScopeOf returns the tasks a task-scoped project is limited to, or nil for a
// fully impacted (or unknown) project.
func (t ImpactTrace) ScopeOf(id string) []string {
	return t.Scopes[id]
}

// ImpactStep is one hop of a path through the trace. Kind is "" on the first
// step, which is a seed.
type ImpactStep struct {
	Project string
	Kind    ImpactEdgeKind
}

// PathTo walks Edges back from id to a seed and returns the hops seed-first.
// Nil when id is not in the trace.
func (t ImpactTrace) PathTo(id string) []ImpactStep {
	var reversed []ImpactStep
	seen := make(map[string]bool)
	for current := id; ; {
		if seen[current] {
			return nil
		}
		seen[current] = true
		if _, seeded := t.Seeds[current]; seeded {
			reversed = append(reversed, ImpactStep{Project: current})
			break
		}
		edge, ok := t.Edges[current]
		if !ok {
			return nil
		}
		reversed = append(reversed, ImpactStep{Project: current, Kind: edge.Kind})
		current = edge.From
	}
	path := make([]ImpactStep, len(reversed))
	for i, step := range reversed {
		path[len(reversed)-1-i] = step
	}
	return path
}

// ImpactResult is ChangeImpact's full answer.
type ImpactResult struct {
	// Projects is the impacted set, ordered by ws.Projects.
	Projects []*Project
	// UnownedRootFiles are the changed workspace-root paths that reached no
	// project, sorted and deduplicated.
	UnownedRootFiles []string
	// Trace explains every entry of Projects.
	Trace ImpactTrace
}

// TraceChangeImpact is the one change-to-project pass. ChangeImpact and
// ProjectsForChangedFiles are projections of it.
//
// It keeps the evidence the pass computes anyway: which file claimed which
// project and how, and which edge first reached each propagated project. An
// earlier version of the pass threw both away, so an operator watching `--impacted` select
// 106 of 111 projects had no surface that said why, and the `why_impacted`
// MCP tool answered from a narrower graph than the one that had selected them.
func TraceChangeImpact(ws *Workspace, changedFiles []string, opts ChangeImpactOptions) ImpactResult {
	if ws == nil || len(changedFiles) == 0 {
		return ImpactResult{}
	}

	seeds, unownedRootFiles := classifyChangedPaths(ws, changedFiles, ownershipIncludingDeclaredInputs)
	if file, pattern, ok := firstMatchingFilePattern(changedFiles, opts.WorkspaceInputPatterns); ok {
		for _, p := range ws.Projects {
			seeds[p.ID] = append(seeds[p.ID], ImpactSeed{File: filepath.ToSlash(file), Kind: ImpactSeedWorkspaceInput, Via: pattern})
		}
		// A caller that declared these patterns HAS an answer for a
		// workspace-root change, so nothing here went unclaimed. Reporting the
		// paths anyway would describe a mapping this call did not perform.
		unownedRootFiles = nil
	}

	// Propagate through dependency edges AND extension-consumer edges together.
	// The extension edges are impact-only: they exist here and nowhere else, so
	// they widen selection without touching scheduling order.
	// They widen to a consumer's tasks of that extension only.
	//
	// The seeds are enqueued in ws.Projects order, never map order: two seeds
	// that reach one project at the same depth must record the same parent on
	// every run, or the trace would name a different edge each time it is read.
	consumers := extensionConsumerIndex(ws)
	seeds, seedScopes := scopeSeedsToTasks(ws, opts.Tasks, consumers, seeds)
	directIDs := make([]string, 0, len(seeds))
	for _, p := range ws.Projects {
		if _, seeded := seeds[p.ID]; seeded {
			directIDs = append(directIDs, p.ID)
		}
	}
	affectedSet, edges, scopes := propagateImpact(ws, opts.Tasks, consumers, directIDs, seedScopes,
		movedContracts(ws, changedFiles))

	result := make([]*Project, 0)
	for _, p := range ws.Projects {
		if affectedSet[p.ID] {
			result = append(result, p)
		}
	}

	return ImpactResult{
		Projects:         result,
		UnownedRootFiles: unownedRootFiles,
		Trace:            ImpactTrace{Seeds: seeds, Edges: edges, Scopes: scopes},
	}
}

// ImpactReach is the closure a change to one project reaches, with the scope
// of every project in it: the same propagation TraceChangeImpact runs, seeded
// with from alone and fully. Empty when from is unknown.
//
// It is what `why_impacted` answers from. An earlier version of that tool walked
// DependencyGraph.DependentPath, which has no extension-consumer edges, so a
// pair whose only path crossed one was reported as "does not transitively
// impact" while `impacted` listed the very same project. Sharing the one
// propagation, rather than a second walk over the same edges, is what makes
// the two tools agree on the reach AND on the scope by construction.
func ImpactReach(ws *Workspace, from string) ImpactResult {
	return ImpactReachWithTasks(ws, from, nil)
}

// ImpactReachWithTasks is ImpactReach resolved onto tasks: the seed runs every
// task of `from`, which is what "a change to this project" means when the
// caller names a project instead of a file, and every project the walk reaches
// carries the tasks that change reaches. A change to a project as a whole
// includes its committed contract, so every contract edge the walk meets
// fires, and names no file.
func ImpactReachWithTasks(ws *Workspace, from string, index TaskImpactIndex) ImpactResult {
	if ws == nil || ws.ProjectByID(from) == nil {
		return ImpactResult{}
	}
	affected, edges, scopes := propagateImpact(ws, index, extensionConsumerIndex(ws), []string{from}, nil,
		contractMoves{every: true})
	result := make([]*Project, 0, len(affected))
	for _, p := range ws.Projects {
		if affected[p.ID] {
			result = append(result, p)
		}
	}
	return ImpactResult{
		Projects: result,
		Trace:    ImpactTrace{Seeds: map[string][]ImpactSeed{from: nil}, Edges: edges, Scopes: scopes},
	}
}

// ImpactPath returns the path ImpactReach's trace records from one project to
// another, with each hop's kind. [{from, ""}] when from == to; nil when either
// project is unknown or no path exists.
func ImpactPath(ws *Workspace, from, to string) []ImpactStep {
	return ImpactPathWithTasks(ws, from, to, nil)
}

// ImpactPathWithTasks is ImpactPath over the task-level reach.
func ImpactPathWithTasks(ws *Workspace, from, to string, index TaskImpactIndex) []ImpactStep {
	if ws == nil || ws.ProjectByID(to) == nil {
		return nil
	}
	return ImpactReachWithTasks(ws, from, index).Trace.PathTo(to)
}

// impactScope is one project's place in the closure: full means every task of
// every command runs, tasks the set that does. A scope only ever grows and full
// is its top, which is what makes the walk below a terminating fixpoint rather
// than a breadth-first pass that has to get the order right.
type impactScope struct {
	full  bool
	tasks map[string]bool
	// toolTasks are the tasks an extension-consumer edge added: the project
	// runs them because the tool that executes them was rebuilt, not because
	// anything the project reads changed. They are part of its scope and
	// nothing propagates out of them — the approximation ADR 0042 names.
	toolTasks map[string]bool
}

// mark identifies the size of what a scope PROPAGATES: -1 for full, the task
// count otherwise, and 0 for a project no edge has reached that way. Because a
// scope only grows, a mark that repeats means nothing new to propagate.
func (s *impactScope) mark() int {
	if s == nil {
		return 0
	}
	if s.full {
		return -1
	}
	return len(s.tasks)
}

// selected reports whether the project is in the impacted set at all, tool
// tasks included.
func (s *impactScope) selected() bool {
	return s != nil && (s.full || len(s.tasks) > 0 || len(s.toolTasks) > 0)
}

// source is the task list to propagate out of this project: nil for a full
// project, which TaskImpactIndex reads as "every task".
func (s *impactScope) source() []string {
	if s == nil || s.full {
		return nil
	}
	return sortedKeys(s.tasks)
}

// scope is everything the project runs: what it propagates plus what a
// rebuilt tool runs on it.
func (s *impactScope) scope() []string {
	if s == nil || s.full {
		return nil
	}
	merged := make(map[string]bool, len(s.tasks)+len(s.toolTasks))
	for task := range s.tasks {
		merged[task] = true
	}
	for task := range s.toolTasks {
		merged[task] = true
	}
	return sortedKeys(merged)
}

func (s *impactScope) makeFull() {
	s.full = true
	s.tasks = nil
	s.toolTasks = nil
}

func (s *impactScope) add(tasks []string) {
	if s.full {
		return
	}
	if s.tasks == nil {
		s.tasks = make(map[string]bool, len(tasks))
	}
	for _, task := range tasks {
		if task != "" {
			s.tasks[task] = true
		}
	}
}

func (s *impactScope) addTool(tasks []string) {
	if s.full {
		return
	}
	if s.toolTasks == nil {
		s.toolTasks = make(map[string]bool, len(tasks))
	}
	for _, task := range tasks {
		if task != "" {
			s.toolTasks[task] = true
		}
	}
}

func sortedKeys(set map[string]bool) []string {
	list := make([]string, 0, len(set))
	for key := range set {
		list = append(list, key)
	}
	sort.Strings(list)
	return list
}

// scopeSeedsToTasks resolves each changed file's claim on a project onto the
// TASKS of that project which read the file, and drops the claims no task
// reads.
//
// The claim kinds are not equivalent, and which ones the index may narrow is
// the whole decision:
//
//   - path-owner and declared-input NAME A FILE that some task either declares
//     as an input or does not. The index's answer is authoritative, empty
//     included: a `.md` no task reads claims nothing, and its project is not
//     selected.
//   - root-watched-file is a provider's project-level answer over a
//     workspace-root entry. The index narrows it when it can attribute the file
//     to tasks — a `from: "workspace"` input declares exactly these paths — and
//     the project stays full when it cannot, because the provider claimed the
//     file for a reason it did not have to state per task.
//   - scope-config and workspace-input keep the project full. A scope's
//     putnami.json decides which extensions apply and which tags fire, so it
//     changes which tasks EXIST rather than what one of them reads; a
//     workspace-input pattern is the caller's own coarse claim over every
//     project.
//
// An in-workspace extension project is the last full case, and for the same
// reason. Its putnami.extension.json IS the declaration every consumer's jobs
// are planned from, so no task can be said to read it; and a file of it no task
// declares may be the RUNTIME its consumers execute — a shell entrypoint, a
// bundled script — which is not the extension's own input but is every
// consumer's tool. Both keep the extension whole, exactly as an implementation
// change does. A file of the extension some task does read is attributed
// normally, so a `_test.go` inside it still re-runs its tests alone.
func scopeSeedsToTasks(
	ws *Workspace,
	index TaskImpactIndex,
	consumers map[string][]string,
	seeds map[string][]ImpactSeed,
) (map[string][]ImpactSeed, map[string]*impactScope) {
	if index == nil {
		return seeds, nil
	}
	kept := make(map[string][]ImpactSeed, len(seeds))
	scopes := make(map[string]*impactScope, len(seeds))
	for _, p := range ws.Projects {
		claims, seeded := seeds[p.ID]
		if !seeded {
			continue
		}
		scope := &impactScope{}
		var keptClaims []ImpactSeed
		for _, claim := range claims {
			if !claimTasks(index, consumers, p, claim, scope) {
				continue
			}
			keptClaims = append(keptClaims, claim)
		}
		if len(keptClaims) == 0 {
			continue
		}
		kept[p.ID] = keptClaims
		scopes[p.ID] = scope
	}
	return kept, scopes
}

// claimTasks merges one claim into a project's seed scope and reports whether
// the claim reached anything. The kind decides whether an index answer of
// "nothing" means "no action reads this" or "this claim is not a task
// declaration"; scopeSeedsToTasks states which is which.
func claimTasks(
	index TaskImpactIndex,
	consumers map[string][]string,
	p *Project,
	claim ImpactSeed,
	scope *impactScope,
) bool {
	if claim.Kind == ImpactSeedScopeConfig || claim.Kind == ImpactSeedWorkspaceInput ||
		isExtensionManifestOf(p, claim.File) || index.ReadsAsExtensionRuntime(p.ID, claim.File) {
		scope.makeFull()
		return true
	}
	if tasks := index.TasksReadingPath(p.ID, claim.File); len(tasks) > 0 {
		scope.add(tasks)
		return true
	}
	if claim.Kind != ImpactSeedRootWatchedFile && !isExtensionProject(p, consumers) {
		return false
	}
	scope.makeFull()
	return true
}

// isExtensionProject reports whether a project is an in-workspace extension
// SOME project or the workspace document names in its `extensions` list. The
// consumer index's keys are exactly those projects, so the relation is read off
// the index the walk already built.
//
// It is deliberately not the answer to "does this project carry an extension
// manifest": an extension delivered as an ordinary package DEPENDENCY — the
// hook-only TypeScript extensions are the shape — is named in no `extensions`
// list and is absent here. isExtensionManifestOf is the one caller that must
// not inherit that blind spot.
func isExtensionProject(p *Project, consumers map[string][]string) bool {
	_, isExtension := consumers[p.ID]
	return isExtension
}

// isExtensionManifestOf reports whether a changed path is a project's own
// extension manifest.
//
// It asks the FILE, not the consumer index. A putnami.extension.json is the
// declaration every consumer's jobs are planned from — its hooks, its tasks,
// its cache contracts — so no task of the declaring project can be said to read
// it and the project keeps its whole self. Deriving "is an extension" from the
// consumer index here would miss every extension a consumer reaches as a
// package dependency rather than through an `extensions` entry: changing such a
// manifest's preBuild hook would then select the declaring project alone and no
// project that RUNS the hook, which is a gate that never ran.
func isExtensionManifestOf(p *Project, file string) bool {
	return filepath.Base(file) == extproto.ManifestFilename &&
		cleanIndexPath(filepath.Dir(file)) == cleanIndexPath(p.Path)
}

// propagateImpact returns the closure of seed project IDs under the union of
// three reverse-edge families — the dependency graph's impact dependents (its
// dependents minus the implicit include→scope-self ordering edges), the
// derived contract edges from a provider to the generated clients of its
// contract, and the impact-only extension-consumer edges from
// extensionConsumerIndex — with every project in the closure carrying the TASKS
// of it the change reaches.
//
// Selection is task-level (ADR 0044). A seed carries the tasks that read the
// changed file; a dependency or contract edge carries into the dependent the
// tasks whose `^` step reference names one of the tasks the source runs, and
// carries nothing when none does. A seeded `test` therefore reaches no
// dependent — no manifest declares `^test` — while a seeded `describe` reaches
// every importer's `describe` and what sits behind it. FULL — every task of
// every command runs — is a SEED state only: it is what a seed whose claim the
// index could not attribute gets, and such a project carries across its edges
// what every one of its tasks would carry. Full dominates, and a project
// reached twice takes the UNION of what reached it.
//
// A contract edge fires only out of a provider listed in moved, the providers
// whose committed contract is one of the changed files (ADR 0054): the
// contract is the client's whole provider-side input, so an implementation
// change of the provider reaches no client. moved.every fires every contract
// edge with no evidence, which is ImpactReach's question about a change to a
// project as a whole; the zero value fires none.
//
// The extension-consumer edge fires from an extension whose binary the change
// rebuilt, which is the question `^` already answers: it fires when the
// extension is full, or when the tasks it runs reach a dependent's task at all.
// A `_test.go` inside an extension therefore re-runs that extension's tests and
// nothing else in the workspace, while one byte of its implementation or its
// manifest re-runs that extension's tasks on every consumer. Nothing propagates
// out of the tasks that edge added: the consumer's own sources did not change,
// which is the approximation ADR 0042 names and the cache key catches.
//
// Without a TaskImpactIndex the walk is the project-level one it was before ADR
// 0044: a seed is full, a dependency or contract edge makes a dependent full,
// and an extension-consumer edge scopes a consumer to the extension project IDs
// that reached it.
//
// The second result names, for every non-seed project in the closure, the edge
// that reached it; the third lists, for every task-scoped project, its sorted
// tasks. The walk is a fixpoint over a queue seeded in ws.Projects order and
// grown in graph order, and a scope only grows, so both are deterministic.
func propagateImpact(
	ws *Workspace,
	index TaskImpactIndex,
	consumers map[string][]string,
	seedIDs []string,
	seedScopes map[string]*impactScope,
	moved contractMoves,
) (map[string]bool, map[string]ImpactEdge, map[string][]string) {
	w := impactWalk{
		ws:         ws,
		index:      index,
		consumers:  consumers,
		moved:      moved,
		seed:       make(map[string]bool, len(seedIDs)),
		state:      make(map[string]*impactScope, len(seedIDs)),
		edges:      make(map[string]ImpactEdge),
		propagated: make(map[string]int),
		queue:      append([]string(nil), seedIDs...),
	}
	for _, id := range seedIDs {
		w.seed[id] = true
		if scope := seedScopes[id]; scope != nil {
			w.state[id] = scope
			continue
		}
		w.state[id] = &impactScope{full: true}
	}
	w.run()

	affected := make(map[string]bool, len(w.state))
	scopes := make(map[string][]string, len(w.state))
	for id, scope := range w.state {
		if !scope.selected() {
			continue
		}
		affected[id] = true
		if !scope.full {
			scopes[id] = scope.scope()
		}
	}
	return affected, w.edges, scopes
}

// impactWalk is propagateImpact's working state. It is a type rather than a
// stack of closures because the queue, the scopes and the edges are read and
// written by every edge family, and a closure capturing all three reads as a
// hidden parameter list.
type impactWalk struct {
	ws        *Workspace
	index     TaskImpactIndex
	consumers map[string][]string
	// moved gates the contract edge; propagateImpact states the rule.
	moved      contractMoves
	seed       map[string]bool
	state      map[string]*impactScope
	edges      map[string]ImpactEdge
	propagated map[string]int
	queue      []string
}

func (w *impactWalk) run() {
	for len(w.queue) > 0 {
		current := w.queue[0]
		w.queue = w.queue[1:]
		scope := w.state[current]
		mark := scope.mark()
		if last, walked := w.propagated[current]; walked && last == mark {
			continue
		}
		w.propagated[current] = mark
		carried, carries := w.carried(scope)
		if carries {
			// Dependency edges are walked before contract edges so a project
			// both families reach names the dependency edge, the stronger
			// reason: it reads the changed project's own bytes, while a
			// contract edge reads only the contract.
			for _, next := range w.ws.Graph.ImpactDependentsOf(current) {
				w.reach(next, ImpactEdge{From: current, Kind: ImpactEdgeDependency}, carried)
			}
			if edge, fires := w.contractEdgeFrom(current); fires {
				for _, next := range w.ws.Graph.ContractClientsOf(current) {
					w.reach(next, edge, carried)
				}
			}
		}
		// An extension whose whole self is impacted rebuilt the binary its
		// consumers run, whatever its manifest declares about ordering; one
		// whose tasks carry nothing across an edge rebuilt nothing.
		if !scope.full && !carries {
			continue
		}
		for _, next := range w.consumers[current] {
			w.reachConsumer(current, next)
		}
	}
}

// carried answers what one project hands across an edge: the tasks a dependent
// runs because the tasks this project runs moved. The second result is false
// when the project carries NOTHING — a seeded `test` is the case that matters —
// and the walk then crosses no edge out of it at all.
//
// Without an index the project-level rule stands: every impacted project
// carries its whole self across every edge.
func (w *impactWalk) carried(scope *impactScope) ([]string, bool) {
	if w.index == nil {
		return nil, true
	}
	reached := w.index.TasksReachedFrom(scope.source())
	return reached, len(reached) > 0
}

// contractEdgeFrom returns the edge a provider's contract edges record, and
// whether they fire at all: always under moved.every, and otherwise only when
// the provider's committed contract is one of the changed files, in which case
// the edge names that file and its digest.
func (w *impactWalk) contractEdgeFrom(provider string) (ImpactEdge, bool) {
	edge := ImpactEdge{From: provider, Kind: ImpactEdgeContract}
	if w.moved.every {
		return edge, true
	}
	move, moved := w.moved.byProvider[provider]
	if !moved {
		return ImpactEdge{}, false
	}
	edge.Via, edge.ContractSHA256 = move.path, move.sha256
	return edge, true
}

// reach merges what a dependency or contract edge carries into a project and
// enqueues it when that changed what the project propagates. The edge is
// recorded on the first reach and rewritten when the project is upgraded to
// full, so a project that runs everything names the reason it does.
func (w *impactWalk) reach(to string, edge ImpactEdge, tasks []string) {
	if to == edge.From {
		return
	}
	scope := w.scopeOf(to)
	before := scope.mark()
	wasSelected := scope.selected()
	if w.index == nil {
		scope.makeFull()
	} else {
		scope.add(tasks)
	}
	after := scope.mark()
	if after == before {
		return
	}
	if !w.seed[to] && (!wasSelected || after == -1) {
		w.edges[to] = edge
	}
	w.queue = append(w.queue, to)
}

// contractMove is one provider's moved contract: the committed file that
// changed, and its digest.
type contractMove struct {
	path   string
	sha256 string
}

// contractMoves is the contract-edge gate of one walk. Its zero value fires no
// contract edge, so a caller that omits it cannot reopen the fan-out ADR 0054
// closed. Only a caller asking about a change to a project as a whole sets
// every.
type contractMoves struct {
	every      bool
	byProvider map[string]contractMove
}

// movedContracts maps each provider whose committed contract is one of the
// changed files to that file and its digest. Each changed path is cleaned the
// way classifyChangedPaths cleans it, so a contract matches in any spelling
// the diff or the watcher produced.
func movedContracts(ws *Workspace, changedFiles []string) contractMoves {
	moved := make(map[string]contractMove)
	changed := make(map[string]bool, len(changedFiles))
	for _, file := range changedFiles {
		changed[filepath.ToSlash(cleanOwnerLookupPath(ws, file))] = true
	}
	for _, p := range ws.Projects {
		if p.ContractPath == "" {
			continue
		}
		contract := path.Clean(filepath.ToSlash(p.ContractPath))
		if changed[contract] {
			moved[p.ID] = contractMove{path: contract, sha256: p.ContractSHA256}
		}
	}
	return contractMoves{byProvider: moved}
}

// reachConsumer applies the extension-consumer edge: the consumer runs the
// tasks of the rebuilt extension and stays where it is. Nothing is enqueued —
// those tasks propagate nothing.
func (w *impactWalk) reachConsumer(extensionID, consumerID string) {
	if consumerID == extensionID {
		return
	}
	scope := w.scopeOf(consumerID)
	if scope.full {
		return
	}
	if !scope.selected() && !w.seed[consumerID] {
		w.edges[consumerID] = ImpactEdge{From: extensionID, Kind: ImpactEdgeExtensionConsumer}
	}
	if w.index == nil {
		// The project-level scope: the extension project's own ID.
		scope.addTool([]string{extensionID})
		return
	}
	scope.addTool(w.index.TasksOfExtension(extensionID))
}

func (w *impactWalk) scopeOf(id string) *impactScope {
	scope := w.state[id]
	if scope == nil {
		scope = &impactScope{}
		w.state[id] = scope
	}
	return scope
}

// extensionConsumerIndex maps each in-workspace extension project's ID to the
// IDs of the projects that effectively run under it. These are IMPACT-ONLY
// edges: every consuming project's jobs run the tasks the
// extension declares, so a change to the extension must select its consumers'
// tasks of it even though no project declares a build dependency on the
// extension (propagateImpact scopes what the edge selects) — but the
// edges are deliberately NOT added to the scheduling graph, where they would
// reorder runs and create cycles (the workspace-level extension list includes
// the extension projects themselves).
//
// Effective extensions are resolved exactly as discovery understands them:
// workspace-level `extensions` entries apply to every project; a project's own
// `extensions` list adds to that. Entries resolve by project ID (`/go/...`),
// workspace-relative path (`./extensions/local`), or name (`@putnami/go`);
// entries that resolve to no workspace project (registry-installed extensions
// such as `@putnami/cloud`) contribute nothing, because a git diff cannot
// change them.
func extensionConsumerIndex(ws *Workspace) map[string][]string {
	resolve := func(entry string) string {
		var p *Project
		if strings.HasPrefix(entry, "/") {
			p = ws.ProjectByID(entry)
		} else {
			if path, ok := normalizeWorkspaceExtensionPath(entry); ok {
				p = ws.ProjectByPath(path)
			}
			if p == nil {
				p = ws.ProjectByName(entry)
			}
		}
		if p == nil {
			return ""
		}
		return p.ID
	}

	consumers := make(map[string][]string)
	add := func(extID, consumerID string) {
		if extID == "" || extID == consumerID {
			return
		}
		if !containsString(consumers[extID], consumerID) {
			consumers[extID] = append(consumers[extID], consumerID)
		}
	}

	var workspaceExtIDs []string
	if ws.Config != nil {
		for _, name := range ws.Config.Extensions.Names() {
			if id := resolve(name); id != "" {
				workspaceExtIDs = append(workspaceExtIDs, id)
			}
		}
	}
	for _, p := range ws.Projects {
		for _, extID := range workspaceExtIDs {
			add(extID, p.ID)
		}
		for _, entry := range p.Extensions {
			add(resolve(entry), p.ID)
		}
	}
	return consumers
}

// normalizeWorkspaceExtensionPath mirrors extension discovery's workspace-path
// probe. Traversal and a ref that names a volume (a Windows drive or UNC share)
// are rejected, while "./" and transparent physical project paths are cleaned
// before ProjectByPath performs the lookup.
func normalizeWorkspaceExtensionPath(ref string) (string, bool) {
	if filepath.VolumeName(ref) != "" {
		return "", false
	}
	rel := strings.TrimLeft(ref, `/\`)
	if rel == "" {
		return "", false
	}
	rel = filepath.FromSlash(strings.ReplaceAll(rel, `\`, `/`))
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// firstMatchingFilePattern returns the first file that matches any pattern,
// and the pattern that matched it. The three probes are moved VERBATIM from the
// deleted watch classifier, tolerant `**` handling included, because
// narrowing them would silently stop watch from re-running on lockfile edits it
// used to catch. The match is named rather than reported as a bool so the
// workspace-input seed can say which file and which pattern widened the
// selection to every project.
//
// The probes read the file in slash form, whatever separator the caller used,
// and match with path.Match, so `*` never crosses a `/` on any host.
func firstMatchingFilePattern(files, patterns []string) (file, pattern string, ok bool) {
	if len(patterns) == 0 {
		return "", "", false
	}
	for _, file := range files {
		slashFile := filepath.ToSlash(file)
		for _, pattern := range patterns {
			if matched, _ := path.Match(pattern, slashFile); matched {
				return file, pattern, true
			}
			// Also try matching with ** prefix stripped for recursive globs.
			if strings.HasPrefix(pattern, "**/") {
				subPattern := pattern[3:]
				if matched, _ := path.Match(subPattern, path.Base(slashFile)); matched {
					return file, pattern, true
				}
			}
			// Simple suffix match for patterns like "**/*.go".
			if strings.HasPrefix(pattern, "**") {
				suffix := strings.TrimPrefix(pattern, "**")
				if strings.HasSuffix(slashFile, suffix) {
					return file, pattern, true
				}
			}
		}
	}
	return "", "", false
}

// ProjectOwnersForPath returns the projects that own a workspace-relative path:
// the nearest project whose directory holds the path, plus every project that
// claims it as an asset. It applies the same rules as
// ImpactedProjectsWithBaseline before graph propagation.
func ProjectOwnersForPath(ws *Workspace, path string) []*Project {
	if ws == nil {
		return nil
	}
	ids := directlyAffectedProjectIDs(ws, []string{path})
	out := make([]*Project, 0, len(ids))
	for _, p := range ws.Projects {
		if ids[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// projectOwnerIndex answers which projects own a workspace-relative path by
// directory and by asset claim.
type projectOwnerIndex struct {
	// dirs maps a project's cleaned directory path to the IDs registered at
	// exactly that directory. A path is owned by the DEEPEST registered
	// ancestor only; a root project (".") is therefore the owner of last resort.
	dirs map[string][]string
	// assets maps a cross-project asset source (file or directory, workspace-
	// relative) to the projects that declared it. Asset claims are additive:
	// every claimant whose source covers the path is selected on top of the
	// directory owner, and a deeper asset claim never displaces that owner.
	assets map[string][]string
}

func directlyAffectedProjectIDs(ws *Workspace, paths []string) map[string]bool {
	seeds, _ := classifyChangedPaths(ws, paths, ownershipByPath)
	affected := make(map[string]bool, len(seeds))
	for id := range seeds {
		affected[id] = true
	}
	return affected
}

// pathOwnership selects which claims over a changed path classifyChangedPaths
// honors.
//
// The two are not interchangeable, which is why the caller has to say. Owning a
// file and reading it are different relations: the project whose directory
// holds a file is where an author edits it, while a project that declared the
// file as a task input merely has to re-run when it changes. `find_owner` and
// the change plan's direct-project list want the first — answering "@putnami/cli"
// for every putnami.json in the workspace would be a worse answer than none.
// Impact selection wants both, because a project that must re-run is affected
// however it came to read the file.
type pathOwnership int

const (
	// ownershipByPath counts only the claims that make a project a path's
	// owner: its own directory, its cross-project assets, and a provider's
	// workspace-root watchedFiles answer.
	ownershipByPath pathOwnership = iota
	// ownershipIncludingDeclaredInputs adds every project that READS the path
	// without holding it: one that declared it as a file input of one of its
	// commands, and one that inherits it as a scope config.
	ownershipIncludingDeclaredInputs
)

// classifyChangedPaths maps changed paths onto their owning projects and, in
// the same pass, collects the ones that reached NOBODY while sitting directly
// at the workspace root.
//
// The second return value is not a diagnostic afterthought: an unowned root
// path is the only way a non-empty diff can select an empty project set, and
// core cannot narrow it further. Such a file belongs to no project by PATH, so
// core next consults the per-project watchedFiles paths providers already
// answered over the workspace probe. The provider owns the vocabulary and the
// mapping; core performs only exact path matching. A root path no provider
// claims is reported so the caller can say the remaining uncertainty out loud.
//
// Only root-level paths are collected. A changed file inside a directory that
// no project claims (a repository's CI config, a top-level doc folder) is
// ordinarily unowned and says nothing about the workspace's own inputs, so
// naming it would bury the case that matters in a list nobody reads.
func classifyChangedPaths(ws *Workspace, paths []string, ownership pathOwnership) (map[string][]ImpactSeed, []string) {
	idx := buildProjectOwnerIndex(ws)
	rootFileOwners := buildProviderRootWatchedFileOwnerIndex(ws)
	var declaredInputs declaredInputIndex
	var scopeConfigs scopeConfigIndex
	if ownership == ownershipIncludingDeclaredInputs {
		declaredInputs = buildDeclaredInputIndex(ws)
		scopeConfigs = buildScopeConfigIndex(ws)
	}
	seeds := make(map[string][]ImpactSeed)
	seenUnowned := make(map[string]bool)
	var unownedRoot []string
	for _, file := range paths {
		lookup := cleanOwnerLookupPath(ws, file)
		claimed := false
		claim := func(id string, kind ImpactEdgeKind, via string) {
			claimed = true
			seeds[id] = append(seeds[id], ImpactSeed{File: filepath.ToSlash(lookup), Kind: kind, Via: via})
		}
		for _, owner := range idx.ownersForPath(lookup) {
			claim(owner.ID, ImpactSeedPathOwner, filepath.ToSlash(owner.Via))
		}
		if isWorkspaceRootPath(lookup) {
			entry := filepath.ToSlash(lookup)
			for _, id := range rootFileOwners[entry] {
				claim(id, ImpactSeedRootWatchedFile, entry)
			}
		}
		// A declared input claims a path wherever it sits, root or not. A root
		// path it claims is therefore no longer unowned: some project answered
		// for it, and the answer is "I read this file".
		for _, reader := range declaredInputs.readersForPath(lookup) {
			claim(reader.ID, ImpactSeedDeclaredInput, reader.Via)
		}
		// A scope config claims every project that inherits from it, the same
		// way: the project reads the file through the scope chain, and is no
		// more its owner than a declared reader is.
		for _, inheritor := range scopeConfigs.inheritorsOfPath(lookup) {
			claim(inheritor.ID, ImpactSeedScopeConfig, filepath.ToSlash(inheritor.Via))
		}
		if claimed || !isWorkspaceRootPath(lookup) || seenUnowned[lookup] {
			continue
		}
		seenUnowned[lookup] = true
		unownedRoot = append(unownedRoot, lookup)
	}
	sort.Strings(unownedRoot)
	return seeds, unownedRoot
}

// declaredInputIndex answers which projects declared a changed path as a file
// input of one of their commands — `options.<layer>.filePatterns` in their
// putnami.json, the same declaration the build cache hashes.
//
// It exists because the two readings of that one declaration had drifted:
// the cache keyed on the declared files while selection ignored them,
// so a change to a file a project declares as a test input landed with that
// project's tests unrun under `--impacted`. `@putnami/cli` declares the
// agent-workflow sources and owns the only drift gate over them, so every skill
// change shipped with its gate unrun on CI.
//
// Patterns stay in the project-relative form they are authored in, and each
// changed path is made relative to the declaring project before matching, which
// is exactly the coordinate system the cache hasher resolves them in
// (`filepath.Join(ws.Root, project.Path)` as the pattern root). The matcher is
// the protocol's, for the same reason: one declaration, one grammar.
//
// Patterns that stay inside the declaring project are included rather than
// filtered out, and they widen nothing: path ownership already selects a
// project for every file under its own directory. That also settles the only
// way this could have narrowed a selection — a declared exclusion such as
// "!src/gen/**" makes the project not a READER of that file, never not its
// owner, so the project is selected either way.
type declaredInputIndex struct {
	// projects lists only the projects that declared at least one file input,
	// in ws.Projects order, so the answer for a path is deterministic.
	projects []declaredInputProject
}

type declaredInputProject struct {
	id string
	// path is the project's workspace-relative root, cleaned, which declared
	// patterns are relative to.
	path string
	// patterns is one entry per option layer that declared filePatterns, each
	// as authored. They are kept apart rather than concatenated because a cache
	// key only ever groups the layers that apply to one job; see
	// declaredFilePatterns.
	patterns [][]string
}

func buildDeclaredInputIndex(ws *Workspace) declaredInputIndex {
	var idx declaredInputIndex
	if ws == nil {
		return idx
	}
	for _, p := range ws.Projects {
		patterns := declaredFilePatterns(p)
		if len(patterns) == 0 {
			continue
		}
		root := filepath.Clean(p.Path)
		if root == "" {
			root = "."
		}
		idx.projects = append(idx.projects, declaredInputProject{id: p.ID, path: root, patterns: patterns})
	}
	return idx
}

// declaredFilePatterns reads each option layer's `filePatterns` as its OWN
// pattern set, ordered by layer key so the answer is stable across runs.
//
// Every layer counts, not the ones that apply to one command: selection is per
// project, and a project whose `test` inputs changed has to be selected by a
// run of any command — the plan then decides which of its jobs to schedule.
// Reading all layers is also what makes the rule hold for `lint`, `build` and
// `validate` inputs, and for an extension-keyed layer such as
// `options."/typescript/extension".filePatterns`, without this function having
// to resolve which extension a job would run under.
//
// The layers stay SEPARATE, and that is the whole point of returning a list of
// lists. A cache key concatenates only the layers that apply to its own job —
// `options.{cmd}`, `options.{ext}` and `options.{ext}:{cmd}`, never another
// command's layer — so `options.lint` declaring "!../../shared/**" says nothing
// about what a `test~test` key reads. Flattening all layers into one set would
// let that exclusion cancel `options.test`'s "../../shared/**", and the project
// would go unselected for a change its test key does read: the same bug, one
// layer down.
func declaredFilePatterns(p *Project) [][]string {
	if p == nil || p.Config == nil || len(p.Config.Options) == 0 {
		return nil
	}
	layers := make([]string, 0, len(p.Config.Options))
	for layer := range p.Config.Options {
		layers = append(layers, layer)
	}
	sort.Strings(layers)
	var sets [][]string
	for _, layer := range layers {
		raw, ok := p.Config.Options[layer]["filePatterns"]
		if !ok {
			continue
		}
		values, ok := raw.([]any)
		if !ok {
			continue
		}
		patterns := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok && s != "" {
				patterns = append(patterns, s)
			}
		}
		if len(patterns) > 0 {
			sets = append(sets, patterns)
		}
	}
	return sets
}

// inputClaim is one project's declared-input claim on a path: the project, and
// the pattern set that selected the path, joined by one space. The whole set is
// named rather than the one include that matched because wsproto.SelectsPath
// answers per set, and an exclusion in the set can be what decides.
type inputClaim struct {
	ID  string
	Via string
}

// readersForPath returns the projects whose declared inputs select this
// workspace-relative path, each with the layer that selected it.
//
// One matching layer is enough. Selection is deliberately at least as wide as
// every key it stands in for: a real key groups the layers that apply to its
// job and lets any of their exclusions bite, so evaluating each layer alone
// applies strictly weaker exclusions and can never select less than the key
// reads. Erring wide costs a job that had nothing to do; erring narrow is a
// gate that never ran.
func (idx declaredInputIndex) readersForPath(path string) []inputClaim {
	if len(idx.projects) == 0 {
		return nil
	}
	var claims []inputClaim
	for _, p := range idx.projects {
		rel, err := filepath.Rel(p.path, path)
		if err != nil {
			continue
		}
		for _, patterns := range p.patterns {
			if wsproto.SelectsPath(rel, patterns) {
				claims = append(claims, inputClaim{ID: p.id, Via: strings.Join(patterns, " ")})
				break
			}
		}
	}
	return claims
}

// scopeConfigIndex answers which projects inherit from a scope's putnami.json —
// the files LoadScopeChain merged into each project's tags, extensions and name
// pattern, recorded on Project.Scope.ConfigPaths at discovery.
//
// It exists because the inheritance is a real input relation that nothing
// declared. A scope's tags decide which of a child's jobs fire and feed
// its cache keys, its extensions decide which providers contribute jobs at all,
// and its name pattern decides the child's identity: one line in the scope
// file reconfigures every include, and until this index a diff touching only
// that line selected the scope-self (when the scope is activated) or nothing
// at all. Before this index, an activated scope hid the gap by accident, through the
// implicit include→scope-self edge that ADR 0038 made ordering-only; this is
// the relation that edge was being mistaken for.
//
// A child cannot say this itself: a `filePatterns` entry would have to name a
// file above its own directory and repeat the scope layout in every include.
// The scope chain is resolved once per project anyway, so the sources of that
// resolution are the declaration. The scope-self, when there is one, stays
// the path's directory owner and is seeded as such by ownersForPath; this
// index adds readers, never owners, so `find_owner` keeps answering the
// scope-self alone.
type scopeConfigIndex struct {
	// inheritors maps a scope config's cleaned workspace-relative path to the
	// IDs of the projects whose chain merged it, in ws.Projects order.
	inheritors map[string][]string
}

func buildScopeConfigIndex(ws *Workspace) scopeConfigIndex {
	idx := scopeConfigIndex{inheritors: make(map[string][]string)}
	if ws == nil {
		return idx
	}
	for _, p := range ws.Projects {
		for _, source := range p.Scope.ConfigPaths {
			key := cleanIndexPath(filepath.FromSlash(source))
			if !containsString(idx.inheritors[key], p.ID) {
				idx.inheritors[key] = append(idx.inheritors[key], p.ID)
			}
		}
	}
	return idx
}

// inheritorsOfPath returns the projects that inherit from this workspace-
// relative path, each naming the scope directory the file configures.
func (idx scopeConfigIndex) inheritorsOfPath(path string) []inputClaim {
	ids := idx.inheritors[cleanIndexPath(path)]
	if len(ids) == 0 {
		return nil
	}
	via := cleanIndexPath(filepath.Dir(path))
	claims := make([]inputClaim, 0, len(ids))
	for _, id := range ids {
		claims = append(claims, inputClaim{ID: id, Via: via})
	}
	return claims
}

// buildProviderRootWatchedFileOwnerIndex projects the workspace-root entries in
// providers' merged per-project watchedFiles answers onto core's project IDs.
// It deliberately performs no filename inference: a path absent from the
// provider view has no special meaning here, regardless of extension or
// language. The view is copied under its probe lock before it is traversed, so
// watch and planning can share the workspace safely.
func buildProviderRootWatchedFileOwnerIndex(ws *Workspace) map[string][]string {
	idx := make(map[string][]string)
	if ws == nil {
		return idx
	}
	view := ws.currentProbeView()
	for _, project := range ws.Projects {
		merged, ok := view[ProbePathOf(project.Path)]
		if !ok {
			continue
		}
		for _, rootFile := range merged.WatchedFiles {
			cleaned, ok := wsproto.NormalizeProbePath(rootFile)
			if !ok || cleaned == wsproto.ProbeRootPath || strings.Contains(cleaned, "/") {
				continue
			}
			if !containsString(idx[cleaned], project.ID) {
				idx[cleaned] = append(idx[cleaned], project.ID)
			}
		}
	}
	return idx
}

// isWorkspaceRootPath reports whether a workspace-relative path names an entry
// sitting directly at the workspace root rather than inside any directory.
func isWorkspaceRootPath(rel string) bool {
	if rel == "" || rel == "." {
		return false
	}
	return filepath.Dir(rel) == "."
}

func buildProjectOwnerIndex(ws *Workspace) projectOwnerIndex {
	idx := projectOwnerIndex{dirs: make(map[string][]string), assets: make(map[string][]string)}
	if ws == nil {
		return idx
	}
	assetPaths := CollectProjectAssetPaths(ws)
	for _, p := range ws.Projects {
		idx.addDir(p.ID, p.Path)
		for _, ap := range assetPaths[p.ID] {
			idx.addAsset(p.ID, ap)
		}
	}
	return idx
}

func (idx *projectOwnerIndex) addDir(projectID, path string) {
	key := cleanIndexPath(path)
	idx.dirs[key] = append(idx.dirs[key], projectID)
}

func (idx *projectOwnerIndex) addAsset(projectID, path string) {
	key := cleanIndexPath(path)
	idx.assets[key] = append(idx.assets[key], projectID)
}

// cleanIndexPath maps "" and "." to "." so a root project and a root-level
// asset both sit at the top of the walk.
func cleanIndexPath(path string) string {
	cleaned := filepath.Clean(path)
	if cleaned == "" {
		return "."
	}
	return cleaned
}

// pathClaim is one project's path-ownership claim on a file: the project, and
// the directory the claim rests on — the project directory that owns the file,
// "." for a root project that owned it by last resort, or the asset path a
// claimant declared.
type pathClaim struct {
	ID  string
	Via string
}

// ownersForPath returns the nearest directory owner plus every asset claimant,
// each naming the directory that makes it an owner. The walk runs from the path
// up to ".", takes the FIRST directory that holds a project and no other, and
// collects asset claims at every level. "." is visited last, so a project
// rooted at the workspace owns only what no deeper project claims.
//
// An earlier version of the walk attributed a path to EVERY ancestor project, so a file
// inside a nested project also seeded its enclosing project (and, for an
// activated scope, the scope-self on every child edit). An enclosing project
// that reads a nested subtree says so through a declared dependency or a
// declared file input, both of which selection honors elsewhere — and both of
// which seed it with their own claim kind, so the trace still names the reason.
func (idx projectOwnerIndex) ownersForPath(path string) []pathClaim {
	seen := make(map[string]bool)
	var claims []pathClaim
	add := func(id, via string) {
		if !seen[id] {
			seen[id] = true
			claims = append(claims, pathClaim{ID: id, Via: via})
		}
	}
	dir := cleanIndexPath(path)
	owned := false
	for {
		if !owned {
			if matched, ok := idx.dirs[dir]; ok {
				for _, id := range matched {
					add(id, dir)
				}
				owned = true
			}
		}
		for _, id := range idx.assets[dir] {
			add(id, dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return claims
		}
		dir = parent
	}
}

func cleanOwnerLookupPath(ws *Workspace, path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return "."
	}
	if filepath.IsAbs(p) && ws != nil && ws.Root != "" {
		if rel, ok := workspaceRelativePath(ws.Root, p); ok {
			p = rel
		}
	}
	p = filepath.Clean(p)
	if p == "." {
		return "."
	}
	return strings.TrimPrefix(p, "."+string(filepath.Separator))
}

func workspaceRelativePath(root, path string) (string, bool) {
	root = CanonicalRoot(root)
	for _, candidate := range []string{filepath.Clean(path), canonicalExistingPrefix(path)} {
		if candidate == "" {
			continue
		}
		rel, err := filepath.Rel(root, candidate)
		if err == nil && isRelativeToWorkspace(rel) {
			return rel, true
		}
	}
	return "", false
}

func canonicalExistingPrefix(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	current := filepath.Clean(abs)
	var suffix []string
	for {
		if real, err := ResolveLinks(current); err == nil {
			parts := append([]string{real}, suffix...)
			return filepath.Join(parts...)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}

func isRelativeToWorkspace(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CollectProjectAssetPaths resolves cross-project asset source paths from
// both build.assets and options.generate.assets in each project's config.
// Returned paths are relative to the workspace root, in slash form on every
// platform, like the declared input roots they are compared with.
func CollectProjectAssetPaths(ws *Workspace) map[string][]string {
	result := make(map[string][]string)
	for _, p := range ws.Projects {
		if p.Config == nil {
			continue
		}
		sources := append(
			buildAssetSources(p.Config.Build),
			generateAssetSources(p.Config.Options["generate"]["assets"])...,
		)
		for _, src := range sources {
			result[p.ID] = append(result[p.ID], resolveAssetSource(p.Path, src))
		}
	}
	return result
}

// buildAssetSources reads the typed build.assets declarations.
func buildAssetSources(build *wsproto.BuildConfig) []string {
	if build == nil {
		return nil
	}
	sources := make([]string, 0, len(build.Assets))
	for _, a := range build.Assets {
		if a.From == "" {
			continue
		}
		sources = append(sources, a.From)
	}
	return sources
}

// generateAssetSources decodes the value at options.generate.assets, which —
// unlike build.assets — arrives as an untyped tree because extension options
// are not part of the project schema. Each level is asserted with an early
// continue, so the shape this accepts is readable as a list rather than
// inferred from an indentation staircase:
//
//	options.generate.assets: [ { "from": "<non-empty string>" }, … ]
//
// It takes the raw value rather than the options map so the caller's index
// expression states which key is being decoded, and so this function names the
// decoded shape once instead of once per enclosing level.
//
// A malformed entry is skipped, not an error. This feeds --impacted selection,
// where the alternative to skipping is failing every command in a workspace
// over one bad extension option; a skipped entry only costs the selection an
// edge it would otherwise have known about.
func generateAssetSources(assetsRaw any) []string {
	entries, ok := assetsRaw.([]any)
	if !ok {
		return nil
	}
	sources := make([]string, 0, len(entries))
	for _, entry := range entries {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		from, ok := fields["from"].(string)
		if !ok || from == "" {
			continue
		}
		sources = append(sources, from)
	}
	return sources
}

// resolveAssetSource makes one declared source workspace-relative, in slash
// form. A leading slash means the workspace root; anything else is relative to
// the declaring project.
func resolveAssetSource(projectPath, src string) string {
	if strings.HasPrefix(src, "/") {
		return path.Clean(filepath.ToSlash(src[1:]))
	}
	return path.Join(filepath.ToSlash(projectPath), filepath.ToSlash(src))
}
