package turbo

import (
	"errors"
	"fmt"
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Job names of the workspace tasks.
const (
	// SyncJob writes each package's putnami.json.
	SyncJob = "workspace-sync"
	// CheckJob fails on a stale package graph.
	CheckJob = "turbo-graph-check"
)

// Diagnostic codes.
const (
	// CodeSyncFailed is a sync that failed and wrote nothing.
	CodeSyncFailed = "turbo.sync_failed"
	// CodeSyncStale is a package whose putnami.json no longer matches what
	// the sync writes.
	CodeSyncStale = "turbo.sync_stale"
	// CodeTaskUnmapped is a turbo.json task no Putnami task maps.
	CodeTaskUnmapped = "turbo.task_unmapped"
	// CodeReportsUnnamed is a delegated package whose test task names no
	// JUnit report.
	CodeReportsUnnamed = "turbo.reports_unnamed"
)

// RunSync is the job of the workspace sync task. The dry-run parameter
// prints the diff of each file and writes nothing; the mode parameter stands
// for the mode the workspace config records.
func RunSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	opts := Options{Mode: ctx.Params.String("mode"), DryRun: ctx.Params.Bool("dry-run", false, "dryRun")}
	result, err := Sync(ctx.WorkspaceRoot, opts)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	// A lifecycle run shows a job's summary only when it is always visible:
	// the report and its diffs are what the person asked to see.
	emit.SummaryWithData(strings.Join(Report(result, opts.DryRun), "\n"), map[string]any{"visibility": "always"})
	for _, u := range result.Unmapped {
		emit.DiagnosticWithCode("info", fmt.Sprintf("turbo.json task %s is not mapped: %s", u.Task, u.Reason), "turbo.json", 0, 0, CodeTaskUnmapped)
	}
	for _, dir := range result.Unreported {
		emit.DiagnosticWithCode("info", fmt.Sprintf("%s names no JUnit report: its test result is the exit code of turbo run test alone; name its reports in options[%q].tasks.test.reports of its putnami.json", dir, ExtensionName), dir+"/putnami.json", 0, 0, CodeReportsUnnamed)
	}
	return "OK", map[string]any{
		"mode": result.Mode, "dryRun": opts.DryRun, "written": result.Written, "retracted": result.Retracted,
		"unmapped": result.Unmapped, "unreported": result.Unreported,
	}, nil
}

// Report is the human summary of a sync: what it wrote, or with dryRun would
// write, with each diff, then the tasks it does not map and the packages
// whose test result is their exit code.
func Report(r Result, dryRun bool) []string {
	if !r.Repository {
		return []string{"no turbo.json and Node workspace at the workspace root: nothing to synchronize"}
	}
	verb := "written"
	if dryRun {
		verb = "to write"
	}
	lines := []string{fmt.Sprintf("Turborepo packages in %s mode: %d putnami.json %s, %d current, %d former packages retracted", r.Mode, len(r.Written), verb, r.Unchanged, len(r.Retracted))}
	if dryRun {
		for _, d := range r.Diffs {
			lines = append(lines, strings.TrimSuffix(d, "\n"))
		}
	}
	if len(r.Unmapped) > 0 {
		lines = append(lines, "Not mapped, kept on Turborepo:")
		for _, u := range r.Unmapped {
			lines = append(lines, fmt.Sprintf("  %s: %s", u.Task, u.Reason))
		}
	}
	if len(r.Unreported) > 0 {
		lines = append(lines, fmt.Sprintf("Test result from the exit code alone, no JUnit report named: %s", strings.Join(r.Unreported, ", ")))
	}
	return lines
}

// RunCheck is the job of the validate task: it fails when a sync would
// change a putnami.json, so a package manifest that gained or lost a
// workspace dependency cannot pass a gate that selected the projects on the
// old edges.
func RunCheck(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	result, err := Sync(ctx.WorkspaceRoot, Options{DryRun: true})
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	stale := result.Stale()
	if len(stale) == 0 {
		return "OK", nil, nil
	}
	for i, file := range stale {
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s no longer matches the package manifests and turbo.json: run putnami projects sync and commit the result\n%s", file, result.Diffs[i]), file, 0, 0, CodeSyncStale)
	}
	return "FAILED", nil, errors.New(CodeSyncStale)
}
