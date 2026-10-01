# Overview

`go.putnami.dev/cli/model` holds the Putnami CLI's data model. It is the answer to a
single question: *what is a workspace, an extension and a job, independently of the
code that runs them?*

## Why this module exists

`tooling/cli` grew as one module in which type declarations and the I/O that drives
them lived side by side. That coupling had three costs:

- **Reading.** Understanding what a `Project` or a `JobResult` *is* meant reading the
  loader, the scheduler and the renderer that happened to share the file.
- **Testing.** Exercising the model dragged in a filesystem, a subprocess or a clock.
- **Dependencies.** Five projection packages — `internal/output`, `internal/machine`,
  `internal/watch`, `internal/workspace_state`, `internal/changeplan` — render,
  serialize or summarize a run and consume *no* scheduler behaviour, yet each
  depended transitively on the executor, the CPU allocator and the remote-cache
  client, because `jobs.JobResult` was declared in the same package as
  `newScheduler`.

Splitting the model out made each of those cheap without changing a single
behaviour. [ADR 0006](../../cli/doc/adr/0006-cli-model-and-command-verticals.md)
records the decision, the rules the move followed, and the ratchets that hold it.

## What lives here

Type declarations and pure methods over them:

| Package | Owns |
| --- | --- |
| `workspace` | `Workspace`, `Project`, `ScopeContribution`, the dependency graph, target/filter/include resolution, identity, the probe view, change→project impact mapping, auto-selection inputs |
| `extension` | `ExtensionDescription`, `JobDefinition`, manifest and contract types, pipeline expansion, the expression evaluator, flag declarations, reserved-provider rules |
| `jobs` | `ScheduledJob`, `JobResult`, `Execution`, the plan contract, the canonical result reducer, the runtime-event parser, invocation and identity types, task resource profiles, the job-context shape |

## What does not

Anything with an effect. Loading a `putnami.json`, discovering extensions on disk,
spawning a job process, computing a cache entry, rendering a run — all of that stays
in `go.putnami.dev/tooling/cli`. When a file mixed the two, the type declarations and
pure methods moved and the I/O functions stayed.

One exception, recorded rather than hidden: link resolution. The whole module's
filesystem access is `workspace.ResolveLinks` (`workspace/resolve_links*.go`),
called by `workspace.CanonicalRoot` (`workspace/workspace.go`) and
`canonicalExistingPrefix` (`workspace/impact.go`). `CanonicalRoot` is here because
`filter.go`, `target.go` and `impact.go` all need it, and duplicating it across the
module boundary would have been worse than admitting it.

`ResolveLinks` is `filepath.EvalSymlinks` outside Windows. On Windows it also
follows a directory junction, which `filepath.EvalSymlinks` leaves in place, with
the same `CreateFile` and `GetFinalPathNameByHandleW` calls as
`go.putnami.dev/sdk/extension/dirlink.Resolve`. The module cannot import that
helper (see the dependency rule below), so it keeps a standard-library copy, and
`TestModelResolveLinksAgreesWithDirlink` in `tooling/cli` pins the two to the
same answer.

## Dependency rule

This module may depend only on `go.putnami.dev/protocol/*`. It must never import
`go.putnami.dev/tooling/cli` — the dependency runs one way, from the CLI to the model.
This is enforced by `go.mod` rather than by review: an import pointing the other way
does not compile, so a symbol split onto the wrong side of the line is a build
failure, not a defect that ships.

On the CLI side, `internal/cli/model_decoupling_ratchet_test.go` pins the other
half of the property — the five projection packages must not reach *back* into
`internal/{jobs,workspace,extension}` — with a written, both-directions-checked
exception list for the three that still need one symbol each.

## Naming

Packages are named after the internal `tooling/cli` packages they were extracted from,
so selectors at call sites are unchanged: `workspace.Project` stays `workspace.Project`,
`jobs.JobResult` stays `jobs.JobResult`. Moving a symbol here is an import-line change
for its consumers, never a rewrite. Each origin package keeps a `model_alias.go` of
`type X = model.X` aliases so consumers can be flipped one at a time.

That convenience has a cost worth knowing about: because the two spellings are
identical, a package re-coupling itself to the origin is invisible at every call
site. The import line is the only evidence, which is exactly what the ratchet
above asserts on.
