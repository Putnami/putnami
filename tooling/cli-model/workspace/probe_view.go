package workspace

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The merged provider view IS the project's identity.
//
// Earlier, this view rode alongside core's own parsers and was deliberately kept
// out of every cache key, because folding it in while both existed would have
// made a project's key depend on whether that particular run happened to probe.
// This change deletes the parsers, so the view stops being a second opinion and
// becomes the only one — and the key moves ONCE, here.
//
// The invariant that replaces the old separation is stated as an equality:
//
//	identity(tree) == identity(tree)
//
// regardless of whether this run adopted a RECORDED answer from
// `.putnami/workspace-index.json` or a FRESH one from a provider process. Three
// things hold it up, and all three are testable from here:
//
//   - The merge is a pure function of the provider answers plus the authored
//     putnami.json values (wsproto.MergeProbeResults), and a validated snapshot
//     stores the answers themselves — not a summary — so replaying them
//     reproduces the merge exactly (sync.go, mergeRecordedResults).
//   - Applying a view is IDEMPOTENT and does not read what a previous
//     application wrote: ResolveProjectIdentity reads only the authored config,
//     the scope contribution captured at discovery, and the merged view.
//   - Applying a view invalidates every memoized digest and rebuilds the graph
//     and the name/ID indexes, so nothing computed before adoption survives it.
//
// A run that adopts NOTHING — a fresh checkout with no snapshot, or a workspace
// whose extensions cannot be resolved — resolves projects from authored config
// alone. That is a strictly smaller identity, not a different one, and it is
// visible: such a workspace has no provider-derived dependency edges, so a
// command that plans over the graph fails the probe policy rather than building
// the wrong thing (RequireProbe).

// probeView is the merged provider answer for one workspace, guarded because
// Load memoizes one *Workspace per root and shares it across a run's phases and
// the scheduler's goroutines.
type probeView struct {
	mu     sync.RWMutex
	merged map[string]wsproto.MergedProject
}

// AdoptProbeView records the merged provider answer for this workspace and
// re-resolves every project's identity from it.
//
// It copies the map so a later mutation of the caller's value cannot change what
// a concurrent reader sees, and it rebuilds the dependency graph and the
// name/ID indexes, because a probe can change both what a project is called and
// what it depends on.
//
// A view that would resolve to a workspace Load REFUSES — two projects answering
// to one name, or a dependency cycle — is reported as a warning and NOT adopted;
// the previously resolved identity is kept. Adoption happens after the
// workspace is already loaded, so failing here would take down the recovery
// commands, and adopting anyway would put a live workspace into the exact state
// its own loader rejects. Keeping the previous view is the only third option,
// and it is the one that leaves every repair path reachable.
func (ws *Workspace) AdoptProbeView(merged map[string]wsproto.MergedProject) {
	if ws == nil {
		return
	}
	if err := ws.AdoptValidatedProbeView(merged); err != nil {
		ws.Warnings = append(ws.Warnings, fmt.Sprintf(
			"workspace probe: %v; the reported view was NOT adopted and the previously resolved identity is kept", err))
	}
	// Name divergences are NOT appended here. Load already recorded the ones its
	// adopted view showed, and the callers that re-adopt (the run's probe phase,
	// `projects sync`) report the fresh set themselves — appending here would
	// print each finding twice on every probing run.
}

// HasProbeView reports whether a NON-EMPTY provider view is currently adopted.
//
// It is the answer to "has any provider actually described this workspace?", and
// the callers that ask are the ones whose output is a lie without one: a project
// resolved from authored config alone has core's directory-basename fallback as
// its source identity, not the identity its own manifest declares, so every
// convention-named project looks unaligned (`projects sync`'s divergence report)
// and the graph carries zero provider-derived edges.
func (ws *Workspace) HasProbeView() bool {
	if ws == nil {
		return false
	}
	ws.probe.mu.RLock()
	defer ws.probe.mu.RUnlock()
	return len(ws.probe.merged) > 0
}

// currentProbeView copies the merged view this workspace has adopted, so a
// rejected candidate can be rolled back to it.
func (ws *Workspace) currentProbeView() map[string]wsproto.MergedProject {
	ws.probe.mu.RLock()
	defer ws.probe.mu.RUnlock()
	if ws.probe.merged == nil {
		return nil
	}
	copied := make(map[string]wsproto.MergedProject, len(ws.probe.merged))
	for path, project := range ws.probe.merged {
		copied[path] = project
	}
	return copied
}

// AdoptValidatedProbeView applies merged and KEEPS it only when the workspace it
// produces is one core can load; otherwise the previous view is restored and the
// reason is returned.
//
// This is the guard that makes a poisoned view recoverable instead of permanent.
// The failure it exists for: a run persists a merged view that resolves two
// projects to one name (or introduces a cycle), and every later Load adopts the
// RECORDED view before it runs its own fatal checks — so the workspace stops
// loading, and every command that could repair it must load first. Validating
// the candidate BEFORE it is adopted or persisted closes both halves: Load falls
// back to the un-adopted resolution with a warning, and the creating run refuses
// to write it.
//
// Application is all-or-nothing because applyProbeView is a pure function of
// (authored config, scope contribution, view): re-applying the previous view
// reproduces exactly the workspace that existed before the candidate was tried,
// never a blend of the two.
func (ws *Workspace) AdoptValidatedProbeView(merged map[string]wsproto.MergedProject) error {
	previous := ws.currentProbeView()
	ws.AdoptProbeViewUnvalidated(merged)
	err := ws.probeViewIntegrityError()
	if err == nil {
		return nil
	}
	ws.AdoptProbeViewUnvalidated(previous)
	return err
}

// probeViewIntegrityError names why the workspace as currently resolved is one
// Load refuses, or nil when it is loadable.
//
// The two conditions are exactly Load's fatal checks, and they are stated once
// here so "a view core can adopt" and "a workspace core can load" cannot drift
// apart — the drift is what wedged a workspace in the first place.
func (ws *Workspace) probeViewIntegrityError() error {
	if err := CheckDuplicateNames(ws.Projects); err != nil {
		return err
	}
	if cycle := ws.Graph.FindCycle(); cycle != nil {
		return fmt.Errorf("dependency cycle detected: %s", strings.Join(cycle, " → "))
	}
	return nil
}

// AdoptProbeViewUnvalidated is the unguarded half: record the view, re-resolve
// identity, rebuild the derived structures. Only AdoptValidatedProbeView and the
// callers applying an EMPTY view (the un-adopted resolution, which has nothing
// to validate) use it directly — a candidate provider answer must go through the
// validated path, or the workspace can end up in the state Load refuses.
func (ws *Workspace) AdoptProbeViewUnvalidated(merged map[string]wsproto.MergedProject) {
	ws.probe.mu.Lock()
	if merged == nil {
		ws.probe.merged = nil
	} else {
		copied := make(map[string]wsproto.MergedProject, len(merged))
		for path, project := range merged {
			copied[path] = project
		}
		ws.probe.merged = copied
	}
	ws.probe.mu.Unlock()

	applyProbeView(ws, merged)
}

// applyProbeView re-resolves every project from (authored config, scope
// contribution, merged view) and rebuilds everything derived from the result.
//
// Two passes are required and the order is load-bearing: dependency edges are
// reported as PATHS and resolved to project NAMES, so every name must be final
// before any edge is resolved. Doing it in one pass would make an edge's
// spelling depend on discovery order.
func applyProbeView(ws *Workspace, merged map[string]wsproto.MergedProject) {
	if ws == nil {
		return
	}
	for _, project := range ws.Projects {
		view, ok := merged[ProbePathOf(project.Path)]
		if !ok {
			ResolveProjectIdentity(project, nil)
			continue
		}
		ResolveProjectIdentity(project, &view)
	}

	byPath := make(map[string]*Project, len(ws.Projects))
	for _, project := range ws.Projects {
		byPath[CleanWorkspacePath(project.Path)] = project
	}
	for _, project := range ws.Projects {
		view, ok := merged[ProbePathOf(project.Path)]
		if !ok {
			continue
		}
		project.Dependencies = resolveDependencyNames(project, view.Dependencies, byPath)
		project.DependencySources = resolveDependencySources(project, view.DependencySources, byPath)
	}

	ws.rebuildIndexes()
}

// ResolveProjectIdentity is the merge rule, in force order, as one function.
//
//	explicit putnami.json  >  provider source identity  >  scope namePattern  >  directory basename
//
// SourceName records the identity BEFORE the namePattern override, which is what
// makes "the manifest on disk says something else" detectable: it is the value
// `projects sync` reports and each provider's own sync task writes.
//
// List members (tags, publish, runsWith, extensions) and dependencies come from
// the merged view, which has already unioned the authored putnami.json values
// into them; a scope contributes only where the project itself declared
// nothing, never overwriting.
//
// It is a pure function of its arguments: calling it twice with the same view
// produces the same project, and calling it with a different view produces the
// project that view describes rather than a blend of the two.
func ResolveProjectIdentity(p *Project, view *wsproto.MergedProject) {
	cfg := p.Config

	p.Name = ""
	p.Type = ""
	p.Version = ""
	p.Tags = nil
	p.Publish = nil
	p.Extensions = nil
	p.RunsWith = nil
	p.Dependencies = nil
	p.DependencySources = nil
	p.Visibility = ""
	p.Metadata = nil

	if cfg != nil {
		p.Name = cfg.Name
		p.Type = cfg.Type
		p.Tags = cfg.Tags
		// Visibility is AUTHORED only. A provider describes what a project
		// imports; who may import it back is the project owner's statement, and
		// no scope inherits it — a boundary a project did not write is a
		// boundary its owner never agreed to.
		p.Visibility = cfg.Visibility
		p.Dependencies = append([]string(nil), cfg.Dependencies...)
		// dockerBaseProject is an artifact dependency rather than a source-language
		// import. Materialize it as a real graph edge here so it participates in
		// ordering, dependencyClosure, cache dependencies, and impacted propagation.
		if dep := artifactBaseProjectDependency(cfg); dep != "" && !slices.Contains(p.Dependencies, dep) {
			p.Dependencies = append(p.Dependencies, dep)
		}
		p.Publish = cfg.Publish
		p.Extensions = cfg.Extensions
		p.RunsWith = cfg.RunsWith
	}

	if view != nil {
		if p.Name == "" {
			p.Name = view.SourceName
		}
		if p.Type == "" {
			p.Type = view.Type
		}
		if p.Version == "" {
			p.Version = view.Version
		}
		// NIL, not empty. An authored `"tags": []` is the explicit statement
		// "this project has no tags", and it must suppress the provider's view —
		// otherwise `projects tag --remove` clearing the last tag is a silent
		// no-op for any project whose tags the provider also reports (a
		// package.json `putnami.tags` block, say): the key would be authored as
		// empty and the merged view would hand the same tags right back. The
		// config decode preserves the distinction because encoding/json leaves a
		// []string nil for an absent key and non-nil-empty for `[]`; `omitempty`
		// affects encoding only. Every other list here keeps len()==0 because
		// none of them has a "cleared" spelling a command can write.
		if p.Tags == nil {
			p.Tags = view.Tags
		}
		if len(p.Publish) == 0 {
			p.Publish = view.Publish
		}
		if len(p.RunsWith) == 0 {
			p.RunsWith = view.RunsWith
		}
		if len(p.Extensions) == 0 {
			p.Extensions = view.Extensions
		}
		p.Metadata = view.Metadata
	}

	// Whether an explicit package identity was declared (putnami.json, or a
	// provider's own manifest). When declared, a parent scope namePattern defers
	// to it so self-identifying nested modules — a generated cross-language
	// client, say — keep their real, build-addressable name instead of being
	// renamed by the scope convention.
	nameDeclared := p.Name != ""
	if p.Name == "" {
		p.Name = filepath.Base(p.Path)
	}
	p.SourceName = p.Name
	if !nameDeclared && p.Scope.Name != "" {
		p.Name = p.Scope.Name
	}

	// Same nil-vs-empty rule as the view fallthrough above: an explicit clear
	// must not be undone by the scope's tags either. The merged view only ever
	// produces nil or a non-empty list (wsproto normalizes), so a non-nil empty
	// p.Tags here can only have come from an authored `"tags": []`.
	if p.Tags == nil {
		p.Tags = p.Scope.Tags
	}
	if len(p.Extensions) == 0 {
		p.Extensions = p.Scope.Extensions
	}
}

// artifactBaseProjectDependency extracts the package graph's authored artifact
// reference. Malformed values are diagnosed by the package task with project
// context; they must never invent an edge here.
func artifactBaseProjectDependency(cfg *wsproto.ProjectConfig) string {
	if cfg == nil || cfg.Options == nil {
		return ""
	}
	options, ok := cfg.Options["package"]
	if !ok {
		return ""
	}
	value, ok := options["dockerBaseProject"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

// resolveDependencyNames maps the merged view's dependency entries onto project
// NAMES, which is the spelling the dependency graph is keyed on.
//
// A provider reports edges as repo-relative PATHS — that is the protocol's
// contract, and it is what lets a provider resolve its own module graph without
// learning core's naming rules. The merge additionally folds the AUTHORED
// putnami.json dependencies in verbatim, and those are names or IDs, not paths.
// So each entry is resolved as a path first and kept as-is when no project
// answers to that path: an unresolvable entry is a name the workspace does not
// contain, and dropping it would silently delete a declared edge.
//
// The result is sorted and deduplicated because it feeds the dependency graph
// and the project's metadata digest; Go map iteration is random, and a list that
// reordered between two runs over one tree would oscillate every cache key that
// observes it.
func resolveDependencyNames(p *Project, entries []string, byPath map[string]*Project) []string {
	set := make(map[string]bool, len(entries)+len(p.Dependencies))
	for _, declared := range p.Dependencies {
		set[declared] = true
	}
	for _, entry := range entries {
		if target, ok := byPath[CleanWorkspacePath(entry)]; ok {
			if target == p {
				continue // a project never depends on itself
			}
			set[target.Name] = true
			continue
		}
		set[entry] = true
	}
	delete(set, p.Name)
	delete(set, "")

	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// resolveDependencySources maps the merged view's edge provenance onto the
// dependency spelling the project carries, so the graph can attribute an edge
// under the same key it resolves it with.
//
// A provider reports provenance keyed by PATH, exactly like the edges it
// attributes. An entry naming a path no project answers to is kept verbatim for
// the same reason resolveDependencyNames keeps an unresolvable entry: the entry
// is then a name, and dropping it would silently lose the attribution of an
// edge the graph still carries. An entry for a dependency the project does not
// carry contributes nothing — it attributes no edge.
func resolveDependencySources(p *Project, entries map[string]wsproto.DependencySource,
	byPath map[string]*Project) map[string]wsproto.DependencySource {
	if len(entries) == 0 {
		return nil
	}
	carried := make(map[string]bool, len(p.Dependencies))
	for _, dependency := range p.Dependencies {
		carried[dependency] = true
	}
	out := make(map[string]wsproto.DependencySource, len(entries))
	for entry, source := range entries {
		name := entry
		if target, ok := byPath[CleanWorkspacePath(entry)]; ok {
			if target == p {
				continue
			}
			name = target.Name
		}
		if !carried[name] {
			continue
		}
		out[name] = wsproto.StrongerDependencySource(out[name], source)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rebuildIndexes refreshes everything derived from project identity: the
// name/ID lookups, the dependency graph, and the memoized digests.
//
// Dropping the memoized digests is not housekeeping. Every scheduled job's cache
// key asks for its project's metadata digest, and a digest computed before a
// probe changed the project's dependency edges would key the build on a graph
// that no longer exists.
func (ws *Workspace) rebuildIndexes() {
	ws.projectMap = make(map[string]*Project, len(ws.Projects))
	ws.projectByID = make(map[string]*Project, len(ws.Projects))
	for _, p := range ws.Projects {
		if _, exists := ws.projectMap[p.Name]; !exists {
			ws.projectMap[p.Name] = p
		}
		ws.projectByID[p.ID] = p
	}
	ws.Graph = BuildGraph(ws.Projects)

	ws.digestMu.Lock()
	ws.probeDigest = ""
	ws.identityDigest = ""
	ws.metadataDigests = nil
	ws.digestMu.Unlock()
}

// NameDivergences reports every project whose SOURCE identity disagrees with its
// resolved name — the report `projects sync` prints and each provider's own sync
// task acts on.
//
// This is what remains of C3b's old/new comparison guard, and the change of
// purpose is deliberate. That guard existed to catch a provider disagreeing with
// the core parser it was scheduled to replace; with the parsers deleted there is
// nothing left to disagree WITH, so keeping it would be comparing the probe to
// itself. What is still worth reporting is the divergence that made core align
// names in the first place: a manifest on disk that spells the project
// differently from the name the workspace resolved for it.
//
// Findings are sorted so two runs over one tree print the same lines in the same
// order.
func NameDivergences(ws *Workspace) []string {
	if ws == nil {
		return nil
	}
	findings := make([]string, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		if project.SourceName == "" || project.SourceName == project.Name {
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"project %s: source name %q differs from resolved name %q — run `putnami projects sync` to align",
			project.Path, project.SourceName, project.Name))
	}
	sort.Strings(findings)
	return findings
}

// ProviderMetadataValue returns the first non-empty value a provider reported
// for a top-level metadata key, in extension-name order.
//
// This is the ONE place core looks INSIDE a provider metadata block, and it is
// deliberately confined to human-facing reporting (the agent context pack's
// application entrypoint). Metadata is opaque to identity: it never resolves a
// name, a type or an edge, and this accessor's answer never reaches a cache key
// — the whole block already does, canonically, through ProjectMetadataDigest.
// A missing key is an ordinary answer, not an error.
func (ws *Workspace) ProviderMetadataValue(project *Project, key string) string {
	if ws == nil || project == nil || len(project.Metadata) == 0 {
		return ""
	}
	extensions := make([]string, 0, len(project.Metadata))
	for extension := range project.Metadata {
		extensions = append(extensions, extension)
	}
	sort.Strings(extensions)

	for _, extension := range extensions {
		var block map[string]json.RawMessage
		if json.Unmarshal(project.Metadata[extension], &block) != nil {
			continue
		}
		raw, ok := block[key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil && value != "" {
			return value
		}
	}
	return ""
}
