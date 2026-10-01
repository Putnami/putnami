package distribution

import (
	"encoding/json"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func mustRef(t *testing.T, releaseSet *ReleaseSet) ReleaseSetRef {
	t.Helper()
	ref, diagnostics := DeriveReleaseSetRef(releaseSet)
	if diag.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	return ref
}

func strictJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func otherRef() ReleaseSetRef {
	return ReleaseSetRef{ID: "rs_" + strings.Repeat("d", 64), Digest: "sha256:" + strings.Repeat("d", 64)}
}

// visibilityChain is the chain this repository declares: internal everywhere,
// public for stable versions, no per-publication override.
func visibilityChain() VisibilityChain {
	return VisibilityChain{
		Repo:       VisibilityInternal,
		Registries: map[string]Visibility{"npm": VisibilityInternal, "oci": VisibilityInternal},
		Versions:   VersionVisibility{Stable: VisibilityPublic, Prerelease: VisibilityInternal},
		Set:        nil,
		Members:    []MemberVisibility{{Ecosystem: "npm", Coordinate: "@putnami/web", Visibility: VisibilityPublic}},
	}
}

func TestResolveContractAndExchange(t *testing.T) {
	releaseSet := representativeSet()
	ref := mustRef(t, releaseSet)
	head := &ChannelHead{Ref: ref, Generation: 42, ReleaseSet: releaseSet}
	request := ResolveRequest{ProtocolVersion: ProtocolVersion, Namespace: "putnami", Channels: []string{"canary", "staging"}}
	response := ResolveResponse{ProtocolVersion: ProtocolVersion, Heads: map[string]*ChannelHead{"canary": head, "staging": nil}}

	parsedRequest, diagnostics := ParseAndValidateResolveRequest(strictJSON(t, request))
	if parsedRequest == nil || diag.HasErrors(diagnostics) || len(parsedRequest.Channels) != 2 {
		t.Fatalf("resolve request rejected: %#v %v", parsedRequest, diagnostics)
	}
	if got := string(strictJSON(t, request)); got != `{"protocolVersion":2,"namespace":"putnami","channels":["canary","staging"]}` {
		t.Fatalf("channel resolve wire drifted: %s", got)
	}
	parsedResponse, diagnostics := ParseAndValidateResolveResponse(strictJSON(t, response))
	if parsedResponse == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("resolve response rejected: %v", diagnostics)
	}
	// An empty channel is a complete answer, not a failure.
	if staging, answered := parsedResponse.Heads["staging"]; !answered || staging != nil {
		t.Fatalf("null head lost: %#v", parsedResponse.Heads)
	}
	if parsedResponse.Heads["canary"].ReleaseSet.Members[0].Ecosystem != "archive" {
		t.Fatal("nested release set was not normalized")
	}
	if parsedResponse.Heads["canary"].Generation != 42 {
		t.Fatalf("generation lost: %d", parsedResponse.Heads["canary"].Generation)
	}
	if !strings.Contains(string(strictJSON(t, response)), `"staging":null`) {
		t.Fatalf("an empty channel must be explicit null on the wire: %s", strictJSON(t, response))
	}
	if diagnostics := ValidateResolveExchange(&request, &response); diag.HasErrors(diagnostics) {
		t.Fatalf("valid resolve exchange rejected: %v", diagnostics)
	}

	// A head that exists carries a generation of at least 1.
	generationZero := ResolveResponse{ProtocolVersion: ProtocolVersion, Heads: map[string]*ChannelHead{"canary": {Ref: ref, Generation: 0, ReleaseSet: releaseSet}}}
	if diagnostics := ValidateResolveResponse(&generationZero); !findDiagnostic(diagnostics, ErrorCodeInvalidGeneration, "heads.canary.generation") {
		t.Fatalf("generation 0 accepted for a present head: %v", diagnostics)
	}
	withoutSet := ResolveResponse{ProtocolVersion: ProtocolVersion, Heads: map[string]*ChannelHead{"canary": {Ref: ref, Generation: 1}}}
	if diagnostics := ValidateResolveResponse(&withoutSet); !findDiagnostic(diagnostics, ErrorCodeMissingField, "heads.canary.releaseSet") {
		t.Fatalf("a resolved head without its snapshot accepted: %v", diagnostics)
	}
	mismatched := ResolveResponse{ProtocolVersion: ProtocolVersion, Heads: map[string]*ChannelHead{"canary": {Ref: otherRef(), Generation: 1, ReleaseSet: releaseSet}}}
	if diagnostics := ValidateResolveResponse(&mismatched); !findDiagnostic(diagnostics, ErrorCodeRefMismatch, "heads.canary.ref") {
		t.Fatalf("head ref that does not recompute from the set accepted: %v", diagnostics)
	}

	// The answer names exactly the requested channels.
	extra := ResolveResponse{ProtocolVersion: ProtocolVersion, Heads: map[string]*ChannelHead{"canary": head, "staging": nil, "latest": nil}}
	if diagnostics := ValidateResolveExchange(&request, &extra); !findDiagnostic(diagnostics, ErrorCodeUnknownField, "heads.latest") {
		t.Fatalf("unrequested channel accepted: %v", diagnostics)
	}
	partial := ResolveResponse{ProtocolVersion: ProtocolVersion, Heads: map[string]*ChannelHead{"canary": head}}
	if diagnostics := ValidateResolveExchange(&request, &partial); !findDiagnostic(diagnostics, ErrorCodeMissingField, "heads.staging") {
		t.Fatalf("unanswered channel accepted: %v", diagnostics)
	}
	crossNamespace := request
	crossNamespace.Channels = []string{"canary"}
	crossNamespace.Namespace = "other"
	if diagnostics := ValidateResolveExchange(&crossNamespace, &partial); !findDiagnostic(diagnostics, ErrorCodeRefMismatch, "heads.canary.releaseSet.namespace") {
		t.Fatalf("cross-namespace resolve accepted: %v", diagnostics)
	}

	// A release-id selector answers with release, never with heads, and its
	// generation is 0 because an immutable set is named by no channel.
	byID := ResolveRequest{ProtocolVersion: ProtocolVersion, Namespace: "putnami", ReleaseID: ref.ID}
	release := ResolveResponse{ProtocolVersion: ProtocolVersion, Release: &ChannelHead{Ref: ref, ReleaseSet: releaseSet}}
	if parsed, diagnostics := ParseAndValidateResolveResponse(strictJSON(t, release)); parsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("release answer rejected: %v", diagnostics)
	}
	if diagnostics := ValidateResolveExchange(&byID, &release); diag.HasErrors(diagnostics) {
		t.Fatalf("valid releaseId exchange rejected: %v", diagnostics)
	}
	if diagnostics := ValidateResolveExchange(&byID, &response); !findDiagnostic(diagnostics, ErrorCodeRefMismatch, "release") {
		t.Fatalf("releaseId answered with channel heads accepted: %v", diagnostics)
	}
	stamped := ResolveResponse{ProtocolVersion: ProtocolVersion, Release: &ChannelHead{Ref: ref, Generation: 3, ReleaseSet: releaseSet}}
	if diagnostics := ValidateResolveResponse(&stamped); !findDiagnostic(diagnostics, ErrorCodeInvalidGeneration, "release.generation") {
		t.Fatalf("a stamped generation on an immutable release accepted: %v", diagnostics)
	}
	otherID := byID
	otherID.ReleaseID = otherRef().ID
	if diagnostics := ValidateResolveExchange(&otherID, &release); !findDiagnostic(diagnostics, ErrorCodeRefMismatch, "release.ref.id") {
		t.Fatalf("different releaseId accepted: %v", diagnostics)
	}

	both := ResolveResponse{ProtocolVersion: ProtocolVersion, Heads: map[string]*ChannelHead{"canary": head}, Release: &ChannelHead{Ref: ref, ReleaseSet: releaseSet}}
	if diagnostics := ValidateResolveResponse(&both); !findDiagnostic(diagnostics, ErrorCodeInvalidSelector, "") {
		t.Fatalf("an answer carrying both heads and release accepted: %v", diagnostics)
	}
	neither := ResolveResponse{ProtocolVersion: ProtocolVersion}
	if diagnostics := ValidateResolveResponse(&neither); !findDiagnostic(diagnostics, ErrorCodeInvalidSelector, "") {
		t.Fatalf("an empty answer accepted: %v", diagnostics)
	}
	if diagnostics := ValidateResolveResponse(&ResolveResponse{ProtocolVersion: 1, Heads: map[string]*ChannelHead{"canary": nil}}); !findDiagnostic(diagnostics, ErrorCodeInvalidProtocolVersion, "protocolVersion") {
		t.Fatalf("a version-1 envelope accepted: %v", diagnostics)
	}
}

func TestResolveSelectorIsStrictExclusiveAndCanonical(t *testing.T) {
	validID := "rs_" + strings.Repeat("a", 64)
	tests := []struct {
		name  string
		input string
		code  string
		field string
	}{
		{"missing both", `{"protocolVersion":2,"namespace":"putnami"}`, ErrorCodeInvalidSelector, ""},
		{"both", `{"protocolVersion":2,"namespace":"putnami","channels":["canary"],"releaseId":"` + validID + `"}`, ErrorCodeInvalidSelector, ""},
		{"release id overloaded as a channel", `{"protocolVersion":2,"namespace":"putnami","channels":["` + validID + `"]}`, ErrorCodeInvalidSelector, "channels[0]"},
		{"empty channel list", `{"protocolVersion":2,"namespace":"putnami","channels":[]}`, ErrorCodeBoundsExceeded, "channels"},
		{"empty channel name", `{"protocolVersion":2,"namespace":"putnami","channels":[""]}`, ErrorCodeInvalidChannel, "channels[0]"},
		{"non-portable channel", `{"protocolVersion":2,"namespace":"putnami","channels":["branches/feature"]}`, ErrorCodeInvalidChannel, "channels[0]"},
		{"duplicate channel", `{"protocolVersion":2,"namespace":"putnami","channels":["canary","canary"]}`, ErrorCodeDuplicateField, "channels[1]"},
		{"empty release id", `{"protocolVersion":2,"namespace":"putnami","releaseId":""}`, ErrorCodeInvalidRef, "releaseId"},
		{"empty list plus release id is still both", `{"protocolVersion":2,"namespace":"putnami","channels":[],"releaseId":"` + validID + `"}`, ErrorCodeInvalidSelector, ""},
		{"uppercase release id", `{"protocolVersion":2,"namespace":"putnami","releaseId":"rs_` + strings.Repeat("A", 64) + `"}`, ErrorCodeInvalidRef, "releaseId"},
		{"digest is not an id", `{"protocolVersion":2,"namespace":"putnami","releaseId":"sha256:` + strings.Repeat("a", 64) + `"}`, ErrorCodeInvalidRef, "releaseId"},
		{"null release id", `{"protocolVersion":2,"namespace":"putnami","releaseId":null}`, ErrorCodeNullField, "releaseId"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, diagnostics := ParseAndValidateResolveRequest([]byte(test.input))
			if parsed != nil || !findDiagnostic(diagnostics, test.code, test.field) {
				t.Fatalf("want %s at %q, got %#v %v", test.code, test.field, parsed, diagnostics)
			}
		})
	}

	programmaticBoth := &ResolveRequest{ProtocolVersion: ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"}, ReleaseID: validID}
	if diagnostics := ValidateResolveRequest(programmaticBoth); !findDiagnostic(diagnostics, ErrorCodeInvalidSelector, "") {
		t.Fatalf("programmatic dual selector accepted: %v", diagnostics)
	}
	tooMany := &ResolveRequest{ProtocolVersion: ProtocolVersion, Namespace: "putnami", Channels: make([]string, MaxChannelsPerRelease+1)}
	if diagnostics := ValidateResolveRequest(tooMany); !findDiagnostic(diagnostics, ErrorCodeBoundsExceeded, "channels") {
		t.Fatalf("channel bound not enforced: %v", diagnostics)
	}
}

func TestReleaseContractBindsSetChannelsAndVisibility(t *testing.T) {
	releaseSet := representativeSet()
	ref := mustRef(t, releaseSet)
	request := ReleaseRequest{
		ProtocolVersion: ProtocolVersion,
		Namespace:       "putnami",
		ReleaseSet:      *releaseSet,
		Channels: []ChannelRequest{
			{Name: "canary", Expected: nil, Visibility: VisibilityInternal},
			{Name: "ts-v0.3.0", Expected: nil, Visibility: VisibilityInternal, Immutable: true},
		},
		Visibility: visibilityChain(),
	}
	parsedRequest, diagnostics := ParseAndValidateReleaseRequest(strictJSON(t, request))
	if parsedRequest == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("release request rejected: %v", diagnostics)
	}
	if parsedRequest.Channels[0].Expected != nil || parsedRequest.ReleaseSet.Members[0].Ecosystem != "archive" {
		t.Fatalf("release request not normalized: %#v", parsedRequest)
	}
	wire := string(strictJSON(t, request))
	if !strings.Contains(wire, `"expected":null`) || !strings.Contains(wire, `"set":null`) {
		t.Fatalf("expected and set must be explicit null on the wire: %s", wire)
	}
	if !strings.Contains(wire, `"immutable":true`) {
		t.Fatalf("an immutable channel must be explicit on the wire: %s", wire)
	}

	current := map[string]*ChannelHead{"canary": {Ref: ref, Generation: 42}, "ts-v0.3.0": {Ref: ref, Generation: 1}}
	response := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeReleased, Current: current}
	parsedResponse, diagnostics := ParseAndValidateReleaseResponse(strictJSON(t, response))
	if parsedResponse == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("release response rejected: %v", diagnostics)
	}
	if diagnostics := ValidateReleaseExchange(&request, &response); diag.HasErrors(diagnostics) {
		t.Fatalf("valid release exchange rejected: %v", diagnostics)
	}
	alreadyCurrent := response
	alreadyCurrent.Outcome = ReleaseOutcomeAlreadyCurrent
	if diagnostics := ValidateReleaseExchange(&request, &alreadyCurrent); diag.HasErrors(diagnostics) {
		t.Fatalf("already-current exchange rejected: %v", diagnostics)
	}

	// A release answer reports pointers only, with a generation of at least 1.
	withSet := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeReleased, Current: map[string]*ChannelHead{"canary": {Ref: ref, Generation: 1, ReleaseSet: releaseSet}, "ts-v0.3.0": {Ref: ref, Generation: 1}}}
	if diagnostics := ValidateReleaseResponse(&withSet); !findDiagnostic(diagnostics, ErrorCodeFieldNotAllowed, "current.canary.releaseSet") {
		t.Fatalf("a release answer carrying the set accepted: %v", diagnostics)
	}
	zeroGeneration := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeReleased, Current: map[string]*ChannelHead{"canary": {Ref: ref}, "ts-v0.3.0": {Ref: ref, Generation: 1}}}
	if diagnostics := ValidateReleaseResponse(&zeroGeneration); !findDiagnostic(diagnostics, ErrorCodeInvalidGeneration, "current.canary.generation") {
		t.Fatalf("generation 0 accepted after a release: %v", diagnostics)
	}
	nullHead := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeReleased, Current: map[string]*ChannelHead{"canary": nil, "ts-v0.3.0": {Ref: ref, Generation: 1}}}
	if diagnostics := ValidateReleaseResponse(&nullHead); !findDiagnostic(diagnostics, ErrorCodeMissingField, "current.canary") {
		t.Fatalf("released with a null head accepted: %v", diagnostics)
	}
	drifted := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeReleased, Current: map[string]*ChannelHead{"canary": {Ref: otherRef(), Generation: 1}, "ts-v0.3.0": {Ref: ref, Generation: 1}}}
	if diagnostics := ValidateReleaseExchange(&request, &drifted); !findDiagnostic(diagnostics, ErrorCodeRefMismatch, "current.canary") {
		t.Fatalf("released with a different head accepted: %v", diagnostics)
	}

	// The answer names exactly the requested channels.
	partial := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeReleased, Current: map[string]*ChannelHead{"canary": {Ref: ref, Generation: 1}}}
	if diagnostics := ValidateReleaseExchange(&request, &partial); !findDiagnostic(diagnostics, ErrorCodeMissingField, `current.ts-v0.3.0`) {
		t.Fatalf("release that skipped a channel accepted: %v", diagnostics)
	}
	extra := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeReleased, Current: map[string]*ChannelHead{"canary": {Ref: ref, Generation: 1}, "ts-v0.3.0": {Ref: ref, Generation: 1}, "latest": {Ref: ref, Generation: 1}}}
	if diagnostics := ValidateReleaseExchange(&request, &extra); !findDiagnostic(diagnostics, ErrorCodeUnknownField, "current.latest") {
		t.Fatalf("release that moved an unrequested channel accepted: %v", diagnostics)
	}

	// A conflict writes nothing and names at least one head that differs.
	conflict := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeConflict, Current: map[string]*ChannelHead{"canary": {Ref: otherRef(), Generation: 7}, "ts-v0.3.0": nil}}
	if diagnostics := ValidateReleaseExchange(&request, &conflict); diag.HasErrors(diagnostics) {
		t.Fatalf("conflict with a foreign head rejected: %v", diagnostics)
	}
	falseConflict := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeConflict, Current: map[string]*ChannelHead{"canary": nil, "ts-v0.3.0": nil}}
	if diagnostics := ValidateReleaseExchange(&request, &falseConflict); !findDiagnostic(diagnostics, ErrorCodeInvalidOutcome, "outcome") {
		t.Fatalf("every head equal to expected is not a conflict: %v", diagnostics)
	}

	// A set already expected on every channel is already-current, never released.
	noop := request
	noop.Channels = []ChannelRequest{{Name: "canary", Expected: &ref, Visibility: VisibilityInternal}}
	noopResponse := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeReleased, Current: map[string]*ChannelHead{"canary": {Ref: ref, Generation: 42}}}
	if diagnostics := ValidateReleaseExchange(&noop, &noopResponse); !findDiagnostic(diagnostics, ErrorCodeInvalidOutcome, "outcome") {
		t.Fatalf("a no-op accepted as released: %v", diagnostics)
	}

	// Request-level rules.
	cases := []struct {
		name   string
		mutate func(*ReleaseRequest)
		code   string
		field  string
	}{
		{"no channel", func(r *ReleaseRequest) { r.Channels = nil }, ErrorCodeBoundsExceeded, "channels"},
		{"non-portable channel", func(r *ReleaseRequest) { r.Channels[0].Name = "branches/feature" }, ErrorCodeInvalidChannel, "channels[0].name"},
		{"duplicate channel", func(r *ReleaseRequest) { r.Channels[1].Name = "canary" }, ErrorCodeDuplicateField, "channels[1].name"},
		{"unknown channel visibility", func(r *ReleaseRequest) { r.Channels[0].Visibility = "secret" }, ErrorCodeInvalidVisibility, "channels[0].visibility"},
		{"malformed expected", func(r *ReleaseRequest) { r.Channels[0].Expected = &ReleaseSetRef{ID: "rs_bad"} }, ErrorCodeInvalidRef, "channels[0].expected.id"},
		{"namespace mismatch", func(r *ReleaseRequest) { r.Namespace = "other" }, ErrorCodeRefMismatch, "releaseSet.namespace"},
		{"unknown repo visibility", func(r *ReleaseRequest) { r.Visibility.Repo = "secret" }, ErrorCodeInvalidVisibility, "visibility.repo"},
		{"bad registry kind", func(r *ReleaseRequest) { r.Visibility.Registries = map[string]Visibility{"NPM": VisibilityPublic} }, ErrorCodeInvalidEcosystem, "visibility.registries.NPM"},
		{"unknown registry visibility", func(r *ReleaseRequest) { r.Visibility.Registries = map[string]Visibility{"npm": "secret"} }, ErrorCodeInvalidVisibility, "visibility.registries.npm"},
		{"unknown stable visibility", func(r *ReleaseRequest) { r.Visibility.Versions.Stable = "secret" }, ErrorCodeInvalidVisibility, "visibility.versions.stable"},
		{"unknown prerelease visibility", func(r *ReleaseRequest) { r.Visibility.Versions.Prerelease = "secret" }, ErrorCodeInvalidVisibility, "visibility.versions.prerelease"},
		{"unknown set visibility", func(r *ReleaseRequest) { level := Visibility("secret"); r.Visibility.Set = &level }, ErrorCodeInvalidVisibility, "visibility.set"},
		{"member visibility outside the set", func(r *ReleaseRequest) {
			r.Visibility.Members = []MemberVisibility{{Ecosystem: "npm", Coordinate: "@putnami/absent", Visibility: VisibilityPublic}}
		}, ErrorCodeUnclosedDependency, "visibility.members[0]"},
		{"unknown member visibility", func(r *ReleaseRequest) { r.Visibility.Members[0].Visibility = "secret" }, ErrorCodeInvalidVisibility, "visibility.members[0].visibility"},
		{"version 1 envelope", func(r *ReleaseRequest) { r.ProtocolVersion = 1 }, ErrorCodeInvalidProtocolVersion, "protocolVersion"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mutated := ReleaseRequest{
				ProtocolVersion: ProtocolVersion, Namespace: "putnami", ReleaseSet: *representativeSet(),
				Channels: []ChannelRequest{
					{Name: "canary", Expected: nil, Visibility: VisibilityInternal},
					{Name: "ts-v0.3.0", Expected: nil, Visibility: VisibilityInternal},
				},
				Visibility: visibilityChain(),
			}
			test.mutate(&mutated)
			if diagnostics := ValidateReleaseRequest(&mutated); !findDiagnostic(diagnostics, test.code, test.field) {
				t.Fatalf("want %s at %s, got %v", test.code, test.field, diagnostics)
			}
		})
	}

	t.Run("too many registry kinds", func(t *testing.T) {
		mutated := request
		mutated.Visibility.Registries = map[string]Visibility{}
		for index := 0; index <= MaxRegistryKinds; index++ {
			mutated.Visibility.Registries["eco"+strings.Repeat("x", index)] = VisibilityInternal
		}
		if diagnostics := ValidateReleaseRequest(&mutated); !findDiagnostic(diagnostics, ErrorCodeBoundsExceeded, "visibility.registries") {
			t.Fatalf("registry-kind bound not enforced: %v", diagnostics)
		}
	})

	if _, diagnostics := ParseAndValidateReleaseResponse([]byte(`{"protocolVersion":2,"outcome":"advanced","current":{}}`)); !diag.HasErrors(diagnostics) {
		t.Fatal("a version-1 advance outcome accepted as a release outcome")
	}
	oversized := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcome(strings.Repeat("x", MaxTokenBytes+1)), Current: current}
	if diagnostics := ValidateReleaseResponse(&oversized); !findDiagnostic(diagnostics, ErrorCodeBoundsExceeded, "outcome") {
		t.Fatalf("oversized outcome accepted: %v", diagnostics)
	}
	if diagnostics := ValidateReleaseExchange(nil, &response); !diag.HasErrors(diagnostics) {
		t.Fatal("nil release request accepted")
	}
	if diagnostics := ValidateReleaseExchange(&request, nil); !diag.HasErrors(diagnostics) {
		t.Fatal("nil release response accepted")
	}
}

func TestChannelSetContract(t *testing.T) {
	ref := mustRef(t, representativeSet())
	request := ChannelSetRequest{
		ProtocolVersion: ProtocolVersion, Namespace: "putnami", Channel: "latest",
		Expected: &ref, From: ChannelSource{Channel: "ts-v0.3.0"},
	}
	parsed, diagnostics := ParseAndValidateChannelSetRequest(strictJSON(t, request))
	if parsed == nil || diag.HasErrors(diagnostics) || parsed.From.Channel != "ts-v0.3.0" {
		t.Fatalf("channel-set request rejected: %#v %v", parsed, diagnostics)
	}
	byID := request
	byID.Expected = nil
	byID.From = ChannelSource{ReleaseID: ref.ID}
	if parsed, diagnostics := ParseAndValidateChannelSetRequest(strictJSON(t, byID)); parsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("channel-set from a release id rejected: %v", diagnostics)
	}
	if !strings.Contains(string(strictJSON(t, byID)), `"expected":null`) {
		t.Fatalf("expected must be explicit null on the wire: %s", strictJSON(t, byID))
	}

	tests := []struct {
		name  string
		input string
		code  string
		field string
	}{
		{"no source", `{"protocolVersion":2,"namespace":"putnami","channel":"latest","expected":null,"from":{}}`, ErrorCodeInvalidSelector, "from"},
		{"both sources", `{"protocolVersion":2,"namespace":"putnami","channel":"latest","expected":null,"from":{"channel":"canary","releaseId":"` + ref.ID + `"}}`, ErrorCodeInvalidSelector, "from"},
		{"empty source channel plus release id", `{"protocolVersion":2,"namespace":"putnami","channel":"latest","expected":null,"from":{"channel":"","releaseId":"` + ref.ID + `"}}`, ErrorCodeInvalidSelector, "from"},
		{"release id as a source channel", `{"protocolVersion":2,"namespace":"putnami","channel":"latest","expected":null,"from":{"channel":"` + ref.ID + `"}}`, ErrorCodeInvalidSelector, "from.channel"},
		{"malformed source release id", `{"protocolVersion":2,"namespace":"putnami","channel":"latest","expected":null,"from":{"releaseId":"rs_bad"}}`, ErrorCodeInvalidRef, "from.releaseId"},
		{"non-portable target", `{"protocolVersion":2,"namespace":"putnami","channel":"release/latest","expected":null,"from":{"channel":"canary"}}`, ErrorCodeInvalidChannel, "channel"},
		{"non-portable source", `{"protocolVersion":2,"namespace":"putnami","channel":"latest","expected":null,"from":{"channel":"release/canary"}}`, ErrorCodeInvalidChannel, "from.channel"},
		{"version 1 envelope", `{"protocolVersion":1,"namespace":"putnami","channel":"latest","expected":null,"from":{"channel":"canary"}}`, ErrorCodeInvalidProtocolVersion, "protocolVersion"},
		{"missing expected", `{"protocolVersion":2,"namespace":"putnami","channel":"latest","from":{"channel":"canary"}}`, ErrorCodeMissingField, "expected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, diagnostics := ParseAndValidateChannelSetRequest([]byte(test.input))
			if parsed != nil || !findDiagnostic(diagnostics, test.code, test.field) {
				t.Fatalf("want %s at %s, got %#v %v", test.code, test.field, parsed, diagnostics)
			}
		})
	}

	// channel-set answers with a release response keyed by the one target
	// channel. Reporting the expected head is a no-op, therefore
	// already-current rather than released.
	response := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeAlreadyCurrent, Current: map[string]*ChannelHead{"latest": {Ref: ref, Generation: 8}}}
	if diagnostics := ValidateChannelSetExchange(&request, &response); diag.HasErrors(diagnostics) {
		t.Fatalf("valid channel-set exchange rejected: %v", diagnostics)
	}

	// A successful immutable-source move is bound to the exact source id.
	byIDResponse := ReleaseResponse{ProtocolVersion: ProtocolVersion, Outcome: ReleaseOutcomeReleased, Current: map[string]*ChannelHead{"latest": {Ref: ref, Generation: 9}}}
	if diagnostics := ValidateChannelSetExchange(&byID, &byIDResponse); diag.HasErrors(diagnostics) {
		t.Fatalf("valid channel-set from release id rejected: %v", diagnostics)
	}
	wrongSource := byIDResponse
	wrongSource.Current = map[string]*ChannelHead{"latest": {Ref: otherRef(), Generation: 9}}
	if diagnostics := ValidateChannelSetExchange(&byID, &wrongSource); !findDiagnostic(diagnostics, ErrorCodeRefMismatch, "current.latest") {
		t.Fatalf("channel-set success for another release id accepted: %v", diagnostics)
	}

	// An idempotent retry may observe the desired source after the original
	// expectation has become stale and is still already-current, not conflict.
	retry := byID
	other := otherRef()
	retry.Expected = &other
	retryResponse := byIDResponse
	retryResponse.Outcome = ReleaseOutcomeAlreadyCurrent
	if diagnostics := ValidateChannelSetExchange(&retry, &retryResponse); diag.HasErrors(diagnostics) {
		t.Fatalf("idempotent channel-set retry rejected: %v", diagnostics)
	}

	falseMove := response
	falseMove.Outcome = ReleaseOutcomeReleased
	if diagnostics := ValidateChannelSetExchange(&request, &falseMove); !findDiagnostic(diagnostics, ErrorCodeInvalidOutcome, "outcome") {
		t.Fatalf("unchanged head accepted as released: %v", diagnostics)
	}
	falseConflict := response
	falseConflict.Outcome = ReleaseOutcomeConflict
	if diagnostics := ValidateChannelSetExchange(&request, &falseConflict); !findDiagnostic(diagnostics, ErrorCodeInvalidOutcome, "outcome") {
		t.Fatalf("expected head accepted as conflict: %v", diagnostics)
	}
	extra := byIDResponse
	extra.Current = map[string]*ChannelHead{"latest": {Ref: ref, Generation: 9}, "canary": {Ref: ref, Generation: 1}}
	if diagnostics := ValidateChannelSetExchange(&byID, &extra); !findDiagnostic(diagnostics, ErrorCodeUnknownField, "current.canary") {
		t.Fatalf("unrequested channel accepted: %v", diagnostics)
	}
}

func TestChannelStatusContract(t *testing.T) {
	releaseSet := representativeSet()
	ref := mustRef(t, releaseSet)
	request := ChannelStatusRequest{ProtocolVersion: ProtocolVersion, Namespace: "putnami", Channel: "latest"}
	if parsed, diagnostics := ParseAndValidateChannelStatusRequest(strictJSON(t, request)); parsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("channel-status request rejected: %v", diagnostics)
	}
	response := ChannelStatusResponse{
		ProtocolVersion: ProtocolVersion,
		Desired:         &ChannelHead{Ref: ref, Generation: 7, ReleaseSet: releaseSet},
		Observed:        map[string]uint64{"npm": 7, "go": 7, "oci": 6, "put": 7},
	}
	parsed, diagnostics := ParseAndValidateChannelStatusResponse(strictJSON(t, response))
	if parsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("channel-status response rejected: %v", diagnostics)
	}
	// A registry lagging behind the desired generation is the point of the report.
	if parsed.Observed["oci"] >= parsed.Desired.Generation {
		t.Fatalf("observed generations lost: %#v", parsed.Observed)
	}
	if parsed.Desired.ReleaseSet.Members[0].Ecosystem != "archive" {
		t.Fatal("the desired head's set was not normalized")
	}

	empty := ChannelStatusResponse{ProtocolVersion: ProtocolVersion, Desired: nil}
	if got := string(strictJSON(t, empty)); got != `{"protocolVersion":2,"desired":null}` {
		t.Fatalf("empty channel status wire drifted: %s", got)
	}
	if parsed, diagnostics := ParseAndValidateChannelStatusResponse(strictJSON(t, empty)); parsed == nil || diag.HasErrors(diagnostics) || parsed.Desired != nil {
		t.Fatalf("a channel with no head rejected: %v", diagnostics)
	}

	badKind := response
	badKind.Observed = map[string]uint64{"NPM": 1}
	if diagnostics := ValidateChannelStatusResponse(&badKind); !findDiagnostic(diagnostics, ErrorCodeInvalidEcosystem, "observed.NPM") {
		t.Fatalf("a registry kind that is not an ecosystem id accepted: %v", diagnostics)
	}
	tooMany := response
	tooMany.Observed = map[string]uint64{}
	for index := 0; index <= MaxRegistryKinds; index++ {
		tooMany.Observed["eco"+strings.Repeat("x", index)] = 1
	}
	if diagnostics := ValidateChannelStatusResponse(&tooMany); !findDiagnostic(diagnostics, ErrorCodeBoundsExceeded, "observed") {
		t.Fatalf("registry-kind bound not enforced: %v", diagnostics)
	}
	zeroGeneration := response
	zeroGeneration.Desired = &ChannelHead{Ref: ref, Generation: 0, ReleaseSet: releaseSet}
	if diagnostics := ValidateChannelStatusResponse(&zeroGeneration); !findDiagnostic(diagnostics, ErrorCodeInvalidGeneration, "desired.generation") {
		t.Fatalf("generation 0 accepted for a desired head: %v", diagnostics)
	}
	if diagnostics := ValidateChannelStatusRequest(&ChannelStatusRequest{ProtocolVersion: ProtocolVersion, Namespace: "putnami", Channel: "release/latest"}); !findDiagnostic(diagnostics, ErrorCodeInvalidChannel, "channel") {
		t.Fatalf("a non-portable channel accepted: %v", diagnostics)
	}
}

func TestParseAndValidateChannelHeadIsStrict(t *testing.T) {
	releaseSet := representativeSet()
	ref := mustRef(t, releaseSet)
	head := ChannelHead{Ref: ref, Generation: 42, ReleaseSet: releaseSet}
	parsed, diagnostics := ParseAndValidateChannelHead(strictJSON(t, head))
	if parsed == nil || diag.HasErrors(diagnostics) || parsed.Generation != 42 {
		t.Fatalf("channel head rejected: %#v %v", parsed, diagnostics)
	}
	if parsed.ReleaseSet.Members[0].Ecosystem != "archive" {
		t.Fatal("the head's set was not normalized")
	}
	pointerOnly := ChannelHead{Ref: ref, Generation: 1}
	if got := string(strictJSON(t, pointerOnly)); strings.Contains(got, "releaseSet") {
		t.Fatalf("a pointer-only head must omit releaseSet: %s", got)
	}
	if parsed, diagnostics := ParseAndValidateChannelHead(strictJSON(t, pointerOnly)); parsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("a pointer-only head rejected: %v", diagnostics)
	}
	mismatched := ChannelHead{Ref: otherRef(), Generation: 1, ReleaseSet: releaseSet}
	if diagnostics := ValidateChannelHead(&mismatched); !findDiagnostic(diagnostics, ErrorCodeRefMismatch, "ref") {
		t.Fatalf("a head whose ref does not recompute accepted: %v", diagnostics)
	}
	if _, diagnostics := ParseAndValidateChannelHead([]byte(`{"ref":{"id":"` + ref.ID + `","digest":"` + ref.Digest + `"}}`)); !findDiagnostic(diagnostics, ErrorCodeMissingField, "generation") {
		t.Fatalf("a head without a generation accepted: %v", diagnostics)
	}
}

func TestPublishOutcomeIsStrictAndTyped(t *testing.T) {
	ref := mustRef(t, representativeSet())
	outcome := ReleaseSetPublishOutcome{
		ProtocolVersion: ProtocolVersion, Namespace: "putnami", Ref: ref,
		Channels: map[string]*ChannelHead{"canary": {Ref: ref, Generation: 42}, "ts-v0.3.0": {Ref: ref, Generation: 1}},
	}
	parsed, diagnostics := ParseAndValidateReleaseSetPublishOutcome(strictJSON(t, outcome))
	if parsed == nil || diag.HasErrors(diagnostics) || parsed.Channels["canary"].Generation != 42 {
		t.Fatalf("publish outcome rejected: %v", diagnostics)
	}
	tests := []struct {
		name   string
		mutate func(*ReleaseSetPublishOutcome)
		code   string
		field  string
	}{
		{"no channel", func(o *ReleaseSetPublishOutcome) { o.Channels = map[string]*ChannelHead{} }, ErrorCodeBoundsExceeded, "channels"},
		{"null head", func(o *ReleaseSetPublishOutcome) { o.Channels["canary"] = nil }, ErrorCodeMissingField, "channels.canary"},
		{"non-portable channel", func(o *ReleaseSetPublishOutcome) {
			o.Channels = map[string]*ChannelHead{"release/canary": {Ref: o.Ref, Generation: 1}}
		}, ErrorCodeInvalidChannel, "channels.release/canary"},
		{"generation 0", func(o *ReleaseSetPublishOutcome) { o.Channels["canary"].Generation = 0 }, ErrorCodeInvalidGeneration, "channels.canary.generation"},
		{"head of another set", func(o *ReleaseSetPublishOutcome) { o.Channels["canary"] = &ChannelHead{Ref: otherRef(), Generation: 1} }, ErrorCodeRefMismatch, "channels.canary.ref.id"},
		{"version 1 envelope", func(o *ReleaseSetPublishOutcome) { o.ProtocolVersion = 1 }, ErrorCodeInvalidProtocolVersion, "protocolVersion"},
		{"bad namespace", func(o *ReleaseSetPublishOutcome) { o.Namespace = "Putnami" }, ErrorCodeInvalidNamespace, "namespace"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := ReleaseSetPublishOutcome{
				ProtocolVersion: ProtocolVersion, Namespace: "putnami", Ref: ref,
				Channels: map[string]*ChannelHead{"canary": {Ref: ref, Generation: 42}},
			}
			test.mutate(&mutated)
			if diagnostics := ValidateReleaseSetPublishOutcome(&mutated); !findDiagnostic(diagnostics, test.code, test.field) {
				t.Fatalf("want %s at %s, got %v", test.code, test.field, diagnostics)
			}
		})
	}
	input := []byte(`{"protocolVersion":2,"namespace":"putnami","ref":null,"channels":{}}`)
	if parsed, diagnostics := ParseAndValidateReleaseSetPublishOutcome(input); parsed != nil || !findDiagnostic(diagnostics, ErrorCodeNullField, "ref") {
		t.Fatalf("null publish ref accepted: %#v %v", parsed, diagnostics)
	}
}

func TestNilProviderValuesFailClosed(t *testing.T) {
	checks := [][]diag.Diagnostic{
		ValidateResolveRequest(nil), ValidateResolveResponse(nil), ValidateResolveExchange(nil, nil),
		ValidateReleaseRequest(nil), ValidateReleaseResponse(nil), ValidateReleaseExchange(nil, nil),
		ValidateChannelSetRequest(nil), ValidateChannelStatusRequest(nil), ValidateChannelStatusResponse(nil),
		ValidateChannelHead(nil), ValidateReleaseSetPublishOutcome(nil),
	}
	for i, diagnostics := range checks {
		if !diag.HasErrors(diagnostics) {
			t.Errorf("nil check %d passed", i)
		}
	}
}
