// Package documents holds the gates that read this repository's whole
// candidate tree: the public-cut scan, and the release rehearsal with its
// release plan and recorded verdict. The gates that read a known document set
// live in tooling/cli-document-set, which keys on those documents alone.
//
// They live in their own project because a cache key is per project and per
// command, never per task, and only a whole-tree input keys what they read.
// Such an input selects its project for every change of the repository, so
// this project declares none: the two whole-tree gates run on main, without a
// cache, where PUTNAMI_WHOLE_TREE_GATES is set, and skip elsewhere. The tests
// that always run key on the project and on the documents its manifest names.
//
// The package carries no production code on purpose: it is a test surface over
// the repository, not a library. Its module path is a child of the CLI's so it
// may import go.putnami.dev/tooling/cli/internal/... — see go.mod.
package documents
