package nx

import (
	"regexp"
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
			// A target that runs its default configuration is named
			// <project>:<target>:<configuration>.
			key, section, ran := taskOf(sections, component+":"+target)
			if failedTask(failed, component+":"+target) {
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

// taskOf answers the section of a target, named as the command names it or
// with the configuration Nx resolved: its key, section and whether it ran.
func taskOf(sections map[string]string, target string) (string, string, bool) {
	if section, ok := sections[target]; ok {
		return target, section, true
	}
	for key, section := range sections {
		if strings.HasPrefix(key, target+":") {
			return key, section, true
		}
	}
	return target, "", false
}

// failedTask reports whether the summary lists the target as failed, with or
// without its configuration.
func failedTask(failed map[string]bool, target string) bool {
	if failed[target] {
		return true
	}
	for key := range failed {
		if strings.HasPrefix(key, target+":") {
			return true
		}
	}
	return false
}

// readRunMany splits Nx's static output into each target's section, by
// project:target, and the targets its closing summaries list as failed.
func readRunMany(output string) (map[string]string, map[string]bool) {
	sections := map[string]string{}
	failed := map[string]bool{}
	var current string
	var body []string
	inSummary := false
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
			current = m[1]
			continue
		}
		if summaryStart.MatchString(line) {
			flush()
			inSummary = true
			continue
		}
		if inSummary {
			if m := failedItem.FindStringSubmatch(line); m != nil {
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
