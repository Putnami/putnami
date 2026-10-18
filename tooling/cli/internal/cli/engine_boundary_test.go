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
// internal/watch all legitimately import internal/jobs/scheduler — for
// scheduler.JobResult, scheduler.RawJobEvent, scheduler.ScheduledJob,
// scheduler.ReduceRun and the event-type constants. What must not cross the
// boundary is a SYMBOL SET: the execution entry points. depguard cannot express
// "this package may be imported, but only internal/engine may name RunPlan in
// it", so a depguard rule strong enough to
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
// The ~62 direct scheduler constructions live in internal/jobs/scheduler/*_test.go and use
// the package-private newScheduler. They are not a back door: an unexported
// identifier is unreachable from every other package by the COMPILER, test file
// or not. That is why A6a unexported the constructor and the two setters rather
// than exporting a "for tests only" helper — a helper would have had to be
// exported to be usable, and an exported helper is the boundary this slice
// exists to close.

// schedulerImportPath is the package whose execution surface is engine-only.
// The scheduler left internal/jobs, so its surface is restricted under its own
// import path.
const schedulerImportPath = "go.putnami.dev/tooling/cli/internal/jobs/scheduler"

// remoteCacheImportPath is the remote cache client the engine loads and hands
// to the scheduler. It left internal/jobs, so its loader and its type are
// restricted under their own import path.
const remoteCacheImportPath = "go.putnami.dev/tooling/cli/internal/jobs/remotecache"

// plannerImportPath is the planner. It left internal/jobs, so its entry point is
// restricted under its own import path.
const plannerImportPath = "go.putnami.dev/tooling/cli/internal/jobs/planner"

// schedulerTestDir holds the scheduler's own tests. They name the remote cache
// client to wire one into an in-package scheduler, which was unqualified before
// the client left internal/jobs. Its production files may not name it: the
// scheduler reaches the client only through the cachekey.RemoteCache interface.
const schedulerTestDir = "internal/jobs/scheduler"

// releaseSetTestDir holds the release-set and publication tests. They moved
// with their package from internal/jobs to internal/jobs/publication, and some
// of them plan or run a fixture the way the scheduler's own tests do.
const releaseSetTestDir = "internal/jobs/publication"

// plannerPlanTestException is one scheduler or release-set test that plans a
// fixture with planner.Plan so the scheduler can run the plan. It is a named
// file, not a directory, so a new test under either directory cannot start
// planning silently.
type plannerPlanTestException struct {
	// dir is the test's package directory, module relative: schedulerTestDir
	// or releaseSetTestDir.
	dir string
	// file is the test file name in dir.
	file string
	// why states what the test needs a real plan for.
	why string
}

// path is the exception's test file, module relative.
func (e plannerPlanTestException) path() string { return e.dir + "/" + e.file }

// plannerPlanTestExceptions are the tests allowed to name planner.Plan. The
// list is checked in both directions: a listed file that no longer calls
// planner.Plan fails, so the entry is deleted rather than left as a standing
// permission. A test that only plans belongs in internal/jobs/planner, where the
// call is unqualified.
var plannerPlanTestExceptions = []plannerPlanTestException{
	{
		dir:  schedulerTestDir,
		file: "integration_test.go",
		why:  "plans a fixture workspace, then runs the plan end to end through the scheduler",
	},
	{
		dir:  releaseSetTestDir,
		file: "release_set_deployment_test.go",
		why:  "plans a package step, then reads release-set fixtures and helpers that live in the publication package",
	},
	{
		dir:  schedulerTestDir,
		file: "tree_keyed_cache_test.go",
		why:  "plans under a history fixture, then calls the scheduler package's unexported version-file step and BuildRunVersions",
	},
}

// schedulerRunTestException is one release-set or publication test that runs
// its fixture plan with scheduler.RunPlan, as it did in-package before the
// scheduler left internal/jobs.
type schedulerRunTestException struct {
	// file is the test file, module relative, directly under releaseSetTestDir.
	file string
	// why states what the test needs a real run for.
	why string
}

// schedulerRunSymbols are the restricted scheduler symbols a test listed in
// schedulerRunTestExceptions may name: the run entry, its request and its
// tuning. Constructing a Scheduler or reading a raw SchedulerResult stays
// engine-only.
var schedulerRunSymbols = map[string]bool{"RunPlan": true, "RunRequest": true, "SchedulerConfig": true}

// schedulerRunTestExceptions are the tests outside the engine allowed to name
// schedulerRunSymbols. The list is checked in both directions, like
// plannerPlanTestExceptions: a listed file that no longer calls
// scheduler.RunPlan fails, so the entry is deleted.
var schedulerRunTestExceptions = []schedulerRunTestException{
	{
		file: "internal/jobs/publication/publication_test.go",
		why:  "runs a publication plan through the scheduler, so the engine-side uploads and the outbox seam run as they do in a release",
	},
	{
		file: "internal/jobs/publication/release_set_e2e_test.go",
		why:  "runs a release-set plan end to end through the scheduler: the barrier, the publish jobs, the finalizer and a deploy",
	},
}

// enginePackageDir is the one package allowed to name the restricted symbols,
// as a module-relative directory. Files in it — production and test alike — may
// name them; every other package in the module may not.
const enginePackageDir = "internal/engine"

// restrictedJobSymbols are, by import path, the exports that constitute
// "running a plan". Naming any of them outside internal/engine is a second run
// loop being born.
var restrictedJobSymbols = map[string]map[string]string{
	schedulerImportPath: {
		"RunPlan":         "the ONE execution entry (engine/execute.go); a second caller is a run without the session file, run markers, profiler or remote cache",
		"RunRequest":      "RunPlan's argument; naming it outside the engine means assembling a run elsewhere",
		"Scheduler":       "ADR 0001 §3: no adapter constructs or configures a scheduler.Scheduler",
		"SchedulerConfig": "scheduler tuning is derived from the engine Request's global flags, in one place",
		"SchedulerResult": "the raw scheduler result; adapters read the canonical engine.SessionResult instead",
	},
	remoteCacheImportPath: {
		"RemoteCache":     "remote-cache lifetime is engine-owned (setup timing, Close after the run marker is published)",
		"LoadRemoteCache": "same: a second loader means a run whose cache stats and trust policy nobody reconciled",
	},
	plannerImportPath: {
		"Plan": "planning is an engine stage (engine/plan.go); a private plan skips contract validation and the missing-extension/starved-command guards",
	},
}

// jobSymbolReference is one selector expression naming a restricted symbol.
type jobSymbolReference struct {
	file   string
	line   int
	pkg    string // the import path the symbol is restricted under
	symbol string
}

// qualified spells the reference the way the source does by default, as
// "scheduler.RunPlan" or "remotecache.LoadRemoteCache".
func (r jobSymbolReference) qualified() string {
	return r.pkg[strings.LastIndex(r.pkg, "/")+1:] + "." + r.symbol
}

// boundaryExempt reports whether ref may name its symbol outside the engine: a
// scheduler test naming the remote cache client, one of the tests listed in
// plannerPlanTestExceptions naming planner.Plan, or one of the tests listed in
// schedulerRunTestExceptions naming a schedulerRunSymbols symbol.
func boundaryExempt(ref jobSymbolReference) bool {
	if !strings.HasSuffix(ref.file, "_test.go") {
		return false
	}
	switch ref.pkg {
	case remoteCacheImportPath:
		return filepath.ToSlash(filepath.Dir(ref.file)) == schedulerTestDir
	case plannerImportPath:
		for _, exception := range plannerPlanTestExceptions {
			if exception.path() == ref.file {
				return true
			}
		}
	case schedulerImportPath:
		if !schedulerRunSymbols[ref.symbol] {
			return false
		}
		for _, exception := range schedulerRunTestExceptions {
			if exception.file == ref.file {
				return true
			}
		}
	}
	return false
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
			engineRefs[ref.qualified()] = true
			continue
		}
		if boundaryExempt(ref) {
			continue
		}
		violations = append(violations, ref.file+":"+strconv.Itoa(ref.line)+
			"  "+ref.qualified()+" — "+restrictedJobSymbols[ref.pkg][ref.symbol])
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("%d reference(s) to the jobs execution surface outside %s:\n    %s\n"+
			"  ADR 0001 §3: the scheduler is an internal execution stage. Route the work through\n"+
			"  engine.Engine.Run instead — it owns selection, planning, preflight, the cache, the\n"+
			"  session file, run markers and the canonical result. Adding a case here is a new ADR.",
			len(violations), enginePackageDir, strings.Join(violations, "\n    "))
	}

	// Self-check: if the scan found nothing in the engine either, the walk or an
	// import path is wrong and the assertion above passed vacuously. The engine
	// calls these entry points today, by construction.
	for _, symbol := range []string{"planner.Plan", "scheduler.RunPlan", "remotecache.LoadRemoteCache"} {
		if !engineRefs[symbol] {
			t.Fatalf("scan found no reference to %s in %s — the boundary scan is broken, not the tree "+
				"(check %s, %s, %s and the walk root %s)", symbol, enginePackageDir, schedulerImportPath, plannerImportPath, remoteCacheImportPath, root)
		}
	}
}

// TestEngineBoundary_OnlySchedulerTestsNameTheRemoteCacheClientOrPlan pins the
// exemptions: a scheduler test may name the remote cache client, only the
// tests listed in plannerPlanTestExceptions may name planner.Plan, only the
// release-set and publication tests listed in schedulerRunTestExceptions may
// name the scheduler's run entry, its request and its tuning, and a production
// file or any other package may name none of them.
func TestEngineBoundary_OnlySchedulerTestsNameTheRemoteCacheClientOrPlan(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		ref  jobSymbolReference
		want bool
	}{
		{jobSymbolReference{file: "internal/jobs/scheduler/remote_task_test.go", pkg: remoteCacheImportPath, symbol: "LoadRemoteCache"}, true},
		{jobSymbolReference{file: "internal/jobs/scheduler/scheduler.go", pkg: remoteCacheImportPath, symbol: "RemoteCache"}, false},
		{jobSymbolReference{file: "internal/jobs/publication/release_set_test.go", pkg: remoteCacheImportPath, symbol: "RemoteCache"}, false},
		{jobSymbolReference{file: "internal/jobs/toolchain/toolchain_test.go", pkg: remoteCacheImportPath, symbol: "RemoteCache"}, false},
		{jobSymbolReference{file: "internal/commands/sessions/sessions_run_test.go", pkg: remoteCacheImportPath, symbol: "LoadRemoteCache"}, false},
		{jobSymbolReference{file: "internal/jobs/scheduler/run_plan_test.go", pkg: schedulerImportPath, symbol: "RunPlan"}, false},
		{jobSymbolReference{file: "internal/jobs/publication/release_set_e2e_test.go", pkg: schedulerImportPath, symbol: "RunPlan"}, true},
		{jobSymbolReference{file: "internal/jobs/publication/publication_test.go", pkg: schedulerImportPath, symbol: "SchedulerConfig"}, true},
		{jobSymbolReference{file: "internal/jobs/publication/release_set_e2e_test.go", pkg: schedulerImportPath, symbol: "Scheduler"}, false},
		{jobSymbolReference{file: "internal/jobs/publication/unlisted_test.go", pkg: schedulerImportPath, symbol: "RunPlan"}, false},
		{jobSymbolReference{file: "internal/jobs/publication/release_set.go", pkg: schedulerImportPath, symbol: "RunPlan"}, false},
		{jobSymbolReference{file: "internal/jobs/release_set_e2e_test.go", pkg: schedulerImportPath, symbol: "RunPlan"}, false},
		{jobSymbolReference{file: "internal/jobs/scheduler/integration_test.go", pkg: plannerImportPath, symbol: "Plan"}, true},
		{jobSymbolReference{file: "internal/jobs/publication/release_set_deployment_test.go", pkg: plannerImportPath, symbol: "Plan"}, true},
		{jobSymbolReference{file: "internal/jobs/release_set_deployment_test.go", pkg: plannerImportPath, symbol: "Plan"}, false},
		{jobSymbolReference{file: "internal/jobs/scheduler/unlisted_test.go", pkg: plannerImportPath, symbol: "Plan"}, false},
		{jobSymbolReference{file: "internal/jobs/publication/integration_test.go", pkg: plannerImportPath, symbol: "Plan"}, false},
		{jobSymbolReference{file: "internal/jobs/scheduler/scheduler.go", pkg: plannerImportPath, symbol: "Plan"}, false},
		{jobSymbolReference{file: "internal/commands/lifecycle/deps_test.go", pkg: plannerImportPath, symbol: "Plan"}, false},
	} {
		if got := boundaryExempt(c.ref); got != c.want {
			t.Errorf("boundaryExempt(%s in %s) = %v, want %v", c.ref.qualified(), c.ref.file, got, c.want)
		}
	}
}

// TestEngineBoundary_PlannerPlanTestExceptionsAreExact checks the exception
// list in both directions: every listed file exists and still names planner.Plan.
// The other direction, a file naming planner.Plan that is not listed, is a
// violation of TestEngineBoundary_OnlyTheEngineRunsJobs.
func TestEngineBoundary_PlannerPlanTestExceptionsAreExact(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	naming := map[string]bool{}
	for _, ref := range restrictedJobReferences(t, root) {
		if ref.pkg == plannerImportPath && ref.symbol == "Plan" {
			naming[ref.file] = true
		}
	}
	for _, exception := range plannerPlanTestExceptions {
		if !strings.HasSuffix(exception.file, "_test.go") || strings.Contains(exception.file, "/") ||
			(exception.dir != schedulerTestDir && exception.dir != releaseSetTestDir) {
			t.Errorf("%s is not a test file directly under %s or %s — the exemption covers scheduler and release-set tests only",
				exception.path(), schedulerTestDir, releaseSetTestDir)
		}
		if strings.TrimSpace(exception.why) == "" {
			t.Errorf("%s is exempt without a reason — state what the test needs a real plan for", exception.path())
		}
		if !naming[exception.path()] {
			t.Errorf("%s no longer names planner.Plan, so its exception is stale — delete it.\n"+
				"  An exception nobody needs is a standing permission the next test under %s falls through.",
				exception.path(), exception.dir)
		}
	}
}

// TestEngineBoundary_SchedulerRunTestExceptionsAreExact checks the run
// exception list in both directions: every listed file is a release-set test
// that still names scheduler.RunPlan. The other direction, a file naming the
// run surface that is not listed, is a violation of
// TestEngineBoundary_OnlyTheEngineRunsJobs.
func TestEngineBoundary_SchedulerRunTestExceptionsAreExact(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	running := map[string]bool{}
	for _, ref := range restrictedJobReferences(t, root) {
		if ref.pkg == schedulerImportPath && ref.symbol == "RunPlan" {
			running[ref.file] = true
		}
	}
	for _, exception := range schedulerRunTestExceptions {
		if !strings.HasSuffix(exception.file, "_test.go") ||
			filepath.ToSlash(filepath.Dir(exception.file)) != releaseSetTestDir {
			t.Errorf("%s is not a test file directly under %s — the exemption covers release-set tests only",
				exception.file, releaseSetTestDir)
		}
		if strings.TrimSpace(exception.why) == "" {
			t.Errorf("%s is exempt without a reason — state what the test needs a real run for", exception.file)
		}
		if !running[exception.file] {
			t.Errorf("%s no longer names scheduler.RunPlan, so its exception is stale — delete it.\n"+
				"  An exception nobody needs is a standing permission the next test under %s falls through.",
				exception.file, releaseSetTestDir)
		}
	}
}

// restrictedJobReferences returns every selector expression in the module that
// names a restricted symbol through an import of internal/jobs/scheduler or
// internal/jobs/remotecache or internal/jobs/planner, as module relative "dir/file.go" paths.
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
		locals := map[string]string{}
		for importPath := range restrictedJobSymbols {
			if local, imported := jobsImportName(file, importPath); imported {
				locals[local] = importPath
			}
		}
		if len(locals) == 0 {
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
			if !ok || pkg.Obj != nil {
				return true
			}
			importPath, ok := locals[pkg.Name]
			if !ok {
				return true
			}
			if _, restricted := restrictedJobSymbols[importPath][selector.Sel.Name]; !restricted {
				return true
			}
			refs = append(refs, jobSymbolReference{
				file:   rel,
				line:   fset.Position(selector.Sel.Pos()).Line,
				pkg:    importPath,
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

// jobsImportName returns the identifier importPath is bound to in file (its
// alias, or the last element of the path), and whether the file imports it at
// all. A blank or dot import is reported as not imported: neither can produce
// a "scheduler.X" selector, and a dot import of internal/jobs/scheduler would
// not compile against the unexported constructor anyway.
func jobsImportName(file *ast.File, importPath string) (string, bool) {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if spec.Name == nil {
			return importPath[strings.LastIndex(importPath, "/")+1:], true
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
// unexported, so no package outside internal/jobs/scheduler can reach them however hard it
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
	path := filepath.Join(root, "internal", "jobs", "scheduler", "scheduler.go")
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
			t.Errorf("internal/jobs/scheduler/scheduler.go declares exported %s — it must be %s.\n"+
				"  Exporting it hands every package a way to build a run without the engine's\n"+
				"  session, run markers, profiler and remote cache (ADR 0001 §3).", fn.Name.Name, unexported)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("internal/jobs/scheduler/scheduler.go no longer declares %s; if it moved, move this pin with it", name)
		}
	}
}
