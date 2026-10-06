package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The import scan behind a Go edge's provenance.
//
// A `require` or a local `replace` states that a module is AVAILABLE to the
// build; only an `import` in a source file states that the build READS it. The
// two disagree whenever a dependency leaves the code and stays in the manifest,
// and the edge is then a declaration: it carries impact, ordering and an import
// boundary no action of the build asks for.
//
// The scan walks the module's own tree under the rule `go mod tidy` walks it,
// so "imported" here means the same thing the manifest is meant to record:
//
//   - a test file counts exactly like a non-test file, because a requirement a
//     `_test.go` alone imports is one tidy keeps;
//   - build constraints are ignored, because tidy considers every build
//     configuration;
//   - `testdata`, `vendor` and directories whose name starts with "." or "_"
//     are skipped, because the go command loads no package from them — `.gen`
//     is one of them, so a generated tree a build wrote never changes the
//     answer between a cold clone and a warm checkout;
//   - a subdirectory holding its own go.mod is a different module.
//
// The scan is read-only and toolchain-free: no `go list`, no module download,
// no build cache. Each file is parsed in imports-only mode, which stops at the
// first declaration.

// moduleImportScan is one module's answer: which of the workspace modules asked
// about its own sources import, and whether the walk read everything it meant
// to.
type moduleImportScan struct {
	// imported holds the asked-about module paths a source file imports.
	imported map[string]bool
	// complete reports that every file under the module was read and parsed. A
	// scan that is not complete attributes NOTHING: an unreadable directory or
	// an unparsable file must never turn a real import into a phantom edge.
	complete bool
}

// importsModule answers whether the module imports one of the paths asked
// about. An incomplete scan answers yes for every path, because the honest
// answer is unknown and the safe one is the edge the manifest already states.
func (s moduleImportScan) importsModule(module string) bool {
	if !s.complete {
		return true
	}
	return s.imported[module]
}

// scanModuleImports reads the module rooted at moduleDir and reports which of
// wanted its sources import.
//
// An import is credited to the module that provides it, resolved against
// wanted and providers together, and counts only when that module is wanted. A
// module nested under a wanted one therefore keeps its own packages even when
// this go.mod requires the parent alone.
//
// The walk stops as soon as every wanted module has been seen: a healthy module
// imports what it requires, so the common case reads a prefix of the tree.
func scanModuleImports(moduleDir string, wanted, providers []string) moduleImportScan {
	scan := moduleImportScan{imported: make(map[string]bool, len(wanted)), complete: true}
	if len(wanted) == 0 {
		return scan
	}
	isWanted := make(map[string]bool, len(wanted))
	for _, module := range wanted {
		isWanted[module] = true
	}
	candidates := append(append([]string(nil), wanted...), providers...)
	fset := token.NewFileSet()
	err := filepath.WalkDir(moduleDir, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if len(scan.imported) == len(wanted) {
			return fs.SkipAll
		}
		if entry.IsDir() {
			return skipGoDirectory(current, moduleDir, entry.Name())
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, current, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if module := longestModuleMatch(imported, candidates); isWanted[module] {
				scan.imported[module] = true
			}
		}
		return nil
	})
	if err != nil {
		scan.complete = false
	}
	return scan
}

// skipGoDirectory applies the go command's package-loading rule to one
// directory: the module root is always entered, a nested module is a different
// module, and the names the toolchain never loads a package from are skipped.
func skipGoDirectory(current, moduleDir, name string) error {
	if current == moduleDir {
		return nil
	}
	if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return fs.SkipDir
	}
	if info, err := os.Stat(filepath.Join(current, "go.mod")); err == nil && !info.IsDir() {
		return fs.SkipDir
	}
	return nil
}

// longestModuleMatch resolves an import path to the module that provides it:
// the longest of modules that is the import path itself or a prefix of it.
// Longest wins so a module nested under another — `.../cli/model` under
// `.../cli` — is credited alone for its own packages.
func longestModuleMatch(imported string, modules []string) string {
	best := ""
	for _, module := range modules {
		if imported != module && !strings.HasPrefix(imported, module+"/") {
			continue
		}
		if len(module) > len(best) {
			best = module
		}
	}
	return best
}
