package test

import (
	"encoding/json"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// event renders one `go test -json` event line.
func event(action, pkg, test, output string) string {
	raw, _ := json.Marshal(map[string]string{"Action": action, "Package": pkg, "Test": test, "Output": output})
	return string(raw)
}

// The suites a transcript skipped with the test provider's marker are named
// once each, across the transcripts of a batch, in one line; other skips and
// passing suites are not.
func TestSkippedSuitesWithoutATestDatabaseAreSummarized(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-database-binding", "suites-skipped-without-a-test-database-are-summarized")
	first := strings.Join([]string{
		event("run", "example.com/a", "TestRepo", ""),
		event("output", "example.com/a", "TestRepo", "    repo_test.go:12: "+noTestDatabaseMarker+" default=svc@127.0.0.1:5432/svc did not answer\n"),
		event("skip", "example.com/a", "TestRepo", ""),
		event("output", "example.com/a", "TestShort", "    short_test.go:3: skipped in short mode\n"),
		event("output", "example.com/a", "", "ok  \texample.com/a\n"),
		"not json, but mentions no test database",
	}, "\n")
	second := strings.Join([]string{
		event("output", "example.com/b", "TestStore/insert", "    store_test.go:9: "+noTestDatabaseMarker+" (no DATABASE_TEST_BINDINGS is set)\n"),
		event("output", "example.com/a", "TestRepo", "    repo_test.go:12: "+noTestDatabaseMarker+" again\n"),
	}, "\n")

	names := skippedWithoutTestDatabase(first, second)
	want := []string{"example.com/a.TestRepo", "example.com/b.TestStore/insert"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("skipped = %v, want %v", names, want)
	}
	line := skippedSuitesLine(names)
	if !strings.HasPrefix(line, "2 suite(s) skipped") || !strings.Contains(line, "example.com/a.TestRepo, example.com/b.TestStore/insert.") {
		t.Errorf("line = %q", line)
	}
	if skippedSuitesLine(nil) != "" {
		t.Error("a run that skipped nothing has a line")
	}
}

// A long list names the first suites and counts the rest.
func TestSkippedSuitesLineIsBounded(t *testing.T) {
	var names []string
	for i := range 12 {
		names = append(names, "p.Test"+string(rune('A'+i)))
	}
	line := skippedSuitesLine(names)
	if !strings.HasPrefix(line, "12 suite(s)") || !strings.Contains(line, "p.TestJ and 2 more.") || strings.Contains(line, "p.TestK") {
		t.Errorf("line = %q", line)
	}
}
