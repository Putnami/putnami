package client

import "context"

// InterceptorFunc is the function signature for the next step in the chain.
type InterceptorFunc func(ctx context.Context, req *Request) (*Response, error)

// Interceptor wraps a request/response with cross-cutting concerns.
// It receives the request, the next function in the chain, and returns the response.
type Interceptor func(ctx context.Context, req *Request, next InterceptorFunc) (*Response, error)

// buildChain composes interceptors into a single function.
// Interceptors are applied in order: first interceptor wraps the second, etc.
func buildChain(interceptors []Interceptor, transport InterceptorFunc) InterceptorFunc {
	chain := transport
	// Apply in reverse order so first interceptor runs first.
	for i := len(interceptors) - 1; i >= 0; i-- {
		ic := interceptors[i]
		next := chain
		chain = func(ctx context.Context, req *Request) (*Response, error) {
			return ic(ctx, req, next)
		}
	}
	return chain
}
