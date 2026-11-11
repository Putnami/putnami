package turbo

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/tscscript"
	"go.putnami.dev/tooling/observe/detect/nodeworkspace"
	"go.putnami.dev/tooling/stacks"
)

// Tag marks the putnami.json of each package the sync writes, so a later sync
// finds the packages that left the workspace.
const Tag = "turbo"

// MappedTasks are the turbo.json tasks a Putnami task maps, in run order.
var MappedTasks = stacks.TaskNames

// ownedExtensions are the extensions the sync writes, and so removes.
var ownedExtensions = []string{ExtensionName, TypeScriptExtension}

// Options are the choices of one sync.
type Options struct {
	// Mode is the adoption mode; empty reads the workspace config.
	Mode string
	// DryRun computes the result and its diffs, and writes nothing.
	DryRun bool
	// Import lists, in delegate mode, the package paths and globs whose
	// packages the sync writes as import mode does, in place of the
	// workspace config when ImportGiven is set.
	Import      []string
	ImportGiven bool
}

// Unmapped is a turbo.json task no Putnami task maps.
type Unmapped struct {
	Task   string `json:"task"`
	Reason string `json:"reason"`
}

// Result is what one sync did, or would do.
type Result struct {
	// Repository is false at a root without turbo.json or Node workspace:
	// the sync did nothing.
	Repository bool
	// Mode is the adoption mode the sync wrote.
	Mode string
	// Written are the workspace-relative putnami.json files the sync created
	// or changed, sorted.
	Written []string
	// Retracted are the putnami.json files of former packages the sync
	// removed what it owns from, sorted.
	Retracted []string
	// Unchanged counts the files whose content was already current.
	Unchanged int
	// Diffs are the unified diffs of the written and retracted files, in
	// their order.
	Diffs []string
	// Unmapped are the turbo.json tasks that keep running through
	// Turborepo, sorted by task.
	Unmapped []Unmapped
	// Unreported are the directories of the delegated packages whose test
	// task names no JUnit report, sorted: their result is the exit code.
	Unreported []string
	// Imported are the directories of the TypeScript packages a delegated
	// workspace's import list moved to @putnami/typescript, sorted.
	Imported []string
	// Unmatched are the entries of the import list that select no package.
	Unmatched []string
}

// Stale reports the files a sync would change.
func (r Result) Stale() []string {
	return append(slices.Clone(r.Written), r.Retracted...)
}

// member is one package of the workspace, with what the sync writes for it.
type member struct {
	dir        string
	extensions []string
	deps       []string
	disabled   []string
}

// Sync writes the package graph of the Turborepo repository at root into each
// member's putnami.json, in the mode opts names or the workspace config
// records: the extension it runs, its edges to the other members as project
// IDs, the turbo tag, and in disable.jobs each of build, lint and test the
// package has no script or no turbo.json task for. Every other member of the
// file is kept. A package that left the workspace loses what the sync wrote.
func Sync(root string, opts Options) (Result, error) {
	var result Result
	if !isRepository(root) {
		return result, nil
	}
	detection, ok := nodeworkspace.Detect(root)
	if !ok {
		return result, nil
	}
	result.Repository = true
	mode := opts.Mode
	if mode == "" {
		var err error
		if mode, err = WorkspaceMode(root); err != nil {
			return Result{}, err
		}
	} else if _, err := checkMode(mode); err != nil {
		return Result{}, err
	}
	result.Mode = mode
	imports, err := importList(root, mode, opts)
	if err != nil {
		return Result{}, err
	}

	rootTasks, err := readTasks(root)
	if err != nil {
		return Result{}, err
	}
	unmapped := map[string]string{}
	noteUnmapped(rootTasks, unmapped)

	var members []member
	current := map[string]bool{}
	for _, p := range detection.Projects {
		if p.Path == wsproto.ProbeRootPath {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(p.Path))
		own, err := readTasks(dir)
		if err != nil {
			return Result{}, err
		}
		noteUnmapped(own, unmapped)
		run, err := scripts(dir)
		if err != nil {
			return Result{}, err
		}
		m := member{dir: p.Path, extensions: []string{ExtensionName}}
		packageMode := mode
		if imports.Selects(p.Path) {
			packageMode = ModeImport
		}
		imported := packageMode == ModeImport && isTypeScript(dir)
		if imported && mode == ModeDelegate {
			result.Imported = append(result.Imported, p.Path)
		}
		if imported {
			m.extensions = append(m.extensions, TypeScriptExtension)
		}
		for _, dep := range p.Dependencies {
			if dep != wsproto.ProbeRootPath {
				m.deps = append(m.deps, wsproto.ProjectIDFromPath(dep))
			}
		}
		for _, task := range MappedTasks {
			declared := rootTasks[task] || rootTasks[p.SourceName+"#"+task] || own[task]
			script, scripted := run[task]
			switch {
			case !scripted || !declared:
				m.disabled = append(m.disabled, task)
			case !imported:
			case runsTypeScriptTask(task, script):
				// One extension runs each task: the TypeScript extension's
				// task already runs what the script runs.
				m.disabled = append(m.disabled, ExtensionName+":"+task)
			default:
				m.disabled = append(m.disabled, TypeScriptExtension+":"+task)
				unmapped[p.SourceName+"#"+task] = fmt.Sprintf("%s runs %s, not %q: it keeps running through Turborepo, with turbo run %s",
					TypeScriptExtension, strings.Join(typeScriptCommands[task], " or "), script, task)
			}
		}
		members = append(members, m)
		current[p.Path] = true
	}
	for task, reason := range unmapped {
		result.Unmapped = append(result.Unmapped, Unmapped{Task: task, Reason: reason})
	}
	sort.Slice(result.Unmapped, func(i, j int) bool { return result.Unmapped[i].Task < result.Unmapped[j].Task })
	slices.Sort(result.Imported)
	result.Unmatched = imports.Unmatched()

	// The sync owns the edges between the packages it marks: the current
	// members, and the former ones whose putnami.json still carries the tag.
	former, err := markedProjects(root)
	if err != nil {
		return Result{}, err
	}
	owned := map[string]bool{}
	for dir := range current {
		owned[wsproto.ProjectIDFromPath(dir)] = true
	}
	for _, dir := range former {
		owned[wsproto.ProjectIDFromPath(dir)] = true
	}

	for _, m := range members {
		rel := path.Join(m.dir, wsproto.ConfigFilename)
		doc, existing, err := readProject(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", rel, err)
		}
		if existing == nil {
			doc["$schema"] = ProjectSchema
		}
		before := clone(doc)
		applyMember(root, doc, m, owned)
		if !slices.Contains(m.disabled, stacks.TaskTest) && !slices.Contains(m.disabled, ExtensionName+":"+stacks.TaskTest) && len(junitReports(doc)) == 0 {
			result.Unreported = append(result.Unreported, m.dir)
		}
		if existing != nil && reflect.DeepEqual(clone(doc), before) {
			result.Unchanged++
			continue
		}
		changed, err := commit(root, rel, doc, existing, opts.DryRun, &result)
		if err != nil {
			return Result{}, err
		}
		if changed {
			result.Written = append(result.Written, rel)
		} else {
			result.Unchanged++
		}
	}
	for _, dir := range former {
		if current[dir] {
			continue
		}
		rel := path.Join(dir, wsproto.ConfigFilename)
		doc, existing, err := readProject(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", rel, err)
		}
		retract(root, doc, owned)
		changed, err := commit(root, rel, doc, existing, opts.DryRun, &result)
		if err != nil {
			return Result{}, err
		}
		if changed {
			result.Retracted = append(result.Retracted, rel)
		}
	}
	return result, nil
}

// importList is the import list of a delegated workspace: opts.Import when
// given, else options["@putnami/turbo"].import. Import mode reads none.
func importList(root, mode string, opts Options) (*stacks.ImportList, error) {
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

// noteUnmapped records each task of tasks no Putnami task maps, with why.
func noteUnmapped(tasks TaskSet, into map[string]string) {
	for name := range tasks {
		pkg, task, scoped := strings.Cut(name, "#")
		if !scoped {
			task = name
		}
		if slices.Contains(MappedTasks, task) && pkg != "/" && pkg != "//" {
			continue
		}
		reason := fmt.Sprintf("no Putnami task maps it: it keeps running through Turborepo, with turbo run %s", task)
		if scoped && (pkg == "//" || pkg == "/") {
			reason = fmt.Sprintf("a root task belongs to no package: it keeps running through Turborepo, with turbo run %s", task)
		}
		into[name] = reason
	}
}

// applyMember merges what the sync owns for m into doc.
func applyMember(root string, doc map[string]any, m member, owned map[string]bool) {
	extensions := slices.DeleteFunc(stringList(doc["extensions"]), func(e string) bool { return slices.Contains(ownedExtensions, e) })
	setList(doc, "extensions", append(extensions, m.extensions...))

	kept := slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !keepDependency(root, d, owned) })
	setList(doc, "dependencies", sortedUnique(append(kept, m.deps...)))

	tags := stringList(doc["tags"])
	if !slices.Contains(tags, Tag) {
		tags = append(tags, Tag)
	}
	setList(doc, "tags", tags)

	disable, _ := doc["disable"].(map[string]any)
	if disable == nil {
		disable = map[string]any{}
	}
	jobs := slices.DeleteFunc(stringList(disable["jobs"]), ownedJob)
	setList(disable, "jobs", append(jobs, m.disabled...))
	if len(disable) == 0 {
		delete(doc, "disable")
	} else {
		doc["disable"] = disable
	}
}

// retract removes from the putnami.json of a former package what the sync
// wrote: the extensions, the edges to packages, the tag and the disabled
// tasks.
func retract(root string, doc map[string]any, owned map[string]bool) {
	setList(doc, "extensions", slices.DeleteFunc(stringList(doc["extensions"]), func(e string) bool { return slices.Contains(ownedExtensions, e) }))
	setList(doc, "dependencies", slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !keepDependency(root, d, owned) }))
	setList(doc, "tags", slices.DeleteFunc(stringList(doc["tags"]), func(t string) bool { return t == Tag }))
	if disable, ok := doc["disable"].(map[string]any); ok {
		setList(disable, "jobs", slices.DeleteFunc(stringList(disable["jobs"]), ownedJob))
		if len(disable) == 0 {
			delete(doc, "disable")
		}
	}
}

// keepDependency reports whether the sync keeps a dependency it does not
// write: one on a project that is no package it marks, and whose directory,
// for a project ID, still exists.
func keepDependency(root, dep string, owned map[string]bool) bool {
	if owned[dep] {
		return false
	}
	if rel, ok := strings.CutPrefix(dep, "/"); ok {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// commit writes doc at rel when its bytes differ from existing, or with
// dryRun records the diff alone. It reports whether the file changes.
func commit(root, rel string, doc map[string]any, existing []byte, dryRun bool, result *Result) (bool, error) {
	data, err := encodeProject(doc)
	if err != nil {
		return false, fmt.Errorf("%s: %w", rel, err)
	}
	if string(data) == string(existing) {
		return false, nil
	}
	result.Diffs = append(result.Diffs, UnifiedDiff(rel, existing, data))
	if dryRun {
		return true, nil
	}
	if err := replaceFile(filepath.Join(root, filepath.FromSlash(rel)), data); err != nil {
		return false, fmt.Errorf("%s: %w", rel, err)
	}
	return true, nil
}

// ownedJob reports whether the sync owns a disable.jobs entry: build, lint
// or test, bare or qualified by the extension that runs it.
func ownedJob(job string) bool {
	for _, task := range MappedTasks {
		if job == task || job == ExtensionName+":"+task || job == TypeScriptExtension+":"+task {
			return true
		}
	}
	return false
}

// typeScriptCommands are the programs the TypeScript extension's task of the
// same name runs. A script that runs one keeps its tool when the extension
// takes the task over, and so does a build script that runs tsc alone, which
// the extension compiles with that tsc. Any other tsc, vitest, jest or eslint
// script stays on Turborepo, because moving it would change the tool that
// checks the code.
var typeScriptCommands = map[string][]string{
	stacks.TaskBuild: {"bun build", "tsc alone"},
	stacks.TaskTest:  {"bun test"},
	stacks.TaskLint:  {"biome", "bunx biome", "bunx @biomejs/biome"},
}

// runsTypeScriptTask reports whether a package script runs what the
// TypeScript extension's task runs.
func runsTypeScriptTask(task, script string) bool {
	if task == stacks.TaskBuild && tscscript.Runs(script) {
		return true
	}
	script = strings.TrimSpace(script)
	for _, program := range typeScriptCommands[task] {
		if script == program || strings.HasPrefix(script, program+" ") {
			return true
		}
	}
	return false
}
