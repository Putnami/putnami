package lint

import (
	"fmt"
	"strings"

	"go.putnami.dev/typescript/extension/internal/parse"
)

// Check runs biome lint on the project.
func Check(biomeBin, projectPath, configPath string, fix bool, maxDiagnostics int, diagnosticLevel string) (parse.BiomeReport, bool, error) {
	return CheckProjects(biomeBin, []string{projectPath}, "", configPath, fix, maxDiagnostics, diagnosticLevel)
}

// testRuleSkips keeps the writing pass from running Biome's test rules. Their
// fixes remove `.skip` and `.only`, which silently changes what runs and
// happens before the skip guard reads the file. The skip guard judges those
// calls instead. The built-in preset turns noSkippedTests off, but a workspace
// biome.json written before that still turns it on, and Biome turns
// noFocusedTests on by itself in a workspace that depends on a test framework.
var testRuleSkips = []string{"--skip=suspicious/noSkippedTests", "--skip=suspicious/noFocusedTests"}

// CheckProjects runs one Biome lint invocation over all selected roots.
func CheckProjects(
	biomeBin string,
	projectPaths []string,
	workspaceRoot, configPath string,
	fix bool,
	maxDiagnostics int,
	diagnosticLevel string,
) (parse.BiomeReport, bool, error) {
	args := []string{"lint", "--reporter=json", "--config-path=" + configPath}
	if fix {
		args = append(args, "--write", "--unsafe")
		args = append(args, testRuleSkips...)
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
		return parse.EmptyBiomeReport(), false, fmt.Errorf("biome lint: %w", err)
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
		return report, false, fmt.Errorf("biome lint failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return report, result.Success, nil
}
