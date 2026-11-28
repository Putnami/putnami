package sessionreporter

import (
	"slices"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	features "go.putnami.dev/protocol/features"
)

// sessionFile is the finalized session document a capability may transmit
// beside the event stream.
const sessionFile = "session.json"

// planFile is the recorded plan a capability may transmit before the event
// stream. The engine writes it whole before the capability starts.
const planFile = "plan.json"

// specVerificationFile is the spec gate's record a capability may transmit
// after the event stream. The engine writes it whole before Finish.
const specVerificationFile = features.SpecVerificationRecordFilename

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
	// View: its events.jsonl frames carry the trimmed view of the stream, not
	// its raw bytes, from checkpoint version 2 on (ADR 0083).
	View bool
	// LargeFrames: the engine names framing version 3 in its provider's
	// environment and cuts frames at the cap the provider acknowledges
	// (ADR 0131).
	LargeFrames bool
	// Packages: the engine names framing version 4 in its provider's
	// environment, and the session switches to packages at the first
	// acknowledgement that offers them before any events.jsonl or
	// session.json frame left (ADR 0135).
	Packages bool
}

// SessionReporter delivers the recorded plan, the trimmed view of the event
// stream, the finalized session and the spec gate's record.
var SessionReporter = Capability{
	Name: protocolcli.SessionReporterCommand, Label: "session reporter", Activity: "session reporting",
	SelectorEnv: protocolcli.SessionReporterEnv, TokenEnv: protocolcli.SessionReporterTokenEnv,
	Artifacts:     protocolcli.SessionReportingArtifacts(protocolcli.SessionReporterCommand),
	BatchInterval: EventsBatchInterval, StateFile: StateFile, LockFile: "reporting.lock",
	View: true, LargeFrames: true, Packages: true,
}

// LogReporter delivers the raw event stream only.
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
// session.json closes it before the events final marker; one that sends
// spec-verification.json frames it once that marker is acknowledged.
func (c Capability) sends(artifact string) bool {
	return slices.Contains(c.Artifacts, artifact)
}

// The checkpoint versions. rawCheckpointVersion frames events.jsonl from its
// raw bytes; viewCheckpointVersion, for a View capability only, from the view,
// and records beside the view cursor the raw offset the subscription
// acknowledges. A CLI that predates the view refuses version 2.
// packagesCheckpointVersion, for a Packages capability only, is a session that
// switched to packages (ADR 0135): it frames no events.jsonl, session.json or
// spec-verification.json, records the packages acknowledged and the raw offset
// the subscription acknowledges, and a CLI that predates packages refuses it
// rather than frame a stream its receiver holds as packages.
//
// The three others are those of a session that records reused tasks by
// reference to their cache entry (ADR 0137): their framing puts back, before
// a task:end that names an entry, the records the entry holds.
// resolvedViewCheckpointVersion and resolvedPackagesCheckpointVersion are the
// view and the packages of such a session; resolvedRawCheckpointVersion frames
// every record of its stream, the put-back ones included, from a view that
// trims nothing, for a capability that frames raw bytes otherwise. A CLI that
// predates references refuses all three, and a session that records by
// reference is never framed raw.
const (
	rawCheckpointVersion              = 1
	viewCheckpointVersion             = 2
	packagesCheckpointVersion         = 3
	resolvedViewCheckpointVersion     = 4
	resolvedPackagesCheckpointVersion = 5
	resolvedRawCheckpointVersion      = 6
)

// checkpointVersion is the version of a new checkpoint of the capability for
// a session that holds every record.
func (c Capability) checkpointVersion() int {
	return c.checkpointVersionOf(false)
}

// checkpointVersionOf is the version of a new checkpoint of the capability,
// for a session that records reused tasks by reference when references is
// set.
func (c Capability) checkpointVersionOf(references bool) int {
	switch {
	case c.View && references:
		return resolvedViewCheckpointVersion
	case c.View:
		return viewCheckpointVersion
	case references:
		return resolvedRawCheckpointVersion
	}
	return rawCheckpointVersion
}

// framesView reports whether a checkpoint of version frames the trimmed view.
func framesView(version int) bool {
	return version == viewCheckpointVersion || version == resolvedViewCheckpointVersion
}

// sendsPackages reports whether a checkpoint of version is on packages.
func sendsPackages(version int) bool {
	return version == packagesCheckpointVersion || version == resolvedPackagesCheckpointVersion
}

// resolvesReferences reports whether a checkpoint of version puts back the
// records a task:end's cache reference names.
func resolvesReferences(version int) bool {
	return version == resolvedViewCheckpointVersion || version == resolvedPackagesCheckpointVersion || version == resolvedRawCheckpointVersion
}
