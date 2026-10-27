package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestIsToolchainResolveInvocationMatchesOnlyTheReservedCall(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "only-the-reserved-call-is-claimed")
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"reserved call", []string{ProbeControlVerb, ToolchainResolveControlCommand}, true},
		{"ordinary task named like the control command", []string{ToolchainResolveControlCommand}, false},
		{"verb alone", []string{ProbeControlVerb}, false},
		{"probe call", []string{ProbeControlVerb, ProbeControlCommand}, false},
		{"extra arguments", []string{ProbeControlVerb, ToolchainResolveControlCommand, "--json"}, false},
		{"nothing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsToolchainResolveInvocation(tc.args); got != tc.want {
				t.Fatalf("IsToolchainResolveInvocation(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
	if IsProbeInvocation(ToolchainResolveControlArgs()) {
		t.Fatal("the probe claims the toolchain-resolve call")
	}
}

func TestToolchainResolveControlArgsIsTheSpawnShape(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "only-the-reserved-call-is-claimed")
	args := ToolchainResolveControlArgs()
	if !IsToolchainResolveInvocation(args) {
		t.Fatalf("ToolchainResolveControlArgs() = %q, which IsToolchainResolveInvocation rejects", args)
	}
	// A caller that mutates the returned slice must not corrupt the next one.
	args[0] = "mutated"
	if !IsToolchainResolveInvocation(ToolchainResolveControlArgs()) {
		t.Fatal("ToolchainResolveControlArgs() returned a shared slice a caller could poison")
	}
}

// encodedToolchainResolveRequest is the core's side of the exchange for the
// valid request, as the extension reads it on stdin.
func encodedToolchainResolveRequest(t *testing.T) *bytes.Buffer {
	t.Helper()
	var request bytes.Buffer
	if err := EncodeToolchainResolveRequest(&request, validToolchainResolveRequest()); err != nil {
		t.Fatalf("EncodeToolchainResolveRequest: %v", err)
	}
	return &request
}

func TestServeToolchainResolveRoundTrip(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "one-bounded-document-each-way")
	asked := validToolchainResolveRequest()
	asked.Identities = []string{"golangci-lint", "go", "go"}
	var request bytes.Buffer
	if err := EncodeToolchainResolveRequest(&request, asked); err != nil {
		t.Fatalf("EncodeToolchainResolveRequest: %v", err)
	}

	var seen ToolchainResolveRequest
	var stdout bytes.Buffer
	handled, err := ServeToolchainResolve(ToolchainResolveControlArgs(), &request, &stdout,
		func(r ToolchainResolveRequest) (ToolchainResolveResult, error) {
			seen = r
			answer := validToolchainResolveResult()
			answer.Version, answer.Extension = 0, ""
			return answer, nil
		})
	if !handled || err != nil {
		t.Fatalf("ServeToolchainResolve = (%v, %v), want (true, nil)", handled, err)
	}
	if want := []string{"go", "golangci-lint"}; !slices.Equal(seen.Identities, want) {
		t.Fatalf("answer saw identities %q, want %q", seen.Identities, want)
	}
	if seen.Platform != "darwin/arm64" || seen.Locked["go"].Source != "https://go.dev/dl/" {
		t.Fatalf("answer saw %+v, want the core's platform and committed entry", seen)
	}
	if !strings.HasSuffix(stdout.String(), "}\n") || strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("stdout must carry exactly one document line, got %q", stdout.String())
	}

	result, diags := ParseAndValidateToolchainResolveResult(stdout.Bytes())
	if len(diags) != 0 {
		t.Fatalf("the core rejects the served answer: %v", diags)
	}
	if result.Version != ToolchainResolveProtocolVersion || result.Extension != "@putnami/go" {
		t.Fatalf("result is attributed to %q at version %d, want the asked extension at %d",
			result.Extension, result.Version, ToolchainResolveProtocolVersion)
	}
	if got := result.Toolchains["go"].Pin; got == nil || !strings.HasPrefix(got.Archives["darwin/arm64"].URL, "https://go.dev/dl/") {
		t.Fatalf("go pin did not survive the exchange: %+v", got)
	}
}

func TestServeToolchainResolveDeclinesOtherInvocations(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "only-the-reserved-call-is-claimed")
	for _, args := range [][]string{{"build"}, ProbeControlArgs()} {
		var stdout bytes.Buffer
		handled, err := ServeToolchainResolve(args, strings.NewReader(""), &stdout,
			func(ToolchainResolveRequest) (ToolchainResolveResult, error) {
				t.Fatal("answer must not run for an unhandled call")
				return ToolchainResolveResult{}, nil
			})
		if handled || err != nil {
			t.Fatalf("ServeToolchainResolve(%q) = (%v, %v), want (false, nil)", args, handled, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("ServeToolchainResolve wrote %q for an unhandled call", stdout.String())
		}
	}
}

func TestServeToolchainResolveRequiresAnAnswerFunction(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "nothing-is-written-on-failure")
	var stdout bytes.Buffer
	handled, err := ServeToolchainResolve(ToolchainResolveControlArgs(), encodedToolchainResolveRequest(t), &stdout, nil)
	if !handled || err == nil {
		t.Fatalf("ServeToolchainResolve = (%v, %v), want a handled error", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeToolchainResolve wrote %q with no answer function", stdout.String())
	}
}

func TestServeToolchainResolveRejectsAnUnreadableRequest(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "one-bounded-document-each-way")
	// A member this protocol version does not know, a request the contract
	// rejects, and a document followed by log output are each a question the
	// extension cannot fully read.
	for name, request := range map[string]string{
		"unknown member":   `{"version":1,"extension":"x","identities":["go"],"platform":"linux/amd64","workspaceRoot":"/w"}`,
		"invalid request":  `{"version":1,"extension":"x","identities":["go","go"],"platform":"linux/amd64"}`,
		"other version":    `{"version":2,"extension":"x","identities":["go"],"platform":"linux/amd64"}`,
		"trailing data":    `{"version":1,"extension":"x","identities":["go"],"platform":"linux/amd64"}` + "\ninfo: done\n",
		"empty document":   " \n",
		"locked not asked": `{"version":1,"extension":"x","identities":["go"],"locked":{"bun":{"version":"1.3.14","integrities":{}}},"platform":"linux/amd64"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			handled, err := ServeToolchainResolve(ToolchainResolveControlArgs(), strings.NewReader(request), &stdout,
				func(ToolchainResolveRequest) (ToolchainResolveResult, error) {
					t.Fatal("answer must not run for a rejected request")
					return ToolchainResolveResult{}, nil
				})
			if !handled {
				t.Fatal("ServeToolchainResolve must handle the reserved invocation even when the request is bad")
			}
			if err == nil {
				t.Fatal("expected an error for an unreadable request")
			}
			if stdout.Len() != 0 {
				t.Fatalf("ServeToolchainResolve wrote %q for a rejected request; stdout must stay empty", stdout.String())
			}
		})
	}
}

func TestServeToolchainResolveWritesNothingWhenTheAnswerFails(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "nothing-is-written-on-failure")
	var stdout bytes.Buffer
	sentinel := errors.New("go.dev unreachable")
	handled, err := ServeToolchainResolve(ToolchainResolveControlArgs(), encodedToolchainResolveRequest(t), &stdout,
		func(ToolchainResolveRequest) (ToolchainResolveResult, error) {
			return validToolchainResolveResult(), sentinel
		})
	if !handled || !errors.Is(err, sentinel) {
		t.Fatalf("ServeToolchainResolve = (%v, %v), want the answer's own error", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeToolchainResolve wrote %q after a failed answer; a partial answer must never reach the lock", stdout.String())
	}
}

// An answer the core would reject is rejected on the extension's side, before
// a byte reaches stdout.
func TestServeToolchainResolveRejectsAnAnswerTheContractRejects(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "nothing-is-written-on-failure")
	cases := map[string]func(*ToolchainResolveResult){
		"unasked identity": func(r *ToolchainResolveResult) {
			r.Toolchains["bun"] = ToolchainResolution{Problem: &ToolchainProblem{Message: "not asked"}}
		},
		"other extension": func(r *ToolchainResolveResult) { r.Extension = "@putnami/typescript" },
		"pin missing the host platform": func(r *ToolchainResolveResult) {
			delete(r.Toolchains["go"].Pin.Archives, "darwin/arm64")
			delete(r.Toolchains["go"].Pin.Integrities, "darwin/arm64")
		},
		"pin with a version 4 source": func(r *ToolchainResolveResult) {
			r.Toolchains["go"].Pin.Source = "https://go.dev/dl/"
		},
		"pin and problem": func(r *ToolchainResolveResult) {
			resolution := r.Toolchains["go"]
			resolution.Problem = &ToolchainProblem{Message: "both"}
			r.Toolchains["go"] = resolution
		},
		"oversized answer": func(r *ToolchainResolveResult) {
			r.Toolchains["go"].Pin.Version = strings.Repeat("9", MaxProbeDocumentBytes)
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			handled, err := ServeToolchainResolve(ToolchainResolveControlArgs(), encodedToolchainResolveRequest(t), &stdout,
				func(ToolchainResolveRequest) (ToolchainResolveResult, error) {
					answer := validToolchainResolveResult()
					edit(&answer)
					return answer, nil
				})
			if !handled || err == nil {
				t.Fatalf("ServeToolchainResolve = (%v, %v), want a contract rejection", handled, err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("ServeToolchainResolve wrote %d bytes for a rejected answer", stdout.Len())
			}
		})
	}
}

// The answer function receives a copy: editing it cannot widen the question
// its answer is checked against.
func TestServeToolchainResolveChecksTheAnswerAgainstTheCoreRequest(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "nothing-is-written-on-failure")
	var stdout bytes.Buffer
	handled, err := ServeToolchainResolve(ToolchainResolveControlArgs(), encodedToolchainResolveRequest(t), &stdout,
		func(r ToolchainResolveRequest) (ToolchainResolveResult, error) {
			r.Identities[0] = "bun"
			r.Locked["go"].Integrities["darwin/arm64"] = "edited"
			return ToolchainResolveResult{Toolchains: map[string]ToolchainResolution{
				"bun": {Problem: &ToolchainProblem{Message: "not asked"}},
			}}, nil
		})
	if !handled || err == nil || !strings.Contains(err.Error(), "toolchains.bun") {
		t.Fatalf("ServeToolchainResolve = (%v, %v), want the unasked identity rejected", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeToolchainResolve wrote %q for a rejected answer", stdout.String())
	}
}

func TestDecodeToolchainResolveDocumentRejectsTrailingEmptyAndOversized(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "one-bounded-document-each-way")
	for name, input := range map[string][]byte{
		"trailing data": []byte(`{"version":1,"extension":"x"}` + "\ninfo: done\n"),
		"empty":         []byte("   \n"),
		"oversized":     bytes.Repeat([]byte("a"), MaxProbeDocumentBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeToolchainResolveDocument(bytes.NewReader(input))
			if err == nil {
				t.Fatal("expected the document to be rejected")
			}
			if !strings.Contains(err.Error(), "toolchain-resolve document") {
				t.Fatalf("error = %v, want it to name the toolchain-resolve document", err)
			}
		})
	}
}

// The probe and the toolchain-resolve call share one document reader; the
// probe's messages are part of its contract and stay exact.
func TestDecodeProbeDocumentMessagesAreExact(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "one-bounded-document-each-way")
	cases := map[string]struct {
		input []byte
		want  string
	}{
		"empty":     {[]byte(" \n"), "probe document is empty"},
		"oversized": {bytes.Repeat([]byte("a"), MaxProbeDocumentBytes+1), "probe document exceeds the 8388608 byte contract cap"},
		"trailing": {[]byte(`{} {}`),
			"probe document carries trailing data; stdout must carry the result and nothing else"},
		"not JSON": {[]byte(`{`), "probe document is not one JSON value: unexpected EOF"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeProbeDocument(bytes.NewReader(tc.input))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("DecodeProbeDocument error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEncodeToolchainResolveRequestStampsAndNormalizes(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "the-request-is-stamped-and-the-answer-byte-stable")
	request := validToolchainResolveRequest()
	request.Version = 99
	request.Identities = []string{"golangci-lint", "go", "golangci-lint"}
	var buf bytes.Buffer
	if err := EncodeToolchainResolveRequest(&buf, request); err != nil {
		t.Fatalf("EncodeToolchainResolveRequest: %v", err)
	}
	var decoded ToolchainResolveRequest
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Version != ToolchainResolveProtocolVersion {
		t.Fatalf("version = %d, want %d: the core stamps the wire version, callers do not", decoded.Version, ToolchainResolveProtocolVersion)
	}
	if want := []string{"go", "golangci-lint"}; !slices.Equal(decoded.Identities, want) {
		t.Fatalf("wire identities = %q, want %q", decoded.Identities, want)
	}
	if want := []string{"golangci-lint", "go", "golangci-lint"}; !slices.Equal(request.Identities, want) {
		t.Fatalf("encoding reordered the caller's identities to %q", request.Identities)
	}
}

func TestEncodeToolchainResolveRequestRejectsAnInvalidRequest(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "the-request-is-stamped-and-the-answer-byte-stable")
	request := validToolchainResolveRequest()
	request.Platform = "darwin-arm64"
	var buf bytes.Buffer
	err := EncodeToolchainResolveRequest(&buf, request)
	if err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("EncodeToolchainResolveRequest error = %v, want the platform rejected", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("EncodeToolchainResolveRequest wrote %q for a rejected request", buf.String())
	}
}

// The exchange is byte-stable: one answer, however its maps were built,
// produces one document.
func TestServeToolchainResolveIsByteStable(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "a-reserved-call-exchanges-one-bounded-document", "the-request-is-stamped-and-the-answer-byte-stable")
	rendered := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		var stdout bytes.Buffer
		if _, err := ServeToolchainResolve(ToolchainResolveControlArgs(), encodedToolchainResolveRequest(t), &stdout,
			func(ToolchainResolveRequest) (ToolchainResolveResult, error) {
				return validToolchainResolveResult(), nil
			}); err != nil {
			t.Fatalf("ServeToolchainResolve: %v", err)
		}
		rendered = append(rendered, stdout.String())
	}
	if rendered[0] != rendered[1] {
		t.Fatalf("one answer produced two documents:\n%s\n%s", rendered[0], rendered[1])
	}
	if strings.Index(rendered[0], `"archives"`) > strings.Index(rendered[0], `"integrities"`) {
		t.Fatalf("a pin writes integrities before archives:\n%s", rendered[0])
	}
}
