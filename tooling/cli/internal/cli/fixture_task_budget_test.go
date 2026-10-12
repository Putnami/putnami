package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// A fixture task in this package carries "timeoutMs": -1, the manifest's
// unbounded sentinel, the rule internal/jobs pins with
// TestFixtureJobsCarryNoIncidentalWallClockBudget.
//
// A task budget is wall clock from the moment the job is prepared. On a loaded
// host a /bin/sh that exits at once can spend 10 s waiting for a CPU, and the
// job then fails with "job timed out": the CLI exits 1, which reads as the
// regression a test guards. Raising the number only moves the load at which
// the verdict flips. The Go test binary's -timeout stays the backstop.
var fixtureTaskBudget = regexp.MustCompile(`"timeoutMs"\s*:\s*(\d+)`)

// TestFixtureTasksCarryNoIncidentalWallClockBudget reads this package's test
// sources, because a budget comes back one manifest literal at a time and
// nothing else notices until a loaded gate fails.
//
// The exception list is a census, not an allowlist: every name must still
// carry a literal budget, so a function that stops needing one leaves the list.
func TestFixtureTasksCarryNoIncidentalWallClockBudget(t *testing.T) {
	t.Parallel()

	budgetIsNotATaskTimeout := map[string]bool{
		// An extension tool definition: the tool runner has no unbounded
		// sentinel and replaces a missing budget with its own 30 s default.
		"collabCommandTool": true,
	}

	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob package test files: %v (%d files)", err, len(files))
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				lit, ok := node.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				for _, match := range fixtureTaskBudget.FindAllStringSubmatch(lit.Value, -1) {
					budget, err := strconv.ParseInt(match[1], 10, 64)
					if err != nil || budget <= 0 {
						continue
					}
					if budgetIsNotATaskTimeout[fn.Name.Name] {
						seen[fn.Name.Name] = true
						continue
					}
					t.Errorf("%s: %s gives a fixture task \"timeoutMs\": %d; use -1, or list the function here if the budget is not a task timeout",
						fset.Position(lit.Pos()), fn.Name.Name, budget)
				}
				return true
			})
		}
	}
	for name := range budgetIsNotATaskTimeout {
		if !seen[name] {
			t.Errorf("%s no longer carries a literal budget; remove it from the exception census", name)
		}
	}
}
