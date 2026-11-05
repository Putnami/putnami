// Package gitread reads immutable Git history and worktree source state for
// the SDD engines: the architecture baseline, the merge base of a branch, the
// tree entries and blobs at a commit, and the source-v1 binding of a project
// root in the worktree or at a commit.
//
// It starts git through the extension SDK's gitread package, which also owns
// the reads the CLI shares: HEAD, commits, refs and history depth.
// `@putnami/sdd` is a standalone extension module: it may depend on
// `go.putnami.dev/protocol/*` and on the extension SDK, never on a CLI internal
// package.
//
// A source binding computed here is compared against one an evidence document
// recorded elsewhere, so it must stay byte-identical to the one the CLI
// computes: the argument hardening, the literal pathspecs, the bounded reads
// and the source-v1 record shapes. The record one worktree path contributes
// comes from the SDK's sourcebinding package, which also takes a tracked
// file's executable bit from the index on a host that does not store one.
//
// # Safety
//
// Every argument that reaches `git` is validated first. Object IDs must be hex,
// revisions must not look like options, and repository paths are passed with
// the `:(literal)` pathspec magic so a filename that contains pathspec syntax
// cannot widen a read. The package never writes: no checkout, no worktree, no
// index mutation.
package gitread
