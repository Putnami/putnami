// Package areas splits a repository into the parts an agent works in. Areas
// come from the workspace manifests the repository declares, at its root or
// in a top-level directory, and from a Bazel workspace's packages. Code that
// no manifest declares gets inferred areas: the directories that own a
// language manifest, then, when the root would still hold most of the code,
// the top-level directories. Files outside every area form the root area.
package areas

import (
	"encoding/json"
	"encoding/xml"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/internal/scope"
)

// RootPath is the path of the root area.
const RootPath = "."

// Area is one part of the repository.
type Area struct {
	Name   string
	Path   string
	Source contract.AreaSource
	// Role says whether the area holds code, a test tree or a documentation
	// site. The root area always holds code.
	Role contract.AreaRole
}

// Manifest is a workspace manifest found at the repository root or in a
// top-level directory.
type Manifest struct {
	// Kind names the tool that reads the manifest, such as "pnpm" or "go.work".
	Kind string
	// Path is the manifest's repository-relative path.
	Path string
}

// Layout is the repository's areas and the workspace manifests that declare
// them.
type Layout struct {
	// Areas are sorted by path; the root area, when present, comes first.
	Areas []Area
	// Manifests are the workspace manifests found, sorted by path. Empty when
	// every area is inferred.
	Manifests []Manifest
	byPath    map[string]int
}

// Reader returns the content of files at HEAD, keyed by path.
type Reader func(paths []string) map[string][]byte

// manifestFiles are the files that may declare a workspace, at the root or
// in a top-level directory.
var manifestFiles = []string{
	"Cargo.toml", "go.work", "lerna.json", "nx.json", "package.json",
	"pnpm-workspace.yaml", "pom.xml", "putnami.workspace.json", "pyproject.toml",
}

// bazelFiles mark a Bazel workspace at the root; its packages are the
// directories that hold a BUILD file.
var bazelFiles = []string{"MODULE.bazel", "WORKSPACE.bazel", "WORKSPACE"}

var buildFiles = []string{"BUILD", "BUILD.bazel"}

// Wanted lists the files Detect may read among the tracked paths, so that a
// caller can fetch them in one batch.
func Wanted(files []string) []string {
	wanted := slices.Clone(manifestFiles)
	for _, file := range files {
		base := path.Base(file)
		switch {
		case base == "putnami.json":
			wanted = append(wanted, file)
		case strings.Count(file, "/") == 1 && slices.Contains(manifestFiles, base) && !scope.Skipped(file):
			wanted = append(wanted, file)
		}
	}
	return wanted
}

// languageManifests mark a directory that builds on its own.
var languageManifests = map[string]bool{
	"Cargo.toml": true, "Gemfile": true, "build.gradle": true, "build.gradle.kts": true,
	"composer.json": true, "go.mod": true, "mix.exs": true, "package.json": true,
	"pom.xml": true, "pyproject.toml": true, "setup.py": true,
}

// maxInferredDepth is how deep a language manifest may sit and still mark an
// inferred area: backend/api/src/pyproject.toml does, a manifest in a
// package's own tooling further down does not.
const maxInferredDepth = 3

// maxRootShare is the share of the code the root area may hold before the
// top-level directories become areas of their own.
const maxRootShare = 0.5

// maxAreaPath keeps a path usable as an area name, which the contract bounds
// to 128 bytes. A longer or unusual path folds into its parent area.
const maxAreaPath = 128

var safePath = regexp.MustCompile(`^[A-Za-z0-9._@+-]+(/[A-Za-z0-9._@+-]+)*$`)

// Detect derives the layout from the tracked files at HEAD.
func Detect(files []string, read Reader) Layout {
	dirsWith := map[string][]string{}
	var authored []string
	for _, file := range files {
		if scope.Skipped(file) {
			continue
		}
		authored = append(authored, file)
		dirsWith[path.Base(file)] = append(dirsWith[path.Base(file)], path.Dir(file))
	}
	manifests, members, claimed := declared(read(Wanted(files)), dirsWith, read)
	layout := Layout{Manifests: manifests, byPath: map[string]int{}}
	layout.add(members, contract.AreaFromManifest)
	layout.add(inferred(dirsWith, members, claimed), contract.AreaInferred)
	if layout.rootShare(authored) > maxRootShare {
		layout.add(topLevel(authored, layout), contract.AreaInferred)
	}
	for _, file := range authored {
		if layout.Assign(file) < 0 {
			layout.Areas = append([]Area{{Path: RootPath, Source: contract.AreaInferred}}, layout.Areas...)
			layout.index()
			break
		}
	}
	name(layout.Areas)
	assignRoles(layout.Areas, authored)
	return layout
}

// TestTrees are the directory names that hold a test tree: Rails' spec/,
// Jest's __tests__/, and the usual test/, tests/ and e2e/.
var TestTrees = []string{"__tests__", "e2e", "spec", "test", "tests"}

// docsDirs are the directory names that hold a documentation site.
var docsDirs = []string{"doc", "docs", "website", "www"}

// docSiteConfigs are the files, at an area's root, that configure a
// documentation site generator: Docusaurus, MkDocs, VitePress, mdBook,
// Antora and DocFX. Sphinx counts when conf.py sits beside index.rst.
var docSiteConfigs = []string{
	"docusaurus.config.js", "docusaurus.config.ts", "docusaurus.config.mjs", "docusaurus.config.cjs",
	"mkdocs.yml", "mkdocs.yaml", ".vitepress/config.js", ".vitepress/config.ts", ".vitepress/config.mjs", ".vitepress/config.mts",
	"book.toml", "antora.yml", "docfx.json",
}

// TestTree returns the directory of the test tree a file lies in: the path
// up to its first test-tree segment, such as "spec" for
// spec/models/account_spec.rb. It returns false outside every test tree.
func TestTree(file string) (string, bool) {
	segments := strings.Split(path.Dir(file), "/")
	for i, segment := range segments {
		if slices.Contains(TestTrees, segment) {
			return strings.Join(segments[:i+1], "/"), true
		}
	}
	return "", false
}

// assignRoles marks the areas that support the code rather than hold it: an
// area whose path names a test tree holds tests, and an area whose path names
// a documentation directory, or that configures a documentation site
// generator at its root, holds docs. Every other area, and the root, holds
// code.
func assignRoles(areas []Area, files []string) {
	configured := map[string]bool{}
	sphinx := map[string]int{}
	for _, file := range files {
		for _, config := range docSiteConfigs {
			if dir, ok := strings.CutSuffix(file, "/"+config); ok {
				configured[dir] = true
			}
		}
		if base := path.Base(file); base == "conf.py" || base == "index.rst" {
			if sphinx[path.Dir(file)]++; sphinx[path.Dir(file)] == 2 {
				configured[path.Dir(file)] = true
			}
		}
	}
	for i, area := range areas {
		areas[i].Role = contract.AreaCode
		if area.Path == RootPath {
			continue
		}
		segments := strings.Split(area.Path, "/")
		switch {
		case slices.ContainsFunc(segments, func(s string) bool { return slices.Contains(TestTrees, s) }):
			areas[i].Role = contract.AreaTests
		case slices.ContainsFunc(segments, func(s string) bool { return slices.Contains(docsDirs, s) }), configured[area.Path]:
			areas[i].Role = contract.AreaDocs
		}
	}
}

// add appends the usable directories that are not areas yet, and keeps the
// areas sorted by path.
func (l *Layout) add(dirs []string, source contract.AreaSource) {
	for _, dir := range dirs {
		if _, ok := l.byPath[dir]; ok || dir == RootPath || len(dir) > maxAreaPath || !safePath.MatchString(dir) || Fixture(dir) {
			continue
		}
		l.Areas = append(l.Areas, Area{Path: dir, Source: source})
		l.byPath[dir] = len(l.Areas) - 1
	}
	sort.Slice(l.Areas, func(i, j int) bool { return l.Areas[i].Path < l.Areas[j].Path })
	l.index()
}

// rootShare is the share of the authored files no area holds.
func (l Layout) rootShare(authored []string) float64 {
	if len(authored) == 0 {
		return 0
	}
	outside := 0
	for _, file := range authored {
		if l.Assign(file) < 0 {
			outside++
		}
	}
	return float64(outside) / float64(len(authored))
}

func (l *Layout) index() {
	l.byPath = map[string]int{}
	for i, area := range l.Areas {
		l.byPath[area.Path] = i
	}
}

// Assign returns the index of the deepest area that contains file, or -1.
func (l Layout) Assign(file string) int {
	for dir := path.Dir(file); ; dir = path.Dir(dir) {
		if i, ok := l.byPath[dir]; ok {
			return i
		}
		if dir == RootPath || dir == "/" {
			return -1
		}
	}
}

// Declared reports whether a workspace manifest declares the areas.
func (l Layout) Declared() bool { return len(l.Manifests) > 0 }

// name gives each area its directory's base name, or its full path when two
// areas share a base name, so that names stay unique.
func name(areas []Area) {
	count := map[string]int{}
	for _, area := range areas {
		count[base(area.Path)]++
	}
	for i, area := range areas {
		areas[i].Name = base(area.Path)
		if count[areas[i].Name] > 1 {
			areas[i].Name = area.Path
		}
	}
}

func base(dir string) string {
	if dir == RootPath {
		return "root"
	}
	return path.Base(dir)
}

// Fixture reports whether a path lies in test data, whose manifests and
// configurations describe a sample workspace rather than a part of this one.
func Fixture(dir string) bool {
	return slices.Contains(strings.Split(dir, "/"), "testdata")
}

// putnamiProjects keeps the directories whose putnami.json declares a
// project. A putnami.json that lists "includes" only groups projects.
func putnamiProjects(dirs []string, read Reader) []string {
	files := make([]string, len(dirs))
	for i, dir := range dirs {
		files[i] = path.Join(dir, "putnami.json")
	}
	contents := read(files)
	var projects []string
	for i, dir := range dirs {
		var project struct {
			Includes []string `json:"includes"`
		}
		if json.Unmarshal(contents[files[i]], &project) == nil && len(project.Includes) > 0 {
			continue
		}
		projects = append(projects, dir)
	}
	return projects
}

// claims maps a workspace kind to the language manifest whose directories
// it lists. Under a workspace root, a directory with that manifest that the
// workspace leaves out is left out on purpose, not waiting to be inferred.
var claims = map[string]string{
	"pnpm": "package.json", "npm-workspaces": "package.json", "lerna": "package.json",
	"go.work": "go.mod", "cargo": "Cargo.toml", "uv": "pyproject.toml", "maven": "pom.xml",
}

// claim is a language manifest a workspace root governs.
type claim struct {
	root, manifest string
}

func declared(contents map[string][]byte, dirsWith map[string][]string, read Reader) ([]Manifest, []string, []claim) {
	var manifests []Manifest
	var members []string
	var claimed []claim
	add := func(kind, file string, dirs []string) {
		manifests = append(manifests, Manifest{Kind: kind, Path: file})
		members = append(members, dirs...)
		if manifest, ok := claims[kind]; ok {
			claimed = append(claimed, claim{root: path.Dir(file), manifest: manifest})
		}
	}
	if _, ok := contents["putnami.workspace.json"]; ok {
		add("putnami", "putnami.workspace.json", putnamiProjects(dirsWith["putnami.json"], read))
	}
	for _, file := range bazelFiles {
		if slices.Contains(dirsWith[file], RootPath) {
			add("bazel", file, bazelPackages(dirsWith))
			break
		}
	}
	for _, root := range workspaceRoots(contents) {
		before := len(manifests)
		workspace(root, contents, dirsWith, add)
		if root != RootPath && len(manifests) > before {
			// A nested workspace's own directory holds its shared
			// configuration: it is an area around its members.
			members = append(members, root)
		}
	}
	return manifests, unique(members), claimed
}

// workspaceRoots are the root and the top-level directories that hold a
// workspace manifest, root first.
func workspaceRoots(contents map[string][]byte) []string {
	seen := map[string]bool{}
	var nested []string
	for file := range contents {
		dir := path.Dir(file)
		if strings.Count(file, "/") == 1 && !seen[dir] && !scope.Skipped(file) && !strings.HasPrefix(dir, ".") && slices.Contains(manifestFiles, path.Base(file)) {
			seen[dir] = true
			nested = append(nested, dir)
		}
	}
	sort.Strings(nested)
	return append([]string{RootPath}, nested...)
}

// workspace reads the manifests of one workspace root. Their globs and paths
// are relative to that root. A nested root counts only when it declares at
// least one member: a plain package.json in a top-level directory declares
// nothing.
func workspace(root string, contents map[string][]byte, dirsWith map[string][]string, add func(kind, file string, dirs []string)) {
	file := func(name string) string { return path.Join(root, name) }
	candidates := func(name string) []string { return within(root, dirsWith[name]) }
	declare := func(kind, name string, dirs []string) {
		if len(dirs) > 0 || root == RootPath {
			add(kind, file(name), joined(root, dirs))
		}
	}
	if data, ok := contents[file("pnpm-workspace.yaml")]; ok {
		declare("pnpm", "pnpm-workspace.yaml", match(yamlList(string(data), "packages"), candidates("package.json")))
	}
	if data, ok := contents[file("package.json")]; ok {
		if globs := packageWorkspaces(data); len(globs) > 0 {
			declare("npm-workspaces", "package.json", match(globs, candidates("package.json")))
		}
	}
	if data, ok := contents[file("lerna.json")]; ok {
		var lerna struct {
			Packages []string `json:"packages"`
		}
		_ = json.Unmarshal(data, &lerna)
		if len(lerna.Packages) == 0 {
			lerna.Packages = []string{"packages/*"}
		}
		declare("lerna", "lerna.json", match(lerna.Packages, candidates("package.json")))
	}
	if _, ok := contents[file("nx.json")]; ok {
		declare("nx", "nx.json", candidates("project.json"))
	}
	if data, ok := contents[file("go.work")]; ok {
		declare("go.work", "go.work", goWorkUses(string(data)))
	}
	if data, ok := contents[file("Cargo.toml")]; ok {
		if globs := tomlList(string(data), "workspace", "members"); len(globs) > 0 {
			declare("cargo", "Cargo.toml", match(globs, candidates("Cargo.toml")))
		}
	}
	if data, ok := contents[file("pyproject.toml")]; ok {
		if globs := tomlList(string(data), "tool.uv.workspace", "members"); len(globs) > 0 {
			declare("uv", "pyproject.toml", match(globs, candidates("pyproject.toml")))
		}
	}
	if data, ok := contents[file("pom.xml")]; ok {
		if modules := mavenModules(data); len(modules) > 0 {
			declare("maven", "pom.xml", match(modules, candidates("pom.xml")))
		}
	}
}

// mavenModules reads the modules a Maven aggregator declares, in the project
// and in its profiles, as directories relative to the pom.xml. A module may
// name its project file rather than its directory.
func mavenModules(data []byte) []string {
	type modules struct {
		Modules []string `xml:"modules>module"`
	}
	var project struct {
		Modules  []string  `xml:"modules>module"`
		Profiles []modules `xml:"profiles>profile"`
	}
	if xml.Unmarshal(data, &project) != nil {
		return nil
	}
	declared := project.Modules
	for _, profile := range project.Profiles {
		declared = append(declared, profile.Modules...)
	}
	dirs := make([]string, 0, len(declared))
	for _, module := range declared {
		module = strings.TrimSpace(module)
		if strings.HasSuffix(module, ".xml") {
			module = path.Dir(module)
		}
		if module != "" && module != "." && !strings.HasPrefix(path.Clean(module), "..") {
			dirs = append(dirs, path.Clean(module))
		}
	}
	return dirs
}

// within returns the directories under root, relative to it; root itself is
// left out.
func within(root string, dirs []string) []string {
	if root == RootPath {
		return dirs
	}
	var out []string
	for _, dir := range dirs {
		if rest, ok := strings.CutPrefix(dir, root+"/"); ok {
			out = append(out, rest)
		}
	}
	return out
}

func joined(root string, dirs []string) []string {
	if root == RootPath {
		return dirs
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, path.Join(root, dir))
	}
	return out
}

// bazelPackages returns the top-level directories that hold a Bazel
// package, a directory with a BUILD file. The top-level directory is the
// unit a team owns; one package is often a single library.
func bazelPackages(dirsWith map[string][]string) []string {
	var dirs []string
	for _, name := range buildFiles {
		for _, dir := range dirsWith[name] {
			if top, _, _ := strings.Cut(dir, "/"); dir != RootPath && !strings.HasPrefix(top, ".") && !Fixture(dir) {
				dirs = append(dirs, top)
			}
		}
	}
	return unique(dirs)
}

// inferred returns the directories that own a language manifest, down to
// maxInferredDepth, that no declared area covers and no workspace governs.
// A directory that holds declared areas only groups them and is left out.
func inferred(dirsWith map[string][]string, declaredDirs []string, claimed []claim) []string {
	var dirs []string
	for manifest := range languageManifests {
		for _, dir := range dirsWith[manifest] {
			if dir == RootPath || strings.Count(dir, "/")+1 > maxInferredDepth || Fixture(dir) || related(dir, declaredDirs) || governed(dir, manifest, claimed) {
				continue
			}
			dirs = append(dirs, dir)
		}
	}
	return unique(dirs)
}

func governed(dir, manifest string, claimed []claim) bool {
	for _, c := range claimed {
		if c.manifest == manifest && (c.root == RootPath || strings.HasPrefix(dir, c.root+"/")) {
			return true
		}
	}
	return false
}

// related reports whether dir is one of dirs, lies inside one, or contains
// one.
func related(dir string, dirs []string) bool {
	for _, other := range dirs {
		if dir == other || strings.HasPrefix(dir, other+"/") || strings.HasPrefix(other, dir+"/") {
			return true
		}
	}
	return false
}

// topLevel returns the top-level directories of the authored files no area
// holds. Dot directories hold tool configuration, not code.
func topLevel(authored []string, layout Layout) []string {
	var dirs []string
	for _, file := range authored {
		if top, _, nested := strings.Cut(file, "/"); nested && !strings.HasPrefix(top, ".") && layout.Assign(file) < 0 {
			dirs = append(dirs, top)
		}
	}
	return unique(dirs)
}

func unique(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = path.Clean(value)
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// match keeps the candidate directories that a workspace glob includes and
// no negated glob excludes.
func match(globs []string, candidates []string) []string {
	var include, exclude []string
	for _, glob := range globs {
		glob = strings.TrimSpace(glob)
		negated := strings.HasPrefix(glob, "!")
		glob = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(glob, "!"), "./"), "/")
		glob = strings.TrimSuffix(glob, "/")
		if glob == "" {
			continue
		}
		if negated {
			exclude = append(exclude, glob)
		} else {
			include = append(include, glob)
		}
	}
	var out []string
	for _, dir := range candidates {
		if matchAny(include, dir) && !matchAny(exclude, dir) {
			out = append(out, dir)
		}
	}
	return out
}

func matchAny(globs []string, dir string) bool {
	for _, glob := range globs {
		if Glob(strings.Split(glob, "/"), strings.Split(dir, "/")) {
			return true
		}
	}
	return false
}

// Glob matches path segments against pattern segments, where "**" matches
// zero or more segments and any other segment follows path.Match.
func Glob(pattern, segments []string) bool {
	if len(pattern) == 0 {
		return len(segments) == 0
	}
	if pattern[0] == "**" {
		for skip := 0; skip <= len(segments); skip++ {
			if Glob(pattern[1:], segments[skip:]) {
				return true
			}
		}
		return false
	}
	if len(segments) == 0 {
		return false
	}
	if ok, _ := path.Match(pattern[0], segments[0]); !ok {
		return false
	}
	return Glob(pattern[1:], segments[1:])
}

func packageWorkspaces(data []byte) []string {
	var manifest struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if json.Unmarshal(data, &manifest) != nil || len(manifest.Workspaces) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(manifest.Workspaces, &list) == nil {
		return list
	}
	var nested struct {
		Packages []string `json:"packages"`
	}
	_ = json.Unmarshal(manifest.Workspaces, &nested)
	return nested.Packages
}

// yamlList reads a top-level key's block list, the only shape
// pnpm-workspace.yaml uses for its packages.
func yamlList(document, key string) []string {
	var items []string
	inList := false
	for _, line := range strings.Split(document, "\n") {
		trimmed := strings.TrimSpace(stripComment(line, "#"))
		switch {
		case strings.HasPrefix(line, key+":"):
			inList = true
			if rest := strings.TrimSpace(strings.TrimPrefix(line, key+":")); strings.HasPrefix(rest, "[") {
				return quotedList(rest)
			}
		case !inList || trimmed == "":
		case strings.HasPrefix(trimmed, "- "):
			items = append(items, unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))))
		case !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t"):
			inList = false
		}
	}
	return items
}

// tomlList reads a string array under a table, possibly across lines.
func tomlList(document, table, key string) []string {
	inTable := false
	collecting := false
	var value strings.Builder
	for _, line := range strings.Split(document, "\n") {
		trimmed := strings.TrimSpace(stripComment(line, "#"))
		if strings.HasPrefix(trimmed, "[") && !collecting {
			inTable = trimmed == "["+table+"]"
			continue
		}
		if !inTable {
			continue
		}
		if !collecting {
			name, rest, ok := strings.Cut(trimmed, "=")
			if !ok || strings.TrimSpace(name) != key {
				continue
			}
			collecting = true
			trimmed = strings.TrimSpace(rest)
		}
		value.WriteString(trimmed)
		if strings.Contains(trimmed, "]") {
			return quotedList(value.String())
		}
	}
	return nil
}

var quoted = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)

func quotedList(value string) []string {
	matches := quoted.FindAllStringSubmatch(value, -1)
	items := make([]string, 0, len(matches))
	for _, match := range matches {
		items = append(items, match[1]+match[2])
	}
	return items
}

func goWorkUses(document string) []string {
	var dirs []string
	inBlock := false
	for _, line := range strings.Split(document, "\n") {
		trimmed := strings.TrimSpace(stripComment(line, "//"))
		switch {
		case inBlock && trimmed == ")":
			inBlock = false
		case inBlock && trimmed != "":
			dirs = append(dirs, unquote(trimmed))
		case trimmed == "use (":
			inBlock = true
		case strings.HasPrefix(trimmed, "use "):
			dirs = append(dirs, unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "use "))))
		}
	}
	var clean []string
	for _, dir := range dirs {
		if dir = strings.TrimPrefix(path.Clean(dir), "./"); !strings.HasPrefix(dir, "..") {
			clean = append(clean, dir)
		}
	}
	return clean
}

func stripComment(line, marker string) string {
	if i := strings.Index(line, marker); i >= 0 {
		return line[:i]
	}
	return line
}

func unquote(value string) string {
	return strings.Trim(value, `"'`)
}
