package swift

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// MaxExportBytes bounds the export the detector reads.
const MaxExportBytes = 64 << 20

// Package is one Swift package.
type Package struct {
	// Dir is the repository directory that holds Package.swift.
	Dir string
	// Name is the package name, else the directory name.
	Name string
	// Exported reports that swiftpm.json gave the targets.
	Exported bool
	// Targets are in the order the export or the directories list them.
	Targets []Target
	// Products maps a product name to the names of its targets.
	Products map[string][]string
	// LocalPackages are the repository directories of the local packages it
	// depends on, sorted.
	LocalPackages []string
}

// Target is one target of a Swift package.
type Target struct {
	Name string
	// Dir is the target's repository directory.
	Dir  string
	Test bool
	// Dependencies are the names of the targets of the same package it
	// depends on; only an export gives them.
	Dependencies []string
	// Products are the names of the products it depends on; only an export
	// gives them.
	Products []string
}

// Offers reports whether the package provides the product name: one of its
// products, else a target or the package itself by that name.
func (p Package) Offers(product string) bool {
	if _, ok := p.Products[product]; ok {
		return true
	}
	if p.Name == product {
		return true
	}
	for _, t := range p.Targets {
		if t.Name == product && !t.Test {
			return true
		}
	}
	return false
}

// TestTargets returns the package's test targets.
func (p Package) TestTargets() []Target {
	var out []Target
	for _, t := range p.Targets {
		if t.Test {
			out = append(out, t)
		}
	}
	return out
}

// readPackage reads the package at the repository directory dir: from
// swiftpm.json when it is there and usable, else from Package.swift and the
// directories under Sources and Tests.
func readPackage(root, dir string) (Package, []diag.Diagnostic) {
	abs := filepath.Join(root, filepath.FromSlash(dir))
	manifest, _ := os.ReadFile(filepath.Join(abs, Manifest))
	static := readManifest(manifest)
	pkg := Package{Dir: dir, Name: static.name, Products: static.products}
	if pkg.Name == "" {
		pkg.Name = path.Base(filepath.ToSlash(abs))
	}
	locals := map[string]bool{}
	for _, rel := range static.localPaths {
		if local, ok := join(dir, rel); ok {
			locals[local] = true
		}
	}
	var diags []diag.Diagnostic
	exportPath := path.Join(dir, ExportFile)
	export, err := readExport(filepath.Join(abs, ExportFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		diags = append(diags, diag.Warningf(CodeExportUnreadable, exportPath,
			"%s is left out, and %s is read without it: %v", exportPath, path.Join(dir, Manifest), err))
	default:
		pkg.Exported = true
		if export.Name != "" {
			pkg.Name = export.Name
		}
		if len(export.Products) > 0 {
			pkg.Products = map[string][]string{}
			for _, p := range export.Products {
				pkg.Products[p.Name] = p.Targets
			}
		}
		for _, t := range export.Targets {
			targetDir, ok := join(dir, t.Path)
			if !ok || t.Name == "" {
				continue
			}
			pkg.Targets = append(pkg.Targets, Target{
				Name: t.Name, Dir: targetDir, Test: t.Type == "test",
				Dependencies: t.TargetDependencies, Products: t.ProductDependencies,
			})
		}
		for _, d := range export.Dependencies {
			if d.Type == "fileSystem" {
				identity := d.Identity
				if identity == "" {
					identity = strings.ToLower(path.Base(strings.ReplaceAll(d.Path, `\`, "/")))
				}
				for _, local := range localByIdentity(root, dir, identity, d.Path, static.localPaths) {
					locals[local] = true
				}
			}
		}
	}
	if !pkg.Exported {
		pkg.Targets = targetDirectories(abs, dir)
	}
	for local := range locals {
		if local != dir {
			pkg.LocalPackages = append(pkg.LocalPackages, local)
		}
	}
	sort.Strings(pkg.LocalPackages)
	return pkg, diags
}

// localByIdentity returns the local package directories a fileSystem
// dependency of the export names: each .package(path:) of the manifest whose
// identity, the lowercased last segment of its path, is identity; else the
// repository directory with a Package.swift that the longest trailing
// segments of the dependency's absolute path name, since the export names it
// on the machine that wrote it.
func localByIdentity(root, dir, identity, depPath string, localPaths []string) []string {
	var out []string
	for _, rel := range localPaths {
		local, ok := join(dir, rel)
		if ok && strings.ToLower(path.Base(local)) == identity {
			out = append(out, local)
		}
	}
	if len(out) > 0 {
		return out
	}
	segments := strings.Split(strings.Trim(strings.ReplaceAll(depPath, `\`, "/"), "/"), "/")
	for start := 1; start < len(segments); start++ {
		if local, ok := join(".", strings.Join(segments[start:], "/")); ok && local != "." && hasManifest(root, local) {
			return []string{local}
		}
	}
	return nil
}

func hasManifest(root, dir string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir), Manifest))
	return err == nil && !info.IsDir()
}

// export is the part of `swift package describe --type json` the detector
// reads.
type export struct {
	Name         string `json:"name"`
	Dependencies []struct {
		Identity string `json:"identity"`
		Type     string `json:"type"`
		Path     string `json:"path"`
	} `json:"dependencies"`
	Products []struct {
		Name    string   `json:"name"`
		Targets []string `json:"targets"`
	} `json:"products"`
	Targets []struct {
		Name                string   `json:"name"`
		Path                string   `json:"path"`
		Type                string   `json:"type"`
		TargetDependencies  []string `json:"target_dependencies"`
		ProductDependencies []string `json:"product_dependencies"`
	} `json:"targets"`
}

func readExport(file string) (export, error) {
	info, err := os.Stat(file)
	if err != nil {
		return export{}, err
	}
	if info.Size() > MaxExportBytes {
		return export{}, errors.New("it is larger than 64 MiB")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return export{}, err
	}
	// The JSON document starts at its first brace: a swift build log before
	// it is no content.
	if at := strings.IndexByte(string(data), '{'); at > 0 {
		data = data[at:]
	}
	var out export
	if err := json.Unmarshal(data, &out); err != nil {
		return export{}, errors.New("it is not the JSON object `" + ExportCommand + "` writes: " + err.Error())
	}
	if len(out.Targets) == 0 {
		return export{}, errors.New("it lists no target")
	}
	return out, nil
}

// targetDirectories lists the directories under Sources and Tests of the
// package at abs, sorted: the targets of a package that keeps the default
// layout.
func targetDirectories(abs, dir string) []Target {
	var out []Target
	for _, parent := range []string{"Sources", "Tests"} {
		entries, err := os.ReadDir(filepath.Join(abs, parent))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			targetDir, ok := join(dir, parent+"/"+entry.Name())
			if !ok {
				continue
			}
			out = append(out, Target{Name: entry.Name(), Dir: targetDir, Test: parent == "Tests"})
		}
	}
	return out
}

// manifest is what the static read finds in a Package.swift.
type manifest struct {
	name       string
	localPaths []string
	products   map[string][]string
}

var (
	packageName = regexp.MustCompile(`\bPackage\s*\(\s*name\s*:\s*"([^"]+)"`)
	localPath   = regexp.MustCompile(`\.package\s*\(\s*(?:name\s*:\s*"[^"]*"\s*,\s*)?path\s*:\s*"([^"]+)"`)
	product     = regexp.MustCompile(`\.(?:library|executable)\s*\(\s*name\s*:\s*"([^"]+)"[^)]*?targets\s*:\s*\[([^\]]*)\]`)
	quoted      = regexp.MustCompile(`"([^"]+)"`)
)

// readManifest matches the literal calls of a Package.swift, its comments
// left out: the package name, the .package(path:) dependencies and the
// .library and .executable products.
func readManifest(data []byte) manifest {
	source := stripComments(string(data))
	var out manifest
	if m := packageName.FindStringSubmatch(source); m != nil {
		out.name = m[1]
	}
	for _, m := range localPath.FindAllStringSubmatch(source, -1) {
		if !slices.Contains(out.localPaths, m[1]) {
			out.localPaths = append(out.localPaths, m[1])
		}
	}
	for _, m := range product.FindAllStringSubmatch(source, -1) {
		if out.products == nil {
			out.products = map[string][]string{}
		}
		for _, target := range quoted.FindAllStringSubmatch(m[2], -1) {
			out.products[m[1]] = append(out.products[m[1]], target[1])
		}
	}
	return out
}

// stripComments removes the // and /* */ comments of Swift source, and keeps
// string literals whole.
func stripComments(source string) string {
	var b strings.Builder
	inString := false
	for i := 0; i < len(source); i++ {
		c := source[i]
		switch {
		case inString:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(source) {
				i++
				b.WriteByte(source[i])
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
			b.WriteByte(c)
		case strings.HasPrefix(source[i:], "//"):
			for i < len(source) && source[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case strings.HasPrefix(source[i:], "/*"):
			end := strings.Index(source[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
