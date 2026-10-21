package grpc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/app"
	diag "go.putnami.dev/protocol/diagnostic"
	grpcservices "go.putnami.dev/protocol/grpc-services"
	"google.golang.org/grpc"
)

// GRPCServicesDescribePath is the putnami.grpc-services.v1 inventory a
// describe run writes, relative to DescribeContext.OutputDir.
const GRPCServicesDescribePath = "schema/grpc-services.json"

// GRPCServicesDescribeTarget is the describe target that asks for the service
// inventory alone. Each gRPC surface also answers to its own plugin name.
const GRPCServicesDescribeTarget = "grpc-services"

// inventoryPackage is the provenance package of every service this package
// reports.
const inventoryPackage = "go.putnami.dev/grpc"

// serviceSource is a composed gRPC surface that reports the services it
// registered. Plugin, GatewayPlugin and ApiBridge implement it.
type serviceSource interface {
	Name() string
	inventoryRoot() *app.Module
	registeredServices(project string) ([]grpcservices.Service, error)
}

// describeGRPCServices writes the one service inventory of the application
// self belongs to. Every composed surface is a describer, and exactly one of
// them writes: the first surface, in module-tree order, that the context asks
// for. Electing among requested surfaces keeps a describe that targets one
// surface by name complete, because the elected surface writes the services
// of every surface, requested or not.
//
// The inventory needs the configured application root, so a surface added
// with UseForDescribe, which is never configured, is refused instead of
// writing a partial inventory.
func describeGRPCServices(ctx *app.DescribeContext, self serviceSource) error {
	if !wantsServiceInventory(ctx, self) {
		return nil
	}
	root := self.inventoryRoot()
	if root == nil {
		return fmt.Errorf(
			"grpc: %s has no configured application root, so it cannot describe the service inventory; compose it with Use, because UseForDescribe is not supported for gRPC surfaces",
			self.Name())
	}
	sources := app.Collect[serviceSource](root)
	var owner serviceSource
	found := false
	for _, source := range sources {
		if owner == nil && wantsServiceInventory(ctx, source) {
			owner = source
		}
		if source == self {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("grpc: %s is not composed in the module tree of its configured root %q", self.Name(), root.Name())
	}
	if owner != self {
		return nil
	}

	var services []grpcservices.Service
	for _, source := range sources {
		registered, err := source.registeredServices(root.Name())
		if err != nil {
			return err
		}
		services = append(services, registered...)
	}
	services, err := collapseIdenticalServices(services)
	if err != nil {
		return err
	}
	manifest, diags := grpcservices.Canonicalize(services)
	if diag.HasErrors(diags) {
		return formatGRPCServicesDiagnostics(diags)
	}
	body, diags := grpcservices.CanonicalJSON(manifest)
	if diag.HasErrors(diags) {
		return formatGRPCServicesDiagnostics(diags)
	}
	out := filepath.Join(ctx.OutputDir, GRPCServicesDescribePath)
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return err
	}
	return os.WriteFile(out, body, 0o600)
}

// wantsServiceInventory mirrors the application's describer filter for a
// surface: its own name, or the inventory target.
func wantsServiceInventory(ctx *app.DescribeContext, source serviceSource) bool {
	return ctx.Wants(source.Name()) || ctx.Wants(GRPCServicesDescribeTarget)
}

// collapseIdenticalServices keeps one copy of services that are byte-identical
// once their methods are sorted, such as one service registered on two native
// servers. Two different services with the same name, transport and basePath
// stay in the list, and canonicalization rejects them as a duplicate.
func collapseIdenticalServices(services []grpcservices.Service) ([]grpcservices.Service, error) {
	seen := make(map[string]bool, len(services))
	out := make([]grpcservices.Service, 0, len(services))
	for _, service := range services {
		methods := append([]grpcservices.Method{}, service.Methods...)
		sort.SliceStable(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })
		service.Methods = methods
		key, err := json.Marshal(service)
		if err != nil {
			return nil, fmt.Errorf("grpc: encode service %s: %w", service.Name, err)
		}
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		out = append(out, service)
	}
	return out, nil
}

func formatGRPCServicesDiagnostics(diags []diag.Diagnostic) error {
	parts := make([]string, 0, len(diags))
	for _, d := range diags {
		if d.Field == "" {
			parts = append(parts, fmt.Sprintf("[%s] %s", d.Code, d.Message))
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s] %s: %s", d.Code, d.Field, d.Message))
	}
	return fmt.Errorf("grpc service inventory validation failed: %s", strings.Join(parts, "; "))
}

// --- native gRPC server ---

// Describe writes the service inventory. See describeGRPCServices.
func (p *Plugin) Describe(ctx *app.DescribeContext) error {
	return describeGRPCServices(ctx, p)
}

// AdditionalDescribeTargets makes the plugin answer the inventory target.
func (p *Plugin) AdditionalDescribeTargets() []string {
	return []string{GRPCServicesDescribeTarget}
}

func (p *Plugin) inventoryRoot() *app.Module { return p.root }

// registeredServices runs the registrars Start runs, and reflection when it is
// enabled, on a bare server that is never started: no listener, no network.
// The services and their streaming flags are what that server registered.
func (p *Plugin) registeredServices(project string) ([]grpcservices.Service, error) {
	server := grpc.NewServer()
	defer server.Stop()
	p.registerServices(server)

	info := server.GetServiceInfo()
	names := make([]string, 0, len(info))
	for name := range info {
		names = append(names, name)
	}
	sort.Strings(names)
	services := make([]grpcservices.Service, 0, len(names))
	for _, name := range names {
		methods := make([]grpcservices.Method, 0, len(info[name].Methods))
		for _, method := range info[name].Methods {
			methods = append(methods, grpcservices.Method{
				Name:            method.Name,
				ClientStreaming: method.IsClientStream,
				ServerStreaming: method.IsServerStream,
			})
		}
		services = append(services, grpcservices.Service{
			Name:         name,
			Transport:    grpcservices.TransportGRPC,
			MethodsKnown: true,
			Methods:      methods,
			Provenance: grpcservices.Provenance{
				Project:    project,
				Package:    inventoryPackage,
				SourceKind: grpcservices.SourceGRPCServer,
			},
		})
	}
	return services, nil
}

// --- Connect gateway ---

// Describe writes the service inventory. See describeGRPCServices.
func (p *GatewayPlugin) Describe(ctx *app.DescribeContext) error {
	return describeGRPCServices(ctx, p)
}

// AdditionalDescribeTargets makes the gateway answer the inventory target.
func (p *GatewayPlugin) AdditionalDescribeTargets() []string {
	return []string{GRPCServicesDescribeTarget}
}

func (p *GatewayPlugin) inventoryRoot() *app.Module { return p.root }

// registeredServices lists every service RegisterOn mounted. A gateway handler
// is opaque, so its methods are unknown; basePath is the prefix route the
// mount added to the HTTP server.
func (p *GatewayPlugin) registeredServices(project string) ([]grpcservices.Service, error) {
	mounts := make([]string, 0, len(p.served))
	for mount := range p.served {
		mounts = append(mounts, mount)
	}
	sort.Strings(mounts)
	services := make([]grpcservices.Service, 0, len(mounts))
	for _, mount := range mounts {
		services = append(services, grpcservices.Service{
			Name:         p.served[mount],
			Transport:    grpcservices.TransportConnect,
			BasePath:     mount,
			MethodsKnown: false,
			Methods:      []grpcservices.Method{},
			Provenance: grpcservices.Provenance{
				Project:    project,
				Package:    inventoryPackage,
				SourceKind: grpcservices.SourceConnectGateway,
			},
		})
	}
	return services, nil
}

// --- api → Connect bridge ---

// Describe writes the service inventory. See describeGRPCServices.
func (b *ApiBridge) Describe(ctx *app.DescribeContext) error {
	return describeGRPCServices(ctx, b)
}

// AdditionalDescribeTargets makes the bridge answer the inventory target.
func (b *ApiBridge) AdditionalDescribeTargets() []string {
	return []string{GRPCServicesDescribeTarget}
}

func (b *ApiBridge) inventoryRoot() *app.Module { return b.root }

// recordServedMethod records one Connect URL Configure mounted. Two routes
// that share a URL serve one method; when they disagree on server streaming,
// the served shape is ambiguous and the inventory refuses it.
func (b *ApiBridge) recordServedMethod(rpcPath string, serverStreaming bool) {
	if previous, ok := b.served[rpcPath]; ok && previous != serverStreaming && b.servedErr == nil {
		b.servedErr = fmt.Errorf("grpc: the api bridge mounts %s twice, once as a server stream and once as a unary call", rpcPath)
	}
	b.served[rpcPath] = serverStreaming
}

// registeredServices groups the mounted Connect URLs "/<service>/<method>" by
// service. Every listed method is an exact POST route at basePath followed by
// the method name. Client and bidirectional streams are never mounted, so no
// listed method streams from the client.
func (b *ApiBridge) registeredServices(project string) ([]grpcservices.Service, error) {
	if b.servedErr != nil {
		return nil, b.servedErr
	}
	grouped := map[string][]grpcservices.Method{}
	for rpcPath, serverStreaming := range b.served {
		service, method, ok := strings.Cut(strings.TrimPrefix(rpcPath, "/"), "/")
		if !ok || !strings.HasPrefix(rpcPath, "/") {
			return nil, fmt.Errorf("grpc: the api bridge mounted %s, which is not a /<service>/<method> Connect URL", rpcPath)
		}
		grouped[service] = append(grouped[service], grpcservices.Method{
			Name:            method,
			ClientStreaming: false,
			ServerStreaming: serverStreaming,
		})
	}
	names := make([]string, 0, len(grouped))
	for name := range grouped {
		names = append(names, name)
	}
	sort.Strings(names)
	services := make([]grpcservices.Service, 0, len(names))
	for _, name := range names {
		services = append(services, grpcservices.Service{
			Name:         name,
			Transport:    grpcservices.TransportConnect,
			BasePath:     "/" + name + "/",
			MethodsKnown: true,
			Methods:      grouped[name],
			Provenance: grpcservices.Provenance{
				Project:    project,
				Package:    inventoryPackage,
				SourceKind: grpcservices.SourceAPIBridge,
			},
		})
	}
	return services, nil
}
