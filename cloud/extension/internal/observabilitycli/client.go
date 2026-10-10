package observabilitycli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	observabilityapiclient "go.putnami.dev/cloud/clients/observability-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// observabilityClient resolves observability-api's generated Go client bound
// to ctx's control plane, forwarding ctx's workspace bearer per call
// (clicore.WorkspaceContext.CallContext). The logs/metrics/traces unary reads
// go through it in place of hand-written HTTP calls. The streaming `--follow`
// reads (follow.go) do not use it.
func observabilityClient(ctx *queryCtx) (*observabilityapiclient.ObservabilityClient, error) {
	return clicore.NewServiceClient[observabilityapiclient.ObservabilityClient](observabilityapiclient.RegisterObservabilityClient, ctx.ServiceBinding())
}

// observabilityGet issues one GET against a workspace observability list
// endpoint through observability-api's generated client and returns the
// re-encoded response body. path is the trailing route segment under
// /v1/workspaces/{workspace} (e.g. "/logs", "/traces", "/metrics"); noun names
// the surface in error messages and the cancellation notice. It carries
// clicore.CallWithSession's single 401 re-mint and renders a provider refusal
// as "<noun>: <status line>", ExitAuth on 401, since the generated client
// withholds the provider's free-text message. Shared by the logs, traces,
// and metrics commands so all three behave identically on the wire. The
// caller decodes the returned bytes back into the same generated page
// (LogsPage / MetricsPage / TracesPage), so re-encoding it changes nothing
// the decoder reads.
func observabilityGet(reqCtx context.Context, ctx *queryCtx, noun, path string, q url.Values) ([]byte, int, error) {
	client, err := observabilityClient(ctx)
	if err != nil {
		return nil, 0, err
	}
	var body []byte
	switch path {
	case "/logs":
		page, callErr := clicore.CallWithSession(reqCtx, ctx.WorkspaceContext, func(callCtx context.Context) (*observabilityapiclient.LogsPage, error) {
			return client.GetV1WorkspacesLogs(callCtx, observabilityapiclient.GetV1WorkspacesLogsInput{
				Path:  observabilityapiclient.GetV1WorkspacesLogsPath{Workspace: ctx.WorkspaceID},
				Query: logsQueryFrom(q),
			})
		})
		if callErr != nil {
			return nil, clicore.ServiceStatus(callErr), observabilityCallError(reqCtx, callErr, noun, ctx.WorkspaceURL(path))
		}
		body, err = json.Marshal(page)
	case "/metrics":
		page, callErr := clicore.CallWithSession(reqCtx, ctx.WorkspaceContext, func(callCtx context.Context) (*observabilityapiclient.MetricsPage, error) {
			return client.GetV1WorkspacesMetrics(callCtx, observabilityapiclient.GetV1WorkspacesMetricsInput{
				Path:  observabilityapiclient.GetV1WorkspacesMetricsPath{Workspace: ctx.WorkspaceID},
				Query: metricsQueryFrom(q),
			})
		})
		if callErr != nil {
			return nil, clicore.ServiceStatus(callErr), observabilityCallError(reqCtx, callErr, noun, ctx.WorkspaceURL(path))
		}
		body, err = json.Marshal(page)
	case "/traces":
		page, callErr := clicore.CallWithSession(reqCtx, ctx.WorkspaceContext, func(callCtx context.Context) (*observabilityapiclient.TracesPage, error) {
			return client.GetV1WorkspacesTraces(callCtx, observabilityapiclient.GetV1WorkspacesTracesInput{
				Path:  observabilityapiclient.GetV1WorkspacesTracesPath{Workspace: ctx.WorkspaceID},
				Query: tracesQueryFrom(q),
			})
		})
		if callErr != nil {
			return nil, clicore.ServiceStatus(callErr), observabilityCallError(reqCtx, callErr, noun, ctx.WorkspaceURL(path))
		}
		body, err = json.Marshal(page)
	default:
		return nil, 0, clicore.NewError("unknown observability endpoint "+path, clicore.ExitAPI)
	}
	if err != nil {
		return nil, 0, clicore.NewError("encode observability response: "+err.Error(), clicore.ExitAPI)
	}
	return body, http.StatusOK, nil
}

// observabilityCallError maps a failed generated call to the CLI's existing
// error surface: "<noun>: <status line>" (ExitAuth on 401, ExitAPI otherwise),
// the canceled-by-user notice, or a transport failure — the same shape
// ServerErrorMessageFromBody produced, minus the free-text body the generated
// client withholds.
func observabilityCallError(reqCtx context.Context, err error, noun, target string) error {
	return clicore.ServiceCallError(reqCtx, err, clicore.ServiceCallOptions{
		Target:        target,
		Prefix:        noun + ": ",
		CancelMessage: noun + " query canceled by user",
	})
}

// queryString reads key from q as a generated Query field: present when
// non-empty, nil (the field's declared "absent") otherwise.
func queryString(q url.Values, key string) *string {
	if v := q.Get(key); v != "" {
		return &v
	}
	return nil
}

// queryInt64 reads key from q as a generated Query int64 field. A missing or
// non-numeric value (buildLogsQuery/applyCommonQuery only ever set a validated
// non-negative integer) leaves the field absent rather than erroring here.
func queryInt64(q url.Values, key string) *int64 {
	v := q.Get(key)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// logsQueryFrom projects buildLogsQuery's url.Values onto the generated
// client's declared query shape. deployment/q/revision are never set by
// buildLogsQuery today and are left absent.
func logsQueryFrom(q url.Values) observabilityapiclient.GetV1WorkspacesLogsQuery {
	return observabilityapiclient.GetV1WorkspacesLogsQuery{
		Cursor:      queryString(q, "cursor"),
		Environment: queryString(q, "environment"),
		From:        queryString(q, "from"),
		Level:       queryString(q, "level"),
		Limit:       queryInt64(q, "limit"),
		Service:     queryString(q, "service"),
		Since:       queryString(q, "since"),
	}
}

// metricsQueryFrom projects buildMetricsQuery's url.Values onto the generated
// client's declared query shape. deployment/name/revision are never set by
// buildMetricsQuery today and are left absent.
func metricsQueryFrom(q url.Values) observabilityapiclient.GetV1WorkspacesMetricsQuery {
	return observabilityapiclient.GetV1WorkspacesMetricsQuery{
		Cursor:      queryString(q, "cursor"),
		Environment: queryString(q, "environment"),
		From:        queryString(q, "from"),
		Limit:       queryInt64(q, "limit"),
		Service:     queryString(q, "service"),
		To:          queryString(q, "to"),
		Window:      queryString(q, "window"),
	}
}

// tracesQueryFrom projects buildTracesQuery's url.Values onto the generated
// client's declared query shape. deployment/revision are never set by
// buildTracesQuery today and are left absent.
func tracesQueryFrom(q url.Values) observabilityapiclient.GetV1WorkspacesTracesQuery {
	return observabilityapiclient.GetV1WorkspacesTracesQuery{
		Cursor:      queryString(q, "cursor"),
		Environment: queryString(q, "environment"),
		From:        queryString(q, "from"),
		Limit:       queryInt64(q, "limit"),
		Service:     queryString(q, "service"),
		Slow:        queryString(q, "slow"),
		To:          queryString(q, "to"),
	}
}

// decodePageBody unmarshals a success response body into the typed page pointed
// to by page. An empty body leaves the zero page (an empty result set); a
// malformed body surfaces as an ExitAPI error so a broken response is a clear
// client error rather than a silent empty page.
func decodePageBody(body []byte, page any) error {
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, page); err != nil {
		return clicore.NewError("invalid JSON response from observability endpoint", clicore.ExitAPI)
	}
	return nil
}

// formatTimestamp renders a wire timestamp for the human surface exactly as the
// server serialized it (RFC 3339 with nanosecond precision), and as an empty
// string for a zero/absent time so a missing timestamp reads as no leading
// column rather than the year-1 zero value.
func formatTimestamp(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.Format(time.RFC3339Nano)
}

// setServiceFilter pins the outgoing query to a single app's telemetry by
// sending the `service` correlation filter derived from the resolved
// <app>. The value is the canonical app/project name (clicore.ResolveApp) —
// byte-identical to the bare OTEL `service` resource attribute the deploy path
// stamps (provisioner CloudRunServiceRequest.AppName = project.Name) — so
// `putnami logs/traces/metrics <app>` filters to exactly that app. It is
// deliberately the app name, NOT the Cloud Run service name: that name is
// derived server-side from the workspace slug, which the CLI never holds, so
// sending it would silently drop every entry. An empty/unresolved app leaves the
// query workspace-wide rather than sending an empty (match-nothing) filter.
func setServiceFilter(q url.Values, app string) {
	if app = strings.TrimSpace(app); app != "" {
		q.Set("service", app)
	}
}

// listSurface describes one observability list endpoint for streamList: the
// route + typed response shape and how to render one item on the human surface.
// It is generic over the surface's printed item type T (logEntry /
// traceSummary / metricSeries, items.go), so a --output=jsonl item keeps the
// field order and fields the server writes, not a re-marshaled generic map.
type listSurface[T any] struct {
	// noun is the user-facing word for one page's items ("logs", "traces",
	// "metrics"); it names the surface in error messages, the empty-result line,
	// and the more-available hint.
	noun string
	// path is the trailing route segment under /v1/workspaces/{workspace}.
	path string
	// follow reports whether this surface honors --all (and therefore advertises
	// it in the more-available hint). Logs and traces follow; metrics does not.
	follow bool
	// scope renders the "app in env" description for the empty-result message.
	scope func(ctx *queryCtx) string
	// decode parses one page envelope into its typed items and the next cursor.
	decode func(body []byte) ([]T, string, error)
	// render formats one decoded item as a human line.
	render func(item T) string
}

// streamList fetches and renders pages from an observability list endpoint. It
// is the shared engine behind logs/traces/metrics and preserves the logs
// command's contract exactly:
//
//   - Without --all it fetches exactly one page and, when more remain, surfaces
//     the next cursor as a human hint or — in structured (--output=jsonl/--json)
//     mode — on stderr so stdout stays a pure one-object-per-item stream.
//   - With --all it follows nextCursor until it drains, bounded by maxFollowPages
//     with a same-cursor guard so a pathological server that keeps echoing a
//     non-empty cursor cannot spin the CLI forever.
//   - The caller-built base query is never mutated (each page clones it).
func streamList[T any](reqCtx context.Context, ctx *queryCtx, params map[string]any, base url.Values, ioctx clicore.IO, s listSurface[T]) error {
	structured := structuredOutput(params)
	followAll := s.follow && clicore.Truthy(clicore.Param(params, "all"))

	total := 0
	var nextCursor string

	for page := 0; page < maxFollowPages; page++ {
		requested := nextCursor
		q := cloneValues(base)
		if requested != "" {
			q.Set("cursor", requested)
		}
		body, _, err := observabilityGet(reqCtx, ctx, s.noun, s.path, q)
		if err != nil {
			return err
		}
		items, next, err := s.decode(body)
		if err != nil {
			return err
		}
		for _, item := range items {
			if structured {
				ioctx.Stdout(compactJSON(item))
			} else {
				ioctx.Stdout(s.render(item))
			}
			total++
		}
		nextCursor = next
		// Stop on the last page, when the caller asked for a single page, or if
		// the server echoes the cursor we just sent (a would-be infinite loop).
		if nextCursor == "" || !followAll || nextCursor == requested {
			break
		}
	}

	if structured {
		if nextCursor != "" && !followAll && ioctx.Stderr != nil {
			ioctx.Stderr("more " + s.noun + " available; re-run with --cursor " + nextCursor + cursorHintTail(s.follow))
		}
		return nil
	}
	if total == 0 {
		ioctx.Stdout("No " + s.noun + " found for " + s.scope(ctx) + ".")
		return nil
	}
	if nextCursor != "" && !followAll {
		ioctx.Stdout(fmt.Sprintf("More %s available — re-run with --cursor %s%s for the next page.", s.noun, nextCursor, humanCursorHintTail(s.follow)))
	}
	return nil
}

// applyCommonQuery sets the query params shared by the traces and metrics
// surfaces: the environment correlation label, the --from/--to time range, and
// cursor/limit pagination. --from/--to each accept an RFC3339 timestamp or a
// positive Go-duration lookback (resolved to now-duration), so the CLI always
// sends the server-valid RFC3339 the endpoints parse rather than a bare
// duration the server would reject with a 400. Returns an ExitUsage error for a
// malformed time or a negative limit. The <app> positional is applied as the
// `service` filter at the command entry points (via setServiceFilter), not
// here — this helper only sets the environment/time/pagination params shared by
// traces and metrics.
func applyCommonQuery(q url.Values, params map[string]any, environment string, now time.Time) error {
	if environment != "" {
		q.Set("environment", environment)
	}
	if v, ok, err := resolveQueryTime(clicore.StringParam(params, "from"), now); err != nil {
		return clicore.NewError("--from must be an RFC3339 timestamp or a positive Go duration lookback (e.g. 1h, 15m)", clicore.ExitUsage)
	} else if ok {
		q.Set("from", v)
	}
	if v, ok, err := resolveQueryTime(clicore.StringParam(params, "to"), now); err != nil {
		return clicore.NewError("--to must be an RFC3339 timestamp or a positive Go duration lookback (e.g. 1h, 15m)", clicore.ExitUsage)
	} else if ok {
		q.Set("to", v)
	}
	if cursor := clicore.StringParam(params, "cursor"); cursor != "" {
		q.Set("cursor", cursor)
	}
	if n := clicore.NumberParam(params, "limit"); n != nil {
		if *n < 0 {
			return clicore.NewError("--limit must be a non-negative integer", clicore.ExitUsage)
		}
		q.Set("limit", strconv.Itoa(int(*n)))
	}
	return nil
}

// resolveQueryTime maps a --from/--to expression onto an RFC3339 timestamp. An
// RFC3339 input is normalized to UTC; a positive Go duration (1h, 15m) is
// resolved to now-duration. An empty value is a no-op (ok=false); anything else
// is an error so a typo surfaces client-side as ExitUsage rather than a server
// 400.
func resolveQueryTime(value string, now time.Time) (string, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC().Format(time.RFC3339), true, nil
	}
	if d, err := time.ParseDuration(value); err == nil && d > 0 {
		return now.Add(-d).UTC().Format(time.RFC3339), true, nil
	}
	return "", false, fmt.Errorf("invalid time %q", value)
}

// cursorHintTail appends the " or --all" suggestion only for surfaces that
// honor --all, so the metrics hint (no --all flag) never advertises it.
func cursorHintTail(follow bool) string {
	if follow {
		return " or --all"
	}
	return ""
}

// humanCursorHintTail is cursorHintTail's parenthesized form for the human line.
func humanCursorHintTail(follow bool) string {
	if follow {
		return " (or --all)"
	}
	return ""
}
