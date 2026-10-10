package engine

import (
	"fmt"
	"io"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/profiler"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// The two v1 session-file projections that used to live here — SessionStats and
// sessionJobEntry — were deleted with the v1 session writer. The
// recorded session is the versioned cli.SessionFile now, projected by
// internal/machine from the same canonical reduction, so nothing derives a
// second stats vocabulary on the way to disk.

// recordProfilerSessionEvent projects scheduler session records into the trace.
// Coalesced work keeps its successful DAG status, but the trace end event and
// instant marker expose the distinct outcome for direct counting.
//
// It reads the record's TYPED task. It used to re-read "status",
// "outcome", "duration" and "cache" back out of the untyped payload and
// re-derived the reuse classification from them — including a fallback for a
// producer that had not filled "outcome" in. jobs.ReuseKind makes both the
// re-derivation and that fallback structurally impossible.
func recordProfilerSessionEvent(prof *profiler.Profiler, record jobs.SessionRecord) {
	if record.Type != jobs.SessionRecordJobEnd || record.Task == nil {
		return
	}
	task := record.Task
	traceStatus := string(task.Status)
	if task.Reuse == jobs.ReuseCoalesced {
		traceStatus = jobs.JobOutcomeCoalesced
	}
	prof.JobEnd(record.JobKey, traceStatus, task.Timing.Duration)
	switch {
	case task.Reuse == jobs.ReuseCoalesced:
		prof.Instant(record.JobKey, "cache-coalesced", "cache", nil)
	case task.Reuse.CacheHit():
		prof.Instant(record.JobKey, "cache-hit", "cache", nil)
	}
}

// reportSchedulerTuning prints a human-readable summary of the scheduler's
// auto-tuning decision and DAG wait metrics. The compact parallelism line shows
// in verbose/debug (and whenever profiling is on); the critical path and ready
// wait detail show in debug or with --trace-profile, so profiling needs no
// external trace analysis. Quiet suppresses everything.
func reportSchedulerTuning(w io.Writer, g *GlobalFlags, tuning *jobs.TuningReport) {
	if tuning == nil || g.Quiet {
		return
	}
	showSummary := g.Verbose || g.Debug || g.TraceProfile != ""
	if !showSummary {
		return
	}

	p := tuning.Parallel
	iox.Fprintf(w, "  Scheduler: %s · %d workers · %d logical CPU", p.Mode, p.Workers, p.LogicalCPU)
	if p.CPUCapacity > 0 && p.CPUCapacity != p.LogicalCPU {
		iox.Fprintf(w, " · %d allocatable CPU", p.CPUCapacity)
	}
	if p.MemoryTotalMiB > 0 {
		iox.Fprintf(w, " · %d MiB", p.MemoryTotalMiB)
		if p.MemoryUsableMiB > 0 && p.MemoryUsableMiB != p.MemoryTotalMiB {
			iox.Fprintf(w, " (%d usable)", p.MemoryUsableMiB)
		}
	}
	// Surface the memory cap only when it actually bound the worker count, so the
	// human line is not misread when memory had headroom to spare.
	if p.MemoryCapWorkers > 0 && p.Workers == p.MemoryCapWorkers {
		iox.Fprintf(w, " · memory cap %d", p.MemoryCapWorkers)
	}
	iox.Fprintf(w, " · heavy %.0f%%\n", tuning.HeavyJobRatio*100)

	if !(g.Debug || g.TraceProfile != "") {
		return
	}
	if cp := tuning.CriticalPath; cp != nil && len(cp.Chain) > 0 {
		iox.Fprintf(w, "  Critical path (%s):\n", formatDurationMs(cp.DurationMs))
		for _, n := range cp.Chain {
			iox.Fprintf(w, "    %-40s %s\n", n.Job, formatDurationMs(n.DurationMs))
		}
	}
	if len(tuning.ReadyWait) > 0 {
		iox.Fprintf(w, "  Ready wait by command:\n")
		for _, rw := range tuning.ReadyWait {
			iox.Fprintf(w, "    %-12s total %s · max %s · %d jobs\n",
				rw.Command, formatDurationMs(rw.TotalMs), formatDurationMs(rw.MaxMs), rw.Jobs)
		}
	}
	if len(tuning.CPUBudgets) > 0 {
		const maxBudgetLines = 12
		iox.Fprintf(w, "  CPU budgets (%d executed jobs):\n", len(tuning.CPUBudgets))
		for i, b := range tuning.CPUBudgets {
			if i == maxBudgetLines {
				iox.Fprintf(w, "    … %d more\n", len(tuning.CPUBudgets)-maxBudgetLines)
				break
			}
			if b.Weight != 1 {
				iox.Fprintf(w, "    %-40s %d cpus · weight %.1f · expected cpu %s\n",
					b.Job, b.Budget, b.Weight, formatDurationMs(int64(b.ExpectedCPUMs)))
			} else {
				iox.Fprintf(w, "    %-40s %d cpus · expected cpu %s\n",
					b.Job, b.Budget, formatDurationMs(int64(b.ExpectedCPUMs)))
			}
		}
	}
}

// reportCacheSummary prints the local cache-serving leg first, including the
// in-process and child-process work that otherwise sits between near-zero task
// spans. When a remote provider participated, its economics follow on a
// separate line. Quiet suppresses everything.
func reportCacheSummary(w io.Writer, g *GlobalFlags, c *jobs.CacheStatsSnapshot) {
	if c == nil || g.Quiet || !c.HasActivity() {
		return
	}

	if c.LocalHits > 0 || c.LocalMisses > 0 || c.LocalServedMs > 0 {
		iox.Fprintf(w, "  Cache: %d local hit · %d miss · served in %s", c.LocalHits, c.LocalMisses,
			formatDurationMs(c.LocalServedMs))
		iox.Fprintf(w, " (keys %s · bindings %s · restore-verify %s · spawned %d processes)\n",
			formatDurationMs(c.LocalKeysMs), formatDurationMs(c.LocalBindingsMs),
			formatDurationMs(c.LocalRestoreVerifyMs), c.LocalSpawnedProcesses)
	}

	remoteActivity := c.KeysRequested > 0 || c.Uploads > 0 || c.Restored > 0 || c.HintsWarmed > 0
	if !remoteActivity {
		return
	}

	iox.Fprintf(w, "  Remote cache: %d hit · %d miss · saved %s · spent %s",
		c.Hits, c.Misses, formatDurationMs(c.TimeSavedMs), formatDurationMs(c.OverheadMs()))
	// Break the spent time down so a slow phase (token resolution, a fat upload,
	// a restore drowning in tiny-file round trips) is attributable instead of
	// lumped into one number.
	iox.Fprintf(w, " (setup %s · negotiate %s · restore %s · upload %s)\n",
		formatDurationMs(c.SetupMs), formatDurationMs(c.NegotiateMs), formatDurationMs(c.RestoreMs), formatDurationMs(c.UploadMs))
	if c.UploadErrors > 0 {
		iox.Fprintf(w, "         %d upload error(s) — entries not shared (build unaffected)\n", c.UploadErrors)
	}
	if c.HintsWarmed > 0 {
		iox.Fprintf(w, "         %d hint(s) warmed — jobs re-executed by cache trust policy\n", c.HintsWarmed)
	}

	if !(g.Verbose || g.Debug) {
		return
	}
	iox.Fprintf(w, "         %s fetched · %s uploaded · %s deduped\n",
		formatBytes(c.BytesFetched), formatBytes(c.BytesUploaded), formatBytes(c.BytesDeduped))
}

// formatBytes renders a byte count in compact human units (e.g. "8.4 MB"),
// using powers of 1024 and dropping the decimal for whole or sub-KiB values.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatDurationMs renders a millisecond count in the compact "4m12s" form used
// by the run summary, dropping to "Nms" below a second.
func formatDurationMs(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	totalSeconds := int(ms / 1000)
	h := totalSeconds / 3600
	m := (totalSeconds % 3600) / 60
	s := totalSeconds % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// writeRunReport records the run's contracted bilan beside the
// session it synthesizes.
//
// It runs LAST, after the session has been finalized, because a report is
// written from a settled run: the interval it states is the session's own, so
// the two documents report the same wall. It is best-effort at every step for
// the reason the contract states — the report is a projection, never an input,
// so a report that could not be written is a missing file and never a failed
// run.
//
// The session's ephemerality decides the ORIGIN rather than whether to write at
// all. `latest` is a measurement pointer an adapter must stay out of, but a
// report carries the origin field session metadata never had, so a reader
// filters MCP runs out instead of losing them.
func writeRunReport(
	wsRoot string,
	session *workspace_state.Session,
	run machine.Run,
	req *Request,
	tuning *jobs.TuningReport,
	cache *jobs.CacheStatsSnapshot,
) error {
	if session == nil || req == nil {
		return nil
	}
	report := run.Report(machine.ReportMeta{
		SessionID:       session.ID,
		StartTime:       session.StartTime,
		EndTime:         session.EndTime,
		Origin:          reportOrigin(req),
		EnforceCoverage: reportEnforceCoverage(req.CommandParams["enforce-coverage"]),
		Fix:             reportFix(req.CommandParams["fix"]),
		Git:             reportGit(wsRoot, req),
	}, tuning, cache)
	if report == nil {
		return nil
	}
	store := workspace_state.NewReportStore(wsRoot)
	err := store.Write(report)
	// Retention rides the write: the one moment the store certainly
	// grew is the one moment it is re-bounded, and a prune that could not run
	// is files kept, never a failed run.
	store.Prune()
	return err
}

// reportOrigin names the surface that drove the run. EphemeralSession is set by
// exactly one caller — the MCP adapter (Request.EphemeralSession) — so it is the
// origin signal the engine already carries.
func reportOrigin(req *Request) string {
	if req.EphemeralSession {
		return protocolcli.ReportOriginMCP
	}
	return protocolcli.ReportOriginCLI
}

// reportEnforceCoverage reads the enforcing cadence off the run's
// `enforce-coverage` command param value (buildCommandParams produces the bool
// for the bare flag and the string for `--enforce-coverage=true`). The marker is
// what lets a longitudinal consumer compare like with like instead of reading a
// fast run's absent coverage as a regression.
func reportEnforceCoverage(param any) bool {
	switch value := param.(type) {
	case bool:
		return value
	case string:
		return value == "true" || value == "1"
	default:
		return false
	}
}

// reportFix reads the run's explicit `fix` command param value: the bool
// buildCommandParams produces for `--fix` and `--no-fix`, or the string "true"
// or "false" it produces for `--fix=<value>` and `--fix <value>`. It is nil for
// any other value and for a run without the flag.
func reportFix(param any) *bool {
	switch value := param.(type) {
	case bool:
		return &value
	case string:
		if value == "true" || value == "false" {
			fix := value == "true"
			return &fix
		}
	}
	return nil
}

// reportGit is the repository state the run was produced against.
//
// The sha is REQUIRED by the contract and required to be a full object id: a
// report is a durable fact filed against a commit, and two reports filed under
// a symbolic ref describe different ones. A read that fails means this is not a
// worktree, and the whole block is then absent rather than partially guessed.
//
// Dirtiness and branch are read off the version snapshot the run captured
// BEFORE any in-run mutation (Request.VersionSnapshot), which costs nothing
// here and is the state the run actually observed. Without a snapshot the
// member stays absent: a report that cannot say whether the tree matched the
// sha says nothing rather than claiming a clean tree.
//
// The full object id has to be read AFTER the run (the snapshot keeps only the
// abbreviation), so it is checked against the snapshot's: a job or hook that
// commits, amends or checks out during the run moves HEAD, and filing the run
// under the RESULTING commit would bind measurements to code they never ran
// against. A moved HEAD therefore drops the whole block — absent, never wrong.
func reportGit(wsRoot string, req *Request) *protocolcli.ReportGit {
	if wsRoot == "" {
		return nil
	}
	sha, err := git.HeadSHA(wsRoot)
	if err != nil || sha == "" {
		return nil
	}
	state := &protocolcli.ReportGit{Sha: sha, Baseline: req.Global.Baseline}
	if snapshot := req.VersionSnapshot; snapshot != nil {
		if snapshot.SHA != "" && !strings.HasPrefix(sha, snapshot.SHA) {
			return nil
		}
		state.Branch = attachedBranch(snapshot.Branch)
		dirty := snapshot.IsDirty
		state.Dirty = &dirty
	} else if branch, branchErr := git.CurrentBranch(wsRoot); branchErr == nil {
		state.Branch = attachedBranch(branch)
	}
	return state
}

// attachedBranch drops the "HEAD" a detached checkout reports: the contract's
// branch is absent on a detached HEAD, and the literal ref name is not a branch.
func attachedBranch(branch string) string {
	if branch == "HEAD" {
		return ""
	}
	return branch
}
