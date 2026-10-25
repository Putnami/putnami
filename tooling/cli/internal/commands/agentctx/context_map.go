// Package agentctx: the workspace orientation map.
//
// `putnami context map` renders the map from artifacts already on disk, as a
// map-reduce (internal/mapgen): one fragment per project from PROJECT-SCOPED
// inputs, then one reduce over the LIVE project set. A project not in the live
// set contributes nothing, so deletions and renames need no diffing logic.
//
// The command prints the map and writes nothing: Markdown by default, and with
// `--output=json` the map document as the result data — the same document the
// MCP `workspace_map` tool serves. `--projects` and `--impacted` narrow the
// printed projects; the reduce still runs over the whole live set, so every
// printed entry carries its complete edges. A build writes no map.
package agentctx

import (
	"fmt"
	"os"
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
	// Projects is the raw `--projects` selector ("" selects every project).
	Projects string
	// Impacted requests the `--impacted` selection.
	Impacted bool
	// Baseline is the `--baseline` git ref backing `--impacted`.
	Baseline string
	// OutputFormat is the `--output` value ("jsonl", "json", or "" for Markdown).
	OutputFormat string
}

// ContextMapCommand is the `putnami context map [--projects <sel>]
// [--impacted] [--output <format>]` entrypoint. It builds the map in memory,
// narrows it to the selection, and prints it. It writes nothing.
func ContextMapCommand(wsRoot string, opts ContextMapOptions) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	if err := requireResolvedIdentity(ws); err != nil {
		return err
	}
	keep, err := selectMapProjects(ws, opts)
	if err != nil {
		return err
	}
	resolved, err := mapgen.ResolveMap(ws, nil)
	if err != nil {
		return err
	}
	printed := scopeWorkspaceMap(resolved.Map, MapSectionAll, keep)

	if protocolcli.OutputMode(opts.OutputFormat).IsStructured() {
		_, err := protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL,
			protocolcli.NewResultV2("context map", printed, nil))
		return err
	}
	iox.Write(os.Stdout, mapgen.RenderMarkdown(printed))
	return nil
}

// requireResolvedIdentity refuses to render the map from a DEGRADED identity.
//
// A project's language identity and its dependency
// edges come from the merged provider view, recorded in
// `.putnami/workspace-index.json`. A workspace that has adopted none — a fresh
// clone, where that file is gitignored — still loads, but every project resolves
// from authored config alone and the graph carries zero provider-derived edges.
// A `context map` run on a snapshot-less clone would render a strictly smaller
// map than the engine's resolved view gives, and an agent reading it would
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
				"would be incomplete and the workspace map would render smaller than the resolved workspace gives it; " +
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

// selectMapProjects resolves the projects the printed map keeps, as a project-id
// set. A nil set keeps every project: no selection flag was given.
//
// `--impacted` with an empty impacted set is a legitimate selection: it prints a
// map with no project, which is why it does not error the way an explicit
// `--projects` selector matching nothing does.
func selectMapProjects(ws *workspace.Workspace, opts ContextMapOptions) (map[string]bool, error) {
	if opts.Impacted {
		impacted, _, err := workspace.ImpactedProjectsWithBaseline(ws, opts.Baseline)
		if err != nil {
			return nil, protocolcli.Classify(fmt.Errorf("--impacted failed: %w", err), protocolcli.ErrInvalidConfig)
		}
		return projectIDSet(impacted), nil
	}
	if opts.Projects == "" {
		return nil, nil
	}
	selected := workspace.FilterProjects(ws, workspace.FilterOptions{
		Projects:     opts.Projects,
		DirectTarget: true,
		ScopeIndex:   ws.ScopeIndex,
	})
	if len(selected) == 0 {
		return nil, cmderr.NotFoundf("no projects matched: %s", opts.Projects)
	}
	return projectIDSet(selected), nil
}

// projectIDSet returns the ids of projects as a set, empty but never nil, so
// an empty selection still narrows.
func projectIDSet(projects []*workspace.Project) map[string]bool {
	ids := make(map[string]bool, len(projects))
	for _, p := range projects {
		if p != nil {
			ids[p.ID] = true
		}
	}
	return ids
}
