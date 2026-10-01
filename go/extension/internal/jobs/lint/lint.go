// Package lint runs golangci-lint and staticcheck on Go projects.
// Emits JSONL events compatible with the Putnami orchestrator.
package lint

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/go/extension/internal/parse"
	"go.putnami.dev/go/extension/internal/skipguard"
	"go.putnami.dev/go/extension/internal/toolchain"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Run executes the lint job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	options := parseLintOptions(ctx.Params, args)
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "FAILED", nil, err
	}

	// Batch path: the scheduler groups compatible lint jobs and runs one leader
	// with the full miss set in ctx.SelectedProjects. Each batchable manifest task
	// pins its own tool in argv (`--tool golangci-lint` / `--tool staticcheck`),
	// so the selection is dispatched by the tool it was grouped under. `all` is
	// never batchable — no manifest task requests it — and keeps the singleton
	// path below, which is also the path a solo miss and a solo retry take.
	if len(ctx.SelectedProjects) > 0 {
		switch options.tool {
		case "golangci-lint":
			return runGolangciBatch(ctx, options)
		case "staticcheck":
			return runStaticcheckBatch(ctx, options)
		}
	}

	fix := options.fix
	tool := options.tool

	// Resolve extension root from the binary's location.
	// go run compiles from the extension source, so we derive it from the module.
	extensionRoot := resolveExtensionRoot()

	hasFailure := false
	totalErrors := 0
	totalFiles := 0

	// --- golangci-lint ---
	if tool == "golangci-lint" || tool == "all" {
		binary, resolveErr := resolveLintToolBinary("golangci-lint", goBinary, ctx.WorkspaceRoot, emit)

		golangciConfig := toolchain.ResolveGolangciConfig(
			ctx.Project.FullPath, ctx.WorkspaceRoot, extensionRoot, options.config,
		)

		// --- gofmt / goimports formatting gate ---
		// `golangci-lint run --fix` silently *applies* the enabled formatters
		// (gofmt/goimports/gci) and drops them from its report, so unformatted
		// code passes the gate green while the in-tree rewrite is discarded on
		// revert. Check formatting explicitly with `fmt --diff` (never writes)
		// BEFORE the run pass mutates the tree. Without --fix, `run` reports
		// formatting drift itself, so the extra pass is only needed when fixing.
		if resolveErr == nil && fix {
			emit.PhaseStart("gofmt")
			fmtErrors, fmtFiles := runFormatCheck(
				binary, goBinary, golangciConfig, ctx.WorkspaceRoot, ctx.Project.FullPath, emit,
			)
			if fmtErrors > 0 {
				hasFailure = true
				totalErrors += fmtErrors
				totalFiles += fmtFiles
				emit.PhaseEnd("gofmt", "failed")
			} else {
				emit.PhaseEnd("gofmt", "success")
			}
		}

		emit.PhaseStart("golangci-lint")

		if resolveErr == nil {
			lintArgs := golangciRunArgs(options, golangciConfig)

			// Run with retry for ETXTBSY
			const maxRetries = 3
			var output []byte
			var exitErr error

			for attempt := 0; attempt <= maxRetries; attempt++ {
				cmd := exec.Command(binary, lintArgs...)
				cmd.Dir = ctx.Project.FullPath
				cmd.Env = toolchain.WorkspaceBuildEnv(
					os.Environ(),
					ctx.Project.FullPath,
					goBinary,
				)
				output, exitErr = cmd.CombinedOutput()

				if exitErr == nil {
					break
				}

				if strings.Contains(string(output), "ETXTBSY") && attempt < maxRetries {
					delay := time.Duration(attempt+1) * time.Second
					emit.Log("warn", "golangci-lint ETXTBSY, retrying...")
					time.Sleep(delay)
					continue
				}
				break
			}

			if exitErr == nil {
				emit.PhaseEnd("golangci-lint", "success")
			} else {
				outStr := string(output)
				hasFailure = true
				if isToolchainCompatibilityError(outStr) {
					// The linter's Go toolchain is older than the workspace (go.work)
					// requires, so it aborts type-checking with an opaque "package
					// requires newer Go version" and zero findings — surfacing as a
					// cryptic "1 error in 0 files". Fail with a clear, actionable
					// message instead of that panic-shaped noise.
					emit.Diagnostic("error",
						"golangci-lint could not run: its Go toolchain is older than the workspace (go.work) requires. Upgrade the local Go toolchain so the linter matches, then re-run lint.",
						"", 0)
					totalErrors++
				} else {
					result := parse.LintErrors(outStr, ctx.WorkspaceRoot, ctx.Project.FullPath, emit)
					totalErrors += result.Errors
					totalFiles += result.Files
				}
				emit.PhaseEnd("golangci-lint", "failed")
			}
		} else {
			hasFailure = true
			totalErrors++
			emit.Diagnostic("error", resolveErr.Error()+"; install the pinned tools with `putnami deps install --tag go` before running lint", "", 0)
			emit.PhaseEnd("golangci-lint", "failed")
		}

		if options.skipGuard {
			failures, files := runSkipGuard(ctx.WorkspaceRoot, ctx.Project.FullPath, emit)
			if failures > 0 {
				hasFailure = true
				totalErrors += failures
				totalFiles += files
			}
		}
	}

	// --- staticcheck ---
	if tool == "staticcheck" || tool == "all" {
		emit.PhaseStart("staticcheck")

		binary, resolveErr := resolveLintToolBinary("staticcheck", goBinary, ctx.WorkspaceRoot, emit)

		if resolveErr == nil {
			cmd := exec.Command(binary, "./...")
			cmd.Dir = ctx.Project.FullPath
			cmd.Env = toolchain.WorkspaceBuildEnv(
				os.Environ(),
				ctx.Project.FullPath,
				goBinary,
			)
			output, err := cmd.CombinedOutput()

			if err == nil {
				emit.PhaseEnd("staticcheck", "success")
			} else {
				outStr := string(output)
				// Check for Go version compatibility issues
				if isStaticcheckToolchainCompatibilityError(outStr) {
					emit.Log("warn", "staticcheck: skipping due to Go version compatibility issues")
					emit.PhaseEnd("staticcheck", "skipped")
				} else {
					hasFailure = true
					result := parse.LintErrors(outStr, ctx.WorkspaceRoot, ctx.Project.FullPath, emit)
					totalErrors += result.Errors
					totalFiles += result.Files
					emit.PhaseEnd("staticcheck", "failed")
				}
			}
		} else {
			hasFailure = true
			totalErrors++
			emit.Diagnostic("error", resolveErr.Error()+"; install the pinned tools with `putnami deps install --tag go` before running lint", "", 0)
			emit.PhaseEnd("staticcheck", "failed")
		}
	}

	if totalErrors > 0 {
		es := ""
		if totalErrors != 1 {
			es = "s"
		}
		fs := ""
		if totalFiles != 1 {
			fs = "s"
		}
		emit.Summary(fmt.Sprintf("%d error%s in %d file%s", totalErrors, es, totalFiles, fs))
	}

	// Canonical result payload (protocol/runtime: lintSummary).
	resultData := map[string]any{
		"lintSummary": map[string]any{"errors": totalErrors},
	}
	if hasFailure {
		return "FAILED", resultData, nil
	}
	return "OK", resultData, nil
}

// golangciRunArgs uses absolute diagnostic paths so their base is explicit.
// The singleton run executes from a project directory while batch runs execute
// from a group root; --path-mode abs keeps both paths unambiguous.
func golangciRunArgs(options lintOptions, config string) []string {
	args := []string{"run", "--allow-parallel-runners", "--path-mode", "abs"}
	if options.fix {
		args = append(args, "--fix")
	}
	if options.new {
		args = append(args, "--new")
	}
	if config != "" {
		args = append(args, "--config", config)
	}
	if options.timeout != "" {
		if isNumeric(options.timeout) {
			args = append(args, "--timeout", options.timeout+"ms")
		} else {
			args = append(args, "--timeout", options.timeout)
		}
	}
	return args
}

type lintOptions struct {
	fix       bool
	config    string
	new       bool
	tool      string
	timeout   string
	skipGuard bool
}

// parseLintOptions gives explicit task argv precedence and falls back to the
// orchestrator's command parameters. Most lint flags are carried in the job
// context rather than repeated in the manifest argv. When neither names a
// timeout, derive one from the scheduler deadline the CLI handed this task.
// Both golangci paths read the same lintOptions, so batching inherits it too.
func parseLintOptions(params pctx.Params, args []string) lintOptions {
	flags := cli.ParseFlags(args)
	timeout := cli.FlagString(flags, "timeout", params.String("timeout"))
	if timeout == "" {
		timeout = derivedGolangciTimeoutMs(os.Getenv(extensionproto.TaskDeadlineMsEnv))
	}
	return lintOptions{
		fix:     cli.FlagBool(flags, "fix", params.Bool("fix", true)),
		config:  cli.FlagString(flags, "config", params.String("config")),
		new:     cli.FlagBool(flags, "new", params.Bool("new", false)),
		tool:    cli.FlagString(flags, "tool", "all"),
		timeout: timeout,
		skipGuard: cli.FlagBool(flags, skipGuardParam,
			params.Bool(skipGuardParam, true, "skipGuard")),
	}
}

// skipGuardParam turns the skip guard off when false. It rides with the
// golangci-lint tasks, so `--tool staticcheck` alone does not run it.
const skipGuardParam = "skip-guard"

// skipGuardDiagnostics reports the test skips that hide a failing test under
// root, with workspace-relative paths. A reviewed exception is a warning.
func skipGuardDiagnostics(workspaceRoot, root string) []parse.ToolDiagnostic {
	findings, err := skipguard.ScanProject(root)
	if err != nil {
		return []parse.ToolDiagnostic{{
			Category:    skipguard.Category,
			Severity:    "error",
			Description: "skip guard could not read the project: " + err.Error(),
		}}
	}
	diagnostics := make([]parse.ToolDiagnostic, 0, len(findings))
	for _, finding := range findings {
		diagnostics = append(diagnostics, parse.ToolDiagnostic{
			Category:    skipguard.Category,
			Severity:    finding.Severity,
			Description: finding.Message + " (" + skipguard.Category + ")",
			File:        formatDiagPath(workspaceRoot, root, finding.File),
			Line:        finding.Line,
			Column:      finding.Column,
		})
	}
	return diagnostics
}

// runSkipGuard emits the skip guard's diagnostics and returns the error count
// and the number of files that hold an error.
func runSkipGuard(workspaceRoot, projectPath string, emit *jsonl.Emitter) (int, int) {
	emit.PhaseStart(skipguard.Category)
	failures := 0
	files := make(map[string]struct{})
	for _, diagnostic := range skipGuardDiagnostics(workspaceRoot, projectPath) {
		emit.DiagnosticWithCode(diagnostic.Severity, diagnostic.Description,
			diagnostic.File, diagnostic.Line, diagnostic.Column, diagnostic.Category)
		if diagnostic.Severity == "error" {
			failures++
			files[diagnostic.File] = struct{}{}
		}
	}
	if failures > 0 {
		// The renderer sums repeated metrics, so this adds to golangci-lint's.
		emit.Metric("lint-errors", failures, "count")
		emit.PhaseEnd(skipguard.Category, "failed")
	} else {
		emit.PhaseEnd(skipguard.Category, "success")
	}
	return failures, len(files)
}

// golangciDeadlineMarginCapMs limits the reservation between the scheduler's
// deadline and golangci-lint's own clock. Runtime startup, binary resolution,
// the optional fmt gate, parsing, and reporting are roughly fixed overhead, so
// a growing percentage would waste useful time in a large batch.
const golangciDeadlineMarginCapMs int64 = 120_000

// derivedGolangciTimeoutMs converts the positive scheduler deadline exported by
// the CLI into the --timeout passed to golangci-lint. Empty means there was no
// usable scheduler deadline, so the tool's normal config fallback remains in
// charge. Explicit user --timeout values always win before this is called.
//
// The derived value is deadline - min(120s, deadline/5), preserving a margin
// for the work that happens before and after golangci-lint. Since the CLI
// exports a batch leader's already N×-scaled deadline, the derived tool timeout
// grows with the batch too. A 1ms deadline has no strictly smaller whole-ms
// representation; it still returns 1ms rather than falling back to the static
// 8m config, which would be materially less truthful.
func derivedGolangciTimeoutMs(deadline string) string {
	deadlineMs, err := strconv.ParseInt(strings.TrimSpace(deadline), 10, 64)
	if err != nil || deadlineMs <= 0 {
		return ""
	}
	margin := deadlineMs / 5
	if margin > golangciDeadlineMarginCapMs {
		margin = golangciDeadlineMarginCapMs
	}
	if margin < 1 {
		margin = 1
	}
	derivedMs := deadlineMs - margin
	if derivedMs < 1 {
		derivedMs = 1
	}
	return strconv.FormatInt(derivedMs, 10)
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// formatDiffFileRe matches the unified-diff header emitted by
// `golangci-lint fmt --diff` for each unformatted file: `diff <file>.orig <file>`.
var formatDiffFileRe = regexp.MustCompile(`^diff\s+\S+\s+(\S+)$`)

// parseUnformattedFiles extracts the sorted, unique set of file paths reported
// by `golangci-lint fmt --diff`. Paths are relative to the run directory.
func parseUnformattedFiles(output string) []string {
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		if m := formatDiffFileRe.FindStringSubmatch(scanner.Text()); m != nil {
			seen[m[1]] = struct{}{}
		}
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// runFormatCheck reports formatting drift (gofmt/goimports/gci per the resolved
// config) using `golangci-lint fmt --diff`, which never rewrites files. It emits
// one diagnostic per unformatted file and returns (errorCount, fileCount).
func runFormatCheck(
	binary, goBinary, config, workspaceRoot, projectPath string,
	emit *jsonl.Emitter,
) (int, int) {
	args := []string{"fmt", "--diff"}
	if config != "" {
		args = append(args, "--config", config)
	}
	args = append(args, "./...")

	cmd := exec.Command(binary, args...)
	cmd.Dir = projectPath
	cmd.Env = toolchain.WorkspaceBuildEnv(os.Environ(), projectPath, goBinary)
	output, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(output))

	files := parseUnformattedFiles(out)
	switch {
	case len(files) > 0:
		for _, file := range files {
			emit.DiagnosticWithCode("error",
				"File is not properly formatted; run `putnami lint --fix` or `gofmt -w` (gofmt)",
				formatDiagPath(workspaceRoot, projectPath, file), 0, 0, "")
		}
		return len(files), len(files)
	case err != nil && out != "":
		// Non-zero exit without a parseable diff — golangci-lint itself errored.
		// Surface it rather than swallowing the failure.
		emit.Diagnostic("error", out, "", 0)
		return 1, 1
	default:
		return 0, 0
	}
}

// formatDiagPath renders a run-directory-relative file as a workspace-relative
// diagnostic path, mirroring the go extension's other diagnostics.
func formatDiagPath(workspaceRoot, projectPath, file string) string {
	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(projectPath, file)
	}
	if rel, err := filepath.Rel(workspaceRoot, abs); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Clean(abs))
}

func resolveLintToolBinary(
	tool, goBinary, workspaceRoot string,
	emit *jsonl.Emitter,
) (string, error) {
	binary, err := toolchain.ResolvePinnedToolBinary(tool, workspaceRoot)
	if err == nil {
		return binary, nil
	}

	emit.Log("info", fmt.Sprintf("%v; installing the pinned %s", err, tool))
	binary, installErr := toolchain.InstallTool(tool, goBinary, workspaceRoot)
	if installErr != nil {
		return "", fmt.Errorf("%w; unable to install pinned %s: %w", err, tool, installErr)
	}
	return binary, nil
}

func isStaticcheckToolchainCompatibilityError(out string) bool {
	return (strings.Contains(out, "unsupported version") && strings.Contains(out, "internal error")) ||
		(strings.Contains(out, "file requires newer Go version") && strings.Contains(out, "application built with go"))
}

// isToolchainCompatibilityError reports whether tool output is the Go
// toolchain-mismatch abort — "package requires newer Go version go1.NN
// (application built with go1.MM)" — rather than real findings. It means the
// linter binary was built against an older Go than the workspace sources
// declare, so it cannot type-check them and bails with zero findings.
func isToolchainCompatibilityError(out string) bool {
	return strings.Contains(out, "requires newer Go version") &&
		(strings.Contains(out, "application built with go") ||
			strings.Contains(out, "package requires") ||
			strings.Contains(out, "file requires"))
}

// resolveExtensionRoot derives the extension root directory.
// The runtime invocation sets PUTNAMI_EXTENSION_ROOT; this is the primary mechanism.
func resolveExtensionRoot() string {
	if v := os.Getenv("PUTNAMI_EXTENSION_ROOT"); v != "" {
		return v
	}
	// Fallback: try to find it relative to the executable
	ex, err := os.Executable()
	if err != nil {
		return ""
	}
	// go run puts the binary in a temp dir, so this won't work.
	// The runtime invocation sets PUTNAMI_EXTENSION_ROOT.
	dir := filepath.Dir(ex)
	return dir
}
