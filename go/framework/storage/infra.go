package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	protocaps "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	storageproto "go.putnami.dev/protocol/storage"
)

// sidecarSlug names this producer's per-project infra scratch fragment
// (<project>/.gen/infra/storage.json). The Go generator syncs it into
// committed infra/requirements.json.
const sidecarSlug = "storage"

// Plugin participates in the application lifecycle in two phases. During the
// build's describe phase it walks the bucket registry and writes a
// framework-generated infra scratch fragment declaring the project's
// object-storage requirements. At runtime it provides a storage.Backend into
// the DI container, resolved from the injected managed bindings — the
// BindingsDocument carried by the "storage" section of the resolved config
// (see ConfigSection), falling back to the STORAGE_BINDINGS env transport — so
// a workload's registered (logical) buckets resolve to the provider buckets a
// deploy target provisioned for them. The provided backend enforces the
// registered buckets' size and MIME constraints.
type Plugin struct {
	registry *Registry
	backend  Backend

	// configDoc is the managed bindings document resolved from the "storage"
	// config section (nil when the section carries none). It is resolved once,
	// lazily, by whichever of the Backend factory or Configure runs first —
	// the factory is registered lazy but an eager consumer can resolve it
	// before Configure — and memoized via configOnce so both observe the same
	// document for one container build. Provides resets the memo before each
	// lifecycle pass so Validate/failed Start retries re-read config instead of
	// keeping stale nil/error outcomes. It is preferred over the STORAGE_BINDINGS
	// env transport. Read it through resolveConfigDoc, never directly.
	configDoc  *BindingsDocument
	configErr  error
	configOnce sync.Once
}

// NewPlugin creates a storage plugin bound to the global bucket registry.
func NewPlugin() *Plugin {
	return &Plugin{registry: GetRegistry()}
}

// Name implements app.Plugin.
func (p *Plugin) Name() string { return "storage" }

// DesignInfraRequirements projects registered buckets through app's bounded
// infra seam. The bucket registry is the same native source Describe uses.
func (p *Plugin) DesignInfraRequirements() []app.DesignInfraRequirement {
	buckets := p.registryOrGlobal().All()
	requirements := make([]app.DesignInfraRequirement, 0, len(buckets))
	for _, bucket := range buckets {
		if bucket != nil {
			requirements = append(requirements, app.DesignInfraRequirement{Name: bucket.Name, Kind: protocaps.InfraKindStorage})
		}
	}
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].Name < requirements[j].Name })
	return requirements
}

// Compile-time checks: the plugin resolves the managed bindings document
// during configure, emits infra requirements during describe, and provides a
// runtime-bound storage.Backend into the DI container.
var (
	_ app.Configurer = (*Plugin)(nil)
	_ app.Describer  = (*Plugin)(nil)
	_ app.Provider   = (*Plugin)(nil)
)

// Configure resolves the managed bindings document from the "storage" section
// of the resolved application config — the config-resolution transport for
// managed bindings — surfacing a malformed document at startup with the same
// diagnostics the env transport produces. The Backend factory may already
// have resolved it (an eager consumer can resolve Backend before Configure);
// resolveConfigDoc memoizes so both paths observe the same document. A
// document there wins over EnvBindings; with none, the env transport (and the
// fail-closed unbound error) behave exactly as before, so local serve/test
// flows without bindings are unchanged.
func (p *Plugin) Configure(ctx context.Context, _ *app.Module) error {
	_, err := p.resolveConfigDoc(ctx)
	return err
}

// resolveConfigDoc resolves (once) the managed bindings document carried by
// the "storage" config section and memoizes the outcome, so the Backend
// factory and Configure — whichever runs first — observe the same document.
// It holds no lock beyond the sync.Once, so there is no lock-ordering hazard.
func (p *Plugin) resolveConfigDoc(ctx context.Context) (*BindingsDocument, error) {
	p.configOnce.Do(func() {
		p.configDoc, p.configErr = loadConfigBindings(ctx)
	})
	return p.configDoc, p.configErr
}

// resetConfigDoc clears the per-container-build config memo before the app
// collects provider registrations for a lifecycle pass. This preserves the
// Backend-factory/Configure ordering guarantee within one pass while allowing a
// later Validate/Start retry to observe newly available or repaired config.
func (p *Plugin) resetConfigDoc() {
	p.configDoc = nil
	p.configErr = nil
	p.configOnce = sync.Once{}
}

// Provides registers a storage.Backend resolved from the injected managed
// bindings (the resolved-config section first, then the STORAGE_BINDINGS env
// contract; see Configure). The registration is lazy: a workload that added
// storage.NewPlugin() only for its describe-phase infra requirements, or that
// runs locally without bindings, is unaffected unless it actually resolves
// storage.Backend — at which point an absent or malformed contract fails
// closed with a clear error. The provided backend remaps logical buckets to
// their bound provider buckets and enforces the constraints of the plugin's
// registered buckets on Put and on SignedPutURL (see ConstrainedBackend).
func (p *Plugin) Provides() []inject.Registration {
	p.resetConfigDoc()
	return []inject.Registration{
		inject.Provide(inject.TokenOf[Backend](), p.provideBackend,
			inject.WithLazy(), inject.WithOnClose(p.closeBackend)),
	}
}

// provideBackend is the lazy Backend factory: it resolves the managed bindings
// (config section first, env fallback), fails closed when neither transport
// carries a document, and wraps the bound backend so it enforces the registered
// constraints on logical bucket names. Closing the provided backend closes the
// bound backend once.
func (p *Plugin) provideBackend(_ inject.Resolver) (any, error) {
	bound, err := p.resolveBackend(context.Background())
	if err != nil {
		return nil, err
	}
	if bound == nil {
		return nil, errors.New(CodeStorageUnbound,
			"no storage bindings injected: the deploy target must inject the "+ConfigSection+" config section or set "+EnvBindings,
			errors.String("section", ConfigSection), errors.String("env", EnvBindings))
	}
	backend := &ConstrainedBackend{Backend: bound, registry: p.registryOrGlobal()}
	p.backend = backend
	return backend, nil
}

// resolveBackend builds the bound Backend from the managed bindings: the
// document carried by the "storage" config section when it has one (config
// wins; resolved lazily and memoized, so it does not depend on Configure
// having run), the EnvBindings transport otherwise. It returns (nil, nil)
// when neither transport carries bindings so the caller keeps the fail-closed
// unbound diagnostic.
func (p *Plugin) resolveBackend(ctx context.Context) (Backend, error) {
	doc, err := p.resolveConfigDoc(ctx)
	if err != nil {
		return nil, err
	}
	if doc != nil {
		return BackendFromBindings(ctx, doc)
	}
	return DiscoverBackend(ctx)
}

func (p *Plugin) closeBackend() error {
	if p.backend == nil {
		return nil
	}
	backend := p.backend
	p.backend = nil
	return backend.Close()
}

// Describe implements app.Describer. It derives the project's object-storage
// requirements from the registered buckets and writes a per-project infra
// scratch fragment at <ctx.OutputDir>/infra/storage.json, where OutputDir is
// the workload's ".gen" directory.
//
// Bucket names are validated against the infra resource-name pattern; a
// violation aborts describe with a diagnostic rather than emitting an invalid
// manifest. Projects with no registered buckets emit no manifest and any stale
// scratch fragment from a previous run is removed.
func (p *Plugin) Describe(ctx *app.DescribeContext) error {
	if ctx == nil || !ctx.Wants(p.Name()) {
		return nil
	}
	manifest, diags := buildInfraManifest(p.registryOrGlobal().All())
	if diag.HasErrors(diags) {
		return fmt.Errorf("storage: invalid infra storage requirements:\n%s", diag.ErrorText(diags))
	}
	var fragment infra.PerProjectManifest
	if manifest != nil {
		fragment = *manifest
	}
	return infra.WriteSidecarIn(ctx.OutputDir, sidecarSlug, fragment)
}

// registryOrGlobal returns the plugin's registry, falling back to the global
// one when the plugin was constructed without an explicit registry.
func (p *Plugin) registryOrGlobal() *Registry {
	if p.registry != nil {
		return p.registry
	}
	return GetRegistry()
}

// buildInfraManifest derives a per-project infra manifest from the registered
// buckets by projecting a canonical storage Manifest through
// infra.StoragesFromManifest.
//
// Each registered bucket becomes a storage Resource carrying its access, public
// flag, and retention; an unset access defaults to readwrite. The bridge sorts
// by name, so the output is deterministic regardless of registry iteration order. It
// returns (nil, nil) when no buckets are registered, and a non-empty diagnostic
// slice when a bucket name or access violates the protocol.
func buildInfraManifest(buckets []*BucketDefinition) (*infra.PerProjectManifest, []diag.Diagnostic) {
	resources := make([]storageproto.Resource, 0, len(buckets))
	for _, b := range buckets {
		if b == nil {
			continue
		}
		access := b.Options.Access
		if access == "" {
			access = storageproto.AccessReadWrite
		}
		resources = append(resources, storageproto.Resource{
			Name:      b.Name,
			Access:    access,
			Public:    b.Options.Public,
			Retention: strings.TrimSpace(b.Options.Retention),
		})
	}
	if len(resources) == 0 {
		return nil, nil
	}

	entries, diags := infra.StoragesFromManifest(&storageproto.Manifest{
		ProtocolVersion: storageproto.ProtocolVersion,
		Resources:       resources,
	})
	if diag.HasErrors(diags) {
		return nil, diags
	}

	m := &infra.PerProjectManifest{
		Schema:          infra.PerProjectSchemaURL,
		ProtocolVersion: infra.ProtocolVersion,
		Storage:         entries,
	}
	if vdiags := infra.ValidatePerProjectManifest(m); diag.HasErrors(vdiags) {
		return nil, vdiags
	}
	return m, nil
}
