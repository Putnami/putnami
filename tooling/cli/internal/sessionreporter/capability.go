package sessionreporter

import (
	"slices"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
)

// sessionFile is the finalized session document a capability may transmit
// beside the event stream.
const sessionFile = "session.json"

// planFile is the recorded plan a capability may transmit before the event
// stream. The engine writes it whole before the capability starts.
const planFile = "plan.json"

// LogStateFile is the log reporter's checkpoint beside the session. StateFile
// is the session reporter's.
const LogStateFile = "log-reporting.json"

// LogEventsBatchInterval is the longest a live log reporter chunk shorter than
// one frame waits before it leaves.
const LogEventsBatchInterval = 2 * time.Second

// Capability is one native reporting capability: a declared subscriber of the
// session event stream, served by the extension its selector names through a
// reserved command, with its own credential, checkpoint and batching.
type Capability struct {
	// Name is the subscriber name its subscribers.json entry carries. It is also
	// the reserved command the serving extension declares.
	Name string
	// Label names the capability in diagnostics ("session reporter").
	Label string
	// Activity names its delivery in diagnostics ("session reporting").
	Activity string
	// SelectorEnv names the serving extension. Unset disables the capability.
	SelectorEnv string
	// TokenEnv is an optional explicit credential for its provider only.
	TokenEnv string
	// Artifacts are the session artifacts it transmits, in the order they close.
	Artifacts []string
	// BatchInterval is the longest a live events chunk shorter than one frame
	// waits after the previous events chunk.
	BatchInterval time.Duration
	// StateFile and LockFile are its checkpoint and its advisory lock beside the
	// session. Session retention knows both names.
	StateFile, LockFile string
}

// SessionReporter delivers the recorded plan, the event stream and the
// finalized session.
var SessionReporter = Capability{
	Name: protocolcli.SessionReporterCommand, Label: "session reporter", Activity: "session reporting",
	SelectorEnv: protocolcli.SessionReporterEnv, TokenEnv: protocolcli.SessionReporterTokenEnv,
	Artifacts:     protocolcli.SessionReportingArtifacts(protocolcli.SessionReporterCommand),
	BatchInterval: EventsBatchInterval, StateFile: StateFile, LockFile: "reporting.lock",
}

// LogReporter delivers the event stream only.
var LogReporter = Capability{
	Name: protocolcli.LogReporterCommand, Label: "log reporter", Activity: "log reporting",
	SelectorEnv: protocolcli.LogReporterEnv, TokenEnv: protocolcli.LogReporterTokenEnv,
	Artifacts:     protocolcli.SessionReportingArtifacts(protocolcli.LogReporterCommand),
	BatchInterval: LogEventsBatchInterval, StateFile: LogStateFile, LockFile: "log-reporting.lock",
}

// Capabilities is every reporting capability, in the order the engine starts
// them and reports on them. Each one is independent: selecting, starting,
// failing or replaying one never changes another.
func Capabilities() []Capability {
	return []Capability{SessionReporter, LogReporter}
}

// sends reports whether the capability transmits artifact. A capability that
// sends plan.json closes it before any other frame leaves; one that sends
// session.json closes it before the events final marker.
func (c Capability) sends(artifact string) bool {
	return slices.Contains(c.Artifacts, artifact)
}
