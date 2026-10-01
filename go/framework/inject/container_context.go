package inject

import (
	"context"
	"sync"

	"go.putnami.dev/errors"
)

// containerContextState represents the lifecycle state of a ContainerContext.
type containerContextState string

// Container lifecycle states.
const (
	stateIdle    containerContextState = "idle"
	stateStarted containerContextState = "started"
	stateClosed  containerContextState = "closed"
)

// ScopeContext provides read-only access to a scoped container.
type ScopeContext interface {
	// Get resolves a single dependency by token.
	Get(token Token) (any, error)
	// List resolves all providers matching the filter.
	List(filter FilterOptions) ([]any, error)
	// Has returns true if a provider exists.
	Has(token Token) bool
}

// DetachedScope is a scope that must be explicitly closed by the caller.
// Use this when you need a scope outside of the Scope() callback pattern.
type DetachedScope struct {
	ScopeContext
	container *Container
}

// Close closes the detached scope and releases resources.
func (s *DetachedScope) Close() error {
	return s.container.Close()
}

// Context returns a context.Context with this scope attached.
func (s *DetachedScope) Context(parent context.Context) context.Context {
	return withScope(parent, s.container)
}

// ContainerContext is the application-level DI entry point.
// It manages the root container and scoped resolution.
type ContainerContext struct {
	name          string
	root          *Container
	registrations []Registration
	requirements  []Token
	state         containerContextState
	mu            sync.RWMutex
}

// NewContainerContext creates a new container context.
func NewContainerContext(name string) *ContainerContext {
	return &ContainerContext{
		name:  name,
		root:  NewContainer(name, nil),
		state: stateIdle,
	}
}

// Register adds a provider registration. Must be called before Start().
func (cc *ContainerContext) Register(reg Registration) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if cc.state != stateIdle {
		return errors.New(CodeIllegalState, "cannot register after container context has been started",
			errors.String("state", string(cc.state)),
		)
	}

	cc.registrations = append(cc.registrations, reg)
	return cc.root.Register(reg)
}

// Require declares a token that must be provided before Start() succeeds.
func (cc *ContainerContext) Require(token Token) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.requirements = append(cc.requirements, token)
}

// Start validates the container hierarchy and resolves all non-lazy singletons.
func (cc *ContainerContext) Start() error {
	return cc.start(true)
}

// StartLazy validates the container hierarchy without eagerly resolving any
// singletons — every provider is treated as if it were lazy and is built only
// on first Get. The build-time describe phase uses this so running an app
// binary with PUTNAMI_DESCRIBE set never reaches external systems (a DB pool,
// say) just to harvest metadata from the configured plugin chain.
func (cc *ContainerContext) StartLazy() error {
	return cc.start(false)
}

func (cc *ContainerContext) start(resolveEager bool) error {
	cc.mu.Lock()
	if cc.state != stateIdle {
		cc.mu.Unlock()
		return errors.New(CodeIllegalState, "container context is already "+string(cc.state),
			errors.String("state", string(cc.state)),
		)
	}
	cc.state = stateStarted
	cc.mu.Unlock()

	if err := cc.runStart(resolveEager); err != nil {
		// Roll the partial boot back: dispose any singletons that were already
		// constructed (so their resources are released) and return to Idle so
		// the caller can fix the configuration and retry Start. Get/List/Scope
		// observe a not-started context rather than a half-built one. A failure
		// to clean up does not mask the original start error.
		_ = cc.root.resetBuilt() //nolint:errcheck // best-effort rollback; the original start error takes precedence
		cc.mu.Lock()
		cc.state = stateIdle
		cc.mu.Unlock()
		return err
	}
	return nil
}

// runStart performs the validate-and-resolve work for a single start attempt.
// The caller (start) owns the state transitions and the failure rollback.
func (cc *ContainerContext) runStart(resolveEager bool) error {
	// Check requirements
	for _, req := range cc.requirements {
		if !cc.has(req) {
			return newRequirementNotMetError(req, cc.name)
		}
	}

	// Validate all containers
	if allIssues := cc.root.Validate(); len(allIssues) > 0 {
		return newValidationError(allIssues)
	}

	if !resolveEager {
		return nil
	}

	// Resolve all non-lazy singletons
	return cc.root.ResolveAll()
}

// Close disposes of all containers and resources.
func (cc *ContainerContext) Close() error {
	cc.mu.Lock()
	if cc.state == stateClosed {
		cc.mu.Unlock()
		return nil
	}
	cc.state = stateClosed
	cc.mu.Unlock()

	return cc.root.Close()
}

// Get resolves a single dependency from the root or module containers.
func (cc *ContainerContext) Get(token Token) (any, error) {
	cc.mu.RLock()
	if cc.state != stateStarted {
		cc.mu.RUnlock()
		return nil, errors.New(CodeIllegalState, "container context is not started (state: "+string(cc.state)+")",
			errors.String("state", string(cc.state)),
		)
	}
	cc.mu.RUnlock()

	// Try root first
	if cc.root.Has(token) {
		return cc.root.Get(token)
	}

	return nil, newNotRegisteredError(token, "")
}

// List resolves all providers matching the filter from all containers.
func (cc *ContainerContext) List(filter FilterOptions) ([]any, error) {
	cc.mu.RLock()
	if cc.state != stateStarted {
		cc.mu.RUnlock()
		return nil, errors.New(CodeIllegalState, "container context is not started (state: "+string(cc.state)+")",
			errors.String("state", string(cc.state)),
		)
	}
	cc.mu.RUnlock()

	// root.List already deduplicates by token key and resolves each match.
	return cc.root.List(filter)
}

// Has returns true if any container has a provider for the given token.
func (cc *ContainerContext) Has(token Token) bool {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return cc.has(token)
}

func (cc *ContainerContext) has(token Token) bool {
	return cc.root.Has(token)
}

// Scope runs a function within a new scoped container.
// The scope is attached to the context.Context and automatically closed after fn returns.
func (cc *ContainerContext) Scope(ctx context.Context, fn func(ctx context.Context, scope ScopeContext) error) (retErr error) {
	scope, err := cc.CreateScope()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := scope.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeContainerClosed)
		}
	}()

	scopedCtx := scope.Context(ctx)
	return fn(scopedCtx, scope)
}

// CreateScope creates a detached scope. The caller is responsible for closing it.
func (cc *ContainerContext) CreateScope() (*DetachedScope, error) {
	cc.mu.RLock()
	if cc.state != stateStarted {
		cc.mu.RUnlock()
		return nil, errors.New(CodeIllegalState, "container context is not started (state: "+string(cc.state)+")",
			errors.String("state", string(cc.state)),
		)
	}
	cc.mu.RUnlock()

	scopeContainer := cc.root.CreateChild("scope")
	return &DetachedScope{
		ScopeContext: &scopedContainerAdapter{container: scopeContainer},
		container:    scopeContainer,
	}, nil
}

// Fork creates a test fork of this container context.
// The fork copies all registrations and allows overrides before starting.
func (cc *ContainerContext) Fork() *ContainerContextFork {
	return &ContainerContextFork{
		source:    cc,
		overrides: make(map[string]Registration),
	}
}

// IsScopedProvider reports whether the given token is registered as a scoped
// provider. This is a supported integration point for scope-aware framework
// code (for example go.putnami.dev/http, which uses it to decide whether a
// handler dependency must be resolved per request rather than at startup).
func (cc *ContainerContext) IsScopedProvider(token Token) bool {
	cc.mu.RLock()
	defer cc.mu.RUnlock()

	if p, _ := cc.root.findProvider(token, false); p != nil {
		return p.Scope == Scoped
	}
	return false
}

// --- ContainerContextFork ---

// ContainerContextFork creates an isolated copy of a ContainerContext for testing.
type ContainerContextFork struct {
	source    *ContainerContext
	overrides map[string]Registration
}

// Override replaces a provider in the fork. Must be called before Start().
func (f *ContainerContextFork) Override(token Token, factory Factory) *ContainerContextFork {
	f.overrides[token.Key()] = Provide(token, factory)
	return f
}

// OverrideValue replaces a provider with a fixed value.
func (f *ContainerContextFork) OverrideValue(token Token, value any) *ContainerContextFork {
	f.overrides[token.Key()] = ProvideValue(token, value)
	return f
}

// Start builds and starts the forked context with overrides applied.
func (f *ContainerContextFork) Start() (*ContainerContext, error) {
	cc := NewContainerContext(f.source.name + ".fork")

	// Copy registrations, applying overrides
	for _, reg := range f.source.registrations {
		key := reg.provider.Token.Key()
		if override, ok := f.overrides[key]; ok {
			if err := cc.Register(override); err != nil {
				return nil, err
			}
			delete(f.overrides, key)
		} else {
			if err := cc.Register(reg); err != nil {
				return nil, err
			}
		}
	}

	// Register any remaining overrides (new providers)
	for _, override := range f.overrides {
		if err := cc.Register(override); err != nil {
			return nil, err
		}
	}

	if err := cc.Start(); err != nil {
		return nil, err
	}
	return cc, nil
}

// --- scopedContainerAdapter ---

type scopedContainerAdapter struct {
	container *Container
}

func (a *scopedContainerAdapter) Get(token Token) (any, error) {
	return a.container.Get(token)
}

func (a *scopedContainerAdapter) List(filter FilterOptions) ([]any, error) {
	return a.container.List(filter)
}

func (a *scopedContainerAdapter) Has(token Token) bool {
	return a.container.Has(token)
}
