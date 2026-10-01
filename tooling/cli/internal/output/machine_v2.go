package output

import (
	"io"
	"sync"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/machine"
)

// machineV2Renderer emits the machine documents for --output=json and
// --output=jsonl. It is the ONLY renderer either format selects: the v1
// JSONRenderer/JSONLRenderer and the branch that chose between them are gone,
// which is why nothing here is conditional on a contract version.
//
// It was written as a whole renderer rather than a branch inside the v1 pair on
// purpose, and that is what made the deletion a file removal instead of an
// unpicking of conditionals from inside a shipped wire.
//
// Both modes are one type because both render the SAME documents from the same
// projection — the aggregated envelope is the stream's session:end verdict with
// the invocation stamped on it. Splitting them would recreate the "two surfaces
// that disagree about one run" v2 exists to end.
type machineV2Renderer struct {
	out io.Writer
	// command is the invoked command path, recorded in the envelope.
	command string
	// stream selects --output=jsonl (one record per line) over --output=json
	// (one aggregated object at Finish).
	stream bool

	start     time.Time
	planned   []*jobs.ScheduledJob
	completed map[string]bool
	mu        sync.Mutex

	mode           string
	state          machineOutputState
	bounded        bool
	sessionID      string
	appendArtifact func([]byte) error
	artifactErr    error

	// publishConfiguredCap comes from the scheduler's resolved policy (or the
	// user's explicit --max-parallel value). activeDockerPushes and
	// maxDockerPushes are driven from live phase events, so cached replay cannot
	// masquerade as concurrent registry work.
	publishConfiguredCap int
	activeDockerPushes   map[string]bool
	maxDockerPushes      int
}

// newMachineV2Renderer creates the v2 renderer. stream selects the JSONL
// session stream; otherwise the aggregated JSON envelope is written at Finish.
func newMachineV2Renderer(out io.Writer, command string, stream bool) *machineV2Renderer {
	return newMachineV2RendererMode(out, command, stream, protocolcli.MachineOutputModeNormal)
}

func newMachineV2RendererMode(out io.Writer, command string, stream bool, mode string) *machineV2Renderer {
	return &machineV2Renderer{
		out:       out,
		command:   command,
		stream:    stream,
		mode:      mode,
		state:     newMachineOutputState(mode),
		bounded:   true,
		sessionID: unrecordedMachineSessionID,
	}
}

// WritePreviewPlan writes the v2 plan-only document selected by an explicit
// JSON output mode. It returns false for human/cloud modes so the caller can
// retain its existing presentation. Neither document contains a run or a
// machineOutput artifact: planning succeeded, but no session executed.
func WritePreviewPlan(w io.Writer, mode, command string, planned []*jobs.ScheduledJob) (bool, error) {
	plan := machine.PlanSummary(planned)
	var (
		document []byte
		err      error
	)
	switch protocolcli.OutputMode(mode) {
	case protocolcli.OutputJSON:
		document, err = sanitizedMachineDocument(protocolcli.ResultV2{
			ProtocolVersion: protocolcli.ResultProtocolVersion,
			Command:         command,
			Status:          protocolcli.StatusSuccess,
			ExitCode:        protocolcli.ExitSuccess,
			Plan:            &plan,
		})
	case protocolcli.OutputJSONL:
		document, err = sanitizedMachineRecord(protocolcli.SessionStreamRecord{
			ProtocolVersion: protocolcli.ResultProtocolVersion,
			Record:          protocolcli.RecordPlanEnd,
			Time:            time.Now().UTC().Format(time.RFC3339Nano),
			Plan:            &plan,
		})
	default:
		return false, nil
	}
	if err != nil {
		return true, err
	}
	iox.Fprintf(w, "%s\n", document)
	return true, nil
}

// configureMachineSession makes the complete artifact and the bounded live
// stream two outputs of this renderer's one sanitized record path. It returns
// true so WithSessionRecording knows no wrapper (and therefore no second set of
// counters or timestamps) is needed.
func (r *machineV2Renderer) configureMachineSession(sessionID string, appendLine func([]byte) error, mode string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionID = sessionID
	r.appendArtifact = appendLine
	r.bounded = true
	r.mode = mode
	r.state = newMachineOutputState(mode)
	return true
}

// disableMachineArtifact selects the complete legacy stream fallback when the
// engine could not create an artifact before execution. It keeps task/run
// semantics and exit status unchanged while avoiding a false artifact claim.
func (r *machineV2Renderer) disableMachineArtifact() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bounded = false
	r.sessionID = ""
	r.appendArtifact = nil
	r.artifactErr = nil
	return true
}

// Start records the run's start and the plan. The plan is kept because it is
// what names every task: v2 carries a typed identity, and the identity of a
// result the renderer was never told about can only come from the plan node.
func (r *machineV2Renderer) Start(planned []*jobs.ScheduledJob) {
	r.mu.Lock()
	r.start = time.Now()
	r.planned = planned
	r.completed = make(map[string]bool, len(planned))
	r.state = newMachineOutputState(r.mode)
	r.artifactErr = nil
	r.activeDockerPushes = make(map[string]bool)
	r.maxDockerPushes = 0
	r.mu.Unlock()
}

// SetPublishConcurrency receives the scheduler's configured cap before work
// begins. It is an optional jobs.Renderer capability rather than part of the
// base renderer interface because text/live output does not need it.
func (r *machineV2Renderer) SetPublishConcurrency(configuredCap int) {
	r.mu.Lock()
	r.publishConfiguredCap = configuredCap
	r.mu.Unlock()
}

// JobStart emits task:start. The aggregated envelope reports nothing before
// Finish, so its stdout stays exactly one JSON object.
func (r *machineV2Renderer) JobStart(job *jobs.ScheduledJob) {
	r.emitRecord(machine.TaskStart(job, time.Now()))
}

// JobEvent forwards one runtime event as task:event, with the event payload
// carried opaquely (protocols/runtime owns it).
func (r *machineV2Renderer) JobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	if job != nil && event.Type == jobs.EventTypePhase && event.Data["name"] == "docker-push" {
		action, _ := event.Data["action"].(string)
		if action == "start" || action == "end" {
			key := job.Key()
			r.mu.Lock()
			if r.activeDockerPushes == nil {
				r.activeDockerPushes = make(map[string]bool)
			}
			if action == "start" && !r.activeDockerPushes[key] {
				r.activeDockerPushes[key] = true
				if active := len(r.activeDockerPushes); active > r.maxDockerPushes {
					r.maxDockerPushes = active
				}
			}
			if action == "end" {
				delete(r.activeDockerPushes, key)
			}
			r.mu.Unlock()
		}
	}
	r.emitRecord(machine.TaskEvent(job, event, time.Now()))
}

// BatchJobEvent emits the physical batch stream once under its truthful
// workspace identity. The callback runs before per-task retention.
func (r *machineV2Renderer) BatchJobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	r.emitRecord(machine.BatchTaskEvent(job, event, time.Now()))
}

// BatchMemberJobEvent intentionally emits nothing: those events are a display
// projection of the raw batch stream already emitted above. Task terminals
// still carry each member's attributed structured result.
func (r *machineV2Renderer) BatchMemberJobEvent(_ *jobs.ScheduledJob, _ jobs.RawJobEvent) {}

// JobComplete emits task:end with the task's terminal record.
//
// The projection is TaskResultOf, WITH the structured records. The contract
// says task:end carries the task's diagnostics (protocols/cli/doc/02-result-v2.md,
// doc/08-output-and-rendering.md § Record fields), and session.json's record for
// the same task carries them because RunTasks projects with TaskResultOf — so
// the counting-only TaskSummaryOf here made the streamed record disagree with
// the file describing the same run, and a jsonl consumer lost every diagnostic
// the run produced. The cost is nil where it would matter: a scheduler-produced
// result already carries its canonical task (JobResult.Canonical), so this is a
// copy, and the event walk happens at most once per completion, for results
// assembled by hand.
func (r *machineV2Renderer) JobComplete(job *jobs.ScheduledJob, result *jobs.JobResult) {
	if job != nil {
		r.mu.Lock()
		delete(r.activeDockerPushes, job.Key())
		r.completed[job.Key()] = true
		r.mu.Unlock()
	}
	task := machine.Task{
		Identity: machine.Identity(job),
		Result:   jobs.TaskResultOf(job, result),
	}
	r.emitRecord(machine.TaskEndRecords(task, time.Now())...)
}

// RecordSessionEvent retains non-task scheduler/recovery audit signals through
// the same artifact/live path as task records.
func (r *machineV2Renderer) RecordSessionEvent(record jobs.SessionRecord) {
	if event, ok := machineSessionEvent(record, time.Now()); ok {
		r.emitRecord(event)
	}
}

// Finish closes the run: session:end for the stream, the aggregated envelope
// otherwise. Both read one reduction of the same results, so the streamed
// verdict and the aggregated one cannot disagree.
func (r *machineV2Renderer) Finish(results map[string]*jobs.JobResult, outcome jobs.SessionOutcome) {
	r.mu.Lock()
	planned := r.planned
	start := r.start
	configuredCap := r.publishConfiguredCap
	effective := r.maxDockerPushes
	r.mu.Unlock()
	run := machine.RunOf(planned, results, outcome).WithPublishConcurrency(configuredCap, effective)
	run.Session.Cache = outcome.Cache
	if record, ok := machine.ReleaseSetResultEvent(run, results, time.Now()); ok {
		r.emitRecord(record)
	}
	r.emitMissingTaskEnds(run)
	duration := outcome.Duration
	if duration <= 0 {
		duration = time.Since(start)
	}
	durationMs := duration.Milliseconds()
	r.mu.Lock()
	bounded := r.bounded && r.artifactErr == nil
	if !bounded {
		r.bounded = false
		if r.stream {
			r.writeLegacyFinalLocked(run, durationMs)
		}
		r.mu.Unlock()
		if r.stream {
			return
		}
		document, err := sanitizedMachineDocument(run.Envelope(r.command, durationMs))
		if err == nil {
			r.write(document)
		}
		return
	}
	final := protocolcli.BoundedSessionEndRecord{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Record:          protocolcli.RecordSessionEnd,
		Time:            time.Now().UTC().Format(time.RFC3339Nano),
		Run:             run.StreamSummary(durationMs),
		MachineOutput:   r.state.summary(r.sessionID),
	}
	line, err := sanitizedMachineRecord(final)
	if err == nil {
		r.appendArtifactLocked(line)
		if r.artifactErr != nil {
			r.bounded = false
			if r.stream {
				r.writeLegacyFinalLocked(run, durationMs)
			}
		} else if r.stream {
			r.writeLocked(line)
		}
	}
	r.mu.Unlock()
	if r.stream {
		return
	}
	document, err := sanitizedMachineDocument(run.Envelope(r.command, durationMs))
	if err == nil {
		r.write(document)
	}
}

func (r *machineV2Renderer) writeLegacyFinalLocked(run machine.Run, durationMs int64) {
	summary := run.Summary(durationMs)
	final := protocolcli.SessionStreamRecord{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Record:          protocolcli.RecordSessionEnd,
		Time:            time.Now().UTC().Format(time.RFC3339Nano),
		Run:             &summary,
	}
	if line, err := sanitizedMachineRecord(final); err == nil {
		r.writeLocked(line)
	}
}

func (r *machineV2Renderer) emitMissingTaskEnds(run machine.Run) {
	for _, task := range run.Tasks {
		r.mu.Lock()
		seen := r.completed[task.Identity.Key]
		if !seen {
			r.completed[task.Identity.Key] = true
		}
		r.mu.Unlock()
		if !seen {
			r.emitRecord(machine.TaskEndRecords(task, time.Now())...)
		}
	}
}

// emitRecord emits records as one group: it holds the lock across the group,
// so no record another goroutine emits lands between two of them. A record
// that does not sanitize is dropped.
func (r *machineV2Renderer) emitRecord(records ...protocolcli.SessionStreamRecord) {
	lines := make([][]byte, 0, len(records))
	sanitized := make([]protocolcli.SessionStreamRecord, 0, len(records))
	for _, record := range records {
		line, clean, err := sanitizedSessionRecord(record)
		if err != nil {
			continue
		}
		lines = append(lines, line)
		sanitized = append(sanitized, clean)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, line := range lines {
		if !r.bounded {
			if r.stream {
				r.writeLocked(line)
			}
			continue
		}
		r.appendArtifactLocked(line)
		if r.artifactErr != nil {
			r.bounded = false
			if r.stream {
				r.writeLocked(line)
			}
			continue
		}
		admitted := r.state.admit(
			protocolcli.IsMachineOutputFailurePriority(sanitized[i]),
			protocolcli.IsMachineOutputDebugDetail(sanitized[i]),
			int64(len(line)+1),
		)
		if r.stream && admitted {
			r.writeLocked(line)
		}
	}
}

func (r *machineV2Renderer) sessionRecordingError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.artifactErr
}

func (r *machineV2Renderer) appendArtifactLocked(line []byte) {
	if r.appendArtifact == nil || r.artifactErr != nil {
		return
	}
	if err := r.appendArtifact(line); err != nil {
		r.artifactErr = err
	}
}

func (r *machineV2Renderer) write(document []byte) {
	r.mu.Lock()
	r.writeLocked(document)
	r.mu.Unlock()
}

func (r *machineV2Renderer) writeLocked(document []byte) {
	iox.Fprintf(r.out, "%s\n", document)
}
