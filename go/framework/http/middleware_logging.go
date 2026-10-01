package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.putnami.dev/logger"
)

// requestIDCtxKey is the unexported context key under which the RequestID
// middleware stores the correlation ID inside the request context.Context.
type requestIDCtxKey struct{}

// contextWithRequestID returns a copy of ctx that carries the given request ID.
// It is the storage side of the bridge that RequestIDFromContext reads.
func contextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDCtxKey{}, id)
}

// RequestIDFromContext extracts the correlation ID stored by the RequestID
// middleware from a context.Context, or "" if none is present. It is wired into
// the logger's pluggable trace extraction (see the init below) so context-aware
// application and error logs carry the same ID as the access log.
func RequestIDFromContext(ctx context.Context) string {
	id, ok := ctx.Value(requestIDCtxKey{}).(string)
	if !ok {
		return ""
	}
	return id
}

// init bridges the RequestID middleware's context value into the logger's
// pluggable trace extraction so that any *Ctx log emitted from a handler or
// service correlates to the request out of the box. A real telemetry package
// (e.g. go.putnami.dev/telemetry) may install a richer extractor; to avoid
// clobbering it we only set the default bridge when none has been registered
// yet, and the bridge itself falls through to "" so a telemetry-set value still
// wins when this is the active extractor and no request ID is present.
func init() {
	if logger.TraceIDFromContext == nil {
		logger.TraceIDFromContext = RequestIDFromContext
	}
}

// RequestID middleware establishes a correlation ID for each request. It reuses
// an inbound X-Request-ID or X-Cloud-Trace-Context header when present and
// generates a fresh random ID otherwise, so every request is correlatable. The
// resolved ID is stored on the Context (ctx.RequestID), injected into the
// request context.Context (so context-aware logs correlate via
// logger.TraceIDFromContext — see RequestIDFromContext), and echoed on the
// response via X-Request-ID.
func RequestID() Middleware {
	return func(ctx *Context, next func() *Response) *Response {
		traceID := ctx.Header("X-Request-ID")
		if traceID == "" {
			traceID = ctx.Header("X-Cloud-Trace-Context")
		}
		if traceID == "" {
			traceID = generateRequestID()
		}
		ctx.RequestID = traceID
		ctx.SetContext(contextWithRequestID(ctx.Context(), traceID))
		ctx.SetHeader("X-Request-ID", traceID)
		return next()
	}
}

// generateRequestID returns a random 128-bit hex correlation ID. On the
// vanishingly rare chance the entropy source fails, it falls back to a
// timestamp-derived value so a request is never left without an ID.
func generateRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// LoggerOptions configures the logging middleware.
type LoggerOptions struct {
	// Logger is the logger to use. If nil, a default logger is created.
	Logger *logger.Logger
	// Exclude is a list of path prefixes to skip logging for (e.g., "/_/health").
	Exclude []string
}

// Logging middleware logs each request with method, path, status, and duration.
//
//	server.Use(http.Logging(http.LoggerOptions{
//	    Exclude: []string{"/_/health"},
//	}))
func Logging(opts LoggerOptions) Middleware {
	log := opts.Logger
	if log == nil {
		log = logger.Default().Named("http")
	}

	return func(ctx *Context, next func() *Response) *Response {
		// Skip excluded paths
		for _, prefix := range opts.Exclude {
			if strings.HasPrefix(ctx.Path, prefix) {
				return next()
			}
		}

		// Install a fresh request-scoped field bag before the handler runs so
		// in-request work accumulates onto this one terminal record: event
		// publishes (S5) merge in as bag fields, and the 5xx path reads back the
		// real handler error recorded via logger.SetRequestError. Wrap whatever
		// context is current (e.g. the correlation id set by an outer RequestID
		// middleware) so the terminal record still correlates via traceId.
		bag := logger.NewFieldBag()
		ctx.SetContext(logger.ContextWithFieldBag(ctx.Context(), bag))

		start := time.Now()
		resp := next()
		durationMs := time.Since(start).Milliseconds()

		status := 200
		if resp != nil && resp.Status != 0 {
			status = resp.Status
		}

		routePath := ctx.Route
		if routePath == "" {
			routePath = ctx.Path
		}

		outcome := "success"
		if status >= 500 {
			outcome = "failure"
		}

		// Re-read the request context at emit time so the terminal record
		// correlates via traceId regardless of RequestID/Logging ordering: a
		// RequestID middleware that runs inside next() attaches its id above the
		// bag, and reqCtx still resolves the bag beneath it.
		reqCtx := ctx.Context()

		// Domain fields travel as one closed "http" group — exactly method,
		// routePath, status, outcome, durationMs. The correlation id is NOT
		// duplicated here (no requestId key); it surfaces as the top-level
		// traceId emitted by the *Ctx logging path.
		httpGroup := slog.Any("http", map[string]any{
			"method":     ctx.Method,
			"routePath":  routePath,
			"status":     status,
			"outcome":    outcome,
			"durationMs": durationMs,
		})

		// Message is "[METHOD] routePath" for every status: severity plus the
		// structured error carry the failure, so no StatusText or error text
		// leaks into the message (matches the frozen conformance fixture).
		msg := fmt.Sprintf("[%s] %s", ctx.Method, routePath)
		switch {
		case status >= 500:
			// RequestError may be nil when nothing recorded an error (e.g. a
			// handler returned a bare 500); ErrorCtx then emits no error field.
			log.ErrorCtx(reqCtx, msg, logger.RequestError(reqCtx), httpGroup)
		default:
			log.InfoCtx(reqCtx, msg, httpGroup)
		}

		return resp
	}
}
