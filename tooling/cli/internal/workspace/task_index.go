package workspace

import (
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/goembed"
	"go.putnami.dev/tooling/cli/internal/extension"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The task index: which TASKS of a project read a path, and which tasks of a
// dependent one task reaches.
//
// It is the model's TaskImpactIndex (ADR 0044) built here, where the extension
// manifests are loaded, so `tooling/cli-model` keeps its rule of reading no
// manifest. Everything it answers comes from the declarations the build cache
// already keys on: a task's `inputs` ports through the protocol's own
// DeriveTaskCacheKey, a project's `options.<layer>.filePatterns`, its
// `options.generate.assets`, and the `^` step references the planner resolves
// in resolveExternalDeps.
//
// It is deliberately EXTENSION-AGNOSTIC: one `<command>~<step>` entry merges
// what every loaded manifest declares for that command and step, and no
// activation is evaluated. That can only widen an answer — a Go project's
// `build~compile` is credited with reading `src/**/*.ts`, which selects nothing
// because no such file is there — and the plan drops a scope entry that names a
// task the project does not run. The asymmetry ADR 0043 states decides the
// direction: a task wrongly left out is a gate that never ran, one wrongly left
// in is a cache hit.

// taskIndex answers the task-level impact questions for one loaded workspace.
type taskIndex struct {
	ws *Workspace
	// tasks holds one entry per declared task scope, keyed by `<command>~<step>`
	// (or the command alone for a command with no pipeline).
	tasks map[string]*indexedTask
	// every is every declared task scope, sorted.
	every []string
	// closure maps a task scope to itself plus every task of the same project
	// that depends on it, transitively and sorted.
	closure map[string][]string
	// reachedBy maps a task scope to the task scopes of a DEPENDENT project
	// that name it through a `^` step reference, sorted.
	reachedBy map[string][]string
	// reachedByAny is the union of reachedBy's values: what a project that runs
	// every task carries across an edge.
	reachedByAny []string
	// byCommand maps a command name to its task scopes, sorted.
	byCommand map[string][]string
	// byExtension maps an extension's name and its workspace-relative path to
	// the task scopes it declares, sorted.
	byExtension map[string][]string
	// extensionProjects maps an in-workspace extension project's ID to the task
	// scopes that extension declares.
	extensionProjects map[string][]string
	// assets maps a project ID to its cross-project generate asset sources,
	// workspace-relative and slash-separated.
	assets map[string][]string
	// runtimeInputs maps an in-workspace extension project's ID to the
	// extension-root-relative patterns its runtime is prepared from, test files
	// excluded (ReadsAsExtensionRuntime).
	runtimeInputs map[string][]string
}

// indexedTask is one declared task scope, merged across every manifest that
// declares that command and step.
type indexedTask struct {
	scope   string
	command string
	// files are the project-relative patterns the task's inputs select, in the
	// exact flattened, sorted form DeriveTaskCacheKey hands the cache hasher.
	// A `closure` port's patterns are here too, for the reason
	// keyFilePatterns states: the closure covers the seed project.
	files []string
	// workspaceFiles are the patterns a `from: "workspace"` port selects,
	// matched against the workspace-relative path itself.
	workspaceFiles []string
	// closureFiles are the patterns of the task's `closure` ports alone: the
	// files it reads in EVERY project of its dependency closure, not only its
	// own (closureScopePrefix).
	closureFiles []string
}

// NewTaskIndex builds the index from the extensions a run loaded.
//
// It returns a nil INTERFACE when no manifest declares a task, never a nil
// pointer inside one: a caller that could not load the extensions must keep the
// project-level behavior, and an index that answers "no task reads this" for
// every path would select nothing at all.
func NewTaskIndex(ws *Workspace, extensions []*extension.ExtensionDescription) TaskImpactIndex {
	if ws == nil || len(extensions) == 0 {
		return nil
	}
	idx := &taskIndex{
		ws:                ws,
		tasks:             make(map[string]*indexedTask),
		closure:           make(map[string][]string),
		reachedBy:         make(map[string][]string),
		byCommand:         make(map[string][]string),
		byExtension:       make(map[string][]string),
		extensionProjects: make(map[string][]string),
		runtimeInputs:     make(map[string][]string),
	}
	// successors is the intra-project task graph: a task scope mapped to the
	// task scopes of the same project that depend on it.
	successors := make(map[string]map[string]bool)
	// commandDeps collects command-level prerequisites, resolved once every
	// manifest is folded in: a command another extension contributes steps to
	// must be complete before its steps can be linked.
	commandDeps := make(map[string]map[string]bool)
	for _, ext := range extensions {
		idx.addExtension(ext, successors, commandDeps)
	}
	if !idx.attributes() {
		return nil
	}
	idx.every = sortedTaskKeys(idx.tasks)
	idx.linkCommandDeps(successors, commandDeps)
	idx.buildClosure(successors)
	idx.bindExtensionProjects(extensions)
	idx.assets = CollectProjectAssetPaths(ws)
	return idx
}

// attributes reports whether the loaded manifests declare a file input at all.
//
// They may not: an extension whose tasks name no `inputs` port and no cache key
// says nothing about what its actions read, and an index built from it would
// answer "no task reads this" for every path in the workspace — a selection of
// nothing over a real diff, which is the one failure this whole mechanism must
// not have. Such a workspace keeps the project-level mapping, the same way a
// run that could not load its extensions does.
//
// A single declaration is enough, and a task that declares none inside an
// otherwise-declaring workspace reads nothing of its own: it runs when a task it
// depends on runs, through the intra-project closure.
func (idx *taskIndex) attributes() bool {
	for _, task := range idx.tasks {
		if len(task.files) > 0 || len(task.workspaceFiles) > 0 {
			return true
		}
	}
	return false
}

// addExtension folds one manifest's commands into the index.
func (idx *taskIndex) addExtension(
	ext *extension.ExtensionDescription,
	successors map[string]map[string]bool,
	commandDeps map[string]map[string]bool,
) {
	if ext == nil {
		return
	}
	for command, job := range ext.Jobs {
		if job == nil {
			continue
		}
		if job.CommandName != "" {
			command = job.CommandName
		}
		for _, scope := range idx.addCommand(ext, command, job, successors) {
			register(idx.byCommand, command, scope)
			register(idx.byExtension, ext.Name, scope)
			register(idx.byExtension, cleanWorkspacePath(ext.RelPath), scope)
		}
		// A session barrier ("!cmd") orders two commands across the session
		// and carries no data, so it is not an impact edge.
		for _, prerequisite := range job.CommandDependsOn {
			if strings.HasPrefix(prerequisite, "!") {
				continue
			}
			if commandDeps[command] == nil {
				commandDeps[command] = make(map[string]bool)
			}
			commandDeps[command][prerequisite] = true
		}
	}
}

// linkCommandDeps turns each command-level prerequisite into intra-project task
// edges. The planner makes a command's root steps depend on the prerequisite
// command's leaf steps in the same project; every pair is linked here rather
// than the exact roots and leaves, which widens the closure and never narrows
// it.
func (idx *taskIndex) linkCommandDeps(successors map[string]map[string]bool, commandDeps map[string]map[string]bool) {
	for command, prerequisites := range commandDeps {
		for prerequisite := range prerequisites {
			for _, before := range idx.byCommand[prerequisite] {
				for _, after := range idx.byCommand[command] {
					link(successors, before, after)
				}
			}
		}
	}
}

// addCommand folds one command's steps in and returns the task scopes it
// declares.
func (idx *taskIndex) addCommand(
	ext *extension.ExtensionDescription,
	command string,
	job *extension.JobDefinition,
	successors map[string]map[string]bool,
) []string {
	if len(job.PipelineSteps) == 0 {
		// A command with no pipeline is one job named after the command.
		scope := command
		idx.mergeTask(scope, command, job.FilePatterns, nil, nil)
		return []string{scope}
	}
	scopes := make([]string, 0, len(job.PipelineSteps))
	for _, step := range job.PipelineSteps {
		scope := taskScope(command, step.ID)
		files, closureFiles, workspaceFiles := stepKeyPatterns(ext, step.Task)
		idx.mergeTask(scope, command, append(files, job.FilePatterns...), closureFiles, workspaceFiles)
		scopes = append(scopes, scope)
		for _, dep := range step.DependsOn {
			if upstream, ok := strings.CutPrefix(dep, "^"); ok {
				// The `^` reference the planner resolves against every
				// dependency: this step runs after the SAME command's step of
				// that name, in the other project.
				register(idx.reachedBy, taskScope(command, upstream), scope)
				continue
			}
			link(successors, taskScope(command, dep), scope)
		}
		// A finalizer runs BECAUSE its producer and consumers ran, and it is the
		// only step that tears down what they provisioned. It declares no
		// dependency on them — the relation points the other way — so without
		// this edge a scope that names `test` would drop the teardown of the
		// environment that `test` ran against, and the run would leak it.
		if step.Finalizes != nil {
			for _, member := range append([]string{step.Finalizes.Producer}, step.Finalizes.Consumers...) {
				link(successors, taskScope(command, member), scope)
			}
		}
	}
	return scopes
}

// stepKeyPatterns reads a step's task input ports the way the cache does:
// through the protocol's DeriveTaskCacheKey, so selection and the key stand on
// one declaration and one grammar.
//
// files carries the closure ports' patterns too — the closure covers the seed
// project — and closureFiles repeats them alone, because a file one of them
// selects is also read by every dependent that runs the task.
func stepKeyPatterns(ext *extension.ExtensionDescription, taskName string) (files, closureFiles, workspaceFiles []string) {
	task, ok := ext.Tasks[taskName]
	if !ok {
		return nil, nil, nil
	}
	if len(task.Inputs) > 0 {
		key := extension.DeriveTaskCacheKey(task.Inputs)
		return append(append([]string(nil), key.Files...), key.ClosureFiles...),
			append([]string(nil), key.ClosureFiles...), key.WorkspaceFiles
	}
	if task.Cache != nil && task.Cache.Key != nil {
		return append(append([]string(nil), task.Cache.Key.Files...), task.Cache.Key.ClosureFiles...),
			append([]string(nil), task.Cache.Key.ClosureFiles...), task.Cache.Key.WorkspaceFiles
	}
	return nil, nil, nil
}

func (idx *taskIndex) mergeTask(scope, command string, files, closureFiles, workspaceFiles []string) {
	task, known := idx.tasks[scope]
	if !known {
		task = &indexedTask{scope: scope, command: command}
		idx.tasks[scope] = task
	}
	task.files = mergePatterns(task.files, files)
	task.closureFiles = mergePatterns(task.closureFiles, closureFiles)
	task.workspaceFiles = mergePatterns(task.workspaceFiles, workspaceFiles)
}

// buildClosure resolves the transitive intra-project successors of every task
// scope, so one lookup answers "this task and everything that depends on it".
func (idx *taskIndex) buildClosure(successors map[string]map[string]bool) {
	for scope := range idx.tasks {
		reached := map[string]bool{scope: true}
		stack := []string{scope}
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for next := range successors[current] {
				if reached[next] {
					continue
				}
				reached[next] = true
				stack = append(stack, next)
			}
		}
		idx.closure[scope] = sortedSet(reached)
	}
	any := make(map[string]bool)
	for source, reached := range idx.reachedBy {
		idx.reachedBy[source] = idx.closureOf(reached)
		for _, scope := range idx.reachedBy[source] {
			any[scope] = true
		}
	}
	// A project that runs every task moved every file its closure ports
	// select, so it carries what a closure scope carries too.
	for scope, task := range idx.tasks {
		if len(task.closureFiles) == 0 {
			continue
		}
		any[closureScopePrefix+scope] = true
		for _, next := range idx.closure[scope] {
			any[next] = true
		}
	}
	idx.reachedByAny = sortedSet(any)
}

// bindExtensionProjects maps each in-workspace extension project onto the task
// scopes its own manifest declares, each QUALIFIED by that extension project's
// id: `/go/extension#build~describe`.
//
// The qualifier is what keeps the extension-consumer edge as narrow as ADR 0042
// made it. Two extensions routinely declare the same command and step — Go and
// TypeScript both declare `build~generate` — so an unqualified entry would let a
// Go tool change keep a consumer's TypeScript jobs of the same name, on every
// project that runs under both.
//
// An extension is discovered from its project directory, so the description's
// workspace-relative path is the project's path; names are not compared,
// because a registry-installed extension can share a name with a project. A
// pinned build that replaces a project's manifest of the same name binds that
// project to the tasks the pinned build declares.
func (idx *taskIndex) bindExtensionProjects(extensions []*extension.ExtensionDescription) {
	byPath := make(map[string]string, len(idx.ws.Projects))
	for _, p := range idx.ws.Projects {
		byPath[cleanWorkspacePath(p.Path)] = p.ID
	}
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		projectPath, declared := ext.RelPath, cleanWorkspacePath(ext.RelPath)
		if projectPath == "" {
			projectPath, declared = ext.PinnedOver, ext.Name
		}
		if projectPath == "" {
			continue
		}
		id, ok := byPath[cleanWorkspacePath(projectPath)]
		if !ok {
			continue
		}
		scopes := append([]string(nil), idx.extensionProjects[id]...)
		for _, scope := range idx.byExtension[declared] {
			scopes = append(scopes, id+extensionScopeSeparator+scope)
		}
		idx.extensionProjects[id] = dedupeSortedStrings(scopes)
		if ext.Runtime != nil && ext.Runtime.Prepare != nil && len(ext.Runtime.Prepare.Inputs) > 0 {
			idx.runtimeInputs[id] = append(append([]string(nil), ext.Runtime.Prepare.Inputs...), "!**/*_test.go")
		}
	}
}

// ReadsAsExtensionRuntime reports whether a path is part of an in-workspace
// extension's RUNTIME: a file its `runtime.prepare.inputs` select, test files
// excluded.
//
// Such a file is compiled or embedded into the binary every consumer executes
// — `go/extension/tools/versions.json` is embedded and pins the linters every
// Go consumer runs — whatever task of the extension project happens to declare
// it. The index merges every manifest's patterns per `<command>~<step>`, so a
// foreign extension's `lint~check` over `**/*.json` claims that file; left to
// that answer, the extension would run a lint task it does not have and no
// consumer would be reached. The model keeps such a project whole instead.
func (idx *taskIndex) ReadsAsExtensionRuntime(projectID, path string) bool {
	patterns := idx.runtimeInputs[projectID]
	if len(patterns) == 0 {
		return false
	}
	project := idx.ws.ProjectByID(projectID)
	if project == nil {
		return false
	}
	rel, relative := projectRelative(project, filepath.ToSlash(filepath.Clean(path)))
	if !relative || isParentRelative(rel) {
		return false
	}
	return wsproto.SelectsPath(rel, patterns)
}

// TasksReadingPath returns the tasks of a project whose declared inputs select
// a workspace-relative path, closed under the tasks that depend on them.
//
// Three declarations answer, and they are the three the cache hasher reads for
// the same job: the task's own input ports, the project's option layers
// (`options.<layer>.filePatterns`, attributed to the layer's tasks), and the
// project's cross-project generate assets, which every job of the project keys
// on and therefore every task reads.
//
// The two pattern families are matched over different reaches, because their
// authors are different. A task's `from: "project"` ports are written ONCE in an
// extension manifest for every project that runs under it (`**/*.go`), so they
// are read inside the project root and nowhere else; letting one escape would
// have every Go project read every Go file in the workspace. A project's own
// option layers are written BY that project and may name a file above its root
// on purpose — `../../**/putnami.json`, `git:**` — so they are matched in the
// "../" form filepath.Rel produces, the same coordinate system the cache hasher
// resolves them in and the model's declared-input index seeds from. Answering
// "no task reads this" for such a layer would drop exactly the drift gates that
// exist to watch files they do not own.
func (idx *taskIndex) TasksReadingPath(projectID, path string) []string {
	project := idx.ws.ProjectByID(projectID)
	if project == nil {
		return nil
	}
	lookup := filepath.ToSlash(filepath.Clean(path))
	rel, relative := projectRelative(project, lookup)
	if idx.readsAsGenerateAsset(project, lookup) {
		return idx.closureOf(idx.every)
	}
	inProject := relative && !isParentRelative(rel)
	reading := make(map[string]bool)
	for _, scope := range idx.every {
		task := idx.tasks[scope]
		if inProject && selectsTaskPath(filepath.Join(idx.ws.Root, project.Path), rel, task.files) {
			reading[scope] = true
			if selectsTaskPath(filepath.Join(idx.ws.Root, project.Path), rel, task.closureFiles) {
				reading[closureScopePrefix+scope] = true
			}
			continue
		}
		if selectsTaskPath(idx.ws.Root, lookup, task.workspaceFiles) {
			reading[scope] = true
		}
	}
	for layer, patterns := range declaredLayerPatterns(project) {
		if !relative || !wsproto.SelectsPath(rel, patterns) {
			continue
		}
		for _, scope := range idx.layerTasks(layer) {
			reading[scope] = true
		}
	}
	if len(reading) == 0 {
		return nil
	}
	return idx.closureOf(sortedSet(reading))
}

// selectsTaskPath interprets semantic Go embed selectors alongside literal
// globs. A deleted asset still matches its Go source directive. A scan error
// widens impact rather than dropping a task; key computation later fails closed.
func selectsTaskPath(root, relative string, patterns []string) bool {
	if wsproto.SelectsPath(relative, patterns) {
		return true
	}
	for _, pattern := range patterns {
		if !goembed.IsSelector(pattern) {
			continue
		}
		selected, err := goembed.SelectsPath(root, relative, pattern == goembed.TestSelector)
		if err != nil || selected {
			return true
		}
	}
	return false
}

// TasksReachedFrom returns the tasks a dependent runs because the listed tasks
// of a project it reads moved. A nil list stands for every task.
func (idx *taskIndex) TasksReachedFrom(tasks []string) []string {
	if tasks == nil {
		return idx.reachedByAny
	}
	reached := make(map[string]bool)
	for _, task := range tasks {
		if inner, closure := strings.CutPrefix(task, closureScopePrefix); closure {
			// A closure port reads the file in every project of the
			// dependency closure: the dependent runs the same task, and the
			// closure scope travels on so the next dependent does too.
			reached[task] = true
			for _, next := range idx.closure[inner] {
				reached[next] = true
			}
			continue
		}
		for _, next := range idx.reachedBy[task] {
			reached[next] = true
		}
	}
	return sortedSet(reached)
}

// TasksOfExtension returns the task scopes an in-workspace extension project's
// manifest declares.
func (idx *taskIndex) TasksOfExtension(extensionProjectID string) []string {
	return idx.extensionProjects[extensionProjectID]
}

// layerTasks attributes one option layer to task scopes, by the same rule the
// cache key's layer grouping applies: a COMMAND layer reaches that command's
// tasks, an EXTENSION layer reaches the tasks that extension declares, and
// `<extension>:<command>` reaches the intersection. A layer that resolves to
// neither reaches every task — an unattributed declaration is read by whoever
// asks for it, and erring wide here costs a cache hit.
func (idx *taskIndex) layerTasks(layer string) []string {
	if extRef, command, qualified := strings.Cut(layer, ":"); qualified {
		scopes := idx.extensionRefTasks(extRef)
		if len(scopes) == 0 {
			// An extension reference nothing resolves is an unattributed
			// declaration, exactly like an unqualified one below.
			return idx.every
		}
		return intersectSorted(scopes, idx.byCommand[command])
	}
	if scopes, ok := idx.byCommand[layer]; ok {
		return scopes
	}
	if scopes := idx.extensionRefTasks(layer); len(scopes) > 0 {
		return scopes
	}
	return idx.every
}

// extensionRefTasks resolves an option layer's extension reference — a package
// name, a workspace path, or a project ID — onto the tasks that extension
// declares.
func (idx *taskIndex) extensionRefTasks(ref string) []string {
	if scopes, ok := idx.byExtension[ref]; ok {
		return scopes
	}
	if scopes, ok := idx.byExtension[cleanWorkspacePath(strings.TrimPrefix(ref, "./"))]; ok {
		return scopes
	}
	if p := idx.ws.ProjectByID(ref); p != nil {
		return idx.byExtension[cleanWorkspacePath(p.Path)]
	}
	return nil
}

// readsAsGenerateAsset reports whether a path is one of the project's
// cross-project generate assets. Such a file is folded into EVERY job's key of
// the declaring project (jobs.generateAssetFiles), so every task reads it.
func (idx *taskIndex) readsAsGenerateAsset(project *Project, path string) bool {
	for _, source := range idx.assets[project.ID] {
		source = filepath.ToSlash(filepath.Clean(source))
		if path == source || strings.HasPrefix(path, source+"/") {
			return true
		}
	}
	return false
}

// closureOf expands task scopes with everything that depends on them inside the
// project.
func (idx *taskIndex) closureOf(scopes []string) []string {
	reached := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		for _, next := range idx.closure[scope] {
			reached[next] = true
		}
		reached[scope] = true
	}
	return sortedSet(reached)
}

// register adds one task scope under an index key, keeping the entry sorted
// and free of repeats.
func register(into map[string][]string, key, scope string) {
	if key == "" {
		return
	}
	into[key] = dedupeSortedStrings(append(into[key], scope))
}

// declaredLayerPatterns reads each option layer's `filePatterns` as its own
// pattern set, the way the model's declared-input index does: a real key groups
// the layers that apply to one job, so evaluating each alone applies strictly
// weaker exclusions and can never select less than the key reads.
func declaredLayerPatterns(p *Project) map[string][]string {
	if p == nil || p.Config == nil || len(p.Config.Options) == 0 {
		return nil
	}
	layers := make(map[string][]string, len(p.Config.Options))
	for layer, options := range p.Config.Options {
		raw, ok := options["filePatterns"]
		if !ok {
			continue
		}
		values, ok := raw.([]any)
		if !ok {
			continue
		}
		patterns := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok && s != "" {
				patterns = append(patterns, s)
			}
		}
		if len(patterns) > 0 {
			layers[layer] = patterns
		}
	}
	return layers
}

// extensionScopeSeparator joins an extension project id to a task scope. A
// scope entry is `[<extension-project-id>#]<command>~<step>`: unqualified when
// the change reaches the task whoever runs it, qualified when only one
// extension's version of that task is reached.
const extensionScopeSeparator = "#"

// closureScopePrefix marks a scope entry that is not a task but a READ: a file
// a task's `closure` port selects moved, so every dependent running that task
// reads it too. `closure:test~test-env` reaches a dependent's `test~test-env`
// and travels on to the next dependent, which a `^` reference never does — a
// dependency's infra/requirements.json provisions the test environment of
// every project above it. No planned job is named by it; it is carried, never
// matched.
const closureScopePrefix = "closure:"

// taskScope is the plan name of one declared task: `<command>~<step>`, or the
// command alone for a command with no pipeline. It is exactly what
// ScheduledJob.CommandName() and StepID() derive for a planned job, which is
// what lets the plan narrowing match a scope without parsing a job name.
func taskScope(command, step string) string {
	if step == "" {
		return command
	}
	return command + extension.StepSeparator + step
}

// projectRelative makes a workspace-relative path relative to a project, in the
// exact form filepath.Rel produces — a path OUTSIDE the project root keeps its
// "../" prefix, which is the form a declared pattern such as
// "../../**/putnami.json" is written against. It reports false only when no
// relative form exists.
func projectRelative(p *Project, path string) (string, bool) {
	base := cleanWorkspacePath(p.Path)
	if base == "" || base == "." {
		return path, true
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// isParentRelative reports whether a project-relative path climbs out of the
// project root.
func isParentRelative(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, "../")
}

func link(successors map[string]map[string]bool, before, after string) {
	if before == after {
		return
	}
	if successors[before] == nil {
		successors[before] = make(map[string]bool)
	}
	successors[before][after] = true
}

func mergePatterns(into, add []string) []string {
	if len(add) == 0 {
		return into
	}
	return dedupeSortedStrings(append(append([]string(nil), into...), add...))
}

func sortedSet(set map[string]bool) []string {
	list := make([]string, 0, len(set))
	for key := range set {
		list = append(list, key)
	}
	sort.Strings(list)
	return list
}

func sortedTaskKeys(tasks map[string]*indexedTask) []string {
	list := make([]string, 0, len(tasks))
	for key := range tasks {
		list = append(list, key)
	}
	sort.Strings(list)
	return list
}

func dedupeSortedStrings(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for i, value := range values {
		if i > 0 && values[i-1] == value {
			continue
		}
		out = append(out, value)
	}
	return out
}

func intersectSorted(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	in := make(map[string]bool, len(b))
	for _, value := range b {
		in[value] = true
	}
	out := make([]string, 0, len(a))
	for _, value := range a {
		if in[value] {
			out = append(out, value)
		}
	}
	return out
}
