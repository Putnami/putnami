package turbo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.putnami.dev/tooling/stacks"
)

// RunsDir is where turbo run --summarize writes the run summary of each
// command, one file per run, under the workspace root.
const RunsDir = ".turbo/runs"

// logLines is how many lines of a failed task's log its message quotes.
const logLines = 40

// runSummary is what the reader reads of a run summary: the command it
// summarizes, and each task turbo ran, with its execution.
type runSummary struct {
	Execution struct {
		Command string `json:"command"`
	} `json:"execution"`
	Tasks []struct {
		TaskID    string `json:"taskId"`
		LogFile   string `json:"logFile"`
		Directory string `json:"directory"`
		Execution *struct {
			ExitCode *int   `json:"exitCode"`
			Error    string `json:"error"`
		} `json:"execution"`
	} `json:"tasks"`
}

// ReadBatch reads the run summary a turbo run --summarize over several
// packages wrote (ADR 0019 of tooling/stacks), and removes it: a task
// <package>#<task> of the command whose execution exited 0 passes; one that
// exited otherwise, or carries an error, fails with turbo's error and the end
// of its log. A task turbo stopped or never started is not listed, and the
// core fails its package as not run.
func ReadBatch(report stacks.BatchReport) map[string]stacks.BatchOutcome {
	doc, ok := takeSummary(report.Root, report.Started, report.Command)
	if !ok || !report.Failed {
		return nil
	}
	tasks := commandTasks(report.Command)
	if len(tasks) == 0 {
		return nil
	}
	type result struct {
		ran, failed bool
		message     string
	}
	byID := map[string]result{}
	for _, t := range doc.Tasks {
		e := t.Execution
		switch {
		case e == nil:
		case e.Error != "" || (e.ExitCode != nil && *e.ExitCode != 0):
			byID[t.TaskID] = result{ran: true, failed: true, message: failure(report.Root, t.TaskID, e.Error, t.LogFile, t.Directory)}
		case e.ExitCode != nil:
			byID[t.TaskID] = result{ran: true}
		}
	}
	out := map[string]stacks.BatchOutcome{}
	for _, component := range report.Components {
		ran := true
		var messages []string
		for _, task := range tasks {
			r := byID[component+"#"+task]
			ran = ran && r.ran
			if r.failed {
				messages = append(messages, r.message)
			}
		}
		switch {
		case len(messages) > 0:
			out[component] = stacks.BatchOutcome{Failed: true, Message: strings.Join(messages, "\n")}
		case ran:
			out[component] = stacks.BatchOutcome{}
		}
	}
	return out
}

// takeSummary reads the newest run summary of the command written since it
// started, and removes it. A summary of another run, as a run of turbo beside
// Putnami's or an earlier one, is neither read nor removed.
func takeSummary(root string, started time.Time, command []string) (runSummary, bool) {
	dir := filepath.Join(root, filepath.FromSlash(RunsDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return runSummary{}, false
	}
	// A file system may keep whole seconds only.
	since := started.Truncate(time.Second)
	var newest runSummary
	var newestPath string
	var newestAt time.Time
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(since) || (newestPath != "" && !info.ModTime().After(newestAt)) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path) //nolint:gosec // turbo's own summary under the workspace root
		if err != nil {
			continue
		}
		var doc runSummary
		if json.Unmarshal(data, &doc) != nil || !sameRun(strings.Fields(doc.Execution.Command), command) {
			continue
		}
		newest, newestPath, newestAt = doc, path, info.ModTime()
	}
	if newestPath == "" {
		return runSummary{}, false
	}
	_ = os.Remove(newestPath)
	return newest, true
}

// sameRun reports whether two turbo commands run the same tasks over the same
// packages, whatever the program that starts turbo (turbo, npx turbo, pnpm
// turbo), the form of each filter (--filter=a, --filter a, -F a) and the other
// flags: turbo writes the command of a summary in a form of its own.
func sameRun(summarized, command []string) bool {
	tasks, filters, ok := turboRun(summarized)
	if !ok {
		return false
	}
	ownTasks, ownFilters, ok := turboRun(command)
	return ok && slices.Equal(tasks, ownTasks) && slices.Equal(filters, ownFilters)
}

// turboRun reads the tasks and the filters of a turbo run command, each
// sorted: the arguments after run up to the first flag, and the value of each
// --filter or -F.
func turboRun(args []string) (tasks, filters []string, ok bool) {
	i := slices.IndexFunc(args, func(a string) bool { return a == "run" })
	if i < 0 || i == 0 || filepath.Base(args[i-1]) != "turbo" {
		return nil, nil, false
	}
	flags := false
	for j := i + 1; j < len(args); j++ {
		a := args[j]
		switch {
		case a == "--filter" || a == "-F":
			if j+1 < len(args) {
				filters = append(filters, args[j+1])
				j++
			}
			flags = true
		case strings.HasPrefix(a, "--filter="):
			filters = append(filters, strings.TrimPrefix(a, "--filter="))
			flags = true
		case strings.HasPrefix(a, "-F") && len(a) > 2:
			filters = append(filters, strings.TrimPrefix(strings.TrimPrefix(a, "-F"), "="))
			flags = true
		case strings.HasPrefix(a, "-"):
			flags = true
		case !flags:
			tasks = append(tasks, a)
		}
	}
	slices.Sort(tasks)
	slices.Sort(filters)
	return tasks, filters, len(tasks) > 0
}

// commandTasks are the tasks a turbo run command names: the arguments after
// run up to the first flag.
func commandTasks(command []string) []string {
	var tasks []string
	after := false
	for _, arg := range command {
		switch {
		case !after:
			after = arg == "run"
		case strings.HasPrefix(arg, "-"):
			return tasks
		default:
			tasks = append(tasks, arg)
		}
	}
	return tasks
}

// failure is a failed task's message: turbo's error and the end of the log
// turbo kept for it, which the summary names from the workspace root or from
// the package directory.
func failure(root, taskID, turboErr, logFile, directory string) string {
	message := fmt.Sprintf("turbo run %s failed", taskID)
	if turboErr != "" {
		message += ": " + turboErr
	}
	if logFile == "" {
		return message
	}
	for _, path := range []string{filepath.Join(root, filepath.FromSlash(logFile)), filepath.Join(root, filepath.FromSlash(directory), filepath.FromSlash(logFile))} {
		data, err := os.ReadFile(path) //nolint:gosec // turbo's own log under the workspace root
		if err != nil {
			continue
		}
		if tail := lastLines(string(data), logLines); tail != "" {
			message += "\n" + tail
		}
		break
	}
	return message
}

// lastLines is the last n lines of text, without trailing blank lines.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
