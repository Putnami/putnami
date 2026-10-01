package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
)

// TestRunStructuredCommandWrapsLegacyPayloads exercises the dispatcher seam
// shared by the established read-only commands whose helpers historically
// wrote raw JSONL. The command output must now be one ResultV2 document in
// both structured modes, retaining each former JSON document in data.
func TestRunStructuredCommandWrapsLegacyPayloads(t *testing.T) {
	wsRoot := structuredOutputWorkspace(t)
	cfg := wsproto.Load(wsRoot)

	tests := []struct {
		name       string
		args       []string
		command    string
		assertData func(t *testing.T, data any)
	}{
		{
			name:    "projects list preserves all former JSONL rows",
			args:    []string{"projects", "list"},
			command: "projects list",
			assertData: func(t *testing.T, data any) {
				t.Helper()
				entries, ok := data.([]any)
				if !ok || len(entries) != 2 {
					t.Fatalf("projects data = %#v, want two preserved entries", data)
				}
			},
		},
		{
			name:    "config show preserves its object payload",
			args:    []string{"config", "show"},
			command: "config show",
			assertData: func(t *testing.T, data any) {
				t.Helper()
				if _, ok := data.(map[string]any); !ok {
					t.Fatalf("config data = %#v, want object payload", data)
				}
			},
		},
		{
			name:    "workspace describe preserves its object payload",
			args:    []string{"workspace", "describe"},
			command: "workspace describe",
			assertData: func(t *testing.T, data any) {
				t.Helper()
				workspace, ok := data.(map[string]any)
				if !ok || workspace["name"] != "structured-output" {
					t.Fatalf("workspace data = %#v, want described workspace", data)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var jsonResult protocolcli.ResultV2
			for _, format := range []string{"json", "jsonl"} {
				args := append(append([]string{}, tt.args...), "--output="+format)
				parsed := ParseArgs(args, cfg.Aliases, nil)
				if parsed.Err != nil {
					t.Fatalf("ParseArgs(%v): %v", args, parsed.Err)
				}

				app := &App{}
				code := ExitError
				out := captureStdout(t, func() {
					code = app.runStructuredCommand(context.Background(), parsed, cfg, wsRoot)
				})
				if code != ExitSuccess {
					t.Fatalf("%s exit code = %d, want %d", format, code, ExitSuccess)
				}

				var result protocolcli.ResultV2
				if err := json.Unmarshal([]byte(out), &result); err != nil {
					t.Fatalf("%s output is not one result envelope: %v\n%s", format, err, out)
				}
				if result.ProtocolVersion != protocolcli.ResultProtocolVersion ||
					result.Command != tt.command || result.Status != protocolcli.StatusSuccess ||
					result.ExitCode != ExitSuccess {
					t.Fatalf("%s result = %+v, want successful v2 %q envelope", format, result, tt.command)
				}
				if format == "jsonl" && strings.Count(strings.TrimSpace(out), "\n") != 0 {
					t.Fatalf("jsonl wrote more than one result document:\n%s", out)
				}
				tt.assertData(t, result.Data)

				if format == "json" {
					jsonResult = result
				} else if !structuredResultsEqual(jsonResult, result) {
					t.Fatalf("json and jsonl results differ:\njson:  %+v\njsonl: %+v", jsonResult, result)
				}
			}
		})
	}
}

func structuredResultsEqual(left, right protocolcli.ResultV2) bool {
	leftData, _ := json.Marshal(left)
	rightData, _ := json.Marshal(right)
	return bytes.Equal(leftData, rightData)
}

func TestCapturedResultEnvelopePreservesAnExistingV2Result(t *testing.T) {
	t.Parallel()
	want := protocolcli.NewResultV2("doctor", map[string]any{"findings": 0}, nil)
	var out bytes.Buffer
	if _, err := protocolcli.WriteResultV2(&out, protocolcli.OutputJSONL, want); err != nil {
		t.Fatalf("write source envelope: %v", err)
	}

	got, ok := capturedResultEnvelope(out.String())
	if !ok {
		t.Fatalf("capturedResultEnvelope rejected v2 envelope: %s", out.String())
	}
	if !structuredResultsEqual(want, got) {
		t.Fatalf("captured envelope = %+v, want %+v", got, want)
	}
}

// TestRunStructuredCommandForwardsARawContractDocument pins the ONE registered
// exception to the envelope (registerRawCommand): `report` writes a document a
// consumer binds to directly, so the dispatcher forwards its stdout untouched
// — while its failures still take the shared envelope, which is what keeps the
// exception to the success payload.
func TestRunStructuredCommandForwardsARawContractDocument(t *testing.T) {
	wsRoot := structuredOutputWorkspace(t)
	cfg := wsproto.Load(wsRoot)

	report := `{
  "protocolVersion": 2,
  "sessionId": "20260807-093000-aaaaaa",
  "startTime": "2026-08-07T09:30:00+02:00",
  "endTime": "2026-08-07T09:30:02+02:00",
  "origin": "cli",
  "enforceCoverage": false,
  "run": {"outcome": "success", "exitCode": 0, "counts": {"total": 0}, "reuse": {}, "durationMs": 2},
  "commands": [],
  "jobs": [],
  "elidedJobs": 0
}
`
	reportsDir := filepath.Join(wsRoot, ".putnami", "reports")
	if err := os.MkdirAll(reportsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reportsDir, "20260807-093000-aaaaaa.json"), []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}

	parsed := ParseArgs([]string{"report", "--output=json"}, cfg.Aliases, nil)
	if parsed.Err != nil {
		t.Fatalf("ParseArgs: %v", parsed.Err)
	}
	app := &App{}
	code := ExitError
	out := captureStdout(t, func() {
		code = app.runStructuredCommand(context.Background(), parsed, cfg, wsRoot)
	})
	if code != ExitSuccess {
		t.Fatalf("report exit code = %d, want %d", code, ExitSuccess)
	}
	if out != report {
		t.Errorf("report stdout is not the recorded document:\ngot:\n%s\nwant:\n%s", out, report)
	}

	// The failure path keeps the shared envelope: only the success payload is
	// exempt, so an agent still reads status/exitCode/error the same way.
	missing := ParseArgs([]string{"report", "--session", "20260807-999999-zzzzzz", "--output=json"}, cfg.Aliases, nil)
	if missing.Err != nil {
		t.Fatalf("ParseArgs: %v", missing.Err)
	}
	failure := captureStdout(t, func() {
		code = app.runStructuredCommand(context.Background(), missing, cfg, wsRoot)
	})
	if code != ExitError {
		t.Fatalf("missing report exit code = %d, want %d", code, ExitError)
	}
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal([]byte(failure), &envelope); err != nil {
		t.Fatalf("failure output is not one result envelope: %v\n%s", err, failure)
	}
	if envelope.Command != "report" || envelope.Status != protocolcli.StatusFailure {
		t.Errorf("failure envelope = %+v, want a failed report envelope", envelope)
	}
}

func structuredOutputWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, contents string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "putnami.workspace.json"), `{"name":"structured-output","includes":["apps/api","libs/shared"]}`)
	write(filepath.Join(root, "apps", "api", "putnami.json"), `{"name":"api","type":"application"}`)
	write(filepath.Join(root, "libs", "shared", "putnami.json"), `{"name":"shared","type":"library"}`)
	return root
}

// TestPrintCommandError verifies the human dispatch: it always writes the
// "putnami: <message>" line, and appends an indented "  Try: <next>" line only
// when the error carries a suggestion via protocolcli.WithNext.
func TestPrintCommandError(t *testing.T) {
	t.Parallel()
	t.Run("suggestion appended", func(t *testing.T) {
		var buf bytes.Buffer
		err := protocolcli.WithNext(cmderr.ErrNoWorkspace, "putnami workspace init")
		printCommandError(&buf, err)
		out := buf.String()
		if !strings.Contains(out, "putnami: no workspace found\n") {
			t.Errorf("missing standard error line; got:\n%s", out)
		}
		if !strings.Contains(out, "  Try: putnami workspace init\n") {
			t.Errorf("missing Try line; got:\n%s", out)
		}
	})

	t.Run("no suggestion no Try line", func(t *testing.T) {
		var buf bytes.Buffer
		printCommandError(&buf, errors.New("boom"))
		out := buf.String()
		if !strings.Contains(out, "putnami: boom\n") {
			t.Errorf("missing standard error line; got:\n%s", out)
		}
		if strings.Contains(out, "Try:") {
			t.Errorf("should not print a Try line without a suggestion; got:\n%s", out)
		}
	})
}

// TestWriteStructuredFailureHumanMode verifies that in a non-structured mode
// nothing is written to stdout and the human "putnami: <msg>" + "  Try: <next>"
// lines go to stderr, with the exit code derived from the error class.
func TestWriteStructuredFailureHumanMode(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "failure-presentation", "human-failure-output-is-prioritized-and-bounded")
	var out, errBuf bytes.Buffer
	err := protocolcli.WithNext(cmderr.ErrNoWorkspace, "putnami workspace init")

	code := writeStructuredFailure(&out, &errBuf, protocolcli.OutputText, "projects list", err)

	if out.Len() != 0 {
		t.Errorf("human mode wrote to stdout: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "putnami: no workspace found\n") {
		t.Errorf("stderr missing standard error line; got:\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "  Try: putnami workspace init\n") {
		t.Errorf("stderr missing Try line; got:\n%s", errBuf.String())
	}
	if code != protocolcli.ExitUsage {
		t.Errorf("code = %d, want %d", code, protocolcli.ExitUsage)
	}
}

// TestWriteStructuredFailureEmitsEnvelope verifies that in a machine-readable
// mode the failure travels as a VERSION-2 result envelope on stdout carrying
// error.next, so an agent gets the actionable next command programmatically —
// and the stderr Try line is suppressed to avoid duplicating the hint.
//
// The decode target is ResultV2 and protocolVersion is asserted: this seam emits
// every non-job command's failure, and decoding it into the v1 Result struct
// (which has no protocolVersion member) meant the test passed identically before
// and after B1a made the envelope versioned — the one fact a consumer switches
// on was the one fact nothing here checked.
func TestWriteStructuredFailureEmitsEnvelope(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	err := protocolcli.WithNext(cmderr.ErrNoWorkspace, "putnami workspace init")

	code := writeStructuredFailure(&out, &errBuf, protocolcli.OutputJSONL, "projects list", err)

	var result protocolcli.ResultV2
	if e := json.Unmarshal(out.Bytes(), &result); e != nil {
		t.Fatalf("stdout is not a JSON result envelope: %v\n%s", e, out.String())
	}
	if result.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d — an envelope without it is a version-1 document",
			result.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
	if result.Status != protocolcli.StatusFailure {
		t.Errorf("status = %q, want %q", result.Status, protocolcli.StatusFailure)
	}
	if result.Command != "projects list" {
		t.Errorf("command = %q, want %q", result.Command, "projects list")
	}
	if result.Error == nil {
		t.Fatal("result.Error is nil, want a populated failure error")
	}
	if result.Error.Next != "putnami workspace init" {
		t.Errorf("error.next = %q, want %q", result.Error.Next, "putnami workspace init")
	}
	if result.Error.Code != "usage" {
		t.Errorf("error.code = %q, want %q", result.Error.Code, "usage")
	}
	if code != protocolcli.ExitUsage {
		t.Errorf("code = %d, want %d", code, protocolcli.ExitUsage)
	}
	// The hint lives in the envelope, not duplicated as a stderr Try line.
	if strings.Contains(errBuf.String(), "Try:") {
		t.Errorf("structured mode should not print a stderr Try line; got:\n%s", errBuf.String())
	}
}

// TestWriteStructuredFailureReportsAnInterruptAsAborted pins v2's second settled
// decision at this seam: an interrupted command is reported DISTINCTLY, as
// status "aborted" with exit 130, instead of being folded into "failure" the way
// v1 did. Every non-job command's Ctrl-C reaches an agent through this one
// function, so the decision either holds here or holds nowhere outside job runs.
func TestWriteStructuredFailureReportsAnInterruptAsAborted(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "terminal-outcome", "an-interrupt-is-a-distinct-aborted-status")
	var out, errBuf bytes.Buffer

	code := writeStructuredFailure(&out, &errBuf, protocolcli.OutputJSONL, "deps install",
		protocolcli.ErrSignal)

	var result protocolcli.ResultV2
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not a JSON result envelope: %v\n%s", err, out.String())
	}
	if result.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d",
			result.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
	if result.Status != protocolcli.StatusAborted {
		t.Errorf("status = %q, want %q — an interrupt is not a failure in v2",
			result.Status, protocolcli.StatusAborted)
	}
	if result.ExitCode != protocolcli.ExitSignal || code != protocolcli.ExitSignal {
		t.Errorf("exitCode = %d / returned code = %d, want %d for both — the document and the "+
			"process must agree", result.ExitCode, code, protocolcli.ExitSignal)
	}
	if result.Error == nil || result.Error.Code != "signal" {
		t.Errorf("error = %+v, want the signal class", result.Error)
	}
}

// TestRequireWorkspaceSuggestsInit verifies the adopted builtin site attaches
// the recovery command while preserving the sentinel class and message, so
// errors.Is and existing message-based assertions stay green.
func TestRequireWorkspaceSuggestsInit(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "workspace-root", "a-missing-workspace-yields-an-actionable-init-error")
	err := (&CommandEnv{WsRoot: ""}).requireWorkspace()
	if !errors.Is(err, cmderr.ErrNoWorkspace) {
		t.Errorf("err = %v, want ErrNoWorkspace", err)
	}
	if err.Error() != cmderr.ErrNoWorkspace.Error() {
		t.Errorf("message = %q, want %q (unchanged)", err.Error(), cmderr.ErrNoWorkspace.Error())
	}
	if got := protocolcli.SuggestedNext(err); got != "putnami workspace init" {
		t.Errorf("SuggestedNext = %q, want %q", got, "putnami workspace init")
	}
}

// TestRejectStructuredIfUnsupported covers the guard that fails loudly on
// structured output (--output=json/jsonl, incl. --json) for subcommands that
// do not emit it, instead of letting the flag be silently ignored.
//
// We only assert the return contract here; the stderr message goes via
// the package's iox helper to os.Stderr and is straightforward to inspect
// manually with `putnami version tag --output=jsonl`.
func TestRejectStructuredIfUnsupported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		outputFormat string
		cmd          string
		sub          string
		wantRejected bool
		wantCode     int
	}{
		{
			name:         "empty format passes through",
			outputFormat: "",
			cmd:          "version",
			sub:          "get",
			wantRejected: false,
			wantCode:     ExitSuccess,
		},
		{
			name:         "jsonl supported for version get",
			outputFormat: "jsonl",
			cmd:          "version",
			sub:          "get",
			wantRejected: false,
			wantCode:     ExitSuccess,
		},
		{
			name:         "jsonl rejected for unsupported mutating subcommand",
			outputFormat: "jsonl",
			cmd:          "version",
			sub:          "tag",
			wantRejected: true,
			wantCode:     ExitUsage,
		},
		{
			name:         "supported default projects list",
			outputFormat: "jsonl",
			cmd:          "projects",
			sub:          "",
			wantRejected: false,
			wantCode:     ExitSuccess,
		},
		{
			name:         "supported explicit extension list",
			outputFormat: "jsonl",
			cmd:          "extensions",
			sub:          "list",
			wantRejected: false,
			wantCode:     ExitSuccess,
		},
		{
			name:         "supported telemetry show",
			outputFormat: "jsonl",
			cmd:          "telemetry",
			sub:          "show",
			wantRejected: false,
			wantCode:     ExitSuccess,
		},
		{
			name:         "supported default extension install",
			outputFormat: "jsonl",
			cmd:          "extensions",
			sub:          "",
			wantRejected: false,
			wantCode:     ExitSuccess,
		},
		{
			name:         "unsupported extension remove",
			outputFormat: "jsonl",
			cmd:          "extensions",
			sub:          "remove",
			wantRejected: true,
			wantCode:     ExitUsage,
		},
		{
			name:         "supported template update",
			outputFormat: "jsonl",
			cmd:          "templates",
			sub:          "update",
			wantRejected: false,
			wantCode:     ExitSuccess,
		},
		{
			name:         "unsupported context generate",
			outputFormat: "jsonl",
			cmd:          "context",
			sub:          "generate",
			wantRejected: true,
			wantCode:     ExitUsage,
		},
		{
			name:         "unsupported install",
			outputFormat: "jsonl",
			cmd:          "install",
			sub:          "",
			wantRejected: true,
			wantCode:     ExitUsage,
		},
		{
			name:         "unsupported upgrade",
			outputFormat: "jsonl",
			cmd:          "upgrade",
			sub:          "",
			wantRejected: true,
			wantCode:     ExitUsage,
		},
		{
			name:         "non-structured format passes through",
			outputFormat: "cloud-logging",
			cmd:          "upgrade",
			sub:          "",
			wantRejected: false,
			wantCode:     ExitSuccess,
		},
		{
			name:         "json supported for projects list",
			outputFormat: "json",
			cmd:          "projects",
			sub:          "list",
			wantRejected: false,
			wantCode:     ExitSuccess,
		},
		{
			name:         "json rejected for unsupported version tag",
			outputFormat: "json",
			cmd:          "version",
			sub:          "tag",
			wantRejected: true,
			wantCode:     ExitUsage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, rejected := rejectStructuredIfUnsupported(tt.outputFormat, tt.cmd, tt.sub)
			if rejected != tt.wantRejected {
				t.Errorf("rejected = %v, want %v", rejected, tt.wantRejected)
			}
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}
