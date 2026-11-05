// Package sdd is the specification-driven-development engine: what `features`,
// `specs`, `architecture` and `contracts` DECIDE, with nothing about how the
// decision is shown.
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
//	internal/git             -> internal/gitread  (tree, baseline and source reads over the SDK gitread)
//	internal/features        -> internal/features (this module's copy)
//	shared.ResolvedSelection -> Selection         (the wire's `selection` block)
//	shared.ProjectSelection  -> gone              (core parses and resolves it)
//	cmderr                   -> protocol/cli      (cmderr re-exports it)
//	shared.WithResultData     -> protocol/cli      (WithResultData, ResultData)
//	shared.AtomicWriteFile    -> contracts.go      (ported, one function)
//	iox                      -> gone              (output is the command layer's)
//
// `shared.ResolveProjectSelection` was deliberately NOT ported. Resolving a
// selector against a workspace is the orchestrator's job, it already happened,
// and its answer is on the wire. A second resolver here would be a second
// selection contract — and a validator that disagreed with the run that
// scheduled it about what was in scope is worse than one that cannot answer.
package sdd
