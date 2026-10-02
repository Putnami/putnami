package put

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
)

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1: bumping requires a migration story", ProtocolVersion)
	}
	if MaxManifestBytes != 4<<20 || MaxVersionBytes != 256 || MaxArchivePlatforms != 32 {
		t.Fatalf("bounds = %d, %d, %d; want 4 MiB, 256, 32", MaxManifestBytes, MaxVersionBytes, MaxArchivePlatforms)
	}
}

// TestConformance_Paths pins the endpoint templates the Put registry serves,
// relative to the registry base URL.
func TestConformance_Paths(t *testing.T) {
	for got, want := range map[string]string{
		BlobUploadPath("acme", "widget"):                   "/acme/widget/blobs",
		PublishPath("acme", "widget"):                      "/acme/widget/publish",
		ManifestPath("acme", "widget", "1.2.3"):            "/acme/widget/versions/1.2.3/manifest",
		ManifestPath("acme", "widget", "1.2.3+build.7"):    "/acme/widget/versions/1.2.3+build.7/manifest",
		ManifestPath("acme", "widget", "2026 10?x#y%z"):    "/acme/widget/versions/2026%2010%3Fx%23y%25z/manifest",
		ManifestPath("cloud", "doc-contents-cli", "0a1b2"): "/cloud/doc-contents-cli/versions/0a1b2/manifest",
	} {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
	if PublishContentType != "application/json" {
		t.Errorf("PublishContentType = %q", PublishContentType)
	}
}

// TestConformance_Profiles pins the media types each member kind publishes. A
// producer, the engine and the registry read one table.
func TestConformance_Profiles(t *testing.T) {
	want := map[distribution.MemberKind]Profile{
		distribution.KindConfig:    {Kind: distribution.KindConfig, ManifestMediaType: "application/vnd.putnami.config.authored-member+json"},
		distribution.KindMigration: {Kind: distribution.KindMigration, ManifestMediaType: "application/vnd.putnami.data.migration.v2+json", BlobMediaTypes: []string{"application/vnd.putnami.migration-bundle.v1.tar"}},
		distribution.KindDoc:       {Kind: distribution.KindDoc, ManifestMediaType: "application/vnd.putnami.sitecontent.bundle+json", BlobMediaTypes: []string{"application/gzip"}},
		distribution.KindArchive:   {Kind: distribution.KindArchive, ManifestMediaType: "application/vnd.putnami.archive+json", BlobMediaTypes: []string{"application/gzip", "application/octet-stream"}},
	}
	if len(Profiles) != len(want) {
		t.Fatalf("Profiles = %+v, want %d kinds", Profiles, len(want))
	}
	order := make([]distribution.MemberKind, 0, len(Profiles))
	for _, profile := range Profiles {
		got, ok := ProfileFor(profile.Kind)
		if !ok || !reflect.DeepEqual(got, want[profile.Kind]) {
			t.Errorf("ProfileFor(%s) = %+v, want %+v", profile.Kind, got, want[profile.Kind])
		}
		if !ValidMediaType(profile.ManifestMediaType) {
			t.Errorf("%s manifest media type %q is not a valid media type", profile.Kind, profile.ManifestMediaType)
		}
		order = append(order, profile.Kind)
	}
	expected := slices.DeleteFunc(slices.Clone(distribution.MemberKinds), func(kind distribution.MemberKind) bool {
		_, ok := want[kind]
		return !ok
	})
	if !slices.Equal(order, expected) {
		t.Errorf("Profiles order = %v, want the order of distribution.MemberKinds %v", order, expected)
	}
	for _, kind := range []distribution.MemberKind{distribution.KindImage, distribution.KindLibrary, ""} {
		if _, ok := ProfileFor(kind); ok {
			t.Errorf("ProfileFor(%q) answered a profile; put-write/v1 publishes no such member", kind)
		}
	}
}

// TestConformance_WireFieldNames pins the JSON field names the server owns.
func TestConformance_WireFieldNames(t *testing.T) {
	for _, tc := range []struct {
		value any
		keys  []string
	}{
		{BlobReceipt{}, []string{"created_at", "digest", "id", "media_type", "size"}},
		{PublishRequest{}, []string{"media_type", "payload", "version"}},
		{PublishResponse{}, []string{"channel", "manifest", "package", "version"}},
		{Version{}, []string{"created_at", "id", "manifest_id", "package_id", "state", "version", "visibility"}},
		{Manifest{}, []string{"created_at", "id", "media_type", "package_id", "payload"}},
		{ArchivePayload{}, []string{"artifacts"}},
		{ArchiveArtifact{}, []string{"digest", "size"}},
	} {
		if got := jsonFields(reflect.TypeOf(tc.value)); !slices.Equal(got, tc.keys) {
			t.Errorf("%T fields = %v, want %v", tc.value, got, tc.keys)
		}
	}
	if VersionStatePublished != "published" || VisibilityPrivate != "private" {
		t.Error("the published state or the private visibility changed")
	}
}

// TestConformance_SchemaTracksTheGoTypes holds the schema and the Go types to
// the same member set.
func TestConformance_SchemaTracksTheGoTypes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("schemas", "put-write-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Description string `json:"description"`
		Definitions map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	for definition, value := range map[string]any{
		"blobReceipt": BlobReceipt{}, "publishRequest": PublishRequest{}, "publishResponse": PublishResponse{},
		"publishedVersion": Version{}, "manifest": Manifest{}, "archivePayload": ArchivePayload{}, "archiveArtifact": ArchiveArtifact{},
	} {
		properties := schema.Definitions[definition].Properties
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		if want := jsonFields(reflect.TypeOf(value)); !slices.Equal(names, want) {
			t.Errorf("definitions/%s properties = %v, want the %T fields %v", definition, names, value, want)
		}
	}
	for _, rule := range []string{"canonical form", "no duplicate object member", "at most 4194304 bytes", "at most 256 bytes", "at most 32 platforms"} {
		if !strings.Contains(schema.Description, rule) {
			t.Errorf("the schema description does not name %q", rule)
		}
	}
}

// TestConformance_ErrorCodes validates that every protocol error code uses the
// put.* prefix and that the canonical set is complete.
func TestConformance_ErrorCodes(t *testing.T) {
	canonical := []string{
		"put.invalid_coordinate",
		"put.invalid_version",
		"put.invalid_media_type",
		"put.invalid_payload",
		"put.invalid_digest",
		"put.invalid_blob_receipt",
		"put.invalid_publish_request",
		"put.invalid_publish_response",
		"put.invalid_manifest",
		"put.invalid_archive_payload",
	}
	for _, code := range canonical {
		if !ValidErrorCodes[code] {
			t.Errorf("canonical error code %q missing from ValidErrorCodes", code)
		}
	}
	if len(ValidErrorCodes) != len(canonical) {
		t.Errorf("ValidErrorCodes has %d entries, want %d", len(ValidErrorCodes), len(canonical))
	}
}

type parseFunc func([]byte) []diag.Diagnostic

// TestConformance_Fixtures runs every fixture under fixtures/<message>: those in
// valid/ must produce no errors, those in invalid/ must produce at least one.
func TestConformance_Fixtures(t *testing.T) {
	messages := map[string]parseFunc{
		"blob-receipt":     func(b []byte) []diag.Diagnostic { _, d := ParseBlobReceipt(b); return d },
		"publish-request":  func(b []byte) []diag.Diagnostic { _, d := ParsePublishRequest(b); return d },
		"publish-response": func(b []byte) []diag.Diagnostic { _, d := ParsePublishResponse(b); return d },
		"manifest":         func(b []byte) []diag.Diagnostic { _, d := ParseManifest(b); return d },
		"archive-payload":  func(b []byte) []diag.Diagnostic { _, d := ParseArchivePayload(b); return d },
	}
	directories, err := os.ReadDir("fixtures")
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range directories {
		if _, known := messages[directory.Name()]; !known {
			t.Errorf("fixtures/%s names no message this suite parses", directory.Name())
		}
	}
	for message, parse := range messages {
		runFixtureDir(t, message, "valid", parse, false)
		runFixtureDir(t, message, "invalid", parse, true)
	}
}

// TestConformance_CanonicalPayload pins the stored form: the digest a
// publisher computes is the digest of the bytes the registry stores.
func TestConformance_CanonicalPayload(t *testing.T) {
	for _, payload := range []string{
		`{}`,
		`{"artifacts":{"linux-amd64":{"digest":"sha256:` + strings.Repeat("1", 64) + `","size":1}}}`,
		`{"b":1,"a":[true,false,null,"x"]}`,
		`{"html":"\u003cscript\u003e \u0026 \u2028 \u2029"}`,
		`{"unicode":"é ü"}`,
		`{"escapes":"\u0041\/","number":1.0}`,
	} {
		if diags := ValidatePayload([]byte(payload)); len(diags) != 0 {
			t.Errorf("ValidatePayload(%s) = %v, want canonical", payload, diags)
		}
	}
	for payload, want := range map[string]string{
		``:                    "empty",
		`[]`:                  "not a JSON object",
		` {}`:                 "not a JSON object",
		`{} `:                 "canonical form",
		`{"a": 1}`:            "canonical form",
		"{\"a\":1}\n":         "canonical form",
		`{"a":"<b>"}`:         "canonical form",
		`{"a":"&"}`:           "canonical form",
		"{\"a\":\"\u2028\"}":  "canonical form",
		`{"a":1,"a":2}`:       `duplicate object member "a"`,
		`{"a":{"b":1,"b":1}}`: `duplicate object member "b"`,
		"{\"a\":\"\xff\"}":    "not valid UTF-8",
		`{"a":1}{"b":2}`:      "trailing data",
		`{"a":`:               "manifest payload",
	} {
		diags := ValidatePayload([]byte(payload))
		if len(diags) == 0 || !strings.Contains(diags[0].Message, want) {
			t.Errorf("ValidatePayload(%q) = %v, want a refusal naming %q", payload, diags, want)
		}
	}
	if long := `{"a":"` + strings.Repeat("x", MaxManifestBytes) + `"}`; len(ValidatePayload([]byte(long))) == 0 {
		t.Error("a payload above MaxManifestBytes was accepted")
	}
	if got, want := Digest([]byte(`{}`)), "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"; got != want {
		t.Errorf("Digest({}) = %s, want %s", got, want)
	}
}

// TestConformance_BlobReferences pins the members through which a manifest
// references a blob: the registry links exactly these to the package.
func TestConformance_BlobReferences(t *testing.T) {
	one, two, three := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64), "sha256:"+strings.Repeat("3", 64)
	for payload, want := range map[string][]string{
		`{"values":{"region":"eu"}}`:                                                                        nil,
		`{"blob_digest":"` + three + `","bundle_digest":"` + one + `"}`:                                     {three},
		`{"artifact":{"blob":"` + two + `","mediaType":"application/gzip","size":3},"bundle":{"name":"x"}}`: {two},
		`{"artifacts":{"linux-amd64":{"digest":"` + two + `","size":1},"darwin-arm64":{"digest":"` + one + `","size":1},"windows-amd64":{"digest":"` + two + `","size":1}}}`: {one, two},
		`{"blob_digest":"` + one + `","artifact":{"blob":"` + one + `"},"artifacts":{"a-b":{"digest":"` + three + `"}}}`:                                                     {one, three},
	} {
		got, err := BlobReferences([]byte(payload))
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("BlobReferences(%s) = %v, %v; want %v", payload, got, err, want)
		}
	}
	for _, payload := range []string{
		`{"blob_digest":"sha256:abc"}`,
		`{"artifact":{"blob":7}}`,
		`{"artifacts":[{"digest":"` + one + `"}]}`,
		`{"artifacts":{"a-b":{"digest":"` + strings.ToUpper(one) + `"}}}`,
		`[]`,
	} {
		if got, err := BlobReferences([]byte(payload)); err == nil {
			t.Errorf("BlobReferences(%s) = %v, want a refusal", payload, got)
		}
	}
}

func TestConformance_ArchivePlatforms(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "archive-payload", "valid", "template-fan-out.json"))
	if err != nil {
		t.Fatal(err)
	}
	archive, diags := ParseArchivePayload(data)
	if archive == nil {
		t.Fatalf("ParseArchivePayload refused the fan-out fixture: %v", diags)
	}
	platforms := archive.Platforms()
	if len(platforms) != 6 || platforms["windows/arm64"] != "sha256:"+strings.Repeat("7", 64) {
		t.Fatalf("Platforms() = %v", platforms)
	}
}

func TestConformance_CoordinateAndVersion(t *testing.T) {
	for _, coordinate := range []string{"acme/widget", "cloud/doc-contents-platform", "putnami/cli", "a/b", "a.b_c-d/0"} {
		if _, _, err := SplitCoordinate(coordinate); err != nil {
			t.Errorf("SplitCoordinate(%q) = %v", coordinate, err)
		}
	}
	for _, coordinate := range []string{"", "acme", "acme/", "/widget", "Acme/widget", "acme/widget/x", "-acme/widget", "acme/.widget", "acme/" + strings.Repeat("w", 256)} {
		if _, _, err := SplitCoordinate(coordinate); err == nil {
			t.Errorf("SplitCoordinate(%q) accepted", coordinate)
		}
	}
	for _, version := range []string{"1.2.3", "0.2.0-r42+abc", "0a1b2c3d4e5f", "v1", strings.Repeat("9", MaxVersionBytes)} {
		if err := ValidVersion(version); err != nil {
			t.Errorf("ValidVersion(%q) = %v", version, err)
		}
	}
	for _, version := range []string{"", ".", "..", " 1", "1 ", "1/2", "1\x002", "1\n", "1\r", strings.Repeat("9", MaxVersionBytes+1), "\xff"} {
		if err := ValidVersion(version); err == nil {
			t.Errorf("ValidVersion(%q) accepted", version)
		}
	}
}

// The For validators bind an answer to the request: another coordinate,
// version, media type or payload is refused even when the answer is
// well-formed.
func TestConformance_AnswersMatchTheRequest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "publish-response", "valid", "published.json"))
	if err != nil {
		t.Fatal(err)
	}
	response, diags := ParsePublishResponse(data)
	if response == nil {
		t.Fatal(diags)
	}
	payload := []byte(response.Manifest.Payload)
	if diags := ValidatePublishResponseFor(*response, "acme/widget", "1.2.3", ArchiveManifestMediaType, payload); len(diags) != 0 {
		t.Fatalf("the matching answer was refused: %v", diags)
	}
	// The registry keeps the media type of the first upload of a blob, so a
	// receipt with another media type still names the uploaded bytes.
	receipt := BlobReceipt{Digest: "sha256:" + strings.Repeat("1", 64), Size: 2, MediaType: BinaryBlobMediaType}
	if diags := ValidateBlobReceiptFor(receipt, receipt.Digest, 2); len(diags) != 0 {
		t.Fatalf("the receipt of the uploaded bytes was refused: %v", diags)
	}
	for name, check := range map[string]func() []diag.Diagnostic{
		"coordinate": func() []diag.Diagnostic {
			return ValidatePublishResponseFor(*response, "acme/gadget", "1.2.3", ArchiveManifestMediaType, payload)
		},
		"version": func() []diag.Diagnostic {
			return ValidatePublishResponseFor(*response, "acme/widget", "1.2.4", ArchiveManifestMediaType, payload)
		},
		"media type": func() []diag.Diagnostic {
			return ValidatePublishResponseFor(*response, "acme/widget", "1.2.3", ConfigManifestMediaType, payload)
		},
		"payload": func() []diag.Diagnostic {
			return ValidateManifestFor(response.Manifest, ArchiveManifestMediaType, []byte(`{"artifacts":{}}`))
		},
		"blob digest": func() []diag.Diagnostic {
			return ValidateBlobReceiptFor(receipt, "sha256:"+strings.Repeat("2", 64), 2)
		},
		"blob size": func() []diag.Diagnostic {
			return ValidateBlobReceiptFor(receipt, "sha256:"+strings.Repeat("1", 64), 3)
		},
	} {
		if diags := check(); !diag.HasErrors(diags) {
			t.Errorf("an answer with another %s was accepted", name)
		}
	}
}

func runFixtureDir(t *testing.T, message, kind string, parse parseFunc, wantErrors bool) {
	t.Helper()
	glob := filepath.Join("fixtures", message, kind, "*.json")
	paths, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures matched %s", glob)
	}
	for _, path := range paths {
		t.Run(message+"/"+kind+"/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			hasErrors := diag.HasErrors(parse(data))
			if wantErrors && !hasErrors {
				t.Errorf("invalid fixture %s produced no error", path)
			}
			if !wantErrors && hasErrors {
				t.Errorf("valid fixture %s produced errors: %v", path, parse(data))
			}
		})
	}
}

// jsonFields lists the json names of the exported fields of kind, sorted.
func jsonFields(kind reflect.Type) []string {
	fields := make([]string, 0, kind.NumField())
	for index := 0; index < kind.NumField(); index++ {
		name, _, _ := strings.Cut(kind.Field(index).Tag.Get("json"), ",")
		fields = append(fields, name)
	}
	sort.Strings(fields)
	return fields
}
