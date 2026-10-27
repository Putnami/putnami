// Package windowsvet holds the Windows compile gate: it type-checks every
// go.work module, test files included, for windows/amd64 (decision D-W11).
//
// It lives in its own project because a cache key is per project and per
// command, never per task. The gate reads every Go source of the repository,
// so its key holds those sources, the go.mod and go.sum files and go.work. A
// change that touches none of them reuses the verdict.
//
// The package carries no production code on purpose: it is a test surface over
// the repository, not a library.
package windowsvet
