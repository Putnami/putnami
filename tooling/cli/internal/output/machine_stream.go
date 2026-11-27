package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

const unrecordedMachineSessionID = "unrecorded"

// machineOutputState is the producer-side implementation of the fixed
// protocols/cli budget. It stores only counters: complete records are streamed
// to the session artifact, and admitted live records are written immediately.
// The hard partitions make the decision online without allowing early success
// noise to consume capacity reserved for later failure evidence.
type machineOutputState struct {
	mode   string
	budget protocolcli.MachineOutputBudget

	ordinaryBytes   int64
	ordinaryRecords int
	failureBytes    int64
	failureRecords  int
	elided          protocolcli.MachineOutputElisions
}

func newMachineOutputState(mode string) machineOutputState {
	budget, ok := protocolcli.MachineOutputBudgetFor(mode)
	if !ok {
		mode = protocolcli.MachineOutputModeNormal
		budget, _ = protocolcli.MachineOutputBudgetFor(mode)
	}
	return machineOutputState{
		mode:            mode,
		budget:          budget,
		ordinaryBytes:   budget.MaxBytes - budget.FailureReserveBytes - budget.FinalReserveBytes,
		ordinaryRecords: budget.MaxRecords - budget.FailureReserveRecords - budget.FinalReserveRecords,
		failureBytes:    budget.FailureReserveBytes,
		failureRecords:  budget.FailureReserveRecords,
	}
}

// admit applies the contract's whole-record decision to record, whose
// sanitized compact line is size bytes with its LF. A task's lifecycle is
// always live and spends no partition.
func (s *machineOutputState) admit(record protocolcli.SessionStreamRecord, size int64) bool {
	if protocolcli.IsMachineOutputLifecycle(record) {
		return true
	}
	failure := protocolcli.IsMachineOutputFailurePriority(record)
	debugDetail := protocolcli.IsMachineOutputDebugDetail(record)
	if failure {
		if s.failureRecords > 0 && size <= s.failureBytes {
			s.failureRecords--
			s.failureBytes -= size
			return true
		}
		s.elided.Failure.Records++
		s.elided.Failure.Bytes += size
		return false
	}
	if s.mode == protocolcli.MachineOutputModeNormal && debugDetail {
		s.elided.Ordinary.Records++
		s.elided.Ordinary.Bytes += size
		return false
	}
	if s.ordinaryRecords > 0 && size <= s.ordinaryBytes {
		s.ordinaryRecords--
		s.ordinaryBytes -= size
		return true
	}
	s.elided.Ordinary.Records++
	s.elided.Ordinary.Bytes += size
	return false
}

func (s *machineOutputState) summary(sessionID string) protocolcli.MachineOutputSummary {
	if sessionID == "" {
		sessionID = unrecordedMachineSessionID
	}
	return protocolcli.MachineOutputSummary{
		Mode:         s.mode,
		Sanitization: protocolcli.MachineOutputSanitizationV1,
		Budget:       s.budget,
		Elided:       s.elided,
		Artifact: protocolcli.MachineOutputArtifact{
			SessionID: sessionID,
			Path:      protocolcli.MachineOutputArtifactPath,
			Retention: protocolcli.MachineOutputArtifactRetentionSession,
		},
	}
}

// sanitizedMachineRecord produces compact, terminal-safe JSON. It applies
// sanitization to every current and future string value of the record while
// retaining the original struct/member order and exact number lexemes
// (sanitizeMarshaledJSON).
func sanitizedMachineRecord(record any) ([]byte, error) {
	return sanitizedMachineJSON(record, false)
}

func sanitizedMachineDocument(document any) ([]byte, error) {
	return sanitizedMachineJSON(document, true)
}

func sanitizedMachineJSON(document any, indent bool) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	compact, err := sanitizeMarshaledJSON(raw)
	if err != nil {
		return nil, err
	}
	if indent {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, compact, "", "  "); err != nil {
			return nil, err
		}
		return formatted.Bytes(), nil
	}
	return compact, nil
}

// sanitizedSessionRecord returns the sanitized line of record and what the
// output budget classifies it by (machineOutputState.admit): its record kind
// and, for a task:event, its sanitized event. A task's lifecycle and a test
// case are classified by their kind alone, a constant sanitization leaves as
// it is, so their line is not decoded again and the other members of the
// returned record are left empty.
func sanitizedSessionRecord(record protocolcli.SessionStreamRecord) ([]byte, protocolcli.SessionStreamRecord, error) {
	line, err := sanitizedMachineRecord(record)
	if err != nil {
		return nil, protocolcli.SessionStreamRecord{}, err
	}
	switch record.Record {
	case protocolcli.RecordTaskStart, protocolcli.RecordTaskEnd, protocolcli.RecordTestCase:
		return line, protocolcli.SessionStreamRecord{Record: record.Record}, nil
	}
	var sanitized protocolcli.SessionStreamRecord
	if err := json.Unmarshal(line, &sanitized); err != nil {
		return nil, protocolcli.SessionStreamRecord{}, err
	}
	return line, sanitized, nil
}

// sessionRecordingRenderer records the complete sanitized v2 stream for human,
// cloud, and adapter renderers. machineV2Renderer implements the same recorder
// directly so its artifact and selected live stream share one set of counters
// and an identical final line.
type sessionRecordingRenderer struct {
	base      jobs.Renderer
	session   *workspace_state.Session
	mode      string
	state     machineOutputState
	start     time.Time
	planned   []*jobs.ScheduledJob
	completed map[string]bool
	writeErr  error
	mu        sync.Mutex
}

// WithSessionRecording attaches the complete v2 events.jsonl artifact to a
// renderer. A machine renderer configures its own shared recorder; every other
// renderer is wrapped without changing what it displays.
func WithSessionRecording(base jobs.Renderer, session *workspace_state.Session, verbose bool) jobs.Renderer {
	if base == nil || session == nil {
		return base
	}
	mode := protocolcli.MachineOutputModeNormal
	if verbose {
		mode = protocolcli.MachineOutputModeVerbose
	}
	machineRenderer := base
	if carrier, ok := base.(interface {
		SessionRecordingRenderer() jobs.Renderer
	}); ok {
		machineRenderer = carrier.SessionRecordingRenderer()
	}
	if configurable, ok := machineRenderer.(interface {
		configureMachineSession(string, func([]byte) error, string) bool
	}); ok && configurable.configureMachineSession(session.ID, session.AppendMachineOutputLine, mode) {
		return base
	}
	return &sessionRecordingRenderer{
		base:    base,
		session: session,
		mode:    mode,
		state:   newMachineOutputState(mode),
	}
}

// WithoutSessionArtifact switches a machine renderer to a complete unbounded
// legacy stream when session creation failed before execution. Human renderers
// are unchanged. This is an exceptional storage fallback, not a user-selected
// output mode.
func WithoutSessionArtifact(base jobs.Renderer) jobs.Renderer {
	machineRenderer := base
	if carrier, ok := base.(interface {
		SessionRecordingRenderer() jobs.Renderer
	}); ok {
		machineRenderer = carrier.SessionRecordingRenderer()
	}
	if configurable, ok := machineRenderer.(interface {
		disableMachineArtifact() bool
	}); ok {
		configurable.disableMachineArtifact()
	}
	return base
}

// SessionRecordingError returns a persistence error observed after recording
// began. Renderers cannot change task verdicts, so the engine surfaces this
// separately without changing the run's exit status.
func SessionRecordingError(base jobs.Renderer) error {
	if recorder, ok := base.(interface{ sessionRecordingError() error }); ok {
		return recorder.sessionRecordingError()
	}
	if carrier, ok := base.(interface {
		SessionRecordingRenderer() jobs.Renderer
	}); ok {
		if recorder, ok := carrier.SessionRecordingRenderer().(interface{ sessionRecordingError() error }); ok {
			return recorder.sessionRecordingError()
		}
	}
	return nil
}

func (r *sessionRecordingRenderer) Start(planned []*jobs.ScheduledJob) {
	r.mu.Lock()
	r.start = time.Now()
	r.planned = planned
	r.completed = make(map[string]bool, len(planned))
	r.state = newMachineOutputState(r.mode)
	r.writeErr = nil
	r.mu.Unlock()
	r.base.Start(planned)
}

func (r *sessionRecordingRenderer) JobStart(job *jobs.ScheduledJob) {
	r.record(machine.TaskStart(job, time.Now()))
	r.base.JobStart(job)
}

func (r *sessionRecordingRenderer) JobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	r.record(machine.TaskEvent(job, event, time.Now()))
	r.base.JobEvent(job, event)
}

// BatchJobEvent records the unattributed physical batch stream once. It is not
// sent to a human renderer, which continues to receive the member projections
// produced after the typed batch result is available.
func (r *sessionRecordingRenderer) BatchJobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	r.record(machine.BatchTaskEvent(job, event, time.Now()))
}

// BatchMemberJobEvent displays the attributed projection without recording a
// second copy of facts already retained from the physical batch stream.
func (r *sessionRecordingRenderer) BatchMemberJobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	r.base.JobEvent(job, event)
}

func (r *sessionRecordingRenderer) JobComplete(job *jobs.ScheduledJob, result *jobs.JobResult) {
	task := machine.Task{Identity: machine.Identity(job), Result: jobs.TaskResultOf(job, result)}
	if job != nil {
		r.mu.Lock()
		r.completed[job.Key()] = true
		r.mu.Unlock()
	}
	r.record(machine.TaskEndRecords(task, time.Now())...)
	r.base.JobComplete(job, result)
}

// RecordSessionEvent retains scheduler/session signals that are not task
// terminal records. The renderer already records canonical task:end directly;
// persisting the legacy job:end projection too would duplicate one outcome.
func (r *sessionRecordingRenderer) RecordSessionEvent(record jobs.SessionRecord) {
	if event, ok := machineSessionEvent(record, time.Now()); ok {
		r.record(event)
	}
}

func (r *sessionRecordingRenderer) Finish(results map[string]*jobs.JobResult, outcome jobs.SessionOutcome) {
	run := machine.RunOf(r.planned, results, outcome)
	if record, ok := machine.ReleaseSetResultEvent(run, results, time.Now()); ok {
		r.record(record)
	}
	r.emitMissingTaskEnds(run)
	r.mu.Lock()
	run.Session.Cache = outcome.Cache
	duration := outcome.Duration
	if duration <= 0 {
		duration = time.Since(r.start)
	}
	final := protocolcli.BoundedSessionEndRecord{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Record:          protocolcli.RecordSessionEnd,
		Time:            time.Now().UTC().Format(time.RFC3339Nano),
		Run:             run.StreamSummary(duration.Milliseconds()),
		MachineOutput:   r.state.summary(r.session.ID),
	}
	if line, err := sanitizedMachineRecord(final); err == nil {
		r.append(line)
	} else if r.writeErr == nil {
		r.writeErr = err
	}
	r.mu.Unlock()
	r.base.Finish(results, outcome)
}

func (r *sessionRecordingRenderer) emitMissingTaskEnds(run machine.Run) {
	for _, task := range run.Tasks {
		r.mu.Lock()
		seen := r.completed[task.Identity.Key]
		if !seen {
			r.completed[task.Identity.Key] = true
		}
		r.mu.Unlock()
		if !seen {
			r.record(machine.TaskEndRecords(task, time.Now())...)
		}
	}
}

// record records records as one group: it holds the lock across the group, so
// no record another goroutine records lands between two of them. Records are
// sanitized before the lock is taken: every worker records each task it ends,
// and sanitizing under the lock would make them wait for one another.
func (r *sessionRecordingRenderer) record(records ...protocolcli.SessionStreamRecord) {
	type sanitizedRecord struct {
		line       []byte
		classified protocolcli.SessionStreamRecord
		err        error
	}
	sanitized := make([]sanitizedRecord, len(records))
	for i, record := range records {
		sanitized[i].line, sanitized[i].classified, sanitized[i].err = sanitizedSessionRecord(record)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range sanitized {
		if record.err != nil {
			if r.writeErr == nil {
				r.writeErr = record.err
			}
			continue
		}
		r.state.admit(record.classified, int64(len(record.line)+1))
		r.append(record.line)
	}
}

func (r *sessionRecordingRenderer) append(line []byte) {
	if r.writeErr != nil {
		return
	}
	if err := r.session.AppendMachineOutputLine(line); err != nil {
		r.writeErr = fmt.Errorf("append machine session record: %w", err)
	}
}

func (r *sessionRecordingRenderer) sessionRecordingError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeErr
}

// machineSessionEvent projects session-wide scheduler/recovery signals into an
// ordinary v2 task:event owned by a stable workspace-scoped system identity.
// The session stream has no free-standing event variant, and retaining these
// signals is part of the complete audit trail. job:end is intentionally skipped
// because JobComplete writes the richer canonical task:end record.
func machineSessionEvent(record jobs.SessionRecord, at time.Time) (protocolcli.SessionStreamRecord, bool) {
	if record.Type == "" || record.Type == jobs.SessionRecordJobEnd {
		return protocolcli.SessionStreamRecord{}, false
	}
	identity := protocolcli.TaskIdentity{
		Scope: protocolcli.TaskScopeWorkspace,
		Project: protocolcli.ProjectIdentity{
			ID:   "/workspace",
			Name: "workspace",
		},
		Task: protocolcli.TaskRef{
			Name:    "session-events",
			Command: "session",
			Kind:    "session-event",
		},
		Provider: protocolcli.ProviderIdentity{Extension: "@putnami/cli"},
	}
	identity.Key = identity.DerivedKey()
	event := make(map[string]any, len(record.Data)+1)
	for key, value := range record.Data {
		event[key] = value
	}
	event["type"] = record.Type
	return protocolcli.SessionStreamRecord{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Record:          protocolcli.RecordTaskEvent,
		Time:            at.UTC().Format(time.RFC3339Nano),
		Identity:        &identity,
		Event:           event,
	}, true
}

// errUnexpectedJSON reports bytes json.Marshal does not write.
var errUnexpectedJSON = errors.New("machine output: unexpected JSON")

// sanitizeMarshaledJSON applies sanitization v1 to raw, the compact output of
// json.Marshal, in one pass over its bytes, keeping its member order and
// number lexemes. It writes what a token walk writes when it decodes every
// string, sanitizes it and encodes it again (the tests hold the two to the
// same bytes). A string with no escape and valid UTF-8 encodes again as the
// same bytes, so such a member name, and such a value sanitization leaves as
// it is, are copied as they stand. Any other string is decoded and encoded
// again: json.Marshal writes invalid UTF-8 as an escaped U+FFFD, which encodes
// again as the character itself, and copies the bytes of a json.Marshaler or a
// json.RawMessage, invalid UTF-8 included, as they are. A session record of an
// unchanged run holds hundreds of thousands of strings, nearly all plain
// identifiers, paths and times (protocolcli.IsPlainMachineOutputString): this
// keeps their cost to a byte scan.
func sanitizeMarshaledJSON(raw []byte) ([]byte, error) {
	out := make([]byte, 0, len(raw)+16)
	var open []byte // '{' or '[' of each container not yet closed
	expectName := false
	// A sensitive member's value is sanitized like any other, then replaced
	// once it ends: redactAt is where it starts in out, redactDepth the open
	// containers around it, redactPending set between its name and its colon.
	redactAt, redactDepth := -1, 0
	redactPending := false
	var redaction string
	for i := 0; i < len(raw); {
		switch c := raw[i]; c {
		case '{', '[':
			open = append(open, c)
			expectName = c == '{'
			out = append(out, c)
			i++
			continue
		case '}', ']':
			if len(open) == 0 {
				return nil, errUnexpectedJSON
			}
			open = open[:len(open)-1]
			expectName = false
			out = append(out, c)
			i++
		case ',':
			if len(open) == 0 {
				return nil, errUnexpectedJSON
			}
			expectName = open[len(open)-1] == '{'
			out = append(out, c)
			i++
			continue
		case ':':
			out = append(out, c)
			i++
			if redactPending {
				redactPending = false
				redactAt, redactDepth = len(out), len(open)
			}
			continue
		case '"':
			end, escaped := i+1, false
			for ; end < len(raw) && raw[end] != '"'; end++ {
				if raw[end] == '\\' {
					escaped = true
					end++
				}
			}
			if end >= len(raw) {
				return nil, errUnexpectedJSON
			}
			end++
			encoded := raw[i:end]
			i = end
			if expectName {
				expectName = false
				name := encoded[1 : len(encoded)-1]
				if escaped || !utf8.Valid(name) {
					var decoded string
					if err := json.Unmarshal(encoded, &decoded); err != nil {
						return nil, err
					}
					name = []byte(decoded)
					encoded, _ = json.Marshal(decoded)
				}
				out = append(out, encoded...)
				if replacement, sensitive := protocolcli.MachineOutputMemberRedaction(name); sensitive && redactAt < 0 {
					redactPending, redaction = true, replacement
				}
				continue
			}
			if !escaped && protocolcli.IsPlainMachineOutputString(encoded[1:len(encoded)-1]) {
				out = append(out, encoded...)
				break
			}
			var text string
			if err := json.Unmarshal(encoded, &text); err != nil {
				return nil, err
			}
			if sanitized := protocolcli.SanitizeMachineOutputString(text); escaped || sanitized != text || !utf8.Valid(encoded) {
				encoded, _ = json.Marshal(sanitized)
			}
			out = append(out, encoded...)
		default:
			end := i
			for end < len(raw) && strings.IndexByte(",}]:", raw[end]) < 0 {
				end++
			}
			if end == i {
				return nil, errUnexpectedJSON
			}
			out = append(out, raw[i:end]...)
			i = end
		}
		if redactAt >= 0 && len(open) == redactDepth {
			encoded, _ := json.Marshal(redaction)
			out = append(out[:redactAt], encoded...)
			redactAt = -1
		}
	}
	if len(open) != 0 || redactAt >= 0 || redactPending {
		return nil, errUnexpectedJSON
	}
	return out, nil
}
