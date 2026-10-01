package cli

import "testing"

// TestParentSessionFromEnvReadsTheRunLevelEnvironment covers the three answers
// the reader has to give.
//
// The self-parent case is the one that matters. A run that stamped its own id
// as its parent produces a document ValidateDocument rejects, and a consumer
// that trusted it would drop the top-level run out of its gate ledger — the
// exact miscount the member exists to remove. The reader refuses it at the
// source rather than writing a document the validator will reject later.
//
// Every case sets the variable explicitly, including the empty one: these tests
// run inside a task of a real CLI run, which exports the variable, so an
// implicit "unset" case would read its parent's session id and fail.
func TestParentSessionFromEnvReadsTheRunLevelEnvironment(t *testing.T) {
	own := "20260913-023611-4a5b87"

	t.Setenv(ParentSessionEnv, "")
	if got := ParentSessionFromEnv(own); got != "" {
		t.Errorf("ParentSessionFromEnv with no value = %q, want \"\" (a run a user starts is top-level)", got)
	}

	t.Setenv(ParentSessionEnv, "20260913-020000-parent")
	if got := ParentSessionFromEnv(own); got != "20260913-020000-parent" {
		t.Errorf("ParentSessionFromEnv = %q, want the exported parent id", got)
	}

	t.Setenv(ParentSessionEnv, "  20260913-020000-parent  ")
	if got := ParentSessionFromEnv(own); got != "20260913-020000-parent" {
		t.Errorf("ParentSessionFromEnv = %q, want the value trimmed", got)
	}

	t.Setenv(ParentSessionEnv, own)
	if got := ParentSessionFromEnv(own); got != "" {
		t.Errorf("ParentSessionFromEnv = %q, want \"\" — a session may never be its own parent", got)
	}
}
