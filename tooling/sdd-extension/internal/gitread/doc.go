// Package gitread reads immutable Git history and worktree source state by
// executing the `git` binary.
//
// # Origin
//
// Every function here is a copy of the semantics the CLI core implemented in
// `tooling/cli/internal/git`, narrowed to exactly what the SDD engines call:
//
//	ResolveCommit         <- internal/git/git.go
//	HeadSHA               <- internal/git/git.go
//	ResolveBaseline       <- internal/git/git.go, ResolveBaselineDetailed
//	TreeEntry             <- internal/git/tree.go
//	TreeEntryAt           <- internal/git/tree.go
//	TreeEntries           <- internal/git/tree.go
//	ReadTreeBlob          <- internal/git/tree.go
//	CommitSourceBinding   <- internal/git/tree.go
//	ProjectSourceBinding  <- internal/git/source_binding.go, ProjectSourceBindingMeasured
//
// This package is the only implementation left for most of that list. Core's
// `internal/git/tree.go` was the SDD vertical's file and nothing else called
// it, so it was deleted with the vertical; `ResolveBaseline` and
// `ProjectSourceBinding` were thin wrappers whose last caller was
// `internal/features`, and only the entry points core still uses —
// `ResolveBaselineDetailed`, `ProjectSourceBindingMeasured`, `ResolveCommit`,
// `HeadSHA` — survive there. Read the right-hand column as provenance, not as a
// live twin.
//
// The copy exists because `@putnami/sdd` is a standalone extension module: it
// may depend on `go.putnami.dev/protocol/*` and on the extension SDK, never on
// a CLI internal package. The behavior — including the argument hardening, the
// literal pathspecs, the bounded reads, and the source-v1 record shapes — must
// stay byte-identical to what core computed, because a source binding computed
// here is compared against one an evidence document recorded elsewhere. The
// record one worktree path contributes is therefore not a copy: core and
// ProjectSourceBinding both read it through the extension SDK's sourcebinding
// package, which also takes a tracked file's executable bit from the index on a
// host that does not store one.
//
// What was deliberately NOT copied: the version, diff, and merge-base helpers,
// and the subprocess count `ProjectSourceBindingMeasured` reports. No SDD
// engine calls them. ResolveBaseline dropped core's epic-branch tier and its
// ResolvedBaseline tier label with the same reasoning — the architecture engine
// passes no epic branches and discards the label.
//
// # Safety
//
// Every argument that reaches `git` is validated first. Object IDs must be hex,
// revisions must not look like options, and repository paths are passed with
// the `:(literal)` pathspec magic so a filename that contains pathspec syntax
// cannot widen a read. The package never writes: no checkout, no worktree, no
// index mutation.
package gitread
