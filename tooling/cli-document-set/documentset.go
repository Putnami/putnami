// Package documentset holds the gates that run the CLI against a known set of
// this repository's documents: the neutral-contributor recipe of
// CONTRIBUTING.md, the gate and the environments of putnami.ci.json, the
// project manifests those environments follow, and the cloudless release
// fixture.
//
// The checks that only read files as text live in @putnami/repository-checks,
// which imports nothing from this repository. These gates need the CLI's
// code, so they key on the documents they read: a change runs them when it
// touches one of those documents or the code they compile.
//
// The package carries no production code on purpose: it is a test surface over
// the repository, not a library. Its module path is a child of the CLI's so it
// may import go.putnami.dev/tooling/cli/internal/... — see go.mod.
package documentset
