package output

import (
	"sort"
	"strings"
	"time"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// Finish is called when all jobs are complete.
func (r *TextRenderer) Finish(results map[string]*jobs.JobResult, outcome jobs.SessionOutcome) {
	if r.cfg.Lifecycle {
		return
	}
	totalDuration := time.Since(r.start)

	session := jobs.ReduceRun(r.planned, results, outcome)
	// Failed reads the STATUS histogram: a reused result that failed — a cache
	// hit whose declared output drifted from this checkout — is a failure of
	// this run, and the headline must say so (SessionResult.Success reads the
	// same histogram). Such a result is also counted in `cached`, where its
	// bytes came from; the headline names the verdict, the bucket the
	// provenance, and the row carries both.
	succeeded, failed := session.Fresh.Succeeded, session.Status.Failed
	canceled, skipped := session.Fresh.Canceled, session.Fresh.Skipped
	cached, coalesced := session.Cached(), session.Reuse.Coalesced
	total := session.BucketTotal()

	// Show recap table when there are failures and multiple jobs
	if failed > 0 && r.multiJob && !r.cfg.Quiet {
		r.renderFailureRecap(results)
	}

	// Show job summaries from failed jobs (e.g. "44 errors in 9 files")
	if failed > 0 && !r.cfg.Quiet {
		r.renderJobSummaries(results)
		r.renderFailureFollowupHint()
	}

	// Some successful jobs deliberately expose complete dry-run payloads.
	if !r.cfg.Quiet && !r.cfg.Verbose && !r.cfg.Debug {
		renderVisibleSummaries(r.errOut, results)
	}

	// List any artifacts published during this session and their versions.
	if !r.cfg.Quiet {
		r.renderPublished(results)
	}

	iox.Fprintf(r.errOut, "\n")

	if outcome.Aborted {
		// Lead with the abort. The counts that follow describe only the part of
		// the plan that ran, and without this line they read as the whole run.
		iox.Fprintf(r.errOut, "  %s\n", colorize("Session "+abortDescription(outcome), Yellow))
	}

	if failed > 0 {
		iox.Fprintf(r.errOut, "  %d/%d failed", failed, total)
	} else {
		iox.Fprintf(r.errOut, "  %d succeeded", succeeded)
	}
	if canceled > 0 {
		iox.Fprintf(r.errOut, "  %d canceled", canceled)
	}
	if cached > 0 {
		iox.Fprintf(r.errOut, "  %d cached", cached)
	}
	if coalesced > 0 {
		iox.Fprintf(r.errOut, "  %d coalesced", coalesced)
	}
	if skipped > 0 {
		iox.Fprintf(r.errOut, "  %d skipped", skipped)
	}
	iox.Fprintf(r.errOut, "  %s\n\n", formatDuration(totalDuration))
}

// renderFailureRecap shows a summary of which jobs failed, for quick reference
// at the end of a long multi-job run.
func (r *TextRenderer) renderFailureRecap(results map[string]*jobs.JobResult) {
	iox.Fprintf(r.errOut, "\n  Failed:\n")
	// Sort keys for deterministic output
	keys := make([]string, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		result := results[key]
		if result.Status != "failed" {
			continue
		}
		errMsg := ""
		if result.Error != nil && result.Error.Message != "" {
			// Take the first actionable error line only for recap.
			lines, _ := splitErrorDetailLines(result.Error.Message)
			if len(lines) > 0 {
				errMsg = " — " + strings.TrimSpace(lines[0])
			}
		}
		iox.Fprintf(r.errOut, "    %s%s\n", key, errMsg)
	}
}

// renderPublished summarizes the artifacts published during the session and
// keeps the shared publish version visible at the end of the run.
func (r *TextRenderer) renderPublished(results map[string]*jobs.JobResult) {
	published := collectPublished(results)
	if len(published) == 0 {
		return
	}

	iox.Fprintf(r.errOut, "\n  Published:\n")
	for _, line := range formatPublishedSummaryLines(summarizePublished(published)) {
		iox.Fprintf(r.errOut, "    %s\n", line)
	}
}

// renderJobSummaries prints summary labels from failed jobs before the session recap.
func (r *TextRenderer) renderJobSummaries(results map[string]*jobs.JobResult) {
	for _, result := range results {
		if result.Status != "failed" {
			continue
		}
		for _, ev := range result.Events {
			if ev.Type != jobs.EventTypeSummary {
				continue
			}
			if msg, ok := ev.Data["message"].(string); ok && msg != "" {
				iox.Fprintf(r.errOut, "\n  %s", msg)
			}
		}
	}
}

func (r *TextRenderer) renderFailureFollowupHint() {
	iox.Fprintf(r.errOut, "\n  Hint: re-run with --output=jsonl for structured diagnostics and a complete session artifact.\n")
}
