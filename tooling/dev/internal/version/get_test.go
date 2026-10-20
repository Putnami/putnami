package version

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

// feature names the feature the version commands prove.
const feature = "tooling/version-commands"

// twoLines is a host answer with the root line, untagged, and the tooling
// line, tagged.
func twoLines() *fakeHost {
	return &fakeHost{result: wire.VersionLinesResult{
		SourceRevision: "0123456789abcdef0123456789abcdef01234567",
		Lines: []wire.VersionLine{
			{Line: "", TagPattern: "v{version}", Version: "0.2.1-20260101000000-0123456",
				Release: &wire.LineRelease{Version: "0.2.1", Tag: "v0.2.1"}, Commits: []wire.LineCommit{}},
			{Line: "tooling", TagPattern: "tooling/v{version}", Version: "1.4.0", Tagged: true, Tag: "tooling/v1.4.0",
				Commits: []wire.LineCommit{}},
		},
	}}
}

func TestGetPrintsEachLineTheHostAnswers(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "get-prints-every-line", "get-prints-each-line-the-host-answers")
	host := twoLines()
	got := runGet(t, t.TempDir(), host, protocolcli.OutputAuto)
	want := outcome{stdout: "  (workspace root) 0.2.1-20260101000000-0123456\n  tooling 1.4.0\n"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	for _, mode := range []protocolcli.OutputMode{protocolcli.OutputText, protocolcli.OutputCloudLogging} {
		if text := runGet(t, t.TempDir(), host, mode); text != want {
			t.Errorf("%s: got %+v, want the text %+v", mode, text, want)
		}
	}
	for _, args := range [][]string{{"--scope", "/tooling/"}, {"--scope=tooling"}} {
		runGet(t, t.TempDir(), host, protocolcli.OutputAuto, args...)
	}
	if asked := host.asked(); !slices.Equal(asked, []string{"", "", "", "tooling", "tooling"}) {
		t.Errorf("the host was asked for %q, want every line three times, then tooling without its slashes twice", asked)
	}
}

func TestGetPrintsOneEnvelopeUnderStructuredOutput(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "get-prints-every-line", "get-prints-one-envelope-under-structured-output")
	got := runGet(t, t.TempDir(), twoLines(), protocolcli.OutputJSONL)
	want := `{"protocolVersion":2,"command":"version get","status":"success","exitCode":0,"data":[` +
		`{"action":"get","line":"","version":"0.2.1-20260101000000-0123456"},` +
		`{"action":"get","line":"tooling","tag":"tooling/v1.4.0","tagged":true,"version":"1.4.0"}]}` + "\n"
	if got != (outcome{stdout: want}) {
		t.Errorf("jsonl got %+v, want stdout %s", got, want)
	}

	one := &fakeHost{result: wire.VersionLinesResult{SourceRevision: "0123", Lines: twoLines().result.Lines[1:]}}
	got = runGet(t, t.TempDir(), one, protocolcli.OutputJSON, "--scope", "tooling")
	want = "{\n" +
		"  \"protocolVersion\": 2,\n" +
		"  \"command\": \"version get\",\n" +
		"  \"status\": \"success\",\n" +
		"  \"exitCode\": 0,\n" +
		"  \"data\": {\n" +
		"    \"action\": \"get\",\n" +
		"    \"line\": \"tooling\",\n" +
		"    \"tag\": \"tooling/v1.4.0\",\n" +
		"    \"tagged\": true,\n" +
		"    \"version\": \"1.4.0\"\n" +
		"  }\n" +
		"}\n"
	if got != (outcome{stdout: want}) {
		t.Errorf("json got %+v, want stdout %s", got, want)
	}

	refused := &fakeHost{err: &wire.Error{Code: wire.CodeInvalidParams, Message: `no version line "nope"; this workspace declares tooling`}}
	got = runGet(t, t.TempDir(), refused, protocolcli.OutputJSONL, "--scope", "nope")
	want = `{"protocolVersion":2,"command":"version get","status":"failure","exitCode":2,` +
		`"error":{"code":"usage","message":"no version line \"nope\"; this workspace declares tooling"}}` + "\n"
	if got != (outcome{code: 2, stdout: want, stderr: "no version line \"nope\"; this workspace declares tooling\n"}) {
		t.Errorf("a refused scope got %+v, want the failure envelope", got)
	}
}

func TestGetAnswersFromARecordedGraphAndSaysSo(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "recorded-graph-reads-only", "get-answers-from-a-recorded-graph-and-says-so")
	host := twoLines()
	host.result.RecordedGraph = &wire.RecordedGraph{ObservedAt: "2026-01-02T03:04:05Z", RefreshError: "go list failed"}
	got := runGet(t, t.TempDir(), host, protocolcli.OutputAuto)
	want := outcome{
		stdout: "  (workspace root) 0.2.1-20260101000000-0123456\n  tooling 1.4.0\n",
		stderr: "version: the dependency graph could not be refreshed (go list failed); answering from the recorded view observed at 2026-01-02T03:04:05Z\n",
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "usage-and-host-errors", "usage-errors-exit-2")
	host := twoLines()
	for _, tc := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"extra"}, "usage: putnami version get [--scope <line>]\n"},
		{[]string{"--scope"}, "--scope requires a value\n"},
		{[]string{"--scope", "--push"}, "--scope requires a value\n"},
		{[]string{"--scope="}, "--scope requires a value\n"},
	} {
		if got := runGet(t, t.TempDir(), host, protocolcli.OutputAuto, tc.args...); got != (outcome{code: 2, stderr: tc.stderr}) {
			t.Errorf("get %v: got %+v, want exit 2 with %q", tc.args, got, tc.stderr)
		}
	}
	for _, tc := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"a", "b"}, tagUsage + "\n"},
		{[]string{"--scope=-x"}, "--scope requires a value\n"},
	} {
		if got := runTag(t, t.TempDir(), host, tc.args...); got != (outcome{code: 2, stderr: tc.stderr}) {
			t.Errorf("tag %v: got %+v, want exit 2 with %q", tc.args, got, tc.stderr)
		}
	}
	if asked := host.asked(); len(asked) != 0 {
		t.Errorf("a usage error asked the host for %q", asked)
	}
}

func TestHostRefusalsKeepTheirExitCodes(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "usage-and-host-errors", "host-refusals-keep-their-exit-codes")
	dial := errors.New("dial unix /tmp/host.sock: connect: connection refused")
	for _, tc := range []struct {
		name   string
		err    error
		code   int
		stderr string
	}{
		{"a scope that names no line", &wire.Error{Code: wire.CodeInvalidParams, Message: "no version line \"x\""}, 2, "no version line \"x\"\n"},
		{"an invalid configuration", &wire.Error{Code: wire.CodeInvalidConfig, Message: "version: the catalog is invalid"}, 2, "version: the catalog is invalid\n"},
		{"no workspace", &wire.Error{Code: wire.CodeUnavailable, Message: "outside any workspace"}, 2, "outside any workspace\n"},
		{"a failure of the host", &wire.Error{Code: wire.CodeInternalError, Message: "git fetch --unshallow --tags"}, 1, "git fetch --unshallow --tags\n"},
		{"another refusal", &wire.Error{Code: wire.CodeUnauthorized, Message: "refused"}, 1, "putnami host RPC error -32001: refused\n"},
		{"a broken channel", dial, 1, dial.Error() + "\n"},
	} {
		host := &fakeHost{err: tc.err}
		if got := runGet(t, t.TempDir(), host, protocolcli.OutputAuto); got != (outcome{code: tc.code, stderr: tc.stderr}) {
			t.Errorf("get, %s: got %+v, want exit %d with %q", tc.name, got, tc.code, tc.stderr)
		}
		if got := runTag(t, t.TempDir(), host); got != (outcome{code: tc.code, stderr: tc.stderr}) {
			t.Errorf("tag, %s: got %+v, want exit %d with %q", tc.name, got, tc.code, tc.stderr)
		}
	}
}

// failingWriter refuses every write, as a closed stdout does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout is closed") }

// TestGetReportsAnEnvelopeItCannotWrite pins that a result envelope the
// command cannot write is a failure, never a silent success.
func TestGetReportsAnEnvelopeItCannotWrite(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	command := Command{Root: t.TempDir(), Output: protocolcli.OutputJSON, Host: twoLines(), Stdout: failingWriter{}, Stderr: &stderr}
	if code := command.Get(t.Context(), nil); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if stderr.String() != "stdout is closed\n" {
		t.Errorf("stderr %q, want the write error", stderr.String())
	}
}
