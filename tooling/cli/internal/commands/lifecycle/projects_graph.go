package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/inputresolve"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// projectRef is a project named in the answer of a graph query.
type projectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func refForProject(p *workspace.Project) projectRef {
	return projectRef{ID: p.ID, Name: p.Name, Path: p.Path}
}

func refsForIDs(ws *workspace.Workspace, ids []string) []projectRef {
	refs := make([]projectRef, 0, len(ids))
	for _, id := range ids {
		if p := ws.ProjectByID(id); p != nil {
			refs = append(refs, refForProject(p))
		}
	}
	return refs
}

// resolveGraphProject finds a project by ID, then by name, as projects
// describe does. A selector that names none is a not-found error.
func resolveGraphProject(ws *workspace.Workspace, selector string) (*workspace.Project, error) {
	proj := ws.ProjectByID(selector)
	if proj == nil {
		proj = ws.ProjectByName(selector)
	}
	if proj == nil {
		return nil, cmderr.Classify(fmt.Errorf("project not found: %s", selector), cmderr.ErrNotFound)
	}
	return proj, nil
}

// ProjectsOwner lists the projects that own a workspace-relative path: the
// nearest project whose directory holds it, plus every project that claims it
// as an asset. A path no project owns is reported, not an error.
func ProjectsOwner(wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, warnings *workspace.WarningReport) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") || strings.TrimSpace(args[0]) == "" {
		return cmderr.Usagef("path required: projects owner <path>")
	}
	path := args[0]

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	warnings.Report(ws)

	owners := workspace.ProjectOwnersForPath(ws, path)
	refs := make([]projectRef, 0, len(owners))
	for _, p := range owners {
		refs = append(refs, refForProject(p))
	}

	if outputFormat == "jsonl" {
		return printJSONLines(refs)
	}
	iox.Fprintln(os.Stdout)
	if len(refs) == 0 {
		iox.Fprintf(os.Stdout, "  No project owns %s\n\n", path)
		return nil
	}
	iox.Fprintf(os.Stdout, "  Owners of %s\n\n", path)
	printProjectRows(refs)
	iox.Fprintf(os.Stdout, "\n  %d %s\n\n", len(refs), plural(len(refs), "project", "projects"))
	return nil
}

// ProjectsDeps lists the projects a project depends on, or with --dependents
// the projects that depend on it, direct or with --transitive the whole
// closure. The project itself is never listed.
func ProjectsDeps(wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, warnings *workspace.WarningReport) error {
	const usage = "project name required: projects deps <project> [--dependents] [--transitive]"
	var selector string
	var dependents, transitive bool
	for _, arg := range args {
		switch {
		case arg == "--dependents":
			dependents = true
		case arg == "--transitive":
			transitive = true
		case strings.HasPrefix(arg, "-"):
			return cmderr.Usagef("unknown flag for projects deps: %s", arg)
		case selector == "":
			selector = arg
		default:
			return cmderr.Usagef("projects deps takes one project, got %q and %q", selector, arg)
		}
	}
	if selector == "" {
		return cmderr.Usagef("%s", usage)
	}

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	warnings.Report(ws)

	proj, err := resolveGraphProject(ws, selector)
	if err != nil {
		return err
	}

	var ids []string
	switch {
	case !dependents && transitive:
		ids = ws.Graph.TransitiveDependenciesOf([]string{proj.ID})
	case !dependents:
		ids = append([]string(nil), ws.Graph.DependenciesOf(proj.ID)...)
	case transitive:
		ids = ws.Graph.TransitiveDependentsOf([]string{proj.ID})
	default:
		ids = append([]string(nil), ws.Graph.DependentsOf(proj.ID)...)
	}
	ids = withoutID(ids, proj.ID)
	sort.Strings(ids)
	refs := refsForIDs(ws, ids)

	direction := "dependencies"
	if dependents {
		direction = "dependents"
	}
	if outputFormat == "jsonl" {
		type depEntry struct {
			projectRef
			Direction  string `json:"direction"`
			Transitive bool   `json:"transitive"`
		}
		entries := make([]depEntry, 0, len(refs))
		for _, ref := range refs {
			entries = append(entries, depEntry{projectRef: ref, Direction: direction, Transitive: transitive})
		}
		return printJSONLines(entries)
	}

	reach := "direct"
	if transitive {
		reach = "transitive"
	}
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  Project:   %s\n", proj.ID)
	iox.Fprintf(os.Stdout, "  Listing:   %s %s\n\n", reach, direction)
	if len(refs) == 0 {
		iox.Fprintf(os.Stdout, "  (no %s %s)\n\n", reach, direction)
		return nil
	}
	printProjectRows(refs)
	iox.Fprintf(os.Stdout, "\n  %d %s\n\n", len(refs), plural(len(refs), "project", "projects"))
	return nil
}

// whyEdge is one hop of an impact path.
type whyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind is dependency | contract | extension-consumer.
	Kind string `json:"kind"`
}

// whyAnswer is the answer of projects why.
type whyAnswer struct {
	From     projectRef `json:"from"`
	To       projectRef `json:"to"`
	Impacted bool       `json:"impacted"`
	// Self is set when both selectors name the same project.
	Self      bool         `json:"self,omitempty"`
	Path      []projectRef `json:"path"`
	Edges     []whyEdge    `json:"edges"`
	TaskScope []string     `json:"taskScope"`
}

// ProjectsWhy answers whether a change to <from> impacts <to>: the path the
// impact walk takes with the kind of each edge, and the tasks <to> runs when
// the walk reaches it task-scoped. The walk is the one --impacted widens with,
// over the same task index, so the two cannot disagree about a pair.
func ProjectsWhy(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, warnings *workspace.WarningReport) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return cmderr.Usagef("unknown flag for projects why: %s", arg)
		}
	}
	if len(args) != 2 {
		return cmderr.Usagef("two projects required: projects why <from> <to>")
	}

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	warnings.Report(ws)

	from, err := resolveGraphProject(ws, args[0])
	if err != nil {
		return err
	}
	to, err := resolveGraphProject(ws, args[1])
	if err != nil {
		return err
	}

	var index workspace.TaskImpactIndex
	if from.ID != to.ID {
		index = graphTaskIndex(ctx, wsRoot, cfg, ws)
	}
	return printWhy(whyAnswerFor(ws, from, to, index), outputFormat)
}

// graphTaskIndex builds the task index the engine's selection builds, from the
// workspace's discovered extensions. It is nil when they cannot be discovered,
// and the walk then answers at project resolution.
func graphTaskIndex(ctx context.Context, wsRoot string, cfg *wsproto.Config, ws *workspace.Workspace) workspace.TaskImpactIndex {
	if cfg == nil {
		cfg = wsproto.Load(wsRoot)
	}
	projectPaths := make([]string, len(ws.Projects))
	for i, p := range ws.Projects {
		projectPaths[i] = p.Path
	}
	discovered, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil || discovered == nil {
		return nil
	}
	resolver := inputresolve.New(ctx, ws, discovered.Extensions, nil)
	return workspace.NewTaskIndex(ws, discovered.Extensions, resolver.Matcher())
}

func whyAnswerFor(ws *workspace.Workspace, from, to *workspace.Project, index workspace.TaskImpactIndex) whyAnswer {
	answer := whyAnswer{
		From:      refForProject(from),
		To:        refForProject(to),
		Path:      []projectRef{},
		Edges:     []whyEdge{},
		TaskScope: []string{},
	}
	if from.ID == to.ID {
		answer.Impacted, answer.Self = true, true
		answer.Path = refsForIDs(ws, []string{from.ID})
		return answer
	}

	reach := workspace.ImpactReachWithTasks(ws, from.ID, index)
	steps := reach.Trace.PathTo(to.ID)
	if len(steps) == 0 {
		return answer
	}
	ids := make([]string, 0, len(steps))
	for i, step := range steps {
		ids = append(ids, step.Project)
		if i > 0 {
			answer.Edges = append(answer.Edges, whyEdge{From: steps[i-1].Project, To: step.Project, Kind: string(step.Kind)})
		}
	}
	answer.Impacted = true
	answer.Path = refsForIDs(ws, ids)
	// The scope is the reach's, not the path's: a project the walk reached
	// task-scoped runs those tasks only, whichever edge the path names.
	if scope := reach.Trace.ScopeOf(to.ID); scope != nil {
		answer.TaskScope = scope
	}
	return answer
}

func printWhy(answer whyAnswer, outputFormat string) error {
	if outputFormat == "jsonl" {
		return printJSONLines([]whyAnswer{answer})
	}
	iox.Fprintln(os.Stdout)
	switch {
	case answer.Self:
		iox.Fprintf(os.Stdout, "  %s and %s are the same project; no dependency path is needed\n\n", answer.From.ID, answer.To.ID)
		return nil
	case !answer.Impacted:
		iox.Fprintf(os.Stdout, "  A change to %s does not impact %s\n\n", answer.From.ID, answer.To.ID)
		return nil
	}
	ids := make([]string, len(answer.Path))
	for i, ref := range answer.Path {
		ids[i] = ref.ID
	}
	iox.Fprintf(os.Stdout, "  A change to %s impacts %s\n\n", answer.From.ID, answer.To.ID)
	iox.Fprintf(os.Stdout, "  Path:   %s\n", strings.Join(ids, " → "))
	for i, edge := range answer.Edges {
		label := "Edges:"
		if i > 0 {
			label = ""
		}
		iox.Fprintf(os.Stdout, "  %-7s%s → %s (%s)\n", label, edge.From, edge.To, edge.Kind)
	}
	if len(answer.TaskScope) > 0 {
		iox.Fprintf(os.Stdout, "  Tasks:  %s (task-scoped)\n", strings.Join(answer.TaskScope, ", "))
	} else {
		iox.Fprintf(os.Stdout, "  Tasks:  all\n")
	}
	iox.Fprintln(os.Stdout)
	return nil
}

// --- helpers ---

func printProjectRows(refs []projectRef) {
	iox.Fprintf(os.Stdout, "  %-40s %-35s %s\n", "PROJECT", "NAME", "PATH")
	iox.Fprintf(os.Stdout, "  %-40s %-35s %s\n", "-------", "----", "----")
	for _, ref := range refs {
		iox.Fprintf(os.Stdout, "  %-40s %-35s %s\n", ref.ID, ref.Name, ref.Path)
	}
}

// printJSONLines writes one JSON object per line.
func printJSONLines[T any](entries []T) error {
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		iox.Fprintln(os.Stdout, string(data))
	}
	return nil
}

func withoutID(ids []string, remove string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != remove {
			out = append(out, id)
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
