package main

import (
	"fmt"
	"io"

	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// The human half of `contracts generate|check`, ported verbatim from
// `tooling/cli/internal/commands/sdd/contracts_command.go`.
//
// Both stay INTERACTIVE-ONLY in v1 (D2): `check` owns an exit-2 drift cycle and
// per-project positional targeting, neither of which the `validate` job's
// project-scoped activation expresses.

func printContractsGenerateHuman(w io.Writer, report sdd.GenerateReport) {
	fmt.Fprintf(w, "\n  Generated %d contract artifact(s) for %s:\n", len(report.Artifacts), report.Manifest)
	for _, path := range report.Artifacts {
		fmt.Fprintf(w, "    %s\n", path)
	}
	fmt.Fprintln(w)
}

// printContractsCheckHuman prints a readable summary of a check report — for a
// clean run AND for a failing one, because the reason a contract check failed
// is the answer the caller is waiting on.
func printContractsCheckHuman(w io.Writer, report sdd.CheckReport) {
	switch report.Outcome {
	case sdd.OutcomeClean:
		fmt.Fprintf(w, "\n  Contracts up to date for %s (no drift, no breaking changes).\n\n", report.Manifest)
	default:
		fmt.Fprintf(w, "\n  Contract check failed for %s (%s):\n", report.Manifest, report.Outcome)
		for _, drift := range report.Drift {
			fmt.Fprintf(w, "    drift: %s (%s)\n", drift.Path, drift.Reason)
		}
		for _, change := range report.BreakingChanges {
			fmt.Fprintf(w, "    breaking: %s %s %s\n", change.Category, change.Kind, change.Name)
		}
		fmt.Fprintln(w)
	}
}
