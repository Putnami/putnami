// Command putnami-clientgen is the extension runtime for project generation and
// workspace-wide synchronization and drift verification.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/runtimeinfo"
	"go.putnami.dev/tooling/clientgen/extension/internal/workspaceclient"
)

const extensionName = "@putnami/clientgen"

var runtimeVersion string

func commandHandlers() map[string]cli.JobFunc {
	return map[string]cli.JobFunc{
		"generate-go":     generateTarget(clientcontract.GeneratedLanguageGo),
		"generate-ts":     generateTarget(clientcontract.GeneratedLanguageTypeScript),
		"workspace-sync":  runWorkspace(workspaceclient.ModeSync),
		"workspace-adopt": runWorkspace(workspaceclient.ModeAdopt),
		"workspace-check": runWorkspace(workspaceclient.ModeCheck),
	}
}

func generateTarget(language clientcontract.GeneratedLanguage) cli.JobFunc {
	return func(ctx *pctx.Context, _ *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		if ctx.Project.Name == "" {
			return "FAILED", nil, errors.New("client generation requires a selected provider project")
		}
		projectRoot := ctx.Project.FullPath
		if projectRoot == "" {
			projectRoot = filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(ctx.Project.Path))
		}
		output, generated, err := workspaceclient.SynchronizeTarget(ctx.WorkspaceRoot, projectRoot, language)
		if err != nil {
			return "FAILED", nil, err
		}
		if !generated {
			return "OK", map[string]any{}, nil
		}
		outputPort := "typescriptClientOutput"
		if language == clientcontract.GeneratedLanguageGo {
			outputPort = "goClientOutput"
		}
		return "OK", map[string]any{outputPort: output}, nil
	}
}

func runWorkspace(mode workspaceclient.Mode) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		if ctx.WorkspaceRoot == "" {
			return "FAILED", nil, errors.New("workspace client command received no workspace root")
		}
		// Each phase is timed and reported (Report.Timings, one metric event per
		// phase). The session record exposes this task as one durationMs, and
		// the split behind that number is what decides where a slow guard
		// spends its time.
		timings := map[string]int64{}
		phase := func(name string, run func() error) error {
			started := time.Now()
			err := run()
			timings[name] = time.Since(started).Milliseconds()
			return err
		}
		var report workspaceclient.Report
		if mode == workspaceclient.ModeCheck {
			// The check builds nothing and renders nothing. It reads the Git
			// candidate cut its `git:**` key holds and the membership the
			// orchestrator resolved from committed manifests, so a cold clone
			// and a tree the session just built reach one verdict; whether the
			// committed clients are what the current contract GENERATES is the
			// generator tasks' own verdict, judged by the engine on their
			// declared outputs (protocols/extension ADR 0004). This task keeps
			// what no generator task can judge.
			report = workspaceclient.InspectCommitted(ctx.WorkspaceRoot, memberPaths(ctx.WorkspaceProjects))
		} else {
			regenerated, err := synchronizeWorkspace(ctx.WorkspaceRoot, mode, phase)
			if err != nil {
				return "FAILED", nil, err
			}
			report = regenerated
		}
		for name, elapsed := range timings {
			if report.Timings == nil {
				report.Timings = map[string]int64{}
			}
			report.Timings[name] = elapsed
		}
		for _, name := range sortedTimingNames(report.Timings) {
			emit.Metric(workspaceclient.PhaseMetricPrefix+name, report.Timings[name], "ms")
		}
		for _, finding := range report.Findings {
			emit.DiagnosticWithCode("error", finding.Message, finding.Path, finding.Line, finding.Column, finding.Code)
		}
		// Censused debt is reported at warning severity: visible on every run,
		// countable, and never the reason a gate is red.
		for _, finding := range report.PendingTransports {
			emit.DiagnosticWithCode("warning", finding.Message, finding.Path, finding.Line, finding.Column, finding.Code)
		}
		// The SDK drops a job's data payload when the job returns an error, so
		// the counts that say HOW MUCH is unverified would disappear exactly
		// when they are needed. The summary is emitted on both paths.
		emit.SummaryWithData(
			fmt.Sprintf("%d generated-client finding(s), %d censused pending transport(s), %d applied adaptation(s), %d/%d operation targets covered",
				len(report.Findings), len(report.PendingTransports), len(report.AppliedAdaptations),
				report.Coverage.Covered, report.Coverage.Required),
			map[string]any{
				"findings":           len(report.Findings),
				"pendingTransports":  len(report.PendingTransports),
				"appliedAdaptations": len(report.AppliedAdaptations),
				"coverage":           report.Coverage,
				"providers":          len(report.Providers),
				"consumerEdges":      len(report.ConsumerEdges),
				"timings":            report.Timings,
			})
		if !report.Clean() {
			return "FAILED", nil, fmt.Errorf("first-party generated client verification found %d issue(s)", len(report.Findings))
		}
		return "OK", map[string]any{"report": report}, nil
	}
}

// synchronizeWorkspace is the sync and adopt path: build every provider through
// the spawning CLI, regenerate every target in place, compile every consumer,
// then verify the worktree against a fresh render. Adoption additionally reads
// the manifests as they stood before the regeneration, so an import or
// construction rename is proven by two real emitter outputs.
func synchronizeWorkspace(workspaceRoot string, mode workspaceclient.Mode, phase func(string, func() error) error) (workspaceclient.Report, error) {
	var report workspaceclient.Report
	// The "before" manifests are read before ANY write of this command: the
	// provider build below already regenerates the same-language targets.
	var before string
	var snapshotFindings []workspaceclient.Finding
	if mode == workspaceclient.ModeAdopt {
		var cleanup func()
		if err := phase(workspaceclient.PhaseSnapshot, func() (err error) {
			before, cleanup, snapshotFindings, err = workspaceclient.SnapshotGeneratedArtifacts(workspaceRoot)
			return err
		}); err != nil {
			return report, err
		}
		defer cleanup()
	}
	if err := phase(workspaceclient.PhaseMaterialize, func() error {
		return workspaceclient.MaterializeWorkspaceContracts(workspaceRoot)
	}); err != nil {
		return report, err
	}
	if err := phase(workspaceclient.PhaseSynchronize, func() error {
		return workspaceclient.Synchronize(workspaceRoot)
	}); err != nil {
		return report, err
	}
	var applied []workspaceclient.Adaptation
	if mode == workspaceclient.ModeAdopt {
		adopted, err := workspaceclient.ApplySafeBindingAdoptions(workspaceRoot, before)
		if err != nil {
			return report, err
		}
		applied = adopted
	}
	if err := workspaceclient.CompileWorkspaceConsumers(workspaceRoot); err != nil {
		return report, err
	}
	var expectedRoot string
	var cleanup func()
	if err := phase(workspaceclient.PhaseRender, func() (err error) {
		expectedRoot, cleanup, err = workspaceclient.RenderExpected(workspaceRoot)
		return err
	}); err != nil {
		return report, err
	}
	defer cleanup()
	report = workspaceclient.InspectRendered(workspaceRoot, expectedRoot, mode).WithFindings(snapshotFindings...)
	report.AppliedAdaptations = applied
	return report, nil
}

// sortedTimingNames returns the phase names in a fixed order, so the metric
// events of two runs over one workspace come out in the same sequence.
// memberPaths returns the workspace-relative directory of every member project
// the orchestrator resolved.
func memberPaths(refs []pctx.ProjectRef) []string {
	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		paths = append(paths, ref.Path)
	}
	return paths
}

func sortedTimingNames(timings map[string]int64) []string {
	names := make([]string, 0, len(timings))
	for name := range timings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func main() {
	// The extension project carries the framework API emitter as a tools-only
	// dependency. Its local module replacements make the Go extension's coarse
	// describe activation see go.putnami.dev/app even though this binary is a CLI,
	// not an application. Honor the probe explicitly so ordinary build and test
	// workflows do not dispatch a missing clientgen job.
	if os.Getenv("PUTNAMI_DESCRIBE") != "" {
		return
	}
	if handled, err := runtimeinfo.Handle(os.Args[1:], os.Stdout, extensionName, runtimeVersion); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	cli.RunSubcommand(commandHandlers(), cli.WithSkipEmptyProject(false))
}
