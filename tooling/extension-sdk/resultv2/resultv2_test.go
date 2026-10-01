package resultv2

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// report is a stand-in for a subcommand payload: an ordinary struct, so the
// comparison below runs over the same marshaling a real one would.
type report struct {
	Features int      `json:"features"`
	IDs      []string `json:"ids"`
}

func samplePayload() report {
	return report{Features: 2, IDs: []string{"tooling/a", "tooling/b"}}
}

// coreStructuredCommand is what the CLI does for a BUILT-IN structured command,
// written the way it is written there rather than as a call to the code under
// test: tooling/cli/internal/cli/commands.go — App.runStructuredCommand (the
// envelope write at :103-104, the human branch at :59-68) and
// writeStructuredFailure (:181-189).
//
// It lives here, and not in @putnami/cli where it could call the real thing,
// because go.putnami.dev/tooling/cli does not depend on this module and must
// not: the host CLI would then link the library extensions are authored with,
// and every SDK change would impact the CLI's build. The cost of that choice is
// that this reference can drift from core; the line numbers above are what makes
// a drift traceable, and TestEmit_JSONStaysIndented below pins the one behavior
// a silent drift would break without any reference at all.
func coreStructuredCommand(mode protocolcli.OutputMode, cmd, sub string, data any, err error) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer

	// commands.go:61-63 and :88-91 — the envelope's command is the invoked path.
	cmdPath := cmd
	if sub != "" {
		cmdPath = cmd + " " + sub
	}

	if !mode.IsStructured() {
		// commands.go:59-68: the handler already rendered for a human; only a
		// failure reaches writeStructuredFailure, which prints the message and,
		// via printCommandError (:198-203), the suggested next command.
		if err == nil {
			return "", "", 0
		}
		fmt.Fprintf(&errOut, "putnami: %v\n", err)
		if next := protocolcli.SuggestedNext(err); next != "" {
			fmt.Fprintf(&errOut, "  Try: %s\n", next)
		}
		return out.String(), errOut.String(), protocolcli.ExitCodeForError(err)
	}

	// THE MODE IS THE ONE THAT WAS SELECTED. commands.go:44-46 rewrites
	// outputFormat to jsonl for the HANDLER, but :96/:103 pass
	// parsed.Global.Output — the original — to WriteResultV2, so --output=json
	// is indented and --output=jsonl is one compact line.
	if err != nil {
		// writeStructuredFailure (:186-187): the envelope on stdout carries the
		// classified error and any data attached to it; the message also goes to
		// stderr so it is visible when stdout is being piped.
		exit, _ := protocolcli.WriteResultV2(&out, mode, protocolcli.NewResultV2(cmdPath, data, err))
		fmt.Fprintf(&errOut, "putnami: %v\n", err)
		return out.String(), errOut.String(), exit
	}
	exit, _ := protocolcli.WriteResultV2(&out, mode, protocolcli.NewResultV2(cmdPath, data, nil))
	return out.String(), errOut.String(), exit
}

// TestEmit_MatchesTheCoreEnvelope is the parity acceptance: for the same
// invocation, the SDK helper and the CLI's own structured-command path produce
// the same stdout bytes, the same stderr, and the same exit code — across
// success and failure, and across both structured modes and the human one.
func TestEmit_MatchesTheCoreEnvelope(t *testing.T) {
	failure := protocolcli.WithNext(
		protocolcli.Usagef("unknown feature %q", "tooling/missing"),
		"putnami features list")

	cases := []struct {
		name string
		cmd  string
		sub  string
		data any
		err  error
	}{
		{name: "success", cmd: "features", sub: "list", data: samplePayload()},
		{name: "success without a subcommand", cmd: "features", data: samplePayload()},
		{name: "success with no payload", cmd: "specs", sub: "init"},
		{name: "failure", cmd: "features", sub: "inspect", err: failure},
		{
			// A failure may still carry a payload — the CLI attaches one to the
			// error (shared.WithResultData) and reads it back in
			// writeStructuredFailure; an extension passes it directly.
			name: "failure with a payload",
			cmd:  "contracts", sub: "check",
			data: samplePayload(),
			err:  errors.New("contract drift"),
		},
	}

	for _, mode := range []protocolcli.OutputMode{
		protocolcli.OutputJSON, protocolcli.OutputJSONL, protocolcli.OutputText, protocolcli.OutputAuto,
	} {
		for _, tc := range cases {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code := Emit(&stdout, &stderr, mode, tc.cmd, tc.sub, tc.data, tc.err)

				wantOut, wantErr, wantCode := coreStructuredCommand(mode, tc.cmd, tc.sub, tc.data, tc.err)
				if stdout.String() != wantOut {
					t.Errorf("stdout =\n%q\nwant\n%q", stdout.String(), wantOut)
				}
				if stderr.String() != wantErr {
					t.Errorf("stderr = %q, want %q", stderr.String(), wantErr)
				}
				if code != wantCode {
					t.Errorf("exit code = %d, want %d", code, wantCode)
				}
			})
		}
	}
}

// TestEmit_JSONStaysIndented pins the trap directly, without going through the
// reference above: --output=json must be the indented document and
// --output=jsonl the compact line. A helper that normalized the mode to jsonl
// before writing — which is what the CLI does for the HANDLER, and the obvious
// wrong reading of commands.go:44 — would emit the compact line for both, and
// every --output=json consumer would see different bytes from a built-in
// command's.
func TestEmit_JSONStaysIndented(t *testing.T) {
	envelope := Envelope("features", "list", samplePayload(), nil)

	indented, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	var jsonOut, jsonlOut bytes.Buffer
	Emit(&jsonOut, &bytes.Buffer{}, protocolcli.OutputJSON, "features", "list", samplePayload(), nil)
	Emit(&jsonlOut, &bytes.Buffer{}, protocolcli.OutputJSONL, "features", "list", samplePayload(), nil)

	if got, want := jsonOut.String(), string(indented)+"\n"; got != want {
		t.Errorf("--output=json wrote\n%q\nwant the indented document\n%q", got, want)
	}
	if got, want := jsonlOut.String(), string(compact)+"\n"; got != want {
		t.Errorf("--output=jsonl wrote\n%q\nwant the compact line\n%q", got, want)
	}
	if jsonOut.String() == jsonlOut.String() {
		t.Fatal("json and jsonl produced the same bytes; the selected mode is being normalized away")
	}
	if strings.Count(jsonlOut.String(), "\n") != 1 {
		t.Errorf("--output=jsonl wrote %d lines, want exactly one", strings.Count(jsonlOut.String(), "\n"))
	}
}

// TestEmit_HumanModeWritesNoDocument pins that a non-structured mode leaves
// stdout to the caller's own rendering. Writing an envelope there would put a
// JSON document in the middle of a human table.
func TestEmit_HumanModeWritesNoDocument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Emit(&stdout, &stderr, protocolcli.OutputText, "features", "list", samplePayload(), nil); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("human-mode success wrote stdout %q / stderr %q, want nothing", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	err := protocolcli.WithNext(protocolcli.Usagef("feature %q not found", "x"), "putnami features list")
	code := Emit(&stdout, &stderr, protocolcli.OutputText, "features", "inspect", nil, err)
	if code != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, protocolcli.ExitUsage)
	}
	if stdout.Len() != 0 {
		t.Errorf("human-mode failure wrote %q to stdout, want nothing", stdout.String())
	}
	if got, want := stderr.String(), "putnami: feature \"x\" not found\n  Try: putnami features list\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestEnvelope_CarriesTheClassifiedFailure pins what the envelope says about a
// failure: the exit code and the error code both come from the error's
// classification, and the suggestion an extension attaches reaches error.next
// rather than being lost with the human "Try:" line.
func TestEnvelope_CarriesTheClassifiedFailure(t *testing.T) {
	err := protocolcli.WithNext(protocolcli.Authf("not logged in"), "putnami cloud login")
	envelope := Envelope("cloud", "status", nil, err)

	if envelope.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", envelope.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
	if envelope.Command != "cloud status" {
		t.Errorf("command = %q, want %q", envelope.Command, "cloud status")
	}
	if envelope.Status != protocolcli.StatusFailure {
		t.Errorf("status = %q, want %q", envelope.Status, protocolcli.StatusFailure)
	}
	if envelope.ExitCode != protocolcli.ExitAuth {
		t.Errorf("exitCode = %d, want %d", envelope.ExitCode, protocolcli.ExitAuth)
	}
	if envelope.Error == nil {
		t.Fatal("a failure envelope must carry the error")
	}
	if envelope.Error.Message != "not logged in" || envelope.Error.Next != "putnami cloud login" {
		t.Errorf("error = %+v, want the message and the suggestion", envelope.Error)
	}

	success := Envelope("features", "list", samplePayload(), nil)
	if success.Status != protocolcli.StatusSuccess || success.ExitCode != protocolcli.ExitSuccess || success.Error != nil {
		t.Errorf("success envelope = %+v", success)
	}
}

func TestPath(t *testing.T) {
	if got := Path("features", "list"); got != "features list" {
		t.Errorf("Path = %q, want %q", got, "features list")
	}
	if got := Path("features", ""); got != "features" {
		t.Errorf("Path without a subcommand = %q, want %q", got, "features")
	}
}
