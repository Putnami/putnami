package cli

import (
	"go/ast"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Complexity ceilings for the five packages the epic rebuilt.
//
// The epic's deletions (one execution engine, one result model, one machine
// contract, one lock format) all shrank these packages. Nothing keeps them
// shrunk. Every one of them is somewhere a slice would naturally add "just one
// more helper", and the sum of those is how the five run loops and ~20 result
// shapes the epic measured got built the first time — never by a decision,
// always by an increment.
//
// # What is measured, and why these two numbers
//
// Per unit: the number of production FILES and the number of top-level
// FUNCTION DECLARATIONS (methods included). Both are read from the AST, so
// neither moves when a doc comment grows — which matters in this tree, where
// the prose-to-code ratio is deliberately high and a line-count ceiling would
// fire on every well-explained change and be raised until it meant nothing.
//
// Files catch SPRAWL (a package growing new corners); functions catch the
// increment (a package growing new behavior in place). A refactor that splits
// one large file into two, or extracts a helper, is a real movement of both —
// and is exactly the kind of movement that should cost one line of review here.
//
// # A third quantity, for the units that need it: CONCENTRATION
//
// Files and functions are both TOTALS, and a total cannot see distribution. A
// unit that merges its five files back into one keeps the same function count
// and reports FEWER files — which a ceiling-only pin passes — while recreating
// the single mixed-concern file that made a change expensive to review in the
// first place. perFileFuncs pins the unit's LARGEST file in that same
// AST quantity, and it is opt-in rather than universal: a per-file cap on a
// unit whose file boundaries are incidental would fire on ordinary work and
// teach nobody anything, while on a unit whose files ARE its invariants it
// fails exactly when a second invariant moves into one of them.
//
// # Ceiling-only, on purpose
//
// Growth fails, shrinkage passes. These are pins on complexity the epic exists
// to have REMOVED, so failing CI for a reduction would fail it for succeeding
// (the same reasoning as structural_baseline_test.go's ceilingOnly pins). A
// slice that legitimately grows a unit raises its number here, and its commit
// message says what grew and why. `why` states why the current numbers hold. It
// changes only when the unit's reason changes, and it never records a raise or a
// lowering: the history of every move lives in Git.
//
// # Units, not packages
//
// The planner and the scheduler are packages of their own, internal/jobs/planner
// and internal/jobs/scheduler, because the engine boundary holds by import path:
// only internal/engine names planner.Plan and scheduler.RunPlan
// (engine_boundary_test.go). A unit is still a path PREFIX, not a package: the
// scheduler package also holds the runner and the cache use, so the scheduler
// and invocation units are file groups inside it.

// complexityCeiling is one unit's pinned size. Both counts are ceilings.
type complexityCeiling struct {
	// unit is the name used in failures.
	unit string
	// prefix selects the unit's production files by module-relative path.
	prefix string
	// files is the current production file count.
	files int
	// funcs is the current top-level function declaration count, methods
	// included.
	funcs int
	// perFileFuncs, when non-zero, caps the top-level function declarations of
	// the unit's LARGEST single file. files and funcs bound how big a unit is;
	// this bounds how CONCENTRATED it is, which the two totals cannot see.
	// Opt-in: it is set by the units whose file boundaries carry meaning, and
	// the number is the measured maximum rather than a round one.
	perFileFuncs int
	// why states why the current numbers hold and what growth would mean for this
	// unit specifically. It records no history.
	why string
}

var complexityCeilings = []complexityCeiling{
	{
		unit:   "engine",
		prefix: "internal/engine/",
		files:  24,
		funcs:  186,
		why: "internal/engine: run assembly. Its functions are the run's stages (workspace probe, " +
			"selection, planning, scheduling, finalization) and the seams where a run attaches " +
			"what only the engine sees: the run report, the workspace map, the spec gate, the " +
			"publication ancestry, the hosted run's credential and the portable runner's request. " +
			"The growth this pin stops is a caller's policy arriving here as a flag or a mode. " +
			"Raise it only for a stage or an attachment that no adapter can own, and name it in " +
			"the commit message.",
	},
	{
		unit:   "planner",
		prefix: "internal/jobs/planner/plan",
		files:  10,
		funcs:  93,
		why: "internal/jobs/planner/plan*.go: the single planner entrypoint (planner.Plan), its " +
			"dependency, activation, matching, sharing and resource passes, plus the plan " +
			"contract and its validation. Planning is pure, selection in and typed plan out, and " +
			"that purity lets the plan be validated, cached and replanned by watch without " +
			"re-deriving anything. Growth here is usually execution knowledge leaking backwards " +
			"into the plan, a scheduler concern computed early because it was convenient, which " +
			"is what makes a plan stop being reviewable.",
	},
	{
		unit:   "scheduler",
		prefix: "internal/jobs/scheduler/scheduler",
		files:  6,
		funcs:  100,
		why: "internal/jobs/scheduler/scheduler*.go: dispatch. Grants, batching, shared executions " +
			"(one leader and its followers), task output locks and the job process environment. " +
			"Every function here can touch a running job or a cache entry, so a raise adds a " +
			"route through the scheduler, and its commit message proves two things: OWNERSHIP, " +
			"the one state the route writes and the fact that it adds no cache route, and " +
			"ATOMICITY, the lock or ordering that keeps it safe under concurrency, so a warm run " +
			"stays bit-identical.",
	},
	{
		unit:         "invocation",
		prefix:       "internal/jobs/scheduler/invocation",
		files:        5,
		funcs:        55,
		perFileFuncs: 16,
		why: "internal/jobs/scheduler/invocation*.go: the run-time half of the invocation-scoped " +
			"lifecycle. Pruning a `finalizes` relation the planner resolved, the private scratch " +
			"and the lease that outlives a SIGKILL, the sensitive-artifact leak guard (needle " +
			"derivation, a bounded producer spool and redaction), and exactly-once finalization. " +
			"The unit arrived as one file and was split along its invariants, so the risk is the " +
			"split undoing itself: perFileFuncs caps the largest file and fails when one file " +
			"takes on a second invariant, which the totals cannot see. Each invariant is one " +
			"file, except CONFINEMENT, which is two because its halves fail differently (what " +
			"counts as a secret, and what may never publish one). A sixth file means a new " +
			"invariant and a function added to a file means a bigger one: name it in the commit " +
			"message.",
	},
	{
		unit:   "cache",
		prefix: "internal/store/",
		files:  35,
		funcs:  313,
		why: "internal/store: the content-addressed local cache. Entries and task entries " +
			"(success, failure and result-only), CAS, leases, GC with its in-run budget, " +
			"generations, the remote bridge, the build-tagged platform halves (materialization, " +
			"staging, held files) and the collection of a cache key's inputs. Cache-key " +
			"completeness and atomicity under concurrency are the two invariants this repository " +
			"has been burned by most often, and both get harder with every function that can " +
			"observe or mutate an entry. A new file here means a new way to reach the store, " +
			"which is a review, not a detail: a raise's commit message says whether it adds a way " +
			"to reach an entry.",
	},
	{
		unit:   "output",
		prefix: "internal/output/",
		files:  30,
		funcs:  276,
		why: "internal/output: the renderers. One machine renderer (machine_v2.go, with the shared " +
			"sanitizer and recorder of machine_stream.go) projects the canonical reduction for " +
			"both --output=json and --output=jsonl. Everything else is human-facing (live, text, " +
			"lifecycle, serve log, cloud logging) and composes the existing renderers. A second " +
			"machine renderer or a second output contract is the growth this pin stops, and it " +
			"would show up here first.",
	},
}

// TestComplexityCeilings_PerUnit fails when a unit grows past its pinned size.
func TestComplexityCeilings_PerUnit(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	_, files := productionASTs(t, root)

	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	concentrationChecked := 0
	for _, ceiling := range complexityCeilings {
		fileCount, funcCount := 0, 0
		var members []string
		widest, widestFuncs := "", 0
		for _, rel := range rels {
			if !strings.HasPrefix(rel, ceiling.prefix) {
				continue
			}
			fileCount++
			members = append(members, rel)
			fileFuncs := 0
			for _, decl := range files[rel].Decls {
				if _, ok := decl.(*ast.FuncDecl); ok {
					funcCount++
					fileFuncs++
				}
			}
			if fileFuncs > widestFuncs {
				widest, widestFuncs = rel, fileFuncs
			}
		}

		if fileCount == 0 {
			t.Errorf("unit %q matched no production files under %q — the unit moved; move the pin with it",
				ceiling.unit, ceiling.prefix)
			continue
		}
		if ceiling.perFileFuncs > 0 {
			concentrationChecked++
			if widestFuncs > ceiling.perFileFuncs {
				t.Errorf("unit %q: %s declares %d top-level functions, per-file ceiling %d — %d added.\n"+
					"  One file is taking on more of this unit than the split left it with.\n  %s",
					ceiling.unit, widest, widestFuncs, ceiling.perFileFuncs,
					widestFuncs-ceiling.perFileFuncs, ceiling.why)
			}
		}
		if fileCount > ceiling.files {
			t.Errorf("unit %q: %d production files, ceiling %d — %d added.\n  %s\n  Files:\n    %s",
				ceiling.unit, fileCount, ceiling.files, fileCount-ceiling.files, ceiling.why,
				strings.Join(members, "\n    "))
		}
		if funcCount > ceiling.funcs {
			t.Errorf("unit %q: %d top-level functions, ceiling %d — %d added.\n  %s",
				ceiling.unit, funcCount, ceiling.funcs, funcCount-ceiling.funcs, ceiling.why)
		}
	}

	// Non-vacuity: perFileFuncs is opt-in, so a table where every unit dropped it
	// would still pass every assertion above while measuring nothing about
	// concentration. That is the shape of ratchet this file exists not to have.
	if concentrationChecked == 0 {
		t.Error("no unit pins perFileFuncs — the concentration ratchet measures nothing; " +
			"a unit whose files ARE its invariants must keep its per-file number")
	}
}

// TestComplexityCeilings_UnitsAreDisjoint keeps the units from overlapping. The
// scheduler and invocation prefixes both select inside internal/jobs/scheduler,
// so a prefix widened to the directory would count the other unit's files twice
// and could sit under one ceiling while breaching the other — a pin that lies
// is worse than no pin.
func TestComplexityCeilings_UnitsAreDisjoint(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	_, files := productionASTs(t, root)

	owner := map[string]string{}
	for _, ceiling := range complexityCeilings {
		for rel := range files {
			if !strings.HasPrefix(rel, ceiling.prefix) {
				continue
			}
			if previous, taken := owner[rel]; taken {
				t.Errorf("%s is counted by both %q and %q — the ceilings overlap, so neither number "+
					"means what it says", rel, previous, ceiling.unit)
				continue
			}
			owner[rel] = ceiling.unit
		}
	}
	if len(owner) == 0 {
		t.Fatal("no production file matched any unit — the prefixes are wrong")
	}
}

// TestComplexityCeilings_ReportCurrentSizes is not an assertion: it records the
// measured sizes in the test log, so raising a ceiling starts from a number
// somebody read rather than from a number somebody guessed.
func TestComplexityCeilings_ReportCurrentSizes(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	_, files := productionASTs(t, root)

	for _, ceiling := range complexityCeilings {
		fileCount, funcCount, widestFuncs := 0, 0, 0
		for rel, file := range files {
			if !strings.HasPrefix(rel, ceiling.prefix) {
				continue
			}
			fileCount++
			fileFuncs := 0
			for _, decl := range file.Decls {
				if _, ok := decl.(*ast.FuncDecl); ok {
					funcCount++
					fileFuncs++
				}
			}
			if fileFuncs > widestFuncs {
				widestFuncs = fileFuncs
			}
		}
		perFile := "unpinned"
		if ceiling.perFileFuncs > 0 {
			perFile = "ceiling " + strconv.Itoa(ceiling.perFileFuncs)
		}
		t.Log(ceiling.unit + ": files=" + strconv.Itoa(fileCount) +
			" (ceiling " + strconv.Itoa(ceiling.files) + "), funcs=" + strconv.Itoa(funcCount) +
			" (ceiling " + strconv.Itoa(ceiling.funcs) + "), widest file=" +
			strconv.Itoa(widestFuncs) + " (" + perFile + ")")
	}
}
