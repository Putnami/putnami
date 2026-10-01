package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This package was the slowest of `/tooling/cli test~test` because its
// serial tests (the ones without t.Parallel()) ran one after another for
// 248 s of a 276 s package, and 164 s of that was 32 end-to-end tests that
// each run a whole CLI session with a provider child. Go serializes tests
// that touch process-wide state only within one test binary, and a package
// is a binary, so those tests now live in the sibling packages under
// internal/cli/e2e, sharing internal/cli/clitest. Nothing production changed.
//
// The rule this pin holds: an end-to-end test that swaps os.Stdout or
// os.Stderr for the whole process, changes the working directory (t.Chdir) or
// sets the environment (t.Setenv) to drive a full CLI session goes into an
// e2e/* package, never here. A test that stays here and cannot be parallel
// is one that inspects package-private state (a stubbed package variable, a
// registry the process owns) — the cheap kind. The count only comes down:
// a slice that adds a serial test here says why in this comment and raises
// the number, as structural_baseline_test.go's ceilings do.
//
// RAISED 125→127 (ADR 0055), for two tests that install a credential source for
// the whole process, which no parallel download may see:
// TestInstallCredentialProvidersSourcesTheFetchJob reads the source that
// installCredentialProviders installs, and
// TestBootstrapProviderServesOnlyTheLockedDownloads calls the package-private
// openBootstrapProvider, so it cannot move to an e2e package.
//
// RAISED 127→128 (cli/provider-publication), for
// TestPublishPurposeStartsTheProviderBeforeTheFirstHook: it calls the
// package-private installCredentialProviders, and its install-only control
// installs a read credential source for the whole process.
//
// The measurement is the AST, not a regexp: a top-level Test function whose
// body has no `t.Parallel()` statement at its top level. A t.Parallel() inside
// a subtest closure does not free the parent, so it does not count.
const serialTestCeiling = 128

func TestSerialTests_StayUnderTheCeiling(t *testing.T) {
	t.Parallel()
	serial := serialTestsInPackage(t)
	if len(serial) > serialTestCeiling {
		t.Errorf("internal/cli holds %d serial tests, ceiling %d — %d added.\n"+
			"  An end-to-end test that swaps the process streams, changes directory or sets the\n"+
			"  environment belongs in an internal/cli/e2e package (see internal/cli/clitest).\n"+
			"  Serial tests:\n    %s",
			len(serial), serialTestCeiling, len(serial)-serialTestCeiling, strings.Join(serial, "\n    "))
	}
	t.Logf("internal/cli: %d serial tests (ceiling %d)", len(serial), serialTestCeiling)
}

// serialTestsInPackage returns "file: TestName" for every top-level test in
// this directory (subpackages excluded: each is its own binary) that does not
// call t.Parallel() at the top level of its body.
func serialTestsInPackage(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var serial []string
	tests := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !isTestFunc(fn) {
				continue
			}
			tests++
			if !callsParallelAtTopLevel(fn) {
				serial = append(serial, name+": "+fn.Name.Name)
			}
		}
	}
	if tests == 0 {
		t.Fatal("found no test functions — the scan directory is wrong")
	}
	sort.Strings(serial)
	return serial
}

// isTestFunc reports whether fn is a top-level test: named Test*, taking one
// parameter of type *testing.T.
func isTestFunc(fn *ast.FuncDecl) bool {
	if !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && selector.Sel.Name == "T"
}

// callsParallelAtTopLevel reports whether one of the body's own statements is
// a call to <param>.Parallel() on the test's *testing.T parameter.
func callsParallelAtTopLevel(fn *ast.FuncDecl) bool {
	param := ""
	if names := fn.Type.Params.List[0].Names; len(names) == 1 {
		param = names[0].Name
	}
	for _, stmt := range fn.Body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Parallel" {
			continue
		}
		if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == param {
			return true
		}
	}
	return false
}
