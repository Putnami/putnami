package reportstream

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// verboseSession is a real `putnami test --verbose --output=jsonl` capture
// whose stdout was byte-identical to its session's events.jsonl (README), so
// it serves as both copies of one session.
const (
	verboseSessionFixture = "verbose-session.jsonl"
	verboseSessionID      = "20261027-231809-0e209a"
)

func readVerboseSession(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", verboseSessionFixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// sessionWorkspace returns a workspace root whose session id holds artifact as
// its events.jsonl.
func sessionWorkspace(t *testing.T, id string, artifact []byte) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".putnami", "sessions", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), artifact, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// liveStream feeds stdout to a parser in chunks that do not align with lines,
// as the wrapper's tee does.
func liveStream(stdout []byte) *Parser {
	p := NewParser()
	const chunk = 509
	for i := 0; i < len(stdout); i += chunk {
		_, _ = p.Write(stdout[i:min(i+chunk, len(stdout))])
	}
	return p
}

func splitLines(data []byte) [][]byte {
	return bytes.SplitAfter(bytes.TrimRight(data, "\n"), []byte("\n"))
}

// joinLines rebuilds a stream from lines, each ending with exactly one LF.
func joinLines(lines [][]byte) []byte {
	var out bytes.Buffer
	for _, line := range lines {
		out.Write(bytes.TrimRight(line, "\n"))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// withFinalLine replaces the last line of a stream.
func withFinalLine(data []byte, final string) []byte {
	lines := splitLines(data)
	lines[len(lines)-1] = []byte(final)
	return joinLines(lines)
}

func finalLine(data []byte) string {
	lines := splitLines(data)
	return strings.TrimRight(string(lines[len(lines)-1]), "\n")
}

func assertSameRows(t *testing.T, got, want *Batch) {
	t.Helper()
	if !reflect.DeepEqual(got.Tests, want.Tests) {
		t.Fatalf("tests = %+v, want %+v", got.Tests, want.Tests)
	}
	if !reflect.DeepEqual(got.Coverage, want.Coverage) {
		t.Fatalf("coverage = %+v, want %+v", got.Coverage, want.Coverage)
	}
	if !reflect.DeepEqual(got.Builds, want.Builds) {
		t.Fatalf("builds = %+v, want %+v", got.Builds, want.Builds)
	}
}

// When stdout elided nothing, the session artifact folds to the same batch the
// stdout does: same rows, same stream stats.
func TestFoldRunArtifactEqualsStdoutWhenNothingIsElided(t *testing.T) {
	session := readVerboseSession(t)
	stdoutOnly := liveStream(session).Batch()

	batch, note := FoldRun(sessionWorkspace(t, verboseSessionID, session), liveStream(session))
	if note != "" {
		t.Fatalf("note = %q, want the session artifact used", note)
	}
	if batch.Stats.Source != SourceSessionArtifact {
		t.Fatalf("source = %q, want %q", batch.Stats.Source, SourceSessionArtifact)
	}
	assertSameRows(t, batch, stdoutOnly)
	stats := batch.Stats
	stats.Source = ""
	if stats != stdoutOnly.Stats {
		t.Fatalf("artifact stats = %+v, want the stdout's %+v", stats, stdoutOnly.Stats)
	}
	if batch.Tests == nil || len(batch.Tests.Projects) != 1 || batch.Tests.Projects[0].Project != "go.putnami.dev/protocol/gomod" ||
		batch.Tests.Projects[0].Passed != 20 || batch.Tests.Projects[0].Total != 20 {
		t.Fatalf("tests = %+v, want the capture's 20 passing tests", batch.Tests)
	}
	if batch.Coverage == nil || len(batch.Coverage.Projects) != 1 || batch.Coverage.Projects[0].Percentage != 95.89 {
		t.Fatalf("coverage = %+v, want the capture's 95.89%%", batch.Coverage)
	}
}

// withoutTaskEvents is the session as a bounded stdout that elided every
// task:event and test:case record: it keeps the session's final line.
func withoutTaskEvents(t *testing.T, session []byte) []byte {
	t.Helper()
	var kept [][]byte
	for _, line := range splitLines(session) {
		if bytes.Contains(line, []byte(`"record":"test:case"`)) || bytes.Contains(line, []byte(`"record":"task:event"`)) {
			continue
		}
		kept = append(kept, line)
	}
	elided := joinLines(kept)
	if finalLine(elided) != finalLine(session) {
		t.Fatal("the elided copy must keep the session's final line")
	}
	return elided
}

// beforeFinalLine inserts lines just before a stream's final line.
func beforeFinalLine(data []byte, inserted ...[]byte) []byte {
	lines := splitLines(data)
	out := append([][]byte{}, lines[:len(lines)-1]...)
	out = append(out, inserted...)
	return joinLines(append(out, lines[len(lines)-1]))
}

// A bounded stdout that elided the task events loses the tests and coverage
// rows. The session artifact still holds them, and FoldRun folds them from it.
func TestFoldRunRecoversRecordsElidedFromStdout(t *testing.T) {
	session := readVerboseSession(t)
	elided := withoutTaskEvents(t, session)
	if stdoutOnly := liveStream(elided).Batch(); stdoutOnly.Tests != nil || stdoutOnly.Coverage != nil {
		t.Fatalf("the elided stdout still folds tests %+v and coverage %+v", stdoutOnly.Tests, stdoutOnly.Coverage)
	}

	batch, note := FoldRun(sessionWorkspace(t, verboseSessionID, session), liveStream(elided))
	if note != "" || batch.Stats.Source != SourceSessionArtifact || batch.Stats.ElidedLines != 0 {
		t.Fatalf("note = %q, stats = %+v, want the complete session artifact", note, batch.Stats)
	}
	assertSameRows(t, batch, liveStream(session).Batch())
}

// Every case where the artifact is absent, refused or unverifiable folds the
// stdout exactly as a bare parser does, and says why.
func TestFoldRunFallsBackToStdout(t *testing.T) {
	session := readVerboseSession(t)
	final := finalLine(session)
	lines := splitLines(session)
	idMember := `"sessionId":"` + verboseSessionID + `"`

	for _, tc := range []struct {
		name     string
		stdout   []byte
		artifact []byte
		// workspace builds the root; nil uses a workspace holding artifact.
		workspace func(t *testing.T) string
		want      string
	}{
		{
			name:     "no session:end",
			stdout:   joinLines(lines[:len(lines)-1]),
			artifact: session,
			want:     "the output has no v2 session:end record",
		},
		{
			name:     "two sessions",
			stdout:   append(append([]byte{}, session...), session...),
			artifact: session,
			want:     "the output holds 2 sessions",
		},
		{
			name:     "an unbounded session:end",
			stdout:   withFinalLine(session, `{"protocolVersion":2,"record":"session:end","run":{"outcome":"success","exitCode":0}}`),
			artifact: session,
			want:     "its session:end names no session record",
		},
		{
			name:     "a session id that climbs out",
			stdout:   withFinalLine(session, strings.Replace(final, idMember, `"sessionId":"../escape"`, 1)),
			artifact: session,
			want:     `invalid session id "../escape"`,
		},
		{
			name:     "a session id with a separator",
			stdout:   withFinalLine(session, strings.Replace(final, idMember, `"sessionId":"a/b"`, 1)),
			artifact: session,
			want:     `invalid session id "a/b"`,
		},
		{
			name:     "an artifact path other than events.jsonl",
			stdout:   withFinalLine(session, strings.Replace(final, `"path":"events.jsonl"`, `"path":"../../etc/passwd"`, 1)),
			artifact: session,
			want:     `names "../../etc/passwd", not events.jsonl`,
		},
		{
			name:   "no session record on disk",
			stdout: session,
			workspace: func(t *testing.T) string {
				return t.TempDir()
			},
			want: "the session records cannot be read",
		},
		{
			name:   "no artifact in the session",
			stdout: session,
			workspace: func(t *testing.T) string {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, ".putnami", "sessions", verboseSessionID), 0o755); err != nil {
					t.Fatal(err)
				}
				return root
			},
			want: "the session record cannot be read",
		},
		{
			name:   "a session directory linked outside the sessions root",
			stdout: session,
			workspace: func(t *testing.T) string {
				root := t.TempDir()
				outside := t.TempDir()
				if err := os.WriteFile(filepath.Join(outside, "events.jsonl"), session, 0o644); err != nil {
					t.Fatal(err)
				}
				sessions := filepath.Join(root, ".putnami", "sessions")
				if err := os.MkdirAll(sessions, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(sessions, verboseSessionID)); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				return root
			},
			want: "resolves outside",
		},
		{
			name:   "an artifact linked to another file",
			stdout: session,
			workspace: func(t *testing.T) string {
				root := sessionWorkspace(t, "other", session)
				dir := filepath.Join(root, ".putnami", "sessions", verboseSessionID)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(root, ".putnami", "sessions", "other", "events.jsonl")
				if err := os.Symlink(target, filepath.Join(dir, "events.jsonl")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				return root
			},
			want: "resolves outside",
		},
		{
			name:     "another session's artifact",
			stdout:   session,
			artifact: withFinalLine(session, strings.Replace(final, `"durationMs":52550`, `"durationMs":1`, 1)),
			want:     "does not end with this run's session:end",
		},
		{
			name:     "an artifact that goes on after its session:end",
			stdout:   session,
			artifact: append(append([]byte{}, session...), lines[1]...),
			want:     "does not end with this run's session:end",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var root string
			if tc.workspace != nil {
				root = tc.workspace(t)
			} else {
				root = sessionWorkspace(t, verboseSessionID, tc.artifact)
			}
			batch, note := FoldRun(root, liveStream(tc.stdout))
			if !strings.Contains(note, tc.want) {
				t.Fatalf("note = %q, want it to contain %q", note, tc.want)
			}
			if batch.Stats.Source != SourceStdout {
				t.Fatalf("source = %q, want %q", batch.Stats.Source, SourceStdout)
			}
			plain := liveStream(tc.stdout).Batch()
			assertSameRows(t, batch, plain)
			stats := batch.Stats
			stats.Source = ""
			if stats != plain.Stats {
				t.Fatalf("stats = %+v, want the bare stdout fold's %+v", stats, plain.Stats)
			}
		})
	}
}

// The stdout fold reports the elision its bounded session:end records, so the
// ingest sees a stream that is not complete.
func TestFoldRunStdoutReportsElidedRecords(t *testing.T) {
	session := readVerboseSession(t)
	final := strings.Replace(finalLine(session),
		`"elided":{"ordinary":{"records":0,"bytes":0},"failure":{"records":0,"bytes":0}}`,
		`"elided":{"ordinary":{"records":5,"bytes":900},"failure":{"records":1,"bytes":100}}`, 1)
	batch, note := FoldRun(t.TempDir(), liveStream(withFinalLine(session, final)))
	if note == "" || batch.Stats.Source != SourceStdout || batch.Stats.ElidedLines != 6 {
		t.Fatalf("note = %q, stats = %+v, want the stdout fold with 6 elided records", note, batch.Stats)
	}
}

// A command that wrote no putnami JSONL has nothing to fall back from: no
// note, no reports.
func TestFoldRunIsQuietForANonPutnamiCommand(t *testing.T) {
	live := liveStream([]byte("ok  \tgo.example.com/acme\t1.2s\nPASS\n"))
	batch, note := FoldRun(t.TempDir(), live)
	if note != "" || batch.HasReports() {
		t.Fatalf("note = %q, reports = %t, want neither", note, batch.HasReports())
	}
}

// A line the parser cannot fold does not refuse the artifact: one session:end
// and the identical final line bind the file to the run. Such a line is valid
// JSON a subprocess shaped differently, or not JSON at all, and Stats counts
// it.
func TestFoldRunKeepsAnArtifactWithLinesItCannotDecode(t *testing.T) {
	session := readVerboseSession(t)
	const identity = `"identity":{"key":"/libs/a:test~test","scope":"project","project":{"id":"/libs/a","name":"libs/a"},` +
		`"task":{"name":"test~test","command":"test","kind":"test-exec"}}`
	artifact := beforeFinalLine(session,
		// A time a subprocess wrote that is not RFC 3339.
		[]byte(`{"protocolVersion":2,"record":"task:event","time":"yesterday",`+identity+`,"event":{"type":"log","message":"hi"}}`),
		// A count written as 12.0.
		[]byte(`{"protocolVersion":2,"record":"task:event","time":"2026-10-27T22:18:16Z",`+identity+
			`,"event":{"type":"result","data":{"testSummary":{"passed":12.0,"failed":0,"skipped":0,"total":12}}}}`),
		[]byte(`{not json`),
	)
	complete := liveStream(session).Batch()

	batch, note := FoldRun(sessionWorkspace(t, verboseSessionID, artifact), liveStream(withoutTaskEvents(t, session)))
	if note != "" || batch.Stats.Source != SourceSessionArtifact {
		t.Fatalf("note = %q, source = %q, want the session artifact", note, batch.Stats.Source)
	}
	if want := complete.Stats.MalformedLines + 3; batch.Stats.MalformedLines != want {
		t.Fatalf("malformed lines = %d, want %d", batch.Stats.MalformedLines, want)
	}
	if want := complete.Stats.Lines + 3; batch.Stats.Lines != want {
		t.Fatalf("lines = %d, want %d", batch.Stats.Lines, want)
	}
	assertSameRows(t, batch, complete)
}

// The artifact keeps the CLI's own line bound, MaxArtifactLineBytes, not the
// stdout cap: a result line over MaxLineBytes folds, and a line over
// MaxArtifactLineBytes is counted and skipped while the rest still folds.
func TestFoldRunKeepsTheLineCapOnTheArtifact(t *testing.T) {
	session := readVerboseSession(t)
	const identity = `"identity":{"key":"/libs/big:test~test","scope":"project","project":{"id":"/libs/big","name":"libs/big"},` +
		`"task":{"name":"test~test","command":"test","kind":"test-exec"}}`
	large := []byte(`{"protocolVersion":2,"record":"task:event","time":"2026-10-27T22:18:16Z",` + identity +
		`,"event":{"type":"result","message":"` + strings.Repeat("x", 2*MaxLineBytes) +
		`","data":{"testSummary":{"passed":3,"failed":0,"skipped":0,"total":3}}}}`)
	oversized := []byte(`{"protocolVersion":2,"record":"task:event","pad":"` + strings.Repeat("x", MaxArtifactLineBytes) + `"}`)
	artifact := beforeFinalLine(session, large, oversized)

	batch, note := FoldRun(sessionWorkspace(t, verboseSessionID, artifact), liveStream(session))
	if note != "" || batch.Stats.Source != SourceSessionArtifact {
		t.Fatalf("note = %q, source = %q, want the session artifact", note, batch.Stats.Source)
	}
	if batch.Stats.OversizedLines != 1 {
		t.Fatalf("oversized lines = %d, want 1", batch.Stats.OversizedLines)
	}
	want := liveStream(session).Batch()
	want.Tests.Projects = append(want.Tests.Projects, ProjectTests{Project: "libs/big", Passed: 3, Total: 3})
	assertSameRows(t, batch, want)
}
