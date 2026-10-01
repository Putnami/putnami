package cli

import (
	"encoding/json"
	"unicode/utf8"
)

// Test-case statuses: the closed vocabulary of TestCase.Status. They match the
// counters of protocols/runtime's TestSummary, so the records of a task and its
// summary use one set of words.
const (
	TestCaseStatusPassed  = "passed"
	TestCaseStatusFailed  = "failed"
	TestCaseStatusSkipped = "skipped"
)

// Test-case bounds. They are contract clauses, like the report bounds: the
// drift tests pin every one against schemas/result-v2.json, so a producer
// cannot widen them locally. Both validators enforce the text and output
// bounds on every test:case record. The count and byte budgets span records,
// so no per-record check can enforce them; BoundTestCases applies them and
// task:end reports the rest in TaskRecord.TestCasesDropped.
const (
	// TestCaseMaxPerTask bounds the test:case records of one task. A producer
	// past it keeps failed cases first, then skipped, then passed, and counts
	// the rest in TaskRecord.TestCasesDropped.
	TestCaseMaxPerTask = 1000
	// TestCaseMaxOutputBytes bounds TestCase.Output, in UTF-8 bytes.
	TestCaseMaxOutputBytes = 4096
	// TestCaseMaxTextBytes bounds TestCase.Name, Suite and File, in UTF-8 bytes.
	TestCaseMaxTextBytes = 1024
	// TestCaseMaxBytesPerTask bounds the cases of one task by their size: the
	// sum of the compact JSON encoding of each kept case. The count bound alone
	// allows about 7 MB of failure output per task, which a batch multiplies
	// by its members into one result line.
	TestCaseMaxBytesPerTask = 1 << 20
	// TestCaseMaxBytesPerBatch bounds the cases of every member of one batched
	// run together. A batch hands them to the CLI in one result line, which the
	// CLI reads whole, so the budget keeps that line far from the reader's
	// limit whatever the member count. TestCaseBatchMemberBytes splits it.
	TestCaseMaxBytesPerBatch = 8 << 20
)

// testCaseOutputHeadBytes is the share of a truncated output kept from its
// start. The start usually names the failed assertion and the end the error
// the runner printed last, so a truncated output keeps both.
const testCaseOutputHeadBytes = 1024

// testCaseOutputCut marks where a truncated output lost its middle.
const testCaseOutputCut = "\n[…]\n"

// TestCase is one test case a test task ran: the payload of a test:case record.
type TestCase struct {
	// Name is the full test name as the runner reports it: "TestParse/empty"
	// for Go, "parser > rejects empty input" for TypeScript,
	// "TestParser::test_empty" for Python.
	Name string `json:"name"`
	// Suite groups the case: the Go package import path, or the
	// workspace-relative test file for TypeScript and Python.
	Suite string `json:"suite"`
	// Status is TestCaseStatusPassed, TestCaseStatusFailed or
	// TestCaseStatusSkipped.
	Status string `json:"status"`
	// DurationMs is the case's wall time as the runner measured it.
	DurationMs int64 `json:"durationMs"`
	// Output is what the case printed, or the runner's failure or skip message.
	// Only failed and skipped cases carry it. It is bounded by
	// TestCaseMaxOutputBytes and redacted by the machine-output sanitizer.
	Output string `json:"output,omitempty"`
	// OutputTruncated reports that Output lost its middle to the bound.
	OutputTruncated bool `json:"outputTruncated,omitempty"`
	// File is the workspace-relative file the runner ties to the case, when it
	// reports one: where the case is declared for TypeScript and Python, and the
	// first test-file location the case's output names for Go, whose runner
	// reports no declaration site.
	File string `json:"file,omitempty"`
	// Line is the 1-based line in File, when the runner reports it.
	Line int `json:"line,omitempty"`
}

// BoundTestCases applies the test-case contract to the cases one task
// reported, in runner order. It is BoundTestCasesWithin with the
// TestCaseMaxBytesPerTask budget.
func BoundTestCases(cases []TestCase) (kept []TestCase, dropped int) {
	return BoundTestCasesWithin(cases, TestCaseMaxBytesPerTask)
}

// BoundTestCasesWithin applies the test-case contract to the cases one task
// reported, in runner order, within maxBytes of encoded cases. A batch
// producer passes TestCaseBatchMemberBytes; any budget above
// TestCaseMaxBytesPerTask is lowered to it.
//
// It drops a case without a name, a suite or a known status, since a producer
// never guesses them. It sanitizes every string with SanitizeMachineOutputString
// before it cuts over-long text on a code point, so the CLI's own sanitization
// of the record cannot grow a bounded string past its bound or leave part of a
// secret at a cut. It drops the output of a passed case. It then keeps failed
// cases first, then skipped, then passed, each in runner order: a case is kept
// while fewer than TestCaseMaxPerTask are kept and its compact JSON encoding
// fits in what remains of maxBytes. The kept cases keep their original order.
// dropped counts every case it did not keep.
func BoundTestCasesWithin(cases []TestCase, maxBytes int) (kept []TestCase, dropped int) {
	maxBytes = min(maxBytes, TestCaseMaxBytesPerTask)
	valid := make([]TestCase, 0, len(cases))
	sizes := make([]int, 0, len(cases))
	total := 0
	for _, c := range cases {
		c, ok := boundTestCase(c)
		if !ok {
			dropped++
			continue
		}
		size := encodedTestCaseBytes(c)
		valid = append(valid, c)
		sizes = append(sizes, size)
		total += size
	}
	if len(valid) <= TestCaseMaxPerTask && total <= maxBytes {
		return valid, dropped
	}
	keep := make([]bool, len(valid))
	room, bytesRoom := TestCaseMaxPerTask, maxBytes
	for _, status := range []string{TestCaseStatusFailed, TestCaseStatusSkipped, TestCaseStatusPassed} {
		for i := range valid {
			if room == 0 {
				break
			}
			if valid[i].Status == status && sizes[i] <= bytesRoom {
				keep[i] = true
				room--
				bytesRoom -= sizes[i]
			}
		}
	}
	kept = make([]TestCase, 0, min(len(valid), TestCaseMaxPerTask))
	for i, c := range valid {
		if keep[i] {
			kept = append(kept, c)
		} else {
			dropped++
		}
	}
	return kept, dropped
}

// TestCaseBatchMemberBytes is the case budget of one member of a batched run
// with members members: an equal share of TestCaseMaxBytesPerBatch, never more
// than TestCaseMaxBytesPerTask. A batch producer passes it to
// BoundTestCasesWithin for each member.
func TestCaseBatchMemberBytes(members int) int {
	if members < 1 {
		members = 1
	}
	return min(TestCaseMaxBytesPerTask, TestCaseMaxBytesPerBatch/members)
}

// encodedTestCaseBytes is the size of c's compact JSON encoding, the measure
// of the byte budgets.
func encodedTestCaseBytes(c TestCase) int {
	encoded, err := json.Marshal(c)
	if err != nil {
		return TestCaseMaxBytesPerTask + 1
	}
	return len(encoded)
}

func boundTestCase(c TestCase) (TestCase, bool) {
	switch c.Status {
	case TestCaseStatusPassed, TestCaseStatusFailed, TestCaseStatusSkipped:
	default:
		return c, false
	}
	c.Name = truncateUTF8(SanitizeMachineOutputString(c.Name), TestCaseMaxTextBytes)
	c.Suite = truncateUTF8(SanitizeMachineOutputString(c.Suite), TestCaseMaxTextBytes)
	if c.Name == "" || c.Suite == "" {
		return c, false
	}
	c.File = truncateUTF8(SanitizeMachineOutputString(c.File), TestCaseMaxTextBytes)
	if c.File == "" || c.Line < 1 {
		c.Line = 0
	}
	if c.DurationMs < 0 {
		c.DurationMs = 0
	}
	if c.Status == TestCaseStatusPassed {
		c.Output, c.OutputTruncated = "", false
	} else {
		output, truncated := TruncateTestCaseOutput(c.Output)
		c.Output, c.OutputTruncated = output, c.OutputTruncated || truncated
	}
	if c.Output == "" {
		c.OutputTruncated = false
	}
	return c, true
}

// TruncateTestCaseOutput sanitizes one case's output with
// SanitizeMachineOutputString and bounds it to TestCaseMaxOutputBytes. An
// output past the bound keeps its first bytes and its last bytes, cut on code
// points, around a marker line; truncated reports that it did. The result is a
// fixed point of the sanitizer, so sanitizing it again keeps it within the
// bound.
func TruncateTestCaseOutput(output string) (bounded string, truncated bool) {
	output = SanitizeMachineOutputString(output)
	if len(output) <= TestCaseMaxOutputBytes {
		return output, false
	}
	head := truncateUTF8(output, testCaseOutputHeadBytes)
	tail := lastUTF8(output, TestCaseMaxOutputBytes-len(head)-len(testCaseOutputCut))
	return head + testCaseOutputCut + tail, true
}

// truncateUTF8 returns the longest prefix of s within limit bytes that ends on
// a code point boundary. s must be valid UTF-8.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// lastUTF8 returns the longest suffix of s within limit bytes that starts on a
// code point boundary. s must be valid UTF-8.
func lastUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	start := len(s) - limit
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
