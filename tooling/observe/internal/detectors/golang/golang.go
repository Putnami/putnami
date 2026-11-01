// Package golang finds the Go modules of a repository from its go.work and
// go.mod files, without running go: the components, their dependency edges,
// the file and module a Go report's import path names, and the versions each
// go.mod locks.
//
// It reads files only. It does not decide which components a change touches,
// read reports or diff lockfiles: the change analyses consume what it returns.
// Its answer is also a workspace probe answer (Probe), so moving it into its
// own extension behind the probe transport changes no consumer.
package golang

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// Ecosystem is the ecosystem slug of a Go component and of its locked
// dependencies.
const Ecosystem = "go"

// File names the detector reads.
const (
	GoWork = "go.work"
	GoMod  = "go.mod"
	GoSum  = "go.sum"
)

// Diagnostic codes.
const (
	CodeManifestUnreadable = "observe.go_manifest_unreadable"
	CodeManifestInvalid    = "observe.go_manifest_invalid"
	CodeDuplicateModule    = "observe.go_duplicate_module"
	CodeWalkBounded        = "observe.go_walk_bounded"
)

// MaxWalkedEntries bounds the entries the walk for go.mod files visits.
const MaxWalkedEntries = 200000

// skippedDirs are never walked. The go command also ignores testdata and the
// directories whose name starts with a dot or an underscore.
var skippedDirs = map[string]bool{"node_modules": true, "vendor": true, "testdata": true}

// Module is one Go module of the repository: one component.
type Module struct {
	// Dir is the component id: the module's directory relative to the
	// repository root, slash-separated, or "." for the root.
	Dir string `json:"dir"`
	// Path is the module path its go.mod declares, empty when it cannot be read.
	Path string `json:"path,omitempty"`
	// GoMod is the repository-relative go.mod.
	GoMod string `json:"goMod"`
	// GoSum is the repository-relative go.sum, when it exists.
	GoSum string `json:"goSum,omitempty"`
	// Source is evidence.SourceWorkspace for a go.work member and
	// evidence.SourceManifest otherwise.
	Source string `json:"source"`
	// DependsOn lists the ids of the components it requires or replaces, sorted.
	DependsOn []string `json:"dependsOn,omitempty"`

	file *modfile.File
}

// Detection is what the detector found in one repository.
type Detection struct {
	// Workspace is "go.work" when the repository root holds one.
	Workspace string `json:"workspace,omitempty"`
	// Modules are the components, sorted by Dir.
	Modules []Module `json:"modules"`
	// Diagnostics are the manifests it could not read or parse.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`

	workReplaces []*modfile.Replace
	// byPath names the component directory of each module path.
	byPath map[string]string
}

// Detect reads the Go modules of the repository at root. A repository without
// go.work or go.mod gives no module.
func Detect(root string) Detection {
	var d Detection
	members := map[string]bool{}
	workPath := filepath.Join(root, GoWork)
	if data, err := os.ReadFile(workPath); err == nil {
		d.Workspace = GoWork
		work, err := modfile.ParseWork(GoWork, data, nil)
		if err != nil {
			d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeManifestInvalid, GoWork, "go.work cannot be parsed: %v", err))
		} else {
			for _, use := range work.Use {
				if dir, ok := within(".", use.Path); ok {
					members[dir] = true
				} else {
					d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeManifestInvalid, GoWork,
						"go.work uses %s, which is outside the repository and is not a component", use.Path))
				}
			}
			d.workReplaces = work.Replace
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeManifestUnreadable, GoWork, "go.work cannot be read: %v", err))
	}

	dirs, walkDiags := findModules(root)
	d.Diagnostics = append(d.Diagnostics, walkDiags...)
	for dir := range members {
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		module, ok, problems := readModule(root, dir)
		d.Diagnostics = append(d.Diagnostics, problems...)
		if !ok {
			continue
		}
		module.Source = evidence.SourceManifest
		if members[dir] {
			module.Source = evidence.SourceWorkspace
		}
		d.Modules = append(d.Modules, module)
	}
	d.link()
	return d
}

// findModules walks root for go.mod files and returns their directories.
func findModules(root string) ([]string, []diag.Diagnostic) {
	var dirs []string
	var diags []diag.Diagnostic
	walked := 0
	errBound := errors.New("bound")
	// WalkDir returns only errBound: every other error is a diagnostic.
	_ = filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			rel, relErr := filepath.Rel(root, file)
			if relErr != nil {
				rel = file
			}
			diags = append(diags, diag.Warningf(CodeManifestUnreadable, filepath.ToSlash(rel), "%s cannot be searched for go.mod: %v", filepath.ToSlash(rel), err))
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the search for go.mod stopped after %d entries", MaxWalkedEntries))
			return errBound
		}
		name := entry.Name()
		if entry.IsDir() {
			if file != root && (skippedDirs[name] || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if name == GoMod && entry.Type().IsRegular() {
			rel, err := filepath.Rel(root, filepath.Dir(file))
			if err == nil {
				dirs = append(dirs, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	return dirs, diags
}

// readModule reads the go.mod of dir. A go.mod that cannot be parsed still
// makes a module, without a path or edges; one that cannot be read does not.
func readModule(root, dir string) (Module, bool, []diag.Diagnostic) {
	module := Module{Dir: dir, GoMod: join(dir, GoMod)}
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(join(dir, GoSum)))); err == nil && info.Mode().IsRegular() {
		module.GoSum = join(dir, GoSum)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(module.GoMod)))
	if err != nil {
		return module, false, []diag.Diagnostic{diag.Warningf(CodeManifestUnreadable, module.GoMod, "%s cannot be read: %v", module.GoMod, err)}
	}
	file, err := parseGoMod(module.GoMod, data)
	var diags []diag.Diagnostic
	if err != nil {
		diags = append(diags, diag.Warningf(CodeManifestInvalid, module.GoMod, "%s cannot be parsed: %v", module.GoMod, err))
	}
	if file == nil {
		return module, true, diags
	}
	module.file = file
	if file.Module != nil {
		module.Path = file.Module.Mod.Path
	}
	return module, true, diags
}

// parseGoMod parses a go.mod. One the strict grammar refuses, such as one
// with a directive a newer go added, is read again leniently, which keeps its
// module and requires but drops its replace directives; the error says so.
func parseGoMod(name string, data []byte) (*modfile.File, error) {
	file, err := modfile.Parse(name, data, nil)
	if err == nil {
		return file, nil
	}
	lax, laxErr := modfile.ParseLax(name, data, nil)
	if laxErr != nil {
		return nil, err
	}
	return lax, fmt.Errorf("%w; its module and requires are read without its replace directives", err)
}

// link computes each module's edges from its requires and replaces and the
// root go.work's replaces.
func (d *Detection) link() {
	byPath := map[string]string{}
	source := map[string]string{}
	byDir := map[string]bool{}
	// A go.work member names its module path, as the go command builds it;
	// among directories of one source, the first in path order does.
	for _, m := range d.Modules {
		byDir[m.Dir] = true
		source[m.Dir] = m.Source
		if m.Path == "" {
			continue
		}
		if kept, seen := byPath[m.Path]; !seen || (m.Source == evidence.SourceWorkspace && source[kept] != evidence.SourceWorkspace) {
			byPath[m.Path] = m.Dir
		}
	}
	for _, m := range d.Modules {
		if m.Path != "" && byPath[m.Path] != m.Dir {
			d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeDuplicateModule, m.GoMod,
				"%s declares module %s, which %s also declares; the module path names %s", m.GoMod, m.Path, join(byPath[m.Path], GoMod), byPath[m.Path]))
		}
	}
	d.byPath = byPath
	for i := range d.Modules {
		m := &d.Modules[i]
		if m.file == nil {
			continue
		}
		edges := map[string]bool{}
		for _, req := range m.file.Require {
			local := ""
			if target, ok := replace(d.workReplaces, m.Dir, m.file.Replace, req.Mod.Path, req.Mod.Version); ok {
				if target.local != "" {
					local = target.local
				} else if target.path != "" {
					local = byPath[target.path]
				}
			} else {
				local = byPath[req.Mod.Path]
			}
			if local != "" {
				edges[local] = true
			}
		}
		// A local replace in the module's own go.mod is an edge even when
		// nothing requires it, as the Go extension's probe keeps it.
		for _, r := range m.file.Replace {
			if target := replacementOf(m.Dir, r); target.local != "" {
				edges[target.local] = true
			}
		}
		delete(edges, m.Dir)
		for dir := range edges {
			if byDir[dir] {
				m.DependsOn = append(m.DependsOn, dir)
			}
		}
		sort.Strings(m.DependsOn)
	}
}

// replacement is where a replace directive sends a module: a component
// directory for a local replace, a module path and version otherwise.
type replacement struct {
	local   string
	path    string
	version string
	// outside marks a local replace that points outside the repository.
	outside bool
}

// replace returns where the replace directives send the module path at
// version: the root go.work's directives override the go.mod's, as the go
// command applies them, and a directive for that exact version overrides one
// for every version.
func replace(work []*modfile.Replace, dir string, own []*modfile.Replace, modulePath, version string) (replacement, bool) {
	if target, ok := match(".", work, modulePath, version); ok {
		return target, true
	}
	return match(dir, own, modulePath, version)
}

func match(base string, list []*modfile.Replace, modulePath, version string) (replacement, bool) {
	var every *modfile.Replace
	for _, r := range list {
		if r.Old.Path != modulePath {
			continue
		}
		if r.Old.Version == version && version != "" {
			return replacementOf(base, r), true
		}
		if r.Old.Version == "" {
			every = r
		}
	}
	if every == nil {
		return replacement{}, false
	}
	return replacementOf(base, every), true
}

func replacementOf(base string, r *modfile.Replace) replacement {
	if modfile.IsDirectoryPath(r.New.Path) {
		if dir, ok := within(base, r.New.Path); ok {
			return replacement{local: dir}
		}
		return replacement{outside: true}
	}
	return replacement{path: r.New.Path, version: r.New.Version}
}

// within resolves a directory path relative to the base directory and
// reports whether it stays inside the repository.
func within(base, target string) (string, bool) {
	if filepath.IsAbs(target) || strings.HasPrefix(target, "/") {
		return "", false
	}
	dir := path.Clean(path.Join(base, filepath.ToSlash(target)))
	if dir == ".." || strings.HasPrefix(dir, "../") {
		return "", false
	}
	return dir, true
}

func join(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}

// Owner returns the id of the innermost component whose directory holds the
// repository-relative file. An empty, absolute or escaping path has none.
func (d Detection) Owner(file string) (string, bool) {
	file = path.Clean(filepath.ToSlash(file))
	if file == "." || file == ".." || strings.HasPrefix(file, "../") || strings.HasPrefix(file, "/") || filepath.IsAbs(file) {
		return "", false
	}
	best, found := "", false
	for _, m := range d.Modules {
		if m.Dir == "." || file == m.Dir || strings.HasPrefix(file, m.Dir+"/") {
			if !found || depth(m.Dir) > depth(best) {
				best, found = m.Dir, true
			}
		}
	}
	return best, found
}

func depth(dir string) int {
	if dir == "." {
		return 0
	}
	return strings.Count(dir, "/") + 1
}

// Locate maps an import path, or an import path followed by a file name as a
// cover profile writes it, to its repository-relative path and the component
// whose module path is its longest prefix on a slash boundary. A path with an
// empty, . or .. element maps to nothing.
func (d Detection) Locate(importPath string) (file, component string, ok bool) {
	for _, element := range strings.Split(importPath, "/") {
		if element == "" || element == "." || element == ".." {
			return "", "", false
		}
	}
	best := -1
	for i, m := range d.Modules {
		if m.Path == "" || d.byPath[m.Path] != m.Dir {
			continue
		}
		if importPath != m.Path && !strings.HasPrefix(importPath, m.Path+"/") {
			continue
		}
		if best < 0 || len(m.Path) > len(d.Modules[best].Path) {
			best = i
		}
	}
	if best < 0 {
		return "", "", false
	}
	m := d.Modules[best]
	rest := strings.TrimPrefix(strings.TrimPrefix(importPath, m.Path), "/")
	if rest == "" {
		return m.Dir, m.Dir, true
	}
	return join(m.Dir, rest), m.Dir, true
}

// Components returns the modules as evidence components. Their impact is
// left to the change analysis, which knows the changed files.
func (d Detection) Components() []evidence.Component {
	out := make([]evidence.Component, 0, len(d.Modules))
	for _, m := range d.Modules {
		out = append(out, evidence.Component{
			ID: m.Dir, Name: m.Path, Ecosystem: Ecosystem, Source: m.Source, DependsOn: slices.Clone(m.DependsOn),
		})
	}
	return out
}

// Lockfile is a file whose change can change the versions a component locks.
type Lockfile struct {
	// Path is the repository-relative lockfile.
	Path string `json:"path"`
	// Component is the id of the component that locks with it.
	Component string `json:"component"`
	// Ecosystem is the ecosystem of the packages it locks.
	Ecosystem string `json:"ecosystem"`
}

// Lockfiles returns each component's go.mod, which holds the versions it
// selects. go.sum holds checksums, not a selection, so it is not one, and
// go.work belongs to no component: a change made only in its replace
// directives shows in no lockfile.
func (d Detection) Lockfiles() []Lockfile {
	out := make([]Lockfile, 0, len(d.Modules))
	for _, m := range d.Modules {
		out = append(out, Lockfile{Path: m.GoMod, Component: m.Dir, Ecosystem: Ecosystem})
	}
	return out
}

// Locked is one dependency version a go.mod selects.
type Locked struct {
	// Name is the required module path.
	Name string `json:"name"`
	// Version is the version the go.mod selects, after its replace directives;
	// a replace by another module reads <module>@<version>.
	Version string `json:"version"`
	// Indirect marks a require the go.mod annotates // indirect.
	Indirect bool `json:"indirect,omitempty"`
}

// ReadLock returns the dependencies a go.mod's content locks, sorted by name.
// goWork is the content of the root go.work at the same commit, nil when it
// has none: its replace directives override the go.mod's, as the go command
// applies them. A require replaced by a local directory, or of a module the
// detected repository holds, is not one: its code is in the repository. A
// go.mod is read as Detect reads it, so one that only the lenient grammar
// reads gives its requires without its replace directives; a go.mod or a
// go.work that no grammar reads is an error.
func (d Detection) ReadLock(goModPath string, goMod, goWork []byte) ([]Locked, error) {
	file, err := parseGoMod(goModPath, goMod)
	if file == nil {
		return nil, err
	}
	var work []*modfile.Replace
	if goWork != nil {
		parsed, err := modfile.ParseWork(GoWork, goWork, nil)
		if err != nil {
			return nil, err
		}
		work = parsed.Replace
	}
	dir := path.Dir(filepath.ToSlash(goModPath))
	internal := map[string]bool{}
	for _, m := range d.Modules {
		if m.Path != "" {
			internal[m.Path] = true
		}
	}
	var out []Locked
	for _, req := range file.Require {
		if internal[req.Mod.Path] {
			continue
		}
		version := req.Mod.Version
		if target, ok := replace(work, dir, file.Replace, req.Mod.Path, req.Mod.Version); ok {
			switch {
			case target.local != "" || target.outside:
				continue
			case target.path == req.Mod.Path && target.version != "":
				version = target.version
			case target.path != req.Mod.Path:
				version = target.path + "@" + target.version
			}
		}
		out = append(out, Locked{Name: req.Mod.Path, Version: version, Indirect: req.Indirect})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// CompareVersions orders two Go module versions by semantic version, pseudo
// versions included: negative when a is older, positive when newer, 0 when
// equal or when either is not a valid version.
func CompareVersions(a, b string) int {
	if !semver.IsValid(a) || !semver.IsValid(b) {
		return 0
	}
	return semver.Compare(a, b)
}
