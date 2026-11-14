package moon

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
)

// TasksFile is the file whose tasks every project inherits, in moon 1.x.
const TasksFile = ".moon/tasks.yml"

// TasksDir holds the task files a project inherits by its language, stack,
// layer or tags.
const TasksDir = ".moon/tasks"

// ExportCommand is the command that writes DefaultGraphFile.
const ExportCommand = "moon project-graph --json > " + DefaultGraphFile

// Task is one moon task of a project.
type Task struct {
	// Name is the task id within its project.
	Name string
	// Tags are the task's own tags.
	Tags []string
	// Command is the command line the task runs: its script, else its
	// command followed by its args; empty when neither states one.
	Command string
	// Deps are the targets of the task's deps, as moon states them, such as
	// ^:build, ~:build, build or types:build, in their order.
	Deps []string
}

// Project is one moon project, with what an integration needs to run it.
type Project struct {
	// ID is the moon project id.
	ID string
	// Dir is the workspace-relative project directory, "." for the root.
	Dir string
	// Language is moon's language of the project: the one its moon.yml
	// states, else the one its files imply, else "unknown".
	Language string
	// Stack and Layer classify the project; Layer reads moon 1.x's type
	// when no layer is stated. Empty when unstated.
	Stack string
	Layer string
	// Tags are the project's tags, sorted.
	Tags []string
	// Tasks are the project's tasks, sorted by name: its own over those it
	// inherits.
	Tasks []Task
	// DependsOn are the workspace-relative directories of the projects it
	// depends on.
	DependsOn []string
	// SourceFile is the workspace-relative moon.yml of the project, or the
	// export it comes from, or empty.
	SourceFile string
}

// Detection is what Detect read.
type Detection struct {
	// Projects are sorted by directory; empty outside a moon workspace.
	Projects []Project
	// FromExport reports that the projects come from the graph export.
	FromExport bool
	// Diagnostics are the findings of the read; a file that cannot be read
	// costs what it would have stated, never the run.
	Diagnostics []diag.Diagnostic
}

// toolchainFiles are the files where a moon workspace configures its
// toolchains: moon 2.x's, then moon 1.x's.
var toolchainFiles = []string{".moon/toolchains.yml", ".moon/toolchain.yml"}

// InfersTasks names the setting by which the moon workspace at root infers
// tasks from package.json scripts, inferTasksFromScripts under javascript
// (moon 2.x) or node (moon 1.x), as file: key; empty when it infers none.
// Only moon itself lists those tasks: neither moon.yml nor the task files
// state them.
func InfersTasks(root string) string {
	for _, file := range toolchainFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			continue
		}
		for _, toolchain := range []string{"javascript", "node"} {
			settings, _, err := topLevel(string(data), toolchain)
			if m, ok := settings.(map[string]any); err == nil && ok && m["inferTasksFromScripts"] == "true" {
				return file + ": " + toolchain + ".inferTasksFromScripts"
			}
		}
	}
	return ""
}

// IsWorkspace reports whether root holds a moon workspace: its
// .moon/workspace.yml.
func IsWorkspace(root string) bool {
	return exists(root, WorkspaceFile)
}

// Detect reads the moon projects of the repository at root, from the graph
// export when there is a usable one, else from the moon configuration, with
// each project's language, stack, layer, tags and tasks. The tasks of the
// export win; without them a project's tasks are its moon.yml tasks over the
// ones it inherits from .moon/tasks.yml and .moon/tasks/*.yml. A moon 2.x
// export names which tasks the project has by their targets, and which task
// files it inherits.
func Detect(root string, opts Options) Detection {
	d := &detector{root: root}
	found, source, _ := d.projects(opts.GraphFile)
	inherited := d.taskFiles()
	out := Detection{FromExport: source == evidence.SourceOrchestrator}
	for _, p := range found {
		out.Projects = append(out.Projects, d.describe(p, inherited))
	}
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].Dir < out.Projects[j].Dir })
	out.Diagnostics = d.diagnostics
	return out
}

// taskFile is one file of inherited tasks.
type taskFile struct {
	// rel is its workspace-relative path.
	rel string
	// name is its file name without the extension, which names who inherits
	// it in moon 1.x.
	name string
	// by is its inheritedBy condition, moon 2.x's; nil without one.
	by map[string]any
	// tasks are its tasks, by name.
	tasks map[string]map[string]any
}

// taskFiles reads .moon/tasks.yml, then the .yml files under .moon/tasks in
// path order: a later file's task replaces an earlier one of the same name.
func (d *detector) taskFiles() []taskFile {
	var rels []string
	if exists(d.root, TasksFile) {
		rels = append(rels, TasksFile)
	}
	dir := filepath.Join(d.root, filepath.FromSlash(TasksDir))
	_ = filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if ext := path.Ext(entry.Name()); ext == ".yml" || ext == ".yaml" {
			rels = append(rels, relative(d.root, p))
		}
		return nil
	})
	var out []taskFile
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(rel)))
		if err != nil {
			d.warn(CodeManifestUnreadable, rel, "%s cannot be read: %v; no project inherits its tasks", rel, err)
			continue
		}
		tasks, _, err := topLevel(string(data), "tasks")
		var by any
		if err == nil {
			by, _, err = topLevel(string(data), "inheritedBy")
		}
		if err != nil {
			d.warn(CodeManifestUnreadable, rel, "%s cannot be read: %v; no project inherits its tasks", rel, err)
			continue
		}
		f := taskFile{rel: rel, name: strings.TrimSuffix(path.Base(rel), path.Ext(rel)), tasks: taskMap(tasks)}
		f.by, _ = by.(map[string]any)
		out = append(out, f)
	}
	return out
}

// taskMap reads a tasks mapping: each task's settings, by name.
func taskMap(value any) map[string]map[string]any {
	byName, _ := value.(map[string]any)
	out := make(map[string]map[string]any, len(byName))
	for name, settings := range byName {
		s, _ := settings.(map[string]any)
		out[name] = s
	}
	return out
}

// describe completes one project with its moon.yml and the tasks it
// inherits, then with what the export states.
func (d *detector) describe(p *project, inherited []taskFile) Project {
	out := Project{ID: p.id, Dir: p.dir, SourceFile: p.sourceFile, DependsOn: slices.Clone(p.deps)}
	cfg := d.projectConfig(p.dir)
	out.Language, out.Stack, out.Layer, out.Tags = cfg.language, cfg.stack, cfg.layer, cfg.tags
	if e := p.export; e != nil {
		var language string
		if json.Unmarshal(e.Language, &language) == nil && language != "" {
			out.Language = language
		}
		if e.Stack != "" {
			out.Stack = e.Stack
		}
		if layer := cmpOr(e.Layer, e.Type); layer != "" {
			out.Layer = layer
		}
		if e.Config != nil && e.Config.Tags != nil {
			out.Tags = slices.Clone(e.Config.Tags)
		}
	}
	if out.Language == "" {
		out.Language = d.inferLanguage(p.dir)
	}
	slices.Sort(out.Tags)
	out.Tags = slices.Compact(out.Tags)

	tasks := map[string]Task{}
	if p.export != nil && p.export.Tasks != nil {
		for name, t := range p.export.Tasks {
			tasks[name] = Task{Tags: t.Tags, Command: t.commandLine(), Deps: t.targets()}
		}
	} else {
		// The task files moon 2.x says the project inherits win over the
		// ones its inheritedBy conditions match here.
		var files []taskFile
		for _, f := range inherited {
			if f.inheritedBy(out) {
				files = append(files, f)
			}
		}
		if p.export != nil {
			if fromExport, ok := p.export.inheritedFiles(); ok {
				files = fromExport
			}
		}
		settingsOf := map[string]map[string]any{}
		for _, f := range files {
			for name, settings := range f.tasks {
				if base, ok := settingsOf[name]; ok {
					settings = mergeTask(base, settings)
				}
				settingsOf[name] = settings
			}
		}
		settingsOf = inherit(cfg.inheritance, settingsOf)
		for name, settings := range cfg.tasks {
			if base, ok := settingsOf[name]; ok {
				settings = mergeTask(base, settings)
			}
			settingsOf[name] = settings
		}
		// A moon 2.x export names the tasks by their targets: those the
		// configuration states keep its settings, and one moon infers, from
		// a package.json script, states none.
		if p.export != nil && len(p.export.TaskTargets) > 0 {
			named := make(map[string]map[string]any, len(p.export.TaskTargets))
			for _, name := range p.export.taskNames() {
				named[name] = settingsOf[name]
			}
			settingsOf = named
		}
		for name, settings := range settingsOf {
			tasks[name] = Task{Tags: stringsOf(settings["tags"]), Command: commandLine(settings), Deps: depTargets(settings["deps"])}
		}
	}
	for name, task := range tasks {
		tags := slices.Clone(task.Tags)
		slices.Sort(tags)
		out.Tasks = append(out.Tasks, Task{Name: name, Tags: slices.Compact(tags), Command: task.Command, Deps: task.Deps})
	}
	sort.Slice(out.Tasks, func(i, j int) bool { return out.Tasks[i].Name < out.Tasks[j].Name })
	return out
}

func cmpOr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// projectConfig is what one moon.yml states for Detect.
type projectConfig struct {
	language, stack, layer string
	tags                   []string
	tasks                  map[string]map[string]any
	inheritance            inheritance
}

// projectConfig reads the moon.yml of a project directory; a missing or
// unreadable file states nothing, and the reader of the edges already
// reported an unreadable one.
func (d *detector) projectConfig(dir string) projectConfig {
	var cfg projectConfig
	data, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(path.Join(dir, ProjectFile))))
	if err != nil {
		return cfg
	}
	text := string(data)
	scalarOf := func(key string) string {
		v, _, err := topLevel(text, key)
		if err != nil {
			return ""
		}
		s, _ := v.(string)
		return s
	}
	cfg.language = scalarOf("language")
	cfg.stack = scalarOf("stack")
	cfg.layer = cmpOr(scalarOf("layer"), scalarOf("type"))
	if tags, _, err := topLevel(text, "tags"); err == nil {
		cfg.tags = stringsOf(tags)
	}
	if tasks, _, err := topLevel(text, "tasks"); err == nil {
		cfg.tasks = taskMap(tasks)
	}
	if ws, _, err := topLevel(text, "workspace"); err == nil {
		settings, _ := ws.(map[string]any)
		inherited, _ := settings["inheritedTasks"].(map[string]any)
		cfg.inheritance = readInheritance(inherited)
	}
	return cfg
}

// inheritance is a project's workspace.inheritedTasks: which inherited tasks
// it keeps, and under which names.
type inheritance struct {
	// include lists the only tasks kept when set; an empty, non-nil list
	// keeps none.
	include []string
	exclude []string
	rename  map[string]string
}

func readInheritance(settings map[string]any) inheritance {
	var in inheritance
	if v, ok := settings["include"]; ok {
		in.include = stringsOf(v)
		if in.include == nil {
			in.include = []string{}
		}
	}
	in.exclude = stringsOf(settings["exclude"])
	if rename, ok := settings["rename"].(map[string]any); ok {
		in.rename = map[string]string{}
		for from, to := range rename {
			if s, ok := to.(string); ok {
				in.rename[from] = s
			}
		}
	}
	return in
}

// inherit keeps the included tasks less the excluded ones, then renames
// them, whatever a task holds.
func inherit[T any](in inheritance, tasks map[string]T) map[string]T {
	out := map[string]T{}
	for name, task := range tasks {
		if in.include != nil && !slices.Contains(in.include, name) || slices.Contains(in.exclude, name) {
			continue
		}
		if to, ok := in.rename[name]; ok {
			name = to
		}
		out[name] = task
	}
	return out
}

// mergeTask merges the settings of a task over the settings of the layer
// below, an inherited task file or the project's moon.yml over what it
// inherits, as moon merges them by default: the upper command, script and
// other settings win, and its args and tags follow the lower ones, unless its
// options.mergeArgs, options.mergeTags or options.merge says replace or
// prepend.
func mergeTask(base, own map[string]any) map[string]any {
	merged := map[string]any{}
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range own {
		if key != "args" && key != "tags" {
			merged[key] = value
		}
	}
	// deps merge as moon merges them: appended by default, or as
	// options.mergeDeps says.
	if ownDeps, set := own["deps"]; set {
		options, _ := own["options"].(map[string]any)
		inherited, _ := base["deps"].([]any)
		mine, _ := ownDeps.([]any)
		switch options["mergeDeps"] {
		case "replace":
			merged["deps"] = mine
		case "prepend":
			merged["deps"] = append(slices.Clone(mine), inherited...)
		default:
			merged["deps"] = append(slices.Clone(inherited), mine...)
		}
	}
	if _, ok := own["command"]; ok {
		// A project's command replaces the inherited script.
		if _, scripted := own["script"]; !scripted {
			delete(merged, "script")
		}
	}
	options, _ := own["options"].(map[string]any)
	for key, option := range map[string]string{"args": "mergeArgs", "tags": "mergeTags"} {
		ownValues, set := own[key]
		if !set {
			continue
		}
		strategy, stated := options[option]
		if !stated {
			strategy = options["merge"]
		}
		values := mergeList(stringsOf(base[key]), stringsOf(ownValues), strategy)
		if key == "tags" {
			slices.Sort(values)
			values = slices.Compact(values)
		}
		list := make([]any, len(values))
		for i, v := range values {
			list[i] = v
		}
		merged[key] = list
	}
	return merged
}

// mergeList merges the values of an upper layer with those below it: replace
// keeps the upper ones, prepend puts them first, and anything else appends
// them.
func mergeList(base, own []string, strategy any) []string {
	switch strategy {
	case "replace":
		return slices.Clone(own)
	case "prepend":
		return append(slices.Clone(own), base...)
	default:
		return append(slices.Clone(base), own...)
	}
}

// commandLine is the command line a task's settings state: its script, else
// its command, a string or a list, followed by its args; empty when they
// state neither.
func commandLine(settings map[string]any) string {
	if script, ok := settings["script"].(string); ok && strings.TrimSpace(script) != "" {
		return strings.TrimSpace(script)
	}
	command := strings.Join(stringsOf(settings["command"]), " ")
	if command == "" {
		return ""
	}
	return strings.TrimSpace(command + " " + strings.Join(stringsOf(settings["args"]), " "))
}

// inheritedBy reports whether the project inherits the file's tasks. A file
// with an inheritedBy condition, moon 2.x's, matches when each of its
// languages, stacks, layers and tags lists that is set, or their singular
// forms, names the project's value, one of its tags for tags; a tags clause
// matches no project, and a condition this reader does not evaluate, such as
// toolchains, files or order, constrains nothing. Without one, the file's
// name decides, as
// in moon 1.x: tasks.yml and all for every project, then the project's
// language, its platform (node for JavaScript and TypeScript), its stack,
// <language|stack|platform>-<layer>, and tag-<tag>.
func (f taskFile) inheritedBy(p Project) bool {
	if f.by != nil {
		values := map[string][]string{"languages": {p.Language}, "stacks": {p.Stack}, "layers": {p.Layer}, "tags": p.Tags}
		for key, wanted := range f.by {
			value, known := values[key]
			if !known {
				value, known = values[key+"s"]
			}
			if known && !slices.ContainsFunc(stringsOf(wanted), func(w string) bool { return slices.Contains(value, w) }) {
				return false
			}
		}
		return true
	}
	if f.rel == TasksFile || f.name == "all" || f.name == "*" {
		return true
	}
	kinds := []string{p.Language, p.Stack}
	if p.Language == "javascript" || p.Language == "typescript" {
		kinds = append(kinds, "node")
	}
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if f.name == kind || p.Layer != "" && f.name == kind+"-"+p.Layer {
			return true
		}
	}
	for _, tag := range p.Tags {
		if f.name == "tag-"+tag {
			return true
		}
	}
	return false
}

// languageFiles are the files that imply a project's language when its
// moon.yml states none, in the order moon checks them.
var languageFiles = []struct {
	files    []string
	language string
}{
	{[]string{"go.mod", "go.sum"}, "go"},
	{[]string{"composer.json", "composer.lock"}, "php"},
	{[]string{"pyproject.toml", "requirements.txt", "Pipfile", "poetry.lock", "uv.lock"}, "python"},
	{[]string{"Gemfile", "Gemfile.lock"}, "ruby"},
	{[]string{"Cargo.toml", "Cargo.lock"}, "rust"},
	{[]string{"tsconfig.json", "deno.json", "deno.jsonc"}, "typescript"},
	{[]string{"package.json"}, "javascript"},
}

// inferLanguage is the language a project's files imply, "unknown" when none
// does.
func (d *detector) inferLanguage(dir string) string {
	for _, entry := range languageFiles {
		for _, file := range entry.files {
			if exists(d.root, path.Join(dir, file)) {
				return entry.language
			}
		}
	}
	return "unknown"
}

// stringsOf reads a list of strings, skipping any other entry; any other
// value reads as nil.
func stringsOf(value any) []string {
	list, ok := value.([]any)
	if !ok {
		if s, isString := value.(string); isString && s != "" {
			return []string{s}
		}
		return nil
	}
	var out []string
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
