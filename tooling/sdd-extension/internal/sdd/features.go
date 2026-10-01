package sdd

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	internalgit "go.putnami.dev/tooling/sdd/extension/internal/gitread"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const (
	featureSelectorMaxBytes = 512
	featureCandidateLimit   = 20
)

// SelectionReport is the deterministic selection metadata every scoped
// features/specs surface carries. It answers, in structured output and in the
// human header alike, the three questions a bounded answer raises: what was
// selected, what it was measured against, and what had to be followed outside
// the selection to keep the answer correct.
type SelectionReport struct {
	// Mode is "all", "projects", or "impacted".
	Mode string `json:"mode"`
	// Scoped distinguishes a narrowed run from the whole-workspace default.
	Scoped bool `json:"scoped"`
	// Baseline is the ref `--impacted` resolved to, with the tier that produced
	// it, so a fallback baseline is visible rather than implied.
	Baseline       string `json:"baseline,omitempty"`
	BaselineSource string `json:"baselineSource,omitempty"`
	// Projects are the selected project ids, sorted.
	Projects []string `json:"projects"`
	// EmptyImpact marks the legitimate no-op: `--impacted` resolved and nothing
	// changed.
	EmptyImpact bool `json:"emptyImpact,omitempty"`
	// Features and Specs count the authored subjects inside the projection.
	Features int `json:"features"`
	Specs    int `json:"specs"`
	// ExternalRecords are the exact workspace paths outside the selection that
	// were followed because a selected feature's correctness depends on them.
	// Provenance is the whole point: a followed record is named, not counted.
	ExternalRecords []string `json:"externalRecords"`
}

// newSelectionReport projects a resolved selection onto the reported shape.
func newSelectionReport(resolved Selection) SelectionReport {
	report := SelectionReport{
		Mode:            resolved.Mode,
		Scoped:          resolved.Scoped,
		Baseline:        resolved.Baseline,
		BaselineSource:  resolved.BaselineSource,
		Projects:        resolved.ProjectIDs,
		EmptyImpact:     resolved.EmptyImpact,
		ExternalRecords: []string{},
	}
	if report.Projects == nil {
		report.Projects = []string{}
	}
	return report
}

// FeatureCounts is the compact validation and human-summary projection. The
// full provisional snapshot remains the source of every individual fact.
type FeatureCounts struct {
	Features           int `json:"features"`
	IncompleteFeatures int `json:"incompleteFeatures"`
	Requirements       int `json:"requirements"`
	Evidence           int `json:"evidence"`
	Verified           int `json:"verified"`
	Missing            int `json:"missing"`
	Stale              int `json:"stale"`
	Contradicted       int `json:"contradicted"`
	Unclassified       int `json:"unclassified"`
}

// FeatureDiagnosticCounts makes warning-only validation success explicit.
type FeatureDiagnosticCounts struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
}

// FeatureValidationReport is the typed data carried by both successful and
// failed validation ResultV2 envelopes.
type FeatureValidationReport struct {
	Valid       bool                    `json:"valid"`
	Revision    featureengine.Revision  `json:"revision"`
	Selection   SelectionReport         `json:"selection"`
	Summary     FeatureCounts           `json:"summary"`
	Diagnostics []diag.Diagnostic       `json:"diagnostics"`
	Counts      FeatureDiagnosticCounts `json:"diagnosticCounts"`
}

// FeatureSnapshotReport flattens the deliberately provisional engine snapshot
// into ResultV2.data and adds the sorted diagnostics that explain incomplete
// assessments. The anonymous pointer preserves compatibility:"provisional" at
// the payload's top level rather than hiding it below another snapshot key.
type FeatureSnapshotReport struct {
	*featureengine.Snapshot
	Selection   SelectionReport   `json:"selection"`
	Diagnostics []diag.Diagnostic `json:"diagnostics"`
}

// FeatureInspectionReport is the typed inspection success and failure data.
// Candidate fields are populated only when selection cannot return one feature.
type FeatureInspectionReport struct {
	Revision            featureengine.Revision                 `json:"revision"`
	SourceBindings      []featureengine.SourceBinding          `json:"sourceBindings"`
	Requested           string                                 `json:"requested,omitempty"`
	Feature             *featureengine.FeatureAssessment       `json:"feature,omitempty"`
	RelatedUnclassified []featureengine.ContributionAssessment `json:"relatedUnclassified"`
	Candidates          []string                               `json:"candidates,omitempty"`
	CandidateCount      int                                    `json:"candidateCount,omitempty"`
	Diagnostics         []diag.Diagnostic                      `json:"diagnostics"`
	Design              *FeatureDesignInspection               `json:"design,omitempty"`
}

// FeatureDesignInspection is the scoped, disposable design projection for one
// product feature. Multiple framework implementations of the same feature ID
// remain separate so their provenance and paths cannot be conflated.
type FeatureDesignInspection struct {
	Compatibility   string                        `json:"compatibility"`
	ID              string                        `json:"id"`
	Name            string                        `json:"name"`
	Outcome         string                        `json:"outcome"`
	Owner           string                        `json:"owner"`
	Implementations []FeatureDesignImplementation `json:"implementations"`
}

// FeatureDesignImplementation is one project-local subgraph reachable from the
// selected feature, including its bounded consumers.
type FeatureDesignImplementation struct {
	Project       string                      `json:"project"`
	Nodes         []featureproto.DesignNode   `json:"nodes"`
	Edges         []featureproto.DesignEdge   `json:"edges"`
	CriticalPaths []FeatureDesignCriticalPath `json:"criticalPaths"`
}

// FeatureDesignCriticalPath records one shortest route from product intent to
// a terminal technical surface. A relation prefixed by "<" was traversed in
// reverse to include a consumer of the selected producer surface.
type FeatureDesignCriticalPath struct {
	Nodes     []string `json:"nodes"`
	Relations []string `json:"relations"`
	Authority string   `json:"authority"`
}

// FeatureCatalogReport is the compact MCP discovery projection for explicitly
// authored product features. It deliberately carries summaries and declaration
// provenance rather than whole subgraphs: agents discover an ID here, then
// request only that feature through feature_context.
type FeatureCatalogReport struct {
	Compatibility string                    `json:"compatibility"`
	Revision      featureengine.Revision    `json:"revision"`
	Selection     SelectionReport           `json:"selection"`
	Query         string                    `json:"query,omitempty"`
	Graphs        int                       `json:"graphs"`
	Manifests     int                       `json:"manifests"`
	Features      []FeatureDesignSummary    `json:"features"`
	Unreadable    []FeatureDesignGraphIssue `json:"unreadable,omitempty"`
}

// FeatureDesignGraphIssue names one authority artifact that exists but could
// not be projected. The historical type name is retained for wire and source
// compatibility now that the same field also reports invalid durable feature
// manifests. Discovery degrades around either artifact instead of hiding every
// healthy feature in the workspace.
type FeatureDesignGraphIssue struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// FeatureDesignSummary is one semantic feature identity merged across durable
// manifests and the project-local design graphs that implement it.
type FeatureDesignSummary struct {
	ID           string                        `json:"id"`
	Name         string                        `json:"name"`
	Outcome      string                        `json:"outcome"`
	Owner        string                        `json:"owner"`
	Projects     []string                      `json:"projects"`
	Declarations []FeatureAuthorityDeclaration `json:"declarations,omitempty"`
	// Conflicts retains the historical native-graph project view. The first
	// declaration in source order wins so the catalog stays deterministic, and
	// all exact authority records remain available in Declarations.
	Conflicts []string `json:"conflicts,omitempty"`
	// ConflictingSources names every divergent declaration by its exact
	// workspace-relative authority artifact. Declarations retains the complete
	// sorted records so collisions never discard provenance or authored intent.
	ConflictingSources []string `json:"conflictingSources,omitempty"`
}

// FeatureAuthorityDeclaration preserves one exact declaration that minted a
// feature identity. A manifest declaration carries its durable fields; a
// design-graph declaration carries the native producer provenance. Specs,
// directory names, and inferred technical facts never enter this collection.
type FeatureAuthorityDeclaration struct {
	Kind         string                         `json:"kind"`
	Source       string                         `json:"source"`
	Project      string                         `json:"project,omitempty"`
	Name         string                         `json:"name"`
	Outcome      string                         `json:"outcome"`
	Owner        string                         `json:"owner"`
	Type         featureproto.FeatureType       `json:"type,omitempty"`
	Target       featureproto.MaturityStage     `json:"target,omitempty"`
	Relations    []featureproto.Relation        `json:"relations,omitempty"`
	Requirements []featureproto.Requirement     `json:"requirements,omitempty"`
	Provenance   *featureproto.DesignProvenance `json:"provenance,omitempty"`
}

// AgentFeatureContextReport is the intentionally compact MCP projection. The
// CLI inspection can retain the full scoped node/edge graph for protocol work;
// routine agent questions receive categorized facts and one representative
// critical path per technical kind instead of the raw graph.
type AgentFeatureContextReport struct {
	Revision           featureengine.Revision              `json:"revision"`
	Compatibility      string                              `json:"compatibility"`
	ID                 string                              `json:"id"`
	Name               string                              `json:"name"`
	Outcome            string                              `json:"outcome"`
	Owner              string                              `json:"owner"`
	Declarations       []FeatureAuthorityDeclaration       `json:"declarations"`
	ConflictingSources []string                            `json:"conflictingSources,omitempty"`
	Implementations    []AgentFeatureContextImplementation `json:"implementations"`
	// Unreadable names design graphs or manifests that exist but could not be
	// projected, so an agent can distinguish absent context from unreadable
	// authority.
	Unreadable []FeatureDesignGraphIssue `json:"unreadable,omitempty"`
}

// AgentFeatureContextImplementation groups native facts by the questions an
// agent asks rather than leaking the graph's storage shape.
type AgentFeatureContextImplementation struct {
	Project          string                    `json:"project"`
	Modules          []AgentFeatureContextFact `json:"modules,omitempty"`
	Surfaces         []AgentFeatureContextFact `json:"surfaces,omitempty"`
	Schemas          []AgentFeatureContextFact `json:"schemas,omitempty"`
	Dependencies     []AgentFeatureContextFact `json:"dependencies,omitempty"`
	Data             []AgentFeatureContextFact `json:"data,omitempty"`
	Migrations       []AgentFeatureContextFact `json:"migrations,omitempty"`
	Events           []AgentFeatureContextFact `json:"events,omitempty"`
	Outboxes         []AgentFeatureContextFact `json:"outboxes,omitempty"`
	Consumers        []AgentFeatureContextFact `json:"consumers,omitempty"`
	GeneratedClients []AgentFeatureContextFact `json:"generatedClients,omitempty"`
	// TypedClients are hand-written clients that declare which producer they
	// call. They are kept separate from GeneratedClients so an agent never reads
	// a hand-written call as generated-client authority.
	TypedClients []AgentFeatureContextFact `json:"typedClients,omitempty"`
	// Projects and Commands are workspace-level facts the CLI mints from the
	// loaded workspace model rather than reads from a framework-produced graph:
	// the project owning this implementation (plus co-implementing direct
	// dependencies) and the commands that project declares in putnami.json.
	Projects          []AgentFeatureContextFact   `json:"projects,omitempty"`
	Commands          []AgentFeatureContextFact   `json:"commands,omitempty"`
	Config            []AgentFeatureContextFact   `json:"config,omitempty"`
	Infra             []AgentFeatureContextFact   `json:"infra,omitempty"`
	Lifecycle         []AgentFeatureContextFact   `json:"lifecycle,omitempty"`
	Tests             []AgentFeatureContextFact   `json:"tests,omitempty"`
	CriticalPaths     []FeatureDesignCriticalPath `json:"criticalPaths,omitempty"`
	CriticalPathCount int                         `json:"criticalPathCount"`
}

// AgentFeatureContextFact preserves exact identity, authority, properties, and
// source provenance while its containing field supplies the technical kind.
// Derived values stay out of Properties so an agent can always tell a declared
// graph property from one this projection computed.
type AgentFeatureContextFact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Authority string `json:"authority"`
	// GeneratedFrom is derived, not declared: the producer operations a
	// generated client was emitted from.
	GeneratedFrom string                         `json:"generatedFrom,omitempty"`
	Properties    map[string]string              `json:"properties,omitempty"`
	Provenance    *featureproto.DesignProvenance `json:"provenance,omitempty"`
}

// BuildFeatureValidationResult validates the selected projection and reports
// every structural diagnostic and assessment warning it owns. Warnings never
// fail the run, and neither does an error wholly outside the selection.
//
// Extracted from core's `FeaturesValidateCommand`, which was this body plus a
// renderer. The split is the engine/command seam: what the run DECIDED lives
// here, how it is shown is the command group's.
func BuildFeatureValidationResult(ws *workspace.Workspace, selection Selection) (FeatureValidationReport, error) {
	result, revision, err := evaluateWorkspaceFeatures(ws, selection)
	if err != nil {
		return FeatureValidationReport{}, err
	}
	report := FeatureValidationReport{
		Valid:       result.Snapshot != nil && !diag.HasErrors(result.Diagnostics),
		Revision:    revision,
		Selection:   featureSelectionReport(selection, result),
		Summary:     summarizeFeatures(result.Snapshot),
		Diagnostics: featureDiagnostics(result.Diagnostics),
		Counts:      countFeatureDiagnostics(result.Diagnostics),
	}
	return report, featureEvaluationError("features validate", result.Snapshot, result.Diagnostics, report)
}

// BuildFeatureSnapshotResult returns the complete deterministic current-worktree
// projection of the selected features. Its compatibility marker stays
// provisional until a separate compatibility decision freezes the payload.
func BuildFeatureSnapshotResult(ws *workspace.Workspace, selection Selection) (FeatureSnapshotReport, error) {
	result, _, err := evaluateWorkspaceFeatures(ws, selection)
	if err != nil {
		return FeatureSnapshotReport{}, err
	}
	report := FeatureSnapshotReport{
		Snapshot:    result.Snapshot,
		Selection:   featureSelectionReport(selection, result),
		Diagnostics: featureDiagnostics(result.Diagnostics),
	}
	return report, featureEvaluationError("features snapshot", result.Snapshot, result.Diagnostics, report)
}

// featureSelectionReport merges the resolved project selection with what the
// evaluation actually covered. The counts come from the engine rather than from
// the renderer: a projection that is only true of the printed lines would let a
// renderer-only filter pass for scoping.
func featureSelectionReport(resolved Selection, result featureengine.Result) SelectionReport {
	report := newSelectionReport(resolved)
	report.Features = len(result.Scope.SelectedFeatures)
	if !resolved.Scoped && result.Snapshot != nil {
		report.Features = len(result.Snapshot.Features)
	}
	if len(result.Scope.ExternalRecords) > 0 {
		report.ExternalRecords = result.Scope.ExternalRecords
	}
	return report
}

// BuildFeatureContextResult selects one exact feature ID (or one unambiguous
// final ID segment) and returns its compact functional/technical projection
// without writing output. Native design declarations bypass the legacy
// whole-workspace evidence evaluation, so unrelated diagnostics never pollute
// the routine agent path.
func BuildFeatureContextResult(ws *workspace.Workspace, selector string) (FeatureInspectionReport, error) {
	if ws == nil {
		return FeatureInspectionReport{}, protocolcli.Classify(
			errors.New("workspace is required to inspect a feature"),
			protocolcli.ErrInvalidConfig,
		)
	}
	if !validFeatureSelector(selector) {
		report := FeatureInspectionReport{
			Revision:            worktreeFeatureRevision(ws.Root),
			SourceBindings:      []featureengine.SourceBinding{},
			RelatedUnclassified: []featureengine.ContributionAssessment{},
			Diagnostics: []diag.Diagnostic{diag.Errorf(
				featureproto.ErrorCodeInvalidID,
				"featureId",
				"feature selector must be non-empty bounded text without control characters",
			)},
		}
		runErr := WithResultData(protocolcli.Usagef("features inspect requires a valid <feature-id>"), report)
		return report, runErr
	}

	// Native design declarations are self-contained and intentionally bypass
	// the legacy whole-workspace evidence evaluation. This keeps the common
	// authoring path scoped to the requested feature and free of unrelated
	// warnings. If no generated graph matches, the durable v1 path below remains
	// fully compatible.
	design, unreadable, designErr := inspectNativeFeatureDesign(ws, selector)
	if designErr != nil {
		var ambiguity *nativeFeatureSelectorAmbiguity
		if errors.As(designErr, &ambiguity) {
			report := FeatureInspectionReport{
				Revision:            worktreeFeatureRevision(ws.Root),
				SourceBindings:      []featureengine.SourceBinding{},
				Requested:           selector,
				RelatedUnclassified: []featureengine.ContributionAssessment{},
				Candidates:          cappedFeatureCandidates(ambiguity.candidates),
				CandidateCount:      len(ambiguity.candidates),
				Diagnostics:         unreadableDesignGraphDiagnostics(unreadable),
			}
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(
				featureproto.ErrorCodeUnknownFeature,
				"featureId",
				"feature selector is ambiguous; use one exact candidate ID",
			))
			sortFeatureDiagnostics(report.Diagnostics)
			return report, WithResultData(protocolcli.Usagef("feature selector is ambiguous"), report)
		}
		return FeatureInspectionReport{}, designErr
	}
	if design != nil {
		report := FeatureInspectionReport{
			Revision:            worktreeFeatureRevision(ws.Root),
			SourceBindings:      []featureengine.SourceBinding{},
			Requested:           selector,
			RelatedUnclassified: []featureengine.ContributionAssessment{},
			Diagnostics:         unreadableDesignGraphDiagnostics(unreadable),
			Design:              design,
		}
		return report, nil
	}

	// Inspection names one exact feature, so it deliberately evaluates the whole
	// workspace: a scoped evaluation could only make the named feature harder to
	// find. The dispatcher refuses selection flags here for the same reason.
	result, revision, err := evaluateWorkspaceFeatures(ws, Selection{})
	if err != nil {
		return FeatureInspectionReport{}, err
	}
	report := FeatureInspectionReport{
		Revision:            revision,
		SourceBindings:      []featureengine.SourceBinding{},
		Requested:           selector,
		RelatedUnclassified: []featureengine.ContributionAssessment{},
		Diagnostics:         featureDiagnostics(result.Diagnostics),
	}
	if result.Snapshot == nil || diag.HasErrors(result.Diagnostics) {
		runErr := featureEvaluationError("features inspect", result.Snapshot, result.Diagnostics, report)
		return report, runErr
	}
	report.SourceBindings = append(report.SourceBindings, result.Snapshot.SourceBindings...)

	selected, candidates, candidateCount := selectFeature(result.Snapshot, selector)
	if selected == nil {
		report.Candidates = candidates
		report.CandidateCount = candidateCount
		message := "feature selector did not match a feature; use one exact candidate ID"
		runErr := protocolcli.Classify(fmt.Errorf("feature selector matched no feature"), protocolcli.ErrNoMatch)
		if selectorHasAmbiguousLeaf(result.Snapshot, selector) {
			message = "feature selector is ambiguous; use one exact candidate ID"
			runErr = protocolcli.Usagef("feature selector is ambiguous")
		}
		report.Diagnostics = append(report.Diagnostics, diag.Errorf(
			featureproto.ErrorCodeUnknownFeature,
			"featureId",
			"%s",
			message,
		))
		sortFeatureDiagnostics(report.Diagnostics)
		runErr = WithResultData(runErr, report)
		return report, runErr
	}

	feature := *selected
	report.Feature = &feature
	report.RelatedUnclassified = relatedUnclassified(ws, feature, result.Snapshot.Unclassified)
	return report, nil
}

// BuildAgentFeatureContextResult returns the compact projection used by MCP.
// Durable exact-root manifests and native design feature nodes share the same
// selector and declaration surface; only a native graph can contribute an
// implementation. Evidence remains available through `features inspect`, but
// is deliberately not read here.
func BuildAgentFeatureContextResult(ws *workspace.Workspace, selector string) (AgentFeatureContextReport, error) {
	if ws == nil {
		return AgentFeatureContextReport{}, protocolcli.Classify(
			errors.New("workspace is required to build a feature context"),
			protocolcli.ErrInvalidConfig,
		)
	}
	if !validFeatureSelector(selector) {
		return AgentFeatureContextReport{}, protocolcli.Usagef("feature_context requires a valid feature selector")
	}
	// feature_context is an EXACT-feature tool: it stays whole-workspace so an
	// agent that already knows an id never has to guess a selection to reach it.
	graphs, manifests, unreadable, err := loadFeatureAuthorities(ws, nil)
	if err != nil {
		return AgentFeatureContextReport{}, err
	}
	entries := collectFeatureAuthorityDeclarations(ws, graphs, manifests.Manifests)
	target, candidates := selectFeatureAuthorityID(entries, selector)
	if len(candidates) > 1 {
		return AgentFeatureContextReport{}, protocolcli.Usagef(
			"feature selector is ambiguous; use one exact candidate ID: %s",
			strings.Join(cappedFeatureCandidates(candidates), ", "),
		)
	}
	if target == "" {
		return AgentFeatureContextReport{}, protocolcli.NotFoundf(
			"no authored manifest or native design declaration matches feature %q",
			selector,
		)
	}
	summaries := summarizeFeatureAuthorities(ws, graphs, manifests.Manifests)
	var summary *FeatureDesignSummary
	for index := range summaries {
		if summaries[index].ID == target {
			summary = &summaries[index]
			break
		}
	}
	if summary == nil {
		return AgentFeatureContextReport{}, protocolcli.NotFoundf("feature authority disappeared for %q", target)
	}
	design, err := inspectLoadedNativeFeatureDesign(ws, graphs, target, false)
	if err != nil {
		return AgentFeatureContextReport{}, err
	}
	report := AgentFeatureContextReport{
		Revision:           worktreeFeatureRevision(ws.Root),
		Compatibility:      featureproto.DesignGraphCompatibility,
		ID:                 summary.ID,
		Name:               summary.Name,
		Outcome:            summary.Outcome,
		Owner:              summary.Owner,
		Declarations:       summary.Declarations,
		ConflictingSources: summary.ConflictingSources,
		Implementations:    []AgentFeatureContextImplementation{},
		Unreadable:         unreadable,
	}
	if design != nil {
		for _, implementation := range design.Implementations {
			report.Implementations = append(report.Implementations, compactAgentFeatureImplementation(implementation))
		}
	}
	return report, nil
}

func selectFeatureAuthorityID(entries []featureAuthorityEntry, selector string) (string, []string) {
	ids := make(map[string]bool)
	for _, entry := range entries {
		ids[entry.id] = true
	}
	if ids[selector] {
		return selector, []string{selector}
	}
	if strings.Contains(selector, "/") {
		return "", nil
	}
	var matches []string
	for id := range ids {
		if path.Base(id) == selector {
			matches = append(matches, id)
		}
	}
	sort.Strings(matches)
	if len(matches) == 1 {
		return matches[0], matches
	}
	return "", matches
}

// agentFeatureFactBuckets maps every technical node kind onto the compact field
// that carries it. It is exhaustive over featureproto.OrderedDesignNodeKinds
// minus the feature node itself (which is the report root, not a fact), and
// TestCompactFeatureContextBucketsEveryDesignNodeKind pins that: a new protocol
// node kind must be bucketed here rather than silently vanishing from
// feature_context.
var agentFeatureFactBuckets = []struct {
	kinds  []featureproto.DesignNodeKind
	assign func(*AgentFeatureContextImplementation, []AgentFeatureContextFact)
}{
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeModule}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Modules = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeAPIOperation}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Surfaces = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeAPISchema}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Schemas = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeService}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Dependencies = f }},
	// A schema is the datasource namespace a migration writes into; a table is
	// one relation inside it. Both answer "what data does this touch", so they
	// share the compact Data field.
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeDataSchema, featureproto.DesignNodeDataTable}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Data = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeDataMigration}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Migrations = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeEventTopic}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Events = f }},
	// An outbox does NOT share the Events group. A staged row is durable at
	// commit time while the topic is published only after a relay claims it, so
	// folding the two would answer "what does this feature publish" with a
	// promise the runtime has not kept yet.
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeEventOutbox}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Outboxes = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeEventHandler}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Consumers = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeClient}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.GeneratedClients = f }},
	// A typed client is hand-written and only declares its producer, so it never
	// shares the generated-client bucket or its generatedFrom detail.
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeTypedClient}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.TypedClients = f }},
	// Projects and commands are workspace-level kinds minted by this CLI from
	// the workspace model; they are bucketed apart so an owning project or a
	// declared binary is never read as a framework-produced technical surface.
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeProject}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Projects = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeCommand}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Commands = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeConfig}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Config = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeInfra}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Infra = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeLifecycle}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Lifecycle = f }},
	{[]featureproto.DesignNodeKind{featureproto.DesignNodeTest}, func(i *AgentFeatureContextImplementation, f []AgentFeatureContextFact) { i.Tests = f }},
}

func compactAgentFeatureImplementation(implementation FeatureDesignImplementation) AgentFeatureContextImplementation {
	result := AgentFeatureContextImplementation{
		Project:           implementation.Project,
		CriticalPaths:     representativeDesignCriticalPaths(implementation),
		CriticalPathCount: len(implementation.CriticalPaths),
	}
	for _, bucket := range agentFeatureFactBuckets {
		bucket.assign(&result, compactAgentFeatureFacts(implementation, bucket.kinds...))
	}
	return result
}

// compactAgentFeatureFacts folds one or more node kinds into a single agent
// projection group. Several kinds share a group when they answer the same
// question — data.schema and data.table are both "what data does this touch".
func compactAgentFeatureFacts(implementation FeatureDesignImplementation, kinds ...featureproto.DesignNodeKind) []AgentFeatureContextFact {
	facts := make([]AgentFeatureContextFact, 0)
	for _, node := range implementation.Nodes {
		if !slices.Contains(kinds, node.Kind) {
			continue
		}
		fact := AgentFeatureContextFact{
			ID: node.ID, Name: node.Name,
			Authority: designNodeAuthority(implementation, node.ID),
		}
		if len(node.Properties) > 0 {
			fact.Properties = node.Properties
		}
		if node.Kind == featureproto.DesignNodeClient {
			fact.GeneratedFrom = generatedClientOperationDetails(implementation, node.ID)
		}
		if source := compactFactProvenance(implementation, node); source != nil {
			provenance := *source
			fact.Provenance = &provenance
		}
		facts = append(facts, fact)
	}
	if len(facts) == 0 {
		return nil
	}
	return facts
}

// compactFactProvenance keeps semantic nodes shareable across modules while
// preserving the canonical declaration source carried by their ownership edge.
func compactFactProvenance(implementation FeatureDesignImplementation, node featureproto.DesignNode) *featureproto.DesignProvenance {
	if node.Provenance != nil {
		return node.Provenance
	}
	if node.Kind != featureproto.DesignNodeConfig && node.Kind != featureproto.DesignNodeInfra && node.Kind != featureproto.DesignNodeTest {
		return nil
	}
	var selected *featureproto.DesignProvenance
	for index := range implementation.Edges {
		edge := &implementation.Edges[index]
		if edge.To != node.ID || edge.Kind != featureproto.DesignEdgeContains || edge.Provenance == nil {
			continue
		}
		if selected == nil || compareFeatureDesignProvenance(edge.Provenance, selected) < 0 {
			selected = edge.Provenance
		}
	}
	return selected
}

func compareFeatureDesignProvenance(left, right *featureproto.DesignProvenance) int {
	if left == nil {
		if right == nil {
			return 0
		}
		return -1
	}
	if right == nil {
		return 1
	}
	if value := strings.Compare(left.Path, right.Path); value != 0 {
		return value
	}
	if left.Line < right.Line {
		return -1
	}
	if left.Line > right.Line {
		return 1
	}
	return strings.Compare(left.Symbol, right.Symbol)
}

func representativeDesignCriticalPaths(implementation FeatureDesignImplementation) []FeatureDesignCriticalPath {
	nodes := make(map[string]featureproto.DesignNode, len(implementation.Nodes))
	for _, node := range implementation.Nodes {
		nodes[node.ID] = node
	}
	seenKinds := make(map[featureproto.DesignNodeKind]bool)
	paths := make([]FeatureDesignCriticalPath, 0)
	for _, criticalPath := range implementation.CriticalPaths {
		if len(criticalPath.Nodes) == 0 {
			continue
		}
		terminal, ok := nodes[criticalPath.Nodes[len(criticalPath.Nodes)-1]]
		if !ok {
			// The scoped subgraph always contains every node its own critical
			// paths terminate on, so this is unreachable for a graph built by
			// inspectNativeFeatureDesign. Keep the guard rather than indexing
			// blindly, but keep it distinct from the dedup skip below: an
			// unresolvable terminal is missing data, not a duplicate kind.
			continue
		}
		if seenKinds[terminal.Kind] {
			continue
		}
		seenKinds[terminal.Kind] = true
		paths = append(paths, criticalPath)
	}
	if len(paths) == 0 {
		return nil
	}
	return paths
}

// evaluateWorkspaceFeatures evaluates the selected projection.
//
// Core resolved the selection FIRST and evaluated under it, which is the whole
// point: filtering an already-computed whole-workspace assessment would cost
// the same and still parse every unrelated project's artifacts. Here the
// resolution arrived on the wire, so the scope is derived rather than computed
// — the ordering property is the orchestrator's to keep now.
func evaluateWorkspaceFeatures(ws *workspace.Workspace, selection Selection) (
	featureengine.Result, featureengine.Revision, error,
) {
	if ws == nil {
		return featureengine.Result{}, featureengine.Revision{}, protocolcli.Classify(
			errors.New("workspace is required to evaluate features"),
			protocolcli.ErrInvalidConfig,
		)
	}
	revision := worktreeFeatureRevision(ws.Root)
	return featureengine.EvaluateWorkspace(ws, revision, selectedScope(ws, selection)), revision, nil
}

func worktreeFeatureRevision(wsRoot string) featureengine.Revision {
	revision := featureengine.Revision{Kind: featureengine.RevisionKindWorktree}
	if head, err := internalgit.HeadSHA(wsRoot); err == nil {
		revision.Head = "git:" + strings.ToLower(head)
	}
	return revision
}

func featureEvaluationError(command string, snapshot *featureengine.Snapshot, diagnostics []diag.Diagnostic, data any) error {
	if snapshot != nil && !diag.HasErrors(diagnostics) {
		return nil
	}
	errors := countFeatureDiagnostics(diagnostics).Errors
	if errors == 0 {
		errors = 1
	}
	err := protocolcli.Classify(
		fmt.Errorf("%s failed with %d structural diagnostic(s)", command, errors),
		protocolcli.ErrInvalidConfig,
	)
	return WithResultData(err, data)
}

func generatedClientOperationDetails(implementation FeatureDesignImplementation, clientID string) string {
	const limit = 4
	var operations []string
	for _, edge := range implementation.Edges {
		if edge.From != clientID || edge.Kind != featureproto.DesignEdgeGeneratedFrom {
			continue
		}
		target := findDesignNode(implementation.Nodes, edge.To)
		if target == nil {
			continue
		}
		operationID := edge.Properties["operationId"]
		if operationID == "" {
			operationID = target.Properties["operationId"]
		}
		operation := strings.TrimSpace(target.Properties["method"] + " " + target.Properties["path"])
		if operation == "" {
			operation = target.Name
		}
		if operationID != "" {
			operation = operationID + "→" + operation
		}
		operations = append(operations, operation)
	}
	sort.Strings(operations)
	if len(operations) > limit {
		remaining := len(operations) - limit
		operations = append(operations[:limit], fmt.Sprintf("… %d more", remaining))
	}
	return strings.Join(operations, ", ")
}

func designNodeAuthority(implementation FeatureDesignImplementation, nodeID string) string {
	for _, criticalPath := range implementation.CriticalPaths {
		if len(criticalPath.Nodes) > 0 && criticalPath.Nodes[len(criticalPath.Nodes)-1] == nodeID {
			return criticalPath.Authority
		}
	}
	return string(featureproto.DesignAuthorityExact)
}

func summarizeFeatures(snapshot *featureengine.Snapshot) FeatureCounts {
	var counts FeatureCounts
	if snapshot == nil {
		return counts
	}
	counts.Features = len(snapshot.Features)
	counts.Unclassified = len(snapshot.Unclassified)
	for _, feature := range snapshot.Features {
		if feature.Current != feature.Target {
			counts.IncompleteFeatures++
		}
		for _, requirement := range feature.Requirements {
			counts.Requirements++
			counts.Evidence += len(requirement.Evidence)
			switch requirement.State {
			case featureengine.VerificationVerified:
				counts.Verified++
			case featureengine.VerificationMissing:
				counts.Missing++
			case featureengine.VerificationStale:
				counts.Stale++
			case featureengine.VerificationContradicted:
				counts.Contradicted++
			}
		}
	}
	return counts
}

func countFeatureDiagnostics(diagnostics []diag.Diagnostic) FeatureDiagnosticCounts {
	var counts FeatureDiagnosticCounts
	for _, finding := range diagnostics {
		switch finding.Severity {
		case diag.Error:
			counts.Errors++
		case diag.Warning:
			counts.Warnings++
		default:
			counts.Info++
		}
	}
	return counts
}

func featureDiagnostics(input []diag.Diagnostic) []diag.Diagnostic {
	output := append([]diag.Diagnostic{}, input...)
	sortFeatureDiagnostics(output)
	return output
}

func sortFeatureDiagnostics(diagnostics []diag.Diagnostic) {
	sort.Slice(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if featureSeverityRank(left.Severity) != featureSeverityRank(right.Severity) {
			return featureSeverityRank(left.Severity) < featureSeverityRank(right.Severity)
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		return left.Message < right.Message
	})
}

func featureSeverityRank(severity diag.Severity) int {
	switch severity {
	case diag.Error:
		return 0
	case diag.Warning:
		return 1
	default:
		return 2
	}
}

func validFeatureSelector(selector string) bool {
	if selector == "" || len(selector) > featureSelectorMaxBytes {
		return false
	}
	for _, character := range selector {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

type nativeDesignGraphSource struct {
	path  string
	graph *featureproto.DesignGraph
}

// BuildFeatureCatalogResult discovers the two explicit feature authorities —
// native design feature nodes and durable exact-root manifests — and merges
// them into a small, deterministic catalog. It does not evaluate evidence,
// capability artifacts, specs, or source-tree heuristics: an agent can safely
// call it before choosing the one feature to inspect.
// It resolves the project selection first: a scoped catalog reads the design
// graphs of the SELECTED projects only, and the durable manifests of every root
// (one bounded document each) so identity and uniqueness stay workspace-wide.
func BuildFeatureCatalogResult(ws *workspace.Workspace, query string, selection Selection) (FeatureCatalogReport, error) {
	if query != "" && !validFeatureSelector(query) {
		return FeatureCatalogReport{}, protocolcli.Usagef("feature catalog query must be bounded text without control characters")
	}
	if ws == nil {
		return FeatureCatalogReport{}, protocolcli.Classify(
			errors.New("workspace is required to build a feature catalog"),
			protocolcli.ErrInvalidConfig,
		)
	}
	scope := selectedScope(ws, selection)
	graphs, manifests, unreadable, err := loadFeatureAuthorities(ws, scope)
	if err != nil {
		return FeatureCatalogReport{}, err
	}
	// Both inputs are already the selected projection: graphs were read only for
	// seed roots, and SeedManifests keeps the seeds' declarations plus the
	// external manifests that declare one of their identities.
	seeded := featureengine.SeedManifests(scope, manifests.Manifests)
	features := summarizeFeatureAuthorities(ws, graphs, seeded)
	report := FeatureCatalogReport{
		Compatibility: featureproto.DesignGraphCompatibility,
		Revision:      worktreeFeatureRevision(ws.Root),
		Selection:     newSelectionReport(selection),
		Query:         query,
		Graphs:        len(graphs),
		Manifests:     len(seeded),
		Unreadable:    unreadable,
	}
	report.Selection.Features = len(features)
	report.Selection.ExternalRecords = externalManifestPaths(scope, manifests.Manifests)
	if query != "" {
		features = filterFeatureDesignSummaries(features, query)
	}
	report.Features = features
	return report, nil
}

// externalManifestPaths names the durable manifests outside the seed roots that
// declare a selected identity — the cross-boundary declarations a scoped
// catalog followed, reported by exact path rather than by count.
func externalManifestPaths(scope *featureengine.Scope, manifests []featureproto.ManifestSource) []string {
	external := make([]string, 0)
	if !scope.Active() {
		return external
	}
	for _, source := range featureengine.SeedManifests(scope, manifests) {
		if !scope.SeedRoot(featureengine.ManifestRoot(source.Path)) {
			external = append(external, source.Path)
		}
	}
	sort.Strings(external)
	return external
}

func loadFeatureAuthorities(ws *workspace.Workspace, scope *featureengine.Scope) (
	[]nativeDesignGraphSource, featureengine.AuthoredManifestResult, []FeatureDesignGraphIssue, error,
) {
	graphs, unreadable, err := loadNativeDesignGraphs(ws, scope)
	if err != nil {
		return nil, featureengine.AuthoredManifestResult{}, nil, err
	}
	manifests := featureengine.DiscoverAuthoredManifests(ws)
	featureengine.SelectAuthored(scope, manifests.Manifests)
	for _, finding := range manifests.Diagnostics {
		artifact, _, _ := strings.Cut(finding.Field, "#")
		if artifact == "" {
			artifact = featureproto.ManifestFilename
		}
		// A manifest this run could not parse mints no identity, so a scoped run
		// cannot tell whether it concerned the selection. Attributing it by root
		// is the only honest answer: outside the seeds, it is somebody else's
		// broken document and must not surface here.
		if !scope.SeedRoot(featureengine.ManifestRoot(artifact)) {
			continue
		}
		unreadable = append(unreadable, FeatureDesignGraphIssue{
			Path:   artifact,
			Reason: fmt.Sprintf("%s: %s", finding.Code, finding.Message),
		})
	}
	sort.Slice(unreadable, func(i, j int) bool {
		if unreadable[i].Path != unreadable[j].Path {
			return unreadable[i].Path < unreadable[j].Path
		}
		return unreadable[i].Reason < unreadable[j].Reason
	})
	return graphs, manifests, unreadable, nil
}

// unreadableDesignGraphDiagnostics turns skipped artifacts into warnings so the
// native inspection path stays honest about what it could not read. Warnings
// never fail the run, so a stale .gen file degrades the answer visibly instead
// of silently narrowing it.
func unreadableDesignGraphDiagnostics(unreadable []FeatureDesignGraphIssue) []diag.Diagnostic {
	diagnostics := make([]diag.Diagnostic, 0, len(unreadable))
	for _, issue := range unreadable {
		diagnostics = append(diagnostics, diag.Warningf(
			featureproto.ErrorCodeParseError,
			issue.Path,
			"design graph skipped: %s",
			issue.Reason,
		))
	}
	return diagnostics
}

func filterFeatureDesignSummaries(features []FeatureDesignSummary, query string) []FeatureDesignSummary {
	tokens := strings.Fields(strings.ToLower(query))
	filtered := make([]FeatureDesignSummary, 0, len(features))
	for _, feature := range features {
		semantic := []string{feature.ID, feature.Name, feature.Outcome, feature.Owner}
		for _, declaration := range feature.Declarations {
			semantic = append(semantic, declaration.Name, declaration.Outcome, declaration.Owner)
		}
		haystack := strings.ToLower(strings.Join(semantic, " "))
		matched := true
		for _, token := range tokens {
			if !strings.Contains(haystack, token) {
				matched = false
				break
			}
		}
		if matched {
			filtered = append(filtered, feature)
		}
	}
	return filtered
}

const (
	featureAuthorityDesignGraph = "designGraph"
	featureAuthorityManifest    = "manifest"
)

// collectFeatureAuthorityDeclarations is the only place the MCP projection
// mints feature roots. Both inputs have already crossed an explicit protocol
// boundary: feature:<id> nodes in validated design graphs, or features[] entries
// in strictly parsed exact-root manifests. Everything else stays technical
// context and cannot become a feature because its name happens to match.
type featureAuthorityEntry struct {
	id          string
	declaration FeatureAuthorityDeclaration
}

func collectFeatureAuthorityDeclarations(ws *workspace.Workspace, graphs []nativeDesignGraphSource, manifests []featureproto.ManifestSource) []featureAuthorityEntry {
	declarations := make([]featureAuthorityEntry, 0)
	for _, source := range graphs {
		for _, node := range source.graph.Nodes {
			if node.Kind != featureproto.DesignNodeFeature || !strings.HasPrefix(node.ID, "feature:") {
				continue
			}
			id := strings.TrimPrefix(node.ID, "feature:")
			if id == "" {
				continue
			}
			declaration := FeatureAuthorityDeclaration{
				Kind: featureAuthorityDesignGraph, Source: designGraphRelativePath(ws.Root, source.path),
				Project: source.graph.Project, Name: node.Name,
				Outcome: node.Properties["outcome"], Owner: node.Properties["owner"],
			}
			if node.Provenance != nil {
				provenance := *node.Provenance
				declaration.Provenance = &provenance
			}
			declarations = append(declarations, featureAuthorityEntry{id: id, declaration: declaration})
		}
	}

	projectByManifest := make(map[string]string)
	if ws != nil {
		for _, project := range ws.Projects {
			if project == nil {
				continue
			}
			name := project.Name
			if name == "" {
				name = project.ID
			}
			projectByManifest[path.Join(project.Path, featureproto.ManifestFilename)] = name
		}
	}
	for _, source := range manifests {
		for index, authored := range source.Manifest.Features {
			canonical := featureproto.CanonicalManifest(&featureproto.Manifest{Features: []featureproto.Feature{authored}}).Features[0]
			declarations = append(declarations, featureAuthorityEntry{id: canonical.ID, declaration: FeatureAuthorityDeclaration{
				Kind: featureAuthorityManifest, Source: source.Path,
				Project: projectByManifest[source.Path], Name: canonical.Name,
				Outcome: canonical.Outcome, Owner: canonical.Owner,
				Type: canonical.Type, Target: canonical.Target,
				Relations: canonical.Relations, Requirements: canonical.Requirements,
				Provenance: &featureproto.DesignProvenance{
					Path: source.Path, Symbol: fmt.Sprintf("features[%d]", index),
				},
			}})
		}
	}
	sort.Slice(declarations, func(i, j int) bool {
		left, right := declarations[i], declarations[j]
		if left.id != right.id {
			return left.id < right.id
		}
		if left.declaration.Source != right.declaration.Source {
			return left.declaration.Source < right.declaration.Source
		}
		if left.declaration.Kind != right.declaration.Kind {
			return left.declaration.Kind < right.declaration.Kind
		}
		if left.declaration.Project != right.declaration.Project {
			return left.declaration.Project < right.declaration.Project
		}
		return compareFeatureDesignProvenance(left.declaration.Provenance, right.declaration.Provenance) < 0
	})
	return declarations
}

func summarizeFeatureAuthorities(ws *workspace.Workspace, graphs []nativeDesignGraphSource, manifests []featureproto.ManifestSource) []FeatureDesignSummary {
	entries := collectFeatureAuthorityDeclarations(ws, graphs, manifests)
	features := make([]FeatureDesignSummary, 0)
	for start := 0; start < len(entries); {
		end := start + 1
		for end < len(entries) && entries[end].id == entries[start].id {
			end++
		}
		first := entries[start].declaration
		summary := FeatureDesignSummary{
			ID: entries[start].id, Name: first.Name, Outcome: first.Outcome, Owner: first.Owner,
			Declarations: make([]FeatureAuthorityDeclaration, 0, end-start),
		}
		projects := make(map[string]bool)
		conflicts := make(map[string]bool)
		conflictingSources := make(map[string]bool)
		for _, entry := range entries[start:end] {
			declaration := entry.declaration
			summary.Declarations = append(summary.Declarations, declaration)
			if declaration.Project != "" {
				projects[declaration.Project] = true
			}
			if !featureAuthorityDeclarationsDiverge(first, declaration) {
				continue
			}
			conflictingSources[declaration.Source] = true
			if declaration.Kind == featureAuthorityDesignGraph && declaration.Project != "" {
				conflicts[declaration.Project] = true
			}
		}
		for project := range projects {
			summary.Projects = append(summary.Projects, project)
		}
		sort.Strings(summary.Projects)
		for project := range conflicts {
			summary.Conflicts = append(summary.Conflicts, project)
		}
		sort.Strings(summary.Conflicts)
		for source := range conflictingSources {
			summary.ConflictingSources = append(summary.ConflictingSources, source)
		}
		sort.Strings(summary.ConflictingSources)
		features = append(features, summary)
		start = end
	}
	return features
}

func featureAuthorityDeclarationsDiverge(left, right FeatureAuthorityDeclaration) bool {
	if left.Name != right.Name || left.Outcome != right.Outcome || left.Owner != right.Owner {
		return true
	}
	// Design graphs intentionally do not carry the durable maturity overlay.
	// Compare those fields only when both authorities are manifests; their
	// absence from a graph is not a contradiction.
	if left.Kind != featureAuthorityManifest || right.Kind != featureAuthorityManifest {
		return false
	}
	if left.Type != right.Type || left.Target != right.Target || len(left.Relations) != len(right.Relations) || len(left.Requirements) != len(right.Requirements) {
		return true
	}
	for index := range left.Relations {
		if left.Relations[index] != right.Relations[index] {
			return true
		}
	}
	for index := range left.Requirements {
		leftRequirement, rightRequirement := left.Requirements[index], right.Requirements[index]
		if leftRequirement.ID != rightRequirement.ID || leftRequirement.Stage != rightRequirement.Stage || !slices.Equal(leftRequirement.EvidenceKinds, rightRequirement.EvidenceKinds) {
			return true
		}
	}
	return false
}

// loadNativeDesignGraphs collects the readable project-local design graphs the
// selection covers and reports the artifacts it had to skip. Only a
// workspace-load failure is fatal: the graphs themselves are gitignored build
// output, so a stale, truncated, or compatibility-mismatched file in one
// project degrades that project's contribution instead of hiding every healthy
// feature in the workspace.
//
// This is the one read a scoped run must NOT widen. A design graph is a whole
// project's technical subgraph, so parsing every one of them is what makes the
// unscoped surface grow with the repository. Under a scope, an unselected
// project's graph is never opened — not read, not parsed, not reported as
// unreadable — which is why a broken graph outside the selection leaves no
// trace at all in a scoped answer.
func loadNativeDesignGraphs(ws *workspace.Workspace, scope *featureengine.Scope) (
	[]nativeDesignGraphSource, []FeatureDesignGraphIssue, error,
) {
	if ws == nil {
		// Core loaded one here. An extension has no loader, and a design graph read
		// is per PROJECT root, so a missing membership is a missing input rather
		// than something to rediscover.
		return nil, nil, protocolcli.Classify(
			errors.New("workspace is required to read design graphs"),
			protocolcli.ErrInvalidConfig,
		)
	}
	wsRoot := ws.Root
	locations := []string{wsRoot}
	for _, project := range ws.Projects {
		if project != nil && scope.SeedRoot(project.Path) {
			locations = append(locations, filepath.Join(wsRoot, filepath.FromSlash(project.Path)))
		}
	}
	seenLocations := make(map[string]bool)
	graphs := make([]nativeDesignGraphSource, 0)
	var unreadable []FeatureDesignGraphIssue
	for _, location := range locations {
		artifact := filepath.Join(location, ".gen", filepath.FromSlash(featureproto.DesignGraphArtifact))
		if seenLocations[artifact] {
			continue
		}
		seenLocations[artifact] = true
		source, found, reason := readNativeDesignGraph(artifact)
		switch {
		case !found:
			continue
		case reason != "":
			unreadable = append(unreadable, FeatureDesignGraphIssue{
				Path:   designGraphRelativePath(wsRoot, artifact),
				Reason: reason,
			})
		default:
			graphs = append(graphs, source)
		}
	}
	sort.Slice(graphs, func(i, j int) bool { return graphs[i].path < graphs[j].path })
	sort.Slice(unreadable, func(i, j int) bool { return unreadable[i].Path < unreadable[j].Path })
	return graphs, unreadable, nil
}

// readNativeDesignGraph reports (source, found, reason). found distinguishes an
// absent artifact from one that exists but cannot be projected; a non-empty
// reason describes the latter without embedding the absolute path, which every
// caller already reports separately.
func readNativeDesignGraph(artifact string) (nativeDesignGraphSource, bool, string) {
	data, err := os.ReadFile(artifact)
	if os.IsNotExist(err) {
		return nativeDesignGraphSource{}, false, ""
	}
	if err != nil {
		// *PathError repeats the absolute path the caller already reports.
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return nativeDesignGraphSource{}, true, "read design graph: " + err.Error()
	}
	graph, err := featureproto.ParseDesignGraph(data)
	if err != nil {
		return nativeDesignGraphSource{}, true, "parse design graph: " + err.Error()
	}
	return nativeDesignGraphSource{path: artifact, graph: graph}, true, ""
}

func designGraphRelativePath(wsRoot, artifact string) string {
	rel, err := filepath.Rel(wsRoot, artifact)
	if err != nil {
		return filepath.ToSlash(artifact)
	}
	return filepath.ToSlash(rel)
}

type nativeFeatureSelectorAmbiguity struct {
	candidates []string
}

func (err *nativeFeatureSelectorAmbiguity) Error() string {
	return fmt.Sprintf("native feature selector is ambiguous across %d candidates", len(err.candidates))
}

func inspectNativeFeatureDesign(ws *workspace.Workspace, selector string) (*FeatureDesignInspection, []FeatureDesignGraphIssue, error) {
	// Exact-feature inspection, so the whole workspace stays in play.
	graphs, unreadable, err := loadNativeDesignGraphs(ws, nil)
	if err != nil {
		return nil, nil, err
	}
	if len(graphs) == 0 {
		return nil, unreadable, nil
	}

	featureIDs := make(map[string]bool)
	for _, source := range graphs {
		for _, node := range source.graph.Nodes {
			if node.Kind == featureproto.DesignNodeFeature && strings.HasPrefix(node.ID, "feature:") {
				featureIDs[strings.TrimPrefix(node.ID, "feature:")] = true
			}
		}
	}
	target := ""
	if featureIDs[selector] {
		target = selector
	} else if !strings.Contains(selector, "/") {
		var matches []string
		for featureID := range featureIDs {
			if path.Base(featureID) == selector {
				matches = append(matches, featureID)
			}
		}
		sort.Strings(matches)
		if len(matches) == 1 {
			target = matches[0]
		} else if len(matches) > 1 {
			return nil, unreadable, &nativeFeatureSelectorAmbiguity{candidates: matches}
		}
	}
	if target == "" {
		return nil, unreadable, nil
	}
	design, err := inspectLoadedNativeFeatureDesign(ws, graphs, target, true)
	return design, unreadable, err
}

// inspectLoadedNativeFeatureDesign scopes the already validated native graphs
// for one exact feature ID. CLI inspection rejects divergent native semantics;
// MCP passes rejectConflicts=false because its declaration list can preserve
// every colliding source while still returning each exact technical subgraph.
func inspectLoadedNativeFeatureDesign(ws *workspace.Workspace, graphs []nativeDesignGraphSource, target string, rejectConflicts bool) (*FeatureDesignInspection, error) {
	design := &FeatureDesignInspection{
		Compatibility:   featureproto.DesignGraphCompatibility,
		ID:              target,
		Implementations: []FeatureDesignImplementation{},
	}
	featureNodeID := "feature:" + target
	for _, source := range graphs {
		featureNode := findDesignNode(source.graph.Nodes, featureNodeID)
		if featureNode == nil {
			continue
		}
		if design.Name == "" {
			design.Name = featureNode.Name
			design.Outcome = featureNode.Properties["outcome"]
			design.Owner = featureNode.Properties["owner"]
		} else if rejectConflicts && (design.Name != featureNode.Name || design.Outcome != featureNode.Properties["outcome"] || design.Owner != featureNode.Properties["owner"]) {
			// Unlike the workspace-wide catalog, the caller named this exact
			// feature: there is no coherent single answer to return.
			return nil, protocolcli.Classify(
				fmt.Errorf("feature %q has conflicting native declarations across design graphs (including %s)", target, source.path),
				protocolcli.ErrInvalidConfig,
			)
		}
		design.Implementations = append(design.Implementations, scopeFeatureDesign(source.graph, featureNodeID))
	}
	sort.Slice(design.Implementations, func(i, j int) bool {
		return design.Implementations[i].Project < design.Implementations[j].Project
	})
	if len(design.Implementations) == 0 {
		return nil, nil
	}
	enrichFeatureDesignWithWorkspace(design, ws)
	return design, nil
}

// enrichFeatureDesignWithWorkspace mints the workspace-level vertices native
// framework producers cannot know: the project owning each implementation, the
// commands that project declares in putnami.json, and direct workspace
// dependencies between projects implementing the same feature. Every fact is
// read from the loaded workspace model, so each minted node and edge is exact;
// a relationship the model does not state stays absent rather than guessed.
// Projects and commands are deliberately their own node kinds — a project or
// declared binary is never represented as a product feature.
func enrichFeatureDesignWithWorkspace(design *FeatureDesignInspection, ws *workspace.Workspace) {
	implementers := make(map[string]bool, len(design.Implementations))
	for _, implementation := range design.Implementations {
		implementers[implementation.Project] = true
	}
	for index := range design.Implementations {
		implementation := &design.Implementations[index]
		projectNodeID := "project:" + implementation.Project
		projectNode := featureproto.DesignNode{
			ID:   projectNodeID,
			Kind: featureproto.DesignNodeProject,
			Name: implementation.Project,
		}
		var project *workspace.Project
		if ws != nil {
			project = resolveProjectSelector(ws, implementation.Project)
		}
		if project != nil {
			projectType := project.Type
			if projectType == "" {
				projectType = "application"
			}
			projectNode.Properties = map[string]string{"type": projectType, "path": project.Path}
			projectNode.Provenance = &featureproto.DesignProvenance{Path: "putnami.json"}
		}

		moduleParents := make(map[string]bool)
		nodeKinds := make(map[string]featureproto.DesignNodeKind, len(implementation.Nodes))
		for _, node := range implementation.Nodes {
			nodeKinds[node.ID] = node.Kind
		}
		for _, edge := range implementation.Edges {
			if edge.Kind == featureproto.DesignEdgeContains && nodeKinds[edge.From] == featureproto.DesignNodeModule {
				moduleParents[edge.To] = true
			}
		}

		nodes := []featureproto.DesignNode{projectNode}
		edges := make([]featureproto.DesignEdge, 0)
		for _, node := range implementation.Nodes {
			if node.Kind == featureproto.DesignNodeModule && !moduleParents[node.ID] {
				edges = append(edges, featureproto.DesignEdge{
					From: projectNodeID, To: node.ID,
					Kind: featureproto.DesignEdgeContains, Authority: featureproto.DesignAuthorityExact,
				})
			}
		}
		for _, command := range declaredProjectCommands(project) {
			nodes = append(nodes, command)
			edges = append(edges, featureproto.DesignEdge{
				From: projectNodeID, To: command.ID,
				Kind: featureproto.DesignEdgeExposes, Authority: featureproto.DesignAuthorityExact,
				Provenance: &featureproto.DesignProvenance{Path: "putnami.json"},
			})
		}
		if project != nil {
			dependencies := append([]string(nil), project.Dependencies...)
			sort.Strings(dependencies)
			for _, dependency := range dependencies {
				if dependency == implementation.Project || !implementers[dependency] {
					continue
				}
				dependencyNodeID := "project:" + dependency
				nodes = append(nodes, featureproto.DesignNode{
					ID: dependencyNodeID, Kind: featureproto.DesignNodeProject, Name: dependency,
				})
				edges = append(edges, featureproto.DesignEdge{
					From: projectNodeID, To: dependencyNodeID,
					Kind: featureproto.DesignEdgeDependsOn, Authority: featureproto.DesignAuthorityExact,
					Provenance: &featureproto.DesignProvenance{Path: "putnami.json"},
				})
			}
		}
		implementation.Nodes = append(implementation.Nodes, nodes...)
		implementation.Edges = append(implementation.Edges, edges...)
	}
}

// declaredProjectCommands projects the putnami.json bin declaration — the
// workspace protocol's native command registration — into command nodes. The
// string form follows the manifest convention of naming the command after the
// project's unscoped name; the map form names each command explicitly. A
// project without a bin declaration exposes no commands: nothing is inferred
// from project type, name, or build output.
func declaredProjectCommands(project *workspace.Project) []featureproto.DesignNode {
	if project == nil || project.Config == nil || project.Config.Bin == nil {
		return nil
	}
	bin := project.Config.Bin
	command := func(name, entry string) featureproto.DesignNode {
		node := featureproto.DesignNode{
			ID: "command:" + name, Kind: featureproto.DesignNodeCommand, Name: name,
			Provenance: &featureproto.DesignProvenance{Path: "putnami.json"},
		}
		if entry != "" {
			node.Properties = map[string]string{"entry": entry}
		}
		return node
	}
	if bin.Map == nil {
		name := project.Name
		if slash := strings.LastIndex(name, "/"); slash >= 0 {
			name = name[slash+1:]
		}
		if name == "" || bin.String == "" {
			return nil
		}
		return []featureproto.DesignNode{command(name, bin.String)}
	}
	names := make([]string, 0, len(bin.Map))
	for name := range bin.Map {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	nodes := make([]featureproto.DesignNode, 0, len(names))
	for _, name := range names {
		nodes = append(nodes, command(name, bin.Map[name]))
	}
	return nodes
}

type designTraversal struct {
	node      string
	from      string
	edge      featureproto.DesignEdge
	reversed  bool
	authority featureproto.DesignAuthority
}

func scopeFeatureDesign(graph *featureproto.DesignGraph, featureNodeID string) FeatureDesignImplementation {
	nodeByID := make(map[string]featureproto.DesignNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodeByID[node.ID] = node
	}
	visited := map[string]bool{featureNodeID: true}
	predecessor := make(map[string]designTraversal)
	queue := []string{featureNodeID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range graph.Edges {
			next, reversed, ok := designTraversalNeighbor(nodeByID[current], edge)
			if !ok || visited[next] {
				continue
			}
			visited[next] = true
			predecessor[next] = designTraversal{
				node: next, from: current, edge: edge, reversed: reversed, authority: edge.Authority,
			}
			queue = append(queue, next)
		}
	}

	implementation := FeatureDesignImplementation{
		Project:       graph.Project,
		Nodes:         []featureproto.DesignNode{},
		Edges:         []featureproto.DesignEdge{},
		CriticalPaths: []FeatureDesignCriticalPath{},
	}
	for _, node := range graph.Nodes {
		if visited[node.ID] {
			implementation.Nodes = append(implementation.Nodes, node)
		}
	}
	for _, edge := range graph.Edges {
		if visited[edge.From] && visited[edge.To] {
			implementation.Edges = append(implementation.Edges, edge)
		}
	}
	for _, node := range implementation.Nodes {
		if !designPathTarget(node.Kind) || node.ID == featureNodeID {
			continue
		}
		if criticalPath, ok := reconstructDesignPath(featureNodeID, node.ID, predecessor); ok {
			implementation.CriticalPaths = append(implementation.CriticalPaths, criticalPath)
		}
	}
	sort.Slice(implementation.CriticalPaths, func(i, j int) bool {
		left := implementation.CriticalPaths[i].Nodes
		right := implementation.CriticalPaths[j].Nodes
		return left[len(left)-1] < right[len(right)-1]
	})
	return implementation
}

// designTraversalNeighbor walks a design edge from the node currently being
// expanded. Forward edges always apply; a reverse step is allowed only where a
// producer would otherwise be unreachable from the feature root, and only when
// the current node's kind proves the reverse hop stays inside this feature.
// Outbox relationships need no reverse case: the enqueueing module owns the
// `enqueues` edge and is already feature-scoped, so `module -> event.outbox ->
// event.topic` is reached forward. A reverse `publishes` hop from a topic
// cannot be admitted here because the same edge kind also carries the derived
// module publish, and pulling that in would import an unrelated feature's
// module.
func designTraversalNeighbor(current featureproto.DesignNode, edge featureproto.DesignEdge) (string, bool, bool) {
	if edge.From == current.ID {
		return edge.To, false, true
	}
	if edge.To != current.ID {
		return "", false, false
	}
	switch edge.Kind {
	case featureproto.DesignEdgeSubscribes:
		if current.Kind == featureproto.DesignNodeEventTopic {
			return edge.From, true, true
		}
	case featureproto.DesignEdgeGeneratedFrom:
		if current.Kind == featureproto.DesignNodeAPIOperation {
			return edge.From, true, true
		}
	case featureproto.DesignEdgeCalls:
		// A caller is reachable backwards from what it calls: an operation
		// reaches its clients, and a typed client reaches every consumer module
		// that composes it, so "who calls this producer" stays answerable from
		// the producer's own feature.
		if current.Kind == featureproto.DesignNodeAPIOperation || current.Kind == featureproto.DesignNodeTypedClient {
			return edge.From, true, true
		}
	}
	return "", false, false
}

// designPathTarget selects the kinds a critical path may terminate on.
// DesignNodeProject and DesignNodeCommand are deliberately excluded: they are
// workspace-level containment facts minted after traversal, so presenting a
// path through them would claim a runtime route the framework never declared.
func designPathTarget(kind featureproto.DesignNodeKind) bool {
	switch kind {
	case featureproto.DesignNodeAPIOperation,
		featureproto.DesignNodeAPISchema,
		featureproto.DesignNodeService,
		featureproto.DesignNodeDataSchema,
		featureproto.DesignNodeDataTable,
		featureproto.DesignNodeDataMigration,
		featureproto.DesignNodeEventTopic,
		featureproto.DesignNodeEventOutbox,
		featureproto.DesignNodeEventHandler,
		featureproto.DesignNodeClient,
		featureproto.DesignNodeTypedClient,
		featureproto.DesignNodeConfig,
		featureproto.DesignNodeInfra,
		featureproto.DesignNodeLifecycle,
		featureproto.DesignNodeTest:
		return true
	default:
		return false
	}
}

func reconstructDesignPath(featureNodeID, target string, predecessor map[string]designTraversal) (FeatureDesignCriticalPath, bool) {
	nodes := []string{target}
	var relations []string
	authority := featureproto.DesignAuthorityExact
	current := target
	for current != featureNodeID {
		step, ok := predecessor[current]
		if !ok {
			return FeatureDesignCriticalPath{}, false
		}
		relation := string(step.edge.Kind)
		if step.reversed {
			relation = "<" + relation
		}
		relations = append(relations, relation)
		authority = weakestDesignAuthority(authority, step.authority)
		current = step.from
		nodes = append(nodes, current)
	}
	for left, right := 0, len(nodes)-1; left < right; left, right = left+1, right-1 {
		nodes[left], nodes[right] = nodes[right], nodes[left]
	}
	for left, right := 0, len(relations)-1; left < right; left, right = left+1, right-1 {
		relations[left], relations[right] = relations[right], relations[left]
	}
	return FeatureDesignCriticalPath{Nodes: nodes, Relations: relations, Authority: string(authority)}, true
}

// designAuthorityRanks is derived from the protocol vocabulary rather than
// hardcoded: a hardcoded ladder silently ranks an authority it has never heard
// of as the strongest one, which would let a currently-unmodeled step publish an
// exact path.
var designAuthorityRanks = func() map[featureproto.DesignAuthority]int {
	ordered := featureproto.OrderedDesignAuthorities()
	ranks := make(map[featureproto.DesignAuthority]int, len(ordered))
	for rank, authority := range ordered {
		ranks[authority] = rank
	}
	return ranks
}()

// designAuthorityRank returns the strength rank, strongest first. An authority
// outside the vocabulary this CLI knows is ranked weaker than every known one,
// so an unrecognized artifact can only understate what it proves.
func designAuthorityRank(authority featureproto.DesignAuthority) int {
	if rank, known := designAuthorityRanks[authority]; known {
		return rank
	}
	return len(designAuthorityRanks)
}

func weakestDesignAuthority(left, right featureproto.DesignAuthority) featureproto.DesignAuthority {
	if designAuthorityRank(left) >= designAuthorityRank(right) {
		return left
	}
	return right
}

func findDesignNode(nodes []featureproto.DesignNode, id string) *featureproto.DesignNode {
	for index := range nodes {
		if nodes[index].ID == id {
			return &nodes[index]
		}
	}
	return nil
}

func selectFeature(snapshot *featureengine.Snapshot, selector string) (*featureengine.FeatureAssessment, []string, int) {
	for index := range snapshot.Features {
		if snapshot.Features[index].ID == selector {
			return &snapshot.Features[index], nil, 0
		}
	}
	if !strings.Contains(selector, "/") {
		var matches []string
		for _, feature := range snapshot.Features {
			if path.Base(feature.ID) == selector {
				matches = append(matches, feature.ID)
			}
		}
		if len(matches) == 1 {
			for index := range snapshot.Features {
				if snapshot.Features[index].ID == matches[0] {
					return &snapshot.Features[index], nil, 0
				}
			}
		}
		if len(matches) > 1 {
			return nil, cappedFeatureCandidates(matches), len(matches)
		}
	}
	all := make([]string, 0, len(snapshot.Features))
	for _, feature := range snapshot.Features {
		all = append(all, feature.ID)
	}
	return nil, cappedFeatureCandidates(all), len(all)
}

func selectorHasAmbiguousLeaf(snapshot *featureengine.Snapshot, selector string) bool {
	if strings.Contains(selector, "/") {
		return false
	}
	matches := 0
	for _, feature := range snapshot.Features {
		if path.Base(feature.ID) == selector {
			matches++
		}
	}
	return matches > 1
}

func cappedFeatureCandidates(input []string) []string {
	output := append([]string(nil), input...)
	sort.Strings(output)
	if len(output) > featureCandidateLimit {
		output = output[:featureCandidateLimit]
	}
	return output
}

func relatedUnclassified(ws *workspace.Workspace, feature featureengine.FeatureAssessment, unclassified []featureengine.ContributionAssessment) []featureengine.ContributionAssessment {
	owners := make(map[string]bool)
	for _, requirement := range feature.Requirements {
		for _, evidence := range requirement.Evidence {
			if evidence.Contribution != nil {
				owners[evidence.Contribution.Identity.OwnerProject] = true
			}
		}
	}
	if ws != nil {
		for _, project := range ws.Projects {
			if project == nil || path.Join(project.Path, featureproto.ManifestFilename) != feature.Source {
				continue
			}
			for _, owner := range []string{project.ID, project.Name, project.SourceName} {
				if owner != "" {
					owners[owner] = true
				}
			}
		}
	}
	result := make([]featureengine.ContributionAssessment, 0)
	for _, contribution := range unclassified {
		if owners[contribution.Identity.OwnerProject] {
			result = append(result, contribution)
		}
	}
	return result
}
