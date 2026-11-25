package rush

import (
	"fmt"
	"regexp"
	"strings"

	"go.putnami.dev/tooling/stacks"
)

// detailLines is how many lines of the details Rush printed for a failed
// operation its message quotes.
const detailLines = 40

var (
	// ansi matches the escape sequences of a colored output.
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// statusHeader opens one status of Rush's closing summary:
	// ==[ FAILURE: 1 operation ]====.
	statusHeader = regexp.MustCompile(`^==\[ ([A-Z][A-Z ]*[A-Z]): \d+ [a-z]+ \]=*\s*$`)
	// operationHeader opens an operation's output while the command runs:
	// ==[ <name> ]====[ 1 of 3 ]==.
	operationHeader = regexp.MustCompile(`^==\[ .+ \]=+\[ \d+ of \d+ \]==\s*$`)
	// detailHeader opens the details of one operation of a detailed status:
	// --[ FAILURE: <name> ]----[ 1.01 seconds ]--.
	detailHeader = regexp.MustCompile(`^--\[ [A-Z][A-Z ]*: (.+?) \]-*\[ .* \]--\s*$`)
	// listedOperation is one operation of a condensed status: its name, then
	// four spaces or more before its time, if any.
	listedOperation = regexp.MustCompile(`^  (\S.*?)(?: {4,}.*)?$`)
)

// The statuses of Rush's summary, by how the reader takes them: a passed
// operation, a failed one, and one Rush did not run.
var (
	passedStatuses  = map[string]bool{"SUCCESS": true, "FROM CACHE": true, "SKIPPED": true, "NO OP": true}
	failedStatuses  = map[string]bool{"FAILURE": true, "SUCCESS WITH WARNINGS": true}
	skippedStatuses = map[string]bool{"BLOCKED": true, "ABORTED": true}
)

// summaryEnd are the lines Rush ends its summary with.
var summaryEnd = map[string]bool{"Operations failed.": true, "Operations succeeded with warnings.": true}

// operation is one operation of Rush's summary: its status and the details
// Rush printed for it.
type operation struct {
	status  string
	details []string
}

// ReadBatch reads the summary a failed rush command over several projects
// printed (ADR 0019 of tooling/stacks). A project's operations are named
// <project>, or <project> (<phase>) in a phased command. A project with a
// failed operation, or one with warnings, which fail the command, fails with
// the details Rush printed; one whose operations all succeeded, were restored
// from cache, were up to date or had no work passes. A project with an
// operation Rush blocked or aborted, or with none listed, is not named, and
// the core fails it as not run.
func ReadBatch(report stacks.BatchReport) map[string]stacks.BatchOutcome {
	if !report.Failed {
		return nil
	}
	operations := readSummary(string(report.Output))
	if len(operations) == 0 {
		return nil
	}
	verb := "rush"
	if len(report.Command) > 1 {
		verb += " " + report.Command[1]
	}
	out := map[string]stacks.BatchOutcome{}
	for _, component := range report.Components {
		passed, skipped := false, false
		var messages []string
		for name, op := range operations {
			if name != component && !strings.HasPrefix(name, component+" (") {
				continue
			}
			switch {
			case failedStatuses[op.status]:
				messages = append(messages, failure(verb, name, op))
			case skippedStatuses[op.status]:
				skipped = true
			case passedStatuses[op.status]:
				passed = true
			}
		}
		switch {
		case len(messages) > 0:
			out[component] = stacks.BatchOutcome{Failed: true, Message: strings.Join(messages, "\n")}
		case passed && !skipped:
			out[component] = stacks.BatchOutcome{}
		}
	}
	return out
}

// readSummary reads Rush's closing summary from the output: each operation
// it lists, by name. An operation's header while the command runs starts the
// output over, as a new run of the operations does.
func readSummary(output string) map[string]operation {
	operations := map[string]operation{}
	status, detailed := "", ""
	for _, line := range strings.Split(ansi.ReplaceAllString(output, ""), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case operationHeader.MatchString(line):
			operations, status, detailed = map[string]operation{}, "", ""
		case statusHeader.MatchString(line):
			status, detailed = statusHeader.FindStringSubmatch(line)[1], ""
			if !passedStatuses[status] && !failedStatuses[status] && !skippedStatuses[status] {
				status = ""
			}
		case status == "":
		case summaryEnd[strings.TrimSpace(line)]:
			status, detailed = "", ""
		case failedStatuses[status] && detailHeader.MatchString(line):
			detailed = detailHeader.FindStringSubmatch(line)[1]
			operations[detailed] = operation{status: status}
		case failedStatuses[status]:
			if detailed != "" {
				op := operations[detailed]
				op.details = append(op.details, line)
				operations[detailed] = op
			}
		case strings.TrimSpace(line) == "":
		case listedOperation.MatchString(line):
			operations[listedOperation.FindStringSubmatch(line)[1]] = operation{status: status}
		}
	}
	return operations
}

// failure is a failed operation's message: its status and the end of the
// details Rush printed for it.
func failure(verb, name string, op operation) string {
	message := fmt.Sprintf("%s failed for %s", verb, name)
	if op.status == "SUCCESS WITH WARNINGS" {
		message = fmt.Sprintf("%s reported warnings for %s, which fail the command", verb, name)
	}
	details := op.details
	if len(details) > detailLines {
		details = details[len(details)-detailLines:]
	}
	if text := strings.TrimSpace(strings.Join(details, "\n")); text != "" {
		message += "\n" + text
	}
	return message
}
