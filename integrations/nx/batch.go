package nx

import (
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.putnami.dev/tooling/stacks"
)

// sectionLines is how many lines of a failed target's section its message
// quotes.
const sectionLines = 40

var (
	// ansi matches the escape sequences of a colored output.
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// sectionStart is the line that opens a target's section in Nx's static
	// output: > nx run <project>:<target>, maybe followed by a cache note; on
	// GitHub Actions Nx opens a log group before it: ::group::<icon> > nx run.
	sectionStart = regexp.MustCompile(`^\s*(?:::group::[^>]{0,8})?>\s+nx run\s+(\S+)`)
	// summaryStart is the line that opens Nx's closing summary: " NX  …",
	// or " >  NX  …" before Nx 17.
	summaryStart = regexp.MustCompile(`^\s*(?:>\s+)?NX\s{2,}`)
	// failedItem is a target the closing summary lists as failed.
	failedItem = regexp.MustCompile(`^\s*-\s+(?:nx run\s+)?(\S+:\S+)\s*$`)
)

// ReadBatch reads the static output of a failed nx run-many over several
// projects (ADR 0019 of tooling/stacks). A component's target fails when one
// of its own runs failed, with that run's section, or when Nx lists it among
// the tasks it did not run; it passes when one of its runs has a section and
// none failed; it is not named otherwise, and the core fails it as not run.
// A run of <target> is <project>:<target> or <project>:<target>:<config>; a
// quoted target such as <project>:"<target>:x" is another target.
func ReadBatch(report stacks.BatchReport) map[string]stacks.BatchOutcome {
	run := readRunMany(string(report.Output))
	if len(run.sections) == 0 && len(run.failed) == 0 && len(run.notRun) == 0 {
		return nil
	}
	out := map[string]stacks.BatchOutcome{}
	for _, component := range report.Components {
		named, anyFailed := false, false
		var messages []string
		for _, target := range commandTargets(report.Command) {
			verdict := run.judge(component, target)
			switch {
			case verdict.failed:
				anyFailed, named = true, true
				messages = append(messages, verdict.message)
			case verdict.passed:
				named = true
			}
		}
		switch {
		case anyFailed:
			out[component] = stacks.BatchOutcome{Failed: true, Message: strings.Join(messages, "\n")}
		case named:
			out[component] = stacks.BatchOutcome{}
		}
	}
	return out
}

// task is one task of Nx's output: its project, its target and its
// configuration, with the id Nx's summary names it by.
type task struct {
	project, target, config string
}

// parseTask reads a task as a section header prints it: project:target,
// project:target:config, or project:"target":config for a target that holds a
// colon. A summary prints the id unquoted, which parseTask reads as an
// unquoted header.
func parseTask(text string) (task, bool) {
	project, rest, ok := strings.Cut(text, ":")
	if !ok || project == "" || rest == "" {
		return task{}, false
	}
	if quoted, ok := strings.CutPrefix(rest, `"`); ok {
		target, after, ok := strings.Cut(quoted, `"`)
		if !ok || target == "" {
			return task{}, false
		}
		return task{project: project, target: target, config: strings.TrimPrefix(after, ":")}, true
	}
	target, config, _ := strings.Cut(rest, ":")
	return task{project: project, target: target, config: config}, true
}

// id is the task's id as Nx's summary prints it.
func (t task) id() string {
	id := t.project + ":" + t.target
	if t.config != "" {
		id += ":" + t.config
	}
	return id
}

// runMany is what Nx's static output states: each task that ran, with its
// section, by id, and the ids its summary lists as failed or as not run.
type runMany struct {
	sections map[string]string
	tasks    map[string]task
	failed   map[string]bool
	// notRun maps each id Nx did not run, or stopped, to the list that
	// names it.
	notRun map[string]string
}

// The lists of Nx's closing summary the reader reads: the tasks that
// failed, those Nx did not run or skipped, and those it stopped before they
// finished.
const (
	notRunList  = "not run"
	stoppedList = "stopped"
	listFailed  = "failed"
)

// verdict is a target's outcome in one component.
type verdict struct {
	failed, passed bool
	message        string
}

// judge answers the outcome of component:target. Its runs are the tasks
// whose section header names that target, unquoted, with or without a
// configuration. An id the summary lists without a section resolves through
// the headers when one names it; otherwise <component>:<target> is its run,
// and so is <component>:<target>:<x> when the target has no run of its own.
func (r runMany) judge(component, target string) verdict {
	own := component + ":" + target
	runs := map[string]bool{}
	for id, t := range r.tasks {
		if t.project == component && t.target == target {
			runs[id] = true
		}
	}
	ranAlone := len(runs) > 0
	belongs := func(id string) bool {
		if runs[id] {
			return true
		}
		if _, known := r.tasks[id]; known {
			return false
		}
		return id == own || (!ranAlone && strings.HasPrefix(id, own+":"))
	}
	for _, id := range slices.Sorted(maps.Keys(r.failed)) {
		if belongs(id) {
			message := "nx run " + id + " failed"
			if tail := lastLines(r.sections[id], sectionLines); tail != "" {
				message += ":\n" + tail
			}
			return verdict{failed: true, message: message}
		}
	}
	// A task Nx lists as not run, or stopped, fails whatever section it
	// printed: a stopped task prints its partial output as a section.
	for _, id := range slices.Sorted(maps.Keys(r.notRun)) {
		if belongs(id) {
			if r.notRun[id] == stoppedList {
				return verdict{failed: true, message: "nx stopped " + id + " before it finished"}
			}
			return verdict{failed: true, message: "nx did not run " + id}
		}
	}
	return verdict{passed: ranAlone}
}

// readRunMany reads Nx's static output: each section, by the task its header
// names, and the ids the closing summaries list under a heading of failed
// tasks, of tasks Nx did not run or skipped, or of tasks it stopped.
func readRunMany(output string) runMany {
	r := runMany{sections: map[string]string{}, tasks: map[string]task{}, failed: map[string]bool{}, notRun: map[string]string{}}
	var current string
	var body []string
	inSummary := false
	// list is the summary's list the current heading opened: listFailed,
	// notRunList, stoppedList, or none.
	var list string
	flush := func() {
		if current != "" {
			r.sections[current] = strings.TrimSpace(strings.Join(body, "\n"))
		}
		current, body = "", nil
	}
	for _, line := range strings.Split(ansi.ReplaceAllString(output, ""), "\n") {
		line = strings.TrimRight(line, "\r")
		if m := sectionStart.FindStringSubmatch(line); m != nil {
			flush()
			inSummary = false
			if t, ok := parseTask(m[1]); ok {
				current = t.id()
				r.tasks[current] = t
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "::endgroup::") {
			continue
		}
		if summaryStart.MatchString(line) {
			flush()
			inSummary, list = true, ""
			continue
		}
		if inSummary {
			// Each heading opens a list: the tasks Nx stopped, those it did
			// not run or skipped, or those that failed, all listed the same
			// way. Any other heading ends the current list.
			if heading := strings.TrimSpace(line); strings.HasSuffix(heading, ":") {
				lower := strings.ToLower(heading)
				switch {
				case strings.Contains(lower, "stopped"):
					list = stoppedList
				case strings.Contains(lower, "not run"), strings.Contains(lower, "skipped"):
					list = notRunList
				case strings.Contains(lower, "fail"):
					list = listFailed
				default:
					list = ""
				}
				continue
			}
			if m := failedItem.FindStringSubmatch(line); m != nil {
				switch list {
				case listFailed:
					r.failed[m[1]] = true
				case notRunList, stoppedList:
					r.notRun[m[1]] = list
				}
			}
			continue
		}
		if current != "" {
			body = append(body, line)
		}
	}
	flush()
	return r
}

// commandTargets are the targets an nx run-many command names after -t or
// --targets, comma-separated or not.
func commandTargets(command []string) []string {
	var out []string
	takeNext := false
	for _, arg := range command {
		var value string
		switch {
		case takeNext:
			value, takeNext = arg, false
		case arg == "-t" || arg == "--targets" || arg == "--target":
			takeNext = true
			continue
		case strings.HasPrefix(arg, "--targets="), strings.HasPrefix(arg, "--target="), strings.HasPrefix(arg, "-t="):
			_, value, _ = strings.Cut(arg, "=")
		default:
			continue
		}
		for _, t := range strings.Split(value, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// lastLines is the last n lines of text.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// batchStep is the step that runs a target over the components of a batch:
// the profile's batch step, with the target's name in place of the task's.
func batchStep(target string) []string {
	return []string{"npx", "--no", "nx", "run-many", "-t", target, "-p", stacks.BatchPlaceholder, "--outputStyle=static"}
}

// batchSteps are the batch steps of the steps the sync writes: one nx
// run-many per target the project's task runs.
func batchSteps(_ string, steps [][]string) [][]string {
	var out [][]string
	for _, step := range steps {
		if len(step) <= runStepArg {
			return nil
		}
		target, ok := strings.CutPrefix(step[runStepArg], runStepPrefix)
		if !ok {
			return nil
		}
		out = append(out, batchStep(target))
	}
	return out
}
