// Package dotnet is the .NET detector of @putnami/observe: it finds the
// projects of a .NET repository and the projects they reference, without
// running dotnet or MSBuild.
//
// A component is the directory of a C#, F# or Visual Basic project file. The
// projects a solution at the repository root lists are workspace components;
// the other project files of the repository are manifest ones. A
// ProjectReference to the project file of another component is an edge.
// MSBuild is not evaluated: a reference or an assembly name that holds a
// property is not read, and Directory.Build.props and imports are not read.
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
}

// Detect reads the .NET repository at root. A repository without a project
// file gets an empty detection.
func Detect(root string) Detection {
	var out Detection
	files, lockFiles, diags := walk(root)
	out.LockFiles = lockFiles
	out.Diagnostics = diags
	if len(files) == 0 {
		return out
	}
	known := make(map[string]bool, len(files))
	for _, f := range files {
		known[f] = true
	}
	listed, diags := solutions(root, known)
	out.Diagnostics = append(out.Diagnostics, diags...)

	byDir := map[string]*Component{}
	edges := map[string]map[string]bool{}
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
			c = &Component{ID: dir, Name: stem(file), Source: evidence.SourceManifest}
			byDir[dir] = c
			edges[dir] = map[string]bool{}
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
// list. A solution that does not parse, or an entry that names a project file
// the walk did not find, is a warning.
func solutions(root string, known map[string]bool) (map[string]bool, []diag.Diagnostic) {
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
			if !ok || !known[file] {
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
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return p, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "ProjectReference":
			for _, attr := range start.Attr {
				if attr.Name.Local == "Include" {
					references = append(references, attr.Value)
				}
			}
		case "AssemblyName":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return p, err
			}
			if value = strings.TrimSpace(value); value != "" && !strings.Contains(value, "$(") {
				assembly = value
			}
		}
	}
	p.references = references
	if assembly != "" {
		p.assembly = assembly
	}
	return p, nil
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
