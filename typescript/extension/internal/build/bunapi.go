package build

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	pexec "go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/project"
)

// The node-target check. Bun runs the code it builds for itself; a build for
// Node.js is only correct when nothing it loads needs Bun. The check reads the
// module graph bun resolves for the node target, the dependencies included,
// and fails on every module that imports `bun` or a `bun:` module, or that
// references the Bun global, outside a `typeof Bun` guard. It reports each
// such module with the import chain from an entrypoint that reaches it.
//
// The graph comes from a bundle bun writes into a temporary directory with
// `--metafile`: bun resolves the node export conditions, the tsconfig paths and
// the workspace links exactly as it does for the build, and records every
// input module with the imports that survive type elision. `bun` and `bun:`
// imports stay external, so the metafile records them. A bare package that
// does not resolve at build time stays external too, and is not read. The
// uses and their guards are found by bunglobal.mjs, which parses each
// JavaScript or TypeScript module with the TypeScript parser the project
// resolves.

//go:embed bunglobal.mjs
var bunGlobalScript []byte

// bunAPICheckTimeout bounds each of the two bun processes of the check.
const bunAPICheckTimeout = 5 * time.Minute

// ErrNoTypeScriptParser says the typescript package does not resolve from the
// project, so the node-target check cannot read its modules.
var ErrNoTypeScriptParser = errors.New("the node-target check needs the typescript package, which does not resolve from the project: run `putnami install`")

// BunAPIUse is one module of a node-target graph that needs Bun.
type BunAPIUse struct {
	// Module is the module's path relative to the project, slash-separated.
	Module string
	// Imports are the `bun` and `bun:` modules it imports outside a guard, in
	// byte order.
	Imports []string
	// Globals are the positions of its unguarded references to the Bun
	// global, in source order.
	Globals []SourcePosition
	// Chain is the import chain from the entrypoint that reaches the module
	// to the module itself, both included.
	Chain []string
}

// SourcePosition is a 1-based line and column.
type SourcePosition struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Message describes the use for a diagnostic.
func (u BunAPIUse) Message() string {
	var needs []string
	if len(u.Globals) > 0 {
		at := make([]string, 0, len(u.Globals))
		for _, position := range u.Globals {
			at = append(at, fmt.Sprintf("%d:%d", position.Line, position.Column))
		}
		needs = append(needs, "references the Bun global at "+strings.Join(at, ", "))
	}
	if len(u.Imports) > 0 {
		needs = append(needs, "imports "+strings.Join(u.Imports, ", "))
	}
	return fmt.Sprintf("%s %s, which Node.js does not provide; reached by %s",
		u.Module, strings.Join(needs, " and "), strings.Join(u.Chain, " -> "))
}

// NodeTargetChecked says whether a transpile with these parameters builds a
// graph for Node.js, which the node-target check must then read.
func NodeTargetChecked(params TranspileParams) bool {
	return params.Target == "node"
}

// CheckBunAPIsOnNode returns the modules of the project's server graph, built
// for the node target, that need Bun. The browser graph is always built for
// the browser target and is not read.
func CheckBunAPIsOnNode(bunBin, projectPath string) ([]BunAPIUse, error) {
	plan := resolveEntrypointPlan(projectPath)
	if len(plan.Server) == 0 {
		return nil, nil
	}
	typescript, ok := resolveTypeScriptPackage(projectPath)
	if !ok {
		return nil, ErrNoTypeScriptParser
	}
	graph, err := readNodeGraph(bunBin, projectPath, plan.Server)
	if err != nil {
		return nil, err
	}
	scans, err := scanModules(bunBin, typescript, projectPath, graph.sourceModules())
	if err != nil {
		return nil, err
	}
	return graph.uses(plan.Server, scans), nil
}

// nodeGraph is the module graph of a bun metafile: each input module, by its
// path relative to the project, with its imports.
type nodeGraph struct {
	Inputs map[string]struct {
		Imports []struct {
			Path     string `json:"path"`
			External bool   `json:"external"`
		} `json:"imports"`
	} `json:"inputs"`
}

// unresolvedPackage matches bun's report of an import it cannot resolve.
var unresolvedPackage = regexp.MustCompile(`Could not resolve: "([^"]+)"`)

// maxGraphReads bounds the bundles readNodeGraph runs: each one after the
// first externalizes the bare packages the previous one could not resolve.
const maxGraphReads = 3

// readNodeGraph bundles the entrypoints for the node target into a temporary
// directory and returns the module graph bun records. A bare package that
// does not resolve, such as an optional peer that is not installed, is
// externalized and the bundle runs again; an unresolved relative import fails.
func readNodeGraph(bunBin, projectPath string, entrypoints []string) (*nodeGraph, error) {
	dir, err := os.MkdirTemp("", "putnami-ts-node-graph-")
	if err != nil {
		return nil, fmt.Errorf("node-target check: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	metafile := filepath.Join(dir, "metafile.json")
	tsconfig := project.ResolveTsConfig(projectPath, filepath.Dir(projectPath))
	// `bun` and `bun:` imports stay external so the metafile records them;
	// bun bundles a named import of `bun` for no other target than bun.
	externals := []string{"bun", "bun:*"}
	for read := 1; ; read++ {
		args := []string{"build", "--target", "node", "--outdir", filepath.Join(dir, "out"), "--root", projectPath, "--env=disable", "--metafile=" + metafile}
		for _, external := range externals {
			args = append(args, "--external", external)
		}
		if tsconfig != "" {
			args = append(args, "--tsconfig", tsconfig)
		}
		for _, entrypoint := range entrypoints {
			args = append(args, filepath.Join(projectPath, entrypoint))
		}
		result, err := execRunFunc(bunBin, args, pexec.Dir(projectPath), pexec.Timeout(bunAPICheckTimeout))
		if err != nil {
			return nil, fmt.Errorf("node-target check: bundle the graph: %w", err)
		}
		if !result.Success {
			unresolved := unresolvedPackages(result.Stderr)
			if len(unresolved) == 0 || read == maxGraphReads {
				return nil, fmt.Errorf("node-target check: bun could not bundle the graph for the node target: %s", strings.TrimSpace(result.Stderr))
			}
			externals = append(externals, unresolved...)
			continue
		}
		break
	}
	data, err := os.ReadFile(metafile)
	if err != nil {
		return nil, fmt.Errorf("node-target check: read the metafile: %w", err)
	}
	var graph nodeGraph
	if err := json.Unmarshal(data, &graph); err != nil {
		return nil, fmt.Errorf("node-target check: decode the metafile: %w", err)
	}
	return &graph, nil
}

// unresolvedPackages returns the bare packages bun reports it could not
// resolve, in byte order, each with its subpaths. It returns nothing when an
// unresolved import is relative or absolute: that one is a build error.
func unresolvedPackages(stderr string) []string {
	var packages []string
	for _, match := range unresolvedPackage.FindAllStringSubmatch(stderr, -1) {
		specifier := match[1]
		if strings.HasPrefix(specifier, ".") || strings.HasPrefix(specifier, "/") {
			return nil
		}
		if !slices.Contains(packages, specifier) {
			packages = append(packages, specifier)
		}
	}
	sort.Strings(packages)
	return packages
}

// modules returns the graph's input modules in byte order.
func (g *nodeGraph) modules() []string {
	modules := make([]string, 0, len(g.Inputs))
	for module := range g.Inputs {
		modules = append(modules, module)
	}
	sort.Strings(modules)
	return modules
}

// sourceExtensions are the module extensions the scanner parses. Any other
// input, such as a text or JSON file a loader imports, holds no code.
var sourceExtensions = map[string]bool{
	".js": true, ".mjs": true, ".cjs": true, ".jsx": true,
	".ts": true, ".mts": true, ".cts": true, ".tsx": true,
}

// sourceModules returns the graph's JavaScript and TypeScript modules in byte
// order.
func (g *nodeGraph) sourceModules() []string {
	var modules []string
	for _, module := range g.modules() {
		if sourceExtensions[strings.ToLower(filepath.Ext(module))] {
			modules = append(modules, module)
		}
	}
	return modules
}

// isBunModule says whether an import specifier names a module only Bun
// provides.
func isBunModule(specifier string) bool {
	return specifier == "bun" || strings.HasPrefix(specifier, "bun:")
}

// uses returns the modules that need Bun, in byte order, each with the
// shortest import chain that reaches it from an entrypoint; between two
// chains of the same length, the one from the earlier entrypoint in plan
// order.
func (g *nodeGraph) uses(entrypoints []string, scans map[string]moduleScan) []BunAPIUse {
	parents := make(map[string]string)
	reached := make(map[string]bool)
	var queue []string
	for _, entrypoint := range entrypoints {
		module := filepath.ToSlash(filepath.Clean(entrypoint))
		if _, ok := g.Inputs[module]; !ok || reached[module] {
			continue
		}
		reached[module] = true
		queue = append(queue, module)
	}
	for len(queue) > 0 {
		module := queue[0]
		queue = queue[1:]
		for _, imported := range g.Inputs[module].Imports {
			if imported.External || reached[imported.Path] {
				continue
			}
			if _, ok := g.Inputs[imported.Path]; !ok {
				continue
			}
			reached[imported.Path] = true
			parents[imported.Path] = module
			queue = append(queue, imported.Path)
		}
	}

	var uses []BunAPIUse
	for _, module := range g.modules() {
		if !reached[module] {
			continue
		}
		// An import counts when bun kept it after type elision and the
		// scanner found it outside a guard.
		scan := scans[module]
		var imports []string
		for _, imported := range g.Inputs[module].Imports {
			if imported.External && isBunModule(imported.Path) && slices.Contains(scan.Imports, imported.Path) && !slices.Contains(imports, imported.Path) {
				imports = append(imports, imported.Path)
			}
		}
		sort.Strings(imports)
		if len(imports) == 0 && len(scan.Uses) == 0 {
			continue
		}
		var chain []string
		for current := module; ; {
			chain = append([]string{current}, chain...)
			parent, ok := parents[current]
			if !ok {
				break
			}
			current = parent
		}
		uses = append(uses, BunAPIUse{Module: module, Imports: imports, Globals: scan.Uses, Chain: chain})
	}
	return uses
}

// moduleScan is what bunglobal.mjs finds in one module outside a guard: the
// positions of its uses of the Bun global and the `bun` and `bun:` modules it
// imports.
type moduleScan struct {
	Uses    []SourcePosition `json:"uses"`
	Imports []string         `json:"imports"`
}

// scanModules returns, per module, what bunglobal.mjs finds in it. A module
// with nothing to report is absent.
func scanModules(bunBin, typescript, projectPath string, modules []string) (map[string]moduleScan, error) {
	payload, err := json.Marshal(map[string]any{"typescript": typescript, "root": projectPath, "files": modules})
	if err != nil {
		return nil, fmt.Errorf("node-target check: %w", err)
	}
	dir, err := os.MkdirTemp("", "putnami-ts-bun-global-")
	if err != nil {
		return nil, fmt.Errorf("node-target check: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	script := filepath.Join(dir, "bunglobal.mjs")
	if err := os.WriteFile(script, bunGlobalScript, 0o600); err != nil {
		return nil, fmt.Errorf("node-target check: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), bunAPICheckTimeout)
	defer cancel()
	// --no-install: bun must load the parser it is given and fetch nothing.
	cmd := exec.CommandContext(ctx, bunBin, "--no-install", script) //nolint:gosec // the resolved bun and a script this binary embeds
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("node-target check: reading the modules timed out after %s", bunAPICheckTimeout)
		}
		return nil, fmt.Errorf("node-target check: read the modules: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var answer struct {
		Files []struct {
			Path string `json:"path"`
			moduleScan
		} `json:"files"`
		Errors []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		return nil, fmt.Errorf("node-target check: decode the answer of the reader: %w", err)
	}
	if len(answer.Errors) > 0 {
		return nil, fmt.Errorf("node-target check: read %s: %s", answer.Errors[0].Path, answer.Errors[0].Message)
	}
	scans := make(map[string]moduleScan, len(answer.Files))
	for _, file := range answer.Files {
		scan := file.moduleScan
		if len(scan.Uses) == 0 {
			scan.Uses = nil
		}
		if len(scan.Imports) == 0 {
			scan.Imports = nil
		}
		scans[file.Path] = scan
	}
	return scans, nil
}

// resolveTypeScriptPackage returns the directory of the typescript package
// that resolves from dir: the nearest node_modules/typescript, walking up to
// the filesystem root the way package resolution does.
func resolveTypeScriptPackage(dir string) (string, bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		candidate := filepath.Join(dir, "node_modules", "typescript")
		if _, err := os.Stat(filepath.Join(candidate, "package.json")); err == nil {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
