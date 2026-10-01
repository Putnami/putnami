package cli

import (
	"sort"
	"strings"
	"testing"
)

// The model-decoupling ratchet.
//
// A2–A4 lifted the pure workspace, job and extension data model out of
// tooling/cli into go.putnami.dev/cli/model, leaving an alias layer
// (model_alias.go) behind in each internal origin package. A5 flipped the
// consumers that need only the model onto it. This test keeps them there.
//
// # Why a ratchet, and why on IMPORTS
//
// The flip is invisible at a call site. R5 of the refactor named the model
// packages `workspace`, `extension` and `jobs`, so `jobs.TaskResult` and
// `workspace.Project` read identically whichever module supplies them — which is
// what made A5 an import-line swap with no body changes, and is exactly why a
// later branch can re-import the internal origin without anything looking wrong
// at review. The alias layers make the regression compile and keep every
// behavioral test green, so nothing but the import line ever shows that a
// renderer has just re-coupled itself to the scheduler.
//
// The predicate is therefore the production import itself, not a symbol set: the
// packages below consume the pure model and nothing else from the
// workspace/job/extension layer, so "does this package import the origin at all"
// is the whole question. That is the opposite choice from
// engine_boundary_test.go, which must allow the import and restrict a SYMBOL
// SET, because internal/engine legitimately needs both halves.
//
// Test files are deliberately out of scope. A test may reach for the loading or
// scheduling half to build a fixture, and pinning that would make fixtures
// harder to write without protecting the dependency graph that ships.
//
// # Mechanism
//
// It reads cliModules and moduleProductionASTs — the same module list and
// production-AST walk as the structural pins (A0/A4) and the v1 ratchets (B7a) —
// so when a later slice moves one of these packages the scan follows it across
// the module boundary instead of quietly scanning nothing. Imports are read from
// the AST, so a doc comment or a test fixture naming an origin path does not
// count; this file names all three literally, one line below.

// internalWorkspaceImportPath and internalExtensionImportPath are the two other
// origin packages the model was lifted out of. The third, internal/jobs, is
// already spelled by engine_boundary_test.go as jobsImportPath and is reused
// here rather than written twice.
const (
	internalWorkspaceImportPath = "go.putnami.dev/tooling/cli/internal/workspace"
	internalExtensionImportPath = "go.putnami.dev/tooling/cli/internal/extension"
)

// modelOrigin is one internal package the pure model was lifted out of.
type modelOrigin struct {
	// path is the origin package's import path.
	path string
	// name is the identifier the path binds without an alias, which is what an
	// import scan needs to recognize it.
	name string
	// keeps names the half that stayed behind, so a failure can say what the
	// consumer just re-coupled itself to.
	keeps string
}

var modelOrigins = []modelOrigin{
	{jobsImportPath, "jobs", "the planner, scheduler, executor, cache and job-context writer"},
	{internalWorkspaceImportPath, "workspace", "workspace discovery, loading, probing, syncing and snapshotting"},
	{internalExtensionImportPath, "extension", "extension discovery, install, integrity, registry and lockfile I/O"},
}

// modelOnlyPackages are the consumers A5 flipped onto
// go.putnami.dev/cli/model/*, as directory prefixes in the cliModules key space.
//
// All five are projections: they render, serialize or summarize a run and a
// workspace, and none of them loads, probes, installs or executes anything. That
// is the property the ratchet holds — not "these five files as they are today".
var modelOnlyPackages = []string{
	"internal/changeplan/",
	"internal/machine/",
	"internal/output/",
	"internal/watch/",
	"internal/workspace_state/",
}

// modelDecouplingException is one production file A5 could NOT flip, with the
// symbol that blocked it.
//
// An exception is a written justification, never a silent hole: it names the
// exact symbol, so the reviewer's question is "is that symbol really part of the
// execution half?" rather than "why is this package still on the old import?".
// The list is checked in both directions below — an exception that stopped
// applying fails, so it is deleted by review rather than left as a standing
// permission.
type modelDecouplingException struct {
	// file is the production file, in the cliModules key space.
	file string
	// origin is the internal import path it still needs.
	origin string
	// symbol is the one that blocks the flip, qualified as written.
	symbol string
	// why explains what makes that symbol part of the half that stayed.
	why string
}

var modelDecouplingExceptions = []modelDecouplingException{
	{
		file:   "internal/changeplan/changeplan.go",
		origin: jobsImportPath,
		symbol: "jobs.DefaultTimeoutMs",
		why: "the subprocess timeout default (internal/jobs/types.go), declared beside the runner " +
			"and the batch executor that apply it. A change plan states the hard deadline each task " +
			"WOULD run under, so it reads the executor's default instead of restating it — a second " +
			"spelling is how the planned deadline and the executed one start to disagree.",
	},
	{
		file:   "internal/machine/report.go",
		origin: jobsImportPath,
		symbol: "jobs.TuningReport",
		why: "the scheduler's auto-tuning summary (internal/jobs/scheduler_metrics.go), assembled " +
			"from the parallelism decision, the CPU allocator's grants and the measured ready-waits — " +
			"facts that exist only inside a scheduler that ran. The v2 report document forwards two of " +
			"them (parallelism, critical path), so this wire depends on the execution half by " +
			"construction, not by accident.",
	},
	{
		file:   "internal/workspace_state/session_report.go",
		origin: jobsImportPath,
		symbol: "jobs.JobContextVersion",
		why: "the version block of the job CONTEXT FILE the executor writes and every task reads " +
			"(internal/jobs/context.go). The session report records the version stamp a run actually " +
			"shipped with, which is that writer's shape rather than the canonical result model's.",
	},
}

// TestStructuralBaseline_ModelConsumersStayDecoupled fails if a projection
// package re-imports one of the internal origin packages in production code,
// unless the file is on the exception list above.
func TestStructuralBaseline_ModelConsumersStayDecoupled(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	_, files := moduleProductionASTs(t, root)

	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	justified := map[string]bool{}
	for _, exception := range modelDecouplingExceptions {
		justified[exception.file+" "+exception.origin] = true
	}

	// scanned counts production files per pinned package, detected counts every
	// production importer of an origin across the whole CLI, and applied records
	// which exceptions the scan actually needed. All three back a non-vacuity
	// assertion after the loop.
	scanned := map[string]int{}
	detected := map[string]int{}
	applied := map[string]bool{}

	for _, rel := range rels {
		file := files[rel]
		owner, pinned := modelOnlyPackageOf(rel)
		if pinned {
			scanned[owner]++
		}
		for _, origin := range modelOrigins {
			if _, imported := importedAs(file, origin.path, origin.name); !imported {
				continue
			}
			detected[origin.path]++
			if !pinned {
				continue
			}
			key := rel + " " + origin.path
			if justified[key] {
				applied[key] = true
				continue
			}
			t.Errorf("%s imports %s — %s consumes the pure model "+
				"(go.putnami.dev/cli/model/%s) and must not reach back into the origin package, "+
				"which still owns %s.\n"+
				"  Both packages spell their symbols identically, so this import line is "+
				"the ONLY place the coupling is visible: the origin's alias layer keeps the code "+
				"compiling and every behavioral test green.\n"+
				"  If the symbol you need is genuinely part of the execution/loading half, add a "+
				"modelDecouplingException naming it and why. If it is pure, move it to the model "+
				"package instead — that is the direction this ratchet exists to keep.",
				rel, origin.path, owner, origin.name, origin.keeps)
		}
	}

	for _, pkg := range modelOnlyPackages {
		if scanned[pkg] == 0 {
			t.Errorf("no production files under %s — the package moved, was renamed, or now lives in "+
				"another module. Re-key modelOnlyPackages so the scan keeps reading it; leaving the "+
				"entry in place would report an invariant nothing checks.", pkg)
		}
	}

	for _, exception := range modelDecouplingExceptions {
		if !applied[exception.file+" "+exception.origin] {
			t.Errorf("%s no longer imports %s, so the exception for %s is stale — delete it.\n"+
				"  An exception nobody needs is a standing permission the next re-coupling falls "+
				"through, which is the whole reason the list is checked in both directions.",
				exception.file, exception.origin, exception.symbol)
		}
	}

	// Non-vacuity: the scan must be able to SEE each origin somewhere in the
	// CLI's production code, or the loop above matched nothing and would pass
	// however the coupling came back.
	for _, origin := range modelOrigins {
		if detected[origin.path] == 0 {
			t.Errorf("no production file in any module of cliModules imports %s — either the origin "+
				"package is gone (delete its modelOrigins entry with it) or the path is wrong, in "+
				"which case this whole test passes vacuously.", origin.path)
		}
	}
}

// modelOnlyPackageOf returns the modelOnlyPackages entry rel belongs to. The
// prefixes carry their trailing slash, so internal/workspace_state/ is not
// matched by a hypothetical internal/workspace/ entry.
func modelOnlyPackageOf(rel string) (string, bool) {
	for _, pkg := range modelOnlyPackages {
		if strings.HasPrefix(rel, pkg) {
			return pkg, true
		}
	}
	return "", false
}
