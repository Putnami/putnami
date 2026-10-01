package http

// Middleware processes HTTP requests before (or after) the final handler.
// Call next() to pass control to the next middleware in the chain.
// Return a *Response to short-circuit the chain, or nil to let it continue.
type Middleware func(ctx *Context, next func() *Response) *Response

// Handler is the final request handler that produces a response.
type Handler func(ctx *Context) *Response

// Chain composes middlewares into a single handler.
// Middlewares are called in order; the handler is called last.
//
//	chain := http.Chain(logging, auth, rateLimit)
//	handler := chain(myHandler)
//
// Composition happens once, when chain(handler) is called (e.g. at WrapAll
// time), not per request. Invoking the returned Handler only walks the
// pre-built chain; the single per-request allocation is the next() closure
// inherent to the Middleware signature.
func Chain(middlewares ...Middleware) func(Handler) Handler {
	return func(handler Handler) Handler {
		// Build the chain once, from right to left, into a single Handler.
		composed := handler
		for i := len(middlewares) - 1; i >= 0; i-- {
			mw := middlewares[i]
			next := composed
			composed = func(ctx *Context) *Response {
				return mw(ctx, func() *Response { return next(ctx) })
			}
		}
		return composed
	}
}
