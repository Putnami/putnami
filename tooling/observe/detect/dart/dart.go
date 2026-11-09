// Package dart is the Dart detector of @putnami/observe: it finds the Dart
// and Flutter packages of a repository and the dependencies between them,
// without running dart, flutter, pub or melos.
//
// Each directory whose pubspec.yaml names a package is a component. A member
// of the root pub workspace (the workspace list of the root pubspec.yaml, Dart
// 3.6 and later) and a package the root melos.yaml packages globs match are
// workspace components; any other package is a manifest one. The workspace
// root itself is no component: its pubspec.yaml declares the workspace or
// sits beside melos.yaml.
//
// A dependency with a path to another package is an edge. Inside a pub
// workspace or a melos repository, which link their packages to each other, a
// dependency that names another of their packages with no source of its own
// is one too.
package dart

import (
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
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/internal/yaml"
)

// Ecosystem is the evidence ecosystem slug of a Dart or Flutter component.
const Ecosystem = "dart"

// Files the detector reads.
const (
	Pubspec = "pubspec.yaml"
	Melos   = "melos.yaml"
)

// Diagnostic codes.
const (
	CodePubspecInvalid = "observe.dart_pubspec_invalid"
	CodeMelosInvalid   = "observe.dart_melos_invalid"
	CodePatternInvalid = "observe.dart_pattern_invalid"
	CodeWalkBounded    = "observe.dart_walk_bounded"
)

// MaxWalkedEntries bounds the directory entries the pubspec.yaml walk visits.
const MaxWalkedEntries = 200000

// skippedDirs are never walked: installed packages and the CocoaPods of an
// iOS app. Hidden directories, such as .dart_tool and the plugin symlinks of a
// Flutter app, are skipped too, and so is a build directory, the output of
// flutter build, unless it holds a pubspec.yaml of its own.
var skippedDirs = map[string]bool{"node_modules": true, "Pods": true}

// dependencyKeys are the pubspec.yaml tables that declare dependencies.
var dependencyKeys = []string{"dependencies", "dev_dependencies", "dependency_overrides"}

// sources are the pubspec.yaml keys that give a dependency a source of its
// own.
var sources = []string{"path", "git", "sdk", "hosted"}

// Component is one Dart package of a repository.
type Component struct {
	// ID is the package's directory relative to the repository root, or ".".
	ID string
	// Name is the pubspec name.
	Name string
	// Source is workspace for a pub workspace or melos package, manifest
	// otherwise.
	Source string
	// DependsOn lists the ids of the components it depends on, sorted.
	DependsOn []string
	// Flutter reports a package built with the Flutter SDK: its pubspec
	// depends on it (sdk: flutter) or constrains its version
	// (environment.flutter).
	Flutter bool
	// PubWorkspace is the ID of the outermost pub workspace that resolves the
	// package: the root's, ".", or one a pubspec below the root declares.
	// Empty for a package no pub workspace lists, a melos package included.
	PubWorkspace string
	// DeclaresWorkspace reports a pubspec with a workspace list: the root of
	// a pub workspace below the repository root.
	DeclaresWorkspace bool
}

// Detection is what the detector found in one repository.
type Detection struct {
	// Components are sorted by ID; empty in a repository without a Dart
	// package.
	Components  []Component
	Diagnostics []diag.Diagnostic
}

// pkg is one pubspec.yaml that names a package.
type pkg struct {
	dir      string
	name     string
	manifest yaml.Map
	member   bool
	// scope is the workspace whose members link to each other by name: the
	// outermost pub workspace, else "." for a melos package.
	scope string
	// pubWorkspace is the outermost pub workspace that lists the package.
	pubWorkspace string
}

// Detect reads the Dart packages of the repository at root.
func Detect(root string) Detection {
	var out Detection
	dirs, diags := pubspecDirs(root)
	out.Diagnostics = append(out.Diagnostics, diags...)
	packages := map[string]*pkg{}
	for _, dir := range dirs {
		p, d := readPubspec(root, dir)
		out.Diagnostics = append(out.Diagnostics, d...)
		if p != nil {
			packages[dir] = p
		}
	}
	ws, diags := readWorkspace(root, packages["."])
	out.Diagnostics = append(out.Diagnostics, diags...)
	if ws.isRoot {
		delete(packages, ".")
	}
	for dir, p := range packages {
		p.member = ws.member(dir)
		if p.member {
			p.scope = wsproto.ProbeRootPath
		}
		if ws.pubMember(dir) {
			p.pubWorkspace = wsproto.ProbeRootPath
		}
	}
	out.Diagnostics = append(out.Diagnostics, nestedWorkspaces(packages)...)
	// members maps a scope and a package name to the member that has it: only
	// the members of one workspace link to each other by name.
	members := map[string]map[string]string{}
	for dir, p := range packages {
		if p.scope == "" {
			continue
		}
		if members[p.scope] == nil {
			members[p.scope] = map[string]string{}
		}
		if current, taken := members[p.scope][p.name]; !taken || dir < current {
			members[p.scope][p.name] = dir
		}
	}
	for dir, p := range packages {
		c := Component{ID: dir, Name: p.name, Source: evidence.SourceManifest, PubWorkspace: p.pubWorkspace}
		if p.member {
			c.Source = evidence.SourceWorkspace
		}
		_, c.DeclaresWorkspace = p.manifest["workspace"]
		c.DependsOn = edges(p, packages, members[p.scope])
		c.Flutter = usesFlutter(p.manifest)
		out.Components = append(out.Components, c)
	}
	sort.Slice(out.Components, func(i, j int) bool { return out.Components[i].ID < out.Components[j].ID })
	return out
}

// pubspecDirs walks the repository and returns, in path order, the
// directories that hold a pubspec.yaml.
func pubspecDirs(root string) ([]string, []diag.Diagnostic) {
	var dirs []string
	var diags []diag.Diagnostic
	walked := 0
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Dart detector stopped after %d entries", MaxWalkedEntries))
			return fs.SkipAll
		}
		name := entry.Name()
		if entry.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || skippedDirs[name] || name == "build" && !isFile(filepath.Join(p, Pubspec))) {
				return fs.SkipDir
			}
			return nil
		}
		if name != Pubspec || !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return nil
		}
		if dir, ok := wsproto.NormalizeProbePath(filepath.ToSlash(rel)); ok {
			dirs = append(dirs, dir)
		}
		return nil
	})
	if err != nil {
		diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Dart detector cannot walk the repository: %v", err))
	}
	sort.Strings(dirs)
	return dirs, diags
}

// readPubspec reads the pubspec.yaml of dir. A file that does not read, or
// that names no package, is a diagnostic and no package.
func readPubspec(root, dir string) (*pkg, []diag.Diagnostic) {
	file := join(dir, Pubspec)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return nil, []diag.Diagnostic{diag.Warningf(CodePubspecInvalid, file, "%s cannot be read: %v", file, err)}
	}
	doc, err := yaml.Parse(data)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Warningf(CodePubspecInvalid, file, "%s does not read, so it is no package: %v", file, err)}
	}
	manifest, _ := doc.(yaml.Map)
	name := strings.TrimSpace(yaml.String(manifest, "name"))
	if name == "" {
		return nil, []diag.Diagnostic{diag.Warningf(CodePubspecInvalid, file, "%s names no package, so it is no package", file)}
	}
	return &pkg{dir: dir, name: name, manifest: manifest}, nil
}

// workspace is what the root declares: the pub workspace members and the
// melos packages globs.
type workspace struct {
	// isRoot reports whether the root pubspec.yaml is the workspace root.
	isRoot bool
	// pub reports a root pubspec.yaml with a workspace list.
	pub     bool
	members [][]string
	// melos reports a root melos.yaml.
	melos   bool
	include [][]string
	ignore  [][]string
}

// pubMember reports whether the package at dir is a member of the root pub
// workspace.
func (ws workspace) pubMember(dir string) bool {
	if dir == "." && ws.isRoot {
		return false
	}
	parts := splitPath(dir)
	return slices.ContainsFunc(ws.members, func(p []string) bool { return matchSegments(p, parts) })
}

// nestedWorkspaces reads the workspace list of each pubspec below the root
// that declares one: its members are workspace members, which link to each
// other by name, and resolve with the outermost pub workspace that lists the
// workspace root, or with that root.
func nestedWorkspaces(packages map[string]*pkg) []diag.Diagnostic {
	var diags []diag.Diagnostic
	dirs := make([]string, 0, len(packages))
	for dir := range packages {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		root := packages[dir]
		value, declared := root.manifest["workspace"]
		if dir == "." || !declared {
			continue
		}
		file := join(dir, Pubspec)
		list, ok := yaml.Strings(value)
		if !ok {
			diags = append(diags, diag.Warningf(CodePubspecInvalid, file, "the workspace of %s is not a list of paths, so it has no member", file))
		}
		var parsed [][]string
		parsed, diags = patterns(file+"#workspace", list, diags)
		for _, member := range dirs {
			rel, inside := strings.CutPrefix(member, dir+"/")
			if !inside || !slices.ContainsFunc(parsed, func(p []string) bool { return matchSegments(p, splitPath(rel)) }) {
				continue
			}
			p := packages[member]
			p.member = true
			if p.pubWorkspace == "" {
				p.pubWorkspace = dir
			}
		}
	}
	// A pub workspace that is itself a member resolves with the outer one.
	for _, p := range packages {
		for p.pubWorkspace != "" && p.pubWorkspace != wsproto.ProbeRootPath {
			outer := packages[p.pubWorkspace]
			if outer == nil || outer.pubWorkspace == "" || outer.pubWorkspace == p.pubWorkspace {
				break
			}
			p.pubWorkspace = outer.pubWorkspace
		}
		if p.pubWorkspace != "" {
			p.scope = p.pubWorkspace
		}
	}
	for dir, p := range packages {
		// The root of a nested pub workspace links its members by name too.
		if _, declared := p.manifest["workspace"]; declared && dir != "." && p.scope == "" {
			p.scope = dir
		}
	}
	return diags
}

// member reports whether the package at dir belongs to the pub workspace or
// to the melos packages.
func (ws workspace) member(dir string) bool {
	if dir == "." && ws.isRoot {
		return false
	}
	parts := splitPath(dir)
	matches := func(patterns [][]string) bool {
		return slices.ContainsFunc(patterns, func(p []string) bool { return matchSegments(p, parts) })
	}
	return matches(ws.members) || (matches(ws.include) && !matches(ws.ignore))
}

// readWorkspace reads the workspace list of the root pubspec.yaml and the
// root melos.yaml.
func readWorkspace(root string, rootPackage *pkg) (workspace, []diag.Diagnostic) {
	var ws workspace
	var diags []diag.Diagnostic
	if rootPackage != nil {
		if value, declared := rootPackage.manifest["workspace"]; declared {
			ws.isRoot, ws.pub = true, true
			list, ok := yaml.Strings(value)
			if !ok {
				diags = append(diags, diag.Warningf(CodePubspecInvalid, Pubspec, "the workspace of the root %s is not a list of paths, so it has no member", Pubspec))
			}
			ws.members, diags = patterns(Pubspec+"#workspace", list, diags)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, Melos))
	if errors.Is(err, fs.ErrNotExist) {
		return ws, diags
	}
	ws.isRoot, ws.melos = true, true
	if err != nil {
		return ws, append(diags, diag.Warningf(CodeMelosInvalid, Melos, "%s cannot be read: %v", Melos, err))
	}
	doc, err := yaml.Parse(data)
	if err != nil {
		return ws, append(diags, diag.Warningf(CodeMelosInvalid, Melos, "%s does not read, so its packages are no workspace members: %v", Melos, err))
	}
	for _, key := range []string{"packages", "ignore"} {
		list, ok := yaml.Strings(yaml.Get(doc, key))
		if !ok {
			diags = append(diags, diag.Warningf(CodeMelosInvalid, Melos+"#"+key, "the %s of %s is not a list of globs and is ignored", key, Melos))
			continue
		}
		var parsed [][]string
		parsed, diags = patterns(Melos+"#"+key, list, diags)
		if key == "packages" {
			ws.include = parsed
		} else {
			ws.ignore = parsed
		}
	}
	return ws, diags
}

// patterns splits repository-relative globs into segments. A pattern that
// leaves the repository, or is no valid glob, is a diagnostic.
func patterns(field string, list []string, diags []diag.Diagnostic) ([][]string, []diag.Diagnostic) {
	var out [][]string
	for _, raw := range list {
		segments, ok := patternSegments(raw)
		if !ok {
			diags = append(diags, diag.Warningf(CodePatternInvalid, field, "the pattern %q is not a repository-relative glob and is ignored", raw))
			continue
		}
		out = append(out, segments)
	}
	return out, diags
}

func patternSegments(pattern string) ([]string, bool) {
	pattern = strings.TrimSpace(strings.ReplaceAll(pattern, `\`, "/"))
	if pattern == "" || strings.HasPrefix(pattern, "/") {
		return nil, false
	}
	var out []string
	for _, segment := range strings.Split(pattern, "/") {
		switch segment {
		case "", ".":
			continue
		case "..":
			return nil, false
		}
		if _, err := path.Match(segment, ""); err != nil {
			return nil, false
		}
		out = append(out, segment)
	}
	return out, true
}

// matchSegments matches path segments against pattern segments, where a **
// segment matches zero or more path segments.
func matchSegments(pattern, parts []string) bool {
	if len(pattern) == 0 {
		return len(parts) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchSegments(pattern[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], parts[0])
	return err == nil && ok && matchSegments(pattern[1:], parts[1:])
}

// edges returns the components p depends on: each dependency with a path to
// a package, and, when p is a workspace member, each dependency with no source
// of its own that names another member.
func edges(p *pkg, packages map[string]*pkg, members map[string]string) []string {
	var out []string
	for _, key := range dependencyKeys {
		deps, _ := p.manifest[key].(yaml.Map)
		for name, spec := range deps {
			target, ok := "", false
			table, isTable := spec.(yaml.Map)
			switch {
			case isTable && table["path"] != nil:
				target, ok = resolvePath(p.dir, yaml.String(table, "path"))
				if ok {
					_, ok = packages[target]
				}
			case isTable && slices.ContainsFunc(sources, func(s string) bool { _, has := table[s]; return has }):
			case p.scope != "":
				target, ok = members[name]
			}
			if ok && target != p.dir && !slices.Contains(out, target) {
				out = append(out, target)
			}
		}
	}
	sort.Strings(out)
	return out
}

// usesFlutter reports whether a pubspec names the Flutter SDK: a dependency
// with sdk: flutter, or a flutter entry in its environment.
func usesFlutter(manifest yaml.Map) bool {
	if env, ok := manifest["environment"].(yaml.Map); ok && env["flutter"] != nil {
		return true
	}
	for _, key := range dependencyKeys {
		deps, _ := manifest[key].(yaml.Map)
		for _, spec := range deps {
			if table, ok := spec.(yaml.Map); ok && strings.TrimSpace(yaml.String(table, "sdk")) == "flutter" {
				return true
			}
		}
	}
	return false
}

// resolvePath resolves a dependency path, relative to the directory of the
// pubspec.yaml that declares it, to a repository directory. A path that
// leaves the repository, or an absolute one, resolves to none.
func resolvePath(dir, p string) (string, bool) {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if p == "" || strings.HasPrefix(p, "/") || (len(p) > 1 && p[1] == ':') {
		return "", false
	}
	joined := path.Join(dir, p)
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return "", false
	}
	return joined, true
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func splitPath(dir string) []string {
	if dir == "." {
		return nil
	}
	return strings.Split(dir, "/")
}

func join(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}
