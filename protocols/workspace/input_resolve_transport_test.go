package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestIsInputResolveInvocationMatchesOnlyTheReservedCall(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"reserved call", []string{ProbeControlVerb, InputResolveControlCommand}, true},
		{"ordinary task named like the control command", []string{InputResolveControlCommand}, false},
		{"verb alone", []string{ProbeControlVerb}, false},
		{"probe call", []string{ProbeControlVerb, ProbeControlCommand}, false},
		{"toolchain-resolve call", []string{ProbeControlVerb, ToolchainResolveControlCommand}, false},
		{"extra arguments", []string{ProbeControlVerb, InputResolveControlCommand, "--json"}, false},
		{"nothing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInputResolveInvocation(tc.args); got != tc.want {
				t.Fatalf("IsInputResolveInvocation(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
	if IsProbeInvocation(InputResolveControlArgs()) || IsToolchainResolveInvocation(InputResolveControlArgs()) {
		t.Fatal("another control call claims the input-resolve call")
	}
}

func TestInputResolveControlArgsIsTheSpawnShape(t *testing.T) {
	args := InputResolveControlArgs()
	if !IsInputResolveInvocation(args) {
		t.Fatalf("InputResolveControlArgs() = %q, which IsInputResolveInvocation rejects", args)
	}
	// A caller that mutates the returned slice must not corrupt the next one.
	args[0] = "mutated"
	if !IsInputResolveInvocation(InputResolveControlArgs()) {
		t.Fatal("InputResolveControlArgs() returned a shared slice a caller could poison")
	}
}

// encodedInputResolveRequest is the core's side of the exchange for the valid
// request, as the extension reads it on stdin.
func encodedInputResolveRequest(t *testing.T) *bytes.Buffer {
	t.Helper()
	var request bytes.Buffer
	if err := EncodeInputResolveRequest(&request, validInputResolveRequest()); err != nil {
		t.Fatalf("EncodeInputResolveRequest: %v", err)
	}
	return &request
}

func TestServeInputResolveRoundTrip(t *testing.T) {
	var seen InputResolveRequest
	var stdout bytes.Buffer
	handled, err := ServeInputResolve(InputResolveControlArgs(), encodedInputResolveRequest(t), &stdout,
		func(r InputResolveRequest) (InputResolveResult, error) {
			seen = r
			answer := validInputResolveResult()
			answer.Version, answer.Extension = 0, ""
			return answer, nil
		})
	if !handled || err != nil {
		t.Fatalf("ServeInputResolve = (%v, %v), want (true, nil)", handled, err)
	}
	if want := validInputResolveRequest(); !reflect.DeepEqual(seen, want) {
		t.Fatalf("answer saw %+v, want the core's request %+v", seen, want)
	}
	if !strings.HasSuffix(stdout.String(), "}\n") || strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("stdout must carry exactly one document line, got %q", stdout.String())
	}

	result, diags := ParseAndValidateInputResolveResult(stdout.Bytes())
	if len(diags) != 0 {
		t.Fatalf("the core rejects the served answer: %v", diags)
	}
	if result.Version != InputResolveProtocolVersion || result.Extension != "@putnami/go" {
		t.Fatalf("result is attributed to %q at version %d, want the asked extension at %d",
			result.Extension, result.Version, InputResolveProtocolVersion)
	}
	if want := validInputResolveResult(); !reflect.DeepEqual(*result, want) {
		t.Fatalf("answer did not survive the exchange:\n got %+v\nwant %+v", *result, want)
	}
}

func TestServeInputResolveDeclinesOtherInvocations(t *testing.T) {
	for _, args := range [][]string{{"build"}, ProbeControlArgs(), ToolchainResolveControlArgs()} {
		var stdout bytes.Buffer
		handled, err := ServeInputResolve(args, strings.NewReader(""), &stdout,
			func(InputResolveRequest) (InputResolveResult, error) {
				t.Fatal("answer must not run for an unhandled call")
				return InputResolveResult{}, nil
			})
		if handled || err != nil {
			t.Fatalf("ServeInputResolve(%q) = (%v, %v), want (false, nil)", args, handled, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("ServeInputResolve wrote %q for an unhandled call", stdout.String())
		}
	}
}

func TestServeInputResolveRequiresAnAnswerFunction(t *testing.T) {
	var stdout bytes.Buffer
	handled, err := ServeInputResolve(InputResolveControlArgs(), encodedInputResolveRequest(t), &stdout, nil)
	if !handled || err == nil {
		t.Fatalf("ServeInputResolve = (%v, %v), want a handled error", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeInputResolve wrote %q with no answer function", stdout.String())
	}
}

func TestServeInputResolveRejectsAnUnreadableRequest(t *testing.T) {
	// A member this protocol version does not know, a request the contract
	// rejects, and a document followed by log output are each a question the
	// extension cannot fully read.
	for name, request := range map[string]string{
		"unknown member":   `{"version":1,"extension":"x","inputs":[{"project":".","selector":"go-embed:build"}],"workspaceRoot":"/w"}`,
		"invalid selector": `{"version":1,"extension":"x","inputs":[{"project":".","selector":"git:build"}]}`,
		"duplicate input":  `{"version":1,"extension":"x","inputs":[{"project":".","selector":"go-embed:build"},{"project":".","selector":"go-embed:build"}]}`,
		"other version":    `{"version":2,"extension":"x","inputs":[{"project":".","selector":"go-embed:build"}]}`,
		"trailing data":    `{"version":1,"extension":"x","inputs":[{"project":".","selector":"go-embed:build"}]}` + "\ninfo: done\n",
		"empty document":   " \n",
		"no inputs":        `{"version":1,"extension":"x","inputs":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			handled, err := ServeInputResolve(InputResolveControlArgs(), strings.NewReader(request), &stdout,
				func(InputResolveRequest) (InputResolveResult, error) {
					t.Fatal("answer must not run for a rejected request")
					return InputResolveResult{}, nil
				})
			if !handled {
				t.Fatal("ServeInputResolve must handle the reserved invocation even when the request is bad")
			}
			if err == nil {
				t.Fatal("expected an error for an unreadable request")
			}
			if stdout.Len() != 0 {
				t.Fatalf("ServeInputResolve wrote %q for a rejected request; stdout must stay empty", stdout.String())
			}
		})
	}
}

func TestServeInputResolveWritesNothingWhenTheAnswerFails(t *testing.T) {
	var stdout bytes.Buffer
	sentinel := errors.New("go list failed")
	handled, err := ServeInputResolve(InputResolveControlArgs(), encodedInputResolveRequest(t), &stdout,
		func(InputResolveRequest) (InputResolveResult, error) {
			return validInputResolveResult(), sentinel
		})
	if !handled || !errors.Is(err, sentinel) {
		t.Fatalf("ServeInputResolve = (%v, %v), want the answer's own error", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeInputResolve wrote %q after a failed answer; a partial answer must never key a task", stdout.String())
	}
}

// An answer the core would reject is rejected on the extension's side, before
// a byte reaches stdout.
func TestServeInputResolveRejectsAnAnswerTheContractRejects(t *testing.T) {
	cases := map[string]func(*InputResolveResult){
		"other extension":    func(r *InputResolveResult) { r.Extension = "@putnami/typescript" },
		"an input left out":  func(r *InputResolveResult) { r.Inputs = r.Inputs[:2] },
		"answers reordered":  func(r *InputResolveResult) { r.Inputs[0], r.Inputs[2] = r.Inputs[2], r.Inputs[0] },
		"unselected file":    func(r *InputResolveResult) { r.Inputs[2].Files = []string{"assets/app.css"} },
		"unsorted files":     func(r *InputResolveResult) { slices.Reverse(r.Inputs[0].Files) },
		"escaping file":      func(r *InputResolveResult) { r.Inputs[0].Files[0] = "../secret" },
		"error with a basis": func(r *InputResolveResult) { r.Inputs[1].Basis = r.Inputs[0].Basis },
		"git basis pattern":  func(r *InputResolveResult) { r.Inputs[2].Basis.Content[0][0] = "git:**/*.go" },
		"blank error":        func(r *InputResolveResult) { r.Inputs[1].Error = " " },
		"oversized answer": func(r *InputResolveResult) {
			name := "assets/" + strings.Repeat("a", 1024)
			r.Inputs[0].Files = nil
			for i := 0; i < MaxProbeDocumentBytes/len(name)+1; i++ {
				r.Inputs[0].Files = append(r.Inputs[0].Files, fmt.Sprintf("%s%06d", name, i))
			}
		},
		"exclusions-only set": func(r *InputResolveResult) { r.Inputs[2].Basis.Listing = [][]string{{"!vendor/**"}} },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			handled, err := ServeInputResolve(InputResolveControlArgs(), encodedInputResolveRequest(t), &stdout,
				func(InputResolveRequest) (InputResolveResult, error) {
					answer := validInputResolveResult()
					edit(&answer)
					return answer, nil
				})
			if !handled || err == nil {
				t.Fatalf("ServeInputResolve = (%v, %v), want a contract rejection", handled, err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("ServeInputResolve wrote %d bytes for a rejected answer", stdout.Len())
			}
		})
	}
}

// The answer function receives a copy: editing it cannot change the question
// its answer is checked against.
func TestServeInputResolveChecksTheAnswerAgainstTheCoreRequest(t *testing.T) {
	var stdout bytes.Buffer
	handled, err := ServeInputResolve(InputResolveControlArgs(), encodedInputResolveRequest(t), &stdout,
		func(r InputResolveRequest) (InputResolveResult, error) {
			r.Inputs[0].Project = "go/framework/api"
			r.Inputs[0].Patterns[0] = "edited/**"
			answer := validInputResolveResult()
			answer.Inputs[0].Project = "go/framework/api"
			return answer, nil
		})
	if !handled || err == nil || !strings.Contains(err.Error(), "inputs[0].project") {
		t.Fatalf("ServeInputResolve = (%v, %v), want the edited project rejected", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeInputResolve wrote %q for a rejected answer", stdout.String())
	}
}

func TestDecodeInputResolveDocumentRejectsTrailingEmptyAndOversized(t *testing.T) {
	for name, input := range map[string][]byte{
		"trailing data": []byte(`{"version":1,"extension":"x"}` + "\ninfo: done\n"),
		"empty":         []byte("   \n"),
		"oversized":     bytes.Repeat([]byte("a"), MaxProbeDocumentBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeInputResolveDocument(bytes.NewReader(input))
			if err == nil {
				t.Fatal("expected the document to be rejected")
			}
			if !strings.Contains(err.Error(), "input-resolve document") {
				t.Fatalf("error = %v, want it to name the input-resolve document", err)
			}
		})
	}
}

// The core stamps the wire version and keeps its order: the answer follows
// the request position by position.
func TestEncodeInputResolveRequestStampsAndKeepsOrder(t *testing.T) {
	request := validInputResolveRequest()
	request.Version = 99
	slices.Reverse(request.Inputs)
	var buf bytes.Buffer
	if err := EncodeInputResolveRequest(&buf, request); err != nil {
		t.Fatalf("EncodeInputResolveRequest: %v", err)
	}
	var decoded InputResolveRequest
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Version != InputResolveProtocolVersion {
		t.Fatalf("version = %d, want %d: the core stamps the wire version, callers do not", decoded.Version, InputResolveProtocolVersion)
	}
	request.Version = InputResolveProtocolVersion
	if !reflect.DeepEqual(decoded, request) {
		t.Fatalf("wire request = %+v, want the caller's inputs in the caller's order %+v", decoded, request)
	}
}

func TestEncodeInputResolveRequestRejectsAnInvalidRequest(t *testing.T) {
	request := validInputResolveRequest()
	request.Inputs[0].Selector = "git:**"
	var buf bytes.Buffer
	err := EncodeInputResolveRequest(&buf, request)
	if err == nil || !strings.Contains(err.Error(), "inputs[0].selector") {
		t.Fatalf("EncodeInputResolveRequest error = %v, want the selector rejected", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("EncodeInputResolveRequest wrote %q for a rejected request", buf.String())
	}
}

// The exchange is byte-stable: one answer produces one document.
func TestServeInputResolveIsByteStable(t *testing.T) {
	rendered := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		var stdout bytes.Buffer
		if _, err := ServeInputResolve(InputResolveControlArgs(), encodedInputResolveRequest(t), &stdout,
			func(InputResolveRequest) (InputResolveResult, error) {
				return validInputResolveResult(), nil
			}); err != nil {
			t.Fatalf("ServeInputResolve: %v", err)
		}
		rendered = append(rendered, stdout.String())
	}
	if rendered[0] != rendered[1] {
		t.Fatalf("one answer produced two documents:\n%s\n%s", rendered[0], rendered[1])
	}
}
