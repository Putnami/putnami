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

// A fixture task in this package that declares a budget declares
// "timeoutMs": -1, the manifest's unbounded sentinel, the rule internal/jobs
// pins with TestFixtureJobsCarryNoIncidentalWallClockBudget.
//
// A task budget is wall clock from the moment the job is prepared. On a loaded
// host a /bin/sh that exits at once can spend 10 s waiting for a CPU, and the
// job then fails with "job timed out": the CLI exits 1, which reads as the
// regression a test guards. Raising the number only moves the load at which
// the verdict flips. The Go test binary's -timeout stays the backstop.
var fixtureTaskBudget = regexp.MustCompile(`"timeoutMs"\s*:\s*(\d+)`)

// TestFixtureTasksCarryNoIncidentalWallClockBudget reads this package's test
// sources, because a budget comes back one manifest literal or one TimeoutMs
// field at a time and nothing else notices until a loaded gate fails.
func TestFixtureTasksCarryNoIncidentalWallClockBudget(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob package test files: %v (%d files)", err, len(files))
	}
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(node.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s: %v", fset.Position(node.Pos()), node.Value, err)
				}
				for _, match := range fixtureTaskBudget.FindAllStringSubmatch(text, -1) {
					if budget, err := strconv.ParseInt(match[1], 10, 64); err == nil && budget > 0 {
						t.Errorf("%s: a fixture declares \"timeoutMs\": %d; declare -1 on a task, omit it on a tool", fset.Position(node.Pos()), budget)
					}
				}
			case *ast.KeyValueExpr:
				key, ok := node.Key.(*ast.Ident)
				if !ok || key.Name != "TimeoutMs" {
					return true
				}
				lit, ok := node.Value.(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					return true // an identifier, a call, or a negative literal: no budget
				}
				if budget, err := strconv.ParseInt(lit.Value, 0, 64); err == nil && budget > 0 {
					t.Errorf("%s: a fixture declares TimeoutMs: %d; declare -1 on a task, omit it on a tool", fset.Position(node.Pos()), budget)
				}
			}
			return true
		})
	}
}
