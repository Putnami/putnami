package inject

import "reflect"

// Scope defines the lifetime of a provider's instance.
type Scope string

const (
	// Singleton providers are resolved once and cached for the container's lifetime.
	Singleton Scope = "singleton"
	// Scoped providers are resolved once per scope (e.g., per HTTP request).
	Scoped Scope = "scoped"
)

// Visibility controls whether a provider is accessible from child containers.
type Visibility string

const (
	// Public providers are visible to child containers.
	Public Visibility = "public"
	// Private providers are only visible within the registering container.
	Private Visibility = "private"
)

// Factory creates an instance of a dependency. It receives a Resolver
// to look up other dependencies.
type Factory func(r Resolver) (any, error)

// Resolver provides dependency resolution during factory invocation.
type Resolver interface {
	// Resolve returns the instance for the given token.
	Resolve(token Token) (any, error)
	// ResolveAll returns all instances matching the filter.
	ResolveAll(filter FilterOptions) ([]any, error)
}

// CloseHook is called when the container is closed, allowing cleanup of resources.
type CloseHook func() error

// Provider holds the metadata and factory for a single dependency.
type Provider struct {
	// Token identifies this provider in the container (class token, named, or tagged).
	Token Token
	// Factory is the function that creates the provider's value.
	Factory Factory
	// Scope controls the provider's lifetime: Singleton (default) or Scoped.
	Scope Scope
	// Visibility controls whether child containers can resolve this provider: Public (default) or Private.
	Visibility Visibility
	// Tags enable multi-resolution via Container.List() with Tagged tokens.
	Tags []string
	// Deps declares the tokens this provider depends on, used for validation and cycle detection.
	Deps []Token
	// OnClose is called when the container is closed, allowing resource cleanup.
	OnClose CloseHook
	// Lazy skips eager resolution at container startup; the value is resolved on first Get().
	Lazy bool
}

// Registration is an inert provider descriptor created by Provide() and
// passed to Container.Register(). It decouples provider creation from
// container registration.
type Registration struct {
	provider Provider
}

// Option configures a Registration.
type Option func(*Provider)

// Provide creates a Registration for the given token and factory.
//
//	reg := inject.Provide[*UserService](inject.TokenOf[*UserService](),
//	    func(r inject.Resolver) (any, error) {
//	        db, err := inject.ResolveAs[*sql.DB](r, dbToken)
//	        if err != nil { return nil, err }
//	        return NewUserService(db), nil
//	    },
//	    inject.WithTags("service"),
//	)
func Provide(token Token, factory Factory, opts ...Option) Registration {
	p := Provider{
		Token:      token,
		Factory:    factory,
		Scope:      Singleton,
		Visibility: Public,
	}
	for _, opt := range opts {
		opt(&p)
	}
	return Registration{provider: p}
}

// ProvideValue creates a Registration that returns a pre-built value.
//
//	reg := inject.ProvideValue[*Config](configToken, cfg)
func ProvideValue(token Token, value any, opts ...Option) Registration {
	return Provide(token, func(_ Resolver) (any, error) {
		return value, nil
	}, opts...)
}

// ProvideAlias creates a Registration that aliases Dst to the instance registered
// under Src. Resolving Dst lazily resolves Src and returns the same value cast to
// Dst — the alias does not create its own instance, and lifetime follows Src.
//
//	// Reader and Writer are interfaces both satisfied by *internal.Service.
//	// Register Reader normally, then alias Writer to it:
//	inject.Provide(inject.TokenOf[Reader](), newService)
//	inject.ProvideAlias[Writer, Reader]()
//
// Src must be registered separately. If Src's resolved value does not satisfy
// Dst, resolution returns a type-mismatch error.
func ProvideAlias[Dst, Src any](opts ...Option) Registration {
	srcToken := TokenOf[Src]()
	return Provide(TokenOf[Dst](), func(r Resolver) (any, error) {
		val, err := r.Resolve(srcToken)
		if err != nil {
			return nil, err
		}
		typed, ok := val.(Dst)
		if !ok {
			return nil, newTypeMismatchError(srcToken, typeNameOf[Dst]())
		}
		return typed, nil
	}, append([]Option{WithDeps(srcToken)}, opts...)...)
}

// WithScope sets the provider scope.
func WithScope(scope Scope) Option {
	return func(p *Provider) {
		p.Scope = scope
	}
}

// WithVisibility sets the provider visibility.
func WithVisibility(vis Visibility) Option {
	return func(p *Provider) {
		p.Visibility = vis
	}
}

// WithTags adds tags to the provider for multi-resolution via Container.List().
func WithTags(tags ...string) Option {
	return func(p *Provider) {
		p.Tags = append(p.Tags, tags...)
	}
}

// WithDeps declares the tokens this provider depends on.
// Used for validation and cycle detection.
func WithDeps(deps ...Token) Option {
	return func(p *Provider) {
		p.Deps = append(p.Deps, deps...)
	}
}

// WithOnClose registers a cleanup hook called when the container closes.
func WithOnClose(hook CloseHook) Option {
	return func(p *Provider) {
		p.OnClose = hook
	}
}

// WithLazy marks the provider as lazy — it won't be resolved at startup.
func WithLazy() Option {
	return func(p *Provider) {
		p.Lazy = true
	}
}

// ResolveAs is a typed helper for resolving dependencies inside a factory.
//
//	db, err := inject.ResolveAs[*sql.DB](r, dbToken)
func ResolveAs[T any](r Resolver, token Token) (T, error) {
	val, err := r.Resolve(token)
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

// typeNameOf returns the name of a type parameter.
func typeNameOf[T any]() string {
	var zero T
	return reflect.TypeOf(&zero).Elem().String()
}
