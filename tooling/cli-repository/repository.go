// Package repository runs the tests of @putnami/cli that read this
// repository's own files: its manifests, decisions, templates, agent content,
// extension sources and contracts.
//
// Those tests stay in the CLI's packages, beside the unexported code they
// check, and mark themselves with repotest.Require: they skip in the CLI's own
// suite, whose cache key holds only the CLI's sources and fixtures. This
// project runs them with go test over the CLI's module and declares the
// repository files they read, so a change to one project's putnami.json runs
// them, and not the CLI's whole suite.
//
// The package carries no production code on purpose: it is a test surface, not
// a library. Its module path is a child of the CLI's so it may import
// go.putnami.dev/tooling/cli/internal/... — see go.mod.
package repository
