package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The import scan behind a TypeScript edge's provenance.
//
// A `workspace:` entry in package.json states that a workspace package is
// LINKED into this one; only an import, a re-export or a require in a source
// file states that the code READS it. A linked package nothing imports is a
// declaration: it carries impact, ordering and an import boundary the bundler
// never asks for.
//
// The scan reads the package's own committed sources — `src/**` and `test/**`
// are where they live, and the walk covers the whole package directory so a
// root-level entry point is never missed. What it skips is everything a build
// or an install WRITES: `node_modules`, `dist`, `build`, `coverage`, and every
// directory whose name starts with "." (`.gen`, `.putnami`). A generated tree
// must not change the answer, or a cold clone and a warm checkout would
// disagree about the same commit. What a build's generated sources import is
// derived from the manifest instead (generatedPackageImports). A nested package
// directory is a different package.
//
// The scan is read-only and needs no bundler, no type checker and no install.

// importSpecifier matches the module specifier of every form that reads another
// package: `from "x"`, `import "x"`, `import("x")`, `require("x")`.
//
// Over-matching is the safe direction: a specifier found inside a comment or a
// string keeps an edge the manifest already states, while a missed one would
// report a real import as a phantom.
var importSpecifier = regexp.MustCompile(`(?:\bfrom|\bimport|\brequire)\s*\(?\s*["']([^"']+)["']`)

// packageSourceExtensions are the files the scan reads.
var packageSourceExtensions = map[string]bool{
	".ts": true, ".tsx": true, ".mts": true, ".cts": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
}

// packageImportScan is one package's answer: which of the workspace packages
// asked about its own sources import, and whether the walk read everything.
type packageImportScan struct {
	// imported holds the asked-about package names a source file reads.
	imported map[string]bool
	// complete reports that every source file under the package was read. A
	// scan that is not complete attributes NOTHING: an unreadable file must
	// never turn a real import into a phantom edge.
	complete bool
}

// importsPackage answers whether the package reads one of the names asked
// about. An incomplete scan answers yes for every name, because the honest
// answer is unknown and the safe one is the edge the manifest already states.
func (s packageImportScan) importsPackage(name string) bool {
	if !s.complete {
		return true
	}
	return s.imported[name]
}

// scanPackageImports reads the package rooted at packageDir and reports which
// of wanted its sources import.
//
// The walk stops as soon as every wanted package has been seen: a healthy
// package imports what it links, so the common case reads a prefix of the tree.
func scanPackageImports(packageDir string, wanted []string) packageImportScan {
	scan := packageImportScan{imported: make(map[string]bool, len(wanted)), complete: true}
	if len(wanted) == 0 {
		return scan
	}
	err := filepath.WalkDir(packageDir, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if len(scan.imported) == len(wanted) {
			return fs.SkipAll
		}
		if entry.IsDir() {
			return skipPackageDirectory(current, packageDir, entry.Name())
		}
		if !packageSourceExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			return nil
		}
		data, err := os.ReadFile(current) //nolint:gosec // a source file under the probed package
		if err != nil {
			return err
		}
		for _, match := range importSpecifier.FindAllSubmatch(data, -1) {
			if name := longestPackageMatch(string(match[1]), wanted); name != "" {
				scan.imported[name] = true
			}
		}
		return nil
	})
	if err != nil {
		scan.complete = false
	}
	return scan
}

// skipPackageDirectory keeps the walk on committed sources: the package root is
// always entered, a nested package is a different package, and the directories
// an install or a build writes are skipped.
func skipPackageDirectory(current, packageDir, name string) error {
	if current == packageDir {
		return nil
	}
	switch name {
	case "node_modules", "dist", "build", "coverage":
		return fs.SkipDir
	}
	if strings.HasPrefix(name, ".") {
		return fs.SkipDir
	}
	if info, err := os.Stat(filepath.Join(current, "package.json")); err == nil && !info.IsDir() {
		return fs.SkipDir
	}
	return nil
}

// longestPackageMatch resolves a module specifier to the package that provides
// it: the longest asked-about name that is the specifier itself or its prefix.
// Longest wins so a package nested under another's namespace is credited alone
// for its own subpaths.
func longestPackageMatch(specifier string, wanted []string) string {
	best := ""
	for _, name := range wanted {
		if specifier != name && !strings.HasPrefix(specifier, name+"/") {
			continue
		}
		if len(name) > len(best) {
			best = name
		}
	}
	return best
}
