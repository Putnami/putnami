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

// skippedDirs are never walked: build outputs, installed packages and the
// CocoaPods of an iOS app. Hidden directories, such as .dart_tool and the
// plugin symlinks of a Flutter app, are skipped too.
var skippedDirs = map[string]bool{"build": true, "node_modules": true, "Pods": true}

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
	byName := map[string]string{}
	for dir, p := range packages {
		p.member = ws.member(dir)
		if current, taken := byName[p.name]; !taken || dir < current {
			byName[p.name] = dir
		}
	}
	for dir, p := range packages {
		c := Component{ID: dir, Name: p.name, Source: evidence.SourceManifest}
		if p.member {
			c.Source = evidence.SourceWorkspace
		}
		c.DependsOn = edges(p, packages, byName)
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
			if p != root && (strings.HasPrefix(name, ".") || skippedDirs[name]) {
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
// a package, and, when p and the other package are both workspace members,
// each dependency with no source of its own that names it.
func edges(p *pkg, packages map[string]*pkg, byName map[string]string) []string {
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
			case p.member:
				target, ok = byName[name]
				ok = ok && packages[target].member
			}
			if ok && target != p.dir && !slices.Contains(out, target) {
				out = append(out, target)
			}
		}
	}
	sort.Strings(out)
	return out
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
