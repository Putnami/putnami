// Package bazel is the Bazel detector of @putnami/observe: it finds the
// packages of a Bazel repository and the edges between them, without running
// bazel.
//
// A Bazel package is a directory that holds a BUILD or BUILD.bazel file. The
// edges come from the export the CI wrote with
//
//	bazel query 'deps(//...)' --output=streamed_jsonproto > bazel-graph.json
//
// An edge between two targets of different packages of the main repository is
// an edge between their packages; a target of an external repository is no
// edge. Without an export, the BUILD files give the packages and no edge: a
// BUILD file is Starlark, and the detector does not evaluate it.
package bazel

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/observe/detect/memberglob"
)

// Ecosystem is the evidence ecosystem slug of a Bazel package.
const Ecosystem = "bazel"

// GraphFile is the export the detector reads at the repository root when the
// caller names none.
const GraphFile = "bazel-graph.json"

// ExportCommand is the command that writes the export.
const ExportCommand = "bazel query 'deps(//...)' --output=streamed_jsonproto > " + GraphFile

// TestLogs is the convenience link under which bazel test writes one
// directory per test target, named by its package and its name.
const TestLogs = "bazel-testlogs"

// Diagnostic codes.
const (
	CodeGraphMissing    = "observe.bazel_graph_missing"
	CodeGraphUnreadable = "observe.bazel_graph_unreadable"
	CodeWalkBounded     = "observe.bazel_walk_bounded"
)

// MaxWalkedEntries bounds the directory entries the BUILD file walk visits.
const MaxWalkedEntries = 200000

// workspaceFiles mark the root of a Bazel workspace.
var workspaceFiles = []string{"MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel"}

// buildFiles mark a Bazel package.
var buildFiles = []string{"BUILD.bazel", "BUILD"}

// Package is one Bazel package.
type Package struct {
	// Dir is the package's directory relative to the repository root, "." for
	// the root package.
	Dir string
	// Label is the package label: //pricing, or // for the root package.
	Label string
	// DependsOn lists the directories of the packages it depends on.
	DependsOn []string
	// Tested reports that the export lists a test rule of the package: a
	// rule whose class ends in _test, or a test_suite. A package the BUILD
	// files give, without the export, is not known to be tested.
	Tested bool
}

// Detection is what the detector found.
type Detection struct {
	// Packages are sorted by directory.
	Packages []Package
	// FromExport tells whether the packages and their edges come from the
	// query export rather than the BUILD files.
	FromExport  bool
	Diagnostics []diag.Diagnostic
	// targets maps the test log directory of each rule of the export,
	// <package>/<name> relative to bazel-testlogs, to its package directory.
	targets map[string]string
}

// IsWorkspace reports whether root is the root of a Bazel workspace.
func IsWorkspace(root string) bool {
	for _, name := range workspaceFiles {
		if regular(filepath.Join(root, name)) {
			return true
		}
	}
	return false
}

// Detect finds the packages of the Bazel workspace at root. graph names the
// export, absolute or relative to root; empty reads bazel-graph.json at the
// root. A repository that is no Bazel workspace has no package.
func Detect(root, graph string) Detection {
	if !IsWorkspace(root) {
		return Detection{}
	}
	named := graph != ""
	if !named {
		graph = GraphFile
	}
	if !filepath.IsAbs(graph) {
		graph = filepath.Join(root, filepath.FromSlash(graph))
	}
	var d Detection
	export, err := readExport(graph)
	missing := errors.Is(err, fs.ErrNotExist) && !named
	switch {
	case missing:
	case err != nil:
		d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeGraphUnreadable, "",
			"the Bazel query export %s is left out, so the Bazel packages have no edge: %v; write it with `%s`", display(root, graph), err, ExportCommand))
	default:
		d.fromExport(root, export)
		d.FromExport = true
		return d
	}
	dirs, diags := buildDirs(root)
	d.Diagnostics = append(d.Diagnostics, diags...)
	for _, dir := range dirs {
		d.Packages = append(d.Packages, Package{Dir: dir, Label: label(dir)})
	}
	if missing && len(d.Packages) > 0 {
		d.Diagnostics = append(d.Diagnostics, diag.Diagnostic{
			Severity: diag.Info, Code: CodeGraphMissing,
			Message: "no Bazel query export was found, so the Bazel packages have no edge: run `" + ExportCommand + "` before putnami observe, or name the export with --bazel-graph",
		})
	}
	return d
}

// TestTarget returns the package of the test target whose log is the
// repository-relative report, under bazel-testlogs/<package>/<target>/: the
// rule of the export whose log directory holds it, else the longest package
// whose directory prefixes the log's.
func (d Detection) TestTarget(report string) (string, bool) {
	rest, ok := strings.CutPrefix(path.Clean(report), TestLogs+"/")
	if !ok {
		return "", false
	}
	// The log directory, then each directory above it, from the deepest.
	var dirs []string
	for dir := path.Dir(rest); dir != "." && dir != "/"; dir = path.Dir(dir) {
		dirs = append(dirs, dir)
	}
	for _, dir := range dirs {
		if pkg, ok := d.targets[dir]; ok {
			return pkg, true
		}
	}
	packages := make(map[string]bool, len(d.Packages))
	for _, p := range d.Packages {
		packages[p.Dir] = true
	}
	// A package strictly above the log directory, which names its target.
	for _, dir := range dirs[min(1, len(dirs)):] {
		if packages[dir] {
			return dir, true
		}
	}
	if packages["."] && len(dirs) > 0 {
		return ".", true
	}
	return "", false
}

// rule is the part of a query target the detector reads.
type rule struct {
	Name      string   `json:"name"`
	RuleClass string   `json:"ruleClass"`
	RuleInput []string `json:"ruleInput"`
}

// isTest reports whether a rule class is a test rule: bazel test runs it.
func isTest(class string) bool {
	return strings.HasSuffix(class, "_test") || class == "test_suite"
}

// target is one target of the export: a rule, a source file, a generated
// file or a package group. Only rules are read.
type target struct {
	Rule *rule `json:"rule"`
}

// entry is one JSON value of the export: a target of streamed_jsonproto, or
// the query result of jsonproto, which lists them.
type entry struct {
	target
	Targets []target `json:"target"`
}

// readExport reads the rules of an export written with
// --output=streamed_jsonproto, one target per line, or --output=jsonproto.
func readExport(file string) ([]rule, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return nil, errors.New("it is a directory")
	}
	decoder := json.NewDecoder(bufio.NewReader(f))
	var rules []rule
	for {
		var e entry
		if err := decoder.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("it is not the output of bazel query --output=streamed_jsonproto: %w", err)
		}
		for _, t := range append([]target{e.target}, e.Targets...) {
			if t.Rule != nil {
				rules = append(rules, *t.Rule)
			}
		}
	}
	for _, r := range rules {
		if _, _, ok := mainLabel(r.Name); ok {
			return rules, nil
		}
	}
	return nil, errors.New("it holds no rule of the main repository")
}

// fromExport makes a package of each directory that holds a BUILD file and a
// rule of the export, with the edges its rules' inputs give.
func (d *Detection) fromExport(root string, rules []rule) {
	edges := map[string]map[string]bool{}
	tested := map[string]bool{}
	d.targets = map[string]string{}
	known := map[string]bool{}
	for _, r := range rules {
		dir, name, ok := mainLabel(r.Name)
		if !ok {
			continue
		}
		if !isPackage(root, dir, known) {
			continue
		}
		d.targets[logDir(dir, name)] = dir
		if isTest(r.RuleClass) {
			tested[dir] = true
		}
		if edges[dir] == nil {
			edges[dir] = map[string]bool{}
		}
		for _, input := range r.RuleInput {
			if other, _, ok := mainLabel(input); ok && other != dir && isPackage(root, other, known) {
				edges[dir][other] = true
			}
		}
	}
	for dir, targets := range edges {
		p := Package{Dir: dir, Label: label(dir), Tested: tested[dir]}
		for other := range targets {
			p.DependsOn = append(p.DependsOn, other)
		}
		sort.Strings(p.DependsOn)
		d.Packages = append(d.Packages, p)
	}
	sort.Slice(d.Packages, func(i, j int) bool { return d.Packages[i].Dir < d.Packages[j].Dir })
}

// isPackage reports whether dir holds a BUILD file, once per directory.
func isPackage(root, dir string, known map[string]bool) bool {
	is, seen := known[dir]
	if !seen {
		is = holdsBuildFile(root, dir)
		known[dir] = is
	}
	return is
}

// mainLabel returns the package directory and the target name of a label of
// the main repository: //pkg:name, @//pkg:name or @@//pkg:name, where //pkg
// stands for //pkg:pkg. A label of an external repository is not one.
func mainLabel(label string) (dir, name string, ok bool) {
	label = strings.TrimPrefix(strings.TrimPrefix(label, "@"), "@")
	rest, ok := strings.CutPrefix(label, "//")
	if !ok {
		return "", "", false
	}
	pkg, name, explicit := strings.Cut(rest, ":")
	if !explicit && pkg != "" {
		name = path.Base(pkg)
	}
	if pkg == "" {
		return ".", name, name != ""
	}
	if path.Clean(pkg) != pkg || strings.HasPrefix(pkg, "/") || pkg == ".." || strings.HasPrefix(pkg, "../") || name == "" {
		return "", "", false
	}
	return pkg, name, true
}

func logDir(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

func label(dir string) string {
	if dir == "." {
		return "//"
	}
	return "//" + dir
}

// buildDirs walks the repository for the directories that hold a BUILD or
// BUILD.bazel file and that the walk does not skip.
func buildDirs(root string) ([]string, []diag.Diagnostic) {
	skip := newSkipper(root)
	found := map[string]bool{}
	var diags []diag.Diagnostic
	walked := 0
	_ = filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the BUILD file walk stopped after %d entries; the packages below are not read", MaxWalkedEntries))
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if skip.dir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() && isBuildFile(entry.Name()) {
			found[path.Dir(rel)] = true
		}
		return nil
	})
	dirs := make([]string, 0, len(found))
	for dir := range found {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, diags
}

// Ignored reports whether the repository-relative directory dir, or a
// directory above it, is one the BUILD file walk skips: a hidden directory,
// node_modules, a bazel-* convenience link of the root, a directory
// .bazelignore names or one an ignore_directories glob of REPO.bazel
// matches. Bazel loads no package there.
func Ignored(root, dir string) bool {
	skip := newSkipper(root)
	parts := strings.Split(path.Clean(dir), "/")
	for i := range parts {
		if rel := strings.Join(parts[:i+1], "/"); rel != "." && skip.dir(rel) {
			return true
		}
	}
	return false
}

// skipper tells the directories the BUILD file walk skips.
type skipper struct {
	ignored map[string]bool
	globs   [][]string
}

func newSkipper(root string) skipper {
	return skipper{ignored: bazelIgnore(root), globs: ignoreDirectories(root)}
}

// dir reports whether the walk skips the repository-relative directory rel,
// "." being the root, which it never skips.
func (s skipper) dir(rel string) bool {
	if rel == "." {
		return false
	}
	name := path.Base(rel)
	return strings.HasPrefix(name, ".") || name == "node_modules" || s.ignored[rel] ||
		(!strings.Contains(rel, "/") && strings.HasPrefix(name, "bazel-")) || matchesAny(s.globs, rel)
}

// bazelIgnore reads the directories .bazelignore names, one per line,
// relative to the root.
func bazelIgnore(root string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(root, ".bazelignore"))
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[path.Clean(strings.Trim(line, "/"))] = true
	}
	return out
}

// ignoreDirectoriesCall is the ignore_directories call of a REPO.bazel file
// and its list of globs.
var ignoreDirectoriesCall = regexp.MustCompile(`(?s)\bignore_directories\s*\(\s*\[(.*?)\]`)

// stringLiteral is one Starlark string literal of that list.
var stringLiteral = regexp.MustCompile(`"([^"\\]*)"|'([^'\\]*)'`)

// ignoreDirectories reads the globs of the ignore_directories call of
// REPO.bazel, as segments; Bazel loads no package below a directory one
// matches. The detector does not evaluate Starlark: it reads the string
// literals of the call's list.
func ignoreDirectories(root string) [][]string {
	data, err := os.ReadFile(filepath.Join(root, "REPO.bazel"))
	if err != nil {
		return nil
	}
	var globs [][]string
	for _, call := range ignoreDirectoriesCall.FindAllStringSubmatch(string(data), -1) {
		for _, literal := range stringLiteral.FindAllStringSubmatch(call[1], -1) {
			if segments, ok := memberglob.Segments(literal[1] + literal[2]); ok {
				globs = append(globs, segments)
			}
		}
	}
	return globs
}

func matchesAny(globs [][]string, rel string) bool {
	if len(globs) == 0 {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, glob := range globs {
		if memberglob.Match(glob, parts) {
			return true
		}
	}
	return false
}

func isBuildFile(name string) bool {
	for _, b := range buildFiles {
		if name == b {
			return true
		}
	}
	return false
}

func holdsBuildFile(root, dir string) bool {
	return BuildFile(root, dir) != ""
}

// BuildFile is the repository-relative BUILD.bazel or BUILD file of a
// package directory, or empty when it holds none.
func BuildFile(root, dir string) string {
	for _, name := range buildFiles {
		if regular(filepath.Join(root, filepath.FromSlash(dir), name)) {
			return path.Join(dir, name)
		}
	}
	return ""
}

func regular(file string) bool {
	info, err := os.Stat(file)
	return err == nil && info.Mode().IsRegular()
}

func display(root, file string) string {
	if rel, err := filepath.Rel(root, file); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return file
}
