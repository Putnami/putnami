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
	Executor string         `json:"executor"`
	Command  string         `json:"command"`
	Outputs  []string       `json:"outputs"`
	Options  map[string]any `json:"options"`
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
		if !isTestTarget(targetName, target) {
			continue
		}
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
		for _, output := range outputs {
			resolved, ok := resolveOutput(output, root, name, target.Options)
			if ok && !seen[resolved] {
				seen[resolved] = true
				m.TestOutputs = append(m.TestOutputs, resolved)
			}
		}
	}
	sort.Strings(m.TestOutputs)
	data, _ := json.Marshal(m)
	return data
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
