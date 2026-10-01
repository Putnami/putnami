// Architecture engines expose the ARC/DARC protocol through one read-only
// workspace evaluation. Authored declarations remain the authority; this
// extension contributes only repository discovery, the resolved Putnami
// dependency graph, and the immutable Git baseline used by the shrink-only
// ratchet.
package sdd

import (
	"fmt"
	"sort"
	"time"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// ArchitectureCounts is the compact repository projection shared by human and
// structured validation output.
type ArchitectureCounts struct {
	Domains       int `json:"domains"`
	Exports       int `json:"exports"`
	Imports       int `json:"imports"`
	DeclaredEdges int `json:"declaredEdges"`
	ObservedEdges int `json:"observedEdges"`
	Findings      int `json:"findings"`
}

// ArchitectureDiagnosticCounts makes a warning-only successful validation
// distinguishable from a structural failure.
type ArchitectureDiagnosticCounts struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
}

// ArchitectureValidationReport carries both protocol-input diagnostics and
// ratcheted repository findings. A structural failure has no coverage or
// snapshot-derived counts because no global graph was published.
type ArchitectureValidationReport struct {
	Valid            bool                         `json:"valid"`
	Baseline         BaselineStatus               `json:"baseline"`
	Summary          ArchitectureCounts           `json:"summary"`
	Coverage         *archproto.DetectionCoverage `json:"coverage,omitempty"`
	Ratchet          *archproto.RatchetSummary    `json:"ratchet,omitempty"`
	Diagnostics      []diag.Diagnostic            `json:"diagnostics"`
	DiagnosticCounts ArchitectureDiagnosticCounts `json:"diagnosticCounts"`
	Findings         []archproto.Finding          `json:"findings"`
}

// ArchitectureTaskReport is the automatic DAG task's policy envelope around
// the unchanged architecture evaluation report. The anonymous pointer keeps
// evaluated payloads byte-compatible at the top level; it is nil in off mode,
// so a skipped task does not invent `valid`, `findings`, or graph counts.
type ArchitectureTaskReport struct {
	*ArchitectureValidationReport
	Mode                featureproto.VerificationMode       `json:"mode"`
	ModeSource          featureproto.VerificationModeSource `json:"modeSource"`
	AutomaticEvaluation bool                                `json:"automaticEvaluation"`
}

// StructurallyCoherent reports whether the engine produced a complete graph
// evaluation. Only then may report mode turn architecture findings into an
// advisory verdict. Parser/provider/config failures have no coverage or carry
// diagnostic errors and remain blocking in every mode.
func (r ArchitectureValidationReport) StructurallyCoherent() bool {
	return r.Coverage != nil && r.DiagnosticCounts.Errors == 0
}

// ArchitectureSnapshotReport adds evaluation provenance and structural
// diagnostics to the deterministic protocol snapshot.
type ArchitectureSnapshotReport struct {
	Snapshot    *archproto.Snapshot `json:"snapshot,omitempty"`
	Baseline    BaselineStatus      `json:"baseline"`
	Diagnostics []diag.Diagnostic   `json:"diagnostics"`
}

// ArchitectureInspectionReport is a bounded view of one exact domain and all
// declared or observed relationships that touch it.
type ArchitectureInspectionReport struct {
	Requested string                `json:"requested"`
	Domain    *archproto.DomainView `json:"domain,omitempty"`
	// Inbound contains producer-to-consumer data-flow edges whose consumer is
	// the selected domain: authoritative facts flow into this domain.
	Inbound []archproto.DeclaredEdge `json:"inbound"`
	// Outbound contains producer-to-consumer data-flow edges whose producer is
	// the selected domain: its authoritative facts flow to another domain.
	Outbound    []archproto.DeclaredEdge     `json:"outbound"`
	Observed    []archproto.ObservedEdge     `json:"observed"`
	Findings    []archproto.Finding          `json:"findings"`
	Coverage    *archproto.DetectionCoverage `json:"coverage,omitempty"`
	Ratchet     *archproto.RatchetSummary    `json:"ratchet,omitempty"`
	Baseline    BaselineStatus               `json:"baseline"`
	Diagnostics []diag.Diagnostic            `json:"diagnostics"`
}

// BuildArchitectureValidationResult validates declarations, compares the exact
// mapped project edges, and applies baseline/waiver policy. The returned error
// is the run's verdict, with the report attached for the failure envelope.
func BuildArchitectureValidationResult(ws *workspace.Workspace, baselineRef string) (ArchitectureValidationReport, error) {
	result := EvaluateWorkspace(ws, EvaluationOptions{BaselineRef: baselineRef, Today: time.Now().UTC()})
	report := architectureValidationReport(result)
	return report, architectureEvaluationError("architecture validate", result, report)
}

// BuildArchitectureWorktreeValidationResult is the same verdict over the
// CURRENT WORKTREE ALONE: declarations, exact mapped project edges, live
// waivers, and no git history at all.
//
// It is what the cached `architecture-validate` DAG task runs (D9). That
// task's declared inputs are committed file patterns only — ARC declarations,
// capability evidence, and the workspace policy file; a baseline comparison
// would fold in which commit `origin/HEAD` names and what the branch forked
// from, neither of which is in the key, so a stored answer would depend on
// unkeyed state. Dropping the comparison is what makes the task honestly
// cacheable.
//
// What it costs is stated rather than hidden: with no baseline, existing debt
// is not distinguished from a new violation, so this evaluation is STRICTER,
// never laxer. Shrink-only ratcheting remains `architecture validate
// --baseline <ref>`, an interactive and review concern.
func BuildArchitectureWorktreeValidationResult(ws *workspace.Workspace) (ArchitectureValidationReport, error) {
	result := EvaluateWorkspace(ws, EvaluationOptions{WorktreeOnly: true, Today: time.Now().UTC()})
	report := architectureValidationReport(result)
	return report, architectureEvaluationError("architecture validate", result, report)
}

// BuildArchitectureSnapshotResult returns the complete deterministic machine
// view of one evaluation.
func BuildArchitectureSnapshotResult(ws *workspace.Workspace, baselineRef string) (ArchitectureSnapshotReport, error) {
	result := EvaluateWorkspace(ws, EvaluationOptions{BaselineRef: baselineRef, Today: time.Now().UTC()})
	report := ArchitectureSnapshotReport{
		Snapshot:    result.Snapshot,
		Baseline:    result.Baseline,
		Diagnostics: copyArchitectureDiagnostics(result.Diagnostics),
	}
	return report, architectureEvaluationError("architecture snapshot", result, report)
}

// BuildArchitectureInspectionResult selects one exact declared domain. It never
// guesses by directory or owner because the manifest's domain field mints the
// semantic identity.
//
// A structurally broken repository is reported first: there is no coherent
// domain view to return, so "the evaluation failed" outranks "that domain is
// not declared" and the caller sees the cause rather than a symptom.
func BuildArchitectureInspectionResult(ws *workspace.Workspace, domain, baselineRef string) (ArchitectureInspectionReport, error) {
	result := EvaluateWorkspace(ws, EvaluationOptions{BaselineRef: baselineRef, Today: time.Now().UTC()})
	return architectureInspectionResult("architecture inspect", domain, result)
}

// BuildArchitectureWorktreeInspectionResult selects one exact declared domain
// from a CURRENT-WORKTREE-ONLY evaluation. It is the agent-facing counterpart
// to BuildArchitectureInspectionResult: both reuse the same evaluator and
// bounded projection, while this variant never resolves Git history or an
// adoption baseline the tool request does not carry.
func BuildArchitectureWorktreeInspectionResult(ws *workspace.Workspace, domain string) (ArchitectureInspectionReport, error) {
	result := EvaluateWorkspace(ws, EvaluationOptions{WorktreeOnly: true, Today: time.Now().UTC()})
	return architectureInspectionResult("architecture context", domain, result)
}

func architectureInspectionResult(command, domain string, result Result) (ArchitectureInspectionReport, error) {
	report := buildArchitectureInspection(domain, result)
	if result.Snapshot == nil || diag.HasErrors(result.Diagnostics) || archproto.HasBlockingFindings(result.Snapshot.Findings) {
		return report, architectureEvaluationError(command, result, report)
	}
	if report.Domain == nil {
		report.Diagnostics = append(report.Diagnostics, diag.Errorf(
			archproto.ErrorCodeUnknownDomain,
			"domain",
			"domain %q is not declared; use an exact manifest domain ID",
			domain,
		))
		return report, WithResultData(
			protocolcli.Classify(fmt.Errorf("architecture domain %q was not found", domain), protocolcli.ErrNoMatch),
			report,
		)
	}
	return report, nil
}

func architectureValidationReport(result Result) ArchitectureValidationReport {
	report := ArchitectureValidationReport{
		Baseline:         result.Baseline,
		Diagnostics:      copyArchitectureDiagnostics(result.Diagnostics),
		DiagnosticCounts: countArchitectureDiagnostics(result.Diagnostics),
		Findings:         []archproto.Finding{},
	}
	if result.Snapshot == nil {
		return report
	}
	report.Valid = !diag.HasErrors(result.Diagnostics) && !archproto.HasBlockingFindings(result.Snapshot.Findings)
	report.Summary = summarizeArchitecture(result.Snapshot)
	coverage := result.Snapshot.Coverage
	ratchet := result.Snapshot.Ratchet
	report.Coverage = &coverage
	report.Ratchet = &ratchet
	report.Findings = append(report.Findings, result.Snapshot.Findings...)
	return report
}

func buildArchitectureInspection(domain string, result Result) ArchitectureInspectionReport {
	report := ArchitectureInspectionReport{
		Requested:   domain,
		Inbound:     []archproto.DeclaredEdge{},
		Outbound:    []archproto.DeclaredEdge{},
		Observed:    []archproto.ObservedEdge{},
		Findings:    []archproto.Finding{},
		Baseline:    result.Baseline,
		Diagnostics: copyArchitectureDiagnostics(result.Diagnostics),
	}
	if result.Snapshot == nil {
		return report
	}
	coverage := result.Snapshot.Coverage
	ratchet := result.Snapshot.Ratchet
	report.Coverage = &coverage
	report.Ratchet = &ratchet
	for index := range result.Snapshot.Graph.Domains {
		candidate := result.Snapshot.Graph.Domains[index]
		if candidate.ID == domain {
			copy := candidate
			report.Domain = &copy
			break
		}
	}
	for _, edge := range result.Snapshot.Graph.Edges {
		if edge.ConsumerDomain == domain {
			report.Inbound = append(report.Inbound, edge)
		}
		if edge.ProducerDomain == domain {
			report.Outbound = append(report.Outbound, edge)
		}
	}
	for _, edge := range result.Snapshot.Observed {
		if edge.ConsumerDomain == domain || edge.ProducerDomain == domain {
			report.Observed = append(report.Observed, edge)
		}
	}
	for _, finding := range result.Snapshot.Findings {
		if finding.Edge == nil || finding.Edge.ConsumerDomain == domain || finding.Edge.ProducerDomain == domain {
			report.Findings = append(report.Findings, finding)
		}
	}
	return report
}

func architectureEvaluationError(command string, result Result, data any) error {
	if result.Snapshot != nil && !diag.HasErrors(result.Diagnostics) && !archproto.HasBlockingFindings(result.Snapshot.Findings) {
		return nil
	}
	errorCount := countArchitectureDiagnostics(result.Diagnostics).Errors
	if result.Snapshot != nil {
		for _, finding := range result.Snapshot.Findings {
			if finding.Severity == diag.Error {
				errorCount++
			}
		}
	}
	if errorCount == 0 {
		errorCount = 1
	}
	err := protocolcli.Classify(
		fmt.Errorf("%s failed with %d blocking finding(s)", command, errorCount),
		protocolcli.ErrInvalidConfig,
	)
	return WithResultData(err, data)
}

func summarizeArchitecture(snapshot *archproto.Snapshot) ArchitectureCounts {
	var counts ArchitectureCounts
	if snapshot == nil {
		return counts
	}
	counts.Domains = len(snapshot.Graph.Domains)
	counts.DeclaredEdges = len(snapshot.Graph.Edges)
	counts.ObservedEdges = len(snapshot.Observed)
	counts.Findings = len(snapshot.Findings)
	for _, domain := range snapshot.Graph.Domains {
		counts.Exports += len(domain.Exports)
		counts.Imports += len(domain.Imports)
	}
	return counts
}

func countArchitectureDiagnostics(diagnostics []diag.Diagnostic) ArchitectureDiagnosticCounts {
	var counts ArchitectureDiagnosticCounts
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

func copyArchitectureDiagnostics(input []diag.Diagnostic) []diag.Diagnostic {
	output := append([]diag.Diagnostic{}, input...)
	sort.Slice(output, func(i, j int) bool {
		left, right := output[i], output[j]
		if diag.SeverityRank(left.Severity) != diag.SeverityRank(right.Severity) {
			return diag.SeverityRank(left.Severity) < diag.SeverityRank(right.Severity)
		}
		if left.Severity != right.Severity {
			return left.Severity < right.Severity
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Message < right.Message
	})
	return output
}
