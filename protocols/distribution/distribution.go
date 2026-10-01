// Package distribution defines the distribution/release-set wire authority:
// immutable full snapshots of every artifact a publication produced, the
// channels that name them, and the visibility they are released under. It owns
// strict shapes, validation, normalization, canonical bytes, content-addressed
// references, and provider invocation vocabulary. It intentionally contains no
// provider discovery, transport, persistence, authorization, channel storage,
// or registry implementation.
//
// The protocol keeps only the generic part of an ecosystem: an identifier, an
// opaque bounded coordinate, an opaque bounded version, and optional per-platform
// digests. The extension that owns an ecosystem profile validates coordinate and
// version shapes at plan time; the registry that stores the artifact validates at
// write time.
//
// Provider implementations import an exact published module version and run
// its embedded conformance corpus; they do not copy this package's wire types,
// constants, canonicalization, or fixtures into their own repository. Provider-
// backed publication remains disabled until that exact-version conformance run
// reproduces the embedded canonical bytes and references.
package distribution

const (
	// ProtocolName is the immutable semantic name of this protocol.
	ProtocolName = "distribution/release-set/v2"
	// ProtocolVersion is the exact release-set wire version every document carries.
	ProtocolVersion = 2

	// ReleaseSetSchemaURL is the canonical release-set JSON Schema URI.
	ReleaseSetSchemaURL = "https://putnami.dev/schemas/distribution-release-set-v2.json"
	// ProviderSchemaURL is the canonical provider-operation JSON Schema URI.
	ProviderSchemaURL = "https://putnami.dev/schemas/distribution-release-set-provider-v2.json"
	// PublishOutcomeSchemaURL is the canonical successful-outcome JSON Schema URI.
	PublishOutcomeSchemaURL = "https://putnami.dev/schemas/distribution-release-set-publish-outcome-v2.json"

	// ProviderExecutable is the executable token used by the optional provider
	// seam. Selection and subprocess execution remain caller responsibilities.
	ProviderExecutable = "putnami"
	// ProviderCommandName is the flat reserved identity for release-set service.
	// Managed callers require exactly one matching provider and fail closed on
	// ambiguity; discovery and cloudless-mode selection remain outside this package.
	ProviderCommandName = "cloud-release-set"
	// CloudCommand is the first provider command token.
	CloudCommand = "cloud"
	// ReleaseSetCommand is the second provider command token.
	ReleaseSetCommand = "release-set"
	// ResolveCommand selects the multi-channel resolution operation.
	ResolveCommand = "resolve"
	// ReleaseCommand selects the one-step release: store the set and advance
	// every listed channel by compare-and-swap in one transaction.
	ReleaseCommand = "release"
	// ChannelSetCommand selects the metadata-only channel move used by
	// promotion and rollback: no artifact is uploaded.
	ChannelSetCommand = "channel-set"
	// ChannelStatusCommand selects the desired-versus-observed channel report.
	ChannelStatusCommand = "channel-status"
	// RequestFileFlag introduces the absolute request-file path.
	RequestFileFlag = "--request-file"

	// MaxJSONBytes bounds every strict request, response, outcome, and release-set
	// document before decoding.
	MaxJSONBytes = 4 * 1024 * 1024
	// MaxDiagnostics bounds validation output even for adversarial documents.
	MaxDiagnostics = 128
	// MaxMembers bounds the full snapshot cardinality.
	MaxMembers = 4096
	// MaxDependenciesPerMember bounds one member's internal dependency closure.
	MaxDependenciesPerMember = 1024
	// MaxNamespaceBytes bounds a namespace token in UTF-8 bytes.
	MaxNamespaceBytes = 128
	// MaxTokenBytes bounds a closed vocabulary token before it is diagnosed.
	MaxTokenBytes = 64
	// MaxCoordinateBytes bounds one opaque ecosystem coordinate.
	MaxCoordinateBytes = 512
	// MaxVersionBytes bounds one immutable registry version.
	MaxVersionBytes = 256
	// MaxPlatformsPerMember bounds one member's per-platform digests.
	MaxPlatformsPerMember = 32
	// MaxProjectBytes bounds one member's source project path.
	MaxProjectBytes = 200
	// MaxChannelsPerRelease bounds the channels one resolve or release names.
	MaxChannelsPerRelease = 16
	// MaxRegistryKinds bounds the registry kinds a visibility chain or a channel
	// status reports on.
	MaxRegistryKinds = 16
	// MaxMirrorTargetBytes bounds a non-secret external registry destination.
	MaxMirrorTargetBytes = 2048

	// EcosystemPattern is the RE2 grammar of an ecosystem identifier. The
	// protocol admits any identifier that matches it; the extension that owns
	// the profile decides what the identifier means.
	EcosystemPattern = "^[a-z][a-z0-9-]{0,31}$"
	// ChannelPattern is the portable channel alphabet: a name valid at once as an
	// npm dist-tag, a Go query, an OCI tag, and a put channel.
	ChannelPattern = "^[a-z0-9][a-z0-9._-]{0,63}$"
	// MirrorTargetPattern accepts an HTTPS registry URL or a native registry
	// host/path. Credentials, query strings, fragments and whitespace are absent.
	// The backend still validates the destination against its authorized targets.
	MirrorTargetPattern = "^(https://)?[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]{1,5})?(?:/[A-Za-z0-9._~!$&'()*+,;=:@%-]*)*$"
	// MemberProjectPattern is the grammar of a member's source project: the
	// publisher's canonical logical project id, without a leading slash. It is
	// the character class the ci protocol's project selector already admits,
	// bounded to MaxProjectBytes, because the value recorded here must be the
	// same identity a CI document selects a project by.
	//
	// The grammar admits no parenthesis, and that is the point: a workspace may
	// group projects under folders that are transparent to identity, and a
	// selector cannot name one. The publisher therefore records the LOGICAL id,
	// which omits those folders, not the physical directory path.
	MemberProjectPattern = "^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$"
)

// Ecosystem is an open artifact ecosystem identifier matching EcosystemPattern.
// The protocol never enumerates the admissible values: an extension declares an
// ecosystem profile and the identifier becomes usable without a protocol change.
type Ecosystem string

// Visibility is the level an artifact version is served at. The three levels are
// ordered: internal < private < public.
type Visibility string

const (
	// VisibilityInternal restricts an artifact to the workspace members and CI.
	VisibilityInternal Visibility = "internal"
	// VisibilityPrivate admits an authenticated user whose workspace holds a grant.
	VisibilityPrivate Visibility = "private"
	// VisibilityPublic admits anonymous reads and external registry distribution.
	VisibilityPublic Visibility = "public"
)

// Valid reports whether the level is one of the three ordered levels.
func (v Visibility) Valid() bool {
	switch v {
	case VisibilityInternal, VisibilityPrivate, VisibilityPublic:
		return true
	}
	return false
}

// Rank orders the levels from the narrowest to the widest: internal 0, private
// 1, public 2. An invalid level ranks below internal so it never widens a
// comparison by accident.
func (v Visibility) Rank() int {
	switch v {
	case VisibilityInternal:
		return 0
	case VisibilityPrivate:
		return 1
	case VisibilityPublic:
		return 2
	}
	return -1
}

// MaxVisibility returns the wider of two levels. It is the ratchet operator: a
// level already resolved for an artifact version is never narrowed.
func MaxVisibility(a, b Visibility) Visibility {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// ReleaseSet is one immutable, full snapshot. Derived identity is not carried
// here: it is computed from the canonical bytes of these exact fields.
type ReleaseSet struct {
	// ProtocolVersion is the exact release-set wire version and equals 2.
	ProtocolVersion int `json:"protocolVersion"`
	// Namespace is the canonical owner of every member in this full snapshot.
	Namespace string `json:"namespace"`
	// Members is the complete closed membership of the snapshot.
	Members []ReleaseSetMember `json:"members"`
}

// ReleaseSetMember binds one ecosystem coordinate to an immutable artifact and
// its exact internal dependency closure. The member key is
// (ecosystem, coordinate): one project yields any number of members, in one or
// several ecosystems, and the project is provenance rather than identity.
type ReleaseSetMember struct {
	// Ecosystem identifies the member registry and matches EcosystemPattern.
	Ecosystem Ecosystem `json:"ecosystem"`
	// Coordinate is the opaque, bounded, control-character-free coordinate the
	// owning ecosystem profile defines.
	Coordinate string `json:"coordinate"`
	// Version is the opaque, bounded, control-character-free immutable registry
	// version of this member.
	Version string `json:"version"`
	// ArtifactDigest is the verified SHA-256 digest of the published artifact.
	ArtifactDigest string `json:"artifactDigest"`
	// Dependencies lists exact internal edges that close within this release set.
	Dependencies []ReleaseSetDependency `json:"dependencies"`
	// SourceRevision is the full lowercase hex commit the member was published
	// from. A later publication compares its tree against this revision and the
	// selection fingerprint below, never against the previous commit.
	SourceRevision string `json:"sourceRevision"`
	// SelectionFingerprint is sha256:<64 lowercase hex>: the execution cache key
	// of the member's package task minus the embedded version, computed by the
	// engine. A member whose fingerprint equals the working tree's is inherited;
	// any other is republished, whatever the git history in between.
	SelectionFingerprint string `json:"selectionFingerprint"`
	// Platforms maps os/arch to the SHA-256 digest of that platform's artifact,
	// so a resolver can verify one download without the whole index. It is
	// optional for every ecosystem.
	Platforms map[string]string `json:"platforms,omitempty"`
	// Project is the canonical logical id of the project that published this
	// member, without a leading slash — the identity a selector names, which is
	// not always the physical directory path: a workspace may nest a project
	// under folders that are transparent to identity, and they are omitted here.
	//
	// It is PROVENANCE, never member identity: the member key stays
	// (ecosystem, coordinate), and one project yields any number of members. It
	// is empty on a set accepted before the field existed, and on a publisher
	// that records no project.
	Project string `json:"project,omitempty"`
	// Kind classifies the artifact for a consumer that selects members by role
	// rather than by coordinate — "which member is this workload's image, which
	// is its config, which are its migrations". It is empty when the publisher
	// cannot classify the artifact, and on a set accepted before the field
	// existed.
	Kind MemberKind `json:"kind,omitempty"`
	// SourceTree is the full lowercase hex git tree of the checkout the member
	// was built from. Two commits with the same tree hold the same content, so
	// a consumer can recognize a publication whose commit a squash-merge or a
	// rebase rewrote without changing a byte.
	//
	// It is PROVENANCE, never member identity, like project and kind. It is
	// empty on a set accepted before the field existed, and on a publisher that
	// could not read a tree that describes what it built.
	SourceTree string `json:"sourceTree,omitempty"`
}

// MemberKind is the closed vocabulary of artifact roles a release-set member
// can declare. It answers "what IS this artifact", not "how was it built": two
// publishers with different toolchains classify the same role identically, and
// a consumer selects on it without knowing either toolchain.
type MemberKind string

const (
	// KindImage is a container image a workload runs.
	KindImage MemberKind = "image"
	// KindConfig is a configuration document a workload is bound to.
	KindConfig MemberKind = "config"
	// KindMigration is a schema migration applied before a workload runs.
	KindMigration MemberKind = "migration"
	// KindDoc is published content: documentation or site content.
	KindDoc MemberKind = "doc"
	// KindLibrary is a package consumed by other code, not run on its own.
	KindLibrary MemberKind = "library"
	// KindArchive is a downloadable archive, such as a released binary set.
	KindArchive MemberKind = "archive"
)

// MemberKinds is the vocabulary in its declared order. The schema, the
// validator, and any consumer that enumerates roles read this one list.
var MemberKinds = []MemberKind{KindImage, KindConfig, KindMigration, KindDoc, KindLibrary, KindArchive}

// Valid reports whether the kind is one of the closed roles. An empty kind is
// not valid: it is ABSENT, which validation admits separately.
func (k MemberKind) Valid() bool {
	for _, kind := range MemberKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ReleaseSetDependency is one exact internal dependency edge. Its coordinate
// and version must resolve to a member in the same ReleaseSet.
type ReleaseSetDependency struct {
	// Ecosystem identifies the target member registry.
	Ecosystem Ecosystem `json:"ecosystem"`
	// Coordinate is the exact target member coordinate within this release set.
	Coordinate string `json:"coordinate"`
	// Version must equal the version of the target member in this release set.
	Version string `json:"version"`
}

// ReleaseSetRef is the content address derived from canonical release-set
// bytes. ID and Digest always carry the same lowercase SHA-256 hex.
type ReleaseSetRef struct {
	// ID is rs_ followed by the lowercase SHA-256 hex of canonical set bytes.
	ID string `json:"id"`
	// Digest is sha256: followed by the same lowercase hex carried by ID.
	Digest string `json:"digest"`
}

// ChannelHead is what a channel points to, with the monotone generation the
// provider stamped when it accepted the move. A registry applies a projection
// only when the generation increases, so a delayed event never restores an
// older projection and a retry is idempotent.
type ChannelHead struct {
	// Ref is the content address recomputed from ReleaseSet canonical bytes.
	Ref ReleaseSetRef `json:"ref"`
	// Generation is the monotone counter of accepted moves on this channel. It
	// starts at 1 for the first accepted head.
	Generation uint64 `json:"generation"`
	// ReleaseSet is the full immutable snapshot the channel points to. It is
	// present on a resolve answer and absent from release and channel-set
	// answers, which report pointers only.
	ReleaseSet *ReleaseSet `json:"releaseSet,omitempty"`
}

// ResolveRequest resolves either a list of mutable channels or exactly one
// immutable release-set id. ReleaseID deliberately carries no caller-supplied
// digest; the answer always returns the recomputed full ref.
type ResolveRequest struct {
	// ProtocolVersion is the exact provider request wire version and equals 2.
	ProtocolVersion int `json:"protocolVersion"`
	// Namespace scopes channel lookup or immutable-id ownership.
	Namespace string `json:"namespace"`
	// Channels selects 1..MaxChannelsPerRelease portable channel names and is
	// exclusive with ReleaseID.
	Channels []string `json:"channels,omitempty"`
	// ReleaseID selects one immutable rs_ id and is exclusive with Channels.
	ReleaseID string `json:"releaseId,omitempty"`

	// Presence is retained only for strictly decoded JSON so an explicitly
	// empty selector cannot masquerade as an omitted one when the other selector
	// is present. Programmatic callers remain source-compatible and are treated
	// as present when their value is non-empty.
	channelsPresent  bool
	releaseIDPresent bool
}

// ResolveResponse answers a resolve. A channel selector answers with Heads, one
// entry per requested channel; a nil entry is a complete answer meaning the
// channel has no head, distinct from a provider failure. A release-id selector
// answers with Release.
type ResolveResponse struct {
	// ProtocolVersion is the exact provider response wire version and equals 2.
	ProtocolVersion int `json:"protocolVersion"`
	// Heads carries one entry per requested channel; null means empty channel.
	Heads map[string]*ChannelHead `json:"heads,omitempty"`
	// Release answers a ReleaseID lookup. An immutable set is named by no
	// channel, so its Generation is 0.
	Release *ChannelHead `json:"release,omitempty"`
}

// ChannelRequest is one channel a release advances, with the head the caller
// compared against and the visibility that channel confers.
type ChannelRequest struct {
	// Name is the portable channel name this release advances.
	Name string `json:"name"`
	// Expected is the required current head of this channel; null asserts that
	// the channel has no head.
	Expected *ReleaseSetRef `json:"expected"`
	// Visibility is the level this channel confers, one step of the chain.
	Visibility Visibility `json:"visibility"`
	// Immutable creates a channel that accepts expected null once and refuses
	// every later move. A tagged publish uses it for the tag's channel.
	Immutable bool `json:"immutable,omitempty"`
}

// VisibilityChain is the permission inheritance the repository declares, from
// the widest level to the finest: repo, registry, channel, version, set,
// member. The finest level that states a value wins; a silent level inherits.
// The provider resolves it per member at release; the CLI computes nothing.
type VisibilityChain struct {
	// Repo is the level the repository declares for every artifact it produces.
	Repo Visibility `json:"repo"`
	// Registries overrides the repository level per registry kind, keyed by an
	// ecosystem identifier.
	Registries map[string]Visibility `json:"registries,omitempty"`
	// Versions overrides the channel level by the shape of the version.
	Versions VersionVisibility `json:"versions"`
	// Set is the per-publication override, between version and member. It is
	// null when this publication states no override.
	Set *Visibility `json:"set"`
	// Members is the finest level: it names exact members of the released set.
	Members []MemberVisibility `json:"members,omitempty"`
}

// VersionVisibility is the level a version takes from its shape. A silent
// entry inherits the level above it in the chain.
type VersionVisibility struct {
	// Stable is the level of a version without a prerelease suffix.
	Stable Visibility `json:"stable,omitempty"`
	// Prerelease is the level of a version that carries a suffix.
	Prerelease Visibility `json:"prerelease,omitempty"`
}

// MemberVisibility is the finest level of the chain: one exact member of the
// released set has the last word on its own visibility.
type MemberVisibility struct {
	// Ecosystem is the ecosystem half of the member key.
	Ecosystem Ecosystem `json:"ecosystem"`
	// Coordinate is the coordinate half of the member key.
	Coordinate string `json:"coordinate"`
	// Visibility is the level this exact member is released at.
	Visibility Visibility `json:"visibility"`
}

// ReleaseRequest is the one-step release: store the immutable set and advance
// every listed channel by compare-and-swap from its own expected head, in one
// provider transaction. A conflict on any channel writes nothing and names
// every head.
type ReleaseRequest struct {
	// ProtocolVersion is the exact provider request wire version and equals 2.
	ProtocolVersion int `json:"protocolVersion"`
	// Namespace scopes every listed channel and owns every member.
	Namespace string `json:"namespace"`
	// ReleaseSet is the full immutable snapshot to store and name.
	ReleaseSet ReleaseSet `json:"releaseSet"`
	// Channels lists every channel this release advances to the stored set.
	Channels []ChannelRequest `json:"channels"`
	// Visibility is the chain the repository declares, resolved per member by
	// the provider at release.
	Visibility VisibilityChain `json:"visibility"`
	// Mirrors carries non-secret, ecosystem-keyed external copy intent in this
	// same acceptance transaction. Only stored-public members are eligible.
	// Omission requests no new mirror work; it never cancels accepted work.
	Mirrors map[string]MirrorTarget `json:"mirrors,omitempty"`
}

// MirrorTarget names an external registry destination. It is not an
// authorization grant or a credential reference: the backend resolves its own
// authorized target configuration and credentials, and owns copy/retry receipts.
type MirrorTarget struct {
	To string `json:"to"`
}

// ReleaseOutcome is the exclusive release result vocabulary.
type ReleaseOutcome string

const (
	// ReleaseOutcomeReleased means the set was stored and every listed channel
	// advanced.
	ReleaseOutcomeReleased ReleaseOutcome = "released"
	// ReleaseOutcomeAlreadyCurrent means every listed channel already pointed to
	// this exact set; nothing moved.
	ReleaseOutcomeAlreadyCurrent ReleaseOutcome = "already-current"
	// ReleaseOutcomeConflict means at least one channel's expected head did not
	// match; nothing was stored or advanced.
	ReleaseOutcomeConflict ReleaseOutcome = "conflict"
)

// ReleaseResponse reports the release result. Current carries one entry per
// requested channel: the head observed after the operation, null only for a
// conflict against a channel that still has no head.
type ReleaseResponse struct {
	// ProtocolVersion is the exact provider response wire version and equals 2.
	ProtocolVersion int `json:"protocolVersion"`
	// Outcome is exactly released, already-current, or conflict.
	Outcome ReleaseOutcome `json:"outcome"`
	// Current has one entry per requested channel name.
	Current map[string]*ChannelHead `json:"current"`
}

// ChannelSource names the set a metadata-only channel move takes its head from:
// exactly one of an existing channel or an immutable release-set id.
type ChannelSource struct {
	// Channel names an existing channel whose current head is re-released.
	Channel string `json:"channel,omitempty"`
	// ReleaseID names one immutable rs_ id to re-release.
	ReleaseID string `json:"releaseId,omitempty"`

	// Presence is retained only for strictly decoded JSON so an explicitly
	// empty selector cannot masquerade as an omitted one.
	channelPresent   bool
	releaseIDPresent bool
}

// ChannelSetRequest is the metadata-only move: the provider re-releases an
// existing set on the target channel with the same guarantees as a release, and
// uploads no artifact. Promotion and rollback are this one operation.
type ChannelSetRequest struct {
	// ProtocolVersion is the exact provider request wire version and equals 2.
	ProtocolVersion int `json:"protocolVersion"`
	// Namespace scopes both the target channel and the source.
	Namespace string `json:"namespace"`
	// Channel is the portable name of the channel this request moves.
	Channel string `json:"channel"`
	// Expected is the required current head of the target channel; null asserts
	// that it has no head.
	Expected *ReleaseSetRef `json:"expected"`
	// From names the set to point the target channel at.
	From ChannelSource `json:"from"`
}

// ChannelStatusRequest asks for one channel's desired head and the generation
// each registry has applied.
type ChannelStatusRequest struct {
	// ProtocolVersion is the exact provider request wire version and equals 2.
	ProtocolVersion int `json:"protocolVersion"`
	// Namespace scopes the channel being reported on.
	Namespace string `json:"namespace"`
	// Channel is the portable name of the channel being reported on.
	Channel string `json:"channel"`
}

// ChannelStatusResponse exposes desired versus observed: the head the provider
// accepted, and per registry the generation it has applied. A publisher that
// finds no subscriber reports it instead of counting a silent zero.
type ChannelStatusResponse struct {
	// ProtocolVersion is the exact provider response wire version and equals 2.
	ProtocolVersion int `json:"protocolVersion"`
	// Desired is the accepted head, or null when the channel has none.
	Desired *ChannelHead `json:"desired"`
	// Observed maps a registry kind to the channel generation it has applied.
	Observed map[string]uint64 `json:"observed,omitempty"`
}

// ReleaseSetPublishOutcome is the sole successful publish payload carried
// under data.releaseSet by the runtime result event.
type ReleaseSetPublishOutcome struct {
	// ProtocolVersion is the exact successful-outcome wire version and equals 2.
	ProtocolVersion int `json:"protocolVersion"`
	// Namespace identifies the namespace whose channels were advanced.
	Namespace string `json:"namespace"`
	// Ref is the exact immutable release set stored and named by publication.
	Ref ReleaseSetRef `json:"ref"`
	// Channels reports, per advanced channel name, the head the provider accepted.
	Channels map[string]*ChannelHead `json:"channels"`
}
