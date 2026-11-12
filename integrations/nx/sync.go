package nx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	detect "go.putnami.dev/tooling/observe/detect/nx"
	"go.putnami.dev/tooling/stacks"
)

// SyncJob is the job name of the workspace sync task.
const SyncJob = "workspace-sync"

// CheckJob is the job name of the task that fails on a stale project graph.
const CheckJob = "nx-graph-check"

// ProjectSchema is the schema a putnami.json the sync creates names.
const ProjectSchema = "https://putnami.dev/schemas/putnami-project.json"

// Adoption modes.
const (
	// ModeImport hands a TypeScript project's targets to @putnami/typescript
	// and keeps every other target running through Nx.
	ModeImport = "import"
	// ModeDelegate runs every build, test and lint target through nx run.
	ModeDelegate = "delegate"
)

// ModeOption is the member of the workspace options of the extension that
// records the mode putnami init picked.
const ModeOption = "mode"

// DeclaredParam is the parameter of the sync job that lists extensions to
// read as declared beside the ones the workspace config lists: those a rerun
// of putnami init --from would declare before its sync, so that its dry run
// previews what the sync writes once they are.
const DeclaredParam = "declaredExtensions"

// Diagnostic codes of the sync.
const (
	// CodeSyncFailed is a sync that failed and wrote nothing.
	CodeSyncFailed = "nx.sync_failed"
	// CodeSyncStale is a project whose putnami.json no longer matches the
	// Nx graph.
	CodeSyncStale = "nx.sync_stale"
	// CodeUnmapped is a target the sync maps to no Putnami task, or that
	// keeps running through Nx in import mode.
	CodeUnmapped = "nx.target_unmapped"
	// CodeSyncPreview is a putnami.json a dry run would change, with its
	// diff.
	CodeSyncPreview = "nx.sync_preview"
	// CodeImportUnmatched is an entry of options["@putnami/nx"].import that
	// selects no Nx project.
	CodeImportUnmatched = "nx.import_unmatched"
)

// lifecycleTasks are the Putnami tasks a Nx target maps to, by name.
var lifecycleTasks = []string{"build", "lint", "test"}

// SyncOptions tune one sync.
type SyncOptions struct {
	// Mode is the adoption mode; empty reads the workspace option, and
	// import without one.
	Mode string
	// DryRun computes the changes and writes nothing.
	DryRun bool
	// Declared are extensions the sync reads as declared, beside the ones
	// the workspace config lists.
	Declared []string
	// Import lists, in delegate mode, the project paths and globs whose
	// projects the sync writes as import mode does, in place of the
	// workspace option when ImportGiven is set.
	Import      []string
	ImportGiven bool
}

// Unmapped is a target the sync does not hand to a Putnami task of its own.
type Unmapped struct {
	// Project is the Nx project name.
	Project string `json:"project"`
	// Target is the Nx target name.
	Target string `json:"target"`
	// Reason says why, and how the target still runs.
	Reason string `json:"reason"`
}

// Change is one putnami.json a sync writes, or would write in a dry run.
type Change struct {
	// File is the workspace-relative file.
	File string
	// Before is its content before the sync; nil for a new file.
	Before []byte
	// After is its content after the sync.
	After []byte
}

// SyncResult is what one sync did.
type SyncResult struct {
	// Mode is the mode the sync applied.
	Mode string
	// Source names where the graph came from: the graph export, or nx.json.
	// Empty when the root is no Nx workspace.
	Source string
	// Written are the workspace-relative putnami.json files the sync created
	// or changed, or would in a dry run, sorted.
	Written []string
	// Unchanged counts the files whose content was already current.
	Unchanged int
	// Retracted are the workspace-relative putnami.json files of former
	// projects the sync removed the extension from, sorted.
	Retracted []string
	// Unmapped are the targets no Putnami task of the sync's own runs.
	Unmapped []Unmapped
	// Changes are the files of Written and Retracted, with their content.
	Changes []Change
	// EmptyGraph reports a graph that lists no project while projects are
	// marked: the sync wrote nothing, so a filtered graph retracts none.
	EmptyGraph bool
	// Imported are the workspace-relative directories of the TypeScript
	// projects a delegated workspace's import list moved to
	// @putnami/typescript, sorted. A selected project that is no TypeScript
	// project, or whose workspace does not declare the extension, stays on
	// Nx and is listed among Unmapped.
	Imported []string
	// Unmatched are the entries of the import list that select no project.
	Unmatched []string
	// Conventions list each file an import rewrote to adopt Putnami's
	// TypeScript conventions, and each manifest it did not (D-066).
	Conventions []string
}

// Sync writes the project graph of the Nx workspace at root into each
// project's putnami.json: the extension, the projects it depends on as
// project IDs, and in disable.jobs each build, test or lint task the project
// does not run through the extension that owns it. A root with neither
// nx.json nor the graph export is no Nx workspace and gets an empty result.
func Sync(ctx context.Context, root string, opts SyncOptions) (SyncResult, error) {
	_ = ctx
	var result SyncResult
	if !exists(root, "nx.json") && !exists(root, detect.DefaultGraphFile) {
		return result, nil
	}
	mode, err := resolveMode(root, opts.Mode)
	if err != nil {
		return result, err
	}
	result.Mode = mode
	detection := detect.Detect(root, detect.Options{})
	result.Source = "nx.json"
	for _, d := range detection.Diagnostics {
		if d.Code == detect.CodeGraphUnreadable || d.Code == detect.CodeManifestUnreadable {
			return SyncResult{}, fmt.Errorf("the Nx configuration cannot be read, so no edge is written: %s", d.Message)
		}
	}
	if len(detection.Projects) > 0 && detection.Projects[0].FromGraph {
		result.Source = detect.DefaultGraphFile
	}
	current := map[string]bool{}
	for _, p := range detection.Projects {
		current[p.Path] = true
	}
	former, err := markedProjects(root)
	if err != nil {
		return SyncResult{}, err
	}
	if len(current) == 0 && len(former) > 0 {
		result.EmptyGraph = true
		return result, nil
	}
	components := map[string]bool{}
	for dir := range current {
		components[wsproto.ProjectIDFromPath(dir)] = true
	}
	for _, dir := range former {
		components[wsproto.ProjectIDFromPath(dir)] = true
	}
	imports, err := importList(root, mode, opts)
	if err != nil {
		return SyncResult{}, err
	}
	typeScript := slices.Contains(opts.Declared, TypeScriptExtension) || declaresExtension(root, TypeScriptExtension)
	var conventions []stacks.ConventionProject
	for _, p := range detection.Projects {
		projectMode := mode
		if imports.Selects(p.Path) {
			projectMode = ModeImport
		}
		plan := planProject(root, p, projectMode, typeScript)
		if mode == ModeDelegate && plan.typeScript {
			result.Imported = append(result.Imported, p.Path)
		}
		if plan.typeScript {
			conventions = append(conventions, stacks.ConventionProject{Dir: p.Path, Name: p.Name, BunBuild: plan.bunBuild, PublishedBy: stacks.PackagePublished(root, p.Path)})
		}
		result.Unmapped = append(result.Unmapped, plan.unmapped...)
		file := path.Join(p.Path, wsproto.ConfigFilename)
		change, err := writeProject(root, file, plan, components, opts.DryRun)
		if err != nil {
			return SyncResult{}, fmt.Errorf("%s: %w", file, err)
		}
		if change == nil {
			result.Unchanged++
			continue
		}
		result.Written = append(result.Written, file)
		result.Changes = append(result.Changes, *change)
	}
	for _, dir := range former {
		if current[dir] {
			continue
		}
		file := path.Join(dir, wsproto.ConfigFilename)
		change, err := retractProject(root, file, components, opts.DryRun)
		if err != nil {
			return SyncResult{}, fmt.Errorf("%s: %w", file, err)
		}
		if change != nil {
			result.Retracted = append(result.Retracted, file)
			result.Changes = append(result.Changes, *change)
		}
	}
	slices.Sort(result.Imported)
	result.Unmatched = imports.Unmatched()
	writes, warnings, err := stacks.ImportConventions(root, conventions)
	if err != nil {
		return SyncResult{}, err
	}
	_, notes, err := stacks.ApplyConventions(root, writes, opts.DryRun)
	if err != nil {
		return SyncResult{}, err
	}
	// A convention write is a change of the sync, written or not, as each
	// putnami.json is.
	for _, w := range writes {
		result.Changes = append(result.Changes, Change{File: w.File, Before: w.Before, After: w.After})
	}
	result.Conventions = append(append(result.Conventions, notes...), warnings...)
	return result, nil
}

// importList is the import list of a delegated workspace: opts.Import when
// given, else options["@putnami/nx"].import. Import mode reads none.
func importList(root, mode string, opts SyncOptions) (*stacks.ImportList, error) {
	if mode != ModeDelegate {
		return nil, nil
	}
	entries := opts.Import
	if !opts.ImportGiven {
		recorded, err := WorkspaceImport(root)
		if err != nil {
			return nil, err
		}
		entries = recorded
	}
	return stacks.NewImportList(entries)
}

// WorkspaceImport reads options["@putnami/nx"].import of the workspace at
// root; nil when the workspace records none.
func WorkspaceImport(root string) ([]string, error) {
	doc, err := readWorkspace(root)
	if err != nil || doc == nil {
		return nil, err
	}
	layer, _ := doc.Options[ExtensionName].(map[string]any)
	return stacks.ImportEntries(ExtensionName, layer)
}

// Stale answers what Sync would change in the recorded mode, and writes
// nothing.
func Stale(ctx context.Context, root string) (SyncResult, error) {
	return Sync(ctx, root, SyncOptions{DryRun: true})
}

// resolveMode is the mode given, else the one the workspace options record,
// else import.
func resolveMode(root, given string) (string, error) {
	mode := given
	if mode == "" {
		recorded, err := WorkspaceMode(root)
		if err != nil {
			return "", err
		}
		mode = recorded
	}
	if mode == "" {
		mode = ModeImport
	}
	if mode != ModeImport && mode != ModeDelegate {
		return "", fmt.Errorf("unknown mode %q: choose %s or %s", mode, ModeImport, ModeDelegate)
	}
	return mode, nil
}

// WorkspaceMode reads options["@putnami/nx"].mode of the workspace at root;
// empty when the workspace records none.
func WorkspaceMode(root string) (string, error) {
	doc, err := readWorkspace(root)
	if err != nil || doc == nil {
		return "", err
	}
	layer, _ := doc.Options[ExtensionName].(map[string]any)
	mode, _ := layer[ModeOption].(string)
	return mode, nil
}

type workspaceDoc struct {
	Extensions []string       `json:"extensions"`
	Options    map[string]any `json:"options"`
}

func readWorkspace(root string) (*workspaceDoc, error) {
	data, err := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename)) //nolint:gosec // the workspace file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc workspaceDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	return &doc, nil
}

// declaresExtension reports whether the workspace at root declares the
// extension: by name, or by a path whose manifest states that name.
func declaresExtension(root, name string) bool {
	doc, err := readWorkspace(root)
	if err != nil || doc == nil {
		return false
	}
	for _, e := range doc.Extensions {
		if e == name {
			return true
		}
		if !strings.HasPrefix(e, "/") && !strings.HasPrefix(e, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(e, "/")), "putnami.extension.json")) //nolint:gosec // a manifest the workspace declares
		if err != nil {
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &manifest) == nil && manifest.Name == name {
			return true
		}
	}
	return false
}

// projectPlan is what the sync writes for one project.
type projectPlan struct {
	// typeScript reports an imported TypeScript project, whose putnami.json
	// names @putnami/typescript beside the extension: a putnami.json that
	// names extensions is claimed by those alone.
	typeScript bool
	deps       []string
	disabled   []string
	reports    map[string]any
	// runner is the test runner the sync records for @putnami/typescript:
	// vitest or jest, or "" when it records none.
	runner   string
	unmapped []Unmapped
	// compiler reports an @nx/js:tsc build the TypeScript extension compiles
	// with tsc, for which the sync writes the tsc compiler (ADR 0004).
	compiler bool
	// eslint reports a lint target the TypeScript extension takes over that
	// runs ESLint: the project's putnami.json then names eslint as its linter.
	eslint bool
	// nodeRuntime reports an imported TypeScript project that runs on Node:
	// its putnami.json declares the node runtime.
	nodeRuntime bool
	// bunBuild reports a build the TypeScript extension takes over with its
	// Bun build, which writes nothing in the project (D-066).
	bunBuild bool
}

// planProject maps the targets of one project in the mode.
func planProject(root string, p detect.Project, mode string, typeScript bool) projectPlan {
	var plan projectPlan
	for _, d := range p.Dependencies {
		plan.deps = append(plan.deps, wsproto.ProjectIDFromPath(d))
	}
	targets := map[string]detect.Target{}
	for _, t := range p.Targets {
		targets[t.Name] = t
		if !slices.Contains(lifecycleTasks, t.Name) {
			plan.unmapped = append(plan.unmapped, Unmapped{Project: p.Name, Target: t.Name,
				Reason: fmt.Sprintf("no Putnami task runs it: run nx run %s:%s", p.Name, t.Name)})
		}
	}
	isTS := mode == ModeImport && typeScript && isTypeScriptProject(root, p.Path)
	plan.typeScript = isTS
	plan.nodeRuntime = isTS && (targets["serve"].Executor == nodeExecutor || startScriptRunsNode(filepath.Join(root, filepath.FromSlash(p.Path))))
	for _, task := range lifecycleTasks {
		target, ok := targets[task]
		covered := ok && isTS && coveredByTypeScript(root, p.Path, task, target)
		plan.bunBuild = plan.bunBuild || (task == "build" && covered)
		reason := ""
		if ok && task == "build" && !covered {
			// A tsc build moves when the TypeScript extension compiles it
			// with that tsc (ADR 0010 of @putnami/typescript).
			var tscCovered, compiler bool
			tscCovered, compiler, reason = tscBuild(root, p.Path, target)
			covered = isTS && tscCovered
			plan.compiler = covered && compiler
		}
		if reason == "" && ok {
			reason = importReason(root, p.Path, typeScript, isTS, task, target)
		}
		switch {
		case !ok:
			plan.disabled = append(plan.disabled, task)
		case covered:
			plan.disabled = append(plan.disabled, ExtensionName+":"+task)
			if task == "test" {
				plan.runner = testRunner(target)
			}
			if task == "lint" && lintsWithESLint(root, p.Path, target) {
				plan.eslint = true
			}
		case typeScript && hasManifest(root, p.Path):
			// The TypeScript extension claims the project too: the target
			// runs through Nx alone.
			plan.disabled = append(plan.disabled, TypeScriptExtension+":"+task)
			if mode == ModeImport {
				plan.unmapped = append(plan.unmapped, Unmapped{Project: p.Name, Target: task, Reason: reason})
			}
		case mode == ModeImport:
			plan.unmapped = append(plan.unmapped, Unmapped{Project: p.Name, Target: task, Reason: reason})
		}
	}
	if test, ok := targets["test"]; ok && !slices.Contains(plan.disabled, ExtensionName+":test") {
		plan.reports = testReports(root, p.Path, test)
		if plan.reports == nil {
			plan.unmapped = append(plan.unmapped, Unmapped{Project: p.Name, Target: "test",
				Reason: "its test configuration names no JUnit or coverage report: the result is the exit code alone"})
		}
	}
	slices.Sort(plan.disabled)
	return plan
}

func importReason(root, dir string, declared, isTS bool, task string, target detect.Target) string {
	runs := target.Executor
	if target.Command != "" && (runs == "" || runs == "nx:run-script" || runs == "nx:run-commands") {
		runs = target.Command
	}
	if !declared {
		return fmt.Sprintf("the workspace does not declare %s: %s keeps running through nx run", TypeScriptExtension, task)
	}
	if !isTS {
		return fmt.Sprintf("the project is no TypeScript project (package.json, and a tsconfig*.json or a .ts source): %s keeps running through nx run", task)
	}
	if strings.ContainsAny(runs, "&|;") {
		return fmt.Sprintf("its script chains another command (%q), which @putnami/typescript does not run: %s keeps running through nx run", runs, task)
	}
	if task == "test" && stacks.TestArgumentsDropped(runs) != "" {
		return fmt.Sprintf("its test runner takes arguments (%q) that @putnami/typescript would drop: move them to the runner's configuration; %s keeps running through nx run", stacks.TestArgumentsDropped(runs), task)
	}
	keeps := task + " keeps running through nx run"
	if task == "lint" && target.Executor == eslintExecutor {
		return eslintExecutorReason(root, dir, keeps)
	}
	if task == "lint" && strings.Contains(runs, "eslint") {
		return stacks.ESLintArgumentsReason(runs, keeps)
	}
	return fmt.Sprintf("@putnami/typescript runs %s, not %q: %s keeps running through nx run", strings.Join(typeScriptCommands[task], " or "), runs, task)
}

// nodeExecutor is the Nx executor that serves a project with node.
const nodeExecutor = "@nx/js:node"

// nodeRuntimeOption is the member of options["@putnami/typescript"] that
// names the runtime of a project's workload.
const nodeRuntimeOption = "runtime"

// startScriptRunsNode reports whether the start script of the package.json in
// dir runs node: its first word, after any NAME=value assignments, is node.
func startScriptRunsNode(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // a member manifest under the workspace root
	if err != nil {
		return false
	}
	var m struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	for _, word := range strings.Fields(m.Scripts["start"]) {
		if strings.Contains(word, "=") && !strings.HasPrefix(word, "-") {
			continue
		}
		return word == "node"
	}
	return false
}

// markNodeRuntime sets options["@putnami/typescript"].runtime to node in a
// putnami.json document that declares no runtime, and keeps any runtime it
// declares.
func markNodeRuntime(doc map[string]any) {
	options, _ := doc["options"].(map[string]any)
	if options == nil {
		options = map[string]any{}
		doc["options"] = options
	}
	layer, _ := options[TypeScriptExtension].(map[string]any)
	if layer == nil {
		layer = map[string]any{}
		options[TypeScriptExtension] = layer
	}
	if _, declared := layer[nodeRuntimeOption]; !declared {
		layer[nodeRuntimeOption] = "node"
	}
}

// isTypeScriptProject reports whether a project directory holds package.json
// and a tsconfig*.json, or a .ts source in it or in its src directory.
func isTypeScriptProject(root, dir string) bool {
	if !hasManifest(root, dir) {
		return false
	}
	abs := filepath.Join(root, filepath.FromSlash(dir))
	for _, pattern := range []string{"tsconfig*.json", "*.ts", filepath.Join("src", "*.ts")} {
		if matches, _ := filepath.Glob(filepath.Join(abs, pattern)); len(matches) > 0 {
			return true
		}
	}
	return false
}

func hasManifest(root, dir string) bool {
	return exists(root, path.Join(dir, "package.json"))
}

// typeScriptCommands are the programs the TypeScript extension's task of the
// same name runs (ADR 0017 of tooling/stacks): a test target that runs
// Vitest or Jest keeps its runner, a lint target that runs ESLint moves only
// as bare as stacks.ESLintScripts says, and a build that runs tsc alone moves
// too (tscBuild). Another tsc target, or one another Nx executor runs, stays
// on Nx.
var typeScriptCommands = stacks.TypeScriptPrograms

// testExecutors are the Nx executors of a test target the TypeScript
// extension runs, with the runner each one starts.
var testExecutors = map[string]string{
	"@nx/vite:test":   "vitest",
	"@nx/vitest:test": "vitest",
	"@nrwl/vite:test": "vitest",
	"@nx/jest:jest":   "jest",
	"@nrwl/jest:jest": "jest",
}

// eslintExecutor is the Nx executor that lints with ESLint, which the
// TypeScript extension's lint task runs from the project's own configuration.
const eslintExecutor = "@nx/eslint:lint"

// coveredByTypeScript reports whether the TypeScript extension's task runs
// what a target of the project at dir runs: a script or a command whose
// program is the one that task runs, with no other command chained to it, a
// test executor of testExecutors, or for lint a bare ESLint (lintsWithESLint).
func coveredByTypeScript(root, dir, task string, target detect.Target) bool {
	if task == "lint" && lintsWithESLint(root, dir, target) {
		return true
	}
	switch target.Executor {
	case "", "nx:run-script", "nx:run-commands":
	default:
		return task == "test" && testExecutors[target.Executor] != ""
	}
	command := strings.TrimSpace(target.Command)
	if task == "lint" && strings.Contains(command, "eslint") {
		return false
	}
	if task == "test" && stacks.TestArgumentsDropped(command) != "" {
		// The extension passes none of the script's arguments: one that
		// chooses a configuration or a subset changes what the runner checks.
		return false
	}
	return runsOnly(command, typeScriptCommands[task])
}

// lintsWithESLint reports whether a lint target runs ESLint the way the
// TypeScript lint does: a command in stacks.ESLintScripts, or the ESLint executor
// with no option but lintFilePatterns naming the project root.
func lintsWithESLint(root, dir string, target detect.Target) bool {
	switch target.Executor {
	case eslintExecutor:
		dropped, read := eslintExecutorDropped(root, dir)
		return read && len(dropped) == 0
	case "", "nx:run-script", "nx:run-commands":
		return slices.Contains(stacks.ESLintScripts, strings.Join(strings.Fields(target.Command), " "))
	}
	return false
}

// eslintExecutorDropped names, sorted, the options of the lint target of the
// project at dir that the TypeScript lint would drop: every option but
// lintFilePatterns that name the project root, and configurations, whose
// options a default configuration may set. It reads the target in the
// project's project.json, else the nx member of its package.json, under the
// targetDefaults of nx.json for lint and for the ESLint executor. read is
// false when it cannot find or read the target: the caller fails closed.
func eslintExecutorDropped(root, dir string) (dropped []string, read bool) {
	target, ok := lintTarget(root, dir)
	if !ok {
		return nil, false
	}
	defaults, ok := lintTargetDefaults(root)
	if !ok {
		return nil, false
	}
	options := map[string]any{}
	for _, layer := range append(defaults, target) {
		if len(layer.Configurations) > 0 && !slices.Contains(dropped, "configurations") {
			dropped = append(dropped, "configurations")
		}
		for name, value := range layer.Options {
			options[name] = value
		}
	}
	for name, value := range options {
		if name != "lintFilePatterns" || !patternsNameTheProject(value, dir) {
			dropped = append(dropped, name)
		}
	}
	slices.Sort(dropped)
	return dropped, true
}

// patternsNameTheProject reports whether lintFilePatterns name the project
// root alone, which the TypeScript lint lints anyway.
func patternsNameTheProject(value any, dir string) bool {
	patterns, _ := value.([]any)
	for _, pattern := range patterns {
		switch pattern {
		case "{projectRoot}", ".", "./", dir, "./" + dir:
		default:
			return false
		}
	}
	return true
}

// eslintExecutorReason says why an ESLint executor target stays on Nx, ending
// with keeps: it names the options the TypeScript lint would drop.
func eslintExecutorReason(root, dir, keeps string) string {
	dropped, read := eslintExecutorDropped(root, dir)
	if !read {
		return fmt.Sprintf("its ESLint executor's options cannot be read, and @putnami/typescript would drop any: %s", keeps)
	}
	return fmt.Sprintf("its ESLint executor sets options (%s) that @putnami/typescript would drop: move them to the ESLint configuration; %s",
		strings.Join(dropped, ", "), keeps)
}

// nxTargetConfig is the part of an Nx target configuration the ESLint
// mapping reads.
type nxTargetConfig struct {
	Options        map[string]any `json:"options"`
	Configurations map[string]any `json:"configurations"`
}

// lintTarget reads the lint target of the project at dir, in its
// project.json, else the nx member of its package.json. It answers false when
// neither holds one or a file cannot be read.
func lintTarget(root, dir string) (nxTargetConfig, bool) {
	type targets struct {
		Targets map[string]nxTargetConfig `json:"targets"`
	}
	abs := filepath.Join(root, filepath.FromSlash(dir))
	if data, err := os.ReadFile(filepath.Join(abs, "project.json")); err == nil { //nolint:gosec // a project file
		var project targets
		if json.Unmarshal(data, &project) != nil {
			return nxTargetConfig{}, false
		}
		if t, ok := project.Targets["lint"]; ok {
			return t, true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nxTargetConfig{}, false
	}
	data, err := os.ReadFile(filepath.Join(abs, "package.json")) //nolint:gosec // a project file
	if err != nil {
		return nxTargetConfig{}, false
	}
	var pkg struct {
		Nx *targets `json:"nx"`
	}
	if json.Unmarshal(data, &pkg) != nil || pkg.Nx == nil {
		return nxTargetConfig{}, false
	}
	t, ok := pkg.Nx.Targets["lint"]
	return t, ok
}

// lintTargetDefaults reads the targetDefaults of nx.json that apply to an
// ESLint lint target: by executor, then by target name, which Nx applies
// over it. A workspace without nx.json has none; one that cannot be read
// answers false.
func lintTargetDefaults(root string) ([]nxTargetConfig, bool) {
	data, err := os.ReadFile(filepath.Join(root, "nx.json")) //nolint:gosec // the workspace's nx.json
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	var nx struct {
		TargetDefaults map[string]nxTargetConfig `json:"targetDefaults"`
	}
	if json.Unmarshal(data, &nx) != nil {
		return nil, false
	}
	var layers []nxTargetConfig
	for _, key := range []string{eslintExecutor, "lint"} {
		if layer, ok := nx.TargetDefaults[key]; ok {
			layers = append(layers, layer)
		}
	}
	return layers, true
}

// runsOnly reports whether command runs one of programs and nothing else: a
// command that chains or pipes another, such as eslint . && prettier --check .,
// runs a tool the TypeScript extension does not.
func runsOnly(command string, programs []string) bool {
	command = strings.TrimSpace(command)
	if strings.ContainsAny(command, "&|;") {
		return false
	}
	for _, program := range programs {
		if command == program || strings.HasPrefix(command, program+" ") {
			return true
		}
	}
	return false
}

// testRunner is the runner of a test target the TypeScript extension runs:
// vitest or jest, or "" for bun test.
func testRunner(target detect.Target) string {
	if runner := testExecutors[target.Executor]; runner != "" {
		return runner
	}
	return stacks.TestRunner(target.Command)
}

// testConfigs are the test runner configuration files a project's reports
// are named in.
var testConfigs = []string{
	"jest.config.js", "jest.config.ts", "jest.config.cjs", "jest.config.mjs",
	"vite.config.ts", "vite.config.js", "vite.config.mts", "vitest.config.ts", "vitest.config.js", "vitest.config.mts",
}

// rawConfigString matches a string member of a test configuration by key,
// as written.
func rawConfigString(text, key string) string {
	m := regexp.MustCompile(`\b` + key + `\s*:\s*['"]([^'"]+)['"]`).FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}

// configString is rawConfigString with jest's <rootDir>, the directory of its
// configuration, read as the project directory.
func configString(text, key string) string {
	value := strings.ReplaceAll(rawConfigString(text, key), "<rootDir>/", "")
	return strings.ReplaceAll(value, "<rootDir>", ".")
}

// safeReportPath reports whether a report path stays under the directory it
// is read from and names no placeholder the sync does not resolve.
func safeReportPath(file string) bool {
	return file != ".." && !strings.HasPrefix(file, "../") && !path.IsAbs(file) && !strings.Contains(file, "<")
}

// testReports is the reports override of a project's test task: the JUnit
// report and the Cobertura or LCOV report its jest or vitest configuration
// names, relative to the project directory. A jest-junit reporter writes
// outputDirectory/outputName, junit.xml by default; vitest's junit reporter
// writes outputFile. A cobertura or lcov coverage reporter writes its file
// under coverageDirectory, or vitest's coverage.reportsDirectory, coverage by
// default. nil when the configuration names neither.
func testReports(root, dir string, target detect.Target) map[string]any {
	var text string
	for _, name := range testConfigs {
		if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), name)); err == nil { //nolint:gosec // a project file
			text = string(data)
			break
		}
	}
	reports := map[string]any{}
	if strings.Contains(text, "junit") {
		file := ""
		if strings.Contains(text, "jest-junit") {
			rawDir := rawConfigString(text, "outputDirectory")
			outDir, outName := configString(text, "outputDirectory"), configString(text, "outputName")
			if outDir == "" {
				outDir = "."
			}
			if outName == "" {
				outName = "junit.xml"
			}
			file = path.Join(outDir, outName)
			// Under @nx/jest, jest runs in the workspace root, where
			// jest-junit resolves an outputDirectory that does not start
			// from <rootDir>.
			nxJest := strings.HasPrefix(target.Executor, "@nx/jest:") || strings.HasPrefix(target.Executor, "@nrwl/jest:")
			if nxJest && !strings.Contains(rawDir, "<rootDir>") {
				if safeReportPath(file) {
					reports["junit"] = []any{"{root}/" + file}
				}
				file = ""
			}
		} else if out := configString(text, "junit"); out != "" {
			file = path.Clean(out)
		} else if out := configString(text, "outputFile"); out != "" {
			file = path.Clean(out)
		}
		if file != "" && safeReportPath(file) {
			reports["junit"] = []any{file}
		}
	}
	format, name := "", ""
	switch {
	case strings.Contains(text, "cobertura"):
		format, name = "cobertura", "cobertura-coverage.xml"
	case strings.Contains(text, "lcov"):
		format, name = "lcov", "lcov.info"
	}
	if format != "" {
		covDir := configString(text, "coverageDirectory")
		if covDir == "" {
			covDir = configString(text, "reportsDirectory")
		}
		if covDir == "" {
			covDir = "coverage"
		}
		file := path.Join(covDir, name)
		if safeReportPath(file) {
			reports["coverage"] = []any{map[string]any{"format": format, "paths": []any{file}}}
		}
	}
	if len(reports) == 0 {
		return nil
	}
	return reports
}

// skippedDirs are never walked for project files: build outputs, installed
// packages and caches.
var skippedDirs = map[string]bool{"dist": true, "node_modules": true}

// markedProjects lists, sorted, the workspace-relative directories under root
// whose putnami.json names the extension. It skips hidden directories, dist
// and node_modules.
func markedProjects(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if p != root && (strings.HasPrefix(entry.Name(), ".") || skippedDirs[entry.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != wsproto.ConfigFilename {
			return nil
		}
		doc, _, err := readProject(p)
		if err != nil || !slices.Contains(stringList(doc["extensions"]), ExtensionName) {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(out)
	return out, err
}

// ownedJobs are the disable.jobs entries the sync owns.
func ownedJobs() []string {
	var out []string
	for _, task := range lifecycleTasks {
		out = append(out, task, ExtensionName+":"+task, TypeScriptExtension+":"+task)
	}
	return out
}

// writeProject merges the project's Nx facts into the putnami.json at the
// workspace-relative file and writes it when a member the sync owns changes:
// the extension, @putnami/typescript for an imported TypeScript project, the
// dependencies on Nx projects, the entries of disable.jobs it owns,
// options["@putnami/nx"].tasks.test.reports,
// options["@putnami/typescript:test"].runner, an eslint
// options["@putnami/typescript"].lint.linter, and
// options["@putnami/typescript"].compiler as setCompiler owns it. Every other
// member is kept as it is. It returns the change, or nil when the file is
// current.
func writeProject(root, file string, plan projectPlan, components map[string]bool, dry bool) (*Change, error) {
	abs := filepath.Join(root, filepath.FromSlash(file))
	doc, existing, err := readProject(abs)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		doc["$schema"] = ProjectSchema
	}
	before := clone(doc)

	extensions := stringList(doc["extensions"])
	if !slices.Contains(extensions, ExtensionName) {
		extensions = append(extensions, ExtensionName)
	}
	hasTS := slices.Contains(extensions, TypeScriptExtension)
	switch {
	case plan.typeScript && !hasTS:
		extensions = append(extensions, TypeScriptExtension)
	case !plan.typeScript && hasTS:
		extensions = slices.DeleteFunc(extensions, func(e string) bool { return e == TypeScriptExtension })
	}
	doc["extensions"] = extensions

	kept := slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !keepDependency(root, d, components) })
	setList(doc, "dependencies", sortedUnique(append(kept, plan.deps...)))

	setDisabled(doc, plan.disabled)
	setReports(doc, plan.reports)
	stacks.SetTypeScriptRunner(doc, plan.runner)
	setCompiler(doc, plan.typeScript && plan.compiler)
	stacks.SetTypeScriptLinter(doc, plan.eslint)
	if plan.nodeRuntime {
		markNodeRuntime(doc)
	}

	if existing != nil && reflect.DeepEqual(clone(doc), before) {
		return nil, nil
	}
	return write(abs, file, doc, existing, dry)
}

// retractProject removes from the putnami.json of a former project the
// extension, @putnami/typescript, its Nx edges, the disable.jobs entries, the
// reports, the test runner and the eslint linter the sync owns.
func retractProject(root, file string, components map[string]bool, dry bool) (*Change, error) {
	abs := filepath.Join(root, filepath.FromSlash(file))
	doc, existing, err := readProject(abs)
	if err != nil {
		return nil, err
	}
	before := clone(doc)
	setList(doc, "extensions", slices.DeleteFunc(stringList(doc["extensions"]), func(e string) bool { return e == ExtensionName || e == TypeScriptExtension }))
	setList(doc, "dependencies", slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !keepDependency(root, d, components) }))
	setDisabled(doc, nil)
	setReports(doc, nil)
	stacks.SetTypeScriptRunner(doc, "")
	setCompiler(doc, false)
	stacks.SetTypeScriptLinter(doc, false)
	if reflect.DeepEqual(clone(doc), before) {
		return nil, nil
	}
	return write(abs, file, doc, existing, dry)
}

func write(abs, file string, doc map[string]any, existing []byte, dry bool) (*Change, error) {
	data, err := encode(doc)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(data, existing) {
		return nil, nil
	}
	change := &Change{File: file, Before: existing, After: data}
	if dry {
		return change, nil
	}
	return change, replaceFile(abs, data)
}

// setDisabled replaces the entries of disable.jobs the sync owns with jobs,
// and removes disable when nothing is left in it.
func setDisabled(doc map[string]any, jobs []string) {
	disable, _ := doc["disable"].(map[string]any)
	if disable == nil {
		disable = map[string]any{}
	}
	owned := ownedJobs()
	kept := slices.DeleteFunc(stringList(disable["jobs"]), func(j string) bool { return slices.Contains(owned, j) })
	setList(disable, "jobs", append(kept, jobs...))
	if len(disable) == 0 {
		delete(doc, "disable")
	} else {
		doc["disable"] = disable
	}
}

// setReports writes options["@putnami/nx"].tasks.test.reports, and removes
// each layer left empty.
func setReports(doc map[string]any, reports map[string]any) {
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[ExtensionName].(map[string]any)
	tasks, _ := layer["tasks"].(map[string]any)
	test, _ := tasks["test"].(map[string]any)
	if reports != nil {
		if options == nil {
			options = map[string]any{}
			doc["options"] = options
		}
		if layer == nil {
			layer = map[string]any{}
			options[ExtensionName] = layer
		}
		if tasks == nil {
			tasks = map[string]any{}
			layer["tasks"] = tasks
		}
		if test == nil {
			test = map[string]any{}
			tasks["test"] = test
		}
		test["reports"] = reports
		return
	}
	if test == nil {
		return
	}
	delete(test, "reports")
	if len(test) == 0 {
		delete(tasks, "test")
	}
	if len(tasks) == 0 {
		delete(layer, "tasks")
	}
	if len(layer) == 0 {
		delete(options, ExtensionName)
	}
	if len(options) == 0 {
		delete(doc, "options")
	}
}

// keepDependency reports whether the sync keeps a dependency it does not
// write: one on a project that is no Nx project, current or former.
func keepDependency(root, dep string, components map[string]bool) bool {
	if components[dep] {
		return false
	}
	if rel, ok := strings.CutPrefix(dep, "/"); ok {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// readProject reads the putnami.json at file as a JSON object, with its
// bytes; a missing file reads as an empty object and nil bytes.
func readProject(file string) (map[string]any, []byte, error) {
	doc := map[string]any{}
	existing, err := os.ReadFile(file) //nolint:gosec // a project file under the workspace root
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(existing))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("not a JSON object: %w", err)
	}
	return doc, existing, nil
}

// clone deep-copies a decoded JSON object, so the members the sync owns can be
// compared before and after it edits them.
func clone(doc map[string]any) map[string]any {
	data, _ := json.Marshal(doc)
	out := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

// encode is doc two-space indented with sorted keys.
func encode(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// replaceFile replaces file through a temporary file beside it.
func replaceFile(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil { //nolint:gosec // a project directory
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+wsproto.ConfigFilename+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644) //nolint:gosec // a tracked project file
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), file)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}

// stringList reads a JSON array of strings; any other value reads as empty.
func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func setList(doc map[string]any, key string, list []string) {
	if len(list) == 0 {
		delete(doc, key)
		return
	}
	doc[key] = list
}

func sortedUnique(list []string) []string {
	slices.Sort(list)
	return slices.Compact(list)
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// RunSync is the job of the workspace sync task: Sync on the workspace root
// in the mode of the mode parameter, else the recorded one, reading the
// extensions of the declaredExtensions parameter as declared. With the dryRun
// parameter it writes nothing and prints the diff of each file it would
// change.
func RunSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	dry := ctx.Params.Bool("dryRun", false)
	mode := ctx.Params.String(ModeOption)
	declared, err := stringsParam(ctx.Params, DeclaredParam)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	imports, importGiven, err := stacks.ImportParam(ctx.Params[stacks.ImportOption])
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	result, err := Sync(context.Background(), ctx.WorkspaceRoot, SyncOptions{Mode: mode, DryRun: dry, Declared: declared, Import: imports, ImportGiven: importGiven})
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	if result.Source == "" {
		emit.Info("no nx.json at the workspace root: no Nx project to synchronize")
		return "OK", nil, nil
	}
	report(emit, result, dry)
	return "OK", map[string]any{"mode": result.Mode, "written": result.Written, "retracted": result.Retracted, "unmapped": result.Unmapped, "imported": result.Imported, "dryRun": dry}, nil
}

// stringsParam reads a parameter that is a list of strings: a JSON array, or
// a comma-separated string, as a flag gives it. An absent parameter reads as
// empty.
func stringsParam(params pctx.Params, key string) ([]string, error) {
	raw, ok := params[key]
	if !ok {
		return nil, nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list, nil
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return nil, fmt.Errorf("the %s parameter is %s, not a list of extension names", key, raw)
	}
	for _, name := range strings.Split(text, ",") {
		if name = strings.TrimSpace(name); name != "" {
			list = append(list, name)
		}
	}
	return list, nil
}

// report prints the summary, each unmapped target as a warning and, in a dry
// run, each change as a warning that carries its diff, so the run's output
// shows them.
func report(emit *jsonl.Emitter, result SyncResult, dry bool) {
	emit.Info(syncSummary(result, dry))
	for _, u := range result.Unmapped {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("%s:%s %s", u.Project, u.Target, u.Reason), "", 0, 0, CodeUnmapped)
	}
	reportUnmatched(emit, result)
	for _, note := range result.Conventions {
		emit.Diagnostic("warning", note, "", 0)
	}
	if !dry {
		return
	}
	for _, c := range result.Changes {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("%s would change (%s mode):\n%s", c.File, result.Mode, strings.TrimSuffix(Diff(c.File, c.Before, c.After), "\n")), c.File, 0, 0, CodeSyncPreview)
	}
}

// reportUnmatched warns of each entry of the import list that selects no
// project: a typo, or a project not created yet.
func reportUnmatched(emit *jsonl.Emitter, result SyncResult) {
	for _, entry := range result.Unmatched {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("options[%q].%s lists %q, which selects no Nx project of %s", ExtensionName, stacks.ImportOption, entry, result.Source), wsproto.WorkspaceConfigFilename, 0, 0, CodeImportUnmatched)
	}
}

func syncSummary(r SyncResult, dry bool) string {
	if r.EmptyGraph {
		return fmt.Sprintf("%s lists no project, so no putnami.json changes", r.Source)
	}
	verb := "written"
	if dry {
		verb = "would change"
	}
	mode := r.Mode + " mode"
	if len(r.Imported) > 0 {
		mode += fmt.Sprintf(" with %d imported (%s)", len(r.Imported), strings.Join(r.Imported, ", "))
	}
	return fmt.Sprintf("the Nx project graph of %s, %s: %d putnami.json %s, %d current, %d former projects retracted, %d targets unmapped",
		r.Source, mode, len(r.Written), verb, r.Unchanged, len(r.Retracted), len(r.Unmapped))
}

// RunCheck is the job of the validate task: it fails when a sync in the
// recorded mode would change a putnami.json, so a source change that altered
// the edges Nx infers cannot pass a gate that selected the projects on the
// old ones.
func RunCheck(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	result, err := Stale(context.Background(), ctx.WorkspaceRoot)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	reportUnmatched(emit, result)
	stale := append(slices.Clone(result.Written), result.Retracted...)
	if len(stale) == 0 {
		return "OK", nil, nil
	}
	for _, file := range stale {
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s no longer matches the Nx project graph of %s: run putnami projects sync and commit the result", file, result.Source), file, 0, 0, CodeSyncStale)
	}
	return "FAILED", nil, errors.New(CodeSyncStale)
}
