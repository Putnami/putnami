package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// The human half of the four `features` subcommands.
//
// Ported verbatim from `tooling/cli/internal/commands/sdd/features.go` and
// `features_diff.go`. The engine moved and its
// rendering stayed behind on purpose, so this file is the other half of that
// seam: every function here takes a report the engine already decided and
// writes it, and none of them reads a file or reaches a verdict.
//
// The structured half is not here. An interactive subcommand's machine output
// is one ResultV2 envelope written by the SDK helper (`resultv2.Emit`), which
// reproduces `App.runStructuredCommand` byte for byte — including the detail
// that the ENVELOPE is written in the mode the user selected while the handler
// is driven in jsonl. Splitting it that way is what lets a payload be rendered
// once and framed by the one writer that knows the mode.

// printSelectionHuman opens every scoped answer with the scope itself: what was
// selected, what it was measured against, and what had to be followed outside
// it. An unscoped run says so in one line rather than staying silent, because
// "whole workspace" is also an answer a reader needs.
func printSelectionHuman(w io.Writer, selection sdd.SelectionReport) {
	if !selection.Scoped {
		fmt.Fprintln(w, "  Scope: the whole workspace")
		return
	}
	against := ""
	if selection.Baseline != "" {
		against = " against " + selection.Baseline
	}
	fmt.Fprintf(w, "  %d project(s) selected%s\n", len(selection.Projects), against)
	subjects := fmt.Sprintf("%d feature(s) in scope", selection.Features)
	if selection.Specs > 0 || selection.Features == 0 {
		subjects = fmt.Sprintf("%s · %d spec(s) in scope", subjects, selection.Specs)
	}
	fmt.Fprintf(w, "  %s · %d external record(s) followed\n", subjects, len(selection.ExternalRecords))
	for _, record := range selection.ExternalRecords {
		fmt.Fprintf(w, "    followed: %s\n", record)
	}
	if selection.EmptyImpact {
		fmt.Fprintln(w, "  Nothing changed against the resolved baseline, so nothing is in scope.")
	}
}

func printFeatureCatalogHuman(w io.Writer, report sdd.FeatureCatalogReport) {
	fmt.Fprintf(w, "\n  Features (%d)\n", len(report.Features))
	printSelectionHuman(w, report.Selection)
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	fmt.Fprintf(w, "  %d design graph(s) · %d durable manifest(s)\n", report.Graphs, report.Manifests)
	if report.Query != "" {
		fmt.Fprintf(w, "  Query: %s\n", report.Query)
	}
	for _, feature := range report.Features {
		fmt.Fprintf(w, "\n  %s · %s\n", feature.ID, feature.Name)
		fmt.Fprintf(w, "    outcome: %s\n", feature.Outcome)
		fmt.Fprintf(w, "    owner: %s\n", feature.Owner)
		if len(feature.Projects) > 0 {
			fmt.Fprintf(w, "    projects: %s\n", strings.Join(feature.Projects, ", "))
		}
		for _, declaration := range feature.Declarations {
			fmt.Fprintf(w, "    [%s] %s\n", declaration.Kind, declaration.Source)
		}
		if len(feature.ConflictingSources) > 0 {
			fmt.Fprintf(w, "    conflicting declarations: %s\n", strings.Join(feature.ConflictingSources, ", "))
		}
	}
	if len(report.Unreadable) > 0 {
		fmt.Fprintf(w, "\n  Unreadable authorities (%d):\n", len(report.Unreadable))
		for _, issue := range report.Unreadable {
			fmt.Fprintf(w, "    %s · %s\n", issue.Path, issue.Reason)
		}
	}
	fmt.Fprintln(w)
}

func printFeatureValidationHuman(w io.Writer, report sdd.FeatureValidationReport) {
	status := "passed"
	if !report.Valid {
		status = "failed"
	}
	fmt.Fprintf(w, "\n  Feature validation %s\n", status)
	printSelectionHuman(w, report.Selection)
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	if report.Valid {
		printFeatureCounts(w, report.Summary)
	}
	fmt.Fprintf(w, "  Diagnostics: %d error(s) · %d warning(s) · %d info\n",
		report.Counts.Errors, report.Counts.Warnings, report.Counts.Info)
	printFeatureDiagnostics(w, report.Diagnostics)
	if !report.Valid {
		printDocsHint(w)
	}
	fmt.Fprintln(w)
}

func printFeatureSnapshotHuman(w io.Writer, report sdd.FeatureSnapshotReport) {
	if report.Snapshot == nil {
		fmt.Fprintln(w, "\n  Feature snapshot unavailable")
		printSelectionHuman(w, report.Selection)
		printFeatureDiagnostics(w, report.Diagnostics)
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintf(w, "\n  Feature snapshot (%s)\n", report.Compatibility)
	printSelectionHuman(w, report.Selection)
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	counts := sdd.SummarizeFeatures(report.Snapshot)
	printFeatureCounts(w, counts)

	maturity := make(map[featureproto.MaturityStage]int)
	for _, feature := range report.Features {
		maturity[feature.Current]++
	}
	var stages []string
	for _, stage := range featureproto.OrderedMaturityStages() {
		if maturity[stage] > 0 {
			stages = append(stages, fmt.Sprintf("%s %d", stage, maturity[stage]))
		}
	}
	if len(stages) > 0 {
		fmt.Fprintf(w, "  Current maturity: %s\n", strings.Join(stages, " · "))
	}
	printFeatureDiagnostics(w, report.Diagnostics)
	fmt.Fprintln(w)
}

func printFeatureInspectionHuman(w io.Writer, report sdd.FeatureInspectionReport) {
	if report.Design != nil {
		printFeatureDesignHuman(w, report)
		return
	}
	if report.Feature == nil {
		fmt.Fprintln(w, "\n  Feature inspection failed")
		fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
		if len(report.Candidates) > 0 {
			fmt.Fprintf(w, "  Candidate feature IDs (%d):\n", report.CandidateCount)
			for _, candidate := range report.Candidates {
				fmt.Fprintf(w, "    %s\n", candidate)
			}
			if report.CandidateCount > len(report.Candidates) {
				fmt.Fprintf(w, "    … %d more\n", report.CandidateCount-len(report.Candidates))
			}
		}
		printFeatureDiagnostics(w, report.Diagnostics)
		fmt.Fprintln(w)
		return
	}

	feature := report.Feature
	fmt.Fprintf(w, "\n  Feature %s · %s\n", feature.ID, feature.Name)
	fmt.Fprintf(w, "  Outcome: %s\n", feature.Outcome)
	fmt.Fprintf(w, "  Owner: %s\n", feature.Owner)
	fmt.Fprintf(w, "  Source: %s\n", feature.Source)
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	fmt.Fprintf(w, "  Maturity: %s → %s\n", feature.Current, feature.Target)
	if len(feature.Relations) > 0 {
		fmt.Fprintln(w, "  Relations:")
		for _, relation := range feature.Relations {
			fmt.Fprintf(w, "    %s: %s\n", relation.Kind, relation.Target)
		}
	}
	fmt.Fprintln(w, "  Requirements:")
	if len(feature.Requirements) == 0 {
		fmt.Fprintln(w, "    none (modeled intent only)")
	}
	for _, requirement := range feature.Requirements {
		claim := "claimed"
		if !requirement.Claimed {
			claim = "unclaimed"
		}
		fmt.Fprintf(w, "    [%s] %s · %s (%s; accepts %s)\n",
			requirement.State, requirement.Stage, requirement.ID, claim, evidenceKindsLabel(requirement.EvidenceKinds))
		if len(requirement.Evidence) == 0 {
			fmt.Fprintln(w, "      evidence: none")
			continue
		}
		for _, evidence := range requirement.Evidence {
			printFeatureEvidenceHuman(w, evidence)
		}
	}
	if len(report.RelatedUnclassified) > 0 {
		fmt.Fprintln(w, "  Related unclassified contributions:")
		for _, contribution := range report.RelatedUnclassified {
			fmt.Fprintf(w, "    %s\n", contributionIdentityLabel(contribution.Identity))
			printContributionProvenanceHuman(w, contribution, "      ")
		}
	}
	printFeatureDiagnostics(w, report.Diagnostics)
	fmt.Fprintln(w)
}

func printFeatureDesignHuman(w io.Writer, report sdd.FeatureInspectionReport) {
	design := report.Design
	fmt.Fprintf(w, "\n  Feature %s · %s\n", design.ID, design.Name)
	fmt.Fprintf(w, "  Outcome: %s\n", design.Outcome)
	fmt.Fprintf(w, "  Owner: %s\n", design.Owner)
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	fmt.Fprintf(w, "  Design graph: %s · %d implementation(s)\n", design.Compatibility, len(design.Implementations))
	for _, implementation := range design.Implementations {
		fmt.Fprintf(w, "\n  %s\n", implementation.Project)
		printDesignNodeGroup(w, implementation, "API", featureproto.DesignNodeAPIOperation)
		printDesignNodeGroup(w, implementation, "Dependencies", featureproto.DesignNodeService)
		printDesignNodeGroup(w, implementation, "Data", featureproto.DesignNodeDataSchema, featureproto.DesignNodeDataTable)
		printDesignNodeGroup(w, implementation, "Migrations", featureproto.DesignNodeDataMigration)
		printDesignNodeGroup(w, implementation, "Events", featureproto.DesignNodeEventTopic)
		printDesignNodeGroup(w, implementation, "Outboxes", featureproto.DesignNodeEventOutbox)
		printDesignNodeGroup(w, implementation, "Consumers", featureproto.DesignNodeEventHandler)
		printDesignNodeGroup(w, implementation, "Generated clients", featureproto.DesignNodeClient)
		printDesignNodeGroup(w, implementation, "Typed clients", featureproto.DesignNodeTypedClient)
		printDesignNodeGroup(w, implementation, "Projects", featureproto.DesignNodeProject)
		printDesignNodeGroup(w, implementation, "Commands", featureproto.DesignNodeCommand)
		printDesignNodeGroup(w, implementation, "Config", featureproto.DesignNodeConfig)
		printDesignNodeGroup(w, implementation, "Infrastructure", featureproto.DesignNodeInfra)
		printDesignNodeGroup(w, implementation, "Lifecycle", featureproto.DesignNodeLifecycle)
		printDesignNodeGroup(w, implementation, "Tests", featureproto.DesignNodeTest)
		if len(implementation.CriticalPaths) > 0 {
			fmt.Fprintln(w, "    Critical paths:")
			limit := len(implementation.CriticalPaths)
			if limit > 8 {
				limit = 8
			}
			for _, criticalPath := range implementation.CriticalPaths[:limit] {
				fmt.Fprintf(w, "      [%s] %s\n", criticalPath.Authority, designPathLabel(criticalPath))
			}
			if len(implementation.CriticalPaths) > limit {
				fmt.Fprintf(w, "      … %d more\n", len(implementation.CriticalPaths)-limit)
			}
		}
	}
	fmt.Fprintln(w)
}

func printDesignNodeGroup(
	w io.Writer,
	implementation sdd.FeatureDesignImplementation,
	label string,
	kinds ...featureproto.DesignNodeKind,
) {
	var nodes []featureproto.DesignNode
	for _, node := range implementation.Nodes {
		if slices.Contains(kinds, node.Kind) {
			nodes = append(nodes, node)
		}
	}
	if len(nodes) == 0 {
		return
	}
	fmt.Fprintf(w, "    %s:\n", label)
	for _, node := range nodes {
		authority := sdd.DesignNodeAuthority(implementation, node.ID)
		details := designNodeDetails(node)
		// generatedFrom is a generated-client fact. A typed client is
		// hand-written: it renders the producer it declares, never a
		// generated-from provenance it does not have.
		switch node.Kind {
		case featureproto.DesignNodeClient:
			if generatedFrom := sdd.GeneratedClientOperationDetails(implementation, node.ID); generatedFrom != "" {
				details = appendDesignDetail(details, "generatedFrom="+generatedFrom)
			}
		case featureproto.DesignNodeTypedClient:
			if producer := node.Properties["producer"]; producer != "" {
				details = appendDesignDetail(details, "producer="+producer)
			}
			if operations := typedClientOperationDetails(node); operations != "" {
				details = appendDesignDetail(details, "calls="+operations)
			}
		}
		if details != "" {
			details = " · " + details
		}
		source := ""
		if provenance := sdd.CompactFactProvenance(implementation, node); provenance != nil {
			source = " · " + provenance.Path
			if provenance.Line > 0 {
				source += fmt.Sprintf(":%d", provenance.Line)
			}
		}
		fmt.Fprintf(w, "      [%s] %s%s%s\n", authority, node.Name, details, source)
	}
}

func appendDesignDetail(details, detail string) string {
	if details == "" {
		return detail
	}
	return details + " · " + detail
}

// typedClientOperationDetails renders the bounded operation list a typed client
// declares. The operations come from the node's own property because a
// cross-project producer's operation nodes live in the producer's graph.
func typedClientOperationDetails(node featureproto.DesignNode) string {
	const limit = 4
	operations := strings.Split(node.Properties["operations"], ", ")
	operations = slices.DeleteFunc(operations, func(operation string) bool { return operation == "" })
	if len(operations) > limit {
		remaining := len(operations) - limit
		operations = append(operations[:limit], fmt.Sprintf("… %d more", remaining))
	}
	return strings.Join(operations, ", ")
}

func designNodeDetails(node featureproto.DesignNode) string {
	keys := []string{
		"operationId", "fields", "columns", "datasource", "topic", "kind",
		"namespace", "phase", "path", "language", "package", "feature", "type", "entry",
	}
	var details []string
	for _, key := range keys {
		if value := node.Properties[key]; value != "" {
			details = append(details, key+"="+value)
		}
	}
	return strings.Join(details, " · ")
}

func designPathLabel(criticalPath sdd.FeatureDesignCriticalPath) string {
	if len(criticalPath.Nodes) == 0 {
		return ""
	}
	var label strings.Builder
	label.WriteString(criticalPath.Nodes[0])
	for index, relation := range criticalPath.Relations {
		if index+1 >= len(criticalPath.Nodes) {
			break
		}
		if strings.HasPrefix(relation, "<") {
			label.WriteString(" <-")
			label.WriteString(strings.TrimPrefix(relation, "<"))
			label.WriteString("- ")
		} else {
			label.WriteString(" -")
			label.WriteString(relation)
			label.WriteString("-> ")
		}
		label.WriteString(criticalPath.Nodes[index+1])
	}
	return label.String()
}

func printFeatureEvidenceHuman(w io.Writer, evidence featureengine.EvidenceAssessment) {
	state := string(evidence.State)
	if len(evidence.StaleReasons) > 0 {
		state += ": " + strings.Join(evidence.StaleReasons, ", ")
	}
	fmt.Fprintf(w, "      [%s] %s · %s · issuer %s:%s\n",
		state, evidence.Outcome, evidence.ID, evidence.Issuer.Kind, evidence.Issuer.ID)
	fmt.Fprintf(w, "        document: %s\n", evidence.Document)
	fmt.Fprintf(w, "        source: %s\n", sourceSelectorLabel(evidence.Source))
	fmt.Fprintf(w, "        evidence provenance: %s\n",
		locationLabel(evidence.Provenance.Root, evidence.Provenance.Path, evidence.Provenance.Symbol))
	switch evidence.Subject.Kind {
	case featureproto.EvidenceKindCapability:
		if evidence.Subject.Contribution != nil {
			fmt.Fprintf(w, "        contribution: %s\n", contributionIdentityLabel(*evidence.Subject.Contribution))
		}
		if evidence.Contribution != nil {
			printContributionProvenanceHuman(w, *evidence.Contribution, "        ")
		}
	case featureproto.EvidenceKindArtifact:
		if evidence.Subject.Artifact != nil {
			fmt.Fprintf(w, "        artifact: %s · %s\n", evidence.Subject.Artifact.Path, evidence.Subject.Artifact.Digest)
		}
	case featureproto.EvidenceKindAttestation:
		if evidence.Subject.Attestation != nil {
			fmt.Fprintf(w, "        attestation: %s\n", evidence.Subject.Attestation.Claim)
		}
	}
}

func printContributionProvenanceHuman(w io.Writer, contribution featureengine.ContributionAssessment, indent string) {
	provenance := contribution.Provenance
	producer := []string{"project=" + provenance.Project, "sourceKind=" + string(provenance.SourceKind)}
	if provenance.Package != "" {
		producer = append(producer, "package="+provenance.Package)
	}
	if provenance.Version != "" {
		producer = append(producer, "version="+provenance.Version)
	}
	fmt.Fprintf(w, "%sproducer: %s\n", indent, strings.Join(producer, " · "))
	if provenance.Declaration != nil {
		fmt.Fprintf(w, "%sdeclaration: %s\n", indent,
			locationLabel(provenance.Declaration.Root, provenance.Declaration.Path, provenance.Declaration.Symbol))
	}
	if provenance.V1EvidencePath != "" {
		fmt.Fprintf(w, "%sv1 evidence path: %s\n", indent, provenance.V1EvidencePath)
	}
	for _, artifact := range provenance.Artifacts {
		digest := ""
		if artifact.Digest != "" {
			digest = " · " + artifact.Digest
		}
		fmt.Fprintf(w, "%sartifact: %s%s\n", indent, locationLabel(artifact.Root, artifact.Path, ""), digest)
	}
	if contribution.CurrentSourceBinding != "" {
		fmt.Fprintf(w, "%ssource binding: current %s · %s\n", indent,
			emptyLabel(contribution.CurrentSourceBinding), emptyLabel(contribution.SourceState))
	}
	if len(contribution.Containers) > 0 {
		fmt.Fprintf(w, "%stransport containers: %s\n", indent, strings.Join(contribution.Containers, ", "))
	}
}

func printFeatureCounts(w io.Writer, counts sdd.FeatureCounts) {
	fmt.Fprintf(w, "  %d feature(s) · %d incomplete · %d requirement(s) · %d evidence record(s) · %d unclassified contribution(s)\n",
		counts.Features, counts.IncompleteFeatures, counts.Requirements, counts.Evidence, counts.Unclassified)
	fmt.Fprintf(w, "  Requirement states: %d verified · %d missing · %d stale · %d contradicted\n",
		counts.Verified, counts.Missing, counts.Stale, counts.Contradicted)
}

func printFeatureDiagnostics(w io.Writer, diagnostics []diag.Diagnostic) {
	if len(diagnostics) == 0 {
		return
	}
	fmt.Fprintf(w, "  Diagnostics (%d):\n", len(diagnostics))
	for _, finding := range diagnostics {
		field := ""
		if finding.Field != "" {
			field = " · " + finding.Field
		}
		fmt.Fprintf(w, "    [%s] %s%s\n", finding.Severity, finding.Code, field)
		fmt.Fprintf(w, "      %s\n", finding.Message)
	}
}

func printFeatureDiffHuman(w io.Writer, report sdd.FeatureDiffReport, runErr error) {
	fmt.Fprintf(w, "\n  Feature diff (%s)\n", report.Compatibility)
	fmt.Fprintf(w, "  Base: %s\n", featureRevisionLabel(report.Base))
	fmt.Fprintf(w, "  Head: %s\n", featureRevisionLabel(report.Head))
	if runErr != nil {
		fmt.Fprintln(w, "  Comparison unavailable")
		printRevisionDiagnosticsHuman(w, "Base", report.BaseDiagnostics)
		printRevisionDiagnosticsHuman(w, "Head", report.HeadDiagnostics)
		fmt.Fprintln(w)
		return
	}

	fmt.Fprintf(w, "  %d added · %d removed · %d promoted · %d regressed · %d stale · %d contradicted · %d newly unclassified\n",
		len(report.Added), len(report.Removed), len(report.Promoted), len(report.Regressed),
		len(report.Stale), len(report.Contradicted), len(report.NewlyUnclassified))
	for _, feature := range report.Added {
		fmt.Fprintf(w, "    + %s · %s\n", feature.ID, feature.Name)
	}
	for _, feature := range report.Removed {
		fmt.Fprintf(w, "    - %s · %s\n", feature.ID, feature.Name)
	}
	for _, change := range report.Promoted {
		fmt.Fprintf(w, "    ↑ %s · %s → %s\n", change.FeatureID, change.From, change.To)
	}
	for _, change := range report.Regressed {
		fmt.Fprintf(w, "    ↓ %s · %s → %s\n", change.FeatureID, change.From, change.To)
	}
	for _, change := range report.Stale {
		fmt.Fprintf(w, "    ! %s/%s · %s → %s\n",
			change.FeatureID, change.RequirementID, emptyLabel(string(change.From)), change.To)
	}
	for _, change := range report.Contradicted {
		fmt.Fprintf(w, "    × %s/%s · %s → %s\n",
			change.FeatureID, change.RequirementID, emptyLabel(string(change.From)), change.To)
	}
	for _, contribution := range report.NewlyUnclassified {
		fmt.Fprintf(w, "    ? %s\n", contributionIdentityLabel(contribution.Identity))
	}
	printRevisionDiagnosticsHuman(w, "Base", report.BaseDiagnostics)
	printRevisionDiagnosticsHuman(w, "Head", report.HeadDiagnostics)
	fmt.Fprintln(w)
}

func printRevisionDiagnosticsHuman(w io.Writer, label string, diagnostics []diag.Diagnostic) {
	if len(diagnostics) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s diagnostics (%d):\n", label, len(diagnostics))
	for _, finding := range diagnostics {
		field := ""
		if finding.Field != "" {
			field = " · " + finding.Field
		}
		fmt.Fprintf(w, "    [%s] %s%s\n", finding.Severity, finding.Code, field)
		fmt.Fprintf(w, "      %s\n", strings.TrimSpace(finding.Message))
	}
}
