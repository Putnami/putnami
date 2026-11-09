// Package dotnet is the .NET detector of @putnami/observe: it finds the
// projects of a .NET repository and the projects they reference, without
// running dotnet or MSBuild.
//
// A component is the directory of a C#, F# or Visual Basic project file. The
// projects a solution at the repository root lists are workspace components;
// the other project files of the repository are manifest ones. A
// ProjectReference to the project file of another component is an edge.
// MSBuild is not evaluated: a reference or an assembly name that holds a
// property is not read, and imports are not read. The nearest
// Directory.Build.props is read for one thing only: whether it makes its
// projects test projects.
//
// The detector also maps a test assembly's file name, the suite of a TRX or
// JUnit case, to the project that builds it.
package dotnet

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/lockdiff"
)

// Ecosystem is the evidence ecosystem slug of a .NET component.
const Ecosystem = "dotnet"

// Diagnostic codes.
const (
	CodeSolutionInvalid = "observe.dotnet_solution_invalid"
	CodeProjectInvalid  = "observe.dotnet_project_invalid"
	CodeWalkBounded     = "observe.dotnet_walk_bounded"
)

// MaxWalkedEntries bounds the directory entries the project file walk visits.
const MaxWalkedEntries = 200000

// projectExtensions are the project files the detector reads.
var projectExtensions = map[string]bool{".csproj": true, ".fsproj": true, ".vbproj": true}

// skippedDirs are never walked: build outputs, restored packages, test
// results and installed Node packages.
var skippedDirs = map[string]bool{"bin": true, "obj": true, "node_modules": true, "packages": true, "TestResults": true}

// Component is one component of a .NET repository.
type Component struct {
	// ID is the directory of its project file relative to the repository
	// root, or ".".
	ID string
	// Name is the project file name without its extension.
	Name string
	// File is the repository-relative project file that names it.
	File string
	// Test reports whether the project is a test project: see isTest.
	Test bool
	// Type is "application" for a project that uses the Microsoft.NET.Sdk.Web
	// or Microsoft.NET.Sdk.Worker SDK or states OutputType Exe or WinExe,
	// outside any condition, and is no test project; "library" otherwise.
	Type string
	// Source is workspace for a project a root solution lists, else manifest.
	Source string
	// DependsOn lists the ids of the components it references, sorted.
	DependsOn []string
}

// Detection is what the detector found in one repository.
type Detection struct {
	// Components are sorted by ID; empty outside a .NET repository.
	Components []Component
	// LockFiles reports whether the repository holds a packages.lock.json.
	LockFiles   bool
	Diagnostics []diag.Diagnostic
	// assemblies maps a lower-case assembly name to its component.
	assemblies map[string]string
}

// project is one project file.
type project struct {
	assembly   string
	references []string
	test       testMarker
	// executable is set by a web or worker SDK, or an Exe or WinExe
	// OutputType, outside any condition.
	executable bool
}

// testMarker is what an MSBuild file states, outside any condition, about
// whether its project is a test project.
type testMarker int

const (
	// testUnstated: the file states nothing.
	testUnstated testMarker = iota
	// testStated: IsTestProject true, a Microsoft.NET.Test.Sdk package
	// reference, or the MSTest.Sdk SDK.
	testStated
	// testDenied: IsTestProject false.
	testDenied
)

// directoryBuildProps is the file MSBuild imports, nearest first, into every
// project under its directory.
const directoryBuildProps = "Directory.Build.props"

// Detect reads the .NET repository at root. A repository without a project
// file gets an empty detection.
func Detect(root string) Detection {
	var out Detection
	files, lockFiles, diags := walk(root)
	out.LockFiles = lockFiles
	out.Diagnostics = diags
	listed, diags := solutions(root)
	out.Diagnostics = append(out.Diagnostics, diags...)
	known := make(map[string]bool, len(files)+len(listed))
	for _, f := range files {
		known[f] = true
	}
	// A listed project the walk skipped, such as one under packages/, is a
	// project of the repository all the same.
	for f := range listed {
		if !known[f] {
			known[f] = true
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return out
	}
	sort.Strings(files)

	byDir := map[string]*Component{}
	edges := map[string]map[string]bool{}
	props := map[string]testMarker{}
	out.assemblies = map[string]string{}
	for _, file := range files {
		p, err := readProject(root, file)
		if err != nil {
			out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodeProjectInvalid, file, "%s does not parse, so its references are not read: %v", file, err))
		}
		dir := path.Dir(file)
		c := byDir[dir]
		if c == nil {
			// files are sorted, so the first project file of a directory names it.
			c = &Component{ID: dir, Name: stem(file), File: file, Source: evidence.SourceManifest}
			byDir[dir] = c
			edges[dir] = map[string]bool{}
			c.Test = isTest(p.test, nearestProps(root, dir, props))
			c.Type = TypeLibrary
			if p.executable && !c.Test {
				c.Type = TypeApplication
			}
		}
		if listed[file] {
			c.Source = evidence.SourceWorkspace
		}
		if _, taken := out.assemblies[strings.ToLower(p.assembly)]; !taken {
			out.assemblies[strings.ToLower(p.assembly)] = dir
		}
		for _, ref := range p.references {
			if target, ok := reference(dir, ref); ok && known[target] && path.Dir(target) != dir {
				edges[dir][path.Dir(target)] = true
			}
		}
	}
	for dir, c := range byDir {
		for target := range edges[dir] {
			c.DependsOn = append(c.DependsOn, target)
		}
		sort.Strings(c.DependsOn)
		out.Components = append(out.Components, *c)
	}
	sort.Slice(out.Components, func(i, j int) bool { return out.Components[i].ID < out.Components[j].ID })
	return out
}

// isTest decides whether a project is a test project from what its project
// file and the nearest Directory.Build.props state: the project file's
// IsTestProject false denies it, any other statement of either file makes it
// one.
func isTest(projectFile, props testMarker) bool {
	switch projectFile {
	case testStated:
		return true
	case testDenied:
		return false
	}
	return props == testStated
}

// nearestProps reads the test marker of the Directory.Build.props nearest to
// the repository-relative directory dir, in dir or a parent up to the root.
// A file that does not parse states nothing.
func nearestProps(root, dir string, cache map[string]testMarker) testMarker {
	for d := dir; ; d = path.Dir(d) {
		if m, ok := cache[d]; ok {
			return m
		}
		file := path.Join(d, directoryBuildProps)
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(file))); err == nil && info.Mode().IsRegular() {
			p, _ := readProject(root, file)
			cache[d] = p.test
			return p.test
		}
		if d == "." || d == "/" || d == "" {
			cache[d] = testUnstated
			return testUnstated
		}
	}
}

// Locate maps the file name of a test assembly, such as Pricing.Tests.dll or
// a path that ends with it, to the component of the project that builds it.
// Any other name, a namespace or a class name among them, maps to nothing.
func (d Detection) Locate(name string) (file, component string, ok bool) {
	base := path.Base(strings.ReplaceAll(name, `\`, "/"))
	ext := strings.ToLower(path.Ext(base))
	if ext != ".dll" && ext != ".exe" {
		return "", "", false
	}
	component, ok = d.assemblies[strings.ToLower(strings.TrimSuffix(base, path.Ext(base)))]
	return "", component, ok
}

// IsCentralPackages reports whether the repository-relative file holds the
// central package versions the detector reads as a lockfile: a
// Directory.Packages.props, in a repository without NuGet lock files, whose
// lock files would otherwise carry the same changes.
func (d Detection) IsCentralPackages(file string) bool {
	return len(d.Components) > 0 && !d.LockFiles && path.Base(file) == lockdiff.CentralPackages
}

// walk returns the repository-relative project files, sorted, and whether a
// packages.lock.json lies among the walked files.
func walk(root string) ([]string, bool, []diag.Diagnostic) {
	var files []string
	var diags []diag.Diagnostic
	lockFiles := false
	walked := 0
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the .NET detector stopped after %d entries", MaxWalkedEntries))
			return fs.SkipAll
		}
		name := entry.Name()
		if entry.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || skippedDirs[name]) {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if name == lockdiff.NuGetLock {
			lockFiles = true
			return nil
		}
		if !projectExtensions[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		if rel, err := filepath.Rel(root, p); err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the .NET detector cannot walk the repository: %v", err))
	}
	sort.Strings(files)
	return files, lockFiles, diags
}

// solutions returns the project files the .sln and .slnx files at the root
// list, wherever they lie in the repository. A solution that does not parse,
// or an entry that names no project file of the repository, is a warning.
func solutions(root string) (map[string]bool, []diag.Diagnostic) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	listed := map[string]bool{}
	var diags []diag.Diagnostic
	for _, entry := range entries {
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if (ext != ".sln" && ext != ".slnx") || !entry.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			diags = append(diags, diag.Warningf(CodeSolutionInvalid, name, "%s cannot be read: %v", name, err))
			continue
		}
		var paths []string
		if ext == ".sln" {
			paths, err = readSln(data)
		} else {
			paths, err = readSlnx(data)
		}
		if err != nil {
			diags = append(diags, diag.Warningf(CodeSolutionInvalid, name, "%s does not parse, so its projects are manifest components: %v", name, err))
			continue
		}
		for _, p := range paths {
			if !projectExtensions[strings.ToLower(path.Ext(p))] {
				continue
			}
			file, ok := reference(".", p)
			if ok {
				info, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
				ok = err == nil && info.Mode().IsRegular()
			}
			if !ok {
				diags = append(diags, diag.Warningf(CodeSolutionInvalid, name, "%s lists %s, which is no project file of the repository", name, p))
				continue
			}
			listed[file] = true
		}
	}
	return listed, diags
}

// slnProject matches a project entry of a .sln file: its type, name and path.
var slnProject = regexp.MustCompile(`(?m)^\s*Project\("[^"]*"\)\s*=\s*"[^"]*"\s*,\s*"([^"]*)"`)

// readSln returns the paths of the project entries of a .sln file, solution
// folders included.
func readSln(data []byte) ([]string, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !bytes.Contains(data, []byte("Microsoft Visual Studio Solution File")) {
		return nil, errors.New("it has no Microsoft Visual Studio Solution File header")
	}
	var out []string
	for _, m := range slnProject.FindAllSubmatch(data, -1) {
		out = append(out, string(m[1]))
	}
	return out, nil
}

// readSlnx returns the Path of every Project element of a .slnx file.
func readSlnx(data []byte) ([]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	var out []string
	root := true
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if root {
				return nil, errors.New("it holds no Solution element")
			}
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if root {
			if start.Name.Local != "Solution" {
				return nil, errors.New("its root is " + start.Name.Local + ", want Solution")
			}
			root = false
			continue
		}
		if start.Name.Local != "Project" {
			continue
		}
		for _, attr := range start.Attr {
			if attr.Name.Local == "Path" {
				out = append(out, attr.Value)
			}
		}
	}
}

// readProject reads the assembly name and the ProjectReference paths of a
// project file. The assembly name is the AssemblyName the file states
// literally, else the file name without its extension.
func readProject(root, file string) (project, error) {
	p := project{assembly: stem(file)}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return p, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	var references []string
	assembly := ""
	// conditioned holds, for each open element, whether it or an ancestor
	// carries a Condition: a test marker inside one is not read.
	var conditioned []bool
	stated, denied, executable := false, false, false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return p, err
		}
		if _, ok := token.(xml.EndElement); ok && len(conditioned) > 0 {
			conditioned = conditioned[:len(conditioned)-1]
			continue
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		cond := (len(conditioned) > 0 && conditioned[len(conditioned)-1]) || attr(start, "Condition") != "" || conditional[start.Name.Local]
		conditioned = append(conditioned, cond)
		if !cond {
			switch {
			case len(conditioned) == 1 && start.Name.Local == "Project" && usesMSTestSdk(attr(start, "Sdk")):
				stated = true
			case len(conditioned) == 1 && start.Name.Local == "Project" && usesAppSdk(attr(start, "Sdk")),
				start.Name.Local == "Sdk" && usesAppSdk(attr(start, "Name")):
				executable = true
			case start.Name.Local == "OutputType":
				var value string
				if err := decoder.DecodeElement(&value, &start); err != nil {
					return p, err
				}
				conditioned = conditioned[:len(conditioned)-1]
				switch strings.ToLower(strings.TrimSpace(value)) {
				case "exe", "winexe":
					executable = true
				}
				continue
			case start.Name.Local == "Sdk" && usesMSTestSdk(attr(start, "Name")):
				stated = true
			case start.Name.Local == "PackageReference" && strings.EqualFold(strings.TrimSpace(attr(start, "Include")), "Microsoft.NET.Test.Sdk"):
				stated = true
			case start.Name.Local == "IsTestProject":
				var value string
				if err := decoder.DecodeElement(&value, &start); err != nil {
					return p, err
				}
				conditioned = conditioned[:len(conditioned)-1]
				switch strings.ToLower(strings.TrimSpace(value)) {
				case "true":
					stated = true
				case "false":
					denied = true
				}
				continue
			}
		}
		switch start.Name.Local {
		case "ProjectReference":
			for _, attr := range start.Attr {
				if attr.Name.Local == "Include" {
					// An MSBuild item list separates its items with ;.
					for _, item := range strings.Split(attr.Value, ";") {
						if item = strings.TrimSpace(item); item != "" {
							references = append(references, item)
						}
					}
				}
			}
		case "AssemblyName":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return p, err
			}
			conditioned = conditioned[:len(conditioned)-1]
			if value = strings.TrimSpace(value); value != "" && !strings.Contains(value, "$(") {
				assembly = value
			}
		}
	}
	switch {
	case denied:
		p.test = testDenied
	case stated:
		p.test = testStated
	}
	p.references = references
	p.executable = executable
	if assembly != "" {
		p.assembly = assembly
	}
	return p, nil
}

// conditional are the MSBuild elements whose content is conditioned without
// a Condition attribute of its own: a Choose branch, or a target, which sets
// its properties when it runs, not when the project is evaluated.
var conditional = map[string]bool{"When": true, "Otherwise": true, "Target": true}

// attr returns the value of an element's attribute, or "".
func attr(start xml.StartElement, name string) string {
	for _, a := range start.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// Project types.
const (
	TypeApplication = "application"
	TypeLibrary     = "library"
)

// usesAppSdk reports whether an Sdk value names the web or worker SDK, whose
// projects are executables.
func usesAppSdk(value string) bool {
	for _, sdk := range strings.Split(value, ";") {
		name, _, _ := strings.Cut(strings.TrimSpace(sdk), "/")
		if strings.EqualFold(name, "Microsoft.NET.Sdk.Web") || strings.EqualFold(name, "Microsoft.NET.Sdk.Worker") {
			return true
		}
	}
	return false
}

// usesMSTestSdk reports whether an Sdk value, a ;-separated list of names
// with an optional /version each, names MSTest.Sdk.
func usesMSTestSdk(value string) bool {
	for _, sdk := range strings.Split(value, ";") {
		name, _, _ := strings.Cut(strings.TrimSpace(sdk), "/")
		if strings.EqualFold(name, "MSTest.Sdk") {
			return true
		}
	}
	return false
}

// reference resolves a project path, relative to the repository-relative
// directory dir with \ or / separators, to a repository-relative path. A path
// that holds an MSBuild property, is absolute, or leaves the repository
// resolves to nothing.
func reference(dir, include string) (string, bool) {
	include = strings.TrimSpace(strings.ReplaceAll(include, `\`, "/"))
	if include == "" || strings.Contains(include, "$(") || path.IsAbs(include) || (len(include) > 1 && include[1] == ':') {
		return "", false
	}
	p := path.Clean(path.Join(dir, include))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

// stem returns a file's name without its extension.
func stem(file string) string {
	base := path.Base(file)
	return strings.TrimSuffix(base, path.Ext(base))
}
