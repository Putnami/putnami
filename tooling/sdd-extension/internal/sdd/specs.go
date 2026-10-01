package sdd

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	supportproto "go.putnami.dev/protocol/support"
	wsproto "go.putnami.dev/protocol/workspace"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// Spec diagnostic codes the CLI owns because no wire contract can produce them.
// features.* codes stay reserved for facts the protocol decides; these three
// answer questions that need the filesystem and the workspace model:
// which authored feature has no document, which publishable project has no
// reviewed support status or no owning feature, and which linked decision
// record is not actually on disk. ADR 0001 of protocols/features assigns that
// last one to the CLI explicitly.
const (
	// SpecCodeMissingSpec reports an authored feature with no durable spec.
	SpecCodeMissingSpec = "specs.missing_spec"
	// SpecCodeMissingSupportEntry reports a publishable project that the
	// reviewed support catalog does not classify.
	SpecCodeMissingSupportEntry = "specs.missing_support_entry"
	// SpecCodeMissingFeatureLink reports a publishable project with no authored
	// feature declared at, and no spec hosted at, its own root.
	SpecCodeMissingFeatureLink = "specs.missing_feature_link"
	// SpecCodeUnresolvedDecision reports a well-formed decision link that names
	// no record in the current worktree.
	SpecCodeUnresolvedDecision = "specs.unresolved_decision"
	// SpecCodeUnresolvedFeatureAuthority reports a project whose
	// `featureAuthority.owner` names no authored feature. A claim that cannot be
	// resolved must be louder than the missing link it explains away, or the
	// field becomes the cheapest way to silence completeness.
	SpecCodeUnresolvedFeatureAuthority = "specs.unresolved_feature_authority"
	// SpecCodeUnreadableSupportCatalog reports that the support catalog exists
	// but cannot be read, so support completeness was not assessed.
	SpecCodeUnreadableSupportCatalog = "specs.unreadable_support_catalog"
	// SpecCodeMissingSupportCatalog reports that the workspace declares no
	// support catalog at all, so support completeness was not assessed.
	SpecCodeMissingSupportCatalog = "specs.missing_support_catalog"
)

const (
	// specListOutcomeLimit bounds the outcomes one catalog row carries. The
	// catalog is a discovery surface: an agent reads identities here and asks
	// spec_context for the whole document.
	specListOutcomeLimit = 4
	// specSelectorMaxBytes bounds an accepted feature selector.
	specSelectorMaxBytes = 512
)

// SpecSummary is one compact catalog row: the feature a spec details, where the
// document lives, who owns that location, and enough of its intent to choose
// between rows. It deliberately carries counts rather than whole collections;
// the full document is one spec_context call away.
type SpecSummary struct {
	Feature string `json:"feature"`
	// Path is the exact workspace-relative document path.
	Path string `json:"path"`
	// Project is the owning project's name, empty at the workspace root.
	Project string `json:"project,omitempty"`
	// Outcomes are the first specListOutcomeLimit authored outcomes.
	Outcomes []string `json:"outcomes"`
	// OutcomeCount is the authored total, so a truncated list stays honest.
	OutcomeCount int `json:"outcomeCount"`
	NonGoals     int `json:"nonGoals"`
	Requirements int `json:"requirements"`
	Decisions    int `json:"decisions"`
	// Valid reports whether this exact document produced no error diagnostic.
	Valid bool `json:"valid"`
}

// SpecCatalogCounts is the compact discovery summary.
type SpecCatalogCounts struct {
	Specs               int `json:"specs"`
	AuthoredFeatures    int `json:"authoredFeatures"`
	FeaturesWithoutSpec int `json:"featuresWithoutSpec"`
}

// SpecCatalogReport is the shared `specs list` / list_specs projection.
type SpecCatalogReport struct {
	Revision    featureengine.Revision `json:"revision"`
	Selection   SelectionReport        `json:"selection"`
	Counts      SpecCatalogCounts      `json:"counts"`
	Specs       []SpecSummary          `json:"specs"`
	Diagnostics []diag.Diagnostic      `json:"diagnostics"`
}

// SpecDecisionLink is one durable decision record a spec links to, resolved
// against the current worktree. The protocol guarantees only that the link is
// contained and well-formed; whether the file exists is a filesystem fact, so
// the CLI answers it here rather than pretending the wire could.
type SpecDecisionLink struct {
	Path string `json:"path"`
	// Exists reports whether the record is a file in the current worktree.
	Exists bool `json:"exists"`
}

// SpecContextReport is the shared `specs inspect` / spec_context projection: one
// spec, the durable declaration of the feature it details, its resolved
// decision links, its exact source provenance, and the findings scoped to it.
// No design graph, evidence document, or capability manifest is read.
type SpecContextReport struct {
	Revision featureengine.Revision `json:"revision"`
	Feature  string                 `json:"feature"`
	// Source is the exact document provenance; empty when no spec exists yet.
	Source SpecSourceRef `json:"source"`
	// Declarations are the durable authored declarations that mint the feature.
	Declarations []FeatureAuthorityDeclaration `json:"declarations"`
	// Spec is the canonical parsed document, absent when the feature has none.
	Spec        *featureproto.Spec `json:"spec,omitempty"`
	Decisions   []SpecDecisionLink `json:"decisions"`
	Diagnostics []diag.Diagnostic  `json:"diagnostics"`
}

// SpecSourceRef is the exact provenance of one spec document.
type SpecSourceRef struct {
	Path    string `json:"path,omitempty"`
	Project string `json:"project,omitempty"`
}

// SpecValidationSummary counts the subjects validation considered, so a clean
// run still says what it looked at.
type SpecValidationSummary struct {
	Specs               int `json:"specs"`
	AuthoredFeatures    int `json:"authoredFeatures"`
	PublishableProjects int `json:"publishableProjects"`
}

// SpecCompleteness lists the gaps that are reported but never fail the run.
// A gap is authoring work the repository owner schedules; only a broken spec
// contract fails validation.
type SpecCompleteness struct {
	FeaturesWithoutSpec    []string `json:"featuresWithoutSpec"`
	ProjectsWithoutSupport []string `json:"projectsWithoutSupport"`
	ProjectsWithoutFeature []string `json:"projectsWithoutFeature"`
	// SupportAssessed reports whether the support catalog could be read at all.
	// Without it, ProjectsWithoutSupport is empty because the check did not run
	// — not because every project is classified.
	SupportAssessed bool `json:"supportAssessed"`
}

// SpecValidationReport is the typed data of both successful and failed runs.
type SpecValidationReport struct {
	Valid        bool                    `json:"valid"`
	Revision     featureengine.Revision  `json:"revision"`
	Selection    SelectionReport         `json:"selection"`
	Summary      SpecValidationSummary   `json:"summary"`
	Completeness SpecCompleteness        `json:"completeness"`
	Diagnostics  []diag.Diagnostic       `json:"diagnostics"`
	Counts       FeatureDiagnosticCounts `json:"diagnosticCounts"`
}

// SpecInitReport describes exactly what `specs init` did or would do. Contents
// is the canonical document either way, so --dry-run shows the same bytes the
// write path produces.
type SpecInitReport struct {
	Revision featureengine.Revision `json:"revision"`
	Feature  string                 `json:"feature"`
	Path     string                 `json:"path,omitempty"`
	Project  string                 `json:"project,omitempty"`
	Created  bool                   `json:"created"`
	DryRun   bool                   `json:"dryRun"`
	Contents string                 `json:"contents,omitempty"`
	// Declarations are the durable declarations the skeleton was derived from.
	Declarations []FeatureAuthorityDeclaration `json:"declarations"`
	Diagnostics  []diag.Diagnostic             `json:"diagnostics"`
}

// specRepository is the one discovery pass every spec surface shares: canonical
// specs, the authored feature catalog they resolve against, and the sorted
// protocol findings. Building it twice in one command would risk two answers to
// the same question, which is exactly what "do not create a second
// interpretation" forbids.
type specRepository struct {
	workspace *workspace.Workspace
	revision  featureengine.Revision
	discovery featureengine.SpecDiscoveryResult
	// selection is the resolved project selection this repository was built
	// under, reported verbatim by every spec surface.
	selection SelectionReport
	// scope narrows the projection; nil is the whole workspace.
	scope        *featureengine.Scope
	declarations []featureAuthorityEntry
	authored     []string
	// manifests are the strictly parsed authored manifests the workspace
	// declares, kept so a spec surface can read the requirements a feature
	// authors without a second discovery pass.
	manifests []featureproto.ManifestSource
	// byFeature keys the canonical document for each specified feature. When two
	// documents claim one feature the protocol reports features.duplicate_spec
	// and the first path in sorted order wins here, so the answer stays stable.
	byFeature map[string]featureengine.DiscoveredSpec
	// diagnostics are discovery, manifest, and repository findings, sorted.
	diagnostics []diag.Diagnostic
}

// loadSpecRepository discovers specs and the authored features they must
// resolve against, then validates them through the protocol's own repository
// validator. Authored identity comes only from durable manifests: build-time
// design graphs are generated artifacts, and a completeness answer that flips
// depending on whether the workspace has been built is not reviewable.
// The project selection is resolved BEFORE discovery, and the two tiers split
// the same way the feature engine's do: durable manifests and canonical spec
// documents are read at every root, because "one spec per feature" and "this
// feature is authored somewhere" are workspace-wide facts a subset cannot
// prove, while what the repository OWNS — its rows, its counts, its findings —
// is the selected projection.
func loadSpecRepository(ws *workspace.Workspace, selection Selection) (*specRepository, error) {
	if ws == nil {
		return nil, protocolcli.Classify(
			errors.New("workspace is required to discover specs"),
			protocolcli.ErrInvalidConfig,
		)
	}
	scope := selectedScope(ws, selection)
	manifests := featureengine.DiscoverAuthoredManifests(ws)
	// The identity tier runs first so spec discovery can tell a document that
	// details a selected feature from one that details somebody else's.
	selected := featureengine.SelectAuthored(scope, manifests.Manifests)
	discovery := featureengine.DiscoverSpecs(ws, scope)
	// Repository validation keeps the workspace-wide inputs: a duplicate spec or
	// an unauthored feature reference must be detected against every document,
	// not against the slice the caller happens to be looking at.
	authored := featureproto.AuthoredFeatureIDs(manifests.Manifests)

	diagnostics := append([]diag.Diagnostic(nil), scopeManifestDiagnostics(scope, manifests.Diagnostics)...)
	diagnostics = append(diagnostics, discovery.Diagnostics...)
	for _, finding := range featureproto.ValidateSpecRepository(discovery.Sources(), authored) {
		artifact, _, _ := strings.Cut(finding.Field, "#")
		if !discovery.InScope(artifact) {
			continue
		}
		diagnostics = append(diagnostics, finding)
	}

	byFeature := make(map[string]featureengine.DiscoveredSpec, len(discovery.Selected))
	for _, discovered := range discovery.Selected {
		if discovered.Spec == nil {
			continue
		}
		if _, exists := byFeature[discovered.Spec.Feature]; exists {
			continue
		}
		byFeature[discovered.Spec.Feature] = discovered
	}
	if !scope.Active() {
		selected = authored
	}
	report := newSelectionReport(selection)
	report.Features = len(selected)
	report.Specs = len(discovery.Selected)
	for _, external := range discovery.External {
		report.ExternalRecords = append(report.ExternalRecords, external.Path)
	}
	sort.Strings(report.ExternalRecords)
	repository := &specRepository{
		workspace:    ws,
		revision:     worktreeFeatureRevision(ws.Root),
		discovery:    discovery,
		selection:    report,
		scope:        scope,
		declarations: collectFeatureAuthorityDeclarations(ws, nil, featureengine.SeedManifests(scope, manifests.Manifests)),
		authored:     selected,
		manifests:    manifests.Manifests,
		byFeature:    byFeature,
		diagnostics:  featureDiagnostics(diagnostics),
	}
	return repository, nil
}

// scopeManifestDiagnostics keeps only the manifest findings the selection owns.
// A manifest this run could not parse mints no identity, so attribution by root
// is the only honest answer available.
func scopeManifestDiagnostics(scope *featureengine.Scope, findings []diag.Diagnostic) []diag.Diagnostic {
	if !scope.Active() {
		return findings
	}
	kept := make([]diag.Diagnostic, 0, len(findings))
	for _, finding := range findings {
		artifact, _, _ := strings.Cut(finding.Field, "#")
		if !scope.SeedRoot(featureengine.ManifestRoot(artifact)) {
			continue
		}
		kept = append(kept, finding)
	}
	return kept
}

// declarationsFor returns every durable declaration that mints one feature.
func (repository *specRepository) declarationsFor(feature string) []FeatureAuthorityDeclaration {
	declarations := make([]FeatureAuthorityDeclaration, 0, 1)
	for _, entry := range repository.declarations {
		if entry.id == feature {
			declarations = append(declarations, entry.declaration)
		}
	}
	return declarations
}

// scopedDiagnostics returns the findings attributed to one exact document.
// Fields are "<path>" or "<path>#<field>", the convention both discovery and
// the protocol validator already use, so scoping never needs a second index.
func (repository *specRepository) scopedDiagnostics(specPath string) []diag.Diagnostic {
	scoped := make([]diag.Diagnostic, 0)
	for _, finding := range repository.diagnostics {
		if diagnosticNamesPath(finding, specPath) {
			scoped = append(scoped, finding)
		}
	}
	return scoped
}

func diagnosticNamesPath(finding diag.Diagnostic, target string) bool {
	if target == "" {
		return false
	}
	artifact, _, _ := strings.Cut(finding.Field, "#")
	return artifact == target
}

// BuildSpecCatalogResult is the shared discovery projection behind `specs list`
// and the list_specs MCP tool. It reads only canonical specs/*.json documents
// and the durable manifests they resolve against, and it returns the rows the
// resolved project selection owns.
func BuildSpecCatalogResult(ws *workspace.Workspace, selection Selection) (SpecCatalogReport, error) {
	repository, err := loadSpecRepository(ws, selection)
	if err != nil {
		return SpecCatalogReport{}, err
	}
	summaries := make([]SpecSummary, 0, len(repository.discovery.Selected))
	for _, discovered := range repository.discovery.Selected {
		spec := discovered.Spec
		if spec == nil {
			continue
		}
		scoped := repository.scopedDiagnostics(discovered.Path)
		summaries = append(summaries, SpecSummary{
			Feature:      spec.Feature,
			Path:         discovered.Path,
			Project:      discovered.Project,
			Outcomes:     boundedStatements(spec.Outcomes, specListOutcomeLimit),
			OutcomeCount: len(spec.Outcomes),
			NonGoals:     len(spec.NonGoals),
			Requirements: len(spec.Requirements),
			Decisions:    len(spec.Decisions),
			Valid:        !diag.HasErrors(scoped),
		})
	}
	// Discovery degrades rather than fails: a broken document is reported (as a
	// diagnostic, and as valid:false on its row) instead of hiding every healthy
	// spec in the workspace. Failing the contract is `specs validate`'s job, and
	// an agent calling list_specs must still be able to find the specs that are
	// fine — the same degradation list_features already applies.
	report := SpecCatalogReport{
		Revision:  repository.revision,
		Selection: repository.selection,
		Counts: SpecCatalogCounts{
			Specs:               len(summaries),
			AuthoredFeatures:    len(repository.authored),
			FeaturesWithoutSpec: len(featuresWithoutSpec(repository)),
		},
		Specs:       summaries,
		Diagnostics: repository.diagnostics,
	}
	return report, nil
}

// BuildSpecContextResult returns one feature's spec with its durable
// declaration, resolved decision records, exact source path, and the findings
// scoped to that document. Selection is by exact authored feature ID: a spec is
// keyed by feature identity, and a filename never mints one.
func BuildSpecContextResult(ws *workspace.Workspace, selector string) (SpecContextReport, error) {
	if !validSpecSelector(selector) {
		return SpecContextReport{}, protocolcli.Usagef("specs inspect requires a valid <feature-id>")
	}
	repository, err := loadSpecRepository(ws, Selection{})
	if err != nil {
		return SpecContextReport{}, err
	}
	report := SpecContextReport{
		Revision:     repository.revision,
		Feature:      selector,
		Declarations: repository.declarationsFor(selector),
		Decisions:    []SpecDecisionLink{},
		Diagnostics:  []diag.Diagnostic{},
	}
	if len(report.Declarations) == 0 {
		report.Diagnostics = append(report.Diagnostics, diag.Errorf(featureproto.ErrorCodeUnknownFeature, "feature",
			"no authored declaration mints feature %q", selector))
		return report, WithResultData(
			protocolcli.NotFoundf("no authored feature declaration mints %q", selector), report)
	}

	discovered, found := repository.byFeature[selector]
	if !found {
		report.Diagnostics = append(report.Diagnostics, diag.Warningf(SpecCodeMissingSpec, "feature",
			"feature %q has no durable spec; create one with `putnami specs init %s`", selector, selector))
		return report, WithResultData(
			protocolcli.NotFoundf("feature %q has no spec under %s/", selector, featureproto.SpecDirectory), report)
	}
	report.Source = SpecSourceRef{Path: discovered.Path, Project: discovered.Project}
	report.Spec = featureproto.CanonicalSpec(discovered.Spec)
	links, findings := resolveSpecDecisions(repository.workspace, discovered)
	report.Decisions = links
	report.Diagnostics = featureDiagnostics(append(repository.scopedDiagnostics(discovered.Path), findings...))
	return report, specContractError("specs inspect", report.Diagnostics, report)
}

// BuildSpecValidationResult reuses the protocol's own repository validator and
// adds the two facts a wire contract cannot see: which decision links resolve to
// a record in this worktree, and which authored features and publishable
// projects are still unclaimed.
func BuildSpecValidationResult(ws *workspace.Workspace, selection Selection) (SpecValidationReport, error) {
	repository, err := loadSpecRepository(ws, selection)
	if err != nil {
		return SpecValidationReport{}, err
	}
	return buildSpecValidationFromRepository(repository)
}

// buildSpecValidationFromRepository is the shared reduction behind the plain
// validation surface and the criteria-emitting job path, so both read one
// repository and can never disagree about the same worktree.
func buildSpecValidationFromRepository(repository *specRepository) (SpecValidationReport, error) {
	diagnostics := append([]diag.Diagnostic(nil), repository.diagnostics...)
	for _, discovered := range repository.discovery.Selected {
		_, findings := resolveSpecDecisions(repository.workspace, discovered)
		diagnostics = append(diagnostics, findings...)
	}
	completeness, assessed, findings := assessSpecCompleteness(repository)
	diagnostics = append(diagnostics, findings...)

	report := SpecValidationReport{
		Valid:     !diag.HasErrors(diagnostics),
		Revision:  repository.revision,
		Selection: repository.selection,
		Summary: SpecValidationSummary{
			Specs:               len(repository.discovery.Selected),
			AuthoredFeatures:    len(repository.authored),
			PublishableProjects: assessed,
		},
		Completeness: completeness,
		Diagnostics:  featureDiagnostics(diagnostics),
		Counts:       countFeatureDiagnostics(diagnostics),
	}
	return report, specContractError("specs validate", report.Diagnostics, report)
}

// buildSpecInitResult derives the skeleton from the feature declaration and
// nothing else. It states the feature's own authored outcome, an empty
// requirement list, and no decisions: inventing a requirement sentence or an
// ADR link would put words the team never agreed to into a durable artifact.
func BuildSpecInitResult(ws *workspace.Workspace, selector string, dryRun bool) (SpecInitReport, error) {
	if !validSpecSelector(selector) {
		return SpecInitReport{}, protocolcli.Usagef("specs init requires a valid <feature-id>")
	}
	repository, err := loadSpecRepository(ws, Selection{})
	if err != nil {
		return SpecInitReport{}, err
	}
	report := SpecInitReport{
		Revision:     repository.revision,
		Feature:      selector,
		DryRun:       dryRun,
		Declarations: repository.declarationsFor(selector),
		Diagnostics:  []diag.Diagnostic{},
	}
	if len(report.Declarations) == 0 {
		report.Diagnostics = append(report.Diagnostics, diag.Errorf(featureproto.ErrorCodeUnknownFeature, "feature",
			"no authored declaration mints feature %q; declare it in %s first",
			selector, featureproto.ManifestFilename))
		return report, WithResultData(
			protocolcli.NotFoundf("no authored feature declaration mints %q", selector), report)
	}
	if existing, found := repository.byFeature[selector]; found {
		report.Path = existing.Path
		report.Project = existing.Project
		report.Diagnostics = append(report.Diagnostics, diag.Errorf(featureproto.ErrorCodeDuplicateSpec, existing.Path,
			"feature %q is already specified; edit that document instead", selector))
		return report, WithResultData(
			protocolcli.Usagef("feature %q already has a spec at %s", selector, existing.Path), report)
	}

	declaration := report.Declarations[0]
	root := specInitRoot(repository.workspace, declaration.Source)
	relative := path.Join(featureproto.SpecDirectory, specFilename(selector))
	report.Path = joinSpecPath(root, relative)
	report.Project = specProjectName(repository.workspace, root)

	spec := &featureproto.Spec{
		Schema:          featureproto.SpecSchemaURL,
		ProtocolVersion: featureproto.SpecProtocolVersion,
		Feature:         selector,
		Outcomes:        []string{declaration.Outcome},
		Requirements:    []featureproto.SpecRequirement{},
	}
	if findings := featureproto.ValidateSpec(spec); len(findings) > 0 {
		report.Diagnostics = featureDiagnostics(findings)
		return report, WithResultData(
			protocolcli.InvalidConfigf("the feature declaration cannot produce a valid spec skeleton"), report)
	}
	contents, err := featureproto.MarshalSpec(spec)
	if err != nil {
		return report, protocolcli.Classify(fmt.Errorf("encode spec: %w", err), protocolcli.ErrInvalidConfig)
	}
	report.Contents = string(contents)
	if dryRun {
		return report, nil
	}
	if err := writeSpecDocument(repository.workspace.Root, report.Path, contents); err != nil {
		if os.IsExist(err) {
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(featureproto.ErrorCodeInvalidPath, report.Path,
				"a file already exists at the target path; specs init never overwrites user content"))
			return report, WithResultData(
				protocolcli.Usagef("%s already exists; specs init never overwrites a file", report.Path), report)
		}
		return report, protocolcli.Classify(fmt.Errorf("write %s: %w", report.Path, err), protocolcli.ErrInvalidConfig)
	}
	report.Created = true
	return report, nil
}

// writeSpecDocument creates the document exclusively: O_EXCL is what makes "never
// overwrite" a property of the write itself rather than of a check that raced.
func writeSpecDocument(wsRoot, relative string, contents []byte) error {
	if code := validateSpecWritePath(relative); code != "" {
		return fmt.Errorf("%s", code)
	}
	root, err := os.OpenRoot(wsRoot)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // the write result is reported below

	target := filepath.FromSlash(relative)
	if parent := filepath.Dir(target); parent != "." {
		// os.Root refuses to traverse a symlink while creating parents. A
		// pre-existing specs/ link therefore cannot redirect this write outside
		// the selected workspace.
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	file, err := root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// validateSpecWritePath refuses anything but a contained workspace-relative
// path. The path is composed from a validated project root and a derived
// filename, so this guard should never fire; it exists because the one caller
// writes to disk.
func validateSpecWritePath(relative string) string {
	if relative == "" || strings.HasPrefix(relative, "/") || strings.ContainsAny(relative, "\\\x00") {
		return "spec path is not a contained workspace-relative path"
	}
	if path.Clean(relative) != relative {
		return "spec path is not canonical"
	}
	for _, segment := range strings.Split(relative, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "spec path is not a contained workspace-relative path"
		}
	}
	return ""
}

// specInitRoot returns the discovery root that owns the manifest which declared
// the feature, so a spec lands next to the declaration rather than in a
// workspace-root pile.
func specInitRoot(ws *workspace.Workspace, manifestPath string) string {
	root := path.Dir(manifestPath)
	if root == "." || root == "/" {
		return ""
	}
	for _, project := range ws.Projects {
		if project != nil && project.Path == root {
			return root
		}
	}
	return ""
}

func specProjectName(ws *workspace.Workspace, root string) string {
	if root == "" {
		return ""
	}
	for _, project := range ws.Projects {
		if project == nil || project.Path != root {
			continue
		}
		if project.Name != "" {
			return project.Name
		}
		return project.ID
	}
	return ""
}

// specFilename derives a readable default filename from the feature ID. It is a
// convenience only: identity lives in the document's feature field, so renaming
// the file later changes nothing.
func specFilename(feature string) string {
	return path.Base(feature) + ".json"
}

func joinSpecPath(root, relative string) string {
	if root == "" {
		return relative
	}
	return path.Join(root, relative)
}

// resolveSpecDecisions resolves one spec's decision links against the current
// worktree through the contained feature reader. The protocol guarantees the
// link is well-formed and inside the workspace; only the filesystem knows
// whether the record is actually there.
func resolveSpecDecisions(ws *workspace.Workspace, discovered featureengine.DiscoveredSpec) ([]SpecDecisionLink, []diag.Diagnostic) {
	links := make([]SpecDecisionLink, 0)
	var diagnostics []diag.Diagnostic
	if discovered.Spec == nil {
		return links, diagnostics
	}
	canonical := featureproto.CanonicalSpec(discovered.Spec)
	resolved := featureengine.ResolveDecisionRecords(ws, canonical.Decisions)
	for index, link := range canonical.Decisions {
		exists := resolved[link]
		links = append(links, SpecDecisionLink{Path: link, Exists: exists})
		if exists || !containedDecisionLink(link) {
			// A link the protocol already refused is reported by the protocol,
			// which deliberately keeps the offending path out of the message so an
			// absolute or private path cannot leak through a diagnostic. Repeating
			// it here would add nothing and undo that.
			continue
		}
		diagnostics = append(diagnostics, diag.Warningf(SpecCodeUnresolvedDecision,
			fmt.Sprintf("%s#decisions[%d]", discovered.Path, index),
			"the linked durable decision record is not present in this worktree"))
	}
	return links, diagnostics
}

// containedDecisionLink reports whether a link has the contained shape the spec
// contract accepts, so resolution never comments on a path the protocol already
// rejected. It is deliberately a containment check, not a second copy of the
// protocol's decision-link grammar.
func containedDecisionLink(link string) bool {
	if link == "" || strings.HasPrefix(link, "/") || strings.ContainsAny(link, "\\\x00") {
		return false
	}
	if !strings.HasSuffix(link, featureproto.DecisionExtension) {
		return false
	}
	if path.Clean(link) != link {
		return false
	}
	for _, segment := range strings.Split(link, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// assessSpecCompleteness reports the authoring gaps around the spec contract.
// Every finding is a warning: an unwritten spec or an unclassified project is
// scheduled work, while a broken document is a contract violation. The two must
// not share an exit code.
func assessSpecCompleteness(repository *specRepository) (SpecCompleteness, int, []diag.Diagnostic) {
	completeness := SpecCompleteness{
		FeaturesWithoutSpec:    featuresWithoutSpec(repository),
		ProjectsWithoutSupport: []string{},
		ProjectsWithoutFeature: []string{},
	}
	var diagnostics []diag.Diagnostic
	for _, feature := range completeness.FeaturesWithoutSpec {
		diagnostics = append(diagnostics, diag.Warningf(SpecCodeMissingSpec, "feature",
			"authored feature %q has no durable spec", feature))
	}

	catalog, catalogFindings := readSupportCatalog(repository.workspace)
	diagnostics = append(diagnostics, catalogFindings...)
	completeness.SupportAssessed = catalog != nil
	classified := make(map[supportSubjectIdentity]bool)
	if catalog != nil {
		for _, entry := range catalog.Entries {
			classified[supportSubjectIdentity{kind: entry.Kind, id: entry.ID}] = true
		}
	}

	linked := linkedFeatureRoots(repository)
	authoredIDs := make(map[string]bool, len(repository.authored))
	for _, feature := range repository.authored {
		authoredIDs[feature] = true
	}
	assessed := assessedProjects(repository.workspace, classified, repository.scope)
	for _, project := range assessed {
		name := project.Name
		if name == "" {
			name = project.ID
		}
		requiredSupport := supportSubjectIdentity{kind: supportKindForProject(project), id: name}
		if catalog != nil && !classified[requiredSupport] {
			completeness.ProjectsWithoutSupport = append(completeness.ProjectsWithoutSupport, name)
			diagnostics = append(diagnostics, diag.Warningf(SpecCodeMissingSupportEntry, project.Path,
				"publishable project %q has no %q entry in %s", name, requiredSupport.kind, supportproto.CatalogFilename))
		}
		if linked[project.Path] {
			continue
		}
		// A reviewed `featureAuthority` answers the question this check asks, so
		// it closes the gap instead of suppressing the report. An `owner` naming
		// no authored feature is reported and the project still counts as
		// unlinked: an unresolvable answer is worse than none.
		switch authority := projectFeatureAuthority(project); {
		case authority == nil:
		case authority.None != "":
			continue
		case authoredIDs[authority.Owner]:
			continue
		default:
			diagnostics = append(diagnostics, diag.Warningf(SpecCodeUnresolvedFeatureAuthority, project.Path,
				"project %q claims feature %q owns its outcome, but no authored feature has that id",
				name, authority.Owner))
		}
		completeness.ProjectsWithoutFeature = append(completeness.ProjectsWithoutFeature, name)
		diagnostics = append(diagnostics, diag.Warningf(SpecCodeMissingFeatureLink, project.Path,
			"publishable project %q declares no authored feature and hosts no spec", name))
	}
	sort.Strings(completeness.ProjectsWithoutSupport)
	sort.Strings(completeness.ProjectsWithoutFeature)
	return completeness, len(assessed), diagnostics
}

// supportSubjectIdentity mirrors the support protocol's identity boundary:
// kind and id are both required. A package entry and a protocol entry with the
// same id classify different subjects and must never satisfy one another.
type supportSubjectIdentity struct {
	kind supportproto.SubjectKind
	id   string
}

func supportKindForProject(project *workspace.Project) supportproto.SubjectKind {
	// A checked-in conformance test pins this convention. A protocol project
	// cannot silently lose the tag and change the support subject requested here.
	for _, tag := range project.Tags {
		if tag == "protocol" {
			return supportproto.SubjectKindProtocol
		}
	}
	return supportproto.SubjectKindPackage
}

// linkedFeatureRoots marks the roots that carry an explicit link to product
// intent: a durable manifest declaring at least one feature, or a spec hosted
// at that root. Membership is per ROOT, never per module, so a package family
// that shares one feature is not pushed into minting an artificial feature per
// technical module — the check reports the gap, it does not prescribe the shape
// of the fix.
func linkedFeatureRoots(repository *specRepository) map[string]bool {
	linked := make(map[string]bool)
	for _, entry := range repository.declarations {
		if entry.declaration.Kind != featureAuthorityManifest {
			continue
		}
		root := path.Dir(entry.declaration.Source)
		if root == "." {
			root = ""
		}
		linked[root] = true
	}
	for _, discovered := range repository.discovery.Selected {
		linked[discovered.Root] = true
	}
	return linked
}

// readSupportCatalog reads the reviewed support authority through the support
// protocol's own parser. A missing or unreadable catalog disables the support
// check with one explicit finding instead of reporting every project as
// unclassified, which would be an artifact of the reader rather than a fact.
func readSupportCatalog(ws *workspace.Workspace) (*supportproto.Catalog, []diag.Diagnostic) {
	catalogPath := filepath.Join(ws.Root, supportproto.CatalogFilename)
	data, err := os.ReadFile(catalogPath) //nolint:gosec // the workspace root plus one protocol filename
	if err != nil {
		if os.IsNotExist(err) {
			return nil, []diag.Diagnostic{diag.Warningf(SpecCodeMissingSupportCatalog, supportproto.CatalogFilename,
				"no support catalog was found, so project support completeness was not assessed")}
		}
		return nil, []diag.Diagnostic{diag.Warningf(SpecCodeUnreadableSupportCatalog, supportproto.CatalogFilename,
			"the support catalog could not be read, so project support completeness was not assessed")}
	}
	catalog, findings := supportproto.ParseAndValidateCatalog(data)
	if catalog == nil || diag.HasErrors(findings) {
		return nil, []diag.Diagnostic{diag.Warningf(SpecCodeUnreadableSupportCatalog, supportproto.CatalogFilename,
			"the support catalog is invalid (%s), so project support completeness was not assessed",
			firstDiagnosticCode(findings))}
	}
	return catalog, nil
}

func firstDiagnosticCode(findings []diag.Diagnostic) string {
	for _, finding := range findings {
		if finding.Severity == diag.Error {
			return finding.Code
		}
	}
	return supportproto.ErrorCodeParseError
}

// publishableProjects returns every project that declares it is published, in
// either shape the workspace accepts.
//
// The top-level `publish` array is only one of them. TypeScript packages are
// published through `options.publish.npm`, so keying completeness on the array
// alone silently excluded the whole TypeScript framework family, the three
// language extensions, @putnami/scaffold and putnami-extension-sdk — 17
// subjects the reviewed support catalog classifies as public packages. The
// check reported "63 publishable projects" and never once asked whether any of
// those 17 declared a feature or hosted a spec.
func publishableProjects(ws *workspace.Workspace) []*workspace.Project {
	projects := make([]*workspace.Project, 0)
	for _, project := range ws.Projects {
		if project != nil && (len(project.Publish) > 0 || declaresPublishOption(project)) {
			projects = append(projects, project)
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Path < projects[j].Path })
	return projects
}

// projectFeatureAuthority returns the reviewed answer a project gives for
// having no feature of its own, or nil when it gives none.
func projectFeatureAuthority(project *workspace.Project) *wsproto.ProjectFeatureAuthority {
	if project == nil || project.Config == nil {
		return nil
	}
	return project.Config.FeatureAuthority
}

// assessedProjects is the set completeness actually checks: everything that
// declares publication, plus every project the reviewed support catalog already
// classifies. The catalog is the authority on what Putnami calls a public
// package, so a classified subject must be assessable even when its own
// putnami.json declares no channel — putnami-extension-sdk is `stable` in the
// catalog and declares none. Deriving the set from `publish` alone let a
// classified package sit unchecked forever.
// A scope narrows the set to the SELECTED publishable projects: completeness is
// the one count here that is per-project rather than per-feature, so reporting
// the whole workspace's gaps under a selection would be exactly the unbounded
// output scoping exists to remove.
func assessedProjects(ws *workspace.Workspace, classified map[supportSubjectIdentity]bool, scope *featureengine.Scope) []*workspace.Project {
	projects := publishableProjects(ws)
	seen := make(map[string]bool, len(projects))
	for _, project := range projects {
		seen[project.Path] = true
	}
	for _, project := range ws.Projects {
		if project == nil || seen[project.Path] {
			continue
		}
		name := project.Name
		if name == "" {
			name = project.ID
		}
		if classified[supportSubjectIdentity{kind: supportKindForProject(project), id: name}] {
			projects = append(projects, project)
		}
	}
	selected := make([]*workspace.Project, 0, len(projects))
	for _, project := range projects {
		if scope.SeedRoot(project.Path) {
			selected = append(selected, project)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Path < selected[j].Path })
	return selected
}

// consumablePublishChannels are the `options.publish` channels that produce an
// artifact someone outside this repository installs and depends on: an npm
// package, or an extension/template archive `putnami extensions install`
// fetches.
//
// `docker` is deliberately absent. An image is how a deployable workload — the
// two sites, the Go and Python sample applications — ships, and a deployed
// workload is not a package anyone depends on. Counting it would demand a
// public support entry for putnami.dev and for sample applications, turning the
// reviewed catalog into a directory of every buildable thing.
var consumablePublishChannels = []string{"npm", "archives"}

// declaresPublishOption reports whether a project publishes a consumable
// artifact through an extension's `options.publish` block. An explicit `false`
// is a decision not to publish and is honored — the Go and Python sample
// applications set `archives: false` beside `docker: true`.
func declaresPublishOption(project *workspace.Project) bool {
	if project.Config == nil {
		return false
	}
	publish := project.Config.Options["publish"]
	for _, channel := range consumablePublishChannels {
		if enabled, isBool := publish[channel].(bool); isBool && enabled {
			return true
		}
	}
	return false
}

func featuresWithoutSpec(repository *specRepository) []string {
	missing := make([]string, 0)
	for _, feature := range repository.authored {
		if _, found := repository.byFeature[feature]; !found {
			missing = append(missing, feature)
		}
	}
	return missing
}

// specContractError fails a run only on an error-severity finding, so
// completeness warnings never change an exit code.
func specContractError(command string, diagnostics []diag.Diagnostic, data any) error {
	if !diag.HasErrors(diagnostics) {
		return nil
	}
	errorCount := countFeatureDiagnostics(diagnostics).Errors
	err := protocolcli.Classify(
		fmt.Errorf("%s failed with %d spec diagnostic(s)", command, errorCount),
		protocolcli.ErrInvalidConfig,
	)
	return WithResultData(err, data)
}

func boundedStatements(statements []string, limit int) []string {
	bounded := make([]string, 0, limit)
	for index, statement := range statements {
		if index >= limit {
			break
		}
		bounded = append(bounded, statement)
	}
	return bounded
}

func validSpecSelector(selector string) bool {
	if selector == "" || len(selector) > specSelectorMaxBytes {
		return false
	}
	for _, character := range selector {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
