// Package storage defines the Putnami object/blob storage resource protocol:
// the provider-neutral contract a project uses to declare the storage it
// needs, and the binding a deployer hands back so the workload can construct a
// storage backend.
//
// Two shapes:
//
//   - Manifest: a project's declared storage Resources — what it needs, named
//     and provider-neutral. A deployer (e.g. Putnami Cloud) reads it, creates
//     the provider resource, and applies the IAM grants the declaration
//     implies. This is the rich storage contract; the infra-requirements
//     protocol keeps only a thin "needs a bucket named X" pointer.
//
//   - Binding: the resolved backend coordinates a deployer injects into the
//     workload after provisioning. The workload's storage library
//     (go.putnami.dev/storage, @putnami/storage) constructs a backend from it
//     and transfers bytes DIRECTLY to the provider — the protocol never
//     proxies object bytes.
//
// The protocol names no provider and pins no provider-specific vocabulary:
// GCS is the first implementation, but the contract stays portable across S3,
// filesystem, and future backends. See README.md for the protocol /
// provisioning / data-plane split, scope and isolation semantics, lifecycle
// expectations, and the binding contract.
package storage

import "sort"

// ProtocolVersion is the current storage-protocol version. It is stamped into
// every Manifest and Binding and pinned by the JSON schemas. Bumped on any
// backwards-incompatible change to the shapes, enums, or semantics; adding an
// optional field within an existing shape does not bump it.
const ProtocolVersion = 1

// Access is the access level a workload needs to a storage resource. It drives
// the IAM grant scope a deployer provisions. The enum is intentionally closed:
// adding a value requires a ProtocolVersion bump so deployers can decide how to
// react.
type Access string

// Access values.
const (
	AccessRead      Access = "read"
	AccessWrite     Access = "write"
	AccessReadWrite Access = "readwrite"
)

// Valid reports whether a is a recognized access level.
func (a Access) Valid() bool {
	switch a {
	case AccessRead, AccessWrite, AccessReadWrite:
		return true
	default:
		return false
	}
}

// Scope is the isolation boundary a storage resource is owned at. It tells a
// deployer whether two workloads that name the same resource share one provider
// bucket or get isolated ones — e.g. a runtime-scoped resource is unique per
// preview/runtime project, a workspace-scoped one is shared across a workspace.
// The enum is intentionally closed.
type Scope string

// Scope values.
const (
	ScopeProject     Scope = "project"
	ScopeWorkspace   Scope = "workspace"
	ScopeEnvironment Scope = "environment"
	ScopeRuntime     Scope = "runtime"
)

// Valid reports whether s is a recognized scope.
func (s Scope) Valid() bool {
	switch s {
	case ScopeProject, ScopeWorkspace, ScopeEnvironment, ScopeRuntime:
		return true
	default:
		return false
	}
}

// Identity is how a workload authenticates to the provider in a Binding: with
// static credentials the deployer supplies, or with an ambient workload
// identity the platform attaches (no long-lived secret). The enum is
// intentionally closed.
type Identity string

// Identity values.
const (
	IdentityStatic   Identity = "static"
	IdentityWorkload Identity = "workload"
)

// Valid reports whether i is a recognized identity mode.
func (i Identity) Valid() bool {
	switch i {
	case IdentityStatic, IdentityWorkload:
		return true
	default:
		return false
	}
}

// Manifest is a project's declared object/blob storage resources. A deployer
// consumes it to provision the resources and grant the workload access.
type Manifest struct {
	Schema          string     `json:"$schema,omitempty"`
	ProtocolVersion int        `json:"protocolVersion"`
	Resources       []Resource `json:"resources,omitempty"`
}

// Resource is one provider-neutral object/blob storage requirement. Name is the
// logical identifier a deployer maps to a physical bucket/container; every
// other field is an optional declaration of what the workload needs the
// resource to support.
type Resource struct {
	// Name is the logical bucket identifier (the join key back from a Binding).
	Name string `json:"name"`
	// Access is the access level the workload needs (read, write, readwrite).
	Access Access `json:"access,omitempty"`
	// Scope is the isolation boundary the resource is owned at.
	Scope Scope `json:"scope,omitempty"`
	// SignedUrls requests signed-URL capability: the workload mints time-limited
	// GET/PUT URLs so clients transfer bytes directly to the provider, which
	// requires the deployer to grant a signer identity.
	SignedUrls bool `json:"signedUrls,omitempty"`
	// Public requests public (unauthenticated) read access to objects, which a
	// deployer maps to a public bucket policy / IAM grant.
	Public bool `json:"public,omitempty"`
	// Retention is a free-form, deployer-defined retention string (e.g. "30d").
	Retention string `json:"retention,omitempty"`
}

// ProjectedResource is the thin, secret-free projection of one storage Resource
// that the infra-requirements protocol consumes. It carries only the fields a
// deployer needs to provision the bucket and grant the runtime identity — the
// logical name, the access level (the IAM grant scope), whether objects are
// public, and the retention hint. The resolved physical coordinates a workload
// connects with (backend, bucket, prefix, identity) live only in a Binding, so
// they cannot travel into a deployer's infra requirements through this path.
type ProjectedResource struct {
	Name      string
	Access    Access
	Public    bool
	Retention string
}

// Project flattens a Manifest into the thin infra projection, sorted by resource
// name for deterministic aggregation. It mirrors how the database protocol's
// RequirementManifest.Project() feeds protocol/infra: the infra-requirements
// protocol owns no rich storage shape of its own, it projects this one.
//
// Two rich-contract fields are intentionally dropped. Scope is a logical→physical
// resolution concern (which bucket a name resolves to) a deployer reads from the
// Manifest directly, not an IAM-grant input. SignedUrls maps onto a workload-level
// signer grant (infra's runtime security.signBlob), not a per-bucket requirement.
// What remains — {name, access, public, retention} — is exactly what a deployer
// needs to create the bucket and grant the runtime identity. Returns nil for a
// nil or empty manifest.
func (m *Manifest) Project() []ProjectedResource {
	if m == nil || len(m.Resources) == 0 {
		return nil
	}
	out := make([]ProjectedResource, 0, len(m.Resources))
	for _, r := range m.Resources {
		out = append(out, ProjectedResource{
			Name:      r.Name,
			Access:    r.Access,
			Public:    r.Public,
			Retention: r.Retention,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Binding is the resolved backend coordinates a deployer injects into a workload
// after provisioning a Resource. The workload constructs a storage backend from
// it; object bytes then move directly between the workload/clients and the
// provider, never through Putnami.
type Binding struct {
	Schema          string `json:"$schema,omitempty"`
	ProtocolVersion int    `json:"protocolVersion"`
	// Name is the logical resource name the workload requested — the join key
	// back to the Manifest Resource.
	Name string `json:"name"`
	// Backend is the provider-neutral backend kind the workload should construct
	// (e.g. "gcs", "s3", "file"). Left an open string so the protocol is not
	// coupled to a fixed provider set.
	Backend string `json:"backend"`
	// Bucket is the physical bucket/container the Resource resolved to.
	Bucket string `json:"bucket"`
	// Prefix is an optional key prefix within Bucket — e.g. the per-scope
	// isolation prefix a deployer chose so several logical resources can share a
	// physical bucket.
	Prefix string `json:"prefix,omitempty"`
	// Identity is how the workload authenticates to the provider.
	Identity Identity `json:"identity,omitempty"`
	// SignedUrls reports whether signed-URL capability was granted.
	SignedUrls bool `json:"signedUrls,omitempty"`
}
