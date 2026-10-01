package inject

import (
	"sync"

	"go.putnami.dev/errors"
)

// Container is a hierarchical dependency injection container.
// It supports parent-child relationships with visibility control,
// singleton caching, tag-based multi-resolution, and circular dependency detection.
type Container struct {
	name       string
	parent     *Container
	children   []*Container
	providers  map[string]*Provider // key: token.Key()
	instances  map[string]any       // key: token.Key() — instance cache
	building   map[string]*buildState
	tags       map[string][]Token // tag → tokens
	closeHooks []closeHookEntry
	closed     bool
	mu         sync.RWMutex
}

type buildState struct {
	done  chan struct{}
	val   any
	err   error
	token Token   // the token this build produces (for cycle reporting)
	chain []Token // the resolution chain of the goroutine building it
	// waitingOn is the single build this build's goroutine is currently blocked
	// on or synchronously descending into (a collision wait on another
	// goroutine's build, or a nested create of a child build), or nil. Guarded by
	// buildWaitMu; the two edge kinds together form the full wait-for graph used
	// to detect a cross-goroutine cycle (e.g. a→b→a or a→b→c→a first-resolved on
	// separate goroutines) before it deadlocks.
	waitingOn *buildState
}

// buildWaitMu guards buildState.waitingOn across the whole process so the
// register-and-detect step is atomic against concurrent waiters.
var buildWaitMu sync.Mutex

// registerWait records that `from` is about to block on `on`. It returns a
// non-nil token cycle when doing so would deadlock — i.e. following the
// waitingOn chain from `on` leads back to `from` — and does not register the
// edge in that case. The caller must clearWait(from) once the wait completes.
func registerWait(from, on *buildState) []Token {
	buildWaitMu.Lock()
	defer buildWaitMu.Unlock()
	for cur := on; cur != nil; cur = cur.waitingOn {
		if cur == from {
			cycle := make([]Token, 0, len(from.chain)+2)
			cycle = append(cycle, from.chain...)
			cycle = append(cycle, on.token)
			if len(from.chain) > 0 {
				cycle = append(cycle, from.chain[0]) // close the loop for reporting
			}
			return cycle
		}
	}
	from.waitingOn = on
	return nil
}

func clearWait(from *buildState) {
	buildWaitMu.Lock()
	from.waitingOn = nil
	buildWaitMu.Unlock()
}

type closeHookEntry struct {
	token Token
	hook  CloseHook
}

// NewContainer creates a new container with the given name and optional parent.
func NewContainer(name string, parent *Container) *Container {
	c := &Container{
		name:      name,
		parent:    parent,
		providers: make(map[string]*Provider),
		instances: make(map[string]any),
		building:  make(map[string]*buildState),
		tags:      make(map[string][]Token),
	}
	if parent != nil {
		parent.mu.Lock()
		parent.children = append(parent.children, c)
		parent.mu.Unlock()
	}
	return c
}

// Name returns the container name.
func (c *Container) Name() string {
	return c.name
}

// Register adds a provider registration to this container.
func (c *Container) Register(reg Registration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return newContainerClosedError(c.name)
	}

	p := reg.provider
	key := p.Token.Key()

	if _, exists := c.providers[key]; exists {
		return newDuplicateProviderError(p.Token, c.name)
	}

	providerCopy := p
	c.providers[key] = &providerCopy

	// Index tags
	for _, tag := range p.Tags {
		c.tags[tag] = append(c.tags[tag], p.Token)
	}

	// Register the close hook for singleton providers, whose instance lives in
	// this container. Scoped providers create a per-scope instance, so their
	// hook is registered on the owning scope when it instantiates the provider
	// (see instantiate) and runs on that scope's Close.
	if p.OnClose != nil && p.Scope != Scoped {
		c.closeHooks = append(c.closeHooks, closeHookEntry{
			token: p.Token,
			hook:  p.OnClose,
		})
	}

	return nil
}

// Get resolves a single dependency by token. It walks up the parent chain
// respecting visibility rules and caches singleton instances.
func (c *Container) Get(token Token) (any, error) {
	return c.resolve(token, nil, nil)
}

// List resolves all providers matching the given filter options.
// It collects from this container and walks up the parent chain.
func (c *Container) List(filter FilterOptions) ([]any, error) {
	return c.list(filter, nil, nil)
}

// list resolves all matching providers, carrying the active resolution chain
// so that cycle detection keeps working for dependencies pulled in via
// ResolveAll during a factory invocation.
func (c *Container) list(filter FilterOptions, chain []Token, waiter *buildState) ([]any, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, newContainerClosedError(c.name)
	}
	c.mu.RUnlock()

	tokens := c.collectTagged(filter.Tag(), false)
	results := make([]any, 0, len(tokens))
	seen := make(map[string]bool)

	for _, tok := range tokens {
		key := tok.Key()
		if seen[key] {
			continue
		}
		seen[key] = true

		val, err := c.resolve(tok, chain, waiter)
		if err != nil {
			return nil, err
		}
		results = append(results, val)
	}
	return results, nil
}

// Has returns true if a provider for the given token exists in this container
// or any parent (respecting visibility).
func (c *Container) Has(token Token) bool {
	provider, _ := c.findProvider(token, false)
	return provider != nil
}

// CreateChild creates a child container with this container as parent.
func (c *Container) CreateChild(name string) *Container {
	return NewContainer(name, c)
}

// Validate checks the container for issues: missing dependencies,
// circular dependencies, and scope violations.
func (c *Container) Validate() []error {
	c.mu.RLock()
	providers := make([]*Provider, 0, len(c.providers))
	for _, p := range c.providers {
		providers = append(providers, p)
	}
	c.mu.RUnlock()

	var issues []error

	// Check for missing dependencies
	for _, p := range providers {
		for _, dep := range p.Deps {
			if found, _ := c.findProvider(dep, false); found == nil {
				issues = append(issues, newNotRegisteredError(dep, "required by "+TokenName(p.Token)))
			}
		}
	}

	// Detect circular dependencies
	issues = append(issues, c.detectCycles()...)

	// Check scope violations
	for _, p := range providers {
		if p.Scope == Singleton {
			for _, dep := range p.Deps {
				depProvider, _ := c.findProvider(dep, false)
				if depProvider != nil && depProvider.Scope == Scoped {
					issues = append(issues, newScopeViolationError(p.Token, dep))
				}
			}
		}
	}

	return issues
}

// ResolveAll resolves all non-lazy singleton providers in this container.
func (c *Container) ResolveAll() error {
	c.mu.RLock()
	providers := make([]*Provider, 0, len(c.providers))
	for _, p := range c.providers {
		providers = append(providers, p)
	}
	c.mu.RUnlock()

	for _, p := range providers {
		if p.Scope == Singleton && !p.Lazy {
			if _, err := c.resolve(p.Token, nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close disposes of all resources in this container and its children.
// Close hooks are called in reverse registration order.
//
// A singleton's close hook runs only if its instance was actually built: a
// registered-but-never-resolved singleton (for example a lazy DB pool that no
// Get ever opened) holds no resource, so invoking its OnClose would dispose a
// resource that was never acquired. Scoped close hooks are registered at build
// time, so they are always "built" and always run.
func (c *Container) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true

	// Copy children and hooks under lock, and snapshot which tokens have a
	// built instance so we only dispose resources that were actually acquired.
	children := make([]*Container, len(c.children))
	copy(children, c.children)
	hooks := make([]closeHookEntry, len(c.closeHooks))
	copy(hooks, c.closeHooks)
	built := c.builtKeysLocked()
	c.mu.Unlock()

	// Close children first
	var errs []error
	for _, child := range children {
		if err := child.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	errs = append(errs, runCloseHooks(hooks, built)...)

	if len(errs) > 0 {
		return newValidationError(errs)
	}
	return nil
}

// builtKeysLocked returns the set of token keys with a cached instance. The
// caller must hold c.mu.
func (c *Container) builtKeysLocked() map[string]bool {
	built := make(map[string]bool, len(c.instances))
	for key := range c.instances {
		built[key] = true
	}
	return built
}

// runCloseHooks invokes the close hooks for built instances in reverse
// registration order, collecting any errors. A nil built map means "run them
// all" is intentionally NOT supported — callers always pass an explicit set so
// never-built singletons are skipped consistently across Close and resetBuilt.
func runCloseHooks(hooks []closeHookEntry, built map[string]bool) []error {
	var errs []error
	for i := len(hooks) - 1; i >= 0; i-- {
		if !built[hooks[i].token.Key()] {
			continue
		}
		if err := hooks[i].hook(); err != nil {
			errs = append(errs, errors.Wrapf(err, CodeContainerClosed, "close hook for "+TokenName(hooks[i].token),
				errors.String("token", TokenName(hooks[i].token)),
			))
		}
	}
	return errs
}

// resetBuilt runs the close hooks for singletons that were actually
// instantiated in this container and then clears the instance cache, leaving
// the container open and reusable. It is used to roll back a partial eager
// boot (a failed ResolveAll) so that already-constructed singletons — DB pools,
// listeners — release their resources instead of leaking, while still allowing
// the owner to retry Start. Unlike Close, it does not mark the container closed
// and it never touches scoped instances (those live in per-scope children that
// are not created until after a successful Start).
//
// Hooks run in reverse registration order, mirroring Close, and only for
// tokens whose instance was built; a registered-but-never-built singleton's
// hook is left untouched. The registration-time close-hook list is preserved
// so a later successful Start followed by Close still disposes singletons
// correctly. All hook errors are collected and returned as a single
// aggregated error.
func (c *Container) resetBuilt() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}

	// Copy the hooks and snapshot which tokens actually have a built instance,
	// then clear only the build/instance state under the same lock. The
	// closeHooks slice itself is left intact: it is static registration
	// metadata that a retried Start + Close still needs.
	hooks := make([]closeHookEntry, len(c.closeHooks))
	copy(hooks, c.closeHooks)
	built := c.builtKeysLocked()
	c.instances = make(map[string]any)
	c.building = make(map[string]*buildState)
	c.mu.Unlock()

	if errs := runCloseHooks(hooks, built); len(errs) > 0 {
		return newValidationError(errs)
	}
	return nil
}

// --- internal resolution ---

// resolve resolves a token from this container, walking up the parent chain via
// findProvider as needed. chain holds the tokens currently being constructed in
// this resolution, used for cycle detection.
func (c *Container) resolve(token Token, chain []Token, waiter *buildState) (any, error) {
	key := token.Key()

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, newContainerClosedError(c.name)
	}
	inst, cached := c.instances[key]
	c.mu.RUnlock()
	if cached {
		return inst, nil
	}

	// fromChild is false: the requesting container sees its own providers,
	// including private ones; findProvider applies the visibility rule as it
	// walks to parents.
	provider, owner := c.findProvider(token, false)
	if provider == nil {
		return nil, newNotRegisteredError(token, "")
	}

	return c.instantiate(provider, owner, token, chain, waiter)
}

// instantiate creates an instance from a provider and caches it according to
// the provider's scope. Singletons are instantiated and cached in the owning
// container so every descendant shares one instance (and the owner's Close runs
// the close hook). Scoped providers are instantiated and cached in the
// requesting container c (the scope), and their close hook is registered there
// so the resource is released on scope Close.
//
// Cycle detection uses the per-resolution chain rather than shared container
// state, so concurrent first resolutions of the same provider never produce a
// spurious circular-dependency error, and the reported chain is the full path.
func (c *Container) instantiate(provider *Provider, owner *Container, token Token, chain []Token, waiter *buildState) (any, error) {
	key := token.Key()

	// cacheIn owns the instance for this resolution: the provider's owner for
	// singletons, or the requesting scope for scoped providers. Dependencies are
	// resolved from the same container so scoped sub-dependencies land in the
	// scope and singleton sub-dependencies land in their owner.
	cacheIn := owner
	if provider.Scope == Scoped {
		// A scoped provider must be cached in a scope — a descendant container
		// created per request — never in its declaring container. When the
		// requesting container is the owner itself, a singleton/root-level
		// resolution is trying to adopt the scoped instance at the owner's
		// (root) lifetime: the scope-violation the validator catches for
		// declared deps, here for an undeclared runtime dependency that escaped
		// validation. Reject it instead of silently caching it on root.
		if c == owner {
			return nil, newScopeResolutionViolation(token, chain)
		}
		cacheIn = c
	}

	// Fast path: already instantiated.
	cacheIn.mu.RLock()
	if inst, ok := cacheIn.instances[key]; ok {
		cacheIn.mu.RUnlock()
		return inst, nil
	}
	cacheIn.mu.RUnlock()

	// Cycle detection: the token is already on the active resolution chain.
	for i, t := range chain {
		if t.Key() == key {
			cycle := make([]Token, 0, len(chain)-i+1)
			cycle = append(cycle, chain[i:]...)
			cycle = append(cycle, token)
			return nil, newCircularDependencyError(cycle)
		}
	}

	cacheIn.mu.Lock()
	if inst, ok := cacheIn.instances[key]; ok {
		cacheIn.mu.Unlock()
		return inst, nil
	}
	if build, ok := cacheIn.building[key]; ok {
		cacheIn.mu.Unlock()
		// Another goroutine is already building this token. If the caller is
		// itself a factory (waiter != nil), waiting could deadlock: with an
		// undeclared cycle X→Y→X first-resolved on two goroutines, each would
		// block on the other's build. Detect that wait-for cycle and return a
		// circular-dependency error instead of blocking forever.
		if waiter != nil {
			if cycle := registerWait(waiter, build); cycle != nil {
				return nil, newCircularDependencyError(cycle)
			}
			defer clearWait(waiter)
		}
		<-build.done
		return build.val, build.err
	}
	newChain := append(append(make([]Token, 0, len(chain)+1), chain...), token)
	build := &buildState{done: make(chan struct{}), token: token, chain: newChain}
	cacheIn.building[key] = build
	cacheIn.mu.Unlock()

	// Record the create-descent edge in the wait-for graph: the calling factory
	// (waiter) is now synchronously building this new child, so it cannot return
	// until the child's factory does. Without this edge the graph captured only
	// collision waits, so a cross-goroutine cycle whose closing edge runs through
	// a nested create — e.g. an undeclared a→b→c→a first-resolved on three
	// goroutines — stayed invisible and deadlocked. The child has no waitingOn yet
	// so this can never form a cycle at creation time (registerWait returns nil);
	// it is recorded only so a LATER collision wait that closes the loop can see
	// this edge and report the cycle instead of blocking forever.
	if waiter != nil {
		_ = registerWait(waiter, build)
		defer clearWait(waiter)
	}

	resolver := &containerResolver{
		container: cacheIn,
		chain:     newChain,
		build:     build,
	}

	instance, err := provider.Factory(resolver)
	if err != nil {
		err = errors.Wrapf(err, CodeFactoryFailed, "factory for "+TokenName(token)+" failed",
			errors.String("token", TokenName(token)),
		)
	}

	// Publish the result to any waiters. Only the goroutine that created the
	// build state runs the factory, so singleton/scoped constructors keep their
	// "once per cache owner" contract even under concurrent first resolution.
	cacheIn.mu.Lock()
	if err == nil {
		cacheIn.instances[key] = instance
		if provider.Scope == Scoped && provider.OnClose != nil {
			cacheIn.closeHooks = append(cacheIn.closeHooks, closeHookEntry{token: token, hook: provider.OnClose})
		}
	}
	build.val = instance
	build.err = err
	delete(cacheIn.building, key)
	close(build.done)
	cacheIn.mu.Unlock()

	return instance, err
}

// findProvider locates a provider and the container that owns it, walking up
// the parent chain and honoring visibility. It locks each container as it reads
// that container's provider map, so callers must not hold any container lock.
func (c *Container) findProvider(token Token, fromChild bool) (*Provider, *Container) {
	key := token.Key()

	c.mu.RLock()
	p, ok := c.providers[key]
	parent := c.parent
	c.mu.RUnlock()

	if ok && (!fromChild || p.Visibility != Private) {
		return p, c
	}
	if parent != nil {
		return parent.findProvider(token, true)
	}
	return nil, nil
}

// collectTagged collects all tokens with the given tag from this container and parents.
func (c *Container) collectTagged(tag string, fromChild bool) []Token {
	var result []Token

	c.mu.RLock()
	if tokens, ok := c.tags[tag]; ok {
		for _, tok := range tokens {
			p := c.providers[tok.Key()]
			if p != nil && (!fromChild || p.Visibility != Private) {
				result = append(result, tok)
			}
		}
	}
	c.mu.RUnlock()

	if c.parent != nil {
		result = append(result, c.parent.collectTagged(tag, true)...)
	}
	return result
}

// detectCycles uses DFS over declared dependencies to detect circular
// dependencies. It snapshots the provider map under the read lock so callers
// need not hold it.
func (c *Container) detectCycles() []error {
	c.mu.RLock()
	providers := make(map[string]*Provider, len(c.providers))
	for k, v := range c.providers {
		providers[k] = v
	}
	c.mu.RUnlock()

	var issues []error
	visited := make(map[string]bool)
	visiting := make(map[string]bool)

	var visit func(token Token, chain []Token) bool
	visit = func(token Token, chain []Token) bool {
		key := token.Key()
		if visited[key] {
			return false
		}
		if visiting[key] {
			// Found a cycle — build the chain from the cycle start
			cycleStart := -1
			for i, t := range chain {
				if t.Key() == key {
					cycleStart = i
					break
				}
			}
			if cycleStart >= 0 {
				cycle := make([]Token, len(chain)-cycleStart+1)
				copy(cycle, chain[cycleStart:])
				cycle[len(cycle)-1] = token
				issues = append(issues, newCircularDependencyError(cycle))
			}
			return true
		}

		visiting[key] = true
		chain = append(chain, token)

		p := providers[key]
		if p != nil {
			for _, dep := range p.Deps {
				if visit(dep, chain) {
					break
				}
			}
		}

		delete(visiting, key)
		visited[key] = true
		return false
	}

	for _, p := range providers {
		visit(p.Token, nil)
	}
	return issues
}

// --- containerResolver ---

// containerResolver implements Resolver for use during factory invocation.
// chain carries the tokens currently being constructed so that nested
// resolutions can detect circular dependencies.
type containerResolver struct {
	container *Container
	chain     []Token
	// build is the buildState this factory is producing, threaded into nested
	// resolutions so a nested wait on another goroutine's build can detect a
	// cross-goroutine wait-for cycle.
	build *buildState
}

func (r *containerResolver) Resolve(token Token) (any, error) {
	return r.container.resolve(token, r.chain, r.build)
}

func (r *containerResolver) ResolveAll(filter FilterOptions) ([]any, error) {
	return r.container.list(filter, r.chain, r.build)
}
