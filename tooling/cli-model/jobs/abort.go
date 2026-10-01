package jobs

// The abort-source vocabulary. It is declared here, beside SessionOutcome,
// because it is what SessionOutcome.AbortedBy carries and what every renderer
// and session record reports. internal/abort keeps the signal-flag machinery
// that RECORDS the source and re-exports these names unchanged.

// Sources reported by Source.
const (
	// SourceUser is an interactive Ctrl-C (SIGINT).
	SourceUser = "user"
	// SourceSignal is a termination request from a supervisor (SIGTERM).
	SourceSignal = "signal"
)

// Abort sources reported in SessionOutcome.AbortedBy.
const (
	AbortUser   = SourceUser
	AbortSignal = SourceSignal
)
