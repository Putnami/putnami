// Command putnami-clientgen is the extension runtime for client generation and
// the workspace guard over generated clients and handwritten transports.
package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"

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
		"generate-go":     generateClient(pctx.GeneratedLanguageGo),
		"generate-ts":     generateClient(pctx.GeneratedLanguageTypeScript),
		"generate-module": generateModuleTarget,
		"workspace-check": runCheck,
	}
}

// generateClient is the client-generate task of a project that names its
// provider in generatedFrom. The client generates itself: it reads its
// provider's committed contract, whose path the core resolved into the job
// context, and writes only into its own directory.
func generateClient(language string) cli.JobFunc {
	return func(ctx *pctx.Context, _ *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		from := ctx.Project.GeneratedFrom
		if from == nil {
			return "FAILED", nil, fmt.Errorf("%s has no resolved generatedFrom in its job context; a generated client "+
				"names its provider's service in the generatedFrom member of its putnami.json", projectLabel(ctx))
		}
		if from.Language != language {
			return "FAILED", nil, fmt.Errorf("%s declares a %q client in generatedFrom, but this task generates a %q client",
				projectLabel(ctx), from.Language, language)
		}
		if from.ContractPath == "" {
			return "FAILED", nil, fmt.Errorf("%s: no project in the workspace commits a schema/openapi.json that declares "+
				"service %q, or several do; commit the provider's contract or fix the service name in generatedFrom",
				projectLabel(ctx), from.Service)
		}
		projectRoot := projectRootOf(ctx)
		contract := filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(from.ContractPath))
		if language == pctx.GeneratedLanguageGo {
			err := workspaceclient.GenerateGo(ctx.WorkspaceRoot, workspaceclient.GoGeneration{
				Contract:       contract,
				Output:         projectRoot,
				PackageName:    from.PackageName,
				ClientName:     from.ClientName,
				OmitOperations: from.OmitOperations,
			})
			if err != nil {
				return "FAILED", nil, err
			}
			return "OK", map[string]any{}, nil
		}
		packageName, err := workspaceclient.PackageName(projectRoot)
		if err != nil {
			return "FAILED", nil, fmt.Errorf("%s: %w", projectLabel(ctx), err)
		}
		err = workspaceclient.GenerateTypeScript(ctx.WorkspaceRoot, workspaceclient.TypeScriptGeneration{
			Contract:    contract,
			Project:     projectRoot,
			PackageName: packageName,
		})
		if err != nil {
			return "FAILED", nil, err
		}
		return "OK", map[string]any{}, nil
	}
}

// generateModuleTarget is the client-generate task of a provider whose Go
// client is a package of its own module. The provider commits that target in
// clientgen.json and generates it from its own committed contract, inside its
// own tree. It reports the file it owns through the moduleClientOutput port.
func generateModuleTarget(ctx *pctx.Context, _ *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	projectRoot := projectRootOf(ctx)
	target, err := workspaceclient.ReadModuleTarget(projectRoot)
	if err != nil {
		return "FAILED", nil, fmt.Errorf("%s: %w", projectLabel(ctx), err)
	}
	contract := filepath.Join(projectRoot, "schema", "openapi.json")
	if _, err := os.Stat(contract); err != nil {
		return "FAILED", nil, fmt.Errorf("%s commits clientgen.json but no schema/openapi.json to generate its client from: %w",
			projectLabel(ctx), err)
	}
	output := filepath.Join(projectRoot, filepath.FromSlash(target.Output))
	if err := os.MkdirAll(output, 0o750); err != nil {
		return "FAILED", nil, err
	}
	err = workspaceclient.GenerateGo(ctx.WorkspaceRoot, workspaceclient.GoGeneration{
		Contract:       contract,
		Output:         output,
		PackageName:    target.PackageName,
		ClientName:     target.ClientName,
		OmitOperations: target.OmitOperations,
	})
	if err != nil {
		return "FAILED", nil, err
	}
	return "OK", map[string]any{"moduleClientOutput": path.Join(target.Output, "client.gen.go")}, nil
}

// runCheck is the guard: clientgen-check, and the validate contribution. It
// builds nothing and renders nothing. It reads the Git candidate cut its
// `git:**` key holds and the membership the orchestrator resolved from
// the committed workspace and scope manifests, so a cold clone and a tree the session just built
// reach one verdict.
func runCheck(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	if ctx.WorkspaceRoot == "" {
		return "FAILED", nil, errors.New("workspace client command received no workspace root")
	}
	report := workspaceclient.InspectCommitted(ctx.WorkspaceRoot, memberPaths(ctx.WorkspaceProjects))
	// Each phase is timed and reported (Report.Timings, one metric event per
	// phase). The session record exposes this task as one durationMs, and the
	// split behind that number is what decides where a slow guard spends its
	// time.
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
		fmt.Sprintf("%d generated-client finding(s), %d censused pending transport(s)",
			len(report.Findings), len(report.PendingTransports)),
		map[string]any{
			"findings":          len(report.Findings),
			"pendingTransports": len(report.PendingTransports),
			"providers":         len(report.Providers),
			"timings":           report.Timings,
		})
	if !report.Clean() {
		return "FAILED", nil, fmt.Errorf("first-party generated client verification found %d issue(s)", len(report.Findings))
	}
	return "OK", map[string]any{"report": report}, nil
}

// projectRootOf returns the absolute directory of the job's project.
func projectRootOf(ctx *pctx.Context) string {
	if ctx.Project.FullPath != "" {
		return ctx.Project.FullPath
	}
	return filepath.Join(ctx.WorkspaceRoot, filepath.FromSlash(ctx.Project.Path))
}

// projectLabel names the job's project in a message.
func projectLabel(ctx *pctx.Context) string {
	if ctx.Project.Name != "" {
		return ctx.Project.Name
	}
	return ctx.Project.Path
}

// memberPaths returns the workspace-relative directory of every member project
// the orchestrator resolved.
func memberPaths(refs []pctx.ProjectRef) []string {
	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		paths = append(paths, ref.Path)
	}
	return paths
}

// sortedTimingNames returns the phase names in a fixed order, so the metric
// events of two runs over one workspace come out in the same sequence.
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
