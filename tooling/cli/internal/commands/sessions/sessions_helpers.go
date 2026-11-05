package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// --- Types ---

type sessionJobSummary struct {
	project string
	job     string
	errMsg  string
}

type sessionDiagnostic struct {
	severity string
	message  string
	file     string
	line     int
	jobKey   string
}

// --- Helpers ---

// readSessionMeta reads session.json: a session document is the versioned
// cli.SessionFile.
func readSessionMeta(store *workspace_state.SessionStore, id string) (*workspace_state.SessionMetadata, error) {
	path := filepath.Join(store.Root(), id, "session.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := requireRecordedProtocolVersion(path, data); err != nil {
		return nil, err
	}
	var doc protocolcli.SessionFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return sessionMetaFromV2(&doc), nil
}

// readSessionPlan reads plan.json: a plan document is the versioned
// cli.SessionPlanFile.
func readSessionPlan(store *workspace_state.SessionStore, id string) (*workspace_state.PlanSnapshot, error) {
	path := filepath.Join(store.Root(), id, "plan.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := requireRecordedProtocolVersion(path, data); err != nil {
		return nil, err
	}
	var doc protocolcli.SessionPlanFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return planSnapshotFromV2(&doc), nil
}

// requireRecordedProtocolVersion refuses a recorded document that does not
// claim the version-2 contract.
func requireRecordedProtocolVersion(path string, data []byte) error {
	if version := recordedProtocolVersion(data); version < protocolcli.ResultProtocolVersion {
		return fmt.Errorf("%s records protocolVersion %d, want %d", path, version, protocolcli.ResultProtocolVersion)
	}
	return nil
}

// recordedProtocolVersion sniffs the document's own version claim. A document
// without the protocolVersion member reports 0.
func recordedProtocolVersion(data []byte) int {
	var probe struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return 0
	}
	return probe.ProtocolVersion
}

// sessionMetaFromV2 projects the v2 session document onto the in-memory reader
// view. The verdict mapping mirrors machine.Run exactly in reverse: counts are
// the strict histogram (reuse included), Cached folds local+remote provenance,
// and per-task rows recover the v1 outcome vocabulary from status × reuse.
func sessionMetaFromV2(doc *protocolcli.SessionFile) *workspace_state.SessionMetadata {
	meta := &workspace_state.SessionMetadata{
		ID:        doc.SessionID,
		StartTime: doc.StartTime,
		EndTime:   doc.EndTime,
		Duration:  doc.Run.DurationMs,
		Commands:  doc.Commands,
		Scheduler: doc.Scheduler,
		Cache:     doc.Cache,
		Stats: &workspace_state.SessionStats{
			Total:     doc.Run.Counts.Total,
			Succeeded: doc.Run.Counts.Succeeded,
			Failed:    doc.Run.Counts.Failed,
			Canceled:  doc.Run.Counts.Canceled,
			Skipped:   doc.Run.Counts.Skipped,
			Cached:    doc.Run.Reuse.LocalCache + doc.Run.Reuse.RemoteCache,
			Coalesced: doc.Run.Reuse.Coalesced,
			// DurationMs retains the v1 aggregate: sum only tasks that ran.
			// The session's wall-clock duration remains available as meta.Duration.
			DurationMs: 0,
		},
	}
	if doc.Git != nil {
		meta.Git = &workspace_state.SessionGitInfo{Branch: doc.Git.Branch, Baseline: doc.Git.Baseline}
	}
	if len(doc.Tasks) > 0 {
		meta.Jobs = make([]workspace_state.SessionJobEntry, 0, len(doc.Tasks))
		for i := range doc.Tasks {
			if doc.Tasks[i].Reuse == protocolcli.TaskReuseNone {
				meta.Stats.DurationMs += doc.Tasks[i].DurationMs
			}
			meta.Jobs = append(meta.Jobs, sessionJobFromV2(&doc.Tasks[i]))
		}
	}
	return meta
}

func sessionJobFromV2(task *protocolcli.TaskRecord) workspace_state.SessionJobEntry {
	entry := workspace_state.SessionJobEntry{
		Key:        task.Identity.Key,
		Project:    task.Identity.Project.Name,
		Job:        task.Identity.Task.Name,
		TaskKind:   task.Identity.Task.Kind,
		Extension:  task.Identity.Provider.Extension,
		Status:     task.Status,
		Outcome:    v1OutcomeOf(task.Status, task.Reuse),
		TaskWallMs: task.TaskWallMs,
	}
	if task.SpawnToFirstEventMs != 0 {
		spawn := task.SpawnToFirstEventMs
		entry.SpawnToFirstEventMs = &spawn
	}
	return entry
}

// v1OutcomeOf recovers the v1 outcome vocabulary (jobs.ReuseKind.Outcome):
// reuse when the result was reused — local and remote both spell "cached" —
// otherwise the execution status.
func v1OutcomeOf(status, reuse string) string {
	switch reuse {
	case protocolcli.TaskReuseCoalesced:
		return "coalesced"
	case protocolcli.TaskReuseLocalCache, protocolcli.TaskReuseRemoteCache:
		return "cached"
	default:
		return status
	}
}

// planSnapshotFromV2 projects the v2 plan document onto the in-memory snapshot
// view.
func planSnapshotFromV2(doc *protocolcli.SessionPlanFile) *workspace_state.PlanSnapshot {
	plan := &workspace_state.PlanSnapshot{
		SessionID: doc.SessionID,
		Commands:  doc.Commands,
		Jobs:      make([]workspace_state.PlanJobEntry, 0, len(doc.Tasks)),
	}
	for _, task := range doc.Tasks {
		plan.Jobs = append(plan.Jobs, workspace_state.PlanJobEntry{
			Key:       task.Identity.Key,
			Project:   task.Identity.Project.Name,
			Job:       task.Identity.Task.Name,
			Extension: task.Identity.Provider.Extension,
			DependsOn: task.DependsOn,
			After:     task.After,
			Cache:     task.Cache,
		})
	}
	return plan
}

// ledgerSubscriber is the name the sessions commands read a recorded stream
// under. They read after the fact and record no delivery evidence.
const ledgerSubscriber = "ledger"

// readSessionRecords is the sessions commands' one reader of events.jsonl: it
// subscribes to the recorded session's event stream from its first byte and
// visits every record in order, torn trailing bytes included.
func readSessionRecords(store *workspace_state.SessionStore, id string, visit func(sessionstream.Record) error) error {
	stream, err := sessionstream.Open(filepath.Join(store.Root(), id), id)
	if err != nil {
		return err
	}
	reader, err := stream.Subscribe(ledgerSubscriber, 0)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	for {
		record, ok, err := reader.Next()
		if err != nil || !ok {
			return err
		}
		if err := visit(record); err != nil {
			return err
		}
	}
}

// readSessionEvents is the lenient reader `sessions inspect` uses: blank and
// malformed records are skipped.
func readSessionEvents(store *workspace_state.SessionStore, id string) ([]workspace_state.SessionEvent, error) {
	var events []workspace_state.SessionEvent
	err := readSessionRecords(store, id, func(record sessionstream.Record) error {
		line := bytes.TrimSpace(record.Data)
		if len(line) == 0 {
			return nil
		}
		var ev workspace_state.SessionEvent
		if json.Unmarshal(line, &ev) == nil {
			events = append(events, ev)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// printSubscriberEvidence prints the session's subscribers.json, when the run
// had live stream subscribers. An unreadable document is reported, never
// skipped.
func printSubscriberEvidence(store *workspace_state.SessionStore, id string) {
	evidence, err := sessionstream.ReadEvidence(filepath.Join(store.Root(), id))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	iox.Fprintln(os.Stdout)
	if err != nil {
		iox.Fprintf(os.Stdout, "  Subscribers: %s is unreadable: %v\n", protocolcli.SessionSubscribersFileName, err)
		return
	}
	iox.Fprintf(os.Stdout, "  Subscribers (%d records):\n", evidence.Stream.Records)
	for _, subscriber := range evidence.Subscribers {
		iox.Fprintf(os.Stdout, "    %-24s %-9s acknowledged %d records, %d lost\n",
			subscriber.Name, subscriber.Evidence, subscriber.Acknowledged.Records, subscriber.Lost)
	}
}

type recordedTaskEnd struct {
	key        string
	project    string
	job        string
	status     string
	error      string
	durationMs int64
	reused     bool
}

// taskEndFromSessionEvent reads a recorded task:end record.
func taskEndFromSessionEvent(event workspace_state.SessionEvent) (recordedTaskEnd, bool) {
	if event.Record != protocolcli.RecordTaskEnd || event.Task == nil {
		return recordedTaskEnd{}, false
	}
	task := event.Task
	row := recordedTaskEnd{
		key:        task.Identity.Key,
		project:    task.Identity.Project.Name,
		job:        task.Identity.Task.Name,
		status:     task.Status,
		durationMs: task.DurationMs,
		reused:     task.Reuse != protocolcli.TaskReuseNone,
	}
	if task.Error != nil {
		row.error = task.Error.Message
	}
	return row, true
}

func diagnosticFromSessionEvent(event workspace_state.SessionEvent) (sessionDiagnostic, bool) {
	if event.Record != protocolcli.RecordTaskEvent || event.Event == nil {
		return sessionDiagnostic{}, false
	}
	if eventType, _ := event.Event["type"].(string); eventType != "diagnostic" {
		return sessionDiagnostic{}, false
	}
	return sessionDiagnosticFromData(event.Event, identityKey(event.Identity)), true
}

func sessionDiagnosticFromData(data map[string]any, jobKey string) sessionDiagnostic {
	diagnostic := sessionDiagnostic{jobKey: jobKey}
	diagnostic.severity, _ = data["severity"].(string)
	diagnostic.message, _ = data["message"].(string)
	if location, _ := data["location"].(map[string]any); location != nil {
		diagnostic.file, _ = location["file"].(string)
		diagnostic.line = int(numberValue(location["line"]))
	}
	return diagnostic
}

func identityKey(identity *protocolcli.TaskIdentity) string {
	if identity == nil {
		return ""
	}
	return identity.Key
}

func numberValue(value any) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case int:
		return float64(number)
	case int64:
		return float64(number)
	default:
		return 0
	}
}

// sessionsListJSONL emits the listed sessions as ONE JSON array, empty
// included, so the structured result's data is an array at every count (see
// printSummaryRowsJSON). A line per session put a bare object under data for
// one session, and a human message for none.
func sessionsListJSONL(store *workspace_state.SessionStore, sessions []string, latestID string, identities map[string]sessionIdentity) error {
	entries := make([]any, 0, len(sessions))
	for _, id := range sessions {
		meta, err := readSessionMeta(store, id)
		if err != nil {
			continue
		}
		entry := map[string]any{
			"id":       meta.ID,
			"start":    meta.StartTime,
			"end":      meta.EndTime,
			"duration": meta.Duration,
			"commands": meta.Commands,
			"latest":   id == latestID,
		}
		if meta.Stats != nil {
			entry["stats"] = meta.Stats
		}
		// Additive identity members, present only when the record states them:
		// the head commit of the judged tree and the recorded placement.
		if identity, ok := identities[id]; ok {
			if identity.headSHA != "" {
				entry["revision"] = identity.headSHA
			}
			if identity.placement != nil {
				entry["placement"] = identity.placement
			}
		}
		entries = append(entries, entry)
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	iox.Fprintln(os.Stdout, string(data))
	return nil
}

func sessionInspectJSONL(meta *workspace_state.SessionMetadata, plan *workspace_state.PlanSnapshot, events []workspace_state.SessionEvent) error {
	output := map[string]any{
		"metadata": meta,
	}
	if plan != nil {
		output["plan"] = plan
	}
	if len(events) > 0 {
		output["events"] = events
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	iox.Fprintln(os.Stdout, string(data))
	return nil
}

func formatDurationMs64(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return fmt.Sprintf("%dms", ms)
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}

func formatTimestamp(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	return t.Format("2006-01-02 15:04:05")
}
