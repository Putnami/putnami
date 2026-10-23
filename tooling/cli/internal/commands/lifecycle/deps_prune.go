package lifecycle

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
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
//   - a language manifest entry goes through the deps-edit job of the
//     importer's extension, as `deps remove` edits it: the job receives the
//     workspace-relative directories of the targets, and drops the
//     requirement each one's manifest names (the Go extension drops the module
//     path the target's go.mod declares, then tidies the module);
//   - a manifest entry whose importer has no extension with a deps-edit job
//     is reported with the edit to make, never half-applied: remove it from
//     the manifest the workspace provider read the importer from, then run
//     `putnami deps install`;
//   - an orphan client manifest is a deletion, not an edit: the repair is to
//     remove the generated target or restore its provider's contract, and
//     guessing which one the user means is not this command's call.
//
// The extension that edits a manifest finding is the one `deps remove` would
// run for the importer (projectJobProviders): the core names no language.
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
	// pruneRouteManifest: removed from the project's language manifest by the
	// deps-edit job of its extension.
	pruneRouteManifest
)

// routedFinding pairs one finding with the repair it gets.
type routedFinding struct {
	finding workspace.DeclaredEdgeFinding
	route   pruneRoute
	// manifest is the workspace-relative file the workspace provider read the
	// importer from, which this command names for the hand edit of a manifest
	// finding it keeps. It is empty when the provider reported none.
	manifest string
	// editor is the extension whose deps-edit job edits a manifest finding.
	editor string
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
	editors, err := projectJobProviders(ctx, wsRoot, ws, depsEditJob)
	if err != nil {
		return err
	}
	return prunePlan(ctx, wsRoot, ws, editors, projectSelector, dryRun, env.out(), env)
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
// ones it owns. It is the whole command minus the workspace load and the
// discovery of the editors, the extension whose deps-edit job edits each
// project's manifest by project ID, so a test exercises both halves on a
// fixture. env runs the deps-edit jobs.
func prunePlan(ctx context.Context, wsRoot string, ws *workspace.Workspace, editors map[string]string,
	projectSelector string, dryRun bool, out io.Writer, env LifecycleEnv) error {
	routed := routeFindings(ws, editors, selectedFindings(ws, projectSelector))
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
	return applyPrune(ctx, wsRoot, ws, routed, pruned, out, env)
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
func routeFindings(ws *workspace.Workspace, editors map[string]string,
	findings []workspace.DeclaredEdgeFinding) []routedFinding {
	routed := make([]routedFinding, 0, len(findings))
	for _, finding := range findings {
		entry := routedFinding{finding: finding, route: routeOf(ws, editors, finding)}
		if entry.route == pruneRouteManifest {
			entry.editor = editors[finding.Project]
		}
		entry.manifest = manifestOf(ws, entry)
		routed = append(routed, entry)
	}
	return routed
}

// manifestOf names the file the workspace provider read the importer of a
// manifest finding from, when this command keeps the finding. It is empty when
// the provider reported no file: guessing one is worse than naming none.
func manifestOf(ws *workspace.Workspace, entry routedFinding) string {
	if entry.finding.Class != workspace.DeclaredEdgeManifest || entry.route != pruneRouteKeep {
		return ""
	}
	importer := ws.ProjectByID(entry.finding.Project)
	if importer == nil {
		return ""
	}
	return importer.SourceFile
}

// routeOf resolves one finding to its repair.
//
// A manifest finding goes to the deps-edit job of the importer's extension
// (editors) when both ends are workspace projects. Anything else is reported
// rather than guessed at: editing the wrong manifest is worse than editing
// none.
func routeOf(ws *workspace.Workspace, editors map[string]string, finding workspace.DeclaredEdgeFinding) pruneRoute {
	switch finding.Class {
	case workspace.DeclaredEdgeProjectConfig:
		return pruneRouteProjectConfig
	case workspace.DeclaredEdgeManifest:
		importer, target := ws.ProjectByID(finding.Project), ws.ProjectByID(finding.Target)
		if importer == nil || target == nil || editors[importer.ID] == "" {
			return pruneRouteKeep
		}
		return pruneRouteManifest
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
// could, otherwise the manifest that carries the finding, or the one the
// editor's deps-edit job edits.
func pruneLocation(entry routedFinding) string {
	if entry.finding.File != "" {
		return entry.finding.File
	}
	if entry.manifest != "" {
		return entry.manifest
	}
	if entry.editor != "" {
		return "the manifest " + entry.editor + " edits"
	}
	return "its own language manifest"
}

// manualManifestEdit is the edit a manifest finding this command keeps needs
// by hand: the dependency is removed from the manifest the provider read the
// importer from, and the installers run again. Without a named manifest it
// points at the importer's language manifest in general terms.
func manualManifestEdit(entry routedFinding) string {
	if entry.manifest != "" {
		return fmt.Sprintf("remove the dependency on %s from %s, then run `putnami deps install`",
			entry.finding.TargetName, entry.manifest)
	}
	return fmt.Sprintf("remove the dependency on %s from %s's language manifest by hand",
		entry.finding.TargetName, entry.finding.ProjectName)
}

// applyPrune edits the files the routed findings name and refuses the rest.
//
// It edits each project's putnami.json first, then runs the deps-edit job of
// each editor once, with the manifest findings of every project it edits. The
// job puts the manifests it edits back when it fails, and a failure puts every
// putnami.json this command edited back as it was, so a rerun reads the
// provider view this one read.
func applyPrune(ctx context.Context, wsRoot string, ws *workspace.Workspace,
	routed []routedFinding, pruned int, out io.Writer, env LifecycleEnv) error {
	byProject := make(map[string][]routedFinding)
	var kept []routedFinding
	for _, entry := range routed {
		if entry.route == pruneRouteKeep {
			kept = append(kept, entry)
			continue
		}
		byProject[entry.finding.Project] = append(byProject[entry.finding.Project], entry)
	}

	projects := make([]string, 0, len(byProject))
	for id := range byProject {
		projects = append(projects, id)
	}
	sort.Strings(projects)

	edited := make([]*workspace.Project, 0, len(projects))
	configs := make([]string, 0, len(projects))
	for _, id := range projects {
		project := ws.ProjectByID(id)
		if project == nil {
			return fmt.Errorf("project %s disappeared from the workspace while pruning", id)
		}
		edited = append(edited, project)
		configs = append(configs, wsproto.ResolveFile(filepath.Join(wsRoot, project.Path), wsproto.ConfigFilename))
	}
	snapshot, err := snapshotFiles(configs...)
	if err != nil {
		return err
	}
	for _, project := range edited {
		if err := pruneProjectConfig(wsRoot, project, byProject[project.ID]); err != nil {
			return snapshot.restoreAfter(err)
		}
	}
	if err := pruneManifests(ctx, wsRoot, ws, edited, byProject, env); err != nil {
		return snapshot.restoreAfter(err)
	}
	iox.Fprintf(out, "  ✓ pruned %d declared edge(s) across %d project(s)\n", pruned, len(projects))
	return keptFindingsError(kept)
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

// pruneManifests runs the deps-edit job of each editor once, in name order,
// with one prune edit per project it edits: the workspace-relative directories
// of the targets whose requirement the project's manifest drops. A job puts
// back the manifests it edits when it fails; the manifests an earlier editor's
// job edited stay edited.
func pruneManifests(ctx context.Context, wsRoot string, ws *workspace.Workspace,
	edited []*workspace.Project, byProject map[string][]routedFinding, env LifecycleEnv) error {
	byEditor := map[string][]depsEdit{}
	for _, project := range edited {
		editor := ""
		var targets []string
		for _, entry := range byProject[project.ID] {
			if entry.route != pruneRouteManifest {
				continue
			}
			target := ws.ProjectByID(entry.finding.Target)
			if target == nil {
				return fmt.Errorf("project %s disappeared from the workspace while pruning", entry.finding.Target)
			}
			editor = entry.editor
			targets = append(targets, filepath.ToSlash(target.Path))
		}
		if len(targets) == 0 {
			continue
		}
		sort.Strings(targets)
		byEditor[editor] = append(byEditor[editor], depsEdit{project: project, modules: targets})
	}
	editors := make([]string, 0, len(byEditor))
	for editor := range byEditor {
		editors = append(editors, editor)
	}
	sort.Strings(editors)
	for _, editor := range editors {
		if err := runDepsEdit(ctx, wsRoot, editor, "prune", byEditor[editor], false, env); err != nil {
			return err
		}
	}
	return nil
}
