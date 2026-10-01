package lint

import (
	"fmt"
	"strings"

	"go.putnami.dev/typescript/extension/internal/parse"
)

// CheckAll runs Biome's combined `check` command in read-only mode, which
// executes the formatter and the linter in a single file scan. It powers the
// no-fix lint path and replaces the previous two-pass `biome format` +
// `biome lint` invocations, which scanned and parsed the project twice.
//
// Assist is explicitly enabled so singleton and batched invocations report the
// same info-level findings (for example, import organization). Enforcement is
// disabled (`--enforce-assist=false`) so the combined pass stays iso-functional
// with the prior format + lint phases, which never ran assist actions. With the
// default `warn` diagnostic level, assist findings are filtered out, so the
// pass/fail outcome and emitted diagnostics match the previous pipeline.
//
// CheckAll never writes: the fix (writer) path keeps using the distinct
// `biome format` (Format) and `biome lint` (Check) writer phases so auto-fix
// behavior — which deliberately excludes assist actions such as import
// reordering — is preserved exactly.
func CheckAll(biomeBin, projectPath, configPath string, maxDiagnostics int, diagnosticLevel string) (parse.BiomeReport, bool, error) {
	return CheckAllProjects(biomeBin, []string{projectPath}, "", configPath, maxDiagnostics, diagnosticLevel)
}

// CheckAllProjects runs one combined Biome check over all selected roots.
func CheckAllProjects(
	biomeBin string,
	projectPaths []string,
	workspaceRoot, configPath string,
	maxDiagnostics int,
	diagnosticLevel string,
) (parse.BiomeReport, bool, error) {
	args := []string{
		"check",
		"--reporter=json",
		"--config-path=" + configPath,
		"--assist-enabled=true",
		"--enforce-assist=false",
	}
	if maxDiagnostics < 0 {
		args = append(args, "--max-diagnostics=none")
	} else if maxDiagnostics > 0 {
		args = append(args, fmt.Sprintf("--max-diagnostics=%d", maxDiagnostics))
	}
	if diagnosticLevel != "" {
		args = append(args, "--diagnostic-level="+diagnosticLevel)
	}
	runDir, targets, err := biomeTargets(workspaceRoot, projectPaths, configPath)
	if err != nil {
		return parse.EmptyBiomeReport(), false, fmt.Errorf("biome check: %w", err)
	}
	if len(targets) == 0 {
		return parse.EmptyBiomeReport(), true, nil
	}
	args = append(args, targets...)

	result, err := runBiome(biomeBin, args, biomeExecOptions(runDir)...)
	if err != nil {
		return parse.EmptyBiomeReport(), false, err
	}

	report := parse.ParseBiomeOutput(result.Stdout)
	if !result.Success && len(report.Diagnostics) == 0 && strings.TrimSpace(result.Stderr) != "" {
		return report, false, fmt.Errorf("biome check failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return report, result.Success, nil
}
