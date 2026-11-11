package lint

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// eslintCommand returns the program and the arguments that start eslint
// (toolchain.ESLintCommand). A package var so tests observe what each ESLint
// run starts.
var eslintCommand = toolchain.ESLintCommand

// eslintTimeout bounds one ESLint run below the lint tasks' own timeout, so a
// hung run reports here rather than as a killed task.
const eslintTimeout = 4 * time.Minute

// generatedDirectories are the directories Putnami writes in a project, which
// ESLint never lints: git ignores them, and the read-only lint task leaves
// .gen out of its cache key.
var generatedDirectories = []string{".gen", ".putnami"}

// ESLint runs eslintBin on node over the files of the project at projectPath,
// from that directory, so ESLint resolves the flat or legacy configuration
// that applies there. A nested project, a subdirectory that holds its own
// putnami.json, is passed as an ignore pattern and never linted. With fix,
// ESLint writes its fixes and reports what remains; without it, it only
// reports. A diagnostic level of error reports errors alone (--quiet).
//
// Diagnostic paths are relative to workspaceRoot when it is set, else to
// projectPath. The boolean is false when ESLint reports an error.
func ESLint(eslintBin, node, projectPath, workspaceRoot string, fix bool, diagnosticLevel string) (parse.BiomeReport, bool, error) {
	args := []string{"--format", "json", "--no-error-on-unmatched-pattern"}
	if fix {
		args = append(args, "--fix")
	}
	if diagnosticLevel == "error" {
		args = append(args, "--quiet")
	}
	nested, err := nestedProjects(projectPath)
	if err != nil {
		return parse.EmptyBiomeReport(), false, fmt.Errorf("eslint: %w", err)
	}
	for _, dir := range append(append([]string(nil), generatedDirectories...), nested...) {
		args = append(args, "--ignore-pattern", dir+"/**")
	}
	args = append(args, ".")

	program, programArgs, err := eslintCommand(eslintBin, node, args)
	if err != nil {
		return parse.EmptyBiomeReport(), false, fmt.Errorf("eslint: %w", err)
	}
	if err := checkShimArgs(program, programArgs); err != nil {
		return parse.EmptyBiomeReport(), false, fmt.Errorf("eslint: %w", err)
	}
	result, err := execRunFunc(program, programArgs, exec.Dir(projectPath), exec.Timeout(eslintTimeout))
	if err != nil {
		return parse.EmptyBiomeReport(), false, fmt.Errorf("eslint: %w", err)
	}

	base := projectPath
	if workspaceRoot != "" {
		base = workspaceRoot
	}
	report, parseErr := parse.ParseESLintOutput(result.Stdout, base)
	if parseErr != nil {
		if detail := strings.TrimSpace(result.Stderr); detail != "" {
			return report, false, fmt.Errorf("eslint failed (exit %d): %s", result.ExitCode, detail)
		}
		return report, false, fmt.Errorf("eslint failed (exit %d): %w", result.ExitCode, parseErr)
	}
	return report, result.Success && report.Summary.Errors == 0, nil
}

// nestedProjects returns the nested project roots under projectPath, in slash
// form relative to it: each subdirectory that holds its own putnami.json,
// without looking below it. Like biomeTargets, it never follows a symbolic
// link and skips the directories the input hasher skips.
func nestedProjects(projectPath string) ([]string, error) {
	if _, err := os.ReadDir(projectPath); err != nil {
		return nil, err
	}
	var nested []string
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		if rel != "" && holdsProjectManifest(entries) {
			nested = append(nested, rel)
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || unscannedDirectories[name] {
				continue
			}
			walk(filepath.Join(dir, name), path.Join(rel, name))
		}
	}
	walk(projectPath, "")
	return nested, nil
}
