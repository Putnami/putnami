package shared

import (
	"fmt"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ProjectSelection is the already-parsed GLOBAL project-selection flag values a
// read-only command accepts. It carries VALUES, never the parser: a command
// package that takes this struct keeps no edge to internal/cli or to the engine,
// which is the same seam `context map` uses for its two flags.
//
// It is deliberately the whole selection vocabulary rather than a subset. A
// command that accepts `--projects` but silently drops `--tag` is the defect
// this type exists to make impossible: the dispatcher hands the parsed values
// over as one value, so a flag either reaches selection or is rejected.
type ProjectSelection struct {
	// Projects is the raw `--projects` selector: identities, aliases, groups,
	// scope expressions, and subtractions, exactly as a job command reads it.
	Projects string
	// All is `--all`: every project, explicitly.
	All bool
	// Impacted is `--impacted`: the projects the current change reaches.
	Impacted bool
	// Baseline is `--baseline`, the git ref backing `--impacted`.
	Baseline string
	// FilterTag, ExcludeTag, and Exclude are `--tag`, `--exclude-tag`, and
	// `--exclude`.
	FilterTag  string
	ExcludeTag string
	Exclude    string
}

// Requested reports whether the caller asked to narrow anything.
//
// `--all` is deliberately NOT a narrowing: it is the explicit spelling of the
// default (the parser gives it the "*" selector), so a features or specs answer
// is the same with it and without it. Anything else — a selector, an impact
// query, or a tag filter — makes the run scoped.
func (selection ProjectSelection) Requested() bool {
	if selection.Impacted {
		return true
	}
	if strings.TrimSpace(selection.FilterTag) != "" ||
		strings.TrimSpace(selection.ExcludeTag) != "" ||
		strings.TrimSpace(selection.Exclude) != "" {
		return true
	}
	return explicitSelector(selection) != ""
}

// explicitSelector returns the selector the CALLER typed, or "" when there is
// none.
//
// The global flag parser writes its own sentinels into the same field —
// `--all` becomes "*" and `--impacted` becomes "[impacted]" — so reading
// Projects raw would mistake a mode for a selector and refuse `--impacted` as
// "two ways to say selection". Those two values are therefore never selectors
// here; `Impacted` and `All` carry the mode.
func explicitSelector(selection ProjectSelection) string {
	target := strings.TrimSpace(selection.Projects)
	if target == "*" || target == impactedSentinel {
		return ""
	}
	return target
}

// impactedSentinel is what internal/cli's parser writes into Projects for
// `--impacted`.
const impactedSentinel = "[impacted]"

// Any reports whether ANY selection flag was supplied, `--all` and `--baseline`
// included. Exact-target commands use it to refuse a flag they cannot honor
// rather than accept it and change nothing.
func (selection ProjectSelection) Any() bool {
	return selection.Requested() || selection.All || strings.TrimSpace(selection.Baseline) != ""
}

// Selection modes, reported verbatim in structured output.
const (
	// SelectionModeAll is the unscoped whole-workspace projection.
	SelectionModeAll = "all"
	// SelectionModeProjects is an explicit selector, with or without filters.
	SelectionModeProjects = "projects"
	// SelectionModeImpacted is the `--impacted` projection.
	SelectionModeImpacted = "impacted"
)

// ResolvedSelection is the deterministic outcome of resolving a
// ProjectSelection against one workspace. Every collection is sorted, so the
// same flags over the same tree produce the same bytes.
type ResolvedSelection struct {
	// Mode is how the projection was chosen.
	Mode string `json:"mode"`
	// Scoped distinguishes a narrowed run from the whole-workspace default.
	Scoped bool `json:"scoped"`
	// Baseline is the ref `--impacted` actually resolved to, empty otherwise.
	Baseline string `json:"baseline,omitempty"`
	// BaselineSource is the resolution tier that produced Baseline, so a caller
	// can distrust a stale fallback ref the same way a job run can.
	BaselineSource string `json:"baselineSource,omitempty"`
	// ProjectIDs are the selected projects' canonical ids, sorted.
	ProjectIDs []string `json:"projects"`
	// EmptyImpact records the legitimate no-op: `--impacted` resolved cleanly
	// and nothing changed. It is a success, not a "no projects matched" error.
	EmptyImpact bool `json:"emptyImpact,omitempty"`

	projects []*workspace.Project
}

// Projects returns the selected projects, ordered by id.
func (resolved ResolvedSelection) Projects() []*workspace.Project {
	return resolved.projects
}

// Wire projects the resolved selection onto the job context contract's
// `selection` member, so an extension subprocess reads exactly what a
// first-party read-only command reads.
//
// The two types are declared separately because protocols/job cannot import
// this one: ResolvedSelection carries resolved *workspace.Project values, and a
// protocol package that took them on would take on the CLI's workspace loader.
// The equality is pinned by TestResolvedSelectionMarshalsLikeTheWireContract
// instead — the same arrangement protocols/job uses for the staging roots it
// shares with the manifest protocol.
//
// ProjectIDs is passed through rather than re-sorted: ResolveProjectSelection
// already sorts it, and sorting again here would hide a producer that stopped.
func (resolved ResolvedSelection) Wire() protocoljob.Selection {
	return protocoljob.Selection{
		Mode:           resolved.Mode,
		Scoped:         resolved.Scoped,
		Baseline:       resolved.Baseline,
		BaselineSource: resolved.BaselineSource,
		ProjectIDs:     resolved.ProjectIDs,
		EmptyImpact:    resolved.EmptyImpact,
	}
}

// ResolveProjectSelection turns parsed global flags into one selected project
// set, reusing the canonical filter (`workspace.FilterProjects`) and the
// canonical impact resolver (`workspace.ImpactedSelectionForBaseline`) rather
// than reimplementing either.
//
// Two precedents are matched exactly, because a read-only surface that answers
// differently from a job run for the same flags is a second selection contract:
//
//   - `--impacted` with an empty impacted set is a successful, explicit no-op.
//     Nothing changed, so nothing is in scope, and that is an answer.
//   - an explicit selector that matches no project is a not-found error. The
//     caller named something; silence would be a wrong answer.
func ResolveProjectSelection(ws *workspace.Workspace, selection ProjectSelection) (ResolvedSelection, error) {
	if ws == nil {
		return ResolvedSelection{}, cmderr.InvalidConfigf("workspace is required to resolve a project selection")
	}
	if !selection.Requested() {
		return ResolvedSelection{
			Mode:       SelectionModeAll,
			ProjectIDs: sortedProjectIDs(ws.Projects),
			projects:   sortProjectsByID(ws.Projects),
		}, nil
	}

	resolved := ResolvedSelection{Mode: SelectionModeProjects, Scoped: true}
	target := explicitSelector(selection)
	explicit := target != ""
	if !explicit {
		// Only filters were supplied (or `--all` beside them): the candidate set
		// is every project, and the filters do the narrowing.
		target = "*"
	}

	if selection.Impacted {
		if explicit {
			return ResolvedSelection{}, cmderr.Usagef("--impacted and --projects select the same thing two ways: pass one")
		}
		resolved.Mode = SelectionModeImpacted
		impacted, err := workspace.ImpactedSelectionForBaseline(ws, selection.Baseline)
		if err != nil {
			return ResolvedSelection{}, protocolcli.Classify(fmt.Errorf("--impacted failed: %w", err), protocolcli.ErrInvalidConfig)
		}
		resolved.Baseline = impacted.Baseline
		resolved.BaselineSource = string(impacted.BaselineSource)
		if len(impacted.Projects) == 0 {
			resolved.EmptyImpact = true
			resolved.ProjectIDs = []string{}
			resolved.projects = []*workspace.Project{}
			return resolved, nil
		}
		ids := make([]string, 0, len(impacted.Projects))
		for _, project := range impacted.Projects {
			ids = append(ids, project.ID)
		}
		// The impacted set becomes an ordinary selector so tags and exclusions
		// apply to it through the same filter every other selection uses.
		target = strings.Join(ids, ",")
	}

	// DefaultExcludeTags is deliberately left unset. A job run honors the
	// workspace's `disable.tags` because those projects are not meant to BUILD
	// here; a feature or spec inventory that silently omitted them would answer
	// "this project has no features" when the truth is "this run did not look".
	// Tag narrowing on this surface happens only when the caller asks for it.
	selected := workspace.FilterProjects(ws, workspace.FilterOptions{
		Projects:     target,
		FilterTag:    selection.FilterTag,
		ExcludeTag:   selection.ExcludeTag,
		Exclude:      selection.Exclude,
		DirectTarget: explicit,
		ScopeIndex:   scopeIndexOf(ws),
	})
	if len(selected) == 0 {
		if selection.Impacted {
			// The impacted set was non-empty and the filters emptied it. That is
			// still an explicit no-op rather than a failure: the caller asked for
			// "what changed, minus these", and the answer is nothing.
			resolved.EmptyImpact = true
			resolved.ProjectIDs = []string{}
			resolved.projects = []*workspace.Project{}
			return resolved, nil
		}
		return ResolvedSelection{}, cmderr.NotFoundf("no projects matched: %s", describeSelection(selection))
	}
	resolved.ProjectIDs = sortedProjectIDs(selected)
	resolved.projects = sortProjectsByID(selected)
	return resolved, nil
}

func scopeIndexOf(ws *workspace.Workspace) *wsproto.ScopeIndex {
	if ws == nil {
		return nil
	}
	return ws.ScopeIndex
}

// describeSelection renders the flags a failed selection was built from, so the
// error names what the caller typed rather than an internal expression.
func describeSelection(selection ProjectSelection) string {
	var parts []string
	if value := explicitSelector(selection); value != "" {
		parts = append(parts, "--projects "+value)
	}
	if value := strings.TrimSpace(selection.FilterTag); value != "" {
		parts = append(parts, "--tag "+value)
	}
	if value := strings.TrimSpace(selection.ExcludeTag); value != "" {
		parts = append(parts, "--exclude-tag "+value)
	}
	if value := strings.TrimSpace(selection.Exclude); value != "" {
		parts = append(parts, "--exclude "+value)
	}
	if len(parts) == 0 {
		return "the requested selection"
	}
	return strings.Join(parts, " ")
}

func sortProjectsByID(projects []*workspace.Project) []*workspace.Project {
	out := make([]*workspace.Project, 0, len(projects))
	for _, project := range projects {
		if project != nil {
			out = append(out, project)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortedProjectIDs(projects []*workspace.Project) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		if project != nil {
			ids = append(ids, project.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
