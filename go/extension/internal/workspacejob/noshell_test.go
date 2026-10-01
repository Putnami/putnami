package workspacejob_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
)

// lifecycleJobPackages are the packages `putnami install` and
// `putnami upgrade` run in a consumer workspace, relative to this one.
var lifecycleJobPackages = []string{
	".",
	filepath.Join("..", "jobs", "workspaceinstall"),
	filepath.Join("..", "jobs", "depsupgrade"),
}

// TestLifecycleJobsStartNoShell is the static half of the no-shell proof (the
// dynamic half puts traps named after every shell first on PATH and runs the
// jobs): the one place these packages start a process is Job.run, which is
// handed the resolved go command or a pinned tool, and no source file names a
// shell or one of the programs the former scripts ran.
func TestLifecycleJobsStartNoShell(t *testing.T) {
	forbiddenCalls := map[string]bool{
		"syscall.Exec": true, "syscall.ForkExec": true, "syscall.StartProcess": true,
		"os.StartProcess": true, "exec.Command": true,
	}
	forbiddenFragments := []string{"/bin/sh", "/bin/bash", "cmd.exe", "powershell", "pwsh", "/c ", "-c "}
	programs := map[string]bool{}
	for _, program := range jobtest.ShellPrograms {
		programs[program] = true
		programs[program+".exe"] = true
	}

	parsed := 0
	for _, dir := range lifecycleJobPackages {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			parsed++
			for _, spec := range file.Imports {
				imported, _ := strconv.Unquote(spec.Path.Value)
				if imported == "os/exec" && path != "job.go" {
					t.Errorf("%s imports os/exec; only Job.run may start a process", path)
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := n.X.(*ast.Ident); ok && forbiddenCalls[pkg.Name+"."+n.Sel.Name] {
						t.Errorf("%s: %s.%s starts a process outside Job.run", fset.Position(n.Pos()), pkg.Name, n.Sel.Name)
					}
				case *ast.BasicLit:
					if n.Kind != token.STRING {
						return true
					}
					value, err := strconv.Unquote(n.Value)
					if err != nil {
						return true
					}
					if programs[value] {
						t.Errorf("%s: the literal %q names a program the jobs must not start", fset.Position(n.Pos()), value)
					}
					for _, fragment := range forbiddenFragments {
						if strings.Contains(value, fragment) {
							t.Errorf("%s: the literal %q holds the shell fragment %q", fset.Position(n.Pos()), value, fragment)
						}
					}
				}
				return true
			})
		}
	}
	if parsed < 10 {
		t.Fatalf("parsed %d source files; the scan no longer reaches the job packages", parsed)
	}
}
