package main

import (
	"fmt"
	"io"
	"strings"

	archproto "go.putnami.dev/protocol/architecture"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// The human half of the three `architecture` subcommands, ported verbatim from
// `tooling/cli/internal/commands/sdd/architecture.go`.

func printArchitectureValidationHuman(w io.Writer, report sdd.ArchitectureValidationReport) {
	status := "passed"
	if !report.Valid {
		status = "failed"
	}
	fmt.Fprintf(w, "\n  Architecture validation %s\n", status)
	printArchitectureCounts(w, report.Summary)
	printArchitectureBaseline(w, report.Baseline)
	fmt.Fprintf(w, "  Diagnostics: %d error(s) · %d warning(s) · %d info\n",
		report.DiagnosticCounts.Errors, report.DiagnosticCounts.Warnings, report.DiagnosticCounts.Info)
	printArchitectureDiagnostics(w, report.Diagnostics)
	printArchitectureFindings(w, report.Findings)
	if !report.Valid {
		printDocsHint(w)
	}
	fmt.Fprintln(w)
}

func printArchitectureSnapshotHuman(w io.Writer, report sdd.ArchitectureSnapshotReport) {
	if report.Snapshot == nil {
		fmt.Fprintln(w, "\n  Architecture snapshot unavailable")
		printArchitectureDiagnostics(w, report.Diagnostics)
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintln(w, "\n  Architecture snapshot")
	printArchitectureCounts(w, sdd.SummarizeArchitecture(report.Snapshot))
	printArchitectureCoverage(w, report.Snapshot.Coverage)
	printArchitectureBaseline(w, report.Baseline)
	printArchitectureDiagnostics(w, report.Diagnostics)
	printArchitectureFindings(w, report.Snapshot.Findings)
	fmt.Fprintln(w)
}

func printArchitectureInspectionHuman(w io.Writer, report sdd.ArchitectureInspectionReport) {
	if report.Domain == nil {
		fmt.Fprintf(w, "\n  Architecture domain %q unavailable\n", report.Requested)
		printArchitectureDiagnostics(w, report.Diagnostics)
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintf(w, "\n  Architecture domain %s\n", report.Domain.ID)
	fmt.Fprintf(w, "  Owner: %s · source: %s\n", report.Domain.Owner, report.Domain.Source)
	fmt.Fprintf(w, "  %d project(s) · %d owned concept(s) · %d export(s) · %d import(s)\n",
		len(report.Domain.Projects), len(report.Domain.Owns), len(report.Domain.Exports), len(report.Domain.Imports))
	fmt.Fprintf(w, "  Relationships: %d inbound · %d outbound · %d observed\n",
		len(report.Inbound), len(report.Outbound), len(report.Observed))
	printArchitectureCoverage(w, *report.Coverage)
	printArchitectureBaseline(w, report.Baseline)
	printArchitectureDiagnostics(w, report.Diagnostics)
	printArchitectureFindings(w, report.Findings)
	fmt.Fprintln(w)
}

func printArchitectureCounts(w io.Writer, counts sdd.ArchitectureCounts) {
	fmt.Fprintf(w, "  %d domain(s) · %d export(s) · %d import(s)\n", counts.Domains, counts.Exports, counts.Imports)
	fmt.Fprintf(w, "  Edges: %d declared · %d observed · %d finding(s)\n",
		counts.DeclaredEdges, counts.ObservedEdges, counts.Findings)
}

func printArchitectureCoverage(w io.Writer, coverage archproto.DetectionCoverage) {
	fmt.Fprintf(w, "  Coverage: projects=%s · database=%s · http=%s · events=%s\n",
		coverage.ProjectDependencies, coverage.Database, coverage.HTTP, coverage.Events)
}

func printArchitectureBaseline(w io.Writer, baseline sdd.BaselineStatus) {
	if !baseline.Compared {
		fmt.Fprintln(w, "  Baseline: no local baseline file; shrink-only comparison not required")
		return
	}
	prior := "absent at comparison commit"
	if baseline.PriorFile {
		prior = "loaded"
	}
	fmt.Fprintf(w, "  Baseline: %s (%s; prior file %s)\n", baseline.Commit, baseline.Requested, prior)
}

func printArchitectureDiagnostics(w io.Writer, diagnostics []diag.Diagnostic) {
	if len(diagnostics) == 0 {
		return
	}
	fmt.Fprintf(w, "  Input diagnostics (%d):\n", len(diagnostics))
	for _, finding := range diagnostics {
		field := ""
		if finding.Field != "" {
			field = " · " + finding.Field
		}
		fmt.Fprintf(w, "    [%s] %s%s\n", finding.Severity, finding.Code, field)
		fmt.Fprintf(w, "      %s\n", finding.Message)
	}
}

func printArchitectureFindings(w io.Writer, findings []archproto.Finding) {
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(w, "  Architecture findings (%d):\n", len(findings))
	for _, finding := range findings {
		fmt.Fprintf(w, "    [%s] %s · %s\n", finding.Severity, finding.Code, finding.Disposition)
		fmt.Fprintf(w, "      %s\n", finding.Message)
		fmt.Fprintf(w, "      %s\n", strings.TrimSpace(finding.ID))
	}
}

// The human half of the two authoring subcommands. They have no
// recorded parity oracle: both were added after the extraction, so there is no
// built-in answer to match — see doc/05-parity.md.

func printArchitectureInitHuman(w io.Writer, report sdd.ArchitectureInitReport) {
	if report.Existing != "" {
		fmt.Fprintf(w, "\n  Domain %s is already declared by %s\n", report.Domain, report.Existing)
		printArchitectureDiagnostics(w, report.Diagnostics)
		fmt.Fprintln(w)
		return
	}
	verb := "Would create"
	if report.Created {
		verb = "Created"
	}
	fmt.Fprintf(w, "\n  %s %s\n", verb, report.Path)
	fmt.Fprintf(w, "  Domain: %s · owner: %s · %d project(s)\n", report.Domain, report.Owner, len(report.Projects))
	for _, project := range report.Projects {
		fmt.Fprintf(w, "    %s\n", project)
	}
	printArchitectureDiagnostics(w, report.Diagnostics)
	if report.Contents != "" && report.DryRun {
		fmt.Fprintf(w, "\n%s", report.Contents)
	}
	// The scaffold states only what the caller stated. Naming what it left out
	// is the whole handover: exports and imports are agreements between two
	// domains, and a generator that guessed one would be writing a contract
	// nobody negotiated.
	fmt.Fprintln(w, "\n  Next: declare what the domain owns, what it exports, and what it imports.")
	fmt.Fprintln(w)
}

func printArchitectureSyncHuman(w io.Writer, report sdd.ArchitectureSyncReport) {
	mode := "suggestion"
	if report.Applied {
		mode = "applied"
	}
	fmt.Fprintf(w, "\n  Architecture sync (%s)\n", mode)
	fmt.Fprintf(w, "  %d manifest(s) · %d changed · %d written\n",
		report.Summary.Manifests, report.Summary.Changed, report.Summary.Written)
	fmt.Fprintf(w, "  Bindings: %d to add · %d to remove · Projects: %d to remove\n",
		report.Summary.AddedBindings, report.Summary.RemovedBindings, report.Summary.RemovedProjects)
	for _, manifest := range report.Manifests {
		if !manifest.Changed {
			continue
		}
		fmt.Fprintf(w, "  %s (%s):\n", manifest.Path, manifest.Domain)
		for _, project := range manifest.RemovedProjects {
			fmt.Fprintf(w, "    - project %s (not in the resolved workspace)\n", project)
		}
		for _, binding := range manifest.RemovedBindings {
			fmt.Fprintf(w, "    - binding %s\n", syncBindingLabel(binding))
		}
		for _, binding := range manifest.AddedBindings {
			fmt.Fprintf(w, "    + binding %s\n", syncBindingLabel(binding))
		}
	}
	printArchitectureSyncRefusals(w, report.Refusals)
	printArchitectureDiagnostics(w, report.Diagnostics)
	if !report.Applied && report.Summary.Changed > 0 {
		fmt.Fprintln(w, "  Nothing was written. Re-run with --apply, then review the diff: it is the authorization.")
	}
	fmt.Fprintln(w)
}

// printArchitectureSyncRefusals names every decision sync declined to make. It
// is the most important part of the output: an undeclared relation stays a
// failing finding until a human declares the contract, and sync never grants one.
func printArchitectureSyncRefusals(w io.Writer, refusals []sdd.ArchitectureSyncRefusal) {
	if len(refusals) == 0 {
		return
	}
	fmt.Fprintf(w, "  Refused (%d) — these need a declaration, not a generator:\n", len(refusals))
	for _, refusal := range refusals {
		fmt.Fprintf(w, "    [%s] %s -> %s\n", refusal.Reason, refusal.ConsumerProject, refusal.ProducerProject)
		fmt.Fprintf(w, "      %s\n", refusal.Message)
	}
}

// syncBindingLabel renders one permission the way both output modes name it.
func syncBindingLabel(binding sdd.ArchitectureSyncBinding) string {
	return fmt.Sprintf("%s -> %s (%s)", binding.ConsumerProject, binding.ProducerProject, binding.Import)
}
