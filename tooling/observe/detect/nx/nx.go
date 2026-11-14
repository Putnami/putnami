// Package nx detects the projects of an Nx workspace and the edges between
// them, without running nx.
//
// A graph export the CI wrote with `nx graph --file=graph.json` is the
// authority. Without one, a root nx.json makes the repository an Nx workspace,
// and its project.json files and workspace package.json files are read
// statically. The answer is a workspace probe result, so the detector can move
// to an extension of its own without a contract change.
package nx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/workspace"
)

// Extension is the extension the probe result speaks for.
const Extension = "@putnami/observe"

// Ecosystem is the component ecosystem of an Nx project.
const Ecosystem = "nx"

// DefaultGraphFile is where the detector looks for a graph export when the
// caller names none.
const DefaultGraphFile = "graph.json"

// Diagnostic codes.
const (
	CodeGraphUnreadable    = "observe.nx_graph_unreadable"
	CodeManifestUnreadable = "observe.nx_manifest_unreadable"
)

// lockfiles are the root lockfiles a Node workspace may hold, in the order the
// detector picks the first.
var lockfiles = []string{"pnpm-lock.yaml", "yarn.lock", "package-lock.json", "bun.lock", "bun.lockb"}

// Metadata is what the detector tells the change analysis about one project,
// carried as the probe project's metadata.
type Metadata struct {
	// Name is the Nx project name.
	Name string `json:"name"`
	// Ecosystem is always "nx".
	Ecosystem string `json:"ecosystem"`
	// Source is evidence.SourceOrchestrator for a graph export and
	// evidence.SourceWorkspace for the Nx configuration.
	Source string `json:"source"`
	// TestOutputs are the repository-relative files or directories the
	// project's test targets write, where its test and coverage reports land.
	TestOutputs []string `json:"testOutputs,omitempty"`
	// Lockfile is the repository-relative lockfile that pins the project's
	// dependencies.
	Lockfile string `json:"lockfile,omitempty"`
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
	// Empty means DefaultGraphFile.
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
	graphFile := opts.GraphFile
	if graphFile == "" {
		graphFile = DefaultGraphFile
	}
	d := &detector{root: root, lockfile: firstExisting(root, lockfiles)}
	projects, watched, ok := d.fromGraph(graphFile, opts.GraphFile != "")
	if !ok {
		projects, watched = d.fromConfiguration()
	}
	if len(projects) == 0 {
		result.Diagnostics = d.diagnostics
		return result
	}
	result.Projects = keep(projects, req.Paths)
	result.WatchedFiles = watched
	result.Diagnostics = d.diagnostics
	workspace.NormalizeProbeResult(&result)
	return result
}

type detector struct {
	root        string
	lockfile    string
	diagnostics []diag.Diagnostic
	// targets are the targets of each project root the detector answered,
	// with the outputs resolved, for Detect.
	targets map[string][]Target
}

// Target is one Nx target of a project.
type Target struct {
	// Name is the target name, such as build or test.
	Name string `json:"name"`
	// Executor is the target's executor, such as @nx/jest:jest, or
	// nx:run-script for a package.json script.
	Executor string `json:"executor,omitempty"`
	// Command is the command the target runs: the package.json script, or
	// options.command or options.commands of nx:run-commands. Empty for an
	// executor that names no command.
	Command string `json:"command,omitempty"`
	// Outputs are the repository-relative outputs of the target, its tokens
	// resolved and any glob cut to the directory before it; the targetDefaults
	// of nx.json answer for a target that names none.
	Outputs []string `json:"outputs,omitempty"`
	// Test reports whether the target runs tests.
	Test bool `json:"test,omitempty"`
	// Options are the options nx run passes the executor: the target laid
	// over its nx.json targetDefaults entry, its executor's, else its name's,
	// else a glob's that matches its name, then its defaultConfiguration's
	// options, with {projectRoot}, {workspaceRoot} and {projectName} replaced
	// in each string. Executor is the one the entry gives a target that names
	// none.
	Options map[string]any `json:"options,omitempty"`
	// DependsOn are the targets Nx runs before this one, in order: the
	// target's own dependsOn, else that of its nx.json targetDefaults entry,
	// with "..." in the target's list standing for the entry's. A target of
	// the projects it depends on reads ^<target>, one of the project itself
	// <target>; any other entry, such as an object naming projects, is kept
	// as its compact JSON.
	DependsOn []string `json:"dependsOn,omitempty"`
}

// Project is one Nx project with its targets.
type Project struct {
	// Path is the repository-relative project root.
	Path string
	// Name is the Nx project name.
	Name string
	// SourceFile is the project's project.json, else its package.json.
	SourceFile string
	// Dependencies are the repository-relative roots of the projects it
	// depends on.
	Dependencies []string
	// Targets are the project's targets, sorted by name.
	Targets []Target
	// FromGraph reports that a graph export answered for the project.
	FromGraph bool
}

// Detection is what Detect found in one repository.
type Detection struct {
	// Projects are the Nx projects, sorted by path.
	Projects []Project
	// Watched are the repository-relative files the detection read.
	Watched []string
	// Diagnostics are the findings of the detection.
	Diagnostics []diag.Diagnostic
}

// Detect reads every project of the Nx workspace at root with its targets:
// from the graph export when one exists, else from nx.json and each
// project.json and workspace package.json. It reads files only.
func Detect(root string, opts Options) Detection {
	graphFile := opts.GraphFile
	if graphFile == "" {
		graphFile = DefaultGraphFile
	}
	d := &detector{root: root, lockfile: firstExisting(root, lockfiles), targets: map[string][]Target{}}
	projects, watched, fromGraph := d.fromGraph(graphFile, opts.GraphFile != "")
	if !fromGraph {
		projects, watched = d.fromConfiguration()
	}
	out := Detection{Watched: watched, Diagnostics: d.diagnostics}
	for _, p := range projects {
		out.Projects = append(out.Projects, Project{
			Path:         p.Path,
			Name:         p.SourceName,
			SourceFile:   p.SourceFile,
			Dependencies: sortedUnique(p.Dependencies),
			Targets:      d.targets[p.Path],
			FromGraph:    fromGraph,
		})
	}
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].Path < out.Projects[j].Path })
	return out
}

func sortedUnique(list []string) []string {
	out := slices.Clone(list)
	slices.Sort(out)
	return slices.Compact(out)
}

func (d *detector) warn(code, field, format string, args ...any) {
	d.diagnostics = append(d.diagnostics, diag.Warningf(code, field, format, args...))
}

// --- graph export -----------------------------------------------------------

type graphExport struct {
	Graph *struct {
		Nodes        map[string]graphNode         `json:"nodes"`
		Dependencies map[string][]graphDependency `json:"dependencies"`
	} `json:"graph"`
}

type graphNode struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Data struct {
		Root    string              `json:"root"`
		Targets map[string]nxTarget `json:"targets"`
		Tags    []string            `json:"tags"`
	} `json:"data"`
}

type graphDependency struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// fromGraph reads the graph export. It reports ok=false when there is no
// usable export. A missing file is no finding; a file that is not an Nx graph
// export is one when the caller named it or the repository holds nx.json.
func (d *detector) fromGraph(graphFile string, named bool) ([]workspace.ProbeProject, []string, bool) {
	file := graphFile
	if !filepath.IsAbs(file) {
		file = filepath.Join(d.root, filepath.FromSlash(file))
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false
	}
	if err != nil {
		d.warn(CodeGraphUnreadable, graphFile, "the Nx graph export cannot be read: %v; the Nx configuration answers instead", err)
		return nil, nil, false
	}
	var export graphExport
	if err := json.Unmarshal(data, &export); err != nil || export.Graph == nil || export.Graph.Nodes == nil {
		if !named && !exists(d.root, "nx.json") {
			return nil, nil, false
		}
		d.warn(CodeGraphUnreadable, graphFile, "%s is not an Nx graph export (graph.nodes and graph.dependencies); the Nx configuration answers instead", graphFile)
		return nil, nil, false
	}
	keys := make([]string, 0, len(export.Graph.Nodes))
	rooted := false
	for key, node := range export.Graph.Nodes {
		keys = append(keys, key)
		rooted = rooted || node.Data.Root != ""
	}
	if !rooted {
		if !named && !exists(d.root, "nx.json") {
			return nil, nil, false
		}
		d.warn(CodeGraphUnreadable, graphFile, "%s names no project root; the Nx configuration answers instead", graphFile)
		return nil, nil, false
	}
	sort.Strings(keys)
	roots := make(map[string]string, len(keys))
	owners := make(map[string]string, len(keys))
	for _, key := range keys {
		node := export.Graph.Nodes[key]
		if node.Data.Root == "" {
			d.warn(CodeGraphUnreadable, graphFile, "project %s has no root and is skipped", key)
			continue
		}
		root, ok := workspace.NormalizeProbePath(node.Data.Root)
		if !ok {
			d.warn(CodeGraphUnreadable, graphFile, "project %s has a root outside the repository: %q", key, node.Data.Root)
			continue
		}
		if other, taken := owners[root]; taken {
			d.warn(CodeGraphUnreadable, graphFile, "projects %s and %s share the root %s; %s is skipped", other, key, root, key)
			continue
		}
		owners[root] = key
		roots[key] = root
	}
	defaults := d.targetDefaults()
	var projects []workspace.ProbeProject
	for _, key := range keys {
		node := export.Graph.Nodes[key]
		root, ok := roots[key]
		if !ok {
			continue
		}
		name := node.Name
		if name == "" {
			name = key
		}
		var deps []string
		for _, edge := range export.Graph.Dependencies[key] {
			if target, ok := roots[edge.Target]; ok && edge.Target != key {
				deps = append(deps, target)
			}
		}
		project := workspace.ProbeProject{
			Path:         root,
			SourceName:   name,
			SourceFile:   d.manifestOf(root),
			Type:         projectType(node.Type),
			Tags:         node.Data.Tags,
			Dependencies: deps,
		}
		project.Metadata = d.metadata(name, evidence.SourceOrchestrator, root, node.Data.Targets, defaults)
		projects = append(projects, project)
	}
	watched := []string{relative(d.root, file)}
	if exists(d.root, "nx.json") {
		watched = append(watched, "nx.json")
	}
	return projects, watched, true
}

// manifestOf names the Nx manifest of a project root: its project.json, else
// its package.json.
func (d *detector) manifestOf(root string) string {
	for _, name := range []string{"project.json", "package.json"} {
		if p := path.Join(root, name); exists(d.root, p) {
			return p
		}
	}
	return ""
}

func projectType(t string) string {
	switch t {
	case "app", "e2e":
		return "application"
	case "lib":
		return "library"
	}
	return ""
}

// --- configuration ----------------------------------------------------------

type nxTarget struct {
	Executor             string                    `json:"executor"`
	Command              string                    `json:"command"`
	Outputs              []string                  `json:"outputs"`
	Options              map[string]any            `json:"options"`
	Configurations       map[string]map[string]any `json:"configurations"`
	DefaultConfiguration string                    `json:"defaultConfiguration"`
	// DependsOn is nil when the target states none, and empty when it states
	// an empty list.
	DependsOn []json.RawMessage `json:"dependsOn"`
}

type projectJSON struct {
	Name                 string              `json:"name"`
	ProjectType          string              `json:"projectType"`
	Tags                 []string            `json:"tags"`
	ImplicitDependencies []string            `json:"implicitDependencies"`
	Targets              map[string]nxTarget `json:"targets"`
}

type packageJSON struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Workspaces           json.RawMessage   `json:"workspaces"`
	Scripts              map[string]string `json:"scripts"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	Nx                   *projectJSON      `json:"nx"`
}

// candidate is one project directory of the configuration.
type candidate struct {
	root        string
	project     *projectJSON
	pkg         *packageJSON
	name        string
	sourceFile  string
	watched     []string
	implicit    []string
	targets     map[string]nxTarget
	projectType string
	tags        []string
}

// fromConfiguration reads nx.json, project.json and the workspace
// package.json files.
func (d *detector) fromConfiguration() ([]workspace.ProbeProject, []string) {
	if !exists(d.root, "nx.json") {
		return nil, nil
	}
	patterns, workspaceFiles := d.workspacePatterns()
	watched := append([]string{"nx.json"}, workspaceFiles...)
	projectDirs := map[string]bool{}
	packageDirs := map[string]bool{}
	_ = filepath.WalkDir(d.root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if p != d.root && skipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		dir := path.Dir(relative(d.root, p))
		switch entry.Name() {
		case "project.json":
			projectDirs[dir] = true
		case "package.json":
			packageDirs[dir] = true
		}
		return nil
	})
	// A project is a directory with a project.json, or one whose package.json
	// the workspace lists; the package.json beside a project.json is read in
	// both cases.
	dirs := make([]string, 0, len(projectDirs)+len(packageDirs))
	for dir := range projectDirs {
		dirs = append(dirs, dir)
	}
	for dir := range packageDirs {
		if !projectDirs[dir] && dir != "." && matchesAny(patterns, dir) {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	candidates := make([]*candidate, 0, len(dirs))
	for _, dir := range dirs {
		c := &candidate{root: dir}
		if projectDirs[dir] {
			var project projectJSON
			if !d.readJSON(path.Join(dir, "project.json"), &project) {
				continue
			}
			c.project = &project
		}
		if packageDirs[dir] {
			var pkg packageJSON
			if d.readJSON(path.Join(dir, "package.json"), &pkg) {
				c.pkg = &pkg
			} else if c.project == nil {
				continue
			}
		}
		d.resolve(c)
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return nil, watched
	}
	packages := map[string]string{}
	for _, c := range candidates {
		if c.pkg != nil && c.pkg.Name != "" {
			packages[c.pkg.Name] = c.root
		}
	}
	defaults := d.targetDefaults()
	projects := make([]workspace.ProbeProject, 0, len(candidates))
	for _, c := range candidates {
		selected, excluded := implicitDependencies(c, candidates)
		deps := selected
		if c.pkg != nil {
			for _, group := range []map[string]string{c.pkg.Dependencies, c.pkg.DevDependencies, c.pkg.PeerDependencies, c.pkg.OptionalDependencies} {
				for dep := range group {
					if root, ok := packages[dep]; ok && root != c.root && !excluded[root] {
						deps = append(deps, root)
					}
				}
			}
		}
		project := workspace.ProbeProject{
			Path:         c.root,
			SourceName:   c.name,
			SourceFile:   c.sourceFile,
			Type:         c.projectType,
			Tags:         c.tags,
			Dependencies: deps,
			WatchedFiles: c.watched,
		}
		if c.pkg != nil {
			project.Version = c.pkg.Version
		}
		project.Metadata = d.metadata(c.name, evidence.SourceWorkspace, c.root, c.targets, defaults)
		projects = append(projects, project)
	}
	return projects, watched
}

// resolve merges a directory's project.json and package.json the way Nx does:
// project.json wins on the name, and the targets of both are merged.
func (d *detector) resolve(c *candidate) {
	c.targets = map[string]nxTarget{}
	if c.pkg != nil {
		c.watched = append(c.watched, path.Join(c.root, "package.json"))
		c.sourceFile = path.Join(c.root, "package.json")
		c.name = c.pkg.Name
		for script, command := range c.pkg.Scripts {
			c.targets[script] = nxTarget{Executor: "nx:run-script", Command: command}
		}
		if c.pkg.Nx != nil {
			if c.pkg.Nx.Name != "" {
				c.name = c.pkg.Nx.Name
			}
			c.implicit = append(c.implicit, c.pkg.Nx.ImplicitDependencies...)
			c.tags = append(c.tags, c.pkg.Nx.Tags...)
			mergeTargets(c.targets, c.pkg.Nx.Targets)
			c.projectType = projectTypeOf(c.pkg.Nx.ProjectType)
		}
	}
	if c.project != nil {
		c.watched = append(c.watched, path.Join(c.root, "project.json"))
		c.sourceFile = path.Join(c.root, "project.json")
		if c.project.Name != "" {
			c.name = c.project.Name
		}
		c.implicit = append(c.implicit, c.project.ImplicitDependencies...)
		c.tags = append(c.tags, c.project.Tags...)
		mergeTargets(c.targets, c.project.Targets)
		if t := projectTypeOf(c.project.ProjectType); t != "" {
			c.projectType = t
		}
	}
	if c.name == "" {
		c.name = path.Base(c.root)
	}
}

// mergeTargets lays the targets of a manifest over those already known: a
// field the manifest leaves empty keeps the earlier value.
func mergeTargets(into, from map[string]nxTarget) {
	for name, target := range from {
		earlier := into[name]
		if target.Executor == "" {
			target.Executor = earlier.Executor
		}
		if target.Command == "" {
			target.Command = earlier.Command
		}
		if target.Outputs == nil {
			target.Outputs = earlier.Outputs
		}
		if target.Options == nil {
			target.Options = earlier.Options
		}
		if target.Configurations == nil {
			target.Configurations = earlier.Configurations
		}
		if target.DefaultConfiguration == "" {
			target.DefaultConfiguration = earlier.DefaultConfiguration
		}
		if target.DependsOn == nil {
			target.DependsOn = earlier.DependsOn
		}
		into[name] = target
	}
}

func projectTypeOf(t string) string {
	switch t {
	case "application":
		return "application"
	case "library":
		return "library"
	}
	return ""
}

// implicitDependencies resolves the Nx implicitDependencies of c against the
// other projects: a name, a * pattern that matches across /, or tag:<tag>,
// each !-prefixed to exclude. It returns the selected roots and the roots an
// exclusion names, which no package dependency of c may add back.
func implicitDependencies(c *candidate, candidates []*candidate) ([]string, map[string]bool) {
	if len(c.implicit) == 0 {
		return nil, nil
	}
	selected := map[string]bool{}
	excluded := map[string]bool{}
	for _, entry := range c.implicit {
		exclude := strings.HasPrefix(entry, "!")
		pattern := strings.TrimPrefix(entry, "!")
		for _, other := range candidates {
			if other == c || !selects(pattern, other) {
				continue
			}
			selected[other.root] = !exclude
			if exclude {
				excluded[other.root] = true
			}
		}
	}
	var out []string
	for _, other := range candidates {
		if selected[other.root] {
			out = append(out, other.root)
		}
	}
	return out, excluded
}

// selects reports whether an implicitDependencies pattern names a project.
func selects(pattern string, c *candidate) bool {
	if tag, ok := strings.CutPrefix(pattern, "tag:"); ok {
		return slices.Contains(c.tags, tag)
	}
	return wildcard(pattern, c.name)
}

// wildcard matches a name against a pattern whose * matches any run of
// characters, / included.
func wildcard(pattern, name string) bool {
	head, rest, found := strings.Cut(pattern, "*")
	if !found {
		return pattern == name
	}
	if !strings.HasPrefix(name, head) {
		return false
	}
	name = name[len(head):]
	for i := 0; i <= len(name); i++ {
		if wildcard(rest, name[i:]) {
			return true
		}
	}
	return false
}

// workspacePatterns reads the package manager workspace: the union of
// package.json workspaces, pnpm-workspace.yaml packages and lerna.json
// packages. It returns the directory patterns and the files it read.
func (d *detector) workspacePatterns() ([]string, []string) {
	var patterns, files []string
	if exists(d.root, "package.json") {
		files = append(files, "package.json")
		var root packageJSON
		if d.readJSON("package.json", &root) {
			patterns = append(patterns, workspaceList(root.Workspaces)...)
		}
	}
	if data, err := os.ReadFile(filepath.Join(d.root, "pnpm-workspace.yaml")); err == nil {
		files = append(files, "pnpm-workspace.yaml")
		patterns = append(patterns, pnpmPackages(string(data))...)
	}
	if exists(d.root, "lerna.json") {
		files = append(files, "lerna.json")
		var lerna struct {
			Packages []string `json:"packages"`
		}
		if d.readJSON("lerna.json", &lerna) {
			patterns = append(patterns, lerna.Packages...)
		}
	}
	return patterns, files
}

// workspaceList reads package.json workspaces, an array or an object with a
// packages array.
func workspaceList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var object struct {
		Packages []string `json:"packages"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return object.Packages
	}
	return nil
}

// pnpmPackages reads the packages list of pnpm-workspace.yaml: the block
// or flow sequence under a top-level packages key.
func pnpmPackages(yaml string) []string {
	var out []string
	inPackages := false
	for _, line := range strings.Split(yaml, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "-") {
			value, isPackages := strings.CutPrefix(trimmed, "packages:")
			inPackages = isPackages
			if flow := strings.TrimSpace(value); isPackages && strings.HasPrefix(flow, "[") {
				flow, _, _ = strings.Cut(strings.TrimPrefix(flow, "["), "]")
				for _, item := range strings.Split(flow, ",") {
					if item = strings.Trim(strings.TrimSpace(item), `"'`); item != "" {
						out = append(out, item)
					}
				}
				inPackages = false
			}
			continue
		}
		if !inPackages || !strings.HasPrefix(trimmed, "-") {
			continue
		}
		item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		if i := strings.Index(item, " #"); i >= 0 {
			item = strings.TrimSpace(item[:i])
		}
		item = strings.Trim(item, `"'`)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// matchesAny reports whether dir is listed by the workspace patterns: a
// pattern matches by path segments, * within one segment and ** across any
// number, and a later !-prefixed pattern removes what an earlier one added.
func matchesAny(patterns []string, dir string) bool {
	listed := false
	for _, pattern := range patterns {
		exclude := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(pattern, "!"), "./"), "/")
		if pattern == "" {
			continue
		}
		if matchSegments(strings.Split(pattern, "/"), strings.Split(dir, "/")) {
			listed = !exclude
		}
	}
	return listed
}

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

// skipDir names the directories the configuration read does not enter.
func skipDir(name string) bool {
	return name == "node_modules" || name == "dist" || strings.HasPrefix(name, ".")
}

// --- test outputs and metadata ---------------------------------------------

type nxJSON struct {
	TargetDefaults map[string]nxTarget `json:"targetDefaults"`
}

// targetDefaults reads nx.json targetDefaults, keyed by target name or
// executor.
func (d *detector) targetDefaults() map[string]nxTarget {
	if !exists(d.root, "nx.json") {
		return nil
	}
	var config nxJSON
	if !d.readJSON("nx.json", &config) {
		return nil
	}
	return config.TargetDefaults
}

var testRunners = []string{"jest", "vitest", "playwright", "cypress"}

// isTestTarget reports whether a target runs tests: by its name, or by the
// executor or the runner its executor or command names.
func isTestTarget(name string, target nxTarget) bool {
	if name == "test" || name == "e2e" || strings.HasPrefix(name, "test-") || strings.HasSuffix(name, "-test") || strings.HasPrefix(name, "test:") {
		return true
	}
	if strings.HasSuffix(target.Executor, ":test") {
		return true
	}
	runs := strings.ToLower(target.Executor + " " + target.Command)
	if command, ok := target.Options["command"].(string); ok {
		runs += " " + strings.ToLower(command)
	}
	for _, runner := range testRunners {
		if strings.Contains(runs, runner) {
			return true
		}
	}
	return false
}

func (d *detector) metadata(name, source, root string, targets map[string]nxTarget, defaults map[string]nxTarget) json.RawMessage {
	m := Metadata{Name: name, Ecosystem: Ecosystem, Source: source, Lockfile: d.lockfile}
	seen := map[string]bool{}
	targetNames := make([]string, 0, len(targets))
	for targetName := range targets {
		targetNames = append(targetNames, targetName)
	}
	sort.Strings(targetNames)
	for _, targetName := range targetNames {
		target := targets[targetName]
		outputs := target.Outputs
		if outputs == nil {
			if byName, ok := defaults[targetName]; ok {
				outputs = byName.Outputs
			}
		}
		if outputs == nil && target.Executor != "" {
			if byExecutor, ok := defaults[target.Executor]; ok {
				outputs = byExecutor.Outputs
			}
		}
		effective := withDefaults(targetName, target, defaults)
		test := isTestTarget(targetName, effective)
		described := Target{Name: targetName, Executor: effective.Executor, Command: targetCommand(target), Test: test,
			Options: effectiveOptions(effective, root, name), DependsOn: dependsOn(effective.DependsOn)}
		if script, ok := target.Options["script"].(string); ok && described.Command == "" && target.Executor == "nx:run-script" {
			described.Command = d.script(root, script)
		}
		for _, output := range outputs {
			resolved, ok := resolveOutput(output, root, name, effectiveOptions(effective, root, name))
			if !ok {
				continue
			}
			described.Outputs = append(described.Outputs, resolved)
			if test && !seen[resolved] {
				seen[resolved] = true
				m.TestOutputs = append(m.TestOutputs, resolved)
			}
		}
		if d.targets != nil {
			d.targets[root] = append(d.targets[root], described)
		}
	}
	sort.Strings(m.TestOutputs)
	data, _ := json.Marshal(m)
	return data
}

// withDefaults lays a target over its nx.json targetDefaults entry as Nx
// does: the entry of its executor, else of its name, else of the first glob
// key, in sorted order, that matches its name; an entry whose executor
// differs from the target's applies to none. The target's executor, options,
// defaultConfiguration and each configuration's options win over the entry's,
// key by key.
func withDefaults(targetName string, target nxTarget, defaults map[string]nxTarget) nxTarget {
	entry, ok := defaultsFor(targetName, target.Executor, defaults)
	if !ok {
		target.DependsOn = spread(nil, target.DependsOn)
		return target
	}
	merged := target
	if merged.Executor == "" {
		merged.Executor = entry.Executor
	}
	if merged.DefaultConfiguration == "" {
		merged.DefaultConfiguration = entry.DefaultConfiguration
	}
	merged.Options = overlay(entry.Options, target.Options)
	merged.DependsOn = spread(entry.DependsOn, target.DependsOn)
	if len(entry.Configurations) > 0 {
		merged.Configurations = map[string]map[string]any{}
		for name, options := range entry.Configurations {
			merged.Configurations[name] = overlay(options, nil)
		}
		for name, options := range target.Configurations {
			merged.Configurations[name] = overlay(merged.Configurations[name], options)
		}
	}
	return merged
}

// defaultsFor finds the targetDefaults entry of a target (see withDefaults).
func defaultsFor(targetName, executor string, defaults map[string]nxTarget) (nxTarget, bool) {
	compatible := func(entry nxTarget) bool {
		return entry.Executor == "" || executor == "" || entry.Executor == executor
	}
	if entry, ok := defaults[executor]; ok && executor != "" {
		return entry, true
	}
	if entry, ok := defaults[targetName]; ok {
		return entry, compatible(entry)
	}
	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.ContainsAny(key, "*?[") && !strings.Contains(key, ":") {
			if matched, _ := path.Match(key, targetName); matched {
				return defaults[key], compatible(defaults[key])
			}
		}
	}
	return nxTarget{}, false
}

// spreadEntry is the entry of a target's dependsOn that stands for the list
// of its targetDefaults entry.
const spreadEntry = `"..."`

// spread answers the dependsOn of a target laid over its targetDefaults
// entry's: the target's list when it states one, with "..." replaced by the
// entry's, which is none for a target no entry applies to, else the entry's.
func spread(base, top []json.RawMessage) []json.RawMessage {
	if top == nil {
		return base
	}
	out := make([]json.RawMessage, 0, len(top))
	for _, entry := range top {
		if string(entry) == spreadEntry {
			out = append(out, base...)
			continue
		}
		out = append(out, entry)
	}
	return out
}

// dependsOn names each entry of a resolved dependsOn: a string as it is, an
// object naming a target of the projects the project depends on as
// ^<target>, one naming a target of the project itself as <target>, and any
// other entry as its compact JSON.
func dependsOn(entries []json.RawMessage) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		var name string
		if json.Unmarshal(entry, &name) == nil {
			out = append(out, name)
			continue
		}
		var object struct {
			Target       string          `json:"target"`
			Projects     json.RawMessage `json:"projects"`
			Dependencies bool            `json:"dependencies"`
			Params       json.RawMessage `json:"params"`
		}
		decoder := json.NewDecoder(bytes.NewReader(entry))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&object) == nil && object.Target != "" {
			projects := ""
			if len(object.Projects) > 0 && json.Unmarshal(object.Projects, &projects) != nil {
				projects = "list"
			}
			switch {
			case object.Dependencies && projects == "":
				out = append(out, "^"+object.Target)
				continue
			case !object.Dependencies && projects == "dependencies":
				out = append(out, "^"+object.Target)
				continue
			case !object.Dependencies && (projects == "" || projects == "self"):
				out = append(out, object.Target)
				continue
			}
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, entry); err != nil {
			out = append(out, string(entry))
			continue
		}
		out = append(out, compact.String())
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// overlay returns the keys of base replaced by those of top, or nil when
// neither holds one.
func overlay(base, top map[string]any) map[string]any {
	if len(base) == 0 && len(top) == 0 {
		return nil
	}
	merged := make(map[string]any, len(base)+len(top))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range top {
		merged[key] = value
	}
	return merged
}

// effectiveOptions returns the options nx run passes the executor of a target
// already laid over its defaults: its options, then those of its
// defaultConfiguration. The tokens {projectRoot}, {workspaceRoot} and
// {projectName} are replaced in each string, at any depth, a leading
// {workspaceRoot}/ removed as Nx does. It returns nil when neither sets one.
func effectiveOptions(target nxTarget, root, name string) map[string]any {
	merged := overlay(target.Options, target.Configurations[target.DefaultConfiguration])
	if merged == nil {
		return nil
	}
	projectRoot := root
	if projectRoot == workspace.ProbeRootPath {
		projectRoot = "."
	}
	replacer := strings.NewReplacer("{projectRoot}", projectRoot, "{workspaceRoot}", ".", "{projectName}", name)
	for key, value := range merged {
		merged[key] = replaceTokens(value, replacer)
	}
	return merged
}

// replaceTokens replaces the tokens of every string in an option value.
func replaceTokens(value any, replacer *strings.Replacer) any {
	switch v := value.(type) {
	case string:
		return replacer.Replace(strings.TrimPrefix(v, "{workspaceRoot}/"))
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = replaceTokens(item, replacer)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = replaceTokens(item, replacer)
		}
		return out
	}
	return value
}

// script reads the command of a package.json script of a project root; an
// unreadable manifest or a missing script reads as empty.
func (d *detector) script(root, name string) string {
	data, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(root), "package.json"))
	if err != nil {
		return ""
	}
	var pkg packageJSON
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return pkg.Scripts[name]
}

// targetCommand is the command a target runs: its package.json script, or
// options.command, else options.commands joined by " && ", of
// nx:run-commands.
func targetCommand(target nxTarget) string {
	if target.Command != "" {
		return target.Command
	}
	if command, ok := target.Options["command"].(string); ok {
		return command
	}
	commands, _ := target.Options["commands"].([]any)
	var parts []string
	for _, c := range commands {
		switch v := c.(type) {
		case string:
			parts = append(parts, v)
		case map[string]any:
			if s, ok := v["command"].(string); ok {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, " && ")
}

// resolveOutput turns one Nx output into a repository-relative path: its
// tokens resolved and any glob cut to the directory before it. It reports
// false for an exclusion, an absolute path, an unresolved token, and a path
// outside the repository.
func resolveOutput(output, root, name string, options map[string]any) (string, bool) {
	if strings.HasPrefix(output, "!") || strings.HasPrefix(output, "/") {
		return "", false
	}
	projectRoot := root
	if projectRoot == workspace.ProbeRootPath {
		projectRoot = ""
	}
	replacer := strings.NewReplacer("{projectRoot}", projectRoot, "{workspaceRoot}", "", "{projectName}", name)
	resolved := replacer.Replace(output)
	for {
		start := strings.Index(resolved, "{options.")
		if start < 0 {
			break
		}
		end := strings.Index(resolved[start:], "}")
		if end < 0 {
			return "", false
		}
		key := resolved[start+len("{options.") : start+end]
		value, ok := options[key].(string)
		if !ok || value == "" {
			return "", false
		}
		resolved = resolved[:start] + value + resolved[start+end+1:]
	}
	if strings.Contains(resolved, "{") {
		return "", false
	}
	if i := strings.IndexAny(resolved, "*?[{"); i >= 0 {
		resolved = path.Dir(resolved[:i] + "x")
	}
	return workspace.NormalizeProbePath(strings.TrimPrefix(resolved, "/"))
}

// --- helpers ----------------------------------------------------------------

// readJSON decodes a repository-relative JSON file; a failure is a diagnostic.
func (d *detector) readJSON(rel string, into any) bool {
	data, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(rel)))
	if err == nil {
		err = json.Unmarshal(data, into)
	}
	if err != nil {
		d.warn(CodeManifestUnreadable, rel, "%s cannot be read as JSON: %v; its directory is skipped", rel, err)
		return false
	}
	return true
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

func firstExisting(root string, names []string) string {
	for _, name := range names {
		if exists(root, name) {
			return name
		}
	}
	return ""
}

func relative(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}
