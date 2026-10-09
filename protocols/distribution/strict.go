package distribution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Stable distribution protocol diagnostic codes.
const (
	ErrorCodeParseError                  = "distribution.parse_error"
	ErrorCodeUnknownField                = "distribution.unknown_field"
	ErrorCodeDuplicateField              = "distribution.duplicate_field"
	ErrorCodeMissingField                = "distribution.missing_field"
	ErrorCodeNullField                   = "distribution.null_field"
	ErrorCodeInvalidProtocolVersion      = "distribution.invalid_protocol_version"
	ErrorCodeBoundsExceeded              = "distribution.bounds_exceeded"
	ErrorCodeInvalidNamespace            = "distribution.invalid_namespace"
	ErrorCodeInvalidChannel              = "distribution.invalid_channel"
	ErrorCodeInvalidSelector             = "distribution.invalid_selector"
	ErrorCodeInvalidEcosystem            = "distribution.invalid_ecosystem"
	ErrorCodeInvalidCoordinate           = "distribution.invalid_coordinate"
	ErrorCodeInvalidVersion              = "distribution.invalid_version"
	ErrorCodeInvalidArtifactDigest       = "distribution.invalid_artifact_digest"
	ErrorCodeDuplicateMember             = "distribution.duplicate_member"
	ErrorCodeDuplicateDependency         = "distribution.duplicate_dependency"
	ErrorCodeUnclosedDependency          = "distribution.unclosed_dependency"
	ErrorCodeDependencyVersionMismatch   = "distribution.dependency_version_mismatch"
	ErrorCodeNonCanonical                = "distribution.non_canonical"
	ErrorCodeInvalidRef                  = "distribution.invalid_ref"
	ErrorCodeInvalidOutcome              = "distribution.invalid_outcome"
	ErrorCodeRefMismatch                 = "distribution.ref_mismatch"
	ErrorCodeDiagnosticsTruncated        = "distribution.diagnostics_truncated"
	ErrorCodeInvalidSourceRevision       = "distribution.invalid_source_revision"
	ErrorCodeInvalidSourceTree           = "distribution.invalid_source_tree"
	ErrorCodeInvalidSelectionFingerprint = "distribution.invalid_selection_fingerprint"
	ErrorCodeInvalidPlatform             = "distribution.invalid_platform"
	ErrorCodeInvalidProject              = "distribution.invalid_project"
	ErrorCodeInvalidKind                 = "distribution.invalid_kind"
	ErrorCodeFieldNotAllowed             = "distribution.field_not_allowed"
	ErrorCodeInvalidVisibility           = "distribution.invalid_visibility"
	ErrorCodeInvalidMirrorTarget         = "distribution.invalid_mirror_target"
	ErrorCodeInvalidGeneration           = "distribution.invalid_generation"
)

// ValidDiagnosticCodes is the closed automation taxonomy emitted by this
// package. Consumers branch on codes and fields, never human messages.
var ValidDiagnosticCodes = map[string]bool{
	ErrorCodeParseError: true, ErrorCodeUnknownField: true, ErrorCodeDuplicateField: true,
	ErrorCodeMissingField: true, ErrorCodeNullField: true,
	ErrorCodeInvalidProtocolVersion: true, ErrorCodeBoundsExceeded: true,
	ErrorCodeInvalidNamespace: true, ErrorCodeInvalidChannel: true, ErrorCodeInvalidSelector: true,
	ErrorCodeInvalidEcosystem: true, ErrorCodeInvalidCoordinate: true,
	ErrorCodeInvalidVersion: true, ErrorCodeInvalidArtifactDigest: true,
	ErrorCodeDuplicateMember: true, ErrorCodeDuplicateDependency: true,
	ErrorCodeUnclosedDependency: true, ErrorCodeDependencyVersionMismatch: true,
	ErrorCodeNonCanonical: true, ErrorCodeInvalidRef: true, ErrorCodeInvalidOutcome: true,
	ErrorCodeRefMismatch: true, ErrorCodeDiagnosticsTruncated: true,
	ErrorCodeInvalidSourceRevision: true, ErrorCodeInvalidSourceTree: true, ErrorCodeInvalidSelectionFingerprint: true,
	ErrorCodeInvalidPlatform: true, ErrorCodeFieldNotAllowed: true,
	ErrorCodeInvalidProject: true, ErrorCodeInvalidKind: true,
	ErrorCodeInvalidVisibility: true, ErrorCodeInvalidMirrorTarget: true, ErrorCodeInvalidGeneration: true,
}

var (
	namespacePattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	ecosystemPattern      = regexp.MustCompile(EcosystemPattern)
	channelPattern        = regexp.MustCompile(ChannelPattern)
	mirrorTargetPattern   = regexp.MustCompile(MirrorTargetPattern)
	digestPattern         = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	refIDPattern          = regexp.MustCompile(`^rs_[0-9a-f]{64}$`)
	sourceRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sourceTreePattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	platformPattern       = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
	memberProjectPattern  = regexp.MustCompile(MemberProjectPattern)
)

type coordinateKey struct {
	ecosystem  Ecosystem
	coordinate string
}

// IsPortableChannel reports whether name is in the portable channel alphabet:
// valid at once as an npm dist-tag, a Go query, an OCI tag, and a put channel.
func IsPortableChannel(name string) bool { return channelPattern.MatchString(name) }

// IsMemberProject reports whether value is a recordable member project: the
// canonical logical project id, without a leading slash. A publisher whose
// project identity falls outside this grammar records no project rather than an
// unrepresentable one; attribution is optional, and a release must not fail
// because of the shape of a directory name.
func IsMemberProject(value string) bool { return memberProjectPattern.MatchString(value) }

// MemberProjectID returns the identity under which a release-set member names
// a workspace project: the project's canonical logical id without its leading
// slash ("/sites/docs" becomes "sites/docs"). The id already omits a grouping
// folder such as "(internal)/", so the result does too. It does not check the
// grammar; call IsMemberProject for that.
//
// Every artifact that names the same workload derives the name here. A
// deployer that matches a deployment declaration to its release-set member
// then compares two values from one function, not a project name with a path.
func MemberProjectID(projectID string) string { return strings.TrimPrefix(projectID, "/") }

// EncodeTagAsChannel maps a git tag to the immutable channel a tagged publish
// creates: every slash becomes a hyphen, so ts/v0.3.0 becomes ts-v0.3.0. It
// fails when the encoded name is still outside the portable alphabet, because
// the provider refuses any other name.
func EncodeTagAsChannel(tag string) (string, error) {
	encoded := strings.ReplaceAll(tag, "/", "-")
	if !IsPortableChannel(encoded) {
		return "", fmt.Errorf("tag %q encodes to %q, which is not a portable channel name (want %s)", tag, encoded, ChannelPattern)
	}
	return encoded, nil
}

// ParseReleaseSet strictly decodes one bounded release-set document. Semantic
// validation and normalization are separate so callers can surface all
// field-addressable findings from a well-formed wire document.
func ParseReleaseSet(data []byte) (*ReleaseSet, []diag.Diagnostic) {
	return strictDecode[ReleaseSet](data, releaseSetShape)
}

// ParseAndValidateReleaseSet strictly parses, validates, and returns a
// normalized copy. It returns nil whenever a hard diagnostic is present.
func ParseAndValidateReleaseSet(data []byte) (*ReleaseSet, []diag.Diagnostic) {
	releaseSet, diagnostics := ParseReleaseSet(data)
	if releaseSet == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateReleaseSet(releaseSet)...)
	diagnostics = boundDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	return NormalizeReleaseSet(releaseSet), diagnostics
}

// ValidateReleaseSet validates bounds, provenance, digests, uniqueness by
// (ecosystem, coordinate), and full exact-version dependency closure. It never
// validates the shape of a coordinate or a version beyond bounds and control
// characters: those belong to the owning ecosystem profile and to the registry.
// Input ordering is not an error; NormalizeReleaseSet defines it and
// ParseCanonicalReleaseSet enforces bytes.
func ValidateReleaseSet(releaseSet *ReleaseSet) []diag.Diagnostic {
	if releaseSet == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "release set is nil")}
	}
	var diagnostics []diag.Diagnostic
	if releaseSet.ProtocolVersion != ProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocolVersion %d is not supported (want exact integer %d)", releaseSet.ProtocolVersion, ProtocolVersion))
	}
	diagnostics = append(diagnostics, validateNamespace(releaseSet.Namespace)...)
	if len(releaseSet.Members) == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeMissingField, "members", "a release set must contain at least one member"))
	}
	if len(releaseSet.Members) > MaxMembers {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, "members", "%d members exceed the limit of %d", len(releaseSet.Members), MaxMembers))
	}

	memberLimit := min(len(releaseSet.Members), MaxMembers)
	members := make(map[coordinateKey]struct {
		version string
		field   string
	}, memberLimit)
	for i := 0; i < memberLimit; i++ {
		member := releaseSet.Members[i]
		field := fmt.Sprintf("members[%d]", i)
		diagnostics = append(diagnostics, validateEcosystem(field+".ecosystem", member.Ecosystem)...)
		diagnostics = append(diagnostics, validateCoordinate(field+".coordinate", member.Coordinate)...)
		diagnostics = append(diagnostics, validateVersion(field+".version", member.Version)...)
		if !digestPattern.MatchString(member.ArtifactDigest) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidArtifactDigest, field+".artifactDigest", "artifact digest must be sha256:<64 lowercase hex>"))
		}
		diagnostics = append(diagnostics, validateProvenance(field, member)...)
		if len(diagnostics) >= MaxDiagnostics {
			return truncateDiagnostics(diagnostics)
		}
		key := coordinateKey{member.Ecosystem, member.Coordinate}
		if first, exists := members[key]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateMember, field, "member (%q, %q) duplicates %s", member.Ecosystem, member.Coordinate, first.field))
		} else {
			members[key] = struct {
				version string
				field   string
			}{member.Version, field}
		}
		if len(member.Dependencies) > MaxDependenciesPerMember {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, field+".dependencies", "%d dependencies exceed the per-member limit of %d", len(member.Dependencies), MaxDependenciesPerMember))
		}
		seenDependencies := make(map[coordinateKey]string, min(len(member.Dependencies), MaxDependenciesPerMember))
		dependencyLimit := min(len(member.Dependencies), MaxDependenciesPerMember)
		for j := 0; j < dependencyLimit; j++ {
			dependency := member.Dependencies[j]
			dependencyField := fmt.Sprintf("%s.dependencies[%d]", field, j)
			diagnostics = append(diagnostics, validateEcosystem(dependencyField+".ecosystem", dependency.Ecosystem)...)
			diagnostics = append(diagnostics, validateCoordinate(dependencyField+".coordinate", dependency.Coordinate)...)
			diagnostics = append(diagnostics, validateVersion(dependencyField+".version", dependency.Version)...)
			dependencyKey := coordinateKey{dependency.Ecosystem, dependency.Coordinate}
			if first, exists := seenDependencies[dependencyKey]; exists {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateDependency, dependencyField, "dependency (%q, %q) duplicates %s", dependency.Ecosystem, dependency.Coordinate, first))
			} else {
				seenDependencies[dependencyKey] = dependencyField
			}
			if len(diagnostics) >= MaxDiagnostics {
				return truncateDiagnostics(diagnostics)
			}
		}
	}

	// Closure is evaluated after the member index is complete, so dependency
	// order never changes whether an edge resolves.
	for i := 0; i < memberLimit; i++ {
		member := releaseSet.Members[i]
		dependencyLimit := min(len(member.Dependencies), MaxDependenciesPerMember)
		for j := 0; j < dependencyLimit; j++ {
			dependency := member.Dependencies[j]
			field := fmt.Sprintf("members[%d].dependencies[%d]", i, j)
			resolved, exists := members[coordinateKey{dependency.Ecosystem, dependency.Coordinate}]
			if !exists {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnclosedDependency, field, "dependency (%q, %q) does not resolve to a member in this release set", dependency.Ecosystem, dependency.Coordinate))
				continue
			}
			if resolved.version != dependency.Version {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDependencyVersionMismatch, field+".version", "dependency version %q does not equal member version %q", dependency.Version, resolved.version))
			}
			if len(diagnostics) >= MaxDiagnostics {
				return truncateDiagnostics(diagnostics)
			}
		}
	}
	return boundDiagnostics(diagnostics)
}

// ValidateReleaseSetRef validates the two reference spellings and their shared
// hash. It does not assert existence; refs are pure content addresses.
func ValidateReleaseSetRef(field string, ref ReleaseSetRef) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	diagnostics = append(diagnostics, ValidateReleaseSetID(joinField(field, "id"), ref.ID)...)
	if !digestPattern.MatchString(ref.Digest) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRef, joinField(field, "digest"), "release-set digest must be sha256:<64 lowercase hex>"))
	}
	if IsReleaseSetID(ref.ID) && digestPattern.MatchString(ref.Digest) && strings.TrimPrefix(ref.ID, "rs_") != strings.TrimPrefix(ref.Digest, "sha256:") {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRefMismatch, field, "release-set id and digest must carry the same SHA-256 hex"))
	}
	return diagnostics
}

// ValidateReleaseSetID validates the standalone immutable selector accepted by
// resolve. It checks spelling only; existence and namespace ownership belong to
// the provider and the resolve exchange.
func ValidateReleaseSetID(field, releaseID string) []diag.Diagnostic {
	if !IsReleaseSetID(releaseID) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidRef, field, "release-set id must be rs_<64 lowercase hex>")}
	}
	return nil
}

// IsReleaseSetID reports whether value has the exact immutable id spelling.
// It does not assert that a provider stores the referenced release set.
func IsReleaseSetID(value string) bool { return refIDPattern.MatchString(value) }

func validateNamespace(value string) []diag.Diagnostic {
	const field = "namespace"
	if len(value) > MaxNamespaceBytes {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeBoundsExceeded, field, "namespace contains %d bytes, exceeding the limit of %d", len(value), MaxNamespaceBytes)}
	}
	if len(value) == 0 || !namespacePattern.MatchString(value) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidNamespace, field, "namespace must be a lowercase canonical token of 1..%d bytes", MaxNamespaceBytes)}
	}
	return nil
}

func validateChannel(field, value string) []diag.Diagnostic {
	if !IsPortableChannel(value) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidChannel, field, "channel %q is not a portable channel name (want %s)", value, ChannelPattern)}
	}
	return nil
}

// validateEcosystem applies the open ecosystem grammar. The protocol admits any
// identifier that matches EcosystemPattern; what it means belongs to the
// extension that owns the profile.
func validateEcosystem(field string, ecosystem Ecosystem) []diag.Diagnostic {
	if len(ecosystem) > MaxTokenBytes {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeBoundsExceeded, field, "ecosystem token contains %d bytes, exceeding the limit of %d", len(ecosystem), MaxTokenBytes)}
	}
	if !ecosystemPattern.MatchString(string(ecosystem)) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidEcosystem, field, "ecosystem %q is not an ecosystem identifier (want %s)", ecosystem, EcosystemPattern)}
	}
	return nil
}

// validateVisibility checks one level of the chain against the three ordered
// levels. An empty level is silent and inherits, so callers that admit silence
// skip this check.
func validateVisibility(field string, level Visibility) []diag.Diagnostic {
	if len(level) > MaxTokenBytes {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeBoundsExceeded, field, "visibility token contains %d bytes, exceeding the limit of %d", len(level), MaxTokenBytes)}
	}
	if !level.Valid() {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidVisibility, field, "visibility %q is not internal, private, or public", level)}
	}
	return nil
}

// validateProvenance applies the member provenance rules: every member names
// the source revision it was published from and the selection fingerprint the
// engine computed, per-platform digests are well formed when present, and the
// optional source project, artifact kind and source tree are well formed.
//
// Project, kind and source tree are OPTIONAL and validated only when present: a
// set accepted before they existed stays valid, and an empty value is absence
// rather than a defect. A present value is closed, so a consumer that selects
// members by role never has to guess what an unknown token meant, and one that
// compares trees never compares an abbreviated or differently cased spelling.
func validateProvenance(field string, member ReleaseSetMember) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if member.Project != "" && !memberProjectPattern.MatchString(member.Project) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProject, field+".project", "project must be a canonical project id of 1..%d bytes (want %s)", MaxProjectBytes, MemberProjectPattern))
	}
	if member.Kind != "" && !member.Kind.Valid() {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidKind, field+".kind", "kind %q is not one of %s", member.Kind, kindVocabulary()))
	}
	if !sourceRevisionPattern.MatchString(member.SourceRevision) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSourceRevision, field+".sourceRevision", "sourceRevision must be the full lowercase hex commit the member was published from"))
	}
	if member.SourceTree != "" && !sourceTreePattern.MatchString(member.SourceTree) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSourceTree, field+".sourceTree", "sourceTree must be the full lowercase hex git tree the member was built from"))
	}
	if !digestPattern.MatchString(member.SelectionFingerprint) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSelectionFingerprint, field+".selectionFingerprint", "selectionFingerprint must be sha256:<64 lowercase hex>: the member's execution cache key minus its version"))
	}
	if len(member.Platforms) > MaxPlatformsPerMember {
		return append(diagnostics, diag.Errorf(ErrorCodeBoundsExceeded, field+".platforms", "%d platforms exceed the per-member limit of %d", len(member.Platforms), MaxPlatformsPerMember))
	}
	platforms := make([]string, 0, len(member.Platforms))
	for platform := range member.Platforms {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		if !platformPattern.MatchString(platform) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidPlatform, field+".platforms", "platform %q must be a lowercase os/arch pair", platform))
		}
		if !digestPattern.MatchString(member.Platforms[platform]) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidArtifactDigest, field+".platforms."+platform, "platform digest must be sha256:<64 lowercase hex>"))
		}
	}
	return diagnostics
}

// validateCoordinate bounds one opaque coordinate. Its grammar belongs to the
// ecosystem profile, so only emptiness, size, and control characters are
// protocol errors.
func validateCoordinate(field, coordinate string) []diag.Diagnostic {
	if len(coordinate) > MaxCoordinateBytes {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeBoundsExceeded, field, "coordinate contains %d bytes, exceeding the limit of %d", len(coordinate), MaxCoordinateBytes)}
	}
	if len(coordinate) == 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidCoordinate, field, "coordinate must contain 1..%d bytes", MaxCoordinateBytes)}
	}
	if hasControlCharacters(coordinate) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidCoordinate, field, "coordinate must not contain control characters")}
	}
	return nil
}

// validateVersion bounds one opaque version. Its grammar belongs to the
// ecosystem profile; the protocol only refuses emptiness, oversize, and control
// characters.
func validateVersion(field, version string) []diag.Diagnostic {
	if len(version) > MaxVersionBytes {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeBoundsExceeded, field, "version contains %d bytes, exceeding the limit of %d", len(version), MaxVersionBytes)}
	}
	if len(version) == 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidVersion, field, "version must contain 1..%d bytes", MaxVersionBytes)}
	}
	if hasControlCharacters(version) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidVersion, field, "version must not contain control characters")}
	}
	return nil
}

// kindVocabulary renders the closed role vocabulary for a diagnostic message,
// in the order MemberKinds declares.
func kindVocabulary() string {
	tokens := make([]string, 0, len(MemberKinds))
	for _, kind := range MemberKinds {
		tokens = append(tokens, string(kind))
	}
	return strings.Join(tokens, ", ")
}

func hasControlCharacters(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func refsEqual(a, b ReleaseSetRef) bool { return a.ID == b.ID && a.Digest == b.Digest }

func joinField(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func boundDiagnostics(diagnostics []diag.Diagnostic) []diag.Diagnostic {
	if len(diagnostics) <= MaxDiagnostics {
		return diagnostics
	}
	bounded := append([]diag.Diagnostic{}, diagnostics[:MaxDiagnostics-1]...)
	return append(bounded, diag.Errorf(ErrorCodeDiagnosticsTruncated, "", "diagnostics exceeded the limit of %d and were truncated", MaxDiagnostics))
}

func truncateDiagnostics(diagnostics []diag.Diagnostic) []diag.Diagnostic {
	bounded := append([]diag.Diagnostic{}, diagnostics[:MaxDiagnostics-1]...)
	return append(bounded, diag.Errorf(ErrorCodeDiagnosticsTruncated, "", "diagnostics exceeded the limit of %d and were truncated", MaxDiagnostics))
}

type shapeKind uint8

const (
	shapeScalar shapeKind = iota
	shapeObject
	shapeArray
	// shapeMap is an object whose keys are open vocabulary: the platforms,
	// heads, current, channels, and observed maps.
	shapeMap
)

type shapeField struct {
	shape    *jsonShape
	required bool
	nullable bool
}

type jsonShape struct {
	kind shapeKind
	// fields describes a shapeObject.
	fields map[string]shapeField
	// elem describes the element of a shapeArray or the value of a shapeMap.
	elem *jsonShape
	// elemNullable admits an explicit JSON null as a shapeMap value.
	elemNullable bool
}

var scalarShape = &jsonShape{kind: shapeScalar}

func required(shape *jsonShape) shapeField { return shapeField{shape: shape, required: true} }
func requiredNullable(shape *jsonShape) shapeField {
	return shapeField{shape: shape, required: true, nullable: true}
}
func objectShape(fields map[string]shapeField) *jsonShape {
	return &jsonShape{kind: shapeObject, fields: fields}
}
func arrayShape(elem *jsonShape) *jsonShape { return &jsonShape{kind: shapeArray, elem: elem} }
func mapShapeOf(elem *jsonShape, elemNullable bool) *jsonShape {
	return &jsonShape{kind: shapeMap, elem: elem, elemNullable: elemNullable}
}

var mapShape = mapShapeOf(scalarShape, false)

var (
	dependencyShape = objectShape(map[string]shapeField{
		"ecosystem": required(scalarShape), "coordinate": required(scalarShape), "version": required(scalarShape),
	})
	memberShape = objectShape(map[string]shapeField{
		"ecosystem": required(scalarShape), "coordinate": required(scalarShape), "version": required(scalarShape),
		"artifactDigest": required(scalarShape), "dependencies": required(arrayShape(dependencyShape)),
		"sourceRevision": required(scalarShape), "selectionFingerprint": required(scalarShape),
		"platforms": {shape: mapShape},
		// Attribution and the source tree are optional: a set accepted before
		// these fields existed carries none, and must keep validating and
		// deriving its own ref.
		"project": {shape: scalarShape}, "kind": {shape: scalarShape}, "sourceTree": {shape: scalarShape},
	})
	releaseSetShape = objectShape(map[string]shapeField{
		"protocolVersion": required(scalarShape), "namespace": required(scalarShape), "members": required(arrayShape(memberShape)),
	})
	refShape         = objectShape(map[string]shapeField{"id": required(scalarShape), "digest": required(scalarShape)})
	channelHeadShape = objectShape(map[string]shapeField{
		"ref": required(refShape), "generation": required(scalarShape), "releaseSet": {shape: releaseSetShape},
	})
	headMapShape        = mapShapeOf(channelHeadShape, true)
	resolveRequestShape = objectShape(map[string]shapeField{
		"protocolVersion": required(scalarShape), "namespace": required(scalarShape),
		"channels": {shape: arrayShape(scalarShape)}, "releaseId": {shape: scalarShape},
	})
	resolveResponseShape = objectShape(map[string]shapeField{
		"protocolVersion": required(scalarShape),
		"heads":           {shape: headMapShape}, "release": {shape: channelHeadShape, nullable: true},
	})
	channelRequestShape = objectShape(map[string]shapeField{
		"name": required(scalarShape), "expected": requiredNullable(refShape),
		"visibility": required(scalarShape), "immutable": {shape: scalarShape},
	})
	versionVisibilityShape = objectShape(map[string]shapeField{
		"stable": {shape: scalarShape}, "prerelease": {shape: scalarShape},
	})
	memberVisibilityShape = objectShape(map[string]shapeField{
		"ecosystem": required(scalarShape), "coordinate": required(scalarShape), "visibility": required(scalarShape),
	})
	visibilityChainShape = objectShape(map[string]shapeField{
		"repo": required(scalarShape), "registries": {shape: mapShape},
		"versions": required(versionVisibilityShape), "set": requiredNullable(scalarShape),
		"members": {shape: arrayShape(memberVisibilityShape)},
	})
	releaseRequestShape = objectShape(map[string]shapeField{
		"protocolVersion": required(scalarShape), "namespace": required(scalarShape),
		"releaseSet": required(releaseSetShape), "channels": required(arrayShape(channelRequestShape)),
		"visibility": required(visibilityChainShape),
		"mirrors": {shape: mapShapeOf(objectShape(map[string]shapeField{
			"to": required(scalarShape),
		}), false)},
	})
	releaseResponseShape = objectShape(map[string]shapeField{
		"protocolVersion": required(scalarShape), "outcome": required(scalarShape),
		"current": required(headMapShape),
	})
	channelSourceShape = objectShape(map[string]shapeField{
		"channel": {shape: scalarShape}, "releaseId": {shape: scalarShape},
	})
	channelSetRequestShape = objectShape(map[string]shapeField{
		"protocolVersion": required(scalarShape), "namespace": required(scalarShape),
		"channel": required(scalarShape), "expected": requiredNullable(refShape),
		"from": required(channelSourceShape),
	})
	channelStatusRequestShape = objectShape(map[string]shapeField{
		"protocolVersion": required(scalarShape), "namespace": required(scalarShape), "channel": required(scalarShape),
	})
	channelStatusResponseShape = objectShape(map[string]shapeField{
		"protocolVersion": required(scalarShape), "desired": requiredNullable(channelHeadShape),
		"observed": {shape: mapShape},
	})
	publishOutcomeShape = objectShape(map[string]shapeField{
		"protocolVersion": required(scalarShape), "namespace": required(scalarShape),
		"ref": required(refShape), "channels": required(headMapShape),
	})
)

type structuralError struct {
	code    string
	field   string
	message string
}

func (err structuralError) Error() string { return err.message }

func strictDecode[T any](data []byte, shape *jsonShape) (*T, []diag.Diagnostic) {
	if len(data) > MaxJSONBytes {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeBoundsExceeded, "", "JSON document contains %d bytes, exceeding the limit of %d", len(data), MaxJSONBytes)}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectValue(decoder, "", shape, false); err != nil {
		return nil, []diag.Diagnostic{structuralDiagnostic(err)}
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "%v", err)}
		}
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "trailing JSON value %v is not allowed", token)}
	}

	var decoded T
	typed := json.NewDecoder(bytes.NewReader(data))
	typed.DisallowUnknownFields()
	if err := typed.Decode(&decoded); err != nil {
		field := ""
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) {
			field = typeError.Field
		}
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, field, "%v", err)}
	}
	return &decoded, nil
}

func inspectValue(decoder *json.Decoder, path string, shape *jsonShape, nullable bool) error {
	token, err := decoder.Token()
	if err != nil {
		return structuralError{ErrorCodeParseError, path, err.Error()}
	}
	if token == nil {
		if nullable {
			return nil
		}
		return structuralError{ErrorCodeNullField, path, "explicit null is not allowed"}
	}
	delimiter, isDelimiter := token.(json.Delim)
	switch shape.kind {
	case shapeObject:
		if !isDelimiter || delimiter != '{' {
			return structuralError{ErrorCodeParseError, path, "expected a JSON object"}
		}
		return inspectObject(decoder, path, shape)
	case shapeMap:
		if !isDelimiter || delimiter != '{' {
			return structuralError{ErrorCodeParseError, path, "expected a JSON object"}
		}
		return inspectMap(decoder, path, shape)
	case shapeArray:
		if !isDelimiter || delimiter != '[' {
			return structuralError{ErrorCodeParseError, path, "expected a JSON array"}
		}
		index := 0
		for decoder.More() {
			if err := inspectValue(decoder, fmt.Sprintf("%s[%d]", path, index), shape.elem, false); err != nil {
				return err
			}
			index++
		}
		_, err := decoder.Token()
		return err
	default:
		if isDelimiter {
			return structuralError{ErrorCodeParseError, path, "expected a scalar JSON value"}
		}
		return nil
	}
}

func inspectObject(decoder *json.Decoder, path string, shape *jsonShape) error {
	seen := make(map[string]bool, len(shape.fields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return structuralError{ErrorCodeParseError, path, err.Error()}
		}
		name, ok := token.(string)
		if !ok {
			return structuralError{ErrorCodeParseError, path, "object key is not a string"}
		}
		if len(name) > MaxTokenBytes {
			return structuralError{ErrorCodeBoundsExceeded, joinField(path, "<oversized-field>"), fmt.Sprintf("object field name contains %d bytes, exceeding the limit of %d", len(name), MaxTokenBytes)}
		}
		fieldPath := joinField(path, name)
		field, known := shape.fields[name]
		if !known {
			return structuralError{ErrorCodeUnknownField, fieldPath, fmt.Sprintf("unknown field %q", name)}
		}
		if seen[name] {
			return structuralError{ErrorCodeDuplicateField, fieldPath, fmt.Sprintf("field %q appears more than once", name)}
		}
		seen[name] = true
		if err := inspectValue(decoder, fieldPath, field.shape, field.nullable); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return structuralError{ErrorCodeParseError, path, err.Error()}
	}
	var missing []string
	for name, field := range shape.fields {
		if field.required && !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		fieldPath := joinField(path, missing[0])
		return structuralError{ErrorCodeMissingField, fieldPath, fmt.Sprintf("required field %q is missing", missing[0])}
	}
	return nil
}

// inspectMap walks an open-vocabulary object: every key is bounded and unique,
// and every value follows the map's element shape, optionally null.
func inspectMap(decoder *json.Decoder, path string, shape *jsonShape) error {
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return structuralError{ErrorCodeParseError, path, err.Error()}
		}
		name, ok := token.(string)
		if !ok {
			return structuralError{ErrorCodeParseError, path, "object key is not a string"}
		}
		if len(name) > MaxTokenBytes {
			return structuralError{ErrorCodeBoundsExceeded, joinField(path, "<oversized-field>"), fmt.Sprintf("object field name contains %d bytes, exceeding the limit of %d", len(name), MaxTokenBytes)}
		}
		fieldPath := joinField(path, name)
		if seen[name] {
			return structuralError{ErrorCodeDuplicateField, fieldPath, fmt.Sprintf("field %q appears more than once", name)}
		}
		seen[name] = true
		if err := inspectValue(decoder, fieldPath, shape.elem, shape.elemNullable); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return structuralError{ErrorCodeParseError, path, err.Error()}
	}
	return nil
}

func structuralDiagnostic(err error) diag.Diagnostic {
	var structural structuralError
	if errors.As(err, &structural) {
		return diag.Errorf(structural.code, structural.field, "%s", structural.message)
	}
	return diag.Errorf(ErrorCodeParseError, "", "%v", err)
}
