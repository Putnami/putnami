package build

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/project"
)

// typeCheckPatterns are the project files the test type-check reads. tsc's
// wildcards never match a name that starts with a dot, so typeCheckScope adds
// the dot-directories that hold one, such as .gen. A dot-named file stays
// out, as in build~types: the generator hides the entries it writes for
// another target, such as the browser client.
var typeCheckPatterns = []string{"**/*.ts", "**/*.tsx", "**/*.mts", "**/*.cts"}

// typeCheckSkippedDirectories are the directories the type-check leaves out:
// the ones the input hasher skips (tooling/cli/internal/store/cache_hash.go),
// so no file the check reads lies outside the task key, and the package
// directories tsc itself never globs.
var typeCheckSkippedDirectories = []string{"node_modules", ".git", ".putnami", "out", "dist", "vendor", "bower_components", "jspm_packages"}

// The type-checker parameter's values.
const (
	TypeCheckerTsc = "tsc"
	TypeCheckerBun = "bun"
)

// ResolveTypeChecker returns the type-checker the parameter names: tsc when it
// is unset, else tsc or bun as given. The choice is explicit: nothing in the
// project's sources or scripts changes it.
func ResolveTypeChecker(param string) (string, error) {
	switch param {
	case "":
		return TypeCheckerTsc, nil
	case TypeCheckerTsc, TypeCheckerBun:
		return param, nil
	}
	return "", fmt.Errorf("type-checker %q is neither tsc nor bun", param)
}

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
//
// projectTypes checks with the project's own compiler options: tsc reads its
// tsconfig, strictness, module system and types included, so the globals of a
// Vitest or Jest suite resolve and the check judges the code as the project's
// own tsc does. Without it the check applies the types phase's Bun defaults.
//
// checker is the resolved type-checker: tsc runs `bun x tsc`, bun runs
// `bun check` on the same scratch tsconfig with the same compiler options.
// bun check keeps no build info, so buildInfoDir only holds its scratch
// tsconfig.
func RunTypeCheck(bunBin, projectPath, buildInfoDir string, projectTypes bool, checker string) (*TypeCheckResult, error) {
	scope, err := typeCheckScope(projectPath)
	if err != nil {
		return nil, err
	}
	if !scope.found {
		return &TypeCheckResult{Success: true}, nil
	}
	nested := scope.nested

	exclude := make([]string, 0, 2*len(typeCheckSkippedDirectories)+len(nested))
	for _, dir := range typeCheckSkippedDirectories {
		exclude = append(exclude, dir, "**/"+dir)
	}
	exclude = append(exclude, nested...)
	tsconfig := project.ResolveTsConfig(projectPath, filepath.Dir(projectPath))
	include := slices.Concat(typeCheckPatterns, scope.dotIncludes)
	scratch, err := writeScratchTsConfig(buildInfoDir, "tsconfig.typecheck.*.json", projectPath, tsconfig, include, exclude)
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
	ignoreDeprecations := typeScriptIgnoreDeprecations(projectPath)
	flags := tscCompilerFlags(ignoreDeprecations)
	if projectTypes {
		flags = projectCompilerFlags(ignoreDeprecations)
	}
	var args []string
	if checker == TypeCheckerBun {
		if err := requireBunCheck(bunBin, projectPath); err != nil {
			return nil, err
		}
		args = bunCheckArgs(fsRoot, flags, scratch)
	} else {
		args = append([]string{"x", "tsc", "--noEmit", "--rootDir", fsRoot}, flags...)
		if buildInfoDir != "" {
			args = append(args, "--incremental", "--tsBuildInfoFile", filepath.Join(buildInfoDir, "typecheck.tsbuildinfo"))
		}
		args = append(args, "--project", scratch)
	}

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

// bunCheckBun is the earliest Bun release known to ship bun check with
// --no-pretty, --all and --no-script. An earlier Bun reads `bun check` as
// `bun run check`, which would run the project's check script or file instead
// of a type-check.
var bunCheckBun = [3]int{1, 4, 4}

// requireBunCheck fails unless the bun at bunBin is bunCheckBun or later.
func requireBunCheck(bunBin, workDir string) error {
	version, err := readBunVersion(bunBin, workDir)
	if err != nil {
		return err
	}
	if slices.Compare(version[:], bunCheckBun[:]) < 0 {
		return fmt.Errorf("type-checker bun needs Bun %d.%d.%d or later for bun check, and %s is %d.%d.%d; set type-checker to tsc or upgrade Bun",
			bunCheckBun[0], bunCheckBun[1], bunCheckBun[2], bunBin, version[0], version[1], version[2])
	}
	return nil
}

// bunCheckArgs are the arguments of a bun check run on the scratch tsconfig
// with the compiler flags of a tsc run. bun check emits nothing; --no-pretty
// prints tsc's one-line diagnostics, which replaces --pretty false, --all
// shows every error instead of grouping repeated ones, and --no-script keeps a
// check script of the project's package.json from replacing the checker.
func bunCheckArgs(fsRoot string, flags []string, scratch string) []string {
	args := []string{"check", "--no-pretty", "--all", "--no-script", "--rootDir", fsRoot}
	for i := 0; i < len(flags); i++ {
		if flags[i] == "--pretty" {
			i++
			continue
		}
		args = append(args, flags[i])
	}
	return append(args, "-p", scratch)
}

// typeCheckScopeResult is what typeCheckScope finds in a project.
type typeCheckScopeResult struct {
	// nested holds the project-relative paths of the nested projects.
	nested []string
	// dotIncludes holds the include patterns that reach the checked files
	// typeCheckPatterns miss: those under a dot-directory.
	dotIncludes []string
	// found reports whether any TypeScript file to check lies outside the
	// nested projects and the skipped directories.
	found bool
}

// typeCheckScope walks the project and returns its nested projects, the
// patterns its dot-directories need, and whether it holds a file to check.
func typeCheckScope(projectPath string) (typeCheckScopeResult, error) {
	skipped := make(map[string]bool, len(typeCheckSkippedDirectories))
	for _, dir := range typeCheckSkippedDirectories {
		skipped[dir] = true
	}
	var scope typeCheckScopeResult
	dotDirs := map[string]bool{}
	err := filepath.WalkDir(projectPath, func(file string, d os.DirEntry, err error) error {
		if err != nil {
			if file == projectPath {
				return err
			}
			return nil
		}
		if file == projectPath {
			return nil
		}
		rel, err := filepath.Rel(projectPath, file)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipped[d.Name()] {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(file, "putnami.json")); err == nil {
				scope.nested = append(scope.nested, rel)
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || !isTypeScriptFile(d.Name()) {
			return nil
		}
		// A declaration file only feeds the check, so it never makes a project
		// worth checking, but its dot-directory still enters the include: the
		// program needs the types it declares, as build~types reads them.
		if isTypeCheckFile(d.Name()) {
			scope.found = true
		}
		if dir := deepestDotDirectory(path.Dir(rel)); dir != "" && !dotDirs[dir] {
			dotDirs[dir] = true
			for _, pattern := range typeCheckPatterns {
				scope.dotIncludes = append(scope.dotIncludes, dir+"/"+pattern)
			}
		}
		return nil
	})
	return scope, err
}

// deepestDotDirectory returns the prefix of dir that ends with its last
// dot-named segment, or "" when no segment starts with a dot. Below that
// prefix, typeCheckPatterns match every file again.
func deepestDotDirectory(dir string) string {
	segments := strings.Split(dir, "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if strings.HasPrefix(segments[i], ".") && segments[i] != "." {
			return strings.Join(segments[:i+1], "/")
		}
	}
	return ""
}

// isTypeCheckFile reports whether name is a TypeScript file tsc checks: any
// of typeCheckPatterns except a declaration file, which only feeds the check.
func isTypeCheckFile(name string) bool {
	if !isTypeScriptFile(name) {
		return false
	}
	for _, suffix := range []string{".d.ts", ".d.mts", ".d.cts"} {
		if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
			return false
		}
	}
	return true
}

// isTypeScriptFile reports whether name matches one of typeCheckPatterns,
// declaration files included.
func isTypeScriptFile(name string) bool {
	switch filepath.Ext(name) {
	case ".ts", ".tsx", ".mts", ".cts":
		return true
	}
	return false
}

// projectCompilerFlags are the flags of a check that keeps the project's own
// compiler options: it skips declaration files and prints plain diagnostics.
func projectCompilerFlags(ignoreDeprecations string) []string {
	flags := []string{"--skipLibCheck", "true"}
	if ignoreDeprecations != "" {
		flags = append(flags, "--ignoreDeprecations", ignoreDeprecations)
	}
	return append(flags, "--pretty", "false")
}
