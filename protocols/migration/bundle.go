package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// BundleProtocol is the canonical identifier embedded in every migration
// bundle document. It is versioned independently from the execution
// ProtocolVersion so the bundle wire format can evolve on its own cadence.
const BundleProtocol = "migration-bundle.v1"

// BundleProtocolVersion is the numeric version of the bundle wire format.
const BundleProtocolVersion = 1

// BundleFileName is the canonical manifest file name inside a bundle directory.
const BundleFileName = "bundle.json"

// OperationKind classifies the target system a bundle operation acts on.
//
// Runners materialize SQL today; document and event kinds are reserved so the
// protocol shape is stable before their runners exist.
type OperationKind string

// OperationKind values enumerate the supported migration target systems.
const (
	KindSQL      OperationKind = "sql"
	KindDocument OperationKind = "document"
	KindEvents   OperationKind = "events"
)

// ValidOperationKinds is the set of allowed operation kinds.
var ValidOperationKinds = map[OperationKind]bool{
	KindSQL:      true,
	KindDocument: true,
	KindEvents:   true,
}

// SafetyClass classifies the operational risk of applying an operation.
//
// Remote orchestration uses this to gate auto-apply during deploys: only
// SafetySafeOnline operations are eligible for unattended execution.
type SafetyClass string

// SafetyClass values enumerate the canonical safety classifications.
const (
	SafetySafeOnline       SafetyClass = "safe-online"
	SafetyLongRunning      SafetyClass = "long-running"
	SafetyDestructive      SafetyClass = "destructive"
	SafetyRequiresApproval SafetyClass = "requires-approval"
)

// ValidSafetyClasses is the set of allowed safety classifications.
var ValidSafetyClasses = map[SafetyClass]bool{
	SafetySafeOnline:       true,
	SafetyLongRunning:      true,
	SafetyDestructive:      true,
	SafetyRequiresApproval: true,
}

// Capabilities advertises runner-relevant traits of an operation. Hints are
// emitted with omitempty so the canonical JSON form — and therefore the bundle
// digest — stays minimal when traits are absent.
type Capabilities struct {
	Transactional bool `json:"transactional,omitempty"`
	Reversible    bool `json:"reversible,omitempty"`
	Async         bool `json:"async,omitempty"`
	Resumable     bool `json:"resumable,omitempty"`
	// Compatible reports that the schema after this operation stays readable
	// and writable by the previous application image (expand/contract), so an
	// environment may roll back across it without running Down.
	Compatible bool `json:"compatible,omitempty"`
}

// PayloadRef points at a payload file inside the bundle and pins its content
// hash. Path is a forward-slash relative path under the bundle root; Hash is a
// lowercase 64-character SHA-256 hex digest of the payload bytes, matching the
// hash semantics used by Definition.Hash.
type PayloadRef struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// BundleOperation is one migration operation captured in a bundle. Operations
// are logically grouped by kind, target, and namespace; ordering within a
// target follows OrderKey then Name, mirroring the execution protocol.
type BundleOperation struct {
	Kind   OperationKind `json:"kind"`
	Target string        `json:"target"`
	// Schema is the schema the operation's DDL targets — the schema half of the
	// source's datasource. It travels in the bundle so an applier reconstructs a
	// schema-aware source and sets it as the migration search_path, letting one
	// migration set target many schemas without baking schema names into the
	// SQL. Empty (omitempty) means schema-agnostic: the applier's connection
	// search_path governs. It participates in the digest like every other
	// identity field, so a schema-less bundle digests exactly as before.
	Schema       string       `json:"schema,omitempty"`
	Namespace    string       `json:"namespace,omitempty"`
	Name         string       `json:"name"`
	OrderKey     string       `json:"orderKey,omitempty"`
	Up           PayloadRef   `json:"up"`
	Down         *PayloadRef  `json:"down,omitempty"`
	Safety       SafetyClass  `json:"safety"`
	Capabilities Capabilities `json:"capabilities,omitempty"`
}

// GitMetadata records the source revision a bundle was generated from. It is
// provenance only and does not participate in the bundle digest, so the same
// migration content produces the same digest across rebuilds from different
// checkouts.
type GitMetadata struct {
	Revision string `json:"revision,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
}

// Bundle is the top-level migration-bundle.v1 document. It is an immutable
// release artifact: it carries migration definitions and payload references but
// never secrets, DSNs, or environment credentials. Remote execution resolves
// environment bindings server-side from Digest and the operations alone.
type Bundle struct {
	Protocol    string            `json:"protocol"`
	AppName     string            `json:"appName"`
	Source      string            `json:"source,omitempty"`
	Version     string            `json:"version,omitempty"`
	Git         *GitMetadata      `json:"git,omitempty"`
	ImageDigest string            `json:"imageDigest,omitempty"`
	Digest      string            `json:"digest,omitempty"`
	GeneratedAt string            `json:"generatedAt,omitempty"`
	Operations  []BundleOperation `json:"operations"`
}

// ComputePayloadHash returns the canonical lowercase SHA-256 hex digest of a
// payload's bytes. Both the Go and TypeScript bundle emitters must use this
// exact algorithm so equivalent payloads hash identically across languages.
func ComputePayloadHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// bundleDigestPayload is the canonical subset of a bundle that participates in
// the content-addressing digest. Release/provenance fields (version, git, image
// digest, generated timestamp, source path) and the digest itself are
// deliberately excluded so the digest is a stable address for the migration
// content that can be re-published across releases without changing when the
// operations and payload hashes are unchanged.
type bundleDigestPayload struct {
	Protocol   string            `json:"protocol"`
	AppName    string            `json:"appName"`
	Operations []BundleOperation `json:"operations"`
}

// ComputeBundleDigest returns the canonical lowercase SHA-256 hex digest that
// content-addresses a bundle. The bundle is normalized and its operations
// sorted into canonical order before hashing, so two bundles with the same
// migration content always produce the same digest regardless of input order
// or build provenance. This is what makes remote publish idempotent by digest.
func ComputeBundleDigest(b Bundle) string {
	normalized := NormalizeBundle(b)
	payload := bundleDigestPayload{
		Protocol:   normalized.Protocol,
		AppName:    normalized.AppName,
		Operations: normalized.Operations,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NormalizeOperation applies canonical defaults to one bundle operation.
func NormalizeOperation(op BundleOperation) BundleOperation {
	if op.Target == "" {
		op.Target = DefaultDatasource
	}
	if op.OrderKey == "" {
		op.OrderKey = op.Name
	}
	if op.Safety == "" {
		op.Safety = SafetySafeOnline
	}
	// A persisted rollback payload implies the operation is reversible.
	if op.Down != nil {
		op.Capabilities.Reversible = true
	}
	return op
}

// RollbackAllowed reports whether an environment may roll back across a set of
// operations without leaving its database ahead of the image it runs.
//
// Migrations are forward-only, so the question a deploy asks is never "can I
// undo this" but "can the previous image still work against this schema". Two
// answers say yes, and only those two: the operation carries a Down payload
// (Reversible), or the operation was written expand/contract so the previous
// image reads and writes the new schema unchanged (Compatible). One operation
// that says neither refuses the whole set — a rollback is atomic per workload,
// and a partially-rolled-back schema is the state nobody can reason about.
//
// An empty set is allowed: rolling back across nothing is always safe.
func RollbackAllowed(ops []BundleOperation) bool {
	for _, op := range ops {
		if !op.Capabilities.Reversible && !op.Capabilities.Compatible {
			return false
		}
	}
	return true
}

// NormalizeBundle returns a normalized copy of a bundle with canonical defaults
// applied and operations sorted deterministically. The returned bundle carries
// no Digest; callers compute it via ComputeBundleDigest when needed.
func NormalizeBundle(b Bundle) Bundle {
	if b.Protocol == "" {
		b.Protocol = BundleProtocol
	}

	ops := make([]BundleOperation, len(b.Operations))
	for i, op := range b.Operations {
		ops[i] = NormalizeOperation(op)
	}
	sort.SliceStable(ops, func(i, j int) bool {
		return lessOperation(ops[i], ops[j])
	})
	b.Operations = ops

	return b
}

// lessOperation defines the canonical ordering of bundle operations: by kind,
// then target, then namespace, then order key, then name. Hashes break any
// remaining tie so ordering is fully deterministic.
func lessOperation(a, b BundleOperation) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Target != b.Target {
		return a.Target < b.Target
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.OrderKey != b.OrderKey {
		return a.OrderKey < b.OrderKey
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Up.Hash < b.Up.Hash
}
