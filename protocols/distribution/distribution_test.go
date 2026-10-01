package distribution

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// representativeRevision is the source revision every representative member was
// published from; fixtures/valid/mixed.json carries the same document.
const representativeRevision = "8d5edb7513d93b9165ba2a7cb48466d022fc3f63"

// representativeTree is the git tree the representative revision holds.
const representativeTree = "c0ffee5f1e2d3c4b5a69788796a5b4c3d2e1f0a9"

func fingerprint(fill byte) string { return "sha256:" + strings.Repeat(string(fill), 64) }

const (
	memberVersion   = "0.3.0-8d5edb751"
	goMemberVersion = "v0.3.0-8d5edb751"
)

// representativeSet mirrors fixtures/valid/mixed.json: one member per ecosystem
// this repository publishes to, provenance on every member, and platform
// digests on the archive. fixtures/golden.json pins its canonical bytes.
func representativeSet() *ReleaseSet {
	return &ReleaseSet{
		ProtocolVersion: ProtocolVersion,
		Namespace:       "putnami",
		Members: []ReleaseSetMember{
			{Ecosystem: "go", Coordinate: "go.putnami.dev/app", Version: goMemberVersion, ArtifactDigest: fingerprint('c'), Dependencies: []ReleaseSetDependency{}, SourceRevision: representativeRevision, SelectionFingerprint: fingerprint('1')},
			{Ecosystem: "npm", Coordinate: "@putnami/core", Version: memberVersion, ArtifactDigest: fingerprint('a'), Dependencies: []ReleaseSetDependency{}, SourceRevision: representativeRevision, SelectionFingerprint: fingerprint('2')},
			{Ecosystem: "npm", Coordinate: "@putnami/web", Version: memberVersion, ArtifactDigest: fingerprint('b'), Dependencies: []ReleaseSetDependency{{Ecosystem: "npm", Coordinate: "@putnami/core", Version: memberVersion}}, SourceRevision: representativeRevision, SelectionFingerprint: fingerprint('3')},
			{Ecosystem: "oci", Coordinate: "putnami/sites/putnami.dev", Version: memberVersion, ArtifactDigest: fingerprint('d'), Dependencies: []ReleaseSetDependency{{Ecosystem: "npm", Coordinate: "@putnami/web", Version: memberVersion}}, SourceRevision: representativeRevision, SelectionFingerprint: fingerprint('4')},
			{Ecosystem: "archive", Coordinate: "putnami/cli", Version: memberVersion, ArtifactDigest: fingerprint('e'), Dependencies: []ReleaseSetDependency{}, SourceRevision: representativeRevision, SelectionFingerprint: fingerprint('5'), Platforms: map[string]string{"linux/amd64": fingerprint('6'), "darwin/arm64": fingerprint('7')}},
			{Ecosystem: "put", Coordinate: "putnami/sites/putnami.dev/config", Version: memberVersion, ArtifactDigest: fingerprint('f'), Dependencies: []ReleaseSetDependency{}, SourceRevision: representativeRevision, SelectionFingerprint: fingerprint('8')},
		},
	}
}

func findDiagnostic(diagnostics []diag.Diagnostic, code, field string) bool {
	for _, finding := range diagnostics {
		if finding.Code == code && (field == "" || finding.Field == field) {
			return true
		}
	}
	return false
}

func TestCanonicalReleaseSetBytesNormalizesWithoutMutation(t *testing.T) {
	input := representativeSet()
	before, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	canonical, diagnostics := CanonicalReleaseSetBytes(input)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("canonicalize: %v", diagnostics)
	}
	after, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("canonicalization mutated its input")
	}
	goldenBytes, goldenRef := EmbeddedGolden()
	if !bytes.Equal(canonical, goldenBytes) {
		t.Fatalf("canonical bytes drifted from golden\n got: %s\nwant: %s", canonical, goldenBytes)
	}
	if ref := mustRef(t, input); !refsEqual(ref, goldenRef) {
		t.Fatalf("ref drifted: %#v want %#v", ref, goldenRef)
	}
	// The golden pins the member order, the field order, and sorted platform keys.
	if !strings.HasPrefix(string(canonical), `{"protocolVersion":2,"namespace":"putnami","members":[{"ecosystem":"archive"`) {
		t.Fatalf("canonical member order drifted: %s", canonical)
	}
	if !strings.Contains(string(canonical), `"sourceRevision":"`+representativeRevision+`","selectionFingerprint":"`+fingerprint('5')+`","platforms":{"darwin/arm64":`) {
		t.Fatalf("canonical field order or platform sorting drifted: %s", canonical)
	}
	if bytes.Contains(canonical, []byte("contentFingerprint")) {
		t.Fatalf("canonical bytes still carry contentFingerprint: %s", canonical)
	}
}

func TestDeriveReleaseSetRefUsesSameSHA256Hex(t *testing.T) {
	ref, diagnostics := DeriveReleaseSetRef(representativeSet())
	if diag.HasErrors(diagnostics) {
		t.Fatalf("derive ref: %v", diagnostics)
	}
	if !refIDPattern.MatchString(ref.ID) || !digestPattern.MatchString(ref.Digest) {
		t.Fatalf("malformed derived ref: %#v", ref)
	}
	if strings.TrimPrefix(ref.ID, "rs_") != strings.TrimPrefix(ref.Digest, "sha256:") {
		t.Fatalf("id and digest carry different hashes: %#v", ref)
	}
}

func TestParseCanonicalReleaseSetRejectsEquivalentNonCanonicalBytes(t *testing.T) {
	canonical, diagnostics := CanonicalReleaseSetBytes(representativeSet())
	if diag.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	if got, _, diagnostics := ParseCanonicalReleaseSet(canonical); got == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("canonical bytes rejected: %v", diagnostics)
	}
	nonCanonical := append([]byte(" "), canonical...)
	if got, _, diagnostics := ParseCanonicalReleaseSet(nonCanonical); got != nil || !findDiagnostic(diagnostics, ErrorCodeNonCanonical, "") {
		t.Fatalf("non-canonical bytes not rejected: %#v %v", got, diagnostics)
	}
	if got, _, diagnostics := ParseCanonicalReleaseSet([]byte(`{"protocolVersion":2}`)); got != nil || !diag.HasErrors(diagnostics) {
		t.Fatal("incomplete canonical bytes accepted")
	}
}

func TestValidateReleaseSetPinsClosureAndExactSelectors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReleaseSet)
		code   string
		field  string
	}{
		{"bad ecosystem id", func(set *ReleaseSet) { set.Members[1].Ecosystem = "Npm" }, ErrorCodeInvalidEcosystem, "members[1].ecosystem"},
		{"empty ecosystem id", func(set *ReleaseSet) { set.Members[1].Ecosystem = "" }, ErrorCodeInvalidEcosystem, "members[1].ecosystem"},
		{"empty coordinate", func(set *ReleaseSet) { set.Members[0].Coordinate = "" }, ErrorCodeInvalidCoordinate, "members[0].coordinate"},
		{"coordinate with a control character", func(set *ReleaseSet) { set.Members[0].Coordinate = "go.putnami.dev/\napp" }, ErrorCodeInvalidCoordinate, "members[0].coordinate"},
		{"oversized coordinate", func(set *ReleaseSet) { set.Members[0].Coordinate = strings.Repeat("a", MaxCoordinateBytes+1) }, ErrorCodeBoundsExceeded, "members[0].coordinate"},
		{"empty version", func(set *ReleaseSet) { set.Members[0].Version = "" }, ErrorCodeInvalidVersion, "members[0].version"},
		{"version with a control character", func(set *ReleaseSet) { set.Members[0].Version = "v0.3.0\t" }, ErrorCodeInvalidVersion, "members[0].version"},
		{"oversized version", func(set *ReleaseSet) { set.Members[0].Version = strings.Repeat("9", MaxVersionBytes+1) }, ErrorCodeBoundsExceeded, "members[0].version"},
		{"bad digest", func(set *ReleaseSet) { set.Members[0].ArtifactDigest = "sha256:ABC" }, ErrorCodeInvalidArtifactDigest, "members[0].artifactDigest"},
		{"short revision", func(set *ReleaseSet) { set.Members[1].SourceRevision = "8d5edb751" }, ErrorCodeInvalidSourceRevision, "members[1].sourceRevision"},
		{"uppercase revision", func(set *ReleaseSet) { set.Members[1].SourceRevision = strings.ToUpper(representativeRevision) }, ErrorCodeInvalidSourceRevision, "members[1].sourceRevision"},
		{"missing selection fingerprint", func(set *ReleaseSet) { set.Members[2].SelectionFingerprint = "" }, ErrorCodeInvalidSelectionFingerprint, "members[2].selectionFingerprint"},
		{"selection fingerprint is not sha256", func(set *ReleaseSet) {
			set.Members[2].SelectionFingerprint = "md5:" + strings.Repeat("0", 32)
		}, ErrorCodeInvalidSelectionFingerprint, "members[2].selectionFingerprint"},
		{"unknown kind", func(set *ReleaseSet) { set.Members[3].Kind = "container" }, ErrorCodeInvalidKind, "members[3].kind"},
		{"abbreviated source tree", func(set *ReleaseSet) { set.Members[1].SourceTree = "c0ffee5" }, ErrorCodeInvalidSourceTree, "members[1].sourceTree"},
		{"uppercase source tree", func(set *ReleaseSet) { set.Members[1].SourceTree = strings.ToUpper(representativeTree) }, ErrorCodeInvalidSourceTree, "members[1].sourceTree"},
		{"sha-256 source tree", func(set *ReleaseSet) { set.Members[1].SourceTree = strings.Repeat("ab", 32) }, ErrorCodeInvalidSourceTree, "members[1].sourceTree"},
		{"project with a leading slash", func(set *ReleaseSet) { set.Members[0].Project = "/go/framework/app" }, ErrorCodeInvalidProject, "members[0].project"},
		{"project with a control character", func(set *ReleaseSet) { set.Members[0].Project = "go/framework\napp" }, ErrorCodeInvalidProject, "members[0].project"},
		{"oversized project", func(set *ReleaseSet) { set.Members[0].Project = strings.Repeat("a", MaxProjectBytes+1) }, ErrorCodeInvalidProject, "members[0].project"},
		{"bad platform key", func(set *ReleaseSet) { set.Members[4].Platforms = map[string]string{"Linux-amd64": fingerprint('6')} }, ErrorCodeInvalidPlatform, "members[4].platforms"},
		{"bad platform digest", func(set *ReleaseSet) { set.Members[4].Platforms = map[string]string{"linux/amd64": "sha256:short"} }, ErrorCodeInvalidArtifactDigest, "members[4].platforms.linux/amd64"},
		{"duplicate member", func(set *ReleaseSet) { set.Members = append(set.Members, set.Members[1]) }, ErrorCodeDuplicateMember, "members[6]"},
		{"duplicate dependency", func(set *ReleaseSet) {
			set.Members[2].Dependencies = append(set.Members[2].Dependencies, set.Members[2].Dependencies[0])
		}, ErrorCodeDuplicateDependency, "members[2].dependencies[1]"},
		{"unclosed dependency", func(set *ReleaseSet) { set.Members[2].Dependencies[0].Coordinate = "@putnami/missing" }, ErrorCodeUnclosedDependency, "members[2].dependencies[0]"},
		{"version mismatch", func(set *ReleaseSet) { set.Members[2].Dependencies[0].Version = "0.3.0-wrong" }, ErrorCodeDependencyVersionMismatch, "members[2].dependencies[0].version"},
		{"protocol version 1", func(set *ReleaseSet) { set.ProtocolVersion = 1 }, ErrorCodeInvalidProtocolVersion, "protocolVersion"},
		{"no member", func(set *ReleaseSet) { set.Members = nil }, ErrorCodeMissingField, "members"},
		{"bad namespace", func(set *ReleaseSet) { set.Namespace = "Putnami" }, ErrorCodeInvalidNamespace, "namespace"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set := representativeSet()
			test.mutate(set)
			diagnostics := ValidateReleaseSet(set)
			if !findDiagnostic(diagnostics, test.code, test.field) {
				t.Fatalf("want %s at %s, got %v", test.code, test.field, diagnostics)
			}
		})
	}

	// A project yields several members, including several in one ecosystem: the
	// member key is (ecosystem, coordinate), never the project.
	t.Run("two members one ecosystem allowed", func(t *testing.T) {
		set := representativeSet()
		set.Members = append(set.Members, ReleaseSetMember{
			Ecosystem: "put", Coordinate: "putnami/sites/putnami.dev/migrations", Version: memberVersion,
			ArtifactDigest: fingerprint('9'), Dependencies: []ReleaseSetDependency{},
			SourceRevision: representativeRevision, SelectionFingerprint: fingerprint('0'),
		})
		if diagnostics := ValidateReleaseSet(set); diag.HasErrors(diagnostics) {
			t.Fatalf("two members of one project in one ecosystem rejected: %v", diagnostics)
		}
	})

	// The protocol never judges the shape of a coordinate or a version: a Python
	// or Java ecosystem joins without a protocol change.
	t.Run("unknown ecosystem accepted", func(t *testing.T) {
		set := representativeSet()
		set.Members[1].Ecosystem = "pypi"
		set.Members[1].Coordinate = "putnami_core"
		set.Members[1].Version = "0.3.0.dev1"
		set.Members[2].Dependencies = nil
		if diagnostics := ValidateReleaseSet(set); diag.HasErrors(diagnostics) {
			t.Fatalf("an ecosystem the protocol never heard of was rejected: %v", diagnostics)
		}
	})

	t.Run("platform bound", func(t *testing.T) {
		set := representativeSet()
		set.Members[4].Platforms = map[string]string{}
		for index := 0; index <= MaxPlatformsPerMember; index++ {
			set.Members[4].Platforms["linux/arch"+strings.Repeat("x", index)] = fingerprint('6')
		}
		if diagnostics := ValidateReleaseSet(set); !findDiagnostic(diagnostics, ErrorCodeBoundsExceeded, "members[4].platforms") {
			t.Fatalf("platform bound not enforced: %v", diagnostics)
		}
	})
}

func TestNormalizeReleaseSetCopiesProvenanceWithoutAliasing(t *testing.T) {
	input := representativeSet()
	normalized := NormalizeReleaseSet(input)
	archive := normalized.Members[0]
	if archive.Ecosystem != "archive" || archive.SourceRevision != representativeRevision || archive.SelectionFingerprint != fingerprint('5') {
		t.Fatalf("archive member not first or provenance dropped: %#v", archive)
	}
	if len(archive.Platforms) != 2 {
		t.Fatalf("platforms dropped: %#v", archive.Platforms)
	}
	archive.Platforms["linux/arm64"] = fingerprint('9')
	if len(input.Members[4].Platforms) != 2 {
		t.Fatal("normalized platforms alias the caller's map")
	}
	if normalized.Members[1].Platforms != nil {
		t.Fatal("an empty platforms map must normalize to nil so canonical bytes never carry the key")
	}
	if NormalizeReleaseSet(nil) != nil {
		t.Fatal("normalizing nil must return nil")
	}
}

func TestStrictParsingRejectsAmbiguousJSON(t *testing.T) {
	minimal := `{"protocolVersion":2,"namespace":"putnami","members":[]`
	tests := []struct {
		name  string
		input string
		code  string
		field string
	}{
		{"unknown", minimal + `,"extra":true}`, ErrorCodeUnknownField, "extra"},
		{"duplicate", `{"protocolVersion":2,"namespace":"putnami","namespace":"other","members":[]}`, ErrorCodeDuplicateField, "namespace"},
		{"missing", `{"protocolVersion":2,"namespace":"putnami"}`, ErrorCodeMissingField, "members"},
		{"null", `{"protocolVersion":2,"namespace":"putnami","members":null}`, ErrorCodeNullField, "members"},
		{"trailing", minimal + `} {}`, ErrorCodeParseError, ""},
		{"non-integer version", `{"protocolVersion":2.0,"namespace":"putnami","members":[]}`, ErrorCodeParseError, "protocolVersion"},
		{"member is not an object", `{"protocolVersion":2,"namespace":"putnami","members":[1]}`, ErrorCodeParseError, "members[0]"},
		{"platforms is not an object", `{"protocolVersion":2,"namespace":"putnami","members":[{"platforms":[]}]}`, ErrorCodeParseError, "members[0].platforms"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, diagnostics := ParseReleaseSet([]byte(test.input))
			if parsed != nil || !findDiagnostic(diagnostics, test.code, test.field) {
				t.Fatalf("want %s at %s, got %#v %v", test.code, test.field, parsed, diagnostics)
			}
		})
	}

	// The shapes introduced by the channel operations are just as strict.
	nested := []struct {
		name  string
		input string
		parse func([]byte) []diag.Diagnostic
		code  string
		field string
	}{
		{
			"resolve heads is not an object",
			`{"protocolVersion":2,"heads":[]}`,
			func(data []byte) []diag.Diagnostic {
				_, diagnostics := ParseAndValidateResolveResponse(data)
				return diagnostics
			},
			ErrorCodeParseError, "heads",
		},
		{
			"release channel without expected",
			`{"protocolVersion":2,"namespace":"putnami","channels":[{"name":"canary","visibility":"internal"}],"releaseSet":{},"visibility":{}}`,
			func(data []byte) []diag.Diagnostic {
				_, diagnostics := ParseAndValidateReleaseRequest(data)
				return diagnostics
			},
			ErrorCodeMissingField, "channels[0].expected",
		},
		{
			"visibility chain without an explicit set",
			`{"protocolVersion":2,"namespace":"putnami","visibility":{"repo":"internal","versions":{}},"releaseSet":{},"channels":[]}`,
			func(data []byte) []diag.Diagnostic {
				_, diagnostics := ParseAndValidateReleaseRequest(data)
				return diagnostics
			},
			ErrorCodeMissingField, "visibility.set",
		},
		{
			"channel-set with an unknown source key",
			`{"protocolVersion":2,"namespace":"putnami","channel":"latest","expected":null,"from":{"tag":"ts-v0.3.0"}}`,
			func(data []byte) []diag.Diagnostic {
				_, diagnostics := ParseAndValidateChannelSetRequest(data)
				return diagnostics
			},
			ErrorCodeUnknownField, "from.tag",
		},
		{
			"channel-status without an explicit desired",
			`{"protocolVersion":2,"observed":{"npm":7}}`,
			func(data []byte) []diag.Diagnostic {
				_, diagnostics := ParseAndValidateChannelStatusResponse(data)
				return diagnostics
			},
			ErrorCodeMissingField, "desired",
		},
		{
			"publish outcome carrying a v1 channel",
			`{"protocolVersion":2,"namespace":"putnami","channel":"canary","ref":{"id":"rs_x","digest":"sha256:x"},"channels":{}}`,
			func(data []byte) []diag.Diagnostic {
				_, diagnostics := ParseAndValidateReleaseSetPublishOutcome(data)
				return diagnostics
			},
			ErrorCodeUnknownField, "channel",
		},
	}
	for _, test := range nested {
		t.Run(test.name, func(t *testing.T) {
			if diagnostics := test.parse([]byte(test.input)); !findDiagnostic(diagnostics, test.code, test.field) {
				t.Fatalf("want %s at %s, got %v", test.code, test.field, diagnostics)
			}
		})
	}
}

func TestStrictParsingBoundsInput(t *testing.T) {
	input := bytes.Repeat([]byte{' '}, MaxJSONBytes+1)
	if parsed, diagnostics := ParseReleaseSet(input); parsed != nil || !findDiagnostic(diagnostics, ErrorCodeBoundsExceeded, "") {
		t.Fatalf("oversized JSON not rejected: %#v %v", parsed, diagnostics)
	}
}

func TestValidationBoundsDiagnostics(t *testing.T) {
	set := representativeSet()
	set.Members = make([]ReleaseSetMember, MaxDiagnostics+10)
	for i := range set.Members {
		set.Members[i] = ReleaseSetMember{Ecosystem: "Unsupported", Dependencies: []ReleaseSetDependency{}}
	}
	diagnostics := ValidateReleaseSet(set)
	if len(diagnostics) != MaxDiagnostics {
		t.Fatalf("diagnostic count = %d, want %d", len(diagnostics), MaxDiagnostics)
	}
	if diagnostics[len(diagnostics)-1].Code != ErrorCodeDiagnosticsTruncated {
		t.Fatalf("last diagnostic = %s, want %s", diagnostics[len(diagnostics)-1].Code, ErrorCodeDiagnosticsTruncated)
	}
	if set := (&ReleaseSet{}); ValidateReleaseSet(nil) == nil || len(ValidateReleaseSet(set)) == 0 {
		t.Fatal("nil and empty release sets must fail closed")
	}
}

func TestCommandTokensAreExact(t *testing.T) {
	got := []string{ProviderExecutable, ProviderCommandName, CloudCommand, ReleaseSetCommand, ResolveCommand, ReleaseCommand, ChannelSetCommand, ChannelStatusCommand, RequestFileFlag}
	want := []string{"putnami", "cloud-release-set", "cloud", "release-set", "resolve", "release", "channel-set", "channel-status", "--request-file"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("provider tokens drifted: %q", got)
	}
	if ProtocolName != "distribution/release-set/v2" || ProtocolVersion != 2 {
		t.Fatalf("protocol identity drifted: %q v%d", ProtocolName, ProtocolVersion)
	}
	if EcosystemPattern != "^[a-z][a-z0-9-]{0,31}$" || ChannelPattern != "^[a-z0-9][a-z0-9._-]{0,63}$" {
		t.Fatalf("grammars drifted: %q %q", EcosystemPattern, ChannelPattern)
	}
	if MaxChannelsPerRelease != 16 || MaxRegistryKinds != 16 {
		t.Fatalf("bounds drifted: %d %d", MaxChannelsPerRelease, MaxRegistryKinds)
	}
}

func TestReleaseSetRefValidationRejectsMismatch(t *testing.T) {
	ref := ReleaseSetRef{ID: "rs_" + strings.Repeat("a", 64), Digest: "sha256:" + strings.Repeat("b", 64)}
	if diagnostics := ValidateReleaseSetRef("ref", ref); !findDiagnostic(diagnostics, ErrorCodeRefMismatch, "ref") {
		t.Fatalf("mismatched ref accepted: %v", diagnostics)
	}
	if diagnostics := ValidateReleaseSetID("releaseId", "rs_bad"); !findDiagnostic(diagnostics, ErrorCodeInvalidRef, "releaseId") {
		t.Fatalf("malformed standalone release id accepted: %v", diagnostics)
	}
	if !IsReleaseSetID("rs_"+strings.Repeat("a", 64)) || IsReleaseSetID("rs_bad") {
		t.Fatal("standalone release id predicate drifted")
	}
}

func TestVisibilityOrderAndMax(t *testing.T) {
	if VisibilityInternal.Rank() != 0 || VisibilityPrivate.Rank() != 1 || VisibilityPublic.Rank() != 2 {
		t.Fatal("visibility ranks drifted")
	}
	if Visibility("secret").Rank() != -1 || Visibility("secret").Valid() {
		t.Fatal("an unknown level must be invalid and rank below internal")
	}
	for _, level := range []Visibility{VisibilityInternal, VisibilityPrivate, VisibilityPublic} {
		if !level.Valid() {
			t.Fatalf("%s is not valid", level)
		}
	}
	// The ratchet operator never narrows a level already resolved.
	cases := []struct{ a, b, want Visibility }{
		{VisibilityInternal, VisibilityPublic, VisibilityPublic},
		{VisibilityPublic, VisibilityInternal, VisibilityPublic},
		{VisibilityPrivate, VisibilityInternal, VisibilityPrivate},
		{VisibilityInternal, VisibilityPrivate, VisibilityPrivate},
		{VisibilityPublic, VisibilityPublic, VisibilityPublic},
		{VisibilityInternal, "secret", VisibilityInternal},
	}
	for _, test := range cases {
		if got := MaxVisibility(test.a, test.b); got != test.want {
			t.Fatalf("MaxVisibility(%q, %q) = %q, want %q", test.a, test.b, got, test.want)
		}
	}
}

func TestEncodeTagAsChannel(t *testing.T) {
	cases := []struct {
		tag  string
		want string
	}{
		{"ts/v0.3.0", "ts-v0.3.0"},
		{"v0.3.0", "v0.3.0"},
		{"go/framework/v1.2.3", "go-framework-v1.2.3"},
	}
	for _, test := range cases {
		got, err := EncodeTagAsChannel(test.tag)
		if err != nil || got != test.want {
			t.Fatalf("EncodeTagAsChannel(%q) = %q, %v; want %q", test.tag, got, err, test.want)
		}
		if !IsPortableChannel(got) {
			t.Fatalf("%q is not portable", got)
		}
	}
	for _, tag := range []string{"TS/v0.3.0", "-v0.3.0", "release candidate", strings.Repeat("v", 65), ""} {
		if got, err := EncodeTagAsChannel(tag); err == nil {
			t.Fatalf("EncodeTagAsChannel(%q) = %q, want an error", tag, got)
		}
	}
	if IsPortableChannel("branches/feature") {
		t.Fatal("a slash is not in the portable alphabet")
	}
}

// attributedSet is the representative set with source project and artifact
// kind on every member: what a publisher records once it can attribute a
// member to the project that produced it.
func attributedSet() *ReleaseSet {
	set := representativeSet()
	attribution := []struct {
		project string
		kind    MemberKind
	}{
		{"go/framework/app", KindLibrary},
		{"typescript/framework/core", KindLibrary},
		{"typescript/framework/web", KindLibrary},
		{"sites/putnami.dev", KindImage},
		{"tooling/cli", KindArchive},
		{"sites/putnami.dev", KindConfig},
	}
	for index := range set.Members {
		set.Members[index].Project = attribution[index].project
		set.Members[index].Kind = attribution[index].kind
	}
	return set
}

// TestMemberAttributionIsOptionalAndPartOfIdentity pins the three properties a
// consumer and an existing stored set both depend on: a set that carries
// neither field derives EXACTLY the ref it derived before the fields existed,
// attribution is carried through normalization into canonical bytes, and two
// sets that differ only in attribution are two different immutable sets.
func TestMemberAttributionIsOptionalAndPartOfIdentity(t *testing.T) {
	legacy := representativeSet()
	legacyCanonical, diagnostics := CanonicalReleaseSetBytes(legacy)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("a set carrying no attribution was rejected: %v", diagnostics)
	}
	if bytes.Contains(legacyCanonical, []byte(`"project"`)) || bytes.Contains(legacyCanonical, []byte(`"kind"`)) {
		t.Fatalf("canonical bytes emit empty attribution keys: %s", legacyCanonical)
	}
	_, goldenRef := EmbeddedGolden()
	legacyRef := mustRef(t, legacy)
	if !refsEqual(legacyRef, goldenRef) {
		t.Fatalf("a set with no attribution drifted from its stored ref: %#v want %#v", legacyRef, goldenRef)
	}

	attributed := attributedSet()
	if diagnostics := ValidateReleaseSet(attributed); diag.HasErrors(diagnostics) {
		t.Fatalf("an attributed set was rejected: %v", diagnostics)
	}
	attributedCanonical, diagnostics := CanonicalReleaseSetBytes(attributed)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("canonicalize an attributed set: %v", diagnostics)
	}
	// Attribution lands after the platform digests, in struct order, so the
	// prefix of every member record is byte-identical to the legacy spelling.
	if !bytes.Contains(attributedCanonical, []byte(`"project":"tooling/cli","kind":"archive"`)) {
		t.Fatalf("canonical attribution order drifted: %s", attributedCanonical)
	}
	if ref := mustRef(t, attributed); refsEqual(ref, legacyRef) {
		t.Fatalf("an attributed set derives the unattributed ref %#v", ref)
	}

	// Provenance is part of identity: changing only the project changes the set.
	renamed := attributedSet()
	renamed.Members[0].Project = "go/framework/renamed"
	if ref := mustRef(t, renamed); refsEqual(ref, mustRef(t, attributed)) {
		t.Fatalf("two sets differing only in project share the ref %#v", ref)
	}
	reclassified := attributedSet()
	reclassified.Members[0].Kind = KindArchive
	if ref := mustRef(t, reclassified); refsEqual(ref, mustRef(t, attributed)) {
		t.Fatalf("two sets differing only in kind share the ref %#v", ref)
	}

	// Normalization is the only path into canonical bytes: a copy that dropped
	// attribution would hash as an unattributed set.
	normalized := NormalizeReleaseSet(attributed)
	for _, member := range normalized.Members {
		if member.Project == "" || member.Kind == "" {
			t.Fatalf("normalization dropped attribution: %#v", member)
		}
	}
}

// TestAttributedReleaseSetRoundTripsThroughStrictDecoding pins the wire half:
// the strict reader accepts both spellings, and every closed kind is admitted.
func TestAttributedReleaseSetRoundTripsThroughStrictDecoding(t *testing.T) {
	canonical, diagnostics := CanonicalReleaseSetBytes(attributedSet())
	if diag.HasErrors(diagnostics) {
		t.Fatalf("canonicalize: %v", diagnostics)
	}
	parsed, ref, parseDiagnostics := ParseCanonicalReleaseSet(canonical)
	if parsed == nil || diag.HasErrors(parseDiagnostics) {
		t.Fatalf("canonical attributed bytes rejected: %v", parseDiagnostics)
	}
	if ref != mustRef(t, attributedSet()) {
		t.Fatalf("round-tripped ref = %#v", ref)
	}
	found := map[MemberKind]bool{}
	for _, member := range parsed.Members {
		found[member.Kind] = true
		if member.Project == "" {
			t.Fatalf("round trip dropped the project of %#v", member)
		}
	}
	for _, kind := range []MemberKind{KindImage, KindLibrary, KindArchive, KindConfig} {
		if !found[kind] {
			t.Fatalf("round trip dropped kind %q: %#v", kind, parsed.Members)
		}
	}

	// A legacy document — neither key present — stays valid, and an explicitly
	// empty kind is a rejected spelling of absence, not a silent acceptance.
	legacy, diagnostics := ParseAndValidateReleaseSet(mustFixture(t, "fixtures/valid/minimal.json"))
	if legacy == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("legacy fixture rejected: %v", diagnostics)
	}
	if legacy.Members[0].Project != "" || legacy.Members[0].Kind != "" {
		t.Fatalf("legacy member invented attribution: %#v", legacy.Members[0])
	}
	// An explicitly empty attribution is the same statement as an omitted one,
	// exactly as an empty platforms map is: it decodes to absence and the
	// canonical projection carries no key for it, so the two spellings of one
	// document never derive two refs.
	attributedFixture := mustFixture(t, "fixtures/valid/attributed-members.json")
	empty := bytes.Replace(attributedFixture, []byte(`"project": "sites/putnami.dev",
      "kind": "image"`), []byte(`"project": "",
      "kind": ""`), 1)
	if bytes.Equal(empty, attributedFixture) {
		t.Fatal("the empty-attribution spelling was not substituted")
	}
	emptyParsed, diagnostics := ParseAndValidateReleaseSet(empty)
	if emptyParsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("an explicitly empty attribution was rejected: %v", diagnostics)
	}
	emptyCanonical, diagnostics := CanonicalReleaseSetBytes(emptyParsed)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("canonicalize an explicitly empty attribution: %v", diagnostics)
	}
	// The fixture has six members: one declares no attribution at all, and the
	// substitution above emptied a second one's, leaving four keys.
	if got := bytes.Count(emptyCanonical, []byte(`"kind"`)); got != 4 {
		t.Fatalf("canonical bytes carry %d kind keys, want 4: %s", got, emptyCanonical)
	}
	for _, member := range emptyParsed.Members {
		if member.Ecosystem == "oci" && (member.Kind != "" || member.Project != "") {
			t.Fatalf("an explicitly empty attribution decoded to %#v", member)
		}
	}
}

// TestSourceTreeIsOptionalAndPartOfIdentity pins what a stored set and a
// consumer both depend on: a set without the field derives EXACTLY the ref it
// derived before the field existed, a set with it is a different immutable
// set, and the field survives normalization and the strict reader.
func TestSourceTreeIsOptionalAndPartOfIdentity(t *testing.T) {
	legacy := representativeSet()
	legacyCanonical, diagnostics := CanonicalReleaseSetBytes(legacy)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("a set carrying no source tree was rejected: %v", diagnostics)
	}
	if bytes.Contains(legacyCanonical, []byte(`"sourceTree"`)) {
		t.Fatalf("canonical bytes emit an empty source tree: %s", legacyCanonical)
	}
	golden, goldenRef := EmbeddedGolden()
	if !bytes.Equal(legacyCanonical, golden) || !refsEqual(mustRef(t, legacy), goldenRef) {
		t.Fatalf("a set with no source tree drifted from its stored bytes: %s", legacyCanonical)
	}

	withTree := representativeSet()
	withTree.Members[4].SourceTree = representativeTree
	canonical, diagnostics := CanonicalReleaseSetBytes(withTree)
	if diag.HasErrors(diagnostics) {
		t.Fatalf("a member with a source tree was rejected: %v", diagnostics)
	}
	// The tree lands last, in struct order, so every member record keeps the
	// legacy spelling as its prefix.
	if !bytes.Contains(canonical, []byte(`"linux/amd64":"`+fingerprint('6')+`"},"sourceTree":"`+representativeTree+`"}`)) {
		t.Fatalf("canonical source tree order drifted: %s", canonical)
	}
	if ref := mustRef(t, withTree); refsEqual(ref, goldenRef) {
		t.Fatalf("a set with a source tree derives the legacy ref %#v", ref)
	}
	parsed, ref, parseDiagnostics := ParseCanonicalReleaseSet(canonical)
	if parsed == nil || diag.HasErrors(parseDiagnostics) || ref != mustRef(t, withTree) {
		t.Fatalf("canonical bytes with a source tree = %#v %v", ref, parseDiagnostics)
	}
	if parsed.Members[0].SourceTree != representativeTree {
		t.Fatalf("round trip dropped the source tree: %#v", parsed.Members[0])
	}

	// The fixture carries one member with a tree and one without; its ref is
	// pinned so another implementation reproduces the same bytes.
	fixture, diagnostics := ParseAndValidateReleaseSet(mustFixture(t, "fixtures/valid/source-tree.json"))
	if fixture == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("source-tree fixture rejected: %v", diagnostics)
	}
	if fixture.Members[0].SourceTree != representativeTree || fixture.Members[1].SourceTree != "" {
		t.Fatalf("source-tree fixture members = %#v", fixture.Members)
	}
	const fixtureRef = "rs_3fd2f1736cded52267d48e2e6711d502f915ec43db44e08a1a8cb0139f5e9ff2"
	if got := mustRef(t, fixture); got.ID != fixtureRef {
		t.Fatalf("source-tree fixture ref = %s, want %s", got.ID, fixtureRef)
	}

	// An explicitly empty tree is the same statement as an omitted one.
	empty := bytes.Replace(mustFixture(t, "fixtures/valid/source-tree.json"), []byte(`"sourceTree": "`+representativeTree+`"`), []byte(`"sourceTree": ""`), 1)
	emptyParsed, diagnostics := ParseAndValidateReleaseSet(empty)
	if emptyParsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("an explicitly empty source tree was rejected: %v", diagnostics)
	}
	emptyCanonical, diagnostics := CanonicalReleaseSetBytes(emptyParsed)
	if diag.HasErrors(diagnostics) || bytes.Contains(emptyCanonical, []byte(`"sourceTree"`)) {
		t.Fatalf("an explicitly empty source tree reached the canonical bytes: %s %v", emptyCanonical, diagnostics)
	}
}

// TestMemberKindVocabularyIsClosed pins the vocabulary itself: a consumer that
// groups members by role branches on exactly these six tokens.
func TestMemberKindVocabularyIsClosed(t *testing.T) {
	want := []MemberKind{"image", "config", "migration", "doc", "library", "archive"}
	if len(MemberKinds) != len(want) {
		t.Fatalf("kind vocabulary drifted: %v", MemberKinds)
	}
	for index, kind := range want {
		if MemberKinds[index] != kind || !kind.Valid() {
			t.Fatalf("kind %d = %q, want a valid %q", index, MemberKinds[index], kind)
		}
	}
	for _, invalid := range []MemberKind{"", "Image", "images", "container", "binary"} {
		if invalid.Valid() {
			t.Fatalf("kind %q is not part of the closed vocabulary", invalid)
		}
	}
}

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := fixtureFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}
