// Package commands is the container directory for the CLI's command
// verticals, not a package with logic of its own. It holds one subpackage per
// command surface — internal/commands/{agentctx,cachecmd,ci,completion,
// composecmd,configcmd,doctor,extensions,lifecycle,migrate,qualifycmd,
// sessions,treecmd,versioncmd} — plus the two
// packages a vertical may share: internal/commands/shared for production
// helpers, internal/commands/sharedtest for test-only ones (kept separate so
// the "testing" package never reaches cmd/putnami's dependency graph).
//
// What is left directly here is the CLI's public-surface pin —
// surface_golden_test.go and testdata/surface/*.golden, which exercise
// internal/commands/completion's generators in-process (see that test's own
// doc comment) — because moving it changes nothing about what it pins and a
// root package gives the layout above a place to be documented at all.
//
// internal/cli's TestStructuralBaseline_VerticalsStayIsolated
// (vertical_isolation_ratchet_test.go) is what keeps this
// a container rather than the flat package growing back: a vertical may
// import shared, sharedtest, go.putnami.dev/cli/model/*, and CLI support
// packages, but never a sibling vertical, without a written exception.
package commands
