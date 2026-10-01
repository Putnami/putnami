// Package darc turns a Domain Access & Replication Contract into a component
// that enforces it at runtime.
//
// A DARC import declares what a consumer domain promises about facts it does
// not own: how fresh a copy may be, what happens when it is missing or stale,
// how updates are ordered and de-duplicated, which component may write it, how
// it is rebuilt, and how a deletion reaches it. Until now those promises lived
// only in `putnami.architecture.json`, and nothing in a running process was
// obliged to keep them. A projection could serve a week-old copy under a
// declared five-minute bound and no gate would notice, because the gate reads
// declarations and the drift is in the code.
//
// The components here close that gap by taking the declaration itself as
// configuration. Construction validates the contract through
// `go.putnami.dev/protocol/architecture` — the same verdict `putnami
// architecture validate` applies — and every read, write, and rebuild is
// governed by what the contract says. The manifest stops being a description of
// the code and becomes its configuration.
//
// # The invariants, item for item
//
// The projection checklist in protocols/architecture/README.md is the
// specification, and [Projection] implements it point by point:
//
//   - bootstrap and update transports — [WithBootstrap] and [WithReplay] are
//     required exactly when the declared rebuild strategy names them, so a
//     projection that cannot be rebuilt fails to construct;
//   - bounded staleness with explicit missing/stale behavior — [Projection.Get]
//     applies `consistency.maxStaleness` against `consistency.onMissing` and
//     `consistency.onStale`: fail-closed returns an error, use-stale returns the
//     copy stamped stale;
//   - ordering, idempotency, and late events — [Writer.Apply] compares source
//     versions, drops a repeated delivery of one update, and handles an older
//     update per `consistency.lateEvents`;
//   - provenance, observation time, and freshness — [Record] carries all three
//     as structure rather than convention, and the writer stamps them;
//   - one writer — [Projection.Writer] hands out exactly one handle, to the
//     component the local model names, and refuses a second;
//   - rebuild and replay — [Projection.Rebuild] runs the declared strategy;
//   - deletion — [Writer.Delete] applies the declared strategy, and a
//     `not-applicable` contract refuses to delete at all; every accepted
//     deletion retains its source-version ordering watermark, even when a hard
//     delete removes the visible record.
//
// [Snapshot] and [Command] cover the other two modes that carry a runtime
// obligation: an immutable version-addressed attachment, and a request another
// domain decides whether to honor. Every runtime component refuses a contract
// or carrier that is not active; a planned declaration is still a target.
//
// # What this package does not do
//
// It moves no data. A projection is given its bootstrap and its updates by the
// consumer — over whatever carrier the contract declares — and this package
// governs what happens to them. Choosing an HTTP client, an event subscription,
// or a file reader stays the consumer's job, because the protocol deliberately
// does not rank transports.
//
// It also decides nothing about permissions. Which domain may import which
// export is declared and reviewed in a manifest; nothing here reads a
// workspace, and constructing a component grants no access it was not handed.
//
// # Status
//
// `go.putnami.dev/protocol/architecture` is experimental: its wire format may
// change without a migration path, and this package's API is built on its
// types, so it moves with it. Use it behind the same explicit opt-in.
//
// # Where the evidence half lives
//
// This package is deliberately framework-free: it imports the architecture and
// diagnostic protocols and nothing else, so any Go program can enforce a
// declared contract — being a Putnami application is not a prerequisite
// (ADR 0002). What IS application-owned is the evidence channel:
// `go.putnami.dev/app/darc` re-exports these components unchanged and adds the
// describe-only plugin that turns a registered component into a `domainAccess`
// capability row. Applications import that package; the two names are the same
// types.
//
// # Wiring
//
// A component is an ordinary value, so in an application the existing DI
// conventions apply with nothing added:
//
//	app.ProvideFunc(func() (*darc.Projection[WorkspaceContext], error) {
//	    return darc.NewProjection[WorkspaceContext](workspaceContextContract,
//	        darc.WithBootstrap(loadFromRuntimeAPI))
//	})
//
// or, token-based, `module.Provide(inject.Value(projection))`.
package darc
