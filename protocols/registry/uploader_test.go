package registry

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// uploadFixtures maps each valid upload request fixture to the response
// fixtures that answer it.
var uploadFixtures = map[string][]string{
	"request-upload.json":              {"response-upload.json", "response-upload-refusal.json"},
	"request-upload-npm-loopback.json": {"response-upload-refusal-conflict.json"},
	"request-upload-go.json":           nil,
	"request-upload-oci.json":          nil,
	"request-upload-archive.json":      {"response-upload-archive.json"},
	"request-upload-put.json":          nil,
}

// TestUploadNeedsUploaderAndPublication proves the op rule: upload is
// allowed only in a session that negotiated both uploader-v1 and
// publication-v1, for a request and for its answer.
func TestUploadNeedsUploaderAndPublication(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "uploader-v1-needs-publication-v1", "upload-needs-both-capabilities")
	refused := [][]string{
		nil,
		{CapabilityCredentialV1},
		{CapabilityCredentialV1, CapabilityPublicationV1},
		{CapabilityCredentialV1, CapabilityUploaderV1},
		{CapabilityCredentialV1, CapabilityPublicationV1, "uploader-v2"},
	}
	for _, name := range slices.Sorted(maps.Keys(uploadFixtures)) {
		line := readFixture(t, "valid", name)
		if _, err := ParseNegotiatedCredentialRequest(line, everyCapability); err != nil {
			t.Fatalf("%s: refused in a session that negotiated uploader-v1: %v", name, err)
		}
		for _, negotiated := range refused {
			if _, err := ParseNegotiatedCredentialRequest(line, negotiated); err == nil {
				t.Errorf("%s: accepted in a session that negotiated %v", name, negotiated)
			}
		}
		for _, answer := range uploadFixtures[name] {
			line := readFixture(t, "valid", answer)
			if _, err := ParseNegotiatedCredentialResponse(line, CredentialOpUpload, everyCapability); err != nil {
				t.Fatalf("%s: refused in a session that negotiated uploader-v1: %v", answer, err)
			}
			for _, negotiated := range refused {
				if _, err := ParseNegotiatedCredentialResponse(line, CredentialOpUpload, negotiated); err == nil {
					t.Errorf("%s: accepted in a session that negotiated %v", answer, negotiated)
				}
			}
		}
	}
	_, err := ParseNegotiatedCredentialRequest(readFixture(t, "valid", "request-upload.json"), []string{CapabilityCredentialV1, CapabilityPublicationV1})
	if err == nil || !strings.Contains(err.Error(), "needs the uploader-v1 capability") {
		t.Errorf("an upload request outside uploader-v1: %v", err)
	}
	for _, test := range []struct {
		offered, echoed []string
		allowed         bool
	}{
		{everyCapability, everyCapability, true},
		{everyCapability, []string{CapabilityUploaderV1, CapabilityPublicationV1, CapabilityCredentialV1}, true},
		{everyCapability, []string{CapabilityCredentialV1, CapabilityPublicationV1}, false},
		{[]string{CapabilityCredentialV1, CapabilityPublicationV1}, everyCapability, false},
		{everyCapability, []string{CapabilityCredentialV1}, false},
	} {
		negotiated := NegotiatedCapabilities(test.offered, test.echoed)
		if CredentialOpUpload.Allowed(negotiated) != test.allowed {
			t.Errorf("upload allowed = %v after offering %v and echoing %v", !test.allowed, test.offered, test.echoed)
		}
		if !CredentialOpRelease.Allowed(negotiated) && CredentialOpUpload.Allowed(negotiated) {
			t.Errorf("upload is allowed without release after offering %v and echoing %v", test.offered, test.echoed)
		}
	}
}

// TestUploaderCapabilityNeedsPublication proves the capability rule: an
// initialize request or answer that names uploader-v1 also names
// publication-v1, in any order.
func TestUploaderCapabilityNeedsPublication(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "uploader-v1-needs-publication-v1", "uploader-v1-names-publication-v1")
	for _, test := range []struct {
		capabilities []string
		valid        bool
	}{
		{[]string{CapabilityCredentialV1, CapabilityPublicationV1, CapabilityUploaderV1}, true},
		{[]string{CapabilityUploaderV1, CapabilityCredentialV1, CapabilityPublicationV1}, true},
		{[]string{CapabilityCredentialV1, CapabilityPublicationV1}, true},
		{[]string{CapabilityCredentialV1, CapabilityUploaderV1}, false},
		{[]string{CapabilityUploaderV1}, false},
	} {
		params, err := json.Marshal(CredentialInitializeParams{ProtocolVersion: 1, Capabilities: test.capabilities})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseCredentialInitializeParams(params); (err == nil) != test.valid {
			t.Errorf("initialize params %v: %v, want valid %v", test.capabilities, err, test.valid)
		}
		result, err := json.Marshal(CredentialInitializeResult{ProtocolVersion: 1, Capabilities: test.capabilities})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseCredentialInitializeResult(result); (err == nil) != test.valid {
			t.Errorf("initialize result %v: %v, want valid %v", test.capabilities, err, test.valid)
		}
	}
}

// TestUploadWireCarriesNoCredentialAndNoReuse pins the members of the upload
// payloads: the request names no credential, the provider holds its own, and
// no project beside the member's own; the answer reports no reuse.
func TestUploadWireCarriesNoCredentialAndNoReuse(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "an-upload-carries-no-credential", "the-upload-wire-holds-no-credential")
	if got := jsonMembers(UploadParams{}); got != "kind,member,outbox,planDigest,registry" {
		t.Errorf("upload params members = %s", got)
	}
	if got := jsonMembers(UploadResult{}); got != "digest,platforms" {
		t.Errorf("upload result members = %s", got)
	}
}

// fixtureUpload returns the upload payload of a valid request fixture.
func fixtureUpload(t *testing.T, name string) UploadParams {
	t.Helper()
	params, err := ParseUploadParams(fixturePayload(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return *params
}

// TestUploadParamsRules covers the upload rules no fixture isolates: the kind
// of every member kind, the loopback hosts, the bounds, the outbox forms, the
// canonical encoding of the member, and errors that never repeat the
// registry.
func TestUploadParamsRules(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "an-upload-is-validated", "upload-params-are-validated")
	put := fixtureUpload(t, "request-upload-put.json")
	archive := fixtureUpload(t, "request-upload-archive.json")
	for _, kind := range distribution.MemberKinds {
		put.Kind, archive.Kind = kind, kind
		if err := ValidateUploadParams(put); (err == nil) != (kind != distribution.KindArchive) {
			t.Errorf("a put member of kind %s: %v", kind, err)
		}
		if err := ValidateUploadParams(archive); (err == nil) != (kind == distribution.KindArchive) {
			t.Errorf("an archive member of kind %s: %v", kind, err)
		}
	}
	goParams := fixtureUpload(t, "request-upload-go.json")
	for registry, valid := range map[string]bool{
		"https://go.putnami.dev":            true,
		"https://go.putnami.dev:8443/proxy": true,
		"https://GO.putnami.dev/a/b":        true,
		"http://localhost:4873":             true,
		"http://LOCALHOST":                  true,
		"http://registry.localhost:5000":    true,
		"http://127.0.0.2:5000":             true,
		"http://[::1]:5000/go":              true,
		"http://192.168.1.10:5000":          false,
		"http://go.putnami.dev":             false,
		"https://go.putnami.dev:0":          false,
		"https://go.putnami.dev:65536":      false,
		"https://":                          false,
		"https://:443":                      false,
		"https://go.putnami.dev/#top":       false,
		"https://go.putnami.dev/proxy/":     false,
		"HTTPS://go.putnami.dev":            false,
		"ftp://go.putnami.dev":              false,
		"https://go.putnami.dev/é":          false,
		"https://go.putnami.dev/" + strings.Repeat("a", MaxUploadRegistryBytes-len("https://go.putnami.dev/")):   true,
		"https://go.putnami.dev/" + strings.Repeat("a", MaxUploadRegistryBytes-len("https://go.putnami.dev/")+1): false,
		"https://user:" + "s3cret" + "@go.putnami.dev":                                                           false,
	} {
		goParams.Registry = registry
		err := ValidateUploadParams(goParams)
		if (err == nil) != valid {
			t.Errorf("registry %.60q: %v, want valid %v", registry, err, valid)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the error repeats the registry: %v", err)
		}
	}
	goParams = fixtureUpload(t, "request-upload-go.json")
	for outbox, valid := range map[string]bool{
		"/tmp/putnami-publication-1/go-app": true,
		"/":                                 true,
		`C:\Users\runner\Temp\putnami-publication-1`: true,
		"c:/runner/temp/putnami-publication-1":       true,
		"/tmp/outbox ü":                              true,
		"/tmp/./outbox":                              false,
		`C:\Users\..\outbox`:                         false,
		"/tmp/outbox/..":                             false,
		"outbox":                                     false,
		"C:outbox":                                   false,
		`\\server\share\outbox`:                      false,
		"/tmp/out\nbox":                              false,
		"/tmp/\xffoutbox":                            false,
		"/" + strings.Repeat("a", MaxUploadOutboxBytes-1): true,
		"/" + strings.Repeat("a", MaxUploadOutboxBytes):   false,
	} {
		goParams.Outbox = outbox
		if err := ValidateUploadParams(goParams); (err == nil) != valid {
			t.Errorf("outbox %.60q: %v, want valid %v", outbox, err, valid)
		}
	}
	npm := fixtureUpload(t, "request-upload.json")
	npm.Member.NPM.Tarball.Path = "../escape.tgz"
	if err := ValidateUploadParams(npm); err == nil || !strings.Contains(err.Error(), "member.npm.tarball.path") {
		t.Errorf("a member error names its field under member: %v", err)
	}
	payload := strings.Replace(string(fixturePayload(t, "request-upload-oci.json")), `"tags":["0.3.0","canary"]`, `"tags":[]`, 1)
	if !strings.Contains(payload, `"tags":[]`) {
		t.Fatalf("the payload holds no empty tags: %s", payload)
	}
	if _, err := ParseUploadParams(json.RawMessage(payload)); err == nil {
		t.Error("an empty tags member is accepted; an absent value is omitted")
	}
	if _, err := ParseUploadParams(json.RawMessage(`{}`)); err == nil {
		t.Error("an empty upload payload is accepted")
	}
	if _, err := ParseUploadParams(nil); err == nil {
		t.Error("a missing upload payload is accepted")
	}
	var zero UploadParams
	if err := ValidateUploadParams(zero); err == nil {
		t.Error("a zero upload payload is valid")
	}
	if extension.OutboxEcosystemArchive != string(distribution.KindArchive) {
		t.Error("the archive ecosystem and the archive kind diverged")
	}
}

// TestUploadResultRules covers the answer rules: a sha256 digest, and
// platforms that are absent or name 1 to MaxPlatformsPerMember lowercase
// os/arch pairs mapped to sha256 digests.
func TestUploadResultRules(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "an-upload-is-validated", "upload-results-are-validated")
	digest := "sha256:" + strings.Repeat("e", 64)
	platforms := func(count int) map[string]string {
		set := make(map[string]string, count)
		for index := range count {
			set[fmt.Sprintf("os%d/arch", index)] = digest
		}
		return set
	}
	for _, test := range []struct {
		name   string
		result UploadResult
		valid  bool
	}{
		{"single artifact", UploadResult{Digest: digest}, true},
		{"one platform", UploadResult{Digest: digest, Platforms: platforms(1)}, true},
		{"platform bound", UploadResult{Digest: digest, Platforms: platforms(distribution.MaxPlatformsPerMember)}, true},
		{"over the platform bound", UploadResult{Digest: digest, Platforms: platforms(distribution.MaxPlatformsPerMember + 1)}, false},
		{"empty platforms", UploadResult{Digest: digest, Platforms: map[string]string{}}, false},
		{"uppercase platform", UploadResult{Digest: digest, Platforms: map[string]string{"Linux/amd64": digest}}, false},
		{"platform with a variant", UploadResult{Digest: digest, Platforms: map[string]string{"linux/arm64/v8": digest}}, false},
		{"platform digest", UploadResult{Digest: digest, Platforms: map[string]string{"linux/amd64": "sha256:abc"}}, false},
		{"missing digest", UploadResult{}, false},
	} {
		if err := ValidateUploadResult(test.result); (err == nil) != test.valid {
			t.Errorf("%s: %v, want valid %v", test.name, err, test.valid)
		}
	}
	for _, payload := range []string{`{}`, `{"digest":"` + digest + `","platforms":{}}`, `{"digest":"` + digest + `","reused":true}`, `{"Digest":"` + digest + `"}`} {
		if _, err := ParseUploadResult(json.RawMessage(payload)); err == nil {
			t.Errorf("upload result %s is accepted", payload)
		}
	}
}

// TestUploadLineBound proves an upload line has the publication-v1 bounds:
// beyond the credential-v1 bound up to MaxPublicationLineBytes.
func TestUploadLineBound(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "an-upload-is-validated", "an-upload-line-has-the-publication-bounds")
	line := readFixture(t, "valid", "request-upload.json")
	if _, err := ParseNegotiatedCredentialRequest(padLine(line, MaxCredentialLineBytes+1), everyCapability); err != nil {
		t.Errorf("an upload line over %d bytes: %v", MaxCredentialLineBytes, err)
	}
	if _, err := ParseNegotiatedCredentialRequest(padLine(line, MaxPublicationLineBytes), everyCapability); err != nil {
		t.Errorf("an upload line of %d bytes: %v", MaxPublicationLineBytes, err)
	}
	if _, err := ParseNegotiatedCredentialRequest(padLine(line, MaxPublicationLineBytes+1), everyCapability); err == nil || !strings.Contains(err.Error(), fmt.Sprint(MaxPublicationLineBytes)) {
		t.Errorf("an upload line over %d bytes: %v", MaxPublicationLineBytes, err)
	}
	answer := readFixture(t, "valid", "response-upload.json")
	if _, err := ParseNegotiatedCredentialResponse(padLine(answer, MaxCredentialLineBytes+1), CredentialOpUpload, everyCapability); err != nil {
		t.Errorf("an upload answer over %d bytes: %v", MaxCredentialLineBytes, err)
	}
}

// schemaText is a string definition of the schema: its bounds and grammar.
type schemaText struct {
	MinLength int    `json:"minLength"`
	MaxLength int    `json:"maxLength"`
	Pattern   string `json:"pattern"`
}

// schemaEnum is a definition of the schema that lists its values.
type schemaEnum struct {
	Enum []string `json:"enum"`
}

// schemaRef is a definition of the schema that refers to another.
type schemaRef struct {
	Ref string `json:"$ref"`
}

// schemaContains is an array definition of the schema that holds a constant.
type schemaContains struct {
	Contains struct {
		Const string `json:"const"`
	} `json:"contains"`
}

// readSchema returns schemas/credential-provider-v1.json.
func readSchema(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("schemas", "credential-provider-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestUploadSchemaTracksWireShape keeps the upload definitions of the schema
// in step with the Go grammars, bounds and enums.
func TestUploadSchemaTracksWireShape(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "the-schema-tracks-the-go-contract", "the-upload-schema-tracks-the-go-types")
	var schema struct {
		Definitions struct {
			UploadParams struct {
				Properties struct {
					Registry schemaText `json:"registry"`
					Kind     schemaEnum `json:"kind"`
					Outbox   schemaText `json:"outbox"`
					Member   struct {
						Required   []string `json:"required"`
						Properties struct {
							Ecosystem schemaEnum `json:"ecosystem"`
						} `json:"properties"`
					} `json:"member"`
				} `json:"properties"`
			} `json:"uploadParams"`
			UploadResult struct {
				Properties struct {
					Digest    schemaRef `json:"digest"`
					Platforms struct {
						MinProperties        int        `json:"minProperties"`
						MaxProperties        int        `json:"maxProperties"`
						PropertyNames        schemaText `json:"propertyNames"`
						AdditionalProperties schemaRef  `json:"additionalProperties"`
					} `json:"platforms"`
				} `json:"properties"`
			} `json:"uploadResult"`
			Capabilities struct {
				If   schemaContains `json:"if"`
				Then schemaContains `json:"then"`
			} `json:"capabilities"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(readSchema(t), &schema); err != nil {
		t.Fatal(err)
	}
	params := schema.Definitions.UploadParams.Properties
	if params.Registry.MaxLength != MaxUploadRegistryBytes || params.Registry.Pattern != UploadRegistryPattern {
		t.Error("upload registry grammar or bound diverged")
	}
	if params.Outbox.MaxLength != MaxUploadOutboxBytes || params.Outbox.Pattern != UploadOutboxPattern {
		t.Error("upload outbox grammar or bound diverged")
	}
	kinds := make([]string, 0, len(distribution.MemberKinds))
	for _, kind := range distribution.MemberKinds {
		kinds = append(kinds, string(kind))
	}
	if !slices.Equal(params.Kind.Enum, kinds) {
		t.Errorf("upload kind enum = %v, want %v", params.Kind.Enum, kinds)
	}
	ecosystems := []string{extension.OutboxEcosystemNPM, extension.OutboxEcosystemGo, extension.OutboxEcosystemOCI, extension.OutboxEcosystemPut, extension.OutboxEcosystemArchive}
	if !slices.Equal(params.Member.Properties.Ecosystem.Enum, ecosystems) {
		t.Errorf("upload member ecosystem enum = %v, want %v", params.Member.Properties.Ecosystem.Enum, ecosystems)
	}
	if got := strings.Join(params.Member.Required, ","); got != "ecosystem,coordinate,version,project" {
		t.Errorf("upload member required = %s", got)
	}
	result := schema.Definitions.UploadResult.Properties
	if result.Digest.Ref != "#/definitions/digest" || result.Platforms.AdditionalProperties.Ref != "#/definitions/digest" {
		t.Error("upload result digests diverged from the digest grammar")
	}
	if result.Platforms.MinProperties != 1 || result.Platforms.MaxProperties != distribution.MaxPlatformsPerMember || result.Platforms.PropertyNames.Pattern != UploadPlatformPattern {
		t.Error("upload platforms grammar or bounds diverged")
	}
	capabilities := schema.Definitions.Capabilities
	if capabilities.If.Contains.Const != CapabilityUploaderV1 || capabilities.Then.Contains.Const != CapabilityPublicationV1 {
		t.Error("the capability rule of uploader-v1 diverged")
	}
}

// TestUploadInvalidFixturesFailForTheirRule proves each invalid upload fixture
// is refused for the rule its name states, not for another defect.
func TestUploadInvalidFixturesFailForTheirRule(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "registry/credential-seam-contract", "an-upload-is-validated", "each-invalid-upload-fixture-fails-for-its-rule")
	for name, want := range map[string]string{
		"request-upload-credential-member.json":                 "unknown member \"credential\"",
		"request-upload-missing-member.json":                    "missing member \"member\"",
		"request-upload-npm-with-kind.json":                     "upload of a npm member names no kind",
		"request-upload-empty-kind.json":                        "member \"kind\" is empty",
		"request-upload-null-kind.json":                         "null is not permitted",
		"request-upload-put-without-kind.json":                  "upload of a put member needs a kind",
		"request-upload-put-kind-archive.json":                  "upload kind archive does not fit a put member",
		"request-upload-archive-kind-mismatch.json":             "upload kind config does not fit a archive member",
		"request-upload-unknown-kind.json":                      "upload of a put member needs a kind",
		"request-upload-registry-userinfo.json":                 "without userinfo",
		"request-upload-registry-http.json":                     "uses http to a host that is not loopback",
		"request-upload-registry-trailing-slash.json":           "without userinfo, query, fragment or trailing slash",
		"request-upload-registry-query.json":                    "without userinfo, query, fragment or trailing slash",
		"request-upload-registry-port-range.json":               "port must be between 1 and 65535",
		"request-upload-npm-registry-mismatch.json":             "not the registry the npm member names",
		"request-upload-project-member.json":                    "unknown member \"project\"",
		"request-upload-relative-outbox.json":                   "upload outbox must be an absolute path",
		"request-upload-outbox-dot-segment.json":                "upload outbox holds a . or .. segment",
		"request-upload-member-two-blocks.json":                 "member carries 2 ecosystem blocks",
		"request-upload-member-escaping-path.json":              "member.npm.tarball.path",
		"request-upload-bad-plan-digest.json":                   "upload planDigest must match",
		"request-initialize-uploader-without-publication.json":  "capability uploader-v1 needs publication-v1",
		"response-initialize-uploader-without-publication.json": "capability uploader-v1 needs publication-v1",
		"response-upload-reused.json":                           "unknown member \"reused\"",
		"response-upload-bad-digest.json":                       "upload digest must match",
		"response-upload-bad-platform.json":                     "upload platform \"darwin-arm64\" must match",
		"response-upload-empty-platforms.json":                  "member \"platforms\" is empty",
	} {
		line := readFixture(t, "invalid", name)
		var err error
		if answer, ok := strings.CutPrefix(name, "response-"); ok {
			_, err = ParseNegotiatedCredentialResponse(line, CredentialOp(strings.SplitN(answer, "-", 2)[0]), everyCapability)
		} else {
			_, err = ParseNegotiatedCredentialRequest(line, everyCapability)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error naming %q", name, err, want)
		}
	}
}
