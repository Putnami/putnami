// Package agentctx: the workspace orientation map.
//
// `putnami context map` renders the map from artifacts already on disk, as a
// map-reduce (internal/mapgen): one fragment per project from PROJECT-SCOPED
// inputs, then one reduce over the LIVE project set. The split is what lets a
// selection-scoped run stay cheap while the emitted map is always complete — a
// project not in the live set contributes nothing, so deletions and renames need
// no diffing logic, and the previous map is never an input to the new one.
//
// The map is EPHEMERAL CLI state under `.putnami/context-map/`, never
// committed, so this command has no drift to report and no `--check`: a run
// either refreshes those files or, with `--print`, renders to stdout and touches
// nothing. `putnami build` attaches the same reduce as a post-session finalizer
// (internal/engine/context_map.go) and the MCP `workspace_map` tool serves the
// same document rebuilt in memory, so the map stays correct without a verb.
package agentctx

import (
	"fmt"
	"os"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/mapgen"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ContextMapOptions are the resolved inputs of `putnami context map`. Project
// selection arrives as already-parsed GLOBAL flag values (`--projects`,
// `--impacted`, `--baseline`) so this package never depends on the flag parser
// or on the engine.
type ContextMapOptions struct {
	// Args are the raw remaining arguments; only `--print[=json|md]` is read
	// from them.
	Args []string
	// Projects is the raw `--projects` selector ("" selects every project).
	Projects string
	// Impacted requests the `--impacted` selection.
	Impacted bool
	// Baseline is the `--baseline` git ref backing `--impacted`.
	Baseline string
	// OutputFormat is the `--output` value ("jsonl", "cloud-logging", or "").
	OutputFormat string
}

// Print formats accepted by `--print`.
const (
	printFormatMarkdown = "md"
	printFormatJSON     = "json"
)

// ContextMapCommand is the `putnami context map [--print[=json|md]]
// [--projects <sel>] [--impacted]` entrypoint.
func ContextMapCommand(wsRoot string, opts ContextMapOptions) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	if err := requireResolvedIdentity(ws); err != nil {
		return err
	}
	printFormat, err := parsePrintFlag(opts.Args)
	if err != nil {
		return err
	}
	if printFormat != "" {
		return printWorkspaceMap(ws, printFormat)
	}

	selected, err := selectMapProjects(ws, opts)
	if err != nil {
		return err
	}
	report, err := mapgen.Generate(ws, selected)
	if err != nil {
		return err
	}
	return renderContextMap(opts.OutputFormat, report)
}

// parsePrintFlag reads `--print`, `--print=md`, or `--print=json` out of the raw
// args, returning "" when the flag is absent. Bare `--print` renders markdown:
// the reader of a piped map is a human or a file-first agent, and the machine
// path is `--print=json` (or `--output=jsonl` for the run REPORT — which is why
// this flag is not spelled `--output`).
//
// The format is accepted ATTACHED or as the next token, because the CLI's global
// parser normalizes every unknown `--flag=value` into two tokens before the
// remainder reaches a command (internal/cli/flags.go, splitInlineValues). What a
// user typed as `--print=json` therefore arrives here as `--print` `json`, and a
// caller that builds Args itself must not have to know that.
func parsePrintFlag(args []string) (string, error) {
	for i, arg := range args {
		name, value, hasValue := strings.Cut(arg, "=")
		if name != "--print" {
			continue
		}
		if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			value, hasValue = args[i+1], true
		}
		switch {
		case !hasValue, value == printFormatMarkdown:
			return printFormatMarkdown, nil
		case value == printFormatJSON:
			return printFormatJSON, nil
		default:
			return "", cmderr.Usagef("unknown --print format: %s (want %s or %s)",
				value, printFormatJSON, printFormatMarkdown)
		}
	}
	return "", nil
}

// printWorkspaceMap renders the map to stdout and writes NOTHING — not a
// fragment, not a document. It is the no-toolchain-state path: the same
// in-memory reduce the MCP tool serves, so what a caller pipes is what an agent
// would be told, whether or not a build ever ran here.
//
// --print owns stdout: the run report (`--output=jsonl`, or the human summary)
// is suppressed so the emitted bytes are exactly the document. A caller that
// wants the report shape drops --print.
func printWorkspaceMap(ws *workspace.Workspace, format string) error {
	resolved, err := mapgen.ResolveMap(ws)
	if err != nil {
		return err
	}
	if format == printFormatJSON {
		data, err := mapgen.RenderJSON(resolved.Map)
		if err != nil {
			return err
		}
		iox.Write(os.Stdout, data)
		return nil
	}
	iox.Write(os.Stdout, mapgen.RenderMarkdown(resolved.Map))
	return nil
}

// requireResolvedIdentity refuses to render the map from a DEGRADED identity.
//
// A project's language identity and its dependency
// edges come from the merged provider view, recorded in
// `.putnami/workspace-index.json`. A workspace that has adopted none — a fresh
// clone, where that file is gitignored — still loads, but every project resolves
// from authored config alone and the graph carries zero provider-derived edges.
// The build attachment never sees that state (the engine probes before the
// finalizer runs), so a `context map` run on a snapshot-less clone would render
// a strictly smaller map than a build renders, and an agent reading it would
// conclude the workspace has no dependency edges — a confident wrong answer,
// which is worse than a refusal naming its own repair.
//
// The guard is conditioned on the workspace DECLARING extensions rather than on
// HasProbeView alone: a workspace with no extensions has no provider to ask, so
// config-only resolution is the whole truth there and refusing would be wrong.
//
// It also refuses a STALE view, not just a missing one: the snapshot records
// the digests of its probe inputs, and Validate re-hashes them without running
// any provider, so the check costs file reads only.
func requireResolvedIdentity(ws *workspace.Workspace) error {
	if ws.Config == nil || len(ws.Config.Extensions.Names()) == 0 {
		return nil
	}
	if !ws.HasProbeView() {
		return cmderr.InvalidConfigf(
			"the workspace has no resolved provider view, so project identity and dependency edges " +
				"would be incomplete and the workspace map would render smaller than a build renders it; " +
				"run `putnami projects sync` first")
	}
	// An ADOPTED view is not enough: workspace.Load restores the recorded
	// snapshot after structural validation only, never after checking
	// its input digests — that re-probe belongs to the engine's run pipeline,
	// which this direct command deliberately does not enter. So after a
	// go.mod / package.json / pyproject.toml edit, the loaded identity and
	// dependency edges are the PREVIOUS tree's, and rendering them would be a
	// confident wrong answer. Same philosophy as the refusal above: a refusal
	// naming its own repair beats a stale map an agent will trust.
	snapshot, err := workspace.LoadSnapshot(ws.Root)
	if err != nil || snapshot == nil {
		// Load already warned about an unreadable index and adopted nothing —
		// which the HasProbeView branch above reports. An adopted view with no
		// readable snapshot cannot happen; refuse conservatively if it does.
		return cmderr.InvalidConfigf(
			"the workspace index could not be re-read to check freshness; run `putnami projects sync` first")
	}
	if validity := snapshot.Validate(ws.Root); !validity.Valid {
		detail := validity.Reason
		if len(validity.Changed) > 0 {
			detail = "changed inputs: " + strings.Join(validity.Changed, ", ")
		}
		return cmderr.InvalidConfigf(
			"the recorded provider view is stale (%s), so project identity and dependency edges "+
				"may describe the previous tree; run `putnami projects sync` (or any build) first", detail)
	}
	return nil
}

// selectMapProjects resolves which projects get their fragment REFRESHED. It
// never narrows the rendered map: every live project is reduced either way, so a
// wrong-looking selection can only cost time, never correctness.
//
// `--impacted` with an empty impacted set is a legitimate no-op selection (the
// map still renders in full from validated fragments), which is why it does not
// error the way an explicit `--projects` selector matching nothing does.
func selectMapProjects(ws *workspace.Workspace, opts ContextMapOptions) ([]*workspace.Project, error) {
	if opts.Impacted {
		impacted, _, err := workspace.ImpactedProjectsWithBaseline(ws, opts.Baseline)
		if err != nil {
			return nil, protocolcli.Classify(fmt.Errorf("--impacted failed: %w", err), protocolcli.ErrInvalidConfig)
		}
		return impacted, nil
	}
	if opts.Projects == "" {
		return sortedProjectsByID(ws.Projects), nil
	}
	selected := workspace.FilterProjects(ws, workspace.FilterOptions{
		Projects:     opts.Projects,
		DirectTarget: true,
		ScopeIndex:   ws.ScopeIndex,
	})
	if len(selected) == 0 {
		return nil, cmderr.NotFoundf("no projects matched: %s", opts.Projects)
	}
	return sortedProjectsByID(selected), nil
}

// sortedProjectsByID copies and orders a project slice so the fragment refresh
// order (and therefore any error the run reports first) is deterministic.
func sortedProjectsByID(projects []*workspace.Project) []*workspace.Project {
	out := append([]*workspace.Project(nil), projects...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// renderContextMap writes the run report: the success v2 envelope in structured
// mode, a human summary otherwise. A refresh has no failure verdict to report —
// the map cannot be "wrong", only absent or rewritten.
func renderContextMap(outputFormat string, report mapgen.Report) error {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		_, err := protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL,
			protocolcli.NewResultV2("context map", report, nil))
		return err
	}
	printContextMapHuman(report)
	return nil
}

// printContextMapHuman renders the report for a terminal.
func printContextMapHuman(report mapgen.Report) {
	iox.Fprintf(os.Stdout, "\n  Mapped %d project(s) (%d fragment(s) rebuilt, %d reused).\n",
		report.Projects, report.FragmentsBuilt, report.FragmentsReused)
	if len(report.Outputs) == 0 {
		iox.Fprintf(os.Stdout, "    %s and %s already up to date.\n", mapgen.JSONPath, mapgen.MarkdownPath)
	}
	for _, out := range report.Outputs {
		iox.Fprintf(os.Stdout, "    wrote %s\n", out)
	}
	iox.Fprintln(os.Stdout)
}
