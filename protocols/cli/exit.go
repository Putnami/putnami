// Package cli defines the canonical Putnami CLI contract shared by the
// framework CLI, the extension SDK, and (in time) cloud's cli-core: the
// process exit-code taxonomy, output modes, the structured result envelope,
// and the error classification that maps failures to exit codes.
//
// It is the single source of truth for behavior that each CLI surface used to
// re-implement independently (see the protocols/cli normalization epic).
// Consumers whose own package is named "cli" import it aliased as protocolcli.
package cli

// Process exit codes. This taxonomy is a stable contract: scripts, CI, and
// agents branch on these values, so their meaning must not drift.
//
//	0    Success  the command completed successfully
//	1    Failure  a job/build/test failed, or an unexpected internal error
//	2    Usage    invalid invocation: bad flags or arguments, an unknown
//	              command, a selector that matched nothing, or a contract /
//	              config validation error
//	3    Auth     authentication or authorization failed
//	4    API      a remote or upstream API call (registry, cloud) failed
//	130  Signal   the process was interrupted by a signal (SIGINT/SIGTERM)
const (
	ExitSuccess = 0
	ExitFailure = 1
	ExitUsage   = 2
	ExitAuth    = 3
	ExitAPI     = 4
	ExitSignal  = 130
)
