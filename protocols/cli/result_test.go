package cli

import (
	"encoding/json"
	"testing"
)

// The version-1 envelope is a READ shape now that this module has deleted its
// constructor and writer: nothing in this repository builds one, so these tests
// exercise the only thing left to exercise — decoding a document a CLI build
// published before the removal wrote. Building the fixtures from literal JSON
// rather than from a constructor is the point: a test that round-tripped
// through a Go helper would keep the emitter alive to serve itself.

// v1SuccessDocument is a version-1 success envelope exactly as a pre-removal
// CLI wrote it: no protocolVersion member, no error member.
const v1SuccessDocument = `{
  "command": "build",
  "status": "success",
  "data": {"succeeded": 3},
  "exitCode": 0
}`

// v1FailureDocument is the failure arm, including the error.next suggestion the
// v1 envelope carried for recoverable failures.
const v1FailureDocument = `{
  "command": "cloud status",
  "status": "failure",
  "error": {"code": "auth", "message": "token expired", "next": "putnami cloud login"},
  "exitCode": 3
}`

func TestResultV1_DecodesRecordedSuccessDocument(t *testing.T) {
	var r Result
	if err := json.Unmarshal([]byte(v1SuccessDocument), &r); err != nil {
		t.Fatalf("decode v1 success document: %v", err)
	}
	if r.Command != "build" {
		t.Errorf("Command = %q, want build", r.Command)
	}
	if r.Status != StatusSuccess {
		t.Errorf("Status = %q, want %q", r.Status, StatusSuccess)
	}
	if r.ExitCode != ExitSuccess {
		t.Errorf("ExitCode = %d, want %d", r.ExitCode, ExitSuccess)
	}
	if r.Error != nil {
		t.Errorf("Error = %+v, want nil", r.Error)
	}
}

func TestResultV1_DecodesRecordedFailureDocument(t *testing.T) {
	var r Result
	if err := json.Unmarshal([]byte(v1FailureDocument), &r); err != nil {
		t.Fatalf("decode v1 failure document: %v", err)
	}
	if r.Status != StatusFailure {
		t.Errorf("Status = %q, want %q", r.Status, StatusFailure)
	}
	if r.ExitCode != ExitAuth {
		t.Errorf("ExitCode = %d, want %d", r.ExitCode, ExitAuth)
	}
	if r.Error == nil || r.Error.Code != "auth" || r.Error.Message != "token expired" {
		t.Fatalf("Error = %+v, want code=auth message=\"token expired\"", r.Error)
	}
	if r.Error.Next != "putnami cloud login" {
		t.Errorf("Error.Next = %q, want %q", r.Error.Next, "putnami cloud login")
	}
}

// TestResultV1_HasNoProtocolVersion pins what makes a document v1 in the first
// place, and is what a reader dispatches on: the absence of the member. A v1
// struct that grew one would make version detection ambiguous for every
// consumer that must open both shapes.
func TestResultV1_HasNoProtocolVersion(t *testing.T) {
	data, err := json.Marshal(Result{Command: "build", Status: StatusSuccess})
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatal(err)
	}
	if _, present := members["protocolVersion"]; present {
		t.Errorf("the v1 envelope grew a protocolVersion member: %s", data)
	}
}
