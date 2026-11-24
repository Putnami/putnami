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
// projects (ADR 0019 of tooling/stacks): each component's target that the
// closing summary lists as failed fails with its section, and a target with
// a section that is not listed passes.
func ReadBatch(report stacks.BatchReport) map[string]stacks.BatchOutcome {
	sections, failed := readRunMany(string(report.Output))
	if len(sections) == 0 && len(failed) == 0 {
		return nil
	}
	targets := commandTargets(report.Command)
	out := map[string]stacks.BatchOutcome{}
	for _, component := range report.Components {
		named, anyFailed := false, false
		var messages []string
		for _, target := range targets {
			key, section, ran, isFailed := taskOutcome(sections, failed, component+":"+target)
			if isFailed {
				anyFailed, named = true, true
				message := "nx run " + key + " failed"
				if tail := lastLines(section, sectionLines); tail != "" {
					message += ":\n" + tail
				}
				messages = append(messages, message)
				continue
			}
			named = named || ran
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

// taskOutcome answers a target's outcome: the task key it quotes, its
// section, whether it ran and whether it failed. A target with a section of
// its own name is that task alone: a sibling such as <target>:lib is another
// task. Otherwise the target ran its default configuration,
// <project>:<target>:<configuration>, and it fails when the summary lists any
// such task as failed; it ran when any such task has a section.
func taskOutcome(sections map[string]string, failed map[string]bool, target string) (string, string, bool, bool) {
	if section, ok := sections[target]; ok {
		return target, section, true, failed[target]
	}
	if failed[target] {
		return target, "", false, true
	}
	key, section, ran := target, "", false
	// Sorted, so that the same output always quotes the same section.
	for _, listed := range slices.Sorted(maps.Keys(failed)) {
		if strings.HasPrefix(listed, target+":") {
			section, ran := sections[listed]
			return listed, section, ran, true
		}
	}
	for _, k := range slices.Sorted(maps.Keys(sections)) {
		if strings.HasPrefix(k, target+":") {
			key, section, ran = k, sections[k], true
			break
		}
	}
	return key, section, ran, false
}

// readRunMany splits Nx's static output into each target's section, by
// project:target, and the targets its closing summaries list as failed.
func readRunMany(output string) (map[string]string, map[string]bool) {
	sections := map[string]string{}
	failed := map[string]bool{}
	var current string
	var body []string
	inSummary, inFailed := false, false
	flush := func() {
		if current != "" {
			sections[current] = strings.TrimSpace(strings.Join(body, "\n"))
		}
		current, body = "", nil
	}
	for _, line := range strings.Split(ansi.ReplaceAllString(output, ""), "\n") {
		line = strings.TrimRight(line, "\r")
		if m := sectionStart.FindStringSubmatch(line); m != nil {
			flush()
			inSummary = false
			// Nx quotes a target whose name holds a colon:
			// > nx run a:"test:lib"; its summary does not.
			current = strings.ReplaceAll(m[1], `"`, "")
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "::endgroup::") {
			continue
		}
		if summaryStart.MatchString(line) {
			flush()
			inSummary, inFailed = true, false
			continue
		}
		if inSummary {
			// Only the block of failed tasks: the tasks Nx did not run are
			// listed the same way under their own heading.
			if heading := strings.TrimSpace(line); strings.HasSuffix(heading, ":") {
				lower := strings.ToLower(heading)
				inFailed = strings.Contains(lower, "fail") && !strings.Contains(lower, "not run")
				continue
			}
			if m := failedItem.FindStringSubmatch(line); m != nil && inFailed {
				failed[m[1]] = true
			}
			continue
		}
		if current != "" {
			body = append(body, line)
		}
	}
	flush()
	return sections, failed
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
