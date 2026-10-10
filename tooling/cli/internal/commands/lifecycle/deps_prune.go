package lifecycle

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// `putnami deps prune` — the repair half of the declared-edge check.
//
// The check reports every declaration no import backs. This removes them,
// through the writer that owns each file and never through a rewrite of its
// own:
//
//   - a putnami.json `dependencies` entry goes through core's project-config
//     writer, which preserves key order and leaves unrelated members
//     byte-identical;
//   - a Go module requirement is dropped with `go mod edit -droprequire` and
//     the module tidied with `go mod tidy`, through the go command `deps
//     remove` runs, so the module graph and go.sum stay consistent. It is not
//     `deps remove`'s `go get <module>@none`: that resolves the requirement it
//     removes, and a workspace module no proxy serves cannot be resolved
//     (goDepsEdit);
//   - a TypeScript package dependency has no removal path in this CLI yet —
//     `deps remove` refuses a TypeScript target for the same reason — so it is
//     reported with the edit to make, never half-applied: remove it from the
//     project's package.json, then run `putnami deps install`;
//   - an orphan client manifest is a deletion, not an edit: the repair is to
//     remove the generated target or restore its provider's contract, and
//     guessing which one the user means is not this command's call.
//
// Deciding WHICH ecosystem a manifest finding belongs to happens here rather
// than in the workspace model, which owns no language vocabulary: it is the
// same question `deps remove` already answers about its target.
//
// Refusing is the fail-closed half: a run that cannot repair every finding
// reports the ones it left, so "prune passed" never means "some phantom edges
// are still there".

// pruneRoute is what this command does about one finding.
type pruneRoute int

const (
	// pruneRouteKeep: reported, repaired by hand.
	pruneRouteKeep pruneRoute = iota
	// pruneRouteProjectConfig: removed from the project's putnami.json.
	pruneRouteProjectConfig
	// pruneRouteGoModule: removed from the project's Go module requirements.
	pruneRouteGoModule
)

// routedFinding pairs one finding with the repair it gets.
type routedFinding struct {
	finding workspace.DeclaredEdgeFinding
	route   pruneRoute
	// manifest is the workspace-relative language manifest that carries a
	// manifest finding: the go.mod this command edits, or the package.json it
	// names for a hand edit. It is empty when no single file can be named.
	manifest string
}

// DepsPrune removes the declared dependency edges no import backs.
//
// projectSelector narrows the work to one project by name; dryRun lists what
// would change and touches nothing.
func DepsPrune(ctx context.Context, wsRoot, projectSelector string, dryRun bool, env LifecycleEnv) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	if !ws.HasProbeView() {
		// Without a provider view every edge reads as a declaration, because no
		// provider has said which ones its sources import. Pruning from that
		// graph would delete real dependencies.
		return fmt.Errorf(
			"the workspace has no provider view yet, so no edge can be told from a declaration; " +
				"run `putnami install` (or any build) first, then `putnami deps prune`")
	}
	if err := requireFreshProviderView(ws); err != nil {
		return err
	}
	return prunePlan(ctx, wsRoot, ws, projectSelector, dryRun, env.out(), env)
}

// requireFreshProviderView refuses a provider view that predates the tree.
// workspace.Load adopts the recorded view without re-hashing its inputs, and
// this command runs no provider. After a hand edit of go.mod, or a new import,
// the findings would describe the previous tree: prune would report nothing,
// or drop a requirement an import now backs. Validate re-hashes the recorded
// inputs and starts no provider, as `putnami context map` does.
func requireFreshProviderView(ws *workspace.Workspace) error {
	snapshot, err := workspace.LoadSnapshot(ws.Root)
	if err != nil || snapshot == nil {
		return fmt.Errorf(
			"the workspace index could not be re-read to check that the provider view is current; " +
				"run `putnami install` (or any build) first, then `putnami deps prune`")
	}
	validity := snapshot.Validate(ws.Root)
	if validity.Valid {
		return nil
	}
	detail := validity.Reason
	if len(validity.Changed) > 0 {
		detail = "changed since it was recorded: " + strings.Join(validity.Changed, ", ")
	}
	return fmt.Errorf(
		"the provider view is stale (%s), so its declared edges may describe the previous tree; "+
			"run `putnami install` (or any build) first, then `putnami deps prune`", detail)
}

// prunePlan lists the findings and, unless the run is a dry one, repairs the
// ones it owns. It is the whole command minus the workspace load, so a test
// exercises both halves on a fixture. env runs the workspace installers when a
// Go module is edited on a host without a go command.
func prunePlan(ctx context.Context, wsRoot string, ws *workspace.Workspace,
	projectSelector string, dryRun bool, out io.Writer, env LifecycleEnv) error {
	routed := routeFindings(wsRoot, ws, selectedFindings(ws, projectSelector))
	if len(routed) == 0 {
		iox.Fprintln(out, "  No declared dependency edge without an import.")
		return nil
	}

	pruned := 0
	for _, entry := range routed {
		if entry.route != pruneRouteKeep {
			pruned++
		}
		iox.Fprintf(out, "  %s %s\n", pruneVerb(entry.route), pruneSummary(entry))
	}
	if dryRun {
		iox.Fprintf(out, "  %d declared edge(s) would be pruned; run without --dry-run to apply.\n", pruned)
		return nil
	}
	return applyPrune(ctx, wsRoot, ws, routed, pruned, depsCommand("prune", nil, projectSelector), out, env)
}

// selectedFindings is the check's answer, narrowed to one project when the
// caller named one.
func selectedFindings(ws *workspace.Workspace, projectSelector string) []workspace.DeclaredEdgeFinding {
	findings := workspace.DeclaredEdgeFindings(ws)
	selector := strings.TrimSpace(projectSelector)
	switch selector {
	case "", "*", ".", "[impacted]":
		return findings
	}
	selected := make([]workspace.DeclaredEdgeFinding, 0, len(findings))
	for _, finding := range findings {
		if finding.ProjectName == selector || finding.Project == selector {
			selected = append(selected, finding)
		}
	}
	return selected
}

// routeFindings decides the repair for each finding.
func routeFindings(wsRoot string, ws *workspace.Workspace,
	findings []workspace.DeclaredEdgeFinding) []routedFinding {
	routed := make([]routedFinding, 0, len(findings))
	for _, finding := range findings {
		entry := routedFinding{finding: finding, route: routeOf(wsRoot, ws, finding)}
		entry.manifest = manifestOf(wsRoot, ws, entry)
		routed = append(routed, entry)
	}
	return routed
}

// manifestOf names the language manifest that carries a manifest finding: the
// importer's go.mod for a Go module requirement, else its package.json when it
// has one. Any other manifest is not named, because guessing the wrong file is
// worse than naming none.
func manifestOf(wsRoot string, ws *workspace.Workspace, entry routedFinding) string {
	if entry.finding.Class != workspace.DeclaredEdgeManifest {
		return ""
	}
	importer := ws.ProjectByID(entry.finding.Project)
	if importer == nil {
		return ""
	}
	switch {
	case entry.route == pruneRouteGoModule:
		return filepath.ToSlash(filepath.Join(importer.Path, "go.mod"))
	case packageManifestExists(filepath.Join(wsRoot, importer.Path)):
		return filepath.ToSlash(filepath.Join(importer.Path, "package.json"))
	}
	return ""
}

// packageManifestExists reports whether dir holds a package.json file.
func packageManifestExists(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "package.json"))
	return err == nil && !info.IsDir()
}

// routeOf resolves one finding to its repair.
//
// A manifest finding is a Go module requirement when BOTH ends are Go modules:
// a require of a workspace module needs the target to be one. Anything else is
// reported rather than guessed at — editing the wrong manifest is worse than
// editing none.
func routeOf(wsRoot string, ws *workspace.Workspace, finding workspace.DeclaredEdgeFinding) pruneRoute {
	switch finding.Class {
	case workspace.DeclaredEdgeProjectConfig:
		return pruneRouteProjectConfig
	case workspace.DeclaredEdgeManifest:
		importer, target := ws.ProjectByID(finding.Project), ws.ProjectByID(finding.Target)
		if importer == nil || target == nil {
			return pruneRouteKeep
		}
		if goModExists(filepath.Join(wsRoot, importer.Path)) && goModExists(filepath.Join(wsRoot, target.Path)) {
			return pruneRouteGoModule
		}
		return pruneRouteKeep
	default:
		return pruneRouteKeep
	}
}

// pruneVerb names what this command does about one finding.
func pruneVerb(route pruneRoute) string {
	if route == pruneRouteKeep {
		return "keep "
	}
	return "prune"
}

// pruneSummary states one finding on a single line, naming the file the repair
// touches, and the edit to make by hand for a finding this command keeps.
func pruneSummary(entry routedFinding) string {
	finding := entry.finding
	if finding.Class == workspace.DeclaredEdgeOrphanClient {
		return fmt.Sprintf("%s [%s] service %q — delete %s",
			finding.ProjectName, finding.Class, finding.Entry, finding.File)
	}
	summary := fmt.Sprintf("%s → %s [%s] in %s",
		finding.ProjectName, finding.TargetName, finding.Class, pruneLocation(entry))
	if entry.route == pruneRouteKeep && finding.Class == workspace.DeclaredEdgeManifest {
		summary += " — " + manualManifestEdit(entry)
	}
	return summary
}

// pruneLocation is the file the repair edits: the one the check named when it
// could, otherwise the manifest that carries the finding.
func pruneLocation(entry routedFinding) string {
	if entry.finding.File != "" {
		return entry.finding.File
	}
	if entry.manifest != "" {
		return entry.manifest
	}
	return "its own language manifest"
}

// manualManifestEdit is the edit a manifest finding this command keeps needs
// by hand. A package.json dependency is removed from the file and the
// installers run again, the TypeScript path of `deps remove`'s documentation.
func manualManifestEdit(entry routedFinding) string {
	if strings.HasSuffix(entry.manifest, "package.json") {
		return fmt.Sprintf("remove the dependency on %s from %s, then run `putnami deps install`",
			entry.finding.TargetName, entry.manifest)
	}
	return fmt.Sprintf("remove the dependency on %s from %s's language manifest by hand",
		entry.finding.TargetName, entry.finding.ProjectName)
}

// applyPrune edits the files the routed findings name and refuses the rest.
// rerun is the prune command the user ran (goDepsCommand).
//
// The go the Go module edits run with is resolved once, before the first edit
// of any file. A go that cannot be found, or whose version probe timed out,
// stops the command with the workspace as it was, so the rerun it names reads
// the provider view this one read.
func applyPrune(ctx context.Context, wsRoot string, ws *workspace.Workspace,
	routed []routedFinding, pruned int, rerun string, out io.Writer, env LifecycleEnv) error {
	byProject := make(map[string][]routedFinding)
	var kept []routedFinding
	var goImporter *workspace.Project
	for _, entry := range routed {
		if entry.route == pruneRouteKeep {
			kept = append(kept, entry)
			continue
		}
		byProject[entry.finding.Project] = append(byProject[entry.finding.Project], entry)
		if entry.route == pruneRouteGoModule && goImporter == nil {
			goImporter = ws.ProjectByID(entry.finding.Project)
		}
	}

	projects := make([]string, 0, len(byProject))
	for id := range byProject {
		projects = append(projects, id)
	}
	sort.Strings(projects)

	// installErr is the failure of the workspace installers that installed the
	// go the Go module edits run with. The edits go on with that go, and the
	// command fails at the end.
	var goCmd goCommand
	if goImporter != nil {
		var err error
		if goCmd, err = goDepsCommand(ctx, wsRoot, goImporter.Path, "prune", rerun, env); err != nil {
			return err
		}
	}
	installErr := goCmd.installErr
	for _, id := range projects {
		project := ws.ProjectByID(id)
		if project == nil {
			return goDepsResult("prune", installErr, fmt.Errorf("project %s disappeared from the workspace while pruning", id))
		}
		if err := pruneProjectConfig(wsRoot, project, byProject[id]); err != nil {
			return goDepsResult("prune", installErr, err)
		}
		if err := pruneGoModule(ctx, wsRoot, ws, project, byProject[id], goCmd); err != nil {
			return goDepsResult("prune", installErr, err)
		}
	}
	iox.Fprintf(out, "  ✓ pruned %d declared edge(s) across %d project(s)\n", pruned, len(projects))
	return goDepsResult("prune", installErr, keptFindingsError(kept))
}

// keptFindingsError reports the findings this command leaves for a manual
// repair, each with the edit to make, or nil when it left none. A manifest
// finding names its file and the edit rather than the check's advice, which
// points back at this command.
func keptFindingsError(kept []routedFinding) error {
	if len(kept) == 0 {
		return nil
	}
	reasons := make([]string, 0, len(kept))
	for _, entry := range kept {
		if entry.finding.Class != workspace.DeclaredEdgeManifest {
			reasons = append(reasons, workspace.DeclaredEdgeMessage(entry.finding))
			continue
		}
		reasons = append(reasons, fmt.Sprintf(
			"%s declares a dependency on %s in %s that no import and no declared file input of its own backs, "+
				"and this command does not edit that manifest: %s",
			entry.finding.ProjectName, entry.finding.TargetName, pruneLocation(entry), manualManifestEdit(entry)))
	}
	return fmt.Errorf("%d finding(s) this command does not repair:\n  - %s",
		len(kept), strings.Join(reasons, "\n  - "))
}

// pruneProjectConfig rewrites one project's putnami.json `dependencies`,
// dropping the entries no import backs and leaving every other member of the
// file untouched.
func pruneProjectConfig(wsRoot string, project *workspace.Project, routed []routedFinding) error {
	drop := make(map[string]bool, len(routed))
	for _, entry := range routed {
		if entry.route == pruneRouteProjectConfig {
			drop[entry.finding.Entry] = true
		}
	}
	if len(drop) == 0 {
		return nil
	}
	if project.Config == nil {
		return fmt.Errorf("project %s declares dependencies in no config file", project.Name)
	}
	remaining := make([]string, 0, len(project.Config.Dependencies))
	for _, entry := range project.Config.Dependencies {
		if !drop[entry] {
			remaining = append(remaining, entry)
		}
	}
	// An emptied list is REMOVED rather than written as `[]`: the file then
	// reads as a project that declares no dependency, which is what it is.
	var value any
	if len(remaining) > 0 {
		value = remaining
	}
	if err := workspace.UpdateProjectConfigField(wsRoot, project, "dependencies", value); err != nil {
		return fmt.Errorf("update %s: %w", project.Name, err)
	}
	return nil
}

// pruneGoModule drops the unimported workspace requirements from one project's
// Go module with goCmd, the go `deps remove` runs (applyPrune resolves it).
func pruneGoModule(ctx context.Context, wsRoot string, ws *workspace.Workspace,
	project *workspace.Project, routed []routedFinding, goCmd goCommand) error {
	modules := make([]string, 0, len(routed))
	for _, entry := range routed {
		if entry.route != pruneRouteGoModule {
			continue
		}
		module := goModulePathOf(ws.ProjectByID(entry.finding.Target))
		if module == "" {
			return fmt.Errorf("cannot name the module of %s to remove it from %s",
				entry.finding.TargetName, project.Name)
		}
		modules = append(modules, module)
	}
	if len(modules) == 0 {
		return nil
	}
	sort.Strings(modules)
	return editGoModule(ctx, wsRoot, project, goCmd, "prune", modules)
}

// goModulePathOf is a project's module path: the identity its own manifest
// declares, which the probe reports as the project's source name.
func goModulePathOf(project *workspace.Project) string {
	if project == nil {
		return ""
	}
	if project.SourceName != "" {
		return project.SourceName
	}
	return project.Name
}
