package moon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.putnami.dev/tooling/stacks"
)

// RunReport is the file moon writes at the workspace root after each run:
// one action per node of its pipeline, a task's with its target and status.
const RunReport = ".moon/cache/runReport.json"

// outputLines is how many lines of the output moon kept for a failed task
// its message quotes.
const outputLines = 40

// runReport is what the core reads of RunReport.
type runReport struct {
	Actions []struct {
		Label  string          `json:"label"`
		Status string          `json:"status"`
		Error  *string         `json:"error"`
		Node   json.RawMessage `json:"node"`
	} `json:"actions"`
}

// runTaskLabel is the label of a task's action: RunTask(<target>), or the
// interactive and persistent forms.
var runTaskLabel = regexp.MustCompile(`^Run(?:Interactive|Persistent)?Task\((.+)\)$`)

// ReadBatch reads the run report a failed moon run of several projects wrote
// (ADR 0019 of tooling/stacks): each component's target the command named,
// passed or cached, or failed with moon's error and the end of the output
// moon kept for it.
func ReadBatch(report stacks.BatchReport) map[string]stacks.BatchOutcome {
	// The core removes the run report before the command (BatchReports), so
	// a report here is this command's.
	path := filepath.Join(report.Root, filepath.FromSlash(RunReport))
	data, err := os.ReadFile(path) //nolint:gosec // moon's own report under the stack root
	if err != nil {
		return nil
	}
	var doc runReport
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	type action struct {
		status string
		err    string
	}
	actions := map[string]action{}
	for _, a := range doc.Actions {
		target := actionTarget(a.Label, a.Node)
		if target == "" {
			continue
		}
		var message string
		if a.Error != nil {
			message = *a.Error
		}
		actions[target] = action{status: a.Status, err: message}
	}
	out := map[string]stacks.BatchOutcome{}
	for _, component := range report.Components {
		target := commandTarget(report.Command, component)
		a, ok := actions[target]
		if !ok {
			continue
		}
		switch a.status {
		case "passed", "cached", "cached-from-remote":
			out[component] = stacks.BatchOutcome{}
		case "failed", "failed-and-abort", "timed-out", "invalid":
			out[component] = stacks.BatchOutcome{Failed: true, Message: failure(report.Root, target, a.status, a.err)}
		}
	}
	return out
}

// actionTarget is the target of a task's action: its node's, else its
// label's; empty for an action that runs no task.
func actionTarget(label string, node json.RawMessage) string {
	var n struct {
		Target string `json:"target"`
		Params struct {
			Target string `json:"target"`
		} `json:"params"`
	}
	if json.Unmarshal(node, &n) == nil {
		if n.Params.Target != "" {
			return n.Params.Target
		}
		if n.Target != "" {
			return n.Target
		}
	}
	if m := runTaskLabel.FindStringSubmatch(label); m != nil {
		return m[1]
	}
	return ""
}

// commandTarget is the target the command named for the component:
// <component>:<task>.
func commandTarget(command []string, component string) string {
	for _, arg := range command {
		if strings.HasPrefix(arg, component+":") {
			return arg
		}
	}
	return ""
}

// failure is a failed target's message: moon's error, and the end of the
// output moon kept for the task under .moon/cache/states.
func failure(root, target, status, moonErr string) string {
	message := fmt.Sprintf("moon run %s %s", target, strings.ReplaceAll(status, "-", " "))
	if moonErr != "" {
		message += ": " + moonErr
	}
	project, task, ok := strings.Cut(target, ":")
	if !ok {
		return message
	}
	dir := filepath.Join(root, ".moon", "cache", "states", project, task)
	for _, name := range []string{"stderr.log", "stdout.log"} {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // moon's own output under the stack root
		if tail := lastLines(string(data), outputLines); err == nil && tail != "" {
			message += "\n" + tail
		}
	}
	return message
}

// lastLines is the last n non-empty-trailing lines of text.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
