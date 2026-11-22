package observabilitycli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	observabilityapiclient "go.putnami.dev/cloud/clients/observability-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Traces backs `putnami cloud traces <app>`: a bounded, cursor-paginated query
// against the control plane's workspace traces endpoint
// (GET /v1/workspaces/{workspace}/traces) with a latency
// `--slow` filter and a structured `--output=jsonl` surface. It resolves the
// target app/env/workspace, mints a workspace-scoped token, translates the flags
// into the traces endpoint's query vocabulary, and prints one page (or, with
// --all, every page) of matching trace summaries. `--output=jsonl` / `--json`
// emit one JSON object per trace; otherwise each trace is human-formatted.
//
// Like logs, the <app> positional is resolved for workspace scoping, error
// surfaces, the empty-result message, AND the `service` correlation
// filter: the deploy path stamps the app's canonical name as the bare `service`
// resource attribute, so sending `service=<resolved app>` scopes the query to
// exactly this app (see setServiceFilter).
//
// The listing carries each trace's root span only: observability-api reads
// Cloud Trace's ROOTSPAN view to summarize a page without pulling every span
// tree, so its spanCount is always 1. The command therefore prints no span
// count for a listed trace and says so after the list. `--trace <id>` reads one
// trace by id (GET /v1/workspaces/{workspace}/traces/{traceId}) and prints all
// of its spans, the read the Putnami MCP query_traces tool makes with `trace`.
func Traces(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	clicore.AdoptPositionalApp(params, args)

	if _, ok := params["trace"]; ok {
		traceID, err := traceIDParam(params)
		if err != nil {
			return err
		}
		if err := rejectListingFlags(params); err != nil {
			return err
		}
		// A trace read names the workspace and the trace only: it resolves no
		// app and no environment, so it runs from a workspace root that names
		// no app, as the root-span note suggests.
		link, err := clicore.ReadCloudLink(workspaceRoot)
		if err != nil {
			return err
		}
		wctx, err := clicore.ResolveWorkspaceContextFromLink(params, link, env, ioctx)
		if err != nil {
			return err
		}
		reqCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return showTrace(reqCtx, &queryCtx{WorkspaceContext: wctx}, params, traceID, ioctx)
	}

	ctx, err := newQueryCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}

	base, err := buildTracesQuery(params, ctx.environment, ioctx.Now())
	if err != nil {
		return err
	}
	setServiceFilter(base, ctx.app)

	// signal.NotifyContext wires Ctrl-C / SIGTERM to context cancellation so a
	// hangup during a page fetch tears the connection down promptly rather than
	// orphaning sockets — matching the logs command's request plumbing.
	reqCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return streamTraces(reqCtx, ctx, params, base, ioctx)
}

// buildTracesQuery translates the command flags into the traces endpoint's query
// vocabulary (see the server's parseTracesSelector). The trace-only `--slow`
// latency filter is validated as a positive Go duration client-side and passed
// through verbatim as `slow`; the shared environment/time-range/pagination
// params are applied by applyCommonQuery.
func buildTracesQuery(params map[string]any, environment string, now time.Time) (url.Values, error) {
	q := url.Values{}
	if slow := strings.TrimSpace(clicore.StringParam(params, "slow")); slow != "" {
		if d, err := time.ParseDuration(slow); err != nil || d <= 0 {
			return nil, clicore.NewError("--slow must be a positive Go duration, e.g. 500ms or 2s", clicore.ExitUsage)
		}
		q.Set("slow", slow)
	}
	if err := applyCommonQuery(q, params, environment, now); err != nil {
		return nil, err
	}
	return q, nil
}

// streamTraces fetches and renders pages of trace summaries via the shared
// streamList engine, supplying only the trace-specific line formatter.
func streamTraces(reqCtx context.Context, ctx *queryCtx, params map[string]any, base url.Values, ioctx clicore.IO) error {
	return streamList(reqCtx, ctx, params, base, ioctx, listSurface[traceSummary]{
		noun:   "traces",
		path:   "/traces",
		follow: true,
		scope:  queryScope,
		decode: decodeTracesPage,
		render: formatTraceLine,
		note:   rootSpanNote,
	})
}

// rootSpanNote follows a non-empty listing: a listed trace may carry its root
// span alone (always on Cloud Trace), and --trace prints one trace whole.
const rootSpanNote = "A listed trace may show its root span only; run putnami cloud traces --trace <traceId> to print all of a trace's spans."

// traceIDPattern is a W3C / Cloud Trace trace id: 32 hexadecimal digits.
var traceIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// traceIDParam reads --trace as a trace id, lowercased as the trace store
// keeps it, and refuses anything else (an empty flag, or a bare --trace that
// the flag parser reads as true) before any request.
func traceIDParam(params map[string]any) (string, error) {
	raw, _ := params["trace"].(string)
	id := strings.TrimSpace(raw)
	if !traceIDPattern.MatchString(id) {
		return "", clicore.NewError("--trace takes a trace id of 32 hexadecimal digits", clicore.ExitUsage)
	}
	return strings.ToLower(id), nil
}

// rejectListingFlags refuses the listing flags next to --trace: a trace read
// takes no window, latency filter or pagination, so silently dropping them
// would answer a different question than the one asked.
func rejectListingFlags(params map[string]any) error {
	for _, flag := range []string{"slow", "from", "to", "cursor"} {
		if strings.TrimSpace(clicore.StringParam(params, flag)) != "" {
			return clicore.NewError("--trace reads one trace and takes no --"+flag, clicore.ExitUsage)
		}
	}
	if clicore.NumberParam(params, "limit") != nil {
		return clicore.NewError("--trace reads one trace and takes no --limit", clicore.ExitUsage)
	}
	if clicore.Truthy(clicore.Param(params, "all")) {
		return clicore.NewError("--trace reads one trace and takes no --all", clicore.ExitUsage)
	}
	return nil
}

// showTrace reads one trace by id through observability-api's generated client
// and prints it whole: under --output=jsonl one JSON object (traceDetail, with
// every span and their count), otherwise a header line and the span tree.
func showTrace(reqCtx context.Context, ctx *queryCtx, params map[string]any, traceID string, ioctx clicore.IO) error {
	client, err := observabilityClient(ctx)
	if err != nil {
		return err
	}
	path := "/traces/" + url.PathEscape(traceID)
	got, err := clicore.CallWithSession(reqCtx, ctx.WorkspaceContext, func(callCtx context.Context) (*observabilityapiclient.TraceSummary, error) {
		return client.GetV1WorkspacesWorkspaceTracesTraceId(callCtx, observabilityapiclient.GetV1WorkspacesWorkspaceTracesTraceIdInput{
			Path: observabilityapiclient.GetV1WorkspacesWorkspaceTracesTraceIdPath{Workspace: ctx.WorkspaceID, TraceId: traceID},
		})
	})
	if err != nil {
		if clicore.ServiceStatus(err) == http.StatusNotFound {
			return clicore.NewError("trace "+traceID+" not found in workspace "+ctx.WorkspaceID, clicore.ExitAPI)
		}
		return observabilityCallError(reqCtx, err, "traces", ctx.WorkspaceURL(path))
	}
	trace := traceDetailFrom(deref(got))
	if structuredOutput(params) {
		ioctx.Stdout(compactJSON(trace))
		return nil
	}
	for _, line := range formatTraceDetail(trace) {
		ioctx.Stdout(line)
	}
	return nil
}

// decodeTracesPage unmarshals a traces endpoint body into observability-api's
// generated TracesPage and projects out its trace summaries, in their printed
// shape, and the next cursor.
func decodeTracesPage(body []byte) ([]traceSummary, string, error) {
	var page observabilityapiclient.TracesPage
	if err := decodePageBody(body, &page); err != nil {
		return nil, "", err
	}
	return convertAll(page.Traces, traceSummaryFrom), deref(page.NextCursor), nil
}

// formatTraceLine renders one listed trace as
// `<start>  <duration>  <name>  <traceId>`, with no span count: the listing
// holds the root span only (see traceSummary). Duration is a time.Duration
// (serialized as its int64 nanosecond value), rendered at human-readable width
// (e.g. 523ms).
func formatTraceLine(item traceSummary) string {
	return traceHeadline(item.Start, item.Duration, "", item.Name, item.TraceID)
}

// traceHeadline renders `<start>  <duration>  [<count>]  <name>  <traceId>`,
// leaving out an empty count, name or trace id.
func traceHeadline(start time.Time, duration time.Duration, count, name, traceID string) string {
	line := fmt.Sprintf("%s  %9s", formatTimestamp(start), duration.String())
	for _, part := range []string{count, strings.TrimSpace(name), strings.TrimSpace(traceID)} {
		if part != "" {
			line += "  " + part
		}
	}
	return strings.TrimSpace(line)
}

// formatTraceDetail renders a whole trace: the headline with its span count,
// then one line per span in tree order, indented by depth, each as
// `+<offset from the earliest start>  <duration>  <name>  (<service>)`. A
// span's children follow it, earliest first; a span whose parent the trace
// does not hold starts a tree of its own. Every span prints exactly once, so
// the tree always shows the count the headline names: a span a malformed trace
// leaves unreachable from any root (a parent cycle) prints after the trees, as
// a root of its own.
func formatTraceDetail(trace traceDetail) []string {
	count := fmt.Sprintf("%d spans", trace.SpanCount)
	if trace.SpanCount == 1 {
		count = "1 span"
	}
	lines := []string{traceHeadline(trace.Start, trace.Duration, count, trace.Name, trace.TraceID)}

	origin := trace.Start
	known := make(map[string]bool, len(trace.Spans))
	for _, span := range trace.Spans {
		known[span.SpanID] = true
		if origin.IsZero() || (!span.Start.IsZero() && span.Start.Before(origin)) {
			origin = span.Start
		}
	}
	// Spans are addressed by their index, so a duplicated span id neither
	// hides a span nor merges two subtrees' bookkeeping.
	order := make([]int, len(trace.Spans))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return trace.Spans[order[a]].Start.Before(trace.Spans[order[b]].Start)
	})
	children := map[string][]int{}
	var roots []int
	for _, i := range order {
		span := trace.Spans[i]
		if span.ParentSpanID == "" || span.ParentSpanID == span.SpanID || !known[span.ParentSpanID] {
			roots = append(roots, i)
			continue
		}
		children[span.ParentSpanID] = append(children[span.ParentSpanID], i)
	}
	printed := make([]bool, len(trace.Spans))
	expanded := map[string]bool{}
	var walk func(i, depth int)
	walk = func(i, depth int) {
		if printed[i] {
			return
		}
		printed[i] = true
		span := trace.Spans[i]
		lines = append(lines, formatSpanLine(span, origin, depth))
		if expanded[span.SpanID] {
			return // a duplicated id lists its children under its first copy
		}
		expanded[span.SpanID] = true
		for _, kid := range children[span.SpanID] {
			walk(kid, depth+1)
		}
	}
	for _, i := range roots {
		walk(i, 1)
	}
	for _, i := range order {
		walk(i, 1)
	}
	return lines
}

// formatSpanLine renders one span of a trace at depth.
func formatSpanLine(span traceSpan, origin time.Time, depth int) string {
	offset := time.Duration(0)
	if !span.Start.IsZero() && !origin.IsZero() {
		offset = span.Start.Sub(origin)
	}
	line := fmt.Sprintf("%s+%s  %s  %s", strings.Repeat("  ", depth),
		offset.Round(time.Microsecond), span.Duration.Round(time.Microsecond), strings.TrimSpace(span.Name))
	if service := strings.TrimSpace(span.Attributes["service"]); service != "" {
		line += "  (" + service + ")"
	}
	return line
}
