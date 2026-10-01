package proto

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
)

// PluginOptions configures the proto plugin.
type PluginOptions struct {
	// PackageName is the proto package declared in the document. Default: "api.v1".
	PackageName string
	// GoPackage is the optional `option go_package = "..."` value.
	GoPackage string
	// Output is the path WriteTo uses when called with an empty argument.
	// Default "schema/api.proto". It does not affect Describe, which always
	// writes DefaultOutputPath below the describe output directory, and it is
	// never written during Configure.
	Output string
}

const (
	// DefaultOutputPath is the default committed location for the generated proto.
	DefaultOutputPath = "schema/api.proto"
	// FallbackOutputPath is the gitignored location for callers that want the
	// artifact kept out of the reviewed tree when driving WriteTo themselves.
	FallbackOutputPath = ".gen/schema/api.proto"
)

// Plugin generates a Protocol Buffer document from api.Plugin discovery.
type Plugin struct {
	opts      PluginOptions
	apiPlugin *api.Plugin
	doc       *Document
	mu        sync.RWMutex
}

// NewPlugin creates a new proto plugin.
func NewPlugin(opts PluginOptions) *Plugin {
	if opts.Output == "" {
		opts.Output = DefaultOutputPath
	}
	return &Plugin{opts: opts}
}

// From wires the plugin to an api.Plugin so its DiscoveredRoutes feed proto generation
// during Configure. The api plugin must be registered before this plugin so its
// Configure() runs first.
func (p *Plugin) From(apiPlugin *api.Plugin) *Plugin {
	p.apiPlugin = apiPlugin
	return p
}

// Name implements app.Plugin.
func (p *Plugin) Name() string { return "proto" }

// CapabilitySchemas exposes the concrete proto artifact to capability inventory.
func (*Plugin) CapabilitySchemas() []app.CapabilitySchema {
	return []app.CapabilitySchema{{Name: "api-proto", Kind: "proto", Path: DefaultOutputPath}}
}

// CapabilityDiscoverers reports no separate discoverer; the schema artifact is authoritative.
func (*Plugin) CapabilityDiscoverers() []app.CapabilityDiscoverer { return nil }

// Document returns the generated proto document, or nil if Configure has not run.
func (p *Plugin) Document() *Document {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.doc
}

// Configure pulls discovered routes from the wired api plugin and generates
// the proto document into memory. Implements app.Configurer.
//
// Configure has no filesystem side effect: the contract artifact is a
// build-time output produced by Describe, so a served workload never writes
// into its working directory and never fails to start because that directory
// is read-only. Build-time describe mode and a normal app start therefore share
// an identical Configure path, exactly like the openapi plugin.
func (p *Plugin) Configure(_ context.Context, _ *app.Module) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var routes []api.DiscoveredRoute
	if p.apiPlugin != nil {
		routes = p.apiPlugin.DiscoveredRoutes()
	}

	doc := Generate(routes, Options{
		PackageName: p.opts.PackageName,
		GoPackage:   p.opts.GoPackage,
	})
	if doc.GenerationErr != nil {
		// A declaration proto3 cannot carry faithfully stops the provider here.
		// Publishing the descriptor anyway would advertise a Connect transport
		// whose messages describe a shape the endpoint never sends.
		return doc.GenerationErr
	}
	p.doc = &doc
	if p.apiPlugin != nil {
		p.apiPlugin.PublishClientProtobuf(api.ClientProtobufProjection{
			Descriptor:   ClientDescriptor(doc),
			RouteMethods: doc.RouteMethods(),
		})
	}
	return nil
}

// WriteTo renders the current document to path, creating parent directories as
// needed. An empty path writes to PluginOptions.Output. It is the explicit
// escape hatch for a caller that wants the artifact outside `putnami build` —
// for example a `go run` export step. Returns nil before Configure has produced
// a document.
func (p *Plugin) WriteTo(path string) error {
	p.mu.RLock()
	doc := p.doc
	p.mu.RUnlock()
	if doc == nil {
		return nil
	}
	if path == "" {
		path = p.opts.Output
	}
	return p.writeAt(path, doc.Content)
}

// Describe implements app.Describer. Called during build-time describe mode;
// writes <ctx.OutputDir>/schema/api.proto regardless of whether the on-disk
// Output option was set on the plugin. The runner copies the resulting file
// into the project tree (or skips when generate.schema=false).
//
// A plugin that has no document REMOVES an earlier run's descriptor instead of
// leaving it.
func (p *Plugin) Describe(ctx *app.DescribeContext) error {
	if !ctx.Wants(p.Name()) {
		// This run is not authoritative for the proto descriptor, so it says
		// nothing about whether an existing one is stale.
		return nil
	}
	out := filepath.Join(ctx.OutputDir, DefaultOutputPath)
	p.mu.RLock()
	doc := p.doc
	p.mu.RUnlock()
	if doc == nil {
		return removeStaleDescribeArtifact(out)
	}
	return p.writeAt(out, doc.Content)
}

// removeStaleDescribeArtifact drops an artifact this run did not produce. A
// missing file is the expected case, not an error.
func removeStaleDescribeArtifact(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeAt writes content to outPath. Returns nil when outPath is empty so
// the caller can short-circuit cleanly.
func (p *Plugin) writeAt(outPath, content string) error {
	if outPath == "" {
		return nil
	}
	if !filepath.IsAbs(outPath) {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		outPath = filepath.Join(cwd, outPath)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o750); err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(content), 0o600)
}

// Start is a no-op for the proto plugin.
func (p *Plugin) Start(_ context.Context, _ *app.Module) error { return nil }

// Stop is a no-op for the proto plugin.
func (p *Plugin) Stop(_ context.Context, _ *app.Module) error { return nil }
