// Package python is the Python detector of @putnami/observe: it finds the
// projects of a Python repository, such as a uv workspace that holds an API,
// a worker and the library they share, from its pyproject.toml manifests,
// without running uv, Poetry or pip.
//
// It answers over the workspace probe wire (go.putnami.dev/protocol/workspace):
// Probe has the shape of a probe answer, and its result passes the probe
// protocol's strict validation, so the detector can move to its own extension
// once the CLI can run a probe outside a workspace. Outside a workspace no core
// lists the candidate directories, so Candidates walks the repository for
// them.
//
// A member of the root's uv workspace is a workspace component; any other
// pyproject.toml with a [project] or [tool.poetry] table is a manifest one.
// A uv source that names a workspace member or a local path, and a Poetry
// dependency with a local path, is an edge.
package python

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/detect/memberglob"
	"go.putnami.dev/tooling/observe/internal/toml"
)

// ExtensionName is the extension the detector answers under; it keys the
// provider-owned metadata of each project.
const ExtensionName = "@putnami/observe"

// Ecosystem is the evidence ecosystem slug of a Python component.
const Ecosystem = "python"

// Files the detector reads.
const (
	Marker     = "pyproject.toml"
	UVLock     = "uv.lock"
	PoetryLock = "poetry.lock"
)

// Component sources, as protocol/evidence names them.
const (
	SourceWorkspace = "workspace"
	SourceManifest  = "manifest"
)

// Diagnostic codes.
const (
	CodeManifestInvalid = "observe.python_manifest_invalid"
	CodeWalkBounded     = "observe.python_walk_bounded"
)

// MaxWalkedEntries bounds the directory entries Candidates visits.
const MaxWalkedEntries = 200000

// skippedDirs are never walked: installed packages and caches.
var skippedDirs = map[string]bool{"node_modules": true, "vendor": true, "__pycache__": true, "site-packages": true}

// skipped reports whether a walk leaves the directory of that name out.
func skipped(name string) bool {
	return strings.HasPrefix(name, ".") || skippedDirs[name]
}

// Metadata is what the detector knows about one component beyond the probe
// project's own members. Every path is repository-relative and
// slash-separated.
type Metadata struct {
	// Source is workspace for a uv workspace member, manifest otherwise.
	Source string `json:"source"`
	// Lockfile is the uv.lock or poetry.lock beside the manifest, else the
	// workspace's uv.lock for a member.
	Lockfile string `json:"lockfile,omitempty"`
}

// Detect finds the components of the repository at root.
func Detect(root string) wsproto.ProbeResult {
	paths, diags := Candidates(root)
	result := Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: ExtensionName, Paths: paths})
	result.Diagnostics = append(diags, result.Diagnostics...)
	return result
}

// Answer returns the probe answer of the repository at root, the function
// wsproto.ServeProbe serves.
func Answer(root string) wsproto.ProbeAnswerFunc {
	return func(request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
		return Probe(root, request), nil
	}
}

// Candidates walks the repository at root and returns, in path order, the
// directories that hold a pyproject.toml. It skips node_modules, vendor,
// __pycache__, site-packages and hidden directories, such as .git and .venv.
func Candidates(root string) ([]string, []diag.Diagnostic) {
	var found []string
	var diags []diag.Diagnostic
	walked := 0
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Python detector stopped after %d entries", MaxWalkedEntries))
			return fs.SkipAll
		}
		if entry.IsDir() {
			name := entry.Name()
			if p != root && skipped(name) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != Marker {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return nil
		}
		if dir, ok := wsproto.NormalizeProbePath(filepath.ToSlash(rel)); ok {
			found = append(found, dir)
		}
		return nil
	})
	if err != nil {
		diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Python detector cannot walk the repository: %v", err))
	}
	slices.Sort(found)
	return found, diags
}

// component is one parsed manifest.
type component struct {
	dir      string
	manifest toml.Table
	name     string
	meta     Metadata
}

// workspace is the uv workspace the root pyproject.toml declares.
type workspace struct {
	declared bool
	members  map[string]bool
	sources  toml.Table
}

// Probe answers one probe request against the repository at root: one
// project per requested directory whose pyproject.toml is a uv workspace
// member or has a [project] or [tool.poetry] table. A manifest that does not
// parse is a diagnostic.
func Probe(root string, request wsproto.ProbeRequest) wsproto.ProbeResult {
	result := wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: ExtensionName}
	ws, diags := readWorkspace(root)
	result.Diagnostics = append(result.Diagnostics, diags...)
	var components []*component
	seen := map[string]bool{}
	for _, candidate := range request.Paths {
		dir, ok := wsproto.NormalizeProbePath(candidate)
		if !ok || seen[dir] {
			continue
		}
		seen[dir] = true
		manifest, found, err := readManifest(root, dir)
		if !found {
			continue
		}
		if err != nil {
			file := join(dir, Marker)
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeManifestInvalid, file, "%s is left out: %v", file, err))
			continue
		}
		_, hasProject := manifest["project"].(toml.Table)
		_, hasPoetry := toml.Get(manifest, "tool", "poetry").(toml.Table)
		c := &component{dir: dir, manifest: manifest, name: projectName(manifest), meta: Metadata{Source: SourceManifest}}
		switch {
		case dir == wsproto.ProbeRootPath && ws.declared:
			// The workspace root is a member only when it is a project itself.
			if !hasProject {
				continue
			}
			c.meta.Source = SourceWorkspace
		case ws.members[dir]:
			c.meta.Source = SourceWorkspace
		case !hasProject && !hasPoetry:
			continue
		}
		components = append(components, c)
	}
	slices.SortFunc(components, func(a, b *component) int { return strings.Compare(a.dir, b.dir) })
	byDir := map[string]*component{}
	byName := map[string]string{}
	for _, c := range components {
		byDir[c.dir] = c
	}
	// A workspace member names its project over another directory, else the
	// first directory in path order does.
	for _, workspaceFirst := range []bool{true, false} {
		for _, c := range components {
			if (c.meta.Source == SourceWorkspace) != workspaceFirst || c.name == "" {
				continue
			}
			if _, taken := byName[normalize(c.name)]; !taken {
				byName[normalize(c.name)] = c.dir
			}
		}
	}
	rootLock := existing(root, UVLock)
	for _, c := range components {
		c.meta.Lockfile = firstNonEmpty(existing(root, join(c.dir, UVLock)), existing(root, join(c.dir, PoetryLock)))
		if c.meta.Lockfile == "" && c.meta.Source == SourceWorkspace {
			c.meta.Lockfile = rootLock
		}
		result.Projects = append(result.Projects, project(c, edges(c, ws, byDir, byName)))
	}
	return result
}

// readWorkspace reads the uv workspace of the root pyproject.toml: the
// directories its members globs match, less those its exclude globs match,
// that hold a pyproject.toml, and the sources it gives every member.
func readWorkspace(root string) (workspace, []diag.Diagnostic) {
	var ws workspace
	manifest, found, err := readManifest(root, wsproto.ProbeRootPath)
	if !found || err != nil {
		// Probe reports a root manifest that does not parse.
		return ws, nil
	}
	table, ok := toml.Get(manifest, "tool", "uv", "workspace").(toml.Table)
	if !ok {
		return ws, nil
	}
	ws.declared = true
	ws.members = map[string]bool{}
	ws.sources, _ = toml.Get(manifest, "tool", "uv", "sources").(toml.Table)
	var diags []diag.Diagnostic
	patterns := func(key string) [][]string {
		var out [][]string
		for _, pattern := range toml.Strings(table, key) {
			segments, ok := memberglob.Segments(pattern)
			if !ok {
				diags = append(diags, diag.Warningf(CodeManifestInvalid, Marker, "the uv workspace %s pattern %q is not a repository-relative glob and is ignored", key, pattern))
				continue
			}
			out = append(out, segments)
		}
		return out
	}
	include, exclude := patterns("members"), patterns("exclude")
	dirs, bounded := memberglob.Dirs(root, include, skipped, MaxWalkedEntries)
	if bounded {
		diags = append(diags, diag.Warningf(CodeWalkBounded, Marker, "the uv workspace walk stopped after %d entries; members past it are left out", MaxWalkedEntries))
	}
	for _, dir := range dirs {
		parts := strings.Split(dir, "/")
		if slices.ContainsFunc(exclude, func(segments []string) bool { return memberglob.Match(segments, parts) }) {
			continue
		}
		if existing(root, join(dir, Marker)) != "" {
			ws.members[dir] = true
		}
	}
	return ws, diags
}

// readManifest reads the pyproject.toml of a directory. found is false when
// there is none.
func readManifest(root, dir string) (toml.Table, bool, error) {
	data, err := os.ReadFile(abs(root, join(dir, Marker)))
	if err != nil {
		return nil, false, nil
	}
	manifest, err := toml.Parse(data)
	return manifest, true, err
}

func projectName(manifest toml.Table) string {
	return firstNonEmpty(toml.String(manifest, "project", "name"), toml.String(manifest, "tool", "poetry", "name"))
}

func projectVersion(manifest toml.Table) string {
	return firstNonEmpty(toml.String(manifest, "project", "version"), toml.String(manifest, "tool", "poetry", "version"))
}

// edges returns the directories of the components c depends on: for each
// dependency it declares, through its own uv source, else through the
// workspace root's for a member, as uv applies them; and through its Poetry
// path dependencies.
func edges(c *component, ws workspace, byDir map[string]*component, byName map[string]string) []string {
	var deps []string
	add := func(dir string) {
		if dir != c.dir && byDir[dir] != nil {
			deps = append(deps, dir)
		}
	}
	declared := declaredDependencies(c.manifest)
	own, _ := toml.Get(c.manifest, "tool", "uv", "sources").(toml.Table)
	ownNames := map[string]bool{}
	for name, value := range own {
		ownNames[normalize(name)] = true
		if !declared[normalize(name)] {
			continue
		}
		for _, source := range sourceTables(value) {
			add(resolveSource(c.dir, name, source, byName))
		}
	}
	if c.meta.Source == SourceWorkspace {
		for name, value := range ws.sources {
			if ownNames[normalize(name)] || !declared[normalize(name)] {
				continue
			}
			for _, source := range sourceTables(value) {
				// A path in the root's sources is relative to the root.
				add(resolveSource(wsproto.ProbeRootPath, name, source, byName))
			}
		}
	}
	for _, table := range poetryDependencyTables(c.manifest) {
		for _, value := range table {
			for _, spec := range sourceTables(value) {
				if p, ok := spec["path"].(string); ok {
					if dir, ok := under(c.dir, p); ok {
						add(dir)
					}
				}
			}
		}
	}
	return sortedUnique(deps)
}

// sourceTables returns the tables of a source: one inline table, or a list of
// them for a source that varies by marker.
func sourceTables(value any) []toml.Table {
	if t, ok := value.(toml.Table); ok {
		return []toml.Table{t}
	}
	return toml.Tables(value)
}

// resolveSource returns the directory a uv source points to: the workspace
// member of the dependency's name, or a local path relative to dir.
func resolveSource(dir, name string, source toml.Table, byName map[string]string) string {
	if source["workspace"] == true {
		return byName[normalize(name)]
	}
	if p, ok := source["path"].(string); ok {
		if target, ok := under(dir, p); ok {
			return target
		}
	}
	return ""
}

// requirementName reads the distribution name at the start of a PEP 508
// requirement, such as shared in "shared[extra]>=0.1; python_version>'3'".
var requirementName = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)

// declaredDependencies returns the normalized names of the dependencies a
// manifest declares: its [project] dependencies and optional dependencies,
// its dependency groups, uv's dev-dependencies and its Poetry dependencies.
func declaredDependencies(manifest toml.Table) map[string]bool {
	out := map[string]bool{}
	addRequirements := func(list []string) {
		for _, requirement := range list {
			if m := requirementName.FindStringSubmatch(requirement); m != nil {
				out[normalize(m[1])] = true
			}
		}
	}
	addRequirements(toml.Strings(manifest, "project", "dependencies"))
	addRequirements(toml.Strings(manifest, "tool", "uv", "dev-dependencies"))
	for _, table := range []any{toml.Get(manifest, "project", "optional-dependencies"), manifest["dependency-groups"]} {
		groups, _ := table.(toml.Table)
		for name := range groups {
			addRequirements(toml.Strings(groups, name))
		}
	}
	for _, table := range poetryDependencyTables(manifest) {
		for name := range table {
			out[normalize(name)] = true
		}
	}
	return out
}

// poetryDependencyTables returns the Poetry dependency tables of a manifest:
// [tool.poetry.dependencies], [tool.poetry.dev-dependencies] and the
// dependencies of each [tool.poetry.group].
func poetryDependencyTables(manifest toml.Table) []toml.Table {
	var out []toml.Table
	for _, key := range []string{"dependencies", "dev-dependencies"} {
		if t, ok := toml.Get(manifest, "tool", "poetry", key).(toml.Table); ok {
			out = append(out, t)
		}
	}
	groups, _ := toml.Get(manifest, "tool", "poetry", "group").(toml.Table)
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if t, ok := toml.Get(groups, name, "dependencies").(toml.Table); ok {
			out = append(out, t)
		}
	}
	return out
}

// project turns a component into its probe project.
func project(c *component, deps []string) wsproto.ProbeProject {
	p := wsproto.ProbeProject{
		Path:         c.dir,
		SourceName:   c.name,
		SourceFile:   join(c.dir, Marker),
		Version:      projectVersion(c.manifest),
		Dependencies: deps,
		WatchedFiles: []string{join(c.dir, Marker)},
	}
	if c.meta.Lockfile != "" {
		p.WatchedFiles = append(p.WatchedFiles, c.meta.Lockfile)
	}
	// Metadata marshals a struct of strings, which cannot fail.
	p.Metadata, _ = json.Marshal(c.meta)
	return p
}

// ReadMetadata returns the detector's metadata of a probe project.
func ReadMetadata(p wsproto.ProbeProject) (Metadata, error) {
	var m Metadata
	if len(p.Metadata) == 0 {
		return m, nil
	}
	err := json.Unmarshal(p.Metadata, &m)
	return m, err
}

var separators = regexp.MustCompile(`[-_.]+`)

// normalize is a Python project name in its normalized form (PEP 503).
func normalize(name string) string {
	return separators.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
}

// under joins a path a manifest names to the directory that holds it. An
// absolute path, or one outside the repository, is refused.
func under(dir, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", false
	}
	return wsproto.NormalizeProbePath(join(dir, name))
}

// join joins a slash path to a component directory.
func join(dir, name string) string {
	if dir == wsproto.ProbeRootPath {
		return path.Clean(name)
	}
	return path.Join(dir, name)
}

func abs(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

func existing(root, rel string) string {
	if info, err := os.Stat(abs(root, rel)); err == nil && info.Mode().IsRegular() {
		return rel
	}
	return ""
}

func sortedUnique(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	slices.Sort(list)
	return slices.Compact(list)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
