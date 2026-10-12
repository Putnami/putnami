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

// A fixture task that declares a budget, in this package, in the e2e packages
// or in their clitest helpers, declares "timeoutMs": -1, the manifest's
// unbounded sentinel, the rule internal/jobs pins with
// TestFixtureJobsCarryNoIncidentalWallClockBudget. A task that portable
// execution or `ci change-plan` plans needs a hard deadline and declares
// clitest.PortableTaskDeadlineMs.
//
// A task budget is wall clock from the moment the job is prepared. On a loaded
// host a /bin/sh that exits at once can spend 10 s waiting for a CPU, and the
// job then fails with "job timed out": the CLI exits 1, which reads as the
// regression a test guards. Raising the number only moves the load at which
// the verdict flips. The Go test binary's -timeout stays the backstop.
var fixtureTaskBudget = regexp.MustCompile(`"timeoutMs"\s*:\s*(\d+)`)

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
