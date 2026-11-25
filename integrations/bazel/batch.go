package bazel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/tooling/stacks"
)

// BatchEvents is the build-event file a batch command writes in its {batch}
// directory.
const BatchEvents = "build-events.json"

// logLines is how many lines of a failed test's log its message quotes.
const logLines = 40

// passedTests are the overall statuses of a test that passed.
var passedTests = map[string]bool{"PASSED": true, "FLAKY": true}

// batchEvent is the part of a build event the batch reader reads.
type batchEvent struct {
	ID struct {
		TargetCompleted   *labelID `json:"targetCompleted"`
		TestSummary       *labelID `json:"testSummary"`
		ConfiguredLabel   *labelID `json:"configuredLabel"`
		UnconfiguredLabel *labelID `json:"unconfiguredLabel"`
		TargetConfigured  *labelID `json:"targetConfigured"`
	} `json:"id"`
	Configured *struct {
		TestSize string `json:"testSize"`
	} `json:"configured"`
	// LastMessage marks the last event Bazel writes: a file without it was
	// cut short.
	LastMessage bool `json:"lastMessage"`
	Completed   *struct {
		Success       bool `json:"success"`
		FailureDetail *struct {
			Message string `json:"message"`
		} `json:"failureDetail"`
	} `json:"completed"`
	Aborted *struct {
		Reason      string `json:"reason"`
		Description string `json:"description"`
	} `json:"aborted"`
	TestSummary *struct {
		OverallStatus string `json:"overallStatus"`
		Failed        []struct {
			URI string `json:"uri"`
		} `json:"failed"`
	} `json:"testSummary"`
}

type labelID struct {
	Label string `json:"label"`
}

// label is the label an event is about, empty for another event.
func (e batchEvent) label() string {
	for _, id := range []*labelID{e.ID.TargetCompleted, e.ID.TestSummary, e.ID.ConfiguredLabel, e.ID.UnconfiguredLabel, e.ID.TargetConfigured} {
		if id != nil {
			return id.Label
		}
	}
	return ""
}

// packageResult is what the build-event file states of one package.
type packageResult struct {
	completed bool
	failures  []string
	// tests are the package's test targets, true once one's summary passed.
	tests map[string]bool
}

// passed reports whether the package passed: one of its targets completed,
// none failed, and, for a test command, each of its test targets has a
// summary that passed: a test that completed its build may never have run.
func (r packageResult) passed(testing bool) bool {
	if !r.completed || len(r.failures) > 0 {
		return false
	}
	for _, ok := range r.tests {
		if testing && !ok {
			return false
		}
	}
	return true
}

// ReadBatch reads the build-event file a failed bazel test or bazel build over
// several packages wrote in {batch} (ADR 0019 of tooling/stacks). A package's
// targets are the labels //<package>:<name>. A package fails when one of its
// tests ends other than passed or flaky, with Bazel's status and the end of
// the test's log, or one of its targets failed or was aborted; it passes when
// one of its targets completed, none failed and, for bazel test, each of its
// test targets has a summary that passed. Any other package is not named, and
// the core fails it as not run; so is every package of a file Bazel did not
// finish, which holds no last message. A failure of a label
// of no package of the command, as a test a suite of another package runs,
// names no package: the file cannot say which one it fails.
func ReadBatch(report stacks.BatchReport) map[string]stacks.BatchOutcome {
	if !report.Failed {
		return nil
	}
	verb := "bazel"
	if len(report.Command) > 1 {
		verb += " " + report.Command[1]
	}
	results, ok := readBatchEvents(filepath.Join(report.Dir, BatchEvents), verb)
	if !ok {
		return nil
	}
	for pkg, r := range results {
		if len(r.failures) > 0 && !slices.Contains(report.Components, pkg) {
			return nil
		}
	}
	testing := len(report.Command) > 1 && report.Command[1] == "test"
	out := map[string]stacks.BatchOutcome{}
	for _, component := range report.Components {
		r := results[component]
		switch {
		case len(r.failures) > 0:
			out[component] = stacks.BatchOutcome{Failed: true, Message: strings.Join(r.failures, "\n")}
		case r.passed(testing):
			out[component] = stacks.BatchOutcome{}
		}
	}
	return out
}

// readBatchEvents reads each package's result from a build-event file.
func readBatchEvents(file, verb string) (map[string]packageResult, bool) {
	f, err := os.Open(file) //nolint:gosec // the build-event file the batch's own command wrote
	if err != nil {
		return nil, false
	}
	defer f.Close()
	results := map[string]packageResult{}
	finished := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e batchEvent
		if json.Unmarshal([]byte(line), &e) != nil {
			return nil, false
		}
		finished = finished || e.LastMessage
		label := e.label()
		pkg, ok := labelPackage(label)
		if !ok {
			continue
		}
		r := results[pkg]
		if r.tests == nil {
			r.tests = map[string]bool{}
		}
		switch {
		case e.Aborted != nil:
			message := fmt.Sprintf("%s aborted %s: %s", verb, label, strings.ToLower(e.Aborted.Reason))
			if e.Aborted.Description != "" {
				message += ": " + e.Aborted.Description
			}
			r.failures = append(r.failures, message)
		case e.ID.TestSummary != nil && e.TestSummary != nil && !passedTests[e.TestSummary.OverallStatus]:
			r.failures = append(r.failures, testFailure(verb, label, e.TestSummary.OverallStatus, e.TestSummary.Failed))
		case e.ID.TargetCompleted != nil && e.Completed != nil && !e.Completed.Success:
			message := fmt.Sprintf("%s failed to build %s", verb, label)
			if d := e.Completed.FailureDetail; d != nil && d.Message != "" {
				message += ": " + d.Message
			}
			r.failures = append(r.failures, message)
		case e.ID.TargetCompleted != nil && e.Completed != nil:
			r.completed = true
		case e.ID.TestSummary != nil && e.TestSummary != nil:
			r.tests[label] = true
		case e.ID.TargetConfigured != nil && e.Configured != nil && e.Configured.TestSize != "":
			r.tests[label] = r.tests[label] || false
		default:
			continue
		}
		results[pkg] = r
	}
	if scanner.Err() != nil || !finished {
		return nil, false
	}
	return results, true
}

// testFailure is a failed test's message: its status and the end of the log
// of its first failed run.
func testFailure(verb, label, status string, failed []struct {
	URI string `json:"uri"`
},
) string {
	message := fmt.Sprintf("%s %s %s", verb, label, status)
	for _, run := range failed {
		u, err := url.Parse(run.URI)
		if err != nil || u.Scheme != "file" {
			continue
		}
		data, err := os.ReadFile(filepath.FromSlash(u.Path)) //nolint:gosec // the test log the build-event file names
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) > logLines {
			lines = lines[len(lines)-logLines:]
		}
		if tail := strings.TrimSpace(strings.Join(lines, "\n")); tail != "" {
			message += "\n" + tail
		}
		break
	}
	return message
}
