// Ecosystem profiles: an extension declares what an ecosystem IS.
//
// The release-set protocol used to name the ecosystems it knew — npm, go, oci,
// archive, put — and the CLI hard-coded the two it coordinated. Adding a
// language therefore meant changing the protocol, the CLI and the distribution
// backend at once, and the shape of a registry entry lived in prose nobody
// could validate.
//
// An extension already owns the publish job of its ecosystem and the metadata
// it emits for a release set, so it is the right owner of everything an
// ecosystem is: the shape of a coordinate, the shape and ordering of a version,
// whether the ecosystem has a native channel projection and which channel names
// it accepts, the JSON Schema of its entry in the workspace `registries`
// section, and the job that publishes a member.
//
// Two rules make a profile trustworthy (ADR 0001):
//
//   - ONE OWNER PER PROFILE. An extension that publishes to an ecosystem it
//     does not define lists the id under `uses` and never redeclares it. Two
//     declarations of one ecosystem would diverge on the first pattern edit —
//     even byte-identical ones, since only one of them would be updated — so
//     ResolveProfiles refuses a second owner rather than comparing bytes.
//
//   - A PROFILE IS SELF-CONTAINED. Both patterns compile as RE2, the ordering
//     and channel vocabularies are closed, and `publish` names a command of the
//     SAME manifest. A profile whose publish job does not exist would validate
//     here and fail at release time, on the one path that has no recovery.
//
// Channel names carry a portable alphabet (PortableChannelPattern) that every
// ecosystem must accept, and a profile may narrow it further with
// `channelEncoding` — it may never widen it, because the distribution backend
// stores channel names for every ecosystem in one namespace.

package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// EcosystemIDPattern is the RE2 pattern an ecosystem identifier must match.
// It is deliberately narrower than a coordinate: an id is a path segment in
// registry URLs and a map key in the workspace `registries` section.
const EcosystemIDPattern = `^[a-z][a-z0-9-]{0,31}$`

// PortableChannelPattern is the alphabet EVERY ecosystem must accept for a
// channel name. A profile's ChannelEncoding may narrow it; nothing may widen
// it, because one distribution namespace holds the channels of every ecosystem.
const PortableChannelPattern = `^[a-z0-9][a-z0-9._-]{0,63}$`

// Channel projection vocabularies and version orderings. Both are closed sets:
// a profile that named a third value would be asking the distribution backend
// for a behavior it does not implement.
const (
	// ChannelNative marks an ecosystem whose registry has a native channel
	// projection (an npm dist-tag, an OCI tag), so `channel set` publishes into
	// the ecosystem itself.
	ChannelNative = "native"
	// ChannelNone marks an ecosystem with no native channel. It is followed by
	// `--release` or `--version` only, and `upgrade --channel` skips it.
	ChannelNone = "none"
	// OrderingSemver orders versions by semantic-version precedence.
	OrderingSemver = "semver"
	// OrderingString orders versions by byte comparison, for ecosystems whose
	// versions are opaque strings.
	OrderingString = "string"
)

// EcosystemProfile is one ecosystem, declared by the extension that owns it.
type EcosystemProfile struct {
	// ID is the ecosystem identifier, matching EcosystemIDPattern. It is the
	// key of the ecosystem's entry in the workspace `registries` section and
	// the value of ReleaseSetMember.Ecosystem.
	ID string `json:"id"`
	// Coordinate is the rule a member's coordinate — the package name in this
	// ecosystem — must satisfy.
	Coordinate PatternRule `json:"coordinate"`
	// Version is the rule and the ordering a member's version must satisfy.
	Version VersionRule `json:"version"`
	// Channel is the channel projection of this ecosystem: ChannelNative when
	// the registry has one, ChannelNone otherwise.
	Channel string `json:"channel"`
	// ChannelEncoding optionally narrows PortableChannelPattern for this
	// ecosystem. It must be a compilable RE2 pattern and may only be stricter:
	// a channel name is valid when it matches BOTH patterns.
	ChannelEncoding string `json:"channelEncoding,omitempty"`
	// Registries is the JSON Schema of this ecosystem's entry in the workspace
	// `registries` section. It must be a schema object of `"type": "object"`,
	// so the workspace can validate a registry entry without knowing the
	// ecosystem.
	Registries json.RawMessage `json:"registries"`
	// Publish is the name of the command, in the SAME manifest, that publishes
	// a member of this ecosystem and emits the published-member event.
	Publish string `json:"publish"`
}

// PatternRule is a single RE2 pattern a value must match.
type PatternRule struct {
	// Pattern is the RE2 pattern.
	Pattern string `json:"pattern"`
}

// VersionRule is the shape and the ordering of an ecosystem's versions.
type VersionRule struct {
	// Pattern is the RE2 pattern a version must match.
	Pattern string `json:"pattern"`
	// Ordering is how two versions compare: OrderingSemver or OrderingString.
	Ordering string `json:"ordering"`
}

// PublishedMember is what a publish job emits, once per artifact it produced,
// for the release set being assembled. A project may produce several members in
// one ecosystem, so the event carries its own coordinate rather than inheriting
// the project's identity.
type PublishedMember struct {
	// Ecosystem is the id of the profile this member belongs to.
	Ecosystem string `json:"ecosystem"`
	// Coordinate is the member's package name in that ecosystem.
	Coordinate string `json:"coordinate"`
	// Version is the version the publish job actually published.
	Version string `json:"version"`
	// ArtifactDigest is the content identity of the published artifact, as
	// "sha256:" followed by 64 lowercase hex characters.
	ArtifactDigest string `json:"artifactDigest"`
	// Platforms maps "os/arch" to the digest of that platform's artifact, for a
	// member published as a multi-platform set. Absent for a single artifact.
	Platforms map[string]string `json:"platforms,omitempty"`
}

// PublishedMemberEventKind is the runtime-event kind a publish job emits a
// PublishedMember under.
const PublishedMemberEventKind = "published-member"

// NamedManifest pairs a parsed manifest with the extension name it was loaded
// under, so resolution diagnostics can name the manifest at fault.
type NamedManifest struct {
	// Name is the extension name.
	Name string
	// Manifest is the parsed manifest.
	Manifest *Manifest
}

// OwnedProfile is a profile with its owner, for profiles the CLI supplies
// itself rather than reading from an installed extension.
type OwnedProfile struct {
	// Owner is the name to report as the profile's owner.
	Owner string
	// Profile is the ecosystem profile.
	Profile EcosystemProfile
}

// ProfileRegistry is the resolved set of ecosystem profiles of one workspace:
// every profile the installed extensions own, plus the built-in ones. It is the
// only place the CLI learns which ecosystems exist.
type ProfileRegistry struct {
	profiles map[string]EcosystemProfile
	owners   map[string]string
	// coordinate, version and channel hold the compiled patterns, so a
	// validation call never re-compiles.
	coordinate map[string]*regexp.Regexp
	version    map[string]*regexp.Regexp
	channel    map[string]*regexp.Regexp
}

var portableChannel = regexp.MustCompile(PortableChannelPattern)

var ecosystemID = regexp.MustCompile(EcosystemIDPattern)

// ResolveProfiles resolves the ecosystem profiles of a set of manifests plus a
// set of built-in profiles into one registry.
//
// It refuses a profile declared by two owners — byte-identical or not, since
// two declarations diverge on the first edit to one of them — and a `uses`
// entry no manifest and no built-in declares. Both diagnostics name the
// manifests involved, because the fix is always in one of them.
//
// Manifests are resolved in the order given; diagnostics are deterministic for
// a given order.
func ResolveProfiles(manifests []NamedManifest, builtin []OwnedProfile) (*ProfileRegistry, []diag.Diagnostic) {
	registry := &ProfileRegistry{
		profiles:   make(map[string]EcosystemProfile),
		owners:     make(map[string]string),
		coordinate: make(map[string]*regexp.Regexp),
		version:    make(map[string]*regexp.Regexp),
		channel:    make(map[string]*regexp.Regexp),
	}
	var diags []diag.Diagnostic

	declare := func(owner, field string, profile EcosystemProfile) {
		if previous, taken := registry.owners[profile.ID]; taken {
			diags = append(diags, diag.Errorf(
				"duplicate-ecosystem-profile",
				field,
				"ecosystem %q is already declared by %q; %q must reference it with uses instead of redeclaring it",
				profile.ID, previous, owner,
			))
			return
		}
		registry.owners[profile.ID] = owner
		registry.profiles[profile.ID] = profile
		registry.compile(profile)
	}

	for _, owned := range builtin {
		declare(owned.Owner, "builtin."+owned.Profile.ID, owned.Profile)
	}
	for _, named := range manifests {
		if named.Manifest == nil {
			continue
		}
		for i, profile := range named.Manifest.Ecosystems {
			declare(named.Name, fmt.Sprintf("%s.ecosystems[%d]", named.Name, i), profile)
		}
	}

	for _, named := range manifests {
		if named.Manifest == nil {
			continue
		}
		for i, id := range named.Manifest.Uses {
			if _, known := registry.owners[id]; !known {
				diags = append(diags, diag.Errorf(
					"unknown-ecosystem-use",
					fmt.Sprintf("%s.uses[%d]", named.Name, i),
					"extension %q uses ecosystem %q, which no installed extension declares",
					named.Name, id,
				))
			}
		}
	}

	return registry, diags
}

// compile stores the compiled patterns of a profile. Patterns that do not
// compile are left absent: ValidateProfile is what reports them, and a registry
// built from an unvalidated manifest must not panic.
func (r *ProfileRegistry) compile(p EcosystemProfile) {
	if compiled, err := regexp.Compile(p.Coordinate.Pattern); err == nil {
		r.coordinate[p.ID] = compiled
	}
	if compiled, err := regexp.Compile(p.Version.Pattern); err == nil {
		r.version[p.ID] = compiled
	}
	if p.ChannelEncoding != "" {
		if compiled, err := regexp.Compile(p.ChannelEncoding); err == nil {
			r.channel[p.ID] = compiled
		}
	}
}

// IDs returns every resolved ecosystem id, sorted.
func (r *ProfileRegistry) IDs() []string {
	ids := make([]string, 0, len(r.profiles))
	for id := range r.profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Profile returns the profile registered for an id, its owner, and whether it
// exists.
func (r *ProfileRegistry) Profile(id string) (EcosystemProfile, string, bool) {
	profile, ok := r.profiles[id]
	if !ok {
		return EcosystemProfile{}, "", false
	}
	return profile, r.owners[id], true
}

// ValidateCoordinate reports whether a coordinate satisfies the profile's
// coordinate rule.
func (r *ProfileRegistry) ValidateCoordinate(id, coordinate string) error {
	pattern, ok := r.coordinate[id]
	if !ok {
		return fmt.Errorf("unknown ecosystem %q", id)
	}
	if !pattern.MatchString(coordinate) {
		return fmt.Errorf("coordinate %q does not match the %s pattern %s", coordinate, id, pattern)
	}
	return nil
}

// ValidateVersion reports whether a version satisfies the profile's version
// rule. Ordering is not consulted: it governs comparison, not admission.
func (r *ProfileRegistry) ValidateVersion(id, version string) error {
	pattern, ok := r.version[id]
	if !ok {
		return fmt.Errorf("unknown ecosystem %q", id)
	}
	if !pattern.MatchString(version) {
		return fmt.Errorf("version %q does not match the %s pattern %s", version, id, pattern)
	}
	return nil
}

// ValidateChannelName reports whether a channel name is usable for an
// ecosystem. A name must match the portable alphabet AND, when the profile
// declares one, the profile's own encoding — the profile narrows, never widens.
func (r *ProfileRegistry) ValidateChannelName(id, channel string) error {
	if _, ok := r.profiles[id]; !ok {
		return fmt.Errorf("unknown ecosystem %q", id)
	}
	if !portableChannel.MatchString(channel) {
		return fmt.Errorf("channel %q does not match the portable channel alphabet %s", channel, PortableChannelPattern)
	}
	if encoding, ok := r.channel[id]; ok && !encoding.MatchString(channel) {
		return fmt.Errorf("channel %q does not match the %s channel encoding %s", channel, id, encoding)
	}
	return nil
}

// HasNativeChannel reports whether an ecosystem projects channels natively. An
// unknown id reports false: an ecosystem nobody declares has no projection.
func (r *ProfileRegistry) HasNativeChannel(id string) bool {
	profile, ok := r.profiles[id]
	return ok && profile.Channel == ChannelNative
}

// ValidateProfile checks one profile in isolation: its id, its patterns, its
// closed vocabularies, the shape of its registries schema, and that its publish
// job is a command of the manifest that declares it.
func ValidateProfile(field string, p EcosystemProfile, commands map[string]CommandDefinition) []diag.Diagnostic {
	var diags []diag.Diagnostic

	if !ecosystemID.MatchString(p.ID) {
		diags = append(diags, diag.Errorf("invalid-ecosystem-profile", field+".id",
			"ecosystem id %q must match %s", p.ID, EcosystemIDPattern))
	}
	for name, pattern := range map[string]string{
		"coordinate.pattern": p.Coordinate.Pattern,
		"version.pattern":    p.Version.Pattern,
	} {
		if pattern == "" {
			diags = append(diags, diag.Errorf("invalid-ecosystem-profile", field+"."+name,
				"%s is required", name))
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			diags = append(diags, diag.Errorf("invalid-ecosystem-profile", field+"."+name,
				"%s %q is not a valid RE2 pattern: %v", name, pattern, err))
		}
	}
	if p.Version.Ordering != OrderingSemver && p.Version.Ordering != OrderingString {
		diags = append(diags, diag.Errorf("invalid-ecosystem-profile", field+".version.ordering",
			"ordering %q must be one of: %s, %s", p.Version.Ordering, OrderingSemver, OrderingString))
	}
	if p.Channel != ChannelNative && p.Channel != ChannelNone {
		diags = append(diags, diag.Errorf("invalid-ecosystem-profile", field+".channel",
			"channel %q must be one of: %s, %s", p.Channel, ChannelNative, ChannelNone))
	}
	if p.ChannelEncoding != "" {
		if _, err := regexp.Compile(p.ChannelEncoding); err != nil {
			diags = append(diags, diag.Errorf("invalid-ecosystem-profile", field+".channelEncoding",
				"channelEncoding %q is not a valid RE2 pattern: %v", p.ChannelEncoding, err))
		}
	}
	diags = append(diags, validateRegistriesSchema(field+".registries", p.Registries)...)
	if p.Publish == "" {
		diags = append(diags, diag.Errorf("invalid-ecosystem-profile", field+".publish",
			"publish is required: a profile with no publish job can never contribute a member"))
	} else if _, declared := commands[p.Publish]; !declared {
		diags = append(diags, diag.Errorf("invalid-ecosystem-profile", field+".publish",
			"publish %q is not a command of this manifest", p.Publish))
	}

	// Sort so a profile with several defects reports the same order every run.
	sort.SliceStable(diags, func(i, j int) bool { return diags[i].Field < diags[j].Field })
	return diags
}

// validateRegistriesSchema checks that a profile's registries declaration is a
// JSON Schema object describing an object. Anything else — a string, an array,
// a schema of another type — would make the workspace's registry entry
// unvalidatable, which is the whole point of carrying it here.
func validateRegistriesSchema(field string, raw json.RawMessage) []diag.Diagnostic {
	if len(raw) == 0 {
		return []diag.Diagnostic{diag.Errorf("invalid-ecosystem-profile", field,
			"registries is required: it is the schema of this ecosystem's workspace registry entry")}
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return []diag.Diagnostic{diag.Errorf("invalid-ecosystem-profile", field,
			"registries must be a JSON Schema object: %v", err)}
	}
	if schema["type"] != "object" {
		return []diag.Diagnostic{diag.Errorf("invalid-ecosystem-profile", field+".type",
			"registries schema type must be \"object\", got %v", schema["type"])}
	}
	return nil
}

// validateEcosystems is the manifest-level half of profile validation: the
// per-profile rules, plus the two rules that only a whole manifest can state —
// an id declared twice in the same file, and a `uses` entry the same file
// declares (which would be a second owner spelled as a reference).
func validateEcosystems(m *Manifest) []diag.Diagnostic {
	if m == nil || (len(m.Ecosystems) == 0 && len(m.Uses) == 0) {
		return nil
	}
	var diags []diag.Diagnostic
	declared := make(map[string]int, len(m.Ecosystems))
	for i, profile := range m.Ecosystems {
		field := fmt.Sprintf("ecosystems[%d]", i)
		if previous, duplicate := declared[profile.ID]; duplicate {
			diags = append(diags, diag.Errorf("duplicate-ecosystem-profile", field+".id",
				"ecosystem %q is already declared at ecosystems[%d]", profile.ID, previous))
		} else {
			declared[profile.ID] = i
		}
		diags = append(diags, ValidateProfile(field, profile, m.Commands)...)
	}
	for i, id := range m.Uses {
		field := fmt.Sprintf("uses[%d]", i)
		if !ecosystemID.MatchString(id) {
			diags = append(diags, diag.Errorf("invalid-ecosystem-profile", field,
				"ecosystem id %q must match %s", id, EcosystemIDPattern))
			continue
		}
		if _, own := declared[id]; own {
			diags = append(diags, diag.Errorf("ecosystem-use-of-own-profile", field,
				"ecosystem %q is declared by this manifest; uses names ecosystems another extension owns", id))
		}
	}
	return diags
}

// ParsePublishedMember decodes a published-member event strictly: an unknown
// field is a rejection, because a publish job that emitted a field this build
// does not know would have its member silently truncated into the release set.
func ParsePublishedMember(data []byte) (*PublishedMember, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var member PublishedMember
	if err := dec.Decode(&member); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("invalid-published-member", "", "failed to parse published member: %v", err),
		}
	}
	return &member, nil
}

// digestPattern is the artifact-digest form every ecosystem shares: sha256 over
// the published bytes, lowercase hex.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// platformPattern is the "os/arch" key form of PublishedMember.Platforms.
var platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)

// ValidatePublishedMember checks a published-member event's own invariants. It
// does not consult a ProfileRegistry: the coordinate and version rules belong
// to the profile, and this package validates the event shape that carries them.
func ValidatePublishedMember(m *PublishedMember) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{diag.Errorf("invalid-published-member", "", "published member is nil")}
	}
	var diags []diag.Diagnostic
	if !ecosystemID.MatchString(m.Ecosystem) {
		diags = append(diags, diag.Errorf("invalid-published-member", "ecosystem",
			"ecosystem %q must match %s", m.Ecosystem, EcosystemIDPattern))
	}
	if m.Coordinate == "" {
		diags = append(diags, diag.Errorf("invalid-published-member", "coordinate", "coordinate is required"))
	}
	if m.Version == "" {
		diags = append(diags, diag.Errorf("invalid-published-member", "version", "version is required"))
	}
	if !digestPattern.MatchString(m.ArtifactDigest) {
		diags = append(diags, diag.Errorf("invalid-published-member", "artifactDigest",
			"artifactDigest %q must be sha256: followed by 64 lowercase hex characters", m.ArtifactDigest))
	}
	for _, platform := range sortedKeys(m.Platforms) {
		field := "platforms." + platform
		if !platformPattern.MatchString(platform) {
			diags = append(diags, diag.Errorf("invalid-published-member", field,
				"platform key %q must be os/arch", platform))
		}
		if !digestPattern.MatchString(m.Platforms[platform]) {
			diags = append(diags, diag.Errorf("invalid-published-member", field,
				"platform digest %q must be sha256: followed by 64 lowercase hex characters", m.Platforms[platform]))
		}
	}
	return diags
}
