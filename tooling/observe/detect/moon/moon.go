// Package moon detects the projects of a moon workspace and the edges between
// them, without running moon.
//
// A project graph export the CI wrote with
// `moon project-graph --json > moon-graph.json` is the authority. Without one,
// a root .moon/workspace.yml makes the repository a moon workspace: its
// projects setting, each project's moon.yml and the package.json dependencies
// between projects are read statically. A project is a component whatever its
// language, with or without a package.json. A project of the export is an
// orchestrator component, one of the configuration a workspace component. The
// answer is a workspace probe
// result, so the detector can move to an extension of its own without a
// contract change.
package moon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/workspace"
)

// Extension is the extension the probe result speaks for.
const Extension = "@putnami/observe"

// Ecosystem is the component ecosystem of a moon project.
const Ecosystem = "moon"

// DefaultGraphFile is where the detector looks for a graph export when the
// caller names none.
const DefaultGraphFile = "moon-graph.json"

// WorkspaceFile is the file that makes a repository a moon workspace.
const WorkspaceFile = ".moon/workspace.yml"

// ProjectFile is the configuration file of one moon project.
const ProjectFile = "moon.yml"

// Diagnostic codes.
const (
	CodeGraphUnreadable     = "observe.moon_graph_unreadable"
	CodeWorkspaceUnreadable = "observe.moon_workspace_unreadable"
	CodeManifestUnreadable  = "observe.moon_manifest_unreadable"
)

// Metadata is what the detector tells the change analysis about one project,
// carried as the probe project's metadata.
type Metadata struct {
	// Name is the moon project id.
	Name string `json:"name"`
	// Ecosystem is always "moon".
	Ecosystem string `json:"ecosystem"`
	// Source is evidence.SourceOrchestrator for a graph export and
	// evidence.SourceWorkspace for the moon configuration.
	Source string `json:"source"`
}

// ReadMetadata decodes the metadata the detector wrote on a probe project.
func ReadMetadata(p workspace.ProbeProject) (Metadata, bool) {
	var m Metadata
	if len(p.Metadata) == 0 || json.Unmarshal(p.Metadata, &m) != nil || m.Ecosystem != Ecosystem {
		return Metadata{}, false
	}
	return m, true
}

// Options tune one probe.
type Options struct {
	// GraphFile is the graph export to read, relative to the root or absolute.
	// Empty means DefaultGraphFile, whose absence is no finding.
	GraphFile string
}

// Answer returns the probe answer for the repository at root, ready for
// workspace.ServeProbe.
func Answer(root string, opts Options) workspace.ProbeAnswerFunc {
	return func(req workspace.ProbeRequest) (workspace.ProbeResult, error) {
		return Probe(root, req, opts), nil
	}
}

// Probe answers one workspace probe request for the repository at root. It
// reads files only, and every finding is a diagnostic on the result.
func Probe(root string, req workspace.ProbeRequest, opts Options) workspace.ProbeResult {
	result := workspace.ProbeResult{Version: workspace.ProbeProtocolVersion, Extension: Extension}
	d := &detector{root: root}
	found, source, watched := d.projects(opts.GraphFile)
	result.Diagnostics = d.diagnostics
	if len(found) == 0 {
		return result
	}
	projects := d.probeProjects(found, source)
	result.Projects = keep(projects, req.Paths)
	result.WatchedFiles = watched
	workspace.NormalizeProbeResult(&result)
	return result
}

type detector struct {
	root        string
	diagnostics []diag.Diagnostic
}

// projects reads the projects of the graph export, else of the moon
// configuration, with the evidence source they come from and the files read.
func (d *detector) projects(graphFile string) ([]*project, string, []string) {
	if projects, watched, ok := d.fromGraph(graphFile); ok {
		return projects, evidence.SourceOrchestrator, watched
	}
	projects, watched := d.fromConfiguration()
	return projects, evidence.SourceWorkspace, watched
}

func (d *detector) warn(code, field, format string, args ...any) {
	d.diagnostics = append(d.diagnostics, diag.Warningf(code, field, format, args...))
}

// project is one moon project before it becomes a probe project.
type project struct {
	id         string
	dir        string
	alias      string
	sourceFile string
	declared   []any
	taskDeps   []string
	packages   []string
	deps       []string
	watched    []string
	// export is what the graph export states about the project beyond its
	// source; nil for a project of the configuration.
	export *exportProject
}

func (d *detector) probeProjects(projects []*project, source string) []workspace.ProbeProject {
	out := make([]workspace.ProbeProject, 0, len(projects))
	for _, p := range projects {
		metadata, _ := json.Marshal(Metadata{Name: p.id, Ecosystem: Ecosystem, Source: source})
		out = append(out, workspace.ProbeProject{
			Path:         p.dir,
			SourceName:   p.id,
			SourceFile:   p.sourceFile,
			Dependencies: p.deps,
			WatchedFiles: p.watched,
			Metadata:     metadata,
		})
	}
	return out
}

// --- graph export -----------------------------------------------------------

// graphExport is what `moon project-graph --json` writes: moon 1.x puts the
// projects in graph.nodes; moon 2.x puts node indices there and the projects
// in data, keyed by index. graph.edges holds [from, to, scope] entries, from
// depending on to.
type graphExport struct {
	Graph *struct {
		Nodes []json.RawMessage `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	} `json:"graph"`
	Data map[string]json.RawMessage `json:"data"`
}

type exportProject struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Alias  string `json:"alias"`
	// Language, Stack, Layer and Type are moon's resolved classification;
	// a language moon names as an object, such as a custom one, reads as
	// none.
	Language json.RawMessage `json:"language"`
	Stack    string          `json:"stack"`
	Layer    string          `json:"layer"`
	Type     string          `json:"type"`
	// Config is the project's moon.yml as moon resolved it, with its tags.
	Config *struct {
		Tags []string `json:"tags"`
	} `json:"config"`
	// Tasks are the project's resolved tasks, inherited ones included; nil
	// when the export omits them, as moon 2.x does.
	Tasks map[string]exportTask `json:"tasks"`
	// TaskTargets are the targets of the project's tasks, <project>:<task>,
	// inferred ones included: moon 2.x lists them in place of tasks.
	TaskTargets []string `json:"taskTargets"`
	// Inherited is what moon 2.x says the project inherits: configs holds
	// each task file it matched, by path, in the order moon merges them.
	Inherited json.RawMessage `json:"inherited"`
}

// inheritedFiles are the task files the export says the project inherits,
// in moon's order; ok is false when the export does not say.
func (e *exportProject) inheritedFiles() (files []taskFile, ok bool) {
	var inherited struct {
		Configs json.RawMessage `json:"configs"`
	}
	if len(e.Inherited) == 0 || json.Unmarshal(e.Inherited, &inherited) != nil || len(inherited.Configs) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(inherited.Configs))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		return nil, false
	}
	for decoder.More() {
		key, err := decoder.Token()
		rel, isPath := key.(string)
		if err != nil || !isPath {
			return nil, false
		}
		var config struct {
			Tasks map[string]any `json:"tasks"`
		}
		if decoder.Decode(&config) != nil {
			return nil, false
		}
		files = append(files, taskFile{rel: rel, name: strings.TrimSuffix(path.Base(rel), path.Ext(rel)), tasks: taskMap(config.Tasks)})
	}
	return files, true
}

// taskNames are the task ids the project's task targets name, the part
// after the last colon of each target.
func (e *exportProject) taskNames() []string {
	var names []string
	for _, target := range e.TaskTargets {
		name := target[strings.LastIndex(target, ":")+1:]
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// exportTask is one resolved task of the export: its tags and what it runs.
// Command, Args and Script are read leniently, so a shape this reader does
// not know states no command rather than failing the export.
type exportTask struct {
	Tags    []string        `json:"tags"`
	Command json.RawMessage `json:"command"`
	Args    json.RawMessage `json:"args"`
	Script  json.RawMessage `json:"script"`
}

// commandLine is the command line the task runs: its script, else its
// command followed by its args.
func (t exportTask) commandLine() string {
	settings := map[string]any{}
	for key, raw := range map[string]json.RawMessage{"command": t.Command, "args": t.Args, "script": t.Script} {
		var value any
		if len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
			settings[key] = value
		}
	}
	return commandLine(settings)
}

// fromGraph reads the graph export. It reports ok=false when there is no
// usable export. A missing default file is no finding; a missing named file
// is one, and so is a file that is not a moon graph export when the caller
// named it or the repository holds the workspace file.
func (d *detector) fromGraph(named string) ([]*project, []string, bool) {
	graphFile := named
	if graphFile == "" {
		graphFile = DefaultGraphFile
	}
	file := graphFile
	if !filepath.IsAbs(file) {
		file = filepath.Join(d.root, filepath.FromSlash(file))
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		if named != "" {
			d.warn(CodeGraphUnreadable, graphFile, "the moon graph export %s does not exist; the moon configuration answers instead", graphFile)
		}
		return nil, nil, false
	}
	if err != nil {
		d.warn(CodeGraphUnreadable, graphFile, "the moon graph export cannot be read: %v; the moon configuration answers instead", err)
		return nil, nil, false
	}
	reportable := named != "" || exists(d.root, WorkspaceFile)
	nodes, edges, err := decodeExport(data)
	if err != nil {
		if reportable {
			d.warn(CodeGraphUnreadable, graphFile, "%s is not a moon project graph export: %v; the moon configuration answers instead", graphFile, err)
		}
		return nil, nil, false
	}
	byIndex := make(map[int]*project, len(nodes))
	owners := map[string]string{}
	var projects []*project
	for i, node := range nodes {
		if node.Source == "" {
			d.warn(CodeGraphUnreadable, graphFile, "project %s has no source and is skipped", node.ID)
			continue
		}
		dir, ok := workspace.NormalizeProbePath(node.Source)
		if !ok {
			d.warn(CodeGraphUnreadable, graphFile, "project %s has a source outside the repository: %q", node.ID, node.Source)
			continue
		}
		if other, taken := owners[dir]; taken {
			d.warn(CodeGraphUnreadable, graphFile, "projects %s and %s share the source %s; %s is skipped", other, node.ID, dir, node.ID)
			continue
		}
		owners[dir] = node.ID
		p := &project{id: node.ID, dir: dir, alias: node.Alias, sourceFile: d.projectFile(dir), export: &nodes[i]}
		if exportFile, inside := workspace.NormalizeProbePath(relative(d.root, file)); p.sourceFile == "" && inside {
			p.sourceFile = exportFile
		}
		byIndex[i] = p
		projects = append(projects, p)
	}
	for _, e := range edges {
		from, to := byIndex[e.from], byIndex[e.to]
		if from == nil || to == nil || from == to || e.scope == "root" || slices.Contains(from.deps, to.dir) {
			continue
		}
		from.deps = append(from.deps, to.dir)
	}
	watched := []string{relative(d.root, file)}
	if exists(d.root, WorkspaceFile) {
		watched = append(watched, WorkspaceFile)
	}
	return projects, watched, true
}

type exportEdge struct {
	from, to int
	scope    string
}

// decodeExport reads the projects and edges of an export, in node order.
func decodeExport(data []byte) ([]exportProject, []exportEdge, error) {
	var export graphExport
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, nil, err
	}
	if export.Graph == nil || export.Graph.Nodes == nil {
		return nil, nil, errors.New("it has no graph.nodes")
	}
	nodes := make([]exportProject, len(export.Graph.Nodes))
	sourced := false
	for i, raw := range export.Graph.Nodes {
		var index int
		if json.Unmarshal(raw, &index) == nil {
			found, ok := export.Data[strconv.Itoa(index)]
			if !ok {
				return nil, nil, fmt.Errorf("node %d names no project of data", index)
			}
			raw = found
		}
		if err := json.Unmarshal(raw, &nodes[i]); err != nil {
			return nil, nil, fmt.Errorf("node %d is not a project: %w", i, err)
		}
		if nodes[i].ID == "" {
			nodes[i].ID = strconv.Itoa(i)
		}
		sourced = sourced || nodes[i].Source != ""
	}
	if !sourced {
		return nil, nil, errors.New("its nodes name no project source")
	}
	var edges []exportEdge
	for i, raw := range export.Graph.Edges {
		var entry []json.RawMessage
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, nil, fmt.Errorf("edge %d is not a [from, to, scope] entry", i)
		}
		if entry == nil {
			continue
		}
		var e exportEdge
		if len(entry) < 2 || json.Unmarshal(entry[0], &e.from) != nil || json.Unmarshal(entry[1], &e.to) != nil {
			return nil, nil, fmt.Errorf("edge %d is not a [from, to, scope] entry", i)
		}
		if len(entry) > 2 {
			_ = json.Unmarshal(entry[2], &e.scope)
		}
		edges = append(edges, e)
	}
	return nodes, edges, nil
}

// --- configuration ----------------------------------------------------------

// fromConfiguration reads .moon/workspace.yml, each project's moon.yml and
// package.json.
func (d *detector) fromConfiguration() ([]*project, []string) {
	data, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(WorkspaceFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err == nil {
		var projects []*project
		projects, err = d.workspaceProjects(string(data))
		if err == nil {
			d.edges(projects)
			return projects, []string{WorkspaceFile}
		}
	}
	d.warn(CodeWorkspaceUnreadable, WorkspaceFile, "%s cannot be read: %v; the repository is left to the other detectors", WorkspaceFile, err)
	return nil, nil
}

// workspaceProjects reads the projects setting: a list of globs, a map of
// project id to directory, or both under globs and sources, with globFormat
// naming a globbed project by its directory name or its path. Mapped
// projects come first, so a glob never takes a mapped directory.
func (d *detector) workspaceProjects(text string) ([]*project, error) {
	value, _, err := topLevel(text, "projects")
	if err != nil {
		return nil, err
	}
	var globs []string
	byPath := false
	sources := map[string]string{}
	switch v := value.(type) {
	case nil:
	case []any:
		if globs, err = stringList(v, "projects"); err != nil {
			return nil, err
		}
	case map[string]any:
		_, hasGlobs := v["globs"].([]any)
		_, hasSources := v["sources"].(map[string]any)
		mapped := v
		if hasGlobs || hasSources {
			if globs, err = stringList(v["globs"], "projects.globs"); err != nil {
				return nil, err
			}
			mapped, _ = v["sources"].(map[string]any)
			switch format := v["globFormat"]; format {
			case nil, "dir-name":
			case "source-path":
				byPath = true
			default:
				return nil, fmt.Errorf("projects.globFormat %v is neither dir-name nor source-path", format)
			}
		}
		for id, dir := range mapped {
			s, ok := dir.(string)
			if !ok {
				return nil, fmt.Errorf("the source of project %s is not a path", id)
			}
			sources[id] = s
		}
	default:
		return nil, errors.New("projects is not a list of globs, a map of projects, or both")
	}
	var projects []*project
	owners := map[string]string{}
	add := func(id, dir string) {
		if other, taken := owners[dir]; taken {
			d.warn(CodeWorkspaceUnreadable, WorkspaceFile, "projects %s and %s share the directory %s; %s is skipped", other, id, dir, id)
			return
		}
		owners[dir] = id
		projects = append(projects, &project{id: id, dir: dir})
	}
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		dir, ok := workspace.NormalizeProbePath(sources[id])
		if !ok {
			d.warn(CodeWorkspaceUnreadable, WorkspaceFile, "project %s has a source outside the repository: %q", id, sources[id])
			continue
		}
		if !isDir(d.root, dir) {
			d.warn(CodeWorkspaceUnreadable, WorkspaceFile, "project %s names the directory %s, which does not exist", id, dir)
			continue
		}
		add(id, dir)
	}
	for _, dir := range d.globbed(globs) {
		if _, taken := owners[dir]; taken {
			continue
		}
		id := path.Base(dir)
		if byPath {
			id = dir
		}
		if dir == workspace.ProbeRootPath {
			id = filepath.Base(d.root)
		}
		add(id, dir)
	}
	return projects, nil
}

// globbed returns the directories the globs select, in path order: a
// directory a glob matches, or one that holds the moon.yml a glob matches,
// less those a ! glob matches. "." selects the root. Hidden, node_modules
// and .moon directories are never selected.
func (d *detector) globbed(globs []string) []string {
	var include, exclude [][]string
	root := false
	for _, glob := range globs {
		negated := strings.HasPrefix(glob, "!")
		glob = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(glob, "!"), "./"), "/")
		if glob == "" {
			continue
		}
		if glob == "." {
			root = root || !negated
			continue
		}
		for _, expanded := range expandBraces(glob) {
			if negated {
				exclude = append(exclude, strings.Split(expanded, "/"))
			} else {
				include = append(include, strings.Split(expanded, "/"))
			}
		}
	}
	var out []string
	if root {
		out = append(out, workspace.ProbeRootPath)
	}
	if len(include) == 0 {
		return out
	}
	_ = filepath.WalkDir(d.root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() || p == d.root {
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" {
			return filepath.SkipDir
		}
		dir := relative(d.root, p)
		segments := strings.Split(dir, "/")
		withFile := append(slices.Clone(segments), ProjectFile)
		selected := false
		for _, pattern := range include {
			if matchSegments(pattern, segments) || matchSegments(pattern, withFile) && exists(d.root, path.Join(dir, ProjectFile)) {
				selected = true
				break
			}
		}
		for _, pattern := range exclude {
			if matchSegments(pattern, segments) {
				selected = false
			}
		}
		if selected {
			out = append(out, dir)
		}
		return nil
	})
	return out
}

// edges reads each project's moon.yml and package.json and resolves its
// declared and package dependencies to project directories.
func (d *detector) edges(projects []*project) {
	for _, p := range projects {
		d.readProjectFile(p)
		d.readPackage(p)
	}
	byID := map[string]*project{}
	byAlias := map[string]*project{}
	for _, p := range projects {
		byID[p.id] = p
	}
	for _, p := range projects {
		if p.alias != "" && byID[p.alias] == nil {
			byAlias[p.alias] = p
		}
	}
	resolve := func(name string) *project {
		if p := byID[name]; p != nil {
			return p
		}
		return byAlias[name]
	}
	for _, p := range projects {
		// A dependency of root scope, as moon gives a task or package
		// dependency on the root project, is no edge.
		link := func(target *project, rootScope bool) {
			if target == nil || target == p || rootScope && target.dir == workspace.ProbeRootPath || slices.Contains(p.deps, target.dir) {
				return
			}
			p.deps = append(p.deps, target.dir)
		}
		for _, entry := range p.declared {
			switch v := entry.(type) {
			case string:
				link(resolve(v), false)
			case map[string]any:
				if id, ok := v["id"].(string); ok {
					link(resolve(id), v["scope"] == "root")
				}
			}
		}
		for _, name := range p.taskDeps {
			link(resolve(name), true)
		}
		for _, name := range p.packages {
			link(byAlias[name], true)
		}
	}
}

// readProjectFile reads the id, the dependsOn and the task deps of a
// project's moon.yml. Tasks that do not read cost the task edges only.
func (d *detector) readProjectFile(p *project) {
	rel := path.Join(p.dir, ProjectFile)
	data, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	p.sourceFile = rel
	p.watched = append(p.watched, rel)
	if err == nil {
		err = p.readProjectConfig(string(data))
	}
	if err != nil {
		d.warn(CodeManifestUnreadable, rel, "%s cannot be read: %v; project %s keeps no declared edge", rel, err, p.id)
		return
	}
	if err := p.readTaskDeps(string(data)); err != nil {
		d.warn(CodeManifestUnreadable, rel, "the tasks of %s cannot be read: %v; project %s keeps no task edge", rel, err, p.id)
	}
}

func (p *project) readProjectConfig(text string) error {
	id, _, err := topLevel(text, "id")
	if err != nil {
		return err
	}
	dependsOn, _, err := topLevel(text, "dependsOn")
	if err != nil {
		return err
	}
	switch v := dependsOn.(type) {
	case nil:
	case []any:
		p.declared = v
	default:
		return errors.New("dependsOn is not a list")
	}
	if s, ok := id.(string); ok && s != "" {
		p.id = s
	}
	return nil
}

// readTaskDeps reads the projects the deps of each task name: a target
// <project>:<task>, as a string or as an object with a target. A target of
// the project itself (~ or no project), of its dependencies (^) or of a tag
// (#tag) names no other project.
func (p *project) readTaskDeps(text string) error {
	tasks, _, err := topLevel(text, "tasks")
	if err != nil {
		return err
	}
	byName, _ := tasks.(map[string]any)
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		task, _ := byName[name].(map[string]any)
		deps, _ := task["deps"].([]any)
		for _, dep := range deps {
			target, ok := dep.(string)
			if object, isObject := dep.(map[string]any); isObject {
				target, ok = object["target"].(string)
			}
			project, _, scoped := strings.Cut(target, ":")
			if !ok || !scoped || project == "" || strings.ContainsAny(project[:1], "^~#") {
				continue
			}
			p.taskDeps = append(p.taskDeps, project)
		}
	}
	return nil
}

type packageJSON struct {
	Name                 string            `json:"name"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

// readPackage reads the name and the dependencies of a project's
// package.json: the name is the project's alias, as moon reads it.
func (d *detector) readPackage(p *project) {
	rel := path.Join(p.dir, "package.json")
	data, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	p.watched = append(p.watched, rel)
	var pkg packageJSON
	if err == nil {
		err = json.Unmarshal(data, &pkg)
	}
	if err != nil {
		d.warn(CodeManifestUnreadable, rel, "%s cannot be read as JSON: %v; project %s keeps no package edge", rel, err, p.id)
		return
	}
	p.alias = pkg.Name
	for _, group := range []map[string]string{pkg.Dependencies, pkg.DevDependencies, pkg.PeerDependencies, pkg.OptionalDependencies} {
		for name := range group {
			p.packages = append(p.packages, name)
		}
	}
	sort.Strings(p.packages)
}

// projectFile names the moon.yml of a project directory, when there is one.
func (d *detector) projectFile(dir string) string {
	if rel := path.Join(dir, ProjectFile); exists(d.root, rel) {
		return rel
	}
	return ""
}

// --- helpers ----------------------------------------------------------------

func stringList(value any, field string) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a list of globs", field)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s holds an entry that is not a glob", field)
		}
		out = append(out, s)
	}
	return out, nil
}

// expandBraces expands each {a,b} alternation of a glob.
func expandBraces(glob string) []string {
	open := strings.IndexByte(glob, '{')
	if open < 0 {
		return []string{glob}
	}
	end := strings.IndexByte(glob[open:], '}')
	if end < 0 {
		return []string{glob}
	}
	var out []string
	for _, alternative := range strings.Split(glob[open+1:open+end], ",") {
		out = append(out, expandBraces(glob[:open]+alternative+glob[open+end+1:])...)
	}
	return out
}

// matchSegments matches a path by segments: * within one segment and **
// across any number.
func matchSegments(pattern, dir []string) bool {
	if len(pattern) == 0 {
		return len(dir) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(dir); i++ {
			if matchSegments(pattern[1:], dir[i:]) {
				return true
			}
		}
		return false
	}
	if len(dir) == 0 {
		return false
	}
	if matched, _ := path.Match(pattern[0], dir[0]); !matched {
		return false
	}
	return matchSegments(pattern[1:], dir[1:])
}

// keep returns the projects whose path the request names, or every project
// when it names none.
func keep(projects []workspace.ProbeProject, paths []string) []workspace.ProbeProject {
	if len(paths) == 0 {
		return projects
	}
	wanted := map[string]bool{}
	for _, p := range paths {
		if normalized, ok := workspace.NormalizeProbePath(p); ok {
			wanted[normalized] = true
		}
	}
	return slices.DeleteFunc(projects, func(p workspace.ProbeProject) bool { return !wanted[p.Path] })
}

func exists(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && !info.IsDir()
}

func isDir(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && info.IsDir()
}

func relative(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}
