package sdd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	archsdk "go.putnami.dev/sdk/extension/architecture"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// `architecture sync` regenerates the MECHANICAL half of existing manifests,
// and refuses the other half out loud.
//
// # What is mechanical
//
// Two things follow from the resolved workspace and nothing else:
//
//   - a project entry naming a project the workspace no longer contains. It is
//     already a hard `architecture.unknown_project` error, and removing it takes
//     no decision because there is nothing left to decide about.
//   - a binding whose project edge the graph no longer observes. It is already a
//     hard `architecture.declared_binding_unobserved` error, and removing it
//     RETIRES a permission, which is never the dangerous direction.
//
// # What is a permission, and therefore not mechanical
//
// Adding a binding AUTHORIZES a project edge. Sync proposes one only when the
// consumer domain ALREADY declares an import from that producer domain — the
// relation was negotiated, and the binding merely names which projects implement
// it — and even then the proposal is a diff a human reads. The write is not the
// authorization; the review of the diff is.
//
// When no such import exists, sync REFUSES and says so. It never writes an
// import, because an import is a contract between two domains: minimized facts,
// an access mode, consistency guarantees, a justification. A generator that
// invented one would be turning an observed dependency into a permission, which
// is exactly the "debt becomes permission" anti-pattern ADR 0001 exists to
// forbid. The undeclared edge stays a failing finding until somebody declares
// the contract.
//
// Two narrower cases are refused for the same reason: more than one candidate
// import (sync must not pick which contract authorizes an edge) and a
// planned-only candidate (the protocol forbids a binding on a planned target, so
// the decision is whether to promote the import, and that is a human's).
//
// Adding a PROJECT to a domain is refused by omission for the same reason. Which
// projects a domain contains is an authority statement, not an observation, so
// sync only drops entries the workspace no longer has.

// Architecture sync refusal reasons: the closed vocabulary of decisions sync
// declines to make.
const (
	// SyncRefusalNoDeclaredImport reports an observed cross-domain edge whose
	// consumer domain declares no import from that producer domain.
	SyncRefusalNoDeclaredImport = "no-declared-import"
	// SyncRefusalAmbiguousImport reports more than one candidate import.
	SyncRefusalAmbiguousImport = "ambiguous-import"
	// SyncRefusalPlannedImportOnly reports that every candidate import is
	// planned, and a planned target cannot carry a current binding.
	SyncRefusalPlannedImportOnly = "planned-import-only"
)

// ArchitectureSyncBinding names one exact project-dependency permission and the
// import it belongs to.
type ArchitectureSyncBinding struct {
	Import          string `json:"import"`
	ConsumerProject string `json:"consumerProject"`
	ProducerProject string `json:"producerProject"`
}

// ArchitectureSyncRefusal is one decision sync declined to make, with the exact
// edge that provoked it.
type ArchitectureSyncRefusal struct {
	Reason          string `json:"reason"`
	ConsumerDomain  string `json:"consumerDomain"`
	ProducerDomain  string `json:"producerDomain"`
	ConsumerProject string `json:"consumerProject"`
	ProducerProject string `json:"producerProject"`
	Message         string `json:"message"`
}

// ArchitectureSyncManifest is one manifest's suggestion: what sync would change
// and the exact canonical bytes it would leave behind.
type ArchitectureSyncManifest struct {
	Path            string                    `json:"path"`
	Domain          string                    `json:"domain"`
	Changed         bool                      `json:"changed"`
	Written         bool                      `json:"written"`
	RemovedProjects []string                  `json:"removedProjects"`
	AddedBindings   []ArchitectureSyncBinding `json:"addedBindings"`
	RemovedBindings []ArchitectureSyncBinding `json:"removedBindings"`
	// Contents is present only for a CHANGED manifest. An unchanged file is left
	// exactly as its author committed it — canonical member order or not, because
	// reordering a reviewed declaration is a diff nobody asked for.
	Contents string `json:"contents,omitempty"`
}

// ArchitectureSyncSummary is the compact projection both renderings share.
type ArchitectureSyncSummary struct {
	Manifests       int `json:"manifests"`
	Changed         int `json:"changed"`
	Written         int `json:"written"`
	RemovedProjects int `json:"removedProjects"`
	AddedBindings   int `json:"addedBindings"`
	RemovedBindings int `json:"removedBindings"`
	Refusals        int `json:"refusals"`
}

// ArchitectureSyncReport is what one sync run decided.
type ArchitectureSyncReport struct {
	Applied     bool                       `json:"applied"`
	Summary     ArchitectureSyncSummary    `json:"summary"`
	Manifests   []ArchitectureSyncManifest `json:"manifests"`
	Refusals    []ArchitectureSyncRefusal  `json:"refusals"`
	Diagnostics []diag.Diagnostic          `json:"diagnostics"`
}

// BuildArchitectureSyncResult computes the mechanical half of every declared
// manifest and, with apply set, writes the ones that changed.
//
// It fails on a structurally invalid repository rather than proposing anything:
// a suggestion derived from a document the protocol rejects would be a guess
// about what the author meant.
//
// A run with refusals still succeeds. Sync is an authoring aid, not a gate: the
// undeclared relations it will not grant are reported here and FAIL in
// `architecture validate`, which is where a verdict belongs.
func BuildArchitectureSyncResult(ws *workspace.Workspace, apply bool) (ArchitectureSyncReport, error) {
	report := ArchitectureSyncReport{
		Applied:     apply,
		Manifests:   []ArchitectureSyncManifest{},
		Refusals:    []ArchitectureSyncRefusal{},
		Diagnostics: []diag.Diagnostic{},
	}
	if ws == nil {
		return report, protocolcli.Usagef("architecture sync requires a resolved workspace")
	}
	// The same fail-closed refusal `architecture validate` applies. Over a
	// partial project view, "the graph no longer observes this binding" is
	// unanswerable, and sync would DELETE a permission on the strength of an
	// answer it could not give.
	if ws.HasWarningCode(workspace.WarningCodeProviderViewUnavailable) {
		return report, protocolcli.Usagef(
			"architecture sync needs a complete project view to reconcile bindings; this run received a partial one, " +
				"so a dependency that is absent cannot be told from one that was merely not selected " +
				"(run `putnami projects sync` when a provider probe is the cause)")
	}

	discovery := Discover(ws.Root)
	diagnostics := append([]diag.Diagnostic(nil), discovery.Diagnostics...)
	diagnostics = append(diagnostics, archproto.ValidateRepository(discovery.Sources)...)
	sortDiagnostics(diagnostics)
	report.Diagnostics = copyArchitectureDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		return report, WithResultData(protocolcli.InvalidConfigf(
			"architecture sync needs valid declarations; fix the reported errors first"), report)
	}

	observed := observedBindingKeys(DetectProjectDependencies(ws, discovery.Sources))
	authorized := authorizedBindingKeys(discovery.Sources)
	for _, source := range discovery.Sources {
		if source.Manifest == nil {
			continue
		}
		row, refusals, err := syncOneManifest(ws, source, observed, authorized, apply)
		if err != nil {
			return report, WithResultData(err, report)
		}
		report.Refusals = append(report.Refusals, refusals...)
		report.Manifests = append(report.Manifests, row)
	}
	report.Summary = summarizeArchitectureSync(report)
	return report, nil
}

// syncOneManifest computes one manifest's suggestion and, when applying, writes
// it.
func syncOneManifest(
	ws *workspace.Workspace,
	source archproto.ManifestSource,
	observed map[string]archproto.ObservedEdge,
	authorized map[string]string,
	apply bool,
) (ArchitectureSyncManifest, []ArchitectureSyncRefusal, error) {
	manifest := archproto.CanonicalManifest(source.Manifest)
	row := ArchitectureSyncManifest{
		Path:            source.Path,
		Domain:          manifest.Domain,
		RemovedProjects: []string{},
		AddedBindings:   []ArchitectureSyncBinding{},
		RemovedBindings: []ArchitectureSyncBinding{},
	}

	kept := make([]string, 0, len(manifest.Projects))
	for _, project := range manifest.Projects {
		if ws.ProjectByID(project) == nil {
			row.RemovedProjects = append(row.RemovedProjects, project)
			continue
		}
		kept = append(kept, project)
	}
	manifest.Projects = kept

	for index := range manifest.Imports {
		imported := &manifest.Imports[index]
		retained := make([]archproto.Binding, 0, len(imported.Bindings))
		for _, binding := range imported.Bindings {
			if _, seen := observed[syncBindingKey(binding.ConsumerProject, binding.ProducerProject)]; seen {
				retained = append(retained, binding)
				continue
			}
			row.RemovedBindings = append(row.RemovedBindings, ArchitectureSyncBinding{
				Import:          imported.ID,
				ConsumerProject: binding.ConsumerProject,
				ProducerProject: binding.ProducerProject,
			})
		}
		imported.Bindings = retained
	}

	refusals := make([]ArchitectureSyncRefusal, 0)
	for _, edge := range sortedObservedEdges(observed) {
		if edge.ConsumerDomain != manifest.Domain {
			continue
		}
		if _, granted := authorized[syncBindingKey(edge.ConsumerProject, edge.ProducerProject)]; granted {
			continue
		}
		target, refusal := candidateImport(manifest, edge)
		if target == nil {
			refusals = append(refusals, refusal)
			continue
		}
		target.Bindings = append(target.Bindings, archproto.Binding{
			Kind:            archproto.BindingProjectDependency,
			ConsumerProject: edge.ConsumerProject,
			ProducerProject: edge.ProducerProject,
		})
		row.AddedBindings = append(row.AddedBindings, ArchitectureSyncBinding{
			Import:          target.ID,
			ConsumerProject: edge.ConsumerProject,
			ProducerProject: edge.ProducerProject,
		})
	}

	row.Changed = len(row.RemovedProjects) > 0 || len(row.AddedBindings) > 0 || len(row.RemovedBindings) > 0
	if !row.Changed {
		return row, refusals, nil
	}

	contents, err := architectureManifestBytes(manifest)
	if err != nil {
		return row, refusals, err
	}
	row.Contents = string(contents)
	if !apply {
		return row, refusals, nil
	}
	if err := rewriteArchitectureManifest(ws.Root, source.Path, contents); err != nil {
		return row, refusals, protocolcli.Classify(
			fmt.Errorf("write %s: %w", source.Path, err), protocolcli.ErrInvalidConfig)
	}
	row.Written = true
	return row, refusals, nil
}

// candidateImport finds the one import that already authorizes this producer
// domain, or explains why sync will not choose.
func candidateImport(manifest *archproto.Manifest, edge archproto.ObservedEdge) (*archproto.Import, ArchitectureSyncRefusal) {
	var candidates []*archproto.Import
	planned := 0
	for index := range manifest.Imports {
		imported := &manifest.Imports[index]
		if imported.From.Domain != edge.ProducerDomain {
			continue
		}
		if imported.Status == archproto.StatusPlanned {
			planned++
			continue
		}
		candidates = append(candidates, imported)
	}
	if len(candidates) == 1 {
		return candidates[0], ArchitectureSyncRefusal{}
	}
	refusal := ArchitectureSyncRefusal{
		ConsumerDomain:  edge.ConsumerDomain,
		ProducerDomain:  edge.ProducerDomain,
		ConsumerProject: edge.ConsumerProject,
		ProducerProject: edge.ProducerProject,
	}
	switch {
	case len(candidates) > 1:
		refusal.Reason = SyncRefusalAmbiguousImport
		refusal.Message = fmt.Sprintf(
			"%s depends on %s and domain %s declares %d imports from %s; sync will not choose which contract authorizes the edge",
			edge.ConsumerProject, edge.ProducerProject, edge.ConsumerDomain, len(candidates), edge.ProducerDomain)
	case planned > 0:
		refusal.Reason = SyncRefusalPlannedImportOnly
		refusal.Message = fmt.Sprintf(
			"%s depends on %s and every import domain %s declares from %s is planned; a planned target cannot carry a current binding, "+
				"so promoting the import is the decision to make",
			edge.ConsumerProject, edge.ProducerProject, edge.ConsumerDomain, edge.ProducerDomain)
	default:
		refusal.Reason = SyncRefusalNoDeclaredImport
		refusal.Message = fmt.Sprintf(
			"%s depends on %s and domain %s declares no import from %s; sync does not write imports, so declare the contract first",
			edge.ConsumerProject, edge.ProducerProject, edge.ConsumerDomain, edge.ProducerDomain)
	}
	return nil, refusal
}

// architectureManifestBytes renders the reconciled manifest through the same SDK
// builder an authoring program uses, so a synced file and an authored one are
// the same document.
func architectureManifestBytes(manifest *archproto.Manifest) ([]byte, error) {
	builder := archsdk.NewDomain(manifest.Domain, manifest.Owner).
		Projects(manifest.Projects...).
		Owns(manifest.Owns...).
		Export(manifest.Exports...).
		Import(manifest.Imports...)
	contents, err := builder.CanonicalBytes()
	if err != nil {
		return nil, protocolcli.Classify(
			fmt.Errorf("the reconciled manifest for domain %s is not valid: %w", manifest.Domain, err),
			protocolcli.ErrInvalidConfig)
	}
	return contents, nil
}

func observedBindingKeys(edges []archproto.ObservedEdge) map[string]archproto.ObservedEdge {
	result := make(map[string]archproto.ObservedEdge, len(edges))
	for _, edge := range edges {
		result[syncBindingKey(edge.ConsumerProject, edge.ProducerProject)] = edge
	}
	return result
}

// authorizedBindingKeys indexes every binding the repository already declares,
// across all manifests. A binding is workspace-unique — ValidateRepository
// rejects the same edge authorized twice — so an edge another domain's manifest
// already grants is not a gap this run should fill.
func authorizedBindingKeys(sources []archproto.ManifestSource) map[string]string {
	result := make(map[string]string)
	for _, source := range sources {
		if source.Manifest == nil {
			continue
		}
		for _, imported := range source.Manifest.Imports {
			for _, binding := range imported.Bindings {
				result[syncBindingKey(binding.ConsumerProject, binding.ProducerProject)] = imported.ID
			}
		}
	}
	return result
}

func sortedObservedEdges(edges map[string]archproto.ObservedEdge) []archproto.ObservedEdge {
	result := make([]archproto.ObservedEdge, 0, len(edges))
	for _, edge := range edges {
		result = append(result, edge)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.ConsumerProject != right.ConsumerProject {
			return left.ConsumerProject < right.ConsumerProject
		}
		return left.ProducerProject < right.ProducerProject
	})
	return result
}

func syncBindingKey(consumerProject, producerProject string) string {
	return consumerProject + "\x00" + producerProject
}

func summarizeArchitectureSync(report ArchitectureSyncReport) ArchitectureSyncSummary {
	summary := ArchitectureSyncSummary{
		Manifests: len(report.Manifests),
		Refusals:  len(report.Refusals),
	}
	for _, manifest := range report.Manifests {
		if manifest.Changed {
			summary.Changed++
		}
		if manifest.Written {
			summary.Written++
		}
		summary.RemovedProjects += len(manifest.RemovedProjects)
		summary.AddedBindings += len(manifest.AddedBindings)
		summary.RemovedBindings += len(manifest.RemovedBindings)
	}
	return summary
}

// rewriteArchitectureManifest replaces an EXISTING manifest through a staged
// file and a rename inside the same contained root, so an interrupted run cannot
// leave a reviewed declaration half-written.
func rewriteArchitectureManifest(wsRoot, relative string, contents []byte) error {
	if code := validateArchitectureWritePath(relative); code != "" {
		return fmt.Errorf("%s", code)
	}
	root, err := os.OpenRoot(wsRoot)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // the write result is reported below

	target := filepath.FromSlash(relative)
	staged := target + ".putnami-sync"
	file, err := root.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		file.Close()            //nolint:errcheck // the write error is the one to report
		_ = root.Remove(staged) //nolint:errcheck // best-effort cleanup of a failed stage
		return err
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(staged) //nolint:errcheck // best-effort cleanup of a failed stage
		return err
	}
	if err := root.Rename(staged, target); err != nil {
		_ = root.Remove(staged) //nolint:errcheck // best-effort cleanup of a failed stage
		return err
	}
	return nil
}
