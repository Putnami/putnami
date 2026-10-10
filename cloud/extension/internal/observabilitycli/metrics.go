package observabilitycli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	observabilityapiclient "go.putnami.dev/cloud/clients/observability-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// Metrics backs `putnami cloud metrics <app>`: a bounded, windowed,
// cursor-paginated query against the control plane's workspace metrics endpoint
// (GET /v1/workspaces/{workspace}/metrics) with a `--window`
// lookback and a structured `--output=jsonl` surface. It resolves the target
// app/env/workspace, mints a workspace-scoped token, translates the flags into
// the metrics endpoint's query vocabulary, and prints one page of matching
// series. `--output=jsonl` / `--json` emit one JSON object per series; otherwise
// each series is human-formatted.
//
// Like logs/traces, the <app> positional is resolved for workspace scoping,
// error surfaces, the empty-result message, AND the `service`
// correlation filter: the deploy path stamps the app's canonical name as the
// bare `service` resource attribute, so sending `service=<resolved app>` scopes
// the query to exactly this app (see setServiceFilter).
func Metrics(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	clicore.AdoptPositionalApp(params, args)

	ctx, err := newQueryCtx(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}

	base, err := buildMetricsQuery(params, ctx.environment, ioctx.Now())
	if err != nil {
		return err
	}
	setServiceFilter(base, ctx.app)

	// signal.NotifyContext wires Ctrl-C / SIGTERM to context cancellation so a
	// hangup during a page fetch tears the connection down promptly rather than
	// orphaning sockets — matching the logs command's request plumbing.
	reqCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return streamMetrics(reqCtx, ctx, params, base, ioctx)
}

// buildMetricsQuery translates the command flags into the metrics endpoint's
// query vocabulary (see the server's parseMetricsSelector). The metrics-only
// `--window` lookback is validated as a positive Go duration client-side and
// passed through verbatim as `window` (the server sets From = anchor-window when
// the caller did not pin an explicit --from, so an explicit --from always wins);
// the shared environment/time-range/pagination params are applied by
// applyCommonQuery.
func buildMetricsQuery(params map[string]any, environment string, now time.Time) (url.Values, error) {
	q := url.Values{}
	if window := strings.TrimSpace(clicore.StringParam(params, "window")); window != "" {
		if d, err := time.ParseDuration(window); err != nil || d <= 0 {
			return nil, clicore.NewError("--window must be a positive Go duration, e.g. 1h or 15m", clicore.ExitUsage)
		}
		q.Set("window", window)
	}
	if err := applyCommonQuery(q, params, environment, now); err != nil {
		return nil, err
	}
	return q, nil
}

// streamMetrics fetches and renders a page of metric series via the shared
// streamList engine. Unlike logs/traces, the metrics surface does not honor
// --all (its manifest declares no such flag): follow=false fetches one page and
// surfaces the next cursor without advertising --all.
func streamMetrics(reqCtx context.Context, ctx *queryCtx, params map[string]any, base url.Values, ioctx clicore.IO) error {
	return streamList(reqCtx, ctx, params, base, ioctx, listSurface[metricSeries]{
		noun:   "metrics",
		path:   "/metrics",
		follow: false,
		scope:  queryScope,
		decode: decodeMetricsPage,
		render: formatMetricSeriesLine,
	})
}

// decodeMetricsPage unmarshals a metrics endpoint body into observability-api's
// generated MetricsPage and projects out its series, in their printed shape,
// and the next cursor.
func decodeMetricsPage(body []byte) ([]metricSeries, string, error) {
	var page observabilityapiclient.MetricsPage
	if err := decodePageBody(body, &page); err != nil {
		return nil, "", err
	}
	return convertAll(page.Series, metricSeriesFrom), deref(page.NextCursor), nil
}

// formatMetricSeriesLine renders one metric series as
// `<name>  <n> points  latest=<value>@<ts>  {label=value …}`. The latest sample
// is the last point in the (time-ordered) series; labels are rendered in sorted
// order for a deterministic human line.
func formatMetricSeriesLine(item metricSeries) string {
	name := strings.TrimSpace(item.Name)
	points := item.Points

	line := fmt.Sprintf("%s  %d points", name, len(points))
	if len(points) > 0 {
		last := points[len(points)-1]
		line += fmt.Sprintf("  latest=%g@%s", last.Value, formatTimestamp(last.Timestamp))
	}
	if labels := formatLabels(item.Labels); labels != "" {
		line += "  " + labels
	}
	return strings.TrimSpace(line)
}

// formatLabels renders a label map as `{k1=v1 k2=v2}` with keys sorted so the
// human line is deterministic. A nil/empty label map renders as "".
func formatLabels(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return "{" + strings.Join(parts, " ") + "}"
}
