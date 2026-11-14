// Package pants is the Pants detector of @putnami/observe: it finds the
// components of a Pants repository and the dependencies Pants infers between
// them, without running pants.
//
// The export the CI wrote with `pants peek :: > pants-graph.json` is the
// authority: it lists every target, generated ones included, with the
// dependencies Pants expanded from the BUILD files and inferred from imports.
// A component is a directory: the targets of one BUILD file directory, and
// the targets they generate, are one component. Without an export, each
// directory whose BUILD file declares a first-party target is a component,
// with no edge.
//
// The lockfiles the [python.resolves] table of pants.toml names are read in
// the PEX JSON lock format, as dependency changes of ecosystem pypi.
package pants

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/observe/internal/toml"
)

// Ecosystem is the evidence ecosystem slug of a Pants component.
const Ecosystem = "pants"

// Files the detector reads.
const (
	Marker           = "pants.toml"
	DefaultGraphFile = "pants-graph.json"
)

// ExportCommand is the command that writes the export the detector reads.
const ExportCommand = "pants peek :: > pants-graph.json"

// Diagnostic codes.
const (
	CodeGraphMissing    = "observe.pants_graph_missing"
	CodeGraphUnused     = "observe.pants_graph_unused"
	CodeGraphUnreadable = "observe.pants_graph_unreadable"
	CodeConfigInvalid   = "observe.pants_config_invalid"
	CodeWalkBounded     = "observe.pants_walk_bounded"
)

// MaxWalkedEntries bounds the directory entries the BUILD file walk visits.
const MaxWalkedEntries = 200000

// MaxGraphBytes bounds the export the detector reads.
const MaxGraphBytes = 512 << 20

// thirdParty are the target types that stand for packages from outside the
// repository: they make no component and no edge.
var thirdParty = map[string]bool{
	"python_requirement":       true,
	"python_requirements":      true,
	"uv_requirements":          true,
	"poetry_requirements":      true,
	"pipenv_requirements":      true,
	"pants_requirements":       true,
	"go_mod":                   true,
	"go_third_party_package":   true,
	"jvm_artifact":             true,
	"jvm_artifacts":            true,
	"node_third_party_package": true,
}

// skippedDirs are never walked for BUILD files: build outputs, installed
// packages and caches.
var skippedDirs = map[string]bool{"dist": true, "node_modules": true, "__pycache__": true}

// Component is one component of a Pants repository.
type Component struct {
	// ID is the directory relative to the repository root, or ".".
	ID string
	// Source is orchestrator for an export, workspace for the BUILD files.
	Source string
	// DependsOn lists the ids of the components it depends on, sorted.
	DependsOn []string
}

// Detection is what the detector found in one repository.
type Detection struct {
	// Components are sorted by ID; empty outside a Pants repository.
	Components []Component
	// Lockfiles are the repository-relative lockfiles of the Python resolves,
	// sorted.
	Lockfiles []string
	// Tested lists, sorted, the ids of the components that hold a test
	// target: a target type that ends in _test or _tests, or a go_package
	// with a _test.go source.
	Tested []string
	// Requires maps the id of each component to the directories, sorted, of
	// the third-party and internal targets it depends on in the export, such
	// as a requirement or a resolve lockfile; empty without an export.
	Requires map[string][]string
	// RequiredFiles maps the id of each component that uses a third-party
	// target, even one in its own directory, to the files, sorted, that
	// declare those targets, such as a requirements file; empty without an
	// export.
	RequiredFiles map[string][]string
	Diagnostics   []diag.Diagnostic
}

// IsTested reports whether the component with the id holds a test target.
func (d Detection) IsTested(id string) bool {
	_, found := slices.BinarySearch(d.Tested, id)
	return found
}

// testTarget reports whether a target of the type, with the sources, runs
// tests.
func testTarget(targetType string, sources []string) bool {
	if strings.HasSuffix(targetType, "_test") || strings.HasSuffix(targetType, "_tests") {
		return true
	}
	return targetType == "go_package" && slices.ContainsFunc(sources, func(s string) bool { return strings.HasSuffix(s, "_test.go") })
}

// Options tune one detection.
type Options struct {
	// GraphFile is the export to read, relative to the root or absolute.
	// Empty means DefaultGraphFile at the root.
	GraphFile string
}

// IsLockfile reports whether the repository-relative file is the lockfile of
// a Python resolve.
func (d Detection) IsLockfile(file string) bool {
	_, found := slices.BinarySearch(d.Lockfiles, path.Clean(file))
	return found
}

// Detect reads the Pants repository at root. A root without pants.toml is no
// Pants repository and gets an empty detection, with a warning when the
// caller named an export.
func Detect(root string, opts Options) Detection {
	var out Detection
	config, err := os.ReadFile(filepath.Join(root, Marker))
	if err != nil {
		if opts.GraphFile != "" {
			out.Diagnostics = []diag.Diagnostic{diag.Warningf(CodeGraphUnused, opts.GraphFile,
				"--pants-graph names %s, but the repository root holds no %s, so no Pants export is read", opts.GraphFile, Marker)}
		}
		return out
	}
	out.Lockfiles, out.Diagnostics = resolves(config)
	graphFile := opts.GraphFile
	if graphFile == "" {
		graphFile = DefaultGraphFile
	}
	file := graphFile
	if !filepath.IsAbs(file) {
		file = filepath.Join(root, filepath.FromSlash(file))
	}
	components, tested, requires, diags, ok := fromGraph(root, file, graphFile, opts.GraphFile != "")
	out.Diagnostics = append(out.Diagnostics, diags...)
	if !ok {
		components, tested, diags = fromBuildFiles(root)
		out.Diagnostics = append(out.Diagnostics, diags...)
	}
	out.Components = components
	out.Tested = tested
	if requires != nil {
		out.Requires = make(map[string][]string, len(requires))
		out.RequiredFiles = make(map[string][]string, len(requires))
		for dir, use := range requires {
			if len(use.dirs) > 0 {
				out.Requires[dir] = use.dirs
			}
			out.RequiredFiles[dir] = use.files
		}
	}
	return out
}

// --- the export ---------------------------------------------------------------

// target is one entry of a peek export.
type target struct {
	Address      string   `json:"address"`
	TargetType   string   `json:"target_type"`
	Dependencies []string `json:"dependencies"`
	Sources      []string `json:"sources"`
}

// fromGraph reads the export at file, named graphFile in diagnostics. It
// reports ok=false when there is no usable export. A missing export is an info
// diagnostic, and a warning when the caller named it.
// requirement is what one component uses of the third-party targets: their
// directories, and the files that declare them.
type requirement struct {
	dirs, files []string
}

func fromGraph(root, file, graphFile string, named bool) ([]Component, []string, map[string]requirement, []diag.Diagnostic, bool) {
	info, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		severity := diag.Info
		if named {
			severity = diag.Warning
		}
		return nil, nil, nil, []diag.Diagnostic{{
			Severity: severity, Code: CodeGraphMissing, Field: graphFile,
			Message: fmt.Sprintf("no Pants export at %s, so the components have no edge: write it in the CI with `%s`, or name it with --pants-graph", graphFile, ExportCommand),
		}}, false
	}
	unreadable := func(format string, args ...any) ([]Component, []string, map[string]requirement, []diag.Diagnostic, bool) {
		return nil, nil, nil, []diag.Diagnostic{diag.Warningf(CodeGraphUnreadable, graphFile,
			format+"; the BUILD files answer instead, with no edge", args...)}, false
	}
	if err != nil {
		return unreadable("the Pants export %s cannot be read: %v", graphFile, err)
	}
	if info.Size() > MaxGraphBytes {
		return unreadable("the Pants export %s is larger than %d bytes", graphFile, MaxGraphBytes)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return unreadable("the Pants export %s cannot be read: %v", graphFile, err)
	}
	var targets []target
	if err := json.Unmarshal(data, &targets); err != nil || targets == nil {
		return unreadable("%s is not a Pants peek export, a JSON array of targets", graphFile)
	}
	byAddress := make(map[string]target, len(targets))
	for i, t := range targets {
		if t.Address == "" {
			return unreadable("target %d of %s names no address", i, graphFile)
		}
		byAddress[t.Address] = t
	}
	g := newGraph(root, byAddress)
	edges := map[string]map[string]bool{}
	tested := map[string]bool{}
	requires := map[string]map[string]bool{}
	requiredFiles := map[string]map[string]bool{}
	var diags []diag.Diagnostic
	for _, t := range targets {
		if !firstParty(t.TargetType) {
			continue
		}
		dir, ok := g.dir(t.Address)
		if !ok {
			diags = append(diags, diag.Warningf(CodeGraphUnreadable, graphFile, "target %s lies outside the repository and is skipped", t.Address))
			continue
		}
		// Pants lists the targets of a symlinked directory under the link and
		// under the directory it resolves to: the resolved one carries them.
		if resolved, inside := g.resolve(dir); !inside || resolved != dir {
			continue
		}
		if edges[dir] == nil {
			edges[dir] = map[string]bool{}
		}
		if testTarget(t.TargetType, t.Sources) {
			tested[dir] = true
		}
		for _, dep := range t.Dependencies {
			if known, listed := byAddress[dep]; listed && !firstParty(known.TargetType) {
				if requiredFiles[dir] == nil {
					requiredFiles[dir] = map[string]bool{}
				}
				g.thirdPartyFiles(dep, requiredFiles[dir], map[string]bool{})
				if target, ok := g.component(dep); ok && target != dir {
					if requires[dir] == nil {
						requires[dir] = map[string]bool{}
					}
					requires[dir][target] = true
				}
				continue
			}
			if target, ok := g.component(dep); ok && target != dir {
				edges[dir][target] = true
			}
		}
	}
	out := make([]Component, 0, len(edges))
	for dir, targets := range edges {
		c := Component{ID: dir, Source: evidence.SourceOrchestrator}
		for target := range targets {
			// A target the export does not list, such as a requirement of a
			// partial export, has no component.
			if _, component := edges[target]; component {
				c.DependsOn = append(c.DependsOn, target)
			}
		}
		sort.Strings(c.DependsOn)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	required := make(map[string]requirement, len(requiredFiles))
	for dir, files := range requiredFiles {
		required[dir] = requirement{dirs: sortedKeys(requires[dir]), files: sortedKeys(files)}
	}
	return out, sortedKeys(tested), required, diags, true
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// graph resolves the addresses of one export.
type graph struct {
	root string
	// realRoot is root with its symbolic links resolved.
	realRoot string
	targets  map[string]target
	// resolved caches resolve by directory.
	resolved map[string]resolution
}

// resolution is a directory resolved through its symbolic links.
type resolution struct {
	dir    string
	inside bool
}

func newGraph(root string, targets map[string]target) graph {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	return graph{root: root, realRoot: realRoot, targets: targets, resolved: map[string]resolution{}}
}

// component returns the directory of the component an address belongs to:
// the directory of dir, resolved through its symbolic links.
func (g graph) component(address string) (string, bool) {
	dir, ok := g.dir(address)
	if !ok {
		return "", false
	}
	return g.resolve(dir)
}

// resolve returns the repository-relative directory dir resolves to through
// its symbolic links, and whether it lies inside the repository. A directory
// that cannot be resolved, such as one missing from the checkout, is itself.
func (g graph) resolve(dir string) (string, bool) {
	if dir == wsproto.ProbeRootPath {
		return dir, true
	}
	if r, seen := g.resolved[dir]; seen {
		return r.dir, r.inside
	}
	r := resolution{dir: dir, inside: true}
	if real, err := filepath.EvalSymlinks(filepath.Join(g.root, filepath.FromSlash(dir))); err == nil {
		r.dir, r.inside = "", false
		if rel, err := filepath.Rel(g.realRoot, real); err == nil {
			r.dir, r.inside = wsproto.NormalizeProbePath(filepath.ToSlash(rel))
		}
	}
	g.resolved[dir] = r
	return r.dir, r.inside
}

// dir returns the repository-relative directory an address belongs to: the
// directory of the BUILD file that declares the target or its generator.
//
// An address is [//]path[:target][#generated][@parameters]. The path of a
// target generated for one file is that file, and its target name starts with
// one ../ per directory between the file and the BUILD file. A file name may
// hold an @, as a systemd unit template does: only an @ that the key=value
// parameters follow ends the path.
func (g graph) dir(address string) (string, bool) {
	spec := strings.TrimPrefix(address, "//")
	cut := pathEnd(spec)
	p, name := spec, ""
	if cut >= 0 {
		p = spec[:cut]
		if spec[cut] == ':' {
			name = spec[cut+1:]
			if end := strings.IndexAny(name, "#@"); end >= 0 {
				name = name[:end]
			}
		}
	}
	if g.isFile(address, p, name) {
		p = path.Dir(p)
		for strings.HasPrefix(name, "../") {
			if p == "." {
				return "", false
			}
			name = name[len("../"):]
			p = path.Dir(p)
		}
	}
	return wsproto.NormalizeProbePath(p)
}

// parameters matches the parameters of an address after their @:
// key=value pairs separated by commas.
var parameters = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*=[^,:#@/]*(,[A-Za-z_][A-Za-z0-9_.-]*=[^,:#@/]*)*$`)

// pathEnd is the index of the : or # that ends the path of an address spec,
// or of the @ its parameters follow; -1 when the spec is a path alone.
func pathEnd(spec string) int {
	for i := 0; i < len(spec); i++ {
		switch spec[i] {
		case ':', '#':
			return i
		case '@':
			if parameters.MatchString(spec[i+1:]) {
				return i
			}
		}
	}
	return -1
}

// isFile reports whether the path of an address names a file: when its target
// name climbs to the BUILD file, when it is one of the target's own sources,
// or, for a target the export does not list, when it is a file of the
// checkout.
func (g graph) isFile(address, p, name string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(name, "../") {
		return true
	}
	if t, listed := g.targets[address]; listed && len(t.Sources) > 0 {
		return slices.Contains(t.Sources, p)
	}
	info, err := os.Stat(filepath.Join(g.root, filepath.FromSlash(p)))
	return err == nil && info.Mode().IsRegular()
}

// thirdPartyFiles adds to files the sources of a third-party target and of the
// third-party targets it depends on, each in the directory it resolves to: a
// generated requirement's source is the requirements file target it depends
// on.
func (g graph) thirdPartyFiles(address string, files, seen map[string]bool) {
	t, listed := g.targets[address]
	if !listed || seen[address] || firstParty(t.TargetType) {
		return
	}
	seen[address] = true
	for _, source := range t.Sources {
		clean, ok := wsproto.NormalizeProbePath(source)
		if !ok {
			continue
		}
		if dir, inside := g.resolve(path.Dir(clean)); inside {
			files[path.Join(dir, path.Base(clean))] = true
		}
	}
	for _, dep := range t.Dependencies {
		g.thirdPartyFiles(dep, files, seen)
	}
}

// firstParty reports whether a target type is code of the repository: not a
// third-party package and not an internal target.
func firstParty(targetType string) bool {
	return targetType != "" && !thirdParty[targetType] && !strings.HasPrefix(targetType, "_")
}

// --- the BUILD files ----------------------------------------------------------

// call matches a call at the start of a line of a BUILD file: a target, a
// macro or a BUILD file symbol such as __defaults__.
var call = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(`)

// fromBuildFiles lists the directories whose BUILD file declares a
// first-party target, and those of them that call a test target or a
// go_package beside a _test.go file. It skips dist, node_modules, __pycache__
// and hidden directories.
func fromBuildFiles(root string) ([]Component, []string, []diag.Diagnostic) {
	dirs := map[string]bool{}
	tested := map[string]bool{}
	var diags []diag.Diagnostic
	walked := 0
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Pants detector stopped after %d entries", MaxWalkedEntries))
			return fs.SkipAll
		}
		name := entry.Name()
		if entry.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || skippedDirs[name]) {
				return fs.SkipDir
			}
			return nil
		}
		if (name != "BUILD" && !strings.HasPrefix(name, "BUILD.")) || !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || !declaresFirstParty(data) {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return nil
		}
		if dir, ok := wsproto.NormalizeProbePath(filepath.ToSlash(rel)); ok {
			dirs[dir] = true
			if callsTestTarget(data, filepath.Dir(p)) {
				tested[dir] = true
			}
		}
		return nil
	})
	if err != nil {
		diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Pants detector cannot walk the repository: %v", err))
	}
	out := make([]Component, 0, len(dirs))
	for dir := range dirs {
		out = append(out, Component{ID: dir, Source: evidence.SourceWorkspace})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, sortedKeys(tested), diags
}

// callsTestTarget reports whether a BUILD file in dir calls a test target, or
// a go_package beside a _test.go file.
func callsTestTarget(data []byte, dir string) bool {
	var goTests []string
	if matches, _ := filepath.Glob(filepath.Join(dir, "*_test.go")); len(matches) > 0 {
		goTests = []string{"x_test.go"}
	}
	for _, m := range call.FindAllSubmatch(data, -1) {
		if testTarget(string(m[1]), goTests) {
			return true
		}
	}
	return false
}

// declaresFirstParty reports whether a BUILD file calls a target or macro
// other than a third-party, an internal one or a BUILD file symbol.
func declaresFirstParty(data []byte) bool {
	for _, m := range call.FindAllSubmatch(data, -1) {
		if firstParty(string(m[1])) {
			return true
		}
	}
	return false
}

// --- pants.toml ---------------------------------------------------------------

// buildroot is the placeholder of the repository root in pants.toml values.
const buildroot = "%(buildroot)s/"

// resolves returns the lockfiles the [python.resolves] table names.
func resolves(config []byte) ([]string, []diag.Diagnostic) {
	doc, err := toml.Parse(config)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Warningf(CodeConfigInvalid, Marker, "%s does not parse, so no resolve lockfile is read: %v", Marker, err)}
	}
	table, _ := toml.Get(doc, "python", "resolves").(toml.Table)
	var out []string
	var diags []diag.Diagnostic
	for name, value := range table {
		file, _ := value.(string)
		file, ok := wsproto.NormalizeProbePath(strings.TrimPrefix(file, buildroot))
		if !ok || file == wsproto.ProbeRootPath {
			diags = append(diags, diag.Warningf(CodeConfigInvalid, Marker, "the lockfile of the resolve %s is not a repository path and is not read", name))
			continue
		}
		out = append(out, file)
	}
	sort.Strings(out)
	sort.Slice(diags, func(i, j int) bool { return diags[i].Message < diags[j].Message })
	return slices.Compact(out), diags
}
