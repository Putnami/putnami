package observabilitycli

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"go.putnami.dev/client"
	observabilityapiclient "go.putnami.dev/cloud/clients/observability-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// followMaxReconnects bounds how many times followLogs re-opens the tail after a
// drop that streamed nothing, so a persistently failing or flapping endpoint
// cannot spin forever. A connection that delivered at least one frame resets the
// budget: a live tail making progress keeps following. A var so the reconnect-
// bound test can shrink it.
var followMaxReconnects = 5

// followBackoff is the fixed delay between tail reconnect attempts. A small
// constant keeps the reconnect simple and bounded — no unbounded exponential. A
// var so tests can shrink it.
var followBackoff = 2 * time.Second

// followLogs opens the tail through the generated observability-api client and
// renders each redacted log frame live until the user interrupts (reqCtx
// cancels on Ctrl-C / SIGTERM / context cancel). Human mode color-codes each
// line exactly like the query surface via formatLogLine; --output=jsonl / --json
// emits one compact JSON object per frame on pure stdout via compactJSON.
// Redaction is applied server-side, so the CLI never re-redacts.
//
// The generated stream owns the wire: SSE framing, the frame schema, the
// heartbeat-driven idle budget and the typed errors. What stays here is the
// re-open policy the framework cannot declare over SSE yet: a dropped connection re-opens after followBackoff,
// bounded by followMaxReconnects consecutive drops that streamed nothing; a
// connection that delivered a frame resets that budget. A provider refusal
// (4xx/5xx — an unknown workspace, a 401 that survived the single re-mint) is
// terminal. Because the server seeds the tail watermark at connect time, lines
// produced during a reconnect gap are not backfilled — this is a best-effort
// live tail, not a gap-free reader (use query mode with --since for that).
func followLogs(reqCtx context.Context, ctx *queryCtx, params map[string]any, tailQuery url.Values, ioctx clicore.IO) error {
	structured := structuredOutput(params)
	color := !structured && colorEnabled(ctx.IO.Env)
	emit := func(entry logEntry) {
		ioctx.Stdout(renderFrame(entry, structured, color))
	}

	api, err := newObservabilityAPIClient(ctx.WorkspaceContext)
	if err != nil {
		return err
	}
	in := observabilityapiclient.GetV1WorkspacesLogsTailInput{
		Path:  observabilityapiclient.GetV1WorkspacesLogsTailPath{Workspace: ctx.WorkspaceID},
		Query: tailInputQuery(tailQuery),
	}

	drops := 0
	for {
		streamed, err := followTailOnce(reqCtx, ctx, api, in, emit)
		if reqCtx.Err() != nil {
			return nil // clean interrupt (Ctrl-C / SIGTERM / context cancel)
		}
		if err != nil && !isTailDrop(err) {
			return err // terminal provider refusal — do not reconnect
		}
		if streamed {
			drops = 0 // a connection that delivered frames earns a fresh budget
		}
		drops++
		if drops > followMaxReconnects {
			return err // exhausted the reconnect budget (nil on a clean EOF)
		}
		if serr := followSleep(reqCtx, followBackoff); serr != nil {
			return nil // canceled during backoff
		}
	}
}

// followTailOnce opens one generated tail stream and hands its frames to emit
// until the stream ends. It returns whether at least one frame arrived, and an
// error classified for the caller: nil on a clean end, a tailDropError for a
// transport drop or an idle stream (re-openable), or a surfaced clicore error
// for a provider refusal (terminal).
func followTailOnce(reqCtx context.Context, ctx *queryCtx, api *observabilityapiclient.ObservabilityClient,
	in observabilityapiclient.GetV1WorkspacesLogsTailInput, emit func(logEntry)) (bool, error) {
	stream, err := clicore.OpenStreamWithSession(reqCtx, ctx.WorkspaceContext, func(callCtx context.Context) (*client.Stream[observabilityapiclient.LogEntry], error) {
		return api.GetV1WorkspacesLogsTail(callCtx, in)
	})
	if err != nil {
		if reqCtx.Err() != nil {
			return false, nil
		}
		return false, tailStreamError(err)
	}
	defer stream.Close() //nolint:errcheck // Close only cancels the stream

	streamed := false
	for entry := range stream.Messages() {
		streamed = true
		emit(logEntryFrom(entry))
	}
	if serr := stream.Err(); serr != nil && reqCtx.Err() == nil {
		return streamed, tailStreamError(serr)
	}
	return streamed, nil
}

// tailInputQuery carries the selector buildTailQuery and setServiceFilter
// resolved onto the generated tail input.
func tailInputQuery(q url.Values) observabilityapiclient.GetV1WorkspacesLogsTailQuery {
	var out observabilityapiclient.GetV1WorkspacesLogsTailQuery
	for key, field := range map[string]**string{
		"environment": &out.Environment,
		"level":       &out.Level,
		"service":     &out.Service,
	} {
		if v := strings.TrimSpace(q.Get(key)); v != "" {
			*field = &v
		}
	}
	return out
}

// renderFrame renders one streamed log frame for the live tail: a compact JSON
// object under --output=jsonl / --json (pure stdout, one object per line), else
// the same severity-colored human line the query surface prints (formatLogLine
// reused verbatim).
func renderFrame(entry logEntry, structured, color bool) string {
	if structured {
		return compactJSON(entry)
	}
	return formatLogLine(entry, color)
}

// buildTailQuery builds the selector the tail endpoint honors (tailBaseSelector):
// the environment correlation label and an optional minimum severity
// (lowercased, matching buildLogsQuery). The tail is watermark-driven
// server-side, so from/since/limit/cursor — which the paged query understands —
// are not part of the tail contract.
func buildTailQuery(params map[string]any, environment string) url.Values {
	q := url.Values{}
	if environment != "" {
		q.Set("environment", environment)
	}
	if level := clicore.StringParam(params, "level"); level != "" {
		q.Set("level", strings.ToLower(level))
	}
	return q
}

// followSleep blocks for d or until reqCtx is canceled, returning reqCtx.Err() on
// cancellation so the reconnect loop can distinguish an interrupt from the
// timer.
func followSleep(reqCtx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-reqCtx.Done():
		return reqCtx.Err()
	case <-t.C:
		return nil
	}
}

// tailStreamError classifies a generated-stream failure: a provider refusal is
// terminal, everything else (a dropped transport, an idle stream, a frame the
// contract refused) is a re-openable drop.
func tailStreamError(err error) error {
	if clicore.ServiceStatus(err) != 0 {
		return clicore.ServiceCallError(context.Background(), err, clicore.ServiceCallOptions{Prefix: "logs tail: "})
	}
	return tailDropError{err: err}
}

// tailDropError marks a re-openable failure of the tail stream so followLogs
// re-opens rather than surfacing it. A provider refusal is deliberately NOT
// wrapped, so it stays terminal.
type tailDropError struct{ err error }

func (e tailDropError) Error() string { return e.err.Error() }
func (e tailDropError) Unwrap() error { return e.err }

func isTailDrop(err error) bool {
	var d tailDropError
	return errors.As(err, &d)
}
