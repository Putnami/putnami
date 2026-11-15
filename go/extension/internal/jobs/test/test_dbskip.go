package test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"go.putnami.dev/sdk/extension/jsonl"
)

// noTestDatabaseMarker starts the skip message of a suite whose test database
// did not answer. It is go.putnami.dev/database/testprovider.SkipMarker, held
// here as text because this extension does not depend on the framework; keep
// the two equal.
const noTestDatabaseMarker = "[putnami: no test database]"

// maxNamedSkippedSuites bounds how many suites the summary line names.
const maxNamedSkippedSuites = 10

// skippedWithoutTestDatabase returns the suites a `go test -json` transcript
// skipped because their test database did not answer, as `package.Test`,
// sorted and without duplicates.
func skippedWithoutTestDatabase(outputs ...string) []string {
	seen := make(map[string]bool)
	for _, output := range outputs {
		for line := range strings.SplitSeq(output, "\n") {
			if !strings.Contains(line, "no test database") {
				continue
			}
			var event struct {
				Action  string
				Package string
				Test    string
				Output  string
			}
			if json.Unmarshal([]byte(line), &event) != nil || event.Action != "output" || event.Test == "" ||
				!strings.Contains(event.Output, noTestDatabaseMarker) {
				continue
			}
			seen[event.Package+"."+event.Test] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// skippedSuitesLine is the one line that names the suites skipped without a
// test database, or "" when none was.
func skippedSuitesLine(names []string) string {
	if len(names) == 0 {
		return ""
	}
	shown := names
	more := ""
	if len(shown) > maxNamedSkippedSuites {
		shown = shown[:maxNamedSkippedSuites]
		more = fmt.Sprintf(" and %d more", len(names)-maxNamedSkippedSuites)
	}
	return fmt.Sprintf("%d suite(s) skipped because their test database did not answer, so their SQL did not run: %s%s. "+
		"Start the database, or pass --require-test-db to fail them.", len(names), strings.Join(shown, ", "), more)
}

// skippedSuitesCode is the diagnostic code of the summary line.
const skippedSuitesCode = "TEST_DATABASE_DID_NOT_ANSWER"

// emitSkippedWithoutTestDatabase reports once, as a warning diagnostic a
// plain run shows, the suites the transcripts skipped without a test database.
func emitSkippedWithoutTestDatabase(emit *jsonl.Emitter, outputs ...string) {
	if line := skippedSuitesLine(skippedWithoutTestDatabase(outputs...)); line != "" {
		emit.DiagnosticWithCode("warning", line, "", 0, 0, skippedSuitesCode)
	}
}
