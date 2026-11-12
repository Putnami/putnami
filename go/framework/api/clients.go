package api

import (
	"context"
	"fmt"

	"go.putnami.dev/app"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// describerNameClients is the describe target name for the client declaration.
const describerNameClients = "clients"

// SpecSource is implemented by a plugin that can render the OpenAPI specification
// in-memory. The client declaration collects it from the module tree (the openapi
// plugin satisfies it) and reads the spec directly, rather than depending on the
// openapi describer having already written .gen/schema/openapi.json. This makes
// the openapi → clients dependency explicit (a typed reference resolved via
// [app.Collect]) and removes the intra-describe file-ordering hazard.
type SpecSource interface {
	// OpenAPISpecJSON renders and returns the current OpenAPI spec as JSON.
	OpenAPISpecJSON() ([]byte, error)
}

// ClientsOptions configures the client declaration.
type ClientsOptions struct {
	// ThirdParty permits an OpenAPI document that has no x-putnami-client
	// contract. Provider-owned contracts are strict by default.
	ThirdParty bool
}

// ClientsPlugin declares that the application's API is a client contract. It
// names no client: no target, output, package or module. A client project
// names its provider in the generatedFrom member of its own putnami.json and
// generates itself from the provider's committed schema/openapi.json.
//
// The declaration decides two things about the contract:
//
//   - each operation's producer: every route of an api plugin whose owning
//     module declares a feature carries that project and feature, and the
//     OpenAPI projection writes it into the operation's x-putnami-client;
//   - strictness: describe fails when the document carries no
//     x-putnami-client, unless ThirdParty is set.
//
// Usage:
//
//	apiPlugin := api.New(httpServer)
//	openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "Items"}).From(apiPlugin)
//	clients := api.Clients(api.ClientsOptions{}).From(apiPlugin)
//	app.New("svc").Use(httpServer).Use(apiPlugin).Use(openapiPlugin).Use(clients)
type ClientsPlugin struct {
	opts      ClientsOptions
	apiPlugin *Plugin
	module    *app.Module
}

// Clients creates a client declaration.
func Clients(opts ClientsOptions) *ClientsPlugin {
	return &ClientsPlugin{opts: opts}
}

// From wires the api.Plugin whose routes make the client contract. The spec
// itself is read from the openapi plugin (discovered via [SpecSource]); the api
// plugin reference skips the contract check when no route exists.
func (p *ClientsPlugin) From(apiPlugin *Plugin) *ClientsPlugin {
	p.apiPlugin = apiPlugin
	return p
}

// Name returns the plugin name. Implements app.Plugin.
func (p *ClientsPlugin) Name() string { return describerNameClients }

// Configure captures the owning module and attributes every api plugin's
// routes to the project and feature of the module that owns the plugin.
// Implements app.Configurer.
//
// Ownership is the module that owns the api.Plugin the endpoint was
// registered on, never this declaration's module, so an operation whose owner
// declares no feature stays unattributed instead of borrowing the
// declaration's lineage.
func (p *ClientsPlugin) Configure(_ context.Context, owner *app.Module) error {
	p.module = owner
	project := owner.Root().Name()
	for _, owned := range owner.Root().CollectPlugins() {
		apiPlugin, ok := owned.Plugin.(*Plugin)
		if !ok {
			continue
		}
		feature := owned.Owner.EffectiveFeature()
		if feature == nil {
			continue
		}
		apiPlugin.attributeRoutes(clientcontract.OperationProducer{Project: project, Feature: feature.ID})
	}
	return nil
}

// Describe checks that the provider's contract is first-party. It writes
// nothing: the openapi describer writes the contract, producers included.
// Implements app.Describer.
func (p *ClientsPlugin) Describe(ctx *app.DescribeContext) error {
	if !ctx.Wants(p.Name()) || p.opts.ThirdParty {
		return nil
	}
	if p.apiPlugin != nil && len(p.apiPlugin.DiscoveredRoutes()) == 0 {
		return nil
	}
	specJSON, err := p.specJSON()
	if err != nil {
		return err
	}
	spec, err := ReadOpenAPISpec(specJSON)
	if err != nil {
		return err
	}
	if spec.Contract == nil {
		return fmt.Errorf("api: a provider client contract requires x-putnami-client; set ClientsOptions.ThirdParty only for external contracts")
	}
	return nil
}

// specJSON resolves the OpenAPI spec from the discovered SpecSource (the openapi
// plugin). The dependency is explicit: if no SpecSource is registered describe
// fails with an actionable message rather than passing an unchecked contract.
func (p *ClientsPlugin) specJSON() ([]byte, error) {
	if p.module != nil {
		for _, src := range app.Collect[SpecSource](p.module) {
			return src.OpenAPISpecJSON()
		}
	}
	return nil, fmt.Errorf(
		"api: the client declaration found no OpenAPI source; register openapi.NewPlugin(...).From(apiPlugin) so it can read the spec")
}
