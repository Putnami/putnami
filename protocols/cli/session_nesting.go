package cli

import (
	"os"
	"strings"
)

// ParentSessionEnv names the environment variable a running CLI exports into
// every task subprocess it spawns, carrying the id of the session that run
// recorded. A CLI that finds it set records it as SessionFile.ParentSessionID:
// the run it belongs to was spawned by a task of the named session, and is
// therefore NESTED rather than top-level.
//
// The variable is execution-only, and that is a correctness property, not a
// convention. A session id is unique per run, so anything derived from it that
// reached a cache key, a run marker or a task parameter would make every task
// in the workspace miss on every run, forever. The producer therefore delivers
// it through the run-level process environment the scheduler owns — the channel
// documented as reaching cmd.Env and nothing else — never through the job
// context document, whose members are hashed, and never through task params.
//
// It is also scoped to the task environment on purpose. It is never exported
// into a user's shell, so a run the user starts by hand is top-level, which is
// what it is.
const ParentSessionEnv = "PUTNAMI_PARENT_SESSION"

// ParentSessionFromEnv reads the id of the session that spawned the current run
// from the process environment, given the id the current run recorded for
// itself. It returns "" for a top-level run: the variable is never exported
// into a user's shell, so a run the user started by hand has no parent.
//
// A value equal to ownID is dropped rather than recorded. It cannot arise from
// a real nesting relation — a session id carries a start second and a random
// suffix — so it only ever means the ambient value leaked into a run that then
// stamped it beside its own id. The contract rejects a self-parent
// (ValidateDocument reports cli.result.invalid_key at parentSessionId), because
// a consumer that trusted one would drop the top-level run out of its ledger;
// refusing it here keeps a producer from writing the document at all.
func ParentSessionFromEnv(ownID string) string {
	parent := strings.TrimSpace(os.Getenv(ParentSessionEnv))
	if parent == "" || parent == ownID {
		return ""
	}
	return parent
}
