// Package contracts defines the canonical contract IR: the single
// deterministic document that describes a project's contract vocabulary so a
// downstream compiler can lower it into Go and TypeScript types, JSON Schema,
// OpenAPI inputs, discovery metadata, and docs.
//
// A contract manifest is authored (or emitted) once per project and models the
// vocabulary shared across the framework, the tooling, and the platform:
//
//   - enums (closed value sets),
//   - tagged (discriminated) unions,
//   - structs / DTOs with typed field lists,
//   - config fields with typed defaults,
//   - security scopes and capabilities,
//   - grant names,
//   - claims,
//   - principal kinds,
//   - discovery metadata.
//
// This package defines the IR and its validator ONLY. The emitters that lower
// the IR into concrete artifacts are a later slice; the CLI that drives them is
// later still. Here the IR is the contract, not the pipeline.
//
// This package defines the wire contract only: hand-written Go types are the
// source of truth (no codegen), strict parsing rejects unknown fields, and a
// shared fixtures/ corpus is the cross-language contract that both languages
// read by relative path.
//
// Byte-identical cross-language emission is a goal: the field order of every
// struct is deliberate, and TypeScript mirrors json.MarshalIndent's ordering,
// indentation, HTML/JavaScript-separator escaping, and trailing newline. Keep
// field names camelCase and the ordering intentional.
package contracts

// ProtocolVersion is the current contract-IR protocol version: the version
// every in-tree producer stamps and the JSON schema pins. Bumped whenever a
// backwards-incompatible change to the IR shape, a node kind, or an enum lands.
const ProtocolVersion = 1

// Canonical filenames and paths used by the protocol.
const (
	// ManifestFilename is the contract-IR file name.
	ManifestFilename = "contracts.json"

	// EmitDir is the build-output directory an emitter writes the IR to. It lives
	// under .gen/schema/ (not the .gen/ root) so the codegen committer promotes
	// the document into the tracked tree: files under .gen/schema/ are committed
	// and shipped in a packaged workload, while files at the .gen/ root stay
	// ephemeral and are gitignored. Emitters MUST write here.
	EmitDir = ".gen/schema"

	// CommittedPath is the workspace-relative path of the promoted IR in the
	// tracked tree. The committer strips the leading .gen/ from EmitDir, so
	// .gen/schema/contracts.json is promoted to schema/contracts.json.
	CommittedPath = "schema/" + ManifestFilename
)

// Manifest is the canonical contract IR for a single project. An emitter writes
// it to "<project>/.gen/schema/contracts.json", from which the codegen
// committer promotes it into the tracked tree at
// "<project>/schema/contracts.json" (see EmitDir and CommittedPath).
//
// Field order is deliberate: it is the canonical serialization order that both
// the Go and TypeScript emitters must reproduce byte-for-byte.
type Manifest struct {
	// Schema is the optional URI of the JSON schema describing this IR document.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the contract-IR wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Name is the contract identity — typically the Putnami project the IR
	// describes.
	Name string `json:"name"`
	// Enums declares the closed value sets referenced by fields and unions.
	Enums []Enum `json:"enums,omitempty"`
	// Unions declares the tagged (discriminated) unions.
	Unions []Union `json:"unions,omitempty"`
	// Structs declares the named struct / DTO shapes.
	Structs []Struct `json:"structs,omitempty"`
	// ConfigFields declares the configuration fields and their typed defaults.
	ConfigFields []ConfigField `json:"configFields,omitempty"`
	// Scopes declares the security scopes.
	Scopes []Scope `json:"scopes,omitempty"`
	// Capabilities declares the named authorization capabilities.
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Grants declares the named permission grants.
	Grants []Grant `json:"grants,omitempty"`
	// Claims declares the token / principal claims.
	Claims []Claim `json:"claims,omitempty"`
	// PrincipalKinds declares the recognized principal kinds.
	PrincipalKinds []PrincipalKind `json:"principalKinds,omitempty"`
	// Discovery carries optional discovery metadata for the contract as a whole.
	Discovery *DiscoveryMetadata `json:"discovery,omitempty"`
}

// Enum is a closed value set. A parser that does not recognize an enum value is
// expected to reject it rather than silently degrade — extending the set of an
// existing enum requires a ProtocolVersion bump.
type Enum struct {
	// Name is the enum's type name; it shares the type namespace with unions and
	// structs.
	Name string `json:"name"`
	// Description is human/automation-facing documentation for the enum.
	Description string `json:"description,omitempty"`
	// Values is the non-empty, ordered set of member values.
	Values []EnumValue `json:"values"`
}

// EnumValue is one member of an Enum. Name is the language-neutral constant
// identity; Value is the wire representation.
type EnumValue struct {
	// Name is the constant identity within the enum (e.g. "Framework").
	Name string `json:"name"`
	// Value is the on-the-wire string representation (e.g. "framework").
	Value string `json:"value"`
	// Description is human/automation-facing documentation for the member.
	Description string `json:"description,omitempty"`
}

// Union is a tagged (discriminated) union: a Discriminator field selects one of
// the Variants at runtime. Each variant either references a named Struct or
// carries an inline field list — the two are mutually exclusive, generalizing
// the REST client IR's Body/BodyType split into a real discriminated union.
type Union struct {
	// Name is the union's type name; it shares the type namespace with enums and
	// structs.
	Name string `json:"name"`
	// Description is human/automation-facing documentation for the union.
	Description string `json:"description,omitempty"`
	// Discriminator is the wire field whose value selects the active variant.
	Discriminator string `json:"discriminator"`
	// Variants is the non-empty, ordered set of variants keyed by Tag.
	Variants []UnionVariant `json:"variants"`
}

// UnionVariant is one arm of a Union. Tag is the discriminator value that
// selects it. Exactly one of Struct (a reference to a declared Struct) or
// Fields (an inline field list) must be set.
type UnionVariant struct {
	// Tag is the discriminator value that selects this variant.
	Tag string `json:"tag"`
	// Description is human/automation-facing documentation for the variant.
	Description string `json:"description,omitempty"`
	// Struct references a declared Struct by name. Mutually exclusive with Fields.
	Struct string `json:"struct,omitempty"`
	// Fields is the inline shape of the variant. Mutually exclusive with Struct.
	Fields []Field `json:"fields,omitempty"`
}

// Struct is a named struct / DTO shape: an ordered list of typed fields.
type Struct struct {
	// Name is the struct's type name; it shares the type namespace with enums and
	// unions.
	Name string `json:"name"`
	// Description is human/automation-facing documentation for the struct.
	Description string `json:"description,omitempty"`
	// Fields is the ordered field list. It may be empty for an opaque shape.
	Fields []Field `json:"fields,omitempty"`
}

// Field is one field of a Struct or an inline UnionVariant. Type is either a
// primitive (string, int, float, bool, duration) or the name of a declared
// enum, union, or struct.
type Field struct {
	// Name is the field's wire name (the JSON key).
	Name string `json:"name"`
	// Type is a primitive token or a declared enum/union/struct name.
	Type string `json:"type"`
	// Description is human/automation-facing documentation for the field.
	Description string `json:"description,omitempty"`
	// Optional marks a field that may be absent.
	Optional bool `json:"optional,omitempty"`
	// Repeated marks a field that is an array of Type.
	Repeated bool `json:"repeated,omitempty"`
}

// ConfigField is a configuration field and its typed default. Type is drawn
// from the shared config field-type vocabulary
// (go.putnami.dev/protocol/config); Default, when present, is a JSON-native
// value whose kind must agree with Type.
type ConfigField struct {
	// Name is the field's configuration key.
	Name string `json:"name"`
	// Type is one of the config field-type vocabulary values.
	Type string `json:"type"`
	// Description is human/automation-facing documentation for the field.
	Description string `json:"description,omitempty"`
	// Required marks a field the workload must provide a value for.
	Required bool `json:"required,omitempty"`
	// Default is the typed default value applied when the field is unset. Its
	// JSON kind must agree with Type.
	Default any `json:"default,omitempty"`
	// Sensitive marks values that must not be logged or emitted in cleartext.
	Sensitive bool `json:"sensitive,omitempty"`
}

// Scope is a security scope (e.g. an OAuth-style scope name).
type Scope struct {
	// Name is the scope identity (e.g. "payments:write").
	Name string `json:"name"`
	// Description is human/automation-facing documentation for the scope.
	Description string `json:"description,omitempty"`
}

// Capability is a named authorization capability that draws on a set of scopes.
type Capability struct {
	// Name is the capability identity.
	Name string `json:"name"`
	// Description is human/automation-facing documentation for the capability.
	Description string `json:"description,omitempty"`
	// Scopes lists the declared Scope names this capability draws on.
	Scopes []string `json:"scopes,omitempty"`
}

// Grant is a named permission grant that confers a capability.
type Grant struct {
	// Name is the grant identity.
	Name string `json:"name"`
	// Description is human/automation-facing documentation for the grant.
	Description string `json:"description,omitempty"`
	// Capability is the declared Capability name this grant confers.
	Capability string `json:"capability,omitempty"`
}

// Claim is a token / principal claim with a config-vocabulary type.
type Claim struct {
	// Name is the claim key (e.g. "sub", "tenantId").
	Name string `json:"name"`
	// Type is one of the config field-type vocabulary values.
	Type string `json:"type"`
	// Description is human/automation-facing documentation for the claim.
	Description string `json:"description,omitempty"`
	// Required marks a claim that must be present on a principal.
	Required bool `json:"required,omitempty"`
}

// PrincipalKind names one recognized kind of principal (e.g. user, service).
type PrincipalKind struct {
	// Name is the principal-kind identity.
	Name string `json:"name"`
	// Description is human/automation-facing documentation for the kind.
	Description string `json:"description,omitempty"`
}

// DiscoveryMetadata carries contract-level discovery metadata a catalog or docs
// pipeline surfaces. Every field is optional.
type DiscoveryMetadata struct {
	// Title is the human-facing contract title.
	Title string `json:"title,omitempty"`
	// Summary is a one-line description of the contract.
	Summary string `json:"summary,omitempty"`
	// Version is the contract's own semantic version, distinct from
	// ProtocolVersion.
	Version string `json:"version,omitempty"`
	// Tags are ordered discovery tags for filtering and grouping.
	Tags []string `json:"tags,omitempty"`
}
