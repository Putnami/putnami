package workspace

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/tooling/cli/internal/git"
)

// ImpactedProjects computes which projects are affected by git changes.
// It finds changed files, maps them to projects, then propagates through
// the dependency graph to include transitively affected projects.
func ImpactedProjects(ws *Workspace, baseline string) ([]*Project, error) {
	projects, _, err := ImpactedProjectsWithBaseline(ws, baseline)
	return projects, err
}

// ImpactedProjectsWithBaseline computes impacted projects and returns the
// resolved baseline ref used for the diff.
func ImpactedProjectsWithBaseline(ws *Workspace, baseline string) ([]*Project, string, error) {
	selection, err := ImpactedSelectionForBaseline(ws, baseline)
	return selection.Projects, selection.Baseline, err
}

// ImpactedSelection is one --impacted computation with the evidence for its
// outcome, so a caller can say WHY the selection is what it is instead of
// leaving an operator to infer it from the project count.
//
// The field that carries the explanation is UnownedRootFiles. `--impacted` has
// two outcomes that look identical from outside and mean opposite things: it
// selects nothing because nothing changed, or it selects nothing because every
// changed workspace-root path was claimed by neither path ownership nor a
// provider's project-level watchedFiles answer. The evidence distinguishes that
// remaining gap from a genuinely empty diff.
type ImpactedSelection struct {
	// Projects is the selected set, ordered by ws.Projects.
	Projects []*Project
	// Baseline is the ref the diff actually resolved to.
	Baseline string
	// BaselineSource is the resolution tier that produced Baseline. Fallback
	// tiers (local-trunk, upstream) carry their own explanation because they can
	// measure against a stale or self-referential ref.
	BaselineSource git.BaselineSource
	// DiffBase is the commit the diff was measured against: the merge base of
	// Baseline and HEAD. Two selections with the same DiffBase, the same HEAD
	// and no UncommittedFiles read the same diff.
	DiffBase string
	// ChangedFiles is the raw diff, workspace-relative.
	ChangedFiles []string
	// UncommittedFiles are the ChangedFiles no commit records — staged,
	// unstaged or untracked. They are why a selection computed on one machine
	// can differ from one computed on another for the same commit: a file the
	// run's own bootstrap rewrote shows up here.
	UncommittedFiles []string
	// UnownedRootFiles are the changed paths that sit DIRECTLY at the workspace
	// root, belong to no project by path, and were claimed by no provider's
	// per-project watchedFiles answer — sorted, deduplicated. They contributed
	// nothing to Projects.
	UnownedRootFiles []string
	// Trace explains every entry of Projects: which changed file claimed each
	// directly affected project and how, which edge reached each propagated
	// one, and which of them run a subset of their tasks (Trace.Scopes). It is
	// the evidence for a selection that is too LARGE, where UnownedRootFiles is
	// the evidence for one that is too small.
	Trace ImpactTrace
}

// TaskScopes is the plan's view of Trace.Scopes: every task-scoped project ID
// mapped to the sorted tasks of it the change reaches (`<command>~<step>`).
// Nil when every selected project is full. The map is the selection's own;
// callers must not mutate it.
func (s ImpactedSelection) TaskScopes() map[string][]string {
	if len(s.Trace.Scopes) == 0 {
		return nil
	}
	return s.Trace.Scopes
}

// unownedRootFilesShown bounds the paths UnownedRootFilesExplanation names. A
// root-level change set is small in practice; the cap exists so a mass root
// edit degrades to a count instead of a wall of text.
const unownedRootFilesShown = 5

// impactTraceSeedsShown and impactTraceEdgesShown bound the lines
// TraceExplanation prints. The block is a notice, not the record: the full
// trace is the `impacted` MCP tool's answer and a run's TraceRecord session
// event, so the caps lose nothing and a hundred-project widening degrades to a
// count.
const (
	impactTraceSeedsShown = 5
	impactTraceEdgesShown = 10
	// impactTraceViaWidth bounds one seed's `Via`. A declared-input claim names
	// the whole pattern set, and a project such as `@putnami/cli` declares
	// twenty patterns — one line long enough to bury the five seeds around it.
	// The full set is in the `impacted` MCP answer.
	impactTraceViaWidth = 80
)

// UnownedRootFilesExplanation renders UnownedRootFiles as one line for a human
// notice stream, or "" when there is nothing to explain.
//
// The rendering lives with the selection rather than in the adapter that prints
// it because every surface owes the same account of the same evidence: the CLI
// run, and any other caller that has to answer why a non-empty diff selected
// what it did. A second adapter writing its own sentence would be a second
// claim about one mapping.
func (s ImpactedSelection) UnownedRootFilesExplanation() string {
	if len(s.UnownedRootFiles) == 0 {
		return ""
	}
	shown, extra := s.UnownedRootFiles, 0
	if len(shown) > unownedRootFilesShown {
		shown, extra = shown[:unownedRootFilesShown], len(shown)-unownedRootFilesShown
	}
	line := "  --impacted: " + strings.Join(shown, ", ")
	if extra > 0 {
		line += fmt.Sprintf(" (+%d more)", extra)
	}
	return line + " changed at the workspace root and belong to no project — they select nothing on their own."
}

// BaselineExplanation renders the resolution tier as one line for a human
// notice stream, or "" when the resolved baseline needs no account. Only the
// fallback tiers speak: a local trunk can be stale relative to the remote, and
// an upstream tracking ref is the earlier default that made --impacted
// select nothing on pushed branches — a caller that lands there should see it.
func (s ImpactedSelection) BaselineExplanation() string {
	switch s.BaselineSource {
	case git.BaselineSourceLocalTrunk:
		return "  --impacted: baseline resolved to local '" + s.Baseline + "' — no origin/HEAD or origin/main ref exists; a stale local trunk under-selects."
	case git.BaselineSourceUpstream:
		return "  --impacted: baseline resolved to the upstream tracking ref '" + s.Baseline + "' — no trunk ref exists. Set the workspace config baseline (or epicBranches) if this ref is not the merge target."
	default:
		return ""
	}
}

// TraceExplanation renders Trace as lines for a human notice stream, or nothing
// when there is no trace. It lives with the selection for the reason
// UnownedRootFilesExplanation does: every surface owes the same account of the
// same evidence.
//
// The first line counts: the diff's files, the projects they claimed directly,
// and the projects propagation added, broken down by edge kind. Then one line
// per seed, in ChangedFiles order then Projects order, and one line per
// propagated project in Projects order; each list is capped and closes with a
// count of what it left out.
func (s ImpactedSelection) TraceExplanation() []string {
	if len(s.Trace.Seeds) == 0 && len(s.Trace.Edges) == 0 {
		return nil
	}
	lines := []string{s.traceHeader()}

	seeds := s.orderedSeeds()
	shown := seeds
	if len(shown) > impactTraceSeedsShown {
		shown = shown[:impactTraceSeedsShown]
	}
	for _, seed := range shown {
		lines = append(lines, fmt.Sprintf("    %s → %s  [%s %s]", seed.File, seed.project, seed.Kind, elideVia(seed.Via)))
	}
	if extra := len(seeds) - len(shown); extra > 0 {
		lines = append(lines, fmt.Sprintf("    … +%d more seed(s)", extra))
	}

	edges := 0
	for _, p := range s.Projects {
		edge, ok := s.Trace.Edges[p.ID]
		if !ok {
			continue
		}
		edges++
		if edges > impactTraceEdgesShown {
			continue
		}
		lines = append(lines, fmt.Sprintf("    %s ← %s  [%s%s%s]",
			p.ID, edge.From, edge.Kind, edgeEvidence(edge), scopeSuffix(s.Trace.ScopeOf(p.ID))))
	}
	if extra := edges - impactTraceEdgesShown; extra > 0 {
		lines = append(lines, fmt.Sprintf("    … +%d more edge(s); the `impacted` MCP tool returns every reason.", extra))
	}
	return lines
}

// ImpactTraceRecordType is the session event type that records one --impacted
// selection's complete evidence (TraceRecord).
const ImpactTraceRecordType = "selection:impacted"

// ImpactTraceSeed is one seed line of an ImpactTraceRecord: a changed file's
// claim on a project.
type ImpactTraceSeed struct {
	Project string `json:"project"`
	File    string `json:"file"`
	Kind    string `json:"kind"`
	Via     string `json:"via"`
}

// ImpactTraceEdge is one edge line of an ImpactTraceRecord: the edge that
// first reached a propagated project.
type ImpactTraceEdge struct {
	Project string `json:"project"`
	From    string `json:"from"`
	Kind    string `json:"kind"`
	// Via is present on a contract edge: the provider's committed contract
	// that changed, which is the only reason that edge fires.
	Via string `json:"via,omitempty"`
	// ContractSHA256 is present beside Via: the full sha256 of that contract
	// in the tree the selection read.
	ContractSHA256 string `json:"contractSha256,omitempty"`
}

// ImpactTraceScope is one scope line of an ImpactTraceRecord: a selected
// project that runs the listed tasks of itself only.
type ImpactTraceScope struct {
	Project string `json:"project"`
	// Tasks are the project's task scopes, `<command>~<step>`, sorted.
	Tasks []string `json:"tasks"`
}

// ImpactTraceRecord is one --impacted selection's complete evidence, as the
// data of one ImpactTraceRecordType session event. ChangedFiles, Seeds, Edges
// and Scopes are always present, so a reader can tell "none" from "not
// recorded"; the other lists are omitted when empty.
type ImpactTraceRecord struct {
	Baseline         string             `json:"baseline"`
	BaselineSource   string             `json:"baselineSource,omitempty"`
	DiffBase         string             `json:"diffBase,omitempty"`
	ChangedFiles     []string           `json:"changedFiles"`
	UncommittedFiles []string           `json:"uncommittedFiles,omitempty"`
	UnownedRootFiles []string           `json:"unownedRootFiles,omitempty"`
	Seeds            []ImpactTraceSeed  `json:"seeds"`
	Edges            []ImpactTraceEdge  `json:"edges"`
	Scopes           []ImpactTraceScope `json:"scopes"`
}

// TraceRecord is TraceExplanation with nothing left out: the baseline ref and
// its resolution tier, the commit the diff measured against, every changed
// file and which of them no commit records, the root files that reached
// nobody, every seed in TraceExplanation's order, every edge in Projects
// order and every task-scoped project in Projects order.
//
// The terminal block is capped because it is read by a person. This record is
// read by a machine — a hosted run viewer, or an operator comparing a hosted
// plan with a local one — so it carries every line, and it is the same lines:
// both render one Trace in one order. Before it, the evidence existed
// only under --verbose on a terminal, elided after ten edges, and a hosted run
// that selected 71 projects where a local run selected 48 left no record of
// why.
func (s ImpactedSelection) TraceRecord() ImpactTraceRecord {
	record := ImpactTraceRecord{
		Baseline:         s.Baseline,
		BaselineSource:   string(s.BaselineSource),
		DiffBase:         s.DiffBase,
		ChangedFiles:     append([]string{}, s.ChangedFiles...),
		UncommittedFiles: s.UncommittedFiles,
		UnownedRootFiles: s.UnownedRootFiles,
		Seeds:            make([]ImpactTraceSeed, 0, len(s.Trace.Seeds)),
		Edges:            make([]ImpactTraceEdge, 0, len(s.Trace.Edges)),
		Scopes:           make([]ImpactTraceScope, 0, len(s.Trace.Scopes)),
	}
	for _, seed := range s.orderedSeeds() {
		record.Seeds = append(record.Seeds, ImpactTraceSeed{
			Project: seed.project, File: filepath.ToSlash(seed.File), Kind: string(seed.Kind), Via: filepath.ToSlash(seed.Via),
		})
	}
	for _, p := range s.Projects {
		if edge, ok := s.Trace.Edges[p.ID]; ok {
			record.Edges = append(record.Edges, ImpactTraceEdge{
				Project: p.ID, From: edge.From, Kind: string(edge.Kind),
				Via: filepath.ToSlash(edge.Via), ContractSHA256: edge.ContractSHA256,
			})
		}
		if scope := s.Trace.ScopeOf(p.ID); scope != nil {
			record.Scopes = append(record.Scopes, ImpactTraceScope{Project: p.ID, Tasks: append([]string{}, scope...)})
		}
	}
	return record
}

// impactTraceDigestShown bounds the contract digest one edge line prints.
// Twelve hex digits tell two contracts apart at a glance; the record and the
// `impacted` MCP answer carry all 64.
const impactTraceDigestShown = 12

// edgeEvidence renders what a fired contract edge rests on, inside its edge
// line: ` services/catalog/schema/openapi.json sha256:0123456789ab`. It is ""
// for an edge that names no file, which is every edge but a contract edge.
func edgeEvidence(edge ImpactEdge) string {
	if edge.Via == "" {
		return ""
	}
	evidence := " " + filepath.ToSlash(edge.Via)
	if digest := edge.ContractSHA256; digest != "" {
		if len(digest) > impactTraceDigestShown {
			digest = digest[:impactTraceDigestShown]
		}
		evidence += " sha256:" + digest
	}
	return evidence
}

// impactTraceScopeTasksShown bounds the tasks one edge line names. A tool
// change scopes a consumer to every task that tool declares, which is forty
// entries on one line; the whole list is in the record and in the `impacted`
// MCP answer.
const impactTraceScopeTasksShown = 4

// scopeSuffix renders a task-scoped project's scope inside its edge line, or
// "" for a full project: `[dependency; tasks build~describe, build~compile]`.
func scopeSuffix(scope []string) string {
	if len(scope) == 0 {
		return ""
	}
	shown, extra := scope, 0
	if len(shown) > impactTraceScopeTasksShown {
		shown, extra = shown[:impactTraceScopeTasksShown], len(shown)-impactTraceScopeTasksShown
	}
	suffix := "; tasks " + strings.Join(shown, ", ")
	if extra > 0 {
		suffix += fmt.Sprintf(" (+%d)", extra)
	}
	return suffix
}

// SessionEventData is a session event's data: a record's JSON members, so the
// live stream and the retained event log carry exactly the documented shape.
type SessionEventData = map[string]any

// EventData is the record as a session event's data.
func (r ImpactTraceRecord) EventData() (data SessionEventData) {
	// Strings and string slices only: the round trip cannot fail.
	encoded, _ := json.Marshal(r)
	_ = json.Unmarshal(encoded, &data)
	return data
}

// elideVia bounds one claim's evidence at impactTraceViaWidth, cutting at a
// pattern boundary so what is shown is a prefix of the real set rather than a
// truncated pattern that looks like a different one.
func elideVia(via string) string {
	if len(via) <= impactTraceViaWidth {
		return via
	}
	cut := strings.LastIndex(via[:impactTraceViaWidth], " ")
	if cut <= 0 {
		cut = impactTraceViaWidth
	}
	return via[:cut] + " …"
}

// traceHeader is TraceExplanation's first line. Per-kind counts omit kinds at
// zero, and a trace propagation added nothing to ends after "directly". The
// file count is the diff's, which is larger than the seeds' when a changed
// file reached nobody; a selection built without its diff counts the seeds'.
func (s ImpactedSelection) traceHeader() string {
	changed := len(s.ChangedFiles)
	if changed == 0 {
		files := make(map[string]bool)
		for _, seeds := range s.Trace.Seeds {
			for _, seed := range seeds {
				files[seed.File] = true
			}
		}
		changed = len(files)
	}
	header := fmt.Sprintf("  --impacted: %d changed file(s) reached %d project(s) directly",
		changed, len(s.Trace.Seeds))
	if len(s.Trace.Edges) == 0 {
		return header
	}
	byKind := make(map[ImpactEdgeKind]int)
	for _, edge := range s.Trace.Edges {
		byKind[edge.Kind]++
	}
	var parts []string
	for _, kind := range []ImpactEdgeKind{ImpactEdgeDependency, ImpactEdgeContract, ImpactEdgeExtensionConsumer} {
		if byKind[kind] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", kind, byKind[kind]))
		}
	}
	if scoped := len(s.Trace.Scopes); scoped > 0 {
		parts = append(parts, fmt.Sprintf("%d task-scoped", scoped))
	}
	return fmt.Sprintf("%s; propagation added %d (%s)", header, len(s.Trace.Edges), strings.Join(parts, ", "))
}

// orderedSeed is one seed line's worth of evidence: the claim and the project
// it claimed.
type orderedSeed struct {
	ImpactSeed
	project string
}

// orderedSeeds flattens Trace.Seeds into ChangedFiles order, then Projects
// order. A file the diff lists first is explained first, and within one file
// the projects come in workspace order — the order every other listing uses.
// A seed whose file is not in ChangedFiles (a hand-built trace) sorts after the
// diff, in first-seen order.
func (s ImpactedSelection) orderedSeeds() []orderedSeed {
	rank := make(map[string]int, len(s.ChangedFiles))
	for i, file := range s.ChangedFiles {
		if _, known := rank[file]; !known {
			rank[file] = i
		}
	}
	var seeds []orderedSeed
	for _, p := range s.Projects {
		for _, seed := range s.Trace.Seeds[p.ID] {
			if _, known := rank[seed.File]; !known {
				rank[seed.File] = len(rank)
			}
			seeds = append(seeds, orderedSeed{ImpactSeed: seed, project: p.ID})
		}
	}
	sort.SliceStable(seeds, func(i, j int) bool {
		return rank[seeds[i].File] < rank[seeds[j].File]
	})
	return seeds
}

// ImpactedSelectionForBaseline computes the impacted set and the evidence for
// it. It is what ImpactedProjectsWithBaseline is built from, and the surface a
// caller reaches for when "nothing was selected" needs a reason.
func ImpactedSelectionForBaseline(ws *Workspace, baseline string) (ImpactedSelection, error) {
	return ImpactedSelectionForBaselineWithTasks(ws, baseline, nil)
}

// ImpactedSelectionForBaselineWithTasks is ImpactedSelectionForBaseline
// resolved onto tasks: each changed file selects the tasks that read it, and
// propagation carries tasks instead of whole projects (ADR 0044). A nil index
// keeps the project-level mapping, which is what a caller that could not load
// the workspace's extensions gets.
func ImpactedSelectionForBaselineWithTasks(
	ws *Workspace,
	baseline string,
	tasks TaskImpactIndex,
) (ImpactedSelection, error) {
	resolved, err := ResolveImpactedBaselineDetailed(ws, baseline)
	if err != nil {
		return ImpactedSelection{}, err
	}

	diff, err := git.DiffWorkingTree(ws.Root, resolved.Ref)
	if err != nil {
		return ImpactedSelection{Baseline: resolved.Ref, BaselineSource: resolved.Source}, err
	}

	if len(diff.Files) == 0 {
		return ImpactedSelection{Baseline: resolved.Ref, BaselineSource: resolved.Source, DiffBase: diff.Base}, nil
	}

	// The git diff is the only thing --impacted owns; everything after it — the
	// file→project mapping, cross-project assets, and graph propagation — is the
	// shared calculation in the model package, which the watch loop replans
	// against too.
	impact := traceChangeImpact(ws, diff.Files, ChangeImpactOptions{Tasks: tasks})
	return ImpactedSelection{
		Projects:         impact.Projects,
		Baseline:         resolved.Ref,
		BaselineSource:   resolved.Source,
		DiffBase:         diff.Base,
		ChangedFiles:     diff.Files,
		UncommittedFiles: diff.Uncommitted,
		UnownedRootFiles: impact.UnownedRootFiles,
		Trace:            impact.Trace,
	}, nil
}

// ResolveImpactedBaseline applies Putnami's --impacted baseline precedence:
// explicit flag, workspace config baseline, nearest configured epic branch,
// trunk (origin/HEAD, origin/main), local main/master, then the upstream
// tracking ref — never the current branch itself.
func ResolveImpactedBaseline(ws *Workspace, explicit string) (string, error) {
	resolved, err := ResolveImpactedBaselineDetailed(ws, explicit)
	return resolved.Ref, err
}

// ResolveImpactedBaselineDetailed is ResolveImpactedBaseline plus the
// resolution tier, for callers that explain fallback choices.
func ResolveImpactedBaselineDetailed(ws *Workspace, explicit string) (git.ResolvedBaseline, error) {
	if ws == nil {
		return git.ResolvedBaseline{}, fmt.Errorf("workspace is nil")
	}
	baseline := strings.TrimSpace(explicit)
	var epicBranches []string
	if ws.Config != nil {
		if baseline == "" {
			baseline = ws.Config.Baseline
		}
		epicBranches = ws.Config.EpicBranches
	}
	return git.ResolveBaselineDetailed(ws.Root, baseline, epicBranches)
}
