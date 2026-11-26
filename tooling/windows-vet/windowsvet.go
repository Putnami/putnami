// Package windowsvet holds the Windows compile gate: it type-checks every
// go.work module, test files included, for windows/amd64 (decision D-W11).
//
// It lives in its own project because a cache key is per project and per
// command, never per task. The gate reads every Go source of the repository,
// and a key over those sources selects the project for every Go change. The
// project declares none: the gate runs on main, without a cache, where
// PUTNAMI_WHOLE_TREE_GATES is set, and skips elsewhere.
//
// The package carries no production code on purpose: it is a test surface over
// the repository, not a library.
package windowsvet
