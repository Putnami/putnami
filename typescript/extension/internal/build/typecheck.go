package build

import (
	"os"
	"path/filepath"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/project"
)

// typeCheckPatterns are the project files the test type-check reads.
var typeCheckPatterns = []string{"**/*.ts", "**/*.tsx", "**/*.mts", "**/*.cts"}

// typeCheckSkippedDirectories are the directories the type-check leaves out:
// the ones the input hasher skips (tooling/cli/internal/store/cache_hash.go),
// so no file the check reads lies outside the task key.
var typeCheckSkippedDirectories = []string{"node_modules", ".git", ".putnami", "out", "dist", "vendor"}

// TypeCheckResult holds the outcome of a test type-check.
type TypeCheckResult struct {
	Diagnostics []parse.TscDiagnostic
	Success     bool
	RawOutput   string // combined stdout+stderr from tsc
}

// RunTypeCheck type-checks every TypeScript file of the project, tests,
// fixtures and benchmarks included, and emits nothing. It uses the tsconfig the
// types phase resolves and the same compiler options, so a test is judged the
// way a source file is. A nested project, a subdirectory with its own
// putnami.json, is left to its own check.
//
// buildInfoDir, when non-empty, is a persistent scratch directory that holds
// the scratch tsconfig and tsc's incremental build info, so a later run
// re-checks only what changed.
func RunTypeCheck(bunBin, projectPath, buildInfoDir string) (*TypeCheckResult, error) {
	nested, found, err := typeCheckScope(projectPath)
	if err != nil {
		return nil, err
	}
	if !found {
		return &TypeCheckResult{Success: true}, nil
	}

	exclude := make([]string, 0, 2*len(typeCheckSkippedDirectories)+len(nested))
	for _, dir := range typeCheckSkippedDirectories {
		exclude = append(exclude, dir, "**/"+dir)
	}
	exclude = append(exclude, nested...)
	tsconfig := project.ResolveTsConfig(projectPath, filepath.Dir(projectPath))
	scratch, err := writeScratchTsConfig(buildInfoDir, "tsconfig.typecheck.*.json", projectPath, tsconfig, typeCheckPatterns, exclude)
	if err != nil {
		return nil, err
	}
	defer os.Remove(scratch)

	// rootDir only places emitted files, and the check emits none. An inherited
	// one, such as the extension default's, can lie beside the project rather
	// than above it and fail every file with TS6059, so it widens to the
	// filesystem root.
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		return nil, err
	}
	fsRoot := filepath.VolumeName(absProject) + string(filepath.Separator)
	args := append([]string{"x", "tsc", "--noEmit", "--rootDir", fsRoot}, tscCompilerFlags(typeScriptIgnoreDeprecations(projectPath))...)
	if buildInfoDir != "" {
		args = append(args, "--incremental", "--tsBuildInfoFile", filepath.Join(buildInfoDir, "typecheck.tsbuildinfo"))
	}
	args = append(args, "--project", scratch)

	result, err := execRunFunc(bunBin, args, exec.Dir(projectPath), exec.Timeout(5*time.Minute))
	if err != nil {
		return nil, err
	}
	rawOutput := result.Stdout + result.Stderr
	return &TypeCheckResult{
		Diagnostics: parse.ParseTscOutput(rawOutput),
		Success:     result.Success,
		RawOutput:   rawOutput,
	}, nil
}

// typeCheckScope walks the project and returns the project-relative paths of
// its nested projects, and whether any TypeScript file lies outside them and
// outside the skipped directories.
func typeCheckScope(projectPath string) ([]string, bool, error) {
	skipped := make(map[string]bool, len(typeCheckSkippedDirectories))
	for _, dir := range typeCheckSkippedDirectories {
		skipped[dir] = true
	}
	var nested []string
	found := false
	err := filepath.WalkDir(projectPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if path == projectPath {
				return err
			}
			return nil
		}
		if d.IsDir() {
			if path == projectPath {
				return nil
			}
			if skipped[d.Name()] {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "putnami.json")); err == nil {
				rel, err := filepath.Rel(projectPath, path)
				if err != nil {
					return err
				}
				nested = append(nested, filepath.ToSlash(rel))
				return filepath.SkipDir
			}
			return nil
		}
		if !found && isTypeCheckFile(d.Name()) {
			found = true
		}
		return nil
	})
	return nested, found, err
}

// isTypeCheckFile reports whether name is a TypeScript file tsc checks: any
// of typeCheckPatterns except a declaration file, which only feeds the check.
func isTypeCheckFile(name string) bool {
	switch filepath.Ext(name) {
	case ".ts", ".tsx", ".mts", ".cts":
	default:
		return false
	}
	for _, suffix := range []string{".d.ts", ".d.mts", ".d.cts"} {
		if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
			return false
		}
	}
	return true
}
