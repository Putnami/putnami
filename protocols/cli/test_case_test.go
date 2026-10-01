package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundTestCasesDropsCasesItCannotDescribe(t *testing.T) {
	cases := []TestCase{
		{Name: "TestA", Suite: "example.com/a", Status: TestCaseStatusPassed},
		{Name: "", Suite: "example.com/a", Status: TestCaseStatusPassed},
		{Name: "TestB", Suite: "", Status: TestCaseStatusFailed},
		{Name: "TestC", Suite: "example.com/a", Status: "errored"},
		{Name: "TestD", Suite: "example.com/a", Status: ""},
		{Name: "\x1b[31m\x1b[0m", Suite: "example.com/a", Status: TestCaseStatusFailed},
		{Name: "TestE", Suite: "example.com/a", Status: TestCaseStatusSkipped},
	}
	kept, dropped := BoundTestCases(cases)
	if dropped != 5 {
		t.Errorf("dropped = %d, want 5 (empty name, empty suite, two unknown statuses, a name the sanitizer empties)", dropped)
	}
	if got := testCaseNames(kept); !reflect.DeepEqual(got, []string{"TestA", "TestE"}) {
		t.Errorf("kept = %v, want [TestA TestE]", got)
	}
}

func TestBoundTestCasesKeepsEveryCaseWithinTheCap(t *testing.T) {
	cases := make([]TestCase, TestCaseMaxPerTask)
	for i := range cases {
		cases[i] = TestCase{Name: fmt.Sprintf("Test%04d", i), Suite: "example.com/a", Status: TestCaseStatusPassed}
	}
	kept, dropped := BoundTestCases(cases)
	if len(kept) != TestCaseMaxPerTask || dropped != 0 {
		t.Fatalf("exactly %d cases: kept %d, dropped %d; want all kept", TestCaseMaxPerTask, len(kept), dropped)
	}

	kept, dropped = BoundTestCases(append(cases, TestCase{Name: "TestLast", Suite: "example.com/a", Status: TestCaseStatusPassed}))
	if len(kept) != TestCaseMaxPerTask || dropped != 1 {
		t.Fatalf("one case past the cap: kept %d, dropped %d; want %d and 1", len(kept), dropped, TestCaseMaxPerTask)
	}
	if kept[len(kept)-1].Name != "Test0999" {
		t.Errorf("last kept passed case = %q, want the earliest passed cases kept", kept[len(kept)-1].Name)
	}
}

func TestBoundTestCasesKeepsFailedThenSkippedThenPassedInRunnerOrder(t *testing.T) {
	cases := make([]TestCase, 0, 1207)
	failedAt := map[int]bool{3: true, 250: true, 700: true, 1100: true, 1204: true}
	skippedAt := map[int]bool{10: true, 1150: true}
	for i := range 1207 {
		status := TestCaseStatusPassed
		switch {
		case failedAt[i]:
			status = TestCaseStatusFailed
		case skippedAt[i]:
			status = TestCaseStatusSkipped
		}
		cases = append(cases, TestCase{Name: fmt.Sprintf("Test%04d", i), Suite: "example.com/a", Status: status, DurationMs: 1})
	}
	kept, dropped := BoundTestCases(cases)
	if len(kept) != TestCaseMaxPerTask || dropped != 207 {
		t.Fatalf("1200 passed + 2 skipped + 5 failed: kept %d, dropped %d; want 1000 and 207", len(kept), dropped)
	}
	counts := map[string]int{}
	previous := -1
	for _, c := range kept {
		counts[c.Status]++
		var index int
		if _, err := fmt.Sscanf(c.Name, "Test%04d", &index); err != nil {
			t.Fatalf("unexpected case name %q", c.Name)
		}
		if index <= previous {
			t.Fatalf("kept cases lost runner order: %d after %d", index, previous)
		}
		previous = index
	}
	if counts[TestCaseStatusFailed] != 5 || counts[TestCaseStatusSkipped] != 2 || counts[TestCaseStatusPassed] != 993 {
		t.Errorf("kept statuses = %v, want 5 failed, 2 skipped, 993 passed", counts)
	}
	if kept[len(kept)-1].Name != "Test1204" {
		t.Errorf("last kept case = %q, want the last failed case Test1204", kept[len(kept)-1].Name)
	}
}

func TestBoundTestCasesRemovesPassedOutputAndOrphanMembers(t *testing.T) {
	kept, dropped := BoundTestCases([]TestCase{
		{Name: "TestPass", Suite: "example.com/a", Status: TestCaseStatusPassed, DurationMs: -5, Output: "ok", OutputTruncated: true, File: "a_test.go", Line: 3},
		{Name: "TestFail", Suite: "example.com/a", Status: TestCaseStatusFailed, OutputTruncated: true, Line: 9},
		{Name: "TestSkip", Suite: "example.com/a", Status: TestCaseStatusSkipped, Output: "no network", File: "a_test.go", Line: -1},
	})
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	want := []TestCase{
		{Name: "TestPass", Suite: "example.com/a", Status: TestCaseStatusPassed, File: "a_test.go", Line: 3},
		{Name: "TestFail", Suite: "example.com/a", Status: TestCaseStatusFailed},
		{Name: "TestSkip", Suite: "example.com/a", Status: TestCaseStatusSkipped, Output: "no network", File: "a_test.go"},
	}
	if !reflect.DeepEqual(kept, want) {
		t.Errorf("kept =\n%#v\nwant\n%#v", kept, want)
	}
}

func TestBoundTestCasesCutsTextOnCodePoints(t *testing.T) {
	long := strings.Repeat("é", TestCaseMaxTextBytes) // two bytes each
	kept, _ := BoundTestCases([]TestCase{{Name: "x" + long, Suite: long, File: long, Status: TestCaseStatusPassed}})
	if len(kept) != 1 {
		t.Fatalf("kept %d cases, want 1", len(kept))
	}
	for member, value := range map[string]string{"name": kept[0].Name, "suite": kept[0].Suite, "file": kept[0].File} {
		if len(value) > TestCaseMaxTextBytes || !utf8.ValidString(value) {
			t.Errorf("%s: %d bytes, valid UTF-8 %v; want at most %d valid bytes", member, len(value), utf8.ValidString(value), TestCaseMaxTextBytes)
		}
	}
	if want := "x" + strings.Repeat("é", (TestCaseMaxTextBytes-1)/2); kept[0].Name != want {
		t.Errorf("name kept %d bytes, want the longest code-point prefix (%d bytes)", len(kept[0].Name), len(want))
	}
}

func TestBoundTestCasesRepairsInvalidUTF8(t *testing.T) {
	kept, dropped := BoundTestCases([]TestCase{{
		Name:   "Test\xffBad",
		Suite:  "example.com/\xfe",
		Status: TestCaseStatusFailed,
		Output: "got \xc3\x28",
		File:   "a\xff_test.go",
		Line:   1,
	}})
	if dropped != 0 || len(kept) != 1 {
		t.Fatalf("kept %d, dropped %d; want one repaired case", len(kept), dropped)
	}
	c := kept[0]
	if c.Name != "Test�Bad" || c.Suite != "example.com/�" || c.Output != "got �(" || c.File != "a�_test.go" {
		t.Errorf("repaired case = %#v", c)
	}
}

func TestTruncateTestCaseOutputKeepsHeadAndTailOnCodePoints(t *testing.T) {
	if got, truncated := TruncateTestCaseOutput("short"); got != "short" || truncated {
		t.Errorf("short output = %q, %v; want unchanged", got, truncated)
	}
	exact := strings.Repeat("a", TestCaseMaxOutputBytes)
	if got, truncated := TruncateTestCaseOutput(exact); got != exact || truncated {
		t.Errorf("output of exactly %d bytes was cut", TestCaseMaxOutputBytes)
	}

	// Three-byte runes put a code-point boundary off every byte offset the cut
	// computes, so both the head and the tail must step back to one.
	output := "HEA" + strings.Repeat("€", 3000) + "TAIL"
	got, truncated := TruncateTestCaseOutput(output)
	if !truncated {
		t.Fatal("truncated = false for an output past the bound")
	}
	if len(got) > TestCaseMaxOutputBytes || !utf8.ValidString(got) {
		t.Fatalf("truncated output is %d bytes, valid UTF-8 %v; want at most %d valid bytes", len(got), utf8.ValidString(got), TestCaseMaxOutputBytes)
	}
	head, tail, found := strings.Cut(got, testCaseOutputCut)
	if !found {
		t.Fatalf("truncated output carries no cut marker: %q", got[:64])
	}
	if !strings.HasPrefix(output, head) || len(head) > testCaseOutputHeadBytes || len(head) < testCaseOutputHeadBytes-3 {
		t.Errorf("head is %d bytes, want the longest code-point prefix within %d", len(head), testCaseOutputHeadBytes)
	}
	if !strings.HasSuffix(output, tail) || !strings.HasSuffix(tail, "TAIL") {
		t.Errorf("tail is not a suffix of the output ending in TAIL")
	}
	if room := TestCaseMaxOutputBytes - len(head) - len(testCaseOutputCut); len(tail) > room || len(tail) < room-3 {
		t.Errorf("tail is %d bytes, want the longest code-point suffix within %d", len(tail), room)
	}
}

// Every cut lands on a code point boundary and keeps the longest prefix or
// suffix within the limit, whatever the limit.
func TestUTF8CutsKeepTheLongestWholeCodePoints(t *testing.T) {
	s := "a€😀é"
	for limit := 0; limit <= len(s)+1; limit++ {
		prefix, suffix := truncateUTF8(s, limit), lastUTF8(s, limit)
		for name, cut := range map[string]string{"prefix": prefix, "suffix": suffix} {
			if len(cut) > limit || !utf8.ValidString(cut) {
				t.Errorf("limit %d: %s %q is %d bytes, valid UTF-8 %v", limit, name, cut, len(cut), utf8.ValidString(cut))
			}
		}
		if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
			t.Errorf("limit %d: %q / %q is not a prefix / suffix of %q", limit, prefix, suffix, s)
		}
		if next := s[len(prefix):]; next != "" && len(prefix)+utf8.RuneLen([]rune(next)[0]) <= limit {
			t.Errorf("limit %d: prefix %q stops before a code point that fits", limit, prefix)
		}
		if rest := s[:len(s)-len(suffix)]; rest != "" {
			last, _ := utf8.DecodeLastRuneInString(rest)
			if len(suffix)+utf8.RuneLen(last) <= limit {
				t.Errorf("limit %d: suffix %q stops after a code point that fits", limit, suffix)
			}
		}
	}
}

func TestTruncateTestCaseOutputStaysBoundedThroughSanitization(t *testing.T) {
	// Sanitization turns each one-byte control into a three-byte U+FFFD, so an
	// output bounded before sanitization would outgrow the bound after it.
	output := strings.Repeat("\x00", TestCaseMaxOutputBytes)
	got, truncated := TruncateTestCaseOutput(output)
	if !truncated || len(got) > TestCaseMaxOutputBytes {
		t.Fatalf("output = %d bytes, truncated %v; want at most %d and truncated", len(got), truncated, TestCaseMaxOutputBytes)
	}
	if again := SanitizeMachineOutputString(got); again != got {
		t.Errorf("the bounded output is not a fixed point of the sanitizer")
	}
}

func TestTruncateTestCaseOutputRedactsASecretTheCutWouldSplit(t *testing.T) {
	secret := "gh" + "p_" + strings.Repeat("a", 40)
	// The head keeps the first 1024 bytes, so the cut falls inside the token.
	output := strings.Repeat("x", testCaseOutputHeadBytes-10) + secret + strings.Repeat("y", TestCaseMaxOutputBytes)
	got, _ := TruncateTestCaseOutput(output)
	if strings.Contains(got, "gh"+"p_") {
		t.Errorf("part of a secret survived the cut: %q", got[testCaseOutputHeadBytes-16:testCaseOutputHeadBytes+8])
	}
}

// TestBoundTestCasesOutputValidatesAsRecords pins the producer helper to the
// validators: every case BoundTestCases keeps, once the CLI sanitizes its
// record, is a valid test:case record.
func TestBoundTestCasesOutputValidatesAsRecords(t *testing.T) {
	long := strings.Repeat("\x01é€", TestCaseMaxOutputBytes)
	kept, _ := BoundTestCases([]TestCase{
		{Name: "TestPass", Suite: "example.com/a", Status: TestCaseStatusPassed, Output: "ok", OutputTruncated: true, Line: 4},
		{Name: long, Suite: long, Status: TestCaseStatusFailed, DurationMs: -1, Output: long, File: long, Line: 12},
		{Name: "TestSkip", Suite: "example.com/a", Status: TestCaseStatusSkipped, OutputTruncated: true, File: "a_test.go"},
		{Name: "TestTerm", Suite: "example.com/a", Status: TestCaseStatusFailed, Output: "\x1b[31mred\x1b[0m\x00\u0085", Line: 7},
	})
	if len(kept) != 4 {
		t.Fatalf("kept %d cases, want 4", len(kept))
	}
	identity := TaskIdentity{
		Key:      "/example:test",
		Scope:    TaskScopeProject,
		Project:  ProjectIdentity{ID: "/example", Name: "example"},
		Task:     TaskRef{Name: "test", Command: "test", Kind: "go-test"},
		Provider: ProviderIdentity{Extension: "@putnami/go"},
	}
	for i := range kept {
		record := SessionStreamRecord{
			ProtocolVersion: ResultProtocolVersion,
			Record:          RecordTestCase,
			Time:            "2026-09-28T10:00:00Z",
			Identity:        &identity,
			TestCase:        &kept[i],
		}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		sanitized, err := json.Marshal(SanitizeMachineOutputValue(decoded))
		if err != nil {
			t.Fatal(err)
		}
		if violations := ValidateDocument(DocumentSessionStreamRecord, sanitized); len(violations) != 0 {
			t.Errorf("case %d (%s) does not validate after sanitization: %v", i, kept[i].Status, violations)
		}
	}
}

// The validators enforce the byte bounds, not the schema's code-point
// maxLength: a two-byte character at the bound is accepted and one more byte is
// rejected, on each bounded member.
func TestValidateDocumentEnforcesTestCaseByteBounds(t *testing.T) {
	bounds := map[string]int{
		"name":   TestCaseMaxTextBytes,
		"suite":  TestCaseMaxTextBytes,
		"file":   TestCaseMaxTextBytes,
		"output": TestCaseMaxOutputBytes,
	}
	for member, bound := range bounds {
		t.Run(member, func(t *testing.T) {
			atBound := strings.Repeat("é", bound/2)
			if got := ValidateDocument(DocumentSessionStreamRecord, testCaseRecordWith(t, member, atBound)); len(got) != 0 {
				t.Errorf("%d bytes: violations %v, want none", len(atBound), got)
			}
			overBound := atBound + "x"
			want := []Violation{{Code: ViolationInvalidValue, Path: "testCase." + member}}
			if got := ValidateDocument(DocumentSessionStreamRecord, testCaseRecordWith(t, member, overBound)); !reflect.DeepEqual(got, want) {
				t.Errorf("%d bytes: violations %v, want %v", len(overBound), got, want)
			}
		})
	}
}

// testCaseRecordWith returns a failed test:case record whose member holds
// value.
func testCaseRecordWith(t *testing.T, member, value string) []byte {
	t.Helper()
	testCase := map[string]any{
		"name":       "TestA",
		"suite":      "example.com/a",
		"status":     TestCaseStatusFailed,
		"durationMs": 1,
		"file":       "a_test.go",
	}
	testCase[member] = value
	data, err := json.Marshal(map[string]any{
		"protocolVersion": ResultProtocolVersion,
		"record":          RecordTestCase,
		"time":            "2026-09-28T10:00:00Z",
		"identity": map[string]any{
			"key":      "/example:test",
			"scope":    TaskScopeProject,
			"project":  map[string]any{"id": "/example", "name": "example"},
			"task":     map[string]any{"name": "test", "command": "test", "kind": "go-test"},
			"provider": map[string]any{"extension": "@putnami/go"},
		},
		"testCase": testCase,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testCaseNames(cases []TestCase) []string {
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.Name)
	}
	return names
}

func TestBoundTestCasesWithinKeepsFailedCasesInsideTheByteBudget(t *testing.T) {
	failed := func(name string) TestCase {
		return TestCase{Name: name, Suite: "example.com/a", Status: TestCaseStatusFailed, Output: strings.Repeat("x", 3000)}
	}
	passed := TestCase{Name: "TestPass", Suite: "example.com/a", Status: TestCaseStatusPassed}
	cases := []TestCase{passed, failed("TestFail1"), failed("TestFail2"), failed("TestFail3")}
	size := encodedTestCaseBytes(failed("TestFail1"))

	// Room for two failed cases and the passed one, not for the third failed
	// case: a case that does not fit is skipped, and a smaller one still fits.
	budget := 2*size + encodedTestCaseBytes(passed) + size/2
	kept, dropped := BoundTestCasesWithin(cases, budget)
	names := make([]string, 0, len(kept))
	total := 0
	for _, c := range kept {
		names = append(names, c.Name)
		total += encodedTestCaseBytes(c)
	}
	if want := []string{"TestPass", "TestFail1", "TestFail2"}; !reflect.DeepEqual(names, want) || dropped != 1 {
		t.Fatalf("kept %v, dropped %d; want %v and 1 dropped", names, dropped, want)
	}
	if total > budget {
		t.Fatalf("kept %d bytes, want at most %d", total, budget)
	}
}

func TestBoundTestCasesWithinNeverExceedsTheTaskBudget(t *testing.T) {
	cases := make([]TestCase, 0, 400)
	for i := range 400 {
		cases = append(cases, TestCase{
			Name: fmt.Sprintf("TestFail%03d", i), Suite: "example.com/a", Status: TestCaseStatusFailed,
			Output: strings.Repeat("<", TestCaseMaxOutputBytes),
		})
	}
	kept, dropped := BoundTestCasesWithin(cases, 2*TestCaseMaxBytesPerTask)
	total := 0
	for _, c := range kept {
		total += encodedTestCaseBytes(c)
	}
	if total > TestCaseMaxBytesPerTask || len(kept)+dropped != len(cases) || dropped == 0 {
		t.Fatalf("kept %d cases of %d bytes, dropped %d; want at most %d bytes and the rest dropped",
			len(kept), total, dropped, TestCaseMaxBytesPerTask)
	}
}

func TestTestCaseBatchMemberBytesSharesTheBatchBudget(t *testing.T) {
	for _, tc := range []struct {
		members int
		want    int
	}{
		{0, TestCaseMaxBytesPerTask},
		{1, TestCaseMaxBytesPerTask},
		{8, TestCaseMaxBytesPerTask},
		{16, TestCaseMaxBytesPerBatch / 16},
		{2048, 4096},
	} {
		if got := TestCaseBatchMemberBytes(tc.members); got != tc.want {
			t.Errorf("TestCaseBatchMemberBytes(%d) = %d, want %d", tc.members, got, tc.want)
		}
	}
}
