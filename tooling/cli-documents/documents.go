// Package documents proves, against the CLI's own cache-key collector, that
// the whole-tree checks of @putnami/repository-checks key exactly the files
// their public-cut scanner reads: the candidate cut, tracked files and
// non-ignored additions, with ignored local state left out.
//
// The checks themselves live in @putnami/repository-checks, which imports
// nothing from this repository so that its test task depends on no other
// project. This project imports the CLI's store package to compare that
// project's declaration with the collector the scheduler keys with, so its
// test inputs are the one manifest it reads; its Go imports select it for a
// change to the code it compiles.
//
// The package carries no production code on purpose: it is a test surface, not
// a library. Its module path is a child of the CLI's so it may import
// go.putnami.dev/tooling/cli/internal/... — see go.mod.
package documents
