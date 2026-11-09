package nx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	typeScript := declaresExtension(root, TypeScriptExtension)
	for _, p := range detection.Projects {
		plan := planProject(root, p, mode, typeScript)
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
	return result, nil
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
// extension, by name or by a path whose last segment is its language
// extension directory.
func declaresExtension(root, name string) bool {
	doc, err := readWorkspace(root)
	if err != nil || doc == nil {
		return false
	}
	for _, e := range doc.Extensions {
		if e == name || (name == TypeScriptExtension && strings.HasSuffix(e, "/typescript/extension")) {
			return true
		}
	}
	return false
}

// projectPlan is what the sync writes for one project.
type projectPlan struct {
	deps     []string
	disabled []string
	reports  map[string]any
	unmapped []Unmapped
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
	for _, task := range lifecycleTasks {
		target, ok := targets[task]
		switch {
		case !ok:
			plan.disabled = append(plan.disabled, task)
		case isTS && coveredByTypeScript(task, target):
			plan.disabled = append(plan.disabled, ExtensionName+":"+task)
		case typeScript && hasManifest(root, p.Path):
			// The TypeScript extension claims the project too: the target
			// runs through Nx alone.
			plan.disabled = append(plan.disabled, TypeScriptExtension+":"+task)
			if mode == ModeImport {
				plan.unmapped = append(plan.unmapped, Unmapped{Project: p.Name, Target: task, Reason: importReason(isTS, task, target)})
			}
		case mode == ModeImport:
			plan.unmapped = append(plan.unmapped, Unmapped{Project: p.Name, Target: task, Reason: importReason(isTS, task, target)})
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

func importReason(isTS bool, task string, target detect.Target) string {
	runs := target.Executor
	if target.Command != "" && (runs == "" || runs == "nx:run-script" || runs == "nx:run-commands") {
		runs = target.Command
	}
	if !isTS {
		return fmt.Sprintf("the project is no TypeScript project (package.json, and tsconfig.json or a .ts source): %s keeps running through nx run", task)
	}
	return fmt.Sprintf("@putnami/typescript has no counterpart for %q: %s keeps running through nx run", runs, task)
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

// typeScriptExecutors are the executors whose target the TypeScript
// extension's task of the same name covers.
var typeScriptExecutors = map[string][]string{
	"build": {"@nx/js:tsc", "@nrwl/js:tsc", "@nx/js:swc", "@nrwl/js:swc"},
	"test":  {"@nx/jest:jest", "@nrwl/jest:jest", "@nx/vite:test", "@nx/vitest:test"},
	"lint":  {"@nx/eslint:lint", "@nx/linter:eslint", "@nrwl/linter:eslint"},
}

// typeScriptCommands are the programs whose command the TypeScript
// extension's task of the same name covers.
var typeScriptCommands = map[string][]string{
	"build": {"tsc"},
	"test":  {"jest", "vitest", "bun test"},
	"lint":  {"eslint", "biome"},
}

// coveredByTypeScript reports whether the TypeScript extension's task covers
// a target: by its executor, or, for a script or a command, by the program
// its command runs.
func coveredByTypeScript(task string, target detect.Target) bool {
	switch target.Executor {
	case "", "nx:run-script", "nx:run-commands":
	default:
		return slices.Contains(typeScriptExecutors[task], target.Executor)
	}
	command := strings.TrimSpace(target.Command)
	for _, program := range typeScriptCommands[task] {
		if command == program || strings.HasPrefix(command, program+" ") {
			return true
		}
	}
	return false
}

// testConfigs are the test runner configuration files a project's reports
// are named in.
var testConfigs = []string{
	"jest.config.js", "jest.config.ts", "jest.config.cjs", "jest.config.mjs",
	"vite.config.ts", "vite.config.js", "vite.config.mts", "vitest.config.ts", "vitest.config.js", "vitest.config.mts",
}

// configString matches a string member of a test configuration by key.
func configString(text, key string) string {
	m := regexp.MustCompile(`\b` + key + `\s*:\s*['"]([^'"]+)['"]`).FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}

// testReports is the reports override of a project's test task: the JUnit
// report and the Cobertura or LCOV report its jest or vitest configuration
// names, relative to the project directory. A jest-junit reporter writes
// outputDirectory/outputName, junit.xml by default; vitest's junit reporter
// writes outputFile. A cobertura or lcov coverage reporter writes its file
// under coverageDirectory, or vitest's coverage.reportsDirectory, coverage by
// default. nil when the configuration names neither.
func testReports(root, dir string, target detect.Target) map[string]any {
	_ = target
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
			outDir, outName := configString(text, "outputDirectory"), configString(text, "outputName")
			if outDir == "" {
				outDir = "."
			}
			if outName == "" {
				outName = "junit.xml"
			}
			file = path.Join(outDir, outName)
		} else if out := configString(text, "junit"); out != "" {
			file = path.Clean(out)
		} else if out := configString(text, "outputFile"); out != "" {
			file = path.Clean(out)
		}
		if file != "" && !strings.HasPrefix(file, "../") && !path.IsAbs(file) {
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
		if !strings.HasPrefix(file, "../") && !path.IsAbs(file) {
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
// the extension, the dependencies on Nx projects, the entries of disable.jobs
// it owns and options["@putnami/nx"].tasks.test.reports. Every other member
// is kept as it is. It returns the change, or nil when the file is current.
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
	doc["extensions"] = extensions

	kept := slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !keepDependency(root, d, components) })
	setList(doc, "dependencies", sortedUnique(append(kept, plan.deps...)))

	setDisabled(doc, plan.disabled)
	setReports(doc, plan.reports)

	if existing != nil && reflect.DeepEqual(clone(doc), before) {
		return nil, nil
	}
	return write(abs, file, doc, existing, dry)
}

// retractProject removes from the putnami.json of a former project the
// extension, its Nx edges, the disable.jobs entries and the reports the sync
// owns.
func retractProject(root, file string, components map[string]bool, dry bool) (*Change, error) {
	abs := filepath.Join(root, filepath.FromSlash(file))
	doc, existing, err := readProject(abs)
	if err != nil {
		return nil, err
	}
	before := clone(doc)
	setList(doc, "extensions", slices.DeleteFunc(stringList(doc["extensions"]), func(e string) bool { return e == ExtensionName }))
	setList(doc, "dependencies", slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !keepDependency(root, d, components) }))
	setDisabled(doc, nil)
	setReports(doc, nil)
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
// in the mode of the mode parameter, else the recorded one. With the dryRun
// parameter it writes nothing and prints the diff of each file it would
// change.
func RunSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	dry := ctx.Params.Bool("dryRun", false)
	mode := ctx.Params.String(ModeOption)
	result, err := Sync(context.Background(), ctx.WorkspaceRoot, SyncOptions{Mode: mode, DryRun: dry})
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	if result.Source == "" {
		emit.Info("no nx.json at the workspace root: no Nx project to synchronize")
		return "OK", nil, nil
	}
	report(emit, os.Stderr, result, dry)
	return "OK", map[string]any{"mode": result.Mode, "written": result.Written, "retracted": result.Retracted, "unmapped": result.Unmapped, "dryRun": dry}, nil
}

// report prints the summary, each unmapped target and, in a dry run, the
// diff of each change.
func report(emit *jsonl.Emitter, out io.Writer, result SyncResult, dry bool) {
	emit.Info(syncSummary(result, dry))
	for _, u := range result.Unmapped {
		emit.DiagnosticWithCode("info", fmt.Sprintf("%s:%s %s", u.Project, u.Target, u.Reason), "", 0, 0, CodeUnmapped)
	}
	if !dry {
		return
	}
	for _, c := range result.Changes {
		_, _ = io.WriteString(out, Diff(c.File, c.Before, c.After))
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
	return fmt.Sprintf("the Nx project graph of %s, %s mode: %d putnami.json %s, %d current, %d former projects retracted, %d targets unmapped",
		r.Source, r.Mode, len(r.Written), verb, r.Unchanged, len(r.Retracted), len(r.Unmapped))
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
	stale := append(slices.Clone(result.Written), result.Retracted...)
	if len(stale) == 0 {
		return "OK", nil, nil
	}
	for _, file := range stale {
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s no longer matches the Nx project graph of %s: run putnami projects sync and commit the result", file, result.Source), file, 0, 0, CodeSyncStale)
	}
	return "FAILED", nil, errors.New(CodeSyncStale)
}
