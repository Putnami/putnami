package workspace

import (
	"fmt"
	"sort"
	"strings"

	model "go.putnami.dev/cli/model/workspace"
	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// The declared-edge check.
//
// The reference graph is the one derived from real imports. An edge a manifest
// declares and no import backs is a PHANTOM: it propagates impact to a project
// that reads nothing of the change, it orders a schedule nothing waits for, and
// it opens an import boundary the visibility check can never enforce, because
// no import crosses it.
//
// It runs where the visibility check runs, at the end of every synchronization,
// and for the same reason: this is the first moment the graph is real. Every
// edge carries the manifest family it came from, and a provider attributes an
// import family only to an edge its own sources import — so "declared" here is
// the provider's own statement that nothing reads the target.
//
// THREE BACKINGS make an edge real, and an edge with any of them is never
// reported:
//
//   - an IMPORT: the provider attributed the edge to its own manifest family
//     after reading the project's sources;
//   - a CONTRACT: a generated client reads its provider's committed contract.
//     It backs the contract edge itself, not a `dependencies` entry that
//     duplicates it — see ContractBacked;
//   - a DECLARED INPUT: the dependent declares a file input that reads inside
//     the dependency (`options.<layer>.filePatterns`, `build.assets[].from`,
//     `options.generate.assets[].from`). The build hashes those bytes into the
//     dependent's key and a change to them selects it, so the edge is exactly
//     as real as an import — it is simply declared instead of imported.
//
// UNKNOWN is not DECLARED. A project for which no provider reported any
// `dependencySources` at all has an unattributed edge set: nothing has said
// which of its edges its sources import, and reading that silence as "none of
// them" would report every edge of a whole ecosystem as a phantom. Such a
// project is skipped and named once in the diagnostics. An entry ABSENT from a
// map a provider did report stays `declared`: there the provider answered, and
// the answer is that nothing imports it.
//
// Four classes, one per file a phantom can live in:
//
//  1. A putnami.json `dependencies` entry whose target no import and no
//     contract edge reaches.
//  2. A go.mod `require` or `replace` of a workspace module no Go file of the
//     project imports.
//  3. A package.json `workspace:` dependency (or `putnami.dependencies` entry)
//     naming a workspace package no source file of the project imports.
//     devDependencies are NOT in scope: a `workspace:` devDependency is how a
//     project requests an EXTENSION, and the build reads it by running it.
//  4. An orphan client.putnami.json: a committed generated-client manifest
//     whose `service.id` no provider of this workspace declares any more.
//
// The check reports; `putnami deps prune` repairs what it has a writer for and
// names the edit to make for the rest, and the check's advice says exactly
// that. The mode decides whether a report also refuses.

// DeclaredEdgeClass names the declaration a finding is about, which is the file
// that has to change for it to go away.
type DeclaredEdgeClass string

// The declared-edge classes.
const (
	// DeclaredEdgeProjectConfig is a putnami.json `dependencies` entry.
	DeclaredEdgeProjectConfig DeclaredEdgeClass = "putnami-json"
	// DeclaredEdgeOrphanClient is a committed generated-client manifest whose
	// provider no longer declares the service it names.
	DeclaredEdgeOrphanClient DeclaredEdgeClass = "client-manifest"
	// DeclaredEdgeManifest is an edge a PROVIDER reported and attributed to no
	// import: the project's own language manifest states it and its sources do
	// not read it. Which manifest that is, and how to edit it, belongs to the
	// ecosystem rather than to the workspace model — `deps prune` resolves it
	// the way it already resolves a `deps remove` target.
	DeclaredEdgeManifest DeclaredEdgeClass = "manifest"
)

// DeclaredEdgeFinding is one declaration no import backs.
type DeclaredEdgeFinding struct {
	// Project is the ID of the project whose file carries the declaration.
	Project string
	// ProjectName is that project's resolved name.
	ProjectName string
	// Entry is the dependency spelling the declaration carries. It is the
	// value `deps prune` removes, and it is empty for an orphan manifest.
	Entry string
	// Target is the ID of the project the declaration names, empty when the
	// declaration names nothing this workspace contains (an orphan manifest).
	Target string
	// TargetName is that project's resolved name.
	TargetName string
	// Class names the declaration, and therefore the repair.
	Class DeclaredEdgeClass
	// File is the workspace-relative file to edit, empty when no single file
	// can be named.
	File string
	// ContractBacked reports that a contract edge already reaches the target.
	// The declaration is then not only unbacked but HARMFUL: it re-creates the
	// full dependency edge the contract edge exists to avoid, folding the
	// provider's key into the client's instead of the contract digest alone.
	ContractBacked bool
}

// DeclaredEdgesMode is the severity switch, in the vocabulary the workspace's
// other graph policies use: `off` skips the check, `report` publishes findings
// as warnings without changing an exit code, `enforce` refuses the graph.
type DeclaredEdgesMode string

// Declared-edge modes.
const (
	// DeclaredEdgesModeOff does not evaluate the check at all.
	DeclaredEdgesModeOff DeclaredEdgesMode = "off"
	// DeclaredEdgesModeReport publishes findings as warnings. The run continues.
	DeclaredEdgesModeReport DeclaredEdgesMode = "report"
	// DeclaredEdgesModeEnforce refuses a graph that carries a phantom edge.
	DeclaredEdgesModeEnforce DeclaredEdgesMode = "enforce"
)

// DefaultDeclaredEdgesMode is the mode of a workspace that declares none.
//
// It is `report` for one release, exactly like the visibility check it sits
// beside: a workspace has to be able to run `putnami deps prune` before the
// check refuses its graph, and the repair edits files the user owns.
const DefaultDeclaredEdgesMode = DeclaredEdgesModeReport

// declaredEdgesOptionNamespace and declaredEdgesOptionKey locate the switch in
// the workspace document: `options.workspace.declaredEdges`. It is a sibling of
// `options.workspace.visibility` rather than a member of one shared block,
// because the two checks refuse for different reasons and a workspace has to be
// able to enforce one while it still repairs the other.
const (
	declaredEdgesOptionNamespace = "workspace"
	declaredEdgesOptionKey       = "declaredEdges"
)

// DeclaredEdgeDiagnosticCode is the stable machine-readable reason attached to
// every finding, so a consumer branches on it instead of matching prose.
const DeclaredEdgeDiagnosticCode = "workspace.declared_edge"

// ValidDeclaredEdgesModes is the closed set, in severity order.
var ValidDeclaredEdgesModes = []DeclaredEdgesMode{
	DeclaredEdgesModeOff, DeclaredEdgesModeReport, DeclaredEdgesModeEnforce,
}

// Valid reports whether m is a member of the closed set.
func (m DeclaredEdgesMode) Valid() bool {
	for _, known := range ValidDeclaredEdgesModes {
		if m == known {
			return true
		}
	}
	return false
}

// ResolveDeclaredEdgesMode reads the switch from the workspace document.
//
// An unknown value is an error rather than a fallback, for the reason the
// visibility switch gives: the value decides whether a phantom edge fails the
// run, and resolving a typo to the default would answer a question nobody asked
// — in the direction that reports less.
func ResolveDeclaredEdgesMode(cfg *wsproto.Config) (DeclaredEdgesMode, error) {
	if cfg == nil {
		return DefaultDeclaredEdgesMode, nil
	}
	raw, ok := cfg.Options[declaredEdgesOptionNamespace][declaredEdgesOptionKey]
	if !ok || raw == nil {
		return DefaultDeclaredEdgesMode, nil
	}
	text, ok := raw.(string)
	if !ok {
		return DefaultDeclaredEdgesMode, fmt.Errorf(
			"options.%s.%s must be a string (one of %s)",
			declaredEdgesOptionNamespace, declaredEdgesOptionKey, declaredEdgesModeNames())
	}
	mode := DeclaredEdgesMode(text)
	if !mode.Valid() {
		return DefaultDeclaredEdgesMode, fmt.Errorf(
			"unknown options.%s.%s %q (want one of %s)",
			declaredEdgesOptionNamespace, declaredEdgesOptionKey, text, declaredEdgesModeNames())
	}
	return mode, nil
}

func declaredEdgesModeNames() string {
	names := make([]string, 0, len(ValidDeclaredEdgesModes))
	for _, mode := range ValidDeclaredEdgesModes {
		names = append(names, string(mode))
	}
	sort.Strings(names)
	return fmt.Sprintf("%v", names)
}

// UnattributedProjects names, in project order, every project that carries
// dependency edges and for which no provider reported any provenance at all.
//
// Its edges are not judged, and the answer is stated rather than silently
// skipped: a workspace has to be able to see that a whole ecosystem is outside
// the check rather than conclude it is clean.
func UnattributedProjects(ws *Workspace) []string {
	if ws == nil {
		return nil
	}
	var unattributed []string
	for _, project := range ws.Projects {
		if project != nil && attributionUnavailable(project) {
			unattributed = append(unattributed, project.Name)
		}
	}
	return unattributed
}

// attributionUnavailable reports that a project has edges and no provider said
// anything about any of them.
func attributionUnavailable(project *Project) bool {
	return len(project.Dependencies) > 0 && len(project.DependencySources) == 0
}

// DeclaredEdgeFindings returns every declaration of the workspace's resolved
// graph that no import, contract or declared input backs, sorted by
// (project, class, entry) so two runs over one tree report the same list in the
// same order.
func DeclaredEdgeFindings(ws *Workspace) []DeclaredEdgeFinding {
	if ws == nil || ws.Graph == nil {
		return nil
	}
	assets := CollectProjectAssetPaths(ws)
	var findings []DeclaredEdgeFinding
	for _, project := range ws.Projects {
		findings = append(findings, projectDeclaredEdges(ws, project, assets)...)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Project != findings[j].Project {
			return findings[i].Project < findings[j].Project
		}
		if findings[i].Class != findings[j].Class {
			return findings[i].Class < findings[j].Class
		}
		return findings[i].Entry < findings[j].Entry
	})
	return findings
}

// projectDeclaredEdges evaluates one project's declarations.
func projectDeclaredEdges(ws *Workspace, project *Project,
	assets map[string][]string) []DeclaredEdgeFinding {
	if project == nil || attributionUnavailable(project) {
		return nil
	}
	imported := backedTargets(ws, project, assets)
	authored := authoredDependencySet(project)
	contractProvider := ws.Graph.ContractProviderOf(project.ID)

	// An entry NO declaration of this project's own and NO provider answer
	// carries — the artifact reference the model materializes as an edge —
	// matches neither case below and is left alone: it names a built artifact
	// rather than an import, and no source file will ever back it.
	var findings []DeclaredEdgeFinding
	for _, entry := range project.Dependencies {
		target := ws.ProjectByID(dependencyTargetID(ws, entry))
		if target == nil || imported[target.ID] {
			continue
		}
		source, reported := project.DependencySources[entry]
		switch {
		case authored[entry]:
			findings = append(findings, DeclaredEdgeFinding{
				Project: project.ID, ProjectName: project.Name, Entry: entry,
				Target: target.ID, TargetName: target.Name,
				Class: DeclaredEdgeProjectConfig,
				File:  projectRelativePath(project, wsproto.ConfigFilename),
				// The contract edge survives the removal and keeps ordering
				// the client after its provider, which is what makes this
				// entry removable rather than load-bearing.
				ContractBacked: target.ID == contractProvider,
			})
		case reported && !source.IsImport():
			findings = append(findings, DeclaredEdgeFinding{
				Project: project.ID, ProjectName: project.Name, Entry: entry,
				Target: target.ID, TargetName: target.Name,
				Class:          DeclaredEdgeManifest,
				ContractBacked: target.ID == contractProvider,
			})
		}
	}
	if orphan := orphanClientFinding(ws, project); orphan != nil {
		findings = append(findings, *orphan)
	}
	return findings
}

// orphanClientFinding reports a committed generated-client manifest whose
// service no provider of this workspace declares.
//
// The manifest binds the client to a contract; with no provider answering to
// its service identity, the contract edge does not exist, the client's key
// mixes a contract nobody produces, and a regeneration has no source. The
// repair is a deletion, never an edit, so this finding names the file and stops
// there.
func orphanClientFinding(ws *Workspace, project *Project) *DeclaredEdgeFinding {
	if project.GeneratedClient == nil || ws.Graph.ContractProviderOf(project.ID) != "" {
		return nil
	}
	return &DeclaredEdgeFinding{
		Project: project.ID, ProjectName: project.Name,
		Entry: project.GeneratedClient.ServiceID,
		Class: DeclaredEdgeOrphanClient,
		File:  project.GeneratedClient.ManifestPath,
	}
}

// backedTargets collects the project IDs this project really reads: through a
// REAL import, or through a file input it declares inside them.
//
// One backing is enough. The target is read, whatever else also declares it.
func backedTargets(ws *Workspace, project *Project, assets map[string][]string) map[string]bool {
	backed := make(map[string]bool, len(project.Dependencies))
	for entry, source := range project.DependencySources {
		if !source.IsImport() {
			continue
		}
		if target := dependencyTargetID(ws, entry); target != "" {
			backed[target] = true
		}
	}
	declared := model.DeclaredInputRoots(project)
	roots := make([]string, 0, len(declared)+len(assets[project.ID]))
	roots = append(roots, declared...)
	roots = append(roots, assets[project.ID]...)
	if len(roots) == 0 {
		return backed
	}
	for _, target := range ws.Projects {
		if target == nil || target.ID == project.ID || backed[target.ID] {
			continue
		}
		for _, root := range roots {
			if model.InputRootReadsProject(root, target.Path) {
				backed[target.ID] = true
				break
			}
		}
	}
	return backed
}

// authoredDependencySet is the putnami.json `dependencies` list, in the exact
// spellings the file carries — the values `deps prune` removes from it.
func authoredDependencySet(project *Project) map[string]bool {
	if project.Config == nil {
		return nil
	}
	authored := make(map[string]bool, len(project.Config.Dependencies))
	for _, entry := range project.Config.Dependencies {
		authored[entry] = true
	}
	return authored
}

// dependencyTargetID resolves one dependency spelling — an ID or a name — to a
// project ID, or "" for an entry this workspace does not contain.
func dependencyTargetID(ws *Workspace, entry string) string {
	if strings.HasPrefix(entry, "/") {
		if project := ws.ProjectByID(entry); project != nil {
			return project.ID
		}
		return ""
	}
	if project := ws.ProjectByName(entry); project != nil {
		return project.ID
	}
	return ""
}

// DeclaredEdgeDiagnostics renders one diagnostic per finding, at the severity
// the mode asks for. `off` produces nothing — the check is not evaluated,
// rather than evaluated and muted.
func DeclaredEdgeDiagnostics(ws *Workspace, mode DeclaredEdgesMode) []diag.Diagnostic {
	if ws == nil || mode == DeclaredEdgesModeOff {
		return nil
	}
	findings := DeclaredEdgeFindings(ws)
	unattributed := UnattributedProjects(ws)
	if len(findings) == 0 && len(unattributed) == 0 {
		return nil
	}
	diagnostics := make([]diag.Diagnostic, 0, len(findings)+len(unattributed))
	for _, project := range unattributed {
		// Always a warning, whatever the mode: a check that could not run is
		// not a violation, and refusing a graph over it would fail every
		// workspace whose provider has not learned to attribute yet.
		diagnostics = append(diagnostics, diag.Warningf(DeclaredEdgeDiagnosticCode, project,
			"declared-edge attribution unavailable for %s: provider reports no dependencySources", project))
	}
	for _, finding := range findings {
		message := DeclaredEdgeMessage(finding)
		location := finding.File
		if location == "" {
			location = finding.Project
		}
		if mode == DeclaredEdgesModeEnforce {
			diagnostics = append(diagnostics, diag.Errorf(DeclaredEdgeDiagnosticCode, location, "%s", message))
			continue
		}
		diagnostics = append(diagnostics, diag.Warningf(DeclaredEdgeDiagnosticCode, location, "%s", message))
	}
	return diagnostics
}

// DeclaredEdgeMessage states one finding in the terms of the file that carries
// it, and names the repair.
func DeclaredEdgeMessage(finding DeclaredEdgeFinding) string {
	if finding.Class == DeclaredEdgeOrphanClient {
		return fmt.Sprintf(
			"%s commits a generated client manifest for service %q, and no project of this workspace declares that "+
				"service any more: the contract edge does not exist and nothing regenerates the client — delete %s "+
				"(and the generated files it inventories), or restore the provider's contract",
			finding.ProjectName, finding.Entry, finding.File)
	}
	suffix := "run `putnami deps prune` to remove it"
	if finding.Class == DeclaredEdgeManifest {
		// deps prune rewrites only the manifests it has a writer for and names
		// the edit to make by hand in any other, so the advice promises no more.
		suffix = "remove it from that manifest; `putnami deps prune` does so where it can edit the manifest, " +
			"and names the edit to make where it cannot"
	}
	if finding.ContractBacked {
		suffix = "the contract edge to the same provider survives its removal and keeps ordering this project after " +
			"it; " + suffix
	}
	if finding.Class == DeclaredEdgeManifest {
		return fmt.Sprintf(
			"%s declares a dependency on %s in its own language manifest that no import and no declared file input "+
				"of its own backs — the edge propagates impact and ordering nothing reads: %s",
			finding.ProjectName, finding.TargetName, suffix)
	}
	return fmt.Sprintf(
		"%s declares a dependency on %s in %s that no import and no declared file input of its own backs — the edge "+
			"propagates impact and ordering nothing reads: %s, or declare what it reads from %s as a file input",
		finding.ProjectName, finding.TargetName, finding.File, suffix, finding.TargetName)
}

// declaredEdgesRefusal is the enforced verdict: the graph carries edges the
// build does not perform, stated on the typed channel graph-dependent commands
// already fail on.
func declaredEdgesRefusal(findings []diag.Diagnostic) *wsproto.ProbeFailure {
	failure := wsproto.NewProbeFailure(wsproto.ProbeFailureDeclaredEdge, "",
		"%d declared dependency edge(s) no import backs (options.%s.%s is %q); "+
			"run `putnami deps prune`, which removes the ones it can edit and names the edit to make for the rest",
		len(findings), declaredEdgesOptionNamespace, declaredEdgesOptionKey, DeclaredEdgesModeEnforce)
	failure.Diagnostics = append(failure.Diagnostics, findings...)
	return failure
}
