package http

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
)

// scopeFailureResponse logs a per-request scoped DI resolution failure
// server-side and returns a generic 500. The internal error text (provider type
// names, wrapped messages) must not reach the client — this mirrors the
// sanitize-then-log pattern in server.go's buildHandler and the api resolver.
func scopeFailureResponse(ctx context.Context, err error) *Response {
	logger.Default().Named("http").ErrorCtx(ctx, "DI scope resolution failed", err)
	return InternalError("Internal Server Error")
}

var (
	contextType         = reflect.TypeOf((*Context)(nil))
	endpointContextType = reflect.TypeOf((*EndpointContext)(nil))
	responseType        = reflect.TypeOf((*Response)(nil))
)

// injectedParam describes a single parameter in an Inject-wrapped handler.
type injectedParam struct {
	index     int          // position in the function signature
	token     inject.Token // DI token (nil for context params)
	isContext bool         // true if this is *Context or *EndpointContext
	isScoped  bool         // true if this dep is scoped (resolved per-request)
}

// InjectedHandler holds the reflection metadata for a handler wrapped with Inject.
// Before finalization, calling the handler panics. After finalization, singleton
// deps are pre-resolved and cached; scoped deps are resolved per-request.
type InjectedHandler struct {
	fn        reflect.Value
	params    []injectedParam
	ctxIndex  int // index of the *Context / *EndpointContext param
	finalized bool
	hasScoped bool                     // true if any param is scoped
	resolved  []reflect.Value          // pre-resolved values (zero value placeholder for context and scoped slots)
	cc        *inject.ContainerContext // container used for finalization (for idempotency check)
	argsPool  sync.Pool                // pool for []reflect.Value slices
}

// DependencyTokens returns the DI dependencies declared by the wrapped
// handler, excluding its request context. Build-time design discovery uses the
// same reflection metadata as runtime injection, so authors declare nothing a
// second time.
func (ih *InjectedHandler) DependencyTokens() []inject.Token {
	tokens := make([]inject.Token, 0, len(ih.params))
	for _, param := range ih.params {
		if !param.isContext {
			tokens = append(tokens, param.token)
		}
	}
	return tokens
}

// Inject wraps a function with DI-resolved parameters.
//
// The function must return *Response. One parameter must be *Context (or
// *EndpointContext for use with the endpoint builder). All other parameters
// are resolved from the DI container.
//
// Singleton deps are resolved once at startup (zero per-request cost).
//
//	server.GET("/users", http.Inject(func(svc *UserService, ctx *http.Context) *http.Response {
//	    return http.JSON(svc.ListAll())
//	}))
func Inject(handler any) *InjectedHandler {
	fn := reflect.ValueOf(handler)
	ft := fn.Type()

	if ft.Kind() != reflect.Func {
		panic(fmt.Sprintf("http.Inject: expected a function, got %T", handler))
	}
	if ft.NumOut() != 1 || ft.Out(0) != responseType {
		panic(fmt.Sprintf("http.Inject: function must return *http.Response, got %s", ft))
	}

	ih := &InjectedHandler{
		fn:     fn,
		params: make([]injectedParam, ft.NumIn()),
	}

	ctxFound := false
	for i := 0; i < ft.NumIn(); i++ {
		pt := ft.In(i)
		if pt == contextType || pt == endpointContextType {
			if ctxFound {
				panic("http.Inject: function must have exactly one *Context or *EndpointContext parameter")
			}
			ctxFound = true
			ih.params[i] = injectedParam{index: i, isContext: true}
			ih.ctxIndex = i
		} else {
			ih.params[i] = injectedParam{
				index: i,
				token: inject.TokenOf2(pt),
			}
		}
	}
	if !ctxFound {
		panic("http.Inject: function must have a *Context or *EndpointContext parameter")
	}

	return ih
}

// scopeContext extracts the framework context.Context from the handler's
// context parameter. This is the context where the server attaches the DI
// scope (via ctx.WithContext), NOT ctx.Request.Context() which is the raw
// HTTP request context without the scope.
func (ih *InjectedHandler) scopeContext(args []reflect.Value) context.Context {
	ctxVal := args[ih.ctxIndex].Interface()
	switch c := ctxVal.(type) {
	case *Context:
		return c.ctx
	case *EndpointContext:
		return c.ctx
	default:
		panic("http.Inject: unexpected context type")
	}
}

// Handle is the Handler func that gets registered on the router.
// Before finalization it panics. After finalization it calls the wrapped
// function with pre-resolved singletons and per-request scoped deps.
func (ih *InjectedHandler) Handle(ctx *Context) *Response {
	if !ih.finalized {
		panic("http.Inject: handler called before DI finalization — ensure the server plugin is used with app.Use()")
	}

	argsp := ih.argsPool.Get().(*[]reflect.Value) //nolint:errcheck // pool always returns *[]reflect.Value
	args := *argsp
	copy(args, ih.resolved)
	args[ih.ctxIndex] = reflect.ValueOf(ctx)

	if ih.hasScoped {
		scopeCtx := ih.scopeContext(args)
		if err := ih.resolveScoped(args, scopeCtx); err != nil {
			ih.argsPool.Put(argsp)
			return scopeFailureResponse(scopeCtx, err)
		}
	}

	results := ih.fn.Call(args)
	ih.argsPool.Put(argsp)
	if results[0].IsNil() {
		return nil
	}
	return results[0].Interface().(*Response) //nolint:errcheck // return type validated in Inject()
}

// HandleEndpoint is like Handle but accepts *EndpointContext.
// Used by the EndpointBuilder when the injected function takes *EndpointContext.
func (ih *InjectedHandler) HandleEndpoint(ctx *EndpointContext) *Response {
	if !ih.finalized {
		panic("http.Inject: handler called before DI finalization — ensure the server plugin is used with app.Use()")
	}

	argsp := ih.argsPool.Get().(*[]reflect.Value) //nolint:errcheck // pool always returns *[]reflect.Value
	args := *argsp
	copy(args, ih.resolved)
	args[ih.ctxIndex] = reflect.ValueOf(ctx)

	if ih.hasScoped {
		scopeCtx := ih.scopeContext(args)
		if err := ih.resolveScoped(args, scopeCtx); err != nil {
			ih.argsPool.Put(argsp)
			return scopeFailureResponse(scopeCtx, err)
		}
	}

	results := ih.fn.Call(args)
	ih.argsPool.Put(argsp)
	if results[0].IsNil() {
		return nil
	}
	return results[0].Interface().(*Response) //nolint:errcheck // return type validated in Inject()
}

// resolveScoped resolves scoped parameters from the request context's DI scope.
func (ih *InjectedHandler) resolveScoped(args []reflect.Value, ctx context.Context) error {
	for _, p := range ih.params {
		if !p.isScoped {
			continue
		}
		val, err := inject.ResolveFromContext(ctx, p.token)
		if err != nil {
			return errors.Wrapf(err, CodeScope, "scoped dependency resolution failed",
				errors.String("token", inject.TokenName(p.token)))
		}
		args[p.index] = reflect.ValueOf(val)
	}
	return nil
}

// Finalize resolves all DI parameters from the container. Singletons are
// resolved once and cached. Scoped deps are marked for per-request resolution.
// This is called by ServerPlugin.Configure().
//
// If cc is nil (no DI container was created), context-only handlers are
// finalized successfully. Handlers with DI dependencies return a clear
// error at startup instead of panicking at request time.
func (ih *InjectedHandler) Finalize(cc *inject.ContainerContext) error {
	if ih.finalized && ih.cc == cc {
		return nil // idempotent — already finalized with this container
	}
	ih.resolved = make([]reflect.Value, len(ih.params))

	for i, p := range ih.params {
		if p.isContext {
			ih.resolved[p.index] = reflect.Zero(contextType)
			continue
		}

		if cc == nil {
			return errors.New(CodeScope,
				"http.Inject: handler requires a DI dependency but no DI container is available "+
					"— register providers with app.Provide/ProvideFunc",
				errors.String("token", inject.TokenName(p.token)))
		}

		// Check if this is a scoped provider — resolve per-request instead of at startup
		if cc.IsScopedProvider(p.token) {
			ih.params[i].isScoped = true
			ih.hasScoped = true
			// Use zero value as placeholder; resolved per-request in Handle
			ih.resolved[p.index] = reflect.Zero(ih.fn.Type().In(p.index))
			continue
		}

		val, err := cc.Get(p.token)
		if err != nil {
			return errors.Wrapf(err, CodeScope, "http.Inject: failed to resolve DI dependency",
				errors.String("token", inject.TokenName(p.token)))
		}
		ih.resolved[p.index] = reflect.ValueOf(val)
	}

	ih.argsPool = sync.Pool{
		New: func() any {
			s := make([]reflect.Value, len(ih.params))
			return &s
		},
	}
	ih.finalized = true
	ih.cc = cc
	return nil
}
