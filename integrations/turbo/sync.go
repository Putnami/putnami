package turbo

import (
	"encoding/json"
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
	// Conventions list each file an import rewrote to adopt Putnami's
	// TypeScript conventions, and each manifest it did not (D-066).
	Conventions []string
	// Notes say how Putnami runs a task that moved differently from
	// Turborepo, such as the projects --impacted lints.
	Notes []string
	// TurboMissing reports a package that delegates a task while no turbo
	// is reachable, in node_modules/.bin at the workspace root or on PATH:
	// that task fails with stacks.toolchain_missing.
	TurboMissing bool
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
	// nodeRuntime reports an imported TypeScript package whose start script
	// runs node: its putnami.json declares the node runtime.
	nodeRuntime bool
	// testMoved reports a test script that moved, which type-checked
	// nothing; lintChecks a lint script that moved and passes no write flag.
	testMoved, lintChecks bool
	// order is what each task that moved waits for, from turbo.json's
	// dependsOn (movedOrder).
	order map[string][]string
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
	rootOrder, err := readTaskOrder(root)
	if err != nil {
		return Result{}, err
	}
	unmapped := map[string]string{}
	noteUnmapped(rootTasks, unmapped)

	var members []member
	var conventions []stacks.ConventionProject
	lintMoved, declare := false, false
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
		ownOrder, err := readTaskOrder(dir)
		if err != nil {
			return Result{}, err
		}
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
		for _, dep := range p.Dependencies {
			if dep != wsproto.ProbeRootPath {
				m.deps = append(m.deps, wsproto.ProjectIDFromPath(dep))
			}
		}
		bunBuild, moved, biomeLint := false, false, false
		published := stacks.PackagePublished(root, p.Path)
		for _, task := range MappedTasks {
			declared := rootTasks[task] || rootTasks[p.SourceName+"#"+task] || own[task]
			script, scripted := run[task]
			kept := ""
			if imported && task == stacks.TaskBuild && stacks.TypeScriptBuildsWithBun(script) {
				kept = stacks.BunBuildKeptReason(root, p.Path, published, keeps(task))
			}
			switch {
			case !scripted || !declared:
				m.disabled = append(m.disabled, task)
			case !imported:
			case kept != "":
				m.disabled = append(m.disabled, TypeScriptExtension+":"+task)
				unmapped[p.SourceName+"#"+task] = kept
			case stacks.TypeScriptRuns(task, script):
				// One extension runs each task: the TypeScript extension's
				// task already runs what the script runs.
				m.disabled = append(m.disabled, ExtensionName+":"+task)
				moved = true
				m.order = movedOrder(m.order, task, p.SourceName, rootOrder, ownOrder, &result.Notes)
				bunBuild = bunBuild || (task == stacks.TaskBuild && stacks.TypeScriptBuildsWithBun(script))
				m.testMoved = m.testMoved || task == stacks.TaskTest
				if task == stacks.TaskLint {
					lintMoved = true
					m.lintChecks = !stacks.LintWrites(script)
					biomeLint = !stacks.LintsWithESLint(script)
				}
			default:
				m.disabled = append(m.disabled, TypeScriptExtension+":"+task)
				unmapped[p.SourceName+"#"+task] = unmappedReason(task, script)
			}
		}
		// An imported package names @putnami/typescript only when one of its
		// tasks moves (ADR 0017 of tooling/stacks): one that moves none runs
		// on Turborepo alone, as a delegated package does.
		if moved {
			m.extensions = append(m.extensions, TypeScriptExtension)
			m.nodeRuntime = startScriptRunsNode(dir)
			conventions = append(conventions, stacks.ConventionProject{Dir: p.Path, Name: p.SourceName, BunBuild: bunBuild, PublishedBy: published, BiomeLint: biomeLint})
			declare = true
			if mode == ModeDelegate {
				result.Imported = append(result.Imported, p.Path)
			}
		} else {
			m.disabled = slices.DeleteFunc(m.disabled, func(job string) bool { return strings.HasPrefix(job, TypeScriptExtension+":") })
		}
		members = append(members, m)
		current[p.Path] = true
	}
	for task, reason := range unmapped {
		result.Unmapped = append(result.Unmapped, Unmapped{Task: task, Reason: reason})
	}
	sort.Slice(result.Unmapped, func(i, j int) bool { return result.Unmapped[i].Task < result.Unmapped[j].Task })
	if lintMoved {
		result.Notes = append(result.Notes, stacks.LintDependentsNote)
	}
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

	delegates := false
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
		delegates = delegates || m.delegates()
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
	// The convention writes come after the putnami.json files, whose diffs
	// Diffs holds first, in the order of Stale.
	writes, warnings, err := stacks.ImportConventions(root, conventions)
	if err != nil {
		return Result{}, err
	}
	_, notes, err := stacks.ApplyConventions(root, writes, opts.DryRun)
	if err != nil {
		return Result{}, err
	}
	for _, w := range writes {
		result.Diffs = append(result.Diffs, UnifiedDiff(w.File, w.Before, w.After))
	}
	result.Conventions = append(append(result.Conventions, notes...), warnings...)
	result.TurboMissing = delegates && !turboReachable(root)
	// The workspace declares @putnami/typescript only when a task moves to
	// it, so an import that moves none installs nothing.
	if declare {
		changed, change, err := stacks.DeclareExtensions(root, []string{TypeScriptExtension}, opts.DryRun)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", wsproto.WorkspaceConfigFilename, err)
		}
		if changed {
			result.Written = append(result.Written, wsproto.WorkspaceConfigFilename)
		}
		if change != nil {
			result.Diffs = append(result.Diffs, UnifiedDiff(change.File, change.Before, change.After))
		}
	}
	return result, nil
}

// movedOrder adds to order what the task of the package named name waits for,
// once it moved to @putnami/typescript: the dependsOn turbo.json gives it, the
// package's own turbo.json first, then the root's name#task, then the root's
// task, the first that states one giving it and $TURBO_EXTENDS$ standing for
// the next one's (resolveOrder). ^build, ^test and ^lint keep their form, and
// build, test or lint of the package itself are its own. Every other entry, a
// package task, another task or an input, is noted and not written.
func movedOrder(order map[string][]string, task, name string, rootOrder, ownOrder TaskOrder, notes *[]string) map[string][]string {
	own, ownStated := ownOrder[task]
	specific, specificStated := rootOrder[name+"#"+task]
	shared, sharedStated := rootOrder[task]
	deps := resolveOrder([][]string{own, specific, shared}, []bool{ownStated, specificStated, sharedStated})
	for _, dep := range deps {
		target := strings.TrimPrefix(dep, "^")
		if !slices.Contains(MappedTasks, target) || (target == task && !strings.HasPrefix(dep, "^")) {
			*notes = append(*notes, fmt.Sprintf("not written: %s#%s depends on %s in turbo.json, which no Putnami command of the package maps; the task that moved to %s does not wait for it", name, task, dep, TypeScriptExtension))
			continue
		}
		if order == nil {
			order = map[string][]string{}
		}
		stacks.AddCommandOrder(order, task, dep)
	}
	return order
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
	if m.nodeRuntime {
		markNodeRuntime(doc)
	}
	stacks.SetTypeScriptTestTypesOff(doc, m.testMoved)
	stacks.SetTypeScriptLintFixOff(doc, m.lintChecks)
	stacks.SetCommandOrder(doc, m.order)
}

// retract removes from the putnami.json of a former package what the sync
// wrote: the extensions, the edges to packages, the tag and the disabled
// tasks.
func retract(root string, doc map[string]any, owned map[string]bool) {
	setList(doc, "extensions", slices.DeleteFunc(stringList(doc["extensions"]), func(e string) bool { return slices.Contains(ownedExtensions, e) }))
	setList(doc, "dependencies", slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !keepDependency(root, d, owned) }))
	setList(doc, "tags", slices.DeleteFunc(stringList(doc["tags"]), func(t string) bool { return t == Tag }))
	stacks.SetTypeScriptTestTypesOff(doc, false)
	stacks.SetTypeScriptLintFixOff(doc, false)
	stacks.SetCommandOrder(doc, nil)
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

// delegates reports whether the package runs one of build, lint and test
// through Turborepo: its job is neither disabled nor moved.
func (m member) delegates() bool {
	return slices.ContainsFunc(MappedTasks, func(task string) bool {
		return !slices.Contains(m.disabled, task) && !slices.Contains(m.disabled, ExtensionName+":"+task)
	})
}

// turboReachable reports whether a delegated task finds turbo where it looks
// (ADR 0018 of the stack core): in the directories the profile's
// toolchain.path names under the workspace root, then on PATH.
func turboReachable(root string) bool {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return true
	}
	_, ok := stacks.LookPathIn(profile.Toolchain.TaskEnv(root, os.Environ()), profile.Toolchain.Command[0])
	return ok
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

// unmappedReason says why a script of a TypeScript package keeps running
// through Turborepo in import mode.
func unmappedReason(task, script string) string {
	return stacks.TypeScriptUnmappedReason(task, script, keeps(task))
}

// keeps says how a task that stays on Turborepo runs.
func keeps(task string) string {
	return fmt.Sprintf("it keeps running through Turborepo, with turbo run %s", task)
}

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
