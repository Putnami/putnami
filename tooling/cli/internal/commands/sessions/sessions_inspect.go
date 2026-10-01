package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// SessionsInspect shows detailed information about a specific session.
// SessionsInspectRun is its counterpart for a remote attempt reference.
func SessionsInspect(wsRoot string, args []string, outputFormat string) error {
	if len(args) == 0 {
		return cmderr.Usagef("session ID required: sessions inspect <id>\n  Use 'sessions inspect latest' for the most recent session, or --run <ref> for a remote attempt")
	}

	store := workspace_state.NewSessionStore(wsRoot)
	sessionID := args[0]

	// Handle "latest" alias
	if sessionID == "latest" {
		sessionID = store.LatestID()
		if sessionID == "" {
			return cmderr.Classify(fmt.Errorf("no sessions found"), cmderr.ErrNoMatch)
		}
	}

	sessDir := filepath.Join(store.Root(), sessionID)
	if _, err := os.Stat(sessDir); os.IsNotExist(err) {
		return cmderr.Classify(fmt.Errorf("session not found: %s", sessionID), cmderr.ErrNotFound)
	}

	meta, err := readSessionMeta(store, sessionID)
	if err != nil {
		return fmt.Errorf("read session metadata: %w", err)
	}

	plan, _ := readSessionPlan(store, sessionID)

	events, _ := readSessionEvents(store, sessionID)

	if outputFormat == "jsonl" {
		return sessionInspectJSONL(meta, plan, events)
	}

	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  Session:  %s\n", meta.ID)
	iox.Fprintf(os.Stdout, "  Started:  %s\n", formatTimestamp(meta.StartTime))
	iox.Fprintf(os.Stdout, "  Ended:    %s\n", formatTimestamp(meta.EndTime))
	iox.Fprintf(os.Stdout, "  Duration: %s\n", formatDurationMs64(meta.Duration))
	iox.Fprintf(os.Stdout, "  Commands: %s\n", strings.Join(meta.Commands, ", "))

	if meta.Git != nil {
		if meta.Git.Branch != "" {
			iox.Fprintf(os.Stdout, "  Branch:   %s\n", meta.Git.Branch)
		}
		if meta.Git.Baseline != "" {
			iox.Fprintf(os.Stdout, "  Baseline: %s\n", meta.Git.Baseline)
		}
	}

	if meta.Stats != nil {
		iox.Fprintln(os.Stdout)
		iox.Fprintln(os.Stdout, "  Stats:")
		iox.Fprintf(os.Stdout, "    Total:     %d\n", meta.Stats.Total)
		iox.Fprintf(os.Stdout, "    Succeeded: %d\n", meta.Stats.Succeeded)
		iox.Fprintf(os.Stdout, "    Failed:    %d\n", meta.Stats.Failed)
		iox.Fprintf(os.Stdout, "    Skipped:   %d\n", meta.Stats.Skipped)
		iox.Fprintf(os.Stdout, "    Cached:    %d\n", meta.Stats.Cached)
		iox.Fprintf(os.Stdout, "    Coalesced: %d\n", meta.Stats.Coalesced)
	}

	printSessionJobs(meta.Jobs)

	if plan != nil && len(plan.Jobs) > 0 {
		iox.Fprintln(os.Stdout)
		iox.Fprintf(os.Stdout, "  Plan (%d jobs):\n", len(plan.Jobs))
		for _, job := range plan.Jobs {
			cache := ""
			if job.Cache {
				cache = " [cache]"
			}
			deps := ""
			if len(job.DependsOn) > 0 {
				deps = fmt.Sprintf(" → %s", strings.Join(job.DependsOn, ", "))
			}
			after := ""
			if len(job.After) > 0 {
				after = fmt.Sprintf(" [after: %s]", strings.Join(job.After, ", "))
			}
			iox.Fprintf(os.Stdout, "    %-35s %s%s%s%s\n", job.Key, job.Extension, cache, deps, after)
		}
	}

	var failedJobs []sessionJobSummary
	for _, ev := range events {
		terminal, ok := taskEndFromSessionEvent(ev)
		if ok && terminal.status == "failed" {
			failedJobs = append(failedJobs, sessionJobSummary{
				project: terminal.project,
				job:     terminal.job,
				errMsg:  terminal.error,
			})
		}
	}

	if len(failedJobs) > 0 {
		iox.Fprintln(os.Stdout)
		iox.Fprintln(os.Stdout, "  Failed jobs:")
		for _, fj := range failedJobs {
			iox.Fprintf(os.Stdout, "    ✗ %s %s\n", fj.project, fj.job)
			if fj.errMsg != "" {
				for _, line := range strings.Split(fj.errMsg, "\n") {
					if line != "" {
						iox.Fprintf(os.Stdout, "      %s\n", line)
					}
				}
			}
		}
	}

	var diagnostics []sessionDiagnostic
	for _, ev := range events {
		if diagnostic, ok := diagnosticFromSessionEvent(ev); ok {
			diagnostics = append(diagnostics, diagnostic)
		}
	}

	if len(diagnostics) > 0 {
		iox.Fprintln(os.Stdout)
		iox.Fprintf(os.Stdout, "  Diagnostics (%d):\n", len(diagnostics))
		for _, d := range diagnostics {
			loc := ""
			if d.file != "" {
				loc = d.file
				if d.line > 0 {
					loc = fmt.Sprintf("%s:%d", d.file, d.line)
				}
				loc = " " + loc
			}
			iox.Fprintf(os.Stdout, "    [%s]%s: %s\n", d.severity, loc, d.message)
		}
	}

	printSubscriberEvidence(store, sessionID)

	iox.Fprintln(os.Stdout)
	return nil
}

func printSessionJobs(jobs []workspace_state.SessionJobEntry) {
	if len(jobs) == 0 {
		return
	}
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  Jobs (%d):\n", len(jobs))
	for _, job := range jobs {
		overhead := "n/a"
		if job.SpawnToFirstEventMs != nil {
			overhead = fmt.Sprintf("%dms", *job.SpawnToFirstEventMs)
		}
		iox.Fprintf(
			os.Stdout,
			"    %-35s %-24s wall=%dms spawn→first-event=%s\n",
			job.Key,
			job.TaskKind,
			job.TaskWallMs,
			overhead,
		)
	}
}
