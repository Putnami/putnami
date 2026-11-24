// Package documentset holds the gates that read a known set of this
// repository's documents: the governance surface, the neutral-contributor
// recipe, the first-use block, the published documentation the CLI cites, the
// project manifests and the environments of putnami.ci.json.
//
// They live apart from the whole-tree gates of @putnami/cli-documents because a
// cache key is per project and per command, never per task. The whole-tree
// gates key on every candidate file and run on main only; these key on the
// documents they read, so a pull request runs them exactly when one of those
// documents changes.
//
// The package carries no production code on purpose: it is a test surface over
// the repository, not a library. Its module path is a child of the CLI's so it
// may import go.putnami.dev/tooling/cli/internal/... — see go.mod.
package documentset
