package inject

import (
	"context"
)

// scopeKey is the context key for storing the scoped container.
type scopeKey struct{}

// withScope attaches a scoped container to a context.Context.
func withScope(ctx context.Context, scope *Container) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ScopeFrom retrieves the scoped container from a context.Context.
// Returns nil if no scope is active.
func ScopeFrom(ctx context.Context) *Container {
	scope, ok := ctx.Value(scopeKey{}).(*Container)
	if !ok {
		return nil
	}
	return scope
}

// Resolve is a convenience function that resolves a typed dependency
// from the current scope in the context. This is the primary way to
// access scoped dependencies in request handlers.
//
//	func handler(ctx context.Context) {
//	    svc, err := inject.Resolve[*UserService](ctx, userToken)
//	    ...
//	}
func Resolve[T any](ctx context.Context, token Token) (T, error) {
	scope := ScopeFrom(ctx)
	if scope == nil {
		var zero T
		return zero, newNotRegisteredError(token, "no active scope in context")
	}
	val, err := scope.Get(token)
	if err != nil {
		var zero T
		return zero, err
	}
	typed, ok := val.(T)
	if !ok {
		var zero T
		return zero, newTypeMismatchError(token, typeNameOf[T]())
	}
	return typed, nil
}

// ResolveFromContext resolves a dependency by token from the scoped container
// in the given context. Unlike Resolve[T], this returns any — suitable for
// reflection-based callers like http.Inject.
func ResolveFromContext(ctx context.Context, token Token) (any, error) {
	scope := ScopeFrom(ctx)
	if scope == nil {
		return nil, newNotRegisteredError(token, "no active scope in context")
	}
	return scope.Get(token)
}

// ResolveAll is a convenience function that resolves all matching dependencies
// from the current scope.
func ResolveAll[T any](ctx context.Context, filter FilterOptions) ([]T, error) {
	scope := ScopeFrom(ctx)
	if scope == nil {
		return nil, newNotRegisteredError(filter, "no active scope in context")
	}
	vals, err := scope.List(filter)
	if err != nil {
		return nil, err
	}
	results := make([]T, 0, len(vals))
	for _, val := range vals {
		typed, ok := val.(T)
		if !ok {
			return nil, newTypeMismatchError(filter, typeNameOf[T]())
		}
		results = append(results, typed)
	}
	return results, nil
}
