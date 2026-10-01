package build

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/project"
)

// buildTscArgs builds the tsc command args for type declaration generation.
//
// When tsBuildInfoFile is non-empty, incremental compilation is enabled: tsc
// records per-file build state in that file so a later run re-type-checks and
// re-emits only the changed delta. Both flags are omitted when it is empty (no
// warm cache directory is available, e.g. unit tests without a CacheRoot), which
// preserves the cold, full-recheck behavior byte-for-byte.
//
// ignoreDeprecations is the --ignoreDeprecations value the running compiler
// accepts (see typeScriptIgnoreDeprecations); the flag is omitted when it is
// empty.
func buildTscArgs(outputPath, tsconfig string, sourceFiles []string, tsBuildInfoFile, ignoreDeprecations string) []string {
	args := []string{"x", "tsc",
		"--rootDir", ".",
		"--outDir", outputPath,
		"--declaration",
		"--emitDeclarationOnly",
		"--declarationMap",
		"--experimentalDecorators",
		"--strict", "true",
		"--target", "esnext",
		"--module", "esnext",
		"--types", "bun",
		"--moduleResolution", "bundler",
		"--jsx", "react-jsx",
		"--lib", "ESNext,DOM",
		"--skipLibCheck", "true",
		"--skipDefaultLibCheck", "true",
		"--forceConsistentCasingInFileNames", "true",
	}
	if ignoreDeprecations != "" {
		args = append(args, "--ignoreDeprecations", ignoreDeprecations)
	}
	// Force plain (non-ANSI) diagnostics so ParseTscOutput can extract
	// file/line/column; tsc emits pretty/colorized output in a TTY otherwise.
	args = append(args, "--pretty", "false")
	// --incremental composes with --emitDeclarationOnly: tsc persists file
	// signatures to tsBuildInfoFile and, on a warm run, re-checks/re-emits only
	// the files whose inputs changed.
	if tsBuildInfoFile != "" {
		args = append(args, "--incremental", "--tsBuildInfoFile", tsBuildInfoFile)
	}
	if tsconfig != "" {
		args = append(args, "--project", tsconfig)
	} else {
		args = append(args, sourceFiles...)
	}
	return args
}

// ignoreDeprecationsByMajor maps a TypeScript major version to the
// --ignoreDeprecations value that compiler accepts. A compiler rejects a value
// newer than itself: TypeScript 5.9 fails every build with TS5103 when it is
// given "6.0". So the value follows the TypeScript that runs, the one
// workspace-install adds from the manifest's workspaceDevDependencies or the one
// the workspace pinned, never a constant.
var ignoreDeprecationsByMajor = map[int]string{5: "5.0", 6: "6.0"}

// typeScriptIgnoreDeprecations returns the --ignoreDeprecations value for the
// TypeScript that `bun x tsc` runs from projectPath, or "" when that TypeScript
// is not found or its major version has no known value.
func typeScriptIgnoreDeprecations(projectPath string) string {
	major, ok := installedTypeScriptMajor(projectPath)
	if !ok {
		return ""
	}
	return ignoreDeprecationsByMajor[major]
}

// installedTypeScriptMajor returns the major version of the typescript package
// that resolves from dir: the nearest node_modules/typescript/package.json,
// walking up to the filesystem root the way package resolution does.
func installedTypeScriptMajor(dir string) (int, bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return 0, false
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "node_modules", "typescript", "package.json"))
		if err == nil {
			var pkg struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &pkg) != nil {
				return 0, false
			}
			head, _, _ := strings.Cut(pkg.Version, ".")
			major, err := strconv.Atoi(head)
			if err != nil {
				return 0, false
			}
			return major, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, false
		}
		dir = parent
	}
}

// TypesResult holds the output of a tsc type-declaration build.
type TypesResult struct {
	GeneratedFiles []string
	Diagnostics    []parse.TscDiagnostic
	Success        bool
	RawOutput      string // combined stdout+stderr from tsc
}

// createTypesTsConfig generates a temporary tsconfig that extends the project's
// tsconfig but restricts include to only source directories (src, bin, .gen).
// This avoids type-checking test files during declaration generation.
//
// The file is scratch: it is written to scratchDir (the task's persistent types
// directory, or the system temporary directory when there is none) and never
// into the project, whose directory can lie inside another task's declared
// output. The config resolves exactly as it would from the project directory:
// every path in it is absolute, and the settings tsc derives from the loading
// config's directory are re-declared against the project (see
// typesCompilerOptions and typesFiles).
//
// A unique filename is used so concurrent RunTypes invocations on the same
// project (e.g. build~types and package~types planned by `publish`) don't
// race on writing/removing a shared `tsconfig.types.json`.
func createTypesTsConfig(scratchDir, projectPath, baseTsConfig string) (string, error) {
	projectPath, err := filepath.Abs(projectPath)
	if err != nil {
		return "", err
	}
	baseTsConfig, err = filepath.Abs(baseTsConfig)
	if err != nil {
		return "", err
	}
	inProject := func(pattern string) string {
		return filepath.ToSlash(filepath.Join(projectPath, pattern))
	}
	chain := project.ReadTsConfigChain(baseTsConfig)
	cfg := map[string]any{
		"extends":         filepath.ToSlash(baseTsConfig),
		"compilerOptions": typesCompilerOptions(projectPath, chain),
		"include": []string{
			inProject("src/**/*.ts"), inProject("src/**/*.tsx"), inProject("bin/**/*.ts"),
			inProject(".gen/**/*.ts"), inProject(".gen/**/*.tsx"),
		},
		"exclude": []string{
			inProject("**/*.test.ts"), inProject("**/*.spec.ts"), inProject("**/*.test.tsx"), inProject("**/*.spec.tsx"),
		},
	}
	if files, ok := typesFiles(projectPath, chain); ok {
		cfg["files"] = files
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if scratchDir != "" {
		if scratchDir, err = filepath.Abs(scratchDir); err != nil {
			return "", err
		}
		if err := os.MkdirAll(scratchDir, 0o755); err != nil {
			return "", err
		}
	}
	f, err := os.CreateTemp(scratchDir, "tsconfig.types.*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// configDirTemplate is the tsconfig template tsc replaces with the directory
// of the config file it loads, wherever in the extends chain it is written.
const configDirTemplate = "${configDir}"

// pathCompilerOptions are the compilerOptions whose relative paths tsc
// resolves against the directory of the config that declares them.
var pathCompilerOptions = map[string]bool{
	"baseUrl": true, "declarationDir": true, "outDir": true, "outFile": true, "paths": true,
	"rootDir": true, "rootDirs": true, "tsBuildInfoFile": true, "typeRoots": true,
}

// typesCompilerOptions returns the compilerOptions the scratch config declares
// so that it resolves as it would from projectPath:
//   - typeRoots: the chain's own, anchored on the configs that declare them;
//     otherwise the defaults tsc derives from the project directory.
//   - every inherited option that uses ${configDir}, with the template
//     replaced by the project directory and relative paths anchored on the
//     declaring config.
func typesCompilerOptions(projectPath string, chain project.TsConfigChain) map[string]any {
	opts := map[string]any{"typeRoots": defaultTypeRoots(projectPath)}
	_, hasBaseURL := chain.CompilerOptions["baseUrl"]
	for name, option := range chain.CompilerOptions {
		if name != "typeRoots" && !mentionsConfigDir(option.Value) {
			continue
		}
		anchor := ""
		if pathCompilerOptions[name] && !(name == "paths" && hasBaseURL) {
			// paths entries resolve against baseUrl when one is set.
			anchor = option.Dir
		}
		opts[name] = anchorTsConfigValue(option.Value, anchor, projectPath)
	}
	return opts
}

// typesFiles returns the files list to re-declare when the inherited one uses
// ${configDir}; any other inherited list already resolves correctly.
func typesFiles(projectPath string, chain project.TsConfigChain) (any, bool) {
	if chain.Files == nil || !mentionsConfigDir(chain.Files.Value) {
		return nil, false
	}
	return anchorTsConfigValue(chain.Files.Value, chain.Files.Dir, projectPath), true
}

// anchorTsConfigValue replaces ${configDir} with configDir in every string of
// value and, when anchor is set, makes every remaining relative path absolute
// against it.
func anchorTsConfigValue(value any, anchor, configDir string) any {
	switch v := value.(type) {
	case string:
		v = strings.ReplaceAll(v, configDirTemplate, filepath.ToSlash(configDir))
		if anchor != "" && !filepath.IsAbs(filepath.FromSlash(v)) {
			v = filepath.ToSlash(filepath.Join(anchor, filepath.FromSlash(v)))
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = anchorTsConfigValue(item, anchor, configDir)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = anchorTsConfigValue(item, anchor, configDir)
		}
		return out
	default:
		return value
	}
}

// mentionsConfigDir reports whether any string in value uses ${configDir}.
func mentionsConfigDir(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, configDirTemplate)
	case []any:
		for _, item := range v {
			if mentionsConfigDir(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if mentionsConfigDir(item) {
				return true
			}
		}
	}
	return false
}

// defaultTypeRoots returns the node_modules/@types directory of dir and of
// every ancestor, nearest first: the type roots tsc derives for a config file
// in dir that declares none.
func defaultTypeRoots(dir string) []string {
	var roots []string
	for {
		roots = append(roots, filepath.ToSlash(filepath.Join(dir, "node_modules", "@types")))
		parent := filepath.Dir(dir)
		if parent == dir {
			return roots
		}
		dir = parent
	}
}

// legacyTypesTsConfig matches the name of a temporary tsconfig earlier
// releases wrote into the project directory: os.CreateTemp's random digits in
// place of the pattern's star.
var legacyTypesTsConfig = regexp.MustCompile(`^tsconfig\.types\.[0-9]+\.json$`)

// removeLegacyTypesTsConfigs deletes the temporary tsconfig files earlier
// releases could leave in the project directory when a run was interrupted.
// Any other file, such as a tsconfig.types.esm.json the project owns, stays.
func removeLegacyTypesTsConfigs(projectPath string) error {
	entries, err := os.ReadDir(projectPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !legacyTypesTsConfig.MatchString(entry.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(projectPath, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// RunTypes invokes tsc to generate type declarations.
//
// tsBuildInfoDir, when non-empty, is a PERSISTENT per-(command,project) scratch
// directory (under the workspace CacheRoot) that survives across cache misses —
// unlike the content-addressed outputPath, which the scheduler clears every miss.
// It holds two artifacts: tsc's incremental .tsbuildinfo and a "mirror" of the
// complete declaration set, so a miss re-type-checks only the changed delta.
//
// tsc ALWAYS emits directly into outputPath (`--outDir == outputPath`) so every
// emitted .d.ts.map keeps correct outputPath-relative `sources` paths — relocating
// the outDir and copying maps to a different depth would break those relative
// chains and regress IDE go-to-source. Because a warm incremental run re-emits
// ONLY the changed files, RunTypes SEEDs outputPath from the mirror before tsc
// runs (restoring the unchanged files the scheduler cleared) and REFRESHes the
// mirror from outputPath after, so the captured set is always COMPLETE. The
// mirror is storage only, never consumed in place, so its maps stay correct.
// When tsBuildInfoDir is empty, RunTypes keeps the cold path exactly: tsc emits
// straight into outputPath with no seed/mirror/incremental state.
func RunTypes(bunBin, projectPath, outputPath, tsBuildInfoDir string) (*TypesResult, error) {
	tsconfig := project.ResolveTsConfig(projectPath, filepath.Dir(projectPath))

	// The full source-file list is only needed as command-line arguments when
	// there is no tsconfig; with a tsconfig, tsc discovers files via --project
	// and the list is discarded. So when a tsconfig exists we use a
	// short-circuit emptiness gate instead of enumerating src/bin in full.
	var sourceFiles []string
	if tsconfig != "" {
		if !hasSourceFiles(projectPath) {
			if err := clearTypesIncrementalState(outputPath, tsBuildInfoDir); err != nil {
				return nil, err
			}
			return &TypesResult{Success: true}, nil
		}

		// Generate a temporary types-only tsconfig that extends the project's
		// but restricts include to source directories only. This avoids TS7
		// errors about files on the command line conflicting with tsconfig.
		if err := removeLegacyTypesTsConfigs(projectPath); err != nil {
			return nil, err
		}
		typesTsConfig, err := createTypesTsConfig(tsBuildInfoDir, projectPath, tsconfig)
		if err != nil {
			return nil, err
		}
		defer os.Remove(typesTsConfig)
		tsconfig = typesTsConfig
	} else {
		sourceFiles = discoverSourceFiles(projectPath)
		if len(sourceFiles) == 0 {
			if err := clearTypesIncrementalState(outputPath, tsBuildInfoDir); err != nil {
				return nil, err
			}
			return &TypesResult{Success: true}, nil
		}
	}

	// Resolve the incremental warm state. tsc emits directly into outputPath in
	// every mode so declaration maps keep correct outputPath-relative source
	// paths. With a warm dir, the .tsbuildinfo and a mirror of the complete
	// declaration set persist under tsBuildInfoDir. SEED outputPath from the
	// mirror before tsc runs so the scheduler's per-miss clear does not drop the
	// files a partial-emit run leaves untouched. Without a warm dir, keep the cold
	// behavior: no seed/mirror and no buildinfo.
	tsBuildInfoFile := ""
	mirrorDir := ""
	if tsBuildInfoDir != "" {
		tsBuildInfoFile = filepath.Join(tsBuildInfoDir, "types.tsbuildinfo")
		mirrorDir = filepath.Join(tsBuildInfoDir, "mirror")
		if err := seedTypesOutputFromMirror(projectPath, outputPath, tsBuildInfoDir, mirrorDir, tsBuildInfoFile); err != nil {
			return nil, err
		}
	}

	args := buildTscArgs(outputPath, tsconfig, sourceFiles, tsBuildInfoFile, typeScriptIgnoreDeprecations(projectPath))

	result, err := execRunFunc(bunBin, args, exec.Dir(projectPath), exec.Timeout(5*time.Minute))
	if err != nil {
		return nil, err
	}

	// Refresh the mirror only after a successful compile. A failed incremental
	// compile can leave partial declarations and build state behind; preserving
	// them would make the next successful run seed an invalid output set.
	if tsBuildInfoDir != "" {
		if result.Success {
			if err := refreshTypesMirror(outputPath, mirrorDir); err != nil {
				return nil, err
			}
		} else if err := os.Remove(tsBuildInfoFile); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	rawOutput := result.Stdout + result.Stderr

	// Parse tsc output
	diagnostics := parse.ParseTscOutput(rawOutput)

	// Discover generated .d.ts files from outputPath, so GeneratedFiles and its
	// metric reflect the full set consumers will see.
	var generatedFiles []string
	filepath.WalkDir(outputPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".d.ts") {
			generatedFiles = append(generatedFiles, path)
		}
		return nil
	})

	return &TypesResult{
		GeneratedFiles: generatedFiles,
		Diagnostics:    diagnostics,
		Success:        result.Success,
		RawOutput:      rawOutput,
	}, nil
}

// clearTypesIncrementalState removes the cached declaration set when no
// eligible source remains. Otherwise a later source addition could seed stale
// declarations that no longer belong to the project.
func clearTypesIncrementalState(outputPath, tsBuildInfoDir string) error {
	if tsBuildInfoDir == "" {
		return nil
	}
	if err := os.RemoveAll(outputPath); err != nil {
		return err
	}
	return os.RemoveAll(tsBuildInfoDir)
}

// seedTypesOutputFromMirror restores only declarations that can still be
// emitted by the project's current source set. tsc never removes files from an
// outDir, so copying an unpruned mirror would resurrect declarations for
// deleted or renamed source files indefinitely.
func seedTypesOutputFromMirror(projectPath, outputPath, tsBuildInfoDir, mirrorDir, tsBuildInfoFile string) error {
	if err := os.MkdirAll(tsBuildInfoDir, 0o755); err != nil {
		return err
	}

	info, err := os.Stat(mirrorDir)
	if os.IsNotExist(err) {
		if err := os.RemoveAll(outputPath); err != nil {
			return err
		}
		if err := os.Remove(tsBuildInfoFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("types declaration mirror is not a directory: %s", mirrorDir)
	}

	if err := pruneTypesMirror(projectPath, mirrorDir); err != nil {
		return err
	}
	if err := os.RemoveAll(outputPath); err != nil {
		return err
	}
	return CopyDir(mirrorDir, outputPath)
}

// refreshTypesMirror replaces the mirror with the complete successful output.
func refreshTypesMirror(outputPath, mirrorDir string) error {
	if err := os.RemoveAll(mirrorDir); err != nil {
		return err
	}
	if _, err := os.Stat(outputPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return CopyDir(outputPath, mirrorDir)
}

// pruneTypesMirror removes declaration artifacts that no current source can
// produce. Both declarations and declaration maps must be removed together.
func pruneTypesMirror(projectPath, mirrorDir string) error {
	expected := expectedDeclarationArtifacts(projectPath)
	return filepath.WalkDir(mirrorDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(mirrorDir, path)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(rel, ".d.ts") && !strings.HasSuffix(rel, ".d.ts.map") {
			return nil
		}
		if expected[rel] {
			return nil
		}
		return os.Remove(path)
	})
}

// expectedDeclarationArtifacts maps each eligible declaration source to the
// output paths implied by --rootDir . and --declarationMap.
func expectedDeclarationArtifacts(projectPath string) map[string]bool {
	expected := make(map[string]bool)
	for _, dir := range []string{"src", "bin", ".gen"} {
		dirPath := filepath.Join(projectPath, dir)
		filepath.WalkDir(dirPath, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isTypeSourceFile(path) {
				return nil
			}
			rel, err := filepath.Rel(projectPath, path)
			if err != nil {
				return nil
			}
			declaration := strings.TrimSuffix(rel, filepath.Ext(rel)) + ".d.ts"
			expected[declaration] = true
			expected[declaration+".map"] = true
			return nil
		})
	}
	return expected
}

// sourceDirs are the directories scanned for TypeScript source files.
var sourceDirs = []string{"src", "bin"}

// isTypeSourceFile reports whether path is a non-declaration, non-test
// TypeScript source file eligible for declaration generation.
func isTypeSourceFile(path string) bool {
	if strings.HasSuffix(path, ".d.ts") {
		return false
	}
	if strings.Contains(path, ".test.") || strings.Contains(path, ".spec.") {
		return false
	}
	ext := filepath.Ext(path)
	return ext == ".ts" || ext == ".tsx"
}

func discoverSourceFiles(projectPath string) []string {
	var files []string

	for _, dir := range sourceDirs {
		dirPath := filepath.Join(projectPath, dir)
		filepath.WalkDir(dirPath, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !isTypeSourceFile(path) {
				return nil
			}
			rel, _ := filepath.Rel(projectPath, path)
			files = append(files, rel)
			return nil
		})
	}

	return files
}

// hasSourceFiles reports whether src/ or bin/ contains at least one eligible
// TypeScript source file, short-circuiting the walk on the first match. Used
// as an emptiness gate when the full file list is not needed (tsconfig present).
func hasSourceFiles(projectPath string) bool {
	for _, dir := range sourceDirs {
		dirPath := filepath.Join(projectPath, dir)
		found := false
		filepath.WalkDir(dirPath, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if isTypeSourceFile(path) {
				found = true
				return filepath.SkipAll
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}
