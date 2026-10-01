// Package migration is the transversal migration framework. It hosts the
// cross-kind contracts (Source, Runner, Record, Registry) that runners for
// SQL — and future kinds such as GCS or document collections — implement.
//
// Authoring formats and storage backends live in kind-specific packages
// (e.g. go.putnami.dev/database for SQL). This package contains only the
// shared types, the in-process registry, and the lifecycle entry points
// that the app builder and the migrate CLI call.
package migration

import (
	"context"
	"time"
)

// Kind identifies the migration domain a Source or Runner operates on. The
// string is the stable identity used in logs, CLI output, and JSON
// describer artifacts.
type Kind string

// KindSQL is the canonical kind for relational-database migrations. It is
// the only kind shipped initially; new kinds register alongside it
// without modifications to this package.
const KindSQL Kind = "sql"

// Source is what a feature plugin contributes via
// app.MigrationContributor.MigrationSources(). A single plugin may
// contribute many sources — stacking an embed.FS of paired SQL files with
// inline definitions, or covering multiple kinds — and each source is
// dispatched to the Runner registered for its Kind.
//
// Concrete Source values are tagged-union-style: each kind defines its own
// type (e.g. database.SQLSource) that satisfies this interface. The
// transversal Registry stores them opaquely and hands them to the matching
// Runner at apply time.
type Source interface {
	// Kind returns the migration domain this source belongs to.
	Kind() Kind
	// Namespace returns the contributing plugin's identifier — typically
	// the value of its Plugin.Name(). Used for diagnostics, logging, and
	// the recommended ${namespace}/${basename} migration naming.
	Namespace() string
}

// RecordStatus classifies a migration's lifecycle state. Runners populate
// it on each Record they return.
//
// State transitions (forward = apply; reverse = rollback):
//
//	pending  --apply-->  applied  --rollback-->  pending
//	pending  --apply-->  failed   (failure row recorded)
//
// StatusFailed today conflates "apply failed" with "rollback failed" —
// the state-store row's success column is 0 in both cases. The
// distinction (was this row a failed CREATE TABLE or a failed DROP
// TABLE attempt?) is recoverable from the surrounding context and the
// error_message column. If consumers need it explicitly we can split
// into StatusApplyFailed / StatusRollbackFailed pre-1.0; flagged for
// re-evaluation after the first production runner emerges.
type RecordStatus string

// Canonical record statuses.
const (
	// StatusApplied means the migration has executed successfully and is
	// recorded in the state store.
	StatusApplied RecordStatus = "applied"
	// StatusRolledBack means the migration was applied and later reversed.
	StatusRolledBack RecordStatus = "rolled-back"
	// StatusPending means the migration is registered but not yet applied.
	StatusPending RecordStatus = "pending"
	// StatusFailed means an apply OR rollback attempt recorded a failure
	// row. See RecordStatus docs for the rationale on conflating the two.
	StatusFailed RecordStatus = "failed"
)

// Record is the cross-kind result type Runners return. It carries the
// universal fields the CLI needs to render `status`, `up`, `down`,
// `verify`, and `inspect` output homogeneously across kinds.
//
// Fields that don't apply to a kind are left zero-valued. The optional
// Target field is the kind-specific scope name (datasource for SQL, bucket
// for GCS, collection for document) and is exposed verbatim to the user.
type Record struct {
	Kind       Kind         `json:"kind"`
	Namespace  string       `json:"namespace,omitempty"`
	Name       string       `json:"name"`
	Status     RecordStatus `json:"status"`
	Hash       string       `json:"hash,omitempty"`
	DownHash   string       `json:"downHash,omitempty"`
	ExecutedAt time.Time    `json:"executedAt,omitzero"`
	DurationMs int64        `json:"durationMs,omitempty"`
	Error      string       `json:"error,omitempty"`
	// Source is the diagnostic origin string set by the loader, e.g.
	// "embed:iam/migrations/20260520120000_create_users.up.sql" or
	// "inline:iam".
	Source string `json:"source,omitempty"`
	// Target is the kind-specific scope: datasource for SQL, bucket for
	// GCS, etc. Empty for kinds with a single global scope.
	Target string `json:"target,omitempty"`
}

// ApplyOpts is the input to Runner.Apply.
//
// During the framework's automatic Start lifecycle phase, callers set
// AllowSourceOnly so feature plugins may expose static migration metadata
// without requiring a runner in apps that do not own that backend. Runners
// still receive Force=false and respect their own auto-apply settings
// (e.g. database.MigrationConfig.AutoApply, false in environments where
// a separate migrate CLI/job owns the operation). Explicit migration calls
// keep AllowSourceOnly false so missing runners fail loudly; when a human or
// CI step invokes `up` via the migrate CLI, Force=true is also set so the
// apply happens regardless of runner gating.
type ApplyOpts struct {
	// To narrows the apply to migrations up to and including the named
	// migration. Empty means "apply every pending migration".
	//
	// The value may be either the fully namespaced form
	// "iam/20260520120000_create_users" or the bare basename
	// "20260520120000_create_users" when it is unambiguous within the
	// runner's scope. The SQL runner resolves the name per-datasource:
	// if two namespaces (e.g. iam/001 and secrets/001) both contain the
	// same basename on the same datasource, the resolver errors with
	// the candidate list so the caller can disambiguate. Cross-datasource
	// collisions are not ambiguous — each datasource resolves
	// independently.
	To string
	// Force, when true, ignores per-runner auto-apply gating. Set by
	// the migrate CLI; left false by the framework's lifecycle Migrate
	// phase.
	Force bool
	// AllowSourceOnly, when true, permits sources whose Kind has no
	// registered Runner. Start sets this for the automatic lifecycle apply;
	// explicit migration entry points leave it false so misconfiguration is
	// still reported before any runner is invoked.
	AllowSourceOnly bool
}

// RollbackOpts is the input to Runner.Rollback. Empty Opts roll back only
// the most recent applied migration; setting To rolls back every
// migration applied AFTER the named one (the named one stays applied).
type RollbackOpts struct {
	// To names a previously applied migration. When set, every migration
	// applied after it is rolled back in reverse execution order; the
	// named migration itself remains applied.
	//
	// The value may be either the fully namespaced form
	// "iam/20260520120000_create_users" or the bare basename when it is
	// unambiguous across namespaces; ambiguity is a Runner-level error.
	To string
}

// DriftReport summarizes the diff between a Runner's in-memory registry
// view and the persisted state store. Empty() returns true when the two
// are in sync.
type DriftReport struct {
	Kind                Kind        `json:"kind"`
	HashDrifts          []HashDrift `json:"hashDrifts,omitempty"`
	MissingFromRegistry []Record    `json:"missingFromRegistry,omitempty"`
	MissingFromStore    []Record    `json:"missingFromStore,omitempty"`
}

// Empty reports whether the registry and state store agree.
func (d DriftReport) Empty() bool {
	return len(d.HashDrifts) == 0 &&
		len(d.MissingFromRegistry) == 0 &&
		len(d.MissingFromStore) == 0
}

// HashDrift describes one applied migration whose stored hash no longer
// matches the current registry's hash for the same name.
type HashDrift struct {
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name"`
	StoredHash  string `json:"storedHash"`
	CurrentHash string `json:"currentHash"`
	Target      string `json:"target,omitempty"`
}

// Runner is the kind-specific engine. Exactly one Runner registers per
// Kind into a Registry; that runner consumes every Source of its kind
// contributed by feature plugins.
//
// Implementations live in kind-owning packages — e.g.
// go.putnami.dev/database registers a SQL Runner. The transversal
// migration package is intentionally backend-free.
type Runner interface {
	// Kind returns the domain this runner serves. Must equal the Kind of
	// every Source the runner consumes.
	Kind() Kind
	// Apply runs every pending migration of this kind across the
	// runner's targets. Returns one Record per migration acted upon (no
	// records when everything is up-to-date). When opts.Force is false,
	// the runner is free to no-op according to its own auto-apply
	// settings — that is how production services skip startup migration
	// in favor of a dedicated migrate job.
	Apply(ctx context.Context, opts ApplyOpts) ([]Record, error)
	// Status returns the recorded state of every migration of this kind
	// — applied, rolled-back, or pending — across the runner's targets.
	Status(ctx context.Context) ([]Record, error)
	// Rollback reverses applied migrations per opts. Returns one Record
	// per migration rolled back, in execution order.
	Rollback(ctx context.Context, opts RollbackOpts) ([]Record, error)
	// Verify compares the runner's registered migrations against the
	// state store and returns a DriftReport. Empty report means no drift.
	Verify(ctx context.Context) (DriftReport, error)
}
