package app

import (
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
)

// This file owns the second half of the capability ownership contract.
//
// The scheduler stamp in .gen/version.json enumerates WORKSPACE projects only:
// the CLI walks putnami.json dependencies and can neither see nor version a
// published module the workload consumes from the module cache (core
// deliberately does not parse provider-owned language manifests). A workload
// that composes a published framework contributor — go.putnami.dev/events
// consumed as a released module rather than as a sibling project — therefore
// has contributions whose concrete producer is not, and can never be, in that
// inventory.
//
// The complementary half is resolved here, from the only authority that knows
// it: the linker's own record of the modules that went into this binary. A
// contributor that is neither stamped nor provided by a published module of
// this build remains an error, because that is a real workspace
// misconfiguration rather than a normal external dependency.
//
// Everything derived here must be reproducible on any machine: the module path
// and the resolved module version, never a module-cache location.

// publishedModule is one published module dependency of the running binary.
// Every field comes from the build's own module graph, so a rebuild of the same
// commit on any machine observes the same values.
type publishedModule struct {
	// Path is the canonical path of the module that actually SUPPLIED the code:
	// the replacement when a versioned `replace` redirects the requirement, and
	// the requirement itself otherwise. This is the identity a manifest records,
	// because it names the source compiled into the binary.
	Path string
	// Version is the resolved version of that module, e.g. "v0.1.0-8dc640802".
	Version string
	// ImportPath is the module path source code imports the module BY. A
	// versioned replacement redirects where code comes from without changing a
	// single import, so the two diverge exactly there; it is left empty when it
	// equals Path, which keeps the unreplaced form canonical.
	ImportPath string
}

// importPath is the path packages of this module are imported under.
func (m publishedModule) importPath() string {
	if m.ImportPath != "" {
		return m.ImportPath
	}
	return m.Path
}

// capabilityPublishedModules is the seam through which the capability
// inventory observes this binary's published dependencies. Production always
// reads the linked build info; tests replace it to describe a published
// contributor the test binary cannot actually link.
var capabilityPublishedModules = publishedBuildModules

// publishedBuildModules reads the running binary's build info. A build that
// carries no module information (an unusual linker mode, or a `go test` binary,
// which records only its main module) yields no modules, which leaves external
// contributors unresolvable and therefore reported rather than guessed.
func publishedBuildModules() []publishedModule {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	return publishedModulesOf(info)
}

// publishedModulesOf keeps only dependencies that are genuinely PUBLISHED, and
// the main module — which has no released version — is never one of them.
func publishedModulesOf(info *debug.BuildInfo) []publishedModule {
	if info == nil {
		return nil
	}
	modules := make([]publishedModule, 0, len(info.Deps))
	seen := make(map[string]bool, len(info.Deps))
	for _, dep := range info.Deps {
		if dep == nil {
			continue
		}
		module, ok := publishedModuleOf(dep)
		if !ok || seen[module.importPath()] {
			continue
		}
		seen[module.importPath()] = true
		modules = append(modules, module)
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].importPath() < modules[j].importPath() })
	return modules
}

// publishedModuleOf resolves one dependency to the module that supplied its
// code. `replace` has two forms and they are NOT equivalent here: a versioned
// replacement (`=> other/module v1.2.0`) redirects to another RELEASED module,
// which is still a published external contributor and must resolve; a local
// replacement (`=> ./dir`, a go.work sibling) points at source the workspace
// itself supplies, has no version by construction, and must not silently
// satisfy an ownership lookup that a missing workspace stamp should report.
func publishedModuleOf(dep *debug.Module) (publishedModule, bool) {
	importPath := strings.TrimSpace(dep.Path)
	if importPath == "" {
		return publishedModule{}, false
	}
	source := dep
	if dep.Replace != nil {
		source = dep.Replace
	}
	sourcePath := strings.TrimSpace(source.Path)
	if sourcePath == "" || localModulePath(sourcePath) || !publishedModuleVersion(source.Version) {
		return publishedModule{}, false
	}
	module := publishedModule{Path: sourcePath, Version: source.Version}
	if sourcePath != importPath {
		module.ImportPath = importPath
	}
	return module, true
}

// localModulePath reports whether a replacement target names a directory rather
// than a module: an absolute path, or one that begins with "./" or "../".
func localModulePath(path string) bool {
	slashed := filepath.ToSlash(path)
	return strings.HasPrefix(slashed, "/") || slashed == "." || slashed == ".." ||
		strings.HasPrefix(slashed, "./") || strings.HasPrefix(slashed, "../") ||
		filepath.IsAbs(path) || strings.Contains(path, `\`)
}

// publishedModuleVersion accepts exactly the resolved semantic versions the Go
// module graph records for a released dependency. "(devel)" and an empty
// version identify a module built from local source, which has no published
// identity to stamp into a manifest.
func publishedModuleVersion(version string) bool {
	value := strings.TrimSpace(version)
	if !strings.HasPrefix(value, "v") || value == "(devel)" {
		return false
	}
	return isResolvedCapabilityVersion(value)
}

// externalOwnerForPackage attributes a package path to the published module that
// provides it. The longest matching module path wins, mirroring how the stamped
// inventory resolves nested workspace projects.
//
// Both of a module's paths are matched. The import path answers a live producer,
// whose package path the runtime reports as the code imports it; the supplying
// path answers a manifest that already recorded this rule's output, which names
// the module the code came from. Under a versioned replacement those differ, and
// a lookup that knew only one of them would fail on the other caller.
func (inv *capabilityInventory) externalOwnerForPackage(packageName string) (generatedCapabilityPackage, bool) {
	if strings.TrimSpace(packageName) == "" {
		return generatedCapabilityPackage{}, false
	}
	best, bestLen := -1, -1
	for index, module := range inv.modules {
		claimed := modulePathClaim(module, packageName)
		if claimed > bestLen {
			best, bestLen = index, claimed
		}
	}
	if best < 0 {
		return generatedCapabilityPackage{}, false
	}
	return externalCapabilityPackage(inv.modules[best]), true
}

// modulePathClaim reports the length of the longest module path that contains
// packageName, or -1 when the module does not provide it at all.
func modulePathClaim(module publishedModule, packageName string) int {
	claimed := -1
	for _, candidate := range []string{module.importPath(), module.Path} {
		if candidate == "" || len(candidate) <= claimed {
			continue
		}
		if packageName == candidate || strings.HasPrefix(packageName, candidate+"/") {
			claimed = len(candidate)
		}
	}
	return claimed
}

// externalOwnerForFile attributes a compile-time source path to the published
// module it was compiled from. The longest module-root-relative reduction wins,
// so a nested module is never shadowed by its parent.
func (inv *capabilityInventory) externalOwnerForFile(file string) (generatedCapabilityPackage, bool) {
	best := -1
	for index, module := range inv.modules {
		if _, ok := moduleRelativeSourcePath(file, module); !ok {
			continue
		}
		if best < 0 || len(module.Path) > len(inv.modules[best].Path) {
			best = index
		}
	}
	if best < 0 {
		return generatedCapabilityPackage{}, false
	}
	return externalCapabilityPackage(inv.modules[best]), true
}

// externalCapabilityPackage is the inventory entry for a published module. It
// deliberately carries no SourceRoot, EvidencePath, or SourceBinding: those
// describe workspace source the scheduler stamped, and a published module has
// none. The published identity is the SUPPLYING module path and its resolved
// version — the code in the binary, not the requirement that named it — while
// the binding falls back to the semantic owner in provenance().
func externalCapabilityPackage(module publishedModule) generatedCapabilityPackage {
	return generatedCapabilityPackage{
		Package:            module.Path,
		Version:            module.Version,
		external:           true,
		externalImportPath: module.ImportPath,
	}
}

// moduleRelativeSourcePath reduces a compile-time file path to its
// module-root-relative form.
//
// runtime.FuncForPC reports the path the file was COMPILED from, which is the
// module cache under a plain build ("<GOMODCACHE>/example.com/m@v1.2.3/x.go"),
// the bare "example.com/m@v1.2.3/x.go" under -trimpath, and a vendor directory
// under -mod=vendor. Only the reduction is reproducible; the prefix is a
// property of the machine that ran the build, so it is dropped rather than
// recorded.
func moduleRelativeSourcePath(file string, module publishedModule) (string, bool) {
	if strings.TrimSpace(file) == "" || module.Path == "" {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(file))
	markers := []string{module.Path + "@" + module.Version + "/"}
	// The module cache encodes upper-case letters so two module paths that
	// differ only in case cannot collide on a case-insensitive filesystem; a
	// -trimpath build keeps the canonical path. Try both encodings.
	if escaped := escapeModuleElement(module.Path) + "@" + escapeModuleElement(module.Version) + "/"; escaped != markers[0] {
		markers = append(markers, escaped)
	}
	// A versioned replacement vendors under the requirement it replaces rather
	// than under the module that supplied the code. Both paths are tried.
	markers = append(markers, "vendor/"+module.importPath()+"/")
	if module.ImportPath != "" {
		markers = append(markers, "vendor/"+module.Path+"/")
	}
	for _, marker := range markers {
		rel := ""
		switch index := strings.LastIndex(clean, "/"+marker); {
		case strings.HasPrefix(clean, marker):
			rel = clean[len(marker):]
		case index >= 0:
			rel = clean[index+1+len(marker):]
		default:
			continue
		}
		if canonicalWorkspaceRelativePath(rel, false) {
			return rel, true
		}
	}
	return "", false
}

// escapeModuleElement mirrors the module-cache path encoding: every upper-case
// ASCII letter becomes "!" followed by its lower-case form.
func escapeModuleElement(value string) string {
	var escaped strings.Builder
	for _, current := range value {
		if current >= 'A' && current <= 'Z' {
			escaped.WriteByte('!')
			escaped.WriteRune(current + ('a' - 'A'))
			continue
		}
		escaped.WriteRune(current)
	}
	return escaped.String()
}
