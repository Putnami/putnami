package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestIsProbeInvocationMatchesOnlyTheReservedCall(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"reserved call", []string{ProbeControlVerb, ProbeControlCommand}, true},
		{"ordinary task named like the control command", []string{ProbeControlCommand}, false},
		{"verb alone", []string{ProbeControlVerb}, false},
		{"extra arguments", []string{ProbeControlVerb, ProbeControlCommand, "--json"}, false},
		{"nothing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProbeInvocation(tc.args); got != tc.want {
				t.Fatalf("IsProbeInvocation(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestProbeControlArgsIsTheSpawnShape(t *testing.T) {
	args := ProbeControlArgs()
	if !IsProbeInvocation(args) {
		t.Fatalf("ProbeControlArgs() = %q, which IsProbeInvocation rejects", args)
	}
	// A caller that mutates the returned slice must not corrupt the next one.
	args[0] = "mutated"
	if !IsProbeInvocation(ProbeControlArgs()) {
		t.Fatal("ProbeControlArgs() returned a shared slice a caller could poison")
	}
}

func TestServeProbeRoundTrip(t *testing.T) {
	var request bytes.Buffer
	if err := EncodeProbeRequest(&request, ProbeRequest{
		Extension: "@putnami/typescript",
		Reason:    ProbeReasonLoad,
		Paths:     []string{"apps/web", "libs/core"},
	}); err != nil {
		t.Fatalf("EncodeProbeRequest: %v", err)
	}

	var stdout bytes.Buffer
	var seen ProbeRequest
	handled, err := ServeProbe(ProbeControlArgs(), &request, &stdout, func(r ProbeRequest) (ProbeResult, error) {
		seen = r
		return ProbeResult{
			Projects: []ProbeProject{
				// Deliberately unsorted and un-normalized: ServeProbe must
				// canonicalize before the bytes leave the process, or two
				// providers over one tree would digest differently.
				{Path: "libs/core", SourceName: "@acme/core", SourceFile: "libs/core/package.json"},
				{Path: "./apps/web/", SourceName: "@acme/web", Dependencies: []string{"libs/core"}},
			},
		}, nil
	})
	if !handled {
		t.Fatal("ServeProbe did not handle the reserved invocation")
	}
	if err != nil {
		t.Fatalf("ServeProbe: %v", err)
	}

	if seen.Version != ProbeProtocolVersion || seen.Extension != "@putnami/typescript" {
		t.Fatalf("provider saw %+v, want the encoded request", seen)
	}

	result, diags := ParseAndValidateProbeResult(stdout.Bytes())
	if result == nil || len(diags) > 0 {
		t.Fatalf("result does not conform: %v", diags)
	}
	if result.Extension != "@putnami/typescript" {
		t.Fatalf("result extension = %q; ServeProbe must attribute an unattributed answer", result.Extension)
	}
	if len(result.Projects) != 2 || result.Projects[0].Path != "apps/web" || result.Projects[1].Path != "libs/core" {
		t.Fatalf("projects = %+v, want normalized and sorted by path", result.Projects)
	}
}

func TestServeProbeDeclinesOrdinarySubcommands(t *testing.T) {
	var stdout bytes.Buffer
	handled, err := ServeProbe([]string{"build"}, strings.NewReader("{}"), &stdout, func(ProbeRequest) (ProbeResult, error) {
		t.Fatal("answer must not run for an ordinary subcommand")
		return ProbeResult{}, nil
	})
	if handled || err != nil {
		t.Fatalf("ServeProbe(build) = (%v, %v), want (false, nil)", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeProbe wrote %q for an unhandled call", stdout.String())
	}
}

func TestServeProbeRejectsAnUnreadableRequest(t *testing.T) {
	// A request with a member this provider's protocol version does not know is
	// a question it cannot fully read; answering it anyway would answer a
	// different question than the one core asked.
	var stdout bytes.Buffer
	handled, err := ServeProbe(ProbeControlArgs(),
		strings.NewReader(`{"version":1,"extension":"x","futureMember":true}`), &stdout,
		func(ProbeRequest) (ProbeResult, error) {
			t.Fatal("answer must not run for a rejected request")
			return ProbeResult{}, nil
		})
	if !handled {
		t.Fatal("ServeProbe must handle the reserved invocation even when the request is bad")
	}
	if err == nil {
		t.Fatal("expected an error for an unknown request member")
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeProbe wrote %q for a rejected request; stdout must stay empty", stdout.String())
	}
}

func TestServeProbeWritesNothingWhenTheProviderFails(t *testing.T) {
	var request bytes.Buffer
	if err := EncodeProbeRequest(&request, ProbeRequest{Extension: "x"}); err != nil {
		t.Fatalf("EncodeProbeRequest: %v", err)
	}
	var stdout bytes.Buffer
	sentinel := errors.New("toolchain unavailable")
	handled, err := ServeProbe(ProbeControlArgs(), &request, &stdout, func(ProbeRequest) (ProbeResult, error) {
		return ProbeResult{Projects: []ProbeProject{{Path: "a"}}}, sentinel
	})
	if !handled || !errors.Is(err, sentinel) {
		t.Fatalf("ServeProbe = (%v, %v), want the provider's own error", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeProbe wrote %q after a provider failure; a half-truth must never be merged", stdout.String())
	}
}

func TestServeProbeRejectsAnAnswerTheContractRejects(t *testing.T) {
	var request bytes.Buffer
	if err := EncodeProbeRequest(&request, ProbeRequest{Extension: "x"}); err != nil {
		t.Fatalf("EncodeProbeRequest: %v", err)
	}
	var stdout bytes.Buffer
	handled, err := ServeProbe(ProbeControlArgs(), &request, &stdout, func(ProbeRequest) (ProbeResult, error) {
		// An absolute path would make the digest depend on the checkout location.
		return ProbeResult{Projects: []ProbeProject{{Path: "/etc/passwd"}}}, nil
	})
	if !handled || err == nil {
		t.Fatalf("ServeProbe = (%v, %v), want a contract rejection", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("ServeProbe wrote %q for a rejected answer", stdout.String())
	}
}

func TestDecodeProbeDocumentRejectsTrailingData(t *testing.T) {
	// A provider that logs to stdout produces exactly this shape.
	_, err := DecodeProbeDocument(strings.NewReader(`{"version":1,"extension":"x"}` + "\ninfo: done\n"))
	if err == nil {
		t.Fatal("expected trailing data to be rejected")
	}
	if !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("error = %v, want it to name the trailing data", err)
	}
}

func TestDecodeProbeDocumentRejectsEmptyAndOversized(t *testing.T) {
	if _, err := DecodeProbeDocument(strings.NewReader("   \n")); err == nil {
		t.Fatal("expected an empty document to be rejected")
	}

	oversized := bytes.Repeat([]byte("a"), MaxProbeDocumentBytes+1)
	if _, err := DecodeProbeDocument(bytes.NewReader(oversized)); err == nil {
		t.Fatal("expected an oversized document to be rejected")
	}
}

func TestEncodeProbeRequestAlwaysStampsTheProtocolVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeProbeRequest(&buf, ProbeRequest{Version: 99, Extension: "x"}); err != nil {
		t.Fatalf("EncodeProbeRequest: %v", err)
	}
	var decoded ProbeRequest
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Version != ProbeProtocolVersion {
		t.Fatalf("version = %d, want %d: core stamps the wire version, callers do not", decoded.Version, ProbeProtocolVersion)
	}
}

// The exchange must be byte-stable: the same facts, however authored, must
// produce the same document, because that document's digest keys the snapshot.
func TestServeProbeIsByteStableAcrossAuthoringOrder(t *testing.T) {
	answers := []ProbeResult{
		{Projects: []ProbeProject{
			{Path: "b", Tags: []string{"z", "a"}},
			{Path: "a", Tags: []string{"a"}},
		}},
		{Projects: []ProbeProject{
			{Path: "./a/", Tags: []string{"a", "a"}},
			{Path: "b", Tags: []string{"a", "z"}},
		}},
	}
	rendered := make([]string, 0, len(answers))
	for _, answer := range answers {
		var request bytes.Buffer
		if err := EncodeProbeRequest(&request, ProbeRequest{Extension: "x"}); err != nil {
			t.Fatalf("EncodeProbeRequest: %v", err)
		}
		var stdout bytes.Buffer
		if _, err := ServeProbe(ProbeControlArgs(), &request, &stdout, func(ProbeRequest) (ProbeResult, error) {
			return answer, nil
		}); err != nil {
			t.Fatalf("ServeProbe: %v", err)
		}
		rendered = append(rendered, stdout.String())
	}
	if rendered[0] != rendered[1] {
		t.Fatalf("authoring order changed the document:\n%s\n%s", rendered[0], rendered[1])
	}
}

// dependencySources is a negotiated member. A core that did not ask may
// predate it and strict-decodes the answer, so the provider side strips it
// from every answer to a request that did not set the request member — the
// provider does not have to know which core it is talking to.
func TestServeProbe_AnswersDependencySourcesOnlyWhenAsked(t *testing.T) {
	answer := func(ProbeRequest) (ProbeResult, error) {
		return ProbeResult{Projects: []ProbeProject{{
			Path:              "apps/web",
			SourceName:        "@acme/web",
			Dependencies:      []string{"libs/core"},
			DependencySources: map[string]DependencySource{"libs/core": DependencySourceDeclared},
		}}}, nil
	}
	for _, asked := range []bool{false, true} {
		var request bytes.Buffer
		if err := EncodeProbeRequest(&request, ProbeRequest{
			Extension:         "@putnami/typescript",
			Paths:             []string{"apps/web", "libs/core"},
			DependencySources: asked,
		}); err != nil {
			t.Fatalf("EncodeProbeRequest: %v", err)
		}
		var stdout bytes.Buffer
		if _, err := ServeProbe(ProbeControlArgs(), &request, &stdout, answer); err != nil {
			t.Fatalf("ServeProbe(asked=%t): %v", asked, err)
		}
		if got := strings.Contains(stdout.String(), `"dependencySources"`); got != asked {
			t.Errorf("asked=%t: answer carries dependencySources=%t:\n%s", asked, got, stdout.String())
		}
		if !asked {
			// The answer to a request that did not ask is the v1 shape: a
			// strict decoder built before the member must accept it.
			var frozen struct {
				Version   int    `json:"version"`
				Extension string `json:"extension"`
				Projects  []struct {
					Path         string   `json:"path"`
					SourceName   string   `json:"sourceName"`
					Dependencies []string `json:"dependencies"`
				} `json:"projects"`
			}
			decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&frozen); err != nil {
				t.Errorf("a pre-member strict decoder rejects the unasked answer: %v", err)
			}
		}
	}
}
