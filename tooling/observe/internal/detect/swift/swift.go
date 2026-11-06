// Package swift holds the Swift Package Manager and Xcode detectors of
// @putnami/observe: it finds the Swift packages, their targets and the Xcode
// projects of a repository, and the edges between them, without running
// swift or xcodebuild.
//
// A Swift package is a directory that holds Package.swift. The package and
// each of its targets are components. The export the CI wrote beside the
// manifest with `swift package describe --type json > swiftpm.json` is the
// authority for the targets and their dependencies; without it, a static
// read takes the directories under Sources and Tests as the targets and the
// .package(path:) calls as the local packages.
//
// An Xcode project is a component at the directory that holds its .xcodeproj,
// and each local package its project.pbxproj references is an edge.
//
// A JUnit case whose class name reads Module.Class is placed on the test
// target named Module, through Detection.Locate.
package swift

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Evidence ecosystem slugs.
const (
	Ecosystem      = "swift"
	XcodeEcosystem = "xcode"
)

// Files the detectors read.
const (
	Manifest   = "Package.swift"
	ExportFile = "swiftpm.json"
	PBXProject = "project.pbxproj"
)

// ExportCommand is the command that writes the export the detector reads.
const ExportCommand = "swift package describe --type json > swiftpm.json"

// Diagnostic codes.
const (
	CodeStaticRead        = "observe.swiftpm_static_read"
	CodeExportUnreadable  = "observe.swiftpm_export_unreadable"
	CodeProjectUnreadable = "observe.xcode_project_unreadable"
	CodeWalkBounded       = "observe.swift_walk_bounded"
)

// MaxWalkedEntries bounds the directory entries the walk visits.
const MaxWalkedEntries = 200000

// skippedDirs are never walked: build outputs, dependency checkouts and
// installed packages.
var skippedDirs = map[string]bool{
	"Pods": true, "Carthage": true, "DerivedData": true, "SourcePackages": true, "node_modules": true,
}

// Component is one component of a Swift or Xcode repository.
type Component struct {
	// ID is the directory relative to the repository root, or ".".
	ID   string
	Name string
	// Ecosystem is swift for a package or a target, xcode for a project.
	Ecosystem string
	// Source is workspace for a package or a target, manifest for a project.
	Source string
	// DependsOn lists the ids of the components it depends on, sorted.
	DependsOn []string
}

// Detection is what the detectors found in one repository.
type Detection struct {
	// Packages are sorted by directory.
	Packages []Package
	// Projects are sorted by directory.
	Projects    []Project
	Diagnostics []diag.Diagnostic
	locator     *locator
}

// Detect walks the repository at root for Swift packages and Xcode projects.
// It never fails: a finding is a diagnostic.
func Detect(root string) Detection {
	manifests, projects, diags := walk(root)
	var out Detection
	out.Diagnostics = diags
	var static []string
	for _, dir := range manifests {
		pkg, pkgDiags := readPackage(root, dir)
		out.Diagnostics = append(out.Diagnostics, pkgDiags...)
		if !pkg.Exported {
			static = append(static, path.Join(dir, Manifest))
		}
		out.Packages = append(out.Packages, pkg)
	}
	if len(static) > 0 {
		out.Diagnostics = append(out.Diagnostics, staticReadDiagnostic(static))
	}
	byDir := map[string][]string{}
	for _, p := range projects {
		dir := path.Dir(p)
		byDir[dir] = append(byDir[dir], p)
	}
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		project, projectDiags := readProject(root, dir, byDir[dir])
		out.Diagnostics = append(out.Diagnostics, projectDiags...)
		out.Projects = append(out.Projects, project)
	}
	out.locator = newLocator(root, out)
	return out
}

// maxListedPackages caps the packages the static read diagnostic names.
const maxListedPackages = 10

func staticReadDiagnostic(manifests []string) diag.Diagnostic {
	listed := manifests
	more := ""
	if len(listed) > maxListedPackages {
		more = fmt.Sprintf(", and %d more", len(listed)-maxListedPackages)
		listed = listed[:maxListedPackages]
	}
	count := "1 Package.swift is"
	if len(manifests) > 1 {
		count = fmt.Sprintf("%d Package.swift files are", len(manifests))
	}
	return diag.Diagnostic{
		Severity: diag.Info, Code: CodeStaticRead, Field: listed[0],
		Message: fmt.Sprintf("%s read without %s: %s%s. The static read takes the directories under Sources and Tests as the targets and the .package(path:) calls as the local packages; it sees no target dependency, custom target path or condition. Write the export beside each Package.swift with `%s`",
			count, ExportFile, strings.Join(listed, ", "), more, ExportCommand),
	}
}

// PackageComponents returns the components of the Swift packages: each
// package and each of its targets.
func (d Detection) PackageComponents() []Component {
	byDir := map[string]Package{}
	for _, p := range d.Packages {
		byDir[p.Dir] = p
	}
	var out []Component
	for _, p := range d.Packages {
		pkg := Component{ID: p.Dir, Name: p.Name, Ecosystem: Ecosystem, Source: evidence.SourceWorkspace}
		dirOf := map[string]string{}
		for _, t := range p.Targets {
			dirOf[t.Name] = t.Dir
			if !t.Test {
				pkg.DependsOn = append(pkg.DependsOn, t.Dir)
			}
		}
		out = append(out, pkg)
		for _, t := range p.Targets {
			c := Component{ID: t.Dir, Name: t.Name, Ecosystem: Ecosystem, Source: evidence.SourceWorkspace}
			if p.Exported {
				for _, dep := range t.Dependencies {
					if dir, ok := dirOf[dep]; ok {
						c.DependsOn = append(c.DependsOn, dir)
					}
				}
				for _, product := range t.Products {
					for _, local := range p.LocalPackages {
						if other, ok := byDir[local]; ok && other.Offers(product) {
							c.DependsOn = append(c.DependsOn, local)
						}
					}
				}
			} else if t.Test {
				for _, other := range p.Targets {
					if !other.Test {
						c.DependsOn = append(c.DependsOn, other.Dir)
					}
				}
			} else {
				c.DependsOn = append(c.DependsOn, p.LocalPackages...)
			}
			out = append(out, c)
		}
	}
	return finish(out)
}

// ProjectComponents returns the components of the Xcode projects.
func (d Detection) ProjectComponents() []Component {
	var out []Component
	for _, p := range d.Projects {
		out = append(out, Component{
			ID: p.Dir, Name: p.Name, Ecosystem: XcodeEcosystem, Source: evidence.SourceManifest,
			DependsOn: append([]string(nil), p.LocalPackages...),
		})
	}
	return finish(out)
}

// finish drops self edges and duplicate edges, and sorts.
func finish(components []Component) []Component {
	for i, c := range components {
		seen := map[string]bool{}
		var edges []string
		for _, dep := range c.DependsOn {
			if dep != c.ID && !seen[dep] {
				seen[dep] = true
				edges = append(edges, dep)
			}
		}
		sort.Strings(edges)
		components[i].DependsOn = edges
	}
	sort.SliceStable(components, func(i, j int) bool { return components[i].ID < components[j].ID })
	return components
}

// Locate maps a JUnit class or suite name of the form Module.Class to the
// file that declares Class under the test target named Module, and that
// target's component. A name without a module is looked for in every test
// target. When the target holds no such file, the file is the target's
// directory.
func (d Detection) Locate(name string) (file, component string, ok bool) {
	if d.locator == nil {
		return "", "", false
	}
	return d.locator.locate(name)
}

// walk lists the directories that hold Package.swift and the .xcodeproj
// directories, both repository-relative and sorted.
func walk(root string) (manifests, projects []string, diags []diag.Diagnostic) {
	walked := 0
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		walked++
		if walked > MaxWalkedEntries {
			diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Swift detectors stopped after %d entries", MaxWalkedEntries))
			return fs.SkipAll
		}
		name := entry.Name()
		if entry.IsDir() {
			if p == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || skippedDirs[name] || strings.HasSuffix(name, ".xcworkspace") {
				return fs.SkipDir
			}
			if strings.HasSuffix(name, ".xcodeproj") {
				if rel, ok := relative(root, p); ok {
					projects = append(projects, rel)
				}
				return fs.SkipDir
			}
			return nil
		}
		if name != Manifest || !entry.Type().IsRegular() {
			return nil
		}
		if rel, ok := relative(root, filepath.Dir(p)); ok {
			manifests = append(manifests, rel)
		}
		return nil
	})
	if err != nil {
		diags = append(diags, diag.Warningf(CodeWalkBounded, "", "the Swift detectors cannot walk the repository: %v", err))
	}
	sort.Strings(manifests)
	sort.Strings(projects)
	return manifests, projects, diags
}

// relative returns the repository path of an absolute path under root, "."
// for the root itself.
func relative(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", false
	}
	return wsproto.NormalizeProbePath(filepath.ToSlash(rel))
}

// join joins a repository directory and a relative path, and reports whether
// the result stays in the repository.
func join(dir, rel string) (string, bool) {
	if rel == "" || path.IsAbs(rel) {
		return "", false
	}
	return wsproto.NormalizeProbePath(path.Join(dir, rel))
}
