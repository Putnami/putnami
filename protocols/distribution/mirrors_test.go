package distribution

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func mirrorReleaseRequest() ReleaseRequest {
	return ReleaseRequest{
		ProtocolVersion: ProtocolVersion, Namespace: "putnami", ReleaseSet: *representativeSet(),
		Channels:   []ChannelRequest{{Name: "canary", Visibility: VisibilityInternal}},
		Visibility: visibilityChain(),
	}
}

func TestReleaseCarriesOptionalMirrorTargetsWithoutChangingTheSet(t *testing.T) {
	request := mirrorReleaseRequest()
	wireWithoutMirrors := string(strictJSON(t, request))
	if strings.Contains(wireWithoutMirrors, `"mirrors"`) {
		t.Fatal("a release without mirror intent must omit mirrors")
	}
	ref := mustRef(t, &request.ReleaseSet)
	request.Mirrors = map[string]MirrorTarget{
		"npm":             {To: "https://registry.npmjs.org"},
		"oci":             {To: "docker.io/putnami"},
		"custom-registry": {To: "registry.example.com:443/releases"},
	}
	parsed, diagnostics := ParseAndValidateReleaseRequest(strictJSON(t, request))
	if parsed == nil || diag.HasErrors(diagnostics) || !maps.Equal(parsed.Mirrors, request.Mirrors) {
		t.Fatalf("mirror intent did not survive the strict provider boundary: %+v, %v", parsed, diagnostics)
	}
	if got := mustRef(t, &parsed.ReleaseSet); got != ref {
		t.Fatalf("transporting mirror intent changed immutable set identity: %+v, want %+v", got, ref)
	}
	for _, mirrors := range []string{"", `,"mirrors":{}`} {
		wire := strings.TrimSuffix(wireWithoutMirrors, "}") + mirrors + "}"
		if parsed, diagnostics := ParseAndValidateReleaseRequest([]byte(wire)); parsed == nil || diag.HasErrors(diagnostics) {
			t.Fatalf("omitted/empty mirror intent refused: %v", diagnostics)
		}
	}
}

func TestMirrorTargetsRejectUnboundedOrCredentialBearingDestinations(t *testing.T) {
	for _, tc := range []struct {
		name, ecosystem, target, code string
	}{
		{"empty", "npm", "", ErrorCodeBoundsExceeded},
		{"too-long", "npm", strings.Repeat("a", MaxMirrorTargetBytes+1), ErrorCodeBoundsExceeded},
		{"invalid-ecosystem", "NPM", "registry.example.com", ErrorCodeInvalidEcosystem},
		{"http", "npm", "http://registry.example.com", ErrorCodeInvalidMirrorTarget},
		{"userinfo", "npm", "https://user:secret@registry.example.com", ErrorCodeInvalidMirrorTarget},
		{"native-userinfo", "oci", "user@registry.example.com", ErrorCodeInvalidMirrorTarget},
		{"query", "npm", "https://registry.example.com?token=secret", ErrorCodeInvalidMirrorTarget},
		{"fragment", "npm", "https://registry.example.com#secret", ErrorCodeInvalidMirrorTarget},
		{"space", "npm", "registry.example.com secret", ErrorCodeInvalidMirrorTarget},
		{"newline", "npm", "registry.example.com\nsecret", ErrorCodeInvalidMirrorTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := mirrorReleaseRequest()
			request.Mirrors = map[string]MirrorTarget{tc.ecosystem: {To: tc.target}}
			field := "mirrors." + tc.ecosystem + ".to"
			if tc.code == ErrorCodeInvalidEcosystem {
				field = "mirrors." + tc.ecosystem
			}
			if diagnostics := ValidateReleaseRequest(&request); !findDiagnostic(diagnostics, tc.code, field) {
				t.Fatalf("invalid mirror accepted programmatically: %v", diagnostics)
			}
			parsed, diagnostics := ParseAndValidateReleaseRequest(strictJSON(t, request))
			if parsed != nil || !findDiagnostic(diagnostics, tc.code, field) {
				t.Fatalf("invalid mirror accepted on the wire: %+v, %v", parsed, diagnostics)
			}
			if strings.Contains(fmt.Sprint(diagnostics), "secret") {
				t.Fatalf("a rejected destination leaked credential material: %v", diagnostics)
			}
		})
	}
	if diagnostics := ValidateMirrorTargets(map[string]MirrorTarget{"npm": {To: strings.Repeat("a", MaxMirrorTargetBytes)}}); diag.HasErrors(diagnostics) {
		t.Fatalf("target at the documented bound rejected: %v", diagnostics)
	}
	targets := map[string]MirrorTarget{}
	for index := 0; index < MaxRegistryKinds; index++ {
		targets[fmt.Sprintf("registry-%d", index)] = MirrorTarget{To: "registry.example.com"}
	}
	if diagnostics := ValidateMirrorTargets(targets); diag.HasErrors(diagnostics) {
		t.Fatalf("mirror map at the documented bound rejected: %v", diagnostics)
	}
	targets["overflow"] = MirrorTarget{To: "registry.example.com"}
	if diagnostics := ValidateMirrorTargets(targets); !findDiagnostic(diagnostics, ErrorCodeBoundsExceeded, "mirrors") {
		t.Fatalf("mirror map bound not enforced: %v", diagnostics)
	}
}

func TestMirrorIntentStrictShapeRejectsAmbiguousOrSecretFields(t *testing.T) {
	base := strings.TrimSuffix(string(strictJSON(t, mirrorReleaseRequest())), "}")
	for _, mirrors := range []string{
		`null`, `[]`, `{"npm":null}`, `{"npm":{}}`,
		`{"npm":{"to":null}}`, `{"npm":{"to":4}}`,
		`{"npm":{"to":"registry.example.com","credential":"secret"}}`,
		`{"npm":{"to":"registry.example.com","to":"other.example.com"}}`,
		`{"npm":{"to":"registry.example.com"},"npm":{"to":"other.example.com"}}`,
	} {
		if parsed, diagnostics := ParseAndValidateReleaseRequest([]byte(base + `,"mirrors":` + mirrors + "}")); parsed != nil || !diag.HasErrors(diagnostics) {
			t.Fatalf("ambiguous mirror intent accepted: %+v, %v", parsed, diagnostics)
		}
	}
}
