// Package machine emits the machine contract — the versioned envelope, the
// session stream, the MCP result and the recorded session files
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
// # The retired environment variable
//
// PUTNAMI_MACHINE_OUTPUT selected the contract during the canary window. It is
// INERT: no surface reads it to choose an emitter, and setting it to any value
// is not an error. Because "v1" is the one spelling whose MEANING changed —
// a caller asking for v1 now receives v2 — it warns once on stderr
// (WarnRetiredSelection) instead of silently ignoring the request. Rolling back
// to v1 documents means pinning a CLI build published before v1 was removed
// (`putnami pin 0.1.0-<sha>`), not setting an environment variable.
//
// # What this package is
//
// Everything here is a pure projection of the canonical reduction
// (jobs.ReduceRun / jobs.SessionResult) onto the contract's documents. The four
// surfaces read ONE reduction, which is what makes "the stream and the
// aggregated envelope cannot disagree about a verdict" structural rather than
// tested:
//
//	internal/output/renderer.go   --output=json and --output=jsonl
//	internal/mcp/adapter.go       run_jobs / plan_jobs
//	internal/engine/execute.go    session.json and plan.json
//
// # Session readers
//
// `putnami sessions show` and `putnami sessions inspect` read v2
// session.json/plan.json natively and keep a v1 fallback: sessions recorded on
// disk by an older CLI stay readable (internal/commands/sessions_helpers.go).
// The recording side is v2-only.
package machine

import (
	"io"
	"strings"
	"sync"
)

// RetiredSelectionEnv is the environment variable that used to select the
// machine-contract version. Nothing reads it to choose an emitter any more; see
// the package comment.
const RetiredSelectionEnv = "PUTNAMI_MACHINE_OUTPUT"

// retiredV1Value is the value that used to roll a run back to the v1 machine
// documents. It is the only spelling that warns, because it is the only one
// whose meaning changed when the v1 emitters were deleted.
const retiredV1Value = "v1"

// warnOnce keeps the notice to one line per process: the variable is read once
// per run, and a warning repeated per surface would be three lines of noise on
// a run that is otherwise fine.
var warnOnce sync.Once

// WarnRetiredSelection writes the one-time notice to w when the environment
// requests the deleted v1 contract. Any other value (including the historical
// "v2") is silently ignored: it already selects what it asked for.
//
// It is deliberately a WARNING and not an error. The variable was the canary's
// documented rollback lever, so a CI runner or a script still carrying it must
// keep working — it just no longer gets v1 bytes, and it is told so on stderr
// rather than discovering it by parsing an unexpected document.
func WarnRetiredSelection(w io.Writer, value string) {
	if !requestsRetiredV1(value) {
		return
	}
	warnOnce.Do(func() {
		_, _ = io.WriteString(w,
			"putnami: "+RetiredSelectionEnv+"="+retiredV1Value+" is ignored — the version-1 machine "+
				"output was removed and every machine document now carries \"protocolVersion\": 2.\n"+
				"  To keep reading version-1 documents, pin a CLI build published before the removal "+
				"(putnami pin 0.1.0-<sha>). See protocols/cli/doc/02-result-v2.md.\n")
	})
}

// requestsRetiredV1 is the value rule, split out so it can be tested without
// the process environment. Case and surrounding space are ignored, matching how
// the selection read its values before it was retired.
func requestsRetiredV1(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), retiredV1Value)
}
