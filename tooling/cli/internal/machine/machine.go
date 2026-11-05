// Package machine emits the machine contract — the versioned envelope, the
// session stream and the recorded session files
// (protocols/cli, doc/02-result-v2.md).
//
// # There is one contract
//
// Version 2 is the ONLY machine contract the CLI emits. Every document this
// package produces carries protocolVersion: 2; a document without the member is
// not a v2 document.
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
