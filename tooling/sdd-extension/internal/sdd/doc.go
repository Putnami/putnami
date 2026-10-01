// Package sdd is the specification-driven-development engine: what `features`,
// `specs`, `architecture` and `contracts` DECIDE, with nothing about how the
// decision is shown.
//
// # Origin
//
// Every file here is a copy of `tooling/cli/internal/commands/sdd`, cut along
// one seam:
//
//	engine   -> here      the report a run produces, and the error it fails with
//	command  -> stays     argument parsing, positional arity, human output,
//	                      ResultV2 envelopes (moved in a later commit)
//
// The seam is `Build<Something>Result`. Core's `<Something>Command` functions
// were "build the report, then render it"; three of the four verticals already
// had the builder split out for the MCP tools, and the ones that did not
// (features validate/snapshot, features diff, the three architecture commands)
// got one here with the same body. So the command layer that arrives later
// calls exactly what the MCP tools call, and neither surface can drift.
//
// Nothing in this package writes to stdout, reads a flag, or knows an exit
// code exists beyond the classified error it returns.
//
// # The dependency swap
//
// Core's engines read a workspace the CLI loader had resolved and a selection
// the CLI had just parsed. An extension gets both from the job context wire:
//
//	internal/workspace       -> internal/wsview   (the wire-derived view)
//	internal/git             -> internal/gitread  (immutable git reads)
//	internal/features        -> internal/features (this module's copy)
//	shared.ResolvedSelection -> Selection         (the wire's `selection` block)
//	shared.ProjectSelection  -> gone              (core parses and resolves it)
//	cmderr                   -> protocol/cli      (cmderr re-exports it)
//	shared.WithResultData     -> resultdata.go     (ported, two functions)
//	shared.AtomicWriteFile    -> contracts.go      (ported, one function)
//	iox                      -> gone              (output is the command layer's)
//
// `shared.ResolveProjectSelection` was deliberately NOT ported. Resolving a
// selector against a workspace is the orchestrator's job, it already happened,
// and its answer is on the wire. A second resolver here would be a second
// selection contract — and a validator that disagreed with the run that
// scheduled it about what was in scope is worse than one that cannot answer.
package sdd
