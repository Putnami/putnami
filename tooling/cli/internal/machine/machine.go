// Package machine emits the machine contract — the versioned envelope, the
// session stream and the recorded session files
// (protocols/cli, doc/02-result-v2.md).
//
// # There is one contract
//
// Version 2 is the ONLY machine contract the CLI emits. An early release added
// it behind an opt-in, a later release made it the default, and the release
// after that deleted the v1 emitters and the selection point along with them.
// Every document this package produces carries protocolVersion: 2; a document
// without the member is v1 and can now only come from a build published before
// v1 was removed.
//
// # What this package is
//
// Everything here is a pure projection of the canonical reduction
// (jobs.ReduceRun / jobs.SessionResult) onto the contract's documents. The
// surfaces read ONE reduction, which is what makes "the stream and the
// aggregated envelope cannot disagree about a verdict" structural rather than
// tested:
//
//	internal/output/renderer.go   --output=json and --output=jsonl
//	internal/engine/execute.go    session.json and plan.json
package machine
