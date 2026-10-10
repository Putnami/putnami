package observabilitycli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
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
func Traces(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	clicore.AdoptPositionalApp(params, args)

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
	})
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

// formatTraceLine renders one trace summary as
// `<start>  <duration>  <n> spans  <name>  <traceId>`. Duration is a
// time.Duration (serialized as its int64 nanosecond value), rendered at
// human-readable width (e.g. 523ms).
func formatTraceLine(item traceSummary) string {
	start := formatTimestamp(item.Start)
	dur := item.Duration.String()
	spanCount := item.SpanCount
	name := strings.TrimSpace(item.Name)
	traceID := strings.TrimSpace(item.TraceID)

	line := fmt.Sprintf("%s  %9s  %d spans", start, dur, spanCount)
	if name != "" {
		line += "  " + name
	}
	if traceID != "" {
		line += "  " + traceID
	}
	return strings.TrimSpace(line)
}
