package build

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/project"
)

// browserConditions are the package.json export conditions whose targets are
// resolved by a browser-like loader. Entrypoints reached only through one of
// them are transpiled in their own bun build invocation so code splitting can
// never place server-only modules in a chunk the browser entry imports.
// Bun has no react-native target, so react-native entries share the browser
// graph — it is the only isolated non-server graph the bundler offers.
var browserConditions = map[string]bool{
	"browser":      true,
	"react-native": true,
}

// buildTranspileArgs builds the bun build args for transpilation.
func buildTranspileArgs(projectPath, outputPath string, params TranspileParams, entrypoints, externals []string, tsconfig string) []string {
	args := []string{"build"}
	for _, ext := range externals {
		args = append(args, "--external", ext)
	}
	args = append(args, "--outdir", outputPath)
	target := params.Target
	if target == "" {
		target = "bun"
	}
	args = append(args, "--target", target)
	args = append(args, "--root", projectPath)
	// Library output never depends on the builder's environment. An explicit
	// --env=disable keeps every process.env read, NODE_ENV included, a runtime
	// read for the consumer, and compiles JSX to the production automatic
	// runtime (react/jsx-runtime jsx/jsxs). Without it, Bun inlines the
	// builder's NODE_ENV and emits jsxDEV from react/jsx-dev-runtime whenever
	// NODE_ENV is not "production" or the project has a bunfig.toml; a
	// production consumer bundle then resolves jsxDEV to undefined.
	args = append(args, "--env=disable")
	if params.Sourcemap != "" && params.Sourcemap != "none" {
		args = append(args, "--sourcemap="+params.Sourcemap)
	}
	if params.Minify {
		// --minify-identifiers is safe again on Bun 1.4: re-bundling the
		// pre-minified ESM output (bun build / bun build --compile downstream)
		// no longer mis-binds identifiers. Validated by re-bundling and
		// compiling the minified utils/runtime dists and comparing every
		// export against the source modules.
		args = append(args, "--minify-syntax", "--minify-whitespace", "--minify-identifiers")
	}
	if params.Splitting {
		args = append(args, "--splitting", "--format=esm")
	}
	if tsconfig != "" {
		args = append(args, "--tsconfig", tsconfig)
	}
	if params.MetafilePath != "" {
		args = append(args, "--metafile-md="+params.MetafilePath)
	}
	for _, ep := range entrypoints {
		args = append(args, filepath.Join(projectPath, ep))
	}
	return args
}

// RunTranspile invokes bun build for transpilation.
//
// Entrypoints are partitioned into independent build graphs (see
// entrypointPlan): the server graph — default/node/bun export conditions plus
// bin executables — and, when the package declares a browser-family export
// condition, a browser graph built with the browser target. Each graph is a
// separate bun build invocation, so `--splitting` can only emit shared chunks
// *within* a graph and server-only modules can never end up in a chunk the
// published browser entry imports. Both invocations write into the same
// --outdir with the same --root, so emitted entry paths land exactly where the
// published `exports` expect them; chunk filenames are content-hashed, so the
// two graphs can only ever agree on a name when the bytes are identical.
func RunTranspile(bunBin, projectPath, outputPath string, params TranspileParams) ([]string, []string, error) {
	plan := resolveEntrypointPlan(projectPath)
	if plan.isEmpty() {
		return nil, nil, nil
	}

	var externals []string
	if params.Bundle != "bundled" {
		externals = resolveExternals(projectPath)
	}

	// The output dir must hold exactly this build's files. Bun emits
	// content-hashed shared chunks (tsconfig-<hash>.js); when a chunk's content
	// changes the hash changes and the previous file is never overwritten, so
	// lib/ accumulates stale chunks across rebuilds. npm packaging copies lib/
	// wholesale, which made the staged tarball depend on the machine's rebuild
	// history — two publishers packed different bytes for the same version and
	// the managed release-set byte verification failed. Both graphs share this
	// outdir, so clean once before the loop, never inside it.
	if err := os.RemoveAll(outputPath); err != nil {
		return nil, nil, fmt.Errorf("cleaning transpile output %s: %w", outputPath, err)
	}
	if err := os.MkdirAll(outputPath, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating transpile output %s: %w", outputPath, err)
	}

	tsconfig := project.ResolveTsConfig(projectPath, filepath.Dir(projectPath))

	for _, graph := range plan.graphs() {
		graphParams := params
		if graph.browser {
			graphParams.Target = "browser"
		}
		if params.Metafile {
			// One report per graph: both invocations share --outdir, so a
			// single filename would silently overwrite the server report.
			graphParams.MetafilePath = filepath.Join(outputPath, "metafile."+graph.name+".md")
		}
		args := buildTranspileArgs(projectPath, outputPath, graphParams, graph.entrypoints, externals, tsconfig)

		result, err := execRunFunc(bunBin, args, exec.Dir(projectPath), exec.Timeout(5*time.Minute))
		if err != nil {
			return nil, []string{fmt.Sprintf("Transpile failed [%s graph]: %v", graph.name, err)}, nil
		}
		if !result.Success {
			return nil, []string{fmt.Sprintf("Transpile failed [%s graph]: %s", graph.name, result.Stderr)}, nil
		}
	}

	// Discover output files
	var files []string
	filepath.WalkDir(outputPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})

	return files, nil, nil
}

// TranspileParams holds transpile-specific options.
type TranspileParams struct {
	Target    string
	Bundle    string
	Sourcemap string
	Minify    bool
	Splitting bool
	// Metafile writes bun's markdown module-graph report (--metafile-md,
	// Bun 1.4) next to the transpiled output, one file per build graph.
	Metafile bool
	// MetafilePath is the resolved per-graph report path; RunTranspile derives
	// it from Metafile, callers leave it empty.
	MetafilePath string
}

// transpileGraph is one bun build invocation: a set of entrypoints that may
// share chunks with each other and with nothing else.
type transpileGraph struct {
	name        string
	browser     bool
	entrypoints []string
}

// entrypointPlan is the publication-shaped partition of a package's
// entrypoints. Server holds everything reachable through a non-browser export
// condition (default/node/bun/...), the `main`/`src` fallbacks, and every bin
// executable. Browser holds the entrypoints reachable *only* through a
// browser-family condition. An entrypoint reachable from both stays in Server:
// a module the server graph legitimately owns is never silently relocated.
type entrypointPlan struct {
	Server  []string
	Browser []string
}

func (p entrypointPlan) isEmpty() bool {
	return len(p.Server) == 0 && len(p.Browser) == 0
}

// graphs returns the bun build invocations this plan requires, in a stable
// order. A package with no browser-family condition yields exactly one graph.
func (p entrypointPlan) graphs() []transpileGraph {
	var out []transpileGraph
	if len(p.Server) > 0 {
		out = append(out, transpileGraph{name: "server", entrypoints: p.Server})
	}
	if len(p.Browser) > 0 {
		out = append(out, transpileGraph{name: "browser", browser: true, entrypoints: p.Browser})
	}
	return out
}

// resolveEntrypointPlan reads package.json and partitions every transpilable
// entrypoint into the build graphs that must not share chunks.
func resolveEntrypointPlan(projectPath string) entrypointPlan {
	pkg := project.ReadPackageJSONSafe(filepath.Join(projectPath, "package.json"))
	if pkg == nil {
		return entrypointPlan{Server: defaultEntrypoints(projectPath)}
	}

	// Track, per entrypoint file, whether any non-browser condition reaches it.
	seen := map[string]bool{}
	serverReached := map[string]bool{}
	var order []string
	record := func(path string, browser bool) {
		if !seen[path] {
			seen[path] = true
			order = append(order, path)
		}
		if !browser {
			serverReached[path] = true
		}
	}

	// Check exports
	if pkg.Exports != nil {
		exports := pkg.GetExportsMap()
		for _, key := range sortedKeys(exports) {
			raw := exports[key]
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" && project.FileExists(filepath.Join(projectPath, s)) {
				record(s, false)
				continue
			}
			var conditions map[string]string
			if json.Unmarshal(raw, &conditions) == nil {
				for _, cond := range sortedKeys(conditions) {
					v := conditions[cond]
					if v != "" && project.FileExists(filepath.Join(projectPath, v)) {
						record(v, browserConditions[cond])
					}
				}
			}
		}
	}

	var plan entrypointPlan
	for _, ep := range order {
		if serverReached[ep] {
			plan.Server = append(plan.Server, ep)
			continue
		}
		plan.Browser = append(plan.Browser, ep)
	}

	if plan.isEmpty() {
		plan.Server = defaultEntrypoints(projectPath)
	}

	// Include bin/*.ts files as additional entrypoints so they are transpiled
	// alongside the main exports. Without this, published packages ship raw .ts
	// bin scripts whose relative imports into ../src/ break because only .d.ts
	// declarations (not source files) are included in the package. Executables
	// always belong to the server graph.
	plan.Server = append(plan.Server, resolveBinEntrypoints(projectPath, pkg)...)

	return plan
}

// sortedKeys returns a map's keys in byte order so entrypoint ordering — and
// therefore the emitted chunk layout — does not depend on Go map iteration.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveBinEntrypoints returns bin/*.ts files that should be transpiled.
func resolveBinEntrypoints(projectPath string, pkg *project.PackageJSON) []string {
	if pkg == nil {
		return nil
	}
	binMap := pkg.GetBinMap()
	if len(binMap) == 0 {
		return nil
	}
	var bins []string
	for _, name := range sortedKeys(binMap) {
		p := binMap[name]
		if p != "" && project.FileExists(filepath.Join(projectPath, p)) {
			bins = append(bins, p)
		}
	}
	return bins
}

func defaultEntrypoints(projectPath string) []string {
	for _, f := range []string{"src/main.ts", "src/index.ts", "src/lib.ts"} {
		if project.FileExists(filepath.Join(projectPath, f)) {
			return []string{f}
		}
	}
	return nil
}

func resolveExternals(projectPath string) []string {
	pkg := project.ReadPackageJSONSafe(filepath.Join(projectPath, "package.json"))
	if pkg == nil {
		return nil
	}

	externals := make(map[string]bool)
	for dep := range pkg.Dependencies {
		externals[dep] = true
	}
	for dep := range pkg.PeerDependencies {
		externals[dep] = true
	}

	result := make([]string, 0, len(externals))
	for dep := range externals {
		result = append(result, dep)
	}
	return result
}
