package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// A fixture task that declares a budget, in this package, in the e2e packages
// or in their clitest helpers, declares "timeoutMs": -1, the manifest's
// unbounded sentinel, the rule internal/jobs/scheduler pins with
// TestFixtureJobsCarryNoIncidentalWallClockBudget. A task that portable
// execution or `ci change-plan` plans needs a hard deadline and declares
// clitest.PortableTaskDeadlineMs.
//
// A task budget is wall clock from the moment the job is prepared. On a loaded
// host a /bin/sh that exits at once can spend 10 s waiting for a CPU, and the
// job then fails with "job timed out": the CLI exits 1, which reads as the
// regression a test guards. Raising the number only moves the load at which
// the verdict flips. The Go test binary's -timeout stays the backstop.
var fixtureTaskBudget = regexp.MustCompile(`"timeoutMs"\s*:\s*([^\s,}]+)`)

// portableTaskDeadline names clitest.PortableTaskDeadlineMs, the one budget a
// fixture may declare outside a "-1".
const portableTaskDeadline = "PortableTaskDeadlineMs"

// TestFixtureTasksCarryNoIncidentalWallClockBudget reads the test sources of
// this package and of the e2e packages, and the clitest helpers they share,
// because a budget comes back one manifest literal or one TimeoutMs field at a
// time and nothing else notices until a loaded gate fails.
func TestFixtureTasksCarryNoIncidentalWallClockBudget(t *testing.T) {
	t.Parallel()

	var files []string
	for _, pattern := range []string{"*_test.go", "e2e/*/*_test.go", "clitest/*.go"} {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			t.Fatalf("glob %s: %v (%d files)", pattern, err, len(matches))
		}
		files = append(files, matches...)
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
					if match[1] != "-1" {
						t.Errorf("%s: a fixture declares a timeoutMs of %s; declare -1 on a task, omit it on a tool", fset.Position(node.Pos()), match[1])
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && key.Name == "TimeoutMs" {
					checkFixtureBudget(t, fset, node.Value)
				}
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "TimeoutMs" && i < len(node.Rhs) {
						checkFixtureBudget(t, fset, node.Rhs[i])
					}
				}
			}
			return true
		})
	}
}

// checkFixtureBudget accepts a TimeoutMs value that is a negative literal or
// names PortableTaskDeadlineMs, and refuses any other: a positive literal, or
// a constant or expression the guard cannot read.
func checkFixtureBudget(t *testing.T, fset *token.FileSet, value ast.Expr) {
	t.Helper()
	switch value := value.(type) {
	case *ast.UnaryExpr:
		if lit, ok := value.X.(*ast.BasicLit); ok && value.Op == token.SUB && lit.Kind == token.INT {
			return
		}
	case *ast.Ident:
		if value.Name == portableTaskDeadline {
			return
		}
	case *ast.SelectorExpr:
		if value.Sel.Name == portableTaskDeadline {
			return
		}
	}
	t.Errorf("%s: a fixture declares TimeoutMs %s; declare -1 on a task, omit it on a tool, or use clitest.%s where the ChangePlan needs a hard deadline",
		fset.Position(value.Pos()), types.ExprString(value), portableTaskDeadline)
}
