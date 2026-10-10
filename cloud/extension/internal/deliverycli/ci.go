// putnami cloud ci — the workspace-scoped operator surface for delivery (CI)
// runs and the pipeline circuit-breaker. Every verb drives the control-plane
// API with the workspace-scoped bearer (clicore.ResolveWorkspaceContext): list,
// view, start, cancel, retry, logs (history + live SSE tail), status, and the
// repair and the pause/drain/resume pipeline modes. There is NO direct Cloud
// Run, database, or image-registry access — the CLI only ever talks to the
// control plane.
//
// The live log tail (`logs --follow`) goes through the generated delivery-api
// client (deliveryapi.go). The unary verbs still use local wire twins until
// they move to the same client: this module deliberately stays off the
// server dependency tree (it is also compiled under GOWORK=off, importing only
// internal/clicore + the generated delivery-api client), so every CI
// request/response DTO below is a LOCAL TWIN of the delivery API's records
// contract (ci_runs, ci_ops, ci_status) and of the API's CI log contract
// (cilogs). Keep the json tags in lockstep; TestCI*WireKeys pins them. The
// `--size` names come from the size enum delivery-api's generated client
// declares (ciRunnerSizes).
package deliverycli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.putnami.dev/client"
	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
	protocolcli "go.putnami.dev/protocol/cli"
)

// ciListPageLimit is the widest page the list verb fetches. It mirrors the
// server's runs.MaxListLimit (the delivery API's run records) — the largest page the
// list route serves — so a window the client still has to filter is scanned in
// as few round-trips as possible. Kept as a local twin (the server package is
// off the CLI dependency tree).
const ciListPageLimit = 200

// ciListDefaultLimit is how many runs `list` prints when the caller names no
// `--limit`. A terminal page's worth: the verb's job is "what happened lately",
// and the run history of a busy workspace is measured in thousands.
const ciListDefaultLimit = 20

// ciListMaxPages bounds how many offset pages the list verb walks. The server
// applies the list filters, so a filtered list is normally one page; a
// host older than the filters answers the plain history, and the client then
// filters it here, paginating by offset up to this bound. A generous ceiling
// that still guards a pathological backlog from spinning the CLI forever.
const ciListMaxPages = 20

// ciListPhaseFetchCap bounds the per-run single-run enrichment the `--phase`
// filter needs: the lean list window omits per-phase state (only `view` populates
// Phases), so `--phase` is the one list filter that cannot be answered from the
// window alone — it enriches each surviving run via its by-id view, bounded here.
const ciListPhaseFetchCap = 100

// ciFollowMaxReconnects bounds how many times ciFollowLogs re-opens the tail
// after a drop that streamed nothing. A connection that delivered ≥1 frame resets
// the budget (a live tail making progress keeps following). A var so the
// reconnect-bound test can shrink it.
var ciFollowMaxReconnects = 5

// ciFollowBackoff is the fixed delay between tail reconnect attempts. A var so
// tests run without wall-clock waits.
var ciFollowBackoff = 2 * time.Second

// --- Local wire twins (keep in lockstep with the server contract) ---

// CIRunResponse mirrors the delivery API's ci_runs.CIRunResponse.
type CIRunResponse struct {
	ID          string               `json:"id"`
	Status      string               `json:"status"`
	Conclusion  string               `json:"conclusion,omitempty"`
	Repo        string               `json:"repo,omitempty"`
	Branch      string               `json:"branch,omitempty"`
	SHA         string               `json:"sha,omitempty"`
	PullRequest int                  `json:"pullRequest,omitempty"`
	Provenance  string               `json:"provenance,omitempty"`
	Checks      map[string]string    `json:"checks,omitempty"`
	CreatedAt   time.Time            `json:"createdAt"`
	UpdatedAt   time.Time            `json:"updatedAt"`
	Phases      []CIRunPhaseResponse `json:"phases,omitempty"`
	// Heartbeat is the run's latest liveness sample, enriched by the
	// server on the single-run get only. Absent on a run that never beat.
	Heartbeat *CIRunHeartbeat `json:"heartbeat,omitempty"`
	// Cache is the run's cache economics, enriched by the server on the
	// single-run get from the run's stored builds report. Absent — never zeroed —
	// for a run with no builds report, because "nobody knows what this run reused"
	// is not "this run reused nothing".
	Cache *CIRunCache `json:"cache,omitempty"`
	// Timing is the run's wall-clock decomposition, enriched by the
	// server on the single-run get. Absent for a run with no timing fact.
	Timing *CIRunTiming `json:"timing,omitempty"`
	// Disk is how full the runner's disk got, enriched by the server on
	// the single-run get from the run's own annotations. Absent for a run whose
	// runner did not measure, because an unmeasured disk is not an empty one.
	Disk *CIRunDisk `json:"disk,omitempty"`
	// Failure is the runner's attribution of WHICH step failed, enriched by the
	// server on the single-run get from the run's own annotations. Absent when the
	// runner attributed nothing — which the view states rather than papers over.
	Failure *CIRunFailure `json:"failure,omitempty"`
	// FailedTasks are the tasks the gate reported failed: from the run's
	// derived failure digest when there is one, with each task's error,
	// diagnostics and last output lines, otherwise from the stored builds
	// report, names only. Absent on a run with neither.
	FailedTasks []CIRunFailedTask `json:"failedTasks,omitempty"`
	// FailedTasksOmitted counts the failed tasks past the listed ones.
	FailedTasksOmitted int `json:"failedTasksOmitted,omitempty"`
	// Session is the delivery account of the run's session record,
	// served for a terminal run only.
	Session *CIRunSession `json:"session,omitempty"`
}

// CIRunFailure mirrors apis.CIRunFailureResponse: the failing step and its
// qualifier, in the runner's own vocabulary. It carries no infrastructure-vs-code
// verdict on purpose — that word has one owner, and the aggregate check renders
// it (see the server twin's comment).
type CIRunFailure struct {
	Step   string `json:"step"`
	Detail string `json:"detail,omitempty"`
}

// CIRunFailedTask mirrors apis.CIRunFailedTaskResponse: one failing task's
// project and task name, and, from the failure digest, why it failed.
type CIRunFailedTask struct {
	Project       string                      `json:"project"`
	Task          string                      `json:"task"`
	Error         string                      `json:"error,omitempty"`
	Diagnostics   []CIRunFailedTaskDiagnostic `json:"diagnostics,omitempty"`
	Output        []string                    `json:"output,omitempty"`
	SharedOutput  *CIRunFailedTaskShared      `json:"sharedOutput,omitempty"`
	DetailOmitted bool                        `json:"detailOmitted,omitempty"`
}

// CIRunFailedTaskDiagnostic mirrors apis.CIRunFailedTaskDiagnostic.
type CIRunFailedTaskDiagnostic struct {
	File    string `json:"file,omitempty"`
	Line    int64  `json:"line,omitempty"`
	Message string `json:"message,omitempty"`
}

// CIRunFailedTaskShared mirrors apis.CIRunFailedTaskSharedOutput: output a
// batched invocation printed without naming a member project.
type CIRunFailedTaskShared struct {
	Source string   `json:"source"`
	Lines  []string `json:"lines"`
}

// CIRunSession mirrors apis.CIRunSessionResponse: how much of the run's
// session record reached the control plane, per part.
type CIRunSession struct {
	Complete bool             `json:"complete"`
	Session  CIRunSessionPart `json:"session"`
	Events   CIRunSessionPart `json:"events"`
}

// CIRunSessionPart mirrors apis.CIRunSessionPartResponse.
type CIRunSessionPart struct {
	Shipped bool  `json:"shipped"`
	Chunks  int   `json:"chunks"`
	Bytes   int64 `json:"bytes"`
	Final   bool  `json:"final"`
}

// CIRunTiming mirrors apis.CIRunTimingResponse: where one run's wall time went,
// stage by stage. Readings are microseconds and each is absent when unmeasured.
type CIRunTiming struct {
	QueueUsec       *int64 `json:"queueUsec,omitempty"`
	ColdStartUsec   *int64 `json:"coldStartUsec,omitempty"`
	FetchUsec       *int64 `json:"fetchUsec,omitempty"`
	InstallUsec     *int64 `json:"installUsec,omitempty"`
	GateUsec        *int64 `json:"gateUsec,omitempty"`
	TailUsec        *int64 `json:"tailUsec,omitempty"`
	RunDurationUsec *int64 `json:"runDurationUsec,omitempty"`
	CheckoutMode    string `json:"checkoutMode,omitempty"`
	CLIVersion      string `json:"cliVersion,omitempty"`
}

// CIRunDisk mirrors apis.CIRunDiskResponse: the runner's disk occupancy
// in GiB. Each reading is absent when the runner did not measure it;
// PeakAt and PeakPhase only ever accompany a PeakGiB.
type CIRunDisk struct {
	PeakGiB       *float64 `json:"peakGiB,omitempty"`
	PeakAt        string   `json:"peakAt,omitempty"`
	PeakPhase     string   `json:"peakPhase,omitempty"`
	TotalGiB      *float64 `json:"totalGiB,omitempty"`
	EndGiB        *float64 `json:"endGiB,omitempty"`
	GoCacheGiB    *float64 `json:"goCacheGiB,omitempty"`
	GoModcacheGiB *float64 `json:"goModcacheGiB,omitempty"`
}

// CIRunCache mirrors reports.CacheEconomics: one run's cache reuse, derived by
// the control plane from the stored per-task builds record.
//
// The two durations are deliberately NOT a "time saved". A task served from
// cache reports the milliseconds its replay cost, not the minutes the execution
// it replaced would have taken, so served and executed time are reported as the
// two measurements they are.
type CIRunCache struct {
	Tasks      int   `json:"tasks"`
	Hits       int   `json:"hits"`
	Misses     int   `json:"misses"`
	HitPercent int   `json:"hitPercent"`
	ServedMS   int64 `json:"servedMs"`
	ExecutedMS int64 `json:"executedMs"`
}

// CIRunHeartbeat mirrors heartbeats.CIRunHeartbeatResponse: the run's latest
// liveness sample. AgeSeconds is served by the control plane against its OWN
// clock, so the CLI renders staleness without comparing a local clock to a
// remote timestamp. The readings are pointers because an omitted one is "not
// measured" — a substrate without /proc reports no IO counters — which is a
// different fact from a measured zero. Cumulative vs delta is stated by the
// field name: CPUSeconds/RSSKB are totals, every `*Delta*` field is the change
// since the producer's previous beat.
type CIRunHeartbeat struct {
	Version         int       `json:"version"`
	Sequence        int64     `json:"sequence"`
	Attempt         int       `json:"attempt"`
	Session         string    `json:"session,omitempty"`
	Phase           string    `json:"phase"`
	ElapsedSeconds  int64     `json:"elapsedSeconds,omitempty"`
	AgeSeconds      int       `json:"ageSeconds"`
	CPUSeconds      *float64  `json:"cpuSeconds,omitempty"`
	CPUDeltaSeconds *float64  `json:"cpuDeltaSeconds,omitempty"`
	RSSKB           *int64    `json:"rssKb,omitempty"`
	IOReadDeltaKB   *int64    `json:"ioReadDeltaKb,omitempty"`
	IOWriteDeltaKB  *int64    `json:"ioWriteDeltaKb,omitempty"`
	Processes       *int      `json:"processes,omitempty"`
	ObservedAt      time.Time `json:"observedAt"`
}

// CIRunPhaseResponse mirrors ci_runs.CIRunPhaseResponse.
type CIRunPhaseResponse struct {
	Phase      string     `json:"phase"`
	Status     string     `json:"status"`
	Conclusion string     `json:"conclusion,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Required   bool       `json:"required"`
	Attempt    int        `json:"attempt"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// CIRunListResponse mirrors ci_runs.CIRunListResponse.
type CIRunListResponse struct {
	Runs       []CIRunResponse `json:"runs"`
	Limit      int             `json:"limit"`
	Offset     int             `json:"offset"`
	NextOffset int             `json:"nextOffset,omitempty"`
	// Filter names the filters the server applied to this page. A host older
	// than the server-side filters answers without it.
	Filter *CIRunListFilter `json:"filter,omitempty"`
}

// CIRunListFilter mirrors ci_runs.CIRunListFilter.
type CIRunListFilter struct {
	Repo         string     `json:"repo,omitempty"`
	Branch       string     `json:"branch,omitempty"`
	Ref          string     `json:"ref,omitempty"`
	PullRequest  int        `json:"pullRequest,omitempty"`
	Status       string     `json:"status,omitempty"`
	Phase        string     `json:"phase,omitempty"`
	CreatedAfter *time.Time `json:"createdAfter,omitempty"`
	IDPrefix     string     `json:"idPrefix,omitempty"`
}

// CIRunStartRequest mirrors ci_ops.CIRunStartRequest.
type CIRunStartRequest struct {
	Provider    string `json:"provider,omitempty"`
	Repo        string `json:"repo"`
	Ref         string `json:"ref,omitempty"`
	Branch      string `json:"branch,omitempty"`
	SHA         string `json:"sha"`
	PullRequest int    `json:"pullRequest,omitempty"`
	Phases      string `json:"phases,omitempty"`
	MaxParallel int    `json:"maxParallel,omitempty"`
	Size        string `json:"size,omitempty"`
	NoCache     bool   `json:"noCache,omitempty"`
	Reason      string `json:"reason"`
}

// CIRunCancelRequest mirrors ci_ops.CIRunCancelRequest.
type CIRunCancelRequest struct {
	Reason string `json:"reason"`
}

// CIRunRetryRequest mirrors ci_ops.CIRunRetryRequest.
type CIRunRetryRequest struct {
	Phase       string `json:"phase,omitempty"`
	NoCache     bool   `json:"noCache,omitempty"`
	MaxParallel int    `json:"maxParallel,omitempty"`
	Size        string `json:"size,omitempty"`
	Reason      string `json:"reason"`
}

// CIRepairRequest mirrors ci_ops.CIRepairRequest.
type CIRepairRequest struct {
	Reason string `json:"reason"`
}

// CIRepairResult mirrors ci_ops.CIRepairResult.
type CIRepairResult struct {
	Workspace            string `json:"workspace"`
	ReapedRuns           int    `json:"reapedRuns"`
	ReapedPhases         int    `json:"reapedPhases"`
	CancellationRequests int    `json:"cancellationRequests"`
	ScannedRuns          int    `json:"scannedRuns"`
	ReconciledRuns       int    `json:"reconciledRuns"`
	ScanLowerBound       bool   `json:"scanLowerBound,omitempty"`
}

// PipelineModeRequest mirrors ci_ops.PipelineModeRequest.
type PipelineModeRequest struct {
	Mode          string `json:"mode"`
	Reason        string `json:"reason"`
	CancelRunning bool   `json:"cancelRunning,omitempty"`
	ReviewRef     string `json:"reviewRef,omitempty"`
	IncidentRef   string `json:"incidentRef,omitempty"`
}

// PipelineModeResponse mirrors ci_ops.PipelineModeResponse.
type PipelineModeResponse struct {
	Scope        string   `json:"scope"`
	Workspace    string   `json:"workspace,omitempty"`
	Mode         string   `json:"mode"`
	Actor        string   `json:"actor"`
	Reason       string   `json:"reason"`
	CanceledRuns []string `json:"canceledRuns,omitempty"`
}

// CIStatusResponse mirrors ci_status.CIStatusResponse.
type CIStatusResponse struct {
	Workspace           string              `json:"workspace"`
	Healthy             bool                `json:"healthy"`
	Dimensions          []CIStatusDimension `json:"dimensions"`
	QueueDepth          int                 `json:"queueDepth"`
	OldestQueuedSeconds int                 `json:"oldestQueuedSeconds,omitempty"`
	StuckRuns           int                 `json:"stuckRuns"`
	QueueLowerBound     bool                `json:"queueLowerBound,omitempty"`
	// Execution provenance: the substrate this workspace's runs take, and
	// what the latest run recorded about what it actually executed on that axis.
	DispatchAxis                    string `json:"dispatchAxis,omitempty"`
	LastRunDigest                   string `json:"lastRunDigest,omitempty"`
	LastRunCLIVersion               string `json:"lastRunCliVersion,omitempty"`
	LastRunRunnerChannel            string `json:"lastRunRunnerChannel,omitempty"`
	LastRunRunnerSelector           string `json:"lastRunRunnerSelector,omitempty"`
	LastRunSourceRevision           string `json:"lastRunSourceRevision,omitempty"`
	CheckOutboxPending              int    `json:"checkOutboxPending,omitempty"`
	CheckOutboxOldestPendingSeconds int    `json:"checkOutboxOldestPendingSeconds,omitempty"`
	// Run liveness: the newest in-flight run's latest heartbeat. Absent
	// when nothing is executing or the executing run has not beaten yet.
	LiveRun                 string `json:"liveRun,omitempty"`
	LiveRunPhase            string `json:"liveRunPhase,omitempty"`
	LiveRunSequence         int64  `json:"liveRunSequence,omitempty"`
	LiveRunHeartbeatSeconds int    `json:"liveRunHeartbeatSeconds,omitempty"`
	// Cache economics of the most recent run: which run they were read
	// from, and what it reused. Absent when that run stored no builds report.
	LastRun      string      `json:"lastRun,omitempty"`
	LastRunCache *CIRunCache `json:"lastRunCache,omitempty"`
}

// CIStatusDimension mirrors ci_status.CIStatusDimension.
type CIStatusDimension struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	State       string `json:"state"`
	Explanation string `json:"explanation"`
	Recovery    string `json:"recovery"`
}

// CIRunLogsResponse mirrors the API's CI log contract,
// cilogs.CIRunLogsResponse. NextCursor is an opaque token the CLI passes back
// verbatim.
type CIRunLogsResponse struct {
	Entries     []CILogEntry         `json:"entries"`
	NextCursor  string               `json:"nextCursor,omitempty"`
	Diagnostics *CIRunLogDiagnostics `json:"diagnostics,omitempty"`
}

// CILogEntry mirrors the run log entry the server writes, in the field order
// and with the always-written timestamp that `ci logs --output=jsonl` has
// always printed. delivery-api's generated LogEntry carries the same members,
// but it marshals its keys alphabetically and drops an absent timestamp, so
// the CLI keeps this twin and converts each generated entry into it once
// (ciLogEntryFrom).
type CILogEntry struct {
	Timestamp    time.Time         `json:"timestamp"`
	Severity     ciSeverity        `json:"severity,omitempty"`
	SeverityText string            `json:"severityText,omitempty"`
	Body         string            `json:"body,omitempty"`
	TraceID      string            `json:"traceId,omitempty"`
	SpanID       string            `json:"spanId,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// CIRunLogDiagnostics mirrors cilogs.CIRunLogDiagnostics.
type CIRunLogDiagnostics struct {
	FirstErrors       []string          `json:"firstErrors,omitempty"`
	Failures          []CIRunLogFailure `json:"failures,omitempty"`
	TruncatedFailures int               `json:"truncatedFailures,omitempty"`
	Reproduce         string            `json:"reproduce,omitempty"`
}

// CIRunLogFailure mirrors cilogs.CIRunLogFailure: one failing task and the
// file-anchored diagnostics it produced.
type CIRunLogFailure struct {
	Project     string               `json:"project,omitempty"`
	Task        string               `json:"task,omitempty"`
	Message     string               `json:"message,omitempty"`
	Diagnostics []CIRunLogDiagnostic `json:"diagnostics,omitempty"`
	Truncated   int                  `json:"truncated,omitempty"`
}

// CIRunLogDiagnostic mirrors cilogs.CIRunLogDiagnostic: one finding, in the
// shape a `file:line:col message` line is built from.
type CIRunLogDiagnostic struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Severity string `json:"severity,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// CIRunLogFrame mirrors cilogs.CIRunLogFrame: one SSE tail frame carrying EITHER
// a redacted log entry or a run-state event, plus the opaque resume cursor valid
// AFTER it. Both payloads are optional, so a frame that carries neither still
// advances the cursor and is otherwise ignored.
type CIRunLogFrame struct {
	Entry  *CILogEntry       `json:"entry,omitempty"`
	Run    *CIRunLogRunEvent `json:"run,omitempty"`
	Cursor string            `json:"cursor"`
}

// CIRunLogRunEvent mirrors cilogs.CIRunLogRunEvent: the run's state as the
// control plane holds it, and whether this is the last frame the tail will send.
// Final is what lets `--follow` stop on its own when the run's log is complete
// instead of reconnecting until the budget drains.
type CIRunLogRunEvent struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	Final      bool   `json:"final"`
}

// CI is the `putnami cloud ci <verb>` entrypoint. It dispatches the sub-verb on
// the leading positional and hands off to the per-verb handler. Every verb drives
// the control plane with workspace auth; an unknown sub-verb is a usage error
// that prints the help. It mirrors Report's (params, args, workspaceRoot, env,
// ioctx) shape.
func CI(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	switch clicore.FirstPositional(args) {
	case "", "help":
		return ciHelp(params, ioctx)
	case "enable", "disable", "subscription":
		return ciSubscriptionCommand(params, clicore.FirstPositional(args), workspaceRoot, env, ioctx)
	case "list":
		return ciList(params, workspaceRoot, env, ioctx)
	case "view":
		return ciView(params, args, workspaceRoot, env, ioctx)
	case "start":
		return ciStart(params, workspaceRoot, env, ioctx)
	case "cancel":
		return ciCancel(params, args, workspaceRoot, env, ioctx)
	case "retry":
		return ciRetry(params, args, workspaceRoot, env, ioctx)
	case "logs":
		return ciLogs(params, args, workspaceRoot, env, ioctx)
	case "status":
		return ciStatus(params, args, workspaceRoot, env, ioctx)
	case "usage":
		return ciUsage(params, workspaceRoot, env, ioctx)
	case "insights":
		return ciInsights(params, args, workspaceRoot, env, ioctx)
	case "timings":
		return ciTimings(params, args, workspaceRoot, env, ioctx)
	case "results":
		return ciResults(params, args, workspaceRoot, env, ioctx)
	case "repair":
		return ciRepair(params, workspaceRoot, env, ioctx)
	case "report":
		return Report(params, clicore.DropFirstPositional(args), workspaceRoot, env, ioctx)
	case "init", "validate", "fmt", "explain":
		return ciContractVerb(clicore.FirstPositional(args), params, args, workspaceRoot, ioctx)
	case "pause":
		return ciPipeline(params, "paused", workspaceRoot, env, ioctx)
	case "drain":
		return ciPipeline(params, "draining", workspaceRoot, env, ioctx)
	case "resume":
		return ciPipeline(params, "active", workspaceRoot, env, ioctx)
	default:
		_ = ciHelp(params, ioctx)
		return clicore.NewError("unknown cloud ci sub-verb: "+clicore.FirstPositional(args), clicore.ExitUsage)
	}
}

func ciHelp(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "cloud ci enable|disable|subscription", "description": "manage the live CI subscription; enable accepts --runner-channel stable|canary"},
		{"command": "cloud channels follow <namespace> stable|canary", "description": "change a producer track; an rs_ id pins an exact release and --runner-channel changes the runner atomically"},
		{"command": "cloud ci list", "description": "list workspace CI runs, newest first (--limit N, default 20, 0 for the whole window; filtered by the server with --repo/--branch/--sha/--ref/--pr/--status/--phase/--age)"},
		{"command": "cloud ci view <run>", "description": "show one run with its per-phase attempts, and — when it failed — the failing step, the failing tasks, and their file:line diagnostics"},
		{"command": "cloud ci start --repo <o/r> --sha <sha> --reason <why>", "description": "manually start a run for a pushed SHA (--ref/--branch/--pr/--phases/--size/--max-parallel/--no-cache optional)"},
		{"command": "cloud ci cancel <run> --reason <why>", "description": "idempotently cancel a run"},
		{"command": "cloud ci retry <run> --reason <why>", "description": "retry a run (whole run, or --phase <p>, with --size/--no-cache/--max-parallel)"},
		{"command": "cloud ci logs <run>", "description": "read a run's logs, rendered for humans (--raw for the runner's protocol records); --follow tails live (--phase/--project/--q/--level/--from/--to/--cursor/--limit)"},
		{"command": "cloud ci status", "description": "CI readiness checks, queue, last run reuse, and runs and spend over 7 days"},
		{"command": "cloud ci usage", "description": "aggregate the workspace's runs over a window (--window <dur>, default 168h) by substrate, profile, source, gate wall, and estimated spend"},
		{"command": "cloud ci insights <run>", "description": "one run's contracted gate report — CPU balance sheet, cache economics, scheduler shape, per-command and per-job detail (--output=json for the whole document)"},
		{"command": "cloud ci timings <run>", "description": "one run's per-task timeline — absolute start, end and CPU share per task, longest wall first — so a run's critical path can be reconstructed (--limit N, default 40, 0 for every row; --output=json emits the whole table unless --limit asks for fewer)"},
		{"command": "cloud ci timings <run> --against <other-run>", "description": "why each task of the run executed instead of being served by the cache, read against a run that could have filled it: no cache key, a key that changed, or the same key with no entry"},
		{"command": "cloud ci results <run>", "description": "what one run's gate found — failed tests with their output, coverage per project against its threshold, diagnostics by file and line, and what it published (--output=json for every kept case)"},
		{"command": "cloud ci repair --reason <why>", "description": "idempotently terminalize stale rows, cancel persisted compute, and reconcile terminal checks"},
		{"command": "cloud ci pause|drain|resume --reason <why>", "description": "set the workspace pipeline circuit-breaker mode"},
		{"command": "cloud ci report -- <command>", "description": "run a command and report its outcome to a CI run"},
		{"command": "cloud ci init [--force]", "description": "write a putnami.ci.json that runs the lint, test, build and validate jobs the workspace declares"},
		{"command": "cloud ci validate", "description": "check putnami.ci.json against its schema and the jobs the workspace's extensions declare"},
		{"command": "cloud ci fmt [--check]", "description": "rewrite putnami.ci.json in canonical form; --check only reports"},
		{"command": "cloud ci explain --event push|tag|pull_request --branch <b> | --git-tag <t> [--pr <n>]", "description": "show which rule matches an event and what the run would publish, without calling the provider"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud ci commands:")
	for _, c := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-70s %s", c["command"], c["description"]))
	}
	ioctx.Stdout("")
	ioctx.Stdout("<run> takes any unambiguous run-id prefix, not the full 64 characters; `list` prints ids at that width.")
	ioctx.Stdout("`logs --level` matches each entry's OWN findings, not the severity of the pipe that carried them.")
	ioctx.Stdout("`view <run>` on a failed run lists each failed task with its error, diagnostics and last output lines, derived by the control plane from the")
	ioctx.Stdout("run's session record; its `Session:` line says whether that record arrived. `timings <run>` lists every task. A run whose record never arrived has no digest.")
	ioctx.Stdout("Every verb drives the control-plane API with workspace auth (no direct Cloud Run, database, or registry access).")
	ioctx.Stdout("list sends its filters to the server; against an older server that ignores them, it filters the fetched history itself.")
	ioctx.Stdout("start/cancel/retry/repair/pause/drain/resume require --reason and prompt for confirmation (bypass with --yes).")
	ioctx.Stdout("start/retry accept --max-parallel <n> to set THIS run's gate fan-out from 1 to 32; omitted uses the resolved allocation width.")
	ioctx.Stdout("start/retry accept --size " + strings.Join(ciRunnerSizes(), "|") + " to override change-aware runner sizing for THIS run; omitted resolves plan class, then workspace default. The five versioned sizes allocate exact 2/4/8/16/32 vCPU on GCE Spot.")
	ioctx.Stdout("The fan-out also sizes the runner's test PostgreSQL ceiling, so a width that image cannot serve is refused by the run itself.")
	ioctx.Stdout("start/retry accept --no-cache to force THIS run's gate to execute every task instead of reusing cache entries; it never widens the gate's impacted selection.")
	return nil
}

// --- list ---

// ciListFilters is the parsed set of list filters. The client sends each one to
// the server, and applies itself only the ones a page says the server
// did not apply: all of them against a host older than the filters.
type ciListFilters struct {
	repo   string
	branch string
	sha    string
	pr     int
	hasPR  bool
	status string
	phase  string
	age    time.Duration
	hasAge bool
}

// serverQuery is the run list request carrying every filter. --sha and --ref
// travel as the server's ref, which keeps their meaning: a commit SHA prefix
// or a branch name. A pull request of zero has no server form, so it stays a
// client filter; --age travels as the instant it reaches back to.
func (f ciListFilters) serverQuery(now time.Time) deliveryapiclient.GetV1WorkspacesRunsQuery {
	text := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	q := deliveryapiclient.GetV1WorkspacesRunsQuery{
		Repo:   text(f.repo),
		Branch: text(f.branch),
		Ref:    text(f.sha),
		Status: text(f.status),
		Phase:  text(f.phase),
	}
	if f.hasPR && f.pr > 0 {
		pr := int64(f.pr)
		q.PullRequest = &pr
	}
	if f.hasAge {
		q.CreatedAfter = text(now.Add(-f.age).UTC().Format(time.RFC3339Nano))
	}
	return q
}

// pendingAfter is the part of the filter the client still applies itself:
// every filter the applied set does not name. No applied set means a host
// older than the server-side filters, so the whole filter is pending.
func (f ciListFilters) pendingAfter(applied *CIRunListFilter) ciListFilters {
	if applied == nil {
		return f
	}
	pending := f
	if applied.Repo != "" {
		pending.repo = ""
	}
	if applied.Branch != "" {
		pending.branch = ""
	}
	if applied.Ref != "" {
		pending.sha = ""
	}
	if applied.PullRequest != 0 {
		pending.pr, pending.hasPR = 0, false
	}
	if applied.Status != "" {
		pending.status = ""
	}
	if applied.Phase != "" {
		pending.phase = ""
	}
	if applied.CreatedAfter != nil {
		pending.age, pending.hasAge = 0, false
	}
	return pending
}

// any reports whether a filter is set.
func (f ciListFilters) any() bool {
	return f != ciListFilters{}
}

func ciList(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	filters, err := parseCIListFilters(params)
	if err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()

	limit, err := ciListLimit(params)
	if err != nil {
		return err
	}

	walk, err := ciListWalk(reqCtx, ctx, filters, limit, ioctx.Now())
	if err != nil {
		return err
	}
	collected := walk.collected

	// --phase is the one filter the lean list window cannot answer (Phases is
	// populated only by the by-id view), so a server that did not apply it
	// leaves the client to enrich the survivors, bounded.
	if walk.pending.phase != "" {
		var truncated bool
		collected, truncated, err = ciFilterByPhase(reqCtx, ctx, collected, filters.phase)
		if err != nil {
			return err
		}
		// The enrichment is capped at ciListPhaseFetchCap runs; older matches past
		// the cap were never checked, so warn rather than present a silently partial
		// list as complete.
		if truncated && ioctx.Stderr != nil {
			ioctx.Stderr(fmt.Sprintf("note: --phase checked only the first %d runs in the window; matches in older runs were not scanned — narrow with --repo/--branch/--age.", ciListPhaseFetchCap))
		}
	}

	// The window is trimmed to the page LAST, so every filter above still decides
	// over the full window and only the printing is bounded.
	truncated := 0
	if limit > 0 && len(collected) > limit {
		truncated = len(collected) - limit
		collected = collected[:limit]
	}

	if clicore.StructuredOutput(params) {
		// D5: one machine object PER LINE (never a wrapped page), stable shape.
		for i := range collected {
			ioctx.Stdout(ciCompactJSON(collected[i]))
		}
		return nil
	}
	ciRenderRunList(ioctx, collected)
	// A bounded list must say it is bounded, or the oldest row printed reads as the
	// oldest run there is. Two different bounds can apply, and they mean different
	// things: the page bound is the caller's and is raised with --limit; the window
	// bound is this client's paging cap and --limit cannot reach past it.
	// The count is exact only when the walk read the whole history; a walk that
	// stopped early knows that more runs match, not how many.
	switch {
	case truncated > 0 && walk.exhausted:
		ioctx.Stdout(fmt.Sprintf("… %d more in the window — raise --limit, or narrow with --repo/--branch/--age.", truncated))
	case truncated > 0:
		ioctx.Stdout("… more runs match — raise --limit, or narrow with --repo/--branch/--age.")
	}
	if walk.capped {
		switch {
		case walk.serverFiltered && !walk.pending.any():
			ioctx.Stdout(fmt.Sprintf(
				"… and the walk stopped after %d matching runs (%d pages); older matches were not read — narrow with --repo/--branch/--age.",
				walk.read, walk.pages))
		case walk.serverFiltered:
			// The server applied some filters, so the rows read are neither all
			// runs nor only matches.
			ioctx.Stdout(fmt.Sprintf(
				"… and the walk stopped after reading %d runs (%d pages); older runs were not read — narrow with --repo/--branch/--age.",
				walk.read, walk.pages))
		default:
			ioctx.Stdout(fmt.Sprintf(
				"… and the walk stopped after the newest %d runs (%d pages); older history was not scanned — narrow with --repo/--branch/--age.",
				walk.read, walk.pages))
		}
	}
	return nil
}

// ciListWalkResult is what the list walk read.
type ciListWalkResult struct {
	collected []CIRunResponse
	// pending is the part of the filter the client applied itself.
	pending ciListFilters
	// serverFiltered reports that the server applied the filter it echoed, so
	// the offsets the walk followed count matching runs, not all runs.
	serverFiltered bool
	// exhausted reports that the walk reached the end of the history.
	exhausted bool
	// capped reports that the walk stopped at ciListMaxPages while the server
	// still had more to give. That bound is invisible from the output — the
	// last row printed looks like the oldest run there is — so it is stated.
	capped bool
	read   int
	pages  int
}

// ciListWalk reads the run list newest-first until it holds more than `limit`
// matches, or reaches the end of the history, or the page cap.
//
// The first page decides the mode for the whole walk, because the two modes
// count offsets in different spaces. A server that applied filters echoes
// them in its `filter` member, and its next offset counts matching runs. A
// server older than the filters echoes nothing, and its next offset counts all
// runs; the client then reads plain pages and filters them itself. A later
// page whose echo differs from the first (a rolling deploy answered by the
// other side) restarts the walk from offset 0 in plain mode, once, so no page
// is read at an offset from the wrong space.
//
// One row past `limit` is asked for, so the walk knows whether more runs match
// without guessing from the next offset.
func ciListWalk(reqCtx context.Context, ctx *clicore.WorkspaceContext, filters ciListFilters, limit int, now time.Time) (ciListWalkResult, error) {
	filtered := filters.serverQuery(now)
	pageSize := ciListPageLimit
	if limit > 0 && limit+1 < pageSize {
		pageSize = limit + 1
	}
	var (
		out       = ciListWalkResult{pending: filters}
		firstEcho *CIRunListFilter
		offset    int
		// decided is set once the first page has chosen the mode.
		decided bool
	)
	for out.pages < ciListMaxPages {
		query := deliveryapiclient.GetV1WorkspacesRunsQuery{}
		if !decided || out.serverFiltered {
			query = filtered
		}
		pageLimit, pageOffset := int64(pageSize), int64(offset)
		query.Limit, query.Offset = &pageLimit, &pageOffset
		resp, err := ciListRuns(reqCtx, ctx, query)
		if err != nil {
			return out, err
		}
		out.pages++
		switch {
		case !decided:
			decided = true
			firstEcho = resp.Filter
			out.serverFiltered = resp.Filter != nil
			out.pending = filters.pendingAfter(resp.Filter)
		case out.serverFiltered && !ciSameListFilter(firstEcho, resp.Filter):
			// The plain walk gets a fresh page budget: it starts once at most,
			// because serverFiltered stays false from here on.
			out = ciListWalkResult{pending: filters}
			offset, pageSize = 0, ciListPageLimit
			continue
		}
		out.read += len(resp.Runs)
		for i := range resp.Runs {
			if ciRunMatches(resp.Runs[i], out.pending, now) {
				out.collected = append(out.collected, resp.Runs[i])
			}
		}
		if resp.NextOffset == 0 {
			out.exhausted = true
			return out, nil
		}
		// Runs come back newest-first, so the first `limit` matches are the
		// answer and one more says that more match. A --phase filter the server
		// did not apply is the exception: it is decided after the walk, so its
		// matches cannot be counted yet.
		if out.pending.phase == "" && limit > 0 && len(out.collected) > limit {
			return out, nil
		}
		offset = resp.NextOffset
		if !out.serverFiltered || out.pending.any() {
			pageSize = ciListPageLimit
		}
	}
	out.capped = true
	return out, nil
}

// ciSameListFilter reports whether two pages echoed the same applied filter.
func ciSameListFilter(a, b *CIRunListFilter) bool {
	if a == nil || b == nil {
		return a == b
	}
	sameTime := (a.CreatedAfter == nil) == (b.CreatedAfter == nil) &&
		(a.CreatedAfter == nil || a.CreatedAfter.Equal(*b.CreatedAfter))
	return sameTime && a.Repo == b.Repo && a.Branch == b.Branch && a.Ref == b.Ref &&
		a.PullRequest == b.PullRequest && a.Status == b.Status && a.Phase == b.Phase && a.IDPrefix == b.IDPrefix
}

// ciListLimit resolves how many runs `list` prints. It defaults to
// ciListDefaultLimit rather than to the whole window: an unfiltered `ci list` used
// to print every run the workspace had ever recorded — thousands of lines, because
// --limit was a LOGS flag that this verb silently ignored. `--limit 0` restores
// the unbounded form for a caller that really wants the lot.
func ciListLimit(params map[string]any) (int, error) {
	value := clicore.Param(params, "limit")
	if value == nil {
		return ciListDefaultLimit, nil
	}
	// A value that is PRESENT but unusable is refused rather than folded into the
	// default. NumberParam alone cannot do this: it returns nil for `--limit foo`,
	// which is indistinguishable from "not supplied" and would silently print 20
	// rows as if the flag had been honored — the same class of quiet no-op that
	// made --limit meaningless for this verb in the first place. It also parses as
	// a float, so `--limit 1.5` would truncate to 1 without saying so.
	n := clicore.NumberParam(params, "limit")
	if n == nil || *n != math.Trunc(*n) || *n < 0 {
		return 0, clicore.NewError(fmt.Sprintf(
			"--limit must be a non-negative whole number of runs (got %v); --limit 0 prints the whole window", value),
			clicore.ExitUsage)
	}
	return int(*n), nil
}

func parseCIListFilters(params map[string]any) (ciListFilters, error) {
	f := ciListFilters{
		repo:   strings.TrimSpace(clicore.StringParam(params, "repo")),
		branch: strings.TrimSpace(clicore.StringParam(params, "branch")),
		sha:    strings.TrimSpace(clicore.FirstString(clicore.StringParam(params, "sha"), clicore.StringParam(params, "ref"))),
		status: strings.TrimSpace(clicore.StringParam(params, "status")),
		phase:  strings.TrimSpace(clicore.StringParam(params, "phase")),
	}
	if n := clicore.NumberParam(params, "pr", "pullRequest"); n != nil {
		f.pr = int(*n)
		f.hasPR = true
	}
	if raw := strings.TrimSpace(clicore.StringParam(params, "age")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return f, clicore.NewError("--age must be a positive Go duration (e.g. 1h, 30m, 24h)", clicore.ExitUsage)
		}
		f.age = d
		f.hasAge = true
	}
	return f, nil
}

// ciRunMatches applies the window-derivable filters (repo/ref/sha/pr/status/age)
// to one run. --phase is applied separately (it needs per-run enrichment).
func ciRunMatches(run CIRunResponse, f ciListFilters, now time.Time) bool {
	if f.repo != "" && !strings.EqualFold(run.Repo, f.repo) {
		return false
	}
	if f.branch != "" && !strings.EqualFold(run.Branch, f.branch) {
		return false
	}
	if f.sha != "" && !strings.HasPrefix(strings.ToLower(run.SHA), strings.ToLower(f.sha)) && !strings.EqualFold(run.Branch, f.sha) {
		return false
	}
	if f.hasPR && run.PullRequest != f.pr {
		return false
	}
	if f.status != "" && !strings.EqualFold(run.Status, f.status) && !strings.EqualFold(run.Conclusion, f.status) {
		return false
	}
	if f.hasAge && !run.CreatedAt.IsZero() && now.Sub(run.CreatedAt) > f.age {
		return false
	}
	return true
}

// ciFilterByPhase keeps only runs that carry an attempt of the named phase,
// enriching each surviving run via its by-id view (the list omits Phases). It is
// bounded by ciListPhaseFetchCap so a huge window cannot fan out unboundedly; it
// reports whether that bound was hit (more runs than the cap), so the caller can
// warn that older matches were not scanned rather than present a partial list as
// complete.
func ciFilterByPhase(reqCtx context.Context, ctx *clicore.WorkspaceContext, runs []CIRunResponse, phase string) ([]CIRunResponse, bool, error) {
	truncated := len(runs) > ciListPhaseFetchCap
	var kept []CIRunResponse
	for i := range runs {
		if i >= ciListPhaseFetchCap {
			break
		}
		detail, err := ciGetRun(reqCtx, ctx, "list", runs[i].ID, false)
		if err != nil {
			return nil, false, err
		}
		for j := range detail.Phases {
			if strings.EqualFold(detail.Phases[j].Phase, phase) {
				kept = append(kept, runs[i])
				break
			}
		}
	}
	return kept, truncated, nil
}

// --- view ---

func ciView(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	run := ciRunID(args)
	if run == "" {
		return clicore.NewError("cloud ci view requires a run id: `putnami cloud ci view <run>`", clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	// The resolved id is discarded: everything below keys off resp.ID, which is the
	// server's own answer and therefore the authoritative one.
	resp, _, err := ciRunRequest(reqCtx, ctx, run, func(id string) (*CIRunResponse, error) {
		return ciGetRun(reqCtx, ctx, "view", id, true)
	})
	if err != nil {
		return err
	}
	if clicore.StructuredOutput(params) {
		ioctx.Stdout(ciCompactJSON(*resp))
		return nil
	}
	ciRenderRunDetail(ioctx, resp)
	failures, truncatedTasks := ciViewDiagnostics(reqCtx, ctx, resp)
	ciRenderFailures(ioctx, failures, truncatedTasks)
	return nil
}

// ciViewDiagnostics fetches the gate's file-anchored diagnostics for a run that
// FAILED, so `view` ends at the offending line instead of at the offending phase.
//
// It is deliberately narrow and best-effort. Only a `failure` conclusion pays the
// second request: a green run has nothing to show, and a timed-out one has no
// diagnostics by construction — a scheduler deadline claims no source location,
// which is the same reason the check output refuses to annotate from timeout
// evidence. Any error here returns nothing at all rather than failing the view;
// the run facts are the answer, and the diagnostics are the enrichment.
// It returns the failing tasks plus how many the server's cap left out.
func ciViewDiagnostics(reqCtx context.Context, ctx *clicore.WorkspaceContext, run *CIRunResponse) ([]CIRunLogFailure, int) {
	if !strings.EqualFold(strings.TrimSpace(run.Conclusion), "failure") {
		return nil, 0
	}
	// The failure digest already printed each task's diagnostics and output.
	if ciFailedTasksCarryDetail(run.FailedTasks) {
		return nil, 0
	}
	resp, err := ciGetRunLogs(reqCtx, ctx, "view", run.ID, url.Values{})
	if err != nil || resp == nil || resp.Diagnostics == nil {
		return nil, 0
	}
	return resp.Diagnostics.Failures, resp.Diagnostics.TruncatedFailures
}

// ciMaxParallel parses the optional per-run gate fan-out (`--max-parallel`,
// part of the CI concurrency controls). It returns 0 for "not requested", which the
// wire omits and the control plane forwards as "no CI_MAX_PARALLEL", so the
// RUNNER's own default (4) applies and the default lives in exactly one place.
//
// `auto` is absence, not a request. The host CLI's default context commonly
// supplies that value for this well-known flag name even when the user chose
// nothing, so treating it as a width would silently re-dispatch every run at
// whatever `auto` parsed to.
//
// Everything else that is not a positive whole number is a USAGE error rather than
// a fallback to the default: a ladder cell that quietly ran at another width is
// worse than one that never ran. There is deliberately NO upper bound here — the
// runner image owns which widths it can serve (the fan-out sizes its test
// PostgreSQL ceiling), and a maximum copied into this client would be a second
// ceiling to keep in step with the first.
func ciMaxParallel(params map[string]any, verb string) (int, error) {
	raw := clicore.StringParam(params, "max-parallel", "maxParallel")
	if raw == "" || strings.EqualFold(raw, "auto") {
		return 0, nil
	}
	maxParallel, err := strconv.Atoi(raw)
	if err != nil || maxParallel < 1 || maxParallel > ciMaxRunnerParallelism {
		return 0, clicore.NewError(
			fmt.Sprintf("cloud ci %s --max-parallel must be a whole number between 1 and %d; omit it to use the resolved allocation width", verb, ciMaxRunnerParallelism),
			clicore.ExitUsage)
	}
	return maxParallel, nil
}

// ciMaxRunnerParallelism is the widest per-run gate fan-out the server accepts
// for `--max-parallel`. Local: delivery-api's schema bounds maxParallel at 32,
// but its generated client declares no constant for that maximum: the
// framework's Go client generator only checks a `maximum` at run time.
const ciMaxRunnerParallelism = 32

// ciRunnerSizes returns every runner size a caller may name, smallest
// allocation first: the size enum delivery-api's generated client declares for
// a run start (a retry declares the same list), without `auto`, which means
// "not requested". Each call returns a new slice.
func ciRunnerSizes() []string {
	return []string{
		string(deliveryapiclient.CIRunStartRequestSizeXsmall),
		string(deliveryapiclient.CIRunStartRequestSizeSmall),
		string(deliveryapiclient.CIRunStartRequestSizeStandard),
		string(deliveryapiclient.CIRunStartRequestSizeLarge),
		string(deliveryapiclient.CIRunStartRequestSizeXlarge),
	}
}

// ciIsRunnerSize reports whether name is exactly one of ciRunnerSizes. It does
// not trim or lower-case: the caller normalizes its own input first.
func ciIsRunnerSize(name string) bool {
	for _, size := range ciRunnerSizes() {
		if size == name {
			return true
		}
	}
	return false
}

// ciRunnerSize validates the manual smart-sizing escape hatch. Empty/auto means
// normal plan → workspace → fleet resolution; every explicit value is closed so
// a typo can never silently launch the standard (and more expensive) machine.
// The names come from ciRunnerSizes, the enum the server validates against.
func ciRunnerSize(params map[string]any, verb string) (string, error) {
	size := strings.ToLower(strings.TrimSpace(clicore.StringParam(params, "size")))
	if size == "" || size == "auto" {
		return "", nil
	}
	if ciIsRunnerSize(size) {
		return size, nil
	}
	return "", clicore.NewError(
		fmt.Sprintf("cloud ci %s --size must be one of %s; omit it (or use auto) for change-aware sizing",
			verb, strings.Join(ciRunnerSizes(), ", ")),
		clicore.ExitUsage)
}

// ciNoCache parses the optional per-run forced-cold gate (`--no-cache`,
// part of the same concurrency controls). It returns false for "not requested", which the wire omits
// and the control plane forwards as "no CI_NO_CACHE", so the RUNNER's own default
// applies and that default lives in exactly one place.
//
// The extension parser resolves `--no-cache` as cache=false, while the host CLI
// forwards its global flag as no-cache=true. Accept both shapes in this one
// reader shared by start and retry so the two dispatch paths cannot disagree.
//
// There is no invalid value to reject here: the flag is a boolean at this layer.
// The value a runner could fail to honor is the ENV token, and it is refused
// there — the entrypoint reads CI_NO_CACHE as an enumeration and aborts at
// bootstrap on anything but `true`/`false`, rather than degrading to a warm run.
func ciNoCache(params map[string]any) bool {
	return clicore.Truthy(clicore.Param(params, "no-cache", "noCache")) ||
		!clicore.BoolParam(params, true, "cache")
}

// --- start ---

func ciStart(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	reason, err := ciReason(params)
	if err != nil {
		return err
	}
	repo := strings.TrimSpace(clicore.StringParam(params, "repo"))
	if repo == "" {
		return clicore.NewError("cloud ci start requires --repo <owner/name>", clicore.ExitUsage)
	}
	sha := strings.TrimSpace(clicore.StringParam(params, "sha"))
	if sha == "" {
		return clicore.NewError("cloud ci start requires --sha <commit>", clicore.ExitUsage)
	}
	maxParallel, err := ciMaxParallel(params, "start")
	if err != nil {
		return err
	}
	size, err := ciRunnerSize(params, "start")
	if err != nil {
		return err
	}
	body := CIRunStartRequest{
		Provider:    strings.TrimSpace(clicore.StringParam(params, "provider")),
		Repo:        repo,
		Ref:         strings.TrimSpace(clicore.StringParam(params, "ref")),
		Branch:      strings.TrimSpace(clicore.StringParam(params, "branch")),
		SHA:         sha,
		Phases:      strings.TrimSpace(clicore.StringParam(params, "phases")),
		MaxParallel: maxParallel,
		Size:        size,
		// --no-cache forces THIS run's gate to execute rather than reuse.
		// Omitted leaves the runner's own default, so an ordinary manual start is
		// unchanged.
		NoCache: ciNoCache(params),
		Reason:  reason,
	}
	if n := clicore.NumberParam(params, "pr", "pullRequest"); n != nil {
		body.PullRequest = int(*n)
	}
	if err := ciConfirm(params, ioctx, fmt.Sprintf("Start a CI run for %s@%s?", repo, shortSHA(sha))); err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	// start returns 202 (run opened; the network tail proceeds async).
	var request deliveryapiclient.CIRunStartRequest
	if err := ciReencode(body, &request); err != nil {
		return clicore.NewError("cloud ci start: encode request: "+err.Error(), clicore.ExitAPI)
	}
	resp, err := ciCall[CIRunResponse](reqCtx, ctx, "start", ctx.WorkspaceURL("/runs"),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIRunResponse, error) {
			result, err := api.CreateV1WorkspacesRuns(callCtx, deliveryapiclient.CreateV1WorkspacesRunsInput{
				Path: deliveryapiclient.CreateV1WorkspacesRunsPath{Workspace: ctx.WorkspaceID},
				Body: request,
			})
			if err != nil {
				return nil, err
			}
			return result.Body, nil
		})
	if err != nil {
		return err
	}
	ciWriteRun(params, ioctx, resp, fmt.Sprintf("Started CI run %s (%s) for %s@%s.", resp.ID, resp.Status, resp.Repo, shortSHA(resp.SHA)))
	return nil
}

// --- cancel ---

func ciCancel(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	run := ciRunID(args)
	if run == "" {
		return clicore.NewError("cloud ci cancel requires a run id: `putnami cloud ci cancel <run> --reason <why>`", clicore.ExitUsage)
	}
	reason, err := ciReason(params)
	if err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	if err := ciConfirm(params, ioctx, fmt.Sprintf("Cancel CI run %s?", run)); err != nil {
		return err
	}
	// cancel returns 200 (idempotent; a terminal run is a no-op).
	resp, _, err := ciRunRequest(reqCtx, ctx, run, func(id string) (*CIRunResponse, error) {
		return ciCall[CIRunResponse](reqCtx, ctx, "cancel", ctx.WorkspaceURL("/runs/"+url.PathEscape(id)+"/cancel"),
			func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIRunResponse, error) {
				return api.CreateV1WorkspacesRunsCancel(callCtx, deliveryapiclient.CreateV1WorkspacesRunsCancelInput{
					Path: deliveryapiclient.CreateV1WorkspacesRunsCancelPath{Workspace: ctx.WorkspaceID, Run: id},
					Body: deliveryapiclient.CIRunCancelRequest{Reason: reason},
				})
			})
	})
	if err != nil {
		return err
	}
	ciWriteRun(params, ioctx, resp, fmt.Sprintf("Canceled CI run %s (now %s).", resp.ID, ciConclusionOrStatus(resp)))
	return nil
}

// --- retry ---

func ciRetry(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	run := ciRunID(args)
	if run == "" {
		return clicore.NewError("cloud ci retry requires a run id: `putnami cloud ci retry <run> --reason <why>`", clicore.ExitUsage)
	}
	reason, err := ciReason(params)
	if err != nil {
		return err
	}
	maxParallel, err := ciMaxParallel(params, "retry")
	if err != nil {
		return err
	}
	size, err := ciRunnerSize(params, "retry")
	if err != nil {
		return err
	}
	body := CIRunRetryRequest{
		Phase: strings.TrimSpace(clicore.StringParam(params, "phase")),
		// --no-cache both forces the gate cold and records the explicit
		// clean/cache-busting request as run provenance.
		NoCache:     ciNoCache(params),
		MaxParallel: maxParallel,
		Size:        size,
		Reason:      reason,
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	action := fmt.Sprintf("Retry CI run %s?", run)
	if body.Phase != "" {
		action = fmt.Sprintf("Retry phase %s of CI run %s?", body.Phase, run)
	}
	if err := ciConfirm(params, ioctx, action); err != nil {
		return err
	}
	// retry returns 202 (new attempt opened; the network tail proceeds async).
	resp, _, err := ciRunRequest(reqCtx, ctx, run, func(id string) (*CIRunResponse, error) {
		var request deliveryapiclient.CIRunRetryRequest
		if err := ciReencode(body, &request); err != nil {
			return nil, clicore.NewError("cloud ci retry: encode request: "+err.Error(), clicore.ExitAPI)
		}
		return ciCall[CIRunResponse](reqCtx, ctx, "retry", ctx.WorkspaceURL("/runs/"+url.PathEscape(id)+"/retry"),
			func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIRunResponse, error) {
				result, err := api.CreateV1WorkspacesRunsRetry(callCtx, deliveryapiclient.CreateV1WorkspacesRunsRetryInput{
					Path: deliveryapiclient.CreateV1WorkspacesRunsRetryPath{Workspace: ctx.WorkspaceID, Run: id},
					Body: request,
				})
				if err != nil {
					return nil, err
				}
				return result.Body, nil
			})
	})
	if err != nil {
		return err
	}
	ciWriteRun(params, ioctx, resp, ciRetryMessage(body.Phase, resp))
	return nil
}

// ciRetryMessage names what the retry opened. A `--phase deploy` retry opens no
// new run: it reopens the deploy phase of the SAME run as a new attempt,
// so the message names that attempt rather than a run id the
// operator already has.
func ciRetryMessage(phase string, resp *CIRunResponse) string {
	if phase != "deploy" {
		return fmt.Sprintf("Opened retry %s (%s).", resp.ID, resp.Status)
	}
	attempt := 0
	for _, p := range resp.Phases {
		if p.Phase == "deploy" && p.Attempt > attempt {
			attempt = p.Attempt
		}
	}
	if attempt == 0 {
		return fmt.Sprintf("Reopened the deploy of run %s; follow it with `putnami cloud ci view %s`.", resp.ID, resp.ID)
	}
	return fmt.Sprintf("Reopened the deploy of run %s as attempt %d; follow it with `putnami cloud ci view %s`.", resp.ID, attempt, resp.ID)
}

// --- logs ---

func ciLogs(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	run := ciRunID(args)
	if run == "" {
		return clicore.NewError("cloud ci logs requires a run id: `putnami cloud ci logs <run>`", clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	// Resolved ONCE, before either mode runs, so history and the live tail cannot
	// disagree about what `--level` means or about which renderer applies. An
	// unusable level is refused here rather than by whichever branch happens to
	// look at it.
	view, err := ciLogView(params)
	if err != nil {
		return err
	}
	if clicore.Truthy(clicore.Param(params, "follow")) {
		// The tail is a long-lived stream rather than one request, so it cannot use
		// its own response as the probe: the id is resolved before the connection is
		// opened, and only when it could be an abbreviation. The lookup is
		// best-effort here — an id that needs no expanding must not be blocked by a
		// failed search for one, and the tail request reports the real fault (an
		// expired token, an unknown run) about the id the operator actually named.
		if !ciIsFullRunID(run) {
			if resolved, rerr := ciResolveRunPrefix(reqCtx, ctx, run); rerr == nil {
				run = resolved
			}
		}
		return ciFollowLogs(reqCtx, ctx, run, params, view, ioctx)
	}
	q := ciLogsQuery(params)
	resp, run, err := ciRunRequest(reqCtx, ctx, run, func(id string) (*CIRunLogsResponse, error) {
		return ciGetRunLogs(reqCtx, ctx, "logs", id, q)
	})
	if err != nil {
		return err
	}
	// `--level` is applied HERE, not by the server (see ciLogsQuery), and BEFORE
	// the structured branch: it is a selector, so the machine surface has to honor
	// it exactly as the human one does. Filtering only the human path would make
	// `--output=jsonl --level error` quietly emit everything.
	entries, filtered := view.filter(resp.Entries)
	if clicore.StructuredOutput(params) {
		// D5: one machine object PER LINE for the history — the entries, never a
		// wrapped page. The next-cursor hint goes to stderr so stdout stays pure.
		for i := range entries {
			ioctx.Stdout(ciCompactJSON(entries[i]))
		}
		if resp.NextCursor != "" && ioctx.Stderr != nil {
			ioctx.Stderr("more logs available; re-run with --cursor " + resp.NextCursor)
		}
		return nil
	}
	switch {
	case len(entries) == 0 && filtered > 0:
		// The distinction matters: "nothing matched your filter" sends the reader to
		// a different next command than "this run produced no logs at all", and
		// conflating the two is what made a red run answer "No logs found".
		ioctx.Stdout(fmt.Sprintf("No logs at or above --level %s for run %s (%d entries below it).",
			view.level, run, filtered))
	case len(entries) == 0:
		ioctx.Stdout("No logs found for run " + run + ".")
	}
	for i := range entries {
		ioctx.Stdout(view.render(entries[i]))
	}
	ciRenderDiagnostics(ioctx, resp.Diagnostics)
	if resp.NextCursor != "" {
		ioctx.Stdout("More logs available — re-run with --cursor " + resp.NextCursor + " for the next page.")
	}
	return nil
}

// ciLogsView is how one `ci logs` invocation reads its entries — the `--level`
// threshold and the choice of renderer — resolved once from the flags and then
// used identically by the history page and the live tail.
//
// It exists because the two modes drifted: the tail built its own query and its
// own emit, so it kept forwarding `--level` to a server that filters on the
// severity of the TRANSPORT (every protocol line is LOG), and it kept printing
// raw protocol JSON after the history surface had learned to render it. A single
// resolved value is what makes "the same flags mean the same thing live and after
// the fact" a property of the code rather than of two lists staying in step.
type ciLogsView struct {
	// level is the requested word, kept for the message that explains an empty page.
	level string
	// threshold is the parsed minimum; hasLevel distinguishes "no filter" from
	// "filter at the bottom of the scale".
	threshold ciSeverity
	hasLevel  bool
	raw       bool
}

// ciLogView resolves the view from the flags, refusing an unusable level.
//
// An unrecognized level is a usage error rather than a silently-ignored filter.
// The server used to 400 on one; now that the filter is applied client-side,
// refusing it here is what keeps `--level erro` from quietly printing everything
// and reading as "nothing was filtered out".
func ciLogView(params map[string]any) (ciLogsView, error) {
	view := ciLogsView{raw: clicore.Truthy(clicore.Param(params, "raw"))}
	word := strings.TrimSpace(clicore.StringParam(params, "level"))
	if word == "" {
		return view, nil
	}
	threshold, ok := ciParseLevel(word)
	if !ok {
		return ciLogsView{}, clicore.NewError(
			"--level must be one of trace, debug, info, warn, error, fatal (got "+word+")", clicore.ExitUsage)
	}
	view.level = strings.ToLower(word)
	view.threshold = threshold
	view.hasLevel = true
	return view, nil
}

// keep reports whether one entry passes the `--level` threshold, measured on its
// EFFECTIVE severity (ciEntrySeverity) rather than the severity it arrived with.
func (v ciLogsView) keep(entry CILogEntry) bool {
	return !v.hasLevel || ciEntrySeverity(entry) >= v.threshold
}

// filter returns the surviving entries and how many the threshold removed.
func (v ciLogsView) filter(entries []CILogEntry) ([]CILogEntry, int) {
	if !v.hasLevel {
		return entries, 0
	}
	kept := make([]CILogEntry, 0, len(entries))
	for i := range entries {
		if v.keep(entries[i]) {
			kept = append(kept, entries[i])
		}
	}
	return kept, len(entries) - len(kept)
}

// render formats one entry for the human surface: the decoded protocol line, or
// the verbatim body under --raw.
func (v ciLogsView) render(entry CILogEntry) string {
	if v.raw {
		return ciFormatLogLine(entry)
	}
	return ciRenderLogEntry(entry)
}

// ciLogsQuery translates the log flags into the run-log endpoint's query
// vocabulary (phase/project/q/level/from/to/cursor/limit).
func ciLogsQuery(params map[string]any) url.Values {
	q := url.Values{}
	for _, f := range []struct{ flag, key string }{
		{"phase", "phase"}, {"project", "project"}, {"q", "q"},
		{"from", "from"}, {"to", "to"}, {"cursor", "cursor"},
	} {
		if v := strings.TrimSpace(clicore.StringParam(params, f.flag)); v != "" {
			q.Set(f.key, v)
		}
	}
	// `--level` is DELIBERATELY not forwarded. The server filters on each entry's
	// own severity, and a runner protocol line's severity is the severity of the
	// pipe that carried it — LOG, for every one of them — so forwarding `error`
	// filtered away the whole stream INCLUDING the session record the findings
	// live in, and the page came back empty about a run that had just failed. The
	// filter is applied client-side over the effective severity instead
	// (ciEntrySeverity), which is the severity a reader means.
	if n := clicore.NumberParam(params, "limit"); n != nil {
		q.Set("limit", strconv.Itoa(int(*n)))
	}
	return q
}

// --- logs --follow (generated delivery-api server stream) ---

// ciFollowLogs opens the run-log tail through the generated delivery-api client
// and renders each frame's entry live until the user interrupts (reqCtx cancels
// on Ctrl-C / SIGTERM). The generated stream owns the wire: the SSE framing,
// the frame schema, the heartbeat-driven idle budget and the typed errors.
// What stays here is the re-open policy the framework cannot declare over SSE
// yet: a dropped connection re-opens after
// ciFollowBackoff with the last frame's cursor, bounded by
// ciFollowMaxReconnects consecutive drops that streamed nothing; a connection
// that delivered a frame resets the budget; a provider refusal (4xx/5xx) is
// terminal.
func ciFollowLogs(reqCtx context.Context, ctx *clicore.WorkspaceContext, run string, params map[string]any, view ciLogsView, ioctx clicore.IO) error {
	structured := clicore.StructuredOutput(params)
	emit := func(entry CILogEntry) {
		// The SAME threshold and renderer the history page uses. A tail that filtered
		// or formatted differently would answer the identical command differently
		// depending on whether the run had finished.
		if !view.keep(entry) {
			return
		}
		if structured {
			ioctx.Stdout(ciCompactJSON(entry))
			return
		}
		ioctx.Stdout(view.render(entry))
	}

	// `--level` is NOT forwarded, for the reason ciLogsQuery spells out: the tail
	// filters on the severity each entry ARRIVED with, and a runner protocol line
	// arrives at LOG whatever it reports. Forwarding `error` here dropped the
	// session record the findings live in — the exact failure this change fixed for
	// the history page, which the tail had kept because it built its own query.
	var query deliveryapiclient.GetV1WorkspacesRunsLogsTailQuery
	for flag, field := range map[string]**string{"phase": &query.Phase, "project": &query.Project, "q": &query.Q} {
		if v := strings.TrimSpace(clicore.StringParam(params, flag)); v != "" {
			*field = &v
		}
	}

	api, err := newDeliveryAPIClient(ctx)
	if err != nil {
		return err
	}

	lastCursor := ""
	drops := 0
	for {
		streamed, cursor, done, err := ciFollowOnce(reqCtx, ctx, api, run, query, lastCursor, emit)
		if cursor != "" {
			lastCursor = cursor
		}
		if reqCtx.Err() != nil {
			return nil // clean interrupt
		}
		if err != nil && !ciIsDrop(err) {
			return err // terminal provider refusal — do not reconnect
		}
		if done {
			// The server said the run's log is complete. Reconnecting would only
			// re-open a stream that has nothing left to send, so follow ends here —
			// which is what makes `--follow` on a finished run terminate instead of
			// burning the reconnect budget.
			return nil
		}
		if streamed {
			drops = 0 // a connection that delivered frames earns a fresh budget
		}
		drops++
		if drops > ciFollowMaxReconnects {
			return err // exhausted the reconnect budget (nil on a clean EOF)
		}
		if serr := ciFollowSleep(reqCtx, ciFollowBackoff); serr != nil {
			return nil // canceled during backoff
		}
	}
}

// ciFollowOnce opens one generated tail stream, replaying the resume cursor
// when set, and hands each frame's entry to emit. It returns whether any frame
// arrived, the last frame's cursor (for the next re-open), whether the server
// declared the stream finished, and an error classified for the caller: nil on
// a clean end, a ciDropError for a transport drop or an idle stream
// (re-openable), or a surfaced clicore error for a provider refusal (terminal).
func ciFollowOnce(reqCtx context.Context, ctx *clicore.WorkspaceContext, api *deliveryapiclient.DeliveryClient, run string,
	query deliveryapiclient.GetV1WorkspacesRunsLogsTailQuery, cursor string, emit func(CILogEntry)) (bool, string, bool, error) {
	if cursor != "" {
		query.Cursor = &cursor
	}
	in := deliveryapiclient.GetV1WorkspacesRunsLogsTailInput{
		Path:  deliveryapiclient.GetV1WorkspacesRunsLogsTailPath{Workspace: ctx.WorkspaceID, Run: run},
		Query: query,
	}
	stream, err := clicore.OpenStreamWithSession(reqCtx, ctx, func(callCtx context.Context) (*client.Stream[deliveryapiclient.CIRunLogFrame], error) {
		return api.GetV1WorkspacesRunsLogsTail(callCtx, in)
	})
	if err != nil {
		if reqCtx.Err() != nil {
			return false, "", false, nil
		}
		return false, "", false, ciStreamError(err)
	}
	defer stream.Close() //nolint:errcheck // Close only cancels the stream

	streamed := false
	lastCursor := ""
	done := false
	for frame := range stream.Messages() {
		streamed = true
		if frame.Cursor != nil && *frame.Cursor != "" {
			lastCursor = *frame.Cursor
		}
		if event, ok := frame.Run.Value(); ok && event.Final != nil && *event.Final {
			done = true
		}
		// A frame without an entry carries run state only: it advances the cursor
		// (above) and renders nothing. Rendering a zero entry here would print a
		// blank timestamped line for every run event.
		if entry, ok := frame.Entry.Value(); ok {
			emit(ciLogEntryFrom(entry))
		}
	}
	if serr := stream.Err(); serr != nil && reqCtx.Err() == nil {
		return streamed, lastCursor, done, ciStreamError(serr)
	}
	return streamed, lastCursor, done, nil
}

// ciLogEntryFrom converts a generated log entry into the CILogEntry the history
// page renders, so both surfaces keep one renderer and one level filter. An
// absent member reads as its zero value.
func ciLogEntryFrom(entry deliveryapiclient.LogEntry) CILogEntry {
	return CILogEntry{
		Timestamp:    deref(entry.Timestamp),
		Severity:     ciSeverity(deref(entry.Severity)),
		SeverityText: deref(entry.SeverityText),
		Body:         deref(entry.Body),
		TraceID:      deref(entry.TraceId),
		SpanID:       deref(entry.SpanId),
		Attributes:   deref(entry.Attributes),
	}
}

// ciStreamError classifies a generated-stream failure: a provider refusal is a
// terminal clicore error with the exit code the rest of the CLI uses for that
// status, and everything else (a dropped transport, an idle stream, a frame the
// contract refused) is a re-openable drop.
func ciStreamError(err error) error {
	if clicore.ServiceStatus(err) != 0 {
		return clicore.ServiceCallError(context.Background(), err, clicore.ServiceCallOptions{Prefix: "cloud ci logs tail: "})
	}
	return ciDropError{err: err}
}

// ciFollowSleep blocks for d or until reqCtx is canceled, returning reqCtx.Err()
// on cancellation so the reconnect loop can distinguish an interrupt from the
// timer.
func ciFollowSleep(reqCtx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-reqCtx.Done():
		return reqCtx.Err()
	case <-t.C:
		return nil
	}
}

// ciDropError marks a re-openable failure of the tail stream so ciFollowLogs
// re-opens rather than surfacing it. A provider refusal is deliberately NOT
// wrapped, so it stays terminal.
type ciDropError struct{ err error }

func (e ciDropError) Error() string { return e.err.Error() }
func (e ciDropError) Unwrap() error { return e.err }

func ciIsDrop(err error) bool {
	var d ciDropError
	return errors.As(err, &d)
}

// --- status ---

// ciStatus prints the CI status node. A workspace that cannot be
// resolved, or a session the status read refuses, keeps its error and exit
// code; any other failed read is an unknown status.
func ciStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if extra := clicore.Positionals(args)[1:]; len(extra) > 0 {
		return clicore.NewError("cloud ci status takes no name; got "+strings.Join(extra, " "), clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	node, err := ciStatusNode(ctx, ioctx)
	if err != nil && clicore.ExitCode(err) == clicore.ExitAuth {
		return err
	}
	return clicore.WriteStatus(params, ioctx, node)
}

// --- usage ---

// CIUsageWallSummary mirrors apis.CIUsageWallSummary.
type CIUsageWallSummary struct {
	N       int `json:"n"`
	MinSecs int `json:"minSecs"`
	MaxSecs int `json:"maxSecs"`
	AvgSecs int `json:"avgSecs"`
}

// CIUsageSpend mirrors apis.CIUsageSpend.
type CIUsageSpend struct {
	Currency         string  `json:"currency"`
	Estimated        bool    `json:"estimated"`
	SpotEUR          float64 `json:"spotEur"`
	CloudRunEUR      float64 `json:"cloudRunEur"`
	TotalEUR         float64 `json:"totalEur"`
	RunsWithDuration int     `json:"runsWithDuration"`
}

// CIUsageResponse mirrors apis.CIUsageResponse.
type CIUsageResponse struct {
	Workspace      string                        `json:"workspace"`
	WindowHours    int                           `json:"windowHours"`
	ScannedRuns    int                           `json:"scannedRuns"`
	ScanLowerBound bool                          `json:"scanLowerBound,omitempty"`
	Substrates     map[string]int                `json:"substrates,omitempty"`
	Profiles       map[string]int                `json:"profiles,omitempty"`
	Sources        map[string]int                `json:"sources,omitempty"`
	Outcomes       map[string]int                `json:"outcomes,omitempty"`
	Gate           map[string]CIUsageWallSummary `json:"gate,omitempty"`
	Spend          CIUsageSpend                  `json:"spend"`
}

func ciUsage(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	resp, err := readCIUsage(reqCtx, ctx, strings.TrimSpace(clicore.StringParam(params, "window")))
	if err != nil {
		return err
	}
	if clicore.StructuredOutput(params) {
		ioctx.Stdout(ciCompactJSON(*resp))
		return nil
	}
	ciRenderUsage(ioctx, resp)
	return nil
}

// ciRenderUsage prints the rollup as aligned sections. Empty maps render as a
// single "(none)" line so a quiet window reads as empty rather than broken.
func ciRenderUsage(ioctx clicore.IO, u *CIUsageResponse) {
	header := fmt.Sprintf("CI usage — %s, last %dh (%d runs scanned", u.Workspace, u.WindowHours, u.ScannedRuns)
	if u.ScanLowerBound {
		header += ", lower bound"
	}
	ioctx.Stdout(header + ")")
	ciRenderCountSection(ioctx, "By substrate (where runs actually ran)", u.Substrates)
	ciRenderCountSection(ioctx, "By profile", u.Profiles)
	ciRenderCountSection(ioctx, "By resolution source", u.Sources)
	ciRenderCountSection(ioctx, "By outcome", u.Outcomes)
	if len(u.Gate) > 0 {
		ioctx.Stdout("Gate wall (seconds):")
		for _, k := range ciSortedKeys(u.Gate) {
			g := u.Gate[k]
			ioctx.Stdout(fmt.Sprintf("  %-20s n=%-4d min=%-6d avg=%-6d max=%d", k, g.N, g.MinSecs, g.AvgSecs, g.MaxSecs))
		}
	}
	label := "estimated"
	if !u.Spend.Estimated {
		label = "billed"
	}
	ioctx.Stdout(fmt.Sprintf("Spend (%s, %s, over %d runs with a measured duration):", label, u.Spend.Currency, u.Spend.RunsWithDuration))
	ioctx.Stdout(fmt.Sprintf("  spot=%.2f  cloudrun=%.2f  total=%.2f", u.Spend.SpotEUR, u.Spend.CloudRunEUR, u.Spend.TotalEUR))
}

func ciRenderCountSection(ioctx clicore.IO, title string, counts map[string]int) {
	ioctx.Stdout(title + ":")
	if len(counts) == 0 {
		ioctx.Stdout("  (none)")
		return
	}
	for _, k := range ciSortedKeys(counts) {
		ioctx.Stdout(fmt.Sprintf("  %-20s %d", k, counts[k]))
	}
}

// ciSortedKeys returns a map's keys in a stable order, so a rendered rollup is
// byte-identical across runs regardless of Go's map iteration.
func ciSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- insights ---

// ciInsights prints ONE run's contracted gate report — the
// framework-owned `reportFile` document the CLI emitted for that run's gate and
// the runner uploaded, stored verbatim by the control plane and returned verbatim
// by `GET /v1/workspaces/{ws}/runs/{run}/insights`.
//
// It is the per-run counterpart of `usage`: `usage` answers "what is this
// workspace spending", this answers "where did THIS run's wall and CPU go".
// Until it existed the document was reachable only with curl and a bearer token,
// so the richest evidence a run produces — the CPU balance sheet, the cache
// economics, the scheduler's own critical path — was in the control plane and
// read by nobody, which is the same failure the report itself was built to end.
//
// TWO REQUESTS, DELIBERATELY. The run is fetched first so an abbreviated id
// resolves against the route that actually knows whether a run exists, which is
// what makes the report's own 404 unambiguous: a run this workspace has never
// seen and a run that simply carries no report are different answers, and only
// the second one is worth a suggestion about which runs carry reports.
func ciInsights(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	run := ciRunID(args)
	if run == "" {
		return clicore.NewError("cloud ci insights requires a run id: `putnami cloud ci insights <run>`", clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	runResp, resolved, err := ciRunRequest(reqCtx, ctx, run, func(id string) (*CIRunResponse, error) {
		return ciGetRun(reqCtx, ctx, "insights", id, false)
	})
	if err != nil {
		return err
	}
	if runResp.ID != "" {
		resolved = runResp.ID
	}
	target := ctx.WorkspaceURL("/runs/" + url.PathEscape(resolved) + "/insights")
	report, err := ciCall[protocolcli.ReportFile](reqCtx, ctx, "insights", target,
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.ReportFile, error) {
			return api.GetV1WorkspacesRunsInsights(callCtx, deliveryapiclient.GetV1WorkspacesRunsInsightsInput{
				Path: deliveryapiclient.GetV1WorkspacesRunsInsightsPath{Workspace: ctx.WorkspaceID, Run: resolved},
			})
		})
	if err != nil {
		if ciIsNotFound(err) {
			return clicore.NewError("run "+ciShortRunID(resolved)+" carries no gate report: the runner posts one only for a gate that recorded a session, "+
				"so a run that never dispatched, died before its gate, or ran a CLI older than the report contract has none", clicore.ExitAPI)
		}
		return err
	}
	if clicore.StructuredOutput(params) {
		ioctx.Stdout(ciCompactJSON(*report))
		return nil
	}
	ciRenderInsights(ioctx, runResp, report)
	return nil
}

// ciRenderInsights prints the report as sections, in the order a deep dive reads
// them: what ran, what it cost, then what it found. Every absent block is SKIPPED
// rather than zeroed — the document's own rule is that a fact it could not
// measure is omitted, and rendering "0" for one would invent a measurement.
func ciRenderInsights(ioctx clicore.IO, run *CIRunResponse, r *protocolcli.ReportFile) {
	ioctx.Stdout(fmt.Sprintf("Insights — run %s (%s%s)", ciShortRunID(run.ID), run.Status, ciConclusionSuffix(run.Conclusion)))
	ioctx.Stdout(fmt.Sprintf("  session   %s", r.SessionID))
	if r.StartTime != "" && r.EndTime != "" {
		ioctx.Stdout(fmt.Sprintf("  window    %s → %s", r.StartTime, r.EndTime))
	}
	if r.Git != nil && r.Git.Sha != "" {
		line := "  git       " + ciShortRunID(r.Git.Sha)
		if r.Git.Branch != "" {
			line += " on " + r.Git.Branch
		}
		if r.Git.Baseline != "" {
			line += " (baseline " + r.Git.Baseline + ")"
		}
		ioctx.Stdout(line)
	}
	c := r.Run.Counts
	ioctx.Stdout(fmt.Sprintf("  verdict   %s (exit %d) — %d tasks: %d ok, %d failed, %d canceled, %d skipped",
		r.Run.Outcome, r.Run.ExitCode, c.Total, c.Succeeded, c.Failed, c.Canceled, c.Skipped))
	ioctx.Stdout(fmt.Sprintf("  wall      %s   reuse: local %d, remote %d, coalesced %d",
		ciDurationMs(r.Run.DurationMs), r.Run.Reuse.LocalCache, r.Run.Reuse.RemoteCache, r.Run.Reuse.Coalesced))
	ciRenderInsightsCPU(ioctx, r.Run.CPU)
	if r.Cache != nil {
		ioctx.Stdout(fmt.Sprintf("  cache     %d hits / %d misses, %s saved, %s fetched, %s uploaded",
			r.Cache.Hits, r.Cache.Misses, ciDurationMs(r.Cache.TimeSavedMs),
			ciBytes(r.Cache.BytesFetched), ciBytes(r.Cache.BytesUploaded)))
	}
	if r.Scheduler != nil {
		ioctx.Stdout(fmt.Sprintf("  scheduler parallelism %d, critical path %s",
			r.Scheduler.Parallelism, ciDurationMs(r.Scheduler.CriticalPathMs)))
	}
	ciRenderInsightsCommands(ioctx, r.Commands)
	ciRenderInsightsJobs(ioctx, r)
}

// ciRenderInsightsCPU prints the gate's own balance sheet: what the runner
// granted over this run's wall against what its subprocesses actually burned.
// The utilization percentage is the number every CPU decision downstream is
// taken from, so it is computed here rather than left to the reader.
//
// It states the block's own boundary out loud. `actual` sums the gate's physical
// executions and EXCLUDES the CLI's own process, git, and the test PostgreSQL —
// measured at roughly 70% of the container's cgroup total — so an operator
// comparing it against `ci_cpu_usage_usec` is not surprised by the gap.
func ciRenderInsightsCPU(ioctx clicore.IO, cpu *protocolcli.RunCPU) {
	if cpu == nil {
		ioctx.Stdout("  cpu       (absent — this gate recorded no execution ledger or no runner environment)")
		return
	}
	line := fmt.Sprintf("  cpu       %s actual / %s allocated (%d millicores from %s, %d executions)",
		ciDurationMs(cpu.ActualMs), ciDurationMs(cpu.AllocatedMs), cpu.AllocatedMillicores, cpu.AllocatedSource, cpu.Executions)
	if cpu.AllocatedMs > 0 {
		line += fmt.Sprintf(" — %.0f%% utilized", 100*float64(cpu.ActualMs)/float64(cpu.AllocatedMs))
	}
	ioctx.Stdout(line)
	ioctx.Stdout("            (gate subprocesses only: the CLI, git and the test PostgreSQL are outside it)")
}

func ciRenderInsightsCommands(ioctx clicore.IO, commands []protocolcli.ReportCommand) {
	if len(commands) == 0 {
		return
	}
	ioctx.Stdout("Commands:")
	for i := range commands {
		cmd := commands[i]
		line := fmt.Sprintf("  %-10s %3d tasks, %s fresh wall, %d errors / %d warnings",
			cmd.Command, cmd.Counts.Total, ciDurationMs(cmd.FreshWallMs), cmd.Errors, cmd.Warnings)
		if cmd.Tests != nil {
			line += fmt.Sprintf(" — tests %d/%d passed, %d skipped", cmd.Tests.Passed, cmd.Tests.Total, cmd.Tests.Skipped)
		}
		if cmd.Coverage != nil {
			line += fmt.Sprintf(" — coverage %.1f%%", cmd.Coverage.Percentage)
			if cmd.Coverage.Enforced {
				line += " (enforced)"
			}
		}
		ioctx.Stdout(line)
	}
}

// ciRenderInsightsJobs prints the report's per-job rows, slowest first, and says
// what the PRODUCER's bound left out.
//
// The elision line is not decoration. `reportFile.jobs` is capped at 64 entries
// by the protocol schema itself, and a full-workspace gate selects several
// hundred tasks — so on any real run this list is a sample, and a sample
// presented as a total is the one way this verb could lie. What the bound
// dropped is counted by the producer (`elidedJobs`) precisely so it can be said.
func ciRenderInsightsJobs(ioctx clicore.IO, r *protocolcli.ReportFile) {
	if len(r.Jobs) == 0 && r.ElidedJobs == 0 {
		return
	}
	jobs := make([]protocolcli.ReportJob, len(r.Jobs))
	copy(jobs, r.Jobs)
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].DurationMs > jobs[j].DurationMs })
	ioctx.Stdout(fmt.Sprintf("Jobs (%d of %d selected tasks, slowest first):", len(jobs), len(jobs)+r.ElidedJobs))
	for i := range jobs {
		job := jobs[i]
		line := fmt.Sprintf("  %-9s %-28s %-8s %8s", job.Outcome, job.Project, job.Task, ciDurationMs(job.DurationMs))
		if job.Reuse != "" {
			line += "  " + job.Reuse
		}
		if job.CPUMs > 0 {
			line += fmt.Sprintf("  cpu %s", ciDurationMs(job.CPUMs))
		}
		ioctx.Stdout(line)
		for _, d := range job.Diagnostics {
			ioctx.Stdout("      " + ciDiagnosticLine(d))
		}
		if job.TruncatedCount > 0 {
			ioctx.Stdout(fmt.Sprintf("      … %d more diagnostic(s) dropped by the report's per-task bound", job.TruncatedCount))
		}
	}
	if r.ElidedJobs > 0 {
		ioctx.Stdout(fmt.Sprintf("  … %d further task(s) reported no row: the gate report's 64-task bound left them out "+
			"(a PRODUCER-side protocol limit, not a control-plane one). The command counts above still include their "+
			"diagnostics, and `cloud ci logs` carries the unbounded per-task stream.", r.ElidedJobs))
	}
}

// --- timings ---

// ciTimingsDefaultLimit is how many task rows `timings` prints by default. The
// server's table is bounded at 4096 rows and a full Cloud gate fills about 745
// of them, so an unlimited default would be a screenful of scrollback for every
// invocation. `--limit 0` prints the whole table.
const ciTimingsDefaultLimit = 40

// CIRunTimingsResponse twins the control plane's per-task timing table
// (the delivery API's CIRunTimingsResponse).
//
// It is a SEPARATE surface from `insights`, and deliberately so: `reportFile` is
// a closed document (`additionalProperties: false`) whose job list is capped at
// 64, so per-task absolute start and end cannot ride inside it — and they are
// exactly what reconstructing a run's critical path needs, because a session's
// task records carry durations and no wall clock.
type CIRunTimingsResponse struct {
	RunID          string            `json:"runId"`
	SessionID      string            `json:"sessionId,omitempty"`
	Tasks          []CIRunTimingTask `json:"tasks"`
	ElidedTasks    int               `json:"elidedTasks"`
	MaxTasks       int               `json:"maxTasks"`
	MalformedLines int               `json:"malformedLines"`
}

// CIRunTimingTask is one row of the timing table.
//
// InputDigest is the cache key the scheduler computed for the task. It is absent
// for a task the scheduler does not cache, and on a table the control plane
// derived before it recorded the key.
type CIRunTimingTask struct {
	Key         string `json:"key"`
	Project     string `json:"project"`
	Task        string `json:"task"`
	Command     string `json:"command"`
	Status      string `json:"status"`
	Reuse       string `json:"reuse"`
	Start       string `json:"start,omitempty"`
	End         string `json:"end,omitempty"`
	DurationMs  int64  `json:"durationMs"`
	InputDigest string `json:"inputDigest,omitempty"`
	ExecutionID string `json:"executionId,omitempty"`
	CPUMs       *int64 `json:"cpuMs,omitempty"`
}

// ciTimings prints ONE run's per-task timeline.
//
// It answers the question `insights` cannot: not "what did this run cost" but
// "WHEN did each task run". The 2026-09-11 analysis that found the critical path
// — one `go test` process for 21 to 37 libraries, 222 to 338 seconds inside the
// graph against 26 alone, starting late because a cold run gives the scheduler no
// duration history — had to replay the task graph locally, because the run log
// keeps a bounded sample of the task records and the session record died with the
// Spot VM. This verb is what makes that a read instead of a replay.
//
// With `--against <other-run>` it answers a third question instead: WHY each
// task of this run executed rather than being served by the cache (see
// ciCompareTimings).
func ciTimings(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	run := ciRunID(args)
	if run == "" {
		return clicore.NewError("cloud ci timings requires a run id: `putnami cloud ci timings <run>`", clicore.ExitUsage)
	}
	limit, explicit, err := ciTimingsLimit(params)
	if err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	runResp, timings, err := ciLoadTimings(reqCtx, ctx, run)
	if err != nil {
		return err
	}
	if against := strings.TrimSpace(clicore.StringParam(params, "against")); against != "" {
		_, baseline, err := ciLoadTimings(reqCtx, ctx, against)
		if err != nil {
			return err
		}
		comparison, err := ciCompareTimings(timings, baseline)
		if err != nil {
			return err
		}
		if clicore.StructuredOutput(params) {
			if explicit {
				ciTruncateTimingCauses(comparison, limit)
			}
			ioctx.Stdout(ciCompactJSON(*comparison))
			return nil
		}
		ciRenderTimingCauses(ioctx, comparison, limit)
		return nil
	}
	if clicore.StructuredOutput(params) {
		// `--limit` is HONORED here, not parsed and dropped. It applies only when
		// the caller supplied it: a machine consumer asking for the document should
		// get the document, so the human path's 40-row default does not reach this
		// one. What the client's own bound drops is folded into `elidedTasks`,
		// whose stated meaning is "task terminals this list leaves out" — so
		// len(tasks)+elidedTasks stays the run's terminal count either way, and
		// `maxTasks` still names the SERVER's bound for a reader who wants the
		// split.
		if explicit {
			ciTruncateTimings(timings, limit)
		}
		ioctx.Stdout(ciCompactJSON(*timings))
		return nil
	}
	ciRenderTimings(ioctx, runResp, timings, limit)
	return nil
}

// ciLoadTimings reads one run and its timing table.
//
// TWO REQUESTS, DELIBERATELY, exactly as `insights` does it: the run is fetched
// first so an abbreviated id resolves against the route that knows whether a run
// exists, which is what makes the table's own 404 unambiguous.
func ciLoadTimings(reqCtx context.Context, ctx *clicore.WorkspaceContext, run string) (*CIRunResponse, *CIRunTimingsResponse, error) {
	runResp, resolved, err := ciRunRequest(reqCtx, ctx, run, func(id string) (*CIRunResponse, error) {
		return ciGetRun(reqCtx, ctx, "timings", id, false)
	})
	if err != nil {
		return nil, nil, err
	}
	if runResp.ID != "" {
		resolved = runResp.ID
	}
	target := ctx.WorkspaceURL("/runs/" + url.PathEscape(resolved) + "/insights/timings")
	timings, err := ciCall[CIRunTimingsResponse](reqCtx, ctx, "timings", target,
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIRunTimingsResponse, error) {
			return api.GetV1WorkspacesRunsInsightsTimings(callCtx, deliveryapiclient.GetV1WorkspacesRunsInsightsTimingsInput{
				Path: deliveryapiclient.GetV1WorkspacesRunsInsightsTimingsPath{Workspace: ctx.WorkspaceID, Run: resolved},
			})
		})
	if err != nil {
		if ciIsNotFound(err) {
			return nil, nil, clicore.NewError("run "+ciShortRunID(resolved)+" carries no per-task timings: the control plane derives them from the "+
				"event stream the runner ships, so a run that never dispatched, died before its gate, or ran a runner older than the "+
				"session-record transport has none", clicore.ExitAPI)
		}
		return nil, nil, err
	}
	if timings.RunID == "" {
		timings.RunID = resolved
	}
	return runResp, timings, nil
}

// ciTruncateTimings applies a client-side row bound and accounts for what it
// dropped, so the emitted document's own arithmetic stays true.
func ciTruncateTimings(t *CIRunTimingsResponse, limit int) {
	if limit <= 0 || limit >= len(t.Tasks) {
		return
	}
	t.ElidedTasks += len(t.Tasks) - limit
	t.Tasks = t.Tasks[:limit]
}

// ciTimingsLimit resolves how many task rows `timings` prints, and whether the
// caller asked at all.
//
// A value that is PRESENT but unusable is refused rather than folded into the
// default, for the same reason `list` refuses one: a silently ignored flag is
// indistinguishable from an honored one. The second result exists so
// `--output=json` can tell "no bound asked for" from "bound of 40", and emit the
// whole document in the first case.
func ciTimingsLimit(params map[string]any) (int, bool, error) {
	value := clicore.Param(params, "limit")
	if value == nil {
		return ciTimingsDefaultLimit, false, nil
	}
	n := clicore.NumberParam(params, "limit")
	if n == nil || *n != math.Trunc(*n) || *n < 0 {
		return 0, false, clicore.NewError(fmt.Sprintf(
			"--limit must be a non-negative whole number of tasks (got %v); --limit 0 prints every row", value),
			clicore.ExitUsage)
	}
	return int(*n), true, nil
}

// ciRenderTimings prints the table longest wall first, which is the order a
// critical-path reading needs: the chain is made of the long tasks, and the rows
// the server's own bound dropped are the short ones.
//
// It says what it does NOT know, twice. A task that emitted no start record has
// no start — a cache hit, a coalesced result, a skipped task — and the instant is
// never back-computed from end minus duration, because a derived instant is not
// a measured one. And a task whose execution the session did not measure has no
// CPU share rather than a zero, which would read as "ran and cost nothing".
func ciRenderTimings(ioctx clicore.IO, run *CIRunResponse, t *CIRunTimingsResponse, limit int) {
	ioctx.Stdout(fmt.Sprintf("Timings — run %s (%s%s)", ciShortRunID(run.ID), run.Status, ciConclusionSuffix(run.Conclusion)))
	if t.SessionID != "" {
		ioctx.Stdout(fmt.Sprintf("  session   %s", t.SessionID))
	}
	total := len(t.Tasks) + t.ElidedTasks
	line := fmt.Sprintf("  tasks     %d of %d recorded terminals", len(t.Tasks), total)
	if t.ElidedTasks > 0 {
		line += fmt.Sprintf(" — the control plane's %d-row bound dropped the %d shortest", t.MaxTasks, t.ElidedTasks)
	}
	ioctx.Stdout(line)
	if t.MalformedLines > 0 {
		ioctx.Stdout(fmt.Sprintf("  stream    %d event line(s) were not records: this table is derived from a damaged stream",
			t.MalformedLines))
	}
	if len(t.Tasks) == 0 {
		return
	}
	shown := len(t.Tasks)
	if limit > 0 && limit < shown {
		shown = limit
	}
	ioctx.Stdout(fmt.Sprintf("Tasks (%d of %d, longest wall first):", shown, len(t.Tasks)))
	for i := 0; i < shown; i++ {
		row := t.Tasks[i]
		out := fmt.Sprintf("  %-9s %-34s %-22s %9s", row.Status, row.Project, row.Task, ciDurationMs(row.DurationMs))
		if row.CPUMs != nil {
			out += fmt.Sprintf("  cpu %8s", ciDurationMs(*row.CPUMs))
		} else {
			out += "  cpu        -"
		}
		if row.Reuse != "" && row.Reuse != "none" {
			out += "  " + row.Reuse
		}
		if row.Start != "" {
			out += "  " + row.Start + " → " + row.End
		} else if row.End != "" {
			out += "  ended " + row.End + " (no start record)"
		}
		ioctx.Stdout(out)
	}
	if shown < len(t.Tasks) {
		ioctx.Stdout(fmt.Sprintf("  … %d further row(s) not printed — raise --limit, or --limit 0 for the whole table.",
			len(t.Tasks)-shown))
	}
}

// ciDiagnosticLine renders one protocol diagnostic as `severity file:line:col
// message (code)`, dropping each part the producer could not resolve — an
// unanchored diagnostic prints its message rather than a fabricated `:0`.
func ciDiagnosticLine(d protocolcli.Diagnostic) string {
	out := d.Severity
	if d.File != "" {
		out += " " + d.File
		if d.Line > 0 {
			out += ":" + strconv.Itoa(d.Line)
			if d.Column > 0 {
				out += ":" + strconv.Itoa(d.Column)
			}
		}
	}
	if message := strings.TrimSpace(d.Message); message != "" {
		out += " " + message
	}
	if code := strings.TrimSpace(d.Code); code != "" {
		out += " (" + code + ")"
	}
	return out
}

// ciConclusionSuffix renders a run's conclusion beside its status, or nothing
// when it has none yet.
func ciConclusionSuffix(conclusion string) string {
	if conclusion = strings.TrimSpace(conclusion); conclusion == "" {
		return ""
	}
	return "/" + conclusion
}

// ciDurationMs renders a millisecond duration the way an operator reads a gate:
// sub-minute in seconds, above that in minutes and seconds.
func ciDurationMs(ms int64) string {
	if ms <= 0 {
		return "0s"
	}
	seconds := ms / 1000
	if seconds < 60 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
}

// ciBytes renders a byte count in the largest unit that keeps it readable.
func ciBytes(n int64) string {
	switch {
	case n <= 0:
		return "0B"
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKiB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.2fGiB", float64(n)/(1024*1024*1024))
	}
}

// --- repair ---

func ciRepair(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	reason, err := ciReason(params)
	if err != nil {
		return err
	}
	if err := ciConfirm(
		params,
		ioctx,
		"Repair stale CI rows and terminal checks in this workspace (no redispatch)?",
	); err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	resp, err := ciCall[CIRepairResult](reqCtx, ctx, "repair", ctx.WorkspaceURL("/ci/repair"),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIRepairResult, error) {
			return api.CreateV1WorkspacesCiRepair(callCtx, deliveryapiclient.CreateV1WorkspacesCiRepairInput{
				Path: deliveryapiclient.CreateV1WorkspacesCiRepairPath{Workspace: ctx.WorkspaceID},
				Body: deliveryapiclient.CIRepairRequest{Reason: reason},
			})
		})
	if err != nil {
		return err
	}
	message := fmt.Sprintf(
		"CI repair for workspace %s: reaped %d run(s) and %d phase(s), requested %d compute cancellation(s), scanned %d run(s), and targeted %d terminal run(s) for check reconciliation.",
		clicore.FirstString(resp.Workspace, ctx.WorkspaceID),
		resp.ReapedRuns,
		resp.ReapedPhases,
		resp.CancellationRequests,
		resp.ScannedRuns,
		resp.ReconciledRuns,
	)
	if resp.ScanLowerBound {
		message += " The historical scan hit its safety cap; scanned/reconciled counts are lower bounds."
	}
	clicore.WriteResult(resp, params, ioctx, message)
	return nil
}

// --- pipeline: pause / drain / resume ---

func ciPipeline(params map[string]any, mode, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	reason, err := ciReason(params)
	if err != nil {
		return err
	}
	body := PipelineModeRequest{
		Mode:          mode,
		Reason:        reason,
		CancelRunning: clicore.BoolParam(params, false, "cancel-running", "cancelRunning"),
		ReviewRef:     strings.TrimSpace(clicore.StringParam(params, "review-ref", "reviewRef")),
		IncidentRef:   strings.TrimSpace(clicore.StringParam(params, "incident-ref", "incidentRef")),
	}
	if err := ciConfirm(params, ioctx, fmt.Sprintf("Set the workspace CI pipeline to %s?", mode)); err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	// D4: the WORKSPACE pipeline route only (delivery.run.manage). pause/drain/
	// resume return 200.
	resp, err := ciCall[PipelineModeResponse](reqCtx, ctx, "pipeline", ctx.WorkspaceURL("/pipeline"),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.PipelineModeResponse, error) {
			request := deliveryapiclient.PipelineModeRequest{Mode: body.Mode, Reason: body.Reason}
			if body.CancelRunning {
				request.CancelRunning = &body.CancelRunning
			}
			if body.ReviewRef != "" {
				request.ReviewRef = &body.ReviewRef
			}
			if body.IncidentRef != "" {
				request.IncidentRef = &body.IncidentRef
			}
			return api.CreateV1WorkspacesPipeline(callCtx, deliveryapiclient.CreateV1WorkspacesPipelineInput{
				Path: deliveryapiclient.CreateV1WorkspacesPipelinePath{Workspace: ctx.WorkspaceID},
				Body: request,
			})
		})
	if err != nil {
		return err
	}
	if clicore.StructuredOutput(params) {
		// D2: emit ONLY the raw PipelineModeResponse (no synthesized consequence
		// field) so the machine shape stays stable.
		ioctx.Stdout(ciCompactJSON(*resp))
		return nil
	}
	ioctx.Stdout(fmt.Sprintf("Pipeline for workspace %s set to %s by %s.", clicore.FirstString(resp.Workspace, ctx.WorkspaceID), resp.Mode, resp.Actor))
	for _, id := range resp.CanceledRuns {
		ioctx.Stdout("  canceled running run " + id)
	}
	// D2: a short, factual GitHub-ruleset consequence note (the response carries
	// no consequence field). Derived from the real ModePaused/ModeDraining
	// semantics and the aggregate "Putnami CI" check relationship.
	for _, line := range ciPipelineConsequenceNote(resp.Mode) {
		ioctx.Stdout("  " + line)
	}
	return nil
}

// ciPipelineConsequenceNote returns the factual GitHub-ruleset consequence lines
// for a pipeline mode (D2), derived from the pipelinemodes ModePaused/ModeDraining
// semantics and the required aggregate "Putnami CI" check relationship — no
// overclaiming. It ends by pointing at the live status surface.
func ciPipelineConsequenceNote(mode string) []string {
	switch mode {
	case "paused":
		return []string{
			"Paused: no new runs open and no compute starts, so the required \"Putnami CI\" check is not produced for new pushes.",
			"PRs gated on it stay pending until you resume.",
			"Queued runs stay queued, and pushes received while paused are held: resume runs each held head that is still current.",
			"See `putnami cloud ci status` for the live ruleset-enforcement dimension.",
		}
	case "draining":
		return []string{
			"Draining: no new compute starts; in-flight runs settle to their conclusions, and queued runs wait for resume.",
			"See `putnami cloud ci status` for the live ruleset-enforcement dimension.",
		}
	default: // active (resume)
		return []string{
			"Active: normal operation restored — runs open and compute starts.",
			"Queued runs start, and each push held while paused gets a run if it is still the head of its pull request or branch.",
			"See `putnami cloud ci status` for the live ruleset-enforcement dimension.",
		}
	}
}

// --- shared request + output helpers ---

// ciRequestContext wires Ctrl-C / SIGTERM to context cancellation so a hangup
// during a request (or a live --follow stream) tears the connection down promptly
// rather than orphaning sockets (mirrors the observability query commands). It
// derives from the caller's context when one is set, so `putnami cloud status`
// can bound and cancel a probe.
func ciRequestContext(ioctx clicore.IO) (context.Context, context.CancelFunc) {
	base := ioctx.Context
	if base == nil {
		base = context.Background()
	}
	return signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
}

// ciCall issues one generated delivery-api call through
// clicore.CallWithSession (the single 401 re-mint) and projects the generated
// response onto the CLI's own wire twin T. The generated struct's JSON tags are
// the provider's wire contract, so the marshal/unmarshal round trip changes
// nothing T's decoder reads. A refusal renders through ciServerErrorMessage
// with the provider's own reason, which the CLI binding carries onto the
// generated error (delivery writes it in the envelope's message), else the status line; the status-keyed annotations (409 gate,
// 404 unknown run, 412 permanent) follow it.
func ciCall[T any, R any](reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, target string,
	call func(context.Context, *deliveryapiclient.DeliveryClient) (*R, error),
) (*T, error) {
	api, err := newDeliveryAPIClient(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := clicore.CallWithSession(reqCtx, ctx, func(callCtx context.Context) (*R, error) {
		return call(callCtx, api)
	})
	if err != nil {
		return nil, ciCallError(reqCtx, verb, target, err)
	}
	var out T
	if resp != nil {
		data, merr := json.Marshal(resp)
		if merr == nil {
			merr = json.Unmarshal(data, &out)
		}
		if merr != nil {
			return nil, ciInvalidResponse(verb)
		}
	}
	return &out, nil
}

// ciCallError maps a failed generated call onto the same errors ciRequest
// renders: a provider refusal becomes a ciStatusError (so the run-id fallback
// still sees a 404), a canceled call reads "cloud ci <verb> canceled by user",
// an answer outside the contract reads as an invalid response, and anything
// else is a transport failure naming the target.
func ciCallError(reqCtx context.Context, verb, target string, err error) error {
	if status := clicore.ServiceStatus(err); status != 0 {
		return ciServerError(verb, clicore.ServiceMessage(err), status)
	}
	if reqCtx.Err() != nil {
		return clicore.NewError("cloud ci "+verb+" canceled by user", clicore.ExitUsage)
	}
	if perrors.Is(err, client.CodeClientResponse) {
		return ciInvalidResponse(verb)
	}
	return clicore.NewError(fmt.Sprintf("request failed for %s: %s", target, err.Error()), clicore.ExitAPI)
}

// ciInvalidResponseError marks an answer outside the provider contract, so a
// caller with its own wording for that case (readCISubscription) can tell it
// from a refusal or a transport failure without matching message text.
type ciInvalidResponseError struct{ err error }

func (e ciInvalidResponseError) Error() string { return e.err.Error() }
func (e ciInvalidResponseError) Unwrap() error { return e.err }

func ciInvalidResponse(verb string) error {
	return ciInvalidResponseError{err: clicore.NewError("cloud ci "+verb+": invalid JSON response from the control plane", clicore.ExitAPI)}
}

// ciListRuns reads one page of the workspace's run list.
func ciListRuns(reqCtx context.Context, ctx *clicore.WorkspaceContext, query deliveryapiclient.GetV1WorkspacesRunsQuery) (*CIRunListResponse, error) {
	return ciCall[CIRunListResponse](reqCtx, ctx, "list", ctx.WorkspaceURL("/runs")+"?"+ciRunsQueryValues(query).Encode(),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIRunListResponse, error) {
			return api.GetV1WorkspacesRuns(callCtx, deliveryapiclient.GetV1WorkspacesRunsInput{
				Path:  deliveryapiclient.GetV1WorkspacesRunsPath{Workspace: ctx.WorkspaceID},
				Query: query,
			})
		})
}

// ciRunsQueryValues renders a run list query for the error target, so a failed
// request names the filters it carried.
func ciRunsQueryValues(query deliveryapiclient.GetV1WorkspacesRunsQuery) url.Values {
	q := url.Values{}
	for key, value := range map[string]*string{
		"repo": query.Repo, "branch": query.Branch, "ref": query.Ref, "status": query.Status,
		"phase": query.Phase, "createdAfter": query.CreatedAfter, "idPrefix": query.IdPrefix,
	} {
		if value != nil {
			q.Set(key, *value)
		}
	}
	for key, value := range map[string]*int64{"pullRequest": query.PullRequest, "limit": query.Limit, "offset": query.Offset} {
		if value != nil {
			q.Set(key, strconv.FormatInt(*value, 10))
		}
	}
	return q
}

// ciGetRun reads one run by id. detail asks for the members only `view`
// renders: each failed task's error and output, and the session record's
// delivery account. A host older than them declares no query and
// answers without them, which `view` renders as before.
func ciGetRun(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, id string, detail bool) (*CIRunResponse, error) {
	var query deliveryapiclient.GetV1WorkspacesWorkspaceRunsRunQuery
	if detail {
		query.Detail = &detail
	}
	return ciCall[CIRunResponse](reqCtx, ctx, verb, ctx.WorkspaceURL("/runs/"+url.PathEscape(id)),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIRunResponse, error) {
			return api.GetV1WorkspacesWorkspaceRunsRun(callCtx, deliveryapiclient.GetV1WorkspacesWorkspaceRunsRunInput{
				Path:  deliveryapiclient.GetV1WorkspacesWorkspaceRunsRunPath{Workspace: ctx.WorkspaceID, Run: id},
				Query: query,
			})
		})
}

// ciGetRunLogs reads one page of a run's log history with the filters
// ciLogsQuery built.
func ciGetRunLogs(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, id string, q url.Values) (*CIRunLogsResponse, error) {
	target := ctx.WorkspaceURL("/runs/" + url.PathEscape(id) + "/logs")
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	query := ciLogsInputQuery(q)
	return ciCall[CIRunLogsResponse](reqCtx, ctx, verb, target,
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIRunLogsResponse, error) {
			return api.GetV1WorkspacesRunsLogs(callCtx, deliveryapiclient.GetV1WorkspacesRunsLogsInput{
				Path:  deliveryapiclient.GetV1WorkspacesRunsLogsPath{Workspace: ctx.WorkspaceID, Run: id},
				Query: query,
			})
		})
}

// ciLogsInputQuery carries ciLogsQuery's filters onto the generated query.
func ciLogsInputQuery(q url.Values) deliveryapiclient.GetV1WorkspacesRunsLogsQuery {
	var query deliveryapiclient.GetV1WorkspacesRunsLogsQuery
	for key, field := range map[string]**string{
		"phase": &query.Phase, "project": &query.Project, "q": &query.Q,
		"from": &query.From, "to": &query.To, "cursor": &query.Cursor,
	} {
		if v := q.Get(key); v != "" {
			*field = &v
		}
	}
	if n, err := strconv.ParseInt(q.Get("limit"), 10, 64); err == nil {
		query.Limit = &n
	}
	return query
}

// ciServerError renders a disallowed status as an honest, specific error. It
// surfaces the server's own message (delivery routes carry a real reason: "no run
// opened: the pipeline is paused or the change was gated", "sha is not a known
// pushed commit…") and, for the three operator-relevant conditions, appends a
// factual clarification. Machine-token / missing-manage-scope 403s surface the
// server message verbatim.
// ciStatusError carries the HTTP status alongside the user-facing error, so the
// run-id fallback can recognize the ONE status that means "this might be an
// abbreviation" without matching on message text. It delegates both Error and
// Unwrap to the wrapped error, so the message printed and the exit code resolved
// (clicore.ExitCode uses errors.As) are byte-identical to before it existed.
type ciStatusError struct {
	err    error
	status int
}

func (e ciStatusError) Error() string { return e.err.Error() }
func (e ciStatusError) Unwrap() error { return e.err }

// ciIsNotFound reports whether err is a 404 from the control plane.
func ciIsNotFound(err error) bool {
	var se ciStatusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

func ciServerError(verb, message string, status int) error {
	return ciStatusError{err: ciServerErrorMessage(verb, message, status), status: status}
}

func ciServerErrorMessage(verb, message string, status int) error {
	if message == "" {
		message = clicore.StatusLine(status)
	}
	switch status {
	case http.StatusUnauthorized:
		return clicore.NewError("cloud ci "+verb+": "+message, clicore.ExitAuth)
	case http.StatusConflict:
		return clicore.NewError("cloud ci "+verb+": "+message+" (409 — CI is paused/draining or the change was policy-gated; see `putnami cloud ci status`)", clicore.ExitAPI)
	case http.StatusNotFound:
		return clicore.NewError("cloud ci "+verb+": "+message+" (404 — unknown run, or a SHA that is not a pushed commit on a repository bound to this workspace)", clicore.ExitAPI)
	case http.StatusPreconditionFailed:
		// A precondition the same request can never meet: attempt-id exhaustion,
		// an unreadable trusted workflow, or a caller fault such as a
		// missing branch. The server message says which and how to get
		// out; the CLI only adds that re-running this command will not work.
		return clicore.NewError("cloud ci "+verb+": "+message+" (412 — not a transient condition: re-running the same command cannot change it)", clicore.ExitAPI)
	default:
		return clicore.NewError("cloud ci "+verb+": "+message, clicore.ExitAPI)
	}
}

// ciReason reads the required --reason for the mutating verbs. An absent/blank
// reason is a usage error (never a silently-empty audit reason).
func ciReason(params map[string]any) (string, error) {
	reason := strings.TrimSpace(clicore.StringParam(params, "reason"))
	if reason == "" {
		return "", clicore.NewError("this action requires --reason <why> (recorded as the audit reason)", clicore.ExitUsage)
	}
	return reason, nil
}

// ciConfirm prompts before a mutating action unless --yes/--force is set. A
// declined or non-interactive prompt aborts with ExitUsage (—yes required),
// mirroring the secrets reveal-approval contract.
func ciConfirm(params map[string]any, ioctx clicore.IO, action string) error {
	if clicore.Truthy(clicore.Param(params, "yes", "force")) {
		return nil
	}
	if ioctx.Confirm == nil {
		return clicore.NewError("this action requires --yes in a non-interactive environment", clicore.ExitUsage)
	}
	answer, ok := ioctx.Confirm(action + " [y/N]: ")
	if !ok {
		return clicore.NewError("this action requires --yes in a non-interactive environment", clicore.ExitUsage)
	}
	if strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
		return nil
	}
	return clicore.NewError("aborted", clicore.ExitUsage)
}

// ciWriteRun emits a single run response: the shared protocol Result envelope
// under structured output, else the human summary line.
func ciWriteRun(params map[string]any, ioctx clicore.IO, run *CIRunResponse, message string) {
	clicore.WriteResult(run, params, ioctx, message)
}

// ciRunID returns the run positional (the second positional after the verb), or
// "" when absent. It respects the flag/value scanning FirstPositional uses so a
// `--reason <why>` value is never mistaken for the run id.
func ciRunID(args []string) string {
	positionals := ciPositionals(args)
	if len(positionals) < 2 {
		return ""
	}
	return positionals[1]
}

// ciShortRunIDLength is how much of a run id the list column prints, and the
// minimum a prefix must carry to be resolved. Run ids are 64 hex characters —
// wider than the SHA column beside them, which the same table already truncates
// to 12 — so a full id turns every row into mostly-id and forces the reader to
// copy 64 characters into the next command they type. Twelve hex characters is
// the same width git settled on for a short SHA, and collisions inside ONE
// workspace's run history are checked for rather than assumed away.
const ciShortRunIDLength = 12

// ciShortRunID truncates a run id for display.
func ciShortRunID(id string) string {
	if len(id) <= ciShortRunIDLength {
		return id
	}
	return id[:ciShortRunIDLength]
}

// ciRunRequest performs one run-scoped request, and expands an ABBREVIATED run id
// only if it has to.
//
// The ordering is the whole point. The literal argument is sent first, so a full
// id — and an id that is simply short — costs exactly the request the verb was
// always going to make, and no client-side index is consulted at all. Only a 404
// (the one answer that means "this might not be an id") starts a prefix search,
// and the resolved id is then retried once. Nothing was mutated by a 404, so the
// retry is safe for `cancel` and `retry` as well as for the reads.
//
// A 404 that resolves to nothing returns the ORIGINAL error, because "no run
// 3cb50fe5" is a better answer than anything this client could say about its own
// search.
func ciRunRequest[T any](reqCtx context.Context, ctx *clicore.WorkspaceContext, run string,
	do func(id string) (*T, error),
) (*T, string, error) {
	resp, err := do(run)
	if err == nil || !ciIsNotFound(err) || ciIsFullRunID(run) {
		return resp, run, err
	}
	resolved, rerr := ciResolveRunPrefix(reqCtx, ctx, run)
	if rerr != nil {
		return nil, run, rerr
	}
	if resolved == run {
		return nil, run, err
	}
	resp, err = do(resolved)
	return resp, resolved, err
}

// ciResolveRunPrefix matches an abbreviated run id against the workspace's run
// window.
//
// AMBIGUITY is the only failure it reports. A prefix matching several runs is an
// error naming the candidates, never a silent pick of the newest, because the
// verbs it feeds include `cancel` and `retry` and guessing which run an operator
// meant is not a recoverable mistake.
//
// Everything else — no match, an unreadable window — returns the input unchanged
// so the CALLER's original error stands. That is deliberate: this function runs
// only after the by-id route has already answered about the literal id, and "no
// run ghost" is a better answer to give than a complaint about the search this
// client went on afterwards.
//
// There is deliberately NO minimum prefix length: the id format is the server's
// to choose, not this client's to assume.
func ciResolveRunPrefix(reqCtx context.Context, ctx *clicore.WorkspaceContext, run string) (string, error) {
	prefix := strings.ToLower(strings.TrimSpace(run))
	if prefix == "" {
		return run, nil
	}
	// Two matches are enough to call the prefix ambiguous, so the server is asked
	// for at most two runs that start with it: one request, whatever the
	// size of the history.
	var matches []string
	collect := func(page []CIRunResponse) {
		for i := range page {
			if len(matches) < 2 && strings.HasPrefix(strings.ToLower(page[i].ID), prefix) {
				matches = append(matches, page[i].ID)
			}
		}
	}
	two, first := int64(2), int64(0)
	resp, err := ciListRuns(reqCtx, ctx, deliveryapiclient.GetV1WorkspacesRunsQuery{IdPrefix: &prefix, Limit: &two, Offset: &first})
	if err != nil {
		return run, nil
	}
	collect(resp.Runs)
	// A server older than the idPrefix filter ignores it and answers the
	// newest runs, without saying it applied the filter. Only then does the
	// client walk the history itself, and it still stops at the second match.
	if resp.Filter == nil || resp.Filter.IDPrefix == "" {
		offset := resp.NextOffset
		for page := 0; offset != 0 && len(matches) < 2 && page < ciListMaxPages; page++ {
			limit, pageOffset := int64(ciListPageLimit), int64(offset)
			resp, err := ciListRuns(reqCtx, ctx, deliveryapiclient.GetV1WorkspacesRunsQuery{Limit: &limit, Offset: &pageOffset})
			if err != nil {
				return run, nil
			}
			collect(resp.Runs)
			offset = resp.NextOffset
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return run, nil
	default:
		return "", clicore.NewError(fmt.Sprintf(
			"run id %q is ambiguous — at least %d runs start with it (%s…); give more characters",
			run, len(matches), strings.Join(ciShortenAll(matches, 3), ", ")), clicore.ExitUsage)
	}
}

// ciFullRunIDLength is the length of a complete run id as the control plane mints
// it (a hex-encoded SHA-256). An argument of exactly that shape can never be an
// abbreviation, so a 404 about one is final.
const ciFullRunIDLength = 64

// ciIsFullRunID reports whether an argument is already a complete run id, which
// is the only case that can skip resolution: a 64-character string that is not
// hex is not an id this client should assume anything about.
func ciIsFullRunID(run string) bool {
	if len(run) != ciFullRunIDLength {
		return false
	}
	for i := 0; i < len(run); i++ {
		c := run[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// ciShortenAll renders up to max ids at display width, for an error that has to
// show the candidates without printing four lines of hex.
func ciShortenAll(ids []string, max int) []string {
	if len(ids) > max {
		ids = ids[:max]
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, ciShortRunID(id))
	}
	return out
}

// ciPositionals collects the non-flag positionals from args, skipping the
// --putnamiContext plumbing pair and flag values, mirroring FirstPositional's
// scan so `cloud ci retry <run> --reason "x"` yields [retry, run] and never
// captures a flag value.
func ciPositionals(argv []string) []string {
	var out []string
	skipNext := false
	for i, raw := range argv {
		if skipNext {
			skipNext = false
			continue
		}
		if raw == "--putnamiContext" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(raw, "--") {
			name := strings.TrimPrefix(raw, "--")
			if strings.Contains(name, "=") || strings.HasPrefix(name, "no-") || clicore.IsBooleanFlag(name) {
				continue
			}
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
				skipNext = true
			}
			continue
		}
		out = append(out, raw)
	}
	return out
}

// ciCompactJSON marshals one item onto a single line for the JSONL surface.
// Because the item is the local twin of the server DTO, this reproduces the wire
// bytes (declaration-order keys, typed numbers).
func ciCompactJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// deref reads an optional generated-client member, or its zero value when the
// provider left it out.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func ciConclusionOrStatus(run *CIRunResponse) string {
	if run.Conclusion != "" {
		return run.Conclusion
	}
	return run.Status
}

// --- human renderers ---

func ciRenderRunList(ioctx clicore.IO, runs []CIRunResponse) {
	if len(runs) == 0 {
		ioctx.Stdout("No CI runs matched.")
		return
	}
	headers := []string{"RUN", "STATUS", "CONCLUSION", "REPO", "BRANCH", "SHA", "PR", "CREATED"}
	rows := make([][]string, 0, len(runs))
	for i := range runs {
		r := runs[i]
		pr := ""
		if r.PullRequest > 0 {
			pr = "#" + strconv.Itoa(r.PullRequest)
		}
		rows = append(rows, []string{
			ciShortRunID(r.ID), r.Status, dash(r.Conclusion), dash(r.Repo), dash(r.Branch),
			shortSHA(r.SHA), dash(pr), ciFormatTime(r.CreatedAt),
		})
	}
	ciRenderTable(ioctx, headers, rows)
}

func ciRenderRunDetail(ioctx clicore.IO, run *CIRunResponse) {
	ioctx.Stdout("Run:        " + run.ID)
	ioctx.Stdout("Status:     " + run.Status)
	if run.Conclusion != "" {
		ioctx.Stdout("Conclusion: " + run.Conclusion)
	}
	if run.Repo != "" {
		ioctx.Stdout("Repo:       " + run.Repo)
	}
	if run.Branch != "" {
		ioctx.Stdout("Branch:     " + run.Branch)
	}
	if run.SHA != "" {
		ioctx.Stdout("SHA:        " + run.SHA)
	}
	if run.PullRequest > 0 {
		ioctx.Stdout("PR:         #" + strconv.Itoa(run.PullRequest))
	}
	if run.Provenance != "" {
		ioctx.Stdout("Provenance: " + run.Provenance)
	}
	// WHAT this run actually executed, directly under the refs that name it. On a
	// pull request the two are no longer the same thing.
	if line := ciTestedLine(run); line != "" {
		ioctx.Stdout("Tested:     " + line)
	}
	ioctx.Stdout("Created:    " + ciFormatTime(run.CreatedAt))
	ioctx.Stdout("Updated:    " + ciFormatTime(run.UpdatedAt))
	if line := ciHeartbeatLine(run.Heartbeat); line != "" {
		ioctx.Stdout("Heartbeat:  " + line)
	}
	if line := ciTimingLine(run.Timing); line != "" {
		ioctx.Stdout("Timing:     " + line)
	}
	if line := ciDiskLine(run.Disk); line != "" {
		ioctx.Stdout("Disk:       " + line)
	}
	if line := ciCacheLine(run.Cache); line != "" {
		ioctx.Stdout("Cache:      " + line)
	}
	// WHY it is red, directly under the facts about it. A run view that names the
	// failing phase but not the failing step sends the reader to the logs for the
	// one sentence they opened the view to get.
	if line := ciFailureLine(run.Failure); line != "" {
		ioctx.Stdout("Failed:     " + line)
	}
	if line := ciSessionLine(run.Session); line != "" {
		ioctx.Stdout("Session:    " + line)
	}
	if len(run.Phases) == 0 {
		ciRenderFailedTasks(ioctx, run.FailedTasks, run.FailedTasksOmitted)
		return
	}
	ioctx.Stdout("")
	ioctx.Stdout("Phases:")
	headers := []string{"PHASE", "STATUS", "CONCLUSION", "REQUIRED", "ATTEMPT", "REASON"}
	rows := make([][]string, 0, len(run.Phases))
	for i := range run.Phases {
		p := run.Phases[i]
		rows = append(rows, []string{
			p.Phase, p.Status, dash(p.Conclusion), strconv.FormatBool(p.Required),
			strconv.Itoa(p.Attempt), dash(p.Reason),
		})
	}
	ciRenderTable(ioctx, headers, rows)
	ciRenderFailedTasks(ioctx, run.FailedTasks, run.FailedTasksOmitted)
}

// Run-annotation keys this surface reads out of the run's `checks` map. They are
// wire twins, like every DTO in this file: the CLI stays off the server
// dependency tree, so it cannot import `runs.AnnotationTestedTree`
// (the run records package) or `ci.AnnotationBaseSHA` (the CI package) and
// spells the three keys here instead. The run's companion `base_ref` names the
// base BRANCH and is not read: this view already prints a `Branch:` line, and a
// second branch name beside it would read as the run's own.
const (
	ciAnnotationTestedTree     = "ci_tested_tree"
	ciAnnotationTestedCheckout = "ci_tested_checkout"
	ciAnnotationBaseSHA        = "base_sha"
)

// ciTestedLine says what the run EXECUTED, which on a pull request is no longer
// what its head sha points at: the runner applies the pull request's
// diff as a squash on the head of its base branch and tests that, so the thing
// that ran is a tree contained in no branch.
//
// The TREE is what gates the line, because the tree is the fact: it is what
// every cache key and every image digest is a content hash of, and it is the
// only one of the three values that a run record cannot already state. A run
// that recorded none — one that stopped before its checkout, and every run
// recorded before the squash existed — renders NO line at all.
//
// A run that built no squash (a main run, a tag publish, a
// publishing pull request) records its head's own tree with the checkout
// `head`, and the line is `head → tree` even when the saga recorded a base:
// that run merged nothing onto it. A tree with no checkout comes from an older
// record, when only the squash recorded a tree, so it reads as a squash.
//
// A squash tree with no base is a record whose fail-soft `base_sha` write was
// lost. The line then omits the base rather than asserting there was none: it
// states the two facts it has and claims nothing about the third.
func ciTestedLine(run *CIRunResponse) string {
	tree := strings.TrimSpace(run.Checks[ciAnnotationTestedTree])
	if tree == "" {
		return ""
	}
	head := shortSHA(strings.TrimSpace(run.SHA))
	if strings.TrimSpace(run.Checks[ciAnnotationTestedCheckout]) == "head" {
		return "head " + head + " → tree " + shortSHA(tree)
	}
	if base := strings.TrimSpace(run.Checks[ciAnnotationBaseSHA]); base != "" {
		return "base " + shortSHA(base) + " + head " + head + " → tree " + shortSHA(tree)
	}
	return "head " + head + " → tree " + shortSHA(tree)
}

// ciFailureLine renders the runner's failure attribution as `step (detail)`, or
// "" when the runner attributed nothing — the caller then prints no line, because
// an absent attribution is stated by its absence and never by a guessed cause.
func ciFailureLine(f *CIRunFailure) string {
	if f == nil || strings.TrimSpace(f.Step) == "" {
		return ""
	}
	// The stored slug is hyphenated (`config-drift`, `worktree-mutated`); the
	// check summary folds hyphens to spaces to read as prose, and this surface
	// matches it so the two render the same cause the same way.
	line := strings.ReplaceAll(strings.TrimSpace(f.Step), "-", " ")
	if d := strings.TrimSpace(f.Detail); d != "" {
		line += " (" + d + ")"
	}
	return line
}

// ciRenderFailedTasks lists the tasks the gate reported failed. It prints nothing
// for a run with none — including a green one, and a red one whose builds report
// never arrived, where an empty "Failing tasks:" heading would assert that the
// gate failed nothing.
//
// A list read from the failure digest also says why: an ERROR column,
// then each task's diagnostics and last output lines. That is the digest the
// runner used to print into its log, now served by the control plane.
func ciRenderFailedTasks(ioctx clicore.IO, tasks []CIRunFailedTask, omitted int) {
	if len(tasks) == 0 {
		return
	}
	ioctx.Stdout("")
	ioctx.Stdout("Failing tasks:")
	withErrors := false
	for i := range tasks {
		withErrors = withErrors || strings.TrimSpace(tasks[i].Error) != ""
	}
	headers := []string{"PROJECT", "TASK"}
	if withErrors {
		headers = append(headers, "ERROR")
	}
	rows := make([][]string, 0, len(tasks))
	for i := range tasks {
		row := []string{dash(tasks[i].Project), dash(tasks[i].Task)}
		if withErrors {
			row = append(row, dash(tasks[i].Error))
		}
		rows = append(rows, row)
	}
	ciRenderTable(ioctx, headers, rows)
	if omitted > 0 {
		ioctx.Stdout(fmt.Sprintf("  … and %d more failing task(s) not shown", omitted))
	}
	for i := range tasks {
		ciRenderFailedTaskDetail(ioctx, &tasks[i])
	}
}

// ciFailedTaskOutputLines bounds the output lines `view` prints per task. The
// digest holds up to forty; `--output=json` carries every one it served.
const ciFailedTaskOutputLines = 10

// ciRenderFailedTaskDetail prints one failed task's diagnostics and last output
// lines, or nothing for a task that carries neither.
func ciRenderFailedTaskDetail(ioctx clicore.IO, task *CIRunFailedTask) {
	if len(task.Diagnostics) == 0 && len(task.Output) == 0 && task.SharedOutput == nil && !task.DetailOmitted {
		return
	}
	ioctx.Stdout("")
	ioctx.Stdout("  " + dash(task.Project) + " " + dash(task.Task))
	for _, d := range task.Diagnostics {
		location := strings.TrimSpace(d.File)
		if location != "" && d.Line > 0 {
			location += ":" + strconv.FormatInt(d.Line, 10)
		}
		switch {
		case location == "":
			ioctx.Stdout("    " + d.Message)
		case d.Message == "":
			ioctx.Stdout("    " + location)
		default:
			ioctx.Stdout("    " + location + ": " + d.Message)
		}
	}
	ciRenderOutputTail(ioctx, "output", task.Output)
	if task.SharedOutput != nil {
		ciRenderOutputTail(ioctx, "output of "+dash(task.SharedOutput.Source), task.SharedOutput.Lines)
	}
	if task.DetailOmitted {
		ioctx.Stdout("    (diagnostics and output left out by the run view's size bound)")
	}
}

// ciRenderOutputTail prints the last ciFailedTaskOutputLines lines under a
// label that says how many it left out.
func ciRenderOutputTail(ioctx clicore.IO, label string, lines []string) {
	if len(lines) == 0 {
		return
	}
	shown := lines
	if len(shown) > ciFailedTaskOutputLines {
		shown = shown[len(shown)-ciFailedTaskOutputLines:]
		ioctx.Stdout(fmt.Sprintf("    %s (last %d of %d lines):", label, len(shown), len(lines)))
	} else {
		ioctx.Stdout("    " + label + ":")
	}
	for _, line := range shown {
		ioctx.Stdout("      | " + line)
	}
}

// ciFailedTasksCarryDetail reports whether the run view already names why its
// tasks failed, which makes the log-derived diagnostics request redundant.
func ciFailedTasksCarryDetail(tasks []CIRunFailedTask) bool {
	for i := range tasks {
		if len(tasks[i].Diagnostics) > 0 || len(tasks[i].Output) > 0 || tasks[i].SharedOutput != nil {
			return true
		}
	}
	return false
}

// ciSessionLine renders the session record's delivery account: whether both
// parts reached the control plane and closed, which is what every derived
// projection of the run (gate report, timings, failure digest) needs.
func ciSessionLine(s *CIRunSession) string {
	if s == nil {
		return ""
	}
	if !s.Session.Shipped && !s.Events.Shipped {
		return "not received"
	}
	parts := "session " + ciSessionPartText(s.Session) + ", events " + ciSessionPartText(s.Events)
	if s.Complete {
		return "complete (" + parts + ")"
	}
	return "incomplete (" + parts + ")"
}

func ciSessionPartText(p CIRunSessionPart) string {
	if !p.Shipped {
		return "not received"
	}
	text := fmt.Sprintf("%d chunk(s) %s", p.Chunks, ciBytes(p.Bytes))
	if !p.Final {
		text += " not closed"
	}
	return text
}

// ciRenderFailures renders the gate's file-anchored diagnostics grouped by task —
// the `file:line:col message [code]` form an editor and a terminal both jump
// from. It is the last section of a red run's view, and the reason that view no
// longer ends at "lint failed".
func ciRenderFailures(ioctx clicore.IO, failures []CIRunLogFailure, truncatedTasks int) {
	if len(failures) == 0 {
		return
	}
	ioctx.Stdout("")
	ioctx.Stdout("Diagnostics:")
	for i := range failures {
		f := failures[i]
		heading := strings.TrimSpace(f.Project)
		if t := strings.TrimSpace(f.Task); t != "" {
			if heading != "" {
				heading += " "
			}
			heading += t
		}
		if heading == "" {
			heading = dash(f.Message)
		}
		ioctx.Stdout("  " + heading)
		for j := range f.Diagnostics {
			ioctx.Stdout("    " + ciFormatDiagnostic(f.Diagnostics[j]))
		}
		// A bounded list that does not say it is bounded reads as the whole list.
		if f.Truncated > 0 {
			ioctx.Stdout(fmt.Sprintf("    … and %d more in this task", f.Truncated))
		}
	}
	// The same rule one level up: a gate that failed 30 tasks must not render as
	// one that failed 10.
	if truncatedTasks > 0 {
		ioctx.Stdout(fmt.Sprintf("  … and %d more failing task(s) not shown", truncatedTasks))
	}
}

// ciFormatDiagnostic renders one finding as `file:line:col message [code]`,
// mirroring the server's ciFormatDiagnostic so the CLI's own rendering and the
// server's FirstErrors preview cannot disagree about the same finding.
func ciFormatDiagnostic(d CIRunLogDiagnostic) string {
	var b strings.Builder
	if d.File != "" {
		b.WriteString(d.File)
		if d.Line > 0 {
			b.WriteString(":" + strconv.Itoa(d.Line))
			if d.Column > 0 {
				b.WriteString(":" + strconv.Itoa(d.Column))
			}
		}
		b.WriteString(": ")
	}
	b.WriteString(d.Message)
	if d.Code != "" {
		b.WriteString(" [" + d.Code + "]")
	}
	return b.String()
}

func ciRenderDiagnostics(ioctx clicore.IO, diag *CIRunLogDiagnostics) {
	if diag == nil {
		return
	}
	if len(diag.FirstErrors) > 0 {
		ioctx.Stdout("")
		ioctx.Stdout("First errors:")
		for _, e := range diag.FirstErrors {
			ioctx.Stdout("  " + e)
		}
	}
	if diag.Reproduce != "" {
		ioctx.Stdout("Reproduce locally: " + diag.Reproduce)
	}
}

// ciRenderTable prints a left-aligned column table over the Stdout sink,
// hand-formatting with %-*s (mirroring reportHelp's alignment convention).
func ciRenderTable(ioctx clicore.IO, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	ioctx.Stdout(ciJoinColumns(headers, widths))
	for _, row := range rows {
		ioctx.Stdout(ciJoinColumns(row, widths))
	}
}

func ciJoinColumns(cols []string, widths []int) string {
	var b strings.Builder
	for i, cell := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		if i == len(cols)-1 {
			b.WriteString(cell)
		} else {
			// fmt.Sprintf (not Fprintf into the builder) keeps gosec's taint analysis
			// from flagging the server-derived cell as an XSS sink (G705); the output
			// is a plain terminal table, never HTML.
			b.WriteString(fmt.Sprintf("%-*s", widths[i], cell))
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// ciFormatLogLine renders one log entry as `<timestamp>  LEVEL  <body>` for the
// human surface (no color: the CI log surface stays plain).
func ciFormatLogLine(entry CILogEntry) string {
	ts := ""
	if !entry.Timestamp.IsZero() {
		ts = entry.Timestamp.Format(time.RFC3339Nano)
	}
	label := strings.ToUpper(strings.TrimSpace(entry.SeverityText))
	if label == "" {
		label = ciSeverityLabel(entry.Severity)
	}
	body := strings.TrimSpace(entry.Body)
	return strings.TrimRight(strings.TrimSpace(ts+"  "+fmt.Sprintf("%-5s", label))+"  "+body, " ")
}

func ciSeverityLabel(sev ciSeverity) string {
	switch {
	case sev >= ciSeverityFatal:
		return "FATAL"
	case sev >= ciSeverityError:
		return "ERROR"
	case sev >= ciSeverityWarn:
		return "WARN"
	case sev >= ciSeverityInfo:
		return "INFO"
	case sev >= ciSeverityDebug:
		return "DEBUG"
	case sev >= ciSeverityTrace:
		return "TRACE"
	default:
		return "LOG"
	}
}

// ciHeartbeatLine renders one run's latest liveness sample, or "" when the run
// never beat (the caller then prints no line at all — an absent heartbeat is
// stated by its absence, never by a fabricated zero).
//
// The sequence and the age are what carry the meaning: an advancing sequence
// with a small age is a run doing work, a frozen one is a run to look at. The
// resource readings ride along only when the substrate measured them.
func ciHeartbeatLine(hb *CIRunHeartbeat) string {
	if hb == nil {
		return ""
	}
	line := fmt.Sprintf("%s #%d, %s ago (attempt %d, elapsed %s)",
		dashUnknown(hb.Phase), hb.Sequence, ciFormatDurationSeconds(hb.AgeSeconds),
		hb.Attempt, ciFormatDurationSeconds(int(hb.ElapsedSeconds)))
	var readings []string
	if hb.CPUDeltaSeconds != nil {
		readings = append(readings, fmt.Sprintf("cpu +%.1fs", *hb.CPUDeltaSeconds))
	}
	if hb.RSSKB != nil {
		readings = append(readings, fmt.Sprintf("rss %dMiB", *hb.RSSKB/1024))
	}
	if hb.Processes != nil {
		readings = append(readings, fmt.Sprintf("procs %d", *hb.Processes))
	}
	if len(readings) > 0 {
		line += " — " + strings.Join(readings, ", ")
	}
	return line
}

// ciTimingLine renders the run's wall-clock decomposition as one line —
// where the time went, stage by stage — so a slow run names its thief without a
// log dive. Only measured stages render: an absent reading stays absent.
func ciTimingLine(t *CIRunTiming) string {
	if t == nil {
		return ""
	}
	var parts []string
	add := func(label string, usec *int64) {
		if usec != nil {
			parts = append(parts, label+" "+ciFormatUsec(*usec))
		}
	}
	add("queue", t.QueueUsec)
	add("cold-start", t.ColdStartUsec)
	add("fetch", t.FetchUsec)
	// The label is `bootstrap`, not `install`: the field measures checkout
	// complete → first gate instruction, so a reader told "install" would go
	// looking at dependency resolution for time the runner spent elsewhere.
	// The wire name stays `installUsec`.
	add("bootstrap", t.InstallUsec)
	add("gate", t.GateUsec)
	add("tail", t.TailUsec)
	add("total", t.RunDurationUsec)
	line := strings.Join(parts, " · ")
	var notes []string
	if t.CheckoutMode != "" {
		notes = append(notes, "checkout "+t.CheckoutMode)
	}
	if t.CLIVersion != "" {
		notes = append(notes, "cli "+t.CLIVersion)
	}
	if len(notes) > 0 {
		if line == "" {
			return strings.Join(notes, ", ")
		}
		line += " (" + strings.Join(notes, ", ") + ")"
	}
	return line
}

// ciDiskLine renders how full the runner's disk got as one line: the
// peak against the disk it was measured on, with the phase and clock it fell
// in, then what stayed resident and the Go caches the gate left behind. Only
// measured readings render; a run whose runner measured nothing gets no line.
func ciDiskLine(d *CIRunDisk) string {
	if d == nil {
		return ""
	}
	var parts []string
	switch {
	case d.PeakGiB != nil:
		peak := "peak " + ciFormatGiB(*d.PeakGiB)
		var notes []string
		if d.TotalGiB != nil && *d.TotalGiB > 0 {
			peak += " of " + ciFormatGiB(*d.TotalGiB)
			notes = append(notes, fmt.Sprintf("%d%%", int(math.Round(*d.PeakGiB / *d.TotalGiB * 100))))
		}
		if d.PeakPhase != "" {
			notes = append(notes, "phase "+d.PeakPhase)
		}
		if d.PeakAt != "" {
			notes = append(notes, d.PeakAt)
		}
		if len(notes) > 0 {
			peak += " (" + strings.Join(notes, ", ") + ")"
		}
		parts = append(parts, peak)
	case d.TotalGiB != nil:
		parts = append(parts, "size "+ciFormatGiB(*d.TotalGiB))
	}
	if d.EndGiB != nil {
		parts = append(parts, "end "+ciFormatGiB(*d.EndGiB))
	}
	if d.GoCacheGiB != nil {
		parts = append(parts, "Go build cache "+ciFormatGiB(*d.GoCacheGiB))
	}
	if d.GoModcacheGiB != nil {
		parts = append(parts, "Go module cache "+ciFormatGiB(*d.GoModcacheGiB))
	}
	return strings.Join(parts, " · ")
}

// ciFormatGiB renders a GiB reading with one decimal: the server stores two,
// and a disk is sized in whole GiB, so a tenth is already finer than any
// decision the number feeds.
func ciFormatGiB(gib float64) string {
	return strconv.FormatFloat(gib, 'f', 1, 64) + " GiB"
}

// ciFormatUsec renders a microsecond reading compactly: below ten seconds it
// keeps one decimal (a 3.7s cold start must not round to "4s" — that precision
// is the reading's whole point), dropping a trailing ".0"; above, it reuses the
// run view's shared duration vocabulary.
func ciFormatUsec(usec int64) string {
	d := time.Duration(usec) * time.Microsecond
	if d < 10*time.Second {
		return strings.Replace(fmt.Sprintf("%.1fs", d.Seconds()), ".0s", "s", 1)
	}
	return ciFormatDurationSeconds(int((d + time.Second/2) / time.Second))
}

// ciCacheLine renders one run's cache economics: the hit/miss split with
// its ratio, and the wall time each side reported.
//
// It returns EMPTY for absent figures, and the caller omits the line entirely —
// a run with no builds report has no cache measurement, and printing "0/0 tasks
// from cache" for it would state a measurement nobody took. The served and
// executed times stay separate because they are two measurements: a cache-served
// task reports its replay cost, not the execution it replaced, so summing them
// into a "saved" number would invent a figure the record does not contain.
func ciCacheLine(cache *CIRunCache) string {
	if cache == nil || cache.Tasks <= 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d tasks reused (%d%%), %d executed — %s served from cache, %s executed",
		cache.Hits, cache.Tasks, cache.HitPercent, cache.Misses,
		ciFormatDurationMS(cache.ServedMS), ciFormatDurationMS(cache.ExecutedMS))
}

// ciFormatDurationMS renders a millisecond duration the way the control plane's
// check summary does, so the two surfaces read alike.
func ciFormatDurationMS(ms int64) string {
	if ms <= 0 {
		return "0s"
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}

// ciFormatDurationSeconds renders a whole-second duration compactly (45s, 3m12s,
// 1h04m). Seconds are the unit the control plane serves, so nothing here has to
// reconcile a local clock with a remote one.
func ciFormatDurationSeconds(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	switch {
	case seconds < 60:
		return strconv.Itoa(seconds) + "s"
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	default:
		return fmt.Sprintf("%dh%02dm", seconds/3600, (seconds%3600)/60)
	}
}

func ciFormatTime(ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	return ts.Format(time.RFC3339)
}

func dash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

// dashUnknown renders an absent provenance value as the WORD unknown rather than
// dash's neutral placeholder. In a table a dash reads as "not
// applicable"; on the provenance line an absent value means the fleet cannot say
// what it ran, which is a finding, not a blank.
func dashUnknown(v string) string {
	if strings.TrimSpace(v) == "" {
		return "unknown"
	}
	return v
}
