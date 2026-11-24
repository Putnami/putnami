// Package documents holds the gates that read this repository's whole
// candidate tree: the public-cut scan, and the release rehearsal with its
// release plan and recorded verdict. They key on every candidate file, so they
// run on main by name rather than in the per-change gate; the gates that read a
// known document set live in tooling/cli-document-set.
//
// They live in their own project because a cache key is per project and per
// command, never per task. Declaring CONTRIBUTING.md as a test input of
// @putnami/cli folded it into the key of EVERY test task of the CLI, so a
// documentation edit re-ran the CLI's whole suite. Here the same declaration
// keys one small suite instead.
//
// The package carries no production code on purpose: it is a test surface over
// the repository, not a library. Its module path is a child of the CLI's so it
// may import go.putnami.dev/tooling/cli/internal/... — see go.mod.
package documents
