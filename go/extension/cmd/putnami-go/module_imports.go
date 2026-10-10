package main

import (
	"go/ast"
	"go/build/constraint"
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
//   - build constraints count only through the `ignore` tag: tidy considers
//     every build configuration except one that sets `ignore`, so a file only
//     `//go:build ignore` admits is skipped, and so is a file whose
//     `//go:build` line is malformed or repeated;
//   - a file whose name starts with "." or "_" is skipped, like a directory;
//   - `testdata`, `vendor` and directories whose name starts with "." or "_"
//     are skipped, because the go command loads no package from them — `.gen`
//     is one of them, so a generated tree a build wrote never changes the
//     answer between a cold clone and a warm checkout;
//   - a directory the go.mod's `ignore` directive names is skipped with its
//     whole subtree, because the go command matches no package in it;
//   - a subdirectory holding its own go.mod is a different module.
//
// The scan is read-only and toolchain-free: no `go list`, no module download,
// no build cache. Each file is parsed in imports-only mode, which stops at the
// first declaration. It reads the whole module: an import of a workspace module
// the go.mod does not require is found only by reading every file.

// moduleImportScan is one module's answer: which of the modules asked about its
// own sources import, and whether the walk read everything it meant to.
type moduleImportScan struct {
	// imported holds the asked-about module paths a source file imports.
	imported map[string]bool
	// packages maps each imported package path to the module credited with it.
	packages map[string]string
	// complete reports that every file under the module was read and parsed.
	// Every recorded import is real either way; an incomplete scan only keeps
	// a require from reading as unimported.
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
// providers its sources import. ignores are the module go.mod's `ignore`
// paths.
//
// An import is credited to the longest provider that provides it, so a module
// nested under another one is credited alone for its own packages. An
// unreadable directory or an unparsable file leaves the scan incomplete, and
// the walk reads every other file.
func scanModuleImports(moduleDir string, ignores, providers []string) moduleImportScan {
	scan := moduleImportScan{imported: make(map[string]bool), packages: make(map[string]string), complete: true}
	if len(providers) == 0 {
		return scan
	}
	ignored := newGoIgnoreRule(ignores)
	fset := token.NewFileSet()
	_ = filepath.WalkDir(moduleDir, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			scan.complete = false
			return nil
		}
		if entry.IsDir() {
			return skipGoDirectory(current, moduleDir, entry.Name(), ignored)
		}
		if !strings.HasSuffix(entry.Name(), ".go") || skipGoFile(entry.Name()) {
			return nil
		}
		file, err := parser.ParseFile(fset, current, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			scan.complete = false
			return nil
		}
		if tidyIgnores(fset, file) {
			return nil
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if module := longestModuleMatch(imported, providers); module != "" {
				scan.imported[module] = true
				scan.packages[imported] = module
			}
		}
		return nil
	})
	return scan
}

// skipGoFile reports that the go command loads no package from a file of
// this name.
func skipGoFile(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// tidyIgnores reports that `go mod tidy` reads nothing of file: no build
// configuration with the `ignore` tag unset satisfies its build constraint, or
// its `//go:build` line is malformed or repeated. A `//go:build` line wins over
// `// +build` lines, which are ANDed and count only in a comment group a blank
// line separates from the package clause.
func tidyIgnores(fset *token.FileSet, file *ast.File) bool {
	packageLine := fset.Position(file.Package).Line
	var goBuild []string
	var plusBuild []constraint.Expr
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		separated := fset.Position(group.End()).Line < packageLine-1
		for _, comment := range group.List {
			switch {
			case constraint.IsGoBuild(comment.Text):
				goBuild = append(goBuild, comment.Text)
			case separated && constraint.IsPlusBuild(comment.Text):
				if expr, err := constraint.Parse(comment.Text); err == nil {
					plusBuild = append(plusBuild, expr)
				}
			}
		}
	}
	switch len(goBuild) {
	case 0:
	case 1:
		expr, err := constraint.Parse(goBuild[0])
		return err != nil || !tidyAdmits(expr, true)
	default:
		return true
	}
	for _, expr := range plusBuild {
		if !tidyAdmits(expr, true) {
			return true
		}
	}
	return false
}

// tidyAdmits evaluates expr the way `go mod tidy` does: `ignore` is false, and
// every other tag takes whichever value satisfies the term it sits in.
func tidyAdmits(expr constraint.Expr, prefer bool) bool {
	switch expr := expr.(type) {
	case *constraint.TagExpr:
		return expr.Tag != "ignore" && prefer
	case *constraint.NotExpr:
		return !tidyAdmits(expr.X, !prefer)
	case *constraint.AndExpr:
		return tidyAdmits(expr.X, prefer) && tidyAdmits(expr.Y, prefer)
	case *constraint.OrExpr:
		return tidyAdmits(expr.X, prefer) || tidyAdmits(expr.Y, prefer)
	default:
		return true
	}
}

// skipGoDirectory applies the go command's package-loading rule to one
// directory: the module root is always entered, a nested module is a different
// module, and the names the toolchain never loads a package from are skipped,
// like the directories the go.mod ignores.
func skipGoDirectory(current, moduleDir, name string, ignored goIgnoreRule) error {
	if current == moduleDir {
		return nil
	}
	if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return fs.SkipDir
	}
	if rel, err := filepath.Rel(moduleDir, current); err == nil && ignored.ignores(rel) {
		return fs.SkipDir
	}
	if info, err := os.Stat(filepath.Join(current, "go.mod")); err == nil && !info.IsDir() {
		return fs.SkipDir
	}
	return nil
}

// goIgnoreRule matches a module-relative directory against a go.mod's `ignore`
// paths under the go command's rule (cmd/go/internal/search.IgnorePatterns).
// Both sides are compared with a slash added at each end, so a path matches
// whole directory names only: a path that starts with `./` matches the
// directory it names under the module root and everything below it, and any
// other path matches the same directories at any depth.
type goIgnoreRule struct {
	rooted   []string
	anywhere []string
}

func newGoIgnoreRule(paths []string) goIgnoreRule {
	var rule goIgnoreRule
	for _, ignored := range paths {
		if rest, rooted := strings.CutPrefix(ignored, "./"); rooted {
			rule.rooted = append(rule.rooted, slashEnclosed(rest))
		} else {
			rule.anywhere = append(rule.anywhere, slashEnclosed(ignored))
		}
	}
	return rule
}

// ignores reports that the go command skips the directory at rel, relative to
// the module root.
func (r goIgnoreRule) ignores(rel string) bool {
	dir := slashEnclosed(rel)
	for _, pattern := range r.rooted {
		if strings.HasPrefix(dir, pattern) {
			return true
		}
	}
	for _, pattern := range r.anywhere {
		if strings.Contains(dir, pattern) {
			return true
		}
	}
	return false
}

// slashEnclosed returns p with forward slashes and a slash at each end.
func slashEnclosed(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
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
