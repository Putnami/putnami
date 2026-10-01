package extension

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The no-manifest-adaptation ratchet.
//
// Contract 3 deleted AdaptReservedFlagShadows and its two
// helpers. Until then, a manifest whose declared cliContract predated the
// current one still LOADED: the loader deleted every flag definition that
// shadowed a reserved global and attached a warning. That is the bridge in its
// purest form — the CLI running a manifest that is not what the extension
// author wrote, adapted by rules nobody re-derives at review time, reported
// through a diagnostic that a passing build hides.
//
// The deletion is currently true because a slice made it true. This test makes
// it structural, so re-adding an adapter is a visible decision rather than a
// helpful-looking commit. Its twin covers the CLI's own loader
// (tooling/cli/internal/cli/v1_bridge_ratchet_test.go); the two modules cannot
// walk each other, so the invariant is stated once per module.
//
// The predicate is a declaration-name prefix. That is a convention made
// enforceable, not a semantic proof — a rewriting function could be called
// anything — and it is chosen because it forces the right review question at
// the right moment: why is this function named Adapt, and what is it changing
// about a document its author signed?
func TestNoManifestAdaptationFunctions(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	scanned := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		scanned++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if strings.HasPrefix(strings.ToLower(fn.Name.Name), "adapt") {
				t.Errorf("%s:%d declares %s — this package validates manifests, it does not rewrite "+
					"them. A manifest either declares the contract this build implements and is "+
					"enforced strictly, or it does not load: contract 3 removed the middle tier, and "+
					"this ratchet pins its absence.", name, fset.Position(fn.Pos()).Line, fn.Name.Name)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no production sources — the assertion would pass vacuously")
	}
}
