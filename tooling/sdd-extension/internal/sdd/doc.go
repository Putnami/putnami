// Package sdd is the specification-driven-development engine: what `features`,
// `specs`, `architecture` and `contracts` DECIDE, with nothing about how the
// decision is shown.
//
// Nothing in this package writes to stdout, reads a flag, or knows an exit
// code exists beyond the classified error it returns.
//
// # Inputs
//
// An extension gets the workspace and the selection from the job context wire,
// not from a CLI loader:
//
//	internal/wsview     the wire-derived workspace view
//	internal/gitread    tree, baseline and source reads over the SDK gitread
//	Selection           the wire's `selection` block (core parses and resolves the flags)
//	protocol/cli        errors, WithResultData and ResultData
//	contracts.go        AtomicWriteFile
//
// Output belongs to the command layer, not to this package.
//
// This package never resolves a selector against a workspace. Resolving is the
// orchestrator's job, it already happened, and its answer is on the wire. A
// second resolver here would be a second selection contract — and a validator
// that disagreed with the run that scheduled it about what was in scope is
// worse than one that cannot answer.
package sdd
