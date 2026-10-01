package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"go.putnami.dev/cli/model/workspace"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/specgate"
	"go.putnami.dev/tooling/cli/internal/store"
)

// The engine's half of the unplanned-attester seam.
//
// internal/specgate is the policy half and stays there: it decides when to ask,
// which projects may be asked, and what an answer means. Turning a project into
// cache bytes needs the planner, the run's command params, its version lines and
// its cache manager — four things this package already holds and that specgate's
// package doc says it must not grow ("imports protocol types and the CLI's own
// result plumbing only"). So the lookup lives here and travels as an interface.
//
// Nothing is materialized. The report is read IN PLACE out of the entry's
// files/ directory: a gate that restored bytes into .putnami/out would leave a
// project that never ran looking like it did, and the next session's collector
// would read a stale artifact as a fresh one.

// cachedObservationRecovery answers specgate.ObservationRecovery from the local
// task-owned cache.
type cachedObservationRecovery struct {
	// req is held whole rather than copied field by field. A recovery lookup has
	// to key EXACTLY as the run keyed: the same command params, the same disable
	// lists, and above all the run's OWN pre-mutation git snapshot
	// (Request.VersionSnapshot) — re-reading the tree here would key against
	// whatever a codegen job left behind, and a dirty suffix the execution keys
	// never carried moves EmbeddedVersion and misses every entry.
	req        *Request
	ws         *workspace.Workspace
	extensions []*extension.ExtensionDescription
	cache      *store.CacheManager
	// disabled carries the one reason no lookup can be served at all, so every
	// candidate is reported unconsulted with an accurate cause instead of a
	// generic "no cache entry".
	disabled string

	once     sync.Once
	versions jobs.RunVersions
}

// newCachedObservationRecovery builds the recovery source for one run, or nil
// when the run cannot carry one.
//
// Under --no-cache the source is built but permanently disabled: no cached byte
// may be served, so Part A yields nothing and every unresolved check falls to
// the non-blocking unobserved classification. That is the accepted outcome —
// `--no-cache` with a narrowed selection deliberately gets no second mechanism
// — and stating it as a reason is what keeps the resulting warning honest.
func newCachedObservationRecovery(req *Request, ws *workspace.Workspace,
	extensions []*extension.ExtensionDescription, cache *store.CacheManager) *cachedObservationRecovery {
	if req == nil || ws == nil {
		return nil
	}
	source := &cachedObservationRecovery{req: req, ws: ws, extensions: extensions, cache: cache}
	switch {
	case req.Global.NoCache:
		source.disabled = "--no-cache: no cached observation may be served"
	case cache == nil:
		source.disabled = "this run has no cache store"
	}
	return source
}

// RecoverReports keys one `test` plan over the candidate set and reports where
// each candidate's captured verification report lives, or why it could not be
// consulted. Only the test tasks that can write the report are asked (see
// jobs.ProducesVerificationReport); the other steps of the command cannot
// answer for it.
func (source *cachedObservationRecovery) RecoverReports(candidates []*workspace.Project) specgate.Recovery {
	recovery := specgate.Recovery{}
	if source == nil || len(candidates) == 0 {
		return recovery
	}
	if source.disabled != "" {
		for _, candidate := range candidates {
			recovery.Unconsulted = append(recovery.Unconsulted,
				specgate.UnconsultedProject{Project: candidate.ID, Reason: source.disabled})
		}
		return recovery
	}

	// ONE plan and ONE keying pass over the whole candidate set, rather than the
	// per-candidate pair the issue sketched. The two are equivalent — a cache key
	// mixes declared inputs and UPSTREAM KEYS, never the run's selection, so a
	// task keys the same whichever projects are planned beside it (that is the
	// property every warm hit already depends on) — and the batch pays one
	// topological pass and one activation-probe cache instead of N of each, on a
	// path that only runs when the gate is already in trouble.
	var disabledJobs, disabledExtensions []string
	if source.req.Config != nil && source.req.Config.Disable != nil {
		disabledJobs, disabledExtensions = source.req.Config.Disable.Jobs, source.req.Config.Disable.Extensions
	}
	planned, err := jobs.Plan(source.ws, []string{"test"}, candidates, source.extensions,
		source.req.CommandParams, disabledJobs, disabledExtensions)
	if err != nil {
		return source.unconsultedAll(candidates, fmt.Sprintf("their test plan could not be derived: %v", err))
	}
	keys, err := jobs.PrecomputeKeys(source.ws, planned, source.req.CommandParams,
		source.runVersions(), source.cache, jobs.CacheBypass{})
	if err != nil {
		return source.unconsultedAll(candidates, fmt.Sprintf("their cache keys could not be derived: %v", err))
	}

	outcomes := make(map[string]*candidateOutcome, len(candidates))
	for _, candidate := range candidates {
		outcomes[candidate.ID] = &candidateOutcome{}
	}
	for _, job := range planned {
		// Only a task that can write the report can answer for it. The
		// test-environment steps are cache:false by contract and a generation
		// entry can be evicted on its own; counting either read a candidate whose
		// report entry was recorded EMPTY as unreachable, which excused its gap.
		if job == nil || job.Project == nil || !jobs.ProducesVerificationReport(job) {
			continue
		}
		outcome := outcomes[job.Project.ID]
		if outcome == nil {
			continue // a prerequisite of a candidate, not a candidate itself
		}
		outcome.reportTasks++
		hash := keys[job.Key()]
		if hash == "" {
			continue // the task is not cacheable, so it can never be recovered
		}
		outcome.keyed++
		entry, err := source.cache.LookupTaskEntry(hash)
		if err != nil || entry == nil {
			continue
		}
		outcome.entries++
		if report := reportInEntry(source.ws.Root, job, entry); report != nil && outcome.report == nil {
			outcome.report = report
		}
	}
	for _, candidate := range candidates {
		outcome := outcomes[candidate.ID]
		if outcome.report != nil {
			recovery.Reports = append(recovery.Reports, *outcome.report)
			continue
		}
		if reason, unconsulted := outcome.unconsultedReason(); unconsulted {
			recovery.Unconsulted = append(recovery.Unconsulted,
				specgate.UnconsultedProject{Project: candidate.ID, Reason: reason})
		}
	}
	return recovery
}

// candidateOutcome is what the lookup learned about one candidate project's
// report tasks: the test tasks that can write the verification report.
type candidateOutcome struct {
	report      *specgate.RecoveredReport
	reportTasks int
	keyed       int
	entries     int
}

// unconsultedReason decides whether a candidate that produced no report was
// CONSULTED-and-silent or never reachable at all. The distinction is the whole
// of Part B: only the second may excuse a still-missing check.
//
// Silence is an answer in two shapes, and both count as consulted:
//
//   - the project has no test task that can write the verification report, so
//     it can never attest anything;
//   - its report task's entry exists and records the verification report
//     EMPTY, which is the explicit statement "this task ran for these inputs
//     and observed nothing". Reading that as "unreachable" would excuse every
//     genuine gap in a project that simply has no bound test yet, which is
//     exactly the regression the gate exists to catch.
func (outcome *candidateOutcome) unconsultedReason() (string, bool) {
	switch {
	case outcome.reportTasks == 0:
		return "", false
	case outcome.keyed < outcome.reportTasks:
		return "not selected, and the test task that would write its verification report is not cacheable, " +
			"so no observation can be recovered", true
	case outcome.entries < outcome.keyed:
		return "not selected, and the test task that writes its verification report has no cache entry for these inputs", true
	default:
		return "", false
	}
}

func (source *cachedObservationRecovery) unconsultedAll(candidates []*workspace.Project, reason string) specgate.Recovery {
	recovery := specgate.Recovery{}
	for _, candidate := range candidates {
		recovery.Unconsulted = append(recovery.Unconsulted,
			specgate.UnconsultedProject{Project: candidate.ID, Reason: reason})
	}
	return recovery
}

// reportInEntry resolves one cached test task's entry to the captured
// verification report inside it, or nil when the entry carries none.
//
// The DESCRIPTOR decides, exactly as restoreReservedDeclaredArtifactEvents does
// on a warm hit: the output must be a PRESENT command-output file at the
// protocol's reserved filename. An output recorded empty means the task ran and
// observed nothing — a fact, not a missing file — and must never be answered
// with whatever happens to sit at that address.
func reportInEntry(wsRoot string, job *jobs.ScheduledJob, entry *store.TaskEntry) *specgate.RecoveredReport {
	if entry.FilesDir == "" {
		return nil
	}
	for _, output := range entry.Outputs {
		if !output.Present() || output.Kind != extensionproto.OutputKindFile ||
			output.Root != extensionproto.OutputRootCommandOutput ||
			output.Path != features.VerificationReportFilename {
			continue
		}
		path := filepath.Join(entry.FilesDir, output.ID)
		return &specgate.RecoveredReport{
			Project: job.Project.ID,
			Task:    job.Key(),
			Path:    path,
			Field:   relativeToWorkspace(wsRoot, path),
		}
	}
	return nil
}

// runVersions shares the versions already resolved by planning and execution.
// A recovery source assembled outside the engine resolves lazily on first use.
func (source *cachedObservationRecovery) runVersions() jobs.RunVersions {
	source.once.Do(func() { source.versions = source.req.runVersions(source.ws) })
	return source.versions
}

// relativeToWorkspace renders a cache-store path the way the record's report
// references render a session artifact. A store outside the workspace stays
// absolute rather than becoming a ".."-walk nobody can act on.
func relativeToWorkspace(wsRoot, path string) string {
	relative, err := filepath.Rel(wsRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}
