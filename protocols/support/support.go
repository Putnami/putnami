// Package support defines the public support-status catalog protocol.
//
// The catalog is a reviewed product-policy authority. It deliberately does not
// reuse features.MaturityStage: feature maturity is an earned evidence ladder,
// while support status is a public commitment made by the repository owners.
package support

const (
	// ProtocolVersion is the exact integer version accepted by v1 readers.
	ProtocolVersion = 1
	// CatalogFilename is the single catalog discovered at the workspace root.
	CatalogFilename = "putnami.support.json"
	// CatalogSchemaURL is the canonical support-catalog JSON Schema URI.
	CatalogSchemaURL = "https://putnami.dev/schemas/putnami-support.json"
)

// SubjectKind identifies what one support declaration classifies. The v1
// vocabulary covers only the classifications owned by the release-readiness
// work; adding another kind is a wire-contract change.
type SubjectKind string

// SubjectKind values are the closed v1 classification targets.
const (
	SubjectKindProtocol SubjectKind = "protocol"
	SubjectKindPackage  SubjectKind = "package"
	SubjectKindFeature  SubjectKind = "feature"
)

// ValidSubjectKinds enumerates the closed v1 subject-kind vocabulary.
var ValidSubjectKinds = map[SubjectKind]bool{
	SubjectKindProtocol: true,
	SubjectKindPackage:  true,
	SubjectKindFeature:  true,
}

// Status is a public support commitment, not an ordered maturity stage.
type Status string

// Status values are the exact public support vocabulary.
const (
	// StatusStable carries the repository's normal public support commitment.
	StatusStable Status = "stable"
	// StatusPreview is public but may change before becoming stable.
	StatusPreview Status = "preview"
	// StatusExperimental has no compatibility or parity promise and is not a
	// default surface.
	StatusExperimental Status = "experimental"
)

// ValidStatuses enumerates the complete v1 support-status vocabulary.
var ValidStatuses = map[Status]bool{
	StatusStable:       true,
	StatusPreview:      true,
	StatusExperimental: true,
}

// ParityStatus records a distinct cross-implementation parity statement.
// V1 needs only an explicit unsupported value; absence makes no parity claim.
type ParityStatus string

// ParityStatus values are separate from support status.
const (
	ParityUnsupported ParityStatus = "unsupported"
)

// ValidParityStatuses enumerates the closed v1 parity vocabulary.
var ValidParityStatuses = map[ParityStatus]bool{
	ParityUnsupported: true,
}

// Catalog is the single reviewed workspace support authority.
type Catalog struct {
	// Schema optionally points editors at the canonical JSON Schema.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the support-catalog wire version.
	ProtocolVersion int `json:"protocolVersion"`
	// Entries contains the explicitly reviewed support declarations.
	Entries []Entry `json:"entries"`
}

// Entry classifies one public protocol, package, or feature. Kind and ID form
// its identity. Default and Parity are optional independent policy statements.
type Entry struct {
	// ID is the stable public identifier of the classified subject.
	ID string `json:"id"`
	// Kind selects the protocol, package, or feature identifier namespace.
	Kind SubjectKind `json:"kind"`
	// Status records the subject's current public support commitment.
	Status Status `json:"status"`
	// Default says whether the subject participates in the default experience.
	// A pointer preserves the difference between an explicit false and no claim.
	Default *bool `json:"default,omitempty"`
	// Parity is separate from support status so "experimental" never acquires a
	// second, conflicting meaning.
	Parity ParityStatus `json:"parity,omitempty"`
}
