package clitest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	runner "go.putnami.dev/protocol/runner"
)

// The supervisor writes its last output and its terminal state while follow
// answers. The client stops following on a terminal answer that is not full,
// so a client that follows the attempt must still receive every line.
func TestFixtureFollowKeepsTheOutputWrittenBeforeATerminalState(t *testing.T) {
	p := &fixtureProvider{root: t.TempDir()}
	dir := p.attemptDir("attempt")
	writeJSON := func(name string, value any) {
		t.Helper()
		if err := writeFixtureJSON(filepath.Join(dir, name), value); err != nil {
			t.Fatal(err)
		}
	}
	appendLine := func(cursor int64, line string) {
		t.Helper()
		record, err := json.Marshal(runner.OutputRecord{Cursor: cursor, Stream: "stdout", Line: line})
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(filepath.Join(dir, FixtureOutputFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = file.Close() }()
		if _, err := file.Write(append(record, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(fixtureMetaFile, fixtureMeta{Key: "key"})
	writeJSON(fixtureStateFile, FixtureState{State: runner.StateRunning})
	appendLine(1, "{")

	// The supervisor finishes between the two reads of the first follow.
	finished := false
	betweenFollowReads = func() {
		if finished {
			return
		}
		finished = true
		appendLine(2, "}")
		exitCode := 0
		writeJSON(fixtureStateFile, FixtureState{State: runner.StateCompleted, ExitCode: &exitCode})
	}
	t.Cleanup(func() { betweenFollowReads = nil })

	var lines []string
	cursor, ended := int64(0), false
	for range 5 {
		answer, err := p.follow(runner.FollowParams{Attempt: "attempt", Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		result, ok := answer.(runner.FollowResult)
		if !ok {
			t.Fatalf("follow answered %T", answer)
		}
		for _, record := range result.Records {
			lines = append(lines, record.Line)
		}
		cursor = result.Cursor
		if runner.TerminalState(result.State) && len(result.Records) < runner.MaxFollowRecords {
			ended = true
			break
		}
	}
	if !ended {
		t.Fatal("follow never answered the terminal state")
	}
	if want := []string{"{", "}"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("a client following the attempt received %q, want %q", lines, want)
	}
}
