package openapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/contracts"
)

// DefaultDescribePath is the project-relative path Describe writes the spec
// to. Matches the runtime convention so the committed file lives in one
// place whether produced by describe mode or a manual export.
const DefaultDescribePath = "schema/openapi.json"

// DescribeGzPath is the gzipped companion path under .gen/. Always written
// by Describe regardless of opt-out so downstream tasks can serve the
// compressed copy without touching the project tree.
const DescribeGzPath = "schema/openapi.json.gz"

// PluginOptions configures the OpenAPI plugin.
type PluginOptions struct {
	// Title is the API title in the spec.
	Title string
	// Version is the API version in the spec.
	Version string
	// Description is the API description.
	Description string
	// Servers lists target servers.
	Servers []Server
	// Route is the HTTP path to serve the spec at. Default: "/_/openapi.json".
	Route string
	// CacheControl sets the Cache-Control header on the served spec.
	// Defaults to "no-store". The document describes the API's security
	// model (required roles/scopes, internal paths) and RegisterOn serves
	// it unauthenticated, so it must not be stored by shared caches/CDNs by
	// default — a "public" directive would let a CDN serve the document to
	// other clients even after the route is placed behind auth. Set this
	// explicitly (e.g. "public, max-age=3600") only when the spec is meant
	// to be public and publicly cacheable.
	CacheControl string
	// SecuritySchemes overrides the OpenAPI components.securitySchemes. When
	// empty, a default "bearerAuth" (HTTP bearer / JWT) scheme is emitted if any
	// route is secured. Set this to document non-bearer schemes (API keys,
	// OAuth2 flows) through the plugin path.
	SecuritySchemes map[string]SecuritySchemeObject
	// Contract is the canonical project contract whose enums, DTOs, and tagged
	// unions are projected into OpenAPI components. Endpoint types whose Go name
	// matches a contract node reference that canonical component.
	Contract *contracts.Manifest
}

// Plugin generates and serves an OpenAPI 3.0.3 specification.
//
// Routes can be added in two ways:
//   - explicit: AddRoute(DiscoveredRoute) for low-level callers,
//   - automatic: From(apiPlugin) so every endpoint registered on the api.Plugin
//     contributes to the spec without duplicate AddRoute calls.
type Plugin struct {
	opts      PluginOptions
	apiPlugin *api.Plugin
	// routes holds routes registered directly through AddRoute. Auto-discovered
	// routes from apiPlugin are read fresh whenever the spec is rendered so
	// lifecycle ordering cannot permanently snapshot a partial API surface.
	routes []DiscoveredRoute
	spec   *Document
	// specJSON is the latest rendered spec. Configure warms it for direct use;
	// Start, Describe, and the first runtime handler call after configure render
	// again from the final route set.
	specJSON     []byte
	configured   bool
	runtimeFresh bool
	mu           sync.RWMutex
}

// The openapi plugin is the OpenAPI source the api client generator reads from.
var _ api.SpecSource = (*Plugin)(nil)

// NewPlugin creates a new OpenAPI plugin.
func NewPlugin(opts PluginOptions) *Plugin {
	if opts.Route == "" {
		opts.Route = "/_/openapi.json"
	}
	if opts.Title == "" {
		opts.Title = "API"
	}
	if opts.Version == "" {
		opts.Version = "1.0.0"
	}
	if opts.CacheControl == "" {
		opts.CacheControl = "no-store"
	}
	return &Plugin{opts: opts}
}

// From wires the OpenAPI plugin to an api.Plugin so its DiscoveredRoutes feed the spec
// automatically. The spec is refreshed at Start and Describe after every Configurer has
// completed, so route-registering plugins that share the api plugin can be ordered around
// the OpenAPI plugin without permanently snapshotting a partial route set.
//
//	apiPlugin := api.New(httpServer)
//	openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "My API"}).From(apiPlugin)
//	app.New("myapp").Use(httpServer).Use(apiPlugin).Use(openapiPlugin)
func (p *Plugin) From(apiPlugin *api.Plugin) *Plugin {
	p.apiPlugin = apiPlugin
	return p
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return "openapi" }

// CapabilitySchemas exposes the concrete OpenAPI artifact to capability inventory.
func (*Plugin) CapabilitySchemas() []app.CapabilitySchema {
	return []app.CapabilitySchema{{Name: "openapi", Kind: "openapi", Path: DefaultDescribePath}}
}

// CapabilityDiscoverers reports no separate discoverer; the schema artifact is authoritative.
func (*Plugin) CapabilityDiscoverers() []app.CapabilityDiscoverer { return nil }

// AddRoute registers a route for inclusion in the OpenAPI spec.
func (p *Plugin) AddRoute(route DiscoveredRoute) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes = append(p.routes, route)
	p.runtimeFresh = false
}

// Spec returns the generated spec, or nil if not yet generated.
func (p *Plugin) Spec() *Document {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.spec
}

// Configure renders an initial spec and prepares it for serving. Start and Describe render
// again after the full configure phase so auto-discovered routes registered by sibling
// Configurers are included deterministically.
func (p *Plugin) Configure(_ context.Context, _ *app.Module) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configured = true
	p.runtimeFresh = false
	return p.renderLocked()
}

func (p *Plugin) renderLocked() error {
	routes := append([]DiscoveredRoute(nil), p.routes...)
	if p.apiPlugin != nil {
		for _, r := range p.apiPlugin.DiscoveredRoutes() {
			routes = append(routes, fromAPIRoute(r))
		}
	}
	spec := GenerateSpec(routes, Options{
		Title:            p.opts.Title,
		Version:          p.opts.Version,
		Description:      p.opts.Description,
		Servers:          p.opts.Servers,
		SecuritySchemes:  p.opts.SecuritySchemes,
		Contract:         p.opts.Contract,
		ClientContract:   clientContractFromAPI(p.apiPlugin),
		ConnectMethods:   connectMethodsFromAPI(p.apiPlugin),
		ConnectEncodings: connectEncodingsFromAPI(p.apiPlugin),
	})
	if err := validateFirstPartyClientContract(spec); err != nil {
		return err
	}
	body, err := spec.JSON()
	if err != nil {
		return err
	}
	p.spec = spec
	p.specJSON = body
	return nil
}

// Start refreshes the spec after every Configurer has completed.
func (p *Plugin) Start(_ context.Context, _ *app.Module) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.renderLocked(); err != nil {
		return err
	}
	p.runtimeFresh = true
	return nil
}

// Stop is a no-op for the OpenAPI plugin.
func (p *Plugin) Stop(_ context.Context, _ *app.Module) error { return nil }

// Describe implements app.Describer. Writes the spec to <ctx.OutputDir>/schema/openapi.json
// and a gzipped sibling at <ctx.OutputDir>/schema/openapi.json.gz so build-time tooling can
// either consume the readable JSON or stream the compressed copy. The runner is responsible
// for copying the JSON file into the project tree (and respecting opt-out); the .gz stays
// under .gen/ regardless since it's a build/runtime cache, not a reviewable artifact.
func (p *Plugin) Describe(ctx *app.DescribeContext) error {
	if !ctx.Wants(p.Name()) {
		// This run is not authoritative for the spec, so it says nothing about
		// whether an existing one is stale.
		return nil
	}
	// A plugin that produces no spec REMOVES an earlier run's copy instead of
	// leaving it.
	jsonPath := filepath.Join(ctx.OutputDir, DefaultDescribePath)
	gzPath := filepath.Join(ctx.OutputDir, DescribeGzPath)
	p.mu.Lock()
	if !p.configured {
		p.mu.Unlock()
		return removeStaleDescribeArtifacts(jsonPath, gzPath)
	}
	if err := p.renderLocked(); err != nil {
		p.mu.Unlock()
		return err
	}
	body := append([]byte(nil), p.specJSON...)
	p.mu.Unlock()
	if body == nil {
		return removeStaleDescribeArtifacts(jsonPath, gzPath)
	}

	if err := os.MkdirAll(filepath.Dir(jsonPath), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(jsonPath, body, 0o600); err != nil {
		return err
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		// zw.Close error is reported via the outer return — we only need
		// to flush state before propagating the original write error.
		_ = zw.Close() //nolint:errcheck
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(gzPath, buf.Bytes(), 0o600)
}

// removeStaleDescribeArtifacts drops the artifacts this run did not produce. A
// missing file is the expected case, not an error.
func removeStaleDescribeArtifacts(paths ...string) error {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) { //nolint:gosec // OutputDir is framework-controlled and the names are constants
			return err
		}
	}
	return nil
}

// OpenAPISpecJSON renders the current spec and returns its canonical JSON bytes,
// identical to the bytes Describe writes to schema/openapi.json and the runtime
// handler serves. It re-renders
// from the latest route set so auto-discovered routes are included, and is safe
// to call once Configure has run. This satisfies the api package's SpecSource
// interface so the client generator can read the spec in-memory — making the
// openapi → clients dependency explicit and avoiding any describe-ordering race
// on the on-disk spec file.
func (p *Plugin) OpenAPISpecJSON() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.configured {
		return nil, nil
	}
	if err := p.renderLocked(); err != nil {
		return nil, err
	}
	if p.specJSON == nil {
		return nil, nil
	}
	return append([]byte(nil), p.specJSON...), nil
}

// RegisterOn registers the spec-serving endpoint on an HTTP server plugin.
// Serving the document is the caller's explicit choice: the plugin never
// mounts the route by itself, because the route is unauthenticated and the
// document discloses the API's security model. Without RegisterOn the plugin
// still renders the document for Describe and for SpecSource.
func (p *Plugin) RegisterOn(server *phttp.ServerPlugin) {
	server.GET(p.opts.Route, p.handler())
}

func (p *Plugin) handler() phttp.Handler {
	return func(_ *phttp.Context) *phttp.Response {
		specJSON, err := p.runtimeSpecJSON()
		if err != nil {
			return phttp.InternalError(err.Error())
		}
		if specJSON == nil {
			return phttp.JSONStatus(503, map[string]string{
				"error": "OpenAPI spec not yet generated",
			})
		}

		resp := phttp.JSONBytes(specJSON)
		resp.Headers.Set("Cache-Control", p.opts.CacheControl)
		return resp
	}
}

func (p *Plugin) runtimeSpecJSON() ([]byte, error) {
	p.mu.RLock()
	if p.runtimeFresh {
		specJSON := p.specJSON
		p.mu.RUnlock()
		return specJSON, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.configured {
		return nil, nil
	}
	if !p.runtimeFresh {
		if err := p.renderLocked(); err != nil {
			return nil, err
		}
		p.runtimeFresh = true
	}
	return p.specJSON, nil
}

// fromAPIRoute converts an api.DiscoveredRoute (transport-agnostic, populated by the api
// plugin during Configure) into the openapi-specific DiscoveredRoute shape this package
// renders.
func fromAPIRoute(r api.DiscoveredRoute) DiscoveredRoute {
	throws := convertResponseMeta(r.Responses.Throws)
	additional := convertResponseMeta(r.Responses.Returns)
	security := securityFromAPIMeta(r.Meta.SecurityOptions)
	if security != nil && securityExcludesPath(security, r.Path) {
		security = nil
	}
	return DiscoveredRoute{
		Method:             r.Method,
		Path:               r.Path,
		StreamMode:         string(r.StreamMode),
		Description:        r.Description,
		Params:             r.ParamsSchema,
		Query:              r.QuerySchema,
		Body:               r.BodySchema,
		BodyBinary:         r.BodyBinary,
		Returns:            r.ReturnsSchema,
		ReturnsBinary:      r.ReturnsBinary,
		ProviderWire:       r.ProviderWire,
		ReturnsStatus:      r.ReturnsStatus,
		ReturnsDescription: r.ReturnsDescription,
		AdditionalReturns:  additional,
		ErrorCodes:         append([]errors.Code(nil), r.Responses.ErrorCodes...),
		ErrorRetryability:  cloneErrorRetryability(r.Responses.ErrorRetryability),
		ErrorDetails:       maps.Clone(r.Responses.ErrorDetails),
		Throws:             throws,
		Security:           security,
		ClientOptions:      r.Meta.ClientOptions,
	}
}

func cloneErrorRetryability(source map[errors.Code]bool) map[errors.Code]bool {
	if source == nil {
		return nil
	}
	result := make(map[errors.Code]bool, len(source))
	for code, retryable := range source {
		result[code] = retryable
	}
	return result
}

func securityExcludesPath(meta *SecurityMeta, path string) bool {
	for _, prefix := range meta.ExcludePaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func clientContractFromAPI(plugin *api.Plugin) *clientcontract.DocumentV1 {
	if plugin == nil {
		return nil
	}
	return plugin.ClientServiceContract()
}

func connectMethodsFromAPI(plugin *api.Plugin) map[string]string {
	if plugin == nil {
		return nil
	}
	return plugin.ClientProtobufMethods()
}

func connectEncodingsFromAPI(plugin *api.Plugin) []clientcontract.Encoding {
	if plugin == nil {
		return nil
	}
	return plugin.ClientConnectEncodings()
}

func validateFirstPartyClientContract(spec *Document) error {
	if spec == nil || spec.ClientContract == nil {
		return nil
	}
	// Generation already knows why a route could not be projected. Reporting that
	// reason first keeps the actionable message ("this provider has no transport
	// that can carry it") ahead of the structural symptom the validator sees.
	if spec.generationErr != nil {
		return spec.generationErr
	}
	if diags := clientcontract.ValidateDocument(spec.ClientContract); len(diags) > 0 {
		return fmt.Errorf("openapi: invalid document x-putnami-client: %s", diags[0].String())
	}
	paths := make([]string, 0, len(spec.Paths))
	for path := range spec.Paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		pathItem := spec.Paths[path]
		methods := make([]string, 0, len(pathItem))
		for method := range pathItem {
			methods = append(methods, method)
		}
		sort.Strings(methods)
		for _, method := range methods {
			operation := pathItem[method]
			if operation.ExternalContract != "" {
				// An external authority owns this operation; the strict reader
				// below refuses it if it also carries x-putnami-client.
				continue
			}
			if operation.ClientContract == nil {
				return fmt.Errorf("openapi: %s %s is missing operation x-putnami-client", strings.ToUpper(method), path)
			}
			if diags := clientcontract.ValidateOperation(operation.ClientContract, spec.ClientContract); len(diags) > 0 {
				return fmt.Errorf("openapi: invalid %s %s x-putnami-client: %s", strings.ToUpper(method), path, diags[0].String())
			}
		}
	}
	// Re-read the exact serialized provider document through the strict neutral
	// reader. This validates component/request/response schemas and their refs in
	// addition to the extension blocks above, so the provider fails before it can
	// publish an unrepresentable first-party contract.
	body, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("openapi: serialize first-party contract for validation: %w", err)
	}
	if _, err := api.ReadOpenAPISpec(body); err != nil {
		return fmt.Errorf("openapi: invalid first-party client contract: %w", err)
	}
	return nil
}

// convertResponseMeta maps api.ResponseMeta (status / description / schema) into the
// openapi ThrowsMeta shape. ThrowsMeta is reused for both error responses and additional
// success responses since the wire shape is identical.
func convertResponseMeta(in []api.ResponseMeta) []ThrowsMeta {
	if len(in) == 0 {
		return nil
	}
	out := make([]ThrowsMeta, 0, len(in))
	for _, r := range in {
		out = append(out, ThrowsMeta{
			Status:      r.Status,
			Description: r.Description,
			Schema:      r.Schema,
		})
	}
	return out
}

// securityFromAPIMeta extracts Roles / Scopes from the api endpoint's SecurityOptions.
//
// The api package keeps SecurityOptions as `any` so it does not depend on the security
// package. Rules are read through the typed phttp.SecurityAuthorizationPolicy or
// phttp.SecurityClaims accessors — rename-safe and reflection-free. If no specific
// claims are present (e.g. a guard function) we still return a non-nil *SecurityMeta
// so the operation is documented as auth-gated. Whether the rule serves anonymous
// callers is read from phttp.SecurityOptionalAuthentication on every path.
func securityFromAPIMeta(opts any) *SecurityMeta {
	if opts == nil {
		return nil
	}
	out := &SecurityMeta{Optional: optionalAuthentication(opts)}
	if policy, ok := opts.(phttp.SecurityAuthorizationPolicy); ok {
		declared := policy.SecurityAuthorizationPolicy()
		out.Roles = append(out.Roles, declared.RolesAll...)
		out.Scopes = append(out.Scopes, declared.ScopesAll...)
		out.ExcludePaths = append(out.ExcludePaths, declared.ExcludePaths...)
		out.Authorization = &clientcontract.Authorization{
			Clients:   append([]string(nil), declared.Clients...),
			ScopesAll: append([]string(nil), declared.ScopesAll...),
			ScopesAny: append([]string(nil), declared.ScopesAny...),
			RolesAll:  append([]string(nil), declared.RolesAll...),
			RolesAny:  append([]string(nil), declared.RolesAny...),
		}
		out.Representable = true
		return out
	}
	if claims, ok := opts.(phttp.SecurityClaims); ok {
		out.Roles = append(out.Roles, claims.SecurityRoles()...)
		out.Scopes = append(out.Scopes, claims.SecurityScopes()...)
		out.Representable = true
		return out
	}
	return out
}

// optionalAuthentication reads the typed phttp.SecurityOptionalAuthentication
// claim. There is no reflection fallback: a rule serves anonymous callers only
// when it says so through the interface, so a field that merely looks like the
// claim can never publish an anonymous alternative.
func optionalAuthentication(opts any) bool {
	claim, ok := opts.(phttp.SecurityOptionalAuthentication)
	return ok && claim.OptionalAuthentication()
}
