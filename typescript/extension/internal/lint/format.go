// Package lint implements the lint job (biome format + check).
package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// execRunFunc is the function used to run subprocesses. Defaults to exec.Run.
var execRunFunc = exec.Run

// SetExecRunForTesting replaces the exec.Run function used by lint functions.
func SetExecRunForTesting(fn func(string, []string, ...exec.Option) (*exec.Result, error)) func() {
	orig := execRunFunc
	execRunFunc = fn
	return func() { execRunFunc = orig }
}

// checkShimArgs refuses a biome command line that cmd.exe would reinterpret. A
// package var so tests can observe that every biome run passes through it.
var checkShimArgs = toolchain.CheckShimArgs

// runBiome runs biome with args, unless Windows would start biomeBin through
// cmd.exe and cmd.exe would reinterpret one of them (toolchain.CheckShimArgs).
func runBiome(biomeBin string, args []string, opts ...exec.Option) (*exec.Result, error) {
	if err := checkShimArgs(biomeBin, args); err != nil {
		return nil, fmt.Errorf("biome: %w", err)
	}
	return execRunFunc(biomeBin, args, opts...)
}

func biomeRunContext(projectPath, configPath string) (string, string) {
	configRoot := biomeConfigRoot(configPath)
	if configRoot != "" && configRoot != projectPath {
		rel, err := filepath.Rel(configRoot, projectPath)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return configRoot, filepath.ToSlash(rel)
		}
	}
	return projectPath, "."
}

func biomeBatchRunContext(workspaceRoot string, projectPaths []string, configPath string) (string, []string) {
	if len(projectPaths) == 1 {
		runDir, target := biomeRunContext(projectPaths[0], configPath)
		return runDir, []string{target}
	}
	runDir := filepath.Clean(workspaceRoot)
	targets := make([]string, 0, len(projectPaths))
	for _, projectPath := range projectPaths {
		rel, err := filepath.Rel(runDir, filepath.Clean(projectPath))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", nil
		}
		targets = append(targets, filepath.ToSlash(rel))
	}
	return runDir, targets
}

func biomeConfigRoot(configPath string) string {
	if configPath == "" {
		return ""
	}
	if info, err := os.Stat(configPath); err == nil {
		if info.IsDir() {
			return configPath
		}
		return filepath.Dir(configPath)
	}
	switch filepath.Ext(configPath) {
	case ".json", ".jsonc":
		return filepath.Dir(configPath)
	default:
		return configPath
	}
}

// biomeExecOptions binds the CLI's per-subprocess CPU budget to Rayon's thread
// pool, which is the parallelism control Biome honors. An explicitly inherited
// RAYON_NUM_THREADS is an operator choice and takes precedence, matching the
// CLI's GOMAXPROCS override convention for Go tools.
func biomeExecOptions(runDir string) []exec.Option {
	opts := []exec.Option{exec.Dir(runDir), exec.Timeout(2 * time.Minute)}
	if _, overridden := os.LookupEnv("RAYON_NUM_THREADS"); overridden {
		return opts
	}
	budget, err := strconv.Atoi(os.Getenv("PUTNAMI_CPU_BUDGET"))
	if err != nil || budget <= 0 {
		return opts
	}
	return append(opts, exec.Env(map[string]string{"RAYON_NUM_THREADS": strconv.Itoa(budget)}))
}

// Format runs biome format on the project.
func Format(biomeBin, projectPath, configPath string, fix bool) (parse.BiomeReport, bool, error) {
	return FormatProjects(biomeBin, []string{projectPath}, "", configPath, fix)
}

// FormatProjects runs one Biome formatter invocation over all selected roots.
func FormatProjects(biomeBin string, projectPaths []string, workspaceRoot, configPath string, fix bool) (parse.BiomeReport, bool, error) {
	args := []string{"format", "--config-path=" + configPath, "--reporter=json"}
	if fix {
		args = append(args, "--write")
	}
	if workspaceRoot != "" {
		// A batch must observe every diagnostic before splitting and applying
		// the ordinary per-project display cap. A global Biome cap could let a
		// noisy early project hide a later project's failure.
		args = append(args, "--max-diagnostics=none")
	}
	runDir, targets, err := biomeTargets(workspaceRoot, projectPaths, configPath)
	if err != nil {
		return parse.EmptyBiomeReport(), false, fmt.Errorf("biome format: %w", err)
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
		return report, false, fmt.Errorf("biome format failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return report, result.Success, nil
}
