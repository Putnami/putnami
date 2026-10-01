package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// CloudLoggingRenderer outputs Google Cloud Logging structured JSON.
type CloudLoggingRenderer struct {
	out     io.Writer
	start   time.Time
	planned []*jobs.ScheduledJob
	mu      sync.Mutex
}

// NewCloudLoggingRenderer creates a cloud logging renderer.
func NewCloudLoggingRenderer(out io.Writer) *CloudLoggingRenderer {
	return &CloudLoggingRenderer{out: out}
}

// Start is called when job execution begins.
func (r *CloudLoggingRenderer) Start(planned []*jobs.ScheduledJob) {
	r.start = time.Now()
	r.planned = planned
}

// JobStart emits a structured log entry for job start.
func (r *CloudLoggingRenderer) JobStart(job *jobs.ScheduledJob) {
	r.emit("INFO", fmt.Sprintf("Starting %s %s", job.Project.Name, job.JobDef.Name), map[string]any{
		"package": job.Project.Name,
		"job":     job.JobDef.Name,
	})
}

// JobEvent maps event types to appropriate severity levels.
func (r *CloudLoggingRenderer) JobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	severity := eventSeverity(event)
	msg := fmt.Sprintf("[%s] %s", event.Type, event.Message)

	labels := map[string]any{
		"package": job.Project.Name,
		"job":     job.JobDef.Name,
		"type":    event.Type,
	}
	if event.Data != nil {
		for k, v := range event.Data {
			labels[k] = v
		}
	}

	r.emit(severity, msg, labels)
}

// JobComplete emits a structured log entry for job completion.
func (r *CloudLoggingRenderer) JobComplete(job *jobs.ScheduledJob, result *jobs.JobResult) {
	severity := "INFO"
	if result.Status == "failed" {
		severity = "ERROR"
	}

	status := result.Outcome()

	msg := fmt.Sprintf("%s %s %s (%s)", job.Project.Name, job.JobDef.Name, status, formatDuration(result.Duration))
	labels := map[string]any{
		"package":   job.Project.Name,
		"job":       job.JobDef.Name,
		"status":    status,
		"duration":  result.Duration.Milliseconds(),
		"cache":     result.CacheHit,
		"coalesced": result.Coalesced,
	}
	if result.Error != nil {
		labels["error"] = result.Error.Message
	}

	r.emit(severity, msg, labels)
}

// Finish emits the session summary.
func (r *CloudLoggingRenderer) Finish(results map[string]*jobs.JobResult, outcome jobs.SessionOutcome) {
	session := jobs.ReduceRun(r.planned, results, outcome)
	succeeded, failed := session.Fresh.Succeeded, session.Fresh.Failed
	canceled, skipped := session.Fresh.Canceled, session.Fresh.Skipped
	cached, coalesced := session.Cached(), session.Reuse.Coalesced
	total := session.BucketTotal()

	severity := "INFO"
	message := fmt.Sprintf("Session complete: %d/%d succeeded", succeeded, total)
	if failed > 0 {
		severity = "ERROR"
	}
	labels := map[string]any{
		"succeeded": succeeded,
		"failed":    failed,
		"canceled":  canceled,
		"skipped":   skipped,
		"cached":    cached,
		"coalesced": coalesced,
		"total":     total,
		"duration":  time.Since(r.start).Milliseconds(),
	}
	if outcome.Aborted {
		// A killed run is not an INFO-level "complete". Log aggregators bucket
		// on severity, so an abort has to leave the success bucket entirely.
		if severity == "INFO" {
			severity = "WARNING"
		}
		message = fmt.Sprintf("Session %s: %d/%d succeeded before the run stopped",
			abortDescription(outcome), succeeded, total)
		labels["aborted"] = true
		if outcome.AbortedBy != "" {
			labels["abortedBy"] = outcome.AbortedBy
		}
	}

	r.emit(severity, message, labels)
}

func (r *CloudLoggingRenderer) emit(severity, message string, labels map[string]any) {
	entry := map[string]any{
		"severity":  severity,
		"message":   message,
		"timestamp": time.Now().Format(time.RFC3339Nano),
	}
	if labels != nil {
		entry["logging.googleapis.com/labels"] = labels
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	r.mu.Lock()
	iox.Fprintf(r.out, "%s\n", line)
	r.mu.Unlock()
}

// eventSeverity maps event types to GCP severity levels.
func eventSeverity(event jobs.RawJobEvent) string {
	switch event.Type {
	case jobs.EventTypeDiagnostic:
		if sev, ok := event.Data["severity"].(string); ok {
			switch sev {
			case "error":
				return "ERROR"
			case "warning":
				return "WARNING"
			}
		}
		return "INFO"
	case jobs.EventTypeLog:
		if level, ok := event.Data["level"].(string); ok {
			switch level {
			case "error":
				return "ERROR"
			case "warn":
				return "WARNING"
			case "debug":
				return "DEBUG"
			}
		}
		return "INFO"
	default:
		return "INFO"
	}
}
