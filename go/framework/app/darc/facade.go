package darc

import (
	"context"
	"time"

	archproto "go.putnami.dev/protocol/architecture"
	darcproto "go.putnami.dev/protocol/architecture/darc"
)

// The runtime components live at `go.putnami.dev/protocol/architecture/darc`,
// beside the contract vocabulary they enforce. They are framework-free on
// purpose: enforcing a declared contract must not require being a Putnami
// application (ADR 0002 of protocols/architecture).
//
// This package re-exports them UNCHANGED — every name below is an alias, so a
// value built here and a value built there are the same type — and adds the one
// thing that is application-owned: the describe carrier in evidence.go, which
// turns a registered component into a `domainAccess` capability row.
// Applications keep importing this package; nothing moved for them.

// The errors a component returns for a declared behavior, re-exported by
// identity so errors.Is works across both import paths.
var (
	ErrMissing               = darcproto.ErrMissing
	ErrStale                 = darcproto.ErrStale
	ErrLateUpdate            = darcproto.ErrLateUpdate
	ErrWriterClaimed         = darcproto.ErrWriterClaimed
	ErrNotTheWriter          = darcproto.ErrNotTheWriter
	ErrDeletionNotApplicable = darcproto.ErrDeletionNotApplicable
	ErrImmutable             = darcproto.ErrImmutable
	ErrNotActive             = darcproto.ErrNotActive
	ErrFactNotImported       = darcproto.ErrFactNotImported
)

// Freshness is how a read describes the copy it returned.
type Freshness = darcproto.Freshness

// The two freshness verdicts the declared staleness bound can support.
const (
	FreshnessFresh Freshness = darcproto.FreshnessFresh
	FreshnessStale Freshness = darcproto.FreshnessStale
)

// Record is one copied fact with its declared provenance, observation time,
// and freshness verdict.
type Record[T any] = darcproto.Record[T]

// ContractError reports a contract a component refuses to run.
type ContractError = darcproto.ContractError

// VersionOrder compares two source versions.
type VersionOrder = darcproto.VersionOrder

// Component is one runtime-enforced contract; the constructors below return one.
type Component = darcproto.Component

// Reference is the reference mode: a stable handle on facts another domain owns.
type Reference = darcproto.Reference

// Projection is the projection mode: a rebuildable local copy under the
// declared consistency, writer, rebuild, and deletion contract.
type Projection[T any] = darcproto.Projection[T]

// ProjectionOption configures a projection at construction.
type ProjectionOption[T any] = darcproto.ProjectionOption[T]

// Update is one observed producer state change a projection applies.
type Update[T any] = darcproto.Update[T]

// Writer is the single write handle the local model names.
type Writer[T any] = darcproto.Writer[T]

// Snapshot is the snapshot mode: an immutable, version-addressed copy.
type Snapshot[T any] = darcproto.Snapshot[T]

// SnapshotOption configures a snapshot at construction.
type SnapshotOption[T any] = darcproto.SnapshotOption[T]

// Command is the command mode: a request another domain decides whether to honor.
type Command[T any] = darcproto.Command[T]

// CommandOption configures a command at construction.
type CommandOption[T any] = darcproto.CommandOption[T]

// SendFunc carries one command payload over the declared transport.
type SendFunc[T any] = darcproto.SendFunc[T]

// NewReference builds the reference component for a declared import.
func NewReference(contract archproto.Import) (*Reference, error) {
	return darcproto.NewReference(contract)
}

// NewProjection builds the projection component for a declared import.
func NewProjection[T any](contract archproto.Import, opts ...ProjectionOption[T]) (*Projection[T], error) {
	return darcproto.NewProjection[T](contract, opts...)
}

// NewSnapshot builds the snapshot component for a declared import.
func NewSnapshot[T any](contract archproto.Import, opts ...SnapshotOption[T]) (*Snapshot[T], error) {
	return darcproto.NewSnapshot[T](contract, opts...)
}

// NewCommand builds the command component for a declared import.
func NewCommand[T any](contract archproto.Import, send SendFunc[T], opts ...CommandOption[T]) (*Command[T], error) {
	return darcproto.NewCommand[T](contract, send, opts...)
}

// WithBootstrap supplies the bootstrap source a rebuildable projection requires.
func WithBootstrap[T any](load func(ctx context.Context) ([]Update[T], error)) ProjectionOption[T] {
	return darcproto.WithBootstrap[T](load)
}

// WithReplay supplies the replay source a replay-rebuilt projection requires.
func WithReplay[T any](load func(ctx context.Context, since string) ([]Update[T], error)) ProjectionOption[T] {
	return darcproto.WithReplay[T](load)
}

// WithVersionOrder replaces the comparator that orders source versions.
func WithVersionOrder[T any](order VersionOrder) ProjectionOption[T] {
	return darcproto.WithVersionOrder[T](order)
}

// WithClock replaces the clock the declared freshness bound is measured against.
func WithClock[T any](now func() time.Time) ProjectionOption[T] {
	return darcproto.WithClock[T](now)
}

// WithSnapshotVersionOrder replaces the comparator that decides the latest version.
func WithSnapshotVersionOrder[T any](order VersionOrder) SnapshotOption[T] {
	return darcproto.WithSnapshotVersionOrder[T](order)
}

// WithSnapshotClock replaces the clock the declared freshness bound is measured against.
func WithSnapshotClock[T any](now func() time.Time) SnapshotOption[T] {
	return darcproto.WithSnapshotClock[T](now)
}

// WithFailureObserver observes every failed send of a command.
func WithFailureObserver[T any](observe func(error)) CommandOption[T] {
	return darcproto.WithFailureObserver[T](observe)
}
