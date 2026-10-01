package http

import (
	"fmt"
	"log/slog"

	"go.putnami.dev/errors"
	"go.putnami.dev/logger"
)

// Recovery middleware catches panics and returns a 500 response.
// Panics are classified as bugs via errors.Bug, which captures a stack trace.
// The full error is logged server-side; only a generic message is sent to the client.
func Recovery() Middleware {
	log := logger.Default().Named("http.recovery")

	return func(ctx *Context, next func() *Response) (resp *Response) {
		defer func() {
			if r := recover(); r != nil {
				bugErr := errors.Bug(fmt.Errorf("panic: %v", r))
				log.Error("panic recovered", bugErr,
					slog.String("method", ctx.Method),
					slog.String("path", ctx.Path),
				)
				// Surface the real panic error on the request's terminal record:
				// the Logging middleware reads it back via logger.RequestError.
				// No-op when no field bag is installed (Logging inactive).
				logger.SetRequestError(ctx.Context(), bugErr)
				resp = InternalError("Internal Server Error")
			}
		}()
		return next()
	}
}
