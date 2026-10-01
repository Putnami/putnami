// Package documents holds the gates that read this repository's own documents:
// the governance surface, the contributor recipe, the release plan and its
// recorded verdict, the public-cut candidate, and the shipped manifests.
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
