// Package observabilitycli holds the observability-domain commands for the
// @putnami/cloud CLI extension. Its first command is `putnami logs <app>`: a
// bounded, cursor-paginated query against the control plane's workspace logs
// endpoint (GET /v1/workspaces/{workspace}/logs) with a
// structured `--output=jsonl` surface from day one.
package observabilitycli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	observabilityapiclient "go.putnami.dev/cloud/clients/observability-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// LogsDefaultEnvironment matches the deploy/secrets commands: an unqualified
// `putnami logs <app>` targets the workspace's default environment (prod) so
// the query scopes to the deployed tier rather than every environment at once.
const LogsDefaultEnvironment = "prod"

// maxFollowPages caps `--all` cursor following so a pathological server that
// keeps handing back a non-empty cursor cannot spin the CLI forever. Each page
// is server-bounded to MaxLimit entries, so this is a generous safety ceiling.
const maxFollowPages = 10000

// Logs backs `putnami logs <app>` (query mode). It resolves the target
// app/env/workspace, mints a workspace-scoped token, translates the flags into
// the logs endpoint's query vocabulary, and prints one page (or, with --all,
// every page) of matching entries. `--output=jsonl` / `--json` emit one JSON
// object per entry; otherwise entries are human-formatted and color-coded by
// severity.
//
// The <app> positional is resolved for workspace scoping, error surfaces, the
// empty-result message, AND the `service` correlation filter: the
// logs endpoint filters on the collector's correlation labels
// (workspace/environment/deployment/revision/service), and the deploy path
// stamps the app's canonical name as the bare `service` resource attribute, so
// sending `service=<resolved app>` scopes the query to exactly this app. See
// setServiceFilter for why the app name (not the Cloud Run service name) is the
// shared identity.
func Logs(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	clicore.AdoptPositionalApp(params, args)

	ctx, err := newQueryCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}

	base, err := buildLogsQuery(params, ctx.environment, ioctx.Now())
	if err != nil {
		return err
	}
	setServiceFilter(base, ctx.app)

	// signal.NotifyContext wires Ctrl-C / SIGTERM to context cancellation so a
	// hangup during a page fetch (or a live --follow stream) tears the connection
	// down promptly rather than orphaning sockets — matching the deploy command's
	// request plumbing.
	reqCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --follow switches from the bounded paged query to the live SSE tail: it
	// opens /logs/tail and renders each redacted frame until the user interrupts.
	// The tail handler honors the same `service` filter, so the live tail is
	// scoped to <app> too. Query mode (no --follow) is unchanged.
	if clicore.Truthy(clicore.Param(params, "follow")) {
		tailQuery := buildTailQuery(params, ctx.environment)
		setServiceFilter(tailQuery, ctx.app)
		return followLogs(reqCtx, ctx, params, tailQuery, ioctx)
	}

	return streamLogs(reqCtx, ctx, params, base, ioctx)
}

// queryCtx is the resolved per-invocation context shared by the observability
// query commands (logs/traces/metrics). It embeds the shared
// *clicore.WorkspaceContext (workspace id, control-plane URL, bearer, IO, one-
// shot re-mint) — the single auth-fetch seam via clicore.CallWithSession and
// clicore.OpenStreamWithSession, so a long --all run or a tail reconnect that
// outlives the token minted at start re-mints once on a 401 and replays — and
// adds the observability-only app + environment fields.
type queryCtx struct {
	*clicore.WorkspaceContext
	app         string
	environment string
}

func newQueryCtx(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*queryCtx, error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, err
	}
	// Resolve the app the same way every cloud command does (positional → cwd
	// walk → root manifest). An unresolvable app is an ExitUsage error with an
	// actionable message resolved BEFORE the token mint; an unknown *workspace*
	// surfaces later as the server's ExitAPI response.
	app, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return nil, err
	}
	environment := clicore.FirstString(clicore.StringParam(params, "env", "environment"), clicore.StringValue(link["environment"]), LogsDefaultEnvironment)
	wctx, err := clicore.ResolveWorkspaceContextFromLink(params, link, env, ioctx)
	if err != nil {
		return nil, err
	}
	return &queryCtx{
		WorkspaceContext: wctx,
		app:              app,
		environment:      environment,
	}, nil
}

// buildLogsQuery translates the command flags into the logs endpoint's query
// vocabulary (see the server's parseLogsSelector). It never sends a param the
// server does not understand: `--since` becomes a concrete RFC3339 `from`, and
// only a non-time expression (e.g. deploy:last) is passed through verbatim as
// `since` — the server ignores an unresolved `since` today, so such a query
// degrades to the full window rather than erroring.
func buildLogsQuery(params map[string]any, environment string, now time.Time) (url.Values, error) {
	q := url.Values{}
	if environment != "" {
		q.Set("environment", environment)
	}
	if level := clicore.StringParam(params, "level"); level != "" {
		// The server validates the level name (trace|debug|info|warn|error|
		// fatal) and returns a 400 for anything else; lowercasing here keeps
		// `--level ERROR` working without pre-empting the server's vocabulary.
		q.Set("level", strings.ToLower(level))
	}
	if cursor := clicore.StringParam(params, "cursor"); cursor != "" {
		q.Set("cursor", cursor)
	}
	if n := clicore.NumberParam(params, "limit"); n != nil {
		if *n < 0 {
			return nil, clicore.NewError("--limit must be a non-negative integer", clicore.ExitUsage)
		}
		q.Set("limit", strconv.Itoa(int(*n)))
	}
	applySince(q, clicore.StringParam(params, "since"), now)
	return q, nil
}

// applySince maps a `--since` expression onto the endpoint's time window. An
// RFC3339 timestamp sets `from` directly; a positive Go duration (1h, 15m) sets
// `from = now - duration`; anything else (e.g. deploy:last) is passed through as
// `since` so an early client degrades gracefully instead of being blocked.
func applySince(q url.Values, since string, now time.Time) {
	since = strings.TrimSpace(since)
	if since == "" {
		return
	}
	if t, err := time.Parse(time.RFC3339, since); err == nil {
		q.Set("from", t.UTC().Format(time.RFC3339))
		return
	}
	if d, err := time.ParseDuration(since); err == nil && d > 0 {
		q.Set("from", now.Add(-d).UTC().Format(time.RFC3339))
		return
	}
	q.Set("since", since)
}

// streamLogs fetches and renders pages of log entries. It delegates the page
// loop, cursor following, and JSONL-vs-human dispatch to the shared streamList
// helper, supplying only the log-specific severity-colored line formatter. See
// streamList for the pagination and structured-output contract.
func streamLogs(reqCtx context.Context, ctx *queryCtx, params map[string]any, base url.Values, ioctx clicore.IO) error {
	color := !structuredOutput(params) && colorEnabled(ctx.IO.Env)
	return streamList(reqCtx, ctx, params, base, ioctx, listSurface[logEntry]{
		noun:   "logs",
		path:   "/logs",
		follow: true,
		scope:  queryScope,
		decode: decodeLogsPage,
		render: func(item logEntry) string { return formatLogLine(item, color) },
	})
}

// decodeLogsPage unmarshals a logs endpoint body into observability-api's
// generated LogsPage and projects out its entries, in their printed shape, and
// the next cursor.
func decodeLogsPage(body []byte) ([]logEntry, string, error) {
	var page observabilityapiclient.LogsPage
	if err := decodePageBody(body, &page); err != nil {
		return nil, "", err
	}
	return convertAll(page.Entries, logEntryFrom), deref(page.NextCursor), nil
}

// queryScope renders the human "app in env" description used in the
// empty-result message across the observability query commands.
func queryScope(ctx *queryCtx) string {
	if ctx.app == "" {
		return "workspace " + ctx.WorkspaceID + " (" + ctx.environment + ")"
	}
	return ctx.app + " (" + ctx.environment + ")"
}

// structuredOutput reports whether the caller wants machine-readable output:
// `--output=jsonl`/`--output=json` (the agent surface) or the `--json` alias.
// All render one JSON object per item on the streaming surface. It delegates to
// clicore.StructuredOutput so the flag contract has ONE meaning across the CLI
// (see clicore.OutputMode). It intentionally does NOT key on ioctx.JSON: under
// native dispatch that hook is set even for a plain human `putnami logs`, whose
// per-line content must stay human-formatted (the parent CLI wraps each line
// into a JSONL event regardless).
func structuredOutput(params map[string]any) bool {
	return clicore.StructuredOutput(params)
}

// logsRequest issues one GET against the workspace logs endpoint and returns the
// raw response body. It is a thin wrapper over the shared observabilityGet so the
// logs request-surface tests keep exercising the exact same auth /
// error / User-Agent plumbing traces and metrics now reuse.
func logsRequest(reqCtx context.Context, ctx *queryCtx, q url.Values) ([]byte, int, error) {
	return observabilityGet(reqCtx, ctx, "logs", "/logs", q)
}

// compactJSON marshals one typed item onto a single line for the JSONL surface.
// The printed item types (items.go) keep the server's field order and typed
// numbers, so this reproduces the server's own bytes.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// cloneValues returns a deep copy of v so per-page cursor overrides never mutate
// the caller-built base query.
func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}

// formatLogLine renders one entry as `<timestamp>  LEVEL  <body>`, color-coding
// the severity label when color is enabled.
func formatLogLine(entry logEntry, color bool) string {
	ts := formatTimestamp(entry.Timestamp)
	label := severityLabel(entry)
	body := strings.TrimSpace(entry.Body)

	padded := fmt.Sprintf("%-5s", label)
	if color {
		if c := colorForSeverity(severityNumber(entry)); c != "" {
			padded = c + padded + colorReset
		}
	}
	return strings.TrimRight(strings.TrimSpace(ts+"  "+padded)+"  "+body, " ")
}

// severityLabel renders the display level for an entry, preferring the raw
// severityText the backend carried, else deriving it from the numeric severity.
func severityLabel(entry logEntry) string {
	if t := strings.TrimSpace(entry.SeverityText); t != "" {
		return strings.ToUpper(t)
	}
	switch sev := severityNumber(entry); {
	case sev >= 21:
		return "FATAL"
	case sev >= 17:
		return "ERROR"
	case sev >= 13:
		return "WARN"
	case sev >= 9:
		return "INFO"
	case sev >= 5:
		return "DEBUG"
	case sev >= 1:
		return "TRACE"
	default:
		return "LOG"
	}
}

// severityNumber returns the OTLP severity number for an entry, falling back to
// the severityText vocabulary when the numeric field is absent.
func severityNumber(entry logEntry) int {
	if n := int(entry.Severity); n > 0 {
		return n
	}
	switch strings.ToUpper(strings.TrimSpace(entry.SeverityText)) {
	case "EMERGENCY", "ALERT", "CRITICAL", "FATAL":
		return 21
	case "ERROR", "ERR":
		return 17
	case "WARNING", "WARN":
		return 13
	case "NOTICE", "INFO":
		return 9
	case "DEBUG":
		return 5
	case "TRACE":
		return 1
	default:
		return 0
	}
}

// ANSI severity colors for the human surface.
const (
	colorReset  = "\x1b[0m"
	colorRed    = "\x1b[31m"
	colorYellow = "\x1b[33m"
	colorGreen  = "\x1b[32m"
	colorGray   = "\x1b[90m"
	colorCyan   = "\x1b[36m"
)

func colorForSeverity(sev int) string {
	switch {
	case sev >= 17:
		return colorRed
	case sev >= 13:
		return colorYellow
	case sev >= 9:
		return colorGreen
	case sev >= 5:
		return colorGray
	case sev >= 1:
		return colorCyan
	default:
		return ""
	}
}

// stdoutIsTerminal reports whether the process stdout is a character device. It
// is a package var so the color gate stays off under piped output (CI, tests,
// `| grep`), where os.Stdout is not a terminal.
var stdoutIsTerminal = func() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// colorEnabled reports whether ANSI severity colors should be emitted: off when
// NO_COLOR is set, when TERM is "dumb", or when stdout is not a terminal.
func colorEnabled(env map[string]string) bool {
	if env != nil {
		if _, ok := env["NO_COLOR"]; ok {
			return false
		}
		if env["TERM"] == "dumb" {
			return false
		}
	}
	return stdoutIsTerminal()
}
