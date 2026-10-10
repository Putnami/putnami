package clicore

import (
	"encoding/json"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// TestResolveOutputModeDelegatesToProtocolCLI pins the shared output-mode flag
// contract: --json is the --output=json alias, any different explicit --output
// conflicts with it, and unknown modes are usage errors.
func TestResolveOutputModeDelegatesToProtocolCLI(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]any
		want    protocolcli.OutputMode
		wantErr bool
	}{
		{"default auto", map[string]any{}, protocolcli.OutputAuto, false},
		{"output jsonl", map[string]any{"output": "jsonl"}, OutputJSONL, false},
		{"output json", map[string]any{"output": "json"}, OutputJSON, false},
		{"output cloud logging", map[string]any{"output": "cloud-logging"}, OutputCloudLogging, false},
		{"output text is human", map[string]any{"output": "text"}, OutputText, false},
		{"json alias", map[string]any{"json": true}, OutputJSON, false},
		{"json alias truthy string", map[string]any{"json": "true"}, OutputJSON, false},
		{"json false stays auto", map[string]any{"json": false}, protocolcli.OutputAuto, false},
		{"agree json+json", map[string]any{"output": "json", "json": true}, OutputJSON, false},
		{"conflict jsonl+json", map[string]any{"output": "jsonl", "json": true}, protocolcli.OutputAuto, true},
		{"conflict text+json", map[string]any{"output": "text", "json": true}, protocolcli.OutputAuto, true},
		{"unknown output with alias", map[string]any{"output": "yaml", "json": true}, protocolcli.OutputAuto, true},
		{"unknown output alone", map[string]any{"output": "yaml"}, protocolcli.OutputAuto, true},
		{"output uppercase is unknown", map[string]any{"output": "JSONL"}, protocolcli.OutputAuto, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveOutputMode(tc.params)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ResolveOutputMode(%v) error = %v, wantErr %v", tc.params, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("ResolveOutputMode(%v) = %q, want %q", tc.params, got, tc.want)
			}
		})
	}
}

// TestStructuredOutput asserts the boolean gate agrees with OutputMode: every
// structured selector is structured; human modes are not.
func TestStructuredOutput(t *testing.T) {
	structured := []map[string]any{
		{"output": "jsonl"},
		{"output": "json"},
		{"json": true},
	}
	human := []map[string]any{
		{},
		{"output": "text"},
		{"json": false},
		{"output": "text", "json": true},
		{"output": "jsonl", "json": true},
	}
	for _, p := range structured {
		if !StructuredOutput(p) {
			t.Errorf("StructuredOutput(%v) = false, want true", p)
		}
	}
	for _, p := range human {
		if StructuredOutput(p) {
			t.Errorf("StructuredOutput(%v) = true, want false", p)
		}
	}
}

// TestWriteResultSelectsStructuredForEverySelector proves the one-shot output
// path (WriteResult) emits the single structured object for --json,
// --output=json, and --output=jsonl alike, and prints the human message by
// default. The jsonl/json distinction does not change a one-shot command: all
// three route through the same JSON hook.
func TestWriteResultSelectsStructuredForEverySelector(t *testing.T) {
	data := map[string]any{"ok": true}

	structuredSelectors := []map[string]any{
		{"json": true},
		{"output": "json"},
		{"output": "jsonl"},
	}
	for _, params := range structuredSelectors {
		var jsonCalls int
		var stdoutLines []string
		io := IO{
			Stdout: func(s string) { stdoutLines = append(stdoutLines, s) },
			JSON: func(v any) {
				jsonCalls++
				result, ok := v.(protocolcli.ResultV2)
				if !ok {
					t.Fatalf("JSON hook got %T, want protocolcli.ResultV2", v)
				}
				if result.Status != protocolcli.StatusSuccess || result.ExitCode != ExitSuccess {
					t.Fatalf("result = %+v, want success exit 0", result)
				}
				data, ok := result.Data.(map[string]any)
				if !ok || data["ok"] != true {
					t.Fatalf("result data = %#v", result.Data)
				}
			},
		}
		WriteResult(data, params, io, "human message")
		if jsonCalls != 1 {
			t.Errorf("params %v: JSON hook called %d times, want 1", params, jsonCalls)
		}
		if len(stdoutLines) != 0 {
			t.Errorf("params %v: expected no human stdout, got %v", params, stdoutLines)
		}
	}

	// Default: human message, no JSON hook.
	var jsonCalls int
	var stdoutLines []string
	io := IO{
		Stdout: func(s string) { stdoutLines = append(stdoutLines, s) },
		JSON:   func(any) { jsonCalls++ },
	}
	WriteResult(data, map[string]any{}, io, "human message")
	if jsonCalls != 0 {
		t.Errorf("default: JSON hook called %d times, want 0", jsonCalls)
	}
	if len(stdoutLines) != 1 || stdoutLines[0] != "human message" {
		t.Errorf("default: stdout = %v, want [\"human message\"]", stdoutLines)
	}
}

func TestWriteResultRendersProtocolEnvelopeToStdout(t *testing.T) {
	var stdoutLines []string
	WriteResult(
		map[string]any{"ok": true},
		map[string]any{"output": "jsonl", "command": "cloud whoami"},
		IO{Stdout: func(s string) { stdoutLines = append(stdoutLines, s) }},
		"human message",
	)
	if len(stdoutLines) != 1 {
		t.Fatalf("stdout lines = %d, want 1: %v", len(stdoutLines), stdoutLines)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdoutLines[0]), &got); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if got["command"] != "cloud whoami" || got["status"] != "success" || got["exitCode"].(float64) != 0 {
		t.Fatalf("envelope = %v", got)
	}
	data := got["data"].(map[string]any)
	if data["ok"] != true {
		t.Fatalf("data = %v", data)
	}
}

func TestWriteErrorResultRendersProtocolFailureEnvelope(t *testing.T) {
	var stdoutLines []string
	WriteErrorResult(
		NewError("login required", ExitAuth),
		map[string]any{"output": "jsonl", "command": "cloud whoami"},
		IO{Stdout: func(s string) { stdoutLines = append(stdoutLines, s) }},
	)
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.Join(stdoutLines, "\n")), &got); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if got["status"] != "failure" || got["exitCode"].(float64) != float64(ExitAuth) {
		t.Fatalf("envelope = %v", got)
	}
	errObj := got["error"].(map[string]any)
	if errObj["code"] != "auth" || errObj["message"] != "login required" {
		t.Fatalf("error = %v", errObj)
	}
}

func TestExitCodePreservesCustomExitErrorCode(t *testing.T) {
	err := NewError("wrapped command exited with code 17", 17)
	if got := ExitCode(err); got != 17 {
		t.Fatalf("ExitCode(custom) = %d, want 17", got)
	}
}

func TestWriteErrorResultPreservesCustomExitErrorCode(t *testing.T) {
	var stdoutLines []string
	WriteErrorResult(
		NewError("wrapped command exited with code 17", 17),
		map[string]any{"output": "jsonl", "command": "cloud report"},
		IO{Stdout: func(s string) { stdoutLines = append(stdoutLines, s) }},
	)
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.Join(stdoutLines, "\n")), &got); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if got["status"] != "failure" || got["exitCode"].(float64) != 17 {
		t.Fatalf("envelope = %v, want custom exit code 17", got)
	}
	errObj := got["error"].(map[string]any)
	if errObj["code"] != "failure" {
		t.Fatalf("error code = %v, want failure", errObj["code"])
	}
}

// TestWriteErrorResultCarriesAttachedData pins the check-command contract: one
// failure envelope holds both what the command found and why it exits 1.
func TestWriteErrorResultCarriesAttachedData(t *testing.T) {
	var stdoutLines []string
	err := WithResultData(NewError("environment prod is not ready", ExitFailure), map[string]any{"ready": false})
	WriteErrorResult(err, map[string]any{"output": "jsonl", "command": "cloud env doctor"},
		IO{Stdout: func(s string) { stdoutLines = append(stdoutLines, s) }})
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.Join(stdoutLines, "\n")), &got); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if got["status"] != "failure" || got["exitCode"].(float64) != float64(ExitFailure) {
		t.Fatalf("envelope = %v", got)
	}
	if data, ok := got["data"].(map[string]any); !ok || data["ready"] != false {
		t.Fatalf("data = %v, want the attached result", got["data"])
	}
	if errObj := got["error"].(map[string]any); errObj["message"] != "environment prod is not ready" {
		t.Fatalf("error = %v", errObj)
	}
	if ExitCode(err) != ExitFailure || err.Error() != "environment prod is not ready" {
		t.Fatalf("the carrier changed the error: %d %q", ExitCode(err), err.Error())
	}
	if WithResultData(nil, "data") != nil {
		t.Fatal("WithResultData(nil) is not nil")
	}
}
