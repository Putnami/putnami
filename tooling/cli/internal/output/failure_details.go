package output

import (
	"fmt"
	"strings"
	"time"

	"go.putnami.dev/cli/model/jobs"
)

// replayedFailureHint is the one line that tells a user why a failure came back
// instantly and how to force the work anyway. It is printed exactly once per
// run, beside the failure list.
const replayedFailureHint = "Nothing changed since the last failure. " +
	"Fix the cause, or pass --retry-failed to run it again."

// replayedFailureNote annotates a failure that was replayed from the local
// failure cache rather than executed: it names the fact that the inputs are
// unchanged, when the task first failed, how long ago that was, and how many
// times this failure has been observed. It returns "" for an ordinary failure,
// so every caller can append it unconditionally.
func replayedFailureNote(result *jobs.JobResult) string {
	if result == nil || result.ReplayedFailure == nil {
		return ""
	}
	replay := result.ReplayedFailure
	if replay.FirstFailedAt.IsZero() {
		return fmt.Sprintf("replayed — inputs unchanged, attempt %d", replay.Attempts)
	}
	return fmt.Sprintf("replayed — inputs unchanged since %s (%s ago), attempt %d",
		replay.FirstFailedAt.Local().Format("15:04"),
		formatFailureAge(time.Since(replay.FirstFailedAt)),
		replay.Attempts)
}

// replayedFailureSuffix is replayedFailureNote ready to append to a status
// line: " (replayed — ...)" or "" for an ordinary failure.
func replayedFailureSuffix(result *jobs.JobResult) string {
	note := replayedFailureNote(result)
	if note == "" {
		return ""
	}
	return "  (" + note + ")"
}

// reusedFailureSuffix names the provenance of a failure that was not executed
// in this run: an entry — a cache hit, or one a sibling process published while
// this run waited on the lease — whose declared output drifted from this
// checkout (jobs/task_drift.go). Without it the row reads like a task that ran
// and failed, and the reader looks for a subprocess that never existed.
func reusedFailureSuffix(result *jobs.JobResult) string {
	if result == nil {
		return ""
	}
	switch result.ReuseKind() {
	case jobs.ReuseLocalCache, jobs.ReuseRemoteCache:
		return "  (restored from cache)"
	case jobs.ReuseCoalesced:
		return "  (restored, coalesced)"
	default:
		return ""
	}
}

// anyReplayedFailure reports whether any failed result in the run was replayed,
// which is what decides whether the --retry-failed hint is worth printing.
func anyReplayedFailure(results map[string]*jobs.JobResult) bool {
	for _, result := range results {
		if result != nil && result.ReplayedFailure != nil {
			return true
		}
	}
	return false
}

// formatFailureAge renders how old a first failure is in one coarse unit. The
// number answers "is this the failure I saw a minute ago, or one from
// yesterday?", so a single unit is the whole requirement.
func formatFailureAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

func failureLogLines(events []jobs.RawJobEvent) []string {
	var lines []string
	seen := make(map[string]bool)

	add := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			return
		}
		seen[line] = true
		lines = append(lines, line)
	}

	for _, ev := range events {
		if ev.Type != jobs.EventTypeLog {
			continue
		}
		level := strings.ToLower(eventString(ev, "level"))
		if level != "error" && level != "warn" && level != "warning" {
			continue
		}

		msg := eventString(ev, "message")
		if msg != "" {
			add(fmt.Sprintf("%s: %s", displayLogLevel(level), msg))
		}

		errData, _ := ev.Data["error"].(map[string]any)
		if errData == nil {
			continue
		}
		if errMsg, _ := errData["message"].(string); errMsg != "" && errMsg != msg {
			add(errMsg)
		}
		if stack, _ := errData["stack"].(string); stack != "" {
			for _, line := range strings.Split(strings.TrimRight(stack, "\n"), "\n") {
				add(line)
			}
		}
	}

	return lines
}

func splitErrorDetailLines(message string) (details []string, generic []string) {
	for _, line := range strings.Split(strings.TrimRight(message, "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isJSONLHintLine(trimmed) {
			continue
		}
		if isGenericExitStatusLine(trimmed) {
			generic = append(generic, trimmed)
			continue
		}
		details = append(details, line)
	}
	return details, generic
}

func genericExitSummary(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf("(no diagnostics emitted; subprocess exited with %s)", strings.TrimSpace(lines[0]))
}

func isJSONLHintLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	return strings.Contains(lower, "--output=jsonl") && strings.Contains(lower, "diagnostic")
}

func isGenericExitStatusLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if !strings.HasPrefix(lower, "exit status ") {
		return false
	}
	code := strings.TrimSpace(strings.TrimPrefix(lower, "exit status "))
	if code == "" {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func eventString(ev jobs.RawJobEvent, key string) string {
	if ev.Data != nil {
		if s, _ := ev.Data[key].(string); s != "" {
			return s
		}
	}
	switch key {
	case "level":
		return ev.Level
	case "message":
		return ev.Message
	default:
		return ""
	}
}

func displayLogLevel(level string) string {
	if level == "warning" {
		return "warn"
	}
	return level
}
