package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/collaboration"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// preparedExtensionSelection turns one read-preparation pass into the
// extension selection an entry path serves from: the prepared root of every
// lock-pinned registry extension, an empty root for one whose exact release
// was not prepared, and the names of the latter, which must not serve.
// extension.SelectPreparedExtensions drops an empty root, so an unprepared
// release is never loaded from a stale stable link. The collaboration
// commands select through it.
func preparedExtensionSelection(report lifecycle.ReadPreparationReport) (map[string]string, []string) {
	roots := make(map[string]string, len(report.Extensions)+len(report.Issues))
	unavailable := make([]string, 0, len(report.Issues))
	for _, item := range report.Extensions {
		roots[item.Name] = item.Root
	}
	for _, issue := range report.Issues {
		if issue.Name == "" {
			continue
		}
		roots[issue.Name] = ""
		unavailable = append(unavailable, issue.Name)
	}
	return roots, unavailable
}

// graphPreparationTimeout bounds one graph refresh for `version`: exact
// lock-pinned artifact selection, local runtime preparation, provider probes
// and the atomic index publish. Individual probes have their own tighter
// transport bound; this deadline also caps lock contention with another CLI.
const graphPreparationTimeout = 60 * time.Second

// synchronizeWorkspaceGraph brings the recorded provider graph up to date with
// the tree: it asks only the providers whose inputs changed since the recorded
// probe, and none when nothing changed. It runs no lifecycle hooks, dependency
// installers or Cloud setup, and fails closed when it cannot record a usable
// view.
func synchronizeWorkspaceGraph(ctx context.Context, wsRoot string, cfg *wsproto.Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, graphPreparationTimeout)
	defer cancel()

	workspace.InvalidateLoadCache(wsRoot)
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load cold workspace: %w", err)
	}
	if cfg == nil {
		cfg = ws.Config
	}
	prepared := lifecycle.PrepareReadOnly(ctx, wsRoot, cfg)
	if err := requireCompleteReadPreparation(prepared); err != nil {
		return err
	}
	preparedRoots := make(map[string]string, len(prepared.Extensions)+len(prepared.Issues))
	for _, item := range prepared.Extensions {
		preparedRoots[item.Name] = item.Root
	}

	projectPaths := make([]string, len(ws.Projects))
	for i, project := range ws.Projects {
		projectPaths[i] = project.Path
	}
	discovered, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil {
		return fmt.Errorf("discover workspace providers: %w", err)
	}
	discovered.Extensions = extension.SelectPreparedExtensions(discovered.Extensions, preparedRoots)
	if err := requireCompleteProviderDiscovery(prepared, discovered); err != nil {
		return err
	}
	if _, err := engine.PrepareWorkspaceGraph(ctx, ws, discovered); err != nil {
		return fmt.Errorf("prepare workspace graph: %w", err)
	}

	workspace.InvalidateLoadCache(wsRoot)
	if view := workspace.RecordedIndexView(wsRoot, time.Now()); !view.Usable() {
		return fmt.Errorf("workspace graph preparation produced no usable index: %s", view.Message)
	}
	return nil
}

func requireCompleteReadPreparation(prepared lifecycle.ReadPreparationReport) error {
	if len(prepared.Issues) == 0 {
		return nil
	}
	failures := make([]string, 0, len(prepared.Issues))
	for _, issue := range prepared.Issues {
		identity := issue.Name
		if issue.Version != "" {
			identity += "@" + issue.Version
		}
		failures = append(failures, fmt.Sprintf("%s: %s", identity, issue.Reason))
	}
	slices.Sort(failures)
	return fmt.Errorf("exact workspace extension preparation is incomplete (%s); run `putnami install` to repair it, then retry",
		strings.Join(failures, "; "))
}

func requireCompleteProviderDiscovery(
	prepared lifecycle.ReadPreparationReport,
	discovered *extension.DiscoveryResult,
) error {
	failures := make([]string, 0)
	if discovered == nil {
		failures = append(failures, "extension discovery returned no result")
	} else {
		for _, skip := range discovered.Skipped {
			identity := skip.Name
			if identity == "" {
				identity = skip.Ref
			}
			failures = append(failures, fmt.Sprintf("%s: %v", identity, skip.Reason))
		}
		for _, item := range prepared.Extensions {
			if extension.FindExtensionByName(discovered.Extensions, item.Name) == nil {
				failures = append(failures, fmt.Sprintf("%s@%s: exact manifest did not load", item.Name, item.Version))
			}
		}
	}
	if len(failures) == 0 {
		return nil
	}
	slices.Sort(failures)
	return fmt.Errorf("workspace provider discovery is incomplete (%s); repair the named extension or upgrade Putnami, then retry",
		strings.Join(failures, "; "))
}

// catalogProjectSelection maps the already-resolved project narrowing of a
// provider tool request onto the selection value the shared resolver takes, so
// a provider receives the SAME resolution every other CLI surface runs —
// canonical filtering, canonical impact baseline, canonical ordering — instead
// of agreeing by convention.
//
// Tag and exclusion filters are deliberately absent from the request
// vocabulary: a second filter vocabulary on the wire would let the two sides
// disagree about what a tag means.
func catalogProjectSelection(selection collaboration.ProjectSelection) shared.ProjectSelection {
	return shared.ProjectSelection{
		Projects: selection.Projects,
		Impacted: selection.Impacted,
		Baseline: selection.Baseline,
	}
}

// toolSelectionWire projects the resolved selection onto the EXTENSION
// contract's selection block.
//
// It goes through ResolvedSelection.Wire() rather than reading the fields
// directly, so this mapping can never drift from the one a job subprocess gets:
// the job wire is the definition, and the tool wire is that definition's members
// under the tool contract's own type. protocols/job's
// TestToolSelectionMarshalsLikeTheJobSelection pins that the two encode to the
// same bytes; this function is what makes the claim true for one invocation.
func toolSelectionWire(resolved shared.ResolvedSelection) *extproto.ToolSelection {
	wire := resolved.Wire()
	return &extproto.ToolSelection{
		Mode:           wire.Mode,
		Scoped:         wire.Scoped,
		Baseline:       wire.Baseline,
		BaselineSource: wire.BaselineSource,
		ProjectIDs:     wire.ProjectIDs,
		EmptyImpact:    wire.EmptyImpact,
	}
}
