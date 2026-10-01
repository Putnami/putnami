package extension

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// npmProfile is the reference profile the resolution tests declare. It is the
// npm shape of ADR 0001, so the tests exercise a profile a first-party
// extension actually ships rather than a synthetic one.
func npmProfile() EcosystemProfile {
	return EcosystemProfile{
		ID:              "npm",
		Coordinate:      PatternRule{Pattern: `^(@[a-z0-9-]+/)?[a-z0-9-]+$`},
		Version:         VersionRule{Pattern: `^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`, Ordering: OrderingSemver},
		Channel:         ChannelNative,
		ChannelEncoding: `^[a-z][a-z0-9.-]{0,31}$`,
		Registries:      json.RawMessage(`{"type":"object"}`),
		Publish:         "publish-npm",
	}
}

func ownerManifest(name string, profiles ...EcosystemProfile) NamedManifest {
	return NamedManifest{Name: name, Manifest: &Manifest{
		Name:       name,
		Ecosystems: profiles,
		Commands:   map[string]CommandDefinition{"publish-npm": {Run: []PipelineStep{{ID: "publish", Task: "exec"}}}},
	}}
}

// TestResolveProfilesRefusesTwoOwners pins the one-owner rule at its sharpest
// point: the two declarations are byte-identical, so a resolution that compared
// bytes would accept them — and then diverge the first time one extension edits
// its copy and the other does not.
func TestResolveProfilesRefusesTwoOwners(t *testing.T) {
	_, diags := ResolveProfiles([]NamedManifest{
		ownerManifest("@putnami/typescript", npmProfile()),
		ownerManifest("@third/party", npmProfile()),
	}, nil)

	if got := diagCodes(diags); !reflect.DeepEqual(got, []string{"duplicate-ecosystem-profile"}) {
		t.Fatalf("codes = %v, want [duplicate-ecosystem-profile] (%v)", got, diags)
	}
	for _, want := range []string{"@putnami/typescript", "@third/party"} {
		if !strings.Contains(diags[0].Message, want) {
			t.Errorf("diagnostic %q does not name %q; the fix is in one of the two manifests", diags[0].Message, want)
		}
	}
}

// TestResolveProfilesRefusesUnknownUse pins the other half of the rule: `uses`
// is a reference, and a reference to nothing would leave a publish job pointing
// at an ecosystem no registry, coordinate rule or channel encoding exists for.
func TestResolveProfilesRefusesUnknownUse(t *testing.T) {
	using := ownerManifest("@putnami/go")
	using.Manifest.Uses = []string{"oci"}

	registry, diags := ResolveProfiles([]NamedManifest{using}, nil)
	if got := diagCodes(diags); !reflect.DeepEqual(got, []string{"unknown-ecosystem-use"}) {
		t.Fatalf("codes = %v, want [unknown-ecosystem-use] (%v)", got, diags)
	}
	if len(registry.IDs()) != 0 {
		t.Fatalf("IDs() = %v, want empty", registry.IDs())
	}

	// Control: the same `uses` resolves once an owner declares the id, which is
	// what proves the diagnostic reports absence rather than firing on every use.
	oci := npmProfile()
	oci.ID = "oci"
	if _, diags := ResolveProfiles([]NamedManifest{using, ownerManifest("@putnami/oci", oci)}, nil); len(diags) != 0 {
		t.Fatalf("a declared use still reported %v", diags)
	}
}

// TestResolveProfilesAcceptsBuiltinOwner pins that a profile the CLI supplies
// itself is a first-class owner: it satisfies a `uses`, and a manifest that
// redeclares it collides exactly as a second extension would.
func TestResolveProfilesAcceptsBuiltinOwner(t *testing.T) {
	builtin := []OwnedProfile{{Owner: "@putnami/extension-sdk", Profile: npmProfile()}}

	using := ownerManifest("@putnami/go")
	using.Manifest.Uses = []string{"npm"}
	registry, diags := ResolveProfiles([]NamedManifest{using}, builtin)
	if len(diags) != 0 {
		t.Fatalf("a builtin owner did not satisfy uses: %v", diags)
	}
	profile, owner, ok := registry.Profile("npm")
	if !ok || owner != "@putnami/extension-sdk" || profile.Publish != "publish-npm" {
		t.Fatalf("Profile(npm) = %v, %q, %v", profile, owner, ok)
	}
	if !registry.HasNativeChannel("npm") || registry.HasNativeChannel("absent") {
		t.Fatal("HasNativeChannel disagrees with the profile")
	}
	if got := registry.IDs(); !reflect.DeepEqual(got, []string{"npm"}) {
		t.Fatalf("IDs() = %v, want [npm]", got)
	}

	_, diags = ResolveProfiles([]NamedManifest{ownerManifest("@third/party", npmProfile())}, builtin)
	if got := diagCodes(diags); !reflect.DeepEqual(got, []string{"duplicate-ecosystem-profile"}) {
		t.Fatalf("redeclaring a builtin profile = %v, want [duplicate-ecosystem-profile]", got)
	}
}

// TestProfileValidatorsUsePatternsAndPortableAlphabet pins that the registry
// validates with the profile's OWN patterns, and that a channel name must clear
// the portable alphabet AND the profile's encoding — the encoding narrows, it
// never widens.
func TestProfileValidatorsUsePatternsAndPortableAlphabet(t *testing.T) {
	registry, diags := ResolveProfiles([]NamedManifest{ownerManifest("@putnami/typescript", npmProfile())}, nil)
	if len(diags) != 0 {
		t.Fatalf("resolve: %v", diags)
	}

	if err := registry.ValidateCoordinate("npm", "@putnami/web"); err != nil {
		t.Errorf("scoped coordinate rejected: %v", err)
	}
	if registry.ValidateCoordinate("npm", "Putnami/Web") == nil {
		t.Error("a coordinate outside the profile pattern was accepted")
	}
	if err := registry.ValidateVersion("npm", "0.3.0-rc.1"); err != nil {
		t.Errorf("prerelease version rejected: %v", err)
	}
	if registry.ValidateVersion("npm", "v0.3.0") == nil {
		t.Error("a version outside the profile pattern was accepted")
	}

	// `ts-v0.3.0` clears both alphabets; `ts/v0.3.0` is outside the portable
	// one; `ts_v0` clears the portable alphabet, which admits underscores, and
	// is rejected by the profile's stricter encoding, which does not.
	if err := registry.ValidateChannelName("npm", "ts-v0.3.0"); err != nil {
		t.Errorf("portable channel name rejected: %v", err)
	}
	if registry.ValidateChannelName("npm", "ts/v0.3.0") == nil {
		t.Error("a channel name outside the portable alphabet was accepted")
	}
	if registry.ValidateChannelName("npm", "ts_v0") == nil {
		t.Error("the profile encoding did not narrow the portable alphabet")
	}

	for name, err := range map[string]error{
		"coordinate": registry.ValidateCoordinate("absent", "x"),
		"version":    registry.ValidateVersion("absent", "1.0.0"),
		"channel":    registry.ValidateChannelName("absent", "stable"),
	} {
		if err == nil {
			t.Errorf("%s validation accepted an unknown ecosystem", name)
		}
	}
}

// TestPublishedMemberIsStrict pins the event's parse and validation contract: an
// unknown field is a rejection rather than a silent truncation, and every
// invariant a member carries into a release set is checked here.
func TestPublishedMemberIsStrict(t *testing.T) {
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	member, diags := ParsePublishedMember([]byte(`{
		"ecosystem": "oci",
		"coordinate": "putnami/cli",
		"version": "0.3.0",
		"artifactDigest": "` + digest + `",
		"platforms": {"linux/arm64": "` + digest + `"}
	}`))
	if len(diags) != 0 {
		t.Fatalf("parse valid member: %v", diags)
	}
	if diags := ValidatePublishedMember(member); len(diags) != 0 {
		t.Fatalf("valid member reported %v", diags)
	}

	if _, diags := ParsePublishedMember([]byte(`{"ecosystem":"oci","tag":"latest"}`)); !hasCode(diags, "invalid-published-member") {
		t.Fatalf("an unknown field was accepted: %v", diags)
	}

	for name, raw := range map[string]string{
		"bad ecosystem": `{"ecosystem":"OCI","coordinate":"c","version":"1","artifactDigest":"` + digest + `"}`,
		"no coordinate": `{"ecosystem":"oci","coordinate":"","version":"1","artifactDigest":"` + digest + `"}`,
		"no version":    `{"ecosystem":"oci","coordinate":"c","version":"","artifactDigest":"` + digest + `"}`,
		"short digest":  `{"ecosystem":"oci","coordinate":"c","version":"1","artifactDigest":"sha256:abc"}`,
		"bad platform":  `{"ecosystem":"oci","coordinate":"c","version":"1","artifactDigest":"` + digest + `","platforms":{"linux":"` + digest + `"}}`,
		"bad platform digest": `{"ecosystem":"oci","coordinate":"c","version":"1","artifactDigest":"` + digest +
			`","platforms":{"linux/arm64":"deadbeef"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			parsed, diags := ParsePublishedMember([]byte(raw))
			if len(diags) != 0 {
				t.Fatalf("parse: %v", diags)
			}
			if !hasCode(ValidatePublishedMember(parsed), "invalid-published-member") {
				t.Fatal("an invalid member was accepted")
			}
		})
	}

	if !hasCode(ValidatePublishedMember(nil), "invalid-published-member") {
		t.Fatal("a nil member must be reported, not panic")
	}
}

// TestMemberProbeIsStrict pins the dry-run probe's parse and validation
// contract: an unknown field or state is a rejection, each state carries the
// digest evidence it claims, and the registry endpoint carries no credential.
func TestMemberProbeIsStrict(t *testing.T) {
	const local = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	const remote = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	base := `"ecosystem":"npm","coordinate":"@acme/widget","version":"1.2.3","registry":"https://registry.example.test"`

	for name, raw := range map[string]string{
		"absent":            `{` + base + `,"state":"absent"}`,
		"absent anonymous":  `{` + base + `,"state":"absent","artifactDigest":"` + local + `","anonymous":true}`,
		"identical":         `{` + base + `,"state":"identical","artifactDigest":"` + local + `","registryDigest":"` + local + `"}`,
		"conflict":          `{` + base + `,"state":"conflict","artifactDigest":"` + local + `","registryDigest":"` + remote + `","reason":"digests differ"}`,
		"conflict no local": `{` + base + `,"state":"conflict","reason":"the dry run built no artifact to compare"}`,
		"unverified":        `{` + base + `,"state":"unverified","reason":"connection refused"}`,
		"bare host": `{"ecosystem":"oci","coordinate":"acme/api","version":"1.2.3","registry":"registry.example.test:5000",` +
			`"state":"absent"}`,
		"one platform": `{` + base + `,"state":"absent","platform":"linux/arm64"}`,
	} {
		t.Run("valid "+name, func(t *testing.T) {
			probe, diags := ParseMemberProbe([]byte(raw))
			if len(diags) != 0 {
				t.Fatalf("parse: %v", diags)
			}
			if diags := ValidateMemberProbe(probe); len(diags) != 0 {
				t.Fatalf("valid probe reported %v", diags)
			}
		})
	}

	if _, diags := ParseMemberProbe([]byte(`{` + base + `,"state":"absent","tag":"latest"}`)); !hasCode(diags, "invalid-member-probe") {
		t.Fatalf("an unknown field was accepted: %v", diags)
	}

	for name, tc := range map[string]struct {
		raw   string
		field string
	}{
		"bad ecosystem": {`{"ecosystem":"NPM","coordinate":"c","version":"1","registry":"r","state":"absent"}`, "ecosystem"},
		"no coordinate": {`{"ecosystem":"npm","coordinate":"","version":"1","registry":"r","state":"absent"}`, "coordinate"},
		"no version":    {`{"ecosystem":"npm","coordinate":"c","version":"","registry":"r","state":"absent"}`, "version"},
		"no registry":   {`{"ecosystem":"npm","coordinate":"c","version":"1","state":"absent"}`, "registry"},
		"registry user info": {
			`{"ecosystem":"npm","coordinate":"c","version":"1","registry":"https://user:secret@r.test","state":"absent"}`, "registry",
		},
		"registry query": {
			`{"ecosystem":"npm","coordinate":"c","version":"1","registry":"https://r.test/?token=secret","state":"absent"}`, "registry",
		},
		"bare host user info": {
			`{"ecosystem":"oci","coordinate":"c","version":"1","registry":"user:secret@r.test","state":"absent"}`, "registry",
		},
		"bad platform":              {`{` + base + `,"state":"absent","platform":"linux"}`, "platform"},
		"unknown state":             {`{` + base + `,"state":"present"}`, "state"},
		"no state":                  {`{` + base + `}`, "state"},
		"short digest":              {`{` + base + `,"state":"absent","artifactDigest":"sha256:abc"}`, "artifactDigest"},
		"short registry digest":     {`{` + base + `,"state":"unverified","reason":"r","registryDigest":"deadbeef"}`, "registryDigest"},
		"absent with a held digest": {`{` + base + `,"state":"absent","registryDigest":"` + remote + `"}`, "registryDigest"},
		"identical without digests": {`{` + base + `,"state":"identical"}`, "registryDigest"},
		"identical with other digest": {
			`{` + base + `,"state":"identical","artifactDigest":"` + local + `","registryDigest":"` + remote + `"}`, "registryDigest",
		},
		"conflict with equal digests": {
			`{` + base + `,"state":"conflict","artifactDigest":"` + local + `","registryDigest":"` + local + `","reason":"r"}`, "registryDigest",
		},
		"conflict without reason":   {`{` + base + `,"state":"conflict","registryDigest":"` + remote + `"}`, "reason"},
		"unverified without reason": {`{` + base + `,"state":"unverified","reason":"  "}`, "reason"},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, diags := ParseMemberProbe([]byte(tc.raw))
			if len(diags) != 0 {
				t.Fatalf("parse: %v", diags)
			}
			diags = ValidateMemberProbe(parsed)
			if len(diags) != 1 || diags[0].Code != "invalid-member-probe" || diags[0].Field != tc.field {
				t.Fatalf("diagnostics = %v, want one invalid-member-probe on %q", diags, tc.field)
			}
			if strings.Contains(diags[0].Message, "secret") {
				t.Fatalf("diagnostic %q repeats the credential it rejects", diags[0].Message)
			}
		})
	}

	if !hasCode(ValidateMemberProbe(nil), "invalid-member-probe") {
		t.Fatal("a nil probe must be reported, not panic")
	}
}

// ecosystemInvalidFixtureCodes maps each ecosystem counter-example to the exact
// diagnostic codes it must produce, in order. This module has no central
// fixture→code table, so the profile corpus carries its own: a new
// counter-example is one fixture file and one line here.
var ecosystemInvalidFixtureCodes = map[string][]string{
	// THE ID IS AN IDENTIFIER — it becomes a registry path segment and a
	// workspace `registries` key, so it cannot carry case or underscores.
	"ecosystem-bad-id.json": {"invalid-ecosystem-profile"},
	// SELF-CONTAINED — a pattern that does not compile would fail at plan time,
	// on every project of the ecosystem at once.
	"ecosystem-bad-pattern.json": {"invalid-ecosystem-profile"},
	// SELF-CONTAINED — a publish job that does not exist validates here and
	// fails at release time, the one path with no recovery.
	"ecosystem-publish-missing.json": {"invalid-ecosystem-profile"},
	// ONE OWNER — two declarations in one file are the same divergence risk as
	// two extensions, reported before resolution ever sees the manifest.
	"ecosystem-duplicate-id.json": {"duplicate-ecosystem-profile"},
	// ONE OWNER — `uses` names ecosystems SOMEONE ELSE owns; using one's own is
	// a second declaration spelled as a reference.
	"ecosystem-uses-own.json": {"ecosystem-use-of-own-profile"},
	// CLOSED VOCABULARY — `channel` says which projection the distribution
	// backend implements, so a third value asks for behavior nobody has.
	"ecosystem-bad-channel.json": {"invalid-ecosystem-profile"},
}

// TestEcosystemFixturesReportStableCodes pins each counter-example to its own
// rule through the entry point consumers call. Exact-match rather than
// contains: a fixture that started failing for a second, unrelated reason would
// stop certifying the rule it was written for.
func TestEcosystemFixturesReportStableCodes(t *testing.T) {
	for name, codes := range ecosystemInvalidFixtureCodes {
		t.Run(name, func(t *testing.T) {
			m := loadFixtureManifest(t, filepath.Join("fixtures/invalid", name))
			if got := diagCodes(FullValidateManifest(m)); !reflect.DeepEqual(got, codes) {
				t.Fatalf("codes = %v, want %v", got, codes)
			}
		})
	}

	// Positive control: the valid profile fixtures must clear the same path, so
	// "the corpus fails" never means "every manifest with a profile fails".
	for _, name := range []string{"ecosystem-owner.json", "ecosystem-uses.json"} {
		t.Run(name, func(t *testing.T) {
			m := loadFixtureManifest(t, filepath.Join("fixtures/valid", name))
			if diags := FullValidateManifest(m); len(diags) != 0 {
				t.Fatalf("valid profile fixture reported %v", diags)
			}
		})
	}

	// Additivity control: a manifest that declares no profile gets no verdict,
	// and attaching a defective profile to the SAME manifest must make the
	// validator speak — so silence means "nothing to say", not "not wired up".
	m := loadFixtureManifest(t, "fixtures/valid/minimal.json")
	if diags := validateEcosystems(m); len(diags) != 0 {
		t.Fatalf("a manifest without profiles got a verdict: %v", diags)
	}
	broken := npmProfile()
	broken.ID = "NPM"
	m.Ecosystems = []EcosystemProfile{broken}
	if !hasCode(ValidateManifest(m), "invalid-ecosystem-profile") {
		t.Fatal("control: a defective profile on this manifest must be reported")
	}
}

// TestValidateProfileRequiresARegistriesObjectSchema pins the one profile field
// that is itself a schema. The workspace validates a registry entry with it
// without knowing the ecosystem, so a declaration that is not an object schema
// would leave that entry unvalidatable.
func TestValidateProfileRequiresARegistriesObjectSchema(t *testing.T) {
	commands := map[string]CommandDefinition{"publish-npm": {}}
	for name, raw := range map[string]json.RawMessage{
		"absent":     nil,
		"not json":   json.RawMessage(`{`),
		"not object": json.RawMessage(`{"type":"string"}`),
	} {
		t.Run(name, func(t *testing.T) {
			profile := npmProfile()
			profile.Registries = raw
			if !hasCode(ValidateProfile("ecosystems[0]", profile, commands), "invalid-ecosystem-profile") {
				t.Fatal("an unusable registries schema was accepted")
			}
		})
	}
	if diags := ValidateProfile("ecosystems[0]", npmProfile(), commands); len(diags) != 0 {
		t.Fatalf("control: a valid profile reported %v", diags)
	}
}
