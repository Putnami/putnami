// Package agentcontext defines the agent-context protocol: the one
// deterministic, redaction-safe artifact that lets Putnami MCP/intelligence
// tools orient in a project without exploring the whole repository. One
// document is emitted per project and aggregates — strictly BY REFERENCE —
// everything an agent needs to find composition roots, capabilities, public
// contracts, representative source, tests, and operational surface.
//
// The document aggregates these sections, in canonical field order:
//
//   - identity/graph — project id, name, path, type, tags, languages, and the
//     ids of its dependencies and dependents;
//   - compositionRoots — the application main, describe entrypoint, and other
//     roots, each with a path and provenance tag;
//   - capabilities / contracts / infra / migrations — references (path + sha256
//     digest, optional kind) into the already-committed artifacts, never their
//     embedded content;
//   - representativeSources — ordered path + line-range entries with a selection
//     reason and a token estimate; ranges only, never file content;
//   - tests — an OPTIONAL section referencing conformance packs by id, the test
//     policy, and fixture digests, or a machine-readable absence reason;
//   - docs — adjacent documentation paths with a checked/unchecked relationship
//     status;
//   - config — a config-schema reference and refs-only operational hints;
//   - provenance — the workspace revision, generator identity, and aggregation
//     method.
//
// Redaction is acceptance-critical. The wire types have NO slot for file
// content: every reference is a path plus a digest or a line range. Entries that
// point at sensitive material carry a Sensitive flag, and the fail-closed
// ValidatePublishSafety gate (see strict.go) rejects a document that references
// a caller-flagged sensitive path without the flag, embeds a suspiciously large
// string, or carries a non-workspace-relative path.
//
// This package is the wire contract only: hand-written Go types are the source
// of truth (no codegen), strict parsing rejects unknown fields, and the
// fixtures/ corpus is the cross-language contract. The aggregator that walks a
// project, its selection rules, and the MCP tool that serves the result live in
// the CLI — the package is the contract, not the pipeline.
//
// Field names are camelCase and field order is deliberate; the canonical
// serialization is json.MarshalIndent(v, "", "  ")+"\n".
package agentcontext

// ProtocolVersion is the current agent-context protocol version: the version
// every producer stamps and the JSON schema pins. Bumped whenever a
// backwards-incompatible change to the document shape, the overrides shape, an
// enum, or the diagnostic taxonomy lands.
const ProtocolVersion = 1

// Canonical filenames and paths used by the protocol.
const (
	// DocumentFilename is the per-project agent-context document file name.
	DocumentFilename = "agent-context.json"

	// DocumentEmitDir is the build-output directory a producer writes the
	// document to. It is the .gen/ ROOT (not .gen/schema/), so the codegen
	// committer never promotes it into the tracked tree: the document embeds the
	// workspace revision and content digests and would churn every commit, so it
	// stays ephemeral and gitignored. Determinism is pinned at a fixed tree by
	// tests, not by committing the artifact.
	DocumentEmitDir = ".gen"

	// OverridesPath is the workspace-relative path of the optional, author-owned
	// overrides file in a consuming project's tracked tree. It is strict-parsed
	// and reviewed like any other source; it narrows deterministic selection
	// without a hand-written context pack.
	OverridesPath = "schema/agent-context.overrides.json"
)

// RootKind classifies a composition root. The enum is closed: adding a kind
// requires a ProtocolVersion bump so consumers can decide how to react.
type RootKind string

// RootKind values.
const (
	// RootKindApplicationMain marks the executable entrypoint of an application
	// (its main package / bootstrap).
	RootKindApplicationMain RootKind = "applicationMain"
	// RootKindDescribeEntrypoint marks the describe/generate entrypoint a project
	// exposes for build-time introspection.
	RootKindDescribeEntrypoint RootKind = "describeEntrypoint"
)

// ValidRootKinds enumerates the canonical composition-root kinds.
var ValidRootKinds = map[RootKind]bool{
	RootKindApplicationMain:    true,
	RootKindDescribeEntrypoint: true,
}

// SourceReason is the provenance tag explaining why a representative source
// range was selected. The enum is closed: adding a reason requires a
// ProtocolVersion bump.
type SourceReason string

// SourceReason values.
const (
	// SourceReasonCapabilityEvidence marks a range selected because it is the
	// evidence path of a capability contribution.
	SourceReasonCapabilityEvidence SourceReason = "capability-evidence"
	// SourceReasonMain marks a range selected because it is (part of) a
	// composition root / main entrypoint.
	SourceReasonMain SourceReason = "main"
	// SourceReasonOverride marks a range added by the author overrides file.
	SourceReasonOverride SourceReason = "override"
)

// ValidSourceReasons enumerates the canonical representative-source reasons.
var ValidSourceReasons = map[SourceReason]bool{
	SourceReasonCapabilityEvidence: true,
	SourceReasonMain:               true,
	SourceReasonOverride:           true,
}

// DocRelationship classifies whether a documentation file's relationship to the
// code it describes has been checked. The enum is closed: adding a status
// requires a ProtocolVersion bump.
type DocRelationship string

// DocRelationship values.
const (
	// DocRelationshipChecked marks a doc whose relationship to the code was
	// verified (e.g. it is co-located with or cross-linked from the code).
	DocRelationshipChecked DocRelationship = "checked"
	// DocRelationshipUnchecked marks a doc surfaced by proximity whose
	// relationship to the code has not been verified.
	DocRelationshipUnchecked DocRelationship = "unchecked"
)

// ValidDocRelationships enumerates the canonical doc-relationship statuses.
var ValidDocRelationships = map[DocRelationship]bool{
	DocRelationshipChecked:   true,
	DocRelationshipUnchecked: true,
}

// TestPolicy names how the test tooling behaves for a project. The three values
// mirror go.putnami.dev/protocol/database TestMode (auto/require/skip) so the
// two contracts agree on the same strings; they are frozen once merged and
// adding a value requires a ProtocolVersion bump.
type TestPolicy string

// TestPolicy values, mirroring database TestMode.
const (
	// TestPolicyAuto allows local/dev convenience provisioning when no usable
	// binding/provider exists.
	TestPolicyAuto TestPolicy = "auto"
	// TestPolicyRequire fails loudly when no usable binding/provider exists.
	TestPolicyRequire TestPolicy = "require"
	// TestPolicySkip skips the tests when no usable binding/provider exists.
	TestPolicySkip TestPolicy = "skip"
)

// ValidTestPolicies enumerates the canonical test policies.
var ValidTestPolicies = map[TestPolicy]bool{
	TestPolicyAuto:    true,
	TestPolicyRequire: true,
	TestPolicySkip:    true,
}

// AbsenceReason is the machine-readable reason a tests section carries no packs.
// The enum is closed: adding a reason requires a ProtocolVersion bump.
type AbsenceReason string

// AbsenceReason values.
const (
	// AbsenceReasonNotCollected marks a section whose data was not collected on
	// this run (e.g. the tests aggregator did not run).
	AbsenceReasonNotCollected AbsenceReason = "not-collected"
	// AbsenceReasonNoPacks marks a project that was inspected but declares no
	// conformance packs.
	AbsenceReasonNoPacks AbsenceReason = "no-packs"
	// AbsenceReasonUnsupportedProjectType marks a project whose type cannot carry
	// the section (e.g. a non-framework project).
	AbsenceReasonUnsupportedProjectType AbsenceReason = "unsupported-project-type"
)

// ValidAbsenceReasons enumerates the canonical absence reasons.
var ValidAbsenceReasons = map[AbsenceReason]bool{
	AbsenceReasonNotCollected:           true,
	AbsenceReasonNoPacks:                true,
	AbsenceReasonUnsupportedProjectType: true,
}

// TokenMethod names how a token estimate was computed. The enum is closed:
// adding a method requires a ProtocolVersion bump.
type TokenMethod string

// TokenMethod values.
const (
	// TokenMethodBytesDiv4 estimates tokens as ceil(bytes/4) over the referenced
	// byte range — the dependency-free heuristic v1 uses.
	TokenMethodBytesDiv4 TokenMethod = "bytes/4"
)

// ValidTokenMethods enumerates the canonical token-estimation methods.
var ValidTokenMethods = map[TokenMethod]bool{
	TokenMethodBytesDiv4: true,
}

// AggregationMethod names how a document aggregates project facts. The enum is
// closed: adding a method requires a ProtocolVersion bump.
type AggregationMethod string

// AggregationMethod values.
const (
	// AggregationMethodByReference marks a document that references committed
	// artifacts by path + digest and source by range, never embedding content.
	AggregationMethodByReference AggregationMethod = "by-reference"
)

// ValidAggregationMethods enumerates the canonical aggregation methods.
var ValidAggregationMethods = map[AggregationMethod]bool{
	AggregationMethodByReference: true,
}

// Identity is the project identity and dependency graph the document describes.
// It carries only workspace-owned facts (ids, name, path, type, tags,
// languages, and neighbor ids); it never restates the artifacts those neighbors
// own.
type Identity struct {
	// ID is the workspace project id (e.g. "/protocols/agentcontext").
	ID string `json:"id"`
	// Name is the project's package/module name.
	Name string `json:"name"`
	// Path is the workspace-relative directory of the project.
	Path string `json:"path"`
	// Type is the workspace project type when set (e.g. "template"); empty for a
	// default project.
	Type string `json:"type,omitempty"`
	// Tags lists the project's workspace tags.
	Tags []string `json:"tags,omitempty"`
	// Languages lists the languages the project is written in (e.g. "go", "typescript").
	Languages []string `json:"languages,omitempty"`
	// Dependencies lists the ids of the projects this project depends on.
	Dependencies []string `json:"dependencies,omitempty"`
	// Dependents lists the ids of the projects that depend on this project.
	Dependents []string `json:"dependents,omitempty"`
}

// CompositionRoot names one entrypoint into the project's wiring. It records
// only the kind, a workspace-relative path, and a short provenance tag — never
// the file's content.
type CompositionRoot struct {
	// Kind classifies the root (see RootKind).
	Kind RootKind `json:"kind"`
	// Path is the workspace-relative path of the root file.
	Path string `json:"path"`
	// Provenance is a short tag naming how the root was identified.
	Provenance string `json:"provenance,omitempty"`
	// Sensitive marks a root that points at material a caller flagged sensitive;
	// the publish-safety gate requires this when the path is in the sensitive set.
	Sensitive bool `json:"sensitive,omitempty"`
}

// ArtifactRef references a committed artifact by workspace-relative path and
// content digest. It is the aggregation-by-reference primitive: capabilities,
// contracts, infra requirements, migration bundles, config schemas, and fixture
// digests are all pointed at, never embedded.
type ArtifactRef struct {
	// Path is the workspace-relative path of the referenced artifact.
	Path string `json:"path"`
	// Digest is the artifact content address in "sha256:<64hex>" form.
	Digest string `json:"digest"`
	// Kind is an optional within-section disambiguator (e.g. an infra "secret"
	// requirement kind).
	Kind string `json:"kind,omitempty"`
	// Sensitive marks a reference to material a caller flagged sensitive; the
	// publish-safety gate requires this when the path is in the sensitive set.
	Sensitive bool `json:"sensitive,omitempty"`
}

// TokenEstimate is the token-budget estimate for a referenced range, carrying
// both the count and the method that produced it so the number is auditable.
type TokenEstimate struct {
	// Estimated is the estimated token count for the referenced range; non-negative.
	Estimated int `json:"estimated"`
	// Method names how Estimated was computed (see TokenMethod).
	Method TokenMethod `json:"method"`
}

// SourceRange references a representative slice of a source file by
// workspace-relative path and line range. It never carries the file content:
// only the range, the selection reason, and a token estimate travel.
type SourceRange struct {
	// Path is the workspace-relative path of the source file.
	Path string `json:"path"`
	// StartLine is the 1-based first line of the range (inclusive).
	StartLine int `json:"startLine"`
	// EndLine is the 1-based last line of the range (inclusive); >= StartLine.
	EndLine int `json:"endLine"`
	// Why is the provenance tag explaining why the range was selected (see SourceReason).
	Why SourceReason `json:"why"`
	// Tokens is the token-budget estimate for the range.
	Tokens TokenEstimate `json:"tokens"`
	// Sensitive marks a range in a file a caller flagged sensitive; the
	// publish-safety gate requires this when the path is in the sensitive set.
	Sensitive bool `json:"sensitive,omitempty"`
}

// DocRef references an adjacent documentation file and records whether its
// relationship to the code has been checked.
type DocRef struct {
	// Path is the workspace-relative path of the documentation file.
	Path string `json:"path"`
	// Relationship records whether the doc-to-code relationship was verified
	// (see DocRelationship).
	Relationship DocRelationship `json:"relationship"`
	// Sensitive marks a doc a caller flagged sensitive; the publish-safety gate
	// requires this when the path is in the sensitive set.
	Sensitive bool `json:"sensitive,omitempty"`
}

// PackRef references a conformance pack by its stable id, mirroring the
// committed <project>/conformance/pack.json shape (id, capabilityKinds,
// languages). It aggregates the pack by reference; the pack's corpus stays
// owned by the pack.
type PackRef struct {
	// ID is the stable conformance-pack identifier.
	ID string `json:"id"`
	// CapabilityKinds lists the capability kinds the pack certifies.
	CapabilityKinds []string `json:"capabilityKinds,omitempty"`
	// Languages lists the language adapters that ship a runner for the pack.
	Languages []string `json:"languages,omitempty"`
}

// TestsSection is the OPTIONAL tests section. When it lists packs it names the
// test policy and referenced fixture digests; when it lists none it MUST carry a
// machine-readable AbsenceReason so a consumer never has to guess why tests are
// missing. A producer fills it from committed conformance-pack metadata alone,
// so a project with no applicable pack states that fact instead of omitting it.
type TestsSection struct {
	// Policy names how the test tooling behaves for the project (see TestPolicy).
	Policy TestPolicy `json:"policy"`
	// Packs lists the referenced conformance packs; empty requires AbsenceReason.
	Packs []PackRef `json:"packs,omitempty"`
	// FixtureDigests references test-fixture artifacts by path + digest.
	FixtureDigests []ArtifactRef `json:"fixtureDigests,omitempty"`
	// AbsenceReason states why Packs is empty; required when Packs is empty and
	// forbidden when it is not (see ValidateDocument).
	AbsenceReason AbsenceReason `json:"absenceReason,omitempty"`
}

// OperationalSurface carries refs-only hints about a project's runtime surface,
// so an agent knows an operational endpoint exists without the document
// restating it. It embeds no addresses or values.
type OperationalSurface struct {
	// PlatformEndpoints reports whether the project exposes the platform
	// operational endpoints (/healthz, /livez, /readyz, /version).
	PlatformEndpoints bool `json:"platformEndpoints,omitempty"`
}

// ConfigSection references the project's config schema and summarizes its
// operational surface. Both are references/hints only — no config values.
type ConfigSection struct {
	// SchemaRef references the committed config schema by path + digest.
	SchemaRef *ArtifactRef `json:"schemaRef,omitempty"`
	// Operational carries refs-only operational surface hints.
	Operational *OperationalSurface `json:"operational,omitempty"`
}

// Generator identifies the tool that produced the document, for provenance.
type Generator struct {
	// Name is the generator's identity (e.g. "putnami").
	Name string `json:"name"`
	// Version is the generator's version.
	Version string `json:"version"`
}

// Provenance records the freshness and origin of the document: the workspace
// revision it was computed at, the generator that produced it, and the
// aggregation method it used.
type Provenance struct {
	// WorkspaceRevision is the workspace revision (e.g. a git sha) the document
	// was computed at, so a consumer can check freshness.
	WorkspaceRevision string `json:"workspaceRevision"`
	// Generator identifies the tool and version that produced the document.
	Generator Generator `json:"generator"`
	// AggregationMethod names how the document aggregates facts (see AggregationMethod).
	AggregationMethod AggregationMethod `json:"aggregationMethod"`
}

// Document is the per-project agent-context artifact. A producer writes it to
// "<project>/.gen/agent-context.json" (ephemeral; see DocumentEmitDir).
//
// Field order is deliberate: it is the canonical serialization order every
// producer must reproduce byte-for-byte.
type Document struct {
	// Schema is the optional URI of the JSON schema describing this document.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the agent-context wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Identity is the project identity and dependency graph.
	Identity Identity `json:"identity"`
	// CompositionRoots lists the project's entrypoints.
	CompositionRoots []CompositionRoot `json:"compositionRoots,omitempty"`
	// Capabilities references the committed capability manifest(s) by path + digest.
	Capabilities []ArtifactRef `json:"capabilities,omitempty"`
	// Contracts references the committed contract artifact(s) by path + digest.
	Contracts []ArtifactRef `json:"contracts,omitempty"`
	// Infra references the committed infra requirement artifact(s) by path + digest.
	Infra []ArtifactRef `json:"infra,omitempty"`
	// Migrations references the committed migration bundle(s) by path + digest.
	Migrations []ArtifactRef `json:"migrations,omitempty"`
	// RepresentativeSources lists selected source ranges (never content).
	RepresentativeSources []SourceRange `json:"representativeSources,omitempty"`
	// Tests is the optional tests section (see TestsSection).
	Tests *TestsSection `json:"tests,omitempty"`
	// Docs lists adjacent documentation with a checked/unchecked relationship.
	Docs []DocRef `json:"docs,omitempty"`
	// Config references the config schema and operational surface hints.
	Config *ConfigSection `json:"config,omitempty"`
	// Provenance records the revision, generator, and aggregation method.
	Provenance Provenance `json:"provenance"`
}

// OverridesFile is the strict-parsed, author-owned overrides document at
// <project>/schema/agent-context.overrides.json. It is deliberately narrow: it
// only adds/removes representative-source and doc entries and force-flags paths
// as sensitive. It never overrides identity, capabilities, or provenance —
// those are framework-owned facts.
//
// Field order is deliberate: it is the canonical serialization order.
type OverridesFile struct {
	// Schema is the optional URI of the JSON schema describing this file.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the agent-context wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// AddSources lists representative-source ranges to add to the selection.
	AddSources []SourceRange `json:"addSources,omitempty"`
	// RemoveSources lists workspace-relative source paths to drop from the selection.
	RemoveSources []string `json:"removeSources,omitempty"`
	// AddDocs lists documentation references to add.
	AddDocs []DocRef `json:"addDocs,omitempty"`
	// RemoveDocs lists workspace-relative doc paths to drop.
	RemoveDocs []string `json:"removeDocs,omitempty"`
	// Sensitive lists workspace-relative paths to force-flag as sensitive so the
	// publish-safety gate treats them as reference-only.
	Sensitive []string `json:"sensitive,omitempty"`
}
