// Package storage provides object storage with pluggable backends.
//
// Buckets define storage containers with constraints (size limits, MIME types).
// Backends implement the actual storage operations (filesystem, memory, S3).
package storage

import (
	"sync"

	storageproto "go.putnami.dev/protocol/storage"
)

// Access is the access level a workload needs to a bucket. It mirrors the
// storage protocol's closed enum and drives the IAM grant a deploy target
// provisions for the runtime identity: the build surfaces it as the
// infra.storage requirement's access field so the role travels with the
// declared requirements instead of being granted by hand.
type Access = storageproto.Access

// Access values, re-exported from the storage protocol so callers configure a
// bucket without importing the protocol package directly.
const (
	AccessRead      = storageproto.AccessRead
	AccessWrite     = storageproto.AccessWrite
	AccessReadWrite = storageproto.AccessReadWrite
)

// BucketOptions configures bucket constraints.
type BucketOptions struct {
	MaxFileSize      int64    // Maximum file size in bytes (0 = unlimited).
	AllowedMimeTypes []string // Allowed MIME types (empty = all allowed).
	Public           bool     // Whether objects are publicly accessible.
	// Access is the access level the workload needs; it is surfaced as the
	// infra.storage requirement's access field during the build's describe
	// phase. Empty defaults to readwrite when the requirement is emitted, since
	// the backend the plugin provides reads and writes the bucket.
	Access Access
	// Retention is a free-form retention policy (e.g. "30d", "1y") surfaced
	// as the infra.storage requirement's retention field during the build's
	// describe phase. Empty means no retention is declared.
	Retention string
}

// BucketDefinition describes a storage bucket.
type BucketDefinition struct {
	Name    string
	Options BucketOptions
}

// Bucket creates a new bucket definition.
func Bucket(name string, opts ...BucketOption) *BucketDefinition {
	b := &BucketDefinition{Name: name}
	for _, opt := range opts {
		opt(&b.Options)
	}
	bucketRegistry.Register(b)
	return b
}

// BucketOption is a functional option for configuring bucket constraints.
type BucketOption func(*BucketOptions)

// WithMaxFileSize sets the maximum file size for objects in the bucket.
func WithMaxFileSize(size int64) BucketOption {
	return func(o *BucketOptions) {
		o.MaxFileSize = size
	}
}

// WithAllowedMimeTypes restricts the MIME types allowed in the bucket.
func WithAllowedMimeTypes(types ...string) BucketOption {
	return func(o *BucketOptions) {
		o.AllowedMimeTypes = types
	}
}

// WithPublic marks the bucket as publicly accessible.
func WithPublic(public bool) BucketOption {
	return func(o *BucketOptions) {
		o.Public = public
	}
}

// WithAccess sets the access level the workload needs to the bucket
// (AccessRead, AccessWrite, or AccessReadWrite). It is surfaced as the
// infra.storage requirement's access field so a deploy target grants the
// runtime identity exactly that role. When unset, the emitted requirement
// defaults to readwrite.
func WithAccess(access Access) BucketOption {
	return func(o *BucketOptions) {
		o.Access = access
	}
}

// WithRetention sets a free-form retention policy for the bucket (e.g. "30d").
// It is surfaced as the infra.storage requirement's retention field when the
// build emits the project's infra-requirements scratch fragment, which the Go
// generator syncs into committed infra/requirements.json.
func WithRetention(retention string) BucketOption {
	return func(o *BucketOptions) {
		o.Retention = retention
	}
}

// Registry tracks all bucket definitions.
type Registry struct {
	mu      sync.RWMutex
	buckets map[string]*BucketDefinition
}

var bucketRegistry = &Registry{
	buckets: make(map[string]*BucketDefinition),
}

// Register adds a bucket to the registry.
func (r *Registry) Register(b *BucketDefinition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buckets[b.Name] = b
}

// Get returns a bucket by name.
func (r *Registry) Get(name string) (*BucketDefinition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.buckets[name]
	return b, ok
}

// All returns all registered buckets.
func (r *Registry) All() []*BucketDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*BucketDefinition, 0, len(r.buckets))
	for _, b := range r.buckets {
		out = append(out, b)
	}
	return out
}

// Clear removes all registered buckets (used in tests).
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buckets = make(map[string]*BucketDefinition)
}

// GetRegistry returns the global bucket registry.
func GetRegistry() *Registry {
	return bucketRegistry
}
