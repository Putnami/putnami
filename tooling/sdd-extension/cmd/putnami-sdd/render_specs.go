package main

import (
	"fmt"
	"io"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// The human half of the four `specs` subcommands, ported verbatim from
// `tooling/cli/internal/commands/sdd/specs.go`.

// specHumanDiagnosticLimit bounds how many findings human output prints.
//
// Structured output always carries the complete list; a human reading a
// workspace-wide completeness sweep must not have the errors scrolled away by a
// hundred authoring reminders, so the cap applies to warnings and info only.
const specHumanDiagnosticLimit = 20

func printSpecCatalogHuman(w io.Writer, report sdd.SpecCatalogReport) {
	fmt.Fprintf(w, "\n  Specs (%d)\n", report.Counts.Specs)
	printSelectionHuman(w, report.Selection)
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	fmt.Fprintf(w, "  %d authored feature(s) · %d without a spec\n",
		report.Counts.AuthoredFeatures, report.Counts.FeaturesWithoutSpec)
	for _, summary := range report.Specs {
		state := ""
		if !summary.Valid {
			state = " · invalid"
		}
		fmt.Fprintf(w, "\n  %s%s\n", summary.Feature, state)
		fmt.Fprintf(w, "    source: %s\n", specSourceLabel(summary.Path, summary.Project))
		for _, outcome := range summary.Outcomes {
			fmt.Fprintf(w, "    outcome: %s\n", outcome)
		}
		if summary.OutcomeCount > len(summary.Outcomes) {
			fmt.Fprintf(w, "    … %d more outcome(s)\n", summary.OutcomeCount-len(summary.Outcomes))
		}
		fmt.Fprintf(w, "    %d non-goal(s) · %d requirement(s) · %d decision(s)\n",
			summary.NonGoals, summary.Requirements, summary.Decisions)
	}
	printSpecDiagnostics(w, report.Diagnostics)
	fmt.Fprintln(w)
}

func printSpecContextHuman(w io.Writer, report sdd.SpecContextReport) {
	fmt.Fprintf(w, "\n  Feature %s\n", report.Feature)
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	for _, declaration := range report.Declarations {
		fmt.Fprintf(w, "  Declared: %s · %s\n", declaration.Name, declaration.Source)
		fmt.Fprintf(w, "    outcome: %s\n", declaration.Outcome)
		fmt.Fprintf(w, "    owner: %s\n", declaration.Owner)
	}
	if report.Spec == nil {
		fmt.Fprintln(w, "  Spec: none")
		printSpecDiagnostics(w, report.Diagnostics)
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintf(w, "  Spec: %s\n", specSourceLabel(report.Source.Path, report.Source.Project))
	fmt.Fprintln(w, "  Outcomes:")
	for _, outcome := range report.Spec.Outcomes {
		fmt.Fprintf(w, "    %s\n", outcome)
	}
	if len(report.Spec.NonGoals) > 0 {
		fmt.Fprintln(w, "  Non-goals:")
		for _, nonGoal := range report.Spec.NonGoals {
			fmt.Fprintf(w, "    %s\n", nonGoal)
		}
	}
	fmt.Fprintln(w, "  Requirements:")
	if len(report.Spec.Requirements) == 0 {
		fmt.Fprintln(w, "    none authored yet")
	}
	for _, requirement := range report.Spec.Requirements {
		fmt.Fprintf(w, "    [%s] %s\n", requirement.ID, requirement.Text)
	}
	if len(report.Decisions) > 0 {
		fmt.Fprintln(w, "  Decisions:")
		for _, decision := range report.Decisions {
			state := "present"
			if !decision.Exists {
				state = "missing"
			}
			fmt.Fprintf(w, "    [%s] %s\n", state, decision.Path)
		}
	}
	printSpecDiagnostics(w, report.Diagnostics)
	fmt.Fprintln(w)
}

func printSpecValidationHuman(w io.Writer, report sdd.SpecValidationReport) {
	status := "passed"
	if !report.Valid {
		status = "failed"
	}
	fmt.Fprintf(w, "\n  Spec validation %s\n", status)
	printSelectionHuman(w, report.Selection)
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	fmt.Fprintf(w, "  %d spec(s) · %d authored feature(s) · %d publishable project(s)\n",
		report.Summary.Specs, report.Summary.AuthoredFeatures, report.Summary.PublishableProjects)
	fmt.Fprintf(w, "  Completeness: %d feature(s) without a spec · %d project(s) without a support entry · %d project(s) without a feature link\n",
		len(report.Completeness.FeaturesWithoutSpec),
		len(report.Completeness.ProjectsWithoutSupport),
		len(report.Completeness.ProjectsWithoutFeature))
	if !report.Completeness.SupportAssessed {
		fmt.Fprintln(w, "  Support completeness was not assessed (no readable support catalog)")
	}
	fmt.Fprintf(w, "  Diagnostics: %d error(s) · %d warning(s) · %d info\n",
		report.Counts.Errors, report.Counts.Warnings, report.Counts.Info)
	printSpecDiagnostics(w, report.Diagnostics)
	if !report.Valid {
		printDocsHint(w)
	}
	fmt.Fprintln(w)
}

func printSpecVerifyHuman(w io.Writer, report sdd.SpecVerifyReport) {
	fmt.Fprintf(w, "\n  Spec verification · revision %s\n", featureRevisionLabel(report.Revision))
	if report.Recorded {
		fmt.Fprintf(w, "  Session: %s (%s)\n", report.Session, report.GeneratedAt)
	} else {
		fmt.Fprintln(w, "  Session: none recorded — states assume no observations")
	}
	printSelectionHuman(w, report.Selection)
	for _, group := range report.Groups {
		state := "ok"
		switch {
		case group.Blocked:
			state = "BLOCKED"
		case !group.AutomaticEvaluation:
			state = "off (automaticEvaluation: false)"
		}
		fmt.Fprintf(w, "  %s · %s [%s, from %s] — %s\n",
			group.Feature, group.Project, group.Mode, group.ModeSource, state)
		for _, requirement := range group.Requirements {
			fmt.Fprintf(w, "    %-14s %s\n", requirement.State, requirement.Requirement)
			for _, check := range requirement.Checks {
				measured := ""
				if check.Measurement != nil {
					// The observed aggregate a threshold verdict was decided
					// from — the authored target stays in the manifest, and
					// the state already carries the recomputed outcome.
					measured = fmt.Sprintf(" · measured %g%s (%s %s)",
						check.Measurement.Value, check.Measurement.Unit,
						check.Measurement.Aggregation, check.Measurement.Name)
				}
				location := ""
				if check.Path != "" {
					location = " · " + check.Path
					if check.Symbol != "" {
						location += "#" + check.Symbol
					}
				}
				fmt.Fprintf(w, "      %-12s %s (%s)%s%s\n", check.State, check.Check, check.Reason, measured, location)
			}
		}
		for _, reference := range group.Reports {
			// A restored reference is bytes the gate recovered from the producing
			// task's cache entry instead of a task that ran in the replayed
			// session. Saying so is the point of recording it: a reader
			// auditing a verdict must be able to tell the two apart.
			origin := ""
			if reference.Restored {
				origin = " · restored from cache"
			}
			fmt.Fprintf(w, "    report %s · %s%s\n", reference.Path, reference.Digest, origin)
		}
	}
	fmt.Fprintf(w, "  Requirements: %d textual · %d executable · %d verified · %d unmapped · %d unexecutable · %d missing · %d stale · %d contradicted · %d unobserved\n",
		report.Counts.SpecRequirements, report.Counts.Executable, report.Counts.Verified, report.Counts.Unmapped,
		report.Counts.Unexecutable, report.Counts.Missing, report.Counts.Stale, report.Counts.Contradicted,
		report.Counts.Unobserved)
	if len(report.Blocked) > 0 {
		fmt.Fprintf(w, "  Blocked: %s\n", strings.Join(report.Blocked, ", "))
	}
	printSpecDiagnostics(w, report.Diagnostics)
	fmt.Fprintln(w)
}

// printSpecsBaselineHuman renders the derived enforce floor and whether the
// committed specs.baseline.json files match it.
func printSpecsBaselineHuman(w io.Writer, report sdd.SpecsBaselineReport) {
	fmt.Fprintf(w, "\n  Specs baseline (%d file(s), one per enforced project)\n", len(report.Paths))
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	if report.Current != nil {
		fmt.Fprintf(w, "  Enforced projects: %d\n", len(report.Current.Projects))
		for _, project := range report.Current.Projects {
			fmt.Fprintf(w, "    %s · %d executable requirement(s)\n",
				project.Project, len(project.ExecutableRequirements))
			for _, requirement := range project.ExecutableRequirements {
				fmt.Fprintf(w, "      %s\n", requirement)
			}
		}
	}
	for _, file := range report.Removed {
		if report.Updated {
			fmt.Fprintf(w, "  Removed %s\n", file)
		} else {
			fmt.Fprintf(w, "  No longer named by the floor: %s\n", file)
		}
	}
	switch {
	case report.Updated:
		fmt.Fprintf(w, "  Updated: the committed baselines now record this floor\n")
	case report.Changed:
		fmt.Fprintf(w, "  Out of date: the committed baselines differ; run `putnami specs baseline --update`\n")
	case report.Current != nil:
		// A report without a derived floor failed before any comparison; its
		// error and diagnostics speak, and no verdict line may claim otherwise.
		fmt.Fprintf(w, "  Up to date: the committed baselines record this floor\n")
	}
	printSpecDiagnostics(w, report.Diagnostics)
	fmt.Fprintln(w)
}

func printSpecInitHuman(w io.Writer, report sdd.SpecInitReport) {
	switch {
	case report.Created:
		fmt.Fprintf(w, "\n  Created %s\n", specSourceLabel(report.Path, report.Project))
	case report.DryRun && report.Contents != "":
		fmt.Fprintf(w, "\n  Would create %s\n", specSourceLabel(report.Path, report.Project))
	default:
		fmt.Fprintf(w, "\n  No spec was created for %s\n", report.Feature)
	}
	fmt.Fprintf(w, "  Revision: %s\n", featureRevisionLabel(report.Revision))
	if report.Contents != "" {
		fmt.Fprintln(w)
		for _, line := range strings.Split(strings.TrimRight(report.Contents, "\n"), "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	printSpecDiagnostics(w, report.Diagnostics)
	fmt.Fprintln(w)
}

// printSpecDiagnostics prints every error and a bounded number of warnings.
func printSpecDiagnostics(w io.Writer, diagnostics []diag.Diagnostic) {
	if len(diagnostics) == 0 {
		return
	}
	fmt.Fprintf(w, "  Diagnostics (%d):\n", len(diagnostics))
	printed := 0
	for _, finding := range diagnostics {
		if finding.Severity != diag.Error && printed >= specHumanDiagnosticLimit {
			continue
		}
		printed++
		field := ""
		if finding.Field != "" {
			field = " · " + finding.Field
		}
		fmt.Fprintf(w, "    [%s] %s%s\n", finding.Severity, finding.Code, field)
		fmt.Fprintf(w, "      %s\n", finding.Message)
	}
	if remaining := len(diagnostics) - printed; remaining > 0 {
		fmt.Fprintf(w, "    … %d more (use --output=json for the complete list)\n", remaining)
	}
}
