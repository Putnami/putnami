package compose

import (
	"strings"
)

// Composition phases a failure is attributed to. Every Error names one, so a
// reader can tell a workload that never started from one that never answered.
const (
	PhasePlan      = "plan"
	PhasePrepare   = "prepare"
	PhaseDatabases = "databases"
	PhaseProxies   = "proxies"
	PhaseStart     = "start"
	PhaseReadiness = "readiness"
	PhaseTeardown  = "teardown"
)

// Error codes. Stable: a caller branches on them, and the message around them
// is free to change.
const (
	CodeNotServeable       = "compose.not_serveable"
	CodeNoServeCommand     = "compose.no_serve_command"
	CodeServeDisabled      = "compose.serve_disabled"
	CodeUnknownMember      = "compose.unknown_member"
	CodeCycle              = "compose.cycle"
	CodeInvalidRequirement = "compose.invalid_requirements"
	CodeConfigDataConflict = "compose.config_data_conflict"
	CodePrepareFailed      = "compose.prepare_failed"
	CodeDatabaseFailed     = "compose.database_failed"
	CodeProxyFailed        = "compose.proxy_failed"
	CodeStartFailed        = "compose.start_failed"
	CodeMemberExited       = "compose.member_exited"
	CodeReadyTimeout       = "compose.ready_timeout"
	CodeLeaseFailed        = "compose.lease_failed"
)

// maxDetailLines bounds the workload output an Error carries.
const maxDetailLines = 20

// Error is a composition failure: a stable code, the member it concerns (a
// project id, empty when it concerns the whole composition), the phase it
// happened in, and at most the last twenty lines of that member's output.
//
// It never carries configuration: the CONFIG_DATA document, connection strings
// and passwords are not reachable from any field. Detail is the workload's own
// output, which may repeat what the workload was given (a failed connection
// logged with its connection string), so Error() leaves it out: a verdict, a
// structured failure document or a log that records the error records its
// code, member, phase and message only. A caller rendering for a person writes
// Detail to stderr itself.
type Error struct {
	Code string
	// ID is the composition's identity when the failure happened after its
	// lease was created, so a caller can name the composition (and find its
	// lease) even though Up returned no Composition. Empty before that.
	ID      string
	Member  string
	Phase   string
	Message string
	Detail  []string
	// Reaped are the orphaned compositions released before this failure: the
	// reap happens first, so a failed start still reports it.
	Reaped []ReapedLease
	// Cleanup is the teardown report of what the failed start had already
	// acquired (lease, databases, proxies, members). Nil when Up failed before
	// acquiring anything, which leaves nothing to release.
	Cleanup *CleanupReport
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	b.WriteString(": ")
	if e.Member != "" {
		b.WriteString(e.Member)
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	b.WriteString(" (phase ")
	b.WriteString(e.Phase)
	b.WriteString(")")
	return b.String()
}

func newError(code, member, phase, message string) *Error {
	return &Error{Code: code, Member: member, Phase: phase, Message: message}
}

// tailLines keeps the last maxDetailLines lines of a member's output.
type tailLines struct {
	lines []string
}

func (t *tailLines) add(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > maxDetailLines {
		t.lines = append([]string(nil), t.lines[len(t.lines)-maxDetailLines:]...)
	}
}

func (t *tailLines) snapshot() []string {
	return append([]string(nil), t.lines...)
}
