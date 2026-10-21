package grpc

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	grpcservices "go.putnami.dev/protocol/grpc-services"
	httproutes "go.putnami.dev/protocol/http-routes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const inventoryProject = "example/orders"

func describeInventory(t *testing.T, a *app.Application, targets ...string) string {
	t.Helper()
	out := t.TempDir()
	if err := a.Describe(out, targets); err != nil {
		t.Fatalf("Describe(%v): %v", targets, err)
	}
	return out
}

func readServiceInventory(t *testing.T, out string) (*grpcservices.Manifest, []byte) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(out, GRPCServicesDescribePath))
	if err != nil {
		t.Fatalf("read service inventory: %v", err)
	}
	manifest, diags := grpcservices.ParseAndValidateManifest(body)
	if diag.HasErrors(diags) {
		t.Fatalf("service inventory does not validate: %v\n%s", diags, body)
	}
	return manifest, body
}

func readRouteInventory(t *testing.T, out string) *httproutes.Manifest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(out, phttp.HTTPRoutesDescribePath))
	if err != nil {
		t.Fatalf("read route inventory: %v", err)
	}
	manifest, diags := httproutes.ParseAndValidateManifest(body)
	if diag.HasErrors(diags) {
		t.Fatalf("route inventory does not validate: %v", diags)
	}
	return manifest
}

func findService(manifest *grpcservices.Manifest, name string, transport grpcservices.Transport) *grpcservices.Service {
	for i := range manifest.Services {
		if manifest.Services[i].Name == name && manifest.Services[i].Transport == transport {
			return &manifest.Services[i]
		}
	}
	return nil
}

func methodByName(service *grpcservices.Service, name string) *grpcservices.Method {
	for i := range service.Methods {
		if service.Methods[i].Name == name {
			return &service.Methods[i]
		}
	}
	return nil
}

func registerHealth(calls *int) ServiceRegistrar {
	return func(s *grpc.Server) {
		*calls++
		healthpb.RegisterHealthServer(s, health.NewServer())
	}
}

// busyPort returns a port another listener holds for the whole test, so a
// describe that tried to listen on it would fail.
func busyPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func TestServiceInventory_NativeListsRegisteredServicesAndStreaming(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "service-inventory", "native-inventory-lists-registered-methods-and-streaming")
	calls := 0
	plugin := NewPlugin(Config{Port: busyPort(t)}).Register(registerHealth(&calls)).WithReflection(true)
	a := app.New(inventoryProject)
	a.Use(plugin)

	manifest, _ := readServiceInventory(t, describeInventory(t, a))

	if calls != 1 {
		t.Fatalf("registrar ran %d times during one describe, want 1", calls)
	}
	if plugin.Server() != nil {
		t.Fatal("describe started the gRPC server")
	}
	healthService := findService(manifest, "grpc.health.v1.Health", grpcservices.TransportGRPC)
	if healthService == nil {
		t.Fatalf("health service missing from %+v", manifest.Services)
	}
	if !healthService.MethodsKnown || healthService.BasePath != "" {
		t.Fatalf("native service = %+v, want known methods and no basePath", healthService)
	}
	if healthService.Provenance != (grpcservices.Provenance{Project: inventoryProject, Package: "go.putnami.dev/grpc", SourceKind: grpcservices.SourceGRPCServer}) {
		t.Fatalf("provenance = %+v", healthService.Provenance)
	}
	declared := make([]string, 0, len(healthpb.Health_ServiceDesc.Methods)+len(healthpb.Health_ServiceDesc.Streams))
	for _, m := range healthpb.Health_ServiceDesc.Methods {
		declared = append(declared, m.MethodName)
	}
	for _, s := range healthpb.Health_ServiceDesc.Streams {
		declared = append(declared, s.StreamName)
	}
	slices.Sort(declared)
	listed := make([]string, 0, len(healthService.Methods))
	for _, m := range healthService.Methods {
		listed = append(listed, m.Name)
	}
	if !slices.Equal(listed, declared) {
		t.Fatalf("health methods = %v, want the registered set %v", listed, declared)
	}
	if check := methodByName(healthService, "Check"); check == nil || check.ClientStreaming || check.ServerStreaming {
		t.Fatalf("Check = %+v, want unary", check)
	}
	if watch := methodByName(healthService, "Watch"); watch == nil || watch.ClientStreaming || !watch.ServerStreaming {
		t.Fatalf("Watch = %+v, want a server stream", watch)
	}
	for _, name := range []string{"grpc.reflection.v1.ServerReflection", "grpc.reflection.v1alpha.ServerReflection"} {
		reflectionService := findService(manifest, name, grpcservices.TransportGRPC)
		if reflectionService == nil {
			t.Fatalf("enabled reflection service %s missing from %+v", name, manifest.Services)
		}
		info := methodByName(reflectionService, "ServerReflectionInfo")
		if info == nil || !info.ClientStreaming || !info.ServerStreaming {
			t.Fatalf("%s ServerReflectionInfo = %+v, want a bidirectional stream", name, info)
		}
	}
}

func TestServiceInventory_ReflectionAppearsOnlyWhenEnabled(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "service-inventory", "reflection-is-listed-only-when-enabled")
	calls := 0
	a := app.New(inventoryProject)
	a.Use(NewPlugin(Config{}).Register(registerHealth(&calls)))

	manifest, _ := readServiceInventory(t, describeInventory(t, a))
	for _, service := range manifest.Services {
		if strings.HasPrefix(service.Name, "grpc.reflection.") {
			t.Fatalf("reflection is listed although it is disabled: %+v", manifest.Services)
		}
	}
	if len(manifest.Services) != 1 {
		t.Fatalf("services = %+v, want only the health service", manifest.Services)
	}
}

func TestServiceInventory_GatewayJoinsItsPrefixRoute(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "service-inventory", "gateway-service-joins-its-prefix-route")
	for _, prefix := range []string{"", "/rpc"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
			gateway := NewGatewayPlugin(GatewayConfig{PathPrefix: prefix}).
				MountHandler("/pkg.v1.Echo/", http.NotFoundHandler()).
				MountHandler("Health", http.NotFoundHandler())
			gateway.RegisterOn(server)
			a := app.New(inventoryProject)
			a.Use(server)
			a.Use(gateway)

			out := describeInventory(t, a)
			services, _ := readServiceInventory(t, out)
			routes := readRouteInventory(t, out)

			if len(services.Services) != 2 {
				t.Fatalf("services = %+v, want the two mounted services", services.Services)
			}
			for _, service := range services.Services {
				if service.Provenance.SourceKind != grpcservices.SourceConnectGateway || service.MethodsKnown || len(service.Methods) != 0 {
					t.Fatalf("gateway service = %+v, want connect-gateway with unknown methods", service)
				}
				if want := prefix + "/" + service.Name + "/"; service.BasePath != want {
					t.Fatalf("basePath = %q, want %q", service.BasePath, want)
				}
				if service.Provenance.Project != routes.Routes[0].Provenance.Project {
					t.Fatalf("project %q differs from the route inventory's %q", service.Provenance.Project, routes.Routes[0].Provenance.Project)
				}
				joined := 0
				for _, route := range routes.Routes {
					if route.Match == httproutes.MatchPrefix && route.Path == service.BasePath &&
						route.Provenance.SourceKind == httproutes.SourceStaticMount && slices.Contains(route.Methods, "POST") {
						joined++
					}
				}
				if joined != 1 {
					t.Fatalf("service %s joins %d prefix routes at %s, want 1: %+v", service.Name, joined, service.BasePath, routes.Routes)
				}
			}
		})
	}
}

func TestServiceInventory_UnmountedGatewayListsNothing(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "service-inventory", "an-empty-surface-writes-an-empty-inventory")
	a := app.New(inventoryProject)
	a.Use(NewGatewayPlugin(GatewayConfig{}).MountHandler("/pkg.v1.Echo/", http.NotFoundHandler()))

	manifest, body := readServiceInventory(t, describeInventory(t, a))
	if len(manifest.Services) != 0 {
		t.Fatalf("an unmounted handler is listed: %+v", manifest.Services)
	}
	empty, diags := grpcservices.Canonicalize(nil)
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	want, diags := grpcservices.CanonicalJSON(empty)
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("empty inventory bytes\n%s\nwant\n%s", body, want)
	}
}

func TestServiceInventory_NoSurfaceWritesNoFile(t *testing.T) {
	a := app.New(inventoryProject)
	a.Use(phttp.NewServerPlugin(phttp.ServerConfig{Port: 0}))
	out := describeInventory(t, a)
	if _, err := os.Stat(filepath.Join(out, GRPCServicesDescribePath)); !os.IsNotExist(err) {
		t.Fatalf("an application without a gRPC surface wrote an inventory: %v", err)
	}
}

// bridgeApplication composes an HTTP server, an api plugin with a unary route,
// a server stream and a client stream, and the bridge over them.
func bridgeApplication(t *testing.T) *app.Application {
	t.Helper()
	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	apiPlugin := api.New(server)
	apiPlugin.Register(api.Endpoint("GET", "/items").
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON([]bridgeUser{}) }))
	apiPlugin.Register(api.Endpoint("GET", "/items/watch").
		Returns(api.StreamOf[bridgeUser]()).
		Handle(api.ServerStream(func(*api.ServerStreamContext[bridgeUser]) error { return nil })))
	apiPlugin.Register(api.Endpoint("GET", "/items/{id}/edits").
		Params(api.Type[bridgePlainIDParams]()).
		Body(api.StreamOf[bridgeCreateBody]()).
		Returns(api.Type[bridgeUser]()).
		Handle(api.ClientStream(func(*api.ClientStreamContext[bridgeCreateBody, bridgeUser]) error { return nil })))
	bridge := NewApiBridge(apiPlugin, server, WithPackage("items.v1"))
	a := app.New(inventoryProject)
	a.Use(server)
	a.Use(apiPlugin)
	a.Use(bridge)
	return a
}

func TestServiceInventory_BridgeMethodsJoinExactPOSTRoutes(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "service-inventory", "bridge-methods-join-exact-post-routes")
	a := bridgeApplication(t)
	out := describeInventory(t, a)
	services, _ := readServiceInventory(t, out)
	routes := readRouteInventory(t, out)

	if len(services.Services) != 1 {
		t.Fatalf("services = %+v, want the one bridged service", services.Services)
	}
	service := services.Services[0]
	if service.Name != "items.v1.ApiService" || service.BasePath != "/items.v1.ApiService/" ||
		!service.MethodsKnown || service.Provenance.SourceKind != grpcservices.SourceAPIBridge {
		t.Fatalf("bridged service = %+v", service)
	}
	if len(service.Methods) != 2 {
		t.Fatalf("methods = %+v, want the unary route and the server stream, never the client stream", service.Methods)
	}
	streams := 0
	for _, method := range service.Methods {
		if method.ClientStreaming {
			t.Fatalf("a client stream is listed: %+v", method)
		}
		if method.ServerStreaming {
			streams++
		}
		joined := 0
		for _, route := range routes.Routes {
			if route.Match == httproutes.MatchExact && route.Path == service.BasePath+method.Name && slices.Contains(route.Methods, "POST") {
				joined++
			}
		}
		if joined != 1 {
			t.Fatalf("method %s joins %d exact POST routes, want 1: %+v", method.Name, joined, routes.Routes)
		}
	}
	if streams != 1 {
		t.Fatalf("methods = %+v, want exactly one server stream", service.Methods)
	}
}

// fullSurfaceApplication composes all three gRPC surfaces in one application.
func fullSurfaceApplication(t *testing.T) *app.Application {
	t.Helper()
	a := bridgeApplication(t)
	server := app.Collect[*phttp.ServerPlugin](a.Module)[0]
	gateway := NewGatewayPlugin(GatewayConfig{}).MountHandler("/pkg.v1.Echo/", http.NotFoundHandler())
	gateway.RegisterOn(server)
	calls := 0
	a.Use(gateway)
	a.Use(NewPlugin(Config{Port: busyPort(t)}).Register(registerHealth(&calls)))
	return a
}

func TestServiceInventory_TargetedDescribeWritesTheWholeInventory(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "service-inventory", "a-targeted-describe-writes-the-whole-inventory")
	manifest, want := readServiceInventory(t, describeInventory(t, fullSurfaceApplication(t)))
	if len(manifest.Services) != 3 {
		t.Fatalf("services = %+v, want one per surface", manifest.Services)
	}
	for _, targets := range [][]string{
		{"grpc"}, {"grpc-gateway"}, {"grpc-api-bridge"}, {GRPCServicesDescribeTarget}, {"grpc-api-bridge", "grpc"},
	} {
		_, got := readServiceInventory(t, describeInventory(t, fullSurfaceApplication(t), targets...))
		if !bytes.Equal(got, want) {
			t.Fatalf("describe %v wrote\n%s\nwant the full inventory\n%s", targets, got, want)
		}
	}
	out := describeInventory(t, fullSurfaceApplication(t), "openapi")
	if _, err := os.Stat(filepath.Join(out, GRPCServicesDescribePath)); !os.IsNotExist(err) {
		t.Fatalf("a describe that asks for no gRPC surface wrote the inventory: %v", err)
	}
}

func TestServiceInventory_UseForDescribeIsRefused(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "service-inventory", "use-for-describe-is-refused")
	for _, describer := range []app.Describer{
		NewPlugin(Config{}),
		NewGatewayPlugin(GatewayConfig{}),
		NewApiBridge(nil, nil),
	} {
		a := app.New(inventoryProject)
		a.UseForDescribe(describer)
		err := a.Describe(t.TempDir(), nil)
		if err == nil || !strings.Contains(err.Error(), "Use") || !strings.Contains(err.Error(), "UseForDescribe") {
			t.Fatalf("%s added with UseForDescribe: err = %v, want a refusal that names Use", describer.Name(), err)
		}
	}
}

func TestServiceInventory_RepeatedConfigureListsEachServiceOnce(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "service-inventory", "repeated-configure-lists-each-service-once")
	a := bridgeApplication(t)
	server := app.Collect[*phttp.ServerPlugin](a.Module)[0]
	gateway := NewGatewayPlugin(GatewayConfig{}).MountHandler("/pkg.v1.Echo/", http.NotFoundHandler())
	gateway.RegisterOn(server)
	calls := 0
	a.Use(gateway)
	a.Use(NewPlugin(Config{}).Register(registerHealth(&calls)))

	// Validate runs every Configure once, and Describe runs them again.
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	manifest, _ := readServiceInventory(t, describeInventory(t, a))
	if len(manifest.Services) != 3 {
		t.Fatalf("services = %+v, want one per surface", manifest.Services)
	}
	bridged := findService(manifest, "items.v1.ApiService", grpcservices.TransportConnect)
	if bridged == nil || len(bridged.Methods) != 2 {
		t.Fatalf("bridged service = %+v, want two methods once each", bridged)
	}
	if calls != 1 {
		t.Fatalf("registrar ran %d times, want once per describe", calls)
	}
}

func TestServiceInventory_IdenticalServicesCollapseAndConflictsFail(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "service-inventory", "conflicting-services-fail-the-describe")
	calls := 0
	identical := app.New(inventoryProject)
	identical.Use(NewPlugin(Config{}).Register(registerHealth(&calls)))
	identical.Use(NewPlugin(Config{}).Register(registerHealth(&calls)))
	manifest, _ := readServiceInventory(t, describeInventory(t, identical))
	if len(manifest.Services) != 1 {
		t.Fatalf("services = %+v, want one copy of the health service registered twice", manifest.Services)
	}

	conflicting := app.New(inventoryProject)
	conflicting.Use(NewPlugin(Config{}).Register(registerHealth(&calls)))
	conflicting.Use(NewPlugin(Config{}).Register(func(s *grpc.Server) {
		s.RegisterService(&grpc.ServiceDesc{
			ServiceName: "grpc.health.v1.Health",
			HandlerType: (*any)(nil),
			Methods:     []grpc.MethodDesc{{MethodName: "Ping"}},
		}, struct{}{})
	}))
	err := conflicting.Describe(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), grpcservices.ErrorCodeDuplicateService) {
		t.Fatalf("err = %v, want %s", err, grpcservices.ErrorCodeDuplicateService)
	}
}

func TestServiceInventory_InvalidGatewayPathFailsTheDescribe(t *testing.T) {
	a := app.New(inventoryProject)
	gateway := NewGatewayPlugin(GatewayConfig{}).MountHandler("/not/a/service/", http.NotFoundHandler())
	gateway.RegisterOn(phttp.NewServerPlugin(phttp.ServerConfig{Port: 0}))
	a.Use(gateway)
	err := a.Describe(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), grpcservices.ErrorCodeInvalidServiceName) {
		t.Fatalf("err = %v, want %s", err, grpcservices.ErrorCodeInvalidServiceName)
	}
}

func TestServiceInventory_BridgeRefusesAURLMountedWithTwoShapes(t *testing.T) {
	bridge := &ApiBridge{served: map[string]bool{}}
	bridge.recordServedMethod("/items.v1.ApiService/GetItems", false)
	bridge.recordServedMethod("/items.v1.ApiService/GetItems", false)
	if bridge.servedErr != nil {
		t.Fatalf("one shape mounted twice is one method: %v", bridge.servedErr)
	}
	bridge.recordServedMethod("/items.v1.ApiService/GetItems", true)
	if _, err := bridge.registeredServices(inventoryProject); err == nil {
		t.Fatal("a URL mounted as a unary call and a server stream was listed")
	}
}
