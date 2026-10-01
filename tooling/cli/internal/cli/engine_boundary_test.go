package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The engine boundary, enforced.
//
// ADR 0001 §3 says the scheduler is an internal execution stage and no adapter
// constructs or configures one. Slices A3a–A5b made that TRUE (five run loops
// collapsed to one); this test makes it ENFORCED, so the sixth run loop cannot
// appear the first time someone needs to execute jobs and does not know the
// engine exists.
//
// # Why a boundary test and not depguard
//
// depguard is an IMPORT-path linter, and the invariant here is not about import
// paths: internal/cli, internal/commands, internal/mcp, internal/output and
// internal/watch all legitimately import internal/jobs — for jobs.JobResult,
// jobs.RawJobEvent, jobs.ScheduledJob, jobs.ReduceRun and the event-type
// constants. What must not cross the boundary is a SYMBOL SET: the execution
// entry points. depguard cannot express "this package may be imported, but only
// internal/engine may name RunPlan in it", so a depguard rule strong enough to
// hold the invariant would have to ban the whole package and would be turned off
// within a week.
//
// The other candidate, a golang.org/x/tools/go/packages type-checked walk, would
// add the module's first external dependency for a check that go/ast already
// answers — internal/cli/catalog_source_test.go (A1b) already reads this
// package's source with go/ast for exactly this class of assertion. So: stdlib
// AST, no new dependency, no new linter plugin. It reads SELECTOR EXPRESSIONS,
// not text, so a comment, a doc reference or a test fixture string that names a
// restricted symbol is correctly ignored — which line-oriented token counting
// (structural_baseline_test.go) cannot do.
//
// # How the test seam stays out of production reach
//
// The ~62 direct scheduler constructions live in internal/jobs/*_test.go and use
// the package-private newScheduler. They are not a back door: an unexported
// identifier is unreachable from every other package by the COMPILER, test file
// or not. That is why A6a unexported the constructor and the two setters rather
// than exporting a "for tests only" helper — a helper would have had to be
// exported to be usable, and an exported helper is the boundary this slice
// exists to close.

// jobsImportPath is the package whose execution surface is engine-only.
const jobsImportPath = "go.putnami.dev/tooling/cli/internal/jobs"

// enginePackageDir is the one package allowed to name the restricted symbols,
// as a module-relative directory. Files in it — production and test alike — may
// name them; every other package in the module may not.
const enginePackageDir = "internal/engine"

// restrictedJobSymbols are the jobs exports that constitute "running a plan".
// Naming any of them outside internal/engine is a second run loop being born.
var restrictedJobSymbols = map[string]string{
	"Plan":            "planning is an engine stage (engine/plan.go); a private plan skips contract validation and the missing-extension/starved-command guards",
	"RunPlan":         "the ONE execution entry (engine/execute.go); a second caller is a run without the session file, run markers, profiler or remote cache",
	"RunRequest":      "RunPlan's argument; naming it outside the engine means assembling a run elsewhere",
	"Scheduler":       "ADR 0001 §3: no adapter constructs or configures a jobs.Scheduler",
	"SchedulerConfig": "scheduler tuning is derived from the engine Request's global flags, in one place",
	"SchedulerResult": "the raw scheduler result; adapters read the canonical engine.SessionResult instead",
	"RemoteCache":     "remote-cache lifetime is engine-owned (setup timing, Close after the run marker is published)",
	"LoadRemoteCache": "same: a second loader means a run whose cache stats and trust policy nobody reconciled",
}

// jobSymbolReference is one selector expression naming a restricted symbol.
type jobSymbolReference struct {
	file   string
	line   int
	symbol string
}

func TestEngineBoundary_OnlyTheEngineRunsJobs(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "single-engine", "only-the-engine-runs-jobs")
	t.Parallel()
	root := moduleRoot(t)
	refs := restrictedJobReferences(t, root)

	var violations []string
	engineRefs := map[string]bool{}
	for _, ref := range refs {
		if strings.HasPrefix(ref.file, enginePackageDir+"/") {
			engineRefs[ref.symbol] = true
			continue
		}
		violations = append(violations, ref.file+":"+strconv.Itoa(ref.line)+
			"  jobs."+ref.symbol+" — "+restrictedJobSymbols[ref.symbol])
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("%d reference(s) to the jobs execution surface outside %s:\n    %s\n"+
			"  ADR 0001 §3: the scheduler is an internal execution stage. Route the work through\n"+
			"  engine.Engine.Run instead — it owns selection, planning, preflight, the cache, the\n"+
			"  session file, run markers and the canonical result. Adding a case here is a new ADR.",
			len(violations), enginePackageDir, strings.Join(violations, "\n    "))
	}

	// Self-check: if the scan found nothing in the engine either, the walk or the
	// import path is wrong and the assertion above passed vacuously. Both entry
	// points are called from internal/engine today, by construction.
	for _, symbol := range []string{"Plan", "RunPlan"} {
		if !engineRefs[symbol] {
			t.Fatalf("scan found no reference to jobs.%s in %s — the boundary scan is broken, not the tree "+
				"(check %s and the walk root %s)", symbol, enginePackageDir, jobsImportPath, root)
		}
	}
}

// restrictedJobReferences returns every selector expression in the module that
// names a restricted symbol through an import of internal/jobs, as module
// relative "dir/file.go" paths.
//
// It reads the AST, so occurrences in comments and string literals do not count:
// structural_baseline_test.go and several doc comments spell these names on
// purpose, and a boundary that fired on prose would be edited away.
func restrictedJobReferences(t *testing.T, root string) []jobSymbolReference {
	t.Helper()
	var refs []jobSymbolReference
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", ".putnami", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		scanned++

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		local, imported := jobsImportName(file)
		if !imported {
			return nil
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			// pkg.Obj != nil means the identifier resolved to a local
			// declaration in this file (a variable or parameter that shadows the
			// import name), so it is not the package qualifier.
			if !ok || pkg.Name != local || pkg.Obj != nil {
				return true
			}
			if _, restricted := restrictedJobSymbols[selector.Sel.Name]; !restricted {
				return true
			}
			refs = append(refs, jobSymbolReference{
				file:   rel,
				line:   fset.Position(selector.Sel.Pos()).Line,
				symbol: selector.Sel.Name,
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("scanned no Go files under %s — the walk root is wrong", root)
	}
	return refs
}

// jobsImportName returns the identifier internal/jobs is bound to in file (its
// alias, or "jobs"), and whether the file imports it at all. A blank or dot
// import is reported as not imported: neither can produce a "jobs.X" selector,
// and a dot import of internal/jobs would not compile against the unexported
// constructor anyway.
func jobsImportName(file *ast.File) (string, bool) {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != jobsImportPath {
			continue
		}
		if spec.Name == nil {
			return "jobs", true
		}
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return "", false
		}
		return spec.Name.Name, true
	}
	return "", false
}

// TestEngineBoundary_SchedulerSeamIsPackagePrivate pins the compiler-enforced
// half of the boundary: the scheduler's construction and configuration seams are
// unexported, so no package outside internal/jobs can reach them however hard it
// tries — including a test file, which is what makes the ~62 in-package
// constructions safe rather than a back door.
//
// It reads declarations from the AST rather than calling them, because calling
// an identifier that does not exist would not compile: a deleted assertion is
// invisible, and a renamed-back-to-exported constructor must fail loudly.
func TestEngineBoundary_SchedulerSeamIsPackagePrivate(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "single-engine", "the-scheduler-seam-is-package-private")
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "jobs", "scheduler.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// want maps each seam to whether it was found. All three must be unexported;
	// an exported spelling of any of them is the failure.
	want := map[string]bool{"newScheduler": false, "setRemoteCache": false, "setSessionEventHandler": false}
	forbidden := map[string]string{
		"NewScheduler":           "newScheduler",
		"SetRemoteCache":         "setRemoteCache",
		"SetSessionEventHandler": "setSessionEventHandler",
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if _, tracked := want[fn.Name.Name]; tracked {
			want[fn.Name.Name] = true
		}
		if unexported, isForbidden := forbidden[fn.Name.Name]; isForbidden {
			t.Errorf("internal/jobs/scheduler.go declares exported %s — it must be %s.\n"+
				"  Exporting it hands every package a way to build a run without the engine's\n"+
				"  session, run markers, profiler and remote cache (ADR 0001 §3).", fn.Name.Name, unexported)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("internal/jobs/scheduler.go no longer declares %s; if it moved, move this pin with it", name)
		}
	}
}
